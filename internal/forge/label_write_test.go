package forge_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
)

// labelForge is a forge the label tests run on: its workspace in
// forgesWorld, its repository on the fake forge, the repository's path
// decoded, as a scripted answer names it, and the prefix of a colour as the
// forge holds it.
type labelForge struct {
	name, slug, repo, decoded, hash string
}

var labelForges = []labelForge{
	{"GitHub", "demo", ghRepo, ghRepo, ""},
	{"GitLab", "gl", glSubWidgets, forgetest.GitLabPrefix + "/projects/group/sub/widgets", "#"},
}

func TestAddLabels_AddsTheNamesAndKeepsTheIssuesOwn(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)
	for _, f := range []struct{ name, slug, repo, method, path, sent string }{
		{"GitHub", "demo", ghRepo, http.MethodPost, ghRepo + "/issues/7/labels", `{"labels":["triage","type/bug"]}`},
		{"GitLab", "gl", glSubWidgets, http.MethodPut, glSubWidgets + "/issues/7", `{"add_labels":"triage,type/bug"}`},
	} {
		t.Run(f.name, func(t *testing.T) {
			w.fake.AddIssue(f.repo, forgetest.Issue{Number: 7, Title: "Crash", State: "open", Labels: []string{"old"}})
			status, body, sent := w.send(t, http.MethodPost, f.slug, "/issues/7/labels", w.ownerKey, []string{"triage", "type/bug"})
			if status != http.StatusNoContent || len(body) != 0 || sent != 1 {
				t.Fatalf("POST %s/issues/7/labels = %d %.300s after %d forge requests, want 204 with no body after 1", f.slug, status, body, sent)
			}
			if last := w.fake.Last(t); last.Method != f.method || last.RawPath != f.path || !sameJSON(last.Body, f.sent) {
				t.Errorf("the forge got %s %s %s, want %s %s %s", last.Method, last.RawPath, last.Body, f.method, f.path, f.sent)
			}
			if i, _ := w.fake.Issue(f.repo, 7); !slices.Equal(i.Labels, []string{"old", "triage", "type/bug"}) {
				t.Errorf("issue 7 carries %q, want old, triage and type/bug", i.Labels)
			}
		})
	}
}

