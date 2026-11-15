package land

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/store"
)

// Attempt is one request to land a change, or a stack of changes, with its
// outcome. It is created running and finished once, in one of the final
// statuses; a final attempt never changes.
type Attempt struct {
	// ID is the attempt's UUID.
	ID string
	// Workspace is the slug of the workspace whose target branch the
	// attempt lands on, which holds the changes.
	Workspace    string
	TargetBranch string
	// Covered are the changes the attempt lands, bottom first, each with
	// the version it had when the attempt was created: a version that
	// arrives later is not landed by this attempt.
	Covered  []CoveredChange
	Status   string
	Trigger  string
	Strategy string
	// Attempts counts the tries to move the target.
	Attempts int
	// OntoSHA is the target head the commits were landed on, and
	// LandedSHA the head the land made it; nil until the attempt landed.
	OntoSHA, LandedSHA *string
	// Noop is set for a land whose commits were all on the target already.
	Noop bool
	// Commits are each landed commit with the version commit it came from.
	Commits []Commit
	// Conflict is the report of an attempt that ended conflict.
	Conflict *gitrepo.ConflictReport
	// Blocked names the change of an attempt that ended blocked.
	Blocked *Blocked
	// Error is the reason of an attempt that ended failed, one of the
	// Error constants.
	Error *string
	// Override is the override the attempt applied, nil for none.
	Override *Override
	// PolicyRevision is the revision of the policy that judged the
	// attempt, nil until it was judged.
	PolicyRevision *int64
	// RequestedBy is who asked for the land, and CredentialID the
	// credential they asked with, which the attempt object does not show.
	RequestedBy  change.Actor
	CredentialID string
	SessionID    *string
	CreatedAt    time.Time
	// FinishedAt is when the attempt ended, nil while it runs.
	FinishedAt *time.Time
}

// CoveredChange is a change an attempt lands, as it was when the attempt
// was created: its number and version, the head and base of that version,
// and the source it came from.
type CoveredChange struct {
	Number, Version int64
	Head, Base      string
	// SourceWorkspace and SourceBranch are the change's source.
	SourceWorkspace, SourceBranch string
}

// Numbers are the numbers of the changes the attempt covers, bottom first.
func (a Attempt) Numbers() []int64 {
	out := make([]int64, len(a.Covered))
	for i, c := range a.Covered {
		out[i] = c.Number
	}
	return out
}

// Commit is one commit a land added to the target: the change it belongs
// to, the commit of the change's version and the commit it landed as.
type Commit struct {
	Change     int64  `json:"change"`
	VersionSHA string `json:"version_sha"`
	LandedSHA  string `json:"landed_sha"`
}

// Blocked is the change whose landing requirements were not met, with the
// status the policy gave it and its requirements.
type Blocked struct {
	Change       int64             `json:"change"`
	Status       string            `json:"status"`
	Requirements []json.RawMessage `json:"requirements"`
}

// MarshalJSON writes the blocked change with requirements never null.
func (b Blocked) MarshalJSON() ([]byte, error) {
	type plain Blocked
	if b.Requirements == nil {
		b.Requirements = []json.RawMessage{}
	}
	return json.Marshal(plain(b))
}

// Override is the override a land request carried, with its reason.
type Override struct {
	Reason string `json:"reason"`
}

// MarshalJSON writes the attempt object: every member present, null where
// the attempt has no value, commits never null and times in whole seconds
// UTC.
func (a Attempt) MarshalJSON() ([]byte, error) {
	commits := a.Commits
	if commits == nil {
		commits = []Commit{}
	}
	var finishedAt *timefmt.Time
	if a.FinishedAt != nil {
		finishedAt = new(timefmt.Time(*a.FinishedAt))
	}
	return json.Marshal(struct {
		ID             string                  `json:"id"`
		Workspace      string                  `json:"workspace_slug"`
		TargetBranch   string                  `json:"target_branch"`
		Changes        []int64                 `json:"changes"`
		Status         string                  `json:"status"`
		Trigger        string                  `json:"trigger"`
		Strategy       string                  `json:"strategy"`
		Attempts       int                     `json:"attempts"`
		OntoSHA        *string                 `json:"onto_sha"`
		LandedSHA      *string                 `json:"landed_sha"`
		Noop           bool                    `json:"noop"`
		Commits        []Commit                `json:"commits"`
		Conflict       *gitrepo.ConflictReport `json:"conflict"`
		Blocked        *Blocked                `json:"blocked"`
		Error          *string                 `json:"error"`
		Override       *Override               `json:"override"`
		PolicyRevision *int64                  `json:"policy_revision"`
		RequestedBy    change.Actor            `json:"requested_by"`
		SessionID      *string                 `json:"session_id"`
		CreatedAt      timefmt.Time            `json:"created_at"`
		FinishedAt     *timefmt.Time           `json:"finished_at"`
	}{
		a.ID, a.Workspace, a.TargetBranch, a.Numbers(), a.Status, a.Trigger, a.Strategy, a.Attempts, a.OntoSHA, a.LandedSHA,
		a.Noop, commits, a.Conflict, a.Blocked, a.Error, a.Override, a.PolicyRevision, a.RequestedBy, a.SessionID,
		timefmt.Time(a.CreatedAt), finishedAt,
	})
}

