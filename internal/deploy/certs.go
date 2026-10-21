package deploy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
	"github.com/versolauth/versola-cli/internal/wait"
)

const (
	// MTLSInitService is the compose service (versola-tools') that issues
	// and renews the mutual-TLS leaves in the deployment's mtls volume.
	MTLSInitService = "mtls-init"
	// mtlsInitChanged is the line it prints when it issued anything.
	mtlsInitChanged   = "mtls-init: changed"
	mtlsInitIssued    = "mtls-init: issued "
	mtlsInitStaged    = "mtls-init: staged "
	mtlsInitCommitted = "mtls-init: committed "
)

// MTLSVolumeName is the volume the leaves live in, for target.
func MTLSVolumeName(target string) string { return "versola-mtls-" + target }

// The phases of mtls-init (its MTLS_INIT_PHASE), which versola-tools
// documents with the script. A tools release from before the pin overlap
// ignores the variable and does what "" does: replaces what is due in place.
const (
	// phasePrepare stages a replacement for each server leaf that is due, as
	// <name>.next.crt/.key, leaving the current pair alone: the pin sets of
	// the edges (current + next) now contain the new certificate.
	phasePrepare = "prepare"
	// phaseIdentity replaces the edge's identity certificate if it is due.
	phaseIdentity = "identity"
	// phaseCommit moves the staged replacements into place.
	phaseCommit = "commit"
	// phaseReport changes nothing and says every certificate's notAfter.
	phaseReport = "report"
)

// certsOps is what a renewal does to the world, a seam so the decisions
// -- when to restart, in which order, what is drained first, what is
// reported when a step fails -- can be tested without Docker.
type certsOps interface {
	// runInit runs mtls-init as a one-off in the given phase ("" is its
	// default: replace what is due, in place) and returns what it printed
	// (stdout and stderr together).
	runInit(phase string) ([]byte, error)
	restart(service string) error
	waitReady(service string) error
	// drain takes a replica out of the proxy and waits until its requests
	// are answered; undrain puts it back.
	drain(s certsStep) error
	undrain(s certsStep) error
	// wait sleeps, the pin overlap's grace period.
	wait(d time.Duration)
}

// certsStep is one replica a renewal restarts.
type certsStep struct {
	Service string
	// Base is the compose service it is a copy of ("auth", "edge", "central");
	// Slot its slot. Only a rolling renewal looks at them.
	Base string
	Slot int
}

// certsPlan is what a renewal restarts, and how.
type certsPlan struct {
	Central, Auth, Edge []certsStep
	// Rolling (vps): the replicas of auth and edge are restarted one at a time,
	// each out of the proxy and drained first, and the server leaves are
	// replaced with an overlap of pins (prepare, commit). Not rolling (local):
	// mtls-init replaces what is due in place and everything is restarted, in
	// order, one waited for after the other.
	Rolling bool
	// PinWait is how long a staged pin is left to reach edges outside this
	// host before the leaf is replaced: they learn it from central, which
	// reads it when it starts. 0: there are none (nothing is published).
	PinWait time.Duration
}

