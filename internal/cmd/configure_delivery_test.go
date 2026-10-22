package cmd

import (
	"strings"
	"testing"

	"github.com/versolauth/versola-cli/internal/state"
)

func str(s string) *string { return &s }
func num(n int) *int       { return &n }
func flag(b bool) *bool    { return &b }

func TestMergeSMS(t *testing.T) {
	t.Run("the first time needs a url", func(t *testing.T) {
		if _, err := mergeSMS(nil, smsInput{}); err == nil || !strings.Contains(err.Error(), "--url") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("defaults", func(t *testing.T) {
		s, err := mergeSMS(nil, smsInput{url: str("https://sms.example.com/send")})
		if err != nil || s.Method != "POST" || s.Body != nil {
			t.Fatalf("%+v, %v", s, err)
		}
	})
	t.Run("a flag left out keeps the stored value", func(t *testing.T) {
		cur := &state.SMS{URL: "https://a.example.com", Method: "PUT", Username: "u", Password: "p", Body: map[string]string{"k": "v"}}
		s, err := mergeSMS(cur, smsInput{url: str("https://b.example.com")})
		if err != nil {
			t.Fatal(err)
		}
		if s.URL != "https://b.example.com" || s.Method != "PUT" || s.Username != "u" || s.Password != "p" || s.Body["k"] != "v" {
			t.Errorf("%+v", s)
		}
		if cur.URL != "https://a.example.com" {
			t.Error("the stored value was changed in place")
		}
	})
	t.Run("--body replaces the whole body", func(t *testing.T) {
		cur := &state.SMS{URL: "https://a.example.com", Method: "POST", Body: map[string]string{"old": "x"}}
		s, err := mergeSMS(cur, smsInput{bodySet: true, body: []string{"phones={{phone}}", "mes={{message}}"}})
		if err != nil || len(s.Body) != 2 || s.Body["old"] != "" {
			t.Fatalf("%+v, %v", s, err)
		}
	})
	t.Run("bad input", func(t *testing.T) {
		for name, in := range map[string]smsInput{
			"relative url":     {url: str("sms.example.com")},
			"ftp":              {url: str("ftp://sms.example.com")},
			"quote in url":     {url: str(`https://a.example.com/"x`)},
			"method":           {url: str("https://a.example.com"), method: str("DELETE")},
			"bad body":         {url: str("https://a.example.com"), bodySet: true, body: []string{"novalue"}},
			"body key":         {url: str("https://a.example.com"), bodySet: true, body: []string{"a-b=c"}},
			"body quote":       {url: str("https://a.example.com"), bodySet: true, body: []string{`a=b"c`}},
			"username only":    {url: str("https://a.example.com"), username: str("u")},
			"password newline": {url: str("https://a.example.com"), username: str("u"), password: str("a\nb")},
		} {
			if _, err := mergeSMS(nil, in); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})
	t.Run("--no-auth drops both, and the password never reaches an error", func(t *testing.T) {
		cur := &state.SMS{URL: "https://a.example.com", Method: "POST", Username: "u", Password: "p"}
		s, err := mergeSMS(cur, smsInput{noAuth: true})
		if err != nil || s.Username != "" || s.Password != "" {
			t.Fatalf("%+v, %v", s, err)
		}
		_, err = mergeSMS(nil, smsInput{url: str("https://a.example.com"), username: str("u"), password: str(`pw"secret`)})
		if err == nil || strings.Contains(err.Error(), "pw") {
			t.Errorf("error: %v", err)
		}
	})
}

func TestMergeSMTP(t *testing.T) {
	t.Run("the first time needs the host and the sender", func(t *testing.T) {
		if _, err := mergeSMTP(nil, smtpInput{from: str("a@b.c")}); err == nil || !strings.Contains(err.Error(), "--host") {
			t.Errorf("%v", err)
		}
		if _, err := mergeSMTP(nil, smtpInput{host: str("h")}); err == nil || !strings.Contains(err.Error(), "--from") {
			t.Errorf("%v", err)
		}
	})
	t.Run("defaults: port 587, STARTTLS on", func(t *testing.T) {
		m, err := mergeSMTP(nil, smtpInput{host: str("h"), from: str("a@b.c")})
		if err != nil || m.Port != 587 || !m.StartTLS {
			t.Fatalf("%+v, %v", m, err)
		}
	})
	t.Run("keeps what was not given, STARTTLS off sticks", func(t *testing.T) {
		cur := &state.SMTP{Host: "h", Port: 25, Username: "u", Password: "p", From: "a@b.c", StartTLS: true}
		m, err := mergeSMTP(cur, smtpInput{startTLS: flag(false)})
		if err != nil || m.Port != 25 || m.Password != "p" || m.StartTLS {
			t.Fatalf("%+v, %v", m, err)
		}
	})
	t.Run("bad input", func(t *testing.T) {
		for name, in := range map[string]smtpInput{
			"port 0":        {host: str("h"), from: str("a@b.c"), port: num(0)},
			"port too big":  {host: str("h"), from: str("a@b.c"), port: num(70000)},
			"from no @":     {host: str("h"), from: str("nobody")},
			"subject quote": {host: str("h"), from: str("a@b.c"), subject: str(`a"b`)},
		} {
			if _, err := mergeSMTP(nil, in); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})
}

func TestPasswordInputFromEnv(t *testing.T) {
	t.Setenv("VERSOLA_TEST_PW", "from-env")
	if pw, ok, err := passwordInput(false, false, "VERSOLA_TEST_PW", "x"); err != nil || !ok || pw != "from-env" {
		t.Errorf("got %q %v %v", pw, ok, err)
	}
	// --no-auth must not be undone by a variable left in the shell.
	if _, ok, err := passwordInput(false, true, "VERSOLA_TEST_PW", "x"); err != nil || ok {
		t.Errorf("--no-auth: used the variable (%v, %v)", ok, err)
	}
	t.Setenv("VERSOLA_TEST_PW", "")
	if _, ok, _ := passwordInput(false, false, "VERSOLA_TEST_PW", "x"); ok {
		t.Error("an empty variable counts as a password")
	}
}

func TestShownURLHidesWhatMayBeASecret(t *testing.T) {
	got := shownURL("https://user:pw-secret@sms.example.com/send?apikey=key-secret#frag")
	for _, leak := range []string{"user", "pw-secret", "apikey", "key-secret", "frag"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q shows %q", got, leak)
		}
	}
	if !strings.Contains(got, "https://sms.example.com/send") {
		t.Errorf("host and path are gone: %q", got)
	}
	if got := shownURL("https://sms.example.com/send"); got != "https://sms.example.com/send" {
		t.Errorf("a plain URL was changed: %q", got)
	}
}
