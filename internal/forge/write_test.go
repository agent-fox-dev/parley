package forge_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
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
	{http.MethodPatch, "/issues/7", map[string]any{"title": "n"}},
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
	w.addWorkspace(t, workspace.Workspace{Slug: "gl-anon", GitURL: new("https://gitlab.com/group/sub/widgets.git"), Settings: w.gitlab()})
	for _, slug := range []string{"demo", "archived"} {
		w.putSecret(t, slug, workspace.SecretGitPAT, slug+"-token")
	}

	malformed, valid := `{"title":`, map[string]any{"title": "t"}
	oversized := map[string]any{"title": "t", "body": strings.Repeat("x", 65537)}
	post, patch := http.MethodPost, http.MethodPatch
	cases := []struct {
		name, slug, method, path string
		body                     any
		status                   int
		errorType                string
		// contains is what the message must hold, when it is set.
		contains string
	}{
		{"an invalid target before a malformed body", "demo", post, "/issues?target=bogus", malformed, 400, "invalid_request", "target"},
		{"upstream on a standard workspace before a malformed body", "demo", post, "/issues?target=upstream", malformed, 400, "workspace_mode_mismatch", ""},
		{"upstream on a standard workspace before a bad number", "demo", patch, "/issues/07?target=upstream", valid, 400, "workspace_mode_mismatch", ""},
		{"target given twice", "demo", post, "/issues?target=origin&target=origin", valid, 400, "invalid_request", "target"},
		{"a bad number before an archived workspace", "archived", patch, "/issues/0", valid, 400, "invalid_request",
			"number must be a positive decimal integer"},
		{"a malformed body before an unknown forge", "unknown", post, "/issues", malformed, 400, "invalid_request", ""},
		{"a blank title before an archived workspace", "archived", post, "/issues", map[string]any{"title": " \t"}, 400, "invalid_request", "title"},
		{"an archived workspace before an unknown forge", "archived-unknown", post, "/issues", valid, 409, "workspace_archived", ""},
		{"an unknown forge before a missing credential", "unknown", post, "/issues", valid, 422, forge.ErrorTypeUnknown, ""},
		{"an invalid API URL before a missing credential", "invalid-api", post, "/issues", valid, 422, forge.ErrorTypeAPIURLInvalid, ""},
		{"an unknown forge before an oversized text", "unknown", post, "/issues", oversized, 422, forge.ErrorTypeUnknown, ""},
		{"GitLab assignees alone before a missing credential", "gl-anon", patch, "/issues/7", map[string]any{"assignees": []string{"ann"}},
			400, "invalid_request", "assignees"},
		{"an oversized text before a missing credential", "anon", post, "/issues", oversized, 413, "payload_too_large", "body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body, sent := w.send(t, c.method, c.slug, c.path, w.ownerKey, c.body)
			e := errorOf(t, body)
			if status != c.status || e.Type != c.errorType || !strings.Contains(e.Message, c.contains) || sent != 0 {
				t.Errorf("%s %s%s = %d %s after %d forge requests, want %d %s naming %q after none",
					c.method, c.slug, c.path, status, body, sent, c.status, c.errorType, c.contains)
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
			n := w.rec.count()
			status, body, sent := w.send(t, http.MethodPost, c.slug, "/issues"+c.query, c.credential, map[string]any{"title": "t"})
			if status != http.StatusCreated || sent != 1 {
				t.Fatalf("POST %s/issues%s = %d %s after %d forge requests, want 201 after 1", c.slug, c.query, status, body, sent)
			}
			last := w.fake.Last(t)
			if last.Method != http.MethodPost || last.RawPath != c.repo+"/issues" || last.Header.Get("Authorization") != "Bearer "+c.token {
				t.Errorf("the forge got %s %s with Authorization %q, want POST %s/issues with the token %s",
					last.Method, last.RawPath, last.Header.Get("Authorization"), c.repo, c.token)
			}
			target := string(forge.TargetOrigin)
			if strings.Contains(c.query, "upstream") {
				target = string(forge.TargetUpstream)
			}
			if events := w.rec.since(n); len(events) != 1 || events[0].meta(t)["target"] != target {
				t.Errorf("the write recorded %d events, the last %+v, want one with the target %s", len(events), events, target)
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
		{http.MethodPatch, "/issues/7", []badBody{
			{"an empty title", map[string]any{"title": ""}, "title"},
			{"a blank title", map[string]any{"title": " \t"}, "title"},
			{"a state that is neither open nor closed", map[string]any{"state": "merged"}, "state"},
			{"a state that is a number", map[string]any{"state": 5}, "state"},
			{"a blank label", map[string]any{"labels": []string{" "}}, "labels"},
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
	{http.MethodPatch, "/issues/7", map[string]any{"title": "t"},
		map[string]any{"title": "t", "body": nil, "state": nil, "labels": nil, "assignees": nil, "extra": 1}},
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
	{http.MethodPatch, "/issues/7", "body", func(s string) map[string]any { return map[string]any{"body": s} }, "body", "description"},
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

// The markers of what no write event may hold: a title, any other text, a
// label's description, the stored token and the forge's message.
const (
	titleMarker = "TITLE-MARKER"
	textMarker  = "TEXT-MARKER"
	descMarker  = "DESC-MARKER"
	tokenMarker = "TOKEN-MARKER"
	forgeMarker = "FORGE-MARKER"
)

// writeEvents are, per write route, a body it accepts, whose texts are the
// markers, and the event the route records for it, against issue 7 of the
// fake GitHub's acme/widgets: its type and resource type; on a success its
// resource id and the route's metadata beyond target and ok, the number
// the forge gives what the route creates being next (0 for nothing); on a
// failure its resource id and the route's metadata beyond target, ok and
// error_type.
var writeEvents = []struct {
	method, path  string
	body          any
	typ, resource string
	next          int64
	id            string
	meta          map[string]any
	failedID      string
	failedMeta    map[string]any
}{
	{http.MethodPost, "/issues", map[string]any{"title": titleMarker, "body": textMarker, "labels": []string{"bug"}},
		"parley.forge.issue.create", "issue", 7,
		"7", map[string]any{"number": 7, "url": "https://github.com/acme/widgets/issues/7"},
		"none", nil},
	{http.MethodPatch, "/issues/7", map[string]any{"state": "closed", "title": titleMarker},
		"parley.forge.issue.update", "issue", 0,
		"7", map[string]any{"number": 7, "url": "https://github.com/acme/widgets/issues/7", "fields": []string{"state", "title"}},
		"7", map[string]any{"number": 7, "fields": []string{"state", "title"}}},
}

// wantEvent checks that got is an event of the type typ about the resource
// id, recorded on the workspace slug by the owner, with exactly the
// metadata members of base and extra, and none of the markers.
func (w *world) wantEvent(t *testing.T, got recorded, slug, typ, resource, id string, base, extra map[string]any) {
	t.Helper()
	want := maps.Clone(base)
	maps.Copy(want, extra)
	wantMeta, _ := json.Marshal(want)
	action := typ[strings.LastIndexByte(typ, '.')+1:]
	if got.Type != typ || got.ResourceType != resource || got.ResourceID != id || got.Action != action {
		t.Errorf("the event is %s on %s %q, action %s, want %s on %s %q, action %s",
			got.Type, got.ResourceType, got.ResourceID, got.Action, typ, resource, id, action)
	}
	if got.Workspace == nil || *got.Workspace != slug || got.ActorType != "api_key" || got.ActorID == nil || *got.ActorID != w.owner {
		t.Errorf("the event is on %v by %s %v, want on %s by the owner's key", got.Workspace, got.ActorType, got.ActorID, slug)
	}
	if !sameJSON(got.Metadata, string(wantMeta)) {
		t.Errorf("the event's metadata is %s, want %s", got.Metadata, wantMeta)
	}
	line, err := json.Marshal(got.Record)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{titleMarker, textMarker, descMarker, tokenMarker, forgeMarker} {
		if strings.Contains(string(line), marker) {
			t.Errorf("the event %s holds %s", line, marker)
		}
	}
}

// eventWorld is a world with the GitHub workspace demo, whose stored token
// is tokenMarker, its task fork demo-fork, and the workspace closed, whose
// forge nothing listens for; adjust changes the forge component's
// dependencies, when it is not nil.
func eventWorld(t *testing.T, adjust func(w *world, d *forge.Deps)) *world {
	t.Helper()
	w := newWorldWith(t, hubtest.Config{}, adjust)
	gh := new("https://github.com/acme/widgets.git")
	w.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "demo-fork", ParentSlug: new("demo")})
	w.addWorkspace(t, workspace.Workspace{Slug: "closed", GitURL: gh, Settings: settings(forge.SettingAPIURL, closedURL(t)+"/github")})
	w.putSecret(t, "demo", workspace.SecretGitPAT, tokenMarker)
	w.putSecret(t, "closed", workspace.SecretGitPAT, tokenMarker)
	return w
}

func TestWriteRoute_RecordsOneEventBeforeItAnswers(t *testing.T) {
	t.Parallel()
	w := eventWorld(t, nil)
	for _, e := range writeEvents {
		for _, slug := range []string{"demo", "demo-fork"} {
			t.Run(e.method+" "+e.path+" on "+slug, func(t *testing.T) {
				w.seedIssue(ghRepo)
				if e.next != 0 {
					w.fake.SetNext(ghRepo, e.next)
				}
				n := w.rec.count()
				status, body, sent := w.send(t, e.method, slug, "/watched"+e.path, w.ownerKey, e.body)
				events := w.rec.since(n)
				if status >= 300 || sent == 0 || len(events) != 1 {
					t.Fatalf("%s %s%s = %d %.300s after %d forge requests and %d events, want success and one event",
						e.method, slug, e.path, status, body, sent, len(events))
				}
				if events[0].answered {
					t.Error("the event was recorded after the answer was written")
				}
				base := map[string]any{"target": "origin", "ok": true}
				if slug != "demo" {
					base["fork"] = slug
				}
				w.wantEvent(t, events[0], "demo", e.typ, e.resource, e.id, base, e.meta)
			})
		}
	}
}

// forgeFailure is a way a forge call fails, and the answer it gets: the
// answer scripted for the route's first forge request, or for the
// workspace closed none, and the hub's status, error type and Retry-After.
type forgeFailure struct {
	name, slug      string
	answer          forgetest.Answer
	status          int
	errorType, wait string
}

// failedWrite sends every route of writeEvents to the forge that fails as f
// does and checks the answer and the one event it records.
func (w *world) failedWrite(t *testing.T, f forgeFailure) {
	t.Helper()
	for _, e := range writeEvents {
		t.Run(e.method+" "+e.path+", "+f.name, func(t *testing.T) {
			w.seedIssue(ghRepo)
			if f.slug != "closed" {
				w.fake.Script(f.answer)
			}
			n := w.rec.count()
			status, header, body := w.Exchange(t, e.method, "/api/v1/workspaces/"+f.slug+e.path, w.ownerKey, e.body, nil)
			if er := errorOf(t, body); status != f.status || er.Type != f.errorType || header.Get("Retry-After") != f.wait {
				t.Errorf("%s %s%s = %d %.300s with Retry-After %q, want %d %s with %q",
					e.method, f.slug, e.path, status, body, header.Get("Retry-After"), f.status, f.errorType, f.wait)
			}
			if strings.Contains(string(body), tokenMarker) {
				t.Errorf("the answer %s holds the token", body)
			}
			events := w.rec.since(n)
			if len(events) != 1 {
				t.Fatalf("%d events were recorded, want one", len(events))
			}
			w.wantEvent(t, events[0], f.slug, e.typ, e.resource, e.failedID,
				map[string]any{"target": "origin", "ok": false, "error_type": f.errorType}, e.failedMeta)
		})
	}
}

func TestWriteRoute_AnswersAFailedForgeCallAsItsMappedError(t *testing.T) {
	t.Parallel()
	w := eventWorld(t, nil)
	refusal := `{"message":"` + forgeMarker + `"}`
	failures := []forgeFailure{
		{"404", "demo", forgetest.Answer{Status: 404, Body: refusal}, 404, forge.ErrorTypeNotFound, ""},
		{"401", "demo", forgetest.Answer{Status: 401, Body: refusal}, 502, forge.ErrorTypeUnauthenticated, ""},
		{"403", "demo", forgetest.Answer{Status: 403, Body: refusal}, 502, forge.ErrorTypeForbidden, ""},
		{"410", "demo", forgetest.Answer{Status: 410, Body: refusal}, 422, forge.ErrorTypeRejected, ""},
		{"422 echoing the token", "demo", forgetest.Answer{Status: 422,
			Body: `{"message":"` + forgeMarker + ` Authorization: Bearer ` + tokenMarker + `"}`}, 422, forge.ErrorTypeRejected, ""},
		{"500", "demo", forgetest.Answer{Status: 500, Body: refusal}, 502, forge.ErrorTypeError, ""},
		{"a 2xx body that is no JSON", "demo", forgetest.Answer{Status: 201, Body: "<html>" + forgeMarker}, 502, forge.ErrorTypeError, ""},
		{"429 with Retry-After 120", "demo", forgetest.Answer{Status: 429, Header: http.Header{"Retry-After": {"120"}}, Body: refusal},
			429, forge.ErrorTypeRateLimited, "120"},
		{"a closed listener", "closed", forgetest.Answer{}, 502, forge.ErrorTypeUnreachable, ""},
	}
	for _, f := range failures {
		w.failedWrite(t, f)
	}
	t.Run("the echoed token is redacted", func(t *testing.T) {
		w.fake.Script(failures[4].answer)
		e := writeEvents[0]
		_, body, _ := w.send(t, e.method, "demo", e.path, w.ownerKey, e.body)
		if strings.Contains(string(body), tokenMarker) || !strings.Contains(string(body), "[redacted]") {
			t.Errorf("the answer %s holds the token or no [redacted]", body)
		}
	})
}

func TestWriteRoute_AnswersAForgeThatDoesNotAnswerInTimeAsATimeout(t *testing.T) {
	t.Parallel()
	w := eventWorld(t, func(_ *world, d *forge.Deps) { d.Client.Deadline = 200 * time.Millisecond })
	w.failedWrite(t, forgeFailure{"no answer before the deadline", "demo", forgetest.Answer{Hold: true},
		http.StatusGatewayTimeout, forge.ErrorTypeTimeout, ""})
}

func TestWriteRoute_RunsToItsEndWhenTheCallerLeaves(t *testing.T) {
	t.Parallel()
	w := eventWorld(t, nil)
	w.fake.SetNext(ghRepo, 7)
	release := make(chan struct{})
	w.fake.Script(forgetest.Answer{Release: release, Through: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL("/api/v1/workspaces/demo/issues"), strings.NewReader(`{"title":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+w.ownerKey)
	req.Header.Set("Content-Type", "application/json")
	left := make(chan error, 1)
	go func() {
		resp, err := (&http.Client{}).Do(req)
		if err == nil {
			resp.Body.Close()
		}
		left <- err
	}()
	waitUntil(t, "the forge to receive the create", func() bool { return w.fake.Count() == 1 })
	cancel()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller did not leave before the answer: %v", err)
	}
	close(release)

	waitUntil(t, "the write's event", func() bool { return w.rec.count() == 1 })
	if _, ok := w.fake.Issue(ghRepo, 7); !ok {
		t.Error("the forge does not hold the issue")
	}
	got := w.rec.since(0)[0]
	if m := got.meta(t); got.Type != "parley.forge.issue.create" || m["ok"] != true || m["number"] != float64(7) {
		t.Errorf("the event is %s with %s, want parley.forge.issue.create, ok and number 7", got.Type, got.Metadata)
	}
	// The close route's row, a caller who leaves once the comment was
	// posted, comes with that route.
}

func TestWriteRoute_AnswersInternalWhenItsEventCannotBeRecorded(t *testing.T) {
	t.Parallel()
	w := eventWorld(t, nil)
	w.rec.failWith(errors.New("the event log cannot be written"))
	w.fake.SetNext(ghRepo, 7)
	status, body, sent := w.send(t, http.MethodPost, "demo", "/issues", w.ownerKey, map[string]any{"title": "t"})
	if e := errorOf(t, body); status != http.StatusInternalServerError || e.Type != "internal" {
		t.Errorf("POST demo/issues = %d %s, want 500 internal", status, body)
	}
	if strings.Contains(string(body), tokenMarker) || strings.Contains(string(body), "acme/widgets") {
		t.Errorf("the answer %s holds the token or the forge's answer", body)
	}
	if _, ok := w.fake.Issue(ghRepo, 7); !ok || sent != 1 || w.fake.Last(t).Method != http.MethodPost {
		t.Errorf("the forge holds the issue: %v, after %d requests, the last %s; want the issue kept after the one create",
			ok, sent, w.fake.Last(t).Method)
	}
}
