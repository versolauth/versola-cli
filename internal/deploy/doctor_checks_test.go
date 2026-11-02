package deploy

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/checks"
	"github.com/versolauth/versola-cli/internal/proxy"
)

// The ports `doctor --target vps` checks are the ones configure checks, by
// --proxy and the scheme of --auth-url.
func TestProxyPortsFor(t *testing.T) {
	for _, c := range []struct {
		url, mode string
		want      []int
	}{
		{"https://id.example.com", proxy.ModeNginx, []int{80, 443}},
		{"http://id.example.com", proxy.ModeNginx, []int{80}},
		{"https://id.example.com", proxy.ModeExternal, []int{proxy.ExternalPort}},
	} {
		auth, err := proxy.ParseAuthURL(c.url, c.mode)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyPortsFor(auth, c.mode, proxy.PublicNames{}); !slices.Equal(got, c.want) {
			t.Errorf("%s --proxy %s: %v, want %v", c.url, c.mode, got, c.want)
		}
	}
}

func TestPostgresCheckOf(t *testing.T) {
	asked := false
	free := func() checks.Result { asked = true; return checks.Result{Name: "Port 5432 free", OK: true} }

	// The container: port 5432 is checked, and the line says why.
	r := postgresCheckOf(PostgresModeBundled, BundledPostgresHost, nil, free)
	if !asked || !r.OK || !strings.Contains(r.Name, "5432") || !strings.Contains(r.Detail, "--postgres-host") {
		t.Errorf("bundled: asked=%v %+v", asked, r)
	}

	// A Postgres of one's own: no port check (a native Postgres on 5432 is
	// not a conflict then), only which one.
	asked = false
	r = postgresCheckOf("", "db.internal:5432", nil, free)
	if asked || !r.OK || !strings.Contains(r.Detail, "db.internal:5432") || !strings.Contains(r.Detail, "postgres init") {
		t.Errorf("own: asked=%v %+v", asked, r)
	}

	// What configure would refuse fails, in configure's words.
	taken := func() checks.Result {
		return checks.Result{Name: "Port 5432 free", Detail: "already in use -- stop it"}
	}
	if r := postgresCheckOf(PostgresModeBundled, BundledPostgresHost, nil, taken); r.OK || !strings.Contains(r.Detail, "already in use") {
		t.Errorf("bundled, port taken: %+v", r)
	}
	if r := postgresCheckOf("", "", errors.New("leave --postgres-host out"), free); r.OK || r.Detail != "leave --postgres-host out" {
		t.Errorf("refused: %+v", r)
	}
}

// The port check of configure and doctor: the deployment's own proxy holding
// a port is not a conflict, anything else is.
func TestPortConflict(t *testing.T) {
	taken := map[int]bool{80: true, 443: true}
	inUse := func(p int) bool { return taken[p] }
	if got := portConflict([]int{80, 443}, nil, inUse); got != 80 {
		t.Errorf("taken by something else: %d", got)
	}
	if got := portConflict([]int{80, 443}, map[int]bool{80: true, 443: true}, inUse); got != 0 {
		t.Errorf("held by our own proxy: %d", got)
	}
	if got := portConflict([]int{80, 443}, map[int]bool{80: true}, inUse); got != 443 {
		t.Errorf("one ours, one not: %d", got)
	}
	if got := portConflict([]int{proxy.ExternalPort}, nil, inUse); got != 0 {
		t.Errorf("free: %d", got)
	}
}
