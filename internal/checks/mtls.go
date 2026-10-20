package checks

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// MTLSLeaves are the certificates in the mutual-TLS volume `versola certs
// renew` keeps valid.
var MTLSLeaves = []string{"server", "central-server", "edge-client"}

// leafScript prints "<leaf> <ok|due|expired|missing|error>" per leaf. Run in
// the step-ca image (it has `step`), which `mtls-init` uses as well:
// needs-renewal exits 0 when the certificate expires within the given time,
// 1 when it does not, anything else on an error; 0s asks whether it already
// has, 33% whether a third of its lifetime or less is left (what mtls-init
// re-issues at).
const leafScript = `for f in ` + "server central-server edge-client" + `; do
  if [ ! -f /mtls/$f.crt ]; then echo "$f missing"; continue; fi
  rc=0; step certificate needs-renewal /mtls/$f.crt --expires-in 0s >/dev/null 2>&1 || rc=$?
  if [ $rc -eq 0 ]; then echo "$f expired"; continue; fi
  if [ $rc -ne 1 ]; then echo "$f error"; continue; fi
  rc=0; step certificate needs-renewal /mtls/$f.crt --expires-in 33% >/dev/null 2>&1 || rc=$?
  case $rc in 0) echo "$f due";; 1) echo "$f ok";; *) echo "$f error";; esac
done`

// MTLSLeafCheck looks at the leaves in volume, with the image of the
// running step-ca container (so nothing is pulled). ok is false when there
// is nothing to look at -- no volume or no such container yet -- which is no
// finding: a host that was not deployed has no certificates.
func MTLSLeafCheck(volume, stepCAContainer string) (Result, bool) {
	if _, err := run(10*time.Second, "docker", "volume", "inspect", volume); err != nil {
		return Result{}, false
	}
	image, err := run(10*time.Second, "docker", "inspect", "--format", "{{.Config.Image}}", stepCAContainer)
	if err != nil || strings.TrimSpace(image) == "" {
		return Result{}, false
	}
	out, err := run(60*time.Second, "docker", "run", "--rm", "--network", "none", "-v", volume+":/mtls:ro",
		"--entrypoint", "sh", strings.TrimSpace(image), "-c", leafScript)
	if err != nil {
		return Result{Name: "mTLS certificates", OK: false, Detail: "couldn't read them: " + err.Error()}, true
	}
	return ParseLeafStatus(out), true
}

// ParseLeafStatus turns leafScript's output into the check's result:
// expired or unreadable fails, due (a third of the lifetime or less left)
// warns, since `versola certs renew` on its schedule is what clears it.
func ParseLeafStatus(out string) Result {
	byState := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		name, state, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			byState[state] = append(byState[state], name)
		}
	}
	list := func(state string) string {
		names := append([]string(nil), byState[state]...)
		sort.Strings(names)
		return strings.Join(names, ", ")
	}
	r := Result{Name: "mTLS certificates"}
	switch {
	case len(byState["expired"]) > 0:
		r.Detail = fmt.Sprintf("expired: %s -- run `versola certs renew`", list("expired"))
	case len(byState["error"]) > 0 || len(byState["missing"]) > 0:
		r.Detail = fmt.Sprintf("unreadable or missing: %s -- run `versola certs renew`", strings.Trim(list("error")+", "+list("missing"), ", "))
	case len(byState["due"]) > 0:
		r.OK, r.Warn = true, true
		r.Detail = fmt.Sprintf("a third of the lifetime or less left: %s -- run `versola certs renew` (schedule it: `versola certs renew --show-timer`)", list("due"))
	case len(byState["ok"]) == len(MTLSLeaves):
		r.OK, r.Detail = true, "valid"
	default:
		r.Detail = "couldn't tell: " + strings.TrimSpace(out)
	}
	return r
}
