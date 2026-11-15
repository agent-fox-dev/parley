package change

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/agent-fox-dev/parley/internal/store"
)

// schema is the change record. A change is keyed by its workspace (the
// target) and number; its versions, the sessions that contributed one, in
// the order of their first version, and the commits it landed hang off it
// and go with it. change_counters keeps the last number given in each
// workspace, so that a number is never given twice, also after the change
// that held it was abandoned. Times are UnixNano integers, as the other
// tables keep them; lists and objects are JSON. At most one draft or open
// change has a given source branch.
var schema = []string{
	`CREATE TABLE changes (
	workspace_slug TEXT NOT NULL,
	number INTEGER NOT NULL,
	title TEXT NOT NULL,
	intent TEXT NOT NULL,
	state TEXT NOT NULL CHECK (state IN ('draft', 'open', 'landed', 'abandoned')),
	abandon_reason TEXT,
	target_branch TEXT NOT NULL,
	source_workspace TEXT NOT NULL,
	source_branch TEXT NOT NULL,
	version INTEGER NOT NULL,
	head_sha TEXT NOT NULL,
	base_sha TEXT NOT NULL,
	patch_id TEXT NOT NULL,
	depends_on INTEGER,
	author_type TEXT NOT NULL,
	author_id TEXT,
	author_username TEXT,
	session_id TEXT,
	mergeable TEXT NOT NULL CHECK (mergeable IN ('clean', 'conflict', 'unknown')),
	conflict TEXT,
	needs_restack INTEGER NOT NULL DEFAULT 0,
	restack_reason TEXT,
	commits INTEGER NOT NULL,
	files_changed INTEGER NOT NULL,
	insertions INTEGER NOT NULL,
	deletions INTEGER NOT NULL,
	paths TEXT NOT NULL,
	paths_truncated INTEGER NOT NULL,
	landing TEXT NOT NULL,
	auto_land INTEGER NOT NULL DEFAULT 0,
	labels TEXT NOT NULL,
	landed TEXT,
	reverts INTEGER,
	reverted_by INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (workspace_slug, number)
)`,
	`CREATE UNIQUE INDEX changes_active_source ON changes (source_workspace, source_branch) WHERE state IN ('draft', 'open')`,
	`CREATE INDEX changes_source ON changes (source_workspace)`,
	`CREATE TABLE change_versions (
	workspace_slug TEXT NOT NULL,
	number INTEGER NOT NULL,
	version INTEGER NOT NULL,
	head_sha TEXT NOT NULL,
	base_sha TEXT NOT NULL,
	kind TEXT NOT NULL CHECK (kind IN ('push', 'commit', 'ref', 'rebase', 'restack', 'revert', 'sync')),
	patch_id TEXT NOT NULL,
	patch_equivalent_to INTEGER,
	actor_type TEXT NOT NULL,
	actor_id TEXT,
	actor_username TEXT,
	credential_type TEXT,
	credential_id TEXT,
	session_id TEXT,
	commits INTEGER NOT NULL,
	files_changed INTEGER NOT NULL,
	insertions INTEGER NOT NULL,
	deletions INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (workspace_slug, number, version),
	FOREIGN KEY (workspace_slug, number) REFERENCES changes (workspace_slug, number) ON DELETE CASCADE
)`,
	`CREATE INDEX change_versions_head ON change_versions (head_sha)`,
	`CREATE TABLE change_sessions (
	workspace_slug TEXT NOT NULL,
	number INTEGER NOT NULL,
	position INTEGER NOT NULL,
	session_id TEXT NOT NULL,
	PRIMARY KEY (workspace_slug, number, session_id),
	FOREIGN KEY (workspace_slug, number) REFERENCES changes (workspace_slug, number) ON DELETE CASCADE
)`,
	`CREATE INDEX change_sessions_session ON change_sessions (session_id)`,
	`CREATE TABLE change_landed_commits (
	workspace_slug TEXT NOT NULL,
	number INTEGER NOT NULL,
	position INTEGER NOT NULL,
	version_sha TEXT NOT NULL,
	landed_sha TEXT NOT NULL,
	PRIMARY KEY (workspace_slug, number, position),
	FOREIGN KEY (workspace_slug, number) REFERENCES changes (workspace_slug, number) ON DELETE CASCADE
)`,
	`CREATE INDEX change_landed_commits_landed ON change_landed_commits (landed_sha)`,
	`CREATE TABLE change_counters (
	workspace_slug TEXT PRIMARY KEY,
	last_number INTEGER NOT NULL
)`,
}

