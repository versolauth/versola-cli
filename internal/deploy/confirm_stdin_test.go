package deploy

import (
	"os"
	"strings"
	"testing"
)

// callsOverStdin feeds input to os.Stdin through a real pipe (what a script or
// `printf 'y\ny\n' | versola ...` provides) and calls ConfirmVpsDeploy n times
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
		errs[i] = ConfirmVpsDeploy("do a thing")
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
		{"y\r\n", "y\r"}, // callers TrimSpace
		{"no newline", "no newline"},
		{"", ""},
	} {
		got, _ := ReadLine(strings.NewReader(c.in))
		if got != c.want {
			t.Errorf("ReadLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Does not consume past the newline.
	r := strings.NewReader("a\nb\n")
	ReadLine(r)
	if got, _ := ReadLine(r); got != "b" {
		t.Errorf("second line = %q, want b", got)
	}
}
