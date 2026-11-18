package forge_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// readRoute is a forge read route as the route tests call it: its path
// below /api/v1/workspaces/<slug>, and the answers the fake forge gives the
// requests one call of it sends.
type readRoute struct {
	path  string
	forge []forgetest.Answer
}

// readRoutes are the forge read routes every caller with read access is
// served on.
var readRoutes = []readRoute{
	{"/forge", nil},
	{"/forge/repository", []forgetest.Answer{{}}},
	{"/issues", []forgetest.Answer{{Body: "[]"}}},
	{"/issues/1", []forgetest.Answer{{}, {Body: "[]"}}},
	{"/issues/1/comments", []forgetest.Answer{{Body: "[]"}}},
	{"/forge-probe", []forgetest.Answer{{}}},
	{"/forge-probe/1", []forgetest.Answer{{}}},
}

// numberRoutes are the forge routes that take :number, with %s where it
// goes.
var numberRoutes = []string{
	"/issues/%s",
	"/issues/%s/comments",
	"/forge-probe/%s",
}

// numberMessage is the refusal of a :number that is not a positive decimal
// integer.
const numberMessage = "number must be a positive decimal integer"

// The forge settings that point a workspace at the fake's GitHub.
func (w *world) github(pairs ...string) map[string]json.RawMessage {
	return settings(append([]string{forge.SettingAPIURL, w.fake.GitHubURL()}, pairs...)...)
}

func TestForgeRoute_ServesEveryCallerWithReadAccess(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "w", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	callers := []struct {
		name, credential string
	}{
		{"the admin token", w.Token},
		{"the owner's API key", w.ownerKey},
		{"the owner's PAT with forge:read", w.pat(t, w.owner, "", forge.ScopeRead)},
		{"the owner's PAT with only forge:write", w.pat(t, w.owner, "", forge.ScopeWrite)},
		{"an org member's PAT with forge:read", w.pat(t, w.member, "", forge.ScopeRead)},
	}
	for _, c := range callers {
		for _, route := range readRoutes {
			w.fake.Script(route.forge...)
			if status, body, _ := w.get(t, "w", route.path, c.credential); status != http.StatusOK {
				t.Errorf("%s on %s = %d %s, want 200", c.name, route.path, status, body)
			}
		}
	}
}

func TestForgeRoute_RefusesWithoutScopeOrAccessAsForAnUnknownSlug(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "w", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	cases := []struct {
		name, credential string
		status           int
		errorType        string
	}{
		{"a PAT without a forge scope", w.pat(t, w.owner, "", "workspaces:read"), http.StatusForbidden, "insufficient_scope"},
		{"a caller without access", w.outKey, http.StatusNotFound, "not_found"},
	}
	for _, c := range cases {
		for _, route := range readRoutes {
			status, body, sent := w.get(t, "w", route.path, c.credential)
			unknownStatus, unknownBody, unknownSent := w.get(t, "no-such-slug", route.path, c.credential)
			if status != c.status || errorOf(t, body).Type != c.errorType {
				t.Errorf("%s on %s = %d %s, want %d %s", c.name, route.path, status, body, c.status, c.errorType)
			}
			if unknownStatus != status || string(unknownBody) != string(body) {
				t.Errorf("%s on %s of an unknown slug = %d %s, want the answer for w, %d %s",
					c.name, route.path, unknownStatus, unknownBody, status, body)
			}
			if sent+unknownSent != 0 {
				t.Errorf("%s on %s: the forge received %d requests, want none", c.name, route.path, sent+unknownSent)
			}
		}
	}
}

// badAPIURLs are stored forge API URL settings that the check at use
// refuses.
var badAPIURLs = []string{
	"http://ghe.example.com", "ghe.example.com/api", "https://", "https://u:p@ghe.example.com",
	"https://ghe.example.com/api?x=1", "https://ghe.example.com/api#f",
}

