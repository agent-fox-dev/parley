package change

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The event types of a change this package writes.
const (
	EventVersion = "parley.change.version"
	EventOpen    = "parley.change.open"
)

// OpenInput is what Open opens a change from.
type OpenInput struct {
	// Workspace is the slug of the target workspace, the one the change
	// belongs to.
	Workspace string
	// SourceWorkspace is the slug of the source workspace, "" for
	// Workspace; SourceBranch the source branch, without refs/heads/.
	SourceWorkspace, SourceBranch string
	// Head is the commit the trigger moved the source branch to, "" to read
	// the branch's head.
	Head string
	// TargetBranch, Title, Intent, AutoLand and SessionID are the values
	// the caller named, nil for the defaults.
	TargetBranch, Title, Intent *string
	AutoLand                    *bool
	SessionID                   *string
	// TriggerSession is the session of the push or commit that triggered
	// the open, nil for none: the default session, before the source
	// fork's.
	TriggerSession *string
	Draft          bool
	// DependsOn is the change the caller named as the one this one
	// depends on, which the stack linker is given; nil for none.
	DependsOn *int64
	Labels    []string
	// Caller is who opens the change.
	Caller Caller
	// Kind is the kind of version 1: KindRef for a head already on the
	// source branch, KindPush or KindCommit for the push or commit that
	// opened it, KindRevert for a revert.
	Kind string
	// Reverts is the number of the change a revert reverts, nil for none.
	Reverts *int64
}

// Open opens a change in tx, the caller's transaction, which it neither
// begins nor commits: it gives the target workspace's next number, records
// version 1 with its ref refs/parley/changes/<number>/1 in the target's
// repository, links its session, stores the change draft or open with
// mergeable unknown, asks the auto_land guard before it stores auto_land,
// then runs the stack linker, the landing evaluator (not for a draft) and
// the version observers in that order, tells the fork activity recorder of
// a fork source, and writes parley.change.version and, unless the change is
// a draft, parley.change.open. It returns the change as stored then.
//
// The version ref is written before Open returns, since a ref cannot join a
// transaction: one left by a transaction that rolls back names no stored
// version and is overwritten when that number is next recorded. An error a
// collaborator returns is returned unchanged.
func (c *Component) Open(ctx context.Context, tx *sql.Tx, in OpenInput) (Change, error) {
	co := c.collaborators()
	o, err := c.resolve(ctx, tx, co, in)
	if err != nil {
		return Change{}, err
	}
	number, err := nextNumber(ctx, tx, o.target.Slug)
	if err != nil {
		return Change{}, err
	}
	now := c.now()
	actor, err := c.actorOf(ctx, tx, in.Caller)
	if err != nil {
		return Change{}, err
	}
	v := Version{
		Number: 1, HeadSHA: o.head, BaseSHA: o.base, Kind: in.Kind, CreatedBy: actor, SessionID: o.session,
		Stats: o.stats, CreatedAt: now, Credential: in.Caller.Credential, PatchID: o.patchID,
	}
	ch := Change{
		WorkspaceSlug: o.target.Slug, Number: number, Ref: Ref(o.target.Slug, number), Title: o.title, Intent: o.intent,
		State: StateOpen, TargetBranch: o.targetBranch, Source: Source{o.source.Slug, in.SourceBranch},
		Version: 1, HeadSHA: o.head, BaseSHA: o.base, Author: actor, SessionID: o.session,
		Mergeable: MergeableUnknown, Stats: o.stats, Paths: o.paths, PathsTruncated: o.truncated,
		Landing: readyLanding(), Labels: o.labels, Reverts: in.Reverts, CreatedAt: now, UpdatedAt: now,
	}
	if in.Draft {
		ch.State, ch.Landing = StateDraft, blockedLanding("draft")
	}
	if err := insertChange(ctx, tx, ch, v); err != nil {
		if errors.Is(err, errSourceTaken) {
			return Change{}, c.changeExists(ctx, tx, o.source.Slug, in.SourceBranch)
		}
		return Change{}, err
	}
	if err := insertVersion(ctx, tx, ch.WorkspaceSlug, number, v); err != nil {
		return Change{}, err
	}
	if err := o.writeVersionRef(ctx, number, 1); err != nil {
		return Change{}, err
	}
	if o.session != nil && co.sessions != nil {
		if err := co.sessions.Record(ctx, tx, *o.session, ch.WorkspaceSlug, number, 1); err != nil {
			return Change{}, err
		}
	}
	if in.AutoLand != nil && *in.AutoLand {
		if co.guard != nil {
			if err := co.guard.CheckAutoLand(ctx, tx, in.Caller, ch, true); err != nil {
				return Change{}, err
			}
		}
		if err := c.SetAutoLand(ctx, tx, ch.WorkspaceSlug, number, true, in.Caller); err != nil {
			return Change{}, err
		}
	}
	if err := c.afterVersion(ctx, tx, co, ch.WorkspaceSlug, number, v, in.DependsOn); err != nil {
		return Change{}, err
	}
	if o.source.Fork && co.activity != nil {
		if err := co.activity.RecordActivity(ctx, tx, o.source.Slug); err != nil {
			c.deps.Logger.Warn("the fork's activity was not recorded", "fork", o.source.Slug, "change", ch.Ref, "error", err)
		}
	}
	if ch, err = readChange(ctx, tx, ch.WorkspaceSlug, number); err != nil {
		return Change{}, err
	}
	if err := c.emitOpen(ctx, tx, in.Caller, ch, v); err != nil {
		return Change{}, err
	}
	return ch, nil
}

