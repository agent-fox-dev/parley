package change_test

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// The reads share one world: changesOfDemo, with change 4 given a second
// version whose head has an inner commit, an expander for checks, the
// member m, who reads demo, and a PAT bound to demo--m1.
func TestRead_ServesAChangeItsVersionsAndItsCommits(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	f := w.changesOfDemo(t)
	inner := commit(t, w.demo, "b4", f.b2, "Inner\n", map[string]string{"i.txt": "i\n"})
	b4 := commit(t, w.demo, "b4", inner, "Outer\n", map[string]string{"i.txt": "o\n"})
	w.mustRecord(t, change.VersionInput{Workspace: "demo", Branch: "b4", Head: b4, Kind: change.KindPush, Caller: w.ownerCaller()})
	w.changes.RegisterExpander("checks", func(_ context.Context, ch change.Change) (any, error) {
		return []map[string]any{{"name": "ci"}}, nil
	})
	_, mKey := w.member(t, "m")
	bound := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"changes:read"}, Workspace: "demo--m1", CreatedAt: time.Now().UTC()})

	t.Run("the change object", func(t *testing.T) {
		status, got, _ := w.get(t, "/workspaces/demo/changes/1", mKey)
		if status != http.StatusOK {
			t.Fatalf("GET changes/1 = %d %v, want 200", status, got)
		}
		want := []string{"workspace_slug", "number", "ref", "title", "intent", "state", "target_branch", "source",
			"version", "head_sha", "base_sha", "depends_on", "dependants", "author", "session_id", "sessions",
			"mergeable", "conflict", "needs_restack", "restack_reason", "stats", "paths", "paths_truncated", "landing",
			"auto_land", "labels", "landed", "reverts", "reverted_by", "created_at", "updated_at"}
		if keys := keysOf(got); !reflect.DeepEqual(keys, slices.Sorted(slices.Values(want))) {
			t.Errorf("members = %v, want %v", keys, want)
		}
		for _, name := range []string{"head_sha", "base_sha"} {
			if s, _ := got[name].(string); len(s) != 40 {
				t.Errorf("%s = %v, want a full commit id", name, got[name])
			}
		}
		for _, name := range []string{"dependants", "sessions", "paths", "labels"} {
			if _, ok := got[name].([]any); !ok {
				t.Errorf("%s = %#v, want an array", name, got[name])
			}
		}
		stamp := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)
		for _, name := range []string{"created_at", "updated_at"} {
			if s, _ := got[name].(string); !stamp.MatchString(s) {
				t.Errorf("%s = %v, want RFC 3339 UTC in whole seconds", name, got[name])
			}
		}
		if !reflect.DeepEqual(got["sessions"], []any{sessionOne}) || got["version"] != 3.0 {
			t.Errorf("sessions, version = %v, %v; want [%s], 3", got["sessions"], got["version"], sessionOne)
		}
	})

	t.Run("include", func(t *testing.T) {
		for _, c := range []struct {
			include string
			status  int
			present map[string]any
			absent  []string
		}{
			{"versions", http.StatusOK, nil, []string{"checks", "stack"}},
			{"", http.StatusOK, nil, []string{"versions", "checks"}},
			{"versions,checks", http.StatusOK, map[string]any{"checks": []any{map[string]any{"name": "ci"}}}, nil},
			{"stack", http.StatusOK, nil, []string{"stack", "versions"}},
			{"bogus", http.StatusBadRequest, nil, nil},
			{"versions,,bogus", http.StatusBadRequest, nil, nil},
			{"versions,,checks", http.StatusBadRequest, nil, nil},
		} {
			path := "/workspaces/demo/changes/1"
			if c.include != "" {
				path += "?include=" + url.QueryEscape(c.include)
			}
			status, got, _ := w.get(t, path, w.ownerKey)
			if status != c.status {
				t.Errorf("include=%s: %d %v, want %d", c.include, status, got, c.status)
				continue
			}
			if status != http.StatusOK {
				if errorType(got) != "invalid_request" {
					t.Errorf("include=%s: %v, want invalid_request", c.include, got)
				}
				continue
			}
			if strings.Contains(c.include, "versions") {
				if n := versionNumbers(got["versions"]); !reflect.DeepEqual(n, []float64{1, 2, 3}) {
					t.Errorf("include=%s: versions numbered %v, want 1, 2, 3", c.include, n)
				}
			}
			for name, v := range c.present {
				if !reflect.DeepEqual(got[name], v) {
					t.Errorf("include=%s: %s = %v, want %v", c.include, name, got[name], v)
				}
			}
			for _, name := range c.absent {
				if _, ok := got[name]; ok {
					t.Errorf("include=%s: has %s, want no such member", c.include, name)
				}
			}
		}
	})

	t.Run("the version list", func(t *testing.T) {
		status, got, _ := w.get(t, "/workspaces/demo/changes/1/versions", w.ownerKey)
		if status != http.StatusOK || !reflect.DeepEqual(versionNumbers(got["items"]), []float64{1, 2, 3}) || got["next_cursor"] != nil {
			t.Fatalf("GET versions = %d %v, want 200 with versions 1, 2, 3 and no cursor", status, got)
		}
		first := got["items"].([]any)[0].(map[string]any)
		want := []string{"base_sha", "created_at", "created_by", "head_sha", "kind", "number", "patch_equivalent_to", "session_id", "stats"}
		if keys := keysOf(first); !reflect.DeepEqual(keys, want) {
			t.Errorf("version members = %v, want %v", keys, want)
		}
		if keys := keysOf(first["created_by"].(map[string]any)); !reflect.DeepEqual(keys, []string{"actor_id", "actor_type", "username"}) {
			t.Errorf("created_by members = %v", keys)
		}
		_, page1, _ := w.get(t, "/workspaces/demo/changes/1/versions?limit=2", w.ownerKey)
		cursor, _ := page1["next_cursor"].(string)
		_, page2, _ := w.get(t, "/workspaces/demo/changes/1/versions?limit=2&cursor="+url.QueryEscape(cursor), w.ownerKey)
		if !reflect.DeepEqual(versionNumbers(page1["items"]), []float64{1, 2}) || cursor == "" ||
			!reflect.DeepEqual(versionNumbers(page2["items"]), []float64{3}) || page2["next_cursor"] != nil {
			t.Errorf("pages of 2 = %v, then %v; want 1, 2 with a cursor, then 3 without", page1, page2)
		}
	})

	t.Run("one version", func(t *testing.T) {
		for _, c := range []struct {
			version   string
			status    int
			errorType string
			number    float64
		}{
			{"2", http.StatusOK, "", 2},
			{"latest", http.StatusOK, "", 3},
			{"0", http.StatusBadRequest, "invalid_request", 0},
			{"-1", http.StatusBadRequest, "invalid_request", 0},
			{"x", http.StatusBadRequest, "invalid_request", 0},
			{"1.5", http.StatusBadRequest, "invalid_request", 0},
			{"4", http.StatusNotFound, "not_found", 0},
		} {
			status, got, _ := w.get(t, "/workspaces/demo/changes/1/versions/"+c.version, w.ownerKey)
			switch {
			case status != c.status:
				t.Errorf("versions/%s = %d %v, want %d", c.version, status, got, c.status)
			case c.errorType != "" && errorType(got) != c.errorType:
				t.Errorf("versions/%s = %v, want %s", c.version, got, c.errorType)
			case c.errorType == "" && (got["number"] != c.number || got["ref"] != change.VersionRef(1, int64(c.number)) || got["head_sha"] == nil):
				t.Errorf("versions/%s = %v, want version %v with its ref", c.version, got, c.number)
			}
		}
	})

	t.Run("by commit", func(t *testing.T) {
		for _, c := range []struct {
			name, sha string
			status    int
			errorType string
			number    float64
			version   any
			landed    bool
		}{
			{"an earlier version's head", f.task2, http.StatusOK, "", 1, 2.0, false},
			{"the current head", f.task1, http.StatusOK, "", 1, 3.0, false},
			{"in upper case", strings.ToUpper(f.task2), http.StatusOK, "", 1, 2.0, false},
			{"a landed commit, also the head of another change", f.landed, http.StatusOK, "", 3, 1.0, true},
			{"an inner commit", inner, http.StatusNotFound, "change_not_found", 0, nil, false},
			{"a commit on main of no change", w.base, http.StatusNotFound, "change_not_found", 0, nil, false},
			{"a short id", "abc123", http.StatusBadRequest, "invalid_request", 0, nil, false},
			{"forty characters that are not hexadecimal", strings.Repeat("g", 40), http.StatusBadRequest, "invalid_request", 0, nil, false},
		} {
			status, got, _ := w.get(t, "/workspaces/demo/changes/by-commit/"+c.sha, w.ownerKey)
			switch {
			case status != c.status:
				t.Errorf("%s: by-commit = %d %v, want %d", c.name, status, got, c.status)
			case c.errorType != "" && errorType(got) != c.errorType:
				t.Errorf("%s: by-commit = %v, want %s", c.name, got, c.errorType)
			case c.errorType == "":
				ch, _ := got["change"].(map[string]any)
				if ch["number"] != c.number || got["version"] != c.version || got["landed"] != c.landed || len(got) != 3 || len(ch) != 31 {
					t.Errorf("%s: by-commit = %v, want change %v, version %v, landed %v", c.name, got, c.number, c.version, c.landed)
				}
			}
		}
	})

	t.Run("a change the workspace does not have", func(t *testing.T) {
		for _, base := range []string{"/workspaces/demo/changes/99", "/workspaces/demo/changes/abc", "/workspaces/demo/changes/0",
			"/workspaces/demo/changes/-1", "/workspaces/demo--m1/changes/1"} {
			for _, route := range []string{"", "/versions", "/versions/1", "/diff"} {
				if status, got, _ := w.get(t, base+route, w.ownerKey); status != http.StatusNotFound || errorType(got) != "change_not_found" {
					t.Errorf("GET %s = %d %v, want 404 change_not_found", base+route, status, got)
				}
			}
		}
		if status, got, _ := w.get(t, "/workspaces/demo--m1/changes/by-commit/"+f.task2, w.ownerKey); status != http.StatusNotFound || errorType(got) != "change_not_found" {
			t.Errorf("by-commit on the fork = %d %v, want 404 change_not_found", status, got)
		}
	})

	t.Run("a caller without the scope or access", func(t *testing.T) {
		gitPAT, _ := w.patFor(t, []string{"git:read"}, nil)
		strangerKey := w.IssueKey(t, w.CreateUser(t, "u9"))
		for _, route := range []string{"/changes", "/changes/1", "/changes/by-commit/" + f.task1, "/changes/1/versions", "/changes/1/versions/1", "/changes/1/diff"} {
			for _, slug := range []string{"demo", "no-such-workspace"} {
				if status, got, _ := w.get(t, "/workspaces/"+slug+route, gitPAT); status != http.StatusForbidden || errorType(got) != "insufficient_scope" {
					t.Errorf("GET %s on %s with git:read = %d %v, want 403 insufficient_scope", route, slug, status, got)
				}
			}
			status, _, hidden := w.get(t, "/workspaces/demo"+route, strangerKey)
			_, _, missing := w.get(t, "/workspaces/no-such-workspace"+route, strangerKey)
			if status != http.StatusNotFound || string(hidden) != string(missing) || !strings.Contains(string(hidden), `"not_found"`) {
				t.Errorf("GET %s without access = %d %s, want 404 not_found as for no workspace: %s", route, status, hidden, missing)
			}
		}
	})

	t.Run("a token bound to a fork", func(t *testing.T) {
		for _, path := range []string{"/changes/1", "/changes/1/versions", "/changes/1/versions/latest", "/changes/1/diff", "/changes/by-commit/" + f.task2} {
			if status, got, _ := w.get(t, "/workspaces/demo"+path, bound); status != http.StatusOK {
				t.Errorf("GET %s of the fork's change = %d %v, want 200", path, status, got)
			}
		}
		for _, path := range []string{"/changes/2", "/changes/2/versions", "/changes/2/versions/1", "/changes/2/diff", "/changes/by-commit/" + f.landed, "/changes/by-commit/" + f.b2} {
			if status, got, _ := w.get(t, "/workspaces/demo"+path, bound); status != http.StatusNotFound || errorType(got) != "change_not_found" {
				t.Errorf("GET %s of another change = %d %v, want 404 change_not_found", path, status, got)
			}
		}
		if status, got, _ := w.get(t, "/workspaces/demo/changes?state=draft&state=open&state=landed", bound); status != http.StatusOK ||
			!reflect.DeepEqual(versionNumbers(got["items"]), []float64{1}) {
			t.Errorf("GET /changes = %d %v, want 200 with only the fork's change 1", status, got)
		}
	})
}

// versionNumbers are the numbers of a decoded list of versions.
func versionNumbers(list any) []float64 {
	var out []float64
	items, _ := list.([]any)
	for _, item := range items {
		v, _ := item.(map[string]any)
		n, _ := v["number"].(float64)
		out = append(out, n)
	}
	return out
}

// keysOf are the member names of a decoded object, sorted.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
