package docker

import "testing"

func TestIsNoSuchContainer(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{"Error: No such object: versola-openbao-vps\n", true},
		{"Error: No such container: versola-openbao-vps\n", true},
		{"Error response from daemon: no such container: versola-openbao-vps", true},
		{"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n", false},
		{"permission denied while trying to connect to the Docker daemon socket", false},
		{"", false},
	} {
		if got := isNoSuchContainer(tc.stderr); got != tc.want {
			t.Errorf("isNoSuchContainer(%q) = %v, want %v", tc.stderr, got, tc.want)
		}
	}
}
