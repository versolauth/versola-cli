// Package docker shells out to the docker CLI.
//
// It exists so that the several places that need to run docker (deploying
// a stack, stopping it, removing images) agree on one thing: docker's own
// output goes straight to the user's terminal, unbuffered. A `docker
// compose pull` of a few hundred megabytes takes a while, and a caller
// that captured that output instead of streaming it would look frozen for
// the entire time.
//
// Nothing here knows anything about Versola itself -- no service names, no
// ports, no image list. This package takes whatever arguments it's given
// and runs them. Which containers exist and how they're wired together
// comes from the versioned versola-tools image instead (see
// internal/deploy), which is what lets one build of this CLI deploy
// several different releases of Versola.
package docker

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Cmd builds a docker exec.Cmd with stdout already wired to the user's
// terminal. Stderr is deliberately left unset for the caller to fill in:
// Run below just points it at os.Stderr, but deploy's versola-tools
// runner also needs to inspect what was written to it, and the two can't
// share one default.
func Cmd(args ...string) *exec.Cmd {
	c := exec.Command("docker", args...)
	c.Stdout = os.Stdout
	return c
}

// Run runs docker with both output streams going to the user's terminal,
// and returns whatever error the process exited with.
func Run(args ...string) error {
	c := Cmd(args...)
	c.Stderr = os.Stderr
	return c.Run()
}

// IsRunning reports whether a container with this name exists and is
// currently running. A container that doesn't exist at all is reported as
// not running rather than as an error — every other place in this CLI
// that checks for something possibly not being there yet (state.Load,
// state.ComposeFile) treats "doesn't exist" as a plain negative, not a
// failure, and callers of this function want the same thing: "is it
// already up, or do I need to start it" doesn't care which kind of "no"
// it gets.
func IsRunning(name string) (bool, error) {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, fmt.Errorf("couldn't check whether %s is running: %w", name, err)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}
