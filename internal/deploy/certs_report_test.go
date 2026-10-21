package deploy

import (
	"bytes"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/topology"
)

const reportOutput = `mtls-init: step is slow today
mtls-init: cert server.crt notBefore=2026-10-01T00:00:00Z notAfter=2026-12-30T00:00:00Z subject=central
mtls-init: cert edge-client.crt notBefore=2026-10-20T00:00:00Z notAfter=2026-11-19T00:00:00Z subject=urn:versola:edge:edge-default
mtls-init: cert ca.crt notAfter=2036-01-01T00:00:00Z subject=Versola Client CA Intermediate CA
mtls-init: cert broken.crt notAfter=not-a-date subject=x
mtls-init: cert noname
`

func TestParseCertReport(t *testing.T) {
	got := parseCertReport([]byte(reportOutput))
	if len(got) != 3 {
		t.Fatalf("= %+v, want the three well-formed lines", got)
	}
	if got[0].Name != "server.crt" || got[0].Subject != "central" || got[0].NotAfter.Format("2006-01-02") != "2026-12-30" || got[0].NotBefore.Format("2006-01-02") != "2026-10-01" {
		t.Errorf("server = %+v", got[0])
	}
	if got[1].Subject != "urn:versola:edge:edge-default" {
		t.Errorf("edge = %+v", got[1])
	}
	// The subject is the rest of the line, spaces and all; no notBefore is not fatal.
	if got[2].Subject != "Versola Client CA Intermediate CA" || !got[2].NotBefore.IsZero() {
		t.Errorf("ca = %+v", got[2])
	}
}

func TestCertInfoDue(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for name, tc := range map[string]struct {
		before, after time.Time
		due           bool
	}{
		"90-day leaf, 60 days left":    {now.Add(-30 * day), now.Add(60 * day), false},
		"90-day leaf, exactly a third": {now.Add(-60 * day), now.Add(30 * day), true},
		"90-day leaf, 29 days left":    {now.Add(-61 * day), now.Add(29 * day), true},
		"30-day identity, 15 days":     {now.Add(-15 * day), now.Add(15 * day), false},
		"30-day identity, 9 days":      {now.Add(-21 * day), now.Add(9 * day), true},
		"no notBefore, 31 days":        {time.Time{}, now.Add(31 * day), false},
		"no notBefore, 29 days":        {time.Time{}, now.Add(29 * day), true},
	} {
		c := CertInfo{NotBefore: tc.before, NotAfter: tc.after}
		if got := c.Due(now); got != tc.due {
			t.Errorf("%s: Due = %v, want %v", name, got, tc.due)
		}
	}
}

func TestWriteCertReport(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	infos := parseCertReport([]byte(reportOutput))
	infos = append(infos,
		CertInfo{Name: "id.example.com:443", NotBefore: now.Add(-100 * 24 * time.Hour), NotAfter: now.Add(-24 * time.Hour)},
		CertInfo{Name: "id-mtls.example.com:443", Error: "connection refused"},
	)
	var out bytes.Buffer
	WriteCertReport(&out, infos, now)
	text := out.String()
	for _, w := range []string{
		"server.crt", "notAfter 2026-12-30 00:00Z", "58 days left",
		"edge-client.crt", "notAfter 2026-11-19 00:00Z", "17 days left", // 30-day identity, 17 left: not a third yet
		"id.example.com:443", "1 days ago  EXPIRED",
		"id-mtls.example.com:443", "not read: connection refused",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("report lacks %q:\n%s", w, text)
		}
	}
	// Soonest first, what could not be read last.
	if strings.Index(text, "id.example.com:443") > strings.Index(text, "edge-client.crt") || strings.Index(text, "edge-client.crt") > strings.Index(text, "server.crt") {
		t.Errorf("not soonest-first:\n%s", text)
	}
	if strings.Index(text, "not read") < strings.Index(text, "ca.crt") {
		t.Errorf("an unreadable one is not last:\n%s", text)
	}
	// A third or less left is said.
	var due bytes.Buffer
	WriteCertReport(&due, []CertInfo{{Name: "x", NotBefore: now.Add(-80 * 24 * time.Hour), NotAfter: now.Add(10 * 24 * time.Hour)}}, now)
	if !strings.Contains(due.String(), "due for renewal") {
		t.Errorf("%s", due.String())
	}
}

