package session

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The groupings of the cost report, and the column expression each groups
// usage records by. Stored timestamps are RFC 3339 in UTC, so their first
// ten characters are the record's UTC calendar date.
var costGroupings = map[string]string{
	"day":     `substr(reported_at, 1, 10)`,
	"session": `session_id`,
	"model":   `model`,
}

// costWindow is how far back the cost report reaches when it is given no
// since.
const costWindow = 24 * time.Hour

// costSums are the token sums of a set of usage records and the number of
// distinct sessions they belong to. It is an answer of token counts only:
// the hub has no prices.
type costSums struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	Sessions        int64 `json:"sessions"`
}

// costItem is one group of the cost report: the UTC date, the session id
// or the model name its records share, and their sums.
type costItem struct {
	Key string `json:"key"`
	costSums
}

// costPeriod is the window of a cost report: since inclusive, until
// exclusive.
type costPeriod struct {
	Since timefmt.Time `json:"since"`
	Until timefmt.Time `json:"until"`
}

// costPosition is the place of a group in the cost report: its key. The
// cursor that carries it is bound to the query, group_by included, so a
// key is never read as one of another grouping.
type costPosition struct {
	Key string `json:"k"`
}

// costRoute serves GET /workspaces/:slug/cost: the sums of the usage
// records of the workspace slug reported in the window [since, until),
// over the whole window, and per day, session or model, ordered by key and
// paged by limit and cursor. The access check of the pipeline has decided
// read access to the workspace, whatever its status. Only the workspace's
// own records count: those of sessions on its task forks carry the forks'
// slugs.
func (c *Component) costRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	slug := server.Param(r, "slug")
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	groupBy, err := oneOf(q, "group_by", []string{"day", "session", "model"})
	if err != nil {
		return err
	}
	if groupBy == "" {
		groupBy = "day"
	}
	period, err := c.costPeriodOf(q)
	if err != nil {
		return err
	}
	var after costPosition
	continuing, err := page.After(&after)
	if err != nil {
		return err
	}
	if continuing && after.Key == "" {
		return server.InvalidRequest("cursor is malformed or belongs to another query")
	}

	since, until := timefmt.Format(time.Time(period.Since)), timefmt.Format(time.Time(period.Until))
	totals, err := c.costTotals(ctx, slug, since, until)
	if err != nil {
		return err
	}
	items, err := c.costItems(ctx, slug, since, until, costGroupings[groupBy], continuing, after.Key, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePageWith(w, page, items, func(i costItem) any { return costPosition{Key: i.Key} },
		server.Member{Name: "workspace", Value: slug},
		server.Member{Name: "period", Value: period},
		server.Member{Name: "group_by", Value: groupBy},
		server.Member{Name: "totals", Value: totals},
	)
}

// costPeriodOf reads the window of a cost report from its query q: since,
// 24 hours before the clock's time when absent, and until, the clock's
// time when absent, both RFC 3339 with any offset, in UTC truncated to
// whole seconds, as stored timestamps are. An absent until is the clock's
// time rounded up to a whole second instead: a record is stored with the
// whole second it was reported in, so a window that ended at the clock's
// time truncated would leave out what was reported in the current second.
// since must be before until.
func (c *Component) costPeriodOf(q url.Values) (costPeriod, error) {
	now := c.deps.Now().UTC()
	since, err := costBound(q, "since", now.Truncate(time.Second).Add(-costWindow))
	if err != nil {
		return costPeriod{}, err
	}
	end := now.Truncate(time.Second)
	if end.Before(now) {
		end = end.Add(time.Second)
	}
	until, err := costBound(q, "until", end)
	if err != nil {
		return costPeriod{}, err
	}
	if !since.Before(until) {
		return costPeriod{}, server.InvalidRequest("since must be before until")
	}
	return costPeriod{Since: timefmt.Time(since), Until: timefmt.Time(until)}, nil
}

// costBound reads the query parameter name of q, an RFC 3339 timestamp,
// in UTC truncated to whole seconds; def when it is absent.
func costBound(q url.Values, name string, def time.Time) (time.Time, error) {
	v, err := single(q, name)
	if err != nil || !q.Has(name) {
		return def, err
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, server.InvalidRequest(name + " must be an RFC 3339 timestamp, such as 2026-01-02T03:04:05Z")
	}
	return t.UTC().Truncate(time.Second), nil
}

// costWhere selects the usage records of the workspace slug reported in
// [since, until), with its arguments.
const costWhere = ` FROM session_usage WHERE workspace_slug = ? AND reported_at >= ? AND reported_at < ?`

// costTotals sums the usage records of the workspace slug reported in
// [since, until): zeros when there are none.
func (c *Component) costTotals(ctx context.Context, slug, since, until string) (costSums, error) {
	var s costSums
	err := c.deps.Store.Main().QueryRowContext(ctx, `
SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_read_tokens), 0),
	COUNT(DISTINCT session_id)`+costWhere, slug, since, until).
		Scan(&s.InputTokens, &s.OutputTokens, &s.CacheReadTokens, &s.Sessions)
	if err != nil {
		return costSums{}, fmt.Errorf("sum the usage of workspace %s: %w", slug, err)
	}
	return s, nil
}

// costItems reads up to limit groups of the usage records of the workspace
// slug reported in [since, until), grouped by the expression key, ordered
// by key ascending, after the key after when continuing.
func (c *Component) costItems(ctx context.Context, slug, since, until, key string, continuing bool, after string, limit int) ([]costItem, error) {
	query := `SELECT ` + key + ` AS k, SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens), COUNT(DISTINCT session_id)` + costWhere
	args := []any{slug, since, until}
	if continuing {
		query += ` AND ` + key + ` > ?`
		args = append(args, after)
	}
	query += ` GROUP BY k ORDER BY k LIMIT ?`
	args = append(args, limit)
	items := []costItem{}
	err := eachRow(ctx, c.deps.Store.Main(), func(scan func(...any) error) error {
		var i costItem
		if err := scan(&i.Key, &i.InputTokens, &i.OutputTokens, &i.CacheReadTokens, &i.Sessions); err != nil {
			return err
		}
		items = append(items, i)
		return nil
	}, query, args...)
	if err != nil {
		return nil, fmt.Errorf("group the usage of workspace %s: %w", slug, err)
	}
	return items, nil
}
