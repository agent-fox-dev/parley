package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The verdicts a user gives on a version.
const (
	verdictApprove = "approve"
	verdictReject  = "reject"
)

// storedVerdict is a verdict as the store keeps it: on version Version of
// the change Number of Workspace, by the user with the username stored on
// it and the credential it was given with. CarriedFrom is the version a
// copy was carried from, nil for a verdict given on its own version.
type storedVerdict struct {
	ID          string
	Workspace   string
	Number      int64
	Version     int64
	UserID      string
	Username    string
	Credential  change.Credential
	Verdict     string
	Comment     string
	CarriedFrom *int64
	CreatedAt   time.Time
}

// counting is what the counting rule knows of the verdict.
func (v storedVerdict) counting() verdict {
	return verdict{UserID: v.UserID, Username: v.Username, Credential: v.Credential, Version: v.Version}
}

// approval is a verdict as the routes answer it: by is the credential's
// type with its user, and counts_for the rules of the requirements it
// counts toward, never null.
type approval struct {
	ID          string       `json:"id"`
	Change      int64        `json:"change"`
	Version     int64        `json:"version"`
	Verdict     string       `json:"verdict"`
	Comment     string       `json:"comment"`
	By          Writer       `json:"by"`
	CountsFor   []string     `json:"counts_for"`
	CarriedFrom *int64       `json:"carried_from"`
	CreatedAt   timefmt.Time `json:"created_at"`
}

// approvalOf is v as the routes answer it, counting toward the rules
// countsFor names.
func approvalOf(v storedVerdict, countsFor []string) approval {
	if countsFor == nil {
		countsFor = []string{}
	}
	return approval{
		ID: v.ID, Change: v.Number, Version: v.Version, Verdict: v.Verdict, Comment: v.Comment,
		By:        Writer{ActorType: v.Credential.Type, ActorID: new(v.UserID), Username: new(v.Username)},
		CountsFor: countsFor, CarriedFrom: v.CarriedFrom, CreatedAt: timefmt.Time(v.CreatedAt),
	}
}

// replaceVerdict stores v in tx in place of the verdict its user had on its
// version, and reports whether there was one. Deleting and inserting in the
// one write transaction leaves the user with exactly one verdict there,
// since write transactions run one at a time; the unique key of the table
// stays as a backstop.
func replaceVerdict(ctx context.Context, tx *sql.Tx, v storedVerdict) (replaced bool, err error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM change_approvals
WHERE workspace_slug = ? AND number = ? AND version = ? AND user_id = ?`, v.Workspace, v.Number, v.Version, v.UserID)
	if err != nil {
		return false, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO change_approvals
(id, workspace_slug, number, version, user_id, username, credential_type, credential_id, verdict, comment, carried_from, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.Workspace, v.Number, v.Version, v.UserID, v.Username, v.Credential.Type, v.Credential.ID,
		v.Verdict, v.Comment, v.CarriedFrom, v.CreatedAt.UnixNano())
	return deleted > 0, err
}

// verdictColumns are the columns scanVerdict reads, in its order.
const verdictColumns = `id, workspace_slug, number, version, user_id, username, credential_type, credential_id,
verdict, comment, carried_from, created_at`

// verdictSeconds is created_at, stored in nanoseconds, at the resolution
// the approval object shows it, so that the list's order is the order of
// the values the caller sees, and ties among them go by id.
const verdictSeconds = `(created_at / 1000000000)`

// verdictPosition is a verdict's place in the list's order.
type verdictPosition struct {
	Seconds int64  `json:"t"`
	ID      string `json:"id"`
}

// positionOf is a's place in the list's order.
func positionOf(a approval) any {
	return verdictPosition{time.Time(a.CreatedAt).Unix(), a.ID}
}

// verdictFilter narrows the verdicts of a change: to those on Version, all
// of them for 0; to those after the position After in the list's order,
// from the first for nil; to the first Limit of them, all for 0.
type verdictFilter struct {
	Version int64
	After   *verdictPosition
	Limit   int
}

// readVerdicts is the verdicts of the change number of the workspace that
// f lets through, read through q, newest first: by created_at, as shown,
// descending, then by id descending.
func readVerdicts(ctx context.Context, q Querier, workspace string, number int64, f verdictFilter) ([]storedVerdict, error) {
	where, args := []string{`workspace_slug = ?`, `number = ?`}, []any{workspace, number}
	if f.Version != 0 {
		where, args = append(where, `version = ?`), append(args, f.Version)
	}
	if f.After != nil {
		where = append(where, `(`+verdictSeconds+` < ? OR (`+verdictSeconds+` = ? AND id < ?))`)
		args = append(args, f.After.Seconds, f.After.Seconds, f.After.ID)
	}
	query := `SELECT ` + verdictColumns + ` FROM change_approvals WHERE ` + strings.Join(where, ` AND `) +
		` ORDER BY ` + verdictSeconds + ` DESC, id DESC`
	if f.Limit > 0 {
		query, args = query+` LIMIT ?`, append(args, f.Limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedVerdict
	for rows.Next() {
		v, err := scanVerdict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// readVerdict is the verdict id of the change number of the workspace,
// read through q; found is false when the change has no verdict of that
// id, also when another change has one.
func readVerdict(ctx context.Context, q Querier, workspace string, number int64, id string) (v storedVerdict, found bool, err error) {
	v, err = scanVerdict(q.QueryRowContext(ctx, `SELECT `+verdictColumns+` FROM change_approvals
WHERE workspace_slug = ? AND number = ? AND id = ?`, workspace, number, id))
	if errors.Is(err, sql.ErrNoRows) {
		return storedVerdict{}, false, nil
	}
	return v, err == nil, err
}

// scanVerdict reads a row of verdictColumns.
func scanVerdict(row interface{ Scan(...any) error }) (storedVerdict, error) {
	var (
		v       storedVerdict
		created int64
	)
	if err := row.Scan(&v.ID, &v.Workspace, &v.Number, &v.Version, &v.UserID, &v.Username, &v.Credential.Type,
		&v.Credential.ID, &v.Verdict, &v.Comment, &v.CarriedFrom, &created); err != nil {
		return storedVerdict{}, err
	}
	v.CreatedAt = time.Unix(0, created).UTC()
	return v, nil
}

// deleteVerdict removes the verdict id in tx, and only it: a copy of it
// carried to another version is a verdict of its own.
func deleteVerdict(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM change_approvals WHERE id = ?`, id)
	return err
}

