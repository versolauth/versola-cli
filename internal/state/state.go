// Package state locates and reads the on-disk directory describing the
// deployment this machine currently has: the compose file, the configs
// versola-tools produced, and a small record of what was deployed and how
// far along it got.
//
// There's only ever one active deployment at a time — configuring again
// overwrites it. That keeps the commands that read this directory
// (status/down/uninstall) simple: they don't track multiple deployments,
// they read whatever was last written.
//
// The record itself lives in state.json rather than being inferred from
// which files happen to exist. Deploying is no longer one command: the
// configs can exist while the database hasn't been migrated yet, and both
// can be true while nothing is running. "Which of those steps have
// happened" is a real question that only an explicit record can answer.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/versolauth/versola-cli/internal/fsutil"
	"github.com/versolauth/versola-cli/internal/proxy"
)

// SchemaVersion is the layout version of state.json as written by this
// build of the CLI. It's recorded in the file so a future CLI reading an
// older deployment's state can tell what it's looking at instead of
// guessing from which fields happen to be present.
//
// Version 2 added SecretsRevision. Version 3 added PostgresHost,
// PreviousVersion, Surge and RollingBack (rolling upgrades and their abort). Version 4 added
// PostgresDB and PostgresCA (--postgres-db, --postgres-ca). A CLI refuses a file of a
// newer layout than its own (see Load) rather than load it and drop the
// fields it does not know on its next Save; an older layout is read, with
// the fields it lacks left empty.
const SchemaVersion = 4

// PostgresCAFileName is where Finalize keeps a copy of --postgres-ca's file,
// in Dir() next to state.json, so that later steps do not depend on the
// path the operator gave still existing.
const PostgresCAFileName = "postgres-ca.crt"

const stateFileName = "state.json"

// oldVersionFileName is what the first builds of this CLI wrote instead of
// state.json. It is no longer read: its presence only tells such a
// deployment apart from no deployment at all (see ErrOldInstall).
const oldVersionFileName = "version"

// ErrNotConfigured means nothing has been deployed on this machine yet.
var ErrNotConfigured = errors.New("no deployment configured yet")

// ErrNewerSchema means state.json was written by a newer versola, whose
// records this one would drop on its next Save.
var ErrNewerSchema = errors.New("state written by a newer versola")

// ErrOldInstall means the deployment on this machine was made by a
// versola-cli old enough that this one no longer carries the code to run
// it as it is (no state.json, no bundle directory, or no reverse proxy of
// the CLI's own). A new configure replaces it with one this CLI runs.
var ErrOldInstall = errors.New("this deployment was made by an older versola-cli -- run `versola configure` again")