// renewCerts runs mtls-init and restarts what a renewal affects, in an order
// that never leaves a caller without a certificate it trusts:
//
//   - a server leaf that is due is first staged (prepare). Edges restart, one
//     replica at a time, to learn the next pin; central restarts to publish it
//     to remote edges, and the overlap is waited out; only then is the leaf
//     replaced (commit) and central, then auth's replicas one at a time,
//     restarted onto it. An edge never meets a leaf it does not pin.
//   - a leaf replaced in place (an old versola-tools, or restart forced) and a
//     forced restart go central, auth, edge: edge last, so its pins are read
//     once the leaf is the new one.
//   - only the edge's identity: the edge's replicas.
//
// Silent when nothing was due and restart is not set.
func renewCerts(ops certsOps, plan certsPlan, restart bool, out io.Writer) error {
	var prep, ident initResult
	var err error
	if prep, err = runPhase(ops, phaseOf(plan, phasePrepare)); err != nil {
		return err
	}
	if plan.Rolling {
		if ident, err = runPhase(ops, phaseIdentity); err != nil {
			return err
		}
	}
	issued := append(append([]string(nil), prep.issued...), ident.issued...)
	staged := prep.staged
	if len(staged) == 0 && len(issued) == 0 && !prep.changed && !ident.changed && !restart {
		return nil
	}
	for _, name := range staged {
		fmt.Fprintf(out, "Staged a new %s (its pin is published before it is used)\n", name)
	}
	for _, name := range issued {
		fmt.Fprintf(out, "Renewed %s\n", name)
	}

	switch {
	case len(staged) > 0:
		return renewStaged(ops, plan, out)
	case restart || hasLeaf(issued) || (!plan.Rolling && (prep.changed || len(issued) > 0)):
		return restartAll(ops, plan, out)
	case plan.Rolling:
		// Only the identity (or an old image's "changed" with no names).
		return restartGroup(ops, plan, plan.Edge, out, nil)
	}
	return nil
}

// phaseOf: a rolling renewal asks for the phase; the others run mtls-init's
// default, which is all they ever did.
func phaseOf(plan certsPlan, phase string) string {
	if plan.Rolling {
		return phase
	}
	return ""
}

func runPhase(ops certsOps, phase string) (initResult, error) {
	output, err := ops.runInit(phase)
	if err != nil {
		return initResult{}, fmt.Errorf("%s failed: %w\n%s", MTLSInitService, err, bytes.TrimSpace(output))
	}
	return parseMTLSInit(output), nil
}

// hasLeaf: a server leaf (not only the edge's identity) was replaced.
func hasLeaf(issued []string) bool {
	for _, n := range issued {
		if n == "server" || n == "central-server" {
			return true
		}
	}
	return false
}

// renewStaged: the pin overlap. Resumable: prepare says "staged" again for a
// replacement that is already staged, so a renewal cut off anywhere is
// finished by running it again.
func renewStaged(ops certsOps, plan certsPlan, out io.Writer) error {
	resume := "run `versola certs renew` again to continue (nothing has been replaced yet)"
	// 1. The edges learn the next pin (and a new identity, if there is one).
	if err := restartGroup(ops, plan, plan.Edge, out, errors.New(resume)); err != nil {
		return err
	}
	// 2. Remote edges learn it from central, which reads the pin files when it
	//    starts; they are given time to fetch it.
	if plan.PinWait > 0 {
		for _, s := range plan.Central {
			if err := restartOne(ops, s, out); err != nil {
				return fmt.Errorf("%w -- %s", err, resume)
			}
		}
		fmt.Fprintf(out, "Waiting %s for edges outside this host to learn the next pin...\n", plan.PinWait)
		ops.wait(plan.PinWait)
	}
	// 3. Replace the leaves.
	if _, err := runPhase(ops, phaseCommit); err != nil {
		return fmt.Errorf("%w -- %s", err, resume)
	}
	// 4. Central, then auth, onto the new leaves. From here on, a failure
	//    leaves the old leaf in whoever has not restarted: --restart.
	return restartTail(ops, plan, out)
}

// restartTail restarts central and auth onto leaves that are replaced.
// after are the replicas still to come once it is done (the edge, when the
// tail is not the end of it), named when a step fails.
func restartTail(ops certsOps, plan certsPlan, out io.Writer, after ...certsStep) error {
	for i, s := range plan.Central {
		if err := restartOne(ops, s, out); err != nil {
			rest := append(append([]certsStep(nil), plan.Central[i+1:]...), plan.Auth...)
			return notRestarted(append(rest, after...), err)
		}
	}
	return restartGroup(ops, plan, plan.Auth, out, nil, after...)
}

// restartAll: central, auth, edge, one waited for after the other.
func restartAll(ops certsOps, plan certsPlan, out io.Writer) error {
	if err := restartTail(ops, plan, out, plan.Edge...); err != nil {
		return err
	}
	return restartGroup(ops, plan, plan.Edge, out, nil)
}

