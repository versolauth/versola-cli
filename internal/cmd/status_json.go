package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

// What `status --json` asks Docker, the host and the timer, replaceable in
// tests.
var (
	statusContainers = deploy.ListContainers
	statusCertReport = deploy.CertReport
	statusProbeCerts = func(st *state.State) []deploy.CertInfo {
		var out []deploy.CertInfo
		for _, addr := range deploy.PublicEndpoints(st) {
			out = append(out, deploy.ProbeCertificate(addr, 5*time.Second))
		}
		return out
	}
	statusCertsWarning = certsWarning
	statusNow          = time.Now
)

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
	return nil
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