// State is what the CLI records about the current deployment.
type State struct {
	SchemaVersion int `json:"schemaVersion"`

	// Target is where this deployment is going: "local" today, "vps"
	// later. It's recorded rather than re-asked because every step after
	// the first needs it, and a deployment silently switching target
	// halfway through would be a very confusing failure.
	Target string `json:"target"`

	// Version is the Versola release being deployed, exactly as it's
	// tagged in the registry (no leading "v" — see bootstrap's error
	// message about that).
	Version string `json:"version"`

	ConfiguredAt time.Time `json:"configuredAt"`

	// MigratedAt records when `versola migrate` last ran successfully
	// against this deployment, or nil if it hasn't yet (e.g. configure
	// just ran and migrate hasn't). A pointer rather than a zero time.Time so "not
	// migrated" is unambiguous in the JSON (the field is simply absent)
	// instead of being represented by year 1. Set by deploy.Migrate,
	// after every service's own migration has actually finished -- not
	// before, for the same reason Finalize below only commits once its
	// own work has fully succeeded.
	MigratedAt *time.Time `json:"migratedAt,omitempty"`

	// BundleDir is the name (not a full path — join it onto Dir()) of the
	// subdirectory holding the compose file and configs versola-tools
	// generated for this deployment. Empty only in records of a CLI from
	// before bundle directories, which this one no longer runs
	// (ErrOldInstall).
	//
	// This exists because of a bug observed in the wild on Docker Desktop
	// for Windows: bind-mounting the *same path* into a container twice —
	// once per `configure` run, since Prepare used to wipe and recreate
	// one fixed directory every time — could make a freshly-started
	// container's own `cp` fail with "File exists" for a file `ls` in that
	// same container shows does not exist. Reproduced with the directory
	// completely removed (not just emptied) between runs, so it isn't
	// leftover files on the Windows side; something in Docker Desktop's
	// file-sharing layer was reusing stale metadata keyed by path. A path
	// that has never been mounted before doesn't have this problem, so
	// Prepare below gives every deployment its own directory name instead
	// of ever reusing one.
	//
	// A new directory each run does NOT mean a new Compose *project* each
	// run, despite Compose's own default of deriving the project name from
	// the compose file's parent directory -- the generated compose file
	// sets `name:` explicitly (see compose.fragment.yml.template) for
	// exactly this reason: every service in it has a fixed container_name,
	// and Docker refuses to create one under a name that already exists
	// under a *different* project. Without a fixed name, a second
	// `configure`/`up` while the previous run's containers were still up
	// would fail on container-name conflicts instead of updating them in
	// place, and once BundleDir moves on, nothing would even know the old
	// containers exist anymore to stop them.
	BundleDir string `json:"bundleDir,omitempty"`

	// AuthURL is the public URL this deployment's auth service is
	// reachable at. For "local" it's the fixed http://localhost:2821 (empty
	// for local deployments recorded before local had versola-cli's own
	// proxy). For "vps" it's whatever --auth-url
	// was passed to `bootstrap`/`configure`, recorded here so Up can
	// print the right domain in its completion message instead of a
	// hardcoded one that's wrong for any deployment other than the
	// original single VPS this was written for (flagged in review on
	// versolauth/versola-cli#7).
	AuthURL string `json:"authUrl,omitempty"`

	// CentralURL is how edge reaches central's mutual-TLS listener
	// (--central-url, or central.<domain> derived from AuthURL), as it was
	// given to versola-tools; empty for vps deployments configured before
	// the flag, and for any that has no https domain to derive it from.
	CentralURL string `json:"centralUrl,omitempty"`

	// MTLSDomain, CentralMTLSDomain and CentralDomain are the public names
	// (--mtls-domain, --central-mtls-domain, --central-domain) the proxy
	// publishes beyond AuthURL's: auth's and central's mutual-TLS listeners
	// and central's enrollment route. Empty: not published, which is what a
	// record from before they existed means. Recorded so that rollout, which
	// configures from the record, keeps them, and so that `certs renew` knows
	// whether a remote edge may exist.
	MTLSDomain        string `json:"mtlsDomain,omitempty"`
	CentralMTLSDomain string `json:"centralMtlsDomain,omitempty"`
	CentralDomain     string `json:"centralDomain,omitempty"`

	// CertLifetimeDays is --cert-lifetime-days: how long the certificates
	// `mtls-init` writes live. nil: the default (365). 0: unlimited (a
	// notAfter 100 years away: nothing renews them, and no timer is installed).
	CertLifetimeDays *int `json:"certLifetimeDays,omitempty"`

	// ProxyMode is the reverse proxy's mode this deployment was configured
	// with (proxy.ModeNginx or proxy.ModeExternal for vps, proxy.ModeLocal
	// for local) -- what `up` waits on and what it tells the operator to do
	// next. Empty only in records of a CLI from before it generated the
	// proxy itself, which this one no longer runs (RequireCurrent).
	ProxyMode string `json:"proxyMode,omitempty"`

	// PostgresHost is the host:port the services reach Postgres at, as
	// --postgres-host gave it to configure (vps only; empty for local, whose
	// Postgres is a service of the compose file). Recorded so that a later
	// step that runs versola-tools again -- a rolling upgrade -- does not
	// have to be told it once more. Empty in records written before layout
	// 3: then it is not known, and such a step has to ask for it.
	PostgresHost string `json:"postgresHost,omitempty"`

	// PostgresDB is --postgres-db (vps only); empty means versola-tools'
	// own default. Recorded for the same reason as PostgresHost. Empty in
	// records written before layout 4.
	PostgresDB string `json:"postgresDb,omitempty"`

	// PostgresCA is the name, inside Dir(), of the copy of --postgres-ca's
	// file (PostgresCAFileName), or empty when there is none. See
	// PostgresCAPath.
	PostgresCA string `json:"postgresCa,omitempty"`

	// PreviousVersion is the Versola version this deployment ran before a
	// rolling upgrade (`versola rollout`) moved it to Version. Written when
	// the rollout prepares the new version's bundle, cleared when it
	// finishes; while it is set, the rollout is under way (see
	// RolloutUnderway) and Version is where it is going, not what runs. It is
	// what `rollout --abort` goes back to.
	PreviousVersion string `json:"previousVersion,omitempty"`

	// Surge marks, per scalable service, the slot a rolling upgrade started
	// as a stand-in while it replaces that service's replicas one by one: a
	// replica beyond the ones the operator asked for, to be removed when the
	// rollout is done with the service. Written before the stand-in is
	// started and removed only once its container is gone, so a rollout cut
	// off at any point knows the slot is not one of the operator's. The
	// slot itself is in Slots too (replicas.yml defines it from there).
	// `up` and `replica` refuse to act while it is set (see MixedVersions).
	Surge map[string]int `json:"surge,omitempty"`

	// RollingBack marks the rollout under way as an abort (`versola rollout
	// --abort`): Version is the version it is going back to, PreviousVersion
	// the one it is leaving. Without it, an abort cut off halfway would look
	// like a rollout to the old version, and a second --abort would turn it
	// round again. Written with the abort's own bundle, cleared when it
	// finishes. (Layout 3: added before layout 3 was released.)
	RollingBack bool `json:"rollingBack,omitempty"`

	// MountedBundleDirs are the bundles containers may currently be
	// started from, so bind-mount files from: `up` adds the current bundle
	// before it starts anything (MarkStarting) and, once everything is up,
	// narrows the list to just that bundle (MarkRunning). Nothing in it is
	// deleted by a `configure` -- including a bundle a failed `up` only
	// partly started. Nil in records written before this field existed;
	// see Finalize for how that's treated.
	MountedBundleDirs []string `json:"mountedBundleDirs,omitempty"`

	// Slots records, per scalable service (compose service name of its
	// first replica: "auth", "edge", and "central" -- which has one replica
	// but gets a stand-in during a rolling upgrade), which replica slots are deployed and
	// at which version -- see package topology for what a slot is. Absent
	// (nil) means one replica in slot 1 at Version, which is what every
	// deployment made before replicas existed is; ActiveSlots turns that
	// into the list callers should use, so none of them has to know the
	// difference.
	//
	// Finalize builds a fresh State, so a `configure` starts over from one
	// replica in slot 1: carrying replicas across a re-configure is up to
	// whatever adds replicas (0c's later steps) and rolling upgrades (0d).
	Slots map[string][]Slot `json:"slots,omitempty"`

	// SecretsRevision is the revision of secrets.schema.json (the schema's
	// own "revision" field, not its format version) this deployment's secrets
	// were last settled against: which secrets it was deployed knowing about.
	// `versola secrets plan` compares it with each secret's `since` to tell a
	// secret that is new in the version being deployed (generate it) from one
	// that existed and is now missing from OpenBao (stop).
	//
	// Absent, or 0, means revision 1 -- every deployment made before this field
	// existed, and the schema's own baseline; read it through
	// EffectiveSecretsRevision, never directly.
	//
	// Written by Finalize (see there): the revision of the schema the deployment
	// was configured with, never lowered by deploying an older release, and
	// kept when a release has no schema at all.
	SecretsRevision int `json:"secretsRevision,omitempty"`
}

