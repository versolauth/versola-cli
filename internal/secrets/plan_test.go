package secrets

import (
	"reflect"
	"sort"
	"testing"
)

// ---- helpers -------------------------------------------------------------

func ptr[T any](v T) *T { return &v }

// candidatesFor gives every (service, name) of the schema a candidate, the same
// one in every service of a shared entry (which is what gen-env writes).
func candidatesFor(s *Schema) Values {
	v := Values{}
	for _, spec := range s.Secrets {
		for _, service := range spec.Services {
			if v[service] == nil {
				v[service] = map[string]string{}
			}
			v[service][spec.Name] = "candidate-" + spec.Name
		}
	}
	return v
}

// storedFor is a store holding a value for everything in the schema except the
// names in without. The utils entries are left out, as the CLI keeps them apart
// (and the policy services are all Plan looks at).
func storedFor(s *Schema, without ...string) Values {
	skip := map[string]bool{}
	for _, n := range without {
		skip[n] = true
	}
	v := Values{}
	for _, spec := range s.Secrets {
		if skip[spec.Name] || (spec.File != nil && *spec.File == UtilsKeyFile) {
			continue
		}
		for _, service := range spec.Services {
			if !contains(PolicyServices, service) {
				continue
			}
			if v[service] == nil {
				v[service] = map[string]string{}
			}
			v[service][spec.Name] = "stored-" + spec.Name
		}
	}
	return v
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func find(r Result, kind ActionKind, service, name string) (Action, bool) {
	for _, a := range r.Actions {
		if a.Kind == kind && a.Service == service && a.Name == name {
			return a, true
		}
	}
	return Action{}, false
}

func problemsOf(r Result, kind ProblemKind) []Problem {
	var out []Problem
	for _, p := range r.Problems {
		if p.Kind == kind {
			out = append(out, p)
		}
	}
	return out
}

func problemNames(r Result, kind ProblemKind) []string {
	var names []string
	for _, p := range problemsOf(r, kind) {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

func count(r Result, kind ActionKind) (n int) {
	for _, a := range r.Actions {
		if a.Kind == kind {
			n++
		}
	}
	return
}

func wantCreate(t *testing.T, r Result, service, name string, source Source, reason Reason) {
	t.Helper()
	a, ok := find(r, ActionCreate, service, name)
	if !ok {
		t.Errorf("no create for %s/%s in %+v", service, name, r.Actions)
		return
	}
	if a.Source != source || a.Reason != reason {
		t.Errorf("%s/%s: create from %q because %q, want %q because %q", service, name, a.Source, a.Reason, source, reason)
	}
}

func wantNoAction(t *testing.T, r Result, service, name string) {
	t.Helper()
	for _, a := range r.Actions {
		if a.Service == service && a.Name == name {
			t.Errorf("unexpected %s for %s/%s", a.Kind, service, name)
		}
	}
}

// the 11 non-utils first-install-only entries of the vps schema (the utils
// pair is left to its own engine): 6 single secrets + the jwt and edge-key groups.
var vpsFirstInstallOnly = []string{
	"CENTRAL_RESOURCE_SECRET", "CLIENT_SECRETS_SECRET", "EDGE_KEY_ID", "EDGE_PRIVATE_KEY", "EDGE_PUBLIC_JWK",
	"EDGE_TOKEN_ENC_KEY", "JWKS_JSON", "JWT_PRIVATE_KEY", "PASSWORDS_SECRET", "POSTGRES_PASSWORD", "REFRESH_TOKENS_SECRET",
}

var vps1 = &Previous{Target: "vps", Revision: 1}

// ---- the decision table ---------------------------------------------------

func TestPlanFirstInstall(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")
	r := Plan(s, candidatesFor(s), Values{}, nil, Options{Target: "vps"})

	if !r.OK() || r.State != StateFirstInstall {
		t.Fatalf("state %s, problems %+v", r.State, r.Problems)
	}
	// Everything that is not the utils pair is created from its candidate: one
	// action per (service, name).
	wantCreate(t, r, "auth", "PASSWORDS_SECRET", SourceGenerated, ReasonFirstInstall)
	wantCreate(t, r, "auth", "JWT_PRIVATE_KEY", SourceGenerated, ReasonFirstInstall)
	wantCreate(t, r, "auth", "AUTH_CODES_SECRET", SourceGenerated, ReasonGenerate)
	for _, service := range []string{"auth", "central", "edge"} {
		wantCreate(t, r, service, "POSTGRES_PASSWORD", SourceGenerated, ReasonFirstInstall)
	}
	wantCreate(t, r, "auth", "CLIENT_SECRETS_SECRET", SourceGenerated, ReasonFirstInstall)
	wantCreate(t, r, "central", "CLIENT_SECRETS_SECRET", SourceGenerated, ReasonFirstInstall)
	if count(r, ActionSkipUtils) != 2 || count(r, ActionKeep) != 0 || count(r, ActionOrphan) != 0 {
		t.Errorf("actions: %+v", r.Actions)
	}
	if r.DeployedRevision != 0 {
		t.Errorf("a first install has no deployed revision, got %d", r.DeployedRevision)
	}
}

func TestPlanUpgrade(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")

	t.Run("a new secret is generated, the rest is kept", func(t *testing.T) {
		// A later revision of the schema that adds a first-install-only secret.
		next := *s
		next.Revision = ptr(2)
		next.Secrets = append(append([]Spec{}, s.Secrets...), Spec{
			Name: "NEW_BOUND_SECRET", Services: []string{"auth"}, Type: "base64url", Size: ptr(32),
			OnMissing: GenerateOnFirstInstallOnly, Since: ptr(2),
		})
		cands := candidatesFor(&next)
		r := Plan(&next, cands, storedFor(&next, "NEW_BOUND_SECRET"), vps1, Options{Target: "vps"})
		if !r.OK() || r.State != StateUpgrade || r.DeployedRevision != 1 || r.SchemaRevision != 2 {
			t.Fatalf("%+v", r)
		}
		wantCreate(t, r, "auth", "NEW_BOUND_SECRET", SourceGenerated, ReasonNewInRevision)
		if _, ok := find(r, ActionKeep, "auth", "PASSWORDS_SECRET"); !ok {
			t.Error("a stored secret must be kept")
		}
		if count(r, ActionCreate) != 1 {
			t.Errorf("only the new secret is created: %+v", r.Actions)
		}
	})

	t.Run("a secret that existed and is gone is lost, and nothing is generated for it", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s, "PASSWORDS_SECRET", "AUTH_CODES_SECRET"), vps1, Options{Target: "vps"})
		if r.OK() || r.State != StateUpgrade {
			t.Fatalf("%+v", r)
		}
		if got := problemNames(r, ProblemLost); !reflect.DeepEqual(got, []string{"PASSWORDS_SECRET"}) {
			t.Errorf("lost: %v", got)
		}
		wantNoAction(t, r, "auth", "PASSWORDS_SECRET")
		// A `generate` secret may always be regenerated: that is what the policy means.
		wantCreate(t, r, "auth", "AUTH_CODES_SECRET", SourceGenerated, ReasonGenerate)
		if len(r.Problems) != 1 {
			t.Errorf("problems: %+v", r.Problems)
		}
	})

	t.Run("since is compared with the recorded revision: equal is lost, above is new", func(t *testing.T) {
		next := *s
		next.Revision = ptr(3)
		next.Secrets = append(append([]Spec{}, s.Secrets...),
			Spec{Name: "SINCE_TWO", Services: []string{"auth"}, OnMissing: GenerateOnFirstInstallOnly, Since: ptr(2)},
			Spec{Name: "SINCE_THREE", Services: []string{"auth"}, OnMissing: GenerateOnFirstInstallOnly, Since: ptr(3)},
		)
		r := Plan(&next, candidatesFor(&next), storedFor(&next, "SINCE_TWO", "SINCE_THREE"), &Previous{Target: "vps", Revision: 2}, Options{Target: "vps"})
		if got := problemNames(r, ProblemLost); !reflect.DeepEqual(got, []string{"SINCE_TWO"}) {
			t.Errorf("lost: %v (a secret of revision 2 on a deployment of revision 2 existed)", got)
		}
		wantCreate(t, r, "auth", "SINCE_THREE", SourceGenerated, ReasonNewInRevision)
	})

	t.Run("a recorded revision of 0 reads as 1", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s, "PASSWORDS_SECRET"), &Previous{Target: "vps"}, Options{Target: "vps"})
		if r.DeployedRevision != 1 || len(problemsOf(r, ProblemLost)) != 1 {
			t.Errorf("%+v", r)
		}
	})
}

