package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDeliveryRoundTrip(t *testing.T) {
	isolate(t)
	d, err := LoadDelivery()
	if err != nil || !d.Empty() {
		t.Fatalf("nothing stored: %+v, %v", d, err)
	}
	d.SMS = &SMS{URL: "https://sms.example.com/send", Method: "POST", Username: "u", Password: "p", Body: map[string]string{"phones": "{{phone}}"}}
	d.SMTP = &SMTP{Host: "smtp.example.com", Port: 587, From: "a@example.com", StartTLS: true}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDelivery()
	if err != nil {
		t.Fatal(err)
	}
	if got.SMS == nil || got.SMS.Password != "p" || got.SMS.Body["phones"] != "{{phone}}" || got.SMTP == nil || got.SMTP.Host != "smtp.example.com" {
		t.Fatalf("read back %+v", got)
	}
}

func TestDeliveryFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	isolate(t)
	d := &Delivery{SMS: &SMS{URL: "https://sms.example.com", Method: "POST", Username: "u", Password: "secret-pw"}}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir()
	fi, err := os.Stat(filepath.Join(dir, DeliveryFileName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestDeliveryEmptySaveRemovesTheFile(t *testing.T) {
	isolate(t)
	d := &Delivery{SMTP: &SMTP{Host: "h", Port: 25, From: "a@b"}}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	d.SMTP = nil
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir()
	if _, err := os.Stat(filepath.Join(dir, DeliveryFileName)); !os.IsNotExist(err) {
		t.Errorf("the file is still there: %v", err)
	}
	// Removing what is not there is fine.
	if err := d.Save(); err != nil {
		t.Errorf("second save: %v", err)
	}
}

func TestDeliveryUnreadableFileIsAnErrorThatQuotesNothing(t *testing.T) {
	isolate(t)
	dir, _ := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DeliveryFileName), []byte(`{"sms": {"password": "leaky-pw" oops`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDelivery()
	if err == nil {
		t.Fatal("a broken file was accepted")
	}
	if strings.Contains(err.Error(), "leaky-pw") {
		t.Errorf("the error quotes the file: %v", err)
	}
}

// A configure builds a new bundle and removes the old ones; the stored settings
// are what it has to keep.
func TestDeliverySurvivesFinalizeAndPruning(t *testing.T) {
	isolate(t)
	d := &Delivery{SMS: &SMS{URL: "https://sms.example.com", Method: "POST"}}
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	finalizeRev(t, "vps", 1)
	finalizeRev(t, "vps", 1)
	got, err := LoadDelivery()
	if err != nil || got.SMS == nil {
		t.Fatalf("lost after two configures: %+v, %v", got, err)
	}
}