// EffectiveSecretsRevision is the schema revision this deployment recorded,
// with an absent record read as 1.
func (s *State) EffectiveSecretsRevision() int {
	if s.SecretsRevision < 1 {
		return 1
	}
	return s.SecretsRevision
}

// Slot is one deployed replica of a service: its slot number and the
// Versola version it runs. The version is per slot because a rolling
// upgrade (0d) has replicas of two versions side by side.
type Slot struct {
	N       int    `json:"n"`
	Version string `json:"version"`
}

// ActiveSlots returns the slots deployed for a service, in slot order as
// recorded -- or, when none are recorded, the single implicit slot 1 at
// the deployment's own version.
func (s *State) ActiveSlots(service string) []Slot {
	if slots := s.Slots[service]; len(slots) > 0 {
		out := make([]Slot, len(slots))
		copy(out, slots)
		return out
	}
	return []Slot{{N: 1, Version: s.Version}}
}

// bundlePath resolves where this state's compose file and configs live.
func (s *State) bundlePath() (string, error) {
	if s.BundleDir == "" {
		return "", ErrOldInstall
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, s.BundleDir), nil
}

// RequireCurrent is ErrOldInstall for a record this CLI can no longer run
// as it is: one from before bundle directories or before the CLI generated
// the reverse proxy itself. Commands that start, change or migrate the
// deployment ask it first; `configure` does not, since it is what replaces
// such a record.
func (s *State) RequireCurrent() error {
	if s.BundleDir == "" || s.ProxyMode == "" {
		return ErrOldInstall
	}
	return nil
}