// SchemaStep creates the change record.
var SchemaStep = store.Step{
	Name:   "changes",
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

// nextNumber gives the next number of the workspace's changes.
func nextNumber(ctx context.Context, tx *sql.Tx, workspace string) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, `INSERT INTO change_counters (workspace_slug, last_number) VALUES (?, 1)
ON CONFLICT (workspace_slug) DO UPDATE SET last_number = last_number + 1 RETURNING last_number`, workspace).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("number a change of %q: %w", workspace, err)
	}
	return n, nil
}

// errSourceTaken is the error of insertChange for a source branch another
// draft or open change has.
var errSourceTaken = errors.New("another draft or open change has this source")

// insertChange stores the new change ch with its current version v, whose
// stats and patch id it copies. A source branch another draft or open
// change has is errSourceTaken.
func insertChange(ctx context.Context, tx *sql.Tx, ch Change, v Version) error {
	paths, err := json.Marshal(nonNil(ch.Paths))
	if err != nil {
		return err
	}
	labels, err := json.Marshal(nonNil(ch.Labels))
	if err != nil {
		return err
	}
	landing, err := json.Marshal(ch.Landing)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO changes (workspace_slug, number, title, intent, state, target_branch,
	source_workspace, source_branch, version, head_sha, base_sha, patch_id, depends_on, author_type, author_id,
	author_username, session_id, mergeable, needs_restack, commits, files_changed, insertions, deletions, paths,
	paths_truncated, landing, auto_land, labels, reverts, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ch.WorkspaceSlug, ch.Number, ch.Title, ch.Intent, ch.State, ch.TargetBranch,
		ch.Source.WorkspaceSlug, ch.Source.Branch, ch.Version, ch.HeadSHA, baseColumn(ch.BaseSHA), v.PatchID, ch.DependsOn,
		ch.Author.ActorType, ch.Author.ActorID, ch.Author.Username, ch.SessionID, ch.Mergeable, ch.NeedsRestack,
		ch.Stats.Commits, ch.Stats.FilesChanged, ch.Stats.Insertions, ch.Stats.Deletions, string(paths),
		ch.PathsTruncated, string(landing), ch.AutoLand, string(labels), ch.Reverts,
		ch.CreatedAt.UnixNano(), ch.UpdatedAt.UnixNano())
	if isUniqueViolation(err) {
		return errSourceTaken
	}
	if err != nil {
		return fmt.Errorf("store change %s: %w", Ref(ch.WorkspaceSlug, ch.Number), err)
	}
	return nil
}

