package event

import (
	"net/url"
	"strconv"
	"strings"

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

// parseFilter reads the filters workspace, types and session_id from q.
func parseFilter(q url.Values) filter {
	var f filter
	f.Workspace = q.Get("workspace")
	if q.Has("types") {
		f.Types = strings.Split(q.Get("types"), ",")
	}
	f.SessionID = strings.ToLower(q.Get("session_id"))
	return f
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