func TestForgeRoute_AnswersTheFirstFailingCheckWithoutCallingTheForge(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gh := new("https://github.com/acme/widgets.git")
	w.addWorkspace(t, workspace.Workspace{Slug: "w", GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "carry-no-upstream", Mode: workspace.ModeCarryPatch, GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "one-segment", GitURL: new("https://github.com/widgets")})
	w.addWorkspace(t, workspace.Workspace{Slug: "unknown", GitURL: new("https://code.example.com/acme/widgets")})
	w.addWorkspace(t, workspace.Workspace{Slug: "upstream-unknown", Mode: workspace.ModeCarryPatch, GitURL: gh,
		UpstreamURL: new("https://code.example.com/up/proj"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "invalid", GitURL: gh, Settings: settings(forge.SettingAPIURL, "http://ghe.example.com")})
	w.addWorkspace(t, workspace.Workspace{Slug: "invalid-no-repo", GitURL: new("https://github.com/widgets"),
		Settings: settings(forge.SettingAPIURL, "http://ghe.example.com")})
	w.addWorkspace(t, workspace.Workspace{Slug: brokenSlug, GitURL: gh, Settings: w.github()})

	type refusal struct {
		name, slug, path string
		status           int
		errorType        string
		// message is the whole message wanted, when it is set; contains
		// what it must hold and lacks what it must not.
		message, contains, lacks string
	}
	cases := []refusal{
		{name: "an invalid target first", slug: "w", path: "/issues/abc?target=bad", status: 400, errorType: "invalid_request", lacks: numberMessage},
		{name: "a mode mismatch before the number", slug: "w", path: "/issues/abc?target=upstream", status: 400, errorType: "workspace_mode_mismatch"},
		{name: "the number before an unknown forge", slug: "unknown", path: "/issues/abc", status: 400, errorType: "invalid_request", message: numberMessage},
		{name: "a query parameter before an unknown forge", slug: "unknown", path: "/issues?state=bogus", status: 400, errorType: "invalid_request"},
		{name: "a query parameter before an invalid API URL", slug: "invalid", path: "/issues?limit=0", status: 400, errorType: "invalid_request"},
		{name: "an invalid API URL before the repository", slug: "invalid-no-repo", path: "/issues/1", status: 422, errorType: forge.ErrorTypeAPIURLInvalid},
		{name: "a remote naming no repository", slug: "one-segment", path: "/forge/repository", status: 422, errorType: forge.ErrorTypeUnknown},
		{name: "an unknown origin forge names its setting", slug: "unknown", path: "/forge/repository", status: 422, errorType: forge.ErrorTypeUnknown,
			contains: forge.SettingAPIURL, lacks: forge.SettingUpstreamAPIURL},
		{name: "an unknown upstream forge names its setting", slug: "upstream-unknown", path: "/forge/repository?target=upstream", status: 422,
			errorType: forge.ErrorTypeUnknown, contains: forge.SettingUpstreamAPIURL},
		{name: "the forge's resolution before the route's check", slug: "one-segment", path: "/forge-probe?refuse", status: 422, errorType: forge.ErrorTypeUnknown},
		{name: "the route's check", slug: "w", path: "/forge-probe?refuse", status: 409, errorType: "probe_refused"},
		{name: "unreadable secrets", slug: brokenSlug, path: "/issues", status: 500, errorType: "internal"},
		{name: "upstream on a standard workspace", slug: "w", path: "/issues?target=upstream", status: 400, errorType: "workspace_mode_mismatch"},
		{name: "upstream on a carry-patch workspace without upstream_url", slug: "carry-no-upstream", path: "/issues?target=upstream",
			status: 400, errorType: "workspace_mode_mismatch"},
	}
	for _, query := range []string{"target=fork", "target=", "target=Origin", "target=origin&target=origin", "target=origin&target=upstream"} {
		cases = append(cases, refusal{name: "target " + query, slug: "w", path: "/issues?" + query, status: 400, errorType: "invalid_request"})
	}
	for _, route := range numberRoutes {
		for _, n := range []string{"0", "01", "+1", "-1", "1.0", "1e3", "abc", "%20", "2147483648", "99999999999999999999"} {
			cases = append(cases, refusal{name: "number " + n, slug: "w", path: fmt.Sprintf(route, n), status: 400,
				errorType: "invalid_request", message: numberMessage})
		}
	}
	for i, setting := range badAPIURLs {
		origin, carry := fmt.Sprintf("bad-api-%d", i), fmt.Sprintf("bad-upstream-api-%d", i)
		w.addWorkspace(t, workspace.Workspace{Slug: origin, GitURL: new("https://github.com/widgets"), Settings: settings(forge.SettingAPIURL, setting)})
		w.addWorkspace(t, workspace.Workspace{Slug: carry, Mode: workspace.ModeCarryPatch, GitURL: gh,
			UpstreamURL: new("https://github.com/up/proj.git"), Settings: w.github(forge.SettingUpstreamAPIURL, setting)})
		cases = append(cases,
			refusal{name: "API URL " + setting, slug: origin, path: "/forge/repository", status: 422, errorType: forge.ErrorTypeAPIURLInvalid},
			refusal{name: "upstream API URL " + setting, slug: carry, path: "/forge/repository?target=upstream", status: 422, errorType: forge.ErrorTypeAPIURLInvalid})
	}
	for _, c := range cases {
		status, body, sent := w.get(t, c.slug, c.path, w.ownerKey)
		e := errorOf(t, body)
		switch {
		case status != c.status || e.Type != c.errorType:
			t.Errorf("%s: %s%s = %d %s, want %d %s", c.name, c.slug, c.path, status, body, c.status, c.errorType)
		case c.message != "" && e.Message != c.message,
			c.contains != "" && !strings.Contains(e.Message, c.contains),
			c.lacks != "" && strings.Contains(e.Message, c.lacks):
			t.Errorf("%s: %s%s message %q, want %q, holding %q and not %q", c.name, c.slug, c.path, e.Message, c.message, c.contains, c.lacks)
		}
		if sent != 0 {
			t.Errorf("%s: %s%s: the forge received %d requests, want none", c.name, c.slug, c.path, sent)
		}
	}
}

func TestForgeRoute_ServesEveryTargetAndWorkspaceStateItAccepts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gh := new("https://github.com/acme/widgets.git")
	w.addWorkspace(t, workspace.Workspace{Slug: "w", GitURL: gh, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "carry", Mode: workspace.ModeCarryPatch, GitURL: gh,
		UpstreamURL: new("https://github.com/up/proj.git"), Settings: w.github(forge.SettingUpstreamAPIURL, w.fake.GitHubURL())})
	w.addWorkspace(t, workspace.Workspace{Slug: "bad-upstream", Mode: workspace.ModeCarryPatch, GitURL: gh,
		UpstreamURL: new("https://github.com/up/proj.git"), Settings: w.github(forge.SettingUpstreamAPIURL, badAPIURLs[0])})
	states := []struct {
		slug   string
		status workspace.Status
		marker workspace.Marker
	}{
		{"pending", workspace.StatusPending, workspace.MarkerNone},
		{"failed", workspace.StatusFailed, workspace.MarkerNone},
		{"archived", workspace.StatusArchived, workspace.MarkerNone},
		{"busy", workspace.StatusReady, workspace.MarkerReclone},
	}
	for _, s := range states {
		w.addWorkspace(t, workspace.Workspace{Slug: s.slug, Status: s.status, Marker: s.marker, GitURL: gh, Settings: w.github()})
	}
	// answers are what the forge answers the requests of one read, the
	// first of which is for forgePath.
	type read struct {
		name, slug, path, forgePath string
		answers                     []forgetest.Answer
	}
	list, object := []forgetest.Answer{{Body: "[]"}}, []forgetest.Answer{{}}
	thread := []forgetest.Answer{{Body: `{"number":1}`}, {Body: "[]"}}
	cases := []read{
		{"no target", "w", "/issues", "/github/repos/acme/widgets/issues", list},
		{"target origin", "w", "/issues?target=origin", "/github/repos/acme/widgets/issues", list},
		{"target upstream of a carry-patch workspace", "carry", "/issues?target=upstream", "/github/repos/up/proj/issues", list},
		{"origin beside an invalid upstream API URL", "bad-upstream", "/forge/repository", "/github/repos/acme/widgets", object},
	}
	for _, s := range states {
		cases = append(cases, read{s.slug, s.slug, "/issues/1", "/github/repos/acme/widgets/issues/1", thread})
	}
	for _, c := range cases {
		w.fake.Script(c.answers...)
		status, body, sent := w.get(t, c.slug, c.path, w.ownerKey)
		var got struct {
			Issue *struct {
				Number int `json:"number"`
			} `json:"issue"`
		}
		if status != http.StatusOK || c.path == "/issues/1" && (json.Unmarshal(body, &got) != nil || got.Issue == nil || got.Issue.Number != 1) {
			t.Errorf("%s: %s%s = %d %s, want 200 with the forge's answer", c.name, c.slug, c.path, status, body)
			continue
		}
		requests := w.fake.Requests()
		if first := requests[len(requests)-max(sent, 1)].Path; sent != len(c.answers) || first != c.forgePath {
			t.Errorf("%s: the forge received %d requests, the first for %s, want %d, the first for %s",
				c.name, sent, first, len(c.answers), c.forgePath)
		}
	}
}

