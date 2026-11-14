package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/deploy"
)

var edgePartnerOpts deploy.EdgePartnerOptions

var edgePartnerCmd = &cobra.Command{
	Use:   "partner [<token-file>]",
	Short: "For a partner of the operator: write the configuration of the partner's own edge and start it",
	Long: `partner runs on a partner's machine: an organisation outside the operator that
runs only an edge, with its own Postgres, on its own host. It writes the edge's
directory (~/.versola/edge/<host of --edge-url>, or --dir) -- a complete
configuration (edge.conf, mode = "partner"), the token, the pins, a compose
file with a volume for the edge's identity -- and starts it with Docker Compose.

  versola edge partner enrollment-token.txt --version 0.8.0 \
    --edge-url https://login.example.com \
    --central-pin central-mtls.crt --auth-pin id-mtls.crt \
    --admin +77010000000 --admin it@example.com \
    --postgres-url 'jdbc:postgresql://db.example.com:5432/edge?sslmode=verify-full' \
    --postgres-password-file edge-db.password \
    --console-dir ./console

What the operator hands over: the enrollment token (vtk1.<address>.<secret>, one
use, valid 24 hours) in a file, the certificates of central's mutual-TLS
listener (--central-pin) and of auth's mutual-TLS address (--auth-pin), PEM, the
listener's own certificate and not a CA, and the console's files (--console-dir:
partner.html, versola-partner.js, versola-partner-theme.js; the image has none).
Everything else -- this edge's id, its key, where central and auth are -- central
tells the edge when it enrolls, and the edge keeps it in its identity volume.
The operator's deployment must publish central's enrollment and mutual-TLS names
and auth's mutual-TLS name, and set them in central's configuration (see the
README, "Public names").

--edge-url (required) is the https origin of the edge and its console: put
your reverse proxy, with TLS for it, in front of 127.0.0.1:8095. --admin
(required, repeatable) is a first administrator of the console: a phone number
in international format or an email address, which they sign in with, verified,
at the operator's sign-in. --postgres-url (required) is the JDBC URL of the
edge's own database, which the edge migrates at start (verified TLS,
sslmode=verify-full, for a database not on this host); --postgres-user
(default edge); --postgres-password-file a file holding its password.

Keep the identity volume (edge-identity of the compose project): without it
the edge needs a new token at every start, and a token cannot be used twice --
central refuses a second enrollment of an enrolled edge, so a lost volume means
asking the operator to register the edge anew. Once the edge has enrolled it no
longer reads the token. Run partner again, without the token file, with the
same flags to change them (the token file in the directory is then kept).

edge.conf and compose.yml are rewritten on every run; edge.local.conf is yours
(never overwritten, included last). edge.secrets.env (0600) is merged, never
replaced: the edge's own keys are generated into it once and kept (a new
EDGE_TOKEN_ENC_KEY would lose the stored sessions and the team);
--postgres-password-file replaces only POSTGRES_PASSWORD. Without a database
password the files are written but the edge is not started.

--no-start writes the files and stops.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEdgePartner,
}

func init() {
	f := edgePartnerCmd.Flags()
	f.StringVar(&edgePartnerOpts.Version, "version", "", "required: the Versola release of the edge's image (as the operator tells you)")
	f.StringVar(&edgePartnerOpts.EdgeURL, "edge-url", "", "required: the https origin of this edge and its console (https://login.<your domain>)")
	f.StringArrayVar(&edgePartnerOpts.CentralPinFiles, "central-pin", nil, "required, repeatable: a PEM file with the certificate of central's mutual-TLS listener")
	f.StringArrayVar(&edgePartnerOpts.AuthPinFiles, "auth-pin", nil, "required, repeatable: a PEM file with the certificate auth's mutual-TLS address presents")
	f.StringArrayVar(&edgePartnerOpts.Admins, "admin", nil, "required, repeatable: a first administrator of the console, a phone number (+77010000000) or an email")
	f.StringVar(&edgePartnerOpts.PostgresURL, "postgres-url", "", "required: the JDBC URL of the edge's own database (jdbc:postgresql://<host>:5432/<db>?sslmode=verify-full)")
	f.StringVar(&edgePartnerOpts.PostgresUser, "postgres-user", "edge", "the user the edge connects to its database as")
	f.StringVar(&edgePartnerOpts.PostgresPasswordFile, "postgres-password-file", "", "a file holding the database password: it goes into edge.secrets.env (POSTGRES_PASSWORD), never printed")
	f.StringVar(&edgePartnerOpts.ConsoleDir, "console-dir", "", "the directory of the console's files (partner.html, versola-partner*.js), mounted read-only")
	f.StringVar(&edgePartnerOpts.Dir, "dir", "", "where the edge's files go (default: ~/.versola/edge/<host of --edge-url>)")
	f.BoolVar(&edgePartnerOpts.NoStart, "no-start", false, "write the files and stop")
	edgeCmd.AddCommand(edgePartnerCmd)
}

func runEdgePartner(cmd *cobra.Command, args []string) error {
	o := edgePartnerOpts
	if len(args) == 1 {
		o.TokenFile = args[0]
	}
	return edgePartner(os.Stdout, os.Stderr, o, deploy.EdgePartner)
}

// edgePartner does what `edge partner` does and says what comes next, on
// stderr: never a secret, only where things are.
func edgePartner(stdout, stderr io.Writer, o deploy.EdgePartnerOptions, do func(deploy.EdgePartnerOptions) (deploy.EdgePartnerResult, error)) error {
	res, err := do(o)
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(stderr, "Warning: %s\n", w)
	}
	compose := filepath.Join(res.Dir, "compose.yml")
	fmt.Fprintf(stderr, "Wrote the edge's files to %s (Compose project %s).\n", res.Dir, res.Project)
	if res.EnrollAddress != "" {
		fmt.Fprintf(stderr, "The token enrolls at %s, once.\n", res.EnrollAddress)
	}
	fmt.Fprintln(stderr, "Fingerprints of the pinned certificates (compare them with what the operator gives you):")
	for _, p := range res.CentralPins {
		fmt.Fprintf(stderr, "  central  %s\n", deploy.Fingerprint(p))
	}
	for _, p := range res.AuthPins {
		fmt.Fprintf(stderr, "  auth     %s\n", deploy.Fingerprint(p))
	}
	switch {
	case len(res.Missing) > 0:
		fmt.Fprintf(stderr, "%s still lacks:\n", filepath.Join(res.Dir, "edge.secrets.env"))
		for _, m := range res.Missing {
			fmt.Fprintf(stderr, "  %s\n", m)
		}
		fmt.Fprintln(stderr, "The edge was not started: it cannot start without it. Run the same command again, adding that flag\n(the token file may be left out then: the one in the directory is kept).")
		return nil
	case !res.Started:
		fmt.Fprintf(stderr, "Not started (--no-start). Start it with:\n  docker compose -f %s up -d\n", compose)
	default:
		fmt.Fprintln(stdout, "The edge is starting.")
	}
	fmt.Fprintf(stderr, `Next:
  - its log: docker compose -f %[1]s logs -f edge
    (the first start enrolls with the token; later starts log that the token is no longer read)
  - put your reverse proxy, with TLS for %[2]s, in front of 127.0.0.1:8095
  - once it runs, open %[2]s/partner/: a first administrator signs in at the operator's
    sign-in with the phone number or email given in --admin, verified there%[4]s
  - keep the identity volume (%[3]s_edge-identity): never `+"`docker compose down -v`"+`; without it
    the edge needs a new token, which only the operator can issue
`, compose, strings.TrimSuffix(o.EdgeURL, "/"), res.Project, noConsoleNote(o.ConsoleDir))
	return nil
}

// noConsoleNote: without --console-dir the edge serves no console, and
// /partner/ answers 404.
func noConsoleNote(consoleDir string) string {
	if consoleDir != "" {
		return ""
	}
	return "\n    (no --console-dir: /partner/ answers 404 until you run this again with the console's files)"
}
