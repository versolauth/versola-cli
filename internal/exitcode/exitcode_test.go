package exitcode

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestCode(t *testing.T) {
	base := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, OK},
		{"unclassified", base, Failure},
		{"unclassified wrapped", fmt.Errorf("context: %w", base), Failure},
		{"usage", With(Usage, base), Usage},
		{"prerequisites", With(Prerequisites, base), Prerequisites},
		{"aborted", With(Aborted, base), Aborted},
		{"plan problems", With(PlanProblems, base), PlanProblems},
		{"interrupted", With(Interrupted, base), Interrupted},
		// rollout wraps configure's secrets-plan error with %w: the class
		// survives any number of wrappings.
		{"wrapped once", fmt.Errorf("rollout: %w", With(PlanProblems, base)), PlanProblems},
		{"wrapped twice", fmt.Errorf("a: %w", fmt.Errorf("b: %w", With(Aborted, base))), Aborted},
		// %v drops the chain, and with it the class.
		{"formatted with %v", fmt.Errorf("a: %v", With(Aborted, base)), Failure},
		{"outermost wins", With(Usage, With(Aborted, base)), Usage},
		{"reported", Reported(Aborted, base), Aborted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Code(c.err); got != c.want {
				t.Errorf("Code = %d, want %d", got, c.want)
			}
		})
	}
}

func TestWithKeepsTextAndChain(t *testing.T) {
	if With(Usage, nil) != nil || Reported(Aborted, nil) != nil {
		t.Fatal("a nil error must stay nil")
	}
	err := With(Usage, fmt.Errorf("--x: %w", io.EOF))
	if err.Error() != "--x: EOF" {
		t.Errorf("text changed: %q", err.Error())
	}
	if !errors.Is(err, io.EOF) {
		t.Error("errors.Is no longer sees the wrapped error")
	}
}

func TestIsReportedOnlyOutermost(t *testing.T) {
	base := errors.New("aborted")
	if IsReported(base) || IsReported(With(Aborted, base)) {
		t.Error("an ordinary error is reported")
	}
	if !IsReported(Reported(Aborted, base)) {
		t.Error("Reported is not reported")
	}
	// Wrapped with more context it must be printed, or the context is lost.
	if IsReported(fmt.Errorf("uninstall: %w", Reported(Aborted, base))) {
		t.Error("a wrapped Reported error is not printed")
	}
}