func TestForgeRoute_ActsOnAForksParent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.addWorkspace(t, workspace.Workspace{Slug: "p", Mode: workspace.ModeCarryPatch, GitURL: new("https://github.com/acme/widgets.git"),
		UpstreamURL: new("https://github.com/up/proj.git"), Settings: w.github(forge.SettingUpstreamAPIURL, w.fake.GitHubURL())})
	w.putSecret(t, "p", workspace.SecretGitPAT, "parent-token")
	w.addWorkspace(t, workspace.Workspace{Slug: "f", ParentSlug: new("p")})
	w.addWorkspace(t, workspace.Workspace{Slug: deniedParent, GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "fd", ParentSlug: new(deniedParent)})

	t.Run("reads the parent's repository with the parent's credential", func(t *testing.T) {
		for target, path := range map[string]string{"": "/github/repos/acme/widgets/issues/1", "?target=upstream": "/github/repos/up/proj/issues/1"} {
			w.fake.Script(forgetest.Answer{}, forgetest.Answer{Body: "[]"})
			status, body, sent := w.get(t, "f", "/issues/1"+target, w.ownerKey)
			if status != http.StatusOK || sent != 2 {
				t.Fatalf("GET f/issues/1%s = %d %s after %d forge requests, want 200 after 2", target, status, body, sent)
			}
			requests := w.fake.Requests()
			for i, r := range requests[len(requests)-2:] {
				if want := []string{path, path + "/comments"}[i]; r.Path != want || r.Header.Get("Authorization") != "Bearer parent-token" {
					t.Errorf("target %q: the forge got %s with Authorization %q, want %s with the parent's token",
						target, r.Path, r.Header.Get("Authorization"), want)
				}
			}
		}
	})

	t.Run("decides on the parent by the user behind a token bound to the fork", func(t *testing.T) {
		bound := w.pat(t, w.owner, "f", forge.ScopeRead)
		w.fake.Script(forgetest.Answer{}, forgetest.Answer{Body: "[]"})
		if status, body, _ := w.get(t, "f", "/issues/1", bound); status != http.StatusOK {
			t.Fatalf("GET f/issues/1 with a token bound to f = %d %s, want 200", status, body)
		}
		qs := w.decider.questions()
		if len(qs) == 0 {
			t.Fatal("the access decider was asked nothing")
		}
		if q := qs[len(qs)-1]; q.slug != "p" || q.need != access.NeedRead || q.caller.UserID != w.owner || q.caller.Admin {
			t.Errorf("the access decider was asked %+v, want the owner's read on p", q)
		}
	})

	t.Run("refuses a caller without access to the parent before the target", func(t *testing.T) {
		status, body, sent := w.get(t, "fd", "/issues?target=bad", w.ownerKey)
		if status != http.StatusForbidden || errorOf(t, body).Type != "forbidden" || sent != 0 {
			t.Errorf("GET fd/issues?target=bad = %d %s with %d forge requests, want 403 forbidden and none", status, body, sent)
		}
	})
}

func TestParseNumber_AcceptsOnlyAPositiveDecimalInt32(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]int64{"1": 1, "42": 42, "2147483647": 2147483647} {
		if n, err := forge.ParseNumber(s); n != want || err != nil {
			t.Errorf("ParseNumber(%q) = %d, %v, want %d", s, n, err, want)
		}
	}
	for _, s := range []string{"", "0", "01", "+1", "-1", "1.0", "1e3", "abc", " ", "2147483648", "99999999999999999999", "١"} {
		if n, err := forge.ParseNumber(s); err == nil || err.Error() != numberMessage {
			t.Errorf("ParseNumber(%q) = %d, %v, want the error %q", s, n, err, numberMessage)
		}
	}
}
