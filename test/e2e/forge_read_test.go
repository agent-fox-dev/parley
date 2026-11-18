package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// forgeHub starts the hub of a workspace slug whose origin, which takes
// token, serves the repository at path, with the hub's forge client
// trusting the fake forge's certificate. It creates the workspace through
// the API with token as its git_pat, waits until it is cloned, points its
// forge_api_url at api, and returns the hub with a PAT of the workspace's
// owner that holds forge:read alone.
func forgeHub(t *testing.T, fake *forgetest.Fake, slug, path, token, api string) (h *workspaceHub, pat string) {
	t.Helper()
	origin := newOrigin(t, gitrepotest.WithRepoPath(path), gitrepotest.WithBasicAuth("x-token-auth", token))
	h = startWorkspaceHub(t, origin, slug, func(c *hub.Composition) {
		c.AdjustForge(func(d *forge.Deps) { d.Client.Transport = fake.Transport() })
	})
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key,
		map[string]any{"slug": slug, "git_url": origin.URL(), "git_pat": token})
	if status != http.StatusCreated {
		t.Fatalf("create %s = %d %s, want 201", slug, status, body)
	}
	h.waitStatus(t, slug, workspace.StatusReady)
	status, body = h.Call(t, http.MethodPatch, "/api/v1/workspaces/"+slug, h.key,
		map[string]any{"settings": map[string]any{"forge_api_url": api}})
	if status != http.StatusOK {
		t.Fatalf("set forge_api_url of %s = %d %s, want 200", slug, status, body)
	}
	return h, h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"forge:read"}})
}

