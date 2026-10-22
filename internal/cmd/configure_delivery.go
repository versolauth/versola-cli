package cmd

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/versolauth/versola-cli/internal/state"
)

// `versola configure sms` and `versola configure smtp` keep the settings of the
// two ways auth delivers a one-time code. They are not asked for by `configure
// <target> <version>` on purpose: a deployment works without them (auth logs a
// warning instead of sending), so the first install stays short.
//
// They only remember what they are given (state.Delivery); the next configure
// or rollout hands it to versola-tools, which writes it into auth.conf, as it
// generates that file from scratch every time.

var (
	smsURL, smsMethod, smsUsername string
	smsBody                        []string
	smsPasswordStdin, smsNoAuth    bool
	smsDisable                     bool

	smtpHost, smtpUsername, smtpFrom, smtpSubject string
	smtpPort                                      int
	smtpStartTLS                                  bool
	smtpPasswordStdin, smtpNoAuth, smtpDisable    bool
)

const (
	smsPasswordEnv  = "VERSOLA_SMS_PASSWORD"
	smtpPasswordEnv = "VERSOLA_SMTP_PASSWORD"
)

var configureSMSCmd = &cobra.Command{
	Use:   "sms",
	Short: "Set the HTTP SMS provider auth sends one-time codes through",
	Long: `sms stores the settings of the SMS provider (auth.conf's otp-provider
block). Auth sends a code by POSTing a JSON object to the provider's URL;
--body says what is in it, and each value may contain {{phone}} (the number)
and {{message}} (the text, code included).

  versola configure sms --url https://sms.example.com/send \
      --username versola --password-stdin \
      --body phones={{phone}} --body mes={{message}}

Nothing is started or changed in a running deployment by this: the settings
are written into auth.conf by the next "versola configure <target> <version>"
(give it the same flags as the last time), which "versola up" then starts,
and a "versola rollout" keeps them. A flag you leave out keeps its stored value;
--body replaces the whole body. Without any provider configured auth only logs
"OTP provider is not configured".

The password is never a flag (it would stay in your shell history and in the
process list): --password-stdin reads it (without echo from a terminal), or set
` + smsPasswordEnv + `. It is kept in ~/.versola/active/` + state.DeliveryFileName + `, readable by you only,
and written as it is into auth.conf of the deployment. --no-auth drops the
stored username and password; --disable removes the provider altogether.

This needs a versola-tools release that passes the settings on (and, for
--body, one that accepts --otp-body-<name>); configure stops and says so when
the generated auth.conf does not carry them.`,
	Args: cobra.NoArgs,
	RunE: runConfigureSMS,
}

var configureSMTPCmd = &cobra.Command{
	Use:   "smtp",
	Short: "Set the SMTP server auth sends one-time codes by email through",
	Long: `smtp stores the settings of the SMTP server (auth.conf's smtp block).

  versola configure smtp --host smtp.example.com --port 587 \
      --username versola --password-stdin --from noreply@example.com \
      --subject "Your verification code"

Nothing is started or changed in a running deployment by this: see
"versola configure sms --help" for how the settings reach auth, and for how the
password is read and kept (here ` + smtpPasswordEnv + `). A flag you leave out keeps
its stored value; the first time --host and --from are required. --starttls
defaults to true (use --starttls=false for a server that does not offer it).
--no-auth drops the stored username and password; --disable removes the server.`,
	Args: cobra.NoArgs,
	RunE: runConfigureSMTP,
}

