package deploy

import (
	"strings"
	"testing"
)

func TestToolsArgsForEst(t *testing.T) {
	// --est reaches versola-tools as EST_ENABLED and the client CA alone; nothing when it is off.
	if got := estArgs(false); got != nil {
		t.Errorf("off = %v", got)
	}
	want := "-e EST_ENABLED=true -e EST_CA_CERTIFICATES=/app/mtls/client-ca.crt"
	if got := strings.Join(estArgs(true), " "); got != want {
		t.Errorf("on = %q, want %q", got, want)
	}
}
