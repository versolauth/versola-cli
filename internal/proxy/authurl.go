package proxy

import (
	"fmt"
	"strconv"
	"strings"
)

// AuthURL is a validated, normalized --auth-url.
type AuthURL struct {
	// URL is the normalized form -- what versola-tools (and so every
	// issuer, endpoint, redirect URI and passkey origin Versola
	// advertises) gets: lowercase scheme://host[:port], no trailing slash.
	URL    string
	Scheme string // "http" or "https"
	Host   string // lowercase, letters/digits/'.'/'-' only
	Port   string // "" when the URL has none
}

// TLS reports whether the proxy terminates TLS itself for this URL in
// the given mode: only when it owns the host's ports and the URL is https.
func (a AuthURL) TLS(mode string) bool {
	return mode == ModeNginx && a.Scheme == "https"
}

// ParseAuthURL validates and normalizes --auth-url for a vps deployment.
//
// One rule behind all of it: the URL Versola advertises must be exactly
// what a browser will see as its origin, and the proxy must actually
// answer there. versola-tools builds every public URL as
// "<auth-url>/<endpoint>", and passkey origins are matched exactly, so:
//
//   - scheme://host[:port] only -- a path, query, fragment or user would
//     be advertised but never reachable (Versola is mounted at the root);
//   - lowercase, and a single trailing slash trimmed -- browsers serialize
//     origins that way;
//   - no default port written out (":80" on http, ":443" on https) and no
//     leading zero -- browsers drop/normalize those away;
//   - host: letters, digits, '.', '-' only (an allow-list, so nothing else
//     can reach the generated configs);
//   - ModeNginx: no port at all -- the proxy only listens on 80/443;
//   - TLS: a public domain name, not an IP or localhost -- Let's Encrypt
//     won't issue for those, and the ACME module would just keep failing.
func ParseAuthURL(raw, mode string) (AuthURL, error) {
	fail := func(reason string) (AuthURL, error) {
		return AuthURL{}, fmt.Errorf("--auth-url %q: %s", raw, reason)
	}

	u := strings.ToLower(strings.TrimSuffix(raw, "/"))
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return fail("must be http(s)://<host>[:<port>], e.g. https://auth.example.com")
	}
	if scheme != "http" && scheme != "https" {
		return fail("scheme must be http or https")
	}

	host, port, hasPort := strings.Cut(rest, ":")
	if host == "" || strings.Trim(host, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" {
		return fail("must be http(s)://<host>[:<port>] -- no path, query, fragment or user; the host may only contain letters, digits, '.' and '-'")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fail("not a valid host name (empty label, or a label starting/ending with '-')")
		}
	}
	if hasPort {
		if port == "" || strings.Trim(port, "0123456789") != "" {
			return fail("port must be digits only")
		}
		if port[0] == '0' {
			return fail("port must not start with 0")
		}
		if n, err := strconv.Atoi(port); err != nil || len(port) > 5 || n > 65535 {
			return fail("port must be 1-65535")
		}
		if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
			return fail("remove the default port -- browsers drop it from the origin, so it would never match")
		}
		if mode == ModeNginx {
			return fail("the proxy serves on 80/443 -- remove the port (or use --proxy external behind your own proxy)")
		}
	}

	a := AuthURL{URL: u, Scheme: scheme, Host: host, Port: port}
	if a.TLS(mode) {
		if strings.Trim(host, "0123456789.") == "" {
			return fail("Let's Encrypt doesn't issue certificates for IP addresses -- use a domain")
		}
		if !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return fail("Let's Encrypt only issues certificates for public domain names (e.g. auth.example.com)")
		}
	}
	return a, nil
}
