package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

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