// insertVersion stores the version v of the change number of the
// workspace, and adds its session to the change's sessions.
func insertVersion(ctx context.Context, tx *sql.Tx, workspace string, number int64, v Version) error {
	var credType, credID *string
	if v.Credential.Type != "" {
		credType, credID = &v.Credential.Type, &v.Credential.ID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO change_versions (workspace_slug, number, version, head_sha, base_sha,
	kind, patch_id, patch_equivalent_to, actor_type, actor_id, actor_username, credential_type, credential_id,
	session_id, commits, files_changed, insertions, deletions, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		workspace, number, v.Number, v.HeadSHA, baseColumn(v.BaseSHA), v.Kind, v.PatchID, v.PatchEquivalentTo,
		v.CreatedBy.ActorType, v.CreatedBy.ActorID, v.CreatedBy.Username, credType, credID, v.SessionID,
		v.Stats.Commits, v.Stats.FilesChanged, v.Stats.Insertions, v.Stats.Deletions, v.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store version %d of %s: %w", v.Number, Ref(workspace, number), err)
	}
	return addSession(ctx, tx, workspace, number, v.SessionID)
}

// addSession adds session, when not nil, to the sessions of the change
// number of the workspace, after those it has.
func addSession(ctx context.Context, tx *sql.Tx, workspace string, number int64, session *string) error {
	if session == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO change_sessions (workspace_slug, number, position, session_id)
SELECT ?, ?, COALESCE(MAX(position), 0) + 1, ? FROM change_sessions WHERE workspace_slug = ? AND number = ?
ON CONFLICT DO NOTHING`, workspace, number, *session, workspace, number)
	if err != nil {
		return fmt.Errorf("store the session of %s: %w", Ref(workspace, number), err)
	}
	return nil
}

// attributeVersion stores v's kind, creator, credential and session as
// those of the stored version v.Number of the change number of the
// workspace, and adds its session to the change's.
func attributeVersion(ctx context.Context, tx *sql.Tx, workspace string, number int64, v Version) error {
	var credType, credID *string
	if v.Credential.Type != "" {
		credType, credID = &v.Credential.Type, &v.Credential.ID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE change_versions SET kind = ?, actor_type = ?, actor_id = ?, actor_username = ?,
	credential_type = ?, credential_id = ?, session_id = ? WHERE workspace_slug = ? AND number = ? AND version = ?`,
		v.Kind, v.CreatedBy.ActorType, v.CreatedBy.ActorID, v.CreatedBy.Username, credType, credID, v.SessionID,
		workspace, number, v.Number); err != nil {
		return fmt.Errorf("attribute version %d of %s: %w", v.Number, Ref(workspace, number), err)
	}
	return addSession(ctx, tx, workspace, number, v.SessionID)
}

