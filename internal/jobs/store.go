package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// schema holds the jobs, one row per job keyed by its UUID. The documents
// are JSON text; payload is never null. Times are UnixNano integers, as the
// other tables keep them. jobs_pending serves the dedup of an enqueue and
// the "one running per type and workspace" rule of a claim; jobs_list
// serves a workspace's jobs of a type, newest first. Dedup has no unique
// index: a running job and its queued follow-up are both pending, and the
// write transactions of the store run one at a time, so the check and the
// insert of an enqueue are one step.
var schema = []string{
	`CREATE TABLE jobs (
	id TEXT PRIMARY KEY,
	type TEXT NOT NULL,
	workspace_slug TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'failed', 'completed', 'dead_letter', 'cancelled')),
	payload TEXT NOT NULL,
	result TEXT,
	detail TEXT,
	retry_count INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER,
	error TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	completed_at INTEGER
)`,
	`CREATE INDEX jobs_pending ON jobs (type, workspace_slug, status)`,
	`CREATE INDEX jobs_list ON jobs (workspace_slug, type, created_at, id)`,
}

// SchemaStep adds the job table.
var SchemaStep = store.Step{
	Name:   "jobs",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range schema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	},
}

// querier is what a read goes through: the store's *sql.DB, or the
// caller's open *sql.Tx so that the read sees what the transaction wrote.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pendingStatuses is the SQL list of the statuses Pending reports.
const pendingStatuses = `('queued', 'running', 'failed')`

// secondsOf is created_at, stored in nanoseconds, at the whole seconds a
// record shows it, so that the list's order is the order of the values a
// caller sees, and ties among them go by id.
const secondsOf = `(created_at / 1000000000)`

// jobColumns are the columns scanJob reads, in its order.
const jobColumns = `id, type, workspace_slug, status, payload, result, detail, retry_count, next_attempt_at, error,
	created_at, updated_at, completed_at`

// scanner is a *sql.Row or *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// scanJob reads a row of jobColumns.
func scanJob(s scanner) (Job, error) {
	var (
		j                       Job
		payload                 string
		result, detail, errText sql.NullString
		next, completed         sql.NullInt64
		created, updated        int64
	)
	err := s.Scan(&j.ID, &j.Type, &j.Workspace, &j.Status, &payload, &result, &detail, &j.RetryCount, &next, &errText,
		&created, &updated, &completed)
	if err != nil {
		return Job{}, err
	}
	j.Payload = json.RawMessage(payload)
	j.Result, j.Detail = document(result), document(detail)
	j.NextAttemptAt, j.CompletedAt = timeOf(next), timeOf(completed)
	if errText.Valid {
		j.Error = &errText.String
	}
	j.CreatedAt, j.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	return j, nil
}

// document is a nullable JSON column as a document, nil for NULL.
func document(s sql.NullString) json.RawMessage {
	if !s.Valid {
		return nil
	}
	return json.RawMessage(s.String)
}

// timeOf is a nullable UnixNano column as a time, nil for NULL.
func timeOf(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64).UTC()
	return &t
}

// readJob reads the job id through q; a job that does not exist is
// ErrNotFound.
func readJob(ctx context.Context, q querier, id string) (Job, error) {
	j, err := scanJob(q.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("read job %s: %w", id, err)
	}
	return j, nil
}

// pendingJobOf is the id of the oldest pending job of typ for slug other
// than except, or "" when there is none.
func pendingJobOf(ctx context.Context, q querier, typ, slug, except string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM jobs WHERE type = ? AND workspace_slug = ? AND status IN `+pendingStatuses+
		` AND id <> ? ORDER BY created_at, id LIMIT 1`, typ, slug, except).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the pending jobs of %s for %s: %w", typ, slug, err)
	}
	return id, nil
}

// insertJob stores j in tx.
func insertJob(ctx context.Context, tx *sql.Tx, j Job) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO jobs (id, type, workspace_slug, status, payload, retry_count, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, j.ID, j.Type, j.Workspace, j.Status, string(j.Payload), j.RetryCount,
		j.CreatedAt.UnixNano(), j.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store job %s: %w", j.ID, err)
	}
	return nil
}

// listJobs reads up to limit jobs of typ for slug through q, in status when
// it is not "", after the position after when it is not nil, ordered by
// created_at as shown, descending, then by id descending.
func listJobs(ctx context.Context, q querier, typ, slug, status string, after *ListPosition, limit int) ([]Job, error) {
	where := []string{`type = ?`, `workspace_slug = ?`}
	args := []any{typ, slug}
	if status != "" {
		where = append(where, `status = ?`)
		args = append(args, status)
	}
	if after != nil {
		where = append(where, `(`+secondsOf+` < ? OR (`+secondsOf+` = ? AND id < ?))`)
		args = append(args, after.Seconds, after.Seconds, after.ID)
	}
	rows, err := q.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE `+strings.Join(where, ` AND `)+
		` ORDER BY `+secondsOf+` DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list the jobs of %s for %s: %w", typ, slug, err)
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("list the jobs of %s for %s: %w", typ, slug, err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the jobs of %s for %s: %w", typ, slug, err)
	}
	return jobs, nil
}
