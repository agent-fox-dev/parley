package change

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Key names a change: the workspace it belongs to, its target, and its
// number there.
type Key struct {
	WorkspaceSlug string `json:"workspace_slug"`
	Number        int64  `json:"number"`
}

// BranchChange is the draft or open change of a source branch: the
// workspace it belongs to, its number, its current version and its state.
type BranchChange struct {
	WorkspaceSlug   string
	Number, Version int64
	State           string
}

// CommitChange is the change a commit belongs to: the workspace it belongs
// to, its number, the highest version whose head the commit is or, for a
// commit the change landed, the highest version whose head the commit was
// landed from (nil when there is none), and whether the change landed the
// commit, with the commit of the version it landed from, which is the
// commit itself when the land kept it.
type CommitChange struct {
	WorkspaceSlug string
	Number        int64
	Version       *int64
	Landed        bool
	LandedFrom    string
}

// SourceChange is a change of a source workspace: the workspace it belongs
// to, its number, its source branch and its state.
type SourceChange struct {
	WorkspaceSlug string
	Number        int64
	Branch, State string
}

// querier is q, or the store when q is nil.
func (c *Component) querier(q Querier) Querier {
	if q == nil {
		return c.deps.Store.Main()
	}
	return q
}

// OpenChangeOfBranch is the draft or open change whose source is the
// branch, named without refs/heads/, of the workspace slug, read through q
// (the caller's transaction, or nil for the store). The change is named
// under the workspace it belongs to, the fork's parent for a fork's
// branch. found is false, with a nil error, when there is none, also when
// the branch's changes all landed or were abandoned.
func (c *Component) OpenChangeOfBranch(ctx context.Context, q Querier, workspace, branch string) (BranchChange, bool, error) {
	var b BranchChange
	err := c.querier(q).QueryRowContext(ctx, `SELECT workspace_slug, number, version, state FROM changes
WHERE source_workspace = ? AND source_branch = ? AND state IN ('draft', 'open')`, workspace, branch).
		Scan(&b.WorkspaceSlug, &b.Number, &b.Version, &b.State)
	if errors.Is(err, sql.ErrNoRows) {
		return BranchChange{}, false, nil
	}
	if err != nil {
		return BranchChange{}, false, fmt.Errorf("find the change of %s:%s: %w", workspace, branch, err)
	}
	return b, true, nil
}

// ChangesInto are the numbers of the draft and open changes of the
// workspace whose target is branch, ascending, read through q (the
// caller's transaction, or nil for the store).
func (c *Component) ChangesInto(ctx context.Context, q Querier, workspace, branch string) ([]int64, error) {
	numbers, err := queryList[int64](ctx, c.querier(q), `SELECT number FROM changes
WHERE workspace_slug = ? AND target_branch = ? AND state IN ('draft', 'open') ORDER BY number`, workspace, branch)
	if err != nil {
		return nil, fmt.Errorf("find the changes into %s:%s: %w", workspace, branch, err)
	}
	return numbers, nil
}

// DraftAndOpen are the numbers of the draft and open changes of the
// workspace, whatever their target branch, ascending, read through q (the
// caller's transaction, or nil for the store). A repository's include
// those from its task forks, which belong to it.
func (c *Component) DraftAndOpen(ctx context.Context, q Querier, workspace string) ([]int64, error) {
	numbers, err := queryList[int64](ctx, c.querier(q), `SELECT number FROM changes
WHERE workspace_slug = ? AND state IN ('draft', 'open') ORDER BY number`, workspace)
	if err != nil {
		return nil, fmt.Errorf("find the draft and open changes of %s: %w", workspace, err)
	}
	return numbers, nil
}

// ofWorkspace selects the changes a workspace asks about: its own, and,
// since a fork holds no changes, those of its parent whose source it is.
const ofWorkspace = `(c.workspace_slug = ? OR c.source_workspace = ?)`

