package openbao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// kvServer answers GET /v1/secret/data/<path> from a map of canned bodies.
func kvServer(t *testing.T, bodies map[string]struct {
	status int
	body   string
}) (*Client, *http.Header) {
	t.Helper()
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
		canned, ok := bodies[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(canned.status)
		_, _ = w.Write([]byte(canned.body))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(&Credentials{Address: srv.URL})
	c.token = "test-token"
	return c, &seen
}

type canned = struct {
	status int
	body   string
}

func TestReadSecretVersioned(t *testing.T) {
	c, seen := kvServer(t, map[string]canned{
		"versola/vps/auth":    {200, `{"data":{"data":{"A":"1","B":"2"},"metadata":{"version":7,"created_time":"x"}}}`},
		"versola/vps/deleted": {200, `{"data":{"data":{},"metadata":{"version":3,"deletion_time":"x"}}}`},
		"versola/vps/broken":  {500, `boom`},
		"versola/vps/nometa":  {200, `{"data":{"data":{"A":"1"}}}`},
	})
	ctx := context.Background()

	t.Run("data and version", func(t *testing.T) {
		rec, ok, err := c.ReadSecretVersioned(ctx, "versola/vps/auth")
		if err != nil || !ok || rec.Version != 7 || rec.Data["A"] != "1" || rec.Data["B"] != "2" {
			t.Fatalf("%+v %v %v", rec, ok, err)
		}
		if (*seen).Get("X-Vault-Token") != "test-token" {
			t.Error("the token is not sent")
		}
	})
	t.Run("nothing written is ok=false and version 0", func(t *testing.T) {
		rec, ok, err := c.ReadSecretVersioned(ctx, "versola/vps/missing")
		if err != nil || ok || rec.Version != 0 || rec.Data != nil {
			t.Fatalf("%+v %v %v", rec, ok, err)
		}
	})
	t.Run("a path whose versions were all deleted is treated as empty, whatever its version says", func(t *testing.T) {
		rec, ok, err := c.ReadSecretVersioned(ctx, "versola/vps/deleted")
		if err != nil || ok || rec.Version != 0 {
			t.Fatalf("%+v %v %v", rec, ok, err)
		}
	})
	t.Run("a server error is an error", func(t *testing.T) {
		if _, ok, err := c.ReadSecretVersioned(ctx, "versola/vps/broken"); err == nil || ok {
			t.Fatal("expected an error")
		}
	})
	t.Run("a response without metadata still reads, as version 0", func(t *testing.T) {
		rec, ok, err := c.ReadSecretVersioned(ctx, "versola/vps/nometa")
		if err != nil || !ok || rec.Version != 0 || rec.Data["A"] != "1" {
			t.Fatalf("%+v %v %v", rec, ok, err)
		}
	})
}

// ReadSecret is what every existing caller uses; it must behave exactly as before.
func TestReadSecretIsUnchanged(t *testing.T) {
	c, _ := kvServer(t, map[string]canned{
		"versola/vps/auth":    {200, `{"data":{"data":{"A":"1"},"metadata":{"version":2}}}`},
		"versola/vps/deleted": {200, `{"data":{"data":{},"metadata":{"version":3}}}`},
		"versola/vps/broken":  {403, `denied`},
	})
	ctx := context.Background()

	data, ok, err := c.ReadSecret(ctx, "versola/vps/auth")
	if err != nil || !ok || len(data) != 1 || data["A"] != "1" {
		t.Errorf("%v %v %v", data, ok, err)
	}
	data, ok, err = c.ReadSecret(ctx, "versola/vps/missing")
	if err != nil || ok || data != nil {
		t.Errorf("404: %v %v %v", data, ok, err)
	}
	data, ok, err = c.ReadSecret(ctx, "versola/vps/deleted")
	if err != nil || ok || data != nil {
		t.Errorf("deleted: %v %v %v", data, ok, err)
	}
	if _, _, err = c.ReadSecret(ctx, "versola/vps/broken"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("error: %v", err)
	}
}
