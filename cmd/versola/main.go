// Command versola is the entry point for the Versola CLI binary.
package main

import (
	"os"

	"github.com/versolauth/versola-cli/internal/cmd"
)

// The exit code says why versola failed (see README.md, "Exit codes"); cmd.Run
// has already printed the error, once, on stderr.
func main() {
	os.Exit(cmd.Run(os.Args[1:], os.Stderr))
}
