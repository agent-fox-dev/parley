package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
)

// SchemaStep creates the sessions, their usage records and their links in
// the transactional store, so that a link or a forced close commits or
// rolls back with another package's transaction. None of them carries a
// foreign key to the workspaces: a session, its usage and its links outlive
// the workspace they name. Ids are stored in lower case and timestamps as
// RFC 3339 in UTC with whole seconds, so that their text sorts as their
// time does.
var SchemaStep = store.Step{
	Name:   "agent_sessions",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE sessions (
	id                          TEXT NOT NULL PRIMARY KEY,
	workspace_slug              TEXT NOT NULL,
	parent_slug                 TEXT,
	run_id                      TEXT,
	node_id                     TEXT,
	archetype                   TEXT,
	model                       TEXT,
	status                      TEXT NOT NULL CHECK (status IN ('active', 'completed', 'failed', 'timeout', 'terminated')),
	user_id                     TEXT,
	credential_type             TEXT NOT NULL CHECK (credential_type IN ('admin_token', 'api_key', 'pat')),
	started_at                  TEXT NOT NULL,
	ended_at                    TEXT,
	duration_ms                 INTEGER,
	error_message               TEXT,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
	metadata                    TEXT
);
CREATE INDEX sessions_started ON sessions (started_at, id);
CREATE INDEX sessions_workspace ON sessions (workspace_slug);
CREATE INDEX sessions_parent ON sessions (parent_slug);
CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE session_usage (
	id                TEXT NOT NULL PRIMARY KEY,
	session_id        TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	workspace_slug    TEXT NOT NULL,
	model             TEXT NOT NULL,
	input_tokens      INTEGER NOT NULL DEFAULT 0,
	output_tokens     INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	reported_at       TEXT NOT NULL
);
CREATE INDEX session_usage_session ON session_usage (session_id, reported_at, id);
CREATE INDEX session_usage_workspace ON session_usage (workspace_slug, reported_at);

CREATE TABLE session_links (
	id         INTEGER PRIMARY KEY,
	session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	kind       TEXT NOT NULL CHECK (kind IN ('fork', 'sandbox', 'version', 'push', 'commit', 'claim', 'land')),
	workspace  TEXT NOT NULL,
	link_key   TEXT NOT NULL,
	number     INTEGER,
	version    INTEGER,
	extra      TEXT,
	created_at TEXT NOT NULL,
	UNIQUE (session_id, kind, workspace, link_key)
);`)
		return err
	},
}

// Querier is what a read goes through: the store's *sql.DB or an open
// *sql.Tx, so that a read inside a transaction sees what it wrote.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// row is a stored session. ParentSlug is "" for a session on a repository;
// UserID is "" for one the admin token opened.
type row struct {
	ID, WorkspaceSlug, ParentSlug   string
	RunID, NodeID, Archetype, Model *string
	Status, UserID, CredentialType  string
	StartedAt                       time.Time
	EndedAt                         *time.Time
	DurationMS                      *int64
	ErrorMessage                    *string
	CacheCreationInputTokens        int64
	Metadata                        json.RawMessage
}

// The statuses of a session: active from its creation; completed, failed
// or timeout by a complete; terminated by a forced close.
const (
	StatusActive     = "active"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusTimeout    = "timeout"
	StatusTerminated = "terminated"
)

// The credential types a session records of its creator.
const (
	CredentialAdminToken = "admin_token"
	CredentialAPIKey     = "api_key"
	CredentialPAT        = "pat"
)

// rowColumns are the columns scanRow reads, in its order.
const rowColumns = `id, workspace_slug, parent_slug, run_id, node_id, archetype, model, status, user_id, credential_type,
	started_at, ended_at, duration_ms, error_message, cache_creation_input_tokens, metadata`

// readRow reads the session id (in lower case) through q; found is false
// when there is none.
func readRow(ctx context.Context, q Querier, id string) (row, bool, error) {
	var (
		r                             row
		parent, user, ended, metadata sql.NullString
		started                       string
	)
	err := q.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM sessions WHERE id = ?`, id).Scan(
		&r.ID, &r.WorkspaceSlug, &parent, &r.RunID, &r.NodeID, &r.Archetype, &r.Model, &r.Status, &user, &r.CredentialType,
		&started, &ended, &r.DurationMS, &r.ErrorMessage, &r.CacheCreationInputTokens, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, false, nil
	}
	if err != nil {
		return row{}, false, fmt.Errorf("read session %s: %w", id, err)
	}
	r.ParentSlug, r.UserID = parent.String, user.String
	if r.StartedAt, err = time.Parse(time.RFC3339, started); err != nil {
		return row{}, false, fmt.Errorf("read session %s: started_at: %w", id, err)
	}
	if ended.Valid {
		t, err := time.Parse(time.RFC3339, ended.String)
		if err != nil {
			return row{}, false, fmt.Errorf("read session %s: ended_at: %w", id, err)
		}
		r.EndedAt = &t
	}
	if metadata.Valid {
		r.Metadata = json.RawMessage(metadata.String)
	}
	return r, true, nil
}

