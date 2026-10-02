package checks

import "testing"

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
