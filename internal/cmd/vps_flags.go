package cmd

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

// vpsFlags are --auth-url, --proxy, --acme-staging, --setup-openbao and
// --postgres-host as one configure (or bootstrap) run uses them: on vps, a
// flag left out takes the value of the deployment this machine tracks (see
// deploy.StoredVps), so a redeploy or a version change is `versola configure
// vps <version>` with nothing to remember -- the same rule --postgres-db,
// --postgres-ca, the public names, --cert-lifetime-days and --est already
// follow, and the one `rollout` follows for all of them. A flag given on the
// command line always wins.
//
// The values taken from the record are never used silently: printVpsSettings
// shows them before the confirmation question, which vps always asks.
type vpsFlags struct {
	AuthURL      string
	Proxy        string
	ACMEStaging  bool
	SetupOpenBao bool
	// PostgresHost is the flag as given: which Postgres a deployment uses is
	// decided, from it and the record, by deploy.Configure (resolvePostgres),
	// which also refuses a switch away from the bundled one.
	PostgresHost string

	// recorded is the deployment the values were taken from; nil on a first
	// deployment, on local, or when the record cannot be read.
	recorded *deploy.RecordedVps
	// given: the flags passed on the command line, by name.
	given map[string]bool
	// droppedStaging: the record has --acme-staging, but with this run's
	// --proxy or --auth-url Versola's proxy no longer gets a certificate
	// itself, so it is left out rather than refused (deploy.Configure refuses
	// an explicit --acme-staging there).
	droppedStaging bool
}

var rememberedVpsFlags = []string{"auth-url", "proxy", "acme-staging", "setup-openbao", "postgres-host"}

// resolveVpsFlags reads the five flags of cmd and, for vps, fills in the ones
// not given from the recorded vps deployment. Call it with the state lock
// held, so the record it reads is the one the run then replaces.
func resolveVpsFlags(cmd *cobra.Command, target string) (vpsFlags, error) {
	f := cmd.Flags()
	var v vpsFlags
	var err error
	if v.AuthURL, err = f.GetString("auth-url"); err != nil {
		return v, err
	}
	if v.Proxy, err = f.GetString("proxy"); err != nil {
		return v, err
	}
	if v.PostgresHost, err = f.GetString("postgres-host"); err != nil {
		return v, err
	}
	if v.ACMEStaging, err = f.GetBool("acme-staging"); err != nil {
		return v, err
	}
	if v.SetupOpenBao, err = f.GetBool("setup-openbao"); err != nil {
		return v, err
	}
	if target != "vps" {
		return v, nil
	}
	v.given = map[string]bool{}
	for _, name := range rememberedVpsFlags {
		if f.Changed(name) {
			v.given[name] = true
		}
	}
	if r := deploy.StoredVps(); r != nil {
		v.recorded = r
		if !v.given["auth-url"] {
			v.AuthURL = r.AuthURL
		}
		// "" only in a record from before the CLI ran the proxy itself: the
		// flag's default then, as on a first deployment.
		if !v.given["proxy"] && r.ProxyMode != "" {
			v.Proxy = r.ProxyMode
		}
		if !v.given["setup-openbao"] {
			v.SetupOpenBao = r.OpenBaoByHand
		}
		if !v.given["acme-staging"] && r.ACMEStaging {
			if stagingApplies(v.AuthURL, v.Proxy) {
				v.ACMEStaging = true
			} else {
				v.droppedStaging = true
			}
		}
	}
	// Checked here, before anything is pulled or started (deploy.Configure
	// checks again for its own callers): a faster, clearer error than one
	// from deep inside a versola-tools run.
	if v.AuthURL == "" {
		// A record written by a newer versola is not "nothing recorded":
		// asking for --auth-url would only lead to deploy.Configure's
		// refusal of that record once it is given. Say the real reason.
		if _, err := state.Load(); errors.Is(err, state.ErrNewerSchema) {
			return v, err
		}
		return v, fmt.Errorf("--auth-url is required for vps deployments (e.g. --auth-url https://id.example.com)")
	}
	return v, nil
}