// MixedVersions explains why the recorded replicas are not one plain
// deployment of Version -- a replica of central, auth or edge recorded at another
// version, or a stand-in slot (Surge) -- or returns "" when they are. That
// is the state a rolling upgrade leaves while it is under way (or after it
// was cut off), and only the rolling upgrade can carry it on: `up` and
// `replica` would start every slot at its recorded version without knowing
// which way the upgrade was going.
func (s *State) MixedVersions() string {
	var why []string
	for _, svc := range []string{"central", "auth", "edge"} {
		var other []string
		for _, sl := range s.ActiveSlots(svc) {
			if sl.Version != s.Version {
				other = append(other, fmt.Sprintf("slot %d at %s", sl.N, sl.Version))
			}
		}
		if len(other) > 0 {
			why = append(why, fmt.Sprintf("%s has %s while the deployment is at %s", svc, strings.Join(other, ", "), s.Version))
		}
	}
	var surge []string
	for svc, n := range s.Surge {
		surge = append(surge, fmt.Sprintf("%s slot %d", svc, n))
	}
	if len(surge) > 0 {
		sort.Strings(surge)
		why = append(why, "a rolling upgrade's stand-in replica is recorded ("+strings.Join(surge, ", ")+")")
	}
	return strings.Join(why, "; ")
}

// RolloutUnderway reports whether a rolling upgrade has begun and not
// finished: its new bundle is prepared (PreviousVersion), or a stand-in
// replica is recorded.
func (s *State) RolloutUnderway() bool {
	return s.PreviousVersion != "" || len(s.Surge) > 0
}

// Dir returns ~/.versola/active, the directory for the current
// deployment.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("couldn't find home directory: %w", err)
	}
	return filepath.Join(home, ".versola", "active"), nil
}

// Prepare creates a fresh, empty bundle directory under ~/.versola/active
// for versola-tools to write this deployment's configs and compose file
// into, and returns its path.
//
// Deliberately does NOT touch state.json or any previous deployment's
// bundle directory -- only Finalize does that, once versola-tools and
// secret resolution have both actually succeeded (see its own comment).
// An earlier version of this function cleared the whole ~/.versola/active
// directory up front instead, on the theory that a previous run's
// state.json shouldn't linger if this run fails before writing its own.
// In practice that traded a small problem for a much bigger one: a
// redeploy that fails partway through (an image pull that times out, an
// OpenBao that's still sealed, ...) cost this machine its only record of
// whatever deployment was still actually running -- status/down/uninstall
// would all report "nothing deployed" against containers that were very
// much still up, most consequentially on vps, where those containers are
// serving real traffic.
//
// The bundle directory itself is deliberately never reused across calls —
// see the comment on State.BundleDir for the Docker Desktop bug this
// sidesteps. Its name only has to be unique on this machine, not
// globally, so a nanosecond timestamp is enough; this CLI never runs two
// configure calls concurrently against the same deployment.
func Prepare() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}

	// 0700, not 0755: versola-tools writes secret candidate material
	// straight into this directory (*.generated-secrets.env -- see
	// resolveServiceSecrets in deploy/secrets.go), sometimes owned by
	// root rather than this process (the tools container runs without
	// --user, so on Linux its writes land as whatever user the image
	// runs as). chmod'ing those individual files after the fact only
	// works when this process actually owns them, which it may not --
	// restricting the directory itself sidesteps that entirely: Linux
	// gates access to a file by whether the *directory* grants
	// read+execute, not by the file's own owner or mode, so nothing
	// outside this process (and root, which bypasses permission checks
	// entirely -- exactly what the tools container needs to write here in
	// the first place) can read anything inside regardless of who ends up
	// owning any individual file.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("couldn't create %s: %w", dir, err)
	}
	// MkdirTemp, not a name from the clock alone: the clock can return the
	// same value twice in a row (on Windows it only advances every
	// millisecond or so), and MkdirAll would then silently reuse an
	// existing bundle. MkdirTemp appends a random suffix and fails rather
	// than reuse. The timestamp stays in the name for readability.
	bundleDir, err := os.MkdirTemp(dir, fmt.Sprintf("bundle-%d-", time.Now().UnixNano()))
	if err != nil {
		return "", fmt.Errorf("couldn't create a bundle directory in %s: %w", dir, err)
	}
	return bundleDir, nil
}

// FinalizeOptions describe the deployment a configure just prepared, for
// Finalize to record.
type FinalizeOptions struct {
	Target, Version string
	// BundleDir is the full path Prepare returned.
	BundleDir string
	AuthURL   string
	ProxyMode string
	// CentralURL is the normalized --central-url, "" for none.
	CentralURL string
	// MTLSDomain, CentralMTLSDomain, CentralDomain: the public names, "" for none.
	MTLSDomain, CentralMTLSDomain, CentralDomain string
	// CertLifetimeDays: see State.CertLifetimeDays.
	CertLifetimeDays *int
	// PostgresHost is --postgres-host (vps), "" for local.
	PostgresHost string
	// PostgresDB is --postgres-db, "" for none.
	PostgresDB string
	// PostgresCAFile is a PEM file to copy into Dir() as PostgresCAFileName
	// and record; "" for none (a copy left by an earlier deployment is
	// removed).
	PostgresCAFile string
	// SecretsRevision is the schema revision secrets were settled against,
	// 0 when the release has no schema.
	SecretsRevision int
	// Keep are further bundles to leave in place (see Finalize).
	Keep []string

	// Slots and PreviousVersion are set by a rolling upgrade preparing its
	// new bundle (see deploy's staged configure): the replicas that keep
	// running, each recorded at the version it runs (otherwise, with
	// Version moved on, they would read as the new one), and the version
	// they run. A plain configure leaves both empty: one replica in slot 1.
	Slots           map[string][]Slot
	PreviousVersion string
	// Surge, MigratedAt and RollingBack are set by an abort preparing the
	// bundle of the version it goes back to (see deploy's stagedConfigure):
	// the stand-in a cut-off rollout left, and the migrations already applied
	// -- an abort never migrates, so the record must not ask for it.
	Surge       map[string]int
	MigratedAt  *time.Time
	RollingBack bool
}

