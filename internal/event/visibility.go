package event

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Caller is who a visibility decision is made for: the admin token, or a
// user acting by an API key or a PAT, which may be bound to a workspace.
type Caller struct {
	// Admin marks the admin token, which sees every event.
	Admin bool
	// UserID is the user the credential acts for; "" for the admin token.
	UserID string
	// CredentialID is "admin", the key_id or the token_id.
	CredentialID string
	// Workspace is the slug a workspace-bound PAT is bound to; "" for every
	// other credential.
	Workspace string
}

// Bound reports whether the caller is a workspace-bound PAT.
func (c Caller) Bound() bool { return c.Workspace != "" }

// CallerOf returns the caller the pipeline authenticated for r. On a public
// route, where nobody is authenticated, it is the zero Caller, who sees no
// event.
func CallerOf(r *http.Request) Caller {
	sc, ok := server.CallerOf(r.Context())
	if !ok {
		return Caller{}
	}
	c := Caller{Admin: sc.Admin, UserID: sc.UserID, CredentialID: sc.CredentialID}
	if p, ok := identity.PrincipalOf(r.Context()); ok && p.Workspace != nil {
		c.Workspace = *p.Workspace
	}
	return c
}

// accessCaller is the access rule's caller for c.
func (c Caller) accessCaller() access.Caller {
	return access.Caller{Admin: c.Admin, UserID: c.UserID}
}

// Access is what the visibility decision asks the hub's access rule, an
// *access.Service: the caller's readable set, and the caller's level on one
// workspace.
type Access interface {
	Readable(ctx context.Context, c access.Caller) (access.ReadableSet, error)
	Level(ctx context.Context, c access.Caller, slug string) (access.Level, access.Record, bool, error)
}

// Coverage is the covered set of workspace-bound tokens, which the package
// that binds tokens owns and installs with SetCoverage.
type Coverage interface {
	// Covers reports whether the binding of the bound caller c covers the
	// workspace slug.
	Covers(ctx context.Context, c Caller, slug string) (bool, error)
}

// CoverageFunc adapts a function to Coverage.
type CoverageFunc func(ctx context.Context, c Caller, slug string) (bool, error)

// Covers calls f.
func (f CoverageFunc) Covers(ctx context.Context, c Caller, slug string) (bool, error) {
	return f(ctx, c, slug)
}

// ChangeSources is the change lookup, which the package that owns changes
// installs with SetChangeSources.
type ChangeSources interface {
	// ChangeSource returns the slug of the source workspace of the change
	// number in the workspace slug, "" when there is no such change.
	ChangeSource(ctx context.Context, workspace string, number int64) (string, error)
}

// ChangeSourcesFunc adapts a function to ChangeSources.
type ChangeSourcesFunc func(ctx context.Context, workspace string, number int64) (string, error)

// ChangeSource calls f.
func (f ChangeSourcesFunc) ChangeSource(ctx context.Context, workspace string, number int64) (string, error) {
	return f(ctx, workspace, number)
}

// injected are the visibility decision's sources that packages built after
// this one install: they are kept apart from Deps so that Bind, which runs
// in a start hook, does not reset what a part installed at composition.
type injected struct {
	mu       sync.RWMutex
	coverage Coverage
	sources  ChangeSources
}

// SetCoverage installs the covered set of workspace-bound tokens; nil
// removes it. Without one a bound token sees no event.
func (c *Component) SetCoverage(cov Coverage) {
	c.injected.mu.Lock()
	defer c.injected.mu.Unlock()
	c.injected.coverage = cov
}

// SetChangeSources installs the change lookup; nil removes it. Without one
// a token bound to a task fork sees no event of the fork's parent.
func (c *Component) SetChangeSources(src ChangeSources) {
	c.injected.mu.Lock()
	defer c.injected.mu.Unlock()
	c.injected.sources = src
}

// ownerOnly are the prefixes of the types only owners of the workspace see.
var ownerOnly = []string{"parley.secret.", "parley.token.", "parley.webhook."}

// isOwnerOnly reports whether typ is a type only owners see.
func isOwnerOnly(typ string) bool {
	return slices.ContainsFunc(ownerOnly, func(p string) bool { return strings.HasPrefix(typ, p) })
}