// insertRow stores the new session r in tx.
func insertRow(ctx context.Context, tx *sql.Tx, r row) error {
	var metadata any
	if r.Metadata != nil {
		metadata = string(r.Metadata)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO sessions (`+rowColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkspaceSlug, nullable(r.ParentSlug), r.RunID, r.NodeID, r.Archetype, r.Model, r.Status, nullable(r.UserID),
		r.CredentialType, timefmt.Format(r.StartedAt), formatOptional(r.EndedAt), r.DurationMS, r.ErrorMessage,
		r.CacheCreationInputTokens, metadata)
	if err != nil {
		return fmt.Errorf("store session %s: %w", r.ID, err)
	}
	return nil
}

// sessionExists reports whether the session id (in lower case) is stored,
// reading through q.
func sessionExists(ctx context.Context, q Querier, id string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read session %s: %w", id, err)
	}
	return true, nil
}

// insertLink stores the link l, recorded at, in tx, unless a link of the
// same session, kind, workspace and key is stored already. Only a version
// link stores its change and version numbers.
func insertLink(ctx context.Context, tx *sql.Tx, l Link, at time.Time) error {
	var number, version any
	if l.Kind == KindVersion {
		number, version = l.Number, l.Version
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO session_links (session_id, kind, workspace, link_key, number, version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (session_id, kind, workspace, link_key) DO NOTHING`,
		l.SessionID, string(l.Kind), l.Workspace, l.Key, number, version, timefmt.Format(at))
	if err != nil {
		return fmt.Errorf("store the %s link %s of session %s: %w", l.Kind, l.Key, l.SessionID, err)
	}
	return nil
}

// nullable is s, or NULL for "".
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// formatOptional is t as a stored timestamp, or NULL for nil.
func formatOptional(t *time.Time) any {
	if t == nil {
		return nil
	}
	return timefmt.Format(*t)
}

// maxListed is the most entries the forks, changes and sandboxes of a
// session object hold; the counts hold the true totals.
const maxListed = 500

// object builds the session object of r, reading its usage records and its
// links from db when it is called, so that a usage report or a recorded
// link shows in the next answer. They are read in one snapshot, so that
// the token summary, the lists and the counts agree with each other while
// links and reports arrive. Every route answers a session with it.
func object(ctx context.Context, db *sql.DB, r row) (Object, error) {
	o := Object{
		ID: r.ID, WorkspaceSlug: r.WorkspaceSlug, RunID: r.RunID, NodeID: r.NodeID, Archetype: r.Archetype, Model: r.Model,
		Status: r.Status, CredentialType: r.CredentialType, StartedAt: timefmt.Time(r.StartedAt),
		DurationMS: r.DurationMS, ErrorMessage: r.ErrorMessage, CacheCreationInputTokens: r.CacheCreationInputTokens,
		Metadata: r.Metadata, Forks: []string{}, Changes: []ChangeRef{}, Sandboxes: []SandboxRef{},
	}
	if r.UserID != "" {
		o.UserID = &r.UserID
	}
	if r.EndedAt != nil {
		ended := timefmt.Time(*r.EndedAt)
		o.EndedAt = &ended
	}
	err := snapshot(ctx, db, func(tx *sql.Tx) error {
		var err error
		if o.TokenSummary, err = tokenSummary(ctx, tx, r.ID); err != nil {
			return err
		}
		return readLinks(ctx, tx, r.ID, &o)
	})
	if err != nil {
		return Object{}, err
	}
	return o, nil
}

// snapshot runs fn in a read-only transaction of db, which sees one
// snapshot of the store. A read-only transaction begins deferred, not with
// the write lock the store's other transactions take (_txlock=immediate),
// so that it neither waits for a writer nor holds one up.
func snapshot(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin a read: %w", err)
	}
	defer tx.Rollback()
	return fn(tx)
}