// afterVersion runs, for the new version v of the change number of the
// workspace, the stack linker, the landing derivation and the version
// observers, in that order, each with the change as stored at its turn, so
// that an observer reads the landing of this version's evaluation. A draft
// keeps its blocked landing and asks no evaluator.
func (c *Component) afterVersion(ctx context.Context, tx *sql.Tx, co collaborators, workspace string, number int64, v Version, dependsOn *int64) error {
	ch, err := readChange(ctx, tx, workspace, number)
	if err != nil {
		return err
	}
	if co.stack != nil {
		if err := co.stack.LinkVersion(ctx, tx, ch, v, dependsOn); err != nil {
			return err
		}
	}
	if ch.State != StateDraft {
		landing := readyLanding()
		if co.evaluator != nil {
			if ch, err = readChange(ctx, tx, workspace, number); err != nil {
				return err
			}
			if landing, err = co.evaluator.EvaluateLanding(ctx, tx, ch, v); err != nil {
				return err
			}
		}
		if err := storeLanding(ctx, tx, workspace, number, landing, c.now()); err != nil {
			return err
		}
	}
	for _, o := range co.observers {
		if ch, err = readChange(ctx, tx, workspace, number); err != nil {
			return err
		}
		if err := o.VersionRecorded(ctx, tx, ch, v, v.Credential); err != nil {
			return err
		}
	}
	return nil
}

// SetAutoLand stores value as the auto_land of the change number of the
// workspace in tx, set by caller; the auto_land guard has been asked
// before. A change that does not exist is the change-not-found error.
func (c *Component) SetAutoLand(ctx context.Context, tx *sql.Tx, workspace string, number int64, value bool, _ Caller) error {
	return update(ctx, tx, workspace, number, c.now(), `auto_land = ?`, value)
}

// ReadChange reads the change number of the workspace, without a scope or
// access check, through q: the caller's transaction, which it sees the
// writes of, or nil for the store. A change that does not exist, also one
// of that number in another workspace, is the change-not-found error.
func (c *Component) ReadChange(ctx context.Context, q Querier, workspace string, number int64) (Change, error) {
	if q == nil {
		q = c.deps.Store.Main()
	}
	return readChange(ctx, q, workspace, number)
}