func TestRemoveLabel(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)

	t.Run("removes the label the last segment names, carried or not", func(t *testing.T) {
		for _, s := range []struct {
			segment, name string
			carried       bool
		}{
			{"old", "old", true}, {"type%2Fbug", "type/bug", true}, {"absent", "absent", false}, {"%252F", "%2F", false},
		} {
			for _, f := range labelForges {
				t.Run(f.name+", "+s.segment, func(t *testing.T) {
					w.fake.AddIssue(f.repo, forgetest.Issue{Number: 7, Title: "Crash", State: "open", Labels: []string{"old", "type/bug"}})
					status, body, sent := w.send(t, http.MethodDelete, f.slug, "/issues/7/labels/"+s.segment, w.ownerKey, nil)
					// GitHub answers 404 for a label the issue does not carry,
					// and the hub then reads the issue.
					want := 1
					if f.slug == "demo" && !s.carried {
						want = 2
					}
					if status != http.StatusNoContent || len(body) != 0 || sent != want {
						t.Fatalf("DELETE %s/issues/7/labels/%s = %d %.300s after %d forge requests, want 204 with no body after %d",
							f.slug, s.segment, status, body, sent, want)
					}
					requests := w.fake.Requests()[w.fake.Count()-sent:]
					if f.slug == "demo" {
						if r := requests[0]; r.Method != http.MethodDelete || r.RawPath != ghRepo+"/issues/7/labels/"+url.PathEscape(s.name) {
							t.Errorf("the forge got %s %s, want DELETE of the label %q", r.Method, r.RawPath, s.name)
						}
						if r := requests[len(requests)-1]; !s.carried && (r.Method != http.MethodGet || r.RawPath != ghRepo+"/issues/7") {
							t.Errorf("the forge got %s %s after the delete, want the read of issue 7", r.Method, r.RawPath)
						}
					} else {
						sentBody, _ := json.Marshal(map[string]string{"remove_labels": s.name})
						if r := requests[0]; r.Method != http.MethodPut || r.RawPath != glSubWidgets+"/issues/7" || !sameJSON(r.Body, string(sentBody)) {
							t.Errorf("the forge got %s %s %s, want PUT %s/issues/7 %s", r.Method, r.RawPath, r.Body, glSubWidgets, sentBody)
						}
					}
					kept := slices.DeleteFunc([]string{"old", "type/bug"}, func(l string) bool { return l == s.name })
					if i, _ := w.fake.Issue(f.repo, 7); !slices.Equal(i.Labels, kept) {
						t.Errorf("issue 7 carries %q, want %q", i.Labels, kept)
					}
				})
			}
		}
	})

	t.Run("sends a name of dots as a segment that names the label, not the issue", func(t *testing.T) {
		w.fake.AddIssue(ghRepo, forgetest.Issue{Number: 7, Title: "Crash", State: "open", Labels: []string{".."}})
		before := w.fake.Count()
		status, body, _ := w.send(t, http.MethodDelete, "demo", "/issues/7/labels/%2E%2E", w.ownerKey, nil)
		if r := w.fake.Requests()[before]; status != http.StatusNoContent || r.Method != http.MethodDelete || r.RawPath != ghRepo+"/issues/7/labels/%2E%2E" {
			t.Errorf("DELETE demo/issues/7/labels/%%2E%%2E = %d %.300s, the forge got %s %s, want 204 and DELETE %s/issues/7/labels/%%2E%%2E",
				status, body, r.Method, r.RawPath, ghRepo)
		}
	})

	t.Run("answers not found for an issue the forge does not hold", func(t *testing.T) {
		for _, f := range labelForges {
			t.Run(f.name, func(t *testing.T) {
				n := w.rec.count()
				status, body, _ := w.send(t, http.MethodDelete, f.slug, "/issues/999/labels/old", w.ownerKey, nil)
				if e := errorOf(t, body); status != http.StatusNotFound || e.Type != forge.ErrorTypeNotFound {
					t.Errorf("DELETE %s/issues/999/labels/old = %d %s, want 404 %s", f.slug, status, body, forge.ErrorTypeNotFound)
				}
				events := w.rec.since(n)
				if len(events) != 1 {
					t.Fatalf("%d events were recorded, want one", len(events))
				}
				if m := events[0].meta(t); events[0].Type != "parley.forge.label.remove" || m["ok"] != false || m["error_type"] != forge.ErrorTypeNotFound {
					t.Errorf("the event is %s with %s, want parley.forge.label.remove, not ok, %s", events[0].Type, events[0].Metadata, forge.ErrorTypeNotFound)
				}
			})
		}
	})

	t.Run("refuses a name that is blank", func(t *testing.T) {
		for _, segment := range []string{"%20", "%09%20"} {
			t.Run(segment, func(t *testing.T) {
				n := w.rec.count()
				status, body, sent := w.send(t, http.MethodDelete, "demo", "/issues/7/labels/"+segment, w.ownerKey, nil)
				if e := errorOf(t, body); status != http.StatusBadRequest || e.Type != "invalid_request" || sent != 0 || w.rec.count() != n {
					t.Errorf("DELETE demo/issues/7/labels/%s = %d %s after %d forge requests and %d events, want 400 invalid_request after none",
						segment, status, body, sent, w.rec.count()-n)
				}
			})
		}
	})
}

// ensure sends a label ensure of body to the workspace slug and returns the
// answer, how many requests the forge received meanwhile and the events it
// recorded.
func (w *world) ensure(t *testing.T, slug string, body any) (status int, answer []byte, sent int, events []recorded) {
	t.Helper()
	n := w.rec.count()
	status, answer, sent = w.send(t, http.MethodPost, slug, "/labels", w.ownerKey, body)
	return status, answer, sent, w.rec.since(n)
}

// wantEnsured checks that events is one label ensure event of the label
// name, ok, created or not.
func wantEnsured(t *testing.T, events []recorded, name string, created bool) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("%d events were recorded, want one", len(events))
	}
	if m := events[0].meta(t); events[0].Type != "parley.forge.label.create" || m["ok"] != true || m["label"] != name || m["created"] != created {
		t.Errorf("the event is %s with %s, want parley.forge.label.create, ok, label %s, created %v", events[0].Type, events[0].Metadata, name, created)
	}
}

