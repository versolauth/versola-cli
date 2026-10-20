package deploy

import (
	"fmt"
	"strings"
)

// Names of the systemd units `certs renew --install-timer` writes.
const (
	CertsServiceUnit = "versola-certs-renew.service"
	CertsTimerUnit   = "versola-certs-renew.timer"
)

// systemdQuote makes a path one word of an ExecStart= line: quoted when it
// has whitespace or quotes, and with `%` doubled, which systemd would read
// as a specifier.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if strings.ContainsAny(s, " \t\"'\\") {
		s = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

// CertsUnits renders the systemd user units that run `certs renew` daily:
// the binary's absolute path, and the PATH it has now, so that `docker` is
// found by the service as it is by this shell.
func CertsUnits(binary, path string) (service, timer string, err error) {
	for name, v := range map[string]string{"binary path": binary, "PATH": path} {
		if v == "" || strings.ContainsAny(v, "\n\r") {
			return "", "", fmt.Errorf("can't write a unit with this %s: %q", name, v)
		}
	}
	service = fmt.Sprintf(`[Unit]
Description=Renew Versola's mutual-TLS certificates

[Service]
Type=oneshot
Environment="PATH=%s"
ExecStart=%s certs renew
`, strings.ReplaceAll(strings.ReplaceAll(path, `\`, `\\`), `"`, `\"`), systemdQuote(binary))
	timer = fmt.Sprintf(`[Unit]
Description=Renew Versola's mutual-TLS certificates daily

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true
Unit=%s

[Install]
WantedBy=timers.target
`, CertsServiceUnit)
	return service, timer, nil
}

// CertsCronLine is the crontab line for a host without systemd. A crontab's
// PATH is minimal, so the binary is given by its absolute path and docker
// has to be where cron looks (/usr/bin).
func CertsCronLine(binary string) string {
	q := binary
	if strings.ContainsAny(q, " \t'\"\\$`&;|<>()*?#~!%") {
		q = "'" + strings.ReplaceAll(strings.ReplaceAll(q, "'", `'\''`), "%", `\%`) + "'"
	}
	return fmt.Sprintf("17 3 * * * %s certs renew", q)
}
