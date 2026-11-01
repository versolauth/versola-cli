---
name: check
description: Run this repo's pre-commit checks in the order CLAUDE.md requires (gofmt -l, go vet, go test with internal/proxy skipped on Windows, the .claude hook tests, cross-compile linux/darwin/windows) and report what passed and failed. Use before every commit and after each fix round of a review loop.
---

# /check

Run the script and report its result:

```bash
bash .claude/skills/check/check.sh
```

It runs every step even after a failure and ends with a one-line summary.

Then:

- **All passed**: say so in one line, and name what was *not* covered (on Windows: `./internal/proxy` tests, which CI runs on Linux; the rollout e2e is never covered by this script).
- **Something failed**: show the failing step's output, fix the cause if it is part of the current task (for gofmt: `gofmt -w` on the files listed, only the ones you changed), and run the script again. Do not weaken, skip or delete a test to get green. If a failure is outside the task (it fails on `origin/main` too), report it instead of fixing it.

The PR description must say what was and was not run; quote this script's summary there.
