package awareness

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// Claim is an advisory, expiring statement that someone intends to work on
// the files its globs match, as the store holds it.
type Claim struct {
	// ID is the claim's UUID.
	ID string
	// Workspace is the workspace the claim is stored on: for a claim made
	// through a task fork, the fork's parent.
	Workspace    string
	TargetBranch string
	// Paths are the claim's globs, in the order given.
	Paths  []string
	Intent string
	// By is who made the claim.
	By Actor
	// SessionID is the session the claim names, nil for none.
	SessionID *string
	// Change is the number of the change the claim's session opened, nil
	// while it is linked to none.
	Change *int64
	// State is the stored state: ClaimActive until the claim is released,
	// expired or fulfilled. A claim stored as active is expired from
	// ExpiresAt on, also before the expiry run has stored it so.
	State     string
	CreatedAt time.Time
	ExpiresAt time.Time
	// EndedAt is when the claim stopped being active, nil while it is; the
	// cleanup removes ended claims by it. It is not part of the claim
	// object.
	EndedAt *time.Time
}

// The states of a claim.
const (
	ClaimActive    = "active"
	ClaimReleased  = "released"
	ClaimExpired   = "expired"
	ClaimFulfilled = "fulfilled"
)

// reportedState is the claim's state as a read at now reports it: expired
// for a claim stored as active whose expires_at has come.
func (cl Claim) reportedState(now time.Time) string {
	if cl.State == ClaimActive && !cl.ExpiresAt.After(now) {
		return ClaimExpired
	}
	return cl.State
}

// ClaimObject is a claim as the API shows it.
type ClaimObject struct {
	ID           string        `json:"id"`
	Workspace    string        `json:"workspace_slug"`
	Paths        []string      `json:"paths"`
	TargetBranch string        `json:"target_branch"`
	Intent       string        `json:"intent"`
	By           SummaryAuthor `json:"by"`
	SessionID    *string       `json:"session_id"`
	Change       *int64        `json:"change"`
	ExpiresAt    timefmt.Time  `json:"expires_at"`
	State        string        `json:"state"`
	CreatedAt    timefmt.Time  `json:"created_at"`
}

// objectOf is the claim cl as the API shows it to a read at now, with the
// state the read reports.
func objectOf(cl Claim, now time.Time) ClaimObject {
	paths := cl.Paths
	if paths == nil {
		paths = []string{}
	}
	return ClaimObject{
		ID: cl.ID, Workspace: cl.Workspace, Paths: paths, TargetBranch: cl.TargetBranch, Intent: cl.Intent,
		By:        SummaryAuthor{ActorType: cl.By.ActorType, ActorID: cl.By.ActorID, Username: cl.By.Username},
		SessionID: cl.SessionID, Change: cl.Change, ExpiresAt: timefmt.Time(cl.ExpiresAt),
		State: cl.reportedState(now), CreatedAt: timefmt.Time(cl.CreatedAt),
	}
}

// claimColumns are the columns of a claim, in the order scanClaim reads
// them.
const claimColumns = `id, workspace_slug, target_branch, paths, intent, actor_type, actor_id, username, session_id,
	change_number, state, created_at, expires_at, ended_at`

// insertClaim stores the claim cl in tx.
func insertClaim(ctx context.Context, tx *sql.Tx, cl Claim) error {
	paths := cl.Paths
	if paths == nil {
		paths = []string{}
	}
	doc, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	var ended *int64
	if cl.EndedAt != nil {
		ended = new(cl.EndedAt.UnixNano())
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO awareness_claims (`+claimColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cl.ID, cl.Workspace, cl.TargetBranch, string(doc), cl.Intent, cl.By.ActorType, cl.By.ActorID, cl.By.Username,
		cl.SessionID, cl.Change, cl.State, cl.CreatedAt.UnixNano(), cl.ExpiresAt.UnixNano(), ended)
	if err != nil {
		return fmt.Errorf("store the claim %s of %s: %w", cl.ID, cl.Workspace, err)
	}
	return nil
}

// liveClaims are the claims stored on workspace for the target branch
// target that are active at now, newest first (created_at descending, then
// id descending), read through q.
func liveClaims(ctx context.Context, q Querier, workspace, target string, now time.Time) ([]Claim, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+claimColumns+` FROM awareness_claims
WHERE workspace_slug = ? AND target_branch = ? AND state = 'active' AND expires_at > ?
ORDER BY created_at DESC, id DESC`, workspace, target, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("read the claims of %s on %s: %w", workspace, target, err)
	}
	defer rows.Close()
	out := []Claim{}
	for rows.Next() {
		cl, err := scanClaim(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, cl)
	}
	return out, rows.Err()
}

// scanClaim reads one row of claimColumns.
func scanClaim(scan func(dest ...any) error) (Claim, error) {
	var (
		cl                Claim
		doc               string
		created, expires  int64
		ended             sql.NullInt64
		actorID, username sql.NullString
		session           sql.NullString
		change            sql.NullInt64
	)
	if err := scan(&cl.ID, &cl.Workspace, &cl.TargetBranch, &doc, &cl.Intent, &cl.By.ActorType, &actorID, &username,
		&session, &change, &cl.State, &created, &expires, &ended); err != nil {
		return Claim{}, err
	}
	if err := json.Unmarshal([]byte(doc), &cl.Paths); err != nil {
		return Claim{}, fmt.Errorf("the paths of the claim %s: %w", cl.ID, err)
	}
	if actorID.Valid {
		cl.By.ActorID = &actorID.String
	}
	if username.Valid {
		cl.By.Username = &username.String
	}
	if session.Valid {
		cl.SessionID = &session.String
	}
	if change.Valid {
		cl.Change = &change.Int64
	}
	cl.CreatedAt, cl.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	if ended.Valid {
		cl.EndedAt = new(time.Unix(0, ended.Int64).UTC())
	}
	return cl, nil
}
