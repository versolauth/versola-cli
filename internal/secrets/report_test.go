package secrets

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// marker is planted in EVERY value Plan is given -- stored and candidate alike --
// and must then be found nowhere in what is printed. Values that were equal stay
// equal and different ones stay different, so the scenarios still mean what they say.
const marker = "S3CR3T-MARKER-9f2b7c"

func withMarker(v Values) Values {
	out := Values{}
	for service, m := range v {
		out[service] = map[string]string{}
		for name, value := range m {
			out[service][name] = marker + "/" + value
		}
	}
	return out
}

// scenarios exercise every kind of action and problem the report can show.
func scenarios(t *testing.T) map[string]Result {
	t.Helper()
	s := mustLoad(t, vpsFixture, "vps")
	local := mustLoad(t, localFixture, "local")
	cands := withMarker(candidatesFor(s))
	conflict := storedFor(s)
	conflict["central"]["CLIENT_SECRETS_SECRET"] = "different"
	orphans := storedFor(s)
	orphans["auth"]["OLD_KEY"] = "x"
	orphans["edge"]["EDGE\x1b[31mINJECT"] = "x" // an unprintable name from the store
	delete(orphans["central"], "CLIENT_SECRETS_SECRET")
	badCands := withMarker(candidatesFor(s))
	badCands["central"]["CLIENT_SECRETS_SECRET"] = marker + "/different"
	delete(badCands["auth"], "AUTH_CODES_SECRET")
	ext := &Schema{SchemaVersion: 1, Target: "vps", Secrets: []Spec{{Name: "OPERATOR_SECRET", Services: []string{"auth"}, OnMissing: External}}}

	run := func(sc *Schema, c, e Values, prev *Previous, o Options) Result {
		return Plan(sc, c, withMarker(e), prev, o)
	}
	return map[string]Result{
		"first install":    run(s, cands, Values{}, nil, Options{Target: "vps"}),
		"upgrade":          run(s, cands, storedFor(s), vps1, Options{Target: "vps"}),
		"lost":             run(s, cands, storedFor(s, "PASSWORDS_SECRET"), vps1, Options{Target: "vps"}),
		"forced":           run(s, cands, storedFor(s, "PASSWORDS_SECRET"), vps1, Options{Target: "vps", GenerateMissing: map[string]bool{"PASSWORDS_SECRET": true, "NOPE": true}}),
		"alarm":            run(s, cands, Values{}, vps1, Options{Target: "vps"}),
		"unknown":          run(s, cands, Values{"auth": {"AUTH_CODES_SECRET": "x"}}, nil, Options{Target: "vps"}),
		"group partial":    run(s, cands, storedFor(s, "JWKS_JSON"), vps1, Options{Target: "vps"}),
		"shared conflict":  run(s, cands, conflict, vps1, Options{Target: "vps"}),
		"fill and orphans": run(s, cands, orphans, vps1, Options{Target: "vps"}),
		"candidates":       run(s, badCands, Values{}, nil, Options{Target: "vps"}),
		"external":         run(ext, cands, Values{}, nil, Options{Target: "vps"}),
		"local":            run(local, withMarker(candidatesFor(local)), Values{}, nil, Options{Target: "local"}),
	}
}

func TestNoSecretValueReachesAnyOutput(t *testing.T) {
	for name, r := range scenarios(t) {
		t.Run(name, func(t *testing.T) {
			var text, js bytes.Buffer
			if err := RenderText(&text, "0.6.3", r); err != nil {
				t.Fatal(err)
			}
			if err := RenderJSON(&js, "0.6.3", r); err != nil {
				t.Fatal(err)
			}
			for format, out := range map[string]string{"text": text.String(), "json": js.String()} {
				if strings.Contains(out, marker) || strings.Contains(strings.ToLower(out), "s3cr3t") {
					t.Errorf("%s output carries a secret value:\n%s", format, out)
				}
			}
		})
	}
}

// The strongest guard is structural: the types the output is built from have no
// field that could hold a value. Adding one makes this fail and forces the
// question to be asked in review.
func TestReportTypesHaveNoRoomForAValue(t *testing.T) {
	fields := func(v any) []string {
		var out []string
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			out = append(out, typ.Field(i).Name)
		}
		sort.Strings(out)
		return out
	}
	for _, c := range []struct {
		v    any
		want []string
	}{
		{Action{}, []string{"Kind", "Name", "Reason", "Service", "Source"}},
		{Problem{}, []string{"Group", "Kind", "Missing", "Name", "Present", "Service", "Services"}},
		{Result{}, []string{"Actions", "DeployedRevision", "Problems", "SchemaRevision", "State", "Target"}},
		{jsonReport{}, []string{"OK", "Result", "Version"}},
	} {
		if got := fields(c.v); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%T fields changed: %v\nIf the new field cannot hold a secret value, add it here; if it can, it does not belong in a report.", c.v, got)
		}
	}
	// And every string-ish field of those is a name or a fixed word, never free text from the inputs:
	// the only free-form strings are the Problem/Action names, which come from the schema or from key names.
}

