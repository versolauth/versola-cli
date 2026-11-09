# Status end-to-end test

Run `bash e2e/status/run.sh` from a checkout with Go, jq and Docker Compose.
It builds the CLI and creates a uniquely named Compose project using BusyBox
stub services, no published ports, volumes or real deployment credentials.
HOME/USERPROFILE point at a temporary directory. The exit trap removes only
this test's Compose project; the temporary files remain for diagnostics.

The built CLI reads real Docker state for healthy, restarting and unhealthy containers,
a stopped and then deleted central, an independently stopped auth replica,
the whole project after down, recovery, and a successfully completed init job.
Both output formats must agree on service warnings, JSON must remain one
object, and service outages must keep exit code 0. Other Docker states and
OpenBao retained in an earlier project are covered by the adjacent Go unit tests; no real Versola
images, VPS networking, certificates or live deployments are exercised here.
