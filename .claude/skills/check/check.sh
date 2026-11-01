#!/usr/bin/env bash
# Runs the pre-commit checks from CLAUDE.md ("Git workflow"), in its order:
# gofmt, go vet, go test, cross-compile linux/darwin/windows. It also tests
# the Claude hooks under .claude/ (the go tool skips dot directories in
# ./..., so `go test ./...` alone never reaches them).
#
# Every step runs even after a failure, so one run shows everything that is
# wrong; the exit status is non-zero if any step failed. Output of a passing
# step is suppressed; a failing step prints its output.
#
# Usage: .claude/skills/check/check.sh   (from anywhere inside the repo)
set -u
cd "$(git rev-parse --show-toplevel)" || exit 2

failed=()
step() {
	local name=$1
	shift
	local out
	if out=$("$@" 2>&1); then
		printf 'PASS  %s\n' "$name"
	else
		printf 'FAIL  %s\n%s\n' "$name" "$out" | sed '1!s/^/      /'
		failed+=("$name")
	fi
}

# `gofmt -l .`, but blind to line endings: on a Windows checkout with
# core.autocrlf=true every file has CRLF and plain gofmt -l lists them all,
# although git commits LF and CI (Linux) sees none of it. claudehook's
# gofmt-check compares each file with its CRs removed.
gofmt_clean() {
	local files
	files=$(go run ./.claude/hooks/claudehook gofmt-check) && return 0
	echo "not gofmt'ed (fix: gofmt -w <file>):"
	echo "$files"
	return 1
}

# internal/proxy's tests need symlink privileges and POSIX paths: on
# Windows they are skipped here and covered by CI on Linux (CLAUDE.md).
go_test() {
	case "$(go env GOOS)" in
	windows)
		echo "(windows: ./internal/proxy skipped, covered by CI)" >&2
		go test $(go list ./... | grep -v '/internal/proxy$')
		;;
	*) go test ./... ;;
	esac
}

cross() { CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -o /dev/null ./cmd/versola; }

step "gofmt -l ."                  gofmt_clean
step "go vet ./..."                go vet ./...
step "go test ./..."               go_test
step "go vet (claude hooks)"       go vet ./.claude/hooks/...
step "go test (claude hooks)"      go test ./.claude/hooks/...
step "build linux/amd64"           cross linux amd64
step "build darwin/amd64"          cross darwin amd64
step "build darwin/arm64"          cross darwin arm64
step "build windows/amd64"         cross windows amd64

if [ ${#failed[@]} -gt 0 ]; then
	printf '\n%d step(s) failed: %s\n' "${#failed[@]}" "$(IFS=,; echo "${failed[*]}")"
	exit 1
fi
printf '\nall checks passed\n'
