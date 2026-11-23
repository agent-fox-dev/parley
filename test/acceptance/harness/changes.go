package harness

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

// Clone clones remote into a new directory with credential as the password
// of the remote's URL, so that every later fetch and push of the clone
// carries it, and returns the clone's path.
func (h *Hub) Clone(remote, credential string) string {
	h.t.Helper()
	dir := filepath.Join(h.t.TempDir(), "clone")
	h.Git("", "", "clone", "-q", AuthURL(remote, credential), dir).Must(h.t)
	return dir
}

// Rev is the commit id rev names in the clone dir.
func (h *Hub) Rev(dir, rev string) string {
	h.t.Helper()
	return strings.TrimSpace(h.Git(dir, "", "rev-parse", "--verify", rev+"^{commit}").Must(h.t).Stdout)
}

// Get sends GET path to the hub with credential and fails the test unless
// the answer is 200.
func (h *Hub) Get(credential, path string) Response {
	h.t.Helper()
	return h.Call(http.MethodGet, path, credential, nil).Want(h.t, http.StatusOK)
}

// ChangeOn returns the open change of workspace whose source is branch,
// failing the test when there is none: the change list has no filter on
// the source.
func (h *Hub) ChangeOn(credential, workspace, branch string) map[string]any {
	h.t.Helper()
	open := h.Get(credential, "/api/v1/workspaces/"+workspace+"/changes?state=open").Items()
	for _, c := range open {
		if Field(c, "source.branch") == branch {
			return c
		}
	}
	h.t.Fatalf("no open change of %s has the source branch %s: %v", workspace, branch, open)
	return nil
}

// Land asks the hub to land change number of workspace with credential,
// waiting for the attempt as long as the land route does by default, and
// returns the answer, failing the test unless it is 200.
func (h *Hub) Land(credential, workspace string, number int) Response {
	h.t.Helper()
	path := fmt.Sprintf("/api/v1/workspaces/%s/changes/%d/land", workspace, number)
	return h.Call(http.MethodPost, path, credential, nil).Want(h.t, http.StatusOK)
}

// Field is the member of m at path, its names separated by dots, such as
// "change.number"; nil when a name along it is missing.
func Field(m map[string]any, path string) any {
	var v any = m
	for _, name := range strings.Split(path, ".") {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = obj[name]
	}
	return v
}

// Num is the JSON number v as an int, -1 when v is not a number.
func Num(v any) int {
	f, ok := v.(float64)
	if !ok {
		return -1
	}
	return int(f)
}
