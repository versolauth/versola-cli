package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

// What `status --json` asks Docker, the host and the timer, replaceable in
// tests.
var (
	statusContainers = deploy.ListContainers
	statusServices   = deploy.DeploymentServices
	statusOpenbao    = deploy.OpenbaoContainerStatus
	statusCertReport = deploy.CertReport
	statusProbeCerts = func(st *state.State) []deploy.CertInfo {
		return probeAll(deploy.PublicEndpoints(st), func(addr string) deploy.CertInfo {
			return deploy.ProbeCertificate(addr, 5*time.Second)
		})
	}
	statusCertsWarning = certsWarning
	statusNow          = time.Now
)

// probeAll runs probe on every address at once and returns the results in the
// order of addrs: an unreachable name costs its timeout once, not once per
// endpoint, so a health check with several dead names is not several times
// slower.
func probeAll(addrs []string, probe func(addr string) deploy.CertInfo) []deploy.CertInfo {
	out := make([]deploy.CertInfo, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = probe(addr)
		}()
	}
	wg.Wait()
	return out
}

// writeStatusJSON is `status --json`: the same facts as the text form, as one
// object on w and nothing else there -- whatever goes wrong becomes a warning
// in it. The object is printed even when Docker can't be asked (containers
// null), and the command then fails, so a health check sees both the reason
// and a non-zero exit.
func writeStatusJSON(w io.Writer, composePath string, exists bool) error {
	if !exists {
		return encodeJSON(w, deploy.NotDeployedReport(version))
	}
	st, err := state.Load()
	if err != nil {
		// ComposeFile just read it: only a concurrent change gets here.
		return err
	}
	r := deploy.NewStatusReport(st, version)
	containers, dockerErr := statusContainers(composePath)
	if dockerErr != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("Docker could not list the containers: %s", firstErrLine(dockerErr)))
	} else {
		r.Containers = containers
	}
	var servicesErr error
	if dockerErr == nil {
		var warnings []string
		warnings, servicesErr = serviceWarnings(composePath, st, containers)
		r.Warnings = append(r.Warnings, warnings...)
	}
	// As in the text form: only for a deployment of this machine's own.
	if st.Target == "vps" || st.Target == "local" {
		now := statusNow()
		// The volume is read through a compose run: with Docker down that
		// is the same failure again, already in the warnings.
		var infos []deploy.CertInfo
		if dockerErr == nil {
			var err error
			if infos, err = statusCertReport(st); err != nil {
				r.Warnings = append(r.Warnings, "the mutual-TLS volume could not be read: "+firstErrLine(err))
			}
		}
		infos = append(infos, statusProbeCerts(st)...)
		r.Certificates = deploy.CertStatuses(infos, now)
		if !st.CertsUnlimited() {
			r.Warnings = append(r.Warnings, deploy.ExpiryWarnings(infos, now)...)
		}
		if msg := statusCertsWarning(st); msg != "" {
			r.Warnings = append(r.Warnings, msg)
		}
	}
	if err := encodeJSON(w, r); err != nil {
		return err
	}
	if dockerErr != nil {
		return dockerErr
	}
	return servicesErr
}

// Both formats use the same inventory and classification. If the inventory
// cannot be read, do not mistake an empty container list for a healthy stack.
func serviceWarnings(composePath string, st *state.State, containers []deploy.ContainerStatus) ([]string, error) {
	services, err := statusServices(composePath)
	if err != nil {
		return []string{"the deployment's services could not be read: " + firstErrLine(err)}, err
	}
	if st != nil && slices.Contains(services, "openbao") && !slices.ContainsFunc(containers, func(c deploy.ContainerStatus) bool { return c.Service == "openbao" }) {
		c, err := statusOpenbao(st.Target)
		if err != nil {
			return []string{"OpenBao's container could not be read: " + firstErrLine(err)}, err
		}
		if c != nil {
			// Keep the caller's snapshot unchanged (containers[] remains the
			// Compose project's list), but classify the retained OpenBao too.
			containers = append(slices.Clone(containers), *c)
		}
	}
	return deploy.ServiceWarnings(st, services, containers), nil
}

func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// firstErrLine: the rest of an error may be a container's output.
func firstErrLine(err error) string {
	l, _, _ := strings.Cut(err.Error(), "\n")
	return l
}
