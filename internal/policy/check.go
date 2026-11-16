package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// checkFailed is the third status a runner reports, beside checkPassed and
// checkPending.
const checkFailed = "failed"

// storedCheck is a check result as the store keeps it: the result of the
// check Name on version Version of the change Number of Workspace, as
// reported last, by By at UpdatedAt. URL is nil and Details is nil when the
// report had none.
type storedCheck struct {
	Workspace string
	Number    int64
	Version   int64
	Name      string
	Status    string
	Summary   string
	URL       *string
	Details   json.RawMessage
	By        Writer
	UpdatedAt time.Time
}

// checkResult is a check result as the routes answer it: details is the
// object the runner reported, null when it reported none.
type checkResult struct {
	Name       string          `json:"name"`
	Change     int64           `json:"change"`
	Version    int64           `json:"version"`
	Status     string          `json:"status"`
	Summary    string          `json:"summary"`
	URL        *string         `json:"url"`
	Details    json.RawMessage `json:"details"`
	ReportedBy Writer          `json:"reported_by"`
	UpdatedAt  timefmt.Time    `json:"updated_at"`
}

// checkOf is s as the routes answer it.
func checkOf(s storedCheck) checkResult {
	return checkResult{
		Name: s.Name, Change: s.Number, Version: s.Version, Status: s.Status, Summary: s.Summary, URL: s.URL,
		Details: s.Details, ReportedBy: s.By, UpdatedAt: timefmt.Time(s.UpdatedAt),
	}
}

// upsertCheck stores s in tx as the result of its check on its version,
// replacing every column of a result stored there before, so that what a
// report leaves out is empty afterwards rather than kept from the last
// one. Write transactions run one at a time, so two reports of the same
// check both succeed and the later one is stored.
func upsertCheck(ctx context.Context, tx *sql.Tx, s storedCheck) error {
	var details any
	if s.Details != nil {
		details = string(s.Details)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO change_checks
(workspace_slug, number, version, name, status, summary, url, details, actor_type, actor_id, username, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (workspace_slug, number, version, name) DO UPDATE SET
	status = excluded.status, summary = excluded.summary, url = excluded.url, details = excluded.details,
	actor_type = excluded.actor_type, actor_id = excluded.actor_id, username = excluded.username,
	updated_at = excluded.updated_at`,
		s.Workspace, s.Number, s.Version, s.Name, s.Status, s.Summary, s.URL, details,
		s.By.ActorType, s.By.ActorID, s.By.Username, s.UpdatedAt.UnixNano())
	return err
}

// checkColumns are the columns scanCheck reads, in its order.
const checkColumns = `workspace_slug, number, version, name, status, summary, url, details, actor_type, actor_id,
username, updated_at`

// readChecks is every result stored for the change number of the
// workspace, on any version, read through q.
func readChecks(ctx context.Context, q Querier, workspace string, number int64) ([]storedCheck, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+checkColumns+` FROM change_checks
WHERE workspace_slug = ? AND number = ?`, workspace, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedCheck
	for rows.Next() {
		var (
			s       storedCheck
			details sql.NullString
			updated int64
		)
		if err := rows.Scan(&s.Workspace, &s.Number, &s.Version, &s.Name, &s.Status, &s.Summary, &s.URL, &details,
			&s.By.ActorType, &s.By.ActorID, &s.By.Username, &updated); err != nil {
			return nil, err
		}
		if details.Valid {
			s.Details = json.RawMessage(details.String)
		}
		s.UpdatedAt = time.Unix(0, updated).UTC()
		out = append(out, s)
	}
	return out, rows.Err()
}

// applying is the result of the check name that applies to version of a
// change whose versions are versions and whose stored results are results:
// the one reported on that version, else the one on the nearest earlier
// version it reaches through patch_equivalent_to. A result is not copied
// when a version is recorded but looked up here, so that it keeps the
// version it was reported on and a requirement with fresh_checks can tell
// it from one reported on the version itself; given no versions, only the
// version's own result applies.
func applying(versions []change.Version, results []storedCheck, version int64, name string) (storedCheck, bool) {
	for _, n := range equivalenceChain(versions, version) {
		for _, r := range results {
			if r.Version == n && r.Name == name {
				return r, true
			}
		}
	}
	return storedCheck{}, false
}

// listChecks is the results that apply to version of the change ch, one
// per check name that has one, ordered by name, read through q and
// answered as the routes answer them.
func (c *Component) listChecks(ctx context.Context, q Querier, ch change.Change, version int64) ([]checkResult, error) {
	results, err := readChecks(ctx, q, ch.WorkspaceSlug, ch.Number)
	if err != nil || len(results) == 0 {
		return []checkResult{}, err
	}
	versions, err := c.deps.Changes.ReadVersions(ctx, q, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(results))
	for _, r := range results {
		names = append(names, r.Name)
	}
	slices.Sort(names)
	out := []checkResult{}
	for _, name := range slices.Compact(names) {
		if r, ok := applying(versions, results, version, name); ok {
			out = append(out, checkOf(r))
		}
	}
	return out, nil
}

// checkPosition is a result's place in the list's order.
type checkPosition struct {
	Name string `json:"name"`
}
