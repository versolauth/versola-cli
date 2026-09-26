package deploy

import (
	"reflect"
	"testing"
)

func TestPickPostgresPassword(t *testing.T) {
	cases := []struct {
		name            string
		existing, cands map[string]map[string]string
		wantPass        string
		wantNew         bool
	}{
		{"local: no key anywhere",
			map[string]map[string]string{},
			map[string]map[string]string{"auth": {"OTHER": "x"}},
			"", false},
		{"fresh openbao: auth's candidate, new",
			map[string]map[string]string{},
			map[string]map[string]string{"auth": {postgresPasswordKey: "a"}, "central": {postgresPasswordKey: "c"}, "edge": {postgresPasswordKey: "e"}},
			"a", true},
		{"already stored: reused, not new",
			map[string]map[string]string{"auth": {postgresPasswordKey: "stored"}},
			map[string]map[string]string{"auth": {postgresPasswordKey: "a"}, "central": {postgresPasswordKey: "c"}},
			"stored", false},
		{"stored only for a later service: still reused",
			map[string]map[string]string{"edge": {postgresPasswordKey: "stored"}},
			map[string]map[string]string{"auth": {postgresPasswordKey: "a"}, "edge": {postgresPasswordKey: "e"}},
			"stored", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, isNew := pickPostgresPassword(c.existing, c.cands)
			if p != c.wantPass || isNew != c.wantNew {
				t.Errorf("got (%q, %v), want (%q, %v)", p, isNew, c.wantPass, c.wantNew)
			}
		})
	}
}

func TestMergeSecrets(t *testing.T) {
	final, wrote := mergeSecrets(
		map[string]string{"KEEP": "old", "EXTRA": "x"},
		map[string]string{"KEEP": "new", "ADD": "cand", postgresPasswordKey: "own-candidate"},
		"shared")
	want := map[string]string{"KEEP": "old", "EXTRA": "x", "ADD": "cand", postgresPasswordKey: "shared"}
	if !wrote || !reflect.DeepEqual(final, want) {
		t.Errorf("got %v (wrote=%v), want %v", final, wrote, want)
	}

	final, wrote = mergeSecrets(map[string]string{postgresPasswordKey: "stored"}, map[string]string{postgresPasswordKey: "cand"}, "shared")
	if wrote || final[postgresPasswordKey] != "stored" {
		t.Errorf("stored password must win and nothing be written, got %v (wrote=%v)", final, wrote)
	}

	final, wrote = mergeSecrets(nil, map[string]string{"A": "1"}, "")
	if !wrote || final["A"] != "1" {
		t.Errorf("nil existing: got %v (wrote=%v)", final, wrote)
	}
}

func TestParsePostgresUser(t *testing.T) {
	conf := `auth { user = "not-this" }
postgres {
  url = "jdbc:postgresql://127.0.0.1:5432/auth?currentSchema=auth"
  user = "versola_app"
  password = ${POSTGRES_PASSWORD}
}`
	if got := parsePostgresUser(conf); got != "versola_app" {
		t.Errorf("got %q", got)
	}
	noUser := `postgres {
  url = "jdbc:postgresql://127.0.0.1:5432/auth"
}
smtp { user = "mailer" }`
	if got := parsePostgresUser(noUser); got != "<postgres user from auth.conf>" {
		t.Errorf("must not read user from a later block: got %q", got)
	}
	if got := parsePostgresUser("no postgres block"); got != "<postgres user from auth.conf>" {
		t.Errorf("fallback: got %q", got)
	}
}

func TestPostgresRoleSQL(t *testing.T) {
	create, alter := postgresRoleSQL(`we"ird`, `pa'ss`)
	if create != `CREATE ROLE "we""ird" WITH LOGIN PASSWORD 'pa''ss';` {
		t.Errorf("create: %s", create)
	}
	if alter != `ALTER ROLE "we""ird" WITH PASSWORD 'pa''ss';` {
		t.Errorf("alter: %s", alter)
	}
}

func TestConflictingPostgresPasswords(t *testing.T) {
	cases := []struct {
		name     string
		existing map[string]map[string]string
		want     []string
	}{
		{"nothing stored", map[string]map[string]string{}, nil},
		{"one stored", map[string]map[string]string{"auth": {postgresPasswordKey: "a"}}, nil},
		{"all agree", map[string]map[string]string{"auth": {postgresPasswordKey: "a"}, "central": {postgresPasswordKey: "a"}, "edge": {postgresPasswordKey: "a"}}, nil},
		{"disagree", map[string]map[string]string{"auth": {postgresPasswordKey: "a"}, "central": {postgresPasswordKey: "b"}, "edge": {"OTHER": "x"}}, []string{"auth", "central"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := conflictingPostgresPasswords(c.existing); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
