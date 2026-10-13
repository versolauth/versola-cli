//go:build windows

package docker

import (
	"os/exec"
	"syscall"
)

// shield starts c in a new process group, which a Ctrl-C in the console
// does not reach.
func shield(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}
