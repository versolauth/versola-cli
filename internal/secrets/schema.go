// Package secrets reads the secret schema versola-tools writes next to a
// release's configs (secrets.schema.json) and decides, from the schema, the
// values the secret store already holds and what this machine last deployed,
// what a `configure` may do about each secret: keep it, create it, or stop.
//
// Everything here is pure: no network, no Docker, no files except Load. That
// is deliberate -- the rules decide whether a deployment's data stays
// readable, so they are exercised by tables of inputs rather than by running
// anything. Nothing here writes to OpenBao; applying a plan is a later step.
//
// Secret VALUES are inputs of Plan (it has to know whether a value exists and
// whether shared copies agree) but never part of anything it returns: no type
// in this package that leaves it has a field that could hold one.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
)

const (
	// FileName is where versola-tools leaves the schema in a bundle.
	FileName = "secrets.schema.json"

	// SupportedSchemaVersion is the newest FORMAT of the file this build reads
	// (the file's "schemaVersion": which fields an entry has and what they
	// mean). Not the same thing as the file's "revision", which says which
	// secrets it lists. A newer format is refused, not guessed at: versola-tools
	// only bumps it when the format changes incompatibly.
	SupportedSchemaVersion = 1

	// UtilsKeyFile is the file the private key of the `utils` client is
	// written to instead of a *.generated-secrets.env. The utils group is
	// recognised by it, not by a name (see Plan).
	UtilsKeyFile = "utils.private-key.jwk"
)

// OnMissing is what the schema says to do when a deployment has no value for
// a secret.
type OnMissing string

const (
	// Generate: take this run's freshly generated candidate.
	Generate OnMissing = "generate"
	// GenerateOnFirstInstallOnly: take the candidate on a first install, or
	// when the secret is new in this revision; otherwise a missing value is
	// an error, because something already depends on the real one.
	GenerateOnFirstInstallOnly OnMissing = "generate-on-first-install-only"
	// External: never generated here; the operator supplies it.
	External OnMissing = "external"
)

func (o OnMissing) known() bool {
	return o == Generate || o == GenerateOnFirstInstallOnly || o == External
}

// Spec is one entry of the schema. Only what the policy needs is interpreted;
// Type and Size are carried along for reports and later steps.
//
// Since is a pointer (and Schema.Revision below) because the JSON field is
// optional: nil means "absent", which is read as 1, and is told apart from an
// explicit 0, which is invalid. A plain int could not do that -- Go decodes
// a missing field and a 0 into the same zero value.
type Spec struct {
	Name      string    `json:"name"`
	Services  []string  `json:"services"`
	Type      string    `json:"type"`
	Size      *int      `json:"size"`
	Group     *string   `json:"group"`
	OnMissing OnMissing `json:"onMissing"`
	Since     *int      `json:"since"`
	File      *string   `json:"file"`
}

// SinceRevision is the revision this secret first appeared in; a file that
// does not say is a schema from before revisions existed, i.e. 1.
func (s Spec) SinceRevision() int {
	if s.Since == nil {
		return 1
	}
	return *s.Since
}

// Schema is a parsed secrets.schema.json.
//
// Unknown fields -- at the top and in every entry -- are ignored, on purpose:
// that is encoding/json's default (DisallowUnknownFields is what turns it
// off), and it is what lets versola-tools add a field without making every
// older CLI refuse the file. What MUST NOT be ignored is a change of meaning,
// which versola-tools signals with a higher schemaVersion; see Parse.
type Schema struct {
	SchemaVersion int    `json:"schemaVersion"`
	Revision      *int   `json:"revision"`
	Target        string `json:"target"`
	Secrets       []Spec `json:"secrets"`
}

// RevisionNumber is the revision of the schema's content; a file with no
// "revision" is revision 1.
func (s *Schema) RevisionNumber() int {
	if s.Revision == nil {
		return 1
	}
	return *s.Revision
}

// ErrNoSchema means the bundle has no secrets.schema.json at all -- the
// versola-tools that wrote it predates the schema. Distinct from a file that
// exists and cannot be used (*ParseError): the first says "use the old rule",
// the second must never be papered over.
var ErrNoSchema = errors.New("no " + FileName + " in the bundle")

// ErrUnsupportedFormat means the file's schemaVersion is newer than this CLI
// reads.
var ErrUnsupportedFormat = errors.New("the schema format is newer than this versola reads")

// ParseError means a secrets.schema.json exists but cannot be used: it is not
// valid JSON, is of an unsupported format, or contradicts itself. The message
// names fields and entries, never values (the file holds none).
type ParseError struct {
	Path string // "" when parsed from memory
	Err  error
}

func (e *ParseError) Error() string {
	if e.Path == "" {
		return FileName + " is unusable: " + e.Err.Error()
	}
	return e.Path + " is unusable: " + e.Err.Error()
}

func (e *ParseError) Unwrap() error { return e.Err }

// KnownServices are the holders a schema entry may name. Anything else is
// refused: a newer schema naming a service this CLI has no OpenBao path for
// would otherwise look like a secret that is simply missing.
var KnownServices = map[string]bool{"auth": true, "central": true, "edge": true, "utils": true}

