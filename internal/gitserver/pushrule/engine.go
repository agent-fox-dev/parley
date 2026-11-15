// Package pushrule decides whether each ref update of a write to a
// workspace is allowed, by one ordered set of rules that every surface that
// writes refs asks with the same input: the git server for a push, the
// source control API for a commit. The surface resolves who the caller is
// and what the workspace is, and hands both over as plain facts; the engine
// answers one decision per update and changes nothing while it does.
package pushrule

import (
	"context"
	"errors"
	"sync"

	"github.com/agent-fox-dev/parley/internal/access"
)

// ZeroID is the all-zero object id: the before of an update that creates
// its ref, and the after of one that deletes it.
const ZeroID = "0000000000000000000000000000000000000000"

// The kinds of credential a caller writes with, as Caller.CredentialType
// holds them.
const (
	CredentialAdminToken = "admin_token"
	CredentialAPIKey     = "api_key"
	CredentialPAT        = "pat"
)

// The error types of a refused update, as Decision.ErrorType holds them:
// one per rule, and ErrorTypeInternal for an update no rule could be
// evaluated for.
const (
	ErrorTypeHubOwnedRef       = "hub_owned_ref"
	ErrorTypeProtectedBranch   = "protected_branch"
	ErrorTypeDefaultBranch     = "default_branch"
	ErrorTypeTrackingBranch    = "tracking_branch"
	ErrorTypeIntegrationBranch = "integration_branch"
	ErrorTypePatchBranch       = "patch_branch"
	ErrorTypePathNotAllowed    = "path_not_allowed"
	ErrorTypeInternal          = "internal"
)

// Update is one ref update of a request: Ref, a full ref name, moves from
// Before to After. A create has Before ZeroID, a delete After ZeroID.
// OpensChange is set when the update opens a change, which decides what a
// path-scoped token is held to.
type Update struct {
	Ref, Before, After string
	OpensChange        bool
}

// Workspace is what the rules need to know of the workspace written to.
// Kind is access.KindRepository or access.KindFork; Parent is a fork's
// parent slug and empty otherwise; DefaultBranch is the branch every clone
// checks out (a fork's base branch), empty when there is none; Mode is
// workspace.ModeStandard or workspace.ModeCarryPatch.
type Workspace struct {
	Slug, Kind, Parent, DefaultBranch, Mode string
}

// Caller is what the rules need to know of who writes. Level is the
// caller's access level on the workspace; CredentialType is one of the
// Credential constants, with the credential's id, and UserID is empty for
// the admin token. Bound is set for a token confined to one workspace.
// Paths are a path-scoped token's globs; nil means the caller is held to no
// paths.
type Caller struct {
	Level                                access.Level
	CredentialType, CredentialID, UserID string
	Bound                                bool
	Paths                                []string
}

// View answers the questions the rules ask of the repository written to,
// including about objects a push holds in quarantine. *gitrepo.Repo
// satisfies it; open it with the quarantine as an alternate object
// directory to have it see those objects.
type View interface {
	// IsAncestor reports whether the commit ancestor is reachable from the
	// commit descendant.
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)
	// NewCommits lists the commits reachable from tips, commits or tags
	// that lead to one, and from no ref of the repository nor, when
	// parentDir is not "", of the repository at that git directory, oldest
	// first with every commit after its parents.
	NewCommits(ctx context.Context, tips []string, parentDir string) ([]string, error)
	// ChangedPaths lists in byte order the paths commit changes against its
	// first parent, every path of its tree for a root commit, and both
	// paths of a rename.
	ChangedPaths(ctx context.Context, commit string) ([]string, error)
}

// Request is one write to evaluate: the updates, in the order the caller
// sent them, of the workspace by the caller, asked of the repository Repo.
// ParentRepo is the git directory of a fork's parent, whose commits are not
// new to the fork, and empty for a workspace that is no fork.
type Request struct {
	Workspace  Workspace
	Caller     Caller
	Updates    []Update
	Repo       View
	ParentRepo string
}

// Settings are the workspace's effective settings that decide how its
// branches may move: the protected_branches globs and protect.owner_push.
// A fork's are its parent's.
type Settings struct {
	ProtectedBranches []string
	OwnerPush         bool
}

// SettingsReader reads a workspace's effective push settings. An error
// refuses every update a rule needs them for.
type SettingsReader interface {
	PushSettings(ctx context.Context, ws Workspace) (Settings, error)
}

// Tracking reports whether branch is a tracking branch of the fork
// forkSlug: one that existed in the parent when the fork was made and is
// read-only in the fork. It is asked about forks only.
type Tracking interface {
	IsTracking(ctx context.Context, forkSlug, branch string) (bool, error)
}

// EngineDeps are the lookups the engine asks while it evaluates. Settings
// is the settings reader; nil means every workspace has the settings'
// defaults, which protect its default branch and allow direct pushes.
// Tracking is the tracking-branch lookup; nil means no fork has tracking
// branches. VersionFiles is the version-files lookup; nil means no update
// without new commits makes a version.
type EngineDeps struct {
	Settings     SettingsReader
	Tracking     Tracking
	VersionFiles VersionFiles
}