func TestRenderJSON(t *testing.T) {
	r := scenarios(t)["lost"]
	var buf bytes.Buffer
	if err := RenderJSON(&buf, "0.6.3", r); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version          string `json:"version"`
		OK               bool   `json:"ok"`
		Target           string `json:"target"`
		State            string `json:"state"`
		SchemaRevision   int    `json:"schemaRevision"`
		DeployedRevision int    `json:"deployedRevision"`
		Actions          []struct {
			Service, Name string
			Action        string `json:"action"`
			Source        string `json:"source"`
			Reason        string `json:"reason"`
		} `json:"actions"`
		Problems []struct {
			Kind    string `json:"kind"`
			Name    string `json:"name"`
			Service string `json:"service"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if doc.Version != "0.6.3" || doc.OK || doc.Target != "vps" || doc.State != "upgrade" || doc.SchemaRevision != 1 || doc.DeployedRevision != 1 {
		t.Errorf("%+v", doc)
	}
	if len(doc.Problems) != 1 || doc.Problems[0].Kind != "lost" || doc.Problems[0].Name != "PASSWORDS_SECRET" {
		t.Errorf("problems: %+v", doc.Problems)
	}
	found := false
	for _, a := range doc.Actions {
		if a.Service == "auth" && a.Name == "AUTH_CODES_SECRET" {
			found = true
		}
		if a.Action == "" || a.Service == "" || a.Name == "" {
			t.Errorf("incomplete action: %+v", a)
		}
	}
	if !found {
		// AUTH_CODES_SECRET is stored in this scenario, so it is "keep".
		t.Error("actions lack the kept secrets")
	}

	t.Run("empty lists are [] and not null", func(t *testing.T) {
		var buf bytes.Buffer
		if err := RenderJSON(&buf, "1", Result{Target: "vps", State: StateFirstInstall, SchemaRevision: 1}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), `"actions": []`) || !strings.Contains(buf.String(), `"problems": []`) || !strings.Contains(buf.String(), `"ok": true`) {
			t.Errorf("%s", buf.String())
		}
	})
}

func TestRenderText(t *testing.T) {
	scs := scenarios(t)

	t.Run("a good plan", func(t *testing.T) {
		var buf bytes.Buffer
		if err := RenderText(&buf, "0.6.3", scs["upgrade"]); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, want := range []string{"Secrets plan: vps, Versola 0.6.3", "deployed revision 1", "PASSWORDS_SECRET", "keep", "skip", "Result: OK"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in\n%s", want, out)
			}
		}
	})
	t.Run("a stopped plan says why and that nothing is applied", func(t *testing.T) {
		var buf bytes.Buffer
		if err := RenderText(&buf, "0.6.3", scs["lost"]); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, want := range []string{"Problems (1)", "lost:", "PASSWORDS_SECRET", "backup", "NOT OK"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in\n%s", want, out)
			}
		}
	})
	t.Run("local's partial-group hint points at uninstall, vps's at a backup", func(t *testing.T) {
		local := mustLoad(t, localFixture, "local")
		lr := Plan(local, candidatesFor(local), storedFor(local, "JWKS_JSON"), nil, Options{Target: "local"})
		var lb, vb bytes.Buffer
		if err := RenderText(&lb, "1", lr); err != nil {
			t.Fatal(err)
		}
		if err := RenderText(&vb, "1", scs["group partial"]); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(lb.String(), "versola uninstall") || strings.Contains(vb.String(), "versola uninstall") || !strings.Contains(vb.String(), "backup") {
			t.Errorf("local:\n%s\nvps:\n%s", lb.String(), vb.String())
		}
	})
	t.Run("an unprintable name from the store cannot reach the terminal", func(t *testing.T) {
		var buf bytes.Buffer
		if err := RenderText(&buf, "1", scs["fill and orphans"]); err != nil {
			t.Fatal(err)
		}
		if strings.ContainsRune(buf.String(), '\x1b') {
			t.Errorf("an escape character got through:\n%q", buf.String())
		}
		if !strings.Contains(buf.String(), "OLD_KEY") || !strings.Contains(buf.String(), "orphan") {
			t.Errorf("orphans are not reported:\n%s", buf.String())
		}
	})
	t.Run("invisible and bidirectional characters in a stored key name are neutralised", func(t *testing.T) {
		for _, c := range []rune{'\u202e', '\u200b', '\u2028', '\u2066', 0x07} {
			if got := clean("A" + string(c) + "B"); got != "A?B" {
				t.Errorf("U+%04X: %q", c, got)
			}
		}
		if clean("PASSWORDS_SECRET") != "PASSWORDS_SECRET" || clean("ключ") != "ключ" {
			t.Error("ordinary names must be left alone")
		}
	})
	t.Run("every problem kind has an explanation, not just its name", func(t *testing.T) {
		seen := map[ProblemKind]bool{}
		for _, r := range scs {
			for _, p := range r.Problems {
				seen[p.Kind] = true
				if msg := explain(r, p); msg == string(p.Kind) || len(msg) < 30 {
					t.Errorf("%s has no explanation: %q", p.Kind, msg)
				}
			}
		}
		for _, kind := range []ProblemKind{ProblemLost, ProblemGroupPartial, ProblemSharedConflict, ProblemUnknownState, ProblemVaultEmpty,
			ProblemExternalMissing, ProblemNoCandidate, ProblemCandidateConflict, ProblemUnknownSecret} {
			if !seen[kind] {
				t.Errorf("the scenarios never produce %s, so its text is untested", kind)
			}
		}
	})
}
