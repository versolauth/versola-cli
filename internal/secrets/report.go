package secrets

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode"
)

// The report is built from a Result's Actions and Problems, which are made of
// names and fixed words. That is the whole defence against a value reaching the
// terminal or a log: there is nothing in a Result that could carry one, so
// neither renderer below can print one, by mistake or otherwise.

// jsonReport is what `secrets plan --json` prints: a Result plus the version
// asked about and the verdict.
type jsonReport struct {
	Version string `json:"version"`
	OK      bool   `json:"ok"`
	Result
}

// RenderJSON writes the plan as one JSON document.
func RenderJSON(w io.Writer, version string, r Result) error {
	// Never null: a consumer iterating over "actions" should not need a check.
	if r.Actions == nil {
		r.Actions = []Action{}
	}
	if r.Problems == nil {
		r.Problems = []Problem{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(jsonReport{Version: version, OK: r.OK(), Result: r})
}

// RenderText writes the plan for a person.
func RenderText(w io.Writer, version string, r Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Secrets plan: %s, Versola %s\n", r.Target, clean(version))
	fmt.Fprintf(&b, "Schema revision %d", r.SchemaRevision)
	if r.DeployedRevision > 0 {
		fmt.Fprintf(&b, ", deployed revision %d", r.DeployedRevision)
	}
	fmt.Fprintf(&b, " — %s\n\n", describeState(r.State))

	if len(r.Actions) == 0 {
		b.WriteString("  (nothing to do)\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		for _, a := range r.Actions {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", clean(a.Service), clean(a.Name), describeAction(a))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(r.Problems) > 0 {
		fmt.Fprintf(&b, "\nProblems (%d):\n", len(r.Problems))
		for _, p := range r.Problems {
			fmt.Fprintf(&b, "  %s: %s\n", p.Kind, explain(r.Target, p))
		}
		b.WriteString("\nResult: NOT OK — nothing would be applied.\n")
	} else {
		b.WriteString("\nResult: OK\n")
	}
	// Said every time: this is a preview, and `configure` does not follow it yet.
	b.WriteString("This is a preview: `versola configure` does not apply this plan yet; it still keeps a stored value and takes a generated one for anything missing.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func describeState(s StateKind) string {
	switch s {
	case StateFirstInstall:
		return "first install (no deployment record, OpenBao is empty)"
	case StateUpgrade:
		return "upgrade (a deployment record exists, OpenBao holds secrets)"
	case StateAlarm:
		return "ALARM: a deployment record exists but OpenBao holds no secrets"
	case StateUnknown:
		return "UNKNOWN: no deployment record, but OpenBao already holds secrets"
	case StateLocal:
		return "local (a throwaway stack: first-install rules are not applied)"
	}
	return string(s)
}

func describeAction(a Action) string {
	switch a.Kind {
	case ActionCreate:
		return "create (" + string(a.Source) + ", " + describeReason(a.Reason) + ")"
	case ActionKeep:
		return "keep"
	case ActionOrphan:
		return "orphan (stored, not in the schema; left alone)"
	case ActionSkipUtils:
		return "skip (the utils key is handled separately)"
	}
	return string(a.Kind)
}

func describeReason(r Reason) string {
	switch r {
	case ReasonFirstInstall:
		return "first install"
	case ReasonNewInRevision:
		return "new in this revision"
	case ReasonGenerate:
		return "generated when missing"
	case ReasonForcedByFlag:
		return "confirmed by the operator"
	case ReasonFillShared:
		return "shared with a service that has it"
	case ReasonLocal:
		return "local is throwaway"
	}
	return string(r)
}

// explain is the one-line meaning of a problem. It is assembled from fixed text
// and names only.
func explain(target string, p Problem) string {
	name := clean(p.Name)
	services := strings.Join(cleanAll(p.Services), ", ")
	switch p.Kind {
	case ProblemLost:
		return fmt.Sprintf("%s (%s) was part of the deployed revision, but OpenBao does not hold it, and stored data depends on it. "+
			"Restore it from a backup; generating a new one would make that data unreadable.", name, services)
	case ProblemGroupPartial:
		hint := "Restore the missing ones from a backup."
		if target == "local" {
			hint = "local is a throwaway stack: `versola uninstall`, then configure again, starts clean."
		}
		return fmt.Sprintf("group %s is stored only in part (stored: %s; missing: %s). Its members only work as a set, so none is generated. %s",
			clean(p.Group), strings.Join(cleanAll(p.Present), ", "), strings.Join(cleanAll(p.Missing), ", "), hint)
	case ProblemSharedConflict:
		return fmt.Sprintf("%s is one value shared by %s, but OpenBao holds different values for them. "+
			"Set the same, correct value under secret/versola/%s/<service> for each of them.", name, services, clean(target))
	case ProblemUnknownState:
		return fmt.Sprintf("%s (%s) is not in OpenBao, and this machine has no record of a deployment, so it cannot be told whether the secret is new or lost. "+
			"It is not generated. Restore it from a backup.", name, services)
	case ProblemVaultEmpty:
		return fmt.Sprintf("this machine has a deployment record for %s, but OpenBao holds no secrets at all: it looks wiped, or is not the OpenBao that deployment used. "+
			"%s (%s) is not generated. Restore OpenBao from a backup.", clean(target), name, services)
	case ProblemExternalMissing:
		return fmt.Sprintf("%s (%s) is supplied by the operator and is not in OpenBao. Set it under secret/versola/%s/<service> first.", name, services, clean(target))
	case ProblemNoCandidate:
		return fmt.Sprintf("versola-tools wrote no value for %s (%s), although the schema lists it: the tools image and its schema disagree.", name, services)
	case ProblemCandidateConflict:
		return fmt.Sprintf("versola-tools wrote different values for the copies of %s (%s), which is one shared value: the tools image is inconsistent.", name, services)
	case ProblemUnknownSecret:
		return fmt.Sprintf("%s is not in the schema, so confirming its generation does nothing: check the spelling.", name)
	case ProblemInvalidSchema:
		return fmt.Sprintf("%s (%s) has a policy this version of versola does not know, so nothing is decided for it: upgrade versola.", name, services)
	}
	return string(p.Kind)
}

// clean makes a name safe to put on a terminal. Names from the schema are
// already checked (see Validate); orphan names come from the store and are
// whatever someone wrote there.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		// Control characters, and the invisible format characters (bidi
		// overrides, zero-width joiners, ...) that can make a name read as
		// something else, plus the Unicode line and paragraph separators.
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func cleanAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = clean(s)
	}
	return out
}
