# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`versola-cli` is a Go (cobra) CLI that deploys and manages Versola (an auth platform) via Docker Compose on a local machine (`local`) or a single VPS (`vps`). It is deliberately thin: it knows nothing about Versola's service topology/config schema. That comes from the separately versioned `versola-tools` Docker image, which the CLI pulls and runs to generate the compose file, configs and `secrets.schema.json`. The CLI then parses those outputs (`internal/topology`, `internal/secrets`) rather than hard-coding ports or service names. `README.md` is the detailed user-facing spec for every command — read the relevant section before changing behaviour.

## Commands

```bash
go build -o versola.exe ./cmd/versola      # build (cross-compile with GOOS/GOARCH; root-level versola* binaries are gitignored)
go vet ./...
go test ./...
go test ./internal/deploy -run TestName    # single test
```

On Windows, run `go test ./...` without `./internal/proxy` (its tests need symlink privileges and POSIX paths); that package is covered by CI on Linux.

CI (`.github/workflows/ci.yml`) runs vet, cross-compiles windows/darwin(amd64,arm64)/linux, then `go test ./...`. Keep code building on all four targets — OS-specific code lives in `*_unix.go` / `*_windows.go` pairs (`internal/docker/shield_*.go`, `internal/state/lock_*.go`; `internal/proxy` has `umask_*_test.go` pairs). Releases: pushing a bare version tag (no leading `v`) triggers `release.yml`; the version is injected via ldflags.

Rollout end-to-end test (needs Docker; uses stub services instead of real Versola): `e2e/rollout/run.sh` — see `e2e/rollout/README.md`. Not verified: this script has not been run. It is a separate set of Go modules (`stub`, `traffic`).

## Architecture

- `cmd/versola/main.go` → `internal/cmd/` (thin cobra commands: flag parsing, confirmation prompts) → `internal/deploy/` (the real logic: `configure`, `migrate`, `up`, `replica`, `rollout*`, `secrets*`, `proxy`, OpenBao provisioning). `bootstrap` = `configure` + `migrate` + `up`.
- `internal/state/` — on-disk state in `~/.versola` (`active/state.json`, bundle directories per configured version, replica slots, a lock file, pending/rollout records). The state file has a versioned layout (currently **4**); newer layouts are unreadable by older CLIs, so changing the schema means bumping the layout and keeping reads of older ones working. Deployments predating certain features are rejected with "run `versola configure` again" rather than migrated.
- `internal/proxy/` — generates the CLI's own nginx reverse-proxy config as numbered *generations* (`proxy-vps/gen-NNNNNN/`, `current` symlink). Changes build a new generation, validate it in a throwaway nginx container, then reload (never restart). It fronts auth (8082 APORT, 8083 MPORT TCP pass-through), central (8090) and the public ports; on a host network (vps) replicas listen at fixed offsets so the proxy can balance them: +1000 for auth's APORT/MPORT (`frontOffset`) and +20000 for central's PORT/DPORT (`centralFrontOffset`), both in `deploy/topology.go`. On `local` (bridge network) these offsets don't apply.
- `internal/openbao/` — OpenBao client, AppRole credentials, auto-unseal. Secrets live in OpenBao; `internal/secrets` + `deploy/secrets_plan.go` compute a plan (`first-install`/`upgrade`/`alarm`/`installing`/`unknown`/`local`) from schema + OpenBao contents + recorded state, and write nothing if the plan has problems. Secret values must never be printed.
- `internal/docker/`, `internal/checks/` (doctor/preflight machine checks), `internal/wait/` (readiness polling), `internal/fsutil/` (atomic writes), `internal/browser/`.
- Rolling upgrades (`deploy/rollout*.go`): resumable — progress is reconstructed from `state.json` plus running containers, so the same command re-run continues after a cut-off. Stand-in replicas in free slots keep capacity during replacement; `--abort` reverses it and is itself resumable. The guards for an unfinished rollout live in `internal/deploy`, not `internal/cmd`, and differ per command: `configure` (`replicasBlockConfigure`, `replica.go`) refuses on `st.RolloutUnderway()` or `st.MixedVersions() != ""`; `up` (`checkRunnable`, `up.go`) refuses on the same two conditions; `replica add|remove` (`replica.go`) refuses on `st.MixedVersions()` only; `down` (`DownBlocked`, `rollout_run.go`) refuses on `RolloutUnderway()` unless `--force`. Preflight checks memory and Postgres connection headroom before each stand-in.

