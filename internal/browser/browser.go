// Package browser opens a URL in the user's default browser. It's a thin
// OS-specific wrapper -- there's no cross-platform standard library way to
// do this, each OS has its own "open whatever handles this" command.
package browser

import (
	"fmt"
	"os/exec"
	"runtime"
)

// Open launches the user's default browser at url. Failure here is never
// fatal to whatever called it — bootstrap succeeding shouldn't be
// conditional on a desktop environment being present to pop a window in
// (e.g. running over SSH), so callers should log and continue, not
// propagate this as a command failure.
func Open(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// Deliberately not "cmd /c start" -- cmd.exe re-parses whatever
		// command line Go hands it, and Go only quotes an argument if it
		// contains a space, so a URL with "&" (a cmd.exe command
		// separator, and a very ordinary character in query strings)
		// would silently get cut in half. rundll32's URL handler takes
		// the URL as a single argument with no shell in between, so
		// there's no re-parsing step for "&" or anything else to hit.
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		c = exec.Command("open", url)
	case "linux":
		c = exec.Command("xdg-open", url)
	default:
		return fmt.Errorf("don't know how to open a browser on %s", runtime.GOOS)
	}
	if err := c.Start(); err != nil {
		return err
	}
	// Start() without a matching Wait() leaves an unreaped child on Unix
	// until this process exits. That's harmless in practice here -- both
	// callers open a browser as their last action before returning -- but
	// reaping it properly costs nothing and doesn't make Open blocking.
	go func() { _ = c.Wait() }()
	return nil
}
