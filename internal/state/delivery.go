package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/versolauth/versola-cli/internal/fsutil"
)

// DeliveryFileName is where `versola configure sms|smtp` keep what they were
// given, in Dir() next to state.json.
//
// A file of its own, not fields of state.json: every configure builds a new
// bundle and generates auth.conf from scratch, so a value that lives only in
// the bundle is lost with it -- but these are not facts about a deployment the
// way state.json's are (they are what the operator typed, and may be set before
// any deployment exists), and a password in state.json, which is world
// readable and gets pasted into bug reports, is the wrong place for one. Not
// OpenBao either: the services read them as plain values of auth.conf, as
// they did when gen-env was run by hand.
//
// It is in Dir(), which pruneBundles never touches (it only removes bundle-*
// directories) and Finalize never rewrites, so a configure keeps it; it goes
// with the rest of ~/.versola/active on `uninstall`.
const DeliveryFileName = "delivery.json"

// Delivery is how auth reaches a user with a one-time code: an HTTP SMS
// provider (auth.conf's otp-provider block) and/or an SMTP server (its smtp
// block). A nil member means "not configured", which leaves the block commented
// out in the generated auth.conf.
type Delivery struct {
	SMS  *SMS  `json:"sms,omitempty"`
	SMTP *SMTP `json:"smtp,omitempty"`
}

// SMS is auth.conf's otp-provider block.
type SMS struct {
	URL    string `json:"url"`
	Method string `json:"method"`
	// Username and Password are the provider's HTTP basic credentials, both
	// empty for none.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	// Body is the JSON object sent to the provider: each value may contain
	// {{phone}} and {{message}}.
	Body map[string]string `json:"body,omitempty"`
}

// SMTP is auth.conf's smtp block.
type SMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	From     string `json:"from"`
	Subject  string `json:"subject,omitempty"`
	StartTLS bool   `json:"startTls"`
}

// Empty reports whether nothing is configured.
func (d *Delivery) Empty() bool {
	return d == nil || (d.SMS == nil && d.SMTP == nil)
}

func deliveryPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, DeliveryFileName), nil
}

// LoadDelivery reads the stored delivery settings. A file that does not exist is
// not an error: it is an empty Delivery, which is what nothing configured means.
// One that cannot be parsed is, so that a configure does not quietly generate a
// deployment without the SMS provider the operator thought they had set.
func LoadDelivery() (*Delivery, error) {
	path, err := deliveryPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Delivery{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("couldn't read %s: %w", path, err)
	}
	var d Delivery
	if err := json.Unmarshal(b, &d); err != nil {
		// The error text of Unmarshal may quote a piece of the input: not
		// passed on, this file holds passwords.
		return nil, fmt.Errorf("couldn't parse %s (is it valid JSON? fix it, or delete it and run `versola configure sms|smtp` again)", path)
	}
	return &d, nil
}

// Save writes d, readable by this user only. An empty Delivery removes the file.
func (d *Delivery) Save() error {
	path, err := deliveryPath()
	if err != nil {
		return err
	}
	if d.Empty() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("couldn't remove %s: %w", path, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("couldn't create %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("couldn't encode the delivery settings: %w", err)
	}
	return fsutil.WriteFileAtomic(path, append(b, '\n'), 0o600)
}
