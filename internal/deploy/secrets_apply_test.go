package deploy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/versolauth/versola-cli/internal/openbao"
	"github.com/versolauth/versola-cli/internal/secrets"
	"github.com/versolauth/versola-cli/internal/state"
)

// A fake OpenBao that keeps secrets in a map and remembers the order of writes.
type fakeBao struct {
	mu        sync.Mutex
	data      map[string]map[string]string // logical path -> data
	writes    []string                     // paths written, in order
	failWrite map[string]bool              // paths whose write answers 500
	onWrite   func(path string)            // called before each write is stored
}

func (f *fakeBao) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/v1/auth/approle/login" {
		_, _ = w.Write([]byte(`{"auth":{"client_token":"t"}}`))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
	switch r.Method {
	case http.MethodGet:
		d, ok := f.data[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := json.Marshal(map[string]any{"data": map[string]any{"data": d, "metadata": map[string]any{"version": 1}}})
		_, _ = w.Write(b)
	case http.MethodPost, http.MethodPut:
		if f.failWrite[path] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if f.onWrite != nil {
			f.onWrite(path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Data map[string]string `json:"data"`
		}
		_ = json.Unmarshal(body, &req)
		f.data[path] = req.Data
		f.writes = append(f.writes, path)
		_, _ = w.Write([]byte(`{}`))
	}
}

const vpsSchemaFixture = "../secrets/testdata/secrets.schema.vps.json"
const localSchemaFixture = "../secrets/testdata/secrets.schema.docker-local.json"

type harness struct {
	t      *testing.T
	target string
	schema *secrets.Schema
	bao    *fakeBao
	client *openbao.Client
	dir    string
}

// newHarness: an isolated home, a fake OpenBao, and an empty bundle directory.
func newHarness(t *testing.T, target, schemaFixture string) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	h := &harness{t: t, target: target, dir: t.TempDir(), bao: &fakeBao{data: map[string]map[string]string{}, failWrite: map[string]bool{}}}
	srv := httptest.NewServer(h.bao)
	t.Cleanup(srv.Close)
	h.client = openbao.NewClient(&openbao.Credentials{Address: srv.URL, RoleID: "r", SecretID: "s"})
	if err := h.client.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if schemaFixture != "" {
		raw, err := os.ReadFile(schemaFixture)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.dir, secrets.FileName), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		cliTarget := target
		h.schema, err = secrets.Load(filepath.Join(h.dir, secrets.FileName), cliTarget)
		if err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func isUtilsSpec(spec secrets.Spec) bool {
	return (spec.File != nil && *spec.File == secrets.UtilsKeyFile) || hasString(spec.Services, "utils") ||
		spec.Name == "UTILITY_CLIENT_PUBLIC_JWK"
}

func hasString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// writeCandidates writes the *.generated-secrets.env versola-tools would: one
// value per secret, the same in every service of a shared entry, tagged by tag.
func (h *harness) writeCandidates(tag string) secrets.Values {
	h.t.Helper()
	cands := secrets.Values{}
	for _, service := range secretServices {
		cands[service] = map[string]string{}
	}
	for _, spec := range h.schema.Secrets {
		if isUtilsSpec(spec) {
			continue
		}
		for _, service := range spec.Services {
			cands[service][spec.Name] = tag + "-" + spec.Name
		}
	}
	for _, service := range secretServices {
		if err := writeDotenv(filepath.Join(h.dir, service+".generated-secrets.env"), cands[service]); err != nil {
			h.t.Fatal(err)
		}
	}
	return cands
}

// store puts value tag-NAME at every (service, name) of the schema except the
// names in without.
func (h *harness) store(tag string, without ...string) {
	for _, spec := range h.schema.Secrets {
		if isUtilsSpec(spec) || hasString(without, spec.Name) {
			continue
		}
		for _, service := range spec.Services {
			p := openbao.SecretPath(h.target, service)
			if h.bao.data[p] == nil {
				h.bao.data[p] = map[string]string{}
			}
			h.bao.data[p][spec.Name] = tag + "-" + spec.Name
		}
	}
}

func (h *harness) recordDeployment(revision int) {
	h.t.Helper()
	b, err := state.Prepare()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := state.FinalizeWithRevision(h.target, "1.0.0", b, "https://id.example.com", "nginx", revision); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) run(opts SecretOptions) (secretsOutcome, error) {
	return resolveSecretsWith(context.Background(), h.client, h.dir, h.target, "1.0.0", opts)
}

func (h *harness) value(service, name string) (string, bool) {
	v, ok := h.bao.data[openbao.SecretPath(h.target, service)][name]
	return v, ok
}

func TestSecretsFirstInstallWritesEverythingConsistently(t *testing.T) {
	h := newHarness(t, "vps", vpsSchemaFixture)
	cands := h.writeCandidates("c1")
	noteWhenFirstWriteHappened := (*state.Pending)(nil)
	h.bao.onWrite = func(string) {
		if noteWhenFirstWriteHappened == nil {
			noteWhenFirstWriteHappened, _ = state.LoadPending()
		}
	}

	out, err := h.run(SecretOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Revision != 1 || out.NewPostgresPassword != cands["auth"]["POSTGRES_PASSWORD"] {
		t.Errorf("%+v", out)
	}
	if noteWhenFirstWriteHappened == nil || noteWhenFirstWriteHappened.Target != "vps" {
		t.Error("the note of an unfinished install must exist before the first secret is written")
	}
	if got := strings.Join(h.bao.writes, ","); got != "versola/vps/auth,versola/vps/central,versola/vps/edge" {
		t.Errorf("writes: %s", got)
	}
	for _, name := range []string{"CLIENT_SECRETS_SECRET", "CENTRAL_SECRET_KEY"} {
		a, _ := h.value("auth", name)
		c, _ := h.value("central", name)
		if a == "" || a != c {
			t.Errorf("%s differs between auth and central", name)
		}
	}
	for _, service := range secretServices {
		if _, err := os.Stat(filepath.Join(h.dir, service+".secrets.env")); err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(filepath.Join(h.dir, service+".generated-secrets.env")); !os.IsNotExist(err) {
			t.Errorf("%s candidates were not removed", service)
		}
	}
}

func TestSecretsAProblemStopsBeforeAnythingIsWritten(t *testing.T) {
	h := newHarness(t, "vps", vpsSchemaFixture)
	h.writeCandidates("c1")
	h.store("old", "PASSWORDS_SECRET") // a secret data depends on has gone missing
	h.recordDeployment(1)

	_, err := h.run(SecretOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("%v", err)
	}
	if len(h.bao.writes) != 0 {
		t.Errorf("wrote %v", h.bao.writes)
	}
	if note, _ := state.LoadPending(); note != nil {
		t.Error("a stopped plan must not leave a note")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "auth.secrets.env")); !os.IsNotExist(err) {
		t.Error("no *.secrets.env may be written either")
	}

	t.Run("--generate-missing confirms exactly the named secret", func(t *testing.T) {
		if _, err := h.run(SecretOptions{GenerateMissing: []string{"PASSWORDS_SECRET"}}); err != nil {
			t.Fatal(err)
		}
		if v, _ := h.value("auth", "PASSWORDS_SECRET"); v != "c1-PASSWORDS_SECRET" {
			t.Errorf("not generated: %q", v)
		}
	})
}

func TestSecretsAnUnconfirmedUnknownNameStopsToo(t *testing.T) {
	h := newHarness(t, "vps", vpsSchemaFixture)
	h.writeCandidates("c1")
	_, err := h.run(SecretOptions{GenerateMissing: []string{"PASWORDS_SECRET"}}) // a typo
	if err == nil || len(h.bao.writes) != 0 {
		t.Fatalf("%v %v", err, h.bao.writes)
	}
}

// The reason the note exists: an install that fails partway leaves secrets in
// OpenBao and no record of a deployment.
func TestSecretsAnInterruptedFirstInstallIsFinished(t *testing.T) {
	for _, failing := range []string{"auth", "central", "edge"} {
		t.Run("the write of "+failing+" fails", func(t *testing.T) {
			h := newHarness(t, "vps", vpsSchemaFixture)
			h.writeCandidates("c1")
			h.bao.failWrite[openbao.SecretPath("vps", failing)] = true
			if _, err := h.run(SecretOptions{}); err == nil {
				t.Fatal("the failing write must fail the run")
			}
			if note, _ := state.LoadPending(); note == nil {
				t.Fatal("the interrupted install must have left its note")
			}

			// The retry: new candidates (versola-tools generates fresh ones each run).
			delete(h.bao.failWrite, openbao.SecretPath("vps", failing))
			cands := h.writeCandidates("c2")
			before := map[string]map[string]string{}
			for p, d := range h.bao.data {
				before[p] = map[string]string{}
				for k, v := range d {
					before[p][k] = v
				}
			}
			plan := secrets.Plan(h.schema, cands, secrets.Values(before2values(h, before)), nil,
				secrets.Options{Target: "vps", InstallInProgress: true})
			if !plan.OK() {
				t.Fatalf("the retry's plan stops: %+v", plan.Problems)
			}
			if _, err := h.run(SecretOptions{}); err != nil {
				t.Fatal(err)
			}
			assertStoreFollowsPlan(t, h, plan, before, cands)

			// Every key group is consistent: all of its members from one run.
			for _, group := range [][]struct{ service, name string }{
				{{"auth", "JWT_PRIVATE_KEY"}, {"central", "JWKS_JSON"}},
				{{"central", "EDGE_PUBLIC_JWK"}, {"edge", "EDGE_PRIVATE_KEY"}, {"edge", "EDGE_KEY_ID"}},
			} {
				tags := map[string]bool{}
				for _, m := range group {
					v, ok := h.value(m.service, m.name)
					if !ok {
						t.Fatalf("%s/%s is missing", m.service, m.name)
					}
					tags[strings.SplitN(v, "-", 2)[0]] = true
				}
				if len(tags) != 1 {
					t.Errorf("a key group mixes values of two runs: %v", group)
				}
			}
			// A value the first run stored and nothing replaces is kept.
			if failing != "auth" {
				if v, _ := h.value("auth", "PASSWORDS_SECRET"); v != "c1-PASSWORDS_SECRET" {
					t.Errorf("a stored secret was replaced: %q", v)
				}
			}
			// The shared ones still agree.
			for _, name := range []string{"CLIENT_SECRETS_SECRET", "CENTRAL_SECRET_KEY"} {
				a, _ := h.value("auth", name)
				c, _ := h.value("central", name)
				if a == "" || a != c {
					t.Errorf("%s differs between auth and central (%q, %q)", name, a, c)
				}
			}
		})
	}

	t.Run("without the note the same store stops: that is what the note is for", func(t *testing.T) {
		h := newHarness(t, "vps", vpsSchemaFixture)
		h.writeCandidates("c1")
		h.bao.failWrite[openbao.SecretPath("vps", "edge")] = true
		if _, err := h.run(SecretOptions{}); err == nil {
			t.Fatal("expected the interrupted write to fail")
		}
		if err := state.ClearPending(); err != nil {
			t.Fatal(err)
		}
		delete(h.bao.failWrite, openbao.SecretPath("vps", "edge"))
		h.writeCandidates("c2")
		writes := len(h.bao.writes)
		if _, err := h.run(SecretOptions{}); err == nil || len(h.bao.writes) != writes {
			t.Fatalf("%v, %d new writes", err, len(h.bao.writes)-writes)
		}
	})
}

func before2values(h *harness, m map[string]map[string]string) secrets.Values {
	v := secrets.Values{}
	for _, service := range secretServices {
		v[service] = m[openbao.SecretPath(h.target, service)]
	}
	return v
}

// assertStoreFollowsPlan: what ended up in OpenBao is what the plan said.
func assertStoreFollowsPlan(t *testing.T, h *harness, plan secrets.Result, before map[string]map[string]string, cands secrets.Values) {
	t.Helper()
	for _, a := range plan.Actions {
		got, ok := h.value(a.Service, a.Name)
		old := before[openbao.SecretPath(h.target, a.Service)][a.Name]
		switch {
		case a.Kind == secrets.ActionKeep:
			if !ok || got != old {
				t.Errorf("%s/%s: kept, but %q became %q", a.Service, a.Name, old, got)
			}
		case a.Kind == secrets.ActionReplace || (a.Kind == secrets.ActionCreate && a.Source == secrets.SourceGenerated):
			// A shared secret has one value, from the candidate of any of its services.
			if !ok || got != cands[a.Service][a.Name] {
				t.Errorf("%s/%s: %s from a candidate, but got %q (candidate %q)", a.Service, a.Name, a.Kind, got, cands[a.Service][a.Name])
			}
		case a.Kind == secrets.ActionCreate && a.Source == secrets.SourceStored:
			// Filled from a service that has it stored.
			var from string
			for _, spec := range h.schema.Secrets {
				if spec.Name == a.Name && hasString(spec.Services, a.Service) {
					for _, svc := range spec.Services {
						if v, has := before[openbao.SecretPath(h.target, svc)][a.Name]; has {
							from = v
						}
					}
				}
			}
			if !ok || got != from || from == "" {
				t.Errorf("%s/%s: fill-shared should copy the stored %q, got %q", a.Service, a.Name, from, got)
			}
		}
	}
}

func TestSecretsSharedSecretsStayOneValue(t *testing.T) {
	// auth holds the shared secrets, central lost them: central must get auth's
	// values and not fresh random ones (mergeSecrets alone used to give it fresh ones).
	h := newHarness(t, "vps", vpsSchemaFixture)
	cands := h.writeCandidates("c1")
	h.store("old")
	delete(h.bao.data[openbao.SecretPath("vps", "central")], "CLIENT_SECRETS_SECRET")
	delete(h.bao.data[openbao.SecretPath("vps", "central")], "CENTRAL_SECRET_KEY")
	h.recordDeployment(1)
	before := map[string]map[string]string{}
	for p, d := range h.bao.data {
		before[p] = map[string]string{}
		for k, v := range d {
			before[p][k] = v
		}
	}
	plan := secrets.Plan(h.schema, cands, before2values(h, before), &secrets.Previous{Target: "vps", Revision: 1}, secrets.Options{Target: "vps"})
	if !plan.OK() {
		t.Fatalf("%+v", plan.Problems)
	}
	if _, err := h.run(SecretOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLIENT_SECRETS_SECRET", "CENTRAL_SECRET_KEY"} {
		if v, _ := h.value("central", name); v != "old-"+name {
			t.Errorf("central/%s = %q, want auth's stored value", name, v)
		}
	}
	assertStoreFollowsPlan(t, h, plan, before, cands)
}

func TestSecretsMergeSharedKeepsThePostgresBehaviour(t *testing.T) {
	final, wrote := mergeSecrets(nil, map[string]string{"A": "1", postgresPasswordKey: "cand"}, "shared")
	if !wrote || final["A"] != "1" || final[postgresPasswordKey] != "shared" {
		t.Errorf("%v %v", final, wrote)
	}
	final, wrote = mergeSecretsShared(map[string]string{"K": "stored"}, map[string]string{"K": "cand", "S": "cand"}, map[string]string{"S": "one-value"})
	if !wrote || final["K"] != "stored" || final["S"] != "one-value" {
		t.Errorf("%v %v", final, wrote)
	}
}

func TestSecretsReleaseWithoutASchema(t *testing.T) {
	t.Run("a first install needs no flag", func(t *testing.T) {
		h := newHarness(t, "vps", "")
		h.schema = mustSchema(t, vpsSchemaFixture)
		h.writeCandidates("c1")
		if out, err := h.run(SecretOptions{}); err != nil || out.Revision != 0 || len(h.bao.writes) != 3 {
			t.Fatalf("%+v %v %v", out, err, h.bao.writes)
		}
		if note, _ := state.LoadPending(); note == nil || note.Target != "vps" || note.SecretsRevision != 0 {
			t.Errorf("a first install proper is noted, so that its own retry is recognised: %+v", note)
		}
	})
	t.Run("the retry of its own interrupted first install needs no flag", func(t *testing.T) {
		h := newHarness(t, "vps", "")
		h.schema = mustSchema(t, vpsSchemaFixture)
		h.writeCandidates("c1")
		h.bao.failWrite[openbao.SecretPath("vps", "central")] = true
		if _, err := h.run(SecretOptions{}); err == nil {
			t.Fatal("expected the interrupted write to fail")
		}
		delete(h.bao.failWrite, openbao.SecretPath("vps", "central"))
		h.writeCandidates("c2")
		if _, err := h.run(SecretOptions{}); err != nil {
			t.Fatalf("the retry of the operator's own attempt must not need --allow-legacy-secrets: %v", err)
		}
		// Without the note the same store is refused.
		h2 := newHarness(t, "vps", "")
		h2.schema = mustSchema(t, vpsSchemaFixture)
		h2.writeCandidates("c1")
		h2.store("old", "PASSWORDS_SECRET")
		if _, err := h2.run(SecretOptions{}); err == nil {
			t.Fatal("a store that is not this machine's own unfinished install must be refused")
		}
	})
	t.Run("secrets already in OpenBao stop it unless --allow-legacy-secrets", func(t *testing.T) {
		h := newHarness(t, "vps", "")
		h.schema = mustSchema(t, vpsSchemaFixture)
		h.writeCandidates("c1")
		h.store("old", "PASSWORDS_SECRET")
		_, err := h.run(SecretOptions{})
		if err == nil || !strings.Contains(err.Error(), "--allow-legacy-secrets") || len(h.bao.writes) != 0 {
			t.Fatalf("%v %v", err, h.bao.writes)
		}
		if _, err := h.run(SecretOptions{AllowLegacy: true}); err != nil {
			t.Fatal(err)
		}
		if v, _ := h.value("auth", "PASSWORDS_SECRET"); v != "c1-PASSWORDS_SECRET" {
			t.Errorf("the old rule generates what is missing: %q", v)
		}
	})
	t.Run("a record of a deployment alone stops it too", func(t *testing.T) {
		h := newHarness(t, "vps", "")
		h.schema = mustSchema(t, vpsSchemaFixture)
		h.writeCandidates("c1")
		h.recordDeployment(1)
		if _, err := h.run(SecretOptions{}); err == nil || len(h.bao.writes) != 0 {
			t.Fatalf("%v %v", err, h.bao.writes)
		}
	})
	t.Run("local never needs the flag", func(t *testing.T) {
		h := newHarness(t, "local", "")
		h.schema = mustSchema(t, localSchemaFixture)
		h.writeCandidates("c1")
		h.store("old", "PASSWORDS_SECRET")
		if _, err := h.run(SecretOptions{}); err != nil {
			t.Fatal(err)
		}
	})
}

func mustSchema(t *testing.T, path string) *secrets.Schema {
	t.Helper()
	cli := "vps"
	if strings.Contains(path, "docker-local") {
		cli = "local"
	}
	s, err := secrets.Load(path, cli)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSecretsLocalIsPlannedButLeavesNoNote(t *testing.T) {
	h := newHarness(t, "local", localSchemaFixture)
	h.writeCandidates("c1")
	if out, err := h.run(SecretOptions{}); err != nil || out.Revision != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	if note, _ := state.LoadPending(); note != nil {
		t.Error("local is a throwaway: no note")
	}
}

func TestSecretsNoValueIsPrintedWhenThePlanStops(t *testing.T) {
	h := newHarness(t, "vps", vpsSchemaFixture)
	h.writeCandidates("S3CR3T-MARKER")
	h.store("S3CR3T-MARKER", "PASSWORDS_SECRET")
	h.recordDeployment(1)

	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	_, err := h.run(SecretOptions{})
	os.Stdout = old
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if err == nil {
		t.Fatal("expected a stop")
	}
	if strings.Contains(string(out)+err.Error(), "S3CR3T") {
		t.Errorf("a value reached the output:\n%s\n%v", out, err)
	}
	if !strings.Contains(string(out), "PASSWORDS_SECRET") {
		t.Errorf("the problem is not named:\n%s", out)
	}
}

// The note is for a first install proper only. An install of unknown origin
// (OpenBao holds secrets, this machine has no record) may be a live deployment:
// a note left by ITS configure would let the next run regenerate what looks lost.
func TestSecretsAnInstallOfUnknownOriginLeavesNoNote(t *testing.T) {
	h := newHarness(t, "vps", vpsSchemaFixture)
	h.writeCandidates("c1")
	h.store("old") // everything is there, no record of a deployment
	if _, err := h.run(SecretOptions{}); err != nil {
		t.Fatal(err)
	}
	if note, _ := state.LoadPending(); note != nil {
		t.Fatalf("a note was written for an install of unknown origin: %+v", note)
	}
	// So when it fails later and a secret then goes missing, the next run still stops.
	delete(h.bao.data[openbao.SecretPath("vps", "auth")], "PASSWORDS_SECRET")
	h.writeCandidates("c2")
	if _, err := h.run(SecretOptions{}); err == nil {
		t.Fatal("a lost secret was regenerated without a flag")
	}
}

// customSchema: the vps fixture plus extra entries, at the given revision.
func customSchema(t *testing.T, revision int, extra string) string {
	t.Helper()
	raw, err := os.ReadFile(vpsSchemaFixture)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var more []any
	if err := json.Unmarshal([]byte(extra), &more); err != nil {
		t.Fatal(err)
	}
	doc["revision"] = revision
	doc["secrets"] = append(doc["secrets"].([]any), more...)
	b, _ := json.Marshal(doc)
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// An upgrade whose release adds a key group, interrupted halfway: nothing
// depends on the new group, so the retry generates it again whole.
func TestSecretsAnInterruptedUpgradeThatAddsAGroupIsFinished(t *testing.T) {
	path := customSchema(t, 2, `[
	  {"name":"NEW_PRIV","services":["auth"],"type":"opaque","size":null,"group":"newg","onMissing":"generate-on-first-install-only","since":2,"file":null},
	  {"name":"NEW_PUB","services":["central"],"type":"opaque","size":null,"group":"newg","onMissing":"generate-on-first-install-only","since":2,"file":null}]`)
	h := newHarness(t, "vps", path)
	h.writeCandidates("c1")
	h.store("old", "NEW_PRIV", "NEW_PUB") // the deployed release did not have the group
	h.recordDeployment(1)
	h.bao.failWrite[openbao.SecretPath("vps", "central")] = true
	if _, err := h.run(SecretOptions{}); err == nil {
		t.Fatal("expected the interrupted write to fail")
	}
	if v, _ := h.value("auth", "NEW_PRIV"); v != "c1-NEW_PRIV" {
		t.Fatalf("setup: auth should hold the first half, got %q", v)
	}
	delete(h.bao.failWrite, openbao.SecretPath("vps", "central"))
	h.writeCandidates("c2")
	if _, err := h.run(SecretOptions{}); err != nil {
		t.Fatalf("the retry is a dead end: %v", err)
	}
	priv, _ := h.value("auth", "NEW_PRIV")
	pub, _ := h.value("central", "NEW_PUB")
	if priv != "c2-NEW_PRIV" || pub != "c2-NEW_PUB" {
		t.Errorf("the group is not from one run: %q %q", priv, pub)
	}
	// What was deployed is untouched.
	if v, _ := h.value("auth", "PASSWORDS_SECRET"); v != "old-PASSWORDS_SECRET" {
		t.Errorf("a deployed secret changed: %q", v)
	}

	t.Run("a group that was already deployed is never regenerated on upgrade", func(t *testing.T) {
		h := newHarness(t, "vps", vpsSchemaFixture)
		h.writeCandidates("c1")
		h.store("old")
		delete(h.bao.data[openbao.SecretPath("vps", "central")], "JWKS_JSON")
		h.recordDeployment(1)
		if _, err := h.run(SecretOptions{GenerateMissing: []string{"JWT_PRIVATE_KEY", "JWKS_JSON"}}); err == nil || len(h.bao.writes) != 0 {
			t.Fatalf("%v %v", err, h.bao.writes)
		}
	})
}

// A shared member of a key group: the restart must replace every copy with the
// candidate, not leave one at the value the interrupted run stored.
func TestSecretsARestartedGroupWithASharedMemberStaysOneValue(t *testing.T) {
	path := customSchema(t, 1, `[
	  {"name":"SHARED_KEY","services":["auth","central"],"type":"opaque","size":null,"group":"sg","onMissing":"generate-on-first-install-only","since":1,"file":null},
	  {"name":"EDGE_HALF","services":["edge"],"type":"opaque","size":null,"group":"sg","onMissing":"generate-on-first-install-only","since":1,"file":null}]`)
	h := newHarness(t, "vps", path)
	h.writeCandidates("c1")
	h.bao.failWrite[openbao.SecretPath("vps", "central")] = true
	if _, err := h.run(SecretOptions{}); err == nil {
		t.Fatal("expected the interrupted write to fail")
	}
	delete(h.bao.failWrite, openbao.SecretPath("vps", "central"))
	h.writeCandidates("c2")
	if _, err := h.run(SecretOptions{}); err != nil {
		t.Fatal(err)
	}
	a, _ := h.value("auth", "SHARED_KEY")
	c, _ := h.value("central", "SHARED_KEY")
	e, _ := h.value("edge", "EDGE_HALF")
	if a != "c2-SHARED_KEY" || c != "c2-SHARED_KEY" || e != "c2-EDGE_HALF" {
		t.Errorf("auth %q, central %q, edge %q", a, c, e)
	}
}

// The process can be killed between the write that stores the new Postgres
// password and the print that tells the operator about it.
func TestSecretsTheNewPostgresPasswordIsSaidAgainWhenAnInstallIsContinued(t *testing.T) {
	h := newHarness(t, "vps", vpsSchemaFixture)
	cands := h.writeCandidates("c1")
	h.bao.failWrite[openbao.SecretPath("vps", "central")] = true
	if _, err := h.run(SecretOptions{}); err == nil {
		t.Fatal("expected the interrupted write to fail")
	}
	delete(h.bao.failWrite, openbao.SecretPath("vps", "central"))
	h.writeCandidates("c2")
	out, err := h.run(SecretOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.NewPostgresPassword != cands["auth"]["POSTGRES_PASSWORD"] {
		t.Errorf("the role never gets its password: %q", out.NewPostgresPassword)
	}

	t.Run("an ordinary upgrade does not repeat it", func(t *testing.T) {
		h := newHarness(t, "vps", vpsSchemaFixture)
		h.writeCandidates("c1")
		h.store("old")
		h.recordDeployment(1)
		if out, err := h.run(SecretOptions{}); err != nil || out.NewPostgresPassword != "" {
			t.Fatalf("%+v %v", out, err)
		}
	})
}

func TestSecretsANoteOfAnotherTargetIsNotThisInstall(t *testing.T) {
	newHarness(t, "vps", "") // isolated home
	if err := state.SavePending(state.Pending{Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if _, installing, err := loadPrevious("vps"); err != nil || installing {
		t.Fatalf("installing=%v err=%v", installing, err)
	}
	if _, installing, err := loadPrevious("local"); err != nil || !installing {
		t.Fatalf("installing=%v err=%v", installing, err)
	}
}

func TestSecretsAFillSharedCopyIsStoredEvenWithoutACandidateForIt(t *testing.T) {
	final, wrote := mergeSecretsShared(map[string]string{"A": "1"}, map[string]string{"A": "x"}, map[string]string{"SHARED": "from-auth"})
	if !wrote || final["SHARED"] != "from-auth" || final["A"] != "1" {
		t.Errorf("%v %v", final, wrote)
	}
}
