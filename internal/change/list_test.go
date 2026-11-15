package change_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
)

// allStates asks a list for the changes in every state.
const allStates = "state=draft&state=open&state=landed&state=abandoned"

// listWorld is a world whose demo holds eight changes:
//
//	n  state      target   source   author  labels  paths
//	1  open       main     demo     owner   perf    c1.txt
//	2  open       release  demo--f1 owner   perf    docs/guide.md
//	3  draft      release  demo     owner   perf    src/main.go
//	4  landed     main     demo     owner           src/a.go
//	5  open       main     demo--f2 m       perf    f2.txt
//	6  draft      main     demo--f1 owner           docs/x/y.md
//	7  abandoned  main     demo     owner   perf    c7.txt
//	8  open       release  demo--f2 m       ui      src/deep/b.go
//
// Change 2 had sessionOne at version 1 and has sessionTwo at version 2;
// change 3 is mergeable conflict, change 8 clean; change 5's landing is
// blocked, as is every draft's; 4 <- 5 <- 6 depend on each other. From the
// most recently updated: 3, 8, then 1 and 6 in the same second (1 later
// within it), 5, 2, 7, 4.
type listWorld struct {
	*world
	mID, mKey string
}

func newListWorld(t *testing.T) *listWorld {
	t.Helper()
	w := &listWorld{world: newWorld(t)}
	w.mID, w.mKey = w.member(t, "m")
	f1 := w.addFork(t, "demo--f1", "main", nil, nil)
	f2 := w.addFork(t, "demo--f2", "main", nil, nil)
	w.set(t, "demo--f2", "owner_id", w.mID)
	asM := func(in *change.OpenInput) {
		in.SourceWorkspace = "demo--f2"
		in.Caller = change.Caller{UserID: w.mID, Credential: change.Credential{Type: change.CredentialAPIKey, ID: "km"}}
	}
	labels := func(l ...string) func(*change.OpenInput) { return func(in *change.OpenInput) { in.Labels = l } }
	file := func(path string) map[string]string { return map[string]string{path: path + "\n"} }

	commit(t, w.demo, "c1", w.base, "C1\n", file("c1.txt"))
	w.openBranch(t, "c1", labels("perf"))
	v1 := commit(t, f1, "f1a", w.base, "F1a\n", file("docs/guide.md"))
	w.openBranch(t, "f1a", func(in *change.OpenInput) {
		in.SourceWorkspace, in.TargetBranch, in.SessionID, in.Labels = "demo--f1", new("release"), new(sessionOne), []string{"perf"}
	})
	v2 := commit(t, f1, "f1a", v1, "F1a again\n", file("docs/guide.md"))
	w.mustRecord(t, change.VersionInput{Workspace: "demo--f1", Branch: "f1a", Head: v2, Kind: change.KindPush,
		Caller: w.ownerCaller(), SessionID: new(sessionTwo)})
	commit(t, w.demo, "c3", w.base, "C3\n", file("src/main.go"))
	w.openBranch(t, "c3", func(in *change.OpenInput) {
		in.TargetBranch, in.Draft, in.Labels = new("release"), true, []string{"perf"}
	})
	c4 := commit(t, w.demo, "c4", w.base, "C4\n", file("src/a.go"))
	w.openBranch(t, "c4", nil)
	commit(t, f2, "f2a", w.base, "F2a\n", file("f2.txt"))
	w.openBranch(t, "f2a", func(in *change.OpenInput) { asM(in); in.Labels = []string{"perf"} })
	commit(t, f1, "f1b", w.base, "F1b\n", file("docs/x/y.md"))
	w.openBranch(t, "f1b", func(in *change.OpenInput) { in.SourceWorkspace, in.Draft = "demo--f1", true })
	commit(t, w.demo, "c7", w.base, "C7\n", file("c7.txt"))
	w.openBranch(t, "c7", labels("perf"))
	commit(t, f2, "f2b", w.base, "F2b\n", file("src/deep/b.go"))
	w.openBranch(t, "f2b", func(in *change.OpenInput) { asM(in); in.TargetBranch, in.Labels = new("release"), []string{"ui"} })

	w.write(t, func(ctx context.Context, tx *sql.Tx) error {
		c := w.changes
		for _, err := range []error{
			c.SetLanded(ctx, tx, "demo", 4, change.Landed{SHA: c4, At: time.Now(), LandID: "l4",
				By: change.Actor{ActorType: change.CredentialAPIKey, ActorID: &w.owner}}),
			c.SetAbandoned(ctx, tx, "demo", 7, "manual"),
			c.SetMergeable(ctx, tx, "demo", 3, change.MergeableConflict, []byte(`{"files":["src/main.go"]}`)),
			c.SetMergeable(ctx, tx, "demo", 8, change.MergeableClean, nil),
			c.SetLanding(ctx, tx, "demo", 5, change.Landing{Status: change.LandingBlocked, Reason: new("policy")}),
			c.SetDependsOn(ctx, tx, "demo", 5, new(int64(4))),
			c.SetDependsOn(ctx, tx, "demo", 6, new(int64(5))),
		} {
			if err != nil {
				return err
			}
		}
		base := time.Date(2027, 1, 2, 3, 4, 0, 0, time.UTC)
		for n, at := range map[int64]time.Duration{3: 8 * time.Second, 8: 7 * time.Second, 1: 6900 * time.Millisecond,
			6: 6100 * time.Millisecond, 5: 5 * time.Second, 2: 4 * time.Second, 7: 3 * time.Second, 4: 2 * time.Second} {
			if _, err := tx.ExecContext(ctx, `UPDATE changes SET updated_at = ? WHERE workspace_slug = 'demo' AND number = ?`,
				base.Add(at).UnixNano(), n); err != nil {
				return err
			}
		}
		return nil
	})
	return w
}

