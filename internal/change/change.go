// Package change keeps the hub's changes: durable, numbered proposals to add
// the commits of a source branch to a target branch, each with the versions
// its source branch had.
//
// A change belongs to the workspace that holds its target branch and is
// numbered there from 1, a number never given twice. Every version's head
// stays fetchable at refs/parley/changes/<number>/<version> in that
// workspace's repository, which the component writes itself.
//
// The component's operations that write, such as Open, run in a store
// transaction the caller owns and neither begin nor commit one, so that a
// change or a version is stored with the ref update that caused it or not
// at all. What other features compute about a change (its stack, its
// landing, its sessions, the auto_land guard, a fork's activity) reaches it
// through the collaborators registered on the component instance
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
package change

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
)

// The states of a change.
const (
	StateDraft     = "draft"
	StateOpen      = "open"
	StateLanded    = "landed"
	StateAbandoned = "abandoned"
)

// The kinds of a version: what moved the source branch to its head.
// KindRef is also the kind of a version recorded for a head that was
// already on the source branch, such as version 1 of a change opened
// through the REST route.
const (
	KindPush    = "push"
	KindCommit  = "commit"
	KindRef     = "ref"
	KindRebase  = "rebase"
	KindRestack = "restack"
	KindRevert  = "revert"
	KindSync    = "sync"
)

// The values of a change's mergeable.
const (
	MergeableClean    = "clean"
	MergeableConflict = "conflict"
	MergeableUnknown  = "unknown"
)

// The reasons of a change's needs_restack.
const (
	// RestackTooDeep is the reason of a change that sits deeper in its
	// stack than [changes].max_stack_depth.
	RestackTooDeep = "stack_too_deep"
	// RestackConflict is the reason of a change whose restack conflicted.
	RestackConflict = "conflict"
	// RestackAutoOff is the reason of a change that a workspace with
	// changes.auto_restack off does not restack.
	RestackAutoOff = "auto_restack_off"
)

// DefaultMaxStackDepth is the deepest a change may sit in its stack when
// Deps names no limit.
const DefaultMaxStackDepth = 20

// The statuses of a change's landing.
const (
	LandingReady   = "ready"
	LandingPending = "pending"
	LandingBlocked = "blocked"
)

// The credential types of a caller, as the events and the author name
// them; ActorSystem is the hub itself, which has no credential.
const (
	CredentialAdminToken = "admin_token"
	CredentialAPIKey     = "api_key"
	CredentialPAT        = "pat"
	ActorSystem          = "system"
)

// MaxPaths is the number of touched files a change lists in paths; it
// sets paths_truncated when its version touches more.
const MaxPaths = 2000

// Limits of a title and an intent: a default taken from a commit or a fork
// is cut to them.
const (
	MaxTitle  = 200
	MaxIntent = 16000
)

// VersionRef is the ref that keeps the head of the version of the change
// number in its workspace's repository.
func VersionRef(number, version int64) string {
	return "refs/parley/changes/" + strconv.FormatInt(number, 10) + "/" + strconv.FormatInt(version, 10)
}

// Ref is the reference of a change as people write it, <workspace>#<number>.
func Ref(workspace string, number int64) string {
	return workspace + "#" + strconv.FormatInt(number, 10)
}

// ErrChangeNotFound is the answer for a change that does not exist, 404
// change_not_found; match it with errors.Is.
var ErrChangeNotFound = server.NewError("change_not_found", http.StatusNotFound, "no such change")

// errChangeNotFound is ErrChangeNotFound naming the change, which still
// matches it.
func errChangeNotFound(workspace string, number int64) error {
	return &notFound{server.NewError("change_not_found", http.StatusNotFound, "no change "+Ref(workspace, number))}
}

// notFound is a change_not_found error that matches ErrChangeNotFound and
// is answered as the API error it wraps.
type notFound struct{ err *server.Error }

func (e *notFound) Error() string { return e.err.Error() }

func (e *notFound) Is(target error) bool { return target == ErrChangeNotFound }

func (e *notFound) Unwrap() error { return e.err }

// Credential is the credential a caller used: its type (CredentialAdminToken,
// CredentialAPIKey or CredentialPAT) and its id (the key id or the token id;
// "admin" for the admin token). The zero value is none, the hub's own.
type Credential struct {
	Type string
	ID   string
}

// Caller is who asks for a change or a version: the user (none for the
// admin token and the hub), the credential, the path globs a path-scoped
// token is limited to (nil for none), and the workspace a bound PAT is
// bound to ("" for every other credential).
type Caller struct {
	UserID, Username string
	Credential       Credential
	Paths            []string
	Bound            string
}

