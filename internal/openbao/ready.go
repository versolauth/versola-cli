package openbao

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrSealed is wrapped into the error Client.Login returns when OpenBao
// answered that it is sealed (HTTP 503, "Vault is sealed"). A sealed OpenBao
// is the normal state right after its container or the server restarts: the
// data on the volume is intact, but nothing can be read until the unseal key
// is submitted again. EnsureUnsealed is what fixes it before a login.
var ErrSealed = errors.New("OpenBao is sealed")

// sealedResponse reports whether a failed response is OpenBao saying it is
// sealed, as opposed to any other 503 (a proxy in front of it, a standby
// node, an overloaded server). The text is OpenBao's own: its sealed answer
// is {"errors":["Vault is sealed"]}.
func sealedResponse(status int, body []byte) bool {
	return status == http.StatusServiceUnavailable && bytes.Contains(bytes.ToLower(body), []byte("sealed"))
}

// How long EnsureUnsealed waits for OpenBao to start answering, and how often
// it asks. After a server reboot Docker starts the OpenBao container at about
// the same time as the user's first command can run; variables so tests don't
// wait for real.
var (
	readyWait = 30 * time.Second
	readyPoll = time.Second
	// activeWait: how long after an unseal EnsureUnsealed waits for the node
	// to become active.
	activeWait = 10 * time.Second
)

// EnsureUnsealed makes sure target's OpenBao at address is up, initialized
// and unsealed, so that a login right after it does not fail with "Vault is
// sealed". It exists because OpenBao starts sealed after every restart of its
// container (so after every reboot of the server), and until now only
// `configure` unsealed it: `rollout --dry-run`, `secrets plan` and
// `secrets test` failed with a bare 503 and no hint.
//
// What it does:
//   - waits up to readyWait for OpenBao to answer at all (right after a boot
//     its container may still be starting);
//   - not initialized -> an error that says to run `versola configure`;
//   - already unsealed -> nothing, silently;
//   - sealed -> submits the unseal key this CLI saved when it initialized
//     OpenBao (~/.versola/openbao/<target>-admin.json), prints one line to
//     out saying so, and waits until the node is active.
//   - an address that is not local and does not answer health is let through
//     untouched (nothing can be unsealed there from here; the login that
//     follows reports the real problem).
//
// When it cannot unseal (no saved key -- OpenBao was set up by hand
// (--setup-openbao); the key was refused) it returns an error that says what
// to do. The key is never printed or put in an error: the only thing that
// leaves this function is the key itself, in the body of the unseal request.
//
// The saved key is only ever sent to a local address (localhost, 127.0.0.0/8,
// ::1): the CLI's own OpenBao is always local, and `secrets login <target>
// <address>` takes an address from the command line -- sending the key to
// whatever was typed there would hand it to anyone who can make a user type
// that address.
func EnsureUnsealed(ctx context.Context, target, address string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}

	initialized, sealed, err := waitForHealth(ctx, target, address)
	if err != nil {
		if ctx.Err() == nil && !isLoopbackAddress(address) {
			// Not this CLI's own OpenBao (a remote one given to `secrets
			// login`, or set up by hand behind a proxy that may not expose
			// /sys/health): it can't be unsealed from here anyway, so don't
			// fail what worked before -- the login that follows reports the
			// real problem, and says "sealed" if that is it (ErrSealed).
			return nil
		}
		return err
	}
	if !initialized {
		return fmt.Errorf("OpenBao at %s is not initialized yet -- `versola configure %s` initializes and unseals it (if you set OpenBao up by hand with --setup-openbao, initialize it with `bao operator init`; develop.md, OpenBao section)", address, target)
	}
	if !sealed {
		return nil
	}

	adminPath, pathErr := adminCredsPath(target)
	if pathErr != nil {
		return pathErr
	}
	admin, err := LoadAdminCreds(target)
	switch {
	case errors.Is(err, ErrNoAdminCreds) || (err == nil && admin.UnsealKey == ""):
		return fmt.Errorf("OpenBao at %s is sealed (it is after every restart of the server) and this machine has no saved unseal key (%s) -- that is the case when OpenBao was set up by hand (--setup-openbao). Unseal it yourself with `bao operator unseal` (develop.md, OpenBao section) and run the command again", address, adminPath)
	case err != nil:
		return err
	}
	if !isLoopbackAddress(address) {
		return fmt.Errorf("OpenBao at %s is sealed; the saved unseal key is only ever sent to a local OpenBao, so unseal this one yourself with `bao operator unseal`", address)
	}

	fmt.Fprintf(out, "OpenBao at %s is sealed (it starts sealed after every restart) -- unsealing it with the key saved in %s\n", address, adminPath)
	if err := Unseal(ctx, address, admin.UnsealKey); err != nil {
		return fmt.Errorf("couldn't unseal OpenBao with the saved key: %w -- the key in %s may belong to another OpenBao (a stale file, or its data volume was recreated). Check that first: starting over with a clean OpenBao destroys every secret stored in it", err, adminPath)
	}
	return waitUntilActive(ctx, address)
}

// waitUntilActive polls health after an unseal until the node has stopped
// being sealed and is no longer a standby: a single node needs a moment after
// the unseal to become active, and a login in that window gets a 503.
func waitUntilActive(ctx context.Context, address string) error {
	deadline := time.Now().Add(activeWait)
	for {
		h, err := healthStatus(ctx, address)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("couldn't check OpenBao after unsealing it: %w", err)
			}
		case h.Sealed && time.Now().After(deadline):
			return fmt.Errorf("OpenBao at %s is still sealed after the saved unseal key was submitted", address)
		case !h.Sealed && !h.Standby:
			return nil
		case !h.Sealed && time.Now().After(deadline):
			// Unsealed but never became active: let the login say what is wrong.
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPoll):
		}
	}
}

// waitForHealth asks OpenBao's health until it answers or readyWait passes.
func waitForHealth(ctx context.Context, target, address string) (initialized, sealed bool, err error) {
	// Only a local OpenBao is a container that may still be starting; an
	// address typed into `secrets login` that doesn't answer is a typo, and
	// waiting half a minute for it would only be slow.
	wait := readyWait
	if !isLoopbackAddress(address) {
		wait = 0
	}
	deadline := time.Now().Add(wait)
	for {
		initialized, sealed, err = Health(ctx, address)
		if err == nil {
			return initialized, sealed, nil
		}
		if ctx.Err() != nil {
			return false, false, ctx.Err()
		}
		if time.Now().After(deadline) {
			if wait == 0 {
				return false, false, err
			}
			return false, false, fmt.Errorf("%w -- waited %s for it. Is its container up? Start it with `docker start versola-openbao-%s` (`versola configure %s` does too, when there are no extra replicas)", err, wait, target, target)
		}
		select {
		case <-ctx.Done():
			return false, false, ctx.Err()
		case <-time.After(readyPoll):
		}
	}
}

// isLoopbackAddress reports whether address (a URL like
// "http://localhost:8200") points at this machine.
func isLoopbackAddress(address string) bool {
	u, err := url.Parse(address)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
