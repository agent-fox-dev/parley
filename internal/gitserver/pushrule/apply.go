package pushrule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// The surfaces that write refs through the ref writer, as
// ApplyRequest.Via names them.
const (
	ViaGit  = "git"
	ViaAPI  = "api"
	ViaLand = "land"
)

// The kinds of version a write makes of a change, as
// ApplyRequest.VersionKind names them for the ref-updated stages:
// VersionKindNone for a write that names none, such as a land.
const (
	VersionKindPush   = "push"
	VersionKindCommit = "commit"
	VersionKindRef    = "ref"
	VersionKindRebase = "rebase"
	VersionKindNone   = ""
)

// The outcomes of Apply, as Result.Outcome holds them. OutcomeAccepted
// moved every ref; OutcomeRefused is a request a rule refused, and
// OutcomeRefMoved one whose compare-and-swap failed, both moving none;
// OutcomeInternal moved none because something failed that was not the
// caller's doing: a rule that could not be evaluated, git, the store or a
// ref-updated stage.
const (
	OutcomeAccepted = "accepted"
	OutcomeRefused  = "refused"
	OutcomeRefMoved = "ref_moved"
	OutcomeInternal = "internal"
)

// ErrorTypeRefMoved is the error type of the update whose ref was not at
// its before value when the refs were written.
const ErrorTypeRefMoved = "ref_moved"

// DefaultForwardLimit is how long a forward to origin may run when
// WriterDeps.ForwardLimit is zero.
const DefaultForwardLimit = 120 * time.Second

const (
	msgNotUpdated      = "not updated: another ref in this push was refused"
	msgForwardTimedOut = "origin rejected push: timed out"
	msgRefMoved        = "ref changed since it was read; fetch and try again"
)

// Version is the version of a change a ref-updated stage recorded for an
// update: version Version of change Number of the workspace WorkspaceSlug.
type Version struct {
	WorkspaceSlug   string
	Number, Version int
}

// StageInput is what a ref-updated stage is given about the request being
// written: the facts of the workspace and of the caller, the session the
// write belongs to (nil for none), the surface it came through, the kind of
// version the caller of Apply named, and the accepted updates with their
// flags, in the order of the request.
type StageInput struct {
	Workspace        Workspace
	Caller           Caller
	SessionID        *string
	Via, VersionKind string
	Updates          []Decision
}

// RefUpdatedStage is a step of the commit of every accepted request, run in
// the store transaction tx after the refs are locked at their before
// values, with the new objects in the repository, and before they move. It
// returns, for each update of in.Updates at the same index, the version it
// recorded for it, nil for none; a shorter list leaves the updates after
// it without one. An error stops the request: no ref moves and nothing any
// stage wrote in tx is kept.
type RefUpdatedStage func(ctx context.Context, tx *sql.Tx, in StageInput) ([]*Version, error)

// HeadSetter records the new head of a workspace's default branch on its
// record. It is called after the refs moved, outside any store
// transaction.
type HeadSetter interface {
	SetHead(ctx context.Context, slug, id string) error
}

// ForkActivity records that a task fork was written to, which is when a
// fork's last activity is. It is called after the refs moved, outside any
// store transaction.
type ForkActivity interface {
	RecordActivity(ctx context.Context, slug string) error
}

// WriterDeps are what the ref writer writes with. Transact runs a function
// in one transaction of the store, as store.Store.Tx does; without it no
// ref-updated stage can run, and a request with stages registered fails as
// internal. Heads is the head_sha setter; nil sets no head. Logger takes
// what fails after the refs moved, which no longer changes the answer; nil
// discards it.
type WriterDeps struct {
	Transact func(ctx context.Context, fn func(*sql.Tx) error) error
	Heads    HeadSetter
	Logger   *slog.Logger
	// ForwardLimit is how long each forward to origin may run; zero means
	// DefaultForwardLimit. Tests shorten it.
	ForwardLimit time.Duration
	// Events is the event log the parley.git.push events are written to;
	// nil writes no event and keeps no journal.
	Events EventWriter
	// Repos opens the repository of a workspace by its slug, which
	// Reconcile reads the refs of a journal entry from.
	Repos func(ctx context.Context, slug string) (*gitrepo.Repo, error)
}

// Writer is the ref writer: the one way the git server, the source control
// API and landing move the refs of a workspace. Its zero value writes with
// no store, no stages and no recorders; Bind hands it the hub's, and the
// features that take part in every write register their collaborators on
// it while the hub is composed (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
type Writer struct {
	mu           sync.RWMutex
	deps         WriterDeps
	stages       []RefUpdatedStage
	forkActivity ForkActivity
	listeners    []PostPushListener
}

