package proxy

import "testing"

func TestHasGlobalIPv6(t *testing.T) {
	loOnly := "00000000000000000000000000000001 01 80 10 80       lo\n"
	linkLocal := loOnly + "fe80000000000000021e67fffe5a8b3c 02 40 20 80   enp3s0\n"
	global := linkLocal + "2a005da0100000010000000000030ee0 02 40 00 80   enp3s0\n"
	ula := linkLocal + "fd00000000000000000000000000abcd 02 40 00 80   enp3s0\n"
	dockerOnly := linkLocal + "2001db80000000000000000000000001 05 40 00 80   docker0\n"

	for _, c := range []struct {
		in   string
		want bool
	}{{"", false}, {loOnly, false}, {linkLocal, false}, {global, true}, {ula, false}, {dockerOnly, false}} {
		if got := hasGlobalIPv6(c.in); got != c.want {
			t.Errorf("hasGlobalIPv6(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseResolvers(t *testing.T) {
	got, err := parseResolvers("# generated\nnameserver 127.0.0.53\noptions edns0\nnameserver 2001:4860:4860::8888\nnameserver fe80::1%eth0\nsearch example.com\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "127.0.0.53 [2001:4860:4860::8888]"; got != want {
		t.Errorf("parseResolvers = %q, want %q", got, want)
	}
	for _, bad := range []string{"", "search example.com\n", "nameserver not-an-ip\n", "nameserver 1.2.3.4;evil\n"} {
		if _, err := parseResolvers(bad); err == nil {
			t.Errorf("parseResolvers(%q): want error", bad)
		}
	}
}

func TestListening(t *testing.T) {
	// Real /proc/net/tcp layout: header, then sl local rem st ...
	table := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1
   2: 0100007F:01BB 0100007F:D3C2 01 00000000:00000000 00:00000000 00000000     0        0 3 1
`
	for _, c := range []struct {
		port int
		want bool
	}{{80, true}, {8080, true}, {443, false}, {2821, false}} {
		if got := listening(table, c.port); got != c.want {
			t.Errorf("listening(:%d) = %v, want %v", c.port, got, c.want)
		}
	}
	tcp6 := `  sl  local_address                         remote_address                        st
   0: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000
`
	if !listening(tcp6, 443) {
		t.Error("listening(tcp6, :443) = false, want true")
	}
	if listening("", 80) {
		t.Error("listening(empty) = true")
	}
}