// A TLS server that wants a client certificate still shows its own first: the
// probe reads it without trusting it and without presenting anything.
func TestProbeCertificate(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	c := ProbeCertificate(addr, 3*time.Second)
	if c.Error != "" || c.NotAfter.IsZero() || c.NotBefore.IsZero() || !c.NotAfter.After(time.Now()) {
		t.Fatalf("= %+v", c)
	}
	if c.Name != addr {
		t.Errorf("name = %q", c.Name)
	}
	// Nothing listening: said, not hidden.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	if c := ProbeCertificate(closed, time.Second); c.Error == "" {
		t.Errorf("a closed port: %+v", c)
	}
	// Not TLS at all.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()
	if c := ProbeCertificate(strings.TrimPrefix(plain.URL, "http://"), time.Second); c.Error == "" {
		t.Errorf("plain http: %+v", c)
	}
	if c := ProbeCertificate("no-port", time.Second); c.Error == "" {
		t.Errorf("an address without a port: %+v", c)
	}
}

func TestPublicEndpoints(t *testing.T) {
	st := &state.State{Target: "vps", AuthURL: "https://id.example.com", ProxyMode: proxy.ModeNginx,
		CentralDomain: "central.example.com", MTLSDomain: "id-mtls.example.com", CentralMTLSDomain: "central-mtls.example.com"}
	got := PublicEndpoints(st)
	want := []string{"id.example.com:443", "central.example.com:443", "id-mtls.example.com:443", "central-mtls.example.com:443"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("= %v, want %v", got, want)
	}
	// Only the auth URL's when nothing else is published.
	st.CentralDomain, st.MTLSDomain, st.CentralMTLSDomain = "", "", ""
	if got := PublicEndpoints(st); len(got) != 1 || got[0] != "id.example.com:443" {
		t.Errorf("= %v", got)
	}
	// Behind an external proxy, on local, or without https: nothing here to look at.
	for name, mutate := range map[string]func(*state.State){
		"external": func(s *state.State) { s.ProxyMode = proxy.ModeExternal },
		"local":    func(s *state.State) { s.Target = "local" },
		"http":     func(s *state.State) { s.AuthURL = "http://id.example.com" },
	} {
		s := *st
		mutate(&s)
		if got := PublicEndpoints(&s); got != nil {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestToolsArgsForPublicNames(t *testing.T) {
	if got := (PublicDomains{}).toolsArgs(); got != nil {
		t.Errorf("no names: %v", got)
	}
	// The enrollment name is not one of the certificates' names: versola-tools gets the two that are.
	got := PublicDomains{MTLS: "id-mtls.example.com", CentralMTLS: "central-mtls.example.com", Enroll: "central.example.com"}.toolsArgs()
	want := []string{"-e", "MTLS_DOMAIN=id-mtls.example.com", "-e", "CENTRAL_MTLS_DOMAIN=central-mtls.example.com"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("= %v, want %v", got, want)
	}
	st := &state.State{MTLSDomain: "a.example.com", CentralMTLSDomain: "b.example.com", CentralDomain: "c.example.com"}
	if p := PublicDomainsOf(st); p != (PublicDomains{MTLS: "a.example.com", CentralMTLS: "b.example.com", Enroll: "c.example.com"}) {
		t.Errorf("= %+v", p)
	}
}

func TestPrintExternalPublicNames(t *testing.T) {
	var out bytes.Buffer
	printExternalPublicNames(&out, &state.State{})
	if out.Len() != 0 {
		t.Errorf("nothing published, yet: %q", out.String())
	}
	printExternalPublicNames(&out, &state.State{MTLSDomain: "id-mtls.example.com", CentralMTLSDomain: "central-mtls.example.com", CentralDomain: "central.example.com"})
	text := out.String()
	for _, w := range []string{"id-mtls.example.com and central-mtls.example.com", "id-mtls.example.com to 127.0.0.1:8083", "central-mtls.example.com to 127.0.0.1:8093", "central.example.com", "POST /edge/enroll", "127.0.0.1:8094"} {
		if !strings.Contains(text, w) {
			t.Errorf("lacks %q:\n%s", w, text)
		}
	}
}

// Taking a replica out of the proxy is a reload without it; putting it back is
// a reload with it. Central is not part of the rolling.
func TestCertsRouterDrainsAndRestores(t *testing.T) {
	ops := newFakeOps()
	mk := func(base string, slots ...int) []replica {
		var out []replica
		for _, n := range slots {
			out = append(out, replica{Slot: n, Base: base, Service: topology.SlotService(base, n), Ports: topology.Service{Port: 9000 + n}})
		}
		return out
	}
	r := &certsRouter{
		ops: ops, mode: proxy.ModeNginx, fronts: proxy.Fronts{},
		auth: mk("auth", 1, 2), edge: mk("edge", 1, 2),
	}
	if err := r.drain(certsStep{Service: "auth-2", Base: "auth", Slot: 2}); err != nil {
		t.Fatal(err)
	}
	if err := r.undrain(certsStep{Service: "auth-2", Base: "auth", Slot: 2}); err != nil {
		t.Fatal(err)
	}
	reloads := []string{}
	for _, e := range ops.events {
		if strings.HasPrefix(e, "reload ") {
			reloads = append(reloads, e)
		}
	}
	wantReloads := []string{"reload auth=auth edge=edge,edge-2", "reload auth=auth,auth-2 edge=edge,edge-2"}
	if len(reloads) != 2 || reloads[0] != wantReloads[0] || reloads[1] != wantReloads[1] {
		t.Fatalf("reloads = %v\nwant     %v", reloads, wantReloads)
	}
	// Drained: the requests it was serving are waited out before it restarts.
	if i, j := indexIn(ops.events, "drained"), indexIn(ops.events, reloads[1]); i < 0 || i > j {
		t.Errorf("not waited for the drain: %v", ops.events)
	}
	// A reload that fails puts the routing back and says so.
	ops.failOn[wantReloads[0]] = errors.New("nginx said no")
	if err := r.drain(certsStep{Service: "auth-2", Base: "auth", Slot: 2}); err == nil {
		t.Error("a failed reload was ignored")
	}
	if r.out["auth"][2] {
		t.Error("the replica is recorded as out after a failed drain")
	}
}

func indexIn(events []string, e string) int {
	for i, x := range events {
		if x == e {
			return i
		}
	}
	return -1
}

// What versola-tools' mtls-init really printed in its report phase (run in a
// throwaway smallstep/step-ca container against a fresh CA, with a public name):
// the lines this CLI parses are the ones it writes.
func TestParseCertReportOfARealRun(t *testing.T) {
	out, err := os.ReadFile("testdata/mtls-init-report.txt")
	if err != nil {
		t.Fatal(err)
	}
	infos := parseCertReport(out)
	if len(infos) != 12 {
		t.Fatalf("parsed %d of %d lines", len(infos), strings.Count(string(out), "\n"))
	}
	by := map[string]CertInfo{}
	for _, c := range infos {
		by[c.Name] = c
	}
	if c := by["edge-client.crt"]; c.Subject != "urn:versola:edge:edge-default" || c.NotAfter.Sub(c.NotBefore).Round(24*time.Hour) != 30*24*time.Hour {
		t.Errorf("edge-client = %+v", c)
	}
	if c := by["server.crt"]; c.NotAfter.Sub(c.NotBefore).Round(24*time.Hour) != 90*24*time.Hour {
		t.Errorf("server = %+v", c)
	}
	if c := by["server-ca.crt"]; c.Subject != "Versola Server CA" {
		t.Errorf("server-ca = %+v", c)
	}
}