// Bind sets what the writer writes with from then on.
func (w *Writer) Bind(d WriterDeps) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deps = d
}

// RegisterRefUpdatedStage adds s after the stages registered before it;
// the stages run in that order in the commit of every accepted request.
func (w *Writer) RegisterRefUpdatedStage(s RefUpdatedStage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stages = append(w.stages, s)
}

// RegisterForkActivity makes f the recorder told of every accepted write to
// a task fork through git or the source control API, replacing the one
// registered before; nil removes it. Without one, no activity is recorded.
func (w *Writer) RegisterForkActivity(f ForkActivity) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.forkActivity = f
}

// ApplyRequest is one write of refs: the verdict about its updates, the
// repository they are written to, opened over a push's quarantine when it
// has one, the facts of the workspace and of the caller, the session it
// belongs to (nil for none), the surface it came through and the kind of
// version it makes. BeforeCommit, when set, runs once the refs are locked
// at their before values and before the stages run; a push moves its
// quarantined objects into the repository there, once the refs are certain
// to be at the values it was evaluated against. An error from it stops the
// request as a stage's does.
type ApplyRequest struct {
	Verdict          Verdict
	Repo             *gitrepo.Repo
	Workspace        Workspace
	Caller           Caller
	SessionID        *string
	Via, VersionKind string
	BeforeCommit     func(ctx context.Context) error
}

// UpdateResult is what became of one update: its decision as the verdict
// had it when the update moved, or the reason it did not, and the version
// a ref-updated stage recorded for it, nil for none.
type UpdateResult struct {
	Decision
	Version *Version
}

// Result is what became of a request: its outcome, one of the Outcome
// constants, and one result per update, in the order of the verdict. The
// result of an accepted request also carries what its event step, Finish,
// writes and reports.
type Result struct {
	Outcome string
	Updates []UpdateResult

	written *written
}

// Refused reports whether the request moved no ref.
func (r Result) Refused() bool { return r.Outcome != OutcomeAccepted }

