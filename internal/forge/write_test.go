package forge_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// writeRoute is a forge write route as the write tests call it: its method,
// its path below /api/v1/workspaces/<slug>, and a body it accepts.
type writeRoute struct {
	method, path string
	body         any
}

// writeRoutes are the forge write routes, each with a body it accepts. The
// tests that run over every write route run over these.
var writeRoutes = []writeRoute{
	{http.MethodPost, "/issues", map[string]any{"title": "t"}},
}

// seedIssue stores issue 7, open and labelled bug, in the fake forge's
// repository repo, so that every write route has an issue to work on.
func (w *world) seedIssue(repo string) {
	w.fake.AddIssue(repo, forgetest.Issue{Number: 7, Title: "Crash", State: "open", Labels: []string{"bug"}})
}

// The fake GitHub's repository upstream/project, which the carry-patch
// workspaces of the write tests track.
const ghUpstream = forgetest.GitHubPrefix + "/repos/upstream/project"

func TestWriteRoute_RefusesWithoutSendingAnything(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gh := new("https://github.com/acme/widgets.git")
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "old", Status: workspace.StatusArchived, GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "anon", GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "cp-anon", Mode: workspace.ModeCarryPatch, GitURL: gh,
		UpstreamURL: new("https://github.com/upstream/project.git"), Settings: w.github(forge.SettingUpstreamAPIURL, w.fake.GitHubURL())})
	for _, slug := range []string{"demo", "old"} {
		w.putSecret(t, slug, workspace.SecretGitPAT, slug+"-token")
	}
	// The forks: of demo owned by a member who can only read demo, and of
	// the archived old owned by its owner and by that member.
	w.addWorkspace(t, workspace.Workspace{Slug: "member-fork", ParentSlug: new("demo"), OwnerID: w.member})
	w.addWorkspace(t, workspace.Workspace{Slug: "old-fork", ParentSlug: new("old")})
	w.addWorkspace(t, workspace.Workspace{Slug: "old-member-fork", ParentSlug: new("old"), OwnerID: w.member})
	readPAT := w.pat(t, w.owner, "", forge.ScopeRead)

	cases := []struct {
		name, credential, slug, query string
		status                        int
		errorType                     string
	}{
		{"a PAT with forge:read only", readPAT, "demo", "", http.StatusForbidden, "insufficient_scope"},
		{"a PAT with forge:read only on an unknown slug", readPAT, "nope", "", http.StatusForbidden, "insufficient_scope"},
		{"a member who can only read", w.memberKey, "demo", "", http.StatusForbidden, "forbidden"},
		{"a member who can only read an archived workspace", w.memberKey, "old", "", http.StatusForbidden, "forbidden"},
		{"a user without access", w.outKey, "demo", "", http.StatusNotFound, "not_found"},
		{"the owner of an archived workspace", w.ownerKey, "old", "", http.StatusConflict, "workspace_archived"},
		{"a fork's owner who can only read its parent", w.memberKey, "member-fork", "", http.StatusForbidden, "forbidden"},
		{"the owner of a fork and its archived parent", w.ownerKey, "old-fork", "", http.StatusConflict, "parent_archived"},
		{"a fork's owner who can only read its archived parent", w.memberKey, "old-member-fork", "", http.StatusForbidden, "forbidden"},
		{"no stored credential", w.ownerKey, "anon", "", http.StatusUnprocessableEntity, forge.ErrorTypeCredentialMissing},
		{"no stored credential for upstream", w.ownerKey, "cp-anon", "?target=upstream", http.StatusUnprocessableEntity, forge.ErrorTypeCredentialMissing},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, route := range writeRoutes {
				status, body, sent := w.send(t, route.method, c.slug, route.path+c.query, c.credential, route.body)
				if status != c.status || errorOf(t, body).Type != c.errorType || sent != 0 {
					t.Errorf("%s %s%s%s = %d %s after %d forge requests, want %d %s after none",
						route.method, c.slug, route.path, c.query, status, body, sent, c.status, c.errorType)
				}
			}
			if n := w.forgeEvents(t); n != 0 {
				t.Errorf("the event log holds %d forge events, want none", n)
			}
		})
	}
}