func TestPlanAlarmWhenARecordExistsButTheStoreIsEmpty(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")
	r := Plan(s, candidatesFor(s), Values{}, vps1, Options{Target: "vps"})
	if r.OK() || r.State != StateAlarm {
		t.Fatalf("%+v", r)
	}
	if got := problemNames(r, ProblemVaultEmpty); !reflect.DeepEqual(got, sortedCopy(vpsFirstInstallOnly)) {
		t.Errorf("every first-install-only secret must stop the plan: %v", got)
	}
	if len(problemsOf(r, ProblemLost)) != 0 {
		t.Errorf("alarm is its own problem, not `lost`: %+v", r.Problems)
	}
	for _, name := range vpsFirstInstallOnly {
		wantNoAction(t, r, "auth", name)
	}
}

func TestPlanUnknownWhenThereIsNoRecordButTheStoreIsNotEmpty(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")
	// Only one ordinary secret is stored: enough to make the store not empty.
	existing := Values{"auth": {"AUTH_CODES_SECRET": "stored"}}
	r := Plan(s, candidatesFor(s), existing, nil, Options{Target: "vps"})
	if r.OK() || r.State != StateUnknown {
		t.Fatalf("%+v", r)
	}
	if got := problemNames(r, ProblemUnknownState); !reflect.DeepEqual(got, sortedCopy(vpsFirstInstallOnly)) {
		t.Errorf("unknown-state: %v", got)
	}
	wantCreate(t, r, "auth", "SESSIONS_SECRET", SourceGenerated, ReasonGenerate) // plain `generate` is still fine
}