// Apply writes the request: every ref of it, or none. A verdict that
// refuses an update writes nothing and runs no forward, and every update
// it allows is answered that it was not updated. Otherwise the pending
// forwards to origin run first, in command order, each under the forward
// limit; one that fails or outlasts its limit refuses the request with
// OutcomeRefused before any ref is written. Then the refs are moved in one
// compare-and-swap transaction of git's against their before values: it is
// prepared, BeforeCommit runs, the ref-updated stages run in one store
// transaction, and the refs then move just before that transaction
// commits. A ref not at its before value refuses the request with
// OutcomeRefMoved; a failure of BeforeCommit or a stage aborts the
// transaction and refuses every update as internal, and Apply returns that
// error unchanged. A store commit that fails after the refs moved is
// logged, and the request stays accepted. After an accepted request, the
// head_sha of a repository whose default branch moved is set, and the
// activity of a fork written through git or the API recorded.
//
// With an event log bound, the request is entered in the push journal, in
// a store transaction of its own, after the forwards and before the refs
// move, and the entry is removed when they do not; the caller then runs
// Finish with the result of an accepted request, which writes its events
// and removes the entry. Via must be ViaGit, ViaAPI or ViaLand: another
// value is an error before anything is done.
//
// No lock spans the request: two requests on one repository are ordered by
// the ref transaction alone, in which a ref the other holds counts as
// moved.
func (w *Writer) Apply(ctx context.Context, req ApplyRequest) (Result, error) {
	w.mu.RLock()
	deps, stages, forkActivity := w.deps, w.stages, w.forkActivity
	w.mu.RUnlock()
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	res := Result{Updates: make([]UpdateResult, len(req.Verdict.Decisions))}
	for i, d := range req.Verdict.Decisions {
		res.Updates[i].Decision = d
	}
	if req.Via != ViaGit && req.Via != ViaAPI && req.Via != ViaLand {
		return res.internal(), fmt.Errorf("apply: unknown surface %q", req.Via)
	}
	if req.Verdict.Refused() {
		res.Outcome = OutcomeRefused
		for i, d := range req.Verdict.Decisions {
			if d.Allowed {
				res.Updates[i].Decision = Decision{Update: d.Update, Message: msgNotUpdated}
			} else if d.ErrorType == ErrorTypeInternal {
				res.Outcome = OutcomeInternal
			}
		}
		return res, nil
	}
	if len(res.Updates) == 0 {
		res.Outcome = OutcomeAccepted
		return res, nil
	}
	if req.Repo == nil {
		return res.internal(), errors.New("apply: no repository to write to")
	}
	if refusedRes, ok, err := res.forward(ctx, deps.ForwardLimit, req.Verdict.Decisions); !ok {
		return refusedRes, err
	}
	wr := &written{entry: journalEntryOf(req), updates: req.Verdict.Decisions}
	if deps.Transact != nil && deps.Events != nil {
		err := deps.Transact(ctx, func(tx *sql.Tx) (err error) {
			wr.entry.ID, err = InsertJournalEntry(ctx, tx, wr.entry)
			return err
		})
		if err != nil {
			return res.internal(), err
		}
	}
	// A request whose refs did not move owes no event.
	unjournal := func() {
		if wr.entry.ID == 0 {
			return
		}
		bg := context.WithoutCancel(ctx)
		err := deps.Transact(bg, func(tx *sql.Tx) error {
			_, err := deleteJournalEntry(bg, tx, wr.entry.ID)
			return err
		})
		if err != nil {
			logger.Error("push: journal entry of a request whose refs did not move not removed",
				"workspace", req.Workspace.Slug, "error", err)
		}
	}
	tx, err := req.Repo.PrepareRefs(ctx, refUpdates(req.Verdict.Decisions))
	var refErr *gitrepo.RefError
	switch {
	case errors.As(err, &refErr):
		unjournal()
		// A name in the way of another ref's is as stale a picture of the
		// repository as a moved ref: the caller fetches and sees it.
		return res.refMoved(refErr.Ref), nil
	case err != nil:
		unjournal()
		return res.internal(), err
	}
	defer func() { _ = tx.Abort() }()
	if req.BeforeCommit != nil {
		if err := req.BeforeCommit(ctx); err != nil {
			_ = tx.Abort()
			unjournal()
			return res.internal(), err
		}
	}
	in := StageInput{Workspace: req.Workspace, Caller: req.Caller, SessionID: req.SessionID, Via: req.Via, VersionKind: req.VersionKind, Updates: req.Verdict.Decisions}
	moved, err := commit(ctx, deps.Transact, stages, tx, in, res.Updates)
	if !moved {
		_ = tx.Abort()
		unjournal()
		return res.internal(), err
	}
	if err != nil {
		logger.Error("push: refs written, store transaction of the ref-updated stages not committed",
			"workspace", req.Workspace.Slug, "error", err)
	}
	res.Outcome = OutcomeAccepted
	res.written = wr
	w.afterWrite(ctx, logger, deps.Heads, forkActivity, req)
	return res, nil
}

// forward runs the pending forwards of ds to origin, in command order, each
// under limit (DefaultForwardLimit when zero), and reports whether all of
// them went through. The first that fails, or has not finished when its
// limit passes, stops the request: no later forward runs, the earlier ones
// are left as origin has them, since a forward cannot be taken back, and
// the result refuses that update with the forward's message, or the
// timed-out one, as patch_branch, and the others as not updated. A forward
// is expected to end when its context does. When ctx itself ends, every
// update is refused as internal with ctx's error.
func (r Result) forward(ctx context.Context, limit time.Duration, ds []Decision) (Result, bool, error) {
	if limit <= 0 {
		limit = DefaultForwardLimit
	}
	for i, d := range ds {
		if d.Forward == nil {
			continue
		}
		fctx, cancel := context.WithTimeout(ctx, limit)
		err := d.Forward(fctx)
		timedOut := errors.Is(fctx.Err(), context.DeadlineExceeded)
		cancel()
		var message string
		switch {
		case ctx.Err() != nil:
			return r.internal(), false, ctx.Err()
		case timedOut:
			message = msgForwardTimedOut
		case err != nil:
			message = err.Error()
		default:
			continue
		}
		r.Outcome = OutcomeRefused
		for j := range r.Updates {
			u := r.Updates[j].Update
			r.Updates[j] = UpdateResult{Decision: Decision{Update: u, Message: msgNotUpdated}}
		}
		r.Updates[i].Decision = Decision{Update: d.Update, Message: message, ErrorType: ErrorTypePatchBranch}
		return r, false, nil
	}
	return r, true, nil
}