var (
	// Names end up in reports and error messages, and orphan names even come
	// from the store: keeping them to this alphabet is what keeps a stray
	// newline or escape sequence out of the terminal.
	nameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	groupRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// SchemaTarget is the target name versola-tools uses in the schema for one of
// this CLI's targets: `local` runs versola-tools' docker-local branch.
func SchemaTarget(cliTarget string) (string, error) {
	switch cliTarget {
	case "local":
		return "docker-local", nil
	case "vps":
		return "vps", nil
	}
	return "", fmt.Errorf(`unsupported target %q — only "local" and "vps" are supported`, cliTarget)
}

// Load reads and checks the schema in path for one of this CLI's targets.
// A missing file is ErrNoSchema (test with errors.Is); any other failure after
// the file was found is a *ParseError (errors.As).
func Load(path, cliTarget string) (*Schema, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w (%s)", ErrNoSchema, path)
		}
		return nil, fmt.Errorf("couldn't read %s: %w", path, err)
	}
	s, err := Parse(raw)
	if err == nil {
		err = s.Validate(cliTarget)
	}
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			pe.Path = path
			return nil, pe
		}
		return nil, &ParseError{Path: path, Err: err}
	}
	return s, nil
}

// Parse decodes a schema and refuses a format newer than SupportedSchemaVersion.
// It checks nothing about the entries; Validate does.
func Parse(data []byte) (*Schema, error) {
	var s Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, &ParseError{Err: fmt.Errorf("not valid JSON: %w", err)}
	}
	switch {
	case s.SchemaVersion < 1:
		// Also what a literal `null` or `{}` decodes to.
		return nil, &ParseError{Err: errors.New(`"schemaVersion" is missing or below 1`)}
	case s.SchemaVersion > SupportedSchemaVersion:
		return nil, &ParseError{Err: fmt.Errorf("%w (schemaVersion %d, this one reads up to %d) -- upgrade versola",
			ErrUnsupportedFormat, s.SchemaVersion, SupportedSchemaVersion)}
	}
	return &s, nil
}

// Validate checks the schema is usable for cliTarget: the right target, sane
// entries, one value per (name, service), groups that agree with themselves,
// `since` within the file's own revision. Every problem is reported, in one
// error, not just the first.
func (s *Schema) Validate(cliTarget string) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	want, err := SchemaTarget(cliTarget)
	if err != nil {
		return &ParseError{Err: err}
	}
	if s.Target != want {
		add("it is for target %q, not %q", s.Target, want)
	}
	revision := s.RevisionNumber()
	if revision < 1 {
		add(`"revision" is %d, it must be at least 1`, revision)
	}
	if len(s.Secrets) == 0 {
		add("it lists no secrets")
	}

	type key struct{ name, service string }
	seen := map[key]bool{}
	groups := map[string][]Spec{}
	for i, spec := range s.Secrets {
		label := fmt.Sprintf("entry %d", i)
		if nameRe.MatchString(spec.Name) {
			label = spec.Name
		} else {
			add("entry %d: the name is empty or not an identifier", i)
		}
		if !spec.OnMissing.known() {
			add("%s: unknown onMissing %q", label, truncate(string(spec.OnMissing)))
		}
		if len(spec.Services) == 0 {
			add("%s: no services", label)
		}
		inEntry := map[string]bool{}
		for _, service := range spec.Services {
			switch {
			case !KnownServices[service]:
				add("%s: unknown service %q", label, truncate(service))
			case service == "utils" && (spec.File == nil || *spec.File != UtilsKeyFile):
				// `utils` is where the utils client's private key lives, not a
				// service Plan reads a store path for.
				add("%s: service utils is only for the entry written to %s", label, UtilsKeyFile)
			case inEntry[service]:
				add("%s: service %s listed twice", label, service)
			case seen[key{spec.Name, service}]:
				add("%s: more than one entry for service %s", label, service)
			}
			inEntry[service] = true
			seen[key{spec.Name, service}] = true
		}
		if spec.Since != nil && (*spec.Since < 1 || *spec.Since > revision) {
			add("%s: since %d is outside 1..%d", label, *spec.Since, revision)
		}
		if spec.File != nil && !plainFileName(*spec.File) {
			// The name is used to find a file in the bundle directory.
			add("%s: file must be a plain file name", label)
		}
		if spec.Group != nil {
			if !groupRe.MatchString(*spec.Group) {
				add("%s: the group name is empty or has unexpected characters", label)
			} else {
				groups[*spec.Group] = append(groups[*spec.Group], spec)
			}
		}
	}
	// A group is taken whole or not at all, so its members have to agree on
	// when that is allowed.
	for group, members := range groups {
		first := members[0]
		for _, m := range members[1:] {
			if m.OnMissing != first.OnMissing {
				add("group %s: members have different onMissing", group)
				break
			}
		}
		for _, m := range members[1:] {
			if m.SinceRevision() != first.SinceRevision() {
				add("group %s: members have different since", group)
				break
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return &ParseError{Err: errors.New(joinProblems(problems))}
}

// plainFileName reports whether name is a bare file name: no directory part,
// not "." or "..".
func plainFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r < ' ' {
			return false
		}
	}
	return true
}

func joinProblems(problems []string) string {
	out := ""
	for i, p := range problems {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}

// truncate keeps a value that came from the file short in an error message.
func truncate(s string) string {
	const max = 40
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
