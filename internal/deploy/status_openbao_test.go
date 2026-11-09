package deploy

import (
	"errors"
	"testing"
)

func TestOpenbaoStatusStrictInspect(t *testing.T) {
	for _, target := range []string{"local", "vps"} {
		c, err := openbaoContainerStatus(target, func(name, format string) (string, bool, error) {
			if name != OpenbaoContainerName(target) || format != containerInspectFormat {
				t.Fatalf("unexpected inspect: %q %q", name, format)
			}
			return `{"name":"/retained-openbao","service":"openbao","state":"running","health":"healthy"}`, true, nil
		})
		if err != nil || c == nil || c.Name != "retained-openbao" || c.Service != "openbao" || c.State != "running" {
			t.Fatalf("got %+v, %v", c, err)
		}
	}
}

func TestOpenbaoStatusMissingAndUnreadable(t *testing.T) {
	wantErr := errors.New("Docker unavailable")
	for _, tc := range []struct {
		found bool
		out   string
		err   error
	}{
		{false, "", nil},
		{false, "", wantErr},
		{true, "invalid JSON", nil},
	} {
		c, err := openbaoContainerStatus("vps", func(string, string) (string, bool, error) { return tc.out, tc.found, tc.err })
		if c != nil || (err != nil) != (tc.err != nil || tc.found) {
			t.Fatalf("got %+v, %v for %+v", c, err, tc)
		}
	}
}