func TestPlanIgnoresARecordOfAnotherTarget(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")
	t.Run("empty store: still a first install", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), Values{}, &Previous{Target: "local", Revision: 1}, Options{Target: "vps"})
		if r.State != StateFirstInstall || !r.OK() || r.DeployedRevision != 0 {
			t.Errorf("%+v", r)
		}
	})
	t.Run("store with secrets: unknown, not an upgrade", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s, "PASSWORDS_SECRET"), &Previous{Target: "local", Revision: 1}, Options{Target: "vps"})
		if r.State != StateUnknown || len(problemsOf(r, ProblemUnknownState)) != 1 {
			t.Errorf("%+v", r)
		}
	})
}

// ---- groups ---------------------------------------------------------------

func TestPlanGroupsAreAllOrNothing(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")

	t.Run("jwt group stored in part", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s, "JWKS_JSON"), vps1, Options{Target: "vps"})
		got := problemsOf(r, ProblemGroupPartial)
		if len(got) != 1 || got[0].Group != "jwt" ||
			!reflect.DeepEqual(got[0].Present, []string{"auth/JWT_PRIVATE_KEY"}) ||
			!reflect.DeepEqual(got[0].Missing, []string{"central/JWKS_JSON"}) {
			t.Fatalf("%+v", r.Problems)
		}
		// Not completed, not reported a second time as lost.
		wantNoAction(t, r, "central", "JWKS_JSON")
		wantNoAction(t, r, "auth", "JWT_PRIVATE_KEY")
		if len(problemsOf(r, ProblemLost)) != 0 {
			t.Errorf("the member of a partial group is not also `lost`: %+v", r.Problems)
		}
	})
	t.Run("edge-key group stored in part, across three entries", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s, "EDGE_PUBLIC_JWK", "EDGE_KEY_ID"), vps1, Options{Target: "vps"})
		got := problemsOf(r, ProblemGroupPartial)
		if len(got) != 1 || got[0].Group != "edge-key" ||
			!reflect.DeepEqual(got[0].Present, []string{"edge/EDGE_PRIVATE_KEY"}) ||
			!reflect.DeepEqual(got[0].Missing, []string{"central/EDGE_PUBLIC_JWK", "edge/EDGE_KEY_ID"}) {
			t.Fatalf("%+v", r.Problems)
		}
	})
	t.Run("a group missing entirely on a first install is generated whole", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), Values{}, nil, Options{Target: "vps"})
		for _, name := range []string{"JWT_PRIVATE_KEY", "JWKS_JSON", "EDGE_PRIVATE_KEY", "EDGE_KEY_ID", "EDGE_PUBLIC_JWK"} {
			found := false
			for _, a := range r.Actions {
				found = found || (a.Name == name && a.Kind == ActionCreate)
			}
			if !found {
				t.Errorf("%s not created", name)
			}
		}
	})
	t.Run("a whole group stored is kept", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s), vps1, Options{Target: "vps"})
		if !r.OK() {
			t.Fatalf("%+v", r.Problems)
		}
		if _, ok := find(r, ActionKeep, "central", "JWKS_JSON"); !ok {
			t.Error("JWKS_JSON not kept")
		}
	})
	t.Run("on local it is an error too: a mismatched key pair is wrong anywhere", func(t *testing.T) {
		local := mustLoad(t, localFixture, "local")
		r := Plan(local, candidatesFor(local), storedFor(local, "JWKS_JSON"), nil, Options{Target: "local"})
		if len(problemsOf(r, ProblemGroupPartial)) != 1 || r.OK() {
			t.Fatalf("%+v", r)
		}
		wantNoAction(t, r, "central", "JWKS_JSON")
	})
	t.Run("confirming one member does not complete the group", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s, "JWKS_JSON"), vps1, Options{Target: "vps", GenerateMissing: map[string]bool{"JWKS_JSON": true}})
		if len(problemsOf(r, ProblemGroupPartial)) != 1 || r.OK() {
			t.Errorf("%+v", r.Problems)
		}
		wantNoAction(t, r, "central", "JWKS_JSON")
	})
}

