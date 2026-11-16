package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/hub"
)

// The agent sessions of the two agents. The event log takes a session id
// only as a UUID.
const (
	sessionA = "a0000000-0000-4000-8000-00000000000a"
	sessionB = "b0000000-0000-4000-8000-00000000000b"
)

// knownSessions stands in for the session feature, which the hub does not
// have yet: it accepts the session ids it holds, for a change and for a
// claim, and records nothing.
type knownSessions map[string]bool

// check is id in lower case when s holds it, else an error matching
// unknown.
func (s knownSessions) check(id string, unknown error) (string, error) {
	id = strings.ToLower(id)
	if !s[id] {
		return "", fmt.Errorf("session %s: %w", id, unknown)
	}
	return id, nil
}

// changeSessions is knownSessions as the change component's session linker.
type changeSessions knownSessions

func (s changeSessions) Check(_ context.Context, _ change.Caller, id, _ string) (string, error) {
	return knownSessions(s).check(id, change.ErrUnknownSession)
}

func (changeSessions) Record(context.Context, *sql.Tx, string, string, int64, int64) error { return nil }

// claimSessions is knownSessions as the awareness component's sessions.
type claimSessions knownSessions

func (s claimSessions) Check(_ context.Context, _ awareness.Actor, id, _ string) (string, error) {
	return knownSessions(s).check(id, awareness.ErrUnknownSession)
}

func (claimSessions) Record(context.Context, *sql.Tx, string, string, string) error { return nil }

// eventually checks cond every 50 ms until it holds, and fails the test
// when it does not hold within d.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, d)
		}
	}
}

// getOK is the JSON object GET path answers the credential with, failing
// the test unless the answer is 200.
func (h *forkHub) getOK(t *testing.T, credential, path string) map[string]any {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, path, credential, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d %s, want 200", path, status, body)
	}
	return decoded(t, body)
}

// objects is the list at member of v, each item a JSON object.
func objects(v map[string]any, member string) []map[string]any {
	items := []map[string]any{}
	raw, _ := v[member].([]any)
	for _, item := range raw {
		m, _ := item.(map[string]any)
		items = append(items, m)
	}
	return items
}

// changeNumbers are the numbers of the changes of the overlap entries.
func changeNumbers(entries []map[string]any) []float64 {
	numbers := []float64{}
	for _, e := range entries {
		c, _ := e["change"].(map[string]any)
		n, _ := c["number"].(float64)
		numbers = append(numbers, n)
	}
	return numbers
}

// loggedRecord is an event of the hub's log as GET /api/v1/events/log
// serves it.
type loggedRecord struct {
	Type         string          `json:"type"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Metadata     json.RawMessage `json:"metadata"`
}

// logOf are the events of demo of the types given in the hub's log,
// oldest first, as the admin token reads them.
func (h *forkHub) logOf(t *testing.T, types ...string) []loggedRecord {
	t.Helper()
	path := "/api/v1/events/log?" + url.Values{"after": {"0"}, "workspace": {"demo"}, "types": {strings.Join(types, ",")}}.Encode()
	status, body := h.Call(t, http.MethodGet, path, h.Token, nil)
	var page struct {
		Items []loggedRecord `json:"items"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &page) != nil {
		t.Fatalf("GET %s = %d %s, want 200 with a page", path, status, body)
	}
	return page.Items
}

