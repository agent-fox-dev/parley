package forge_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
)

func TestCommentOnPull_PostsInTheConversation(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)
	for _, f := range []struct{ name, slug, repo, sent string }{
		{"GitHub", "demo", ghRepo, ghRepo + "/issues/12/comments"},
		{"GitLab", "gl", glSubWidgets, glSubWidgets + "/merge_requests/12/notes"},
	} {
		t.Run(f.name, func(t *testing.T) {
			w.wantComment(t, f.slug, "/pulls/12/comments", f.repo, f.sent, 12, "LGTM")
		})
	}
}

// pullJSON is the pull request p of the fake forge, at the web URL url, as
// the hub answers it.
func pullJSON(t *testing.T, url string, p forgetest.Issue) string {
	t.Helper()
	out, err := json.Marshal(map[string]any{"number": p.Number, "title": p.Title, "body": p.Body, "state": "open", "url": url,
		"draft": p.Draft, "merged": false, "head_sha": "", "head_branch": p.Head, "base_branch": p.Base,
		"author": map[string]string{"login": forgetest.Login}, "created_at": forgetest.Stamp, "updated_at": forgetest.Stamp,
		"closed_at": nil, "merged_at": nil})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestOpenPull_OpensItWithTheHeadAsGivenAndTheDraftAsAsked(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)
	ghURL, glURL := "https://github.com/acme/widgets/pull/12", w.fake.URL+"/group/sub/widgets/-/merge_requests/12"
	gitlab := func(draft any, title string) map[string]any {
		b := map[string]any{"title": title, "head": "feature", "base": "main", "body": "why"}
		if draft != nil {
			b["draft"] = draft
		}
		return b
	}
	cases := []struct {
		name, slug, repo, path, url string
		body                        map[string]any
		// sent is the body the forge must receive, and draft whether the
		// answer is a draft.
		sent  string
		draft bool
	}{
		{"GitHub, no draft", "demo", ghRepo, "/pulls", ghURL,
			map[string]any{"title": "T", "head": "feature", "base": "main"},
			`{"title":"T","head":"feature","base":"main","draft":false}`, false},
		{"GitHub, a draft from another owner's branch with a body", "demo", ghRepo, "/pulls", ghURL,
			map[string]any{"title": "T", "head": "bob:feature", "base": "main", "body": "why", "draft": true},
			`{"title":"T","head":"bob:feature","base":"main","body":"why","draft":true}`, true},
		{"GitHub, draft false", "demo", ghRepo, "/pulls", ghURL,
			map[string]any{"title": "T", "head": "feature", "base": "main", "draft": false},
			`{"title":"T","head":"feature","base":"main","draft":false}`, false},
		{"GitLab, no draft", "gl", glSubWidgets, "/merge_requests", glURL, gitlab(nil, "T"),
			`{"title":"T","source_branch":"feature","target_branch":"main","description":"why"}`, false},
		{"GitLab, a draft", "gl", glSubWidgets, "/merge_requests", glURL, gitlab(true, "T"),
			`{"title":"Draft: T","source_branch":"feature","target_branch":"main","description":"why"}`, true},
		{"GitLab, a draft whose title is marked already", "gl", glSubWidgets, "/merge_requests", glURL, gitlab(true, "Draft: T"),
			`{"title":"Draft: T","source_branch":"feature","target_branch":"main","description":"why"}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w.fake.SetNext(c.repo, 12)
			status, body, sent := w.send(t, http.MethodPost, c.slug, "/pulls", w.ownerKey, c.body)
			if status != http.StatusCreated || sent != 1 {
				t.Fatalf("POST %s/pulls = %d %.300s after %d forge requests, want 201 after 1", c.slug, status, body, sent)
			}
			if last := w.fake.Last(t); last.Method != http.MethodPost || last.RawPath != c.repo+c.path || !sameJSON(last.Body, c.sent) {
				t.Errorf("the forge got %s %s %s, want POST %s%s %s", last.Method, last.RawPath, last.Body, c.repo, c.path, c.sent)
			}
			// The number is the pull request's number on GitHub and the
			// merge request's iid on GitLab, which the fake forge gives a
			// global id other than it.
			p, _ := w.fake.Issue(c.repo, 12)
			var got struct {
				Number     int64  `json:"number"`
				State      string `json:"state"`
				Draft      bool   `json:"draft"`
				BaseBranch string `json:"base_branch"`
			}
			if err := json.Unmarshal(body, &got); err != nil || !sameJSON(body, pullJSON(t, c.url, p)) || got.Number != 12 ||
				got.State != "open" || got.Draft != c.draft || got.BaseBranch != "main" {
				t.Errorf("the answer is %s, want pull request 12, open, draft %v, into main, as the forge holds it: %s",
					body, c.draft, pullJSON(t, c.url, p))
			}
		})
	}
}

func TestOpenPull_AnswersTheForgesRefusal(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)
	cases := []struct {
		name, slug, answer, message string
		status                      int
	}{
		{"GitHub, a pull request exists for the head", "demo", `{"message":"A pull request already exists for acme:feature."}`,
			"the forge answered 422: A pull request already exists for acme:feature.", 422},
		{"GitLab, a merge request exists for the source branch", "gl",
			`{"message":["Another open merge request already exists for this source branch: !5"]}`,
			`the forge answered 409: ["Another open merge request already exists for this source branch: !5"]`, 409},
		{"GitHub, an unknown head", "demo",
			`{"message":"Validation Failed","errors":[{"resource":"PullRequest","field":"head","code":"invalid"}]}`,
			"the forge answered 422: Validation Failed", 422},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w.fake.Script(forgetest.Answer{Status: c.status, Body: c.answer})
			n := w.rec.count()
			status, body, sent := w.send(t, http.MethodPost, c.slug, "/pulls", w.ownerKey,
				map[string]any{"title": "T", "head": "feature", "base": "main"})
			if e := errorOf(t, body); status != http.StatusUnprocessableEntity || e.Type != forge.ErrorTypeRejected ||
				e.Message != c.message || sent != 1 {
				t.Errorf("POST %s/pulls = %d %s after %d forge requests, want 422 %s %q after the create alone",
					c.slug, status, body, sent, forge.ErrorTypeRejected, c.message)
			}
			events := w.rec.since(n)
			if len(events) != 1 {
				t.Fatalf("the create recorded %d events, want one", len(events))
			}
			w.wantEvent(t, events[0], c.slug, "parley.forge.pull_request.create", "pull_request", "none",
				map[string]any{"target": "origin", "ok": false, "error_type": forge.ErrorTypeRejected},
				map[string]any{"head": "feature", "base": "main", "draft": false})
		})
	}
}
