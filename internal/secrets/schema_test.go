package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures in testdata are NOT written by hand to suit the tests: they are
// what SecretSchema.toJson in the versola repository (scripts/gen-env.scala,
// after PR #544) prints for vps and docker-local, reproduced mechanically
// from its `specs` list. They are TEMPORARY: replace them with the file a
// published versola-tools writes as soon as there is one (see testdata/README.md).
const (
	vpsFixture   = "testdata/secrets.schema.vps.json"
	localFixture = "testdata/secrets.schema.docker-local.json"
)

func mustLoad(t *testing.T, path, target string) *Schema {
	t.Helper()
	s, err := Load(path, target)
	if err != nil {
		t.Fatalf("Load(%s, %s): %v", path, target, err)
	}
	return s
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// minimal is a valid one-entry vps schema around the given entries.
func minimal(entries string) string {
	return `{"schemaVersion":1,"revision":1,"target":"vps","secrets":[` + entries + `]}`
}

const okEntry = `{"name":"A_SECRET","services":["auth"],"type":"base64url","size":32,"group":null,"onMissing":"generate","since":1,"file":null}`

func TestLoadTellsMissingFromBroken(t *testing.T) {
	t.Run("no file is ErrNoSchema and not a ParseError", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), FileName), "vps")
		var pe *ParseError
		if !errors.Is(err, ErrNoSchema) || errors.As(err, &pe) {
			t.Fatalf("got %v", err)
		}
	})
	for name, content := range map[string]string{
		"not JSON":              "this is not json",
		"empty file":            "",
		"JSON null":             "null",
		"truncated":             `{"schemaVersion":1,"revision":1,"target":"vps","secrets":[`,
		"a JSON array":          "[]",
		"no secrets":            minimal(""),
		"wrong type of secrets": `{"schemaVersion":1,"target":"vps","secrets":"x"}`,
	} {
		t.Run(name+" is a ParseError and not ErrNoSchema", func(t *testing.T) {
			_, err := Load(writeTemp(t, content), "vps")
			var pe *ParseError
			if !errors.As(err, &pe) || errors.Is(err, ErrNoSchema) {
				t.Fatalf("got %v", err)
			}
			if pe.Path == "" {
				t.Errorf("the error should say which file")
			}
		})
	}
}

func TestParseReadsOptionalFieldsAsOne(t *testing.T) {
	s, err := Parse([]byte(`{"schemaVersion":1,"target":"vps","secrets":[` +
		`{"name":"A_SECRET","services":["auth"],"type":"opaque","onMissing":"generate"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.RevisionNumber() != 1 || s.Secrets[0].SinceRevision() != 1 {
		t.Errorf("an absent revision/since reads as 1, got %d/%d", s.RevisionNumber(), s.Secrets[0].SinceRevision())
	}
	if err := s.Validate("vps"); err != nil {
		t.Errorf("a file without revision/since is valid: %v", err)
	}
}

func TestParseIgnoresUnknownFieldsButNotNewerFormats(t *testing.T) {
	t.Run("unknown fields at the top and in entries are ignored", func(t *testing.T) {
		s, err := Parse([]byte(`{"schemaVersion":1,"revision":3,"target":"vps","generatedBy":"later","secrets":[` +
			`{"name":"A_SECRET","services":["auth"],"type":"opaque","onMissing":"generate","since":2,"rotation":"yearly"}]}`))
		if err != nil || s.RevisionNumber() != 3 || s.Secrets[0].SinceRevision() != 2 {
			t.Fatalf("got %+v, %v", s, err)
		}
	})
	t.Run("a newer format is refused", func(t *testing.T) {
		_, err := Parse([]byte(`{"schemaVersion":2,"target":"vps","secrets":[]}`))
		var pe *ParseError
		if !errors.Is(err, ErrUnsupportedFormat) || !errors.As(err, &pe) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("schemaVersion below 1 is refused", func(t *testing.T) {
		if _, err := Parse([]byte(`{"schemaVersion":0,"target":"vps","secrets":[]}`)); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestValidate(t *testing.T) {
	entry := func(mods ...string) string { // replace "from=>to" in okEntry
		e := okEntry
		for _, m := range mods {
			from, to, _ := strings.Cut(m, "=>")
			e = strings.Replace(e, from, to, 1)
		}
		return e
	}
	groupA := func(name, onMissing string, since int) string {
		return `{"name":"` + name + `","services":["auth"],"type":"opaque","size":null,"group":"g","onMissing":"` + onMissing +
			`","since":` + string(rune('0'+since)) + `,"file":null}`
	}
	cases := []struct {
		name    string
		file    string
		target  string
		wantErr string // "" = valid
	}{
		{"valid", minimal(okEntry), "vps", ""},
		{"a vps schema is not a local one", minimal(okEntry), "local", `target "vps", not "docker-local"`},
		{"unsupported cli target", minimal(okEntry), "k8s", "unsupported target"},
		{"unknown onMissing is refused, not read as generate", minimal(entry(`"generate"=>"sometimes"`)), "vps", "unknown onMissing"},
		{"unknown service", minimal(entry(`"auth"=>"frontend"`)), "vps", "unknown service"},
		{"no services", minimal(entry(`["auth"]=>[]`)), "vps", "no services"},
		{"service twice in one entry", minimal(entry(`["auth"]=>["auth","auth"]`)), "vps", "listed twice"},
		{"duplicate (name, service)", minimal(okEntry + "," + okEntry), "vps", "more than one entry for service auth"},
		{"same name for another service is fine", minimal(okEntry + "," + entry(`["auth"]=>["central"]`)), "vps", ""},
		{"overlap between a shared entry and a single one", minimal(entry(`["auth"]=>["auth","central"]`) + "," + entry(`["auth"]=>["central"]`)), "vps", "more than one entry for service central"},
		{"since 0", minimal(entry(`"since":1` + `=>"since":0`)), "vps", "since 0 is outside"},
		{"since above the revision", minimal(entry(`"since":1` + `=>"since":2`)), "vps", "since 2 is outside 1..1"},
		{"utils outside the utils key entry", minimal(entry(`["auth"]=>["utils"]`)), "vps", "service utils is only for the entry written to"},
		{"utils with the utils key file is fine", minimal(entry(`["auth"]=>["utils"]`, `"file":null`+`=>"file":"utils.private-key.jwk"`)), "vps", ""},
		{"bad name", minimal(entry(`"A_SECRET"=>"A SECRET\n"`)), "vps", "not an identifier"},
		{"file with a directory part", minimal(entry(`"file":null` + `=>"file":"../x"`)), "vps", "plain file name"},
		{"file with a backslash", minimal(entry(`"file":null` + `=>"file":"a\\b"`)), "vps", "plain file name"},
		{"group members with different onMissing", minimal(groupA("A", "generate", 1) + "," + groupA("B", "external", 1)), "vps", "different onMissing"},
		{"group members with different since",
			`{"schemaVersion":1,"revision":2,"target":"vps","secrets":[` + groupA("A", "generate", 1) + "," + groupA("B", "generate", 2) + `]}`,
			"vps", "different since"},
		{"revision 0", `{"schemaVersion":1,"revision":0,"target":"vps","secrets":[` + okEntry + `]}`, "vps", `"revision" is 0`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, c.file), c.target)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("want an error containing %q, got %v", c.wantErr, err)
			}
		})
	}
	t.Run("every problem is reported, not only the first", func(t *testing.T) {
		_, err := Load(writeTemp(t, minimal(entry(`"auth"=>"nope"`, `"generate"=>"x"`))), "vps")
		if err == nil || !strings.Contains(err.Error(), "unknown service") || !strings.Contains(err.Error(), "unknown onMissing") {
			t.Fatalf("got %v", err)
		}
	})
}