// opening is an open resolved: its workspaces and repositories, the
// version's commits and numbers, and the change's values with the defaults
// filled in.
type opening struct {
	target, source         Workspace
	targetRepo, sourceRepo *gitrepo.Repo
	targetBranch           string
	head, base, patchID    string
	stats                  Stats
	paths                  []string
	truncated              bool
	title, intent          string
	session                *string
	labels                 []string
}

// resolve reads what the open in names and fills in its defaults.
func (c *Component) resolve(ctx context.Context, tx *sql.Tx, co collaborators, in OpenInput) (*opening, error) {
	o := &opening{}
	target, found, err := c.deps.Workspaces.Workspace(ctx, tx, in.Workspace)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, server.NotFound(fmt.Sprintf("no workspace %q", in.Workspace))
	}
	o.target, o.source = target, target
	if in.SourceWorkspace != "" && in.SourceWorkspace != in.Workspace {
		source, found, err := c.deps.Workspaces.Workspace(ctx, tx, in.SourceWorkspace)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, sourceNotRelated(in.SourceWorkspace, in.Workspace)
		}
		o.source = source
	}
	if o.session, err = c.session(ctx, co, in, o.source); err != nil {
		return nil, err
	}
	o.targetBranch = target.DefaultBranch
	if o.source.Fork {
		o.targetBranch = o.source.DefaultBranch
	}
	if in.TargetBranch != nil {
		o.targetBranch = *in.TargetBranch
	}
	if o.targetRepo, err = c.deps.Repos(ctx, target.Slug); err != nil {
		return nil, err
	}
	o.sourceRepo = o.targetRepo
	if o.source.Slug != target.Slug {
		if o.sourceRepo, err = c.deps.Repos(ctx, o.source.Slug); err != nil {
			return nil, err
		}
	}
	targetHead, found, err := o.targetRepo.ResolveBranch(ctx, o.targetBranch)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, server.NewError("target_not_found", http.StatusBadRequest, fmt.Sprintf("target_branch: no branch %q in %s", o.targetBranch, target.Slug))
	}
	if o.head = in.Head; o.head == "" {
		if o.head, found, err = o.sourceRepo.ResolveBranch(ctx, in.SourceBranch); err != nil {
			return nil, err
		}
		if !found {
			return nil, server.NewError("source_not_found", http.StatusBadRequest, fmt.Sprintf("source.branch: no branch %q in %s", in.SourceBranch, o.source.Slug))
		}
	}
	// A fork's repository reads its parent's objects, so the version is
	// measured where its head is, before the head is brought into the
	// target's repository.
	if err := o.measure(ctx, targetHead); err != nil {
		return nil, err
	}
	if err := o.describe(ctx, in); err != nil {
		return nil, err
	}
	o.labels = []string{}
	for _, l := range in.Labels {
		if !slices.Contains(o.labels, l) {
			o.labels = append(o.labels, l)
		}
	}
	return o, nil
}

// measure computes the version's base, stats, touched paths and patch id
// against the target's head.
func (o *opening) measure(ctx context.Context, targetHead string) error {
	repo := o.sourceRepo
	var err error
	if o.base, err = repo.MergeBase(ctx, o.head, targetHead); err != nil {
		return err
	}
	changes, err := repo.DiffChanges(ctx, o.base, o.head)
	if err != nil {
		return err
	}
	ahead, _, err := repo.AheadBehind(ctx, targetHead, o.head)
	if err != nil {
		return err
	}
	s := gitrepo.Stats(changes)
	o.stats = Stats{Commits: ahead, FilesChanged: s.FilesChanged, Insertions: s.Insertions, Deletions: s.Deletions}
	o.paths, o.truncated = touchedPaths(changes)
	o.patchID, err = repo.PatchID(ctx, o.base, o.head)
	return err
}