// tokenSummary sums the usage records of the session id.
func tokenSummary(ctx context.Context, q Querier, id string) (TokenSummary, error) {
	s := TokenSummary{ModelsUsed: []string{}}
	err := q.QueryRowContext(ctx, `
SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_read_tokens), 0)
FROM session_usage WHERE session_id = ?`, id).Scan(&s.TotalInputTokens, &s.TotalOutputTokens, &s.TotalCacheReadTokens)
	if err != nil {
		return TokenSummary{}, fmt.Errorf("sum the usage of session %s: %w", id, err)
	}
	err = eachRow(ctx, q, func(scan func(...any) error) error {
		var model string
		if err := scan(&model); err != nil {
			return err
		}
		s.ModelsUsed = append(s.ModelsUsed, model)
		return nil
	}, `SELECT DISTINCT model FROM session_usage WHERE session_id = ? ORDER BY model`, id)
	if err != nil {
		return TokenSummary{}, fmt.Errorf("read the models of session %s: %w", id, err)
	}
	return s, nil
}

// readLinks fills the forks, changes, sandboxes and counts of o from the
// links of the session id: the lists oldest first, cut to maxListed, the
// counts the true totals.
func readLinks(ctx context.Context, q Querier, id string, o *Object) error {
	err := eachRow(ctx, q, func(scan func(...any) error) error {
		var kind string
		var n int64
		if err := scan(&kind, &n); err != nil {
			return err
		}
		o.Counts.set(Kind(kind), n)
		return nil
	}, `SELECT kind, COUNT(*) FROM session_links WHERE session_id = ? GROUP BY kind`, id)
	if err != nil {
		return fmt.Errorf("count the links of session %s: %w", id, err)
	}
	err = q.QueryRowContext(ctx, `
SELECT COUNT(*) FROM (SELECT DISTINCT workspace, number FROM session_links WHERE session_id = ? AND kind = 'version')`,
		id).Scan(&o.Counts.Changes)
	if err != nil {
		return fmt.Errorf("count the changes of session %s: %w", id, err)
	}
	if o.Counts.Forks > 0 {
		err = eachRow(ctx, q, func(scan func(...any) error) error {
			var slug string
			if err := scan(&slug); err != nil {
				return err
			}
			o.Forks = append(o.Forks, slug)
			return nil
		}, `SELECT link_key FROM session_links WHERE session_id = ? AND kind = 'fork' ORDER BY id LIMIT ?`, id, maxListed)
		if err != nil {
			return fmt.Errorf("read the forks of session %s: %w", id, err)
		}
	}
	if o.Counts.Sandboxes > 0 {
		err = eachRow(ctx, q, func(scan func(...any) error) error {
			var s SandboxRef
			if err := scan(&s.Workspace, &s.ID); err != nil {
				return err
			}
			o.Sandboxes = append(o.Sandboxes, s)
			return nil
		}, `SELECT workspace, link_key FROM session_links WHERE session_id = ? AND kind = 'sandbox' ORDER BY id LIMIT ?`, id, maxListed)
		if err != nil {
			return fmt.Errorf("read the sandboxes of session %s: %w", id, err)
		}
	}
	if o.Counts.Changes > 0 {
		err = eachRow(ctx, q, func(scan func(...any) error) error {
			var c ChangeRef
			if err := scan(&c.Workspace, &c.Number); err != nil {
				return err
			}
			o.Changes = append(o.Changes, c)
			return nil
		}, `
SELECT workspace, number FROM session_links WHERE session_id = ? AND kind = 'version'
GROUP BY workspace, number ORDER BY MIN(id) LIMIT ?`, id, maxListed)
		if err != nil {
			return fmt.Errorf("read the changes of session %s: %w", id, err)
		}
	}
	return nil
}

// eachRow runs query with args through q and calls fn with the scan of each
// row in turn.
func eachRow(ctx context.Context, q Querier, fn func(scan func(...any) error) error, query string, args ...any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}