// update sets columns of the change number of the workspace, given as
// "name = ?" with their values, and its updated_at; a change that does not
// exist is the change-not-found error.
func update(ctx context.Context, tx *sql.Tx, workspace string, number int64, now time.Time, set string, args ...any) error {
	args = append(append(args, now.UnixNano()), workspace, number)
	res, err := tx.ExecContext(ctx, `UPDATE changes SET `+set+`, updated_at = ? WHERE workspace_slug = ? AND number = ?`, args...)
	if err != nil {
		return fmt.Errorf("update %s: %w", Ref(workspace, number), err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		return errChangeNotFound(workspace, number)
	}
	return nil
}

// storeVersion makes the version v, with the touched paths of its version
// (truncated when there were more), the current version of the change
// number of the workspace: its number, head, base, patch id, stats, paths
// and session, with mergeable back to unknown.
func storeVersion(ctx context.Context, tx *sql.Tx, workspace string, number int64, v Version, paths []string, truncated bool) error {
	b, err := json.Marshal(nonNil(paths))
	if err != nil {
		return err
	}
	return update(ctx, tx, workspace, number, v.CreatedAt, `version = ?, head_sha = ?, base_sha = ?, patch_id = ?,
	commits = ?, files_changed = ?, insertions = ?, deletions = ?, paths = ?, paths_truncated = ?, session_id = ?,
	mergeable = 'unknown'`, v.Number, v.HeadSHA, baseColumn(v.BaseSHA), v.PatchID, v.Stats.Commits, v.Stats.FilesChanged,
		v.Stats.Insertions, v.Stats.Deletions, string(b), truncated, v.SessionID)
}

// storeBase stores the base o measured the current version, version, of
// the change number of the workspace against, with the stats, touched
// paths and patch id it measured, on the change and on the version, and
// the earlier version the version is now patch-equivalent to, if any: the
// same commits measured from a new base, as a change's own commits once
// it sits on another, may make the same diff as an earlier version.
func storeBase(ctx context.Context, tx *sql.Tx, workspace string, number, version int64, o *opening, now time.Time) error {
	paths, err := json.Marshal(nonNil(o.paths))
	if err != nil {
		return err
	}
	equivalent, err := latestEquivalent(ctx, tx, workspace, number, version, o.patchID)
	if err != nil {
		return err
	}
	if err := update(ctx, tx, workspace, number, now, `base_sha = ?, patch_id = ?, commits = ?, files_changed = ?,
	insertions = ?, deletions = ?, paths = ?, paths_truncated = ?`, o.base, o.patchID, o.stats.Commits,
		o.stats.FilesChanged, o.stats.Insertions, o.stats.Deletions, string(paths), o.truncated); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE change_versions SET base_sha = ?, patch_id = ?, patch_equivalent_to = ?, commits = ?,
	files_changed = ?, insertions = ?, deletions = ? WHERE workspace_slug = ? AND number = ? AND version = ?`, o.base, o.patchID,
		equivalent, o.stats.Commits, o.stats.FilesChanged, o.stats.Insertions, o.stats.Deletions, workspace, number, version)
	if err != nil {
		return fmt.Errorf("store the base of version %d of %s: %w", version, Ref(workspace, number), err)
	}
	return nil
}

// versionBase is the base of the version of the change number of the
// workspace.
func versionBase(ctx context.Context, q Querier, workspace string, number, version int64) (string, error) {
	var base string
	err := q.QueryRowContext(ctx, `SELECT base_sha FROM change_versions WHERE workspace_slug = ? AND number = ? AND version = ?`,
		workspace, number, version).Scan(&base)
	if err != nil {
		return "", fmt.Errorf("read the base of version %d of %s: %w", version, Ref(workspace, number), err)
	}
	return base, nil
}

// latestEquivalent is the number of the latest version of the change
// number of the workspace, of those numbered below before, whose patch id
// is patchID, nil for none.
func latestEquivalent(ctx context.Context, q Querier, workspace string, number, before int64, patchID string) (*int64, error) {
	var version int64
	err := q.QueryRowContext(ctx, `SELECT version FROM change_versions WHERE workspace_slug = ? AND number = ? AND version < ?
AND patch_id = ? ORDER BY version DESC LIMIT 1`, workspace, number, before, patchID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find a version of %s like a new one: %w", Ref(workspace, number), err)
	}
	return &version, nil
}

// storeLanding stores the change's landing.
func storeLanding(ctx context.Context, tx *sql.Tx, workspace string, number int64, l Landing, now time.Time) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return update(ctx, tx, workspace, number, now, `landing = ?`, string(b))
}

// abandonChange moves the change to abandoned, giving reason.
func abandonChange(ctx context.Context, tx *sql.Tx, workspace string, number int64, reason string, now time.Time) error {
	return update(ctx, tx, workspace, number, now, `state = 'abandoned', abandon_reason = ?`, reason)
}

// landChange moves the change to landed with l and its landed-commit
// records.
func landChange(ctx context.Context, tx *sql.Tx, workspace string, number int64, l Landed, now time.Time) error {
	commits := l.Commits
	l.Commits = nil
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := update(ctx, tx, workspace, number, now, `state = 'landed', landed = ?`, string(b)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM change_landed_commits WHERE workspace_slug = ? AND number = ?`, workspace, number); err != nil {
		return err
	}
	for i, c := range commits {
		if _, err := tx.ExecContext(ctx, `INSERT INTO change_landed_commits (workspace_slug, number, position, version_sha, landed_sha)
VALUES (?, ?, ?, ?, ?)`, workspace, number, i+1, c.VersionSHA, c.LandedSHA); err != nil {
			return fmt.Errorf("store the landed commits of %s: %w", Ref(workspace, number), err)
		}
	}
	return nil
}

