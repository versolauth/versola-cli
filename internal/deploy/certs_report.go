package deploy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/docker"
	"github.com/versolauth/versola-cli/internal/proxy"
	"github.com/versolauth/versola-cli/internal/state"
)

// mtlsInitCert is the prefix of the lines mtls-init prints in its report
// phase: one per certificate file in the mutual-TLS volume,
//
//	mtls-init: cert server.crt notBefore=2026-10-01T00:00:00Z notAfter=2026-12-30T00:00:00Z subject=central
const mtlsInitCert = "mtls-init: cert "

// CertInfo is one certificate and when it is valid.
type CertInfo struct {
	// Name is what it is called: a file in the mutual-TLS volume
	// ("server.crt"), or "<host>:<port>" for one a public name presents.
	Name    string
	Subject string
	// NotBefore may be zero (a report that does not say): the remaining
	// fraction of the lifetime is then not known.
	NotBefore, NotAfter time.Time
	// Error is why a probe got nothing; the rest is empty then.
	Error string
}

// Left is how long the certificate is still valid for at now (negative:
// expired).
func (c CertInfo) Left(now time.Time) time.Duration { return c.NotAfter.Sub(now) }

// Due reports whether a third of the lifetime or less is left -- where
// mtls-init and nginx's ACME module renew -- when the lifetime is known.
// Without a notBefore it falls back to what the CLI's own lifetimes make of
// "a third": 30 days, the 90-day leaves'.
func (c CertInfo) Due(now time.Time) bool {
	left := c.Left(now)
	if c.NotBefore.IsZero() || !c.NotAfter.After(c.NotBefore) {
		return left < 30*24*time.Hour
	}
	return left*3 <= c.NotAfter.Sub(c.NotBefore)
}

// parseCertReport reads mtls-init's report lines. Anything else is ignored,
// a line it cannot read too: the report is for a human, and one odd line
// must not hide the others.
func parseCertReport(output []byte) []CertInfo {
	var out []CertInfo
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, mtlsInitCert)
		if !ok {
			continue
		}
		name, rest, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		c := CertInfo{Name: name}
		fields, subject, _ := strings.Cut(rest, "subject=")
		c.Subject = strings.TrimSpace(subject)
		valid := false
		for _, f := range strings.Fields(fields) {
			k, v, _ := strings.Cut(f, "=")
			t, err := time.Parse(time.RFC3339, v)
			switch {
			case k == "notBefore" && err == nil:
				c.NotBefore = t
			case k == "notAfter" && err == nil:
				c.NotAfter, valid = t, true
			}
		}
		if valid {
			out = append(out, c)
		}
	}
	return out
}

// IssuesMutualTLSCertificates: releases before 0.8 deploy no mutual-TLS
// certificates, so there is nothing for a timer to renew or a report to read (a pre-release such as
// 0.8.0-rc1 counts as 0.8). A version that cannot be read is taken to have them:
// a timer that renews nothing is a nuisance, a missing one breaks a deployment.
func IssuesMutualTLSCertificates(version string) bool {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return true
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	if errMajor != nil || errMinor != nil {
		return true
	}
	return major > 0 || minor >= 8
}

// CertReport asks mtls-init (its report phase, which changes nothing) for
// every certificate in the deployment's mutual-TLS volume. The deployment
// must have a compose file; a volume that has not been filled yet is an
// empty report.
func CertReport(st *state.State) ([]CertInfo, error) {
	if st == nil {
		return nil, fmt.Errorf("nothing has been configured yet")
	}
	composePath, exists, err := st.ComposeFilePath()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("the deployment in ~/.versola/active is incomplete (no compose file)")
	}
	// An older release has no certificates, and a tools image from before the
	// report phase would run mtls-init in its default mode -- which replaces what
	// is due in place and restarts nothing. Neither is asked a report of.
	if !IssuesMutualTLSCertificates(st.Version) {
		return nil, nil
	}
	if has, err := composeHasService(composePath, MTLSInitService); err == nil && !has {
		return nil, nil
	}
	c := docker.Cmd(state.ComposeArgs(composePath, "run", "--rm", "-T", "--no-deps", "-e", "MTLS_INIT_PHASE="+phaseReport, MTLSInitService)...)
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("%s (report) failed: %w\n%s", MTLSInitService, err, bytes.TrimSpace(buf.Bytes()))
	}
	return parseCertReport(buf.Bytes()), nil
}

// composeHasService: the compose file declares the service. A failure to ask
// is an error, left to the caller to treat as "unknown".
func composeHasService(composePath, service string) (bool, error) {
	out, err := docker.Cmd(state.ComposeArgs(composePath, "config", "--services")...).Output()
	if err != nil {
		return false, err
	}
	for _, name := range strings.Fields(string(out)) {
		if name == service {
			return true, nil
		}
	}
	return false, nil
}

// PublicEndpoints are the addresses a deployment's names present a
// certificate on, for `doctor`/`status`: the auth URL's (ACME), the
// enrollment name's (ACME) and the mutual-TLS names' (the leaves of the
// deployment's own CA, which the proxy passes through).
func PublicEndpoints(st *state.State) []string {
	if st.Target != "vps" || st.ProxyMode != proxy.ModeNginx {
		return nil
	}
	a, err := proxy.ParseAuthURL(st.AuthURL, st.ProxyMode)
	if err != nil || !a.TLS(st.ProxyMode) {
		return nil
	}
	hosts := []string{a.Host}
	for _, h := range []string{st.CentralDomain, st.MTLSDomain, st.CentralMTLSDomain} {
		if h != "" {
			hosts = append(hosts, h)
		}
	}
	for i, h := range hosts {
		hosts[i] = net.JoinHostPort(h, "443")
	}
	return hosts
}