// Finalize makes the deployment Prepare just built (at bundleDir) the
// active one: it writes state.json describing it, then -- only once that
// succeeds -- removes whatever the previous deployment's bundle directory
// was, if any.
//
// Doing the write first is what actually closes the gap described on
// Prepare: from the moment this is called, state.json only ever points at
// a bundle directory that either fully exists (the new one, once Finalize
// returns) or, if removing the old one happens to fail, still fully
// existed a moment ago -- never at a bundle directory that's only
// half-written, and never at nothing.
//
// o.SecretsRevision is the revision of the secrets schema this configure
// settled secrets against, 0 when the release has none. It is recorded as the
// larger of that and the previous record's for the same target: deploying an
// older release must not make secrets that a newer one added look "new" again,
// and a release without a schema says nothing, so the record keeps what it had.
//
// o.BundleDir is the full path Prepare returned; only its base name ends up
// stored (see State.BundleDir's own comment on why). o.Keep names further
// bundles to leave in place: containers this record doesn't track, like
// OpenBao, which outlives the deployments and keeps mounting its
// openbao.hcl from the bundle it was started from.
func Finalize(o FinalizeOptions) error {
	dir, err := Dir()
	if err != nil {
		return err
	}

	// Loaded before state.json is overwritten below, and deliberately
	// tolerant of any error here (including ErrNotConfigured, the common
	// first-deployment case) -- there being no previous deployment to
	// clean up afterward isn't this function's problem to report, just
	// something to skip.
	prev, prevErr := Load()
	// ...except this one: overwriting a newer versola's state is the loss
	// Load refuses to risk, and the bundles it names would be pruned.
	if errors.Is(prevErr, ErrNewerSchema) {
		return prevErr
	}

	target := o.Target
	s := &State{
		SchemaVersion: SchemaVersion,
		Target:        target,
		Version:       o.Version,
		ConfiguredAt:  time.Now().UTC(),
		BundleDir:     filepath.Base(o.BundleDir),
		AuthURL:       o.AuthURL,
		CentralURL:    o.CentralURL,

		MTLSDomain:        o.MTLSDomain,
		CentralMTLSDomain: o.CentralMTLSDomain,
		CentralDomain:     o.CentralDomain,
		CertLifetimeDays:  o.CertLifetimeDays,
		ProxyMode:         o.ProxyMode,
		PostgresHost:      o.PostgresHost,
		PostgresDB:        o.PostgresDB,
		// Set only by a rolling upgrade's preparation (see FinalizeOptions).
		PreviousVersion: o.PreviousVersion,
		MigratedAt:      o.MigratedAt,
		RollingBack:     o.RollingBack,
	}
	for svc, n := range o.Surge {
		if s.Surge == nil {
			s.Surge = map[string]int{}
		}
		s.Surge[svc] = n
	}
	for svc, slots := range o.Slots {
		s.SetSlots(svc, slots)
	}
	// The bundles containers may still mount carry over: they keep using
	// them until a successful `up` has replaced them all. A previous record
	// without MountedBundleDirs (written before that field existed) can't
	// tell whether its bundle is in use, so it's assumed to be -- keeping
	// one extra directory is harmless, deleting a mounted one isn't.
	s.SecretsRevision = o.SecretsRevision
	if prevErr == nil && prev.Target == target && prev.SecretsRevision > s.SecretsRevision {
		s.SecretsRevision = prev.SecretsRevision
	}
	if prevErr == nil {
		s.MountedBundleDirs = prev.MountedBundleDirs
		if prev.MountedBundleDirs == nil && prev.BundleDir != "" {
			s.MountedBundleDirs = []string{prev.BundleDir}
		}
	}
	caPath := filepath.Join(dir, PostgresCAFileName)
	if o.PostgresCAFile != "" {
		ca, err := os.ReadFile(o.PostgresCAFile)
		if err != nil {
			return fmt.Errorf("couldn't read the Postgres CA file: %w", err)
		}
		if err := fsutil.WriteFileAtomic(caPath, ca, 0o644); err != nil {
			return fmt.Errorf("couldn't keep a copy of the Postgres CA file: %w", err)
		}
		s.PostgresCA = PostgresCAFileName
	} else if err := os.Remove(caPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("couldn't remove the previous Postgres CA copy: %w", err)
	}
	if err := s.Save(); err != nil {
		return err
	}
	// The deployment is recorded: the install is no longer unfinished. A failure
	// to remove the note is harmless -- a record for this target outranks it --
	// but is reported, not swallowed.
	// Only this target's: a note for another target is about another install.
	if note, err := LoadPending(); err == nil && note != nil && note.Target == target {
		if err := ClearPending(); err != nil {
			fmt.Printf("(%v -- safe to delete by hand)\n", err)
		}
	}

	// Everything else goes: the previous configure's bundle if it was never
	// started, and any left behind by a configure that failed halfway.
	pruneBundles(dir, append(append([]string{s.BundleDir}, s.MountedBundleDirs...), o.Keep...)...)
	return nil
}

