package event

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// filter is what a consumer of the stream or the log asks for besides a
// position: the events that match every filter it gives.
type filter struct {
	// Workspace is the one workspace slug the events must concern; "" for
	// any.
	Workspace string
	// Types are the entries an event's type must match one of: an exact
	// type, or a prefix ending in * that matches every type beginning with
	// the text before the *; nil for any type.
	Types []string
	// SessionID is the session the events must belong to, in lower case;
	// "" for any.
	SessionID string
}

// match reports whether rec matches every filter of f.
func (f filter) match(rec Record) bool {
	if f.Workspace != "" && (rec.Workspace == nil || *rec.Workspace != f.Workspace) {
		return false
	}
	if f.SessionID != "" && (rec.SessionID == nil || *rec.SessionID != f.SessionID) {
		return false
	}
	if f.Types == nil {
		return true
	}
	for _, entry := range f.Types {
		if prefix, ok := strings.CutSuffix(entry, "*"); ok {
			if strings.HasPrefix(rec.Type, prefix) {
				return true
			}
		} else if rec.Type == entry {
			return true
		}
	}
	return false
}

// parseQuery reads a request's query string. One that does not parse is
// refused rather than read in part, which would drop a filter or a position
// silently.
func parseQuery(rawQuery string) (url.Values, error) {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, server.InvalidRequest("the query string is malformed")
	}
	return q, nil
}

// maxTypes is how many entries the types filter may have.
const maxTypes = 20

// parseFilter reads the filters workspace, types and session_id from q, and
// refuses a malformed one with 400 invalid_request before any event is
// read: a types value that is empty, has an empty entry, has more than
// maxTypes entries or has an entry with * anywhere but last, and a
// session_id that is not a UUID.
func parseFilter(q url.Values) (filter, error) {
	var f filter
	f.Workspace = q.Get("workspace")
	if q.Has("types") {
		f.Types = strings.Split(q.Get("types"), ",")
		if len(f.Types) > maxTypes {
			return filter{}, server.InvalidRequest(fmt.Sprintf("types has more than %d entries", maxTypes))
		}
		for _, entry := range f.Types {
			if entry == "" {
				return filter{}, server.InvalidRequest("types has an empty entry")
			}
			if i := strings.IndexByte(entry, '*'); i >= 0 && i != len(entry)-1 {
				return filter{}, server.InvalidRequest(fmt.Sprintf("types entry %q has a * that is not its last character", entry))
			}
		}
	}
	if q.Has("session_id") {
		id := q.Get("session_id")
		if !isUUID(id) {
			return filter{}, server.InvalidRequest("session_id must be a UUID")
		}
		f.SessionID = strings.ToLower(id)
	}
	return f, nil
}

// parsePosition reads a resume position given as name: a non-negative
// integer, the seq after which the consumer resumes.
func parsePosition(name, value string) (int64, error) {
	pos, err := strconv.ParseInt(value, 10, 64)
	if err != nil || pos < 0 {
		return 0, server.InvalidRequest(name + " must be a non-negative integer")
	}
	return pos, nil
}

// lastEventID is the header in which a reconnecting client of a stream sends
// the id of the last frame it received.
const lastEventID = "Last-Event-ID"

// streamPosition reads the resume position of a stream: the Last-Event-ID
// header when the request has one, which wins over after, else the query
// parameter after. It returns the name the position was read from, "" when
// the request gives neither.
func streamPosition(r *http.Request, q url.Values) (int64, string, error) {
	if ids := r.Header.Values(lastEventID); len(ids) > 0 {
		pos, err := parsePosition(lastEventID, ids[0])
		return pos, lastEventID, err
	}
	if q.Has("after") {
		pos, err := parsePosition("after", q.Get("after"))
		return pos, "after", err
	}
	return 0, "", nil
}

// checkPosition holds the resume position pos, read from the parameter or
// header name, to the log's bounds b: a position above last_seq is 400
// invalid_request; one below oldest_seq minus 1, after which at least one
// event has left the log, is 410 cursor_expired with oldest_seq, whether or
// not the caller could have seen the events that left. last_seq, oldest_seq
// minus 1 and 0 on a log that was never written are valid.
func checkPosition(name string, pos int64, b Bounds) error {
	if pos > b.LastSeq {
		return server.InvalidRequest(fmt.Sprintf("%s must not be greater than last_seq (%d)", name, b.LastSeq))
	}
	if pos < b.OldestSeq-1 {
		return server.NewError("cursor_expired", http.StatusGone, "event cursor expired").With("oldest_seq", b.OldestSeq)
	}
	return nil
}

// coverage is the covered set installed with SetCoverage, nil while none
// is.
func (c *Component) coverage() Coverage {
	c.injected.mu.RLock()
	defer c.injected.mu.RUnlock()
	return c.injected.coverage
}

// errWorkspaceNotFound refuses a workspace filter the caller may not read.
// It is the access rule's answer for a workspace route, so that an existing
// workspace the caller cannot read and a slug with no workspace answer the
// same bytes.
var errWorkspaceNotFound = server.NotFound("workspace not found")

// checkWorkspace refuses the workspace filter slug unless caller may read
// the workspace: the admin token may read every slug, a deleted
// workspace's included; a workspace-bound token only a slug its binding
// covers, and nothing without a covered set; and every caller only a
// workspace on which the access rule gives it a level. A failed read is
// returned as it is, never a grant.
func (c *Component) checkWorkspace(ctx context.Context, caller Caller, slug string) error {
	if slug == "" || caller.Admin {
		return nil
	}
	if caller.Bound() {
		cov := c.coverage()
		if cov == nil {
			return errWorkspaceNotFound
		}
		ok, err := cov.Covers(ctx, caller, slug)
		if err != nil {
			return fmt.Errorf("check the workspace filter %s: read the token's covered set: %w", slug, err)
		}
		if !ok {
			return errWorkspaceNotFound
		}
	}
	if c.deps.Access == nil {
		return errWorkspaceNotFound
	}
	level, _, found, err := c.deps.Access.Level(ctx, caller.accessCaller(), slug)
	if err != nil {
		return fmt.Errorf("check the workspace filter %s: %w", slug, err)
	}
	if !found || level == access.LevelNone {
		return errWorkspaceNotFound
	}
	return nil
}