// The contract test: the CLI's reading of the schema versola-tools really
// writes. It fails when the two drift apart, instead of the CLI quietly
// treating something differently.
func TestContractRealSchemaFiles(t *testing.T) {
	vps := mustLoad(t, vpsFixture, "vps")
	local := mustLoad(t, localFixture, "local")

	firstInstallOnly := func(s *Schema) (names []string) {
		for _, spec := range s.Secrets {
			if spec.OnMissing == GenerateOnFirstInstallOnly {
				names = append(names, spec.Name)
			}
		}
		return
	}
	if got := len(firstInstallOnly(vps)); got != 13 {
		t.Errorf("vps: %d first-install-only entries, the schema documents 13: %v", got, firstInstallOnly(vps))
	}
	if got := len(firstInstallOnly(local)); got != 12 {
		t.Errorf("docker-local has no Postgres entry, so 12 first-install-only entries: got %d", got)
	}

	t.Run("vps POSTGRES_PASSWORD is one value shared by three services", func(t *testing.T) {
		var found []Spec
		for _, spec := range vps.Secrets {
			if spec.Name == "POSTGRES_PASSWORD" {
				found = append(found, spec)
			}
		}
		if len(found) != 1 || strings.Join(found[0].Services, ",") != "auth,central,edge" {
			t.Errorf("got %+v", found)
		}
	})
	t.Run("docker-local has no Postgres or admin password", func(t *testing.T) {
		for _, spec := range local.Secrets {
			if spec.Name == "POSTGRES_PASSWORD" || spec.Name == "ADMIN_BOOTSTRAP_PASSWORD" {
				t.Errorf("unexpected %s", spec.Name)
			}
		}
	})
	t.Run("the utils key is found by its file, and its group has two members", func(t *testing.T) {
		var withFile *Spec
		for i, spec := range vps.Secrets {
			if spec.File != nil && *spec.File == UtilsKeyFile {
				withFile = &vps.Secrets[i]
			}
		}
		if withFile == nil || withFile.Group == nil || withFile.Name != "UTILS_PRIVATE_KEY_JWK" {
			t.Fatalf("no utils entry with %s: %+v", UtilsKeyFile, withFile)
		}
		members := 0
		for _, spec := range vps.Secrets {
			if spec.Group != nil && *spec.Group == *withFile.Group {
				members++
			}
		}
		if members != 2 {
			t.Errorf("the utils group has %d members", members)
		}
	})
	t.Run("the revision is read", func(t *testing.T) {
		if vps.RevisionNumber() != 1 {
			t.Errorf("revision %d", vps.RevisionNumber())
		}
	})
}