func TestWriteRoute_AnswersTheFirstFailingCheck(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gh := new("https://github.com/acme/widgets.git")
	unknown := new("https://code.example.com/acme/widgets.git")
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "archived", Status: workspace.StatusArchived, GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "unknown", GitURL: unknown})
	w.addWorkspace(t, workspace.Workspace{Slug: "archived-unknown", Status: workspace.StatusArchived, GitURL: unknown})
	w.addWorkspace(t, workspace.Workspace{Slug: "invalid-api", GitURL: gh, Settings: settings(forge.SettingAPIURL, "http://ghe.example.com")})
	w.addWorkspace(t, workspace.Workspace{Slug: "anon", GitURL: gh, Settings: w.github()})
	for _, slug := range []string{"demo", "archived"} {
		w.putSecret(t, slug, workspace.SecretGitPAT, slug+"-token")
	}

	malformed, valid := `{"title":`, map[string]any{"title": "t"}
	oversized := map[string]any{"title": "t", "body": strings.Repeat("x", 65537)}
	cases := []struct {
		name, slug, path string
		body             any
		status           int
		errorType        string
		// contains is what the message must hold, when it is set.
		contains string
	}{
		{"an invalid target before a malformed body", "demo", "/issues?target=bogus", malformed, 400, "invalid_request", "target"},
		{"upstream on a standard workspace before a malformed body", "demo", "/issues?target=upstream", malformed, 400, "workspace_mode_mismatch", ""},
		{"target given twice", "demo", "/issues?target=origin&target=origin", valid, 400, "invalid_request", "target"},
		{"a malformed body before an unknown forge", "unknown", "/issues", malformed, 400, "invalid_request", ""},
		{"a blank title before an archived workspace", "archived", "/issues", map[string]any{"title": " \t"}, 400, "invalid_request", "title"},
		{"an archived workspace before an unknown forge", "archived-unknown", "/issues", valid, 409, "workspace_archived", ""},
		{"an unknown forge before a missing credential", "unknown", "/issues", valid, 422, forge.ErrorTypeUnknown, ""},
		{"an invalid API URL before a missing credential", "invalid-api", "/issues", valid, 422, forge.ErrorTypeAPIURLInvalid, ""},
		{"an unknown forge before an oversized text", "unknown", "/issues", oversized, 422, forge.ErrorTypeUnknown, ""},
		{"an oversized text before a missing credential", "anon", "/issues", oversized, 413, "payload_too_large", "body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, sent := w.send(t, http.MethodPost, c.slug, c.path, w.ownerKey, c.body)
			e := errorOf(t, body)
			if status != c.status || e.Type != c.errorType || !strings.Contains(e.Message, c.contains) || sent != 0 {
				t.Errorf("POST %s%s = %d %s after %d forge requests, want %d %s naming %q after none",
					c.slug, c.path, status, body, sent, c.status, c.errorType, c.contains)
			}
			if n := w.forgeEvents(t); n != 0 {
				t.Errorf("the event log holds %d forge events, want none", n)
			}
		})
	}
}