// listApprovals is the verdicts of the change ch that f lets through, read
// through q, newest first, as the routes answer them: counts_for is
// decided when they are read, against the landing stored for the change's
// current version, so that it follows the policy and the versions as they
// are now.
func (c *Component) listApprovals(ctx context.Context, q Querier, ch change.Change, f verdictFilter) ([]approval, error) {
	verdicts, err := readVerdicts(ctx, q, ch.WorkspaceSlug, ch.Number, f)
	if err != nil || len(verdicts) == 0 {
		return []approval{}, err
	}
	reqs, err := requirementsOf(ch.Landing)
	if err != nil {
		return nil, err
	}
	k, err := c.newCounter(ctx, q, ch.WorkspaceSlug, ch.Number, ch.Version, reqs)
	if err != nil {
		return nil, err
	}
	out := make([]approval, 0, len(verdicts))
	for _, v := range verdicts {
		countsFor, err := k.countsFor(ctx, v)
		if err != nil {
			return nil, err
		}
		out = append(out, approvalOf(v, countsFor))
	}
	return out, nil
}

// requirementsOf is the requirements of the landing l as the change record
// stores them, which the evaluation wrote from Requirement values.
func requirementsOf(l change.Landing) ([]Requirement, error) {
	reqs := make([]Requirement, 0, len(l.Requirements))
	for _, raw := range l.Requirements {
		var r Requirement
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("decode a stored requirement: %w", err)
		}
		reqs = append(reqs, r)
	}
	return reqs, nil
}

// counter decides which requirements of one change the verdicts on it
// count toward, reading through q, once, the workspace's owner and
// organization and who created each version, and each user's memberships
// the first time that user is asked about.
type counter struct {
	q          Querier
	owner, org string
	current    int64
	creators   []versionCreator
	reqs       []Requirement
	members    map[string]bool
}

// newCounter is the counter of the change number of the workspace against
// the requirements reqs of its landing for version current: the one stored
// for its current version, or the one a land of an older version is judged
// with.
func (c *Component) newCounter(ctx context.Context, q Querier, workspace string, number, current int64, reqs []Requirement) (*counter, error) {
	rec, _, err := access.RecordBySlug(ctx, q, workspace)
	if err != nil {
		return nil, err
	}
	versions, err := c.deps.Changes.ReadVersions(ctx, q, workspace, number)
	if err != nil {
		return nil, err
	}
	return &counter{q: q, owner: rec.OwnerID, org: rec.OrgID, current: current,
		creators: creatorsOf(versions), reqs: reqs, members: map[string]bool{}}, nil
}

// countsFor is the rules of the requirements v counts toward, in the
// landing's order. A verdict on another version than the current one counts
// toward none: the landing is the current version's.
func (k *counter) countsFor(ctx context.Context, v storedVerdict) ([]string, error) {
	out := []string{}
	for _, r := range k.reqs {
		ok, err := k.counts(ctx, v, r.Approvals.Approvers)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, r.Rule)
		}
	}
	return out, nil
}

// counts reports whether v counts toward a requirement whose approvers are
// approvers. Only a verdict on the counter's version can: the requirement
// is that version's.
func (k *counter) counts(ctx context.Context, v storedVerdict, approvers []string) (bool, error) {
	if v.Version != k.current || len(approvers) == 0 {
		return false, nil
	}
	member, err := k.isMember(ctx, v.UserID)
	if err != nil {
		return false, err
	}
	pool := approverPool{Owner: k.owner, Members: map[string]bool{v.UserID: member}}
	return counts(v.counting(), approvers, pool, k.creators), nil
}

// isMember reports whether the user is a member of the workspace's
// organization, as the access rule decides it: through the user's active
// organizations. A workspace of no organization has no members.
func (k *counter) isMember(ctx context.Context, user string) (bool, error) {
	if k.org == "" {
		return false, nil
	}
	if m, ok := k.members[user]; ok {
		return m, nil
	}
	orgs, err := identity.ActiveOrgIDs(ctx, k.q, user)
	if err != nil {
		return false, err
	}
	k.members[user] = slices.Contains(orgs, k.org)
	return k.members[user], nil
}
