// Package exitcode attaches a process exit code to an error, so that the one
// place that ends the process (cmd/versola's main, through cmd.Run) can tell
// a pipeline *why* versola failed without parsing its message: wrong
// arguments, a machine that isn't ready, a "no" at a confirmation prompt, a
// secrets plan with problems, or a Ctrl-C.
//
// It sits below every other internal package (it imports nothing of this
// module), so the code that knows the reason -- internal/deploy's prompt,
// internal/cmd's flag checks -- can classify the error where it is created,
// and every caller that wraps it with %w keeps the class. The error's text is
// never changed: scripts and the documentation quote some messages, and the
// code is extra information, not a replacement for them.
//
// The table is the one documented in README.md ("Exit codes"); a new class
// means a new row there too.
package exitcode

import "errors"

// The exit codes versola uses. Failure is what any error without a class
// gets.
const (
	OK            = 0
	Failure       = 1
	Usage         = 2   // wrong arguments or flags, an unknown target, a flag that doesn't apply to the target
	Prerequisites = 3   // the machine isn't ready: `doctor` found problems, configure's prerequisite check failed
	Aborted       = 4   // the answer at a confirmation prompt was not yes
	PlanProblems  = 5   // the secrets plan has problems, so nothing was (or would be) written
	Interrupted   = 130 // stopped by Ctrl-C (128 + SIGINT, the shell's convention)
)

// codedError carries the code next to the error it classifies. Error and
// Unwrap pass straight through, so the text a user sees and every errors.Is /
// errors.As on the wrapped error behave as if the wrapper were not there.
type codedError struct {
	code     int
	err      error
	reported bool
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// With returns err classified as code; nil stays nil, so a call can wrap a
// function's result directly (`return exitcode.With(exitcode.Usage, f())`).
func With(code int, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

// Reported is With for an error whose message the command has already shown
// in its own words (uninstall's "Aborted." on stdout): the process exits with
// code, and Run prints nothing more. Only the outermost error counts (see
// IsReported): wrapped again with more context, it is printed as usual,
// since the context would otherwise be lost.
func Reported(code int, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err, reported: true}
}

// IsReported says whether err itself (not something it wraps) came from
// Reported.
func IsReported(err error) bool {
	c, ok := err.(*codedError)
	return ok && c.reported
}

// Code is the exit code for err: OK for nil, the code of the outermost
// classified error in its chain, Failure when nothing in the chain is
// classified. The outermost wins so that a caller can reclassify an error it
// knows more about; in practice each error is classified once, where it is
// created.
func Code(err error) int {
	if err == nil {
		return OK
	}
	var c *codedError
	if errors.As(err, &c) {
		return c.code
	}
	return Failure
}
