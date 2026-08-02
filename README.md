# versola-cli

Command-line tool for deploying and managing Versola. Deliberately thin: it
checks the local machine and talks to Docker, but doesn't know anything
about Versola's own service topology (ports, service names, config schema)
— that lives in the separately-versioned `versola-tools` image, which
`bootstrap` pulls and runs. See the project design doc for why.

## Status

`doctor`, `bootstrap`, `status`, and `down` are all implemented and have
been tested end-to-end against a real local deployment (Postgres + auth +
central + edge + the gateway, real browser login through to the admin
console). No installer/release pipeline yet — build from source for now.

## Build

Needs Go installed. Builds are cross-platform from any machine — you don't
need a Mac to produce a macOS binary, or Windows to produce a Windows one.

```bash
go mod tidy   # fetches cobra, generates go.sum — needs network the first time
```

**Windows:**
```powershell
go build -o versola.exe ./cmd/versola
```

**macOS (Apple Silicon — M1/M2/M3/etc., the common case today):**
```bash
GOOS=darwin GOARCH=arm64 go build -o versola-darwin-arm64 ./cmd/versola
```

**macOS (Intel):**
```bash
GOOS=darwin GOARCH=amd64 go build -o versola-darwin-amd64 ./cmd/versola
```

**Linux:**
```bash
GOOS=linux GOARCH=amd64 go build -o versola-linux-amd64 ./cmd/versola
```

On macOS/Linux, the built binary needs the executable bit set before it'll
run (Windows doesn't need this step):
```bash
chmod +x versola-darwin-arm64
```

## Run

**Windows:**
```powershell
.\versola.exe doctor
.\versola.exe bootstrap local <version>
.\versola.exe status
.\versola.exe down
```

**macOS/Linux:**
```bash
./versola-darwin-arm64 doctor
./versola-darwin-arm64 bootstrap local <version>
./versola-darwin-arm64 status
./versola-darwin-arm64 down
```
(substitute whichever binary you built above)

### `doctor`

Checks this machine has what Versola needs, without installing or changing
anything:
- Docker daemon reachable (not just `docker` on PATH)
- Docker Compose v2 plugin present
- Port 8080 free on localhost
- Docker has enough memory allocated (~4 GiB — three JVMs + Postgres)
- Enough free disk space (~4 GiB, for pulling Versola's images)

Exits non-zero if any check fails. `bootstrap` runs the same checks itself
before doing anything, so running `doctor` first is optional but gives you
the same picture ahead of time.

Known gap: if no Docker runtime is found, this doesn't yet offer a choice
between installing Docker Desktop or Colima on macOS — it just reports
"not reachable". Worth adding before this goes to users who don't already
have Docker set up.

### `bootstrap local <version>`

Deploys a specific released version of Versola locally: pulls the
matching `versola-tools` image to generate config, then brings up
Postgres, central, auth, edge, and the gateway (nginx + central-ui) via
Docker Compose, waiting on readiness at each stage. Prints the admin
login on success.

### `status`

Shows the containers from the last `bootstrap` run and their health
(`docker compose ps` under the hood).

### `down`

Stops the locally deployed stack. Pass `--volumes` to also delete the
Postgres data volume (kept by default, so a later `bootstrap` picks up
the same data).

## Layout

```
cmd/versola/          entry point
internal/cmd/          cobra commands (doctor, bootstrap, status, down)
internal/checks/        the actual check logic doctor (and bootstrap) run
internal/state/         locates ~/.versola/active, the on-disk state bootstrap writes
internal/wait/          polls a readiness endpoint until it answers 200 or times out
```
