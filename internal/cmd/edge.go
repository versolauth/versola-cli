package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
	"github.com/versolauth/versola-cli/internal/state"
)

var (
	edgeAddGroup    string
	edgeAddExisting bool
	edgeAddOut      string
	edgeAddKeyOut   string

	edgeBootstrapDir     string
	edgeBootstrapKeyFile string
	edgeBootstrapVersion string
	edgeBootstrapNoStart bool
)

var edgeCmd = &cobra.Command{
	Use:   "edge",
	Short: "Register an edge that runs elsewhere, and set one up",
}

var edgeAddCmd = &cobra.Command{
	Use:   "add <id>",
	Short: "Register a remote edge with central and print its bootstrap bundle",
	Long: `add registers an edge with the central of this vps deployment (the admin
API, on this host) and prints a bootstrap bundle -- what an operator carries to
the machine that will run it: the edge's id, a one-time enrollment token
(valid for an hour, spent by the first enrollment), where to enroll and where
to connect afterwards, and the certificates of central's and auth's listeners
to pin. The bundle holds nothing that can be used again.

  versola edge add eu-central --group eu > eu-central.json

--group puts the edge into a group of edges (central creates and classifies
groups; the default group is the first-party one). The group of an edge does
not change.

central shows the key it registers the edge with once. add writes it to a file
of its own (--key-out, default ./<id>.edge-key.json, mode 0600, never
overwritten) -- carry it by another channel than the bundle, and pass it to
"versola edge bootstrap --key-file".

--existing issues a new token for an edge that is already registered (its
identity volume was lost, the token expired); nothing is registered and no key
is written.

A remote edge needs the names it connects to: configure the deployment with
--central-domain and --central-mtls-domain first (and --mtls-domain, to
reach auth's mutual-TLS listener). The bundle goes to stdout (or -o); what is
for the operator to read is on stderr.`,
	Args: cobra.ExactArgs(1),
	RunE: runEdgeAdd,
}

var edgeBootstrapCmd = &cobra.Command{
	Use:   "bootstrap <file>",
	Short: "On a remote machine: set an edge up from its bootstrap bundle and start it",
	Long: `bootstrap runs on the machine that is to run the edge. It reads the bundle
"versola edge add" printed and writes the edge's directory
(~/.versola/edge/<id>, or --dir): its configuration -- where to enroll, where
to connect, which listener certificates to pin --, the one-time token, a
compose file with a volume for its identity (the key and certificate it
enrolls for; keep it: without it the edge enrolls again at every start, and
the token is spent), and then starts it with Docker Compose.

  versola edge bootstrap eu-central.json --key-file eu-central.edge-key.json

edge.local.conf and edge.secrets.env in that directory are yours: created
empty if missing, never overwritten (edge.secrets.env is, only when --key-file
is given, to carry the key central registered the edge with). Everything
else the edge needs -- its public URL, keys of its own -- goes into
edge.local.conf, which edge.conf includes last.

The token expires an hour after "edge add": a late bundle is refused, and
"versola edge add <id> --existing" on the central's machine issues a new
token. A partner's edge does not use this: it has no connection to central.

--no-start writes the files and stops.`,
	Args: cobra.ExactArgs(1),
	RunE: runEdgeBootstrap,
}

func init() {
	edgeAddCmd.Flags().StringVar(&edgeAddGroup, "group", "", "the group of edges to register it in (default: central's default, first-party, group)")
	edgeAddCmd.Flags().BoolVar(&edgeAddExisting, "existing", false, "the edge is already registered: only issue a new enrollment token")
	edgeAddCmd.Flags().StringVarP(&edgeAddOut, "output", "o", "", "write the bundle to this file (mode 0600, not overwritten) instead of stdout")
	edgeAddCmd.Flags().StringVar(&edgeAddKeyOut, "key-out", "", "write the key central registers the edge with to this file (default: ./<id>.edge-key.json; mode 0600, not overwritten)")
	edgeBootstrapCmd.Flags().StringVar(&edgeBootstrapDir, "dir", "", "where the edge's files go (default: ~/.versola/edge/<id>)")
	edgeBootstrapCmd.Flags().StringVar(&edgeBootstrapKeyFile, "key-file", "", "the key file `versola edge add` wrote: its key goes into the edge's secrets")
	edgeBootstrapCmd.Flags().StringVar(&edgeBootstrapVersion, "version", "", "run this Versola release instead of the one the bundle names")
	edgeBootstrapCmd.Flags().BoolVar(&edgeBootstrapNoStart, "no-start", false, "write the files and stop")
	edgeCmd.AddCommand(edgeAddCmd, edgeBootstrapCmd)
}

