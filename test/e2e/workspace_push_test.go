package e2e

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// pushEventType is the event every push handed to origin writes.
const pushEventType = "parley.workspace.push"

// pushEvent is the metadata of a push event this file reads.
type pushEvent struct {
	Branch    string `json:"branch"`
	OK        bool   `json:"ok"`
	Result    string `json:"result"`
	ErrorType string `json:"error_type"`
}

// pushEvents are the push events of the workspace slug for branch, oldest
// first.
func (h *workspaceHub) pushEvents(t *testing.T, slug, branch string) []pushEvent {
	t.Helper()
	var events []pushEvent
	for _, m := range h.events(t, pushEventType, slug) {
		var e pushEvent
		if err := json.Unmarshal([]byte(m), &e); err != nil {
			t.Fatalf("the push event's metadata %s: %v", m, err)
		}
		if e.Branch == branch {
			events = append(events, e)
		}
	}
	return events
}

// pushOrigin is an origin with the history of newOrigin that takes pushes
// from a client with the token of the sync tests.
func pushOrigin(t *testing.T) *gitrepotest.Origin {
	t.Helper()
	o := gitrepotest.SmartOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", syncToken))
	o.History()
	return o
}

func TestWorkspacePush_ADeveloperPublishesALandedBranchFromTheShell(t *testing.T) {
	origin := pushOrigin(t)
	h := startWorkspaceHub(t, origin, "demo")
	h.createReady(t, "demo", origin.URL())
	landed := advance(t, filepath.Join(h.root, "demo.git"), "main", "landed in the hub")

	pushed := h.must(t, "workspace", "push", "demo", "main").json(t)

	if pushed["result"] != "pushed" || pushed["sha"] != landed {
		t.Errorf("the push answered result %v and sha %v, want pushed and %s", pushed["result"], pushed["sha"], landed)
	}
	if got := origin.Head("main"); got != landed {
		t.Errorf("origin's main = %s, want the pushed %s", got, landed)
	}
	// The commit was added on disk, past the hub's record keeping, so the
	// record's head_sha still names the clone's head until a sync; the
	// hub's head of main is read from its copy instead.
	hubHead := gitrepotest.Git(t, filepath.Join(h.root, "demo.git"), "rev-parse", "refs/heads/main")
	if read := h.must(t, "workspace", "get", "demo").json(t); read["origin_head_sha"] != hubHead {
		t.Errorf("the workspace shows origin_head_sha %v, want the hub's head of main %s", read["origin_head_sha"], hubHead)
	}
	if events := h.pushEvents(t, "demo", "main"); len(events) != 1 || !events[0].OK || len(h.events(t, pushEventType, "demo")) != 1 {
		t.Errorf("push events of demo = %+v, want exactly one, ok", events)
	}
}

func TestWorkspacePush_AnAgentsForcedPushIsStoppedByTheLeaseAndSucceedsAfterASync(t *testing.T) {
	origin := pushOrigin(t)
	origin.Branch("feature", "main")
	h := startWorkspaceHub(t, origin, "demo")
	h.createReady(t, "demo", origin.URL())
	hub := filepath.Join(h.root, "demo.git")
	// The hub's feature is rewritten from main's parent, so it does not
	// descend from origin's feature.
	base := gitrepotest.Git(t, hub, "rev-parse", "refs/heads/main~1")
	rewritten := gitrepotest.Git(t, hub, "commit-tree", base+"^{tree}", "-p", base, "-m", "feature rewritten in the hub")
	gitrepotest.Git(t, hub, "update-ref", "refs/heads/feature", rewritten)
	onOrigin := origin.Commit("feature", "feature moves on origin")
	pat := h.IssuePAT(t, identity.PATSpec{UserID: h.user, Permissions: []string{"workspaces:write"}})
	forced := map[string]any{"force": true}

	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/branches/feature/push", pat, forced)
	var refusal struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil || status != http.StatusConflict || refusal.Error.ErrorType != "origin_moved" {
		t.Errorf("the forced push before the sync = %d %s, want 409 origin_moved", status, body)
	}
	if got := origin.Head("feature"); got != onOrigin {
		t.Errorf("after the refused push origin's feature = %s, want its own %s", got, onOrigin)
	}

	if status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/sync", pat, nil); status != http.StatusOK {
		t.Fatalf("sync = %d %s, want 200", status, body)
	}
	status, body = h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/branches/feature/push", pat, forced)
	answer := decoded(t, body)
	if status != http.StatusOK || answer["result"] != "forced" || answer["sha"] != rewritten {
		t.Errorf("the forced push after the sync = %d %s, want 200 forced at %s", status, body, rewritten)
	}
	if got := origin.Head("feature"); got != rewritten {
		t.Errorf("after the forced push origin's feature = %s, want the hub's %s", got, rewritten)
	}

	want := []pushEvent{
		{Branch: "feature", OK: false, ErrorType: "origin_moved"},
		{Branch: "feature", OK: true, Result: "forced"},
	}
	events := h.pushEvents(t, "demo", "feature")
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Errorf("push events of feature = %+v, want %+v", events, want)
	}
}
