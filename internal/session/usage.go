package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The messages of the usage route's refusals.
const (
	msgUsageIDTaken = "a usage record with this id exists in another session"
	msgNotActive    = "the session is not active and accepts no usage"
)

// Usage is a usage record, as the usage routes answer it: one token usage
// increment of a session, with the workspace slug copied from the session
// and the hub's time it was reported at.
type Usage struct {
	ID              string       `json:"id"`
	SessionID       string       `json:"session_id"`
	WorkspaceSlug   string       `json:"workspace_slug"`
	Model           string       `json:"model"`
	InputTokens     int64        `json:"input_tokens"`
	OutputTokens    int64        `json:"output_tokens"`
	CacheReadTokens int64        `json:"cache_read_tokens"`
	ReportedAt      timefmt.Time `json:"reported_at"`
}

// UsageObserver is told of a usage record once it is stored: the workspace
// slug of its session, its model and its three token counts.
type UsageObserver func(workspace, model string, input, output, cacheRead int64)

// usageObservers are the observers registered with OnUsage.
type usageObservers struct {
	mu   sync.Mutex
	list []UsageObserver
}

// OnUsage registers obs, which is called once for every usage record
// stored from then on, after the record is committed, and never for a
// replayed or refused report. It is safe to call at any time, also while
// reports are served.
func (c *Component) OnUsage(obs UsageObserver) {
	c.observers.mu.Lock()
	defer c.observers.mu.Unlock()
	c.observers.list = append(c.observers.list, obs)
}

// notify calls every registered observer with the stored record u.
func (c *Component) notify(u Usage) {
	c.observers.mu.Lock()
	list := c.observers.list
	c.observers.mu.Unlock()
	for _, obs := range list {
		obs(u.WorkspaceSlug, u.Model, u.InputTokens, u.OutputTokens, u.CacheReadTokens)
	}
}

// ActiveCounts returns the number of active sessions of each workspace
// slug that has any; a slug with none is absent.
func (c *Component) ActiveCounts(ctx context.Context) (map[string]int64, error) {
	counts := map[string]int64{}
	err := eachRow(ctx, c.deps.Store.Main(), func(scan func(...any) error) error {
		var slug string
		var n int64
		if err := scan(&slug, &n); err != nil {
			return err
		}
		counts[slug] = n
		return nil
	}, `SELECT workspace_slug, COUNT(*) FROM sessions WHERE status = 'active' GROUP BY workspace_slug`)
	if err != nil {
		return nil, fmt.Errorf("count the active sessions: %w", err)
	}
	return counts, nil
}

// reportRoute serves POST /sessions/:id/usage: it stores one usage record
// of an active session, sent by its creator or the admin token, or answers
// the stored record to a report that sends its id again. It decides in
// this order: the path id, read access (an unreadable session is not
// found), the creator rule, the body, the usage id (a replay stores
// nothing, so it does not need an active session), then the status.
func (c *Component) reportRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	sc, ok := server.CallerOf(ctx)
	if !ok {
		return errNoPrincipal
	}
	caller := access.CallerFrom(sc)
	s, err := c.readableSession(ctx, caller, server.Param(r, "id"))
	if err != nil {
		return err
	}
	if !canChange(caller, s) {
		return server.Forbidden(msgNotCreator)
	}
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	in, err := usageInput(body)
	if err != nil {
		return err
	}
	in.SessionID, in.WorkspaceSlug = s.ID, s.WorkspaceSlug
	in.ReportedAt = timefmt.Time(c.deps.Now().UTC().Truncate(time.Second))

	stored, isNew, err := c.storeUsage(ctx, in)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if isNew {
		c.notify(stored)
		status = http.StatusCreated
	}
	server.WriteJSON(w, status, stored)
	return nil
}

// maxTokens is the most tokens one count of a usage report may carry. The
// counts are summed by the store (SUM over a session's or a workspace's
// records), which fails on a sum past 64 bits and would leave every reader
// of the session or the workspace with an error; at this bound it takes
// millions of records to get there.
const maxTokens = 1 << 40

// usageInput reads the body of a usage report: model, a non-blank string
// of at most maxLabel characters; id, a UUID (a new version 4 UUID when
// none is sent); and the three token counts, integers from 0 to maxTokens,
// 0 when absent.
func usageInput(body map[string]json.RawMessage) (Usage, error) {
	var u Usage
	model, err := optionalString(body, "model")
	if err != nil {
		return Usage{}, err
	}
	if model == nil || strings.TrimSpace(*model) == "" {
		return Usage{}, server.InvalidRequest("model is required and must be a non-blank string")
	}
	if utf8.RuneCountInString(*model) > maxLabel {
		return Usage{}, server.InvalidRequest(fmt.Sprintf("model must be at most %d characters", maxLabel))
	}
	u.Model = *model
	if u.ID, err = sessionIDOf(body, "id"); err != nil {
		return Usage{}, err
	}
	if u.ID == "" {
		u.ID = newID()
	}
	counts := []struct {
		name string
		dst  *int64
	}{{"input_tokens", &u.InputTokens}, {"output_tokens", &u.OutputTokens}, {"cache_read_tokens", &u.CacheReadTokens}}
	for _, n := range counts {
		v, err := optionalCount(body, n.name)
		if err != nil {
			return Usage{}, err
		}
		if v != nil && *v > maxTokens {
			return Usage{}, server.InvalidRequest(fmt.Sprintf("%s must be at most %d", n.name, int64(maxTokens)))
		}
		if v != nil {
			*n.dst = *v
		}
	}
	return u, nil
}

