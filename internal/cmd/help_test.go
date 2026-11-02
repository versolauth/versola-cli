package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// Use a fresh process for each CLI invocation: cobra flags and command
// contexts deliberately live for the whole process, just as in production.
func TestHelpProcess(t *testing.T) {
	if os.Getenv("VERSOLA_TEST_HELP_PROCESS") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Args = append([]string{"versola"}, os.Args[i+1:]...)
			break
		}
	}
	os.Exit(Run(os.Args[1:], os.Stderr))
}

func cliHelp(t *testing.T, args ...string) string {
	t.Helper()
	c := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelpProcess$", "--"}, args...)...)
	c.Env = append(os.Environ(), "VERSOLA_TEST_HELP_PROCESS=1")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestHelpGroupsFollowCustomerPath(t *testing.T) {
	out := cliHelp(t, "--help")
	headings := []string{"Get started:", "Deploy step by step:", "Operate:", "Upgrade and scale:", "Advanced:"}
	prev := -1
	for _, heading := range headings {
		pos := strings.Index(out, heading)
		if pos <= prev {
			t.Fatalf("missing or unordered heading %q:\n%s", heading, out)
		}
		prev = pos
	}
	for _, c := range rootCmd.Commands() {
		if !c.Hidden && c.GroupID == "" {
			t.Errorf("command %q has no group", c.Name())
		}
	}
}

func TestHelpAllShowsAdvancedFlagsWithoutRunningCommand(t *testing.T) {
	for _, name := range []string{"configure", "bootstrap", "doctor", "rollout", "up"} {
		t.Run(name, func(t *testing.T) {
			out := cliHelp(t, name, "--help")
			if strings.Contains(out, advancedFlagsHeading) {
				t.Fatalf("default help includes advanced flags:\n%s", out)
			}
			if !strings.Contains(out, "More flags: versola "+name+" --help-all") {
				t.Fatalf("no advanced help hint:\n%s", out)
			}
			all := cliHelp(t, name, "--help-all")
			if !strings.Contains(all, advancedFlagsHeading) {
				t.Fatalf("advanced flags missing:\n%s", all)
			}
			if name == "configure" || name == "bootstrap" {
				for _, f := range []string{"--auth-url", "--proxy", "--postgres-host", "--yes"} {
					if !strings.Contains(out, f) {
						t.Errorf("default help missing %s", f)
					}
				}
				// Test the flags section separately: prose can mention an advanced flag.
				_, flags, _ := strings.Cut(out, "Flags:")
				if strings.Contains(flags, "--postgres-ca ") {
					t.Errorf("default flags include --postgres-ca")
				}
				if !strings.Contains(all, "--postgres-ca ") {
					t.Errorf("advanced flags omit --postgres-ca")
				}
			}
		})
	}
	// cobra's help subcommand must show configure, rather than its own help.
	out := cliHelp(t, "help", "configure", "--help-all=true")
	if !strings.Contains(out, "versola configure <target> [<version>]") || !strings.Contains(out, advancedFlagsHeading) {
		t.Fatalf("help subcommand printed the wrong page:\n%s", out)
	}
}

func TestRewriteHelpAll(t *testing.T) {
	for _, tt := range []struct {
		args, want []string
		all        bool
	}{
		{[]string{"configure", "--help-all"}, []string{"configure", "--help"}, true},
		{[]string{"configure", "--help-all=1"}, []string{"configure", "--help"}, true},
		{[]string{"help", "configure", "--help-all"}, []string{"help", "configure"}, true},
		{[]string{"configure", "--help-all=false"}, []string{"configure", "--help-all=false"}, false},
		{[]string{"configure", "--", "--help-all"}, []string{"configure", "--", "--help-all"}, false},
		{[]string{"__complete", "configure", "--help-all"}, []string{"__complete", "configure", "--help-all"}, false},
		{[]string{"__completeNoDesc", "configure", "--help-all"}, []string{"__completeNoDesc", "configure", "--help-all"}, false},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			old := helpAll
			helpAll = false
			defer func() { helpAll = old }()
			if got := rewriteHelpAll(tt.args); !reflect.DeepEqual(got, tt.want) || helpAll != tt.all {
				t.Errorf("got %v, helpAll=%v; want %v, %v", got, helpAll, tt.want, tt.all)
			}
		})
	}
}

func TestHiddenFlagsRemainHiddenAfterRendering(t *testing.T) {
	before := configureCmd.Flags().Lookup("postgres-ca")
	old := before.Hidden
	var out bytes.Buffer
	out.WriteString(hiddenFlags(configureCmd).FlagUsages())
	if !strings.Contains(out.String(), "--postgres-ca") || before.Hidden != old {
		t.Fatal("rendering advanced flags must not change normal help or completion")
	}
}