// Visible decides whether caller may see rec, from the caller's access now:
//
//   - the admin token sees every event;
//   - an event of a workspace is seen by a user with a level on the
//     workspace or on the parent of the task fork it concerns, which is
//     stored with the event and so outlives the fork; a type of ownerOnly
//     needs level owner or admin there;
//   - an event of no workspace is seen by the user metadata.scope_id names
//     when metadata.scope is "user", by the members of the active org it
//     names when metadata.scope is "org", and by nobody else;
//   - a workspace-bound token sees, under its user's level, the events of
//     the workspaces its binding covers and, when bound to a task fork, the
//     change events of the fork's parent whose change has the fork as its
//     source; never an event of no workspace.
//
// A read that fails returns false with the error, never a grant.
func (c *Component) Visible(ctx context.Context, caller Caller, rec Record) (bool, error) {
	return c.newViewer(caller).visible(ctx, rec)
}

// VisibleToOwnerOf decides whether an owner of workspace may see rec, the
// form event webhooks use: exactly the events of workspace and of its task
// forks, deleted ones included, of every type. It reads nothing, so its
// error is always nil.
func (c *Component) VisibleToOwnerOf(_ context.Context, workspace string, rec Record) (bool, error) {
	if rec.Workspace == nil || workspace == "" {
		return false, nil
	}
	return *rec.Workspace == workspace || rec.parent == workspace, nil
}

// viewer decides the visibility of a batch of events for one caller. It
// reads each source at most once, when an event first needs it, and is
// dropped with the batch, so that a change of access shows at the next
// delivery.
type viewer struct {
	c        *Component
	caller   Caller
	coverage Coverage
	sources  ChangeSources

	readable     *access.ReadableSet
	orgs         []string
	orgsRead     bool
	covered      map[string]bool
	parent       string
	parentRead   bool
	changeSource map[string]string
}

// newViewer returns the viewer of one batch for caller.
func (c *Component) newViewer(caller Caller) *viewer {
	c.injected.mu.RLock()
	defer c.injected.mu.RUnlock()
	return &viewer{c: c, caller: caller, coverage: c.injected.coverage, sources: c.injected.sources}
}

// visible decides rec as Visible does.
func (v *viewer) visible(ctx context.Context, rec Record) (bool, error) {
	switch {
	case v.caller.Admin:
		return true, nil
	case v.caller.UserID == "":
		return false, nil
	case rec.Workspace == nil:
		if v.caller.Bound() {
			return false, nil
		}
		return v.scoped(ctx, rec)
	}
	if v.caller.Bound() {
		covered, err := v.covers(ctx, *rec.Workspace)
		if err != nil {
			return false, err
		}
		if !covered {
			return v.forkChange(ctx, rec)
		}
	}
	return v.byLevel(ctx, rec)
}

// byLevel decides an event of a workspace by the user's level on the
// workspace and on its stored fork parent.
func (v *viewer) byLevel(ctx context.Context, rec Record) (bool, error) {
	readable, err := v.readableSet(ctx)
	if err != nil {
		return false, fmt.Errorf("decide the visibility of event %d: %w", rec.Seq, err)
	}
	slugs := []string{*rec.Workspace}
	if rec.parent != "" {
		slugs = append(slugs, rec.parent)
	}
	owners := isOwnerOnly(rec.Type)
	for _, slug := range slugs {
		switch readable.Level(slug) {
		case access.LevelAdmin, access.LevelOwner:
			return true, nil
		case access.LevelMember:
			if !owners {
				return true, nil
			}
		}
	}
	return false, nil
}

// readableSet is the user's readable set, read on first use; empty without
// an access rule.
func (v *viewer) readableSet(ctx context.Context) (access.ReadableSet, error) {
	if v.readable == nil {
		var set access.ReadableSet
		if a := v.c.deps.Access; a != nil {
			var err error
			if set, err = a.Readable(ctx, v.caller.accessCaller()); err != nil {
				return access.ReadableSet{}, err
			}
		}
		v.readable = &set
	}
	return *v.readable, nil
}