// CallerOf is the caller an authenticated principal is.
func CallerOf(p identity.Principal) Caller {
	c := Caller{Credential: Credential{Type: p.Type, ID: p.CredentialID}, Paths: p.Paths}
	if p.Type == identity.TypeAdmin {
		c.Credential.Type = CredentialAdminToken
	}
	if p.User != nil {
		c.UserID, c.Username = p.User.ID, p.User.Username
	}
	if p.Workspace != nil {
		c.Bound = *p.Workspace
	}
	return c
}

// actor is the actor the caller is, as a change's author and a version's
// created_by name it.
func (c Caller) actor() Actor {
	a := Actor{ActorType: c.Credential.Type}
	if a.ActorType == "" {
		a.ActorType = ActorSystem
	}
	if c.UserID != "" {
		a.ActorID = &c.UserID
	}
	if c.Username != "" {
		a.Username = &c.Username
	}
	return a
}

// Actor is who did something: the credential type (or system), the user's
// id and username, both null for the admin token and the hub.
type Actor struct {
	ActorType string  `json:"actor_type"`
	ActorID   *string `json:"actor_id"`
	Username  *string `json:"username"`
}

// Source is the branch a change proposes, in its workspace.
type Source struct {
	WorkspaceSlug string `json:"workspace_slug"`
	Branch        string `json:"branch"`
}

