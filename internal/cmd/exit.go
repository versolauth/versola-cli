package cmd

import (
	"fmt"
	"io"
	"sync"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/exitcode"
)

// Run executes the command line args (os.Args[1:] from main) and returns the
// process exit code, having printed the error, if any, exactly once on
// stderr. Called once from main.main.
//
// The root command sets SilenceErrors: cobra used to print "Error: ..." and
// then main printed the same error again, so every failure showed twice. The
// one print is here, without cobra's "Error:" prefix (the line main always
// printed). The code comes from internal/exitcode (see README.md, "Exit
// codes"); what cobra itself rejects is classified here, since cobra's errors
// carry no type of their own:
//
//   - a flag cobra can't parse (unknown flag, bad value, missing value):
//     through the root's FlagErrorFunc, which every subcommand inherits;
//   - a wrong number of arguments: every command's Args validator is wrapped
//     once, on the first Run, so a validator a later change adds to any
//     command is covered without anyone remembering to classify it;
//   - an unknown command: cobra reports it while looking the command up,
//     before any command runs, as a plain error. Looking the same args up
//     again (Find has no side effects) says whether that is where it came
//     from. cobra used to follow that one message with a hint; it is kept,
//     since it is the way out.
func Run(args []string, stderr io.Writer) int {
	prepareExit()
	rootCmd.SetArgs(args)
	called, err := rootCmd.ExecuteC()
	if err == nil {
		return exitcode.OK
	}
	_, _, findErr := rootCmd.Find(args)
	unknownCommand := findErr != nil && called != nil
	if unknownCommand {
		err = exitcode.With(exitcode.Usage, err)
	}
	if !exitcode.IsReported(err) {
		fmt.Fprintln(stderr, err)
		if unknownCommand {
			fmt.Fprintf(stderr, "Run '%v --help' for usage.\n", called.CommandPath())
		}
	}
	return exitcode.Code(err)
}

var prepareOnce sync.Once

// prepareExit installs the classification of cobra's own errors described on
// Run. Once per process: wrapping an Args validator twice would be harmless,
// but tests call Run many times and the tree only has to be walked once (all
// application commands are added in init functions, before any Run).
func prepareExit() {
	prepareOnce.Do(func() {
		// Cobra normally adds completion lazily in ExecuteC. Build that tree
		// before wrapping validators, so its NoArgs errors also exit 2 on
		// the first invocation (the only invocation of an installed CLI).
		rootCmd.InitDefaultCompletionCmd()
		rootCmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
			return exitcode.With(exitcode.Usage, err)
		})
		wrapArgs(rootCmd)
	})
}

func wrapArgs(c *cobra.Command) {
	// A nil Args stays nil: cobra then applies its own legacy check, which is
	// where "unknown command" comes from (classified in Run instead).
	if validate := c.Args; validate != nil {
		c.Args = func(cmd *cobra.Command, args []string) error {
			return exitcode.With(exitcode.Usage, validate(cmd, args))
		}
	}
	for _, sub := range c.Commands() {
		wrapArgs(sub)
	}
}
