package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// retireEvent is the metadata of an archive, reclone or delete event this
// file reads, with the workspace it was written for.
type retireEvent struct {
	Workspace     string
	OriginPush    string   `json:"origin_push"`
	Kind          string   `json:"kind"`
	RevokedTokens []string `json:"revoked_tokens"`
	DeletedForks  []string `json:"deleted_forks"`
}

// retireEvents are the events of type typ of every workspace in the hub's
// event log, oldest first.
func (h *workspaceHub) retireEvents(t *testing.T, typ string) []retireEvent {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT workspace, metadata FROM hub_events WHERE type = ? ORDER BY seq`, typ)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []retireEvent
	for rows.Next() {
		var slug sql.NullString
		var meta string
		if err := rows.Scan(&slug, &meta); err != nil {
			t.Fatal(err)
		}
		var e retireEvent
		if err := json.Unmarshal([]byte(meta), &e); err != nil {
			t.Fatalf("the %s event's metadata %s: %v", typ, meta, err)
		}
		e.Workspace = slug.String
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

// refuseBranch installs an update hook in origin that refuses every update
// of the branch and takes the others.
func refuseBranch(t *testing.T, origin *gitrepotest.Origin, branch string) {
	t.Helper()
	hook := filepath.Join(origin.Dir(), "hooks", "update")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n[ \"$1\" != refs/heads/"+branch+" ]\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// recordingRevoker is the token revoker of the removal step: it answers the
// token tok1 for the fork demo--t1 and records the slugs it was asked for.
type recordingRevoker struct {
	mu    sync.Mutex
	asked [][]string
}

func (r *recordingRevoker) RevokeBoundTo(_ context.Context, _ *sql.Tx, slugs []string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, slices.Clone(slugs))
	if slices.Contains(slugs, "demo--t1") {
		return []string{"tok1"}, nil
	}
	return []string{}, nil
}

func TestWorkspaceRetire_ADeveloperArchivesAWorkspaceAndReactivatesIt(t *testing.T) {
	origin := pushOrigin(t)
	h := startWorkspaceHub(t, origin, "demo")
	h.createReady(t, "demo", origin.URL())
	landed := advance(t, filepath.Join(h.root, "demo.git"), "main", "landed in the hub")

	archived := h.must(t, "workspace", "archive", "demo").json(t)

	if archived["status"] != "archived" {
		t.Errorf("the archive printed status %v, want archived", archived["status"])
	}
	if got := origin.Head("main"); got != landed {
		t.Errorf("origin's main = %s, want the hub's %s", got, landed)
	}
	if w := h.record(t, "demo"); w.Status != workspace.StatusArchived {
		t.Errorf("the stored status after the archive is %s, want archived", w.Status)
	}
	if events := h.retireEvents(t, "parley.workspace.archive"); len(events) != 1 || events[0].OriginPush != "ok" {
		t.Errorf("archive events = %+v, want one with origin_push ok", events)
	}
	asked := origin.Requests()

	reactivated := h.must(t, "workspace", "reactivate", "demo", "--wait", "--poll-interval", "1s").json(t)

	// The kept copy's main, which the archive offered to origin, is the head
	// the workspace had before the archive.
	if reactivated["status"] != "ready" || reactivated["head_sha"] != landed {
		t.Errorf("the reactivate printed status %v and head_sha %v, want ready and %s", reactivated["status"], reactivated["head_sha"], landed)
	}
	if n := origin.Requests() - asked; n != 0 {
		t.Errorf("origin got %d requests between the reactivate and ready, want none", n)
	}
	if events := h.events(t, "parley.workspace.reactivate", "demo"); len(events) != 1 {
		t.Errorf("reactivate events of demo = %d, want 1", len(events))
	}
}

func TestWorkspaceRetire_ADeveloperDeletesAnArchivedWorkspaceWithAForkAndRegistersTheSlugAgain(t *testing.T) {
	origin := pushOrigin(t)
	revoker := &recordingRevoker{}
	h := startWorkspaceHub(t, origin, "demo", func(c *hub.Composition) {
		c.Workspaces().Teardown().RegisterTokenRevoker(revoker)
	})
	h.createReady(t, "demo", origin.URL())
	parent := h.record(t, "demo")
	ctx := context.Background()
	err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		return workspace.Insert(ctx, tx, workspace.Workspace{
			Slug: "demo--t1", Kind: access.KindFork, Mode: workspace.ModeStandard, ParentSlug: new("demo"),
			OwnerID: parent.OwnerID, OrgID: parent.OrgID, Status: workspace.StatusReady,
			CreatedAt: parent.CreatedAt, UpdatedAt: parent.CreatedAt, InstanceID: workspace.NewInstanceID(),
		})
	})
	if err != nil {
		t.Fatalf("insert the fork demo--t1: %v", err)
	}
	copyDir := filepath.Join(h.root, "demo.git")
	// A branch only the hub holds, which origin refuses, so that it is lost
	// with the copy.
	gitrepotest.Git(t, copyDir, "branch", "local-only", "main")
	advance(t, copyDir, "local-only", "only in the hub")
	refuseBranch(t, origin, "local-only")
	h.must(t, "workspace", "archive", "demo")

	deleted := h.must(t, "workspace", "delete", "demo", "--confirm").json(t)

	if want := map[string]any{"status": "deleted", "slug": "demo"}; !reflect.DeepEqual(deleted, want) {
		t.Errorf("the delete printed %v, want %v", deleted, want)
	}
	for _, slug := range []string{"demo", "demo--t1"} {
		if _, found, err := workspace.BySlug(ctx, h.Store.Main(), slug); found || err != nil {
			t.Errorf("the record of %s after the delete: found %v, %v; want none", slug, found, err)
		}
	}
	if _, err := os.Lstat(copyDir); !os.IsNotExist(err) {
		t.Errorf("the copy %s after the delete: %v, want it gone", copyDir, err)
	}
	events := h.retireEvents(t, "parley.workspace.delete")
	if len(events) != 2 {
		t.Fatalf("delete events = %+v, want two", events)
	}
	if e := events[0]; e.Workspace != "demo--t1" || e.Kind != "fork" || !reflect.DeepEqual(e.RevokedTokens, []string{"tok1"}) {
		t.Errorf("the first delete event = %+v, want demo--t1, kind fork, revoked_tokens [tok1]", e)
	}
	if e := events[1]; e.Workspace != "demo" || e.Kind != "repository" || !reflect.DeepEqual(e.DeletedForks, []string{"demo--t1"}) {
		t.Errorf("the second delete event = %+v, want demo, kind repository, deleted_forks [demo--t1]", e)
	}
	revoker.mu.Lock()
	asked := slices.Clone(revoker.asked)
	revoker.mu.Unlock()
	if want := [][]string{{"demo--t1"}, {"demo"}}; !reflect.DeepEqual(asked, want) {
		t.Errorf("the token revoker was asked for %v, want %v", asked, want)
	}
	if events := h.events(t, "parley.workspace.create", "demo"); len(events) != 1 {
		t.Errorf("create events of demo after the delete = %d, want the first one kept", len(events))
	}

	created := h.must(t, "workspace", "create", "--slug", "demo", "--git-url", origin.URL(), "--git-pat", syncToken,
		"--wait", "--poll-interval", "1s").json(t)

	if created["status"] != "ready" {
		t.Errorf("the new create printed status %v, want ready", created["status"])
	}
	if got, want := gitrepotest.Refs(t, copyDir, "refs/heads/"), gitrepotest.Refs(t, origin.Dir(), "refs/heads/"); got["main"] == "" || !reflect.DeepEqual(got, want) {
		t.Errorf("the new copy's branches = %v, want origin's %v alone", got, want)
	}
}

func TestWorkspaceRetire_ADeveloperReclonesAWorkspaceFromTheShell(t *testing.T) {
	origin := pushOrigin(t)
	originStale := origin.Branch("stale", "main")
	h := startWorkspaceHub(t, origin, "demo")
	h.createReady(t, "demo", origin.URL())
	copyDir := filepath.Join(h.root, "demo.git")
	gitrepotest.Git(t, copyDir, "branch", "feat", "main")
	feat := advance(t, copyDir, "feat", "feat in the hub")
	// The hub's stale is rewritten from main's parent, so it does not
	// descend from origin's stale and origin refuses it.
	base := gitrepotest.Git(t, copyDir, "rev-parse", "refs/heads/main~1")
	rewritten := gitrepotest.Git(t, copyDir, "commit-tree", base+"^{tree}", "-p", base, "-m", "stale rewritten in the hub")
	gitrepotest.Git(t, copyDir, "update-ref", "refs/heads/stale", rewritten)
	proxy := newRecordingProxy(t, h.URL(""))
	asked := origin.Requests()

	// The last --endpoint-url given wins, so this run goes through the
	// proxy, which records anything sent to the hub.
	unconfirmed := h.run(t, "workspace", "reclone", "demo", "--endpoint-url", proxy.URL)

	if unconfirmed.code != 2 || unconfirmed.errorType(t) != "usage_error" {
		t.Errorf("reclone without --confirm exited %d with stderr %q, want 2 and usage_error", unconfirmed.code, unconfirmed.stderr)
	}
	if sent := proxy.recorded(); len(sent) != 0 || origin.Requests() != asked {
		t.Errorf("reclone without --confirm sent %v to the hub and %d requests to origin, want none", sent, origin.Requests()-asked)
	}

	recloned := h.must(t, "workspace", "reclone", "demo", "--confirm", "--wait", "--poll-interval", "1s").json(t)

	if recloned["status"] != "ready" {
		t.Errorf("the reclone printed status %v, want ready", recloned["status"])
	}
	if got := origin.Head("feat"); got != feat {
		t.Errorf("origin's feat = %s, want the hub's former %s", got, feat)
	}
	if got := origin.Head("stale"); got != originStale {
		t.Errorf("origin's stale = %s, want its own %s", got, originStale)
	}
	copied := gitrepotest.Refs(t, copyDir, "refs/heads/")
	if copied["stale"] != originStale || copied["feat"] != feat {
		t.Errorf("the new copy's stale %s and feat %s, want origin's %s and the accepted %s",
			copied["stale"], copied["feat"], originStale, feat)
	}
	if events := h.retireEvents(t, "parley.workspace.reclone"); len(events) != 1 || events[0].OriginPush != "partial" {
		t.Errorf("reclone events = %+v, want one with origin_push partial", events)
	}
}