// activeOfSource is the target workspace and number of the draft or open
// change whose source is the branch of the workspace source; found is
// false when there is none.
func activeOfSource(ctx context.Context, q Querier, source, branch string) (workspace string, number int64, found bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT workspace_slug, number FROM changes
WHERE source_workspace = ? AND source_branch = ? AND state IN ('draft', 'open')`, source, branch).Scan(&workspace, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("find the change of %s:%s: %w", source, branch, err)
	}
	return workspace, number, true, nil
}

// changeColumns are the columns scanChange reads, in its order.
const changeColumns = `workspace_slug, number, title, intent, state, abandon_reason, target_branch, source_workspace,
	source_branch, version, head_sha, base_sha, depends_on, author_type, author_id, author_username, session_id,
	mergeable, conflict, needs_restack, restack_reason, commits, files_changed, insertions, deletions, paths,
	paths_truncated, landing, auto_land, labels, landed, reverts, reverted_by, created_at, updated_at`

// readChange reads the change number of the workspace through q, with the
// members other rows hold: its dependants, sessions and landed commits.
func readChange(ctx context.Context, q Querier, workspace string, number int64) (Change, error) {
	var (
		ch                        Change
		base                      string
		abandon, conflict, landed sql.NullString
		paths, landing, labels    string
		created, updated          int64
	)
	err := q.QueryRowContext(ctx, `SELECT `+changeColumns+` FROM changes WHERE workspace_slug = ? AND number = ?`,
		workspace, number).Scan(&ch.WorkspaceSlug, &ch.Number, &ch.Title, &ch.Intent, &ch.State, &abandon,
		&ch.TargetBranch, &ch.Source.WorkspaceSlug, &ch.Source.Branch, &ch.Version, &ch.HeadSHA, &base,
		&ch.DependsOn, &ch.Author.ActorType, &ch.Author.ActorID, &ch.Author.Username, &ch.SessionID, &ch.Mergeable,
		&conflict, &ch.NeedsRestack, &ch.RestackReason, &ch.Stats.Commits, &ch.Stats.FilesChanged,
		&ch.Stats.Insertions, &ch.Stats.Deletions, &paths, &ch.PathsTruncated, &landing, &ch.AutoLand, &labels,
		&landed, &ch.Reverts, &ch.RevertedBy, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Change{}, errChangeNotFound(workspace, number)
	}
	if err != nil {
		return Change{}, fmt.Errorf("read %s: %w", Ref(workspace, number), err)
	}
	ch.Ref = Ref(ch.WorkspaceSlug, ch.Number)
	ch.BaseSHA = baseOf(base)
	ch.abandonReason = abandon.String
	ch.CreatedAt, ch.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	if conflict.Valid {
		ch.Conflict = json.RawMessage(conflict.String)
	}
	for _, m := range []struct {
		raw string
		v   any
	}{{paths, &ch.Paths}, {landing, &ch.Landing}, {labels, &ch.Labels}} {
		if err := json.Unmarshal([]byte(m.raw), m.v); err != nil {
			return Change{}, fmt.Errorf("read %s: %w", ch.Ref, err)
		}
	}
	if landed.Valid {
		ch.Landed = &Landed{}
		if err := json.Unmarshal([]byte(landed.String), ch.Landed); err != nil {
			return Change{}, fmt.Errorf("read %s: %w", ch.Ref, err)
		}
		if ch.Landed.Commits, err = landedCommits(ctx, q, workspace, number); err != nil {
			return Change{}, err
		}
	}
	if ch.Dependants, err = dependants(ctx, q, workspace, number); err != nil {
		return Change{}, err
	}
	if ch.Sessions, err = sessions(ctx, q, workspace, number); err != nil {
		return Change{}, err
	}
	return ch, nil
}

// dependants are the numbers of the draft and open changes of the
// workspace that depend on the change number, lowest first.
func dependants(ctx context.Context, q Querier, workspace string, number int64) ([]int64, error) {
	return queryList[int64](ctx, q, `SELECT number FROM changes
WHERE workspace_slug = ? AND depends_on = ? AND state IN ('draft', 'open') ORDER BY number`, workspace, number)
}

// sessions are the sessions that contributed a version to the change, in
// the order of their first version.
func sessions(ctx context.Context, q Querier, workspace string, number int64) ([]string, error) {
	return queryList[string](ctx, q, `SELECT session_id FROM change_sessions
WHERE workspace_slug = ? AND number = ? ORDER BY position`, workspace, number)
}

// landedCommits are the landed-commit records of the change, in order.
func landedCommits(ctx context.Context, q Querier, workspace string, number int64) ([]LandedCommit, error) {
	rows, err := q.QueryContext(ctx, `SELECT version_sha, landed_sha FROM change_landed_commits
WHERE workspace_slug = ? AND number = ? ORDER BY position`, workspace, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LandedCommit{}
	for rows.Next() {
		var c LandedCommit
		if err := rows.Scan(&c.VersionSHA, &c.LandedSHA); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// versionColumns are the columns scanVersion reads, in its order.
const versionColumns = `version, head_sha, base_sha, kind, patch_id, patch_equivalent_to, actor_type, actor_id,
	actor_username, credential_type, credential_id, session_id, commits, files_changed, insertions, deletions, created_at`

// readVersions reads the versions of the change number of the workspace
// through q, oldest first.
func readVersions(ctx context.Context, q Querier, workspace string, number int64) ([]Version, error) {
	return readVersionsAfter(ctx, q, workspace, number, 0, -1)
}

// readVersionsAfter reads the versions of the change number of the
// workspace numbered above after, oldest first, at most limit of them (all
// for a negative limit).
func readVersionsAfter(ctx context.Context, q Querier, workspace string, number, after int64, limit int) ([]Version, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+versionColumns+` FROM change_versions
WHERE workspace_slug = ? AND number = ? AND version > ? ORDER BY version LIMIT ?`, workspace, number, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read the versions of %s: %w", Ref(workspace, number), err)
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		var (
			credType, credID sql.NullString
			base             string
			created          int64
			ver              Version
		)
		if err := rows.Scan(&ver.Number, &ver.HeadSHA, &base, &ver.Kind, &ver.PatchID, &ver.PatchEquivalentTo,
			&ver.CreatedBy.ActorType, &ver.CreatedBy.ActorID, &ver.CreatedBy.Username, &credType, &credID,
			&ver.SessionID, &ver.Stats.Commits, &ver.Stats.FilesChanged, &ver.Stats.Insertions, &ver.Stats.Deletions,
			&created); err != nil {
			return nil, err
		}
		ver.BaseSHA = baseOf(base)
		ver.Credential = Credential{Type: credType.String, ID: credID.String}
		ver.CreatedAt = time.Unix(0, created).UTC()
		out = append(out, ver)
	}
	return out, rows.Err()
}

// storedVersion is the version v of the change number of the workspace as
// tx holds it once the collaborators ran: the stack linker may have
// measured it from another base.
func storedVersion(ctx context.Context, tx *sql.Tx, workspace string, number int64, v Version) (Version, error) {
	vs, err := readVersionsAfter(ctx, tx, workspace, number, v.Number-1, 1)
	if err != nil {
		return Version{}, err
	}
	if len(vs) == 0 {
		return Version{}, fmt.Errorf("read version %d of %s: %w", v.Number, Ref(workspace, number), sql.ErrNoRows)
	}
	return vs[0], nil
}

// queryList reads the one column of every row query selects.
func queryList[T any](ctx context.Context, q Querier, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// baseOf is the base_sha of a change or a version whose base column, or
// measured base, holds base: nil for "", a head that shares no history
// with the target branch.
func baseOf(base string) *string {
	if base == "" {
		return nil
	}
	return &base
}

// baseColumn is the base column's value for the base_sha base: "" for
// none, which the repository's diffs read as the empty tree.
func baseColumn(base *string) string {
	if base == nil {
		return ""
	}
	return *base
}

// nonNil is s, or an empty list for nil.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// isUniqueViolation reports whether err is SQLite refusing a duplicate of a
// unique index or primary key.
func isUniqueViolation(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && (e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}