func TestWriteRoute_WritesToTheTargetsRepositoryWithItsCredential(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gh := new("https://github.com/acme/widgets.git")
	carry := func(slug string) workspace.Workspace {
		return workspace.Workspace{Slug: slug, Mode: workspace.ModeCarryPatch, GitURL: gh,
			UpstreamURL: new("https://github.com/upstream/project.git"), Settings: w.github(forge.SettingUpstreamAPIURL, w.fake.GitHubURL())}
	}
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "demo-fork", ParentSlug: new("demo")})
	w.addWorkspace(t, workspace.Workspace{Slug: "basic", GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, carry("cp"))
	w.addWorkspace(t, carry("cp-origin-only"))
	w.addWorkspace(t, workspace.Workspace{Slug: "cp-fork", ParentSlug: new("cp")})
	w.putSecret(t, "demo", workspace.SecretGitPAT, "demo-token")
	w.putSecret(t, "basic", workspace.SecretGitUsername, "ann")
	w.putSecret(t, "basic", workspace.SecretGitPassword, "basic-password")
	w.putSecret(t, "cp", workspace.SecretGitPAT, "origin-token")
	w.putSecret(t, "cp", "UPSTREAM_GIT_PAT", "upstream-token")
	w.putSecret(t, "cp-origin-only", workspace.SecretGitPAT, "origin-only-token")
	forkBound := w.pat(t, w.owner, "demo-fork", forge.ScopeWrite)

	cases := []struct {
		name, slug, query, credential string
		repo, token                   string
		// like is the slug whose answer to the same request this one
		// must equal, when it is set.
		like string
	}{
		{"a workspace's token", "demo", "", w.ownerKey, ghRepo, "demo-token", ""},
		{"a task fork, on its parent's repository with its parent's token", "demo-fork", "", w.ownerKey, ghRepo, "demo-token", "demo"},
		{"a task fork by a token bound to it", "demo-fork", "", forkBound, ghRepo, "demo-token", "demo"},
		{"no target", "cp", "", w.ownerKey, ghRepo, "origin-token", ""},
		{"target origin", "cp", "?target=origin", w.ownerKey, ghRepo, "origin-token", ""},
		{"target upstream, with the upstream token", "cp", "?target=upstream", w.ownerKey, ghUpstream, "upstream-token", ""},
		{"target upstream, with the origin token when no upstream one is stored", "cp-origin-only", "?target=upstream", w.ownerKey,
			ghUpstream, "origin-only-token", ""},
		{"target upstream through a task fork", "cp-fork", "?target=upstream", w.ownerKey, ghUpstream, "upstream-token", ""},
	}
	answers := map[string]string{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w.fake.SetNext(c.repo, 7)
			status, body, sent := w.send(t, http.MethodPost, c.slug, "/issues"+c.query, c.credential, map[string]any{"title": "t"})
			if status != http.StatusCreated || sent != 1 {
				t.Fatalf("POST %s/issues%s = %d %s after %d forge requests, want 201 after 1", c.slug, c.query, status, body, sent)
			}
			last := w.fake.Last(t)
			if last.Method != http.MethodPost || last.RawPath != c.repo+"/issues" || last.Header.Get("Authorization") != "Bearer "+c.token {
				t.Errorf("the forge got %s %s with Authorization %q, want POST %s/issues with the token %s",
					last.Method, last.RawPath, last.Header.Get("Authorization"), c.repo, c.token)
			}
			if c.like != "" && string(body) != answers[c.like] {
				t.Errorf("the answer is %s, want the answer for %s, %s", body, c.like, answers[c.like])
			}
			answers[c.slug] = string(body)
		})
	}

	t.Run("every write route sends the stored password as the token", func(t *testing.T) {
		for _, route := range writeRoutes {
			w.seedIssue(ghRepo)
			before := w.fake.Count()
			status, body, sent := w.send(t, route.method, "basic", route.path, w.ownerKey, route.body)
			if status >= 300 || sent == 0 {
				t.Errorf("%s basic%s = %d %s after %d forge requests, want success after at least 1", route.method, route.path, status, body, sent)
			}
			for _, r := range w.fake.Requests()[before:] {
				if got := r.Header.Get("Authorization"); got != "Bearer basic-password" {
					t.Errorf("%s basic%s: the forge got %s %s with Authorization %q, want the stored password",
						route.method, route.path, r.Method, r.RawPath, got)
				}
			}
		}
	})
}

// badBody is a body a write route refuses with 400 invalid_request, and the
// field its message must name: "" for a body that is not what the route
// takes at all.
type badBody struct {
	name  string
	body  any
	field string
}

// names is a list of n distinct names.
func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "n" + strconv.Itoa(i)
	}
	return out
}

