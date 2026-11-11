package cmd

import (
	"reflect"
	"testing"
)

func TestDownPreservesLocalDatabaseContainerUnlessVolumesRequested(t *testing.T) {
	for _, tc := range []struct {
		target  string
		volumes bool
		want    []string
	}{
		{"local", false, []string{"stop"}},
		{"local", true, []string{"down", "--volumes"}},
		{"vps", false, []string{"down"}},
		{"vps", true, []string{"down", "--volumes"}},
	} {
		if got := downAction(tc.target, tc.volumes); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("target=%s volumes=%t: got %v, want %v", tc.target, tc.volumes, got, tc.want)
		}
	}
}
