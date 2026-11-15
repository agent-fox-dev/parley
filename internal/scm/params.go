package scm

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// boolParam is the boolean query parameter name of q, def when q has none.
// Only true and false are booleans; any other value is 400
// invalid_request, so that a mistyped value is not read as either.
func boolParam(q url.Values, name string, def bool) (bool, error) {
	if !q.Has(name) {
		return def, nil
	}
	switch q.Get(name) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, server.InvalidRequest(fmt.Sprintf("%s must be true or false", name))
}

// timeParam is the query parameter name of q as an RFC 3339 timestamp, nil
// when q has none; another value is 400 invalid_request.
func timeParam(q url.Values, name string) (*time.Time, error) {
	if !q.Has(name) {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, q.Get(name))
	if err != nil {
		return nil, server.InvalidRequest(fmt.Sprintf("%s must be an RFC 3339 timestamp, such as 2026-01-02T03:04:05Z", name))
	}
	return &t, nil
}

// pathParam is the query parameter path of q, a path of the repository's
// tree; "" when q has none or an empty one, which is 400 invalid_request
// when required is set. A path is legal when it is relative, has no empty,
// "." or ".." segment, does not lie under .git and holds no NUL; any other
// is 400 invalid_request, so that no path a tree cannot hold reaches git.
func pathParam(q url.Values, required bool) (string, error) {
	path := q.Get("path")
	if path == "" {
		if required {
			return "", server.InvalidRequest("path is required")
		}
		return "", nil
	}
	if strings.HasPrefix(path, "/") || strings.ContainsRune(path, 0) {
		return "", server.InvalidRequest("path must be a relative path of the repository, such as docs/api.md")
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "", ".", "..":
			return "", server.InvalidRequest(fmt.Sprintf("path %q has an empty, . or .. segment", path))
		case ".git":
			return "", server.InvalidRequest(fmt.Sprintf("path %q lies under .git", path))
		}
	}
	return path, nil
}
