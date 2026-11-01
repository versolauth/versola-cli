package main

import (
	"bytes"
	"go/format"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// decision is what pre-shell answers for one command: "" (no opinion, the
// permission rules decide), "ask" (the operator confirms) or "deny".
type decision struct {
	decision string
	reason   string
}

// The rules mirror CLAUDE.md's "Git workflow" section. They are a safety
// net, not a sandbox: a determined command (eval, a script file, an alias)
// gets past any text match. What they catch is the ordinary case -- the
// command a model would type -- and they lean towards asking, because an
// unneeded question costs a click while a missed one can hit production.
//
// "ask" rather than "deny" for everything CLAUDE.md allows with an explicit
// go from the operator (ssh, versola against a host, run.sh clean, secrets):
// the question *is* that go. `git add .`/`-A` is denied because CLAUDE.md
// never allows it -- the working tree holds untracked artifacts (binaries,
// patches, *.hcl, Claude outputs/) that must not be committed.
func checkCommand(cmd string) decision {
	var ask []string
	for _, seg := range splitSegments(cmd) {
		if len(seg) == 0 {
			continue
		}
		if r := denyReason(seg); r != "" {
			return decision{"deny", r}
		}
		if r := askReason(seg); r != "" {
			ask = append(ask, r)
		}
	}
	if len(ask) == 0 {
		return decision{}
	}
	return decision{"ask", strings.Join(ask, "; ")}
}

func denyReason(seg []string) string {
	i, word := commandWord(seg)
	if word != "git" {
		return ""
	}
	sub, args := gitSubcommand(seg[i+1:])
	if sub != "add" {
		return ""
	}
	for _, a := range args {
		p := strings.TrimRight(slash(a), "/")
		switch {
		case p == "" || p == "." || p == "*" || strings.HasPrefix(p, ":/") || p == ":":
		case a == "--all" || a == "--no-ignore-removal" || strings.HasPrefix(a, "-A") || shortFlag(a, "A"):
		default:
			continue
		}
		return "CLAUDE.md: stage files by name (git add path/to/file), never `git add .` or `-A` -- the working tree holds untracked local artifacts that must not be committed"
	}
	return ""
}

// readOnlyVersola are the versola subcommands that change nothing on the
// machine or a host. Everything else (up, down, rollout, configure,
// bootstrap, migrate, replica, secrets, edge, certs, postgres, upgrade,
// uninstall, and whatever is added later) is asked about: CLAUDE.md
// forbids running versola up/rollout against a real host without a go, and
// an unknown future subcommand is safer asked than assumed harmless.
var readOnlyVersola = map[string]bool{
	"version": true, "status": true, "doctor": true, "help": true,
	"completion": true, "--help": true, "-h": true, "--version": true,
}

func askReason(seg []string) string {
	var reasons []string
	i, word := commandWord(seg)
	args := seg[min(i+1, len(seg)):]

	switch word {
	case "git":
		if r := gitAskReason(args); r != "" {
			reasons = append(reasons, r)
		}
	case "ssh", "scp", "sftp", "rsync":
		reasons = append(reasons, "CLAUDE.md: ssh to the production or test stand needs an explicit go from the operator")
	case "go":
		// `go run [flags] ./cmd/versola up ...` is the CLI itself; so is
		// `go run . up` typed inside cmd/versola -- the working directory
		// is unknown here, so `.` is treated as the CLI too.
		if len(args) >= 2 && args[0] == "run" {
			for k := 1; k < len(args); k++ {
				if strings.HasPrefix(args[k], "-") {
					continue
				}
				pkg := slash(args[k])
				if strings.Contains(pkg, "cmd/versola") || pkg == "." || pkg == "./" {
					if r := versolaReason(args[k+1:]); r != "" {
						reasons = append(reasons, r)
					}
				}
				break
			}
		}
	}
	if isVersolaBinary(word) {
		if r := versolaReason(args); r != "" {
			reasons = append(reasons, r)
		}
	}

	for j, t := range seg {
		p := slash(t)
		if path.Base(p) == "run.sh" && j+1 < len(seg) && seg[j+1] == "clean" {
			reasons = append(reasons, "CLAUDE.md: `run.sh clean` removes the e2e stand and needs an explicit go")
		}
		if touchesSecrets(p) {
			reasons = append(reasons, "CLAUDE.md: ~/.versola and *.secrets.env hold secrets; touching them needs an explicit go, and their values must never be printed")
		}
		if t == ">" || t == ">>" {
			if j+1 < len(seg) && strings.Contains(slash(seg[j+1]), ".github/workflows") {
				reasons = append(reasons, "CLAUDE.md: changing .github/workflows/ needs an explicit go")
			}
		}
	}
	if writesWorkflows(word, seg) {
		reasons = append(reasons, "CLAUDE.md: changing .github/workflows/ needs an explicit go")
	}
	return strings.Join(dedupe(reasons), "; ")
}

// gitAskReason covers the git operations CLAUDE.md says to ask first for:
// tags (a bare version tag pushed to origin *is* a release -- release.yml
// triggers on "[0-9]*"), force-pushes including --force-with-lease and a
// +refspec, and deleting branches. Plain --force and push to main are
// already denied by the settings rules; asking here as well is harmless.
func gitAskReason(gitArgs []string) string {
	sub, args := gitSubcommand(gitArgs)
	switch sub {
	case "tag":
		// Listing tags is fine; creating, moving or deleting one is not.
		for _, a := range args {
			switch {
			case a == "-l" || a == "--list" || strings.HasPrefix(a, "--sort") || strings.HasPrefix(a, "--contains") ||
				strings.HasPrefix(a, "--points-at") || strings.HasPrefix(a, "--merged") || strings.HasPrefix(a, "--no-merged"):
				return ""
			case a == "-d" || a == "--delete" || a == "-a" || a == "-s" || a == "-f" || a == "--force" || !strings.HasPrefix(a, "-"):
				return "CLAUDE.md: creating or deleting a tag needs the operator's go -- a bare version tag pushed to origin triggers release.yml"
			}
		}
	case "push":
		for _, a := range args {
			// The destination of a src:dst refspec decides what the push
			// creates: HEAD:refs/tags/0.3.3 or HEAD:0.3.3 makes a tag (and a
			// release) on the remote without any local `git tag`.
			dst := a
			if _, d, ok := strings.Cut(a, ":"); ok && !strings.HasPrefix(a, ":") {
				dst = d
			}
			switch {
			case a == "--tags" || a == "--follow-tags" || a == "--mirror" || a == "--all":
				return "CLAUDE.md: pushing tags needs the operator's go -- a bare version tag triggers release.yml"
			case strings.HasPrefix(a, "--force") || a == "--delete" || shortFlag(a, "fd"):
				return "CLAUDE.md: force-pushing (including --force-with-lease) or deleting a remote branch needs the operator's go"
			case strings.HasPrefix(a, "+"):
				return "CLAUDE.md: a +refspec is a force-push and needs the operator's go"
			case strings.HasPrefix(a, ":"):
				return "CLAUDE.md: pushing :branch deletes it on the remote and needs the operator's go"
			case dst != "" && dst[0] >= '0' && dst[0] <= '9':
				return "CLAUDE.md: pushing a version tag publishes a release (release.yml) and needs the operator's go"
			case dst == "main" || dst == "refs/heads/main":
				return "CLAUDE.md: main is protected -- never push to it; work on a branch"
			case strings.HasPrefix(dst, "refs/tags/"):
				return "CLAUDE.md: pushing a tag needs the operator's go -- a bare version tag triggers release.yml"
			}
		}
	case "branch":
		for _, a := range args {
			if a == "--delete" || shortFlag(a, "dD") {
				return "CLAUDE.md: deleting a branch needs the operator's go"
			}
		}
	}
	return ""
}

// shortFlag reports whether a is a cluster of single-letter options
// (-f, -fu, -df) that contains any of letters.
func shortFlag(a, letters string) bool {
	if len(a) < 2 || a[0] != '-' || a[1] == '-' {
		return false
	}
	return strings.ContainsAny(a[1:], letters)
}

// slash turns backslashes into forward slashes on every OS. Unlike
// filepath.ToSlash (a no-op off Windows) it also normalises a Windows path
// typed into a command on macOS/Linux -- and the tests run there too.
func slash(s string) string { return strings.ReplaceAll(s, `\`, "/") }

func versolaReason(args []string) string {
	sub := ""
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "--version" {
			return ""
		}
		if sub == "" && !strings.HasPrefix(a, "-") {
			sub = a
		}
	}
	if sub == "" || readOnlyVersola[sub] {
		return ""
	}
	return "CLAUDE.md: `versola " + sub + "` changes a deployment (local or a real host); it needs an explicit go from the operator"
}

func isVersolaBinary(word string) bool {
	return word == "versola" || strings.HasPrefix(word, "versola-")
}

// touchesSecrets reports whether a word is a path into the secrets: the
// CLI's state directory under a home (~/.versola, $HOME/.versola,
// C:/Users/x/.versola, $env:USERPROFILE/.versola ...) or a secrets file
// (vps.secrets.env, x.generated-secrets.env).
//
// It looks for paths, not mentions: this repo talks about ~/.versola and
// secrets.env all the time, in commit messages, grep patterns and PR
// bodies. A word with whitespace is prose (a quoted message), `.versola`
// must follow a non-empty path element (so the grep pattern ".versola/"
// or `\.versola` is not a path), and the bare name secrets.env is a
// search term, not a file. A relative `.versola/...` typed from the home
// directory is missed by this -- the Read deny rules in settings.json
// still cover the Read tool there.
func touchesSecrets(p string) bool {
	p = strings.ToLower(p)
	if strings.ContainsAny(p, " \t\n") {
		return false
	}
	if base := path.Base(p); strings.HasSuffix(base, ".secrets.env") || strings.HasSuffix(base, "-secrets.env") {
		return true
	}
	segs := strings.Split(p, "/")
	for k := 1; k < len(segs); k++ {
		if segs[k] == ".versola" && segs[k-1] != "" {
			return true
		}
	}
	return false
}

// writeCommands change or remove files; with a .github/workflows argument
// they are asked about. sed is included only with -i (in place).
var writeCommands = map[string]bool{
	"rm": true, "mv": true, "cp": true, "tee": true, "truncate": true,
	"remove-item": true, "set-content": true, "out-file": true, "add-content": true,
	"move-item": true, "copy-item": true, "new-item": true, "rename-item": true,
	"clear-content": true,
}

func writesWorkflows(word string, seg []string) bool {
	inPlaceSed := false
	if word == "sed" || word == "perl" {
		for _, t := range seg {
			if strings.HasPrefix(t, "-i") || t == "--in-place" {
				inPlaceSed = true
			}
		}
	}
	if !writeCommands[word] && !inPlaceSed {
		return false
	}
	for _, t := range seg {
		if strings.Contains(slash(t), ".github/workflows") {
			return true
		}
	}
	return false
}

// commandWord returns the index and normalised name (base name, lower case,
// without .exe) of the program a segment runs, skipping what only wraps it:
// sudo/env/command/exec/time/nohup, VAR=value assignments and PowerShell's
// call operator &.
func commandWord(seg []string) (int, string) {
	for i, t := range seg {
		if t == "&" || t == "!" || isAssignment(t) {
			continue
		}
		w := normalise(t)
		switch w {
		case "sudo", "env", "command", "exec", "time", "nohup", "nice", "xargs":
			continue
		}
		return i, w
	}
	return len(seg), ""
}

// isAssignment reports whether t is a NAME=value prefix (E2E_CONFIRM_CLEAN=yes).
func isAssignment(t string) bool {
	name, _, ok := strings.Cut(t, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func normalise(t string) string {
	t = strings.Trim(t, "\"'`")
	t = path.Base(slash(t))
	t = strings.ToLower(t)
	return strings.TrimSuffix(t, ".exe")
}

// gitSubcommand skips git's global options (-C <dir>, -c <k=v>, --git-dir=,
// --no-pager, ...) and returns the subcommand and its arguments.
func gitSubcommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
}

// splitSegments breaks a shell command line (bash or PowerShell) into
// simple commands and each of those into words. It understands single and
// double quotes and backslash escapes outside single quotes, splits on
// ; & | && || newlines and parentheses (so `$(ssh host)` and
// `(cd x && ssh host)` are seen), and makes > and >> separate words so a
// redirection target can be inspected. Heredoc bodies (<<WORD ... WORD)
// are data, not commands -- a PR body piped through `--body-file - <<EOF`
// may well quote "git add ." -- so they are skipped. It is not a full
// shell parser; where it is unsure it errs on the side of asking.
func splitSegments(s string) [][]string {
	var (
		segs    [][]string
		cur     []string
		word    strings.Builder
		inW     bool
		quote   rune
		pending []string // heredoc delimiters whose bodies start at the next newline
	)
	flushWord := func() {
		if inW {
			cur = append(cur, word.String())
			word.Reset()
			inW = false
		}
	}
	flushSeg := func() {
		flushWord()
		if len(cur) > 0 {
			segs = append(segs, cur)
			cur = nil
		}
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if quote != 0 {
			if c == quote {
				quote = 0
				continue
			}
			// In double quotes bash drops a backslash only before " \ $ `
			// and a newline; elsewhere it is literal -- as it always is in
			// a quoted Windows path ("C:\Users\x\.versola\...").
			if c == '\\' && quote == '"' && i+1 < len(rs) && strings.ContainsRune("\"\\$`\n", rs[i+1]) {
				i++
				if rs[i] != '\n' {
					word.WriteRune(rs[i])
				}
				continue
			}
			word.WriteRune(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			inW = true
		case '\\':
			// A backslash is an escape in bash but a path separator on
			// Windows (C:\Users\...). Keep it unless it escapes a
			// character that means something to the shell. A backslash
			// before a newline is a line continuation: both vanish.
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
				continue
			}
			if i+1 < len(rs) && strings.ContainsRune(" \t;&|()<>'\"\\", rs[i+1]) {
				i++
				word.WriteRune(rs[i])
			} else {
				word.WriteRune(c)
			}
			inW = true
		case ' ', '\t', '\r':
			flushWord()
		case '<':
			// A here-string (<<< word) is one word of input, not a
			// heredoc: step over all three '<' so the last two are not
			// re-read as `<<word` on the next pass.
			if i+2 < len(rs) && rs[i+1] == '<' && rs[i+2] == '<' {
				flushWord()
				i += 2
				continue
			}
			if i+1 < len(rs) && rs[i+1] == '<' {
				delim, next := heredocDelimiter(rs, i+2)
				if delim != "" {
					flushWord()
					// A heredoc fed to a shell (bash <<EOF) is a script:
					// its body is left in the input to be read as commands.
					if _, w := commandWord(cur); !shells[w] {
						pending = append(pending, delim)
					}
					i = next - 1
					continue
				}
			}
			flushWord()
		case '\n':
			flushSeg()
			for _, d := range pending {
				i = skipHeredocBody(rs, i+1, d)
			}
			pending = nil
		case ';', '|', '(', ')', '{', '}':
			flushSeg()
		case '&':
			// PowerShell's call operator `& prog` starts a command; in
			// bash & ends one. Either way a new segment begins here.
			flushSeg()
		case '>':
			flushWord()
			if i+1 < len(rs) && rs[i+1] == '>' {
				i++
				cur = append(cur, ">>")
			} else {
				cur = append(cur, ">")
			}
		default:
			word.WriteRune(c)
			inW = true
		}
	}
	flushSeg()
	return segs
}

var shells = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true,
	"pwsh": true, "powershell": true, "cmd": true, "ssh": true,
}

