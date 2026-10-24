package checks

import (
	"strings"
	"testing"
)

func TestParseLeafStatus(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		ok, warn  bool
		want      string
	}{
		{"all valid", "server ok\ncentral-server ok\nedge-client ok\n", true, false, "valid"},
		{"one due warns", "server ok\ncentral-server due\nedge-client ok\n", true, true, "central-server"},
		{"expired fails, even beside a due one", "server expired\ncentral-server due\nedge-client ok\n", false, false, "expired: server"},
		{"missing fails", "server ok\ncentral-server missing\nedge-client ok\n", false, false, "central-server"},
		{"unreadable fails", "server error\ncentral-server ok\nedge-client ok\n", false, false, "server"},
		{"nothing printed is not a pass", "", false, false, "couldn't tell"},
		{"fewer leaves than expected is not a pass", "server ok\n", false, false, "couldn't tell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ParseLeafStatus(tc.out)
			if r.OK != tc.ok || r.Warn != tc.warn || !strings.Contains(r.Detail, tc.want) {
				t.Errorf("= %+v", r)
			}
		})
	}
}

func TestResultString(t *testing.T) {
	if got := (Result{Name: "x", OK: true, Warn: true}).String(); !strings.HasPrefix(got, "[warn]") {
		t.Error(got)
	}
}
