package deploy

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/wait"
)

const (
	// MTLSInitService is the compose service (versola-tools') that issues
	// and renews the mutual-TLS leaves in the deployment's mtls volume.
	MTLSInitService = "mtls-init"
	// mtlsInitChanged is the line it prints when it issued anything.
	mtlsInitChanged = "mtls-init: changed"
	mtlsInitIssued  = "mtls-init: issued "
)

// MTLSVolumeName is the volume the leaves live in, for target.
func MTLSVolumeName(target string) string { return "versola-mtls-" + target }

// certsOps is what a renewal does to the world, a seam so the decisions
// -- when to restart, in which order, what is reported when a step fails --
// can be tested without Docker.
type certsOps interface {
	// runInit runs mtls-init as a one-off and returns what it printed
	// (stdout and stderr together).
	runInit() ([]byte, error)
	restart(service string) error
	waitReady(service string) error
}

// certsStep is one service a renewal restarts.
type certsStep struct{ Service string }

// renewCerts runs mtls-init and, when it issued something -- or restart is
// set -- restarts steps in order, waiting for each to be ready before the
// next. The services read their certificates at startup, so that is what
// makes a renewal take effect. Silent when nothing was due.
func renewCerts(ops certsOps, steps []certsStep, restart bool, out io.Writer) error {
	output, err := ops.runInit()
	if err != nil {
		return fmt.Errorf("%s failed: %w\n%s", MTLSInitService, err, bytes.TrimSpace(output))
	}
	issued, changed := parseMTLSInit(output)
	if !changed && !restart {
		return nil
	}
	for _, name := range issued {
		fmt.Fprintf(out, "Renewed %s\n", name)
	}
	for i, s := range steps {
		fmt.Fprintf(out, "Restarting %s...\n", s.Service)
		if err := ops.restart(s.Service); err != nil {
			return notRestarted(steps[i:], fmt.Errorf("couldn't restart %s: %w", s.Service, err))
		}
		if err := ops.waitReady(s.Service); err != nil {
			return notRestarted(steps[i+1:], fmt.Errorf("%s never became ready after its restart: %w", s.Service, err))
		}
	}
	return nil
}

// notRestarted says what is left to do. The leaves are valid now, so a
// second `certs renew` finds nothing due and restarts nothing: the operator
// is told to run it with --restart.
func notRestarted(rest []certsStep, err error) error {
	if len(rest) == 0 {
		return fmt.Errorf("%w -- the certificates are renewed; once it is healthy, run `versola certs renew --restart`", err)
	}
	names := make([]string, len(rest))
	for i, s := range rest {
		names[i] = s.Service
	}
	return fmt.Errorf("%w -- the certificates are renewed but %s still run on the old ones; fix the problem and run `versola certs renew --restart`", err, strings.Join(names, ", "))
}

// parseMTLSInit reads mtls-init's output: the leaves it issued, and whether
// it says it changed anything.
func parseMTLSInit(output []byte) (issued []string, changed bool) {
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == mtlsInitChanged:
			changed = true
		case strings.HasPrefix(line, mtlsInitIssued):
			issued = append(issued, strings.TrimPrefix(line, mtlsInitIssued))
		}
	}
	return issued, changed
}

// RenewCertsOptions: see `versola certs renew`.
type RenewCertsOptions struct {
	// Restart restarts the services even when nothing was renewed.
	Restart bool
}

// RenewCerts renews the mutual-TLS leaves of the deployment st records that
// are due (a third of their lifetime or less left) and restarts central,
// auth and edge, in that order, when it did.
func RenewCerts(opts RenewCertsOptions, st *state.State) error {
	if st == nil {
		return fmt.Errorf("nothing has been configured yet")
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file) — configure it again")
	}
	if err := checkRunnable(st, filepath.Dir(composePath)); err != nil {
		return err
	}
	isVps := st.Target == "vps"
	topo, err := loadTopology(composePath)
	if err != nil {
		return err
	}
	central, err := replicasOf(topo, CentralService, []state.Slot{{N: 1, Version: st.Version}})
	if err != nil {
		return err
	}
	auth, err := replicasOf(topo, AuthService, st.ActiveSlots(AuthService))
	if err != nil {
		return err
	}
	if auth, err = frontAuth(auth, isVps); err != nil {
		return err
	}
	edge, err := replicasOf(topo, EdgeService, st.ActiveSlots(EdgeService))
	if err != nil {
		return err
	}
	readyURLs, err := checkTopology(topo, isVps, deployedProxyReservations(st), central, auth, edge)
	if err != nil {
		return err
	}
	var steps []certsStep
	for _, group := range [][]replica{central, auth, edge} {
		for _, r := range group {
			steps = append(steps, certsStep{Service: r.Service})
		}
	}
	return renewCerts(dockerCertsOps{composePath: composePath, readyURLs: readyURLs}, steps, opts.Restart, os.Stdout)
}

type dockerCertsOps struct {
	composePath string
	readyURLs   map[string]string
}

func (o dockerCertsOps) runInit() ([]byte, error) {
	c := docker.Cmd(state.ComposeArgs(o.composePath, "run", "--rm", "-T", MTLSInitService)...)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	return buf.Bytes(), err
}

func (o dockerCertsOps) restart(service string) error {
	return docker.Run(state.ComposeArgs(o.composePath, "restart", "-t", stopTimeoutSeconds, service)...)
}

func (o dockerCertsOps) waitReady(service string) error {
	return wait.ForReady(o.readyURLs[service], readyTimeout)
}