// Two agents start work on the same file: the second claims the paths and
// sees the first one's change, opens its own change on the claimed,
// overlapping file as it would without either, and each change is told
// about the other; the first agent's check of its overlaps from the shell
// fails.
func TestAwareness_TwoAgentsOnOneFileAreEachToldAboutTheOther(t *testing.T) {
	parallel(t)
	sessions := knownSessions{sessionA: true, sessionB: true}
	h := startForkHubWith(t, nil, func(c *hub.Composition) {
		c.Changes().RegisterSessionLinker(changeSessions(sessions))
		c.Awareness().RegisterSessions(claimSessions(sessions))
	})
	agentA, agentB := h.key, h.memberKey
	// The first agent works in demo, which its user owns; the second, whose
	// user is a member of the organization, in its own fork of demo.
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", agentB, map[string]any{"name": "b", "auto_change": false})
	if status != http.StatusCreated {
		t.Fatalf("create the second agent's fork = %d %s, want 201", status, body)
	}
	forkB := str(decoded(t, body), "workspace", "slug")
	for _, w := range []struct{ credential, slug, branch string }{{agentA, "demo", "feat-a"}, {agentB, forkB, "feat-b"}} {
		run := cloneAs(t, h, w.credential, w.slug)
		run("checkout", "--quiet", "-b", w.branch, "main")
		if err := os.MkdirAll(filepath.Join(strings.TrimSpace(run("rev-parse", "--show-toplevel")), "api"), 0o700); err != nil {
			t.Fatal(err)
		}
		commitFile(t, run, "api/handler.go", "package api // "+w.branch+"\n", "Rework the handler on "+w.branch)
		run("push", "--quiet", "origin", w.branch)
	}
	open := func(credential, source, branch, session string) (int, []byte) {
		t.Helper()
		return h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/changes", credential, map[string]any{
			"source": map[string]any{"workspace_slug": source, "branch": branch}, "session_id": session, "intent": "Rework the handlers.",
		})
	}

	if status, body := open(agentA, "demo", "feat-a", sessionA); status != http.StatusCreated {
		t.Fatalf("open change 1 from feat-a = %d %s, want 201", status, body)
	}
	activity := "/api/v1/workspaces/demo/activity?paths=" + url.QueryEscape("api/**")
	eventually(t, 10*time.Second, "activity on api/** listing change 1", func() bool {
		return slices.Equal(changeNumbers(objects(h.getOK(t, agentA, activity), "changes")), []float64{1})
	})

	r := h.runAs(t, agentB, "claim", "create", "demo", "--paths", "api/**", "--intent", "rework handlers", "--session", sessionB)
	if r.code != 0 {
		t.Fatalf("parley claim create exited %d, stderr %q; want 0", r.code, r.stderr)
	}
	created := r.json(t)
	claimID := str(created, "claim", "id")
	activityOfClaim, _ := created["activity"].(map[string]any)
	if got := changeNumbers(objects(activityOfClaim, "changes")); !slices.Equal(got, []float64{1}) {
		t.Errorf("the claim's activity lists changes %v, want [1]", got)
	}

	status, body = open(agentB, forkB, "feat-b", sessionB)
	if status != http.StatusCreated {
		t.Fatalf("open change 2 from feat-b on the claimed, overlapping file = %d %s, want 201", status, body)
	}
	if c := decoded(t, body); c["number"] != float64(2) || c["state"] != "open" || c["version"] != float64(1) {
		t.Errorf("change 2 opened as number %v, state %v, version %v; want 2, open, 1", c["number"], c["state"], c["version"])
	}

	var overlaps []loggedRecord
	eventually(t, 10*time.Second, "two parley.change.overlap events", func() bool {
		overlaps = h.logOf(t, "parley.change.overlap")
		return len(overlaps) >= 2
	})
	if len(overlaps) != 2 {
		t.Fatalf("the log holds %d parley.change.overlap events, want 2", len(overlaps))
	}
	named := map[string]float64{}
	for _, ev := range overlaps {
		var meta struct {
			With struct {
				Number float64 `json:"number"`
			} `json:"with"`
		}
		if err := json.Unmarshal(ev.Metadata, &meta); err != nil || ev.ResourceType != "change" {
			t.Fatalf("overlap event on %s %s with metadata %s, want a change and a with", ev.ResourceType, ev.ResourceID, ev.Metadata)
		}
		named[ev.ResourceID] = meta.With.Number
	}
	if want := map[string]float64{"1": 2, "2": 1}; fmt.Sprint(named) != fmt.Sprint(want) {
		t.Errorf("overlap events name %v (resource id: with.number), want %v", named, want)
	}

	claim := h.getOK(t, agentB, "/api/v1/workspaces/demo/claims/"+claimID)
	if claim["change"] != float64(2) || claim["state"] != "active" {
		t.Errorf("the claim reads change %v, state %v; want 2 and active", claim["change"], claim["state"])
	}

	o := h.runAs(t, agentA, "change", "overlaps", "demo", "1", "--fail-on-overlap")
	if o.code != 3 {
		t.Errorf("parley change overlaps demo 1 --fail-on-overlap exited %d, stderr %q; want 3", o.code, o.stderr)
	}
	entries := objects(o.json(t), "items")
	if len(entries) != 1 || !slices.Equal(changeNumbers(entries), []float64{2}) || entries[0]["count"] != float64(1) ||
		fmt.Sprint(entries[0]["paths"]) != "[api/handler.go]" {
		t.Errorf("the overlaps of change 1 printed %v, want one entry of change 2 with count 1 and paths [api/handler.go]", entries)
	}
	ofTwo := objects(h.getOK(t, agentB, "/api/v1/workspaces/demo/changes/2/overlaps"), "items")
	if got := changeNumbers(ofTwo); !slices.Equal(got, []float64{1}) {
		t.Errorf("the overlaps of change 2 list changes %v, want [1]", got)
	}
	if creates := h.logOf(t, "parley.claim.create"); len(creates) != 1 || creates[0].ResourceID != claimID {
		t.Errorf("the log holds the claim creations %v, want one of %s", creates, claimID)
	}
}