// badBodies are, per write route, the bodies it refuses before anything is
// sent, and boundBodies the bodies at the bounds of its rules, which it
// sends.
var (
	badBodies = []struct {
		method, path string
		bodies       []badBody
	}{
		{http.MethodPost, "/issues", []badBody{
			{"malformed", `{`, ""}, {"an array", `[]`, ""}, {"a string", `"x"`, ""}, {"a number", `5`, ""}, {"none", nil, ""},
			{"no title", map[string]any{}, "title"},
			{"a null title", map[string]any{"title": nil}, "title"},
			{"a blank title", map[string]any{"title": "   "}, "title"},
			{"a title that is a number", map[string]any{"title": 5}, "title"},
			{"labels that are a string", map[string]any{"title": "t", "labels": "bug"}, "labels"},
			{"101 labels", map[string]any{"title": "t", "labels": names(101)}, "labels"},
			{"a blank label", map[string]any{"title": "t", "labels": []any{"a", ""}}, "labels"},
			{"a label that is a number", map[string]any{"title": "t", "labels": []any{"a", 5}}, "labels"},
			{"101 assignees", map[string]any{"title": "t", "assignees": names(101)}, "assignees"},
			{"a body that is a number", map[string]any{"title": "t", "body": 5}, "body"},
		}},
	}
	boundBodies = []struct {
		method, path string
		bodies       []any
	}{
		{http.MethodPost, "/issues", []any{map[string]any{"title": "t", "labels": names(100), "assignees": names(100)}}},
	}
)

func TestWriteRoute_RefusesABodyThatBreaksItsRules(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.putSecret(t, "demo", workspace.SecretGitPAT, "demo-token")
	w.seedIssue(ghRepo)

	for _, route := range badBodies {
		for _, b := range route.bodies {
			t.Run(route.method+" "+route.path+", "+b.name, func(t *testing.T) {
				status, body, sent := w.send(t, route.method, "demo", route.path, w.ownerKey, b.body)
				e := errorOf(t, body)
				if status != http.StatusBadRequest || e.Type != "invalid_request" || !strings.Contains(e.Message, b.field) || sent != 0 {
					t.Errorf("%s demo%s = %d %s after %d forge requests, want 400 invalid_request naming %q after none",
						route.method, route.path, status, body, sent, b.field)
				}
			})
		}
	}
	if n := w.forgeEvents(t); n != 0 {
		t.Errorf("the event log holds %d forge events, want none", n)
	}
	for _, route := range boundBodies {
		for i, b := range route.bodies {
			t.Run(route.method+" "+route.path+", a body at the bounds "+strconv.Itoa(i), func(t *testing.T) {
				status, body, sent := w.send(t, route.method, "demo", route.path, w.ownerKey, b)
				if status >= 300 || sent == 0 {
					t.Errorf("%s demo%s = %d %.300s after %d forge requests, want success", route.method, route.path, status, body, sent)
				}
			})
		}
	}
}

// decoratedBodies are, per write route, a body and the same body with null
// and unknown members added, which the route must send alike.
var decoratedBodies = []struct {
	method, path     string
	plain, decorated map[string]any
}{
	{http.MethodPost, "/issues", map[string]any{"title": "t"},
		map[string]any{"title": "t", "body": nil, "labels": nil, "assignees": nil, "extra": 1}},
}

func TestWriteRoute_TreatsNullAsAbsentAndIgnoresUnknownMembers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.putSecret(t, "demo", workspace.SecretGitPAT, "demo-token")
	for _, route := range decoratedBodies {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			var statuses [2]int
			var sent [2][]byte
			for i, b := range []map[string]any{route.plain, route.decorated} {
				w.seedIssue(ghRepo)
				statuses[i], _, _ = w.send(t, route.method, "demo", route.path, w.ownerKey, b)
				sent[i] = w.fake.Last(t).Body
			}
			if statuses[0] != statuses[1] || statuses[0] >= 300 || !sameJSON(sent[1], string(sent[0])) {
				t.Errorf("the plain body is answered %d and sent as %s, the decorated one %d and %s; want the same success",
					statuses[0], sent[0], statuses[1], sent[1])
			}
		})
	}
}