func runEdgeAdd(cmd *cobra.Command, args []string) error {
	return edgeAdd(os.Stdout, os.Stderr, args[0], edgeAddGroup, edgeAddExisting, edgeAddOut, edgeAddKeyOut, edgeAddDo)
}

// edgeAddDo is deploy.EdgeAdd against the deployment this machine tracks,
// replaced in tests.
func edgeAddDo(o deploy.EdgeAddOptions) (deploy.EdgeAddResult, error) {
	st, err := state.Load()
	if err != nil {
		return deploy.EdgeAddResult{}, err
	}
	return deploy.EdgeAdd(st, o)
}

// edgeAdd does what `edge add` does: asks for the bundle, writes the key file
// first (central shows the key once: it must not be lost to a failure after
// the registration), then the bundle.
func edgeAdd(stdout, stderr io.Writer, id, group string, existing bool, out, keyOut string, do func(deploy.EdgeAddOptions) (deploy.EdgeAddResult, error)) error {
	if err := deploy.ValidEdgeID(id); err != nil {
		return err
	}
	if keyOut == "" {
		keyOut = id + ".edge-key.json"
	}
	// Refused before anything is registered: the key has to land somewhere.
	if !existing {
		if _, err := os.Stat(keyOut); err == nil {
			return fmt.Errorf("%s exists: pass another --key-out, or remove it (central shows the edge's key once, and it must not overwrite another)", keyOut)
		}
	}
	if out != "" {
		if _, err := os.Stat(out); err == nil {
			return fmt.Errorf("%s exists: it is not overwritten", out)
		}
	}
	res, doErr := do(deploy.EdgeAddOptions{ID: id, Group: group, Existing: existing})
	// Written before any error is reported: central shows the key once, and a
	// failure after the registration still returns it.
	if res.PrivateKey != "" {
		key, err := deploy.EdgeKeyFile(id, res.KeyID, res.PrivateKey)
		if err != nil {
			return err
		}
		if err := writeNew(keyOut, key); err != nil {
			return fmt.Errorf("the edge %s is registered, but its key could not be written (%v) -- central shows it once; rotate the edge's key in central to get a new one", id, err)
		}
		fmt.Fprintf(stderr, "Wrote the edge's key to %s (mode 0600). Carry it by another channel than the bundle.\n", keyOut)
	}
	if doErr != nil {
		return doErr
	}
	bundle, err := deploy.EncodeBundle(res.Bundle)
	if err != nil {
		return err
	}
	if out != "" {
		if err := writeNew(out, bundle); err != nil {
			return fmt.Errorf("couldn't write the bundle: %w", err)
		}
		fmt.Fprintf(stderr, "Wrote the bundle to %s (mode 0600).\n", out)
	} else if _, err := stdout.Write(bundle); err != nil {
		return err
	}
	b := res.Bundle
	fmt.Fprintf(stderr, "\nEdge %s%s: enrolls at %s, connects to %s. The token expires %s and works once.\n", b.EdgeID, groupNote(b.Group), b.EnrollURL, b.CentralURL, b.TokenExpiresAt.Local().Format(time.RFC3339))
	fmt.Fprintln(stderr, "Fingerprints of the pinned certificates (compare them with what the edge machine shows):")
	for _, p := range b.CentralPins {
		fmt.Fprintf(stderr, "  central  %s\n", deploy.Fingerprint(p))
	}
	for _, p := range b.AuthPins {
		fmt.Fprintf(stderr, "  auth     %s\n", deploy.Fingerprint(p))
	}
	fmt.Fprintln(stderr, "On the edge's machine: versola edge bootstrap <bundle file> --key-file <key file>")
	return nil
}

func groupNote(group string) string {
	if group == "" {
		return ""
	}
	return " (group " + group + ")"
}

// writeNew creates a file 0600 and fails if it exists.
func writeNew(path string, content []byte) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func runEdgeBootstrap(cmd *cobra.Command, args []string) error {
	dir, err := deploy.EdgeBootstrap(deploy.EdgeBootstrapOptions{
		File: args[0], Dir: edgeBootstrapDir, KeyFile: edgeBootstrapKeyFile, Version: edgeBootstrapVersion, NoStart: edgeBootstrapNoStart,
	}, time.Now())
	if err != nil {
		return err
	}
	if edgeBootstrapNoStart {
		fmt.Printf("Wrote the edge's files to %s. Start it with: docker compose -f %s up -d\n", dir, filepath.Join(dir, "compose.yml"))
		return nil
	}
	fmt.Printf("The edge is starting from %s. Its identity volume is edge-identity: keep it.\n", dir)
	return nil
}