func init() {
	f := configureSMSCmd.Flags()
	f.StringVar(&smsURL, "url", "", "the provider's URL (http:// or https://)")
	f.StringVar(&smsMethod, "method", "", "HTTP method (default POST)")
	f.StringVar(&smsUsername, "username", "", "HTTP basic username")
	f.StringArrayVar(&smsBody, "body", nil, "an entry of the JSON body, name=value (repeatable; values may use {{phone}} and {{message}}); replaces the stored body")
	f.BoolVar(&smsPasswordStdin, "password-stdin", false, "read the HTTP basic password from stdin")
	f.BoolVar(&smsNoAuth, "no-auth", false, "drop the stored username and password")
	f.BoolVar(&smsDisable, "disable", false, "remove the SMS provider")

	g := configureSMTPCmd.Flags()
	g.StringVar(&smtpHost, "host", "", "SMTP server host")
	g.IntVar(&smtpPort, "port", 0, "SMTP server port (default 587)")
	g.StringVar(&smtpUsername, "username", "", "SMTP username")
	g.StringVar(&smtpFrom, "from", "", "sender address")
	g.StringVar(&smtpSubject, "subject", "", "subject of the message (default: auth's)")
	g.BoolVar(&smtpStartTLS, "starttls", true, "use STARTTLS")
	g.BoolVar(&smtpPasswordStdin, "password-stdin", false, "read the SMTP password from stdin")
	g.BoolVar(&smtpNoAuth, "no-auth", false, "drop the stored username and password")
	g.BoolVar(&smtpDisable, "disable", false, "remove the SMTP server")

	configureCmd.AddCommand(configureSMSCmd, configureSMTPCmd)
}

func runConfigureSMS(cmd *cobra.Command, _ []string) error {
	// Read before the lock: a prompt left open must not hold every other
	// command up.
	var password *string
	if !smsDisable {
		pw, ok, err := passwordInput(smsPasswordStdin, smsNoAuth, smsPasswordEnv, "SMS provider password")
		if err != nil {
			return err
		}
		if ok {
			password = &pw
		}
	}
	// Held across read-modify-write, and so that a configure running at the
	// same time does not read half a file.
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	d, err := state.LoadDelivery()
	if err != nil {
		return err
	}
	if smsDisable {
		if cmd.Flags().NFlag() > 1 {
			return fmt.Errorf("--disable takes no other flag")
		}
		if d.SMS == nil {
			fmt.Println("No SMS provider is stored.")
			return nil
		}
		d.SMS = nil
		if err := d.Save(); err != nil {
			return err
		}
		fmt.Println("Removed the SMS provider.")
		printApplyHint()
		return nil
	}
	in := smsInput{noAuth: smsNoAuth}
	fl := cmd.Flags()
	if fl.Changed("url") {
		in.url = &smsURL
	}
	if fl.Changed("method") {
		in.method = &smsMethod
	}
	if fl.Changed("username") {
		in.username = &smsUsername
	}
	if fl.Changed("body") {
		in.body = smsBody
		in.bodySet = true
	}
	in.password = password
	merged, err := mergeSMS(d.SMS, in)
	if err != nil {
		return err
	}
	d.SMS = merged
	if err := d.Save(); err != nil {
		return err
	}
	fmt.Printf("Stored the SMS provider: %s %s\n", merged.Method, shownURL(merged.URL))
	switch {
	case merged.Username == "" && merged.Password == "":
		fmt.Println("  credentials: none")
	case merged.Password == "":
		fmt.Printf("  credentials: username %s, no password\n", merged.Username)
	default:
		fmt.Printf("  credentials: username %s, password set\n", merged.Username)
	}
	if len(merged.Body) == 0 {
		fmt.Println("  body: empty -- the provider receives {} and will probably reject it; add --body name=value (e.g. --body phones={{phone}} --body mes={{message}})")
	} else {
		keys := make([]string, 0, len(merged.Body))
		for k := range merged.Body {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// Names only: a gateway's login or API key often goes in the body.
		fmt.Printf("  body entries: %s\n", strings.Join(keys, ", "))
	}
	printApplyHint()
	return nil
}

func runConfigureSMTP(cmd *cobra.Command, _ []string) error {
	var password *string // read before the lock, see runConfigureSMS
	if !smtpDisable {
		pw, ok, err := passwordInput(smtpPasswordStdin, smtpNoAuth, smtpPasswordEnv, "SMTP password")
		if err != nil {
			return err
		}
		if ok {
			password = &pw
		}
	}
	unlock, err := state.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	d, err := state.LoadDelivery()
	if err != nil {
		return err
	}
	if smtpDisable {
		if cmd.Flags().NFlag() > 1 {
			return fmt.Errorf("--disable takes no other flag")
		}
		if d.SMTP == nil {
			fmt.Println("No SMTP server is stored.")
			return nil
		}
		d.SMTP = nil
		if err := d.Save(); err != nil {
			return err
		}
		fmt.Println("Removed the SMTP server.")
		printApplyHint()
		return nil
	}
	in := smtpInput{noAuth: smtpNoAuth}
	fl := cmd.Flags()
	if fl.Changed("host") {
		in.host = &smtpHost
	}
	if fl.Changed("port") {
		in.port = &smtpPort
	}
	if fl.Changed("username") {
		in.username = &smtpUsername
	}
	if fl.Changed("from") {
		in.from = &smtpFrom
	}
	if fl.Changed("subject") {
		in.subject = &smtpSubject
	}
	if fl.Changed("starttls") {
		in.startTLS = &smtpStartTLS
	}
	in.password = password
	merged, err := mergeSMTP(d.SMTP, in)
	if err != nil {
		return err
	}
	d.SMTP = merged
	if err := d.Save(); err != nil {
		return err
	}
	fmt.Printf("Stored the SMTP server: %s:%d, from %s, STARTTLS %t\n", merged.Host, merged.Port, merged.From, merged.StartTLS)
	switch {
	case merged.Username == "" && merged.Password == "":
		fmt.Println("  credentials: none")
	case merged.Password == "":
		fmt.Printf("  credentials: username %s, no password\n", merged.Username)
	default:
		fmt.Printf("  credentials: username %s, password set\n", merged.Username)
	}
	printApplyHint()
	return nil
}

// shownURL is raw without what may be a secret: the user and password of its
// authority, the query and the fragment (an API key is often a query
// parameter). They are stored and used all the same.
func shownURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(the URL)"
	}
	hidden := u.User != nil || u.RawQuery != "" || u.Fragment != ""
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	if hidden {
		return u.String() + " (credentials and query not shown)"
	}
	return u.String()
}

