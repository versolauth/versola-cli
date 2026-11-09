package checks

import (
	"strings"
	"testing"
)

func TestPortOwner(t *testing.T) {
	ps := "versola-postgres\t5432/tcp\n" +
		"versola-auth\t0.0.0.0:8080-8081->8080-8081/tcp, [::]:8080-8081->8080-8081/tcp\n" +
		"versola-proxy-local\t127.0.0.1:2821->2821/tcp\n" +
		"versola-nginx-old\t0.0.0.0:2898->80/tcp, [::]:2898->80/tcp\n" +
		"mixed\t127.0.0.1:3000->3000/tcp, 0.0.0.0:3000->3000/tcp\n" +
		"lan\t192.168.1.10:2830->80/tcp\n" +
		"v6loop\t[::1]:2831->80/tcp\n" +
		"v6any\t[::]:2832->80/tcp\n" +
		"lan-and-loop\t192.168.1.10:2833->80/tcp, 127.0.0.1:2833->80/tcp\n"
	for _, c := range []struct {
		port         int
		owner        string
		used         bool
		loopbackOnly bool
	}{
		{2821, "versola-proxy-local", true, true},
		{8081, "versola-auth", true, false},
		{8080, "versola-auth", true, false},
		{2898, "versola-nginx-old", true, false},
		// One all-interfaces publish is enough to leave 127.0.0.1 free.
		{3000, "mixed", true, false},
		// Another specific address doesn't take 127.0.0.1: Docker binds both.
		{2830, "", false, false},
		{2831, "", false, false},
		// Docker binds [::] IPv6-only: it doesn't take 127.0.0.1.
		{2832, "", false, false},
		{2833, "lan-and-loop", true, true},
		// Exposed but not published.
		{5432, "", false, false},
		{9999, "", false, false},
	} {
		owner, used, loopbackOnly := portOwner(ps, c.port)
		if owner != c.owner || used != c.used || loopbackOnly != c.loopbackOnly {
			t.Errorf("port %d: got (%q, %v, %v), want (%q, %v, %v)", c.port, owner, used, loopbackOnly, c.owner, c.used, c.loopbackOnly)
		}
	}
}

func TestVpsMemoryFloor(t *testing.T) {
	const mib = 1024 * 1024
	tests := []struct {
		name                                  string
		replace, openbao, bundled, postgresUp bool
		want                                  int64
	}{
		{"fresh, bundled, nothing running", false, false, true, false, 3*1024*mib + 256*mib + 256*mib},
		{"fresh, own Postgres", false, false, false, false, 3*1024*mib + 256*mib},
		{"fresh, bundled, openbao up", false, true, true, false, 3*1024*mib + 256*mib},
		{"replace, everything running", true, true, true, true, 1024 * mib},
		{"replace, bundled Postgres not running", true, true, true, false, 1024*mib + 256*mib},
		{"replace, own Postgres", true, true, false, false, 1024 * mib},
		{"a running Postgres is not counted when it is not ours", false, true, false, true, 3 * 1024 * mib},
	}
	for _, tc := range tests {
		got, why := VpsMemoryFloor(tc.replace, tc.openbao, tc.bundled, tc.postgresUp)
		if got != tc.want {
			t.Errorf("%s: floor %d MiB, want %d MiB", tc.name, got/mib, tc.want/mib)
		}
		if strings.Contains(why, "Postgres container") != (tc.bundled && !tc.postgresUp) {
			t.Errorf("%s: description %q", tc.name, why)
		}
	}
}