// restartOne restarts a replica and waits for it.
func restartOne(ops certsOps, s certsStep, out io.Writer) error {
	fmt.Fprintf(out, "Restarting %s...\n", s.Service)
	if err := ops.restart(s.Service); err != nil {
		return fmt.Errorf("couldn't restart %s: %w", s.Service, err)
	}
	if err := ops.waitReady(s.Service); err != nil {
		return fmt.Errorf("%s never became ready after its restart: %w", s.Service, err)
	}
	return nil
}

// restartGroup restarts the replicas of one service. Rolling, with other
// replicas to serve meanwhile, each goes out of the proxy and is drained
// first, and goes back in once ready; a service with one replica is restarted
// where it stands, which is what a restart of it always was (add a replica --
// `versola replica add` -- to restart it without a gap). A failure stops the
// rest, with what still runs on the old certificates named -- or, when cause
// is set, with it (the renewal can be resumed).
func restartGroup(ops certsOps, plan certsPlan, group []certsStep, out io.Writer, cause error, after ...certsStep) error {
	for i, s := range group {
		rolling := plan.Rolling && len(group) > 1
		if rolling {
			fmt.Fprintf(out, "Taking %s out of the proxy...\n", s.Service)
			if err := ops.drain(s); err != nil {
				return failedStep(append(append([]certsStep(nil), group[i:]...), after...), fmt.Errorf("couldn't take %s out of the proxy: %w", s.Service, err), cause)
			}
		}
		if err := restartOne(ops, s, out); err != nil {
			// Still out of the proxy: the others serve. `certs renew --restart`
			// (the recovery the error names) routes to it again.
			if rolling {
				err = fmt.Errorf("%w (it stays out of the proxy; the other replicas serve)", err)
			}
			if cause == nil {
				err = notRestarted(append(append([]certsStep(nil), group[i+1:]...), after...), err)
			} else {
				err = fmt.Errorf("%w -- %v", err, cause)
			}
			return err
		}
		if rolling {
			if err := ops.undrain(s); err != nil {
				return failedStep(append(append([]certsStep(nil), group[i+1:]...), after...), fmt.Errorf("%s is ready but the proxy does not route to it again: %w", s.Service, err), cause)
			}
		}
	}
	return nil
}

func failedStep(rest []certsStep, err, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w -- %v", err, cause)
	}
	return notRestarted(rest, err)
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

// initResult is what mtls-init's output says happened.
type initResult struct {
	// issued: leaves replaced in place. staged: replacements staged beside the
	// current pair (waiting for commit). committed: staged ones moved into place.
	issued, staged, committed []string
	changed                   bool
}

// parseMTLSInit reads mtls-init's output: the leaves it issued, staged and
// committed, and whether it says it changed anything.
func parseMTLSInit(output []byte) initResult {
	var r initResult
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == mtlsInitChanged:
			r.changed = true
		case strings.HasPrefix(line, mtlsInitIssued):
			r.issued = append(r.issued, strings.TrimPrefix(line, mtlsInitIssued))
		case strings.HasPrefix(line, mtlsInitStaged):
			r.staged = append(r.staged, strings.TrimPrefix(line, mtlsInitStaged))
		case strings.HasPrefix(line, mtlsInitCommitted):
			r.committed = append(r.committed, strings.TrimPrefix(line, mtlsInitCommitted))
		}
	}
	return r
}

// RenewCertsOptions: see `versola certs renew`.
type RenewCertsOptions struct {
	// Restart restarts the services even when nothing was renewed.
	Restart bool
	// PinWait is how long a staged pin is left to reach edges outside this
	// host before the leaf is replaced (see certsPlan); used only when a
	// mutual-TLS name is published. -1 means the default.
	PinWait time.Duration
}

// DefaultPinWait is how long central's pin set is left to reach remote edges
// before a server leaf is replaced: several of their sync intervals.
const DefaultPinWait = 15 * time.Minute

