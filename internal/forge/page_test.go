package forge_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The fake GitLab's project acme/widgets, as its path is sent.
const glWidgets = forgetest.GitLabPrefix + "/projects/acme%2Fwidgets"

// listRoute is a forge list route as the paging tests call it: its path
// below /api/v1/workspaces/<slug>, what puts n entries in the collection
// it reads on the fake GitHub's acme/widgets or the fake GitLab's, and the
// member that tells the items it answers apart.
type listRoute struct {
	path string
	seed func(w *world, kind forge.ForgeKind, n int)
	id   string
}

// listRoutes are the forge's list routes. A route that pages a forge
// collection is a row here.
var listRoutes = []listRoute{
	{"/issues", func(w *world, kind forge.ForgeKind, n int) {
		issues := make([]any, n)
		for i := range issues {
			if kind == forge.KindGitLab {
				issues[i] = gitlabIssue(i+1, "Issue", nil)
			} else {
				issues[i] = githubIssue(i+1, "Issue", nil)
			}
		}
		if kind == forge.KindGitLab {
			w.fake.SetList(glWidgets+"/issues", issues...)
		} else {
			w.fake.SetList(ghRepo+"/issues", issues...)
		}
	}, "number"},
}

func TestForgeList(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	widgets := new("https://github.com/acme/widgets.git")
	w.addWorkspace(t, workspace.Workspace{Slug: "gh", GitURL: widgets, Settings: w.github()})
	w.addWorkspace(t, workspace.Workspace{Slug: "gl", GitURL: new("https://gitlab.example.com/acme/widgets.git"), Settings: w.gitlab()})
	w.addWorkspace(t, workspace.Workspace{Slug: "carry", Mode: workspace.ModeCarryPatch, GitURL: widgets,
		UpstreamURL: new("https://github.com/up/proj.git"), Settings: w.github(forge.SettingUpstreamAPIURL, w.fake.GitHubURL())})
	slugs := map[forge.ForgeKind]string{forge.KindGitHub: "gh", forge.KindGitLab: "gl"}

	t.Run("pages by the forge's next link with the cursor's page size", func(t *testing.T) {
		for _, route := range listRoutes {
			for kind, slug := range slugs {
				name := slug + route.path
				asked := func(perPage, page string) {
					t.Helper()
					if q := w.lastQuery(t); q.Get("per_page") != perPage || q.Get("page") != page {
						t.Errorf("%s: the forge was asked for per_page=%s&page=%s, want per_page=%s&page=%s",
							name, q.Get("per_page"), q.Get("page"), perPage, page)
					}
				}
				route.seed(w, kind, 5)
				var pages []list
				for i, query := range []string{"limit=2", "cursor=%s", "limit=2&cursor=%s"} {
					if i > 0 {
						if pages[i-1].NextCursor == nil {
							t.Fatalf("%s: page %d has next_cursor null, want a cursor", name, i)
						}
						query = fmt.Sprintf(query, url.QueryEscape(*pages[i-1].NextCursor))
					}
					l, _ := w.list(t, slug, withQuery(route.path, query))
					asked("2", fmt.Sprint(i+1))
					pages = append(pages, l)
				}
				if p := pages[2]; len(pages[0].Items) != 2 || len(pages[1].Items) != 2 || len(p.Items) != 1 || p.NextCursor != nil {
					t.Errorf("%s: pages of %d, %d and %d items, the last with next_cursor %v, want 2, 2 and 1 and null",
						name, len(pages[0].Items), len(pages[1].Items), len(p.Items), p.NextCursor)
				}
				seen := map[string]bool{}
				for _, p := range pages {
					for _, item := range p.Items {
						seen[fmt.Sprint(item[route.id])] = true
					}
				}
				if len(seen) != 5 {
					t.Errorf("%s: the pages hold %d different entries, want the 5 of the forge", name, len(seen))
				}

				w.list(t, slug, route.path)
				asked("50", "1")

				route.seed(w, kind, 0)
				l, _ := w.list(t, slug, route.path)
				if len(l.Items) != 0 || l.NextCursor != nil {
					t.Errorf("%s of an empty collection = %v with next_cursor %v, want [] and null", name, l.Items, l.NextCursor)
				}
			}
		}
	})

	t.Run("refuses a limit out of bounds or a cursor of another query or page size", func(t *testing.T) {
		listRoutes[0].seed(w, forge.KindGitHub, 5)
		first, _ := w.list(t, "carry", "/issues?limit=2&state=open")
		if first.NextCursor == nil {
			t.Fatal("carry/issues?limit=2&state=open has next_cursor null")
		}
		c := url.QueryEscape(*first.NextCursor)
		if _, sent := w.list(t, "carry", "/issues?state=open&cursor="+c); sent != 1 {
			t.Fatalf("the cursor's own query sent %d forge requests, want 1", sent)
		}
		cases := []struct{ name, slug, path string }{
			{"limit 0", "carry", "/issues?limit=0"},
			{"limit 201", "carry", "/issues?limit=201"},
			{"limit x", "carry", "/issues?limit=x"},
			{"a cursor that does not decode", "carry", "/issues?cursor=not-a-cursor"},
			// The comments route stands in for another list route once
			// it exists.
			{"another route", "carry", "/forge-probe?state=open&cursor=" + c},
			{"another workspace", "gh", "/issues?state=open&cursor=" + c},
			{"another filter", "carry", "/issues?state=closed&cursor=" + c},
			{"another target", "carry", "/issues?state=open&target=upstream&cursor=" + c},
			{"another page size", "carry", "/issues?limit=3&state=open&cursor=" + c},
			{"a page size out of bounds", "carry", "/issues?state=open&cursor=" + withPosition(t, *first.NextCursor, `{"page":2,"size":0}`)},
			{"no page", "carry", "/issues?state=open&cursor=" + withPosition(t, *first.NextCursor, `{"size":2}`)},
		}
		for _, c := range cases {
			status, body, sent := w.get(t, c.slug, c.path, w.Token)
			if status != http.StatusBadRequest || errorOf(t, body).Type != "invalid_request" || sent != 0 {
				t.Errorf("%s: GET %s%s = %d %s after %d forge requests, want 400 invalid_request after none",
					c.name, c.slug, c.path, status, body, sent)
			}
		}
	})
}

// withPosition is the cursor c holding the position pos in place of its
// own, escaped for a query.
func withPosition(t *testing.T, c, pos string) string {
	t.Helper()
	doc, err := base64.RawURLEncoding.DecodeString(c)
	var members map[string]json.RawMessage
	if err != nil || json.Unmarshal(doc, &members) != nil {
		t.Fatalf("the cursor %q does not decode", c)
	}
	members["p"] = json.RawMessage(pos)
	doc, _ = json.Marshal(members)
	return url.QueryEscape(base64.RawURLEncoding.EncodeToString(doc))
}