// PostgresCAPath returns the full path of the recorded copy of the Postgres CA
// file, or "" when the deployment has none.
func (s *State) PostgresCAPath() string {
	if s.PostgresCA == "" {
		return ""
	}
	dir, err := Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, s.PostgresCA)
}

// Save writes the state record, replacing whatever was there. It is written
// in this build's layout, whatever layout it was read in: it is this build
// writing the file.
func (s *State) Save() error {
	s.SchemaVersion = SchemaVersion
	dir, err := Dir()
	if err != nil {
		return err
	}
	// MarshalIndent, not Marshal: this file is small, is read by people
	// when something has gone wrong, and gets diffed in bug reports.
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("couldn't encode deployment state: %w", err)
	}
	b = append(b, '\n')
	path := filepath.Join(dir, stateFileName)

	// Written to a temp file and renamed into place rather than a direct
	// os.WriteFile -- WriteFile truncates the destination before writing
	// a single byte of the new content, so a failure partway through (a
	// full disk, most plausibly) would leave state.json empty or
	// half-written instead of unchanged. That defeats the exact thing
	// Finalize calls this for: Configure's earlier steps having failed
	// must never cost this machine its record of whatever deployment was
	// still running before this call started. Rename is atomic on both
	// POSIX and Windows (as long as the temp file is on the same volume,
	// which it is here -- same directory), so this can only ever leave
	// the OLD state.json in place or the fully-written NEW one, never
	// something in between.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("couldn't write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("couldn't finalize %s: %w", path, err)
	}
	return nil
}

// Load reads the current deployment's state.
//
// It returns ErrNotConfigured if nothing has been deployed yet, so
// callers can tell "nothing here" apart from "something here but
// unreadable" — the second is a real problem worth reporting, the first
// usually just means printing a hint about which command to run first.
// Match it with errors.Is rather than ==, so that a future caller that
// wraps it keeps working.
func Load() (*State, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}

	b, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, notConfigured(dir)
		}
		return nil, fmt.Errorf("couldn't read deployment state: %w", err)
	}

	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("couldn't parse %s: %w", filepath.Join(dir, stateFileName), err)
	}
	// A newer versola may have recorded things this one does not know, and
	// saving would drop them (replica slots, say).
	if s.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("%s: %w (state layout %d, this one reads up to %d) -- upgrade versola", filepath.Join(dir, stateFileName), ErrNewerSchema, s.SchemaVersion, SchemaVersion)
	}
	return &s, nil
}

// notConfigured is what a missing state.json means: nothing deployed, or a
// deployment by the first builds of this CLI, which recorded only a bare
// version file. Those are not read any more -- configure replaces them.
func notConfigured(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, oldVersionFileName)); err == nil {
		return ErrOldInstall
	}
	return ErrNotConfigured
}

// ComposeFile returns the path to the compose file configure generates,
// and whether it currently exists. It existing is how status/down/
// uninstall tell whether anything has been deployed yet.
//
// Loads state itself -- fine for callers (status/down/uninstall) that don't
// otherwise need the state record. A caller that already has one loaded
// (e.g. to also read st.Target/st.Version, or specifically to avoid a second
// Load racing against a concurrent Finalize -- see cmd/confirm.go's own
// comment) should call st.ComposeFilePath() directly instead.
func ComposeFile() (path string, exists bool, err error) {
	s, err := Load()
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			// Nothing recorded at all -- same as "the file isn't there"
			// from the caller's point of view, not a real error.
			return "", false, nil
		}
		return "", false, err
	}
	return s.ComposeFilePath()
}

