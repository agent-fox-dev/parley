package scm_test

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/scm"
)

func TestCommits_ListsTheFirstParentHistoryOfARefInPagesFromOneCommit(t *testing.T) {
	w := newWorld(t)
	id := w.addHistory(t)
	path := scmPath("hist", "/commits?ref=main")
	r := w.get(t, path, w.ownerKey)
	if want := []string{id["m"], id["c2"], id["c1"]}; r.status != http.StatusOK || !slices.Equal(r.shas(), want) || r.json["resolved_sha"] != id["m"] {
		t.Errorf("GET %s = %d %s, want the shas %v and resolved_sha %s", path, r.status, r.body, want, id["m"])
	}
	if all := w.get(t, path+"&first_parent=false", w.ownerKey).shas(); !slices.Contains(all, id["s1"]) || slices.Contains(r.shas(), id["s1"]) {
		t.Errorf("the merged side branch's commit is in %v with first_parent=false and in %v without it; want it only in the first", all, r.shas())
	}

	p1 := w.get(t, path+"&limit=2", w.ownerKey)
	next, _ := p1.json["next_cursor"].(string)
	repo := w.workspaces.RepoPath("hist")
	gitrepotest.Git(t, repo, "update-ref", "refs/heads/main", commit(t, repo, id["m"], "later.txt", "later"))
	p2 := w.get(t, path+"&limit=2&cursor="+url.QueryEscape(next), w.ownerKey)
	if got := append(p1.shas(), p2.shas()...); !slices.Equal(got, []string{id["m"], id["c2"], id["c1"]}) ||
		p2.json["resolved_sha"] != id["m"] || p2.json["next_cursor"] != nil {
		t.Errorf("pages %s and %s, the ref moved in between; want m, c2, c1 read from m and no further cursor", p1.body, p2.body)
	}
	if r := w.get(t, scmPath("hist", "/commits?ref=docs&cursor="+url.QueryEscape(next)), w.ownerKey); r.status != http.StatusBadRequest || r.errorType() != "invalid_request" {
		t.Errorf("a cursor with another ref: %d %s, want 400 invalid_request", r.status, r.body)
	}
}

func TestCommits_KeepsTheCommitsTheFiltersAskFor(t *testing.T) {
	w := newWorld(t)
	id := w.addHistory(t)
	for _, c := range []struct {
		query string
		want  []string
	}{
		{"path=docs/a.md", []string{"c2", "c1"}},
		{"path=docs", []string{"c2", "c1"}},
		{"path=new.md", []string{"r"}},
		{"path=new.md&follow=true", []string{"r", "c1"}},
		{"since=" + histT2, []string{"r", "c2"}},
		{"until=" + histT1, []string{"c1"}},
		{"author=ALICE", []string{"c2", "c1"}},
		{"author=example.org", []string{"r"}},
		{"path=never.txt", nil},
	} {
		t.Run(c.query, func(t *testing.T) {
			want := []string{}
			for _, n := range c.want {
				want = append(want, id[n])
			}
			r := w.get(t, scmPath("hist", "/commits?ref=docs&"+c.query), w.ownerKey)
			if r.status != http.StatusOK || !slices.Equal(r.shas(), want) {
				t.Errorf("%d %s, want the shas %v", r.status, r.body, want)
			}
		})
	}
}

// commitLookups name, for the commit of sha alone, the change demo #12
// version 3 and the session sess-1.
type commitLookups struct{ sha *string }

func (commitLookups) OpenChangeOfBranch(context.Context, string, string) (*scm.Change, error) {
	return nil, nil
}

func (l commitLookups) ChangeOfCommit(_ context.Context, _, sha string) (*scm.CommitChange, error) {
	if sha != *l.sha {
		return nil, nil
	}
	return &scm.CommitChange{WorkspaceSlug: "demo", Number: 12, Version: 3}, nil
}

func (l commitLookups) SessionOf(_ context.Context, _, sha string) (*string, error) {
	if sha != *l.sha {
		return nil, nil
	}
	return new("sess-1"), nil
}

