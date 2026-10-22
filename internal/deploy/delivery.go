package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/versolauth/versola-cli/internal/state"
)

// Delivery's flags for gen-env.scala (see its "OTP provider" and "SMTP"
// sections): handed to versola-tools as arguments, like --central-mutual-tls-url.
//
// The CLI does not write these blocks into auth.conf itself -- it knows nothing
// of the config schema (see the package comment of internal/topology) -- it only
// remembers what the operator gave `versola configure sms|smtp` and gives it to
// gen-env again on every configure and rollout, which generate auth.conf from
// scratch.

// deliveryArgs are gen-env's flags for d, none when nothing is configured (the
// blocks then stay commented out, as gen-env writes them by default).
//
// Passed as arguments, so a password is visible to other users of this machine
// in the process list for the seconds the tools container runs: gen-env reads
// no environment variable for it yet. Errors never carry them (an exec error is
// "exit status N").
func deliveryArgs(d *state.Delivery) []string {
	if d.Empty() {
		return nil
	}
	var args []string
	if s := d.SMS; s != nil {
		args = append(args, "--otp=true", "--otp-url="+s.URL, "--otp-method="+s.Method)
		// Only when set: gen-env's default for both is "none".
		if s.Username != "" {
			args = append(args, "--otp-username="+s.Username)
		}
		if s.Password != "" {
			args = append(args, "--otp-password="+s.Password)
		}
		keys := make([]string, 0, len(s.Body))
		for k := range s.Body {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			// One flag per key, the key in the flag's name: gen-env keeps a
			// flag name -> value map, so a repeated flag would keep only its
			// last value.
			args = append(args, "--otp-body-"+k+"="+s.Body[k])
		}
	}
	if m := d.SMTP; m != nil {
		// Always all of them, an empty username and password included: left
		// out, gen-env falls back to its development defaults ("dev").
		args = append(args,
			"--smtp=true",
			"--smtp-host="+m.Host,
			"--smtp-port="+strconv.Itoa(m.Port),
			"--smtp-username="+m.Username,
			"--smtp-password="+m.Password,
			"--smtp-from="+m.From,
			"--smtp-starttls="+strconv.FormatBool(m.StartTLS),
		)
		if m.Subject != "" {
			args = append(args, "--smtp-subject="+m.Subject)
		}
	}
	return args
}

var (
	otpBlockRe  = regexp.MustCompile(`(?m)^\s*otp-provider\s*\{`)
	smtpBlockRe = regexp.MustCompile(`(?m)^\s*smtp\s*\{`)
)

// requireDelivery checks that the auth.conf versola-tools just wrote in dir
// carries what d says: a versola-tools release that does not know the flags
// ignores them without a word, and the result would be an auth that quietly
// sends no codes (NoOp providers only log a warning). Checked on the file, not
// on a version number, because this CLI does not know which release added
// what. Messages carry no values.
func requireDelivery(dir, version string, d *state.Delivery) error {
	if d.Empty() {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "auth.conf"))
	if err != nil {
		return fmt.Errorf("couldn't read the generated auth.conf to check the SMS/SMTP settings: %w", err)
	}
	conf := uncommented(string(b))
	var missing []string
	if s := d.SMS; s != nil {
		if loc := otpBlockRe.FindStringIndex(conf); loc == nil {
			missing = append(missing, "the otp-provider block")
		} else {
			// Only the body block of that otp-provider: keys like url or
			// password also sit in the block itself, and would match.
			body := otpBody(conf[loc[1]:])
			for k := range s.Body {
				if !regexp.MustCompile(`(?m)^\s*"?` + regexp.QuoteMeta(k) + `"?\s*[=:]`).MatchString(body) {
					missing = append(missing, fmt.Sprintf("the otp-provider body entry %q", k))
				}
			}
		}
	}
	if d.SMTP != nil && !smtpBlockRe.MatchString(conf) {
		missing = append(missing, "the smtp block")
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("the generated auth.conf of Versola %s has no %s, although `versola configure sms|smtp` stored them: this release's versola-tools does not apply them (use a newer release, or remove the setting with `versola configure sms --disable` / `versola configure smtp --disable`)", version, strings.Join(missing, ", "))
}

// otpBody is the inside of the first body { ... } in rest (the text after
// "otp-provider {"), "" if there is none. The block holds plain entries, no
// nested braces -- but the values hold some ("{{phone}}"), so the closing brace
// is the first one outside a quoted string.
func otpBody(rest string) string {
	m := regexp.MustCompile(`(?m)^\s*body\s*\{`).FindStringIndex(rest)
	if m == nil {
		return ""
	}
	inner := rest[m[1]:]
	inQuote := false
	for i := 0; i < len(inner); i++ {
		switch c := inner[i]; {
		case c == '\\' && inQuote:
			i++ // the escaped character
		case c == '"':
			inQuote = !inQuote
		case c == '}' && !inQuote:
			return inner[:i]
		}
	}
	return inner
}

// uncommented is conf without its comment lines (# and //), which is what the
// commented-out example blocks of a generated auth.conf are.
func uncommented(conf string) string {
	var out []string
	for _, line := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
