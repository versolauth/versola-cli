---
name: release-prep
description: Prepare a versola-cli release up to, but not including, the tag - check main is clean and green, list what changed since the last tag, check README is in step, propose the version, and print the two commands the operator runs. Never creates or pushes a tag. Usage - /release-prep [version]
disable-model-invocation: true
---

# /release-prep [version]

A pushed tag *is* the release: `release.yml` builds and publishes on any tag matching `[0-9]*` (README "Releasing"). CLAUDE.md requires the operator's go for tags and releases, so this skill **stops before the tag**. Never run `git tag`, `git push` of a tag, or `gh release`.

Read-only steps, in order. Stop and report at the first blocker.

1. **Base.** `git fetch origin --tags`. The release commit is `origin/main` (not the local branch). Note its SHA and subject.
2. **CI on that commit.** `gh run list --branch main --workflow ci.yml --limit 5` and find the run for that SHA. Blocker if it is missing, failed or still running.
3. **Last release.** `git tag --list '[0-9]*' --sort=-v:refname | head -5`. Tags have no leading `v`; the older `v0.2.x` tags sort above every plain tag, which is why the list is filtered. The last release is the highest plain `X.Y.Z` (or `X.Y.Z-suffix`).
4. **What changed.** `git log --first-parent --oneline <last>..origin/main`; for each merged PR (`Merge pull request #N`, or a squash merge whose subject ends in `(#N)`; a commit with neither is listed on its own), `gh pr view N --json title,body` and classify: user-visible behaviour (command, flag, output, state layout), fix, internal/docs/CI. Note any state layout bump (`internal/state`, CLAUDE.md "state layout"): older CLIs cannot read a newer layout, so it must be called out in the release notes.
5. **README is the spec.** For each user-visible change, check the README section of that command describes it. List gaps; a gap is a blocker for a full release (not for a pre-release).
6. **Version.** If the operator gave one, check it is plain (`0.3.3`, `0.4.0-rc1`; no `v`), greater than the last tag and not already a tag. Otherwise propose one: patch for fixes only, minor for new commands/flags or a state layout bump; `-rcN` makes a GitHub pre-release, which the install scripts do not pick as latest.

## Report

- Release commit (SHA, subject) and its CI result.
- Last tag -> proposed version, and why.
- Changes grouped as above, one line each with the PR number; state layout changes first.
- README gaps or "README in step".
- Blockers, if any. If none, the commands for the operator to run themselves:

```bash
git tag <version> <sha>
git push origin <version>
```

Then stop.
