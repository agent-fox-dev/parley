package policy

import (
	"context"
	"database/sql"
	"slices"
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

// newCounter is the counter of the change ch against the requirements reqs
// of its landing, the one stored for its current version.
func (c *Component) newCounter(ctx context.Context, q Querier, ch change.Change, reqs []Requirement) (*counter, error) {
	rec, _, err := access.RecordBySlug(ctx, q, ch.WorkspaceSlug)
	if err != nil {
		return nil, err
	}
	versions, err := c.deps.Changes.ReadVersions(ctx, q, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return nil, err
	}
	return &counter{q: q, owner: rec.OwnerID, org: rec.OrgID, current: ch.Version,
		creators: creatorsOf(versions), reqs: reqs, members: map[string]bool{}}, nil
}

// countsFor is the rules of the requirements v counts toward, in the
// landing's order. A verdict on another version than the current one counts
// toward none: the landing is the current version's.
func (k *counter) countsFor(ctx context.Context, v storedVerdict) ([]string, error) {
	out := []string{}
	if v.Version != k.current || len(k.reqs) == 0 {
		return out, nil
	}
	member, err := k.isMember(ctx, v.UserID)
	if err != nil {
		return nil, err
	}
	pool := approverPool{Owner: k.owner, Members: map[string]bool{v.UserID: member}}
	for _, r := range k.reqs {
		if counts(v.counting(), r.Approvals.Approvers, pool, k.creators) {
			out = append(out, r.Rule)
		}
	}
	return out, nil
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
