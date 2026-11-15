package pushrule

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/store"
)

// JournalStep creates the push journal in the transactional store: one row
// per accepted request whose refs are being written and whose events are
// not written yet. The ref writer adds the row before the refs move and
// removes it in the transaction that writes the request's events, so a hub
// that stops in between finds at its next start what it still owes.
//
// updates is the JSON list of the request's ref updates in command order,
// each with its flags; actor_id is null for the admin token, session_id
// for a write that belongs to no session.
var JournalStep = store.Step{
	Name:   "push_journal",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE push_journal (
	id            INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
	workspace     TEXT NOT NULL,
	parent_slug   TEXT,
	actor_type    TEXT NOT NULL,
	actor_id      TEXT,
	credential_id TEXT,
	session_id    TEXT,
	via           TEXT NOT NULL,
	updates       TEXT NOT NULL
)`)
		return err
	},
}

// JournalEntry is one request of the push journal: its ID, the slug of the
// workspace written to, its parent's slug for a task fork, the caller as
// the request's events name it (ActorType one of the Credential constants,
// ActorID the user, empty for the admin token), the credential's id, the
// session it belongs to (nil for none), the surface it came through and its
// ref updates in command order.
type JournalEntry struct {
	ID                               int64
	Workspace, Parent                string
	ActorType, ActorID, CredentialID string
	SessionID                        *string
	Via                              string
	Updates                          []JournalUpdate
}

// JournalUpdate is one ref update of a journal entry with the flags the
// ref writer wrote it with. Before is ZeroID for a create, After ZeroID for
// a delete.
type JournalUpdate struct {
	Ref, Before, After               string
	Created, Deleted, Forced, Direct bool
}

// journalUpdateJSON is how a JournalUpdate is stored.
type journalUpdateJSON struct {
	Ref     string `json:"ref"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Created bool   `json:"created"`
	Deleted bool   `json:"deleted"`
	Forced  bool   `json:"forced"`
	Direct  bool   `json:"direct"`
}

// Querier is what a read of the journal goes through: the store's *sql.DB
// or an open *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// InsertJournalEntry adds e to the journal in tx and returns the id it was
// stored under; e.ID is ignored.
func InsertJournalEntry(ctx context.Context, tx *sql.Tx, e JournalEntry) (int64, error) {
	stored := make([]journalUpdateJSON, len(e.Updates))
	for i, u := range e.Updates {
		stored[i] = journalUpdateJSON(u)
	}
	updates, err := json.Marshal(stored)
	if err != nil {
		return 0, fmt.Errorf("push journal: %w", err)
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO push_journal (workspace, parent_slug, actor_type, actor_id, credential_id, session_id, via, updates)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		e.Workspace, nullString(e.Parent), e.ActorType, nullString(e.ActorID), nullString(e.CredentialID),
		e.SessionID, e.Via, string(updates)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("push journal: add the entry of %s: %w", e.Workspace, err)
	}
	return id, nil
}

// JournalEntries reads every entry of the journal, oldest first.
func JournalEntries(ctx context.Context, q Querier) ([]JournalEntry, error) {
	rows, err := q.QueryContext(ctx, `
SELECT id, workspace, parent_slug, actor_type, actor_id, credential_id, session_id, via, updates
FROM push_journal ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("push journal: read: %w", err)
	}
	defer rows.Close()
	var entries []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var parent, actorID, credentialID, session sql.NullString
		var updates string
		if err := rows.Scan(&e.ID, &e.Workspace, &parent, &e.ActorType, &actorID, &credentialID, &session, &e.Via, &updates); err != nil {
			return nil, fmt.Errorf("push journal: read: %w", err)
		}
		e.Parent, e.ActorID, e.CredentialID = parent.String, actorID.String, credentialID.String
		if session.Valid {
			e.SessionID = &session.String
		}
		var stored []journalUpdateJSON
		if err := json.Unmarshal([]byte(updates), &stored); err != nil {
			return nil, fmt.Errorf("push journal: entry %d: %w", e.ID, err)
		}
		for _, u := range stored {
			e.Updates = append(e.Updates, JournalUpdate(u))
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("push journal: read: %w", err)
	}
	return entries, nil
}

// deleteJournalEntry removes the entry id in tx and reports whether it was
// there.
func deleteJournalEntry(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM push_journal WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("push journal: remove entry %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Reconcile settles what the journal holds; the hub runs it as it starts,
// before it serves. For each entry it reads the entry's refs: when every
// one holds its after value (is absent, for a delete), the request's refs
// moved and its events were never written, so they are written and the
// entry removed in one transaction; otherwise the refs never moved and the
// entry is removed without events. A ref that holds neither its before nor
// its after value was moved by another writer since the request was
// refused; that entry is removed without events too, and logged. An entry
// whose repository or transaction fails is logged and kept for the next
// start. An error is a failure to read the journal. Without an event log or
// a way to open the repositories it does nothing.
func (w *Writer) Reconcile(ctx context.Context) error {
	w.mu.RLock()
	deps := w.deps
	w.mu.RUnlock()
	if deps.Transact == nil || deps.Events == nil || deps.Repos == nil {
		return nil
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	var entries []JournalEntry
	if err := deps.Transact(ctx, func(tx *sql.Tx) (err error) {
		entries, err = JournalEntries(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	for _, e := range entries {
		moved, neither, err := refsMoved(ctx, deps.Repos, e)
		if err != nil {
			logger.Error("push journal: entry kept, its refs cannot be read", "workspace", e.Workspace, "journal_entry", e.ID, "error", err)
			continue
		}
		if neither {
			logger.Warn("push journal: entry dropped, its refs hold neither their before nor their after values",
				"workspace", e.Workspace, "journal_entry", e.ID)
		}
		err = deps.Transact(ctx, func(tx *sql.Tx) error {
			if moved {
				return settle(ctx, tx, deps.Events, e)
			}
			_, err := deleteJournalEntry(ctx, tx, e.ID)
			return err
		})
		if err != nil {
			logger.Error("push journal: entry kept, it cannot be settled", "workspace", e.Workspace, "journal_entry", e.ID, "error", err)
		}
	}
	return nil
}

// refsMoved reports whether every ref of e holds its after value, and,
// when not, whether some ref holds neither its after nor its before value.
func refsMoved(ctx context.Context, repos func(context.Context, string) (*gitrepo.Repo, error), e JournalEntry) (moved, neither bool, err error) {
	repo, err := repos(ctx, e.Workspace)
	if err != nil {
		return false, false, err
	}
	refs := make([]string, len(e.Updates))
	for i, u := range e.Updates {
		refs[i] = u.Ref
	}
	values, err := repo.RefValues(ctx, refs)
	if err != nil {
		return false, false, err
	}
	at := func(ref, id string) bool {
		got, exists := values[ref]
		if id == ZeroID {
			return !exists
		}
		return got == id
	}
	moved = true
	for _, u := range e.Updates {
		atAfter, atBefore := at(u.Ref, u.After), at(u.Ref, u.Before)
		moved = moved && atAfter
		neither = neither || !atAfter && !atBefore
	}
	return moved, neither && !moved, nil
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
