package secrets

import "sort"

// Values holds secret values by service, then by name: Values["auth"]["PASSWORDS_SECRET"].
// It is how candidates (what versola-tools just generated) and existing values
// (what OpenBao holds) come into Plan. Nothing in a Result refers back to it.
type Values map[string]map[string]string

// PolicyServices are the OpenBao paths whose contents Plan weighs. The `utils`
// path is not among them: the utils group is outside the policy engine (see
// Plan), and it is stored apart from the services on purpose.
var PolicyServices = []string{"auth", "central", "edge"}

// Previous is what this machine recorded about its last configure.
type Previous struct {
	// Target is the CLI target that configure was for ("local", "vps").
	Target string
	// Revision is the schema revision it was deployed with; at least 1 (a
	// deployment that recorded none is revision 1 -- state does that).
	Revision int
}

// Options are the caller's choices for one Plan.
type Options struct {
	// Target is the CLI target being configured: "local" or "vps".
	Target string
	// GenerateMissing names secrets the operator explicitly confirmed may be
	// generated although the rules would stop (lost, or unknown state). It is
	// a confirmation about NAMES; it never makes a group complete.
	GenerateMissing map[string]bool
}

// StateKind is what Plan concluded about this deployment from the machine's
// record and the store.
type StateKind string

const (
	// StateFirstInstall: no record, the store is empty.
	StateFirstInstall StateKind = "first-install"
	// StateUpgrade: a record for this target, the store holds secrets.
	StateUpgrade StateKind = "upgrade"
	// StateAlarm: a record for this target, but the store holds nothing --
	// wiped, or the wrong OpenBao.
	StateAlarm StateKind = "alarm"
	// StateUnknown: no record, but the store holds secrets -- an install from
	// before records, another machine, or a lost ~/.versola.
	StateUnknown StateKind = "unknown"
	// StateLocal: target local. A throwaway stack: the rules about first
	// install and upgrade do not apply to it (see Plan).
	StateLocal StateKind = "local"
)

type ActionKind string

const (
	ActionCreate    ActionKind = "create"
	ActionKeep      ActionKind = "keep"
	ActionOrphan    ActionKind = "orphan"
	ActionSkipUtils ActionKind = "skip-utils"
)

// Source says where a created value comes from.
type Source string

const (
	// SourceGenerated: this run's candidate from versola-tools.
	SourceGenerated Source = "generated"
	// SourceStored: a value the store already holds for a service that shares
	// this secret, copied into the one that lacks it.
	SourceStored Source = "stored"
)

type Reason string

const (
	ReasonFirstInstall  Reason = "first-install"
	ReasonNewInRevision Reason = "new-in-revision"
	ReasonGenerate      Reason = "policy-generate"
	ReasonForcedByFlag  Reason = "forced-by-flag"
	ReasonFillShared    Reason = "fill-shared"
	ReasonLocal         Reason = "local-throwaway"
)

// Action is one decision about one (service, name). It names what happens and
// why. It has no field for a value, and must never get one: the report is
// built from Actions and Problems alone.
type Action struct {
	Service string     `json:"service"`
	Name    string     `json:"name"`
	Kind    ActionKind `json:"action"`
	Source  Source     `json:"source,omitempty"`
	Reason  Reason     `json:"reason,omitempty"`
}

type ProblemKind string

const (
	// ProblemLost: the secret existed when this was deployed, the store does
	// not hold it, and something depends on it.
	ProblemLost ProblemKind = "lost"
	// ProblemGroupPartial: only some members of a group are stored.
	ProblemGroupPartial ProblemKind = "group-partial"
	// ProblemSharedConflict: services sharing one secret hold different values.
	ProblemSharedConflict ProblemKind = "shared-conflict"
	// ProblemUnknownState: no record, but the store is not empty; whether the
	// secret is new or lost cannot be told.
	ProblemUnknownState ProblemKind = "unknown-state"
	// ProblemVaultEmpty: a record says it was deployed, but the store is empty.
	ProblemVaultEmpty ProblemKind = "vault-empty-with-state"
	// ProblemExternalMissing: an operator-supplied secret is not there.
	ProblemExternalMissing ProblemKind = "external-missing"
	// ProblemNoCandidate: the schema lists a secret versola-tools wrote no
	// value for.
	ProblemNoCandidate ProblemKind = "no-candidate"
	// ProblemCandidateConflict: versola-tools wrote different values for the
	// copies of one shared secret.
	ProblemCandidateConflict ProblemKind = "candidate-conflict"
	// ProblemUnknownSecret: Options.GenerateMissing names something not in the
	// schema.
	ProblemUnknownSecret ProblemKind = "unknown-secret"
	// ProblemInvalidSchema: an entry Plan cannot interpret (a policy it does not
	// know). Load refuses such a schema; this is what a Plan fed an unchecked one
	// answers, rather than guessing.
	ProblemInvalidSchema ProblemKind = "invalid-schema"
)