func TestEnsureLabel(t *testing.T) {
	t.Parallel()
	w := forgesWorld(t)

	t.Run("answers a label the forge holds unchanged", func(t *testing.T) {
		for _, f := range labelForges {
			t.Run(f.name, func(t *testing.T) {
				held := forgetest.Label{Name: "type/bug", Color: f.hash + "d73a4a", Description: "Something is broken"}
				w.fake.SetLabels(f.repo, held)
				status, body, sent, events := w.ensure(t, f.slug, map[string]any{"name": "type/bug", "color": "#00FF00", "description": "other"})
				if want := `{"name":"type/bug","color":"d73a4a","description":"Something is broken"}`; status != http.StatusOK || !sameJSON(body, want) {
					t.Errorf("POST %s/labels = %d %s, want 200 %s", f.slug, status, body, want)
				}
				if r := w.fake.Last(t); sent != 1 || r.Method != http.MethodGet || r.RawPath != f.repo+"/labels/type%2Fbug" {
					t.Errorf("the forge got %d requests, the last %s %s, want only GET %s/labels/type%%2Fbug", sent, r.Method, r.RawPath, f.repo)
				}
				if got := w.fake.Labels(f.repo); !slices.Equal(got, []forgetest.Label{held}) {
					t.Errorf("the forge holds the labels %+v, want only %+v", got, held)
				}
				wantEnsured(t, events, "type/bug", false)
			})
		}
	})

	t.Run("creates a label the forge does not hold", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			body  map[string]any
			color string
		}{
			{"no colour", map[string]any{"name": "new"}, "ededed"},
			{"an upper-case colour with #", map[string]any{"name": "new", "color": "#A1B2C3", "description": "d"}, "a1b2c3"},
			{"a colour without #", map[string]any{"name": "new", "color": "00ff00"}, "00ff00"},
		} {
			for _, f := range labelForges {
				t.Run(f.name+", "+c.name, func(t *testing.T) {
					w.fake.SetLabels(f.repo)
					status, body, sent, events := w.ensure(t, f.slug, c.body)
					var got struct{ Name, Color string }
					if status != http.StatusCreated || json.Unmarshal(body, &got) != nil || got.Name != "new" || got.Color != c.color {
						t.Errorf("POST %s/labels = %d %s, want 201 with the label new, colour %s", f.slug, status, body, c.color)
					}
					var created map[string]any
					r := w.fake.Last(t)
					if sent != 2 || r.Method != http.MethodPost || r.RawPath != f.repo+"/labels" ||
						json.Unmarshal(r.Body, &created) != nil || created["color"] != f.hash+c.color {
						t.Errorf("the forge got %d requests, the last %s %s %s, want the lookup, then the create with the colour %s",
							sent, r.Method, r.RawPath, r.Body, f.hash+c.color)
					}
					wantEnsured(t, events, "new", true)
				})
			}
		}
	})

	t.Run("answers the label that another caller created after the lookup", func(t *testing.T) {
		for _, f := range labelForges {
			t.Run(f.name, func(t *testing.T) {
				w.fake.SetLabels(f.repo, forgetest.Label{Name: "race", Color: f.hash + "ededed"})
				w.fake.Script(forgetest.Answer{Path: f.decoded + "/labels/race", Status: http.StatusNotFound, Body: `{"message":"Not Found"}`})
				status, body, sent, events := w.ensure(t, f.slug, map[string]any{"name": "race"})
				if want := `{"name":"race","color":"ededed","description":""}`; status != http.StatusOK || !sameJSON(body, want) || sent != 3 {
					t.Errorf("POST %s/labels = %d %s after %d forge requests, want 200 %s after the lookup, the create and the lookup",
						f.slug, status, body, sent, want)
				}
				wantEnsured(t, events, "race", false)
			})
		}
	})

	t.Run("creates a label once for two callers at a time", func(t *testing.T) {
		for _, f := range labelForges {
			t.Run(f.name, func(t *testing.T) { w.ensureTwiceAtOnce(t, f) })
		}
	})
}

// ensureTwiceAtOnce sends two ensures of one new label on the forge f at
// once, both lookups held until both reached the forge, and fails the test
// unless one is answered 201 and the other 200, with the same label.
func (w *world) ensureTwiceAtOnce(t *testing.T, f labelForge) {
	t.Helper()
	w.fake.SetLabels(f.repo)
	release := make(chan struct{})
	lookup := forgetest.Answer{Path: f.decoded + "/labels/both", Release: release, Through: true}
	w.fake.Script(lookup, lookup)
	before := w.fake.Count()
	var (
		wg       sync.WaitGroup
		statuses [2]int
		bodies   [2]string
	)
	for i := range 2 {
		wg.Go(func() {
			status, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+f.slug+"/labels", w.ownerKey, map[string]any{"name": "both"})
			statuses[i], bodies[i] = status, string(body)
		})
	}
	// Both lookups find no label before either caller creates it.
	waitUntil(t, "both lookups", func() bool { return w.fake.Count() == before+2 })
	close(release)
	wg.Wait()
	slices.Sort(statuses[:])
	if statuses != [2]int{http.StatusOK, http.StatusCreated} || bodies[0] != bodies[1] {
		t.Errorf("the ensures were answered %v, %s and %s, want 200 and 201 with the same label", statuses, bodies[0], bodies[1])
	}
}