// list reads GET /workspaces/<slug>/changes?<query> with the credential
// and fails the test unless it answers 200.
func (w *listWorld) list(t *testing.T, slug, query, credential string) map[string]any {
	t.Helper()
	status, got, _ := w.get(t, "/workspaces/"+slug+"/changes?"+query, credential)
	if status != http.StatusOK {
		t.Fatalf("GET %s/changes?%s = %d %v, want 200", slug, query, status, got)
	}
	return got
}

func TestList_FiltersOrdersAndPagesTheChanges(t *testing.T) {
	t.Parallel()
	w := newListWorld(t)

	t.Run("without filters", func(t *testing.T) {
		got := w.list(t, "demo", "", w.ownerKey)
		items, _ := got["items"].([]any)
		if n := versionNumbers(items); !reflect.DeepEqual(n, []float64{3, 8, 6, 1, 5, 2}) || got["next_cursor"] != nil {
			t.Errorf("numbers = %v, next_cursor %v; want the draft and open changes 3, 8, 6, 1, 5, 2 and no cursor", n, got["next_cursor"])
		}
		for i, item := range items {
			ch := item.(map[string]any)
			if ch["state"] != change.StateDraft && ch["state"] != change.StateOpen {
				t.Errorf("change %v is %v", ch["number"], ch["state"])
			}
			for _, name := range []string{"conflict", "paths", "paths_truncated"} {
				if _, ok := ch[name]; ok {
					t.Errorf("change %v has %s", ch["number"], name)
				}
			}
			if i > 0 {
				prev := items[i-1].(map[string]any)
				pu, u := prev["updated_at"].(string), ch["updated_at"].(string)
				if pu < u || pu == u && prev["number"].(float64) < ch["number"].(float64) {
					t.Errorf("change %v (%s) follows change %v (%s)", ch["number"], u, prev["number"], pu)
				}
			}
		}
		withPaths := w.list(t, "demo", "include=paths", w.ownerKey)
		for _, item := range withPaths["items"].([]any) {
			ch := item.(map[string]any)
			_, hasConflict := ch["conflict"]
			if _, ok := ch["paths"].([]any); !ok || ch["paths_truncated"] != false || hasConflict {
				t.Errorf("include=paths: change %v = %v, want paths and paths_truncated without conflict", ch["number"], ch)
			}
		}
		for _, include := range []string{"versions", "paths,versions", ""} {
			status, out, _ := w.get(t, "/workspaces/demo/changes?include="+include, w.ownerKey)
			if status != http.StatusBadRequest || errorType(out) != "invalid_request" {
				t.Errorf("include=%s = %d %v, want 400 invalid_request", include, status, out)
			}
		}
	})

	t.Run("filters", func(t *testing.T) {
		for _, c := range []struct {
			query, credential string
			want              []float64
		}{
			{"state=landed", w.ownerKey, []float64{4}},
			{"state=draft&state=abandoned", w.ownerKey, []float64{3, 6, 7}},
			{"target_branch=release", w.ownerKey, []float64{3, 8, 2}},
			{"source_workspace=demo--f1", w.ownerKey, []float64{6, 2}},
			{"author=" + w.owner, w.ownerKey, []float64{3, 6, 1, 2}},
			{"author=me", w.mKey, []float64{8, 5}},
			{"session_id=" + sessionOne, w.ownerKey, []float64{2}},
			{"label=perf", w.ownerKey, []float64{3, 1, 5, 2}},
			{"path=" + url.QueryEscape("docs/**"), w.ownerKey, []float64{6, 2}},
			{"path=" + url.QueryEscape("src/*.go"), w.ownerKey, []float64{3}},
			{"mergeable=conflict", w.ownerKey, []float64{3}},
			{"landing_status=blocked", w.ownerKey, []float64{3, 6, 5}},
			{"target_branch=main&label=perf&state=open", w.ownerKey, []float64{1, 5}},
			{"label=none", w.ownerKey, nil},
		} {
			got := w.list(t, "demo", c.query, c.credential)
			if n := versionNumbers(got["items"]); !reflect.DeepEqual(n, c.want) || got["next_cursor"] != nil {
				t.Errorf("%s: numbers %v, next_cursor %v; want %v and no cursor", c.query, n, got["next_cursor"], c.want)
			}
			if items, ok := got["items"].([]any); !ok || items == nil {
				t.Errorf("%s: items = %#v, want an array", c.query, got["items"])
			}
		}
	})

	t.Run("sort and stack_of", func(t *testing.T) {
		for _, c := range []struct {
			query string
			want  []float64
		}{
			{"sort=number&" + allStates, []float64{1, 2, 3, 4, 5, 6, 7, 8}},
			{"sort=number", []float64{1, 2, 3, 5, 6, 8}},
			{"stack_of=5", []float64{4, 5, 6}},
			{"stack_of=6", []float64{4, 5, 6}},
			{"stack_of=4", []float64{4, 5, 6}},
			{"stack_of=1", []float64{1}},
			{"stack_of=5&sort=number&state=landed", []float64{4, 5, 6}},
			{"stack_of=5&source_workspace=demo--f1", []float64{6}},
		} {
			if n := versionNumbers(w.list(t, "demo", c.query, w.ownerKey)["items"]); !reflect.DeepEqual(n, c.want) {
				t.Errorf("%s: numbers %v, want %v", c.query, n, c.want)
			}
		}
	})

	t.Run("on a fork's slug", func(t *testing.T) {
		got := w.list(t, "demo--f1", allStates, w.ownerKey)
		for _, item := range got["items"].([]any) {
			ch := item.(map[string]any)
			if ch["workspace_slug"] != "demo" || ch["source"].(map[string]any)["workspace_slug"] != "demo--f1" {
				t.Errorf("change %v of %v from %v", ch["number"], ch["workspace_slug"], ch["source"])
			}
		}
		if n := versionNumbers(got["items"]); !reflect.DeepEqual(n, []float64{6, 2}) {
			t.Errorf("demo--f1: numbers %v, want 6, 2", n)
		}
		for query, want := range map[string][]float64{"label=perf": {5}, "sort=number": {5, 8}, "stack_of=5": {5}} {
			if n := versionNumbers(w.list(t, "demo--f2", query, w.mKey)["items"]); !reflect.DeepEqual(n, want) {
				t.Errorf("demo--f2?%s: numbers %v, want %v", query, n, want)
			}
		}
		if status, out, _ := w.get(t, "/workspaces/demo--f2/changes?stack_of=6", w.mKey); status != http.StatusNotFound || errorType(out) != "change_not_found" {
			t.Errorf("demo--f2?stack_of=6 = %d %v, want 404 change_not_found", status, out)
		}
	})

	t.Run("a value outside a filter's domain", func(t *testing.T) {
		for _, c := range []struct {
			query, credential string
			status            int
			errorType         string
		}{
			{"state=closed", w.ownerKey, http.StatusBadRequest, "invalid_request"},
			{"state=open&state=closed", w.ownerKey, http.StatusBadRequest, "invalid_request"},
			{"mergeable=maybe", w.ownerKey, http.StatusBadRequest, "invalid_request"},
			{"landing_status=green", w.ownerKey, http.StatusBadRequest, "invalid_request"},
			{"sort=title", w.ownerKey, http.StatusBadRequest, "invalid_request"},
			{"author=me", w.Token, http.StatusBadRequest, "invalid_request"},
			{"stack_of=x", w.ownerKey, http.StatusBadRequest, "invalid_request"},
			{"stack_of=0", w.ownerKey, http.StatusBadRequest, "invalid_request"},
			{"stack_of=99", w.ownerKey, http.StatusNotFound, "change_not_found"},
		} {
			status, out, _ := w.get(t, "/workspaces/demo/changes?"+c.query, c.credential)
			if status != c.status || errorType(out) != c.errorType {
				t.Errorf("%s = %d %v, want %d %s", c.query, status, out, c.status, c.errorType)
			}
		}
	})

	t.Run("pages", func(t *testing.T) {
		for _, order := range []string{"", "sort=number&"} {
			query := order + allStates
			want := versionNumbers(w.list(t, "demo", query+"&limit=200", w.ownerKey)["items"])
			if len(want) != 8 {
				t.Fatalf("%s: unpaged numbers %v, want all eight", query, want)
			}
			for _, limit := range []int{1, 3, 200} {
				var got []float64
				cursor := ""
				for range 10 {
					q := query + "&limit=" + strconv.Itoa(limit)
					if cursor != "" {
						q += "&cursor=" + url.QueryEscape(cursor)
					}
					page := w.list(t, "demo", q, w.ownerKey)
					got = append(got, versionNumbers(page["items"])...)
					cursor, _ = page["next_cursor"].(string)
					if cursor == "" {
						break
					}
				}
				if cursor != "" || !reflect.DeepEqual(got, want) || len(slices.Compact(slices.Sorted(slices.Values(got)))) != len(got) {
					t.Errorf("%s by %d: pages gave %v (last cursor %q), want %v once each", query, limit, got, cursor, want)
				}
			}
		}
	})
}
