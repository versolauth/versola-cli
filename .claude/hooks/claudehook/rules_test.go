package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCheckCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want string // "", "ask" or "deny"
	}{
		// git add: only by name.
		{"git add .", "deny"},
		{"git add -A", "deny"},
		{"git add --all", "deny"},
		{"git -C /repo add .", "deny"},
		{"git status && git add -A && git commit -m x", "deny"},
		{"git add internal/deploy/up.go README.md", ""},
		{"git add .claude/settings.json", ""},
		{"git diff --stat", ""},

		// ssh and friends.
		{"ssh root@stand", "ask"},
		{"sudo ssh root@stand uptime", "ask"},
		{"scp file root@stand:/tmp", "ask"},
		{"echo $(ssh host hostname)", "ask"},
		{"(cd e2e && ssh host)", "ask"},
		{"& 'C:\\Windows\\System32\\OpenSSH\\ssh.exe' host", "ask"},
		{"grep -rn ssh internal", ""},

		// versola itself.
		{"versola up", "ask"},
		{"./versola.exe rollout 0.7.0", "ask"},
		{"versola --yes down", "ask"},
		{"versola-linux-amd64 configure vps 0.7.0", "ask"},
		{"go run ./cmd/versola bootstrap local 0.7.0", "ask"},
		{"versola version", ""},
		{"versola status", ""},
		{"./versola.exe doctor", ""},
		{"versola rollout --help", ""},
		{"go build -o versola.exe ./cmd/versola", ""},
		{"go run ./cmd/versola version", ""},
		{"go test ./internal/deploy -run TestRollout", ""},

		// e2e stand.
		{"E2E_CONFIRM_CLEAN=yes ./run.sh clean", "ask"},
		{"bash e2e/rollout/run.sh clean", "ask"},
		{"bash -n e2e/rollout/run.sh", ""},

		// secrets.
		{"cat ~/.versola/active/state.json", "ask"},
		{"ls $HOME/.versola", "ask"},
		{"Get-Content C:\\Users\\x\\.versola\\active\\state.json", "ask"},
		{"type prod.secrets.env", "ask"},
		{"cat vps.generated-secrets.env", "ask"},
		{"cat internal/state/state.go", ""},

		// workflows.
		{"rm .github/workflows/ci.yml", "ask"},
		{"sed -i 's/1.22/1.25/' .github/workflows/ci.yml", "ask"},
		{"echo x >> .github/workflows/ci.yml", "ask"},
		{"Set-Content -Path .github/workflows/ci.yml -Value x", "ask"},
		{"cat .github/workflows/ci.yml", ""},
		{"sed -n 1,20p .github/workflows/ci.yml", ""},

		// tags, force-pushes, branch deletion.
		{"git tag 0.3.3", "ask"},
		{"git tag -a 0.3.3 -m x", "ask"},
		{"git tag -d 0.3.3", "ask"},
		{"git tag 0.3.3 && git push --tags", "ask"},
		{"git push origin 0.3.3", "ask"},
		{"git push origin refs/tags/0.3.3", "ask"},
		{"git push --follow-tags origin feat/x", "ask"},
		{"git push --force-with-lease origin feat/x", "ask"},
		{"git push origin +feat/x", "ask"},
		{"git push origin :feat/x", "ask"},
		{"git branch -d feat/x", "ask"},
		{"git tag", ""},
		{"git tag --list '[0-9]*' --sort=-v:refname", ""},
		{"git push -u origin chore/claude-setup", ""},
		{"git push origin fix/thing", ""},
		{"git branch --show-current", ""},

		// heredoc bodies are data.
		{"gh pr create --title x --body-file - <<'EOF'\nnever git add . or ssh\nversola up\nEOF\n", ""},
		{"cat <<-EOF > notes.txt\n\tgit add -A\n\tEOF\nssh host", "ask"},
		{"cat <<EOF\nunterminated git add .", ""},
		{"git add . <<EOF\nx\nEOF", "deny"},

		// combined short flags.
		{"git push -fu origin feat/x", "ask"},
		{"git branch -df feat/x", "ask"},
		{"git push -u origin feat/x", ""},

		// quoted Windows paths keep their backslashes.
		{`Get-Content "C:\Users\RBk\.versola\active\state.json"`, "ask"},
		{`& "C:\Windows\System32\OpenSSH\ssh.exe" host`, "ask"},
		{`Remove-Item "C:\repo\.github\workflows\ci.yml"`, "ask"},
		{`cd "C:\Users\RBk\Desktop\versola-cli" && go test ./...`, ""},

		// line continuations.
		{"FOO=1 \\\nversola up", "ask"},
		{"git \\\nadd -A", "deny"},

		// a heredoc fed to a shell is a script; a shift is not a heredoc.
		{"bash <<EOF\nssh host\nEOF", "ask"},
		{"echo $((1<<2))\nssh host", "ask"},

		// here-strings are not heredocs: the next line is still a command.
		{"grep -c x <<<\"hello\"\nssh host", "ask"},
		{"grep -c x <<< hello\ngit add .", "deny"},

		// a refspec that creates a tag on the remote.
		{"git push origin HEAD:refs/tags/0.3.3", "ask"},
		{"git push origin HEAD:0.3.3", "ask"},
		{"git push origin HEAD:chore/x", ""},
		{"git push origin HEAD:refs/heads/main", "ask"},
		{"git push origin feat/x:main", "ask"},
		{"git push origin main -u", "ask"},

		// other spellings of "add everything".
		{"git add ./", "deny"},
		{`git add .\`, "deny"},
		{"git add -vA", "deny"},
		{"git add -A:/", "deny"},
		{"git add -v internal/x.go", ""},

		// go run with flags, or from inside cmd/versola.
		{"go run -race ./cmd/versola up", "ask"},
		{"go run . up", "ask"},
		{"go run ./.claude/hooks/claudehook gofmt-check", ""},

		// mentions of the secrets paths are not paths.
		{`git commit -m "Write ~/.versola/active/state.json atomically"`, ""},
		{`grep -rn "\.versola" internal`, ""},
		{`grep -rn ".versola/" internal`, ""},
		{`git grep "secrets.env"`, ""},
		{"git log -S secrets.env", ""},
		{`cat "$HOME/.versola/active/state.json"`, "ask"},
		{"cat $env:USERPROFILE/.versola/active/state.json", "ask"},

		// quoting: the words inside a quoted commit message are not commands.
		{`git commit -m "Ask before ssh; never git add ."`, ""},
		{"", ""},
	}
	for _, c := range cases {
		got := checkCommand(c.cmd)
		if got.decision != c.want {
			t.Errorf("checkCommand(%q) = %q (%s), want %q", c.cmd, got.decision, got.reason, c.want)
		}
		if got.decision != "" && got.reason == "" {
			t.Errorf("checkCommand(%q): decision %q without a reason", c.cmd, got.decision)
		}
	}
}

func TestFormatGoFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("package x\nimport (\n\"os\"\n\"fmt\"\n)\nvar _ = fmt.Sprint\nvar _ = os.Args\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := formatGoFile(p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "package x\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nvar _ = fmt.Sprint\nvar _ = os.Args\n"
	if string(got) != want {
		t.Errorf("formatted file:\n%s\nwant:\n%s", got, want)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(p); info.Mode().Perm() != 0o640 {
			t.Errorf("mode = %v, want 0640", info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temporary file left behind: %v", entries)
	}

	// A CRLF checkout (core.autocrlf=true) that is otherwise formatted is
	// left alone; an unformatted one keeps its CRLFs.
	crlf := filepath.Join(dir, "crlf.go")
	formattedCRLF := "package x\r\n\r\nvar _ = 1\r\n"
	os.WriteFile(crlf, []byte(formattedCRLF), 0o644)
	if err := formatGoFile(crlf); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(crlf); string(got) != formattedCRLF {
		t.Errorf("formatted CRLF file changed to %q", got)
	}
	os.WriteFile(crlf, []byte("package x\r\nvar  _ = 1\r\n"), 0o644)
	if err := formatGoFile(crlf); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(crlf); string(got) != formattedCRLF {
		t.Errorf("CRLF file formatted to %q, want %q", got, formattedCRLF)
	}
	os.Remove(crlf)

	// A file that does not parse yet (mid-edit) is left untouched.
	bad := filepath.Join(dir, "bad.go")
	src := "package x\nfunc {"
	os.WriteFile(bad, []byte(src), 0o644)
	if err := formatGoFile(bad); err == nil {
		t.Error("formatGoFile on unparsable source: want an error")
	}
	if got, _ := os.ReadFile(bad); string(got) != src {
		t.Errorf("unparsable file changed to %q", got)
	}

	// Non-Go files are ignored.
	md := filepath.Join(dir, "README.md")
	os.WriteFile(md, []byte("x  \n"), 0o644)
	if err := formatGoFile(md); err != nil {
		t.Errorf("formatGoFile on .md: %v", err)
	}
}

func TestUnformattedFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ok.go", "package x\n")
	write("crlf.go", "package x\r\n\r\nvar _ = 1\r\n")
	write("sub/bad.go", "package x\nvar  _ = 1\n")
	write("sub/broken.go", "package x\nfunc {")
	write(".hidden.go", "package x\nvar  _ = 1\n")
	write(".git/x.go", "package x\nvar  _ = 1\n")
	write("notes.txt", "var  _ = 1\n")

	got, err := unformattedFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.ToSlash(filepath.Join(dir, "sub", "bad.go")),
		filepath.ToSlash(filepath.Join(dir, "sub", "broken.go")),
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("unformattedFiles = %v, want %v", got, want)
	}
}