// ComposeFilePath is ComposeFile's own logic, applied to an already-loaded
// state record rather than loading its own -- see ComposeFile's comment on
// when to prefer this.
//
// The path is resolved through the state record (see State.BundleDir)
// rather than assumed to sit directly in Dir(), since a deployment's
// configs no longer necessarily live at a fixed location.
func (s *State) ComposeFilePath() (path string, exists bool, err error) {
	bundleDir, err := s.bundlePath()
	if err != nil {
		return "", false, err
	}
	path = filepath.Join(bundleDir, "compose.yml")
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return path, false, nil
		}
		// Something other than "doesn't exist" -- e.g. a permissions
		// error -- shouldn't be silently treated as "nothing deployed";
		// every caller (status, down, uninstall) already checks this
		// error and would otherwise skip real work based on a false
		// negative here.
		return path, false, fmt.Errorf("couldn't check %s: %w", path, statErr)
	}
	return path, true, nil
}

// ReplicasFile is written into the deployment bundle next to compose.yml
// by `up` when auth or edge have replicas beyond slot 1, and passed with it
// from then on (ComposeArgs).
const ReplicasFile = "replicas.yml"

// OverridesFile is written into the deployment bundle by `up` when the
// CLI changes a service of compose.yml itself -- auth's APORT/MPORT on
// vps, moved behind the reverse proxy -- and passed right after compose.yml
// (ComposeArgs). It is not part of what the CLI reads the ports from
// (TopologyComposeArgs): those are the compose file's own.
const OverridesFile = "overrides.yml"

// ComposeArgs builds the arguments for a `docker compose` call against the
// deployment whose compose.yml is at composePath: "compose -f
// compose.yml", plus overrides.yml, replicas.yml and proxy.yml when each
// is there (proxy.yml when versola-cli generated a reverse proxy next to
// it -- vps and local), then rest.
//
// Every compose call goes through this, so the proxy is always part of
// the same compose project as auth/edge/central (proxy.yml declares the
// same project name): `down` stops it too instead of leaving it holding
// 80/443, `status` lists it, `down --volumes` treats it like the rest.
func ComposeArgs(composePath string, rest ...string) []string {
	return composeArgs(composePath, true, rest...)
}

// TopologyComposeArgs is ComposeArgs without OverridesFile: the compose
// configuration as versola-tools wrote it, which is where the CLI reads the
// ports it then moves (an override read back as the base would be moved
// again on every `up`).
func TopologyComposeArgs(composePath string, rest ...string) []string {
	return composeArgs(composePath, false, rest...)
}

func composeArgs(composePath string, overrides bool, rest ...string) []string {
	args := []string{"compose", "-f", composePath}
	dir := filepath.Dir(composePath)
	files := []string{ReplicasFile, proxy.ComposeFile}
	if overrides {
		files = append([]string{OverridesFile}, files...)
	}
	// The replicas of auth and edge beyond the first (see ReplicasFile) go
	// before the proxy's file, which names no service of theirs.
	for _, f := range files {
		path := filepath.Join(dir, f)
		if _, err := os.Stat(path); err == nil {
			args = append(args, "-f", path)
		}
	}
	return append(args, rest...)
}

// MarkStarting records, before `up` starts any container, that containers
// may from now on mount the current bundle -- so a `configure` after an
// `up` that failed halfway doesn't delete files those containers use.
//
// Both MarkStarting and MarkRunning re-read state.json rather than
// trusting a caller's copy, so only this one field changes (same
// reasoning as the migrate step's own update).
func MarkStarting() error {
	s, err := Load()
	if err != nil {
		return err
	}
	if s.BundleDir == "" {
		return ErrOldInstall
	}
	for _, d := range s.MountedBundleDirs {
		if d == s.BundleDir {
			return nil
		}
	}
	s.MountedBundleDirs = append(s.MountedBundleDirs, s.BundleDir)
	return s.Save()
}