// ChangeOfCommit is the change of the workspace slug that landed the
// commit sha, given in full, or has it as the head of a version, read
// through q (the caller's transaction, or nil for the store). A change that
// landed the commit comes before one that has it as a version head, and
// among several the lowest number. For a fork's slug it answers from the
// parent's changes whose source is the fork. found is false, with a nil
// error, for a commit of no change.
func (c *Component) ChangeOfCommit(ctx context.Context, q Querier, workspace, sha string) (CommitChange, bool, error) {
	q = c.querier(q)
	var (
		cc         CommitChange
		versionSHA string
	)
	err := q.QueryRowContext(ctx, `SELECT c.workspace_slug, c.number, l.version_sha FROM change_landed_commits l
JOIN changes c ON c.workspace_slug = l.workspace_slug AND c.number = l.number
WHERE l.landed_sha = ? AND `+ofWorkspace+` ORDER BY c.number, l.position LIMIT 1`, sha, workspace, workspace).
		Scan(&cc.WorkspaceSlug, &cc.Number, &versionSHA)
	switch {
	case err == nil:
		cc.Landed, cc.LandedFrom = true, versionSHA
		if cc.Version, err = highestVersionAt(ctx, q, cc.WorkspaceSlug, cc.Number, versionSHA); err != nil {
			return CommitChange{}, false, err
		}
		return cc, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return CommitChange{}, false, fmt.Errorf("find the change that landed %s: %w", sha, err)
	}
	var version int64
	err = q.QueryRowContext(ctx, `SELECT c.workspace_slug, c.number, MAX(v.version) FROM change_versions v
JOIN changes c ON c.workspace_slug = v.workspace_slug AND c.number = v.number
WHERE v.head_sha = ? AND `+ofWorkspace+` GROUP BY c.workspace_slug, c.number ORDER BY c.number LIMIT 1`, sha, workspace, workspace).
		Scan(&cc.WorkspaceSlug, &cc.Number, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return CommitChange{}, false, nil
	}
	if err != nil {
		return CommitChange{}, false, fmt.Errorf("find the change of %s: %w", sha, err)
	}
	cc.Version = &version
	return cc, true, nil
}

// SessionOfHead is the session of the first version whose head is the
// commit sha, given in full, of a change of the workspace slug, read
// through q (the caller's transaction, or nil for the store): the session
// of the write that introduced it, nil for a version of none. Among
// several changes the lowest number answers. For a fork's slug it answers
// from the parent's changes whose source is the fork. found is false, with
// a nil error, for a commit that is no version's head.
func (c *Component) SessionOfHead(ctx context.Context, q Querier, workspace, sha string) (session *string, found bool, err error) {
	err = c.querier(q).QueryRowContext(ctx, `SELECT v.session_id FROM change_versions v
JOIN changes c ON c.workspace_slug = v.workspace_slug AND c.number = v.number
WHERE v.head_sha = ? AND `+ofWorkspace+` ORDER BY c.number, v.version LIMIT 1`, sha, workspace, workspace).Scan(&session)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("find the session of %s: %w", sha, err)
	}
	return session, true, nil
}

// highestVersionAt is the highest version of the change number of the
// workspace whose head is sha, nil for none.
func highestVersionAt(ctx context.Context, q Querier, workspace string, number int64, sha string) (*int64, error) {
	var version sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT MAX(version) FROM change_versions WHERE workspace_slug = ? AND number = ? AND head_sha = ?`,
		workspace, number, sha).Scan(&version)
	if err != nil {
		return nil, fmt.Errorf("find the version of %s at %s: %w", Ref(workspace, number), sha, err)
	}
	if !version.Valid {
		return nil, nil
	}
	return &version.Int64, nil
}

// ChangesOfSession are the changes, across workspaces, with a version of
// the session, ordered by workspace slug and number, read through q (the
// caller's transaction, or nil for the store); none for a session of no
// version.
func (c *Component) ChangesOfSession(ctx context.Context, q Querier, session string) ([]Key, error) {
	rows, err := c.querier(q).QueryContext(ctx, `SELECT DISTINCT workspace_slug, number FROM change_sessions
WHERE session_id = ? ORDER BY workspace_slug, number`, session)
	if err != nil {
		return nil, fmt.Errorf("find the changes of the session %s: %w", session, err)
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.WorkspaceSlug, &k.Number); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ChangeNumbersOfSource are the changes, in any state, whose source
// workspace is the workspace slug, ordered by number, read through q (the
// caller's transaction, or nil for the store); none for a workspace that is
// the source of no change. The changes of a fork belong to its parent,
// whose repository keeps their version refs.
func (c *Component) ChangeNumbersOfSource(ctx context.Context, q Querier, workspace string) ([]SourceChange, error) {
	rows, err := c.querier(q).QueryContext(ctx, `SELECT workspace_slug, number, source_branch, state FROM changes
WHERE source_workspace = ? ORDER BY workspace_slug, number`, workspace)
	if err != nil {
		return nil, fmt.Errorf("find the changes from %s: %w", workspace, err)
	}
	defer rows.Close()
	out := []SourceChange{}
	for rows.Next() {
		var s SourceChange
		if err := rows.Scan(&s.WorkspaceSlug, &s.Number, &s.Branch, &s.State); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// OpenCounts is the number of draft and open changes of each workspace
// slug, with no entry for a workspace that has none.
func (c *Component) OpenCounts(ctx context.Context) (map[string]int, error) {
	rows, err := c.deps.Store.Main().QueryContext(ctx, `SELECT workspace_slug, COUNT(*) FROM changes
WHERE state IN ('draft', 'open') GROUP BY workspace_slug`)
	if err != nil {
		return nil, fmt.Errorf("count the open changes: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var (
			slug string
			n    int
		)
		if err := rows.Scan(&slug, &n); err != nil {
			return nil, err
		}
		out[slug] = n
	}
	return out, rows.Err()
}
