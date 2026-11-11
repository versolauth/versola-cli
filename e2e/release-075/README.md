# CLI release candidate against Versola 0.7.5

Build the candidate, then run on a local machine with Docker, Python 3, and
no Versola containers or local Versola volumes. The test refuses existing
resources and uses a new home directory, never the operator's CLI state.

```sh
go build -o versola-release ./cmd/versola
python3 e2e/release-075/smoke.py --binary ./versola-release --workdir /absolute/new/test-directory
```

It pulls the real `ghcr.io/versolauth/*:0.7.5` images, runs doctor and
bootstrap, checks discovery, unauthenticated API refusal, console headers
on 200 and 404, ignored forwarded HTTPS on local HTTP, and query/Referer
redaction in real nginx access logs. It then runs down/up and migrations
again. A SQL marker and the Postgres container ID must survive down/up.
Assertion failures and command failures fail the test.

The deployment stays running for a browser login check (local test login
`admin` / `Admin1234!`). Logs and generated credentials stay in the private
work directory; do not upload it. This tests the local target, not a VPS,
ACME, remote edges, rolling upgrades, or an upgrade from an older database.

After the browser checks, run the same binary's `down` with `HOME` (Linux/macOS)
or `USERPROFILE` (Windows) set to the printed test home. Preserve the original
`DOCKER_CONFIG`. Remove only volumes created by this run, comparing against
`baselineVolumes` in `result.json`. Do not run `uninstall`: older local
uninstall code also names the shared legacy OpenBao volume.
