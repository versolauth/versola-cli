package openbao

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// adminHTTP is a short-timeout client for the raw sys/auth-mount admin
// calls below -- separate from Client's own *http.Client (which is scoped
// to one target's AppRole session) since these calls happen before any
// AppRole exists yet, authenticated with the root token instead.
var adminHTTP = &http.Client{Timeout: 10 * time.Second}

// Health reports whether address's OpenBao has been through `sys/init` at
// all, and if so, whether it's currently sealed (true on every fresh
// container start, even with data intact on the volume -- seal state
// itself isn't persisted). Used by ProvisionLocal to decide which of
// Init/Unseal below it actually needs to run, rather than assuming a
// fresh container every time.
func Health(ctx context.Context, address string) (initialized, sealed bool, err error) {
	// OpenBao's own /sys/health intentionally answers with a non-200
	// status for every "not fully up" case (501 uninitialized, 503
	// sealed, ...) -- the body is still valid JSON either way, so this
	// reads it regardless of status rather than treating those as errors.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/v1/sys/health", nil)
	if err != nil {
		return false, false, fmt.Errorf("couldn't build health request: %w", err)
	}
	resp, err := adminHTTP.Do(req)
	if err != nil {
		return false, false, fmt.Errorf("couldn't reach OpenBao at %s: %w", address, err)
	}
	defer resp.Body.Close()

	var h struct {
		Initialized bool `json:"initialized"`
		Sealed      bool `json:"sealed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return false, false, fmt.Errorf("couldn't parse OpenBao's health response: %w", err)
	}
	return h.Initialized, h.Sealed, nil
}

// Init runs OpenBao's one-time initialization with a single key share
// (-key-shares=1 -key-threshold=1 in develop.md's manual `bao operator
// init` -- see ProvisionLocal's own comment on why that's the right
// choice here too, not just for the manual vps flow it was written for).
// Returns the root token and the one unseal key -- both are needed again
// (unseal on every restart, the root token to finish provisioning below),
// and neither is recoverable from OpenBao itself afterward.
func Init(ctx context.Context, address string) (rootToken, unsealKey string, err error) {
	reqBody, err := json.Marshal(map[string]int{"secret_shares": 1, "secret_threshold": 1})
	if err != nil {
		return "", "", fmt.Errorf("couldn't encode init request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, address+"/v1/sys/init", bytes.NewReader(reqBody))
	if err != nil {
		return "", "", fmt.Errorf("couldn't build init request: %w", err)
	}
	resp, err := adminHTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("couldn't reach OpenBao at %s: %w", address, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("couldn't read OpenBao's init response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("OpenBao init failed (%s): %s", resp.Status, string(body))
	}

	var initResp struct {
		Keys      []string `json:"keys"`
		RootToken string   `json:"root_token"`
	}
	if err := json.Unmarshal(body, &initResp); err != nil {
		return "", "", fmt.Errorf("couldn't parse OpenBao's init response: %w", err)
	}
	if len(initResp.Keys) != 1 || initResp.RootToken == "" {
		return "", "", fmt.Errorf("OpenBao's init response didn't have exactly one key and a root token")
	}
	return initResp.RootToken, initResp.Keys[0], nil
}

// Unseal submits the single unseal key Init returned. Needed again after
// every fresh container start/recreation, not just the first time -- see
// Init's own comment.
func Unseal(ctx context.Context, address, unsealKey string) error {
	reqBody, err := json.Marshal(map[string]string{"key": unsealKey})
	if err != nil {
		return fmt.Errorf("couldn't encode unseal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, address+"/v1/sys/unseal", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("couldn't build unseal request: %w", err)
	}
	resp, err := adminHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach OpenBao at %s: %w", address, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("OpenBao unseal failed (%s): %s", resp.Status, string(body))
	}
	return nil
}

// adminRequest is the shared plumbing for the six root-token-authenticated
// setup calls below: build a request, attach X-Vault-Token, and either
// succeed or return an error that includes OpenBao's own response body
// (never the token itself, which never appears in a request body these
// calls send anyway).
func adminRequest(ctx context.Context, method, url, token string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("couldn't encode request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("couldn't build request: %w", err)
	}
	req.Header.Set("X-Vault-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := adminHTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("couldn't reach OpenBao at %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("couldn't read OpenBao's response: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

// alreadyInUse reports whether a 400 response is OpenBao's own "path is
// already in use" -- the exact error `bao secrets enable`/`bao auth
// enable` return for a mount/auth-method that's already there (see
// develop.md's own manual steps, and this repo's actual vps deploy log,
// which hit this literally). Treated as success everywhere it's checked
// below: ProvisionLocal has to be safe to run on every `configure local`,
// not just the first one on a given machine, so "it's already enabled"
// must not be a failure.
func alreadyInUse(statusCode int, body []byte) bool {
	return statusCode == http.StatusBadRequest && strings.Contains(string(body), "already in use")
}

// EnableKV enables the KV v2 secrets engine at the fixed "secret/" mount
// this whole package assumes (see kvMount) -- `bao secrets enable
// -path=secret kv-v2` in develop.md's manual steps. Idempotent: a mount
// that's already there is treated as success, not an error (see
// alreadyInUse).
func EnableKV(ctx context.Context, address, token string) error {
	body, status, err := adminRequest(ctx, http.MethodPost, address+"/v1/sys/mounts/"+kvMount, token,
		map[string]any{"type": "kv", "options": map[string]string{"version": "2"}})
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent && !alreadyInUse(status, body) {
		return fmt.Errorf("couldn't enable kv-v2 (%d): %s", status, string(body))
	}
	return nil
}

// EnableApprole enables the AppRole auth method -- `bao auth enable
// approle`. Idempotent, same reasoning as EnableKV.
func EnableApprole(ctx context.Context, address, token string) error {
	body, status, err := adminRequest(ctx, http.MethodPost, address+"/v1/sys/auth/approle", token,
		map[string]string{"type": "approle"})
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent && !alreadyInUse(status, body) {
		return fmt.Errorf("couldn't enable approle auth (%d): %s", status, string(body))
	}
	return nil
}

// WritePolicy creates or overwrites an ACL policy -- `bao policy write
// <name>`. Always safe to re-run: a `PUT` here replaces the policy body
// wholesale, so writing the exact same policy twice (the common case, this
// package always writes the same one for a given target) is a no-op in
// effect.
func WritePolicy(ctx context.Context, address, token, name, hcl string) error {
	body, status, err := adminRequest(ctx, http.MethodPut, address+"/v1/sys/policies/acl/"+name, token,
		map[string]string{"policy": hcl})
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("couldn't write policy %q (%d): %s", name, status, string(body))
	}
	return nil
}

// CreateApproleRole creates or updates an AppRole role bound to policy --
// `bao write auth/approle/role/<name> token_policies=... secret_id_ttl=0
// token_num_uses=0`. secret_id_ttl=0/token_num_uses=0: no expiry, the same
// choice develop.md's manual steps make and for the same reason -- this is
// a long-lived credential for an unattended deploy tool, not a human's
// short-lived session. Safe to re-run: same reasoning as WritePolicy.
func CreateApproleRole(ctx context.Context, address, token, name, policy string) error {
	body, status, err := adminRequest(ctx, http.MethodPost, address+"/v1/auth/approle/role/"+name, token,
		map[string]any{
			"token_policies": policy,
			"token_ttl":      "1h",
			"token_max_ttl":  "4h",
			"secret_id_ttl":  "0",
			"token_num_uses": "0",
		})
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("couldn't create AppRole role %q (%d): %s", name, status, string(body))
	}
	return nil
}

// ReadRoleID fetches an AppRole role's role-id -- stable across repeated
// calls (unlike GenerateSecretID below), so ProvisionLocal calling this
// again on a later `configure local` gets back the same value a previous
// run already saved.
func ReadRoleID(ctx context.Context, address, token, name string) (string, error) {
	body, status, err := adminRequest(ctx, http.MethodGet, address+"/v1/auth/approle/role/"+name+"/role-id", token, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("couldn't read role-id for %q (%d): %s", name, status, string(body))
	}
	var parsed struct {
		Data struct {
			RoleID string `json:"role_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("couldn't parse role-id response: %w", err)
	}
	return parsed.Data.RoleID, nil
}

// GenerateSecretID mints a fresh secret-id for an AppRole role -- `bao
// write -f auth/approle/role/<name>/secret-id`. Unlike role-id, this
// generates a NEW value every call: ProvisionLocal only ever calls this
// once, the first time a target has no stored Credentials yet, precisely
// to avoid minting a throwaway extra secret-id on every later `configure`.
func GenerateSecretID(ctx context.Context, address, token, name string) (string, error) {
	body, status, err := adminRequest(ctx, http.MethodPost, address+"/v1/auth/approle/role/"+name+"/secret-id", token, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("couldn't generate secret-id for %q (%d): %s", name, status, string(body))
	}
	var parsed struct {
		Data struct {
			SecretID string `json:"secret_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("couldn't parse secret-id response: %w", err)
	}
	return parsed.Data.SecretID, nil
}
