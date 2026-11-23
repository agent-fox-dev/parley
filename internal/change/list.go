package change

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/store"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The values the list's filters accept.
var (
	listStates     = []string{StateDraft, StateOpen, StateLanded, StateAbandoned}
	listMergeables = []string{MergeableClean, MergeableConflict, MergeableUnknown}
	listLandings   = []string{LandingReady, LandingPending, LandingBlocked}
)

// secondsOf is updated_at, stored in nanoseconds, at the resolution the
// change object shows it, so that the list's order is the order of the
// values the caller sees, and ties among them go by number.
const secondsOf = `(updated_at / 1000000000)`

// listQuery is what a list request asks for.
type listQuery struct {
	// workspace is the workspace whose changes are listed: :slug, or the
	// parent of a fork's slug.
	workspace string
	// where and args are the SQL conditions every listed change meets,
	// beyond its workspace, combined with AND.
	where []string
	args  []any
	// scoped and scopedArgs count the conditions, and their arguments,
	// that say which changes the request may see at all, which come first.
	scoped, scopedArgs int
	// path is the glob one of a change's stored paths must match, "" for
	// none.
	path string
	// byNumber orders by number ascending, instead of by updated_at and
	// number descending.
	byNumber bool
	// stackOf is the change whose stack is listed, 0 for none.
	stackOf int64
	// withPaths keeps paths and paths_truncated in the items.
	withPaths bool
}

// filter adds the condition cond with its args.
func (lq *listQuery) filter(cond string, args ...any) {
	lq.where = append(lq.where, cond)
	lq.args = append(lq.args, args...)
}

// listPosition is the place of a change in the list's order: its updated_at
// in seconds and number, or its index in a stack.
type listPosition struct {
	Seconds int64 `json:"s,omitempty"`
	Number  int64 `json:"n,omitempty"`
	Index   int   `json:"i,omitempty"`
}

// listItem is a change as the list shows it: without conflict, and
// without paths and paths_truncated unless include asks for them.
type listItem struct {
	Change
	withPaths bool
}

// MarshalJSON writes the change object without the members the list
// leaves out.
func (it listItem) MarshalJSON() ([]byte, error) {
	type plain Change
	// The members of the outer struct hide those of the change of the
	// same name, and a nil one is left out.
	out := struct {
		plain
		Conflict       *struct{}    `json:"conflict,omitempty"`
		Paths          *[]string    `json:"paths,omitempty"`
		PathsTruncated *bool        `json:"paths_truncated,omitempty"`
		Dependants     []int64      `json:"dependants"`
		Sessions       []string     `json:"sessions"`
		Labels         []string     `json:"labels"`
		CreatedAt      timefmt.Time `json:"created_at"`
		UpdatedAt      timefmt.Time `json:"updated_at"`
	}{
		plain: plain(it.Change), Dependants: nonNil(it.Dependants), Sessions: nonNil(it.Sessions), Labels: nonNil(it.Labels),
		CreatedAt: timefmt.Time(it.CreatedAt), UpdatedAt: timefmt.Time(it.UpdatedAt),
	}
	if it.withPaths {
		paths := nonNil(it.Paths)
		out.Paths, out.PathsTruncated = &paths, &it.PathsTruncated
	}
	return json.Marshal(out)
}

// listChanges answers the changes of the workspace :slug that meet every
// filter of the query, in pages of the list envelope. On a fork's slug they
// are the changes of the fork's parent whose source is the fork, since a
// fork holds no changes; a token bound to a fork, calling on the fork's
// parent, is confined the same way.
func (c *Component) listChanges(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after listPosition
	if _, err := page.After(&after); err != nil {
		return err
	}
	lq, err := parseList(r)
	if err != nil {
		return err
	}
	ctx, q := r.Context(), c.deps.Store.Main()
	if lq.stackOf != 0 {
		return c.listStack(ctx, w, q, page, after, lq)
	}
	where, args := lq.conditions()
	order := `ORDER BY ` + secondsOf + ` DESC, number DESC`
	if lq.byNumber {
		order = `ORDER BY number`
		if after.Number != 0 {
			where, args = append(where, `number > ?`), append(args, after.Number)
		}
	} else if after.Number != 0 {
		where = append(where, `(`+secondsOf+` < ? OR (`+secondsOf+` = ? AND number < ?))`)
		args = append(args, after.Seconds, after.Seconds, after.Number)
	}
	query := `SELECT number, paths FROM changes WHERE ` + strings.Join(where, ` AND `) + ` ` + order
	if lq.path == "" {
		query += fmt.Sprintf(` LIMIT %d`, page.Limit+1)
	}
	numbers, err := matching(ctx, q, lq.path, page.Limit+1, query, args...)
	if err != nil {
		return err
	}
	items, err := readItems(ctx, q, lq, numbers)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, items, func(it listItem) any {
		if lq.byNumber {
			return listPosition{Number: it.Number}
		}
		return listPosition{Seconds: it.UpdatedAt.Unix(), Number: it.Number}
	})
}