// ---- values shared by several services ------------------------------------

func TestPlanSharedSecrets(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")

	t.Run("different stored copies are an error naming the key and the services", func(t *testing.T) {
		existing := storedFor(s)
		existing["central"]["CLIENT_SECRETS_SECRET"] = "a-different-value"
		r := Plan(s, candidatesFor(s), existing, vps1, Options{Target: "vps"})
		got := problemsOf(r, ProblemSharedConflict)
		if len(got) != 1 || got[0].Name != "CLIENT_SECRETS_SECRET" || !reflect.DeepEqual(got[0].Services, []string{"auth", "central"}) {
			t.Fatalf("%+v", r.Problems)
		}
		wantNoAction(t, r, "auth", "CLIENT_SECRETS_SECRET")
	})
	t.Run("the vps Postgres password is one shared value: a stored disagreement is an error", func(t *testing.T) {
		existing := storedFor(s)
		existing["edge"]["POSTGRES_PASSWORD"] = "other"
		r := Plan(s, candidatesFor(s), existing, vps1, Options{Target: "vps"})
		got := problemsOf(r, ProblemSharedConflict)
		if len(got) != 1 || got[0].Name != "POSTGRES_PASSWORD" || len(got[0].Services) != 3 {
			t.Fatalf("%+v", r.Problems)
		}
	})
	t.Run("a copy missing where another is stored takes the stored value", func(t *testing.T) {
		existing := storedFor(s)
		delete(existing["central"], "CLIENT_SECRETS_SECRET")
		r := Plan(s, candidatesFor(s), existing, vps1, Options{Target: "vps"})
		if !r.OK() {
			t.Fatalf("%+v", r.Problems)
		}
		wantCreate(t, r, "central", "CLIENT_SECRETS_SECRET", SourceStored, ReasonFillShared)
		if _, ok := find(r, ActionKeep, "auth", "CLIENT_SECRETS_SECRET"); !ok {
			t.Error("the stored copy is kept")
		}
	})
	t.Run("a stored copy wins over a differing candidate", func(t *testing.T) {
		cands := candidatesFor(s)
		cands["auth"]["CLIENT_SECRETS_SECRET"] = "a-different-candidate" // never looked at: something is stored
		r := Plan(s, cands, storedFor(s), vps1, Options{Target: "vps"})
		if !r.OK() {
			t.Errorf("%+v", r.Problems)
		}
	})
	t.Run("the same name in separate entries is separate values (keyed by name and service)", func(t *testing.T) {
		// The k8s shape: one POSTGRES_PASSWORD entry per service.
		k := &Schema{SchemaVersion: 1, Target: "vps", Secrets: []Spec{
			{Name: "POSTGRES_PASSWORD", Services: []string{"auth"}, OnMissing: GenerateOnFirstInstallOnly},
			{Name: "POSTGRES_PASSWORD", Services: []string{"central"}, OnMissing: GenerateOnFirstInstallOnly},
		}}
		existing := Values{"auth": {"POSTGRES_PASSWORD": "one"}, "central": {"POSTGRES_PASSWORD": "two"}}
		r := Plan(k, Values{}, existing, vps1, Options{Target: "vps"})
		if !r.OK() || count(r, ActionKeep) != 2 {
			t.Errorf("independent values must not be compared with each other: %+v", r)
		}
		// And one missing does not get the other's value.
		r = Plan(k, Values{"central": {"POSTGRES_PASSWORD": "new"}}, Values{"auth": {"POSTGRES_PASSWORD": "one"}}, vps1, Options{Target: "vps"})
		if len(problemsOf(r, ProblemLost)) != 1 {
			t.Errorf("central's own password is lost, auth's is no substitute: %+v", r)
		}
	})
	t.Run("candidates that disagree for a shared secret are reported", func(t *testing.T) {
		cands := candidatesFor(s)
		cands["central"]["CLIENT_SECRETS_SECRET"] = "different"
		r := Plan(s, cands, Values{}, nil, Options{Target: "vps"})
		if got := problemNames(r, ProblemCandidateConflict); !reflect.DeepEqual(got, []string{"CLIENT_SECRETS_SECRET"}) {
			t.Errorf("%v", got)
		}
	})
}