func TestForgeRead_AnAgentTriagesAWorkspacesIssuesFromTheShell(t *testing.T) {
	parallel(t)
	const token = "tok-1"
	fake := forgetest.New(t)
	h, pat := forgeHub(t, fake, "widgets", "acme/widgets.git", token, fake.URL+"/github")
	issue := func(number int, more map[string]any) map[string]any {
		v := map[string]any{
			"number": number, "title": "Crash", "body": "text", "state": "open",
			"html_url": fmt.Sprintf("https://github.com/acme/widgets/issues/%d", number), "user": map[string]any{"login": "kim"},
			"labels": []any{map[string]any{"name": "bug"}}, "created_at": "2026-10-01T08:00:00Z", "updated_at": "2026-10-01T08:00:00Z",
		}
		maps.Copy(v, more)
		return v
	}
	comment := func(id int) map[string]any {
		return map[string]any{
			"id": id, "body": "seen", "user": map[string]any{"login": "kim"}, "created_at": "2026-10-01T09:00:00Z",
			"html_url": fmt.Sprintf("https://github.com/acme/widgets/issues/7#issuecomment-%d", id),
		}
	}
	repo := forgetest.GitHubPrefix + "/repos/acme/widgets"
	fake.SetList(repo+"/issues", issue(7, nil), issue(9, map[string]any{"pull_request": map[string]any{}}), issue(8, nil))
	fake.SetObject(repo+"/issues/7", issue(7, nil))
	fake.SetList(repo+"/issues/7/comments", comment(1), comment(2))
	parley := func(args ...string) ran {
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), append([]string{"--endpoint-url", h.URL(""), "--api-key", pat}, args...), &stdout, &stderr)
		return ran{code, stdout.String(), stderr.String()}
	}

	listed := parley("issue", "list", "widgets", "--label", "bug")
	thread := parley("issue", "get", "widgets", "7")

	var page struct {
		Items []struct {
			Number int `json:"number"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(listed.stdout), &page); err != nil || listed.code != 0 {
		t.Fatalf("issue list exited %d with %q (%v), stderr %q; want 0 and a page", listed.code, listed.stdout, err, listed.stderr)
	}
	numbers := []int{}
	for _, item := range page.Items {
		numbers = append(numbers, item.Number)
	}
	if !reflect.DeepEqual(numbers, []int{7, 8}) || page.NextCursor != nil {
		t.Errorf("issue list printed the issues %v and next_cursor %v, want 7 and 8 and null", numbers, page.NextCursor)
	}
	if thread.code != 0 {
		t.Fatalf("issue get exited %d, stderr %q; want 0", thread.code, thread.stderr)
	}
	th := thread.json(t)
	if comments, _ := th["comments"].([]any); len(comments) != 2 || th["comments_error"] != nil {
		t.Errorf("issue get printed comments %v and comments_error %v, want two and null", th["comments"], th["comments_error"])
	}
	if got, _ := th["issue"].(map[string]any); got["number"] != float64(7) {
		t.Errorf("issue get printed the issue %v, want issue 7", th["issue"])
	}
	requests := fake.Requests()
	if len(requests) == 0 {
		t.Fatal("the fake forge received no request")
	}
	for _, q := range requests {
		if q.Header.Get("Authorization") != "Bearer "+token || !strings.HasPrefix(q.Header.Get("User-Agent"), "parleyd/") {
			t.Errorf("%s %s carried Authorization %q and User-Agent %q, want Bearer %s and parleyd/…",
				q.Method, q.RawPath, q.Header.Get("Authorization"), q.Header.Get("User-Agent"), token)
		}
	}
	if q := requests[0]; q.Path != repo+"/issues" || !strings.Contains(q.RawQuery, "labels=bug") {
		t.Errorf("the forge was first asked %s?%s, want the issues of acme/widgets labelled bug", q.Path, q.RawQuery)
	}
	if out := listed.stdout + listed.stderr + thread.stdout + thread.stderr; strings.Contains(out, token) {
		t.Errorf("the CLI printed the token: %s", out)
	}
}

func TestForgeRead_AnAgentInATaskForkReadsTheParentsForge(t *testing.T) {
	parallel(t)
	const token = "tok-2"
	fake := forgetest.New(t)
	h, pat := forgeHub(t, fake, "gadgets", "acme/gadgets.git", token, fake.URL+"/gitlab")
	parent := h.record(t, "gadgets")
	ctx := context.Background()
	err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		return fork.Insert(ctx, tx, fork.Fork{Workspace: workspace.Workspace{
			Slug: "gadgets--t1", Kind: access.KindFork, Mode: workspace.ModeStandard, ParentSlug: new("gadgets"),
			OwnerID: parent.OwnerID, OrgID: parent.OrgID, Status: workspace.StatusReady, DefaultBranch: new("main"),
			ForkPoint: &workspace.ForkPoint{Ref: "main", SHA: h.origin.Head("main")}, ExpiresAt: new(now.Add(time.Hour)),
			AutoChange: new(false), CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
		}})
	})
	if err != nil {
		t.Fatalf("insert the fork gadgets--t1: %v", err)
	}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	project := forgetest.GitLabPrefix + "/projects/acme%2Fgadgets"
	fake.SetObject(project+"/merge_requests/12", map[string]any{
		"id": 1012, "iid": 12, "title": "Change", "description": "text", "state": "merged", "draft": false,
		"web_url": "https://gitlab.example.com/acme/gadgets/-/merge_requests/12", "author": map[string]any{"username": "kim"},
		"sha": sha, "source_branch": "feature", "target_branch": "main",
		"created_at": "2026-10-01T08:00:00Z", "updated_at": "2026-10-01T09:00:00Z", "merged_at": "2026-10-01T09:00:00Z",
	})
	fake.Script(forgetest.Answer{Path: "/gitlab/api/v4/projects/acme/gadgets/merge_requests/999", Status: http.StatusNotFound, Body: `{"message":"404 Not Found"}`})

	status, merged := h.Call(t, http.MethodGet, "/api/v1/workspaces/gadgets--t1/pulls/12/state", pat, nil)
	asked := fake.Last(t)
	missingStatus, missing := h.Call(t, http.MethodGet, "/api/v1/workspaces/gadgets--t1/pulls/999/state", pat, nil)

	want := map[string]any{"number": float64(12), "state": "merged", "merged": true, "head_sha": sha}
	if got := decoded(t, merged); status != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Errorf("GET …/pulls/12/state = %d %s, want 200 %v", status, merged, want)
	}
	if asked.RawPath != project+"/merge_requests/12" || asked.Header.Get("Authorization") != "Bearer "+token {
		t.Errorf("the forge was asked %s with Authorization %q, want %s/merge_requests/12 with Bearer %s",
			asked.RawPath, asked.Header.Get("Authorization"), project, token)
	}
	refusal, _ := decoded(t, missing)["error"].(map[string]any)
	if missingStatus != http.StatusNotFound || refusal["error_type"] != "forge_not_found" || refusal["message"] != "the forge answered 404: 404 Not Found" {
		t.Errorf("GET …/pulls/999/state = %d %s, want 404 forge_not_found with the message %q",
			missingStatus, missing, "the forge answered 404: 404 Not Found")
	}
	if bytes.Contains(merged, []byte(token)) || bytes.Contains(missing, []byte(token)) {
		t.Errorf("an answer holds the token: %s %s", merged, missing)
	}
}