// listStack answers the stack of the change lq.stackOf: from the change
// that depends on nothing up through the changes that depend on it, in
// any state and whatever sort asks, keeping the changes that meet the
// other filters. A change of the stack is placed by its index in it.
func (c *Component) listStack(ctx context.Context, w http.ResponseWriter, q Querier, page server.Page, after listPosition, lq listQuery) error {
	where, args := lq.conditions()
	named, err := queryList[int64](ctx, q, `SELECT number FROM changes WHERE `+strings.Join(lq.scope(), ` AND `)+` AND number = ?`,
		append(lq.scopeArgs(), lq.stackOf)...)
	if err != nil {
		return err
	}
	if len(named) == 0 {
		return errChangeNotFound(lq.workspace, lq.stackOf)
	}
	stack, err := stackOf(ctx, q, lq.workspace, lq.stackOf)
	if err != nil {
		return err
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(stack)), ",")
	for _, n := range stack {
		args = append(args, n)
	}
	kept, err := matching(ctx, q, lq.path, -1, `SELECT number, paths FROM changes WHERE `+strings.Join(where, ` AND `)+
		` AND number IN (`+in+`)`, args...)
	if err != nil {
		return err
	}
	type placed struct {
		listItem
		index int
	}
	var numbers []int64
	var indexes []int
	for i, n := range stack {
		if i+1 > after.Index && slices.Contains(kept, n) && len(numbers) <= page.Limit {
			numbers, indexes = append(numbers, n), append(indexes, i+1)
		}
	}
	items, err := readItems(ctx, q, lq, numbers)
	if err != nil {
		return err
	}
	out := make([]placed, len(items))
	for i, it := range items {
		out[i] = placed{it, indexes[i]}
	}
	return server.WritePage(w, page, out, func(p placed) any { return listPosition{Index: p.index} })
}

