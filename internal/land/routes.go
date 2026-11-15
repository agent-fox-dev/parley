package land

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes are the land routes, for the server's router: the attempts of a
// change, the attempts of a workspace and one attempt by id.
func (c *Component) Routes() []server.Route {
	need := map[string]string{access.MarkNeed: string(access.NeedRead)}
	read := func(pattern string, h server.HandlerFunc) server.Route {
		return server.Route{Method: http.MethodGet, Pattern: pattern, Scope: "changes:read", Marks: need, Handler: h}
	}
	// The patterns of a change's attempts and of one attempt are those of
	// the contributor table of internal/access, parameter names included,
	// so that a token bound to a fork reaches them on the fork's parent.
	return []server.Route{
		read("/workspaces/:slug/changes/:number/lands", c.changeLands),
		read("/workspaces/:slug/lands", c.workspaceLands),
		read("/workspaces/:slug/lands/:id", c.getLand),
	}
}

// errLandNotFound is the answer for an id that names no attempt of the
// workspace.
func errLandNotFound(workspace, id string) error {
	return server.NewError("land_not_found", http.StatusNotFound, fmt.Sprintf("%s has no land attempt %q", workspace, id))
}

// positive is s as a positive integer in decimal digits, and false for
// anything else, such as 0, -1, +1 or 1.5.
func positive(s string) (int64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// isUUID reports whether s is a UUID in its 36-character form, the one
// form an attempt's id is written in.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return len(s) == 36 && err == nil
}

// secondsOf is created_at, stored in nanoseconds, at the resolution the
// attempt object shows it, so that the lists' order is the order of the
// values the caller sees, and ties among them go by id.
const secondsOf = `(a.created_at / 1000000000)`

// listPosition is an attempt's place in the lists' order.
type listPosition struct {
	Seconds int64  `json:"t"`
	ID      string `json:"id"`
}

// changeLands answers the attempts that cover the change :number of the
// workspace :slug, newest first, in pages of the list envelope. A :number
// that is not a positive integer, or that the workspace has no change of,
// is 404 change_not_found.
func (c *Component) changeLands(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	slug := server.Param(r, "slug")
	number, ok := positive(server.Param(r, "number"))
	if !ok {
		return change.ErrChangeNotFound
	}
	if _, err := c.deps.Changes.ReadChange(r.Context(), c.deps.Store.Main(), slug, number); err != nil {
		return err
	}
	return c.writeList(w, r, page, []string{`a.workspace_slug = ?`,
		`EXISTS (SELECT 1 FROM land_attempt_changes c WHERE c.land_id = a.id AND c.number = ?)`}, slug, number)
}

// workspaceLands answers the attempts whose target is the workspace :slug,
// newest first, in pages of the list envelope, narrowed by every filter
// the query gives: status, repeatable, any of the values; target_branch;
// since, an RFC 3339 timestamp the attempt was created at or after. An
// unknown status or a since that does not parse is 400 invalid_request.
func (c *Component) workspaceLands(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	where, args := []string{`a.workspace_slug = ?`}, []any{server.Param(r, "slug")}
	if wanted := query["status"]; len(wanted) > 0 {
		for _, s := range wanted {
			if !slices.Contains(statuses, s) {
				return server.InvalidRequest("status must be one of " + strings.Join(statuses, ", "))
			}
			args = append(args, s)
		}
		where = append(where, `a.status IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(wanted)), ", ")+`)`)
	}
	if query.Has("target_branch") {
		where, args = append(where, `a.target_branch = ?`), append(args, query.Get("target_branch"))
	}
	if query.Has("since") {
		since, err := time.Parse(time.RFC3339, query.Get("since"))
		if err != nil {
			return server.InvalidRequest("since must be an RFC 3339 timestamp")
		}
		where, args = append(where, `a.created_at >= ?`), append(args, since.UnixNano())
	}
	return c.writeList(w, r, page, where, args...)
}

// writeList answers the page of the attempts that meet every condition of
// where, with args, ordered by created_at, as shown, descending, then by
// id descending.
func (c *Component) writeList(w http.ResponseWriter, r *http.Request, page server.Page, where []string, args ...any) error {
	var after listPosition
	more, err := page.After(&after)
	if err != nil {
		return err
	}
	if more {
		where = append(where, `(`+secondsOf+` < ? OR (`+secondsOf+` = ? AND a.id < ?))`)
		args = append(args, after.Seconds, after.Seconds, after.ID)
	}
	attempts, err := readAttempts(r.Context(), c.deps.Store.Main(), `SELECT `+attemptColumns+` FROM land_attempts a WHERE `+
		strings.Join(where, ` AND `)+` ORDER BY `+secondsOf+` DESC, a.id DESC LIMIT ?`, append(args, page.Limit+1)...)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, attempts, func(a Attempt) any { return listPosition{a.CreatedAt.Unix(), a.ID} })
}

// getLand answers the attempt :id of the workspace :slug: an id that is no
// UUID, unknown, or of an attempt of another workspace is 404
// land_not_found.
func (c *Component) getLand(w http.ResponseWriter, r *http.Request) error {
	slug, id := server.Param(r, "slug"), server.Param(r, "id")
	if !isUUID(id) {
		return errLandNotFound(slug, id)
	}
	a, found, err := readAttempt(r.Context(), c.deps.Store.Main(), slug, id)
	if err != nil {
		return err
	}
	if !found {
		return errLandNotFound(slug, id)
	}
	server.WriteJSON(w, http.StatusOK, a)
	return nil
}
