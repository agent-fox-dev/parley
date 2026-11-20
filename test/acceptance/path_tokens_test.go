package acceptance

import (
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

// pathTokenChange starts a hub with demo, mints its owner a token whose
// writes are limited to a/**, and with it clones demo, pushes the branch pa
// adding a/x.txt, which opens a change, and commits a/y.txt on pa through
// the source control API, which gives the change its version 2. It returns
// the token, the clone and the change's number.
func pathTokenChange(t *testing.T) (hub *harness.Hub, demo *harness.Workspace, token, clone string, number int) {
	t.Helper()
	hub, demo = demoHub(t)
	token = hub.PAT(demo.Owner, []string{"git:write", "changes:write"}, "a/**")
	clone, number = pushedBy(t, hub, demo, token, "pa", map[string]string{"a/x.txt": "x\n"})
	head := hub.Rev(clone, "HEAD")
	if sha := harness.Str(hub.Get(token, scm+"/refs/pa").Body["sha"]); sha != head {
		t.Fatalf("pa is at %s on the hub, want the pushed %s", sha, head)
	}
	hub.Call(http.MethodPost, scm+"/commits", token, map[string]any{
		"branch": "pa", "expected_head_sha": head, "message": "Add a/y.txt", "operations": []any{put("a/y.txt", "y\n")},
	}).Want(t, http.StatusCreated)
	return hub, demo, token, clone, number
}

func TestPathToken_WritesUnderItsPathsAreAccepted(t *testing.T) {
	t.Parallel()
	hub, _, token, _, number := pathTokenChange(t)
	open := hub.Get(token, changes+"?state=open").Items()
	if len(open) != 1 || harness.Field(open[0], "source.branch") != "pa" ||
		harness.Num(open[0]["number"]) != number || harness.Num(open[0]["version"]) != 2 {
		t.Errorf("the open changes are %v, want change %d of pa alone, at version 2", open, number)
	}
}

func TestPathToken_WriteOutsideItsPathsIsRefusedNamingThePath(t *testing.T) {
	t.Parallel()
	hub, demo, token, clone, number := pathTokenChange(t)
	key := demo.Owner.Key
	state := func() (string, int) {
		return harness.Str(hub.Get(key, scm+"/refs/pa").Body["sha"]), harness.Num(hub.Get(key, changePath(number, "")).Body["version"])
	}
	beforeSHA, beforeVersion := state()
	const refusal = "path b/x is outside this token's allowed paths"

	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"git-mixed", map[string]string{"a/x.txt": "x, changed\n", "b/x": "b\n"}},
		{"git-alone", map[string]string{"b/x": "b\n"}},
	} {
		hub.Git(clone, "", "fetch", "-q", "origin").Must(t)
		hub.Git(clone, "", "reset", "-q", "--hard", "origin/pa").Must(t)
		hub.CommitFiles(clone, "Touch b/x", tc.files)
		if push := hub.Git(clone, "", "push", "origin", "pa"); push.Code == 0 || !strings.Contains(push.Stderr, refusal) {
			t.Errorf("%s: git push exited %d with\n%s\nwant a refusal naming b/x", tc.name, push.Code, push.Stderr)
		}
	}

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"api-mixed", map[string]any{"branch": "pa", "expected_head_sha": beforeSHA, "message": "Touch b/x",
			"operations": []any{put("a/z.txt", "z\n"), put("b/x", "b\n")}}},
		{"api-alone-new-branch", map[string]any{"branch": "pb", "base_ref": "main", "message": "Touch b/x",
			"operations": []any{put("b/x", "b\n")}}},
	} {
		r := hub.Call(http.MethodPost, scm+"/commits", token, tc.body)
		message := harness.Str(harness.Field(r.Body, "error.message"))
		if r.Status != http.StatusForbidden || r.ErrorType() != "path_not_allowed" || !strings.Contains(message, "b/x") {
			t.Errorf("%s: the commit answered %d %s, want 403 path_not_allowed naming b/x", tc.name, r.Status, r.Raw)
		}
	}

	if sha, version := state(); sha != beforeSHA || version != beforeVersion {
		t.Errorf("pa is at %s and the change at version %d, want them unmoved at %s and %d", sha, version, beforeSHA, beforeVersion)
	}
	if r := hub.Call(http.MethodGet, scm+"/refs/pb", key, nil); r.Status != http.StatusNotFound {
		t.Errorf("pb answered %d %s, want 404: the refused commit created it", r.Status, r.Raw)
	}
}
