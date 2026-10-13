//go:build !windows

package docker

import (
	"os/exec"
	"syscall"
)

// shield starts c in a new process group, out of the terminal's foreground
// group, so a Ctrl-C there does not reach it. Its stdin stays /dev/null
// (exec.Cmd's default for a nil Stdin): a background process group that
// read the terminal would be stopped.
func shield(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
}