// ---- the utils group is outside the engine ---------------------------------

func TestPlanLeavesTheUtilsGroupAlone(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")
	const public, private = "UTILITY_CLIENT_PUBLIC_JWK", "UTILS_PRIVATE_KEY_JWK"

	check := func(t *testing.T, r Result) {
		t.Helper()
		if _, ok := find(r, ActionSkipUtils, "central", public); !ok {
			t.Errorf("%s not skipped", public)
		}
		if _, ok := find(r, ActionSkipUtils, "utils", private); !ok {
			t.Errorf("%s not skipped", private)
		}
		for _, a := range r.Actions {
			if (a.Name == public || a.Name == private) && a.Kind != ActionSkipUtils {
				t.Errorf("%s/%s got %s", a.Service, a.Name, a.Kind)
			}
		}
		for _, p := range r.Problems {
			if p.Name == public || p.Name == private || p.Group == "utils" {
				t.Errorf("a problem about the utils group: %+v", p)
			}
		}
	}

	t.Run("first install", func(t *testing.T) {
		check(t, Plan(s, candidatesFor(s), Values{}, nil, Options{Target: "vps"}))
	})
	t.Run("upgrade with neither half stored", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s), vps1, Options{Target: "vps"})
		if !r.OK() {
			t.Fatalf("%+v", r.Problems)
		}
		check(t, r)
	})
	t.Run("only the public half stored: not a partial group", func(t *testing.T) {
		existing := storedFor(s)
		existing["central"][public] = "stored"
		r := Plan(s, candidatesFor(s), existing, vps1, Options{Target: "vps"})
		if !r.OK() {
			t.Fatalf("%+v", r.Problems)
		}
		check(t, r)
	})
	t.Run("an alarm", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), Values{}, vps1, Options{Target: "vps"})
		check(t, r)
	})
	t.Run("on local", func(t *testing.T) {
		local := mustLoad(t, localFixture, "local")
		r := Plan(local, candidatesFor(local), Values{}, nil, Options{Target: "local"})
		check(t, r)
	})
	t.Run("no candidate is needed for it", func(t *testing.T) {
		cands := candidatesFor(s)
		delete(cands["central"], public)
		r := Plan(s, cands, Values{}, nil, Options{Target: "vps"})
		if !r.OK() {
			t.Errorf("%+v", r.Problems)
		}
	})
	t.Run("it is found by its file, not by its name", func(t *testing.T) {
		renamed := &Schema{SchemaVersion: 1, Target: "vps", Secrets: []Spec{
			{Name: "SOME_OTHER_NAME", Services: []string{"utils"}, Group: ptr("keys"), File: ptr(UtilsKeyFile), OnMissing: GenerateOnFirstInstallOnly},
			{Name: "SOME_PUBLIC_HALF", Services: []string{"central"}, Group: ptr("keys"), OnMissing: GenerateOnFirstInstallOnly},
		}}
		r := Plan(renamed, Values{}, Values{"auth": {"X": "y"}}, vps1, Options{Target: "vps"})
		if !r.OK() || count(r, ActionSkipUtils) != 2 {
			t.Errorf("%+v", r)
		}
	})
	t.Run("a secret merely NAMED like the utils key is not exempt", func(t *testing.T) {
		impostor := &Schema{SchemaVersion: 1, Target: "vps", Secrets: []Spec{
			{Name: private, Services: []string{"auth"}, OnMissing: GenerateOnFirstInstallOnly},
		}}
		r := Plan(impostor, Values{}, Values{"auth": {"X": "y"}}, vps1, Options{Target: "vps"})
		if len(problemsOf(r, ProblemLost)) != 1 || count(r, ActionSkipUtils) != 0 {
			t.Errorf("%+v", r)
		}
	})
}

// ---- local is a throwaway ---------------------------------------------------