// commit is the commit step of an accepted request whose ref transaction tx
// is prepared: the stages run in one store transaction, in order, each
// version they report going to the update result at its index, and the
// refs move as the last step of that transaction. moved reports whether
// the refs moved; when they did not, err is why, a stage's error
// unchanged. When they did, err is the store's failure to commit.
func commit(ctx context.Context, transact func(context.Context, func(*sql.Tx) error) error, stages []RefUpdatedStage,
	tx *gitrepo.RefTx, in StageInput, results []UpdateResult) (moved bool, err error) {
	if transact == nil {
		if len(stages) > 0 {
			return false, errors.New("apply: ref-updated stages but no store to run them in")
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	var stageErr error
	err = transact(ctx, func(sqlTx *sql.Tx) error {
		for _, s := range stages {
			versions, err := s(ctx, sqlTx, in)
			if err != nil {
				stageErr = err
				return err
			}
			for i, v := range versions {
				if v != nil && i < len(results) && results[i].Version == nil {
					results[i].Version = v
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		moved = true
		return nil
	})
	switch {
	case moved:
		return true, err
	case stageErr != nil:
		return false, stageErr
	case err == nil:
		return false, errors.New("apply: the store transaction ended before the refs moved")
	}
	return false, err
}

// afterWrite reports an accepted request: the new head of a repository's
// default branch to the head_sha setter, and a write through git or the API
// to a fork to the fork activity recorder. Neither changes the answer, so
// a failure is logged.
func (w *Writer) afterWrite(ctx context.Context, logger *slog.Logger, heads HeadSetter, forkActivity ForkActivity, req ApplyRequest) {
	ws := req.Workspace
	if heads != nil && ws.Kind == access.KindRepository && ws.DefaultBranch != "" {
		for _, d := range req.Verdict.Decisions {
			if d.Ref == branchPrefix+ws.DefaultBranch && !d.Deleted {
				if err := heads.SetHead(ctx, ws.Slug, d.After); err != nil {
					logger.Error("push: head_sha not recorded", "workspace", ws.Slug, "error", err)
				}
			}
		}
	}
	if forkActivity != nil && ws.Kind == access.KindFork && (req.Via == ViaGit || req.Via == ViaAPI) {
		if err := forkActivity.RecordActivity(ctx, ws.Slug); err != nil {
			logger.Error("push: fork activity not recorded", "workspace", ws.Slug, "error", err)
		}
	}
}

// refUpdates are the refs of the transaction that writes ds, in git's
// terms, which name an absent ref "" rather than ZeroID.
func refUpdates(ds []Decision) []gitrepo.RefUpdate {
	us := make([]gitrepo.RefUpdate, len(ds))
	for i, d := range ds {
		us[i] = gitrepo.RefUpdate{Ref: d.Ref, Old: d.Before, New: d.After}
		if d.Created {
			us[i].Old = ""
		}
		if d.Deleted {
			us[i].New = ""
		}
	}
	return us
}

// refMoved answers that ref was not at its before value: it carries the
// message, and every other update the not-updated one.
func (r Result) refMoved(ref string) Result {
	r.Outcome = OutcomeRefMoved
	for i := range r.Updates {
		u := r.Updates[i].Update
		r.Updates[i] = UpdateResult{Decision: Decision{Update: u, Message: msgNotUpdated}}
		if u.Ref == ref {
			r.Updates[i].Decision = Decision{Update: u, Message: msgRefMoved, ErrorType: ErrorTypeRefMoved}
		}
	}
	return r
}

// internal answers that every update is refused as internal.
func (r Result) internal() Result {
	r.Outcome = OutcomeInternal
	for i := range r.Updates {
		r.Updates[i] = UpdateResult{Decision: internal.decision(r.Updates[i].Update)}
	}
	return r
}

// AllowedVerdict is the verdict that allows every update of updates
// without asking the rules, for a caller that decided by its own gates,
// such as landing, which moves protected branches the rules would refuse.
// Created, Deleted and Forced are computed as Evaluate computes them,
// asking view whether the before of an update that is neither a create nor
// a delete is an ancestor of its after; Direct is false and no update
// waits on a forward. An error is view's.
func AllowedVerdict(ctx context.Context, view View, updates []Update) (Verdict, error) {
	v := Verdict{Decisions: make([]Decision, 0, len(updates))}
	for _, u := range updates {
		d := Decision{Update: u, Allowed: true, Created: u.Before == ZeroID, Deleted: u.After == ZeroID}
		if !d.Created && !d.Deleted {
			if view == nil {
				return Verdict{}, errNoView
			}
			ancestor, err := view.IsAncestor(ctx, u.Before, u.After)
			if err != nil {
				return Verdict{}, fmt.Errorf("allowed verdict for %s: %w", u.Ref, err)
			}
			d.Forced = !ancestor
		}
		v.Decisions = append(v.Decisions, d)
	}
	return v, nil
}