// stagingApplies: --acme-staging only means something when Versola's proxy
// gets the certificate itself -- --proxy nginx and an https --auth-url (the
// rule deploy.Configure enforces). An invalid value counts as "applies": the
// value is kept and deploy.Configure reports the invalid flag itself.
func stagingApplies(authURL, mode string) bool {
	m, err := proxy.ParseMode(mode)
	if err != nil {
		return true
	}
	auth, err := proxy.ParseAuthURL(authURL, m)
	if err != nil {
		return true
	}
	return auth.TLS(m)
}

// printVpsSettings shows the settings this run uses when any of them came
// from the recorded deployment, each marked when it was given on the command
// line instead. Nothing on a first deployment: every value there is one the
// operator has just typed.
func printVpsSettings(w io.Writer, v vpsFlags) {
	if v.recorded == nil {
		return
	}
	mark := func(name string) string {
		if v.given[name] {
			return "  (given now)"
		}
		return ""
	}
	fmt.Fprintln(w, "\nUsing the recorded settings of this deployment (pass a flag to change one):")
	row := func(label, value, name string) {
		fmt.Fprintf(w, "  %-16s %s%s\n", label, value, mark(name))
	}
	row("--auth-url", v.AuthURL, "auth-url")
	row("--proxy", v.Proxy, "proxy")
	if v.given["postgres-host"] {
		row("Postgres", "your own, at "+v.PostgresHost, "postgres-host")
	} else {
		row("Postgres", v.recorded.PostgresText(), "")
	}
	switch {
	case v.ACMEStaging:
		row("--acme-staging", "on: the certificate comes from Let's Encrypt staging (untrusted)", "acme-staging")
	case v.droppedStaging:
		row("--acme-staging", "not kept: it only applies to --proxy nginx with an https --auth-url", "")
	case v.given["acme-staging"]:
		row("--acme-staging", "off: the certificate comes from Let's Encrypt", "acme-staging")
	}
	switch {
	case v.SetupOpenBao:
		row("--setup-openbao", `on: OpenBao is set up by hand, credentials from "versola secrets login vps"`, "setup-openbao")
	case v.given["setup-openbao"]:
		row("--setup-openbao", "off: this CLI provisions OpenBao", "setup-openbao")
	}
}

// confirmVpsRun prints the settings, then asks the vps confirmation question
// (deploy.ConfirmVpsDeployYes): the settings come first so that the answer is
// given with them on screen.
func confirmVpsRun(w io.Writer, v vpsFlags, yes bool, action string) error {
	printVpsSettings(w, v)
	return deploy.ConfirmVpsDeployYes(yes, action)
}

// The three visible vps flags of configure and bootstrap keep a one-line
// description, so that `--help` fits a screen; what they leave out (the
// remembered values, switching Postgres, the ports each proxy mode needs) is
// in the README, "Redeploying on the same server", "Postgres on vps" and
// "Deploying to a VPS: the reverse proxy".

const authURLUsage = "vps: public URL of auth, e.g. https://id.example.com (needed the first time, then remembered)"

const postgresHostUsage = "vps: host:port of your own Postgres (default: bundled; remembered)"

const proxyUsage = "vps: \"nginx\" (Versola's nginx serves 80/443) or \"external\" (behind your web server, on 127.0.0.1:2821); remembered"

const acmeStagingUsage = "vps only: get the certificate from Let's Encrypt's staging environment (untrusted, but no rate limits) -- for test deployments. Remembered from the last configure vps (left out when --proxy or --auth-url no longer let Versola's proxy get the certificate); --acme-staging=false goes back to a trusted certificate"

const setupOpenBaoUsage = "vps only: OpenBao is already set up yourself — skip auto-provisioning and require credentials from \"versola secrets login vps\". Remembered from the last configure vps (a deployment whose OpenBao this CLI did not provision); --setup-openbao=false goes back to auto-provisioning"
