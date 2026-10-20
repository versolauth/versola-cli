package deploy

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// callsOverStdin feeds input to os.Stdin through a real pipe (what a script or
// `printf 'y\ny\n' | versola ...` provides) and calls ConfirmVpsDeployYes n times
// in one process, returning each call's error.
func callsOverStdin(t *testing.T, input string, n int) []error {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	w.Close() // EOF after the input
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; r.Close() }()

	errs := make([]error, n)
	for i := range errs {
		errs[i] = ConfirmVpsDeployYes(false, "do a thing")
	}
	return errs
}

func TestConfirmVpsDeployStdin(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []bool // true = expected to be confirmed
	}{
		{"one y", "y\n", []bool{true}},
		{"two y", "y\ny\n", []bool{true, true}},
		{"three y", "y\ny\ny\n", []bool{true, true, true}},
		{"y then n", "y\nn\n", []bool{true, false}},
		{"eof", "", []bool{false}},
		{"eof second", "y\n", []bool{true, false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := callsOverStdin(t, c.input, len(c.want))
			for i, w := range c.want {
				if got := errs[i] == nil; got != w {
					t.Errorf("prompt %d: confirmed=%v, want %v (err=%v)", i+1, got, w, errs[i])
				}
			}
		})
	}
}

func TestConfirmVpsDeployYesSkipsStdin(t *testing.T) {
	// Closed stdin: any read would answer EOF -> "aborted".
	errs := callsOverStdin(t, "", 1)
	if errs[0] == nil {
		t.Fatal("precondition: empty stdin should abort")
	}
	if err := ConfirmVpsDeployYes(true, "do a thing"); err != nil {
		t.Fatalf("assumeYes: %v", err)
	}
}

func TestReadLine(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"y\nrest", "y"},
		{"y\r\n", "y"},
		{"no newline", "no newline"},
		{"y\r", "y"},
		{"", ""},
	} {
		got, _ := ReadLine(strings.NewReader(c.in))
		if got != c.want {
			t.Errorf("ReadLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A newline-less stream is cut, not read to EOF.
	long := strings.NewReader(strings.Repeat("x", 3*maxLineLen))
	if got, err := ReadLine(long); err == nil || len(got) != maxLineLen {
		t.Errorf("long line: len=%d err=%v, want len %d and an error", len(got), err, maxLineLen)
	}
	// The tail of an over-long line is dropped, not left for the next prompt.
	rest := strings.NewReader(strings.Repeat("x", 2*maxLineLen) + "\ny\n")
	ReadLine(rest)
	if got, _ := ReadLine(rest); got != "y" {
		t.Errorf("line after an over-long one = %q, want y", got)
	}
	// Does not consume past the newline.
	r := strings.NewReader("a\nb\n")
	ReadLine(r)
	if got, _ := ReadLine(r); got != "b" {
		t.Errorf("second line = %q, want b", got)
	}
}

// endless never delivers a newline.
type endless struct{ n int }

func (e *endless) Read(p []byte) (int, error) { e.n++; p[0] = 'x'; return 1, nil }

func TestReadLineEndlessStreamTerminates(t *testing.T) {
	e := &endless{}
	if _, err := ReadLine(e); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
	if e.n > maxLineLen+maxDrain+1 {
		t.Errorf("read %d bytes, want at most %d", e.n, maxLineLen+maxDrain+1)
	}
}

func TestConfirmTooLongLineSaysWhy(t *testing.T) {
	err := confirmVpsFrom(strings.NewReader(strings.Repeat("y", 2*maxLineLen)+"\n"), false, "do a thing")
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

func TestReadLineExactLimit(t *testing.T) {
	at := strings.Repeat("x", maxLineLen)
	if got, err := ReadLine(strings.NewReader(at + "\n")); err != nil || got != at {
		t.Errorf("line of exactly maxLineLen: len=%d err=%v", len(got), err)
	}
	if _, err := ReadLine(strings.NewReader(at + "x\n")); !errors.Is(err, ErrLineTooLong) {
		t.Errorf("maxLineLen+1: err = %v, want ErrLineTooLong", err)
	}
}