// measuredTexts are, per write route, the text the forge limits in length:
// its field, a body of the route holding text as that field, and the member
// the forge receives it in on GitHub and on GitLab.
var measuredTexts = []struct {
	method, path, field string
	body                func(text string) map[string]any
	github, gitlab      string
}{
	{http.MethodPost, "/issues", "body", func(s string) map[string]any { return map[string]any{"title": "t", "body": s} }, "body", "description"},
}

func TestWriteRoute_RefusesATextOverTheForgesLimit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "gl", GitURL: new("https://gitlab.com/group/sub/widgets.git"), Settings: w.gitlab()})
	w.putSecret(t, "demo", workspace.SecretGitPAT, "demo-token")
	w.putSecret(t, "gl", workspace.SecretGitPAT, "gl-token")
	w.seedIssue(ghRepo)
	w.seedIssue(glSubWidgets)

	type length struct {
		name, slug, text string
		// limit is the limit a text over it is refused by, and "" for a
		// text that is sent.
		limit string
	}
	lengths := []length{
		{"GitHub, 65,536 characters", "demo", strings.Repeat("x", 65536), ""},
		{"GitHub, 65,537 characters", "demo", strings.Repeat("x", 65537), "65536"},
		{"GitHub, 65,536 two-byte characters", "demo", strings.Repeat("é", 65536), ""},
		{"GitLab, 65,537 characters", "gl", strings.Repeat("x", 65537), ""},
	}
	type row struct {
		length
		// route is the index of the row's route in measuredTexts.
		route int
	}
	var rows []row
	for i := range measuredTexts {
		for _, l := range lengths {
			rows = append(rows, row{l, i})
		}
	}
	// GitLab's own limit is measured on the one route that creates an
	// issue: the text at the limit is in two-byte characters, so that it is
	// longer in bytes than the platform's default body limit.
	rows = append(rows,
		row{length{"GitLab, 1,000,000 two-byte characters", "gl", strings.Repeat("é", 1_000_000), ""}, 0},
		row{length{"GitLab, 1,000,001 characters", "gl", strings.Repeat("x", 1_000_001), "1000000"}, 0})

	for _, r := range rows {
		m := measuredTexts[r.route]
		t.Run(m.method+" "+m.path+" "+m.field+", "+r.name, func(t *testing.T) {
			events := w.forgeEvents(t)
			status, body, sent := w.send(t, m.method, r.slug, m.path, w.ownerKey, m.body(r.text))
			if r.limit != "" {
				e := errorOf(t, body)
				n := strconv.Itoa(utf8.RuneCountInString(r.text))
				if status != http.StatusRequestEntityTooLarge || e.Type != "payload_too_large" || sent != 0 ||
					!strings.Contains(e.Message, m.field) || !strings.Contains(e.Message, n) || !strings.Contains(e.Message, r.limit) {
					t.Errorf("%s %s%s = %d %.300s after %d forge requests, want 413 payload_too_large naming %s, %s and %s after none",
						m.method, r.slug, m.path, status, body, sent, m.field, n, r.limit)
				}
				if got := w.forgeEvents(t); got != events {
					t.Errorf("the event log holds %d forge events, want %d", got, events)
				}
				return
			}
			if status >= 300 || sent == 0 {
				t.Fatalf("%s %s%s = %d %.300s after %d forge requests, want success", m.method, r.slug, m.path, status, body, sent)
			}
			member := m.github
			if r.slug == "gl" {
				member = m.gitlab
			}
			var got map[string]any
			if err := json.Unmarshal(w.fake.Last(t).Body, &got); err != nil || got[member] != r.text {
				t.Errorf("the forge did not receive the text unchanged as %s (%v)", member, err)
			}
		})
	}
}