func TestPlanLocalHandlesFirstInstallOnlyAsGenerate(t *testing.T) {
	local := mustLoad(t, localFixture, "local")

	t.Run("nothing stored", func(t *testing.T) {
		r := Plan(local, candidatesFor(local), Values{}, nil, Options{Target: "local"})
		if !r.OK() || r.State != StateLocal {
			t.Fatalf("%+v", r)
		}
		wantCreate(t, r, "auth", "PASSWORDS_SECRET", SourceGenerated, ReasonLocal)
		wantCreate(t, r, "auth", "AUTH_CODES_SECRET", SourceGenerated, ReasonGenerate)
	})
	t.Run("a deployment record and an empty store is not an alarm", func(t *testing.T) {
		r := Plan(local, candidatesFor(local), Values{}, &Previous{Target: "local", Revision: 1}, Options{Target: "local"})
		if !r.OK() || r.State != StateLocal {
			t.Errorf("%+v", r)
		}
	})
	t.Run("a secret missing from a non-empty store is generated, not lost or unknown", func(t *testing.T) {
		for _, prev := range []*Previous{nil, {Target: "local", Revision: 1}} {
			r := Plan(local, candidatesFor(local), storedFor(local, "PASSWORDS_SECRET"), prev, Options{Target: "local"})
			if !r.OK() {
				t.Fatalf("prev %+v: %+v", prev, r.Problems)
			}
			wantCreate(t, r, "auth", "PASSWORDS_SECRET", SourceGenerated, ReasonLocal)
		}
	})
	t.Run("the same input on vps stops", func(t *testing.T) {
		s := mustLoad(t, vpsFixture, "vps")
		r := Plan(s, candidatesFor(s), storedFor(s, "PASSWORDS_SECRET"), nil, Options{Target: "vps"})
		if r.OK() {
			t.Error("vps must not generate PASSWORDS_SECRET into a store that has other secrets and no record")
		}
	})
}

// ---- the operator's confirmation ---------------------------------------------

func TestPlanGenerateMissing(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")
	confirm := func(names ...string) Options {
		o := Options{Target: "vps", GenerateMissing: map[string]bool{}}
		for _, n := range names {
			o.GenerateMissing[n] = true
		}
		return o
	}

	t.Run("lost, confirmed", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s, "PASSWORDS_SECRET"), vps1, confirm("PASSWORDS_SECRET"))
		if !r.OK() {
			t.Fatalf("%+v", r.Problems)
		}
		wantCreate(t, r, "auth", "PASSWORDS_SECRET", SourceGenerated, ReasonForcedByFlag)
	})
	t.Run("unknown state, confirmed", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s, "PASSWORDS_SECRET"), nil, confirm("PASSWORDS_SECRET"))
		wantCreate(t, r, "auth", "PASSWORDS_SECRET", SourceGenerated, ReasonForcedByFlag)
		if len(problemsOf(r, ProblemUnknownState)) != 0 {
			t.Errorf("%+v", r.Problems)
		}
	})
	t.Run("alarm: only what was confirmed", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), Values{}, vps1, confirm("PASSWORDS_SECRET"))
		wantCreate(t, r, "auth", "PASSWORDS_SECRET", SourceGenerated, ReasonForcedByFlag)
		if len(problemsOf(r, ProblemVaultEmpty)) != len(vpsFirstInstallOnly)-1 {
			t.Errorf("%+v", r.Problems)
		}
	})
	t.Run("only the named secret is affected", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s, "PASSWORDS_SECRET", "REFRESH_TOKENS_SECRET"), vps1, confirm("PASSWORDS_SECRET"))
		if got := problemNames(r, ProblemLost); !reflect.DeepEqual(got, []string{"REFRESH_TOKENS_SECRET"}) {
			t.Errorf("%v", got)
		}
	})
	t.Run("a name the schema does not know is reported", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s), vps1, confirm("PASWORDS_SECRET"))
		if got := problemNames(r, ProblemUnknownSecret); !reflect.DeepEqual(got, []string{"PASWORDS_SECRET"}) {
			t.Errorf("%v", got)
		}
	})
	t.Run("a confirmation for a secret that is stored changes nothing", func(t *testing.T) {
		r := Plan(s, candidatesFor(s), storedFor(s), vps1, confirm("PASSWORDS_SECRET"))
		if !r.OK() || count(r, ActionCreate) != 0 {
			t.Errorf("%+v", r)
		}
	})
	t.Run("it still needs a candidate", func(t *testing.T) {
		cands := candidatesFor(s)
		delete(cands["auth"], "PASSWORDS_SECRET")
		r := Plan(s, cands, storedFor(s, "PASSWORDS_SECRET"), vps1, confirm("PASSWORDS_SECRET"))
		if len(problemsOf(r, ProblemNoCandidate)) != 1 {
			t.Errorf("%+v", r.Problems)
		}
	})
}

// ---- external, candidates, orphans --------------------------------------------