// schema is the store schema of the attempts: one row per attempt, its
// result members as JSON, and one row per change it covers, in order. The
// index on a workspace's statuses finds its running attempts, and the one
// on the covered numbers the attempts of a change.
var schema = []string{
	`CREATE TABLE land_attempts (
	id TEXT PRIMARY KEY,
	workspace_slug TEXT NOT NULL,
	target_branch TEXT NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('running', 'landed', 'conflict', 'blocked', 'failed')),
	trigger TEXT NOT NULL,
	strategy TEXT NOT NULL,
	attempts INTEGER NOT NULL,
	onto_sha TEXT,
	landed_sha TEXT,
	noop INTEGER NOT NULL,
	commits TEXT NOT NULL,
	conflict TEXT,
	blocked TEXT,
	error TEXT,
	override TEXT,
	policy_revision INTEGER,
	actor_type TEXT NOT NULL,
	actor_id TEXT,
	username TEXT,
	credential_id TEXT NOT NULL,
	session_id TEXT,
	created_at INTEGER NOT NULL,
	finished_at INTEGER
)`,
	`CREATE INDEX land_attempts_status ON land_attempts (workspace_slug, status)`,
	`CREATE INDEX land_attempts_created ON land_attempts (workspace_slug, created_at)`,
	`CREATE TABLE land_attempt_changes (
	land_id TEXT NOT NULL REFERENCES land_attempts (id) ON DELETE CASCADE,
	position INTEGER NOT NULL,
	number INTEGER NOT NULL,
	version INTEGER NOT NULL,
	head_sha TEXT NOT NULL,
	base_sha TEXT NOT NULL,
	source_workspace TEXT NOT NULL,
	source_branch TEXT NOT NULL,
	PRIMARY KEY (land_id, position)
)`,
	`CREATE INDEX land_attempt_changes_number ON land_attempt_changes (number)`,
}