// An agent claims paths and stops without releasing the claim: the claim
// expires by itself once its time has passed, leaves the activity of its
// paths, and is removed a week later.
func TestAwareness_ACrashedAgentsClaimExpiresAndIsRemoved(t *testing.T) {
	parallel(t)
	clock := &fakeNow{now: time.Now().Truncate(time.Second)}
	h := startForkHubWith(t, clock.Now, func(c *hub.Composition) {
		c.AdjustAwareness(func(d *awareness.Deps) { d.ExpiryInterval = 50 * time.Millisecond })
	})
	status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/claims", h.key, map[string]any{
		"paths": []string{"docs/**"}, "intent": "write docs", "ttl": "5m",
	})
	if status != http.StatusCreated {
		t.Fatalf("POST claims = %d %s, want 201", status, body)
	}
	claimID := str(decoded(t, body), "claim", "id")
	claimPath := "/api/v1/workspaces/demo/claims/" + claimID
	activity := "/api/v1/workspaces/demo/activity?paths=" + url.QueryEscape("docs/**")
	claimIDs := func() []string {
		ids := []string{}
		for _, cl := range objects(h.getOK(t, h.key, activity), "claims") {
			ids = append(ids, str(cl, "id"))
		}
		return ids
	}
	if got := claimIDs(); !slices.Equal(got, []string{claimID}) {
		t.Fatalf("activity on docs/** lists the claims %v, want [%s]", got, claimID)
	}

	clock.advance(6 * time.Minute)
	ofClaim := func() []string {
		types := []string{}
		for _, ev := range h.logOf(t, "parley.claim.create", "parley.claim.expire") {
			if ev.ResourceType == "claim" && ev.ResourceID == claimID {
				types = append(types, ev.Type)
			}
		}
		return types
	}
	var types []string
	eventually(t, 10*time.Second, "the expiry run's parley.claim.expire", func() bool {
		types = ofClaim()
		return slices.Contains(types, "parley.claim.expire")
	})
	if want := []string{"parley.claim.create", "parley.claim.expire"}; !slices.Equal(types, want) {
		t.Errorf("the claim's events = %v, want %v", types, want)
	}
	if state := h.getOK(t, h.key, claimPath)["state"]; state != "expired" {
		t.Errorf("the claim reads state %v, want expired", state)
	}
	if got := claimIDs(); len(got) != 0 {
		t.Errorf("activity on docs/** lists the claims %v after the expiry, want none", got)
	}

	clock.advance(7 * 24 * time.Hour)
	// A start of the hub runs a maintenance pass, claim_cleanup among its
	// tasks.
	h.Restart(t)
	eventually(t, 10*time.Second, "the removal of the claim", func() bool {
		status, _ := h.Call(t, http.MethodGet, claimPath, h.key, nil)
		return status == http.StatusNotFound
	})
	status, body = h.Call(t, http.MethodGet, claimPath, h.key, nil)
	if status != http.StatusNotFound || str(decoded(t, body), "error", "error_type") != "claim_not_found" {
		t.Errorf("GET the claim a week after it expired = %d %s, want 404 claim_not_found", status, body)
	}
}
