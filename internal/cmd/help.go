package cmd

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// `--help` shows what a first deployment needs; the flags for remote edges,
// test registries and recovery are hidden (cobra's MarkHidden) and keep
// working exactly as before. `--help-all` shows them too, under a heading of
// their own, so they stay one flag away instead of only in the README.
//
// cobra itself only short-circuits on `--help`: it checks that flag right
// after parsing and before the command's Args validator. `--help-all` is an
// ordinary flag to it, so `versola configure --help-all` would fail with
// "accepts 2 arg(s), received 0" before any help is printed. rewriteHelpAll
// therefore turns it into `--help` before cobra sees the arguments, and
// records in helpAll that the hidden flags are wanted.

// helpAll is true when the hidden flags are to be printed too. Set by
// rewriteHelpAll (the usual way) or by the --help-all flag itself, when it
// reaches cobra unrewritten (e.g. `--help-all=1`).
var helpAll bool

const helpAllFlag = "help-all"

// advancedFlagsHeading introduces the hidden flags in `--help-all` output.
const advancedFlagsHeading = "Advanced flags (remote edges, test registries, recovery):"

// setupHelp registers --help-all and the usage template every command
// inherits from root.
func setupHelp(root *cobra.Command) {
	root.PersistentFlags().BoolVar(&helpAll, helpAllFlag, false, "help for a command, with its advanced flags")
	cobra.AddTemplateFunc("helpAll", func() bool { return helpAll })
	cobra.AddTemplateFunc("hasHiddenFlags", func(c *cobra.Command) bool { return hiddenFlags(c).HasFlags() })
	cobra.AddTemplateFunc("hiddenFlagUsages", func(c *cobra.Command) string { return hiddenFlags(c).FlagUsages() })
	root.SetUsageTemplate(usageTemplate)
}

// rewriteHelpAll returns args with `--help-all` replaced by `--help`, and sets
// helpAll if it was there. Only arguments before a `--` are looked at: what
// follows it is not a flag. Two exceptions:
//
//   - `versola help <cmd> --help-all`: the help command given `--help` would
//     print its own help instead of <cmd>'s, so `--help-all` is only dropped;
//     the help command then prints <cmd>'s help, which reads helpAll.
//   - shell completion (`versola __complete ...`) is left alone: it completes
//     the words typed, and turning one into `--help` would complete the wrong
//     thing.
func rewriteHelpAll(args []string) []string {
	if len(args) > 0 && (args[0] == cobra.ShellCompRequestCmd || args[0] == cobra.ShellCompNoDescRequestCmd) {
		return args
	}
	out := make([]string, 0, len(args)+1)
	replaced := false
	for i, a := range args {
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if !isHelpAllArg(a) {
			out = append(out, a)
			continue
		}
		if !replaced {
			out = append(out, "--help")
			replaced = true
		}
	}
	if !replaced {
		return args
	}
	helpAll = true
	if firstWord(out) == "help" {
		kept := out[:0]
		for _, a := range out {
			if a != "--help" {
				kept = append(kept, a)
			}
		}
		return kept
	}
	return out
}

// isHelpAllArg: `--help-all`, or `--help-all=<true>`. `--help-all=false` is
// left for the flag parser (it changes nothing), and so is a value that is not
// a bool, which the parser then reports.
func isHelpAllArg(a string) bool {
	if a == "--"+helpAllFlag {
		return true
	}
	v, ok := strings.CutPrefix(a, "--"+helpAllFlag+"=")
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// firstWord is the first argument that is not a flag, or "".
func firstWord(args []string) string {
	for _, a := range args {
		if a == "--" {
			return ""
		}
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// hideFlags hides the named flags of c from `--help` (they stay in
// `--help-all` and keep working). A name that is not a flag of c panics at
// start-up rather than leaving a renamed flag silently visible.
func hideFlags(c *cobra.Command, names ...string) {
	for _, n := range names {
		if err := c.Flags().MarkHidden(n); err != nil {
			panic(err)
		}
	}
}

// advancedDeployFlags are the flags configure and bootstrap hide: remote
// edges and FAPI clients (the public names, --central-url, --est, the
// certificate lifetime and timer), test registries and staging certificates,
// a Postgres elsewhere (--postgres-db, --postgres-ca), and recovery
// (--generate-missing, --allow-legacy-secrets, --setup-openbao). A first
// deployment on one server needs none of them.
var advancedDeployFlags = append([]string{
	"est", "central-url", "no-certs-timer", "registry", "acme-staging",
	"postgres-db", "postgres-ca", "generate-missing", "allow-legacy-secrets", "setup-openbao",
}, publicFlagNames...)

// hiddenFlags is a flag set of c's hidden flags (its own and inherited), with
// Hidden cleared on the copies so that FlagUsages prints them. The flags
// themselves are not changed: the copies share their Value.
func hiddenFlags(c *cobra.Command) *pflag.FlagSet {
	fs := pflag.NewFlagSet(c.Name(), pflag.ContinueOnError)
	for _, set := range []*pflag.FlagSet{c.LocalFlags(), c.InheritedFlags()} {
		set.VisitAll(func(f *pflag.Flag) {
			if !f.Hidden || fs.Lookup(f.Name) != nil {
				return
			}
			cp := *f
			cp.Hidden = false
			fs.AddFlag(&cp)
		})
	}
	return fs
}

// usageTemplate is cobra's default usage template (v1.8.1) with two
// additions after "Flags:": the hidden flags under advancedFlagsHeading when
// helpAll is set, and otherwise -- only for a command that has hidden flags --
// a last line saying how to see them.
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if and helpAll (hasHiddenFlags .)}}

` + advancedFlagsHeading + `
{{hiddenFlagUsages . | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}{{if and (not helpAll) (hasHiddenFlags .)}}

More flags: {{.CommandPath}} --help-all{{end}}
`
