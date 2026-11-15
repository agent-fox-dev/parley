package scm_test

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestRefs_ListsTheRefsBelowAPrefixInNameOrder(t *testing.T) {
	w := newWorld(t)
	branches := []string{"refs/heads/dev", "refs/heads/feature/x", "refs/heads/main", "refs/heads/release", "refs/heads/topic"}
	every := append(slices.Clone(branches), "refs/parley/changes/12/3", "refs/tags/v1", "refs/tags/v2")
	tests := []struct {
		query string
		want  []string
	}{
		{"", branches},
		{"?prefix=refs/tags/", []string{"refs/tags/v1", "refs/tags/v2"}},
		{"?prefix=refs/parley/changes/", []string{"refs/parley/changes/12/3"}},
		{"?prefix=refs/heads/feature/", []string{"refs/heads/feature/x"}},
		{"?prefix=refs/", every},
		{"?prefix=refs/none/", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			r := w.get(t, scmPath("demo", "/refs"+tt.query), w.ownerKey)
			if r.status != http.StatusOK || !slices.Equal(r.names(), tt.want) || r.json["next_cursor"] != nil {
				t.Errorf("%d %s, want the names %q and next_cursor null", r.status, r.body, tt.want)
			}
		})
	}
	var paged []string
	for cursor, pages := "", 0; pages == 0 || cursor != ""; pages++ {
		q := "?prefix=refs/&limit=2"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		r := w.get(t, scmPath("demo", "/refs"+q), w.ownerKey)
		if r.status != http.StatusOK || pages > len(every) {
			t.Fatalf("page %d: %d %s", pages, r.status, r.body)
		}
		paged = append(paged, r.names()...)
		cursor, _ = r.json["next_cursor"].(string)
	}
	if !slices.Equal(paged, every) {
		t.Errorf("the pages hold %q, want %q", paged, every)
	}
}

func TestRefs_DescribesARefByItsNamespaceAndTheLookups(t *testing.T) {
	w := newWorld(t)
	tests := []struct {
		name, kind, sha string
		protected       bool
	}{
		{"refs/heads/main", "branch", w.id["c2"], true},
		{"refs/heads/feature/x", "branch", w.id["c3"], false},
		{"refs/tags/v2", "tag", w.id["c2"], false},
		{"refs/parley/changes/12/3", "hub", w.id["c3"], true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := w.get(t, scmPath("demo", "/refs/"+tt.name), w.ownerKey)
			want := map[string]any{"name": tt.name, "kind": tt.kind, "sha": tt.sha, "protected": tt.protected, "tracking": false, "change": nil}
			if r.status != http.StatusOK || !maps.Equal(r.json, want) {
				t.Errorf("%d %s, want %v", r.status, r.body, want)
			}
		})
	}

	stubbed := newWorld(t, func(d *scm.Deps) {
		d.Protected = protectedOnly("feature/x")
		d.Changes = changeOf{"feature/x": {WorkspaceSlug: "demo", Number: 12}}
	})
	r := stubbed.get(t, scmPath("demo", "/refs/feature/x"), stubbed.ownerKey)
	if r.json["protected"] != true || !reflect.DeepEqual(r.json["change"], map[string]any{"workspace_slug": "demo", "number": 12.0}) {
		t.Errorf("feature/x with the stub lookups: %s, want protected true and change {demo, 12}", r.body)
	}
}

func TestRefs_ReadsANameThatDoesNotStartWithRefsAsABranch(t *testing.T) {
	w := newWorld(t)
	a := w.get(t, scmPath("demo", "/refs/feature/x"), w.ownerKey)
	b := w.get(t, scmPath("demo", "/refs/refs/heads/feature/x"), w.ownerKey)
	if a.status != http.StatusOK || !reflect.DeepEqual(a.json, b.json) || a.json["name"] != "refs/heads/feature/x" {
		t.Errorf("feature/x = %d %s, refs/heads/feature/x = %s; want the same object named refs/heads/feature/x", a.status, a.body, b.body)
	}
	if r := w.get(t, scmPath("demo", "/refs/refs/tags/v1"), w.ownerKey); r.json["kind"] != "tag" {
		t.Errorf("refs/tags/v1 = %s, want kind tag", r.body)
	}
}

func TestRefs_RefusesANameNoListedRefHas(t *testing.T) {
	w := newWorld(t)
	for _, name := range []string{"nope", "refs/heads/nope", "v1", "refs/notes/x"} {
		t.Run(name, func(t *testing.T) {
			r := w.get(t, scmPath("demo", "/refs/"+name), w.ownerKey)
			if r.status != http.StatusNotFound || r.errorType() != "ref_not_found" {
				t.Errorf("%d %s, want 404 ref_not_found", r.status, r.body)
			}
		})
	}
}

// protectedOnly is a protected-branch lookup that protects the branches
// named and no other.
type protectedOnly string

func (p protectedOnly) ProtectedBranches(context.Context, workspace.Workspace) (func(string) bool, error) {
	return func(branch string) bool { return branch == string(p) }, nil
}

// changeOf is a change lookup that names the change of each source branch
// it holds and of no commit.
type changeOf map[string]scm.Change

func (c changeOf) OpenChangeOfBranch(_ context.Context, _, branch string) (*scm.Change, error) {
	if ch, ok := c[branch]; ok {
		return &ch, nil
	}
	return nil, nil
}

func (changeOf) ChangeOfCommit(context.Context, string, string) (*scm.CommitChange, error) {
	return nil, nil
}
