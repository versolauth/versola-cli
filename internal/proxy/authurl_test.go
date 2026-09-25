package proxy

import (
	"strings"
	"testing"
)

func TestParseAuthURLAccepts(t *testing.T) {
	cases := []struct {
		raw, mode       string
		url, host, port string
		tls             bool
	}{
		{"https://id.example.com", ModeNginx, "https://id.example.com", "id.example.com", "", true},
		{"https://id.example.com/", ModeNginx, "https://id.example.com", "id.example.com", "", true},
		{"HTTPS://ID.Example.COM/", ModeNginx, "https://id.example.com", "id.example.com", "", true},
		{"http://1.2.3.4", ModeNginx, "http://1.2.3.4", "1.2.3.4", "", false},
		{"https://id.example.com", ModeExternal, "https://id.example.com", "id.example.com", "", false},
		{"https://id.example.com:8443", ModeExternal, "https://id.example.com:8443", "id.example.com", "8443", false},
		{"http://id.example.com:8080", ModeExternal, "http://id.example.com:8080", "id.example.com", "8080", false},
		{"https://1.2.3.4", ModeExternal, "https://1.2.3.4", "1.2.3.4", "", false},
		{"https://x.com:65535", ModeExternal, "https://x.com:65535", "x.com", "65535", false},
		{"https://my-host.example.com", ModeNginx, "https://my-host.example.com", "my-host.example.com", "", true},
		{"http://localhost", ModeNginx, "http://localhost", "localhost", "", false},
		{"https://intranet", ModeExternal, "https://intranet", "intranet", "", false},
		{"https://" + strings.Repeat("a", 63) + ".com", ModeNginx, "https://" + strings.Repeat("a", 63) + ".com", strings.Repeat("a", 63) + ".com", "", true},
		// exactly 253 characters: the longest valid name
		{"https://" + strings.Repeat("abcdefghi.", 25) + "com", ModeNginx, "https://" + strings.Repeat("abcdefghi.", 25) + "com", strings.Repeat("abcdefghi.", 25) + "com", "", true},
	}
	for _, c := range cases {
		a, err := ParseAuthURL(c.raw, c.mode)
		if err != nil {
			t.Errorf("ParseAuthURL(%q, %s): unexpected error: %v", c.raw, c.mode, err)
			continue
		}
		if a.URL != c.url || a.Host != c.host || a.Port != c.port || a.TLS(c.mode) != c.tls {
			t.Errorf("ParseAuthURL(%q, %s) = %+v tls=%v, want url=%s host=%s port=%s tls=%v",
				c.raw, c.mode, a, a.TLS(c.mode), c.url, c.host, c.port, c.tls)
		}
	}
}

func TestParseAuthURLRejects(t *testing.T) {
	both := []struct{ raw, want string }{
		{"https://id.example.com/prefix", "no path"},
		{"https://id.example.com//", "no path"},
		{"https://id.example.com?x=1", "no path"},
		{"https://id.example.com#f", "no path"},
		{"https://u@id.example.com", "no path"},
		{"https://a b.com", "no path"},
		{"https://a.com;x", "no path"},
		{"https://[::1]", "no path"},
		{"https://a..com", "not a valid host"},
		{"https://.a.com", "not a valid host"},
		{"https://-a.com", "not a valid host"},
		{"https://a-.com", "not a valid host"},
		{"https://" + strings.Repeat("a", 64) + ".com", "longer than 63"},
		{"https://" + strings.Repeat("abcdefghi.", 26) + "com", "longer than 253"}, // 263 chars
		{"id.example.com", "must be http(s)://"},
		{"https", "must be http(s)://"},
		{"https://", "must be http(s)://"},
		{"", "must be http(s)://"},
		{"ftp://a.com", "scheme must be"},
		{"https://a.com:443", "default port"},
		{"http://a.com:80", "default port"},
		{"https://a.com:0443", "must not start with 0"},
		{"https://a.com:0", "must not start with 0"},
		{"https://a.com:65536", "1-65535"},
		{"https://a.com:99999999999999999999", "1-65535"},
		{"https://a.com:8x", "digits only"},
		{"https://a.com:", "digits only"},
		{"https://a.com:80:90", "digits only"},
	}
	for _, mode := range []string{ModeNginx, ModeExternal} {
		for _, c := range both {
			_, err := ParseAuthURL(c.raw, mode)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("ParseAuthURL(%q, %s): want error containing %q, got %v", c.raw, mode, c.want, err)
			}
		}
	}

	nginxOnly := []struct{ raw, want string }{
		{"http://a.com:8080", "serves on 80/443"},
		{"https://a.com:8443", "serves on 80/443"},
		{"https://1.2.3.4", "IP addresses"},
		{"https://localhost", "public domain"},
		{"https://auth.localhost", "public domain"},
		{"https://intranet", "public domain"},
	}
	for _, c := range nginxOnly {
		_, err := ParseAuthURL(c.raw, ModeNginx)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseAuthURL(%q, nginx): want error containing %q, got %v", c.raw, c.want, err)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, ok := range []string{ModeNginx, ModeExternal} {
		if _, err := ParseMode(ok); err != nil {
			t.Errorf("ParseMode(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "NGINX", "caddy", "none"} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q): want error", bad)
		}
	}
}

func TestPorts(t *testing.T) {
	for _, c := range []struct {
		raw, mode string
		want      []int
	}{
		{"https://id.example.com", ModeNginx, []int{80, 443}},
		{"http://1.2.3.4", ModeNginx, []int{80}},
		{"https://id.example.com", ModeExternal, []int{ExternalPort}},
	} {
		a := mustParse(t, c.raw, c.mode)
		got := Ports(a, c.mode)
		if len(got) != len(c.want) {
			t.Errorf("Ports(%s, %s) = %v, want %v", c.raw, c.mode, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("Ports(%s, %s) = %v, want %v", c.raw, c.mode, got, c.want)
			}
		}
	}
}

func mustParse(t *testing.T, raw, mode string) AuthURL {
	t.Helper()
	a, err := ParseAuthURL(raw, mode)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