## Conventions

- Tests sit beside the code (`*_test.go`); `internal/topology` and `internal/secrets` use fixtures in `testdata/`.
- The codebase carries long explanatory comments about *why* (failure modes, ordering, security); match that density when touching tricky logic.
- GitHub Actions are pinned by commit SHA (see ci.yml/release.yml); keep that policy.
- Versola release versions and CLI release tags have no leading `v`.
- Update `README.md` when command behaviour changes — it doubles as the spec.

## Git workflow

- `main` is protected: work on a branch (`feat/...`, `fix/...`, `docs/...`, `chore/...`), one task per branch and per pull request. Create it from a fresh base: `git fetch origin`, then `git switch -c <branch> origin/main`. Never commit to or push `main`.
- You may commit and push your own branch without asking. Ask first for: force-push (including `--force-with-lease`), rebasing a branch that is already pushed, tags (a bare version tag triggers `release.yml`), releases, deleting branches, merging or closing pull requests. Never merge a pull request yourself.
- Before every commit run `git status` and `git diff --stat`, and stage files by name (`git add path/to/file`), never `git add .` or `-A`: the working tree often holds untracked local artifacts (`*.patch`, `versola-*` binaries, `Claude outputs/`, `*.hcl`, `package-lock.json`) that must not be committed. The commit must contain only the files of the task.
- Before the commit run, in this order: `gofmt -l .` (it must print nothing; fix with `gofmt -w` on the files you changed), `go vet ./...`, `go test ./...` (on Windows without `./internal/proxy`, see Commands), and cross-compile `GOOS=linux`, `GOOS=darwin` and `GOOS=windows`. Imports must stay sorted. Say in the pull request what was and was not run.
- **One commit, one push, at the end.** Do not commit or push while you work and do not make a new commit per review round or per fix: keep the changes in the working tree through all the rounds below, then make a single commit and a single push when the loop has finished. If the work is already committed, fold later fixes into that one commit before pushing (never rewrite a commit that is already pushed without asking).
- Commit message: one short line, imperative, no trailers and no attribution lines (`Fix the pathspec in the release procedure`).
- Pull request description in English: what changed and why, which tests were added at which level (unit / e2e), and any gap left (missing test level, unverified path) with the reason. After pushing, show the operator `git diff origin/main --stat`.
- Do not touch the production or the test stand (`ssh`, `versola up`/`rollout` against a real host, `run.sh clean`), secrets (`~/.versola`, `*.secrets.env`) or `.github/workflows/` without an explicit go from the operator. Secret values are never printed, logged or committed.
- When a task needs a decision (design, a new flag, a state layout change), propose two or three options with the worst case of each and wait; do not pick silently.

## Review loop before the commit

After the change works (formatting, vet and tests pass), review and fix it in rounds, until a round finds nothing that matters:

1. Start an independent reviewer: a subagent with fresh context. Give it the diff (`git diff origin/main`, including untracked new files) and the files around it, not your own conclusions. Tell it to follow the output format of `../versola/REVIEW.md` where it applies (verdict, findings as `[BLOCKER|MAJOR|MINOR|NIT] file:line - title` with a concrete failing scenario and a fix, then "Checked, no issues" and "Not verified"; skip its Scala/database sections). Things to look for in this repo: a prompt or command that can act destructively without a question, secret values reaching output, logs or errors, state layout compatibility, atomic writes, behaviour on windows/darwin/linux, README (it is the spec) out of sync with the code, missing test levels.
2. Check every finding yourself against the code before acting: a reviewer can be wrong. Fix the BLOCKER and MAJOR findings that are real, and MINOR ones only if they are cheap and inside the task. Do not fix NITs. Do not add protection against a scenario that cannot happen in this CLI, and do not widen the pull request beyond its purpose to satisfy a review: write such ideas down as follow-ups instead.
3. After the fixes re-run the checks above (`gofmt -l .`, `go vet`, `go test`, cross-compile) and start a NEW review round with a fresh reviewer on the updated diff. Repeat until a round returns no real BLOCKER or MAJOR finding. Stop also after five rounds, or if two rounds in a row keep finding new MAJOR problems in the same area: then report to the operator what is left instead of going on.
4. At the end report: the number of rounds; for each round what was found, what you fixed and what you rejected with the reason; what is still open or not verified (tests that could not be run, levels missing). Then make the single commit and the single push described above, open the pull request, and show `git diff origin/main...HEAD --stat`.
