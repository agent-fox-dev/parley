package pushrule

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
)

// EventPush is the type of the event that records one accepted ref update.
const EventPush = "parley.git.push"

// EventWriter stores a hub event inside the caller's store transaction:
// the hub's event log, *event.Component, is one.
type EventWriter interface {
	Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error)
}

// PostPush is what a post-push listener is told of an accepted write: the
// slug of the workspace written to, the updates that moved, in command
// order, the surface the write came through and the session it belongs to,
// nil for none.
type PostPush struct {
	Workspace string
	Updates   []Decision
	Via       string
	SessionID *string
}

// PostPushListener is told of every accepted write of refs, whatever its
// surface, on a goroutine of its own once the write's events are written,
// so that the writer's caller does not wait for it. Its error is logged and
// changes nothing; so is a panic.
type PostPushListener func(ctx context.Context, p PostPush) error

// RegisterPostPushListener adds l after the listeners registered before
// it; every accepted write is reported to each of them by Finish, so the
// surfaces that write refs send no notification of their own.
func (w *Writer) RegisterPostPushListener(l PostPushListener) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.listeners = append(w.listeners, l)
}

// written is what an accepted request owes once its refs moved: its
// journal entry, whose ID is 0 when the writer keeps no journal, and its
// updates as the verdict allowed them.
type written struct {
	entry   JournalEntry
	updates []Decision
}

// Finish is the event step of an accepted request, which the caller of
// Apply runs with Apply's result once it is done with the moved refs; a
// push maps itself to changes in between. In one store transaction it
// writes one parley.git.push event per update, in command order, then the
// request's own events (ApplyRequest.Events), and removes the request's
// journal entry. When that transaction fails, the
// failure is logged and the entry kept, so the next start writes the
// events, and the request stays accepted. Then it tells every post-push
// listener, each on a goroutine of its own. A result that is not accepted,
// or that moved no ref, writes and tells nothing.
//
// The transaction runs to its end even when ctx ends: the refs have moved.
func (w *Writer) Finish(ctx context.Context, res Result) {
	wr := res.written
	if res.Refused() || wr == nil || len(wr.updates) == 0 {
		return
	}
	w.mu.RLock()
	deps, listeners := w.deps, w.listeners
	w.mu.RUnlock()
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx = context.WithoutCancel(ctx)
	if deps.Transact != nil && deps.Events != nil {
		if err := deps.Transact(ctx, func(tx *sql.Tx) error { return settle(ctx, tx, deps.Events, wr.entry) }); err != nil {
			logger.Error("push: events not written; the journal keeps them for the next start",
				"workspace", wr.entry.Workspace, "journal_entry", wr.entry.ID, "error", err)
		}
	}
	p := PostPush{Workspace: wr.entry.Workspace, Updates: wr.updates, Via: wr.entry.Via, SessionID: wr.entry.SessionID}
	for _, l := range listeners {
		go tell(ctx, logger, l, p)
	}
}

// tell runs one post-push listener, which can neither fail the write nor
// take the hub down.
func tell(ctx context.Context, logger *slog.Logger, l PostPushListener, p PostPush) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("push: post-push listener panicked", "workspace", p.Workspace, "panic", fmt.Sprint(r))
		}
	}()
	if err := l(ctx, p); err != nil {
		logger.Error("push: post-push listener failed", "workspace", p.Workspace, "error", err)
	}
}

// settle writes, in tx, the events of the journal entry e, its push events
// and then those it owes beside them, and removes the entry. An entry already removed, by an earlier call, has its events
// written already and is left alone.
func settle(ctx context.Context, tx *sql.Tx, events EventWriter, e JournalEntry) error {
	if e.ID != 0 {
		removed, err := deleteJournalEntry(ctx, tx, e.ID)
		if err != nil || !removed {
			return err
		}
	}
	for _, in := range append(pushEvents(e), e.Events...) {
		if _, err := events.Emit(ctx, tx, in); err != nil {
			return err
		}
	}
	return nil
}

// pushMetadata is the metadata of a parley.git.push event. Before is null
// for a create and After for a delete.
type pushMetadata struct {
	Ref     string  `json:"ref"`
	Before  *string `json:"before"`
	After   *string `json:"after"`
	Created bool    `json:"created"`
	Deleted bool    `json:"deleted"`
	Forced  bool    `json:"forced"`
	Direct  bool    `json:"direct"`
	Via     string  `json:"via"`
}

// pushEvents are the events of the journal entry e, one per update in
// command order. Finish and Reconcile both write them from the entry, so a
// request has the same events whenever they are written.
func pushEvents(e JournalEntry) []event.Input {
	ws := e.Workspace
	var actorID, before, after *string
	if e.ActorID != "" {
		actorID = &e.ActorID
	}
	ins := make([]event.Input, len(e.Updates))
	for i, u := range e.Updates {
		before, after = nil, nil
		if u.Before != ZeroID {
			before = &u.Before
		}
		if u.After != ZeroID {
			after = &u.After
		}
		ins[i] = event.Input{
			Type: EventPush, Workspace: &ws, ActorType: e.ActorType, ActorID: actorID,
			ResourceType: "ref", ResourceID: u.Ref, SessionID: e.SessionID, Parent: e.Parent,
			Metadata: pushMetadata{Ref: u.Ref, Before: before, After: after, Created: u.Created, Deleted: u.Deleted,
				Forced: u.Forced, Direct: u.Direct, Via: e.Via},
		}
	}
	return ins
}

// journalEntryOf is the journal entry of the request req: its caller as
// its events name it, the credential types being the event's actor types.
func journalEntryOf(req ApplyRequest) JournalEntry {
	e := JournalEntry{
		Workspace: req.Workspace.Slug, ActorType: req.Caller.CredentialType, ActorID: req.Caller.UserID,
		CredentialID: req.Caller.CredentialID, SessionID: req.SessionID, Via: req.Via,
		Updates: make([]JournalUpdate, len(req.Verdict.Decisions)), Events: req.Events,
	}
	if req.Workspace.Kind == access.KindFork {
		e.Parent = req.Workspace.Parent
	}
	if e.ActorType == CredentialAdminToken {
		e.ActorID = ""
	}
	for i, d := range req.Verdict.Decisions {
		e.Updates[i] = JournalUpdate{Ref: d.Ref, Before: d.Before, After: d.After, Created: d.Created, Deleted: d.Deleted, Forced: d.Forced, Direct: d.Direct}
	}
	return e
}
