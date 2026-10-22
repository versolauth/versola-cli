package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

func TestDeliveryArgs(t *testing.T) {
	if got := deliveryArgs(nil); got != nil {
		t.Errorf("nil: %v", got)
	}
	if got := deliveryArgs(&state.Delivery{}); got != nil {
		t.Errorf("empty: %v", got)
	}
	got := strings.Join(deliveryArgs(&state.Delivery{
		SMS: &state.SMS{URL: "https://sms.example.com/send", Method: "POST", Username: "u", Password: "p", Body: map[string]string{"mes": "{{message}}", "phones": "{{phone}}"}},
	}), " ")
	want := "--otp=true --otp-url=https://sms.example.com/send --otp-method=POST --otp-username=u --otp-password=p --otp-body-mes={{message}} --otp-body-phones={{phone}}"
	if got != want {
		t.Errorf("sms:\n got %s\nwant %s", got, want)
	}

	// No credentials: the flags are left out, gen-env's default is none.
	got = strings.Join(deliveryArgs(&state.Delivery{SMS: &state.SMS{URL: "http://x", Method: "PUT"}}), " ")
	if strings.Contains(got, "username") || strings.Contains(got, "password") {
		t.Errorf("credentials passed although none are set: %s", got)
	}

	// SMTP without credentials still passes them (empty): gen-env's default
	// for both is "dev".
	args := deliveryArgs(&state.Delivery{SMTP: &state.SMTP{Host: "h", Port: 25, From: "a@b", StartTLS: false}})
	have := map[string]bool{}
	for _, a := range args {
		have[a] = true
	}
	for _, w := range []string{"--smtp=true", "--smtp-host=h", "--smtp-port=25", "--smtp-username=", "--smtp-password=", "--smtp-from=a@b", "--smtp-starttls=false"} {
		if !have[w] {
			t.Errorf("smtp: %q missing from %v", w, args)
		}
	}
	if strings.Contains(strings.Join(args, " "), "--smtp-subject") {
		t.Errorf("an unset subject was passed: %v", args)
	}
}

func writeAuthConf(t *testing.T, conf string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const commentedBlocks = `
# otp-provider {
#   method = "POST"
#   url = ""
#   body {
#     phones = "{{phone}}"
#   }
# }

# smtp {
#   host = ""
# }
`

func TestRequireDelivery(t *testing.T) {
	sms := &state.Delivery{SMS: &state.SMS{URL: "https://sms.example.com", Method: "POST", Body: map[string]string{"phones": "{{phone}}"}}}
	smtp := &state.Delivery{SMTP: &state.SMTP{Host: "h", Port: 25, From: "a@b"}}

	t.Run("nothing stored needs nothing", func(t *testing.T) {
		if err := requireDelivery(t.TempDir(), "1.0.0", &state.Delivery{}); err != nil {
			t.Error(err)
		}
	})
	t.Run("commented example blocks do not count", func(t *testing.T) {
		dir := writeAuthConf(t, commentedBlocks)
		for _, d := range []*state.Delivery{sms, smtp} {
			err := requireDelivery(dir, "0.7.3", d)
			if err == nil || !strings.Contains(err.Error(), "0.7.3") {
				t.Errorf("accepted a tools release that ignored the flags: %v", err)
			}
		}
	})
	t.Run("live blocks pass", func(t *testing.T) {
		dir := writeAuthConf(t, `
otp-provider {
  method = "POST"
  url = "https://sms.example.com"
  body {
    phones = "{{phone}}"
  }
}
smtp {
  host = "h"
}
`)
		both := &state.Delivery{SMS: sms.SMS, SMTP: smtp.SMTP}
		if err := requireDelivery(dir, "1.0.0", both); err != nil {
			t.Error(err)
		}
	})
	t.Run("a body entry the release did not write is found", func(t *testing.T) {
		dir := writeAuthConf(t, `
otp-provider {
  method = "POST"
  url = "https://sms.example.com"
  body {
    # phones = "{{phone}}"
  }
}
`)
		err := requireDelivery(dir, "1.0.0", sms)
		if err == nil || !strings.Contains(err.Error(), `"phones"`) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("a body key named like a key of the block itself is not found there", func(t *testing.T) {
		// An old release writes the block (with its own url line) but no body
		// entries: --body url=... must not be taken as applied.
		dir := writeAuthConf(t, `
otp-provider {
  method = "POST"
  url = "https://sms.example.com"
  body {
    # url = "{{message}}"
  }
}
`)
		d := &state.Delivery{SMS: &state.SMS{URL: "https://sms.example.com", Method: "POST", Body: map[string]string{"url": "{{message}}"}}}
		if err := requireDelivery(dir, "1.0.0", d); err == nil {
			t.Error("accepted a body entry that is not in the body block")
		}
		live := writeAuthConf(t, `
otp-provider {
  url = "https://sms.example.com"
  body {
    url = "{{message}}"
  }
}
`)
		if err := requireDelivery(live, "1.0.0", d); err != nil {
			t.Errorf("refused a body entry that is there: %v", err)
		}
	})
	t.Run("several entries whose values hold braces", func(t *testing.T) {
		// The README's example: the first closing brace in the block is inside
		// the first value, not the end of the block.
		two := &state.Delivery{SMS: &state.SMS{URL: "https://sms.example.com", Method: "POST", Body: map[string]string{"phones": "{{phone}}", "mes": "{{message}}"}}}
		live := writeAuthConf(t, `
otp-provider {
  url = "https://sms.example.com"
  body {
    mes = "{{message}}"
    phones = "{{phone}}"
  }
}
`)
		if err := requireDelivery(live, "1.0.0", two); err != nil {
			t.Errorf("refused a correct file: %v", err)
		}
		// The key after the first value is the one that is not there.
		partial := writeAuthConf(t, `
otp-provider {
  url = "https://sms.example.com"
  body {
    mes = "{{message}}"
  }
}
`)
		err := requireDelivery(partial, "1.0.0", two)
		if err == nil || !strings.Contains(err.Error(), `"phones"`) || strings.Contains(err.Error(), `"mes"`) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("the error holds no value", func(t *testing.T) {
		d := &state.Delivery{SMS: &state.SMS{URL: "https://sms.example.com/tok-123", Method: "POST", Username: "usr", Password: "pw-secret"}}
		err := requireDelivery(writeAuthConf(t, commentedBlocks), "1.0.0", d)
		if err == nil {
			t.Fatal("accepted")
		}
		for _, leak := range []string{"pw-secret", "tok-123", "usr"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("the error contains %q: %v", leak, err)
			}
		}
	})
}