// printApplyHint says what is still to do: the stored settings reach auth only
// through the next configure. Names the deployment this machine tracks, if any.
func printApplyHint() {
	fmt.Println()
	if st, err := state.Load(); err == nil {
		fmt.Printf("This machine tracks Versola %s on %s. The change reaches auth with the next `versola configure %s %s ...` and `versola up` (a `versola rollout` to another version keeps it).\n", st.Version, st.Target, st.Target, st.Version)
		return
	}
	fmt.Println("The change reaches auth with the next `versola configure <target> <version> ...` and `versola up`.")
}

// passwordInput is the password for a provider: read from stdin when asked for
// (without echo from a terminal), else from env when that is set; ok is false
// when neither was used and the stored one stays.
func passwordInput(fromStdin, noAuth bool, env, what string) (pw string, ok bool, err error) {
	if fromStdin {
		pw, err = readSecretLine("Password", what, true)
		if err != nil {
			return "", false, err
		}
		return pw, true, nil
	}
	// Not with --no-auth (it would put back what that drops), and said out
	// loud: a variable left in the shell must not replace a password unseen.
	if v, set := os.LookupEnv(env); set && v != "" && !noAuth {
		fmt.Printf("Using the password from %s.\n", env)
		return v, true, nil
	}
	return "", false, nil
}

type smsInput struct {
	url, method, username *string
	password              *string
	body                  []string
	bodySet, noAuth       bool
}

var (
	bodyKeyRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	smsMethodsAll = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "GET": true}
)

// plainValue rejects what gen-env would write into auth.conf unescaped, inside
// double quotes: a quote, a backslash or a line break would break the file (or
// let a value write HOCON of its own).
func plainValue(name, v string) error {
	if strings.ContainsAny(v, "\"\\\r\n") {
		return fmt.Errorf("%s must not contain a double quote, a backslash or a line break", name)
	}
	return nil
}

