package openbao

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// kvMount is the KV v2 secrets engine mount path this CLI expects to
// already be enabled at (see develop.md's OpenBao setup section) —
// OpenBao's own default name when a KV v2 engine is enabled without
// specifying -path.
const kvMount = "secret"

// Client talks to one target's OpenBao server, authenticated via AppRole.
//
// Not safe for concurrent use by multiple goroutines — this CLI never
// needs that, every command that touches secrets does so from a single
// goroutine — but is meant to be reused across multiple reads/writes
// within one command: call Login once, then ReadSecret/WriteSecret as
// needed, rather than logging in again for each.
type Client struct {
	address  string
	roleID   string
	secretID string
	http     *http.Client
	token    string
}

// NewClient builds a Client for the given credentials. It does not contact
// the server — call Login before ReadSecret/WriteSecret.
func NewClient(creds *Credentials) *Client {
	return &Client{
		address:  creds.Address,
		roleID:   creds.RoleID,
		secretID: creds.SecretID,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Login authenticates via AppRole and stores the resulting client token
// for subsequent calls.
func (c *Client) Login(ctx context.Context) error {
	reqBody, err := json.Marshal(map[string]string{
		"role_id":   c.roleID,
		"secret_id": c.secretID,
	})
	if err != nil {
		return fmt.Errorf("couldn't encode AppRole login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.address+"/v1/auth/approle/login", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("couldn't build AppRole login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach OpenBao at %s: %w", c.address, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("couldn't read OpenBao's login response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Deliberately not including reqBody in this error — it holds
		// secretID.
		return fmt.Errorf("OpenBao login failed (%s): %s", resp.Status, string(body))
	}

	var loginResp struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &loginResp); err != nil {
		return fmt.Errorf("couldn't parse OpenBao's login response: %w", err)
	}
	if loginResp.Auth.ClientToken == "" {
		return fmt.Errorf("OpenBao's login response had no client token")
	}
	c.token = loginResp.Auth.ClientToken
	return nil
}

// SecretPath builds the logical (not HTTP) path for a service's secrets
// under a target, e.g. SecretPath("local", "central") = "versola/local/central".
// One path per service per target: each holds every secret field that
// service's config needs as a flat set of key-value pairs.
func SecretPath(target, service string) string {
	return fmt.Sprintf("versola/%s/%s", target, service)
}

// SecretRecord is what a KV v2 read returns: the fields stored at a path and
// the version of the record they came from.
type SecretRecord struct {
	Data map[string]string
	// Version is KV v2's own number for this record (1 for the first write, +1
	// for every later one). A later write can pass it back as `cas`
	// (check-and-set) to be refused if someone else wrote in between.
	Version int
}

// ReadSecret fetches the fields stored at path (see SecretPath), or
// ok=false if nothing has been written there yet — callers use that to
// tell "generate this for the first time" apart from a real error.
//
// It is ReadSecretVersioned without the version, kept so that callers that
// don't need one stay as they are.
func (c *Client) ReadSecret(ctx context.Context, path string) (data map[string]string, ok bool, err error) {
	rec, ok, err := c.ReadSecretVersioned(ctx, path)
	return rec.Data, ok, err
}

// ReadSecretVersioned is ReadSecret that also reports the record's version.
// When ok is false the record is the zero value (no data, version 0).
func (c *Client) ReadSecretVersioned(ctx context.Context, path string) (rec SecretRecord, ok bool, err error) {
	url := fmt.Sprintf("%s/v1/%s/data/%s", c.address, kvMount, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SecretRecord{}, false, fmt.Errorf("couldn't build OpenBao read request: %w", err)
	}
	req.Header.Set("X-Vault-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return SecretRecord{}, false, fmt.Errorf("couldn't reach OpenBao at %s: %w", c.address, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return SecretRecord{}, false, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return SecretRecord{}, false, fmt.Errorf("couldn't read OpenBao's response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return SecretRecord{}, false, fmt.Errorf("OpenBao read failed (%s): %s", resp.Status, string(body))
	}

	var readResp struct {
		Data struct {
			Data     map[string]string `json:"data"`
			Metadata struct {
				Version int `json:"version"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &readResp); err != nil {
		return SecretRecord{}, false, fmt.Errorf("couldn't parse OpenBao's response: %w", err)
	}
	// A path whose every version has been deleted (not just never written)
	// also 200s, with an empty inner "data" — treat that the same as 404,
	// since either way there's nothing to read.
	if len(readResp.Data.Data) == 0 {
		return SecretRecord{}, false, nil
	}
	return SecretRecord{Data: readResp.Data.Data, Version: readResp.Data.Metadata.Version}, true, nil
}

// WriteSecret stores data at path (see SecretPath), creating a new
// version if something is already there.
func (c *Client) WriteSecret(ctx context.Context, path string, data map[string]string) error {
	reqBody, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		return fmt.Errorf("couldn't encode OpenBao write request: %w", err)
	}

	url := fmt.Sprintf("%s/v1/%s/data/%s", c.address, kvMount, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("couldn't build OpenBao write request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach OpenBao at %s: %w", c.address, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Deliberately not including reqBody in this error — it holds the
		// secret values being written.
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("OpenBao write failed (%s): %s", resp.Status, string(body))
	}
	return nil
}