func TestPlanExternalSecrets(t *testing.T) {
	ext := &Schema{SchemaVersion: 1, Target: "vps", Secrets: []Spec{
		{Name: "OPERATOR_SECRET", Services: []string{"auth"}, OnMissing: External},
	}}
	t.Run("missing is an error in every state, with or without a candidate", func(t *testing.T) {
		for _, prev := range []*Previous{nil, vps1} {
			r := Plan(ext, Values{"auth": {"OPERATOR_SECRET": "cand"}}, Values{}, prev, Options{Target: "vps"})
			if len(problemsOf(r, ProblemExternalMissing)) != 1 || count(r, ActionCreate) != 0 {
				t.Errorf("prev %+v: %+v", prev, r)
			}
		}
		r := Plan(ext, Values{}, Values{}, nil, Options{Target: "vps", GenerateMissing: map[string]bool{"OPERATOR_SECRET": true}})
		if len(problemsOf(r, ProblemExternalMissing)) != 1 {
			t.Error("the confirmation does not make an operator's secret generatable")
		}
	})
	t.Run("stored is kept", func(t *testing.T) {
		r := Plan(ext, Values{}, Values{"auth": {"OPERATOR_SECRET": "x"}}, vps1, Options{Target: "vps"})
		if !r.OK() || count(r, ActionKeep) != 1 {
			t.Errorf("%+v", r)
		}
	})
}

func TestPlanNoCandidate(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")
	cands := candidatesFor(s)
	delete(cands["auth"], "PASSWORDS_SECRET")
	delete(cands["auth"], "AUTH_CODES_SECRET")
	r := Plan(s, cands, Values{}, nil, Options{Target: "vps"})
	if got := problemNames(r, ProblemNoCandidate); !reflect.DeepEqual(got, []string{"AUTH_CODES_SECRET", "PASSWORDS_SECRET"}) {
		t.Errorf("%v", got)
	}
}

func TestPlanOrphans(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")
	existing := storedFor(s)
	existing["auth"]["OLD_KEY"] = "x"
	existing["central"][UtilsKeyFile] = "x"
	// The utils path is not weighed at all.
	existing["utils"] = map[string]string{"UTILITY_CLIENT_PRIVATE_JWK": "x"}
	existing["central"]["UTILITY_CLIENT_PUBLIC_JWK"] = "x" // in the schema: not an orphan
	r := Plan(s, candidatesFor(s), existing, vps1, Options{Target: "vps"})
	if !r.OK() || count(r, ActionOrphan) != 2 {
		t.Fatalf("%+v", r)
	}
	if _, ok := find(r, ActionOrphan, "auth", "OLD_KEY"); !ok {
		t.Error("OLD_KEY not reported")
	}
	if _, ok := find(r, ActionOrphan, "central", "UTILITY_CLIENT_PUBLIC_JWK"); ok {
		t.Error("a name the schema lists is not an orphan")
	}
	// A key under a service that is not that service's own: the schema says central, not edge.
	existing2 := storedFor(s)
	existing2["edge"]["PASSWORDS_SECRET"] = "x"
	if _, ok := find(Plan(s, candidatesFor(s), existing2, vps1, Options{Target: "vps"}), ActionOrphan, "edge", "PASSWORDS_SECRET"); !ok {
		t.Error("a known name under the wrong service is an orphan")
	}
}

// ---- properties -----------------------------------------------------------------

func TestPlanIsDeterministicAndDoesNotTouchItsInputs(t *testing.T) {
	s := mustLoad(t, vpsFixture, "vps")
	cands := candidatesFor(s)
	existing := storedFor(s, "PASSWORDS_SECRET", "JWKS_JSON")
	existing["auth"]["OLD_KEY"] = "x"
	cBefore, eBefore := deepCopy(cands), deepCopy(existing)
	opts := Options{Target: "vps", GenerateMissing: map[string]bool{"PASWORDS": true}}

	first := Plan(s, cands, existing, vps1, opts)
	for i := 0; i < 20; i++ { // map iteration order must not leak into the result
		if again := Plan(s, cands, existing, vps1, opts); !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs:\n%+v\n%+v", i, first, again)
		}
	}
	if !reflect.DeepEqual(cands, cBefore) || !reflect.DeepEqual(existing, eBefore) {
		t.Error("Plan modified its inputs")
	}
	if !sort.SliceIsSorted(first.Actions, func(i, j int) bool {
		a, b := first.Actions[i], first.Actions[j]
		return a.Service < b.Service || (a.Service == b.Service && a.Name < b.Name)
	}) {
		t.Error("actions are not sorted")
	}
}

