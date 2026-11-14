package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/deploy"
)

func TestEdgePartnerIsWired(t *testing.T) {
	var partner = edgePartnerCmd
	found := false
	for _, c := range edgeCmd.Commands() {
		found = found || c == partner
	}
	if !found {
		t.Fatal("`edge partner` is not under `edge`")
	}
	for _, name := range []string{"version", "edge-url", "central-pin", "auth-pin", "admin", "postgres-url", "postgres-user", "postgres-password-file", "console-dir", "dir", "no-start"} {
		if partner.Flags().Lookup(name) == nil {
			t.Errorf("edge partner has no --%s", name)
		}
	}
	// Repeatable, taken as given (StringArray: an email with a comma is one value).
	for _, name := range []string{"central-pin", "auth-pin", "admin"} {
		if f := partner.Flags().Lookup(name); f == nil || f.Value.Type() != "stringArray" {
			t.Errorf("--%s is not repeatable: %+v", name, f)
		}
	}
	if f := partner.Flags().Lookup("postgres-user"); f == nil || f.DefValue != "edge" {
		t.Errorf("--postgres-user = %+v", f)
	}
	if err := partner.Args(partner, []string{"a", "b"}); err == nil {
		t.Error("two token files")
	}
	if err := partner.Args(partner, nil); err != nil {
		t.Errorf("a re-run without the token file: %v", err)
	}
	// The help says what must not be lost, and `edge`'s help names the command.
	if !strings.Contains(partner.Long, "a token cannot be used twice") || !strings.Contains(partner.Long, "identity volume") {
		t.Errorf("edge partner's help on the identity volume:\n%s", partner.Long)
	}
	if !strings.Contains(edgeCmd.Long, `"edge partner"`) || !strings.Contains(edgeBootstrapCmd.Long, "versola edge partner") {
		t.Error("`edge` and `edge bootstrap` do not point a partner at `edge partner`")
	}
}

func TestEdgePartnerReports(t *testing.T) {
	base := deploy.EdgePartnerResult{Dir: "/p/edge", Project: "versola-partner-edge-edge", EnrollAddress: "https://enroll.example.com"}
	for _, tc := range []struct {
		name         string
		res          func() deploy.EdgePartnerResult
		stdout, errs []string
	}{
		{"started", func() deploy.EdgePartnerResult { r := base; r.Started = true; return r },
			[]string{"The edge is starting."}, []string{"enrolls at https://enroll.example.com", "logs -f edge", "https://login.example.com/partner/", "versola-partner-edge-edge_edge-identity"}},
		{"not started", func() deploy.EdgePartnerResult { return base },
			nil, []string{"Not started (--no-start)", "up -d", "/partner/"}},
		{"missing the password", func() deploy.EdgePartnerResult {
			r := base
			r.Missing = []string{"POSTGRES_PASSWORD (...)"}
			r.Warnings = []string{"no versola-partner.js"}
			return r
		}, nil, []string{"Warning: no versola-partner.js", "still lacks", "POSTGRES_PASSWORD", "was not started", "same command again"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := edgePartner(&stdout, &stderr, deploy.EdgePartnerOptions{EdgeURL: "https://login.example.com/"}, func(deploy.EdgePartnerOptions) (deploy.EdgePartnerResult, error) {
				return tc.res(), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.stdout {
				if !strings.Contains(stdout.String(), w) {
					t.Errorf("stdout lacks %q:\n%s", w, stdout.String())
				}
			}
			for _, w := range tc.errs {
				if !strings.Contains(stderr.String(), w) {
					t.Errorf("stderr lacks %q:\n%s", w, stderr.String())
				}
			}
		})
	}
	// Without a console the next step says /partner/ answers 404.
	var stderr bytes.Buffer
	if err := edgePartner(&bytes.Buffer{}, &stderr, deploy.EdgePartnerOptions{EdgeURL: "https://login.example.com"}, func(deploy.EdgePartnerOptions) (deploy.EdgePartnerResult, error) {
		return base, nil
	}); err != nil || !strings.Contains(stderr.String(), "no --console-dir: /partner/ answers 404") {
		t.Errorf("without a console: %v\n%s", err, stderr.String())
	}
	stderr.Reset()
	if err := edgePartner(&bytes.Buffer{}, &stderr, deploy.EdgePartnerOptions{EdgeURL: "https://login.example.com", ConsoleDir: "c"}, func(deploy.EdgePartnerOptions) (deploy.EdgePartnerResult, error) {
		return base, nil
	}); err != nil || strings.Contains(stderr.String(), "answers 404") {
		t.Errorf("with a console: %v\n%s", err, stderr.String())
	}
	want := errors.New("boom")
	if err := edgePartner(&bytes.Buffer{}, &bytes.Buffer{}, deploy.EdgePartnerOptions{}, func(deploy.EdgePartnerOptions) (deploy.EdgePartnerResult, error) {
		return deploy.EdgePartnerResult{}, want
	}); !errors.Is(err, want) {
		t.Errorf("err = %v", err)
	}
}