// Problem is a reason the plan cannot be applied. Names only.
type Problem struct {
	Kind ProblemKind `json:"kind"`
	Name string      `json:"name,omitempty"`
	// Service is set when the problem is about one (service, name).
	Service string `json:"service,omitempty"`
	// Services are all the services sharing the secret the problem is about.
	Services []string `json:"services,omitempty"`
	Group    string   `json:"group,omitempty"`
	// Present and Missing are the members of a partial group, as service/name.
	Present []string `json:"present,omitempty"`
	Missing []string `json:"missing,omitempty"`
}

// Result is a plan: what would be done, and what stops it. Applying it is only
// allowed when OK.
type Result struct {
	Target           string    `json:"target"`
	State            StateKind `json:"state"`
	SchemaRevision   int       `json:"schemaRevision"`
	DeployedRevision int       `json:"deployedRevision,omitempty"`
	Actions          []Action  `json:"actions"`
	Problems         []Problem `json:"problems"`
}

// OK reports that nothing stops the plan.
func (r Result) OK() bool { return len(r.Problems) == 0 }

// Plan decides, for every secret in the schema, what a configure may do.
//
//   - schema is a validated schema for opts.Target (Load does that).
//   - candidates are the freshly generated values (from *.generated-secrets.env);
//     existing is what OpenBao holds for auth, central and edge.
//   - prev is the machine's record of its last configure, nil if there is none.
//     A record for a different target is ignored: it says nothing about this one.
//
// An existing value always wins; the policy only decides what to do when there
// is none. State is concluded as follows, and only matters for secrets that are
// generate-on-first-install-only:
//
//	record  store      meaning
//	none    empty      first install: generate everything
//	yes     not empty  upgrade: generate a secret only if it is new (since > the
//	                   recorded revision); otherwise it is lost
//	yes     empty      alarm: the store looks wiped; stop
//	none    not empty  unknown: cannot tell new from lost; stop
//
// Decisions worth knowing:
//   - Target local is a throwaway stack: its first-install-only secrets are
//     handled as plain `generate`, and the table above does not apply. (A group
//     that is only partly stored is still an error: a key pair that does not
//     match is wrong on any target.)
//   - The utils group -- found by the file its private key is written to, not by
//     a name -- is outside this engine: its two halves live in two places, and
//     reconcileUtilsKey in package deploy owns them. Its entries are reported as
//     skip-utils and never produce a problem.
//   - A secret listed for several services is ONE value. Stored copies that
//     differ are an error; a copy missing where others are stored is filled with
//     the stored value.
//   - A group is all or nothing: stored in part is an error, never completed.
//
// Plan reads its inputs and returns a fresh Result; it never keeps or returns a
// value.
func Plan(schema *Schema, candidates, existing Values, prev *Previous, opts Options) Result {
	res := Result{
		Target:         opts.Target,
		SchemaRevision: schema.RevisionNumber(),
		Actions:        []Action{},
		Problems:       []Problem{},
	}

	// Which state are we in?
	var deployed int
	vaultEmpty := true
	for _, service := range PolicyServices {
		if len(existing[service]) > 0 {
			vaultEmpty = false
		}
	}
	usable := prev != nil && prev.Target == opts.Target
	if usable {
		deployed = prev.Revision
		if deployed < 1 {
			deployed = 1
		}
		res.DeployedRevision = deployed
	}
	switch {
	case opts.Target == "local":
		res.State = StateLocal
	case !usable && vaultEmpty:
		res.State = StateFirstInstall
	case usable && !vaultEmpty:
		res.State = StateUpgrade
	case usable && vaultEmpty:
		res.State = StateAlarm
	default:
		res.State = StateUnknown
	}

	// The utils group is skipped as a whole.
	utilsGroups := map[string]bool{}
	for _, spec := range schema.Secrets {
		if spec.File != nil && *spec.File == UtilsKeyFile && spec.Group != nil {
			utilsGroups[*spec.Group] = true
		}
	}
	isUtils := func(spec Spec) bool {
		if spec.File != nil && *spec.File == UtilsKeyFile {
			return true
		}
		return spec.Group != nil && utilsGroups[*spec.Group]
	}

	// Which (service, name) pairs the schema knows, for finding orphans.
	known := map[string]map[string]bool{}
	for _, spec := range schema.Secrets {
		for _, service := range spec.Services {
			if known[service] == nil {
				known[service] = map[string]bool{}
			}
			known[service][spec.Name] = true
		}
	}

	stored := func(service, name string) (string, bool) {
		v, ok := existing[service][name]
		return v, ok
	}
	// isStored: some service holds a value for this secret.
	isStored := func(spec Spec) bool {
		for _, service := range spec.Services {
			if _, ok := stored(service, spec.Name); ok {
				return true
			}
		}
		return false
	}

	// Groups, except utils: all or nothing.
	groups := map[string][]Spec{}
	for _, spec := range schema.Secrets {
		if spec.Group != nil && !isUtils(spec) {
			groups[*spec.Group] = append(groups[*spec.Group], spec)
		}
	}
	blocked := map[string]bool{} // groups reported as partial
	for group, members := range groups {
		// Partial is decided per member (stored anywhere, or not at all); the
		// labels say where each copy actually is.
		anyStored, anyAbsent := false, false
		var present, missing []string
		for _, m := range members {
			if isStored(m) {
				anyStored = true
			} else {
				anyAbsent = true
			}
			for _, service := range m.Services {
				label := service + "/" + m.Name
				if _, ok := stored(service, m.Name); ok {
					present = append(present, label)
				} else {
					missing = append(missing, label)
				}
			}
		}
		if anyStored && anyAbsent {
			sort.Strings(present)
			sort.Strings(missing)
			blocked[group] = true
			res.Problems = append(res.Problems, Problem{
				Kind: ProblemGroupPartial, Group: group, Present: present, Missing: missing,
			})
		}
	}

	// A confirmation by name counts for a group member only when EVERY member of
	// its group is confirmed: generating half a key pair is what groups exist to
	// prevent, so a lone confirmation is ignored and the member gets its normal
	// problem.
	allConfirmed := map[string]bool{}
	for group, members := range groups {
		all := true
		for _, m := range members {
			all = all && opts.GenerateMissing[m.Name]
		}
		allConfirmed[group] = all
	}
	confirmed := func(spec Spec) Options {
		o := opts
		o.GenerateMissing = map[string]bool{spec.Name: opts.GenerateMissing[spec.Name] && (spec.Group == nil || allConfirmed[*spec.Group])}
		return o
	}

	problem := func(kind ProblemKind, spec Spec) {
		res.Problems = append(res.Problems, Problem{Kind: kind, Name: spec.Name, Services: append([]string(nil), spec.Services...)})
	}

	for _, spec := range schema.Secrets {
		if isUtils(spec) {
			for _, service := range spec.Services {
				res.Actions = append(res.Actions, Action{Service: service, Name: spec.Name, Kind: ActionSkipUtils})
			}
			continue
		}
		if spec.Group != nil && blocked[*spec.Group] {
			continue // already reported as a whole
		}

		// A shared secret is one value: the stored copies have to agree.
		var present, absent []string
		distinct := map[string]bool{}
		for _, service := range spec.Services {
			if v, ok := stored(service, spec.Name); ok {
				present = append(present, service)
				distinct[v] = true
			} else {
				absent = append(absent, service)
			}
		}
		if len(distinct) > 1 {
			problem(ProblemSharedConflict, spec)
			continue
		}

		if len(present) > 0 {
			// Something stored: it wins, and a service lacking it gets it.
			for _, service := range present {
				res.Actions = append(res.Actions, Action{Service: service, Name: spec.Name, Kind: ActionKeep})
			}
			for _, service := range absent {
				res.Actions = append(res.Actions, Action{
					Service: service, Name: spec.Name, Kind: ActionCreate, Source: SourceStored, Reason: ReasonFillShared,
				})
			}
			continue
		}

		// Nothing stored anywhere: the policy decides.
		reason, blocker := whyGenerate(spec, res.State, deployed, confirmed(spec))
		if spec.OnMissing == External {
			problem(ProblemExternalMissing, spec)
			continue
		}
		if blocker != "" {
			problem(blocker, spec)
			continue
		}
		// Allowed. It needs one candidate, the same everywhere.
		seen := map[string]bool{}
		for _, service := range spec.Services {
			if v, ok := candidates[service][spec.Name]; ok {
				seen[v] = true
			}
		}
		switch {
		case len(seen) == 0:
			problem(ProblemNoCandidate, spec)
			continue
		case len(seen) > 1:
			problem(ProblemCandidateConflict, spec)
			continue
		}
		for _, service := range spec.Services {
			res.Actions = append(res.Actions, Action{
				Service: service, Name: spec.Name, Kind: ActionCreate, Source: SourceGenerated, Reason: reason,
			})
		}
	}

	// Names the operator confirmed that the schema has never heard of: most
	// likely a typo, and silently ignoring it would leave them thinking a
	// secret was handled.
	forcedNames := make([]string, 0, len(opts.GenerateMissing))
	for name, on := range opts.GenerateMissing {
		if on {
			forcedNames = append(forcedNames, name)
		}
	}
	sort.Strings(forcedNames)
	inSchema := map[string]bool{}
	for _, spec := range schema.Secrets {
		inSchema[spec.Name] = true
	}
	for _, name := range forcedNames {
		if !inSchema[name] {
			res.Problems = append(res.Problems, Problem{Kind: ProblemUnknownSecret, Name: name})
		}
	}

	// Orphans: stored under a service path but not in the schema. Reported, never removed.
	for _, service := range PolicyServices {
		for name := range existing[service] {
			if !known[service][name] {
				res.Actions = append(res.Actions, Action{Service: service, Name: name, Kind: ActionOrphan})
			}
		}
	}

	sort.SliceStable(res.Actions, func(i, j int) bool {
		a, b := res.Actions[i], res.Actions[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Name < b.Name
	})
	sort.SliceStable(res.Problems, func(i, j int) bool {
		a, b := res.Problems[i], res.Problems[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Name < b.Name
	})
	return res
}

// whyGenerate answers, for a secret with no stored value: may this run
// generate it, and if so on what grounds -- or, if not, which problem to
// report. External secrets are handled by the caller.
func whyGenerate(spec Spec, state StateKind, deployed int, opts Options) (Reason, ProblemKind) {
	switch spec.OnMissing {
	case Generate:
		return ReasonGenerate, ""
	case GenerateOnFirstInstallOnly:
		switch state {
		case StateLocal:
			return ReasonLocal, ""
		case StateFirstInstall:
			return ReasonFirstInstall, ""
		case StateUpgrade:
			if spec.SinceRevision() > deployed {
				return ReasonNewInRevision, ""
			}
			if opts.GenerateMissing[spec.Name] {
				return ReasonForcedByFlag, ""
			}
			return "", ProblemLost
		case StateAlarm:
			if opts.GenerateMissing[spec.Name] {
				return ReasonForcedByFlag, ""
			}
			return "", ProblemVaultEmpty
		default: // StateUnknown
			if opts.GenerateMissing[spec.Name] {
				return ReasonForcedByFlag, ""
			}
			return "", ProblemUnknownState
		}
	}
	// External: the caller reports it. Anything else is a policy this build does
	// not know (Validate refuses it; a Plan fed an unchecked schema gets here):
	// never generate on a guess.
	if spec.OnMissing == External {
		return "", ""
	}
	return "", ProblemInvalidSchema
}