// heredocDelimiter reads the WORD of `<<WORD`, `<<-WORD`, `<< 'WORD'` or
// `<<"WORD"` starting at rs[i] (just after the <<) and returns it with the
// index after it; "" if there is no word there.
func heredocDelimiter(rs []rune, i int) (string, int) {
	if i < len(rs) && rs[i] == '-' {
		i++
	}
	for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
		i++
	}
	var b strings.Builder
	for i < len(rs) {
		c := rs[i]
		if c == '\'' || c == '"' {
			i++
			continue
		}
		if !(c == '_' || c == '-' || c == '.' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			break
		}
		b.WriteRune(c)
		i++
	}
	// `$((1<<2))` is a shift, not a heredoc: a delimiter starting with a
	// digit is not taken as one.
	if d := b.String(); d != "" && (d[0] < '0' || d[0] > '9') {
		return d, i
	}
	return "", i
}

// skipHeredocBody returns the index of the newline that ends the line equal
// to delim (leading tabs allowed, for <<-), searching from start; if the
// body never ends, the rest of the input is the body.
func skipHeredocBody(rs []rune, start int, delim string) int {
	for start < len(rs) {
		end := start
		for end < len(rs) && rs[end] != '\n' {
			end++
		}
		line := strings.TrimRight(strings.TrimLeft(string(rs[start:end]), "\t"), "\r")
		if line == delim {
			return end
		}
		start = end + 1
	}
	return len(rs)
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// formatted returns src run through gofmt and whether that changed it.
//
// Line endings are left out of the comparison: a Windows checkout with
// core.autocrlf=true has CRLF in every file, which plain `gofmt -l` reports
// as unformatted although git commits LF. The source is compared with its
// CRs removed, and a CRLF file gets its CRLFs back, so the working copy
// keeps the line endings it was checked out with.
func formatted(src []byte) ([]byte, bool, error) {
	crlf := bytes.Contains(src, []byte("\r\n"))
	lf := bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	out, err := format.Source(lf)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(out, lf) {
		return src, false, nil
	}
	if crlf {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	return out, true, nil
}

// unformattedFiles walks root the way `gofmt -l` does (every directory;
// files named *.go not starting with a dot; .git skipped, which holds no
// Go) and returns the files gofmt would change, ignoring line endings (see
// formatted). A file that does not parse is reported too, as gofmt -l
// fails on it.
func unformattedFiles(root string) ([]string, error) {
	var bad []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if _, changed, err := formatted(src); err != nil || changed {
			bad = append(bad, filepath.ToSlash(p))
		}
		return nil
	})
	return bad, err
}

// formatGoFile gofmt's one file in place, like `gofmt -w`, using the same
// go/format package gofmt uses (so no gofmt binary is needed on PATH). The
// file is replaced only when formatting changed it, and written through a
// temporary file and a rename so an interrupted hook never leaves a
// truncated source file behind.
func formatGoFile(p string) error {
	if !strings.HasSuffix(p, ".go") {
		return nil
	}
	src, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	out, changed, err := formatted(src)
	if err != nil || !changed {
		return err
	}
	info, err := os.Stat(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".gofmt-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
