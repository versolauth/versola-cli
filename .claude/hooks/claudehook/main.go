// Command claudehook implements the Claude Code hooks configured in
// .claude/settings.json for this repository.
//
// It is written in Go rather than shell on purpose: everyone who works on
// this repo has Go, but not necessarily jq, bash or python (the operator
// works on Windows), and the rules below must behave the same on
// windows/darwin/linux. It lives under .claude/, a directory the go tool
// skips in ./... patterns, so `go build ./...`, `go vet ./...` and the
// release build never see it; it is run explicitly as
// `go run ./.claude/hooks/claudehook <mode>` (go run caches the built
// binary, so a call costs a few hundred milliseconds after the first).
//
// Modes:
//
//	pre-shell  PreToolUse on Bash/PowerShell: asks the operator before a
//	           command that CLAUDE.md says needs an explicit go (ssh, a
//	           state-changing versola command, run.sh clean, touching
//	           ~/.versola or a secrets file) and denies `git add .`/`-A`.
//	post-edit  PostToolUse on Edit/Write: gofmt's the edited .go file.
//
// and one helper that is not a hook:
//
//	gofmt-check  `gofmt -l .` that ignores CRLF line endings (used by
//	             /check; exit 1 and the file list if anything is off).
//
// A hook must never break the session: on any internal error (unreadable
// input, unexpected JSON) it exits 0 with no output, which Claude Code
// treats as "no opinion" -- the permission rules in settings.json still
// apply. Only an explicit decision is printed.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type hookInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

type preToolUseOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
	} `json:"hookSpecificOutput"`
}

func main() {
	// gofmt-check is not a hook: /check's script uses it in place of
	// `gofmt -l .`, which reports every file of a CRLF checkout.
	if len(os.Args) == 2 && os.Args[1] == "gofmt-check" {
		bad, err := unformattedFiles(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		for _, f := range bad {
			fmt.Println(f)
		}
		if len(bad) > 0 {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: claudehook pre-shell|post-edit|gofmt-check")
		os.Exit(0)
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(0)
	}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		os.Exit(0)
	}

	switch os.Args[1] {
	case "pre-shell":
		d := checkCommand(in.ToolInput.Command)
		if d.decision == "" {
			return
		}
		var out preToolUseOutput
		out.HookSpecificOutput.HookEventName = "PreToolUse"
		out.HookSpecificOutput.PermissionDecision = d.decision
		out.HookSpecificOutput.PermissionDecisionReason = d.reason
		_ = json.NewEncoder(os.Stdout).Encode(out)
	case "post-edit":
		// A formatting failure means the file does not parse yet (the
		// edit is half-way through a multi-step change); gofmt -l in
		// /check and CI catches whatever is still unformatted at the end,
		// so this is best-effort and silent.
		_ = formatGoFile(in.ToolInput.FilePath)
	}
}