// stackOf is the stack of the change number of the workspace, lowest
// first: down its stored depends_on links to the change that depends on
// nothing, then up through every change that depends on one already in
// the stack, level by level, lower numbers first within a level.
func stackOf(ctx context.Context, q Querier, workspace string, number int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT number, depends_on FROM changes WHERE workspace_slug = ? ORDER BY number`, workspace)
	if err != nil {
		return nil, fmt.Errorf("read the stack links of %s: %w", workspace, err)
	}
	defer rows.Close()
	dependsOn, dependants := map[int64]int64{}, map[int64][]int64{}
	for rows.Next() {
		var n int64
		var on *int64
		if err := rows.Scan(&n, &on); err != nil {
			return nil, err
		}
		if on != nil {
			dependsOn[n] = *on
			dependants[*on] = append(dependants[*on], n)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A cycle, which the stack feature never stores, ends the walk where
	// it would repeat a change.
	bottom, seen := number, map[int64]bool{number: true}
	for {
		on, ok := dependsOn[bottom]
		if !ok || seen[on] {
			break
		}
		bottom, seen[on] = on, true
	}
	stack, seen := []int64{bottom}, map[int64]bool{bottom: true}
	for i := 0; i < len(stack); i++ {
		for _, d := range dependants[stack[i]] {
			if !seen[d] {
				stack, seen[d] = append(stack, d), true
			}
		}
	}
	return stack, nil
}

// matching runs query, which selects the number and the stored paths of
// changes in the list's order, and returns the numbers of those with a
// path that glob matches (every one for ""), at most limit of them (all
// for a negative limit).
func matching(ctx context.Context, q Querier, glob string, limit int, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list changes: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() && (limit < 0 || len(out) < limit) {
		var (
			n     int64
			raw   string
			paths []string
		)
		if err := rows.Scan(&n, &raw); err != nil {
			return nil, err
		}
		if glob != "" {
			if err := json.Unmarshal([]byte(raw), &paths); err != nil {
				return nil, fmt.Errorf("read the paths of change %d: %w", n, err)
			}
			if !slices.ContainsFunc(paths, func(p string) bool { return identity.MatchPath(glob, p) }) {
				continue
			}
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// readItems reads the changes numbered numbers of the list's workspace, in
// their order, as list items.
func readItems(ctx context.Context, q Querier, lq listQuery, numbers []int64) ([]listItem, error) {
	items := make([]listItem, 0, len(numbers))
	for _, n := range numbers {
		ch, err := readChange(ctx, q, lq.workspace, n)
		if err != nil {
			return nil, err
		}
		items = append(items, listItem{ch, lq.withPaths})
	}
	return items, nil
}

// scope are the conditions of the changes the request may see at all: of
// the workspace, and from the fork for a fork's slug or a token confined
// to its fork; scopeArgs are their arguments.
func (lq listQuery) scope() []string { return lq.where[:lq.scoped] }

func (lq listQuery) scopeArgs() []any { return lq.args[:lq.scopedArgs] }

// conditions are every condition of the query and their arguments, in
// fresh slices.
func (lq listQuery) conditions() ([]string, []any) {
	return slices.Clone(lq.where), slices.Clone(lq.args)
}

// parseList reads the list request: the workspace and confinement the
// shared checks and the token's binding give, the filters, the order and
// include. A value outside a filter's domain, author=me without a user, a
// stack_of that is not a positive integer or an include other than paths is
// 400 invalid_request.
func parseList(r *http.Request) (listQuery, error) {
	slug := server.Param(r, "slug")
	lq := listQuery{workspace: slug}
	if d, ok := access.DecisionOf(r); ok && d.Record.Kind == access.KindFork {
		lq.workspace = d.Record.ParentSlug
	}
	lq.filter(`workspace_slug = ?`, lq.workspace)
	if lq.workspace != slug {
		lq.filter(`source_workspace = ?`, slug)
	}
	if fork, ok := confinedTo(r); ok {
		lq.filter(`source_workspace = ?`, fork)
	}
	lq.scoped, lq.scopedArgs = len(lq.where), len(lq.args)

	query := r.URL.Query()
	for _, include := range query["include"] {
		for name := range strings.SplitSeq(include, ",") {
			if name != "paths" {
				return listQuery{}, server.InvalidRequest(fmt.Sprintf("include of the list takes only paths; %q is not it", name))
			}
			lq.withPaths = true
		}
	}
	switch sort := query["sort"]; {
	case len(sort) == 0:
	case len(sort) == 1 && sort[0] == "number":
		lq.byNumber = true
	default:
		return listQuery{}, server.InvalidRequest("sort takes only number")
	}
	if query.Has("stack_of") {
		n, ok := server.Positive(query.Get("stack_of"))
		if !ok {
			return listQuery{}, server.InvalidRequest("stack_of must be a positive integer")
		}
		lq.stackOf = n
	}

	states := query["state"]
	for _, s := range states {
		if !slices.Contains(listStates, s) {
			return listQuery{}, server.InvalidRequest(fmt.Sprintf("state takes %s; %q is none of them", strings.Join(listStates, ", "), s))
		}
	}
	if len(states) == 0 {
		states = []string{StateDraft, StateOpen}
	}
	// A stack is listed in every state.
	if lq.stackOf == 0 {
		lq.filter(`state IN (`+strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")+`)`, store.Args(states)...)
	}
	for _, f := range []struct {
		name, column string
		domain       []string
	}{
		{"mergeable", `mergeable`, listMergeables},
		{"landing_status", `json_extract(landing, '$.status')`, listLandings},
		{"target_branch", `target_branch`, nil},
		{"source_workspace", `source_workspace`, nil},
	} {
		if !query.Has(f.name) {
			continue
		}
		v := query.Get(f.name)
		if f.domain != nil && !slices.Contains(f.domain, v) {
			return listQuery{}, server.InvalidRequest(fmt.Sprintf("%s takes %s; %q is none of them", f.name, strings.Join(f.domain, ", "), v))
		}
		lq.filter(f.column+` = ?`, v)
	}
	if query.Has("author") {
		author, err := authorOf(r, query)
		if err != nil {
			return listQuery{}, err
		}
		lq.filter(`author_id = ?`, author)
	}
	if query.Has("session_id") {
		lq.filter(`EXISTS (SELECT 1 FROM change_sessions s WHERE s.workspace_slug = changes.workspace_slug
	AND s.number = changes.number AND s.session_id = ?)`, strings.ToLower(query.Get("session_id")))
	}
	if query.Has("label") {
		lq.filter(`EXISTS (SELECT 1 FROM json_each(changes.labels) WHERE json_each.value = ?)`, query.Get("label"))
	}
	lq.path = query.Get("path")
	return lq, nil
}

// authorOf is the user the author filter names: a user id, or the
// caller's user for me, which the admin token, having none, cannot ask.
func authorOf(r *http.Request, query url.Values) (string, error) {
	author := query.Get("author")
	if author != "me" {
		return author, nil
	}
	p, _ := identity.PrincipalOf(r.Context())
	if p.User == nil {
		return "", server.InvalidRequest("author=me names the caller's user, and the admin token has none")
	}
	return p.User.ID, nil
}