func TestPlanEveryCreateOnTheRealSchemasHasASourceAndAReason(t *testing.T) {
	// Whatever the state, a create says where its value comes from and why.
	for _, c := range []struct{ file, target string }{{vpsFixture, "vps"}, {localFixture, "local"}} {
		s := mustLoad(t, c.file, c.target)
		for _, prev := range []*Previous{nil, {Target: c.target, Revision: 1}} {
			for _, existing := range []Values{{}, storedFor(s), storedFor(s, "PASSWORDS_SECRET")} {
				r := Plan(s, candidatesFor(s), existing, prev, Options{Target: c.target})
				for _, a := range r.Actions {
					if a.Kind == ActionCreate && (a.Source == "" || a.Reason == "") {
						t.Errorf("%s: create without a source or reason: %+v", c.target, a)
					}
				}
			}
		}
	}
}

func deepCopy(v Values) Values {
	out := Values{}
	for s, m := range v {
		out[s] = map[string]string{}
		for k, x := range m {
			out[s][k] = x
		}
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// Review findings, pinned.

func TestPlanDoesNotGuessAnUnknownPolicy(t *testing.T) {
	// Load refuses such a schema; Plan, handed an unchecked one, must not turn it into a generate.
	bogus := &Schema{SchemaVersion: 1, Target: "vps", Secrets: []Spec{{Name: "X", Services: []string{"auth"}, OnMissing: "bogus"}}}
	for name, prev := range map[string]*Previous{"first install": nil, "upgrade": vps1} {
		existing := Values{"auth": {"OTHER": "1"}}
		if prev == nil {
			existing = Values{}
		}
		r := Plan(bogus, Values{"auth": {"X": "cand"}}, existing, prev, Options{Target: "vps"})
		if len(problemsOf(r, ProblemInvalidSchema)) != 1 || count(r, ActionCreate) != 0 {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

func TestPlanForcingOneMemberOfAGroupDoesNotHalfGenerateIt(t *testing.T) {
	g := &Schema{SchemaVersion: 1, Target: "vps", Secrets: []Spec{
		{Name: "A", Services: []string{"auth"}, Group: ptr("pair"), OnMissing: GenerateOnFirstInstallOnly},
		{Name: "B", Services: []string{"central"}, Group: ptr("pair"), OnMissing: GenerateOnFirstInstallOnly},
	}}
	cands := Values{"auth": {"A": "a"}, "central": {"B": "b"}}
	existing := Values{"edge": {"OTHER": "1"}}

	r := Plan(g, cands, existing, vps1, Options{Target: "vps", GenerateMissing: map[string]bool{"A": true}})
	if count(r, ActionCreate) != 0 || len(problemsOf(r, ProblemLost)) != 2 {
		t.Errorf("one confirmed member must not be created alone: %+v", r)
	}
	r = Plan(g, cands, existing, vps1, Options{Target: "vps", GenerateMissing: map[string]bool{"A": true, "B": true}})
	if !r.OK() || count(r, ActionCreate) != 2 {
		t.Errorf("the whole group confirmed: %+v", r)
	}
}

func TestPlanResultDoesNotAliasTheSchema(t *testing.T) {
	s := &Schema{SchemaVersion: 1, Target: "vps", Secrets: []Spec{
		{Name: "SHARED", Services: []string{"auth", "central"}, OnMissing: GenerateOnFirstInstallOnly},
	}}
	r := Plan(s, Values{}, Values{"edge": {"OTHER": "1"}}, vps1, Options{Target: "vps"})
	if len(r.Problems) != 1 || len(r.Problems[0].Services) != 2 {
		t.Fatalf("%+v", r)
	}
	r.Problems[0].Services[0] = "MUTATED"
	if s.Secrets[0].Services[0] != "auth" {
		t.Error("a Problem shares its Services slice with the schema")
	}
}

func TestPlanGroupPartialLabelsSayWhereTheCopiesAre(t *testing.T) {
	g := &Schema{SchemaVersion: 1, Target: "vps", Secrets: []Spec{
		{Name: "SHARED", Services: []string{"auth", "central"}, Group: ptr("pair"), OnMissing: GenerateOnFirstInstallOnly},
		{Name: "OTHER", Services: []string{"edge"}, Group: ptr("pair"), OnMissing: GenerateOnFirstInstallOnly},
	}}
	r := Plan(g, Values{}, Values{"auth": {"SHARED": "x"}}, vps1, Options{Target: "vps"})
	got := problemsOf(r, ProblemGroupPartial)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Present, []string{"auth/SHARED"}) ||
		!reflect.DeepEqual(got[0].Missing, []string{"central/SHARED", "edge/OTHER"}) {
		t.Errorf("%+v", r.Problems)
	}
}