func TestCommits_DescribesEachCommitTheSameWay(t *testing.T) {
	w := newWorld(t)
	id := w.addHistory(t)
	alice := map[string]any{"name": "Alice", "email": "alice@example.com", "time": histT2}
	want := map[string]any{
		"sha": id["c2"], "parents": []any{id["c1"]}, "author": alice, "committer": alice, "message": histMessage,
		"tree_sha": strings.TrimSpace(gitrepotest.Git(t, w.workspaces.RepoPath("hist"), "rev-parse", id["c2"]+"^{tree}")),
		"change":   nil, "session_id": nil,
	}
	item := func(r reply) map[string]any {
		items, _ := r.json["items"].([]any)
		if len(items) < 2 {
			t.Fatalf("%d %s, want three commits", r.status, r.body)
		}
		o, _ := items[1].(map[string]any)
		return o
	}
	if got := item(w.get(t, scmPath("hist", "/commits?ref=main"), w.ownerKey)); !reflect.DeepEqual(got, want) {
		t.Errorf("c2 listed = %v, want %v", got, want)
	}
	want["stats"] = map[string]any{"files_changed": 2.0, "insertions": 5.0, "deletions": 1.0}
	if got := item(w.get(t, scmPath("hist", "/commits?ref=main&stats=true"), w.ownerKey)); !reflect.DeepEqual(got, want) {
		t.Errorf("c2 listed with stats = %v, want %v", got, want)
	}

	var sha string
	looked := newWorld(t, func(d *scm.Deps) { d.Changes, d.Sessions = commitLookups{&sha}, commitLookups{&sha} })
	sha = looked.addHistory(t)["c2"]
	o := item(looked.get(t, scmPath("hist", "/commits?ref=main"), looked.ownerKey))
	if !reflect.DeepEqual(o["change"], map[string]any{"workspace_slug": "demo", "number": 12.0, "version": 3.0}) || o["session_id"] != "sess-1" {
		t.Errorf("c2 with lookups that know it: change %v, session_id %v; want demo #12 version 3 and sess-1", o["change"], o["session_id"])
	}
}

func TestCommits_ReadsOneCommitByAnyRevisionWithItsStats(t *testing.T) {
	w := newWorld(t)
	id := w.addHistory(t)
	r := w.get(t, scmPath("hist", "/commits/"+id["c2"]), w.ownerKey)
	stats, _ := r.json["stats"].(map[string]any)
	if _, hasFiles := r.json["files"]; r.status != http.StatusOK || r.json["sha"] != id["c2"] || stats["files_changed"] != 2.0 || hasFiles {
		t.Errorf("GET commits/c2 = %d %s, want c2 with stats over two files and no files", r.status, r.body)
	}
	blob := strings.TrimSpace(gitrepotest.Git(t, w.workspaces.RepoPath("hist"), "rev-parse", id["m"]+":side.txt"))
	merge := w.get(t, scmPath("hist", "/commits/main?files=true"), w.ownerKey)
	want := []any{map[string]any{"path": "side.txt", "old_path": nil, "status": "added", "insertions": 1.0, "deletions": 0.0,
		"binary": false, "old_sha": nil, "new_sha": blob}}
	if merge.json["sha"] != id["m"] || !reflect.DeepEqual(merge.json["files"], want) {
		t.Errorf("GET commits/main?files=true = %d %s, want m's changes against c2, %v", merge.status, merge.body, want)
	}
	root := w.get(t, scmPath("hist", "/commits/"+id["c1"]+"?files=true"), w.ownerKey)
	files, _ := root.json["files"].([]any)
	for _, f := range files {
		if f.(map[string]any)["status"] != "added" {
			t.Errorf("the root commit's file %v, want status added", f)
		}
	}
	if len(files) != 2 {
		t.Errorf("GET commits/c1?files=true = %d %s, want its two files added", root.status, root.body)
	}
}

func TestCommits_ResolveTheirRevisionThroughTheResolver(t *testing.T) {
	w := newWorld(t)
	hist := w.addHistory(t)
	for _, c := range []struct{ slug, path, member, want string }{
		{"demo", "/commits/feature%2Fx", "sha", w.id["c3"]},
		{"demo", "/commits?ref=v2", "resolved_sha", w.id["c2"]},
		{"hist", "/commits", "resolved_sha", hist["m"]},
	} {
		if r := w.get(t, scmPath(c.slug, c.path), w.ownerKey); r.status != http.StatusOK || r.json[c.member] != c.want {
			t.Errorf("GET %s on %s = %d %s, want %s %s", c.path, c.slug, r.status, r.body, c.member, c.want)
		}
	}
}

func TestCommits_RefusesAMalformedParameter(t *testing.T) {
	w := newWorld(t)
	for _, path := range []string{
		"/commits?stats=yes", "/commits?first_parent=1", "/commits?since=yesterday",
		"/commits?until=2026-13-01T00:00:00Z", "/commits?follow=true", "/commits/main?files=maybe",
	} {
		if r := w.get(t, scmPath("demo", path), w.ownerKey); r.status != http.StatusBadRequest || r.errorType() != "invalid_request" {
			t.Errorf("GET %s = %d %s, want 400 invalid_request", path, r.status, r.body)
		}
	}
}