// RenewCerts renews the mutual-TLS leaves of the deployment st records that
// are due (a third of their lifetime or less left) and restarts what uses
// them: on vps rolling, with the next pin published first; on local central,
// auth and edge in that order.
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
	if central, err = frontCentral(central, isVps); err != nil {
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
	plan := certsPlan{Rolling: isVps}
	stepsOf := func(group []replica) []certsStep {
		var out []certsStep
		for _, r := range group {
			out = append(out, certsStep{Service: r.Service, Base: r.Base, Slot: r.Slot})
		}
		return out
	}
	plan.Central, plan.Auth, plan.Edge = stepsOf(central), stepsOf(auth), stepsOf(edge)
	if isVps && (st.MTLSDomain != "" || st.CentralMTLSDomain != "") {
		plan.PinWait = opts.PinWait
		if plan.PinWait < 0 {
			plan.PinWait = DefaultPinWait
		}
	}
	ops := dockerCertsOps{composePath: composePath, readyURLs: readyURLs}
	if isVps {
		fronts, genDir, err := proxyRouting(topo, true)
		if err != nil {
			return err
		}
		ops.router = &certsRouter{
			ops:  dockerReplicaOps{dir: filepath.Dir(composePath), composePath: composePath, mode: st.ProxyMode},
			mode: st.ProxyMode, fronts: fronts, genDir: genDir, central: central, auth: auth, edge: edge,
		}
	}
	return renewCerts(ops, plan, opts.Restart, os.Stdout)
}

// certsRouter takes replicas out of the proxy and back, the way a rollout
// does (routedConfig, routeVia, waitDrained).
type certsRouter struct {
	ops                 replicaOps
	mode                string
	fronts              proxy.Fronts
	genDir              string
	central, auth, edge []replica
	// out are the slots taken out, by service.
	out map[string]map[int]bool
}

func (r *certsRouter) route() error {
	pick := func(svc string, all []replica) []replica {
		var in []replica
		for _, rep := range all {
			if !r.out[svc][rep.Slot] {
				in = append(in, rep)
			}
		}
		return in
	}
	c := routedConfig(r.mode, r.fronts, r.genDir, pick(CentralService, r.central), pick(AuthService, r.auth), pick(EdgeService, r.edge))
	return routeVia(r.ops, r.mode, c)
}

func (r *certsRouter) drain(s certsStep) error {
	if r.out == nil {
		r.out = map[string]map[int]bool{}
	}
	if r.out[s.Base] == nil {
		r.out[s.Base] = map[int]bool{}
	}
	r.out[s.Base][s.Slot] = true
	if err := r.route(); err != nil {
		delete(r.out[s.Base], s.Slot)
		_ = r.route() // best effort: back to routing to it, as before
		return err
	}
	if running, err := r.ops.isRunning(proxy.ContainerFor(r.mode)); err == nil && running {
		r.ops.waitDrained()
	}
	return nil
}

func (r *certsRouter) undrain(s certsStep) error {
	delete(r.out[s.Base], s.Slot)
	return r.route()
}

type dockerCertsOps struct {
	composePath string
	readyURLs   map[string]string
	// router drains replicas (vps); nil on local, where nothing is rolling.
	router *certsRouter
}

func (o dockerCertsOps) runInit(phase string) ([]byte, error) {
	args := []string{"run", "--rm", "-T"}
	if phase != "" {
		args = append(args, "-e", "MTLS_INIT_PHASE="+phase)
	}
	c := docker.Cmd(state.ComposeArgs(o.composePath, append(args, MTLSInitService)...)...)
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

func (o dockerCertsOps) drain(s certsStep) error {
	if o.router == nil {
		return nil
	}
	return o.router.drain(s)
}

func (o dockerCertsOps) undrain(s certsStep) error {
	if o.router == nil {
		return nil
	}
	return o.router.undrain(s)
}

func (o dockerCertsOps) wait(d time.Duration) { time.Sleep(d) }
