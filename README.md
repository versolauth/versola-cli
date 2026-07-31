# versola-cli

Command-line tool for deploying and managing Versola. Deliberately thin: it
checks the local machine and talks to Docker, but doesn't know anything
about Versola's own service topology — see the project design doc for why.

## Status

Early skeleton. Only `doctor` actually does anything yet. `bootstrap`,
`status`, and `down` are stubs that print "not implemented yet".

## Build

```bash
go mod tidy   # fetches cobra, generates go.sum — needs network the first time
go build -o versola ./cmd/versola
```

## Run

```bash
./versola doctor
```

Checks:
- Docker daemon reachable (not just `docker` on PATH)
- Docker Compose v2 plugin present
- Port 8080 free on localhost

Exits non-zero if any check fails.

## Layout

```
cmd/versola/        entry point
internal/cmd/        cobra commands (doctor, bootstrap, status, down)
internal/checks/      the actual check logic doctor runs
```