// ProbeCertificate reads the certificate a TLS server presents for host (the
// SNI name) without trusting it: what it says is the point, not whether it
// is trusted. A mutual-TLS listener that then refuses the missing client
// certificate has already shown its leaf by then.
func ProbeCertificate(addr string, timeout time.Duration) CertInfo {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return CertInfo{Name: addr, Error: err.Error()}
	}
	var leaf *x509.Certificate
	conf := &tls.Config{
		ServerName: host,
		// Only to read the certificate: nothing is sent over this connection.
		InsecureSkipVerify: true, //nolint:gosec
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) > 0 {
				leaf, _ = x509.ParseCertificate(raw[0])
			}
			return nil
		},
	}
	d := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(d, "tcp", addr, conf)
	if conn != nil {
		conn.Close()
	}
	if leaf == nil {
		if err == nil {
			err = fmt.Errorf("no certificate presented")
		}
		return CertInfo{Name: addr, Error: err.Error()}
	}
	return CertInfo{Name: addr, Subject: leaf.Subject.CommonName, NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter}
}

// WriteCertReport prints a table of certificates, soonest to expire first,
// marking those a renewal is due for and those expired.
func WriteCertReport(w io.Writer, infos []CertInfo, now time.Time) {
	sorted := append([]CertInfo(nil), infos...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if (sorted[i].Error != "") != (sorted[j].Error != "") {
			return sorted[i].Error == ""
		}
		return sorted[i].NotAfter.Before(sorted[j].NotAfter)
	})
	width := 0
	for _, c := range sorted {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range sorted {
		if c.Error != "" {
			fmt.Fprintf(w, "  %-*s  not read: %s\n", width, c.Name, c.Error)
			continue
		}
		fmt.Fprintf(w, "  %-*s  notAfter %s  %s%s\n", width, c.Name, c.NotAfter.UTC().Format("2006-01-02 15:04Z"), leftText(c.Left(now)), certMark(c, now))
	}
}

func certMark(c CertInfo, now time.Time) string {
	switch {
	case c.Left(now) <= 0:
		return "  EXPIRED"
	case c.Due(now):
		return "  due for renewal"
	}
	return ""
}

func leftText(d time.Duration) string {
	if d <= 0 {
		return fmt.Sprintf("%d days ago", int((-d)/(24*time.Hour)))
	}
	days := int(d / (24 * time.Hour))
	if days == 0 {
		return fmt.Sprintf("%d hours left", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days left", days)
}

// probeTimeout bounds one look at a public name.
const probeTimeout = 5 * time.Second

// PrintCertificates prints the notAfter of every certificate of the
// deployment st records: those in its mutual-TLS volume, and what its public
// names present. What cannot be read is said, not hidden -- a certificate
// that cannot be seen is the one to look at.
func PrintCertificates(w io.Writer, st *state.State, now time.Time) {
	fmt.Fprintln(w, "Certificates (notAfter):")
	infos, err := CertReport(st)
	if err != nil {
		fmt.Fprintf(w, "  the mutual-TLS volume could not be read: %v\n", errFirstLine(err))
	}
	for _, addr := range PublicEndpoints(st) {
		infos = append(infos, ProbeCertificate(addr, probeTimeout))
	}
	if len(infos) == 0 && err == nil {
		fmt.Fprintln(w, "  none yet")
		return
	}
	WriteCertReport(w, infos, now)
	// Certificates that never expire are not warned about.
	if st.CertsUnlimited() {
		return
	}
	for _, msg := range ExpiryWarnings(infos, now) {
		fmt.Fprintf(w, "WARNING: %s\n", msg)
	}
}

// ExpiryWindow is how close to its end a certificate has to be before it is
// warned about, once a renewal is due for it: a healthy deployment renews at a
// third of the life left, so this is the sign that nothing has.
const ExpiryWindow = 60 * 24 * time.Hour

// ExpiryWarnings say, in words, which certificates have less than ExpiryWindow
// left and are past the point (a third of the life) where `certs renew` renews
// them -- those are the ones a missed renewal leaves behind -- and which have
// expired. Certificates that could not be read are reported by the table.
func ExpiryWarnings(infos []CertInfo, now time.Time) []string {
	var out []string
	for _, c := range infos {
		if c.Error != "" || c.NotAfter.IsZero() {
			continue
		}
		left := c.Left(now)
		switch {
		case left <= 0:
			out = append(out, fmt.Sprintf("%s has expired -- run `versola certs renew` now", c.Name))
		case left < ExpiryWindow && c.Due(now):
			out = append(out, fmt.Sprintf("%s runs out in %s and should have been renewed already -- run `versola certs renew`, and check that its timer runs", c.Name, strings.TrimSuffix(leftText(left), " left")))
		}
	}
	return out
}

// errFirstLine is an error's first line: the rest may be a container's output.
func errFirstLine(err error) string {
	l, _, _ := strings.Cut(err.Error(), "\n")
	return l
}