// touchedPaths are the files changes touch, both sides of a rename, in
// byte order and each once, at most MaxPaths of them; truncated is set
// when there were more.
func touchedPaths(changes []gitrepo.FileChange) (paths []string, truncated bool) {
	for _, ch := range changes {
		paths = append(paths, ch.Path)
		if ch.OldPath != "" {
			paths = append(paths, ch.OldPath)
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if len(paths) > MaxPaths {
		return paths[:MaxPaths], true
	}
	return nonNil(paths), false
}

// describe fills in the title and the intent: the caller's, or the
// subject and the body of the head commit, the intent then the source
// fork's purpose, each default cut to its limit.
func (o *opening) describe(ctx context.Context, in OpenInput) error {
	commits, err := o.sourceRepo.ReadCommits(ctx, []string{o.head})
	if err != nil {
		return err
	}
	subject, body, _ := strings.Cut(commits[0].Message, "\n")
	subject, body = strings.TrimSpace(subject), strings.TrimSpace(body)
	switch {
	case in.Title != nil:
		o.title = *in.Title
	case subject == "":
		return server.InvalidRequest("title: the head commit has no subject to take the title from; name one")
	default:
		o.title = cut(subject, MaxTitle)
	}
	switch {
	case in.Intent != nil:
		o.intent = *in.Intent
	case body != "":
		o.intent = cut(body, MaxIntent)
	case o.source.Fork && o.source.Purpose != nil:
		o.intent = cut(strings.TrimSpace(*o.source.Purpose), MaxIntent)
	}
	return nil
}

// cut is s cut to its first n characters.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// session is the session of the change: the caller's, which the session
// linker must accept, or the default, the trigger's, then the source
// fork's, dropped when the linker refuses it. Without a session linker an
// explicit session is refused and a default dropped.
func (c *Component) session(ctx context.Context, co collaborators, in OpenInput, source Workspace) (*string, error) {
	explicit, id := in.SessionID != nil, in.SessionID
	if !explicit {
		id = in.TriggerSession
		if id == nil && source.Fork {
			id = source.SessionID
		}
	}
	if id == nil {
		return nil, nil
	}
	unknown := server.NewError("unknown_session", http.StatusBadRequest, fmt.Sprintf("session_id: no session %q", *id))
	if co.sessions == nil {
		if explicit {
			return nil, unknown
		}
		return nil, nil
	}
	got, err := co.sessions.Check(ctx, in.Caller, *id, in.Workspace)
	switch {
	case errors.Is(err, ErrUnknownSession) && explicit:
		return nil, unknown
	case errors.Is(err, ErrUnknownSession):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &got, nil
}

// writeVersionRef points the ref of the version of the change number at
// the head in the target's repository, bringing a fork's commit there
// first.
func (o *opening) writeVersionRef(ctx context.Context, number, version int64) error {
	ref := VersionRef(number, version)
	if o.sourceRepo == o.targetRepo {
		return o.targetRepo.SetRef(ctx, ref, o.head)
	}
	return o.targetRepo.SetRefFrom(ctx, o.sourceRepo.Dir(), ref, o.head)
}

// actorOf is the actor the caller is, with the user's name read in tx when
// the caller does not carry it.
func (c *Component) actorOf(ctx context.Context, tx *sql.Tx, caller Caller) (Actor, error) {
	if caller.UserID != "" && caller.Username == "" {
		u, err := identity.UserByID(ctx, tx, caller.UserID)
		if err != nil {
			return Actor{}, fmt.Errorf("read the user %s: %w", caller.UserID, err)
		}
		caller.Username = u.Username
	}
	return caller.actor(), nil
}

// changeExists is the refusal of an open whose source another draft or
// open change has, naming that change.
func (c *Component) changeExists(ctx context.Context, q Querier, source, branch string) error {
	workspace, number, found, err := activeOfSource(ctx, q, source, branch)
	if err != nil {
		return err
	}
	e := server.NewError("change_exists", http.StatusConflict, fmt.Sprintf("%s:%s is already the source of a change", source, branch))
	if found {
		e = e.WithDetails(map[string]any{"workspace_slug": workspace, "number": number, "ref": Ref(workspace, number)})
	}
	return e
}

// sourceNotRelated is the refusal of a source workspace that is neither the
// target nor a fork of it, or that the caller may not see.
func sourceNotRelated(source, target string) error {
	return server.NewError("source_not_related", http.StatusBadRequest,
		fmt.Sprintf("source.workspace_slug: %q is neither %s nor a fork of it", source, target))
}

// emitOpen writes the events of the opened change ch: parley.change.version
// for its version v, then parley.change.open unless it is a draft.
func (c *Component) emitOpen(ctx context.Context, tx *sql.Tx, caller Caller, ch Change, v Version) error {
	draft := ch.State == StateDraft
	if err := c.emit(ctx, tx, caller, ch, EventVersion, map[string]any{
		"version": v.Number, "head_sha": v.HeadSHA, "kind": v.Kind, "base_sha": v.BaseSHA,
		"patch_equivalent_to": v.PatchEquivalentTo, "draft": draft,
	}); err != nil || draft {
		return err
	}
	return c.emit(ctx, tx, caller, ch, EventOpen, map[string]any{
		"version": ch.Version, "head_sha": ch.HeadSHA, "title": ch.Title, "target_branch": ch.TargetBranch,
		"source_workspace": ch.Source.WorkspaceSlug, "source_branch": ch.Source.Branch, "depends_on": ch.DependsOn,
		"was_draft": false,
	})
}

// emit writes the event typ of the change ch by caller in tx, its metadata
// the change's ref and meta.
func (c *Component) emit(ctx context.Context, tx *sql.Tx, caller Caller, ch Change, typ string, meta map[string]any) error {
	meta["ref"] = ch.Ref
	a := caller.actor()
	slug := ch.WorkspaceSlug
	_, err := c.deps.Events.Emit(ctx, tx, event.Input{
		Type: typ, Workspace: &slug, ActorType: a.ActorType, ActorID: a.ActorID, ResourceType: "change",
		ResourceID: strconv.FormatInt(ch.Number, 10), SessionID: ch.SessionID, Metadata: meta,
	})
	return err
}

// openBody is the body of POST /workspaces/:slug/changes.
type openBody struct {
	Source struct {
		WorkspaceSlug *string `json:"workspace_slug"`
		Branch        string  `json:"branch"`
	} `json:"source"`
	TargetBranch *string  `json:"target_branch"`
	Title        *string  `json:"title"`
	Intent       *string  `json:"intent"`
	Draft        *bool    `json:"draft"`
	DependsOn    *int64   `json:"depends_on"`
	AutoLand     *bool    `json:"auto_land"`
	Labels       []string `json:"labels"`
	SessionID    *string  `json:"session_id"`
}

// openRoute opens a change in the workspace :slug from the request's body,
// in a transaction of its own, with version 1 of kind ref: the head is on
// the source branch already. It answers 201 with the change object.
func (c *Component) openRoute(w http.ResponseWriter, r *http.Request) error {
	var body openBody
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	ctx := r.Context()
	p, _ := identity.PrincipalOf(ctx)
	in := OpenInput{
		Workspace: server.Param(r, "slug"), SourceBranch: body.Source.Branch,
		TargetBranch: body.TargetBranch, Title: body.Title, Intent: body.Intent, Draft: body.Draft != nil && *body.Draft,
		DependsOn: body.DependsOn, AutoLand: body.AutoLand, Labels: body.Labels, SessionID: body.SessionID,
		Caller: CallerOf(p), Kind: KindRef,
	}
	if body.Source.WorkspaceSlug != nil {
		in.SourceWorkspace = *body.Source.WorkspaceSlug
	}
	var ch Change
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		ch, err = c.Open(ctx, tx, in)
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, ch)
	return nil
}

// now is the component's clock, in UTC.
func (c *Component) now() time.Time { return c.deps.Now().UTC() }