// activeOrgs are the ids of the active orgs of the user, read on first use;
// none without a membership source.
func (v *viewer) activeOrgs(ctx context.Context) ([]string, error) {
	if !v.orgsRead {
		if m := v.c.deps.Memberships; m != nil {
			orgs, err := m.ActiveOrgIDs(ctx, v.caller.UserID)
			if err != nil {
				return nil, fmt.Errorf("read the active orgs of the caller: %w", err)
			}
			v.orgs = orgs
		}
		v.orgsRead = true
	}
	return v.orgs, nil
}

// boundParent is the parent of the task fork the caller is bound to, read
// on first use; "" when the binding is not a task fork.
func (v *viewer) boundParent(ctx context.Context) (string, error) {
	if !v.parentRead {
		if p, d := v.c.deps.Parents, v.c.deps.Store; p != nil && d != nil {
			parent, err := p.ForkParent(ctx, d.Main(), v.caller.Workspace)
			if err != nil {
				return "", fmt.Errorf("read the workspace record: %w", err)
			}
			v.parent = parent
		}
		v.parentRead = true
	}
	return v.parent, nil
}

// scoped decides an event of no workspace by its metadata.scope.
func (v *viewer) scoped(ctx context.Context, rec Record) (bool, error) {
	scope, id, err := scopeOf(rec.Metadata)
	if err != nil {
		return false, fmt.Errorf("decide the visibility of event %d: %w", rec.Seq, err)
	}
	switch scope {
	case "user":
		return id == v.caller.UserID, nil
	case "org":
		orgs, err := v.activeOrgs(ctx)
		if err != nil {
			return false, fmt.Errorf("decide the visibility of event %d: %w", rec.Seq, err)
		}
		return id != "" && slices.Contains(orgs, id), nil
	default:
		return false, nil
	}
}

// scopeOf reads metadata.scope and metadata.scope_id; a member that is not
// a string reads as "".
func scopeOf(meta json.RawMessage) (scope, id string, err error) {
	if len(meta) == 0 {
		return "", "", nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(meta, &members); err != nil {
		return "", "", fmt.Errorf("read the metadata: %w", err)
	}
	_ = json.Unmarshal(members["scope"], &scope)
	_ = json.Unmarshal(members["scope_id"], &id)
	return scope, id, nil
}

// covers asks the covered set whether the bound caller's binding covers
// slug; without a covered set nothing is covered.
func (v *viewer) covers(ctx context.Context, slug string) (bool, error) {
	if v.coverage == nil {
		return false, nil
	}
	if ok, seen := v.covered[slug]; seen {
		return ok, nil
	}
	ok, err := v.coverage.Covers(ctx, v.caller, slug)
	if err != nil {
		return false, fmt.Errorf("decide the visibility of an event of %s: read the token's covered set: %w", slug, err)
	}
	if v.covered == nil {
		v.covered = map[string]bool{}
	}
	v.covered[slug] = ok
	return ok, nil
}

// forkChange decides an event of a workspace the binding does not cover:
// for a token bound to a task fork, a change event of the fork's parent
// whose change has the fork as its source is decided by the user's level;
// every other is not visible.
func (v *viewer) forkChange(ctx context.Context, rec Record) (bool, error) {
	if v.coverage == nil || v.sources == nil || rec.ResourceType != "change" {
		return false, nil
	}
	number, err := strconv.ParseInt(rec.ResourceID, 10, 64)
	if err != nil || number <= 0 {
		return false, nil
	}
	parent, err := v.boundParent(ctx)
	if err != nil {
		return false, fmt.Errorf("decide the visibility of event %d: %w", rec.Seq, err)
	}
	if parent == "" || *rec.Workspace != parent {
		return false, nil
	}
	key := rec.ResourceID
	source, seen := v.changeSource[key]
	if !seen {
		if source, err = v.sources.ChangeSource(ctx, parent, number); err != nil {
			return false, fmt.Errorf("decide the visibility of event %d: read the change's source: %w", rec.Seq, err)
		}
		if v.changeSource == nil {
			v.changeSource = map[string]string{}
		}
		v.changeSource[key] = source
	}
	if source != v.caller.Workspace {
		return false, nil
	}
	return v.byLevel(ctx, rec)
}