// storeUsage stores the usage record in, or finds the one stored with its
// id, in one transaction. A record of the same session is answered as it
// is (isNew false); one of another session is 409 conflict. The session's
// status is read in the same transaction as the insert, so a report racing
// with a complete or a forced close is either stored before the session
// ends or refused with 409 session_not_active.
func (c *Component) storeUsage(ctx context.Context, in Usage) (stored Usage, isNew bool, err error) {
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		existing, found, err := readUsage(ctx, tx, in.ID)
		switch {
		case err != nil:
			return err
		case found && existing.SessionID == in.SessionID:
			stored = existing
			return nil
		case found:
			return server.Conflict(msgUsageIDTaken)
		}
		var status string
		err = tx.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, in.SessionID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return server.NotFound(msgSessionNotFound)
		}
		if err != nil {
			return fmt.Errorf("read the status of session %s: %w", in.SessionID, err)
		}
		if status != StatusActive {
			return server.NewError("session_not_active", http.StatusConflict, msgNotActive)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO session_usage (`+usageColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			in.ID, in.SessionID, in.WorkspaceSlug, in.Model, in.InputTokens, in.OutputTokens, in.CacheReadTokens,
			timefmt.Format(time.Time(in.ReportedAt)))
		if err != nil {
			return fmt.Errorf("store usage record %s: %w", in.ID, err)
		}
		stored, isNew = in, true
		return nil
	})
	return stored, isNew, err
}

// usageColumns are the columns scanUsage reads, in its order.
const usageColumns = `id, session_id, workspace_slug, model, input_tokens, output_tokens, cache_read_tokens, reported_at`

// scanUsage reads a usage record from the columns usageColumns names.
func scanUsage(scan func(...any) error) (Usage, error) {
	var u Usage
	var reported string
	if err := scan(&u.ID, &u.SessionID, &u.WorkspaceSlug, &u.Model, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens, &reported); err != nil {
		return Usage{}, err
	}
	t, err := time.Parse(time.RFC3339, reported)
	if err != nil {
		return Usage{}, fmt.Errorf("usage record %s: reported_at: %w", u.ID, err)
	}
	u.ReportedAt = timefmt.Time(t)
	return u, nil
}

// readUsage reads the usage record id (in lower case) through q; found is
// false when there is none.
func readUsage(ctx context.Context, q Querier, id string) (Usage, bool, error) {
	u, err := scanUsage(q.QueryRowContext(ctx, `SELECT `+usageColumns+` FROM session_usage WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Usage{}, false, nil
	}
	if err != nil {
		return Usage{}, false, fmt.Errorf("read usage record %s: %w", id, err)
	}
	return u, true, nil
}

// usagePosition is the place of a usage record in a session's usage list:
// its reported_at as stored, then its id.
type usagePosition struct {
	ReportedAt string `json:"r"`
	ID         string `json:"i"`
}

// usageRoute serves GET /sessions/:id/usage: the usage records of a
// session the caller may read, ordered by (reported_at, id), descending
// unless order is asc, paged by limit and cursor, with the session's id
// and the totals of all its records, whatever the page.
func (c *Component) usageRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	sc, ok := server.CallerOf(ctx)
	if !ok {
		return errNoPrincipal
	}
	s, err := c.readableSession(ctx, access.CallerFrom(sc), server.Param(r, "id"))
	if err != nil {
		return err
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	ascending := false
	if q := r.URL.Query(); q.Has("order") {
		switch q.Get("order") {
		case "asc":
			ascending = true
		case "desc":
		default:
			return server.InvalidRequest("order must be asc or desc")
		}
	}
	var after usagePosition
	continuing, err := page.After(&after)
	if err != nil {
		return err
	}
	if continuing && (after.ReportedAt == "" || after.ID == "") {
		return server.InvalidRequest("cursor is malformed or belongs to another query")
	}
	// The page and the totals are read in one snapshot, so that they agree
	// while reports arrive.
	var items []Usage
	var totals TokenSummary
	err = snapshot(ctx, c.deps.Store.Main(), func(tx *sql.Tx) error {
		var err error
		if items, err = listUsage(ctx, tx, s.ID, ascending, continuing, after, page.Limit+1); err != nil {
			return err
		}
		totals, err = tokenSummary(ctx, tx, s.ID)
		return err
	})
	if err != nil {
		return err
	}
	return server.WritePageWith(w, page, items, func(u Usage) any {
		return usagePosition{ReportedAt: timefmt.Format(time.Time(u.ReportedAt)), ID: u.ID}
	}, server.Member{Name: "session_id", Value: s.ID}, server.Member{Name: "totals", Value: totals})
}

// listUsage reads up to limit usage records of the session id through q,
// ordered by (reported_at, id), ascending or descending, after the
// position after when continuing.
func listUsage(ctx context.Context, q Querier, id string, ascending, continuing bool, after usagePosition, limit int) ([]Usage, error) {
	cmp, dir := "<", "DESC"
	if ascending {
		cmp, dir = ">", "ASC"
	}
	query := `SELECT ` + usageColumns + ` FROM session_usage WHERE session_id = ?`
	args := []any{id}
	if continuing {
		query += ` AND (reported_at ` + cmp + ` ? OR reported_at = ? AND id ` + cmp + ` ?)`
		args = append(args, after.ReportedAt, after.ReportedAt, after.ID)
	}
	query += ` ORDER BY reported_at ` + dir + `, id ` + dir + ` LIMIT ?`
	args = append(args, limit)
	items := []Usage{}
	err := eachRow(ctx, q, func(scan func(...any) error) error {
		u, err := scanUsage(scan)
		if err != nil {
			return err
		}
		items = append(items, u)
		return nil
	}, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the usage of session %s: %w", id, err)
	}
	return items, nil
}
