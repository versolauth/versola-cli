// Command versola is the entry point for the Versola CLI binary.
package main

import (
	"fmt"
	"os"

	"github.com/versolauth/versola-cli/internal/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
