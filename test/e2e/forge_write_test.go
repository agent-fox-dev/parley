package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// forgeEvents are the parley.forge.* events of the workspace slug in the
// hub's log, oldest first, as the admin token reads them, each as the log
// serves it.
func (h *workspaceHub) forgeEvents(t *testing.T, slug string) []map[string]any {
	t.Helper()
	path := "/api/v1/events/log?" + url.Values{"after": {"0"}, "workspace": {slug}, "limit": {"200"}}.Encode()
	status, body := h.Call(t, http.MethodGet, path, h.Token, nil)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET %s = %d %s, want 200 with a page", path, status, body)
	}
	var forge []map[string]any
	for _, ev := range page.Items {
		if typ, _ := ev["type"].(string); strings.HasPrefix(typ, "parley.forge.") {
			forge = append(forge, ev)
		}
	}
	return forge
}

// wantForgeEvents fails the test unless events are of the types given, in
// that order, each with metadata ok true.
func wantForgeEvents(t *testing.T, events []map[string]any, types ...string) {
	t.Helper()
	var got []string
	for _, ev := range events {
		got = append(got, ev["type"].(string))
		if meta, _ := ev["metadata"].(map[string]any); meta["ok"] != true {
			t.Errorf("the event %s has metadata %v, want ok true", ev["type"], meta)
		}
	}
	if !reflect.DeepEqual(got, types) {
		t.Errorf("the forge events of demo are %v, want %v", got, types)
	}
}

func TestForgeWrite_AnAgentFilesLabelsAndCommentsOnAnIssueFromTheShell(t *testing.T) {
	parallel(t)
	const token = "tok-write-1"
	const text = "  Seen on main too.\n\nSteps:\n1. run it\n"
	fake := forgetest.New(t)
	h, _ := forgeHub(t, fake, "demo", "acme/widgets.git", token, fake.URL+"/github")
	pat := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"forge:write"}})
	parley := func(stdin string, args ...string) ran {
		var stdout, stderr bytes.Buffer
		code := cli.RunWith(context.Background(), append([]string{"--endpoint-url", h.URL(""), "--api-key", pat}, args...),
			&stdout, &stderr, cli.Hooks{Stdin: strings.NewReader(stdin)})
		return ran{code, stdout.String(), stderr.String()}
	}

	created := parley("", "issue", "create", "demo", "--title", "Flaky test", "--label", "bug")
	labeled := parley("", "issue", "label", "demo", "1", "triage")
	commented := parley(text, "issue", "comment", "demo", "1", "--body-file", "-")

	for name, r := range map[string]ran{"issue create": created, "issue label": labeled, "issue comment": commented} {
		if r.code != 0 {
			t.Fatalf("%s exited %d, stdout %q, stderr %q; want 0", name, r.code, r.stdout, r.stderr)
		}
	}
	if got := created.json(t); got["number"] != float64(1) || got["title"] != "Flaky test" {
		t.Errorf("issue create printed %v, want issue 1 titled Flaky test", got)
	}
	if got, want := labeled.json(t), map[string]any{"status": "labeled", "number": float64(1), "labels": []any{"triage"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("issue label printed %v, want %v", got, want)
	}
	if got := commented.json(t); len(got) != 1 || got["url"] == "" || got["url"] == nil {
		t.Errorf("issue comment printed %v, want an object with only a url", got)
	}

	repo := forgetest.GitHubPrefix + "/repos/acme/widgets"
	issue, found := fake.Issue(repo, 1)
	if !found {
		t.Fatal("the fake forge holds no issue 1")
	}
	if !reflect.DeepEqual(issue.Labels, []string{"bug", "triage"}) || len(issue.Comments) != 1 || issue.Comments[0].Body != text {
		t.Errorf("the forge's issue 1 has labels %v and comments %+v, want bug and triage and one comment %q", issue.Labels, issue.Comments, text)
	}
	for _, q := range fake.Requests() {
		if q.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("%s %s carried Authorization %q, want the stored token", q.Method, q.RawPath, q.Header.Get("Authorization"))
		}
	}
	events := h.forgeEvents(t, "demo")
	wantForgeEvents(t, events, "parley.forge.issue.create", "parley.forge.label.add", "parley.forge.comment.create")
	logged, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	out := created.stdout + created.stderr + labeled.stdout + labeled.stderr + commented.stdout + commented.stderr
	for _, secret := range []string{token, "Seen on main too."} {
		if strings.Contains(out, secret) || bytes.Contains(logged, []byte(secret)) {
			t.Errorf("the commands' output or the forge events hold %q:\n%s\n%s", secret, out, logged)
		}
	}
}

func TestForgeWrite_AnAgentRetriesOpeningAPullRequestAndGetsOne(t *testing.T) {
	parallel(t)
	const token = "tok-write-2"
	fake := forgetest.New(t)
	h, _ := forgeHub(t, fake, "demo", "acme/widgets.git", token, fake.URL+"/github")
	pat := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"forge:write"}})
	body := map[string]any{"title": "Add feature", "head": "feature", "base": "main"}
	key := http.Header{"Idempotency-Key": {"pr-1"}}

	status1, first := h.CallWith(t, http.MethodPost, "/api/v1/workspaces/demo/pulls", pat, body, key)
	status2, again := h.CallWith(t, http.MethodPost, "/api/v1/workspaces/demo/pulls", pat, body, key)

	if status1 != http.StatusCreated || status2 != http.StatusOK || !bytes.Equal(first, again) {
		t.Fatalf("the two opens = %d %s and %d %s, want 201 and 200 with equal bodies", status1, first, status2, again)
	}
	if got := decoded(t, first); got["number"] != float64(1) {
		t.Errorf("the pull request opened is %v, want number 1", got)
	}
	repo := forgetest.GitHubPrefix + "/repos/acme/widgets"
	creates := 0
	for _, q := range fake.Requests() {
		if q.Method == http.MethodPost && q.Path == repo+"/pulls" {
			creates++
		}
	}
	pull, found := fake.Issue(repo, 1)
	if _, second := fake.Issue(repo, 2); creates != 1 || !found || !pull.Pull || pull.Head != "feature" || pull.Base != "main" || second {
		t.Errorf("the forge got %d creates and holds %+v (found %v, a second %v), want one pull request from feature into main",
			creates, pull, found, second)
	}

	status3, comment := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/pulls/1/comments", pat, map[string]any{"body": "ready for review"})
	if got := decoded(t, comment); status3 != http.StatusCreated || got["url"] == "" || got["url"] == nil {
		t.Errorf("the comment = %d %s, want 201 with a url", status3, comment)
	}
	if pull, _ = fake.Issue(repo, 1); len(pull.Comments) != 1 || pull.Comments[0].Body != "ready for review" {
		t.Errorf("the forge's pull request 1 has comments %+v, want one, ready for review", pull.Comments)
	}
	wantForgeEvents(t, h.forgeEvents(t, "demo"), "parley.forge.pull_request.create", "parley.forge.pull_request.comment")
}
