# CLI 0.3.3 candidate for Versola 0.7.5

Status: reviewed draft candidate. Do not tag or publish until CI and the
remaining deployment checks below have been reviewed by the operator.

Base: CLI 0.3.2 (`d81017a28cff291986d0b10a3a7ea4f44f602d69`).
Server baseline: Versola 0.7.5 (`23077cb37756a2f4a4219adadcdbb76b9611d9b2`).
The release branch deliberately excludes later main changes. Merge this PR
into a separate branch based on 0.3.2, never into main. Tag the reviewed
release-branch commit only after the blockers below are resolved.

## Selected changes

| CLI PR | Reason it can be backported |
|---|---|
| #63 | Registry help/error wording; no server contract change |
| #89 | Domain in README and a comment; no runtime change |
| #90 | Patched build toolchain and CI checks; no server contract change |
| #115 | Headers on console static files served by CLI nginx |
| #139 | Strip query parameters from nginx request and Referer access logs |

## Deferred changes

Deferred does not mean incompatible. This is a small patch candidate, not
an attempt to ship every otherwise compatible feature from main.

| CLI PRs | Reason |
|---|---|
| #65 | EST needs Versola #804, absent from 0.7.5; old tools ignore its variables |
| #85, #86, #87 | New VPS Postgres default/initialization; #85 needs Versola #849, absent from 0.7.5 |
| #93, #94, #95, #97, #104, #107, #108, #128, #129, #130, #132, #136, #144, #151, #162, #163 | New Postgres, backup, doctor and uninstall paths or their follow-ups; keep out of this patch train |
| #110, #111, #145, #148, #149 | Compatibility-manifest reader and follow-ups. Reader has a fallback for older tools; not inherently incompatible, but outside this patch train. Server manifest #893 is absent from 0.7.5 |
| #150, #161 | Layout 5 and its downgrade handling; this candidate keeps layout 4 |
| #112, #113, #114 | New real-image rollout harness expects bundled VPS Postgres tools |
| #116 | Compatibility for new non-root edge images (Versola #954); not required by 0.7.5 |
| #98, #99, #100, #102, #103, #106, #127, #133, #134, #135, #140, #152, #160 | Additional commands, diagnostics, validation and fixes on newer CLI paths; deliberately deferred rather than declared incompatible |
| #39, #88, #91, #92 | Broader documentation, VPS stop confirmation and development tooling; outside this patch train |

## Additional data-preservation fix

Ordinary local `down` now uses Compose `stop`, retaining the Postgres
container and its anonymous-volume attachment. Explicit `--volumes`
continues to remove containers and volumes; VPS behavior is unchanged.
No state format, server configuration or database migration changes.

## Evidence and remaining checks

Passed: Windows unit tests except proxy (the repository's documented
Windows exclusion), Linux proxy test binary in WSL, vet, LF-normalized
gofmt, and builds for linux/amd64, darwin/amd64, darwin/arm64,
windows/amd64.

Real-image local test against 0.7.5 passed doctor, first bootstrap, status,
discovery, unauthenticated API refusal, console headers on 200/404, no
HSTS on local HTTP even with a forwarded HTTPS header, and query/Referer
redaction in real nginx access logs. See `e2e/release-075/`.

The initial test found that after `down`, `up` created a new anonymous Postgres volume.
Central then fails Flyway validation because its schema does not exist.
The published 0.7.5 compose fragment has no explicit Postgres data mount;
the image supplies an anonymous `/var/lib/postgresql` volume. This is
inherited from the baseline, not introduced by the five backports. Do not
work around it by re-running migrations into an empty database and calling
the test successful: existing customer data must survive the stop/start.
The local stop fix above resolves this for deployments whose original
container still exists. It cannot identify an orphaned volume after an
older CLI already removed the container; recovery must not guess a volume.

The rerun passed down/up, retained the same Postgres container ID and a
pre-stop SQL marker row, and completed repeated migrations. Unit tests
cover local/VPS with and without explicit volume deletion. Independent
review: one round, Approve, no findings. The reviewer inspected the full
diff, command wiring, state failure paths, templates and smoke assertions.

Browser login remains a release blocker: the password form reached OTP,
but the automation did not complete a confirmed authenticated console
session. Repeated automation attempts triggered the server's temporary
access restriction. Do not weaken or disable it to make the test pass;
repeat once after the restriction expires with correct navigation waits.

Not yet verified: complete browser login, full VPS deployment, ACME, remote edges, rolling upgrade,
upgrade from an older server database, and this candidate's GitHub CI.
No release is published by this work. The website needs no behavior-doc
change: the fix restores the already documented promise that down keeps
local data. Add the website changelog when a release is actually published.