// Stats are the numbers of a version against its base.
type Stats struct {
	Commits      int `json:"commits"`
	FilesChanged int `json:"files_changed"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
}

// Landing says whether a change may land: its status, the reason it is
// blocked (null otherwise) and the requirements of the workspace's policy,
// each a JSON object the landing evaluator writes.
type Landing struct {
	Status       string            `json:"status"`
	Reason       *string           `json:"reason"`
	Requirements []json.RawMessage `json:"requirements"`
}

// MarshalJSON writes the landing with requirements never null.
func (l Landing) MarshalJSON() ([]byte, error) {
	type plain Landing
	if l.Requirements == nil {
		l.Requirements = []json.RawMessage{}
	}
	return json.Marshal(plain(l))
}

// readyLanding is the landing of an open change without an evaluator.
func readyLanding() Landing { return Landing{Status: LandingReady, Requirements: []json.RawMessage{}} }

// blockedLanding is the landing blocked for reason.
func blockedLanding(reason string) Landing {
	return Landing{Status: LandingBlocked, Reason: &reason, Requirements: []json.RawMessage{}}
}

// Landed is how a change landed: the commit its landing made the target's
// head, when, by whom, the landing's id, and each commit of the change's
// version with the commit it landed as.
type Landed struct {
	SHA     string         `json:"sha"`
	At      time.Time      `json:"at"`
	By      Actor          `json:"by"`
	LandID  string         `json:"land_id"`
	Commits []LandedCommit `json:"commits"`
}

// MarshalJSON writes the time as every response does and commits never
// null.
func (l Landed) MarshalJSON() ([]byte, error) {
	type plain Landed
	out := struct {
		plain
		At timefmt.Time `json:"at"`
	}{plain(l), timefmt.Time(l.At)}
	if out.Commits == nil {
		out.Commits = []LandedCommit{}
	}
	return json.Marshal(out)
}

// LandedCommit is a commit of a landed version and the commit it landed as.
type LandedCommit struct {
	VersionSHA string `json:"version_sha"`
	LandedSHA  string `json:"landed_sha"`
}

// Change is a change as GET …/changes/:number shows it.
type Change struct {
	WorkspaceSlug  string          `json:"workspace_slug"`
	Number         int64           `json:"number"`
	Ref            string          `json:"ref"`
	Title          string          `json:"title"`
	Intent         string          `json:"intent"`
	State          string          `json:"state"`
	TargetBranch   string          `json:"target_branch"`
	Source         Source          `json:"source"`
	Version        int64           `json:"version"`
	HeadSHA        string          `json:"head_sha"`
	BaseSHA        *string         `json:"base_sha"`
	DependsOn      *int64          `json:"depends_on"`
	Dependants     []int64         `json:"dependants"`
	Author         Actor           `json:"author"`
	SessionID      *string         `json:"session_id"`
	Sessions       []string        `json:"sessions"`
	Mergeable      string          `json:"mergeable"`
	Conflict       json.RawMessage `json:"conflict"`
	NeedsRestack   bool            `json:"needs_restack"`
	RestackReason  *string         `json:"restack_reason"`
	Stats          Stats           `json:"stats"`
	Paths          []string        `json:"paths"`
	PathsTruncated bool            `json:"paths_truncated"`
	Landing        Landing         `json:"landing"`
	AutoLand       bool            `json:"auto_land"`
	Labels         []string        `json:"labels"`
	Landed         *Landed         `json:"landed"`
	Reverts        *int64          `json:"reverts"`
	RevertedBy     *int64          `json:"reverted_by"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`

	// abandonReason is why an abandoned change was abandoned, "" otherwise.
	abandonReason string
}

// MarshalJSON writes the change object: every member, arrays never null,
// times in whole seconds UTC.
func (c Change) MarshalJSON() ([]byte, error) {
	type plain Change
	out := struct {
		plain
		CreatedAt timefmt.Time `json:"created_at"`
		UpdatedAt timefmt.Time `json:"updated_at"`
	}{plain(c), timefmt.Time(c.CreatedAt), timefmt.Time(c.UpdatedAt)}
	if out.Dependants == nil {
		out.Dependants = []int64{}
	}
	if out.Sessions == nil {
		out.Sessions = []string{}
	}
	if out.Paths == nil {
		out.Paths = []string{}
	}
	if out.Labels == nil {
		out.Labels = []string{}
	}
	if len(out.Conflict) == 0 {
		out.Conflict = nil
	}
	return json.Marshal(out)
}

// Version is one head of a change's source branch, as the version object
// shows it. The creating credential and the patch id are kept with it and
// not shown.
type Version struct {
	Number            int64     `json:"number"`
	HeadSHA           string    `json:"head_sha"`
	BaseSHA           *string   `json:"base_sha"`
	Kind              string    `json:"kind"`
	PatchEquivalentTo *int64    `json:"patch_equivalent_to"`
	CreatedBy         Actor     `json:"created_by"`
	SessionID         *string   `json:"session_id"`
	Stats             Stats     `json:"stats"`
	CreatedAt         time.Time `json:"created_at"`

	// Credential is the credential that created the version, none for a
	// version the hub created.
	Credential Credential `json:"-"`
	// PatchID is the patch id of the version against its base, the same
	// for two versions exactly when their interdiff changes no file.
	PatchID string `json:"-"`
}

// MarshalJSON writes the version object with its time in whole seconds UTC.
func (v Version) MarshalJSON() ([]byte, error) {
	type plain Version
	return json.Marshal(struct {
		plain
		CreatedAt timefmt.Time `json:"created_at"`
	}{plain(v), timefmt.Time(v.CreatedAt)})
}

// Deps are what the component needs. Store, Repos, Workspaces and Access
// are required; every other member has a default.
type Deps struct {
	// Store is the hub's store: the route opens its transaction there and
	// a read without the caller's transaction reads it.
	Store *store.Store
	// Repos opens the repository of the workspace slug.
	Repos func(ctx context.Context, slug string) (*gitrepo.Repo, error)
	// Workspaces reads the workspace records.
	Workspaces WorkspaceReader
	// Access is the access rule, which Authorize asks.
	Access AccessRule
	// Events takes the change events, in the caller's transaction; nil
	// discards them.
	Events EventWriter
	// Now is the clock of the stored times; nil is time.Now.
	Now func() time.Time
	// Logger is told of what fails and changes nothing, such as a fork
	// activity recorder's error; nil discards it.
	Logger *slog.Logger
	// Attribute names, in a conflict report of the workspace, the landed
	// changes the conflict collided with; nil names none, and the report's
	// lists stay empty.
	Attribute func(ctx context.Context, workspace string, report *gitrepo.ConflictReport) error
	// MergeableDebounce is the shortest time between the starts of two
	// mergeability checks of one change; zero, or below, for none.
	MergeableDebounce time.Duration
	// MaxStackDepth is the deepest a change may sit in its stack, itself
	// counted, before it is flagged stack_too_deep; zero, or below, is
	// DefaultMaxStackDepth.
	MaxStackDepth int
}

// Component keeps the changes. Its zero value is ready to be registered and
// to take collaborators; it serves once Bind has given it its dependencies,
// before the first request.
type Component struct {
	deps Deps

	mu        sync.RWMutex
	sessions  SessionLinker
	stack     StackLinker
	evaluator LandingEvaluator
	observers []VersionObserver
	guard     AutoLandGuard
	activity  ForkActivity
	restacker Restacker
	// versionGuard judges the version of a manual restack.
	versionGuard VersionGuard
	expanders    map[string]Expander

	// merge holds the scheduled mergeability checks.
	merge    mergeWorker
	restacks restackWorker
}

// Bind gives the component its dependencies, the defaults in place of the
// members d leaves nil.
func (c *Component) Bind(d Deps) {
	if d.Events == nil {
		d.Events = discardEvents{}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.MaxStackDepth <= 0 {
		d.MaxStackDepth = DefaultMaxStackDepth
	}
	c.deps = d
}
