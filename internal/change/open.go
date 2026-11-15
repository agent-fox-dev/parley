package change

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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
	// CheckPaths asks that every file of version 1 match the globs of a
	// path-scoped caller; the REST route sets it, while a push or a commit
	// had its paths checked when it moved the branch.
	CheckPaths bool
}

// Open opens a change in tx, the caller's transaction, which it neither
// begins nor commits. It first runs every check of an open, in order (the
// binding of a bound token, the input, an explicit session, the caller's
// access, as Authorize judges binding and access, the workspaces' state,
// the branches and commits, a change that already has the source, the
// title and intent, and with CheckPaths the paths of a path-scoped
// caller), and answers the first that fails without writing anything. It then gives the target workspace's next number, records
// version 1 with its ref refs/parley/changes/<number>/1 in the target's
// repository, links its session, stores the change draft or open with
// mergeable unknown, asks the auto_land guard before it stores auto_land,
// then runs the stack linker, the landing evaluator (not for a draft) and
// the version observers in that order, tells the fork activity recorder of
// a fork source, and writes parley.change.version and, unless the change is
// a draft, parley.change.open. It returns the change as stored then.
//
// The version ref is written before the collaborators run, which may read
// the head from the target's repository, and before Open returns, since a
// ref cannot join a transaction. When a later step fails, Open removes the
// ref again, so that a refused open leaves none; one left by a
// transaction the caller rolls back after Open succeeded names no stored
// version and is overwritten when that number is next recorded. An error a
// collaborator returns is returned unchanged.
func (c *Component) Open(ctx context.Context, tx *sql.Tx, in OpenInput) (_ Change, err error) {
	co := c.collaborators()
	o, err := c.check(ctx, tx, co, in)
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
	defer func() { c.dropVersionRef(ctx, o.targetRepo, o.target.Slug, number, 1, err) }()
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
	if err := c.afterVersion(ctx, tx, co, ch.WorkspaceSlug, number, v, in.DependsOn, nil); err != nil {
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
// keeps its blocked landing and asks no evaluator; structural, when not
// nil, is the landing a cause in the version's commits blocks, which is
// stored without asking the evaluator.
func (c *Component) afterVersion(ctx context.Context, tx *sql.Tx, co collaborators, workspace string, number int64, v Version, dependsOn *int64, structural *Landing) error {
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
		if structural != nil {
			landing = *structural
		} else if co.evaluator != nil {
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
	// files are every file the version touches, in byte order; paths the
	// first MaxPaths of them, truncated set when there were more.
	files, paths  []string
	truncated     bool
	title, intent string
	session       *string
	labels        []string
}

// labelPattern is what a label is.
var labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:/-]{0,62}$`)

// MaxLabels is the number of labels a change may have.
const MaxLabels = 20

// check runs every check of the open in, in order, and resolves it, reading
// through q and writing nothing: the binding of a bound token, the input,
// an explicit session, the caller's access, the state of the target and the
// source workspace, the branches and commits of source and target, the
// change that may already have the source, the title and the intent, and
// the paths of a path-scoped token. The first that fails is its answer.
func (c *Component) check(ctx context.Context, q Querier, co collaborators, in OpenInput) (*opening, error) {
	if err := c.authorizeBinding(ctx, in.Caller, in.Workspace, in.SourceWorkspace); err != nil {
		return nil, err
	}
	if err := validate(in); err != nil {
		return nil, err
	}
	o := &opening{}
	if in.SessionID != nil {
		var err error
		if o.session, err = c.session(ctx, co, in, Workspace{}); err != nil {
			return nil, err
		}
	}
	if err := c.authorizeAccess(ctx, in.Caller, in.Workspace, in.SourceWorkspace); err != nil {
		return nil, err
	}
	if err := c.resolve(ctx, q, co, in, o); err != nil {
		return nil, err
	}
	if in.CheckPaths && in.Caller.Paths != nil {
		for _, path := range o.files {
			if !identity.MatchAny(in.Caller.Paths, path) {
				return nil, server.PathNotAllowed(fmt.Sprintf("%s: the token may not change this file", path)).With("path", path)
			}
		}
	}
	return o, nil
}

// validate checks what the open in names by itself.
func validate(in OpenInput) error {
	switch {
	case in.SourceBranch == "":
		return server.InvalidRequest("source.branch: required")
	case in.DependsOn != nil && *in.DependsOn < 1:
		return server.InvalidRequest("depends_on: must be a positive integer")
	case len(in.Labels) > MaxLabels:
		return server.InvalidRequest(fmt.Sprintf("labels: at most %d", MaxLabels))
	}
	for _, l := range in.Labels {
		if !labelPattern.MatchString(l) {
			return server.InvalidRequest(fmt.Sprintf("labels: %q must match %s", l, labelPattern))
		}
	}
	return nil
}

// resolve reads what the open in names, checks the workspaces, branches and
// commits in order, fills in the defaults and checks the title and the
// intent, into o.
func (c *Component) resolve(ctx context.Context, q Querier, co collaborators, in OpenInput, o *opening) error {
	related, err := c.readWorkspaces(ctx, q, in, o)
	if err != nil {
		return err
	}
	targetHead, err := c.targetBranch(ctx, in, o, related)
	if err != nil {
		return err
	}
	if !related {
		return sourceNotRelated(in.SourceWorkspace, o.target.Slug)
	}
	if in.SessionID == nil {
		if o.session, err = c.session(ctx, co, in, o.source); err != nil {
			return err
		}
	}
	if err := c.sourceHead(ctx, in, o); err != nil {
		return err
	}
	ahead, err := o.commitsAhead(ctx, in, targetHead)
	if err != nil {
		return err
	}
	if _, _, found, err := activeOfSource(ctx, q, o.source.Slug, in.SourceBranch); err != nil || found {
		if err != nil {
			return err
		}
		return c.changeExists(ctx, q, o.source.Slug, in.SourceBranch)
	}
	if err := o.measure(ctx, targetHead, ahead); err != nil {
		return err
	}
	if err := o.describe(ctx, in); err != nil {
		return err
	}
	o.labels = []string{}
	for _, l := range in.Labels {
		if !slices.Contains(o.labels, l) {
			o.labels = append(o.labels, l)
		}
	}
	return nil
}

// readWorkspaces reads the target and the source workspace of the open in into
// o and checks that both are usable, before whether the source is related:
// related is false for a source workspace that does not exist or is
// neither the target nor a fork of it.
func (c *Component) readWorkspaces(ctx context.Context, q Querier, in OpenInput, o *opening) (related bool, err error) {
	target, found, err := c.deps.Workspaces.Workspace(ctx, q, in.Workspace)
	if err != nil {
		return false, err
	}
	if !found {
		return false, server.NotFound(fmt.Sprintf("no workspace %q", in.Workspace))
	}
	o.target, o.source = target, target
	related = true
	if in.SourceWorkspace != "" && in.SourceWorkspace != in.Workspace {
		source, found, err := c.deps.Workspaces.Workspace(ctx, q, in.SourceWorkspace)
		if err != nil {
			return false, err
		}
		o.source = source
		related = found && source.Fork && source.Parent == target.Slug
		if !found {
			o.source = Workspace{Slug: in.SourceWorkspace}
		}
	}
	if target.Unusable != nil {
		return false, target.Unusable
	}
	if o.source.Unusable != nil {
		return false, o.source.Unusable
	}
	return related, nil
}

// targetBranch fills in the target branch of the open in, the one named or
// the default, the base branch of a fork source related to the target or
// the target workspace's default branch, opens the target's repository and returns the
// branch's head; a branch the repository does not have, or the integration
// branch of a carry-patch workspace, is refused.
func (c *Component) targetBranch(ctx context.Context, in OpenInput, o *opening, related bool) (string, error) {
	target := o.target
	o.targetBranch = target.DefaultBranch
	if o.source.Fork && related {
		o.targetBranch = o.source.DefaultBranch
	}
	if in.TargetBranch != nil {
		o.targetBranch = *in.TargetBranch
	}
	var err error
	if o.targetRepo, err = c.deps.Repos(ctx, target.Slug); err != nil {
		return "", err
	}
	head, found, err := o.targetRepo.ResolveBranch(ctx, o.targetBranch)
	if err != nil {
		return "", err
	}
	if !found {
		return "", server.NewError("target_not_found", http.StatusBadRequest, fmt.Sprintf("target_branch: no branch %q in %s", o.targetBranch, target.Slug))
	}
	if target.CarryPatch && o.targetBranch == target.IntegrationBranch {
		return "", server.NewError("integration_branch", http.StatusForbidden,
			fmt.Sprintf("target_branch: %s is the integration branch of %s, which the hub rebuilds; target a patch branch", o.targetBranch, target.Slug))
	}
	return head, nil
}

// sourceHead opens the source's repository and fills in the head of the
// open in: the one the trigger named, or the source branch's, which must
// exist and must not be the target branch itself.
func (c *Component) sourceHead(ctx context.Context, in OpenInput, o *opening) error {
	o.sourceRepo = o.targetRepo
	var err error
	if o.source.Slug != o.target.Slug {
		if o.sourceRepo, err = c.deps.Repos(ctx, o.source.Slug); err != nil {
			return err
		}
	}
	if o.head = in.Head; o.head == "" {
		var found bool
		if o.head, found, err = o.sourceRepo.ResolveBranch(ctx, in.SourceBranch); err != nil {
			return err
		}
		if !found {
			return server.NewError("source_not_found", http.StatusBadRequest, fmt.Sprintf("source.branch: no branch %q in %s", in.SourceBranch, o.source.Slug))
		}
	}
	if o.source.Slug == o.target.Slug && in.SourceBranch == o.targetBranch {
		return server.InvalidRequest(fmt.Sprintf("source.branch: %s is the target branch itself", in.SourceBranch))
	}
	return nil
}

// commitsAhead counts the commits of the head that the target branch, at
// targetHead, lacks; there must be at least one and none may be a merge
// commit.
func (o *opening) commitsAhead(ctx context.Context, in OpenInput, targetHead string) (int, error) {
	// A fork's repository reads its parent's objects, so the commits are
	// read and the version measured where its head is, before the head is
	// brought into the target's repository.
	ahead, err := o.sourceRepo.CommitsBetween(ctx, targetHead, o.head, 0)
	if err != nil {
		return 0, err
	}
	if len(ahead) == 0 {
		return 0, server.NewError("nothing_to_land", http.StatusBadRequest,
			fmt.Sprintf("source.branch: %s has no commit that %s lacks", in.SourceBranch, o.targetBranch))
	}
	commits, err := o.sourceRepo.ReadCommits(ctx, ahead)
	if err != nil {
		return 0, err
	}
	for _, cm := range commits {
		if len(cm.Parents) > 1 {
			return 0, server.NewError("merge_commits_unsupported", http.StatusBadRequest,
				fmt.Sprintf("source.branch: the merge commit %s is among the commits to land; rebase instead", cm.ID))
		}
	}
	return len(ahead), nil
}

// measure computes the version's base, stats, touched paths and patch id
// against the target's head; ahead is the number of its commits the target
// lacks.
func (o *opening) measure(ctx context.Context, targetHead string, ahead int) error {
	var err error
	if o.base, err = o.sourceRepo.MergeBase(ctx, o.head, targetHead); err != nil {
		return err
	}
	return o.measureFromBase(ctx, ahead)
}

// measureFromBase computes the version's stats, touched paths and patch id
// against its base, o.base; ahead is the number of its commits.
func (o *opening) measureFromBase(ctx context.Context, ahead int) error {
	repo := o.sourceRepo
	changes, err := repo.DiffChanges(ctx, o.base, o.head)
	if err != nil {
		return err
	}
	s := gitrepo.Stats(changes)
	o.stats = Stats{Commits: ahead, FilesChanged: s.FilesChanged, Insertions: s.Insertions, Deletions: s.Deletions}
	o.files = touchedPaths(changes)
	o.paths, o.truncated = o.files, len(o.files) > MaxPaths
	if o.truncated {
		o.paths = o.files[:MaxPaths]
	}
	files, err := keyed(ctx, repo, o.base, o.head, changes)
	o.patchID = patchID(files)
	return err
}

// touchedPaths are the files changes touch, both sides of a rename, in
// byte order and each once.
func touchedPaths(changes []gitrepo.FileChange) []string {
	var paths []string
	for _, ch := range changes {
		paths = append(paths, ch.Path)
		if ch.OldPath != "" {
			paths = append(paths, ch.OldPath)
		}
	}
	slices.Sort(paths)
	return nonNil(slices.Compact(paths))
}

// describe fills in the title and the intent: the caller's, or the
// subject and the body of the head commit, the intent then the source
// fork's purpose, each default cut to its limit. A title must then be one
// line of at most MaxTitle characters, a given intent at most MaxIntent,
// and the intent must say something, unless the change is a draft or the
// target workspace does not require an intent.
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
	switch {
	case strings.TrimSpace(o.title) == "":
		return server.InvalidRequest("title: must not be empty")
	case utf8.RuneCountInString(o.title) > MaxTitle:
		return server.InvalidRequest(fmt.Sprintf("title: at most %d characters", MaxTitle))
	case strings.ContainsAny(o.title, "\r\n"):
		return server.InvalidRequest("title: must be one line")
	case utf8.RuneCountInString(o.intent) > MaxIntent:
		return server.InvalidRequest(fmt.Sprintf("intent: at most %d characters", MaxIntent))
	case strings.TrimSpace(o.intent) == "" && !in.Draft && o.target.RequireIntent:
		return server.NewError("intent_required", http.StatusBadRequest,
			fmt.Sprintf("intent: %s requires one unless the change is a draft; say why the change exists", o.target.Slug))
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

// dropVersionRef removes from repo, the target's repository, the ref of
// the version of the change number of the workspace, which the open or the
// version that failed with err wrote, so that a
// refusal leaves no ref at a head never recorded, which a fork would copy
// with the change's other refs. It does nothing when err is nil; a removal
// that fails is logged.
func (c *Component) dropVersionRef(ctx context.Context, repo *gitrepo.Repo, workspace string, number, version int64, err error) {
	if err == nil {
		return
	}
	ref := VersionRef(number, version)
	// The request may have ended, which is often why the step failed.
	if derr := repo.DeleteRef(context.WithoutCancel(ctx), ref); derr != nil {
		c.deps.Logger.Warn("the version ref of a refused version was not removed", "workspace", workspace, "ref", ref, "error", derr)
	}
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
	if err := c.emitVersion(ctx, tx, caller, ch, v); err != nil || ch.State == StateDraft {
		return err
	}
	return c.emit(ctx, tx, caller, ch, EventOpen, map[string]any{
		"version": ch.Version, "head_sha": ch.HeadSHA, "title": ch.Title, "target_branch": ch.TargetBranch,
		"source_workspace": ch.Source.WorkspaceSlug, "source_branch": ch.Source.Branch, "depends_on": ch.DependsOn,
		"was_draft": false,
	})
}

// emitVersion writes parley.change.version for the version v of the change
// ch.
func (c *Component) emitVersion(ctx context.Context, tx *sql.Tx, caller Caller, ch Change, v Version) error {
	return c.emit(ctx, tx, caller, ch, EventVersion, map[string]any{
		"version": v.Number, "head_sha": v.HeadSHA, "kind": v.Kind, "base_sha": v.BaseSHA,
		"patch_equivalent_to": v.PatchEquivalentTo, "draft": ch.State == StateDraft,
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

// now is the component's clock, in UTC.
func (c *Component) now() time.Time { return c.deps.Now().UTC() }