// MarkRunning records that everything now runs from the current bundle --
// called by `up` once the stack is up -- and removes every other bundle
// except those in keep: bundles still mounted by containers `up` doesn't
// manage (OpenBao -- see Finalize).
func MarkRunning(keep ...string) error {
	s, err := Load()
	if err != nil {
		return err
	}
	if s.BundleDir == "" {
		return ErrOldInstall
	}
	s.MountedBundleDirs = []string{s.BundleDir}
	if err := s.Save(); err != nil {
		return err
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	pruneBundles(dir, append([]string{s.BundleDir}, keep...)...)
	return nil
}

// RestorePrevious makes an earlier bundle the active deployment again: what
// `versola rollout --abort` does when the rollout it cancels has not started
// any container from its new bundle yet (see deploy's abort). bundle is the
// earlier bundle's directory name; it must still be there. The record is
// rewritten in place -- version, bundle, the slots at that version, no
// rollout under way -- and every other bundle except those in keep goes.
// migratedAt is recorded as the time of the last migration: the schema is at
// least where that version needs it (the cancelled rollout may have migrated
// it further; migrations are not rolled back).
func RestorePrevious(bundle string, migratedAt time.Time, keep ...string) error {
	s, err := Load()
	if err != nil {
		return err
	}
	if s.PreviousVersion == "" || len(s.Surge) > 0 {
		return fmt.Errorf("no rollout to cancel this way is recorded")
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, bundle, "compose.yml")); err != nil {
		return fmt.Errorf("the previous deployment's files (%s) are not there any more: %w", bundle, err)
	}
	prev := s.PreviousVersion
	slots := map[string][]Slot{}
	for _, svc := range []string{"central", "auth", "edge"} {
		for _, sl := range s.ActiveSlots(svc) {
			slots[svc] = append(slots[svc], Slot{N: sl.N, Version: prev})
		}
	}
	s.Version, s.PreviousVersion, s.RollingBack = prev, "", false
	s.BundleDir = bundle
	s.MountedBundleDirs = []string{bundle}
	s.MigratedAt = &migratedAt
	s.ConfiguredAt = time.Now().UTC()
	s.Slots = nil
	for svc, ss := range slots {
		s.SetSlots(svc, ss)
	}
	if err := s.Save(); err != nil {
		return err
	}
	pruneBundles(dir, append([]string{bundle}, keep...)...)
	return nil
}

// BeginRollback records the start of an abort that goes back by the engine,
// without configuring the previous version again: bundle, the earlier
// bundle (it must still be there), becomes the active one, and the record
// says the rollout is being undone -- Version is what was PreviousVersion
// and the other way round, RollingBack is set. The replicas that never left
// the old version already mount that bundle, so the engine leaves them alone
// and recreates only the ones that moved.
//
// Slots keep the versions they are recorded at (the engine corrects a record
// from the containers), a recorded stand-in stays (it is retired). The
// bundle that moved replicas mount is kept until they are replaced.
// migratedAt is recorded as the time of the last migration: an abort never
// migrates, and the schema is at least where the old version needs it.
func BeginRollback(bundle string, migratedAt time.Time, keep ...string) error {
	s, err := Load()
	if err != nil {
		return err
	}
	if s.PreviousVersion == "" || s.RollingBack {
		return fmt.Errorf("no rollout to turn round is recorded")
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, bundle, "compose.yml")); err != nil {
		return fmt.Errorf("the previous deployment's files (%s) are not there any more: %w", bundle, err)
	}
	slots := map[string][]Slot{}
	for _, svc := range []string{"central", "auth", "edge"} {
		slots[svc] = s.ActiveSlots(svc)
	}
	mounted := []string{bundle}
	for _, d := range append(append([]string{}, s.MountedBundleDirs...), s.BundleDir) {
		if d != "" && !slices.Contains(mounted, d) {
			mounted = append(mounted, d)
		}
	}
	s.Version, s.PreviousVersion, s.RollingBack = s.PreviousVersion, s.Version, true
	s.BundleDir = bundle
	s.MountedBundleDirs = mounted
	s.MigratedAt = &migratedAt
	s.ConfiguredAt = time.Now().UTC()
	s.Slots = nil
	for svc, ss := range slots {
		s.SetSlots(svc, ss)
	}
	if err := s.Save(); err != nil {
		return err
	}
	pruneBundles(dir, append(append([]string{}, mounted...), keep...)...)
	return nil
}

// pruneBundles removes every bundle-* directory in dir except those named
// in keep. Only bundle-* directories: dir also holds state.json, the lock
// and the install note, which are not a deployment's files. Failures
// are reported, not fatal -- a leftover directory is only disk space.
func pruneBundles(dir string, keep ...string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	kept := map[string]bool{}
	for _, k := range keep {
		if k != "" {
			kept[k] = true
		}
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "bundle-") || kept[e.Name()] {
			continue
		}
		old := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(old); err != nil {
			fmt.Printf("(couldn't remove a previous deployment's files at %s: %v — safe to delete by hand)\n", old, err)
		}
	}
}

// CertsUnlimited reports whether the deployment's mutual-TLS certificates were
// configured with no expiry (--cert-lifetime-days 0): nothing renews them, so
// there is no timer to install and nothing to warn about.
func (s *State) CertsUnlimited() bool {
	return s != nil && s.CertLifetimeDays != nil && *s.CertLifetimeDays == 0
}