// mergeSMS applies in to cur (nil: nothing stored yet): what was not given keeps
// its stored value, and a first configuration needs the URL.
func mergeSMS(cur *state.SMS, in smsInput) (*state.SMS, error) {
	s := state.SMS{Method: "POST"}
	if cur != nil {
		s = *cur
		s.Body = make(map[string]string, len(cur.Body))
		for k, v := range cur.Body {
			s.Body[k] = v
		}
	}
	if in.url != nil {
		s.URL = strings.TrimSpace(*in.url)
	}
	if s.URL == "" {
		return nil, fmt.Errorf("--url is required (e.g. --url https://sms.example.com/send)")
	}
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("--url must be an absolute http:// or https:// address")
	}
	if err := plainValue("--url", s.URL); err != nil {
		return nil, err
	}
	if in.method != nil {
		s.Method = strings.ToUpper(strings.TrimSpace(*in.method))
	}
	if !smsMethodsAll[s.Method] {
		return nil, fmt.Errorf("--method must be POST, PUT, PATCH or GET")
	}
	if in.noAuth {
		s.Username, s.Password = "", ""
	}
	if in.username != nil {
		s.Username = *in.username
	}
	if in.password != nil {
		s.Password = *in.password
	}
	if err := plainValue("--username", s.Username); err != nil {
		return nil, err
	}
	if err := plainValue("the password", s.Password); err != nil {
		return nil, err
	}
	if (s.Username == "") != (s.Password == "") {
		// auth sends the Authorization header only when it has both.
		return nil, fmt.Errorf("the username and the password go together (auth sends credentials only when it has both): give the missing one, or use --no-auth")
	}
	if in.bodySet {
		s.Body = map[string]string{}
		for _, kv := range in.body {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || !bodyKeyRe.MatchString(k) || v == "" {
				return nil, fmt.Errorf("--body %q: want name=value, the name made of letters, digits and _", kv)
			}
			if err := plainValue("--body "+k, v); err != nil {
				return nil, err
			}
			s.Body[k] = v
		}
	}
	if len(s.Body) == 0 {
		s.Body = nil
	}
	return &s, nil
}

type smtpInput struct {
	host, username, from, subject, password *string
	port                                    *int
	startTLS                                *bool
	noAuth                                  bool
}

// mergeSMTP is mergeSMS for the SMTP server: the first configuration needs the
// host and the sender.
func mergeSMTP(cur *state.SMTP, in smtpInput) (*state.SMTP, error) {
	m := state.SMTP{Port: 587, StartTLS: true}
	if cur != nil {
		m = *cur
	}
	if in.host != nil {
		m.Host = strings.TrimSpace(*in.host)
	}
	if in.from != nil {
		m.From = strings.TrimSpace(*in.from)
	}
	if m.Host == "" {
		return nil, fmt.Errorf("--host is required (e.g. --host smtp.example.com)")
	}
	if m.From == "" {
		return nil, fmt.Errorf("--from is required (e.g. --from noreply@example.com)")
	}
	if !strings.Contains(m.From, "@") {
		return nil, fmt.Errorf("--from must be an email address")
	}
	if in.port != nil {
		m.Port = *in.port
	}
	if m.Port < 1 || m.Port > 65535 {
		return nil, fmt.Errorf("--port must be between 1 and 65535")
	}
	if in.startTLS != nil {
		m.StartTLS = *in.startTLS
	}
	if in.noAuth {
		m.Username, m.Password = "", ""
	}
	if in.username != nil {
		m.Username = *in.username
	}
	if in.password != nil {
		m.Password = *in.password
	}
	if in.subject != nil {
		m.Subject = *in.subject
	}
	for name, v := range map[string]string{"--host": m.Host, "--from": m.From, "--username": m.Username, "the password": m.Password, "--subject": m.Subject} {
		if err := plainValue(name, v); err != nil {
			return nil, err
		}
	}
	return &m, nil
}
