package proxy

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// HostHasGlobalIPv6 reports whether this (Linux) host has a globally
// routable IPv6 address. Only then does the proxy also listen on [::] and
// let the ACME client use AAAA records: on a host with IPv6 disabled a
// [::] listen makes nginx fail to start at all, and without a global
// address an IPv6 connection to Let's Encrypt can't succeed.
//
// Reads /proc/net/if_inet6 (fields: address, ifindex, prefix length,
// scope, flags, name; scope 00 = global). Anything else -- not Linux, no
// IPv6, unreadable -- counts as "no".
func HostHasGlobalIPv6() bool {
	return hasGlobalIPv6(readFileOrEmpty("/proc/net/if_inet6"))
}

func hasGlobalIPv6(ifInet6 string) bool {
	for _, line := range strings.Split(ifInet6, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[3] != "00" {
			continue
		}
		addr, iface := strings.ToLower(f[0]), f[5]
		// Unique-local fc00::/7 is scope "global" to the kernel but not
		// reachable from the internet -- Docker's own bridges get these.
		if strings.HasPrefix(addr, "fc") || strings.HasPrefix(addr, "fd") {
			continue
		}
		if iface == "lo" || strings.HasPrefix(iface, "docker") || strings.HasPrefix(iface, "br-") || strings.HasPrefix(iface, "veth") {
			continue
		}
		return true
	}
	return false
}

// Resolvers returns the host's DNS servers from /etc/resolv.conf, in
// nginx `resolver` syntax (IPv6 addresses in brackets). The proxy runs
// with network_mode: host, so the host's resolvers -- a local stub like
// systemd-resolved's 127.0.0.53 included -- are reachable from it.
func Resolvers() (string, error) {
	return parseResolvers(readFileOrEmpty("/etc/resolv.conf"))
}

func parseResolvers(resolvConf string) (string, error) {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(resolvConf))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(f[1])
		if ip == nil {
			continue // e.g. an IPv6 address with a %zone -- nginx can't use it
		}
		if ip.To4() != nil {
			out = append(out, ip.String())
		} else {
			out = append(out, "["+ip.String()+"]")
		}
	}
	if len(out) == 0 {
		return "", fmt.Errorf("no usable nameserver in /etc/resolv.conf -- the proxy needs one to reach Let's Encrypt")
	}
	return strings.Join(out, " "), nil
}

// PortInUse reports whether something on this host already listens on
// the given TCP port -- on any address, IPv4 or IPv6. Not a test bind:
// binding 80/443 needs root, which this CLI doesn't run as; and not
// `docker ps`: that can't see a natively installed web server.
//
// On Linux it reads the kernel's socket tables (/proc/net/tcp and tcp6),
// which also catch a server bound only to a specific public address or
// only to [::]. Elsewhere, or if those can't be read, it falls back to a
// connection attempt on 127.0.0.1.
func PortInUse(port int) bool {
	if listening(readFileOrEmpty("/proc/net/tcp"), port) || listening(readFileOrEmpty("/proc/net/tcp6"), port) {
		return true
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// listening parses a /proc/net/tcp{,6} table: is any socket in state 0A
// (LISTEN) bound to port? local_address is "<hex address>:<hex port>".
func listening(table string, port int) bool {
	want := fmt.Sprintf(":%04X", port)
	for _, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		if strings.HasSuffix(strings.ToUpper(f[1]), want) {
			return true
		}
	}
	return false
}

func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