// Decision is the engine's answer for one update. An allowed update says
// whether it creates its ref, deletes it, moves it to a commit its old
// value is not an ancestor of (Forced), or is a direct push to a protected
// branch, and carries in Forward the forward to origin the carry-patch
// controller made it wait on, nil when there is none. A refused one carries
// the refusing rule's Message and ErrorType, and Path when the rule refused
// a file.
type Decision struct {
	Update
	Allowed, Created, Deleted, Forced, Direct bool
	Message, ErrorType, Path                  string
	Forward                                   Forward
}

// Verdict is the engine's answer for a request: one decision per update,
// in the order of the request's updates.
type Verdict struct {
	Decisions []Decision
}

// Refused reports whether any update of the request is refused, which
// refuses the whole request.
func (v Verdict) Refused() bool {
	for _, d := range v.Decisions {
		if !d.Allowed {
			return true
		}
	}
	return false
}

// Engine is the push rule engine. Its zero value is ready to evaluate with
// no lookups; Bind hands it the hub's, and RegisterController the
// carry-patch controller of the feature that owns it.
type Engine struct {
	mu         sync.RWMutex
	deps       EngineDeps
	controller Controller
}

// Bind sets the lookups the engine asks from then on.
func (e *Engine) Bind(d EngineDeps) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deps = d
}

// Evaluate decides every update of req before anything is written, and
// writes nothing itself: no ref, no event, no forward to origin; a forward
// the carry-patch controller asks for rides on the allowed decision. A rule
// that cannot be evaluated refuses its update with ErrorTypeInternal, so
// Evaluate has no error of its own. The rule for path-scoped tokens, last
// in the order, looks at the commits of the request as a whole, and runs
// only once every update passed the rules before it.
func (e *Engine) Evaluate(ctx context.Context, req Request) Verdict {
	e.mu.RLock()
	ev := &evaluation{deps: e.deps, controller: e.controller, req: req}
	e.mu.RUnlock()
	v := Verdict{Decisions: make([]Decision, 0, len(req.Updates))}
	for _, u := range req.Updates {
		v.Decisions = append(v.Decisions, ev.decide(ctx, u))
	}
	if !v.Refused() {
		ev.pathScoped(ctx, v.Decisions)
	}
	return v
}

// evaluation is one request under evaluation, with the lookups it asks.
// The workspace's settings are read once, by the first rule that needs
// them; cur is what the rules learnt about the update being decided.
type evaluation struct {
	deps       EngineDeps
	controller Controller
	req        Request

	settingsRead bool
	settings     Settings
	settingsErr  error

	cur current
}

// current is what the rules learnt about one update while deciding it.
type current struct {
	direct              bool    // the owner exemption let it pass the protected-branch rule
	forward             Forward // the carry-patch controller made it wait on a forward to origin
	forcedKnown, forced bool
}

// pushSettings are the effective push settings of the request's workspace.
func (ev *evaluation) pushSettings(ctx context.Context) (Settings, error) {
	if !ev.settingsRead {
		ev.settingsRead = true
		if ev.deps.Settings == nil {
			ev.settings = defaultSettings(ev.req.Workspace)
		} else {
			ev.settings, ev.settingsErr = ev.deps.Settings.PushSettings(ctx, ev.req.Workspace)
		}
	}
	return ev.settings, ev.settingsErr
}

// errNoView is the failure of an ancestry question for a request that came
// without a repository to ask.
var errNoView = errors.New("no repository view to ask")

// decide runs the per-update rules over u in order, the first refusal
// deciding, and allows u when none refuses.
func (ev *evaluation) decide(ctx context.Context, u Update) Decision {
	ev.cur = current{}
	for _, r := range order {
		if no := r(ctx, ev, u); no != nil {
			return no.decision(u)
		}
	}
	d := Decision{Update: u, Allowed: true, Created: u.Before == ZeroID, Deleted: u.After == ZeroID, Direct: ev.cur.direct, Forward: ev.cur.forward}
	if !d.Created && !d.Deleted {
		forced, err := ev.forced(ctx, u)
		if err != nil {
			// An update whose kind is unknown is not allowed blind.
			return internal.decision(u)
		}
		d.Forced = forced
	}
	return d
}

// forced reports whether u, the update being decided, which neither
// creates nor deletes its ref, moves it to a commit its old value is not an
// ancestor of. The answer is kept for the rest of the update's decision, so
// git is asked once however many rules need it.
func (ev *evaluation) forced(ctx context.Context, u Update) (bool, error) {
	if ev.cur.forcedKnown {
		return ev.cur.forced, nil
	}
	if ev.req.Repo == nil {
		return false, errNoView
	}
	ancestor, err := ev.req.Repo.IsAncestor(ctx, u.Before, u.After)
	if err != nil {
		return false, err
	}
	ev.cur.forcedKnown, ev.cur.forced = true, !ancestor
	return ev.cur.forced, nil
}
