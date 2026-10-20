package deploy

import (
	"reflect"
	"testing"
)

// gen-env gets central's URL as its own flag, and nothing when there is none
// (its default stays).
func TestCentralGenEnvArgs(t *testing.T) {
	if got := centralGenEnvArgs("https://central.example.com"); !reflect.DeepEqual(got, []string{"--central-mutual-tls-url=https://central.example.com"}) {
		t.Errorf("got %v", got)
	}
	if got := centralGenEnvArgs(""); got != nil {
		t.Errorf("got %v", got)
	}
}