// SchemaStep creates the store schema of the land attempts.
var SchemaStep = store.Step{
	Name:   "land_attempts",
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

// errFinal is the error of finishAttempt for an attempt that is not
// running: final already, or unknown.
var errFinal = errors.New("the land attempt is not running")

// results are the result members of a, as the store keeps them: commits,
// never null, and conflict, blocked and override as JSON, NULL when a has
// none.
func results(a Attempt) (commits string, conflict, blocked, override *string, err error) {
	if a.Commits == nil {
		a.Commits = []Commit{}
	}
	raw, err := json.Marshal(a.Commits)
	if err != nil {
		return "", nil, nil, nil, err
	}
	if a.Conflict != nil {
		conflict, err = jsonOf(a.Conflict)
	}
	if a.Blocked != nil && err == nil {
		blocked, err = jsonOf(a.Blocked)
	}
	if a.Override != nil && err == nil {
		override, err = jsonOf(a.Override)
	}
	return string(raw), conflict, blocked, override, err
}

// jsonOf is v as JSON text.
func jsonOf(v any) (*string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return new(string(raw)), nil
}

// nanos is t in nanoseconds since the epoch, NULL for nil.
func nanos(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	return new(t.UnixNano())
}

// insertAttempt stores the new attempt a, with the changes it covers, in
// tx.
func insertAttempt(ctx context.Context, tx *sql.Tx, a Attempt) error {
	commits, conflict, blocked, override, err := results(a)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO land_attempts (id, workspace_slug, target_branch, status, trigger, strategy,
	attempts, onto_sha, landed_sha, noop, commits, conflict, blocked, error, override, policy_revision, actor_type,
	actor_id, username, credential_id, session_id, created_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Workspace, a.TargetBranch, a.Status, a.Trigger, a.Strategy, a.Attempts, a.OntoSHA, a.LandedSHA, a.Noop,
		commits, conflict, blocked, a.Error, override, a.PolicyRevision, a.RequestedBy.ActorType, a.RequestedBy.ActorID,
		a.RequestedBy.Username, a.CredentialID, a.SessionID, a.CreatedAt.UnixNano(), nanos(a.FinishedAt))
	if err != nil {
		return fmt.Errorf("store land attempt %s: %w", a.ID, err)
	}
	for i, c := range a.Covered {
		if _, err := tx.ExecContext(ctx, `INSERT INTO land_attempt_changes (land_id, position, number, version, head_sha,
	base_sha, source_workspace, source_branch) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, i, c.Number, c.Version, c.Head, c.Base, c.SourceWorkspace, c.SourceBranch); err != nil {
			return fmt.Errorf("store the changes of land attempt %s: %w", a.ID, err)
		}
	}
	return nil
}

// finishAttempt writes, in tx, the final status of a with its result
// members and its finished_at, in one write that only a running attempt
// takes: an attempt that is final already, or that the workspace does not
// have, is errFinal and changes nothing.
func finishAttempt(ctx context.Context, tx *sql.Tx, a Attempt) error {
	if a.Status == StatusRunning || !slices.Contains(statuses, a.Status) || a.FinishedAt == nil {
		return fmt.Errorf("finish land attempt %s: %q with finished_at %v is no final status", a.ID, a.Status, a.FinishedAt)
	}
	commits, conflict, blocked, override, err := results(a)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE land_attempts SET status = ?, attempts = ?, onto_sha = ?, landed_sha = ?,
	noop = ?, commits = ?, conflict = ?, blocked = ?, error = ?, override = ?, policy_revision = ?, finished_at = ?
WHERE id = ? AND workspace_slug = ? AND status = 'running'`,
		a.Status, a.Attempts, a.OntoSHA, a.LandedSHA, a.Noop, commits, conflict, blocked, a.Error, override,
		a.PolicyRevision, nanos(a.FinishedAt), a.ID, a.Workspace)
	if err != nil {
		return fmt.Errorf("finish land attempt %s: %w", a.ID, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.Join(errFinal, err)
	}
	return nil
}

// attemptColumns are the columns readAttempts scans, of the table a.
const attemptColumns = `a.id, a.workspace_slug, a.target_branch, a.status, a.trigger, a.strategy, a.attempts, a.onto_sha,
	a.landed_sha, a.noop, a.commits, a.conflict, a.blocked, a.error, a.override, a.policy_revision, a.actor_type,
	a.actor_id, a.username, a.credential_id, a.session_id, a.created_at, a.finished_at`

// readAttempt reads the attempt id of the workspace through q; found is
// false for an id the workspace has no attempt of.
func readAttempt(ctx context.Context, q Querier, workspace, id string) (Attempt, bool, error) {
	as, err := readAttempts(ctx, q, `SELECT `+attemptColumns+` FROM land_attempts a WHERE a.workspace_slug = ? AND a.id = ?`,
		workspace, id)
	if err != nil || len(as) == 0 {
		return Attempt{}, false, err
	}
	return as[0], true, nil
}

// readAttempts runs query, which selects attemptColumns, and returns the
// attempts it selects in its order, each with the changes it covers.
func readAttempts(ctx context.Context, q Querier, query string, args ...any) ([]Attempt, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read land attempts: %w", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read land attempts: %w", err)
	}
	for i := range out {
		if out[i].Covered, err = readCovered(ctx, q, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanAttempt reads one row of attemptColumns.
func scanAttempt(rows *sql.Rows) (Attempt, error) {
	var a Attempt
	var commits string
	var conflict, blocked, override sql.NullString
	var created int64
	var finished *int64
	if err := rows.Scan(&a.ID, &a.Workspace, &a.TargetBranch, &a.Status, &a.Trigger, &a.Strategy, &a.Attempts, &a.OntoSHA,
		&a.LandedSHA, &a.Noop, &commits, &conflict, &blocked, &a.Error, &override, &a.PolicyRevision,
		&a.RequestedBy.ActorType, &a.RequestedBy.ActorID, &a.RequestedBy.Username, &a.CredentialID, &a.SessionID,
		&created, &finished); err != nil {
		return Attempt{}, fmt.Errorf("read a land attempt: %w", err)
	}
	a.CreatedAt = time.Unix(0, created).UTC()
	if finished != nil {
		a.FinishedAt = new(time.Unix(0, *finished).UTC())
	}
	if err := json.Unmarshal([]byte(commits), &a.Commits); err != nil {
		return Attempt{}, fmt.Errorf("read the commits of land attempt %s: %w", a.ID, err)
	}
	for _, m := range []struct {
		raw sql.NullString
		dst any
	}{{conflict, &a.Conflict}, {blocked, &a.Blocked}, {override, &a.Override}} {
		if !m.raw.Valid {
			continue
		}
		if err := json.Unmarshal([]byte(m.raw.String), m.dst); err != nil {
			return Attempt{}, fmt.Errorf("read land attempt %s: %w", a.ID, err)
		}
	}
	return a, nil
}

// readCovered reads the changes the attempt id covers, bottom first.
func readCovered(ctx context.Context, q Querier, id string) ([]CoveredChange, error) {
	rows, err := q.QueryContext(ctx, `SELECT number, version, head_sha, base_sha, source_workspace, source_branch
FROM land_attempt_changes WHERE land_id = ? ORDER BY position`, id)
	if err != nil {
		return nil, fmt.Errorf("read the changes of land attempt %s: %w", id, err)
	}
	defer rows.Close()
	var out []CoveredChange
	for rows.Next() {
		var c CoveredChange
		if err := rows.Scan(&c.Number, &c.Version, &c.Head, &c.Base, &c.SourceWorkspace, &c.SourceBranch); err != nil {
			return nil, fmt.Errorf("read the changes of land attempt %s: %w", id, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WorkspaceDeleted deletes, in tx, the land attempts of the workspace,
// with the changes they cover, as the workspace's teardown removes the
// data kept for it (docs/adr/07-register-the-teardown-collaborators-on-the-workspace-service.md).
func (c *Component) WorkspaceDeleted(ctx context.Context, tx *sql.Tx, workspace string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM land_attempts WHERE workspace_slug = ?`, workspace); err != nil {
		return fmt.Errorf("delete the land attempts of %s: %w", workspace, err)
	}
	return nil
}
