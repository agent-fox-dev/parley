package carrypatch_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// addHub is a readHub whose clock is the test's: every add stamps the
// patch with now.
type addHub struct {
	*readHub
	mu  sync.Mutex
	now time.Time
}

// startAddHub starts a hub whose clock stands at a time with a fraction
// of a second, so that an answer's whole-second time shows it was cut.
func startAddHub(t *testing.T) *addHub {
	t.Helper()
	h := &addHub{readHub: &readHub{}, now: time.Now().UTC().Truncate(time.Second).Add(700 * time.Millisecond)}
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Now: h.clock,
		Compose: func(c *hub.Composition) {
			c.AdjustCarryPatch(func(d *carrypatch.Deps) { h.repoPath = d.RepoPath })
		},
	})
	h.owner = h.CreateUser(t, "owner")
	h.key = h.IssueKey(t, h.owner)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, h.owner)
	return h
}

func (h *addHub) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

// advance moves the hub's clock on by d.
func (h *addHub) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

// carry stores the ready carry-patch workspace slug of the owner, changed
// by edit when it is not nil, with a bare copy made by git init that holds
// the branches p1 to p4, and returns the copy's directory.
func (h *addHub) carry(t *testing.T, slug string, edit func(w *workspace.Workspace)) string {
	t.Helper()
	now := time.Now().UTC()
	w := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: workspace.ModeCarryPatch, GitURL: new("https://origin.test/" + slug + ".git"),
		DefaultBranch: new("main"), OwnerID: h.owner, OrgID: h.org, Status: workspace.StatusReady, DisplayName: slug,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
	}
	if edit != nil {
		edit(&w)
	}
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, w) }); err != nil {
		t.Fatalf("store workspace %s: %v", slug, err)
	}
	dir := h.repoPath(slug)
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	for _, b := range []string{"p1", "p2", "p3", "p4"} {
		gitrepotest.Git(t, dir, "update-ref", "refs/heads/"+b, commitIn(t, dir, b))
	}
	return dir
}

// post sends POST /workspaces/<slug>/patches as the owner with body and the
// headers header, and decodes the answer.
func (h *addHub) post(t *testing.T, slug string, body any, header http.Header) answer {
	t.Helper()
	return h.postAs(t, h.key, slug, body, header)
}

func (h *addHub) postAs(t *testing.T, credential, slug string, body any, header http.Header) answer {
	t.Helper()
	status, raw := h.CallWith(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/patches", credential, body, header)
	a := answer{status: status, raw: raw}
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(raw, &a.patch)
	return a
}

// mustAdd posts body to slug and fails the test unless the answer is 201.
func (h *addHub) mustAdd(t *testing.T, slug string, body map[string]any) answer {
	t.Helper()
	a := h.post(t, slug, body, nil)
	if a.status != http.StatusCreated {
		t.Fatalf("POST %v to %s = %d %s, want 201", body, slug, a.status, a.raw)
	}
	return a
}

// order is the branches of the patches of slug that are not deleted, in
// position order, after checking that they hold the positions 1..N.
func (h *addHub) order(t *testing.T, slug string) []string {
	t.Helper()
	live, err := carrypatch.ListLivePatches(context.Background(), h.Store.Main(), slug)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i, p := range live {
		if p.Position == nil || *p.Position != i+1 {
			t.Errorf("the patch %s of %s is at %v, want %d: the positions must be 1..N", p.BranchName, slug, p.Position, i+1)
		}
		out = append(out, p.BranchName)
	}
	return out
}

// count is the number of patch rows of slug, deleted ones included.
func (h *addHub) count(t *testing.T, slug string) int {
	t.Helper()
	var n int
	if err := h.Store.Main().QueryRow(`SELECT COUNT(*) FROM patches WHERE workspace_slug = ?`, slug).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// patchEvent is a stored parley.patch.create.
type patchEvent struct {
	ResourceType, ResourceID, ActorType string
	ActorID                             *string
	Metadata                            map[string]any
}

// events are the stored events of slug, of every type, oldest first; the
// parley.patch.create among them are decoded.
func (h *addHub) events(t *testing.T, slug string) (all int, created []patchEvent) {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT type, resource_type, resource_id, actor_type, actor_id, metadata
FROM hub_events WHERE workspace = ? ORDER BY seq`, slug)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, meta string
		var e patchEvent
		if err := rows.Scan(&typ, &e.ResourceType, &e.ResourceID, &e.ActorType, &e.ActorID, &meta); err != nil {
			t.Fatal(err)
		}
		all++
		if typ == "parley.patch.create" {
			if err := json.Unmarshal([]byte(meta), &e.Metadata); err != nil {
				t.Fatal(err)
			}
			created = append(created, e)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return all, created
}

// lastEvent is the latest parley.patch.create of slug.
func (h *addHub) lastEvent(t *testing.T, slug string) patchEvent {
	t.Helper()
	_, created := h.events(t, slug)
	if len(created) == 0 {
		t.Fatalf("%s has no parley.patch.create", slug)
	}
	return created[len(created)-1]
}

func TestAdd_SingleCreatesActivePatch(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	h.carry(t, "carry", nil)
	stamp := h.clock().Truncate(time.Second).Format(time.RFC3339)

	key := http.Header{"Idempotency-Key": {"first-add"}}
	first := map[string]any{"branch_name": "p1", "description": "first", "upstream_pr_url": ""}
	r := h.post(t, "carry", first, key)
	if r.status != http.StatusCreated {
		t.Fatalf("POST p1 = %d %s, want 201", r.status, r.raw)
	}
	id, _ := r.patch["id"].(string)
	if _, err := uuid.Parse(id); err != nil || len(r.patch) != 14 {
		t.Errorf("the patch has id %q and %d members, want a UUID and 14: %s", id, len(r.patch), r.raw)
	}
	want := map[string]any{
		"workspace_slug": "carry", "branch_name": "p1", "position": float64(1), "status": "active", "conflict_files": []any{},
		"description": "first", "upstream_pr_url": nil, "added_at": stamp, "updated_at": stamp,
		"deleted_at": nil, "origin_sync_state": nil, "origin_sha": nil, "origin_synced_at": nil,
	}
	for member, v := range want {
		if got, ok := r.patch[member]; !ok || fmt.Sprint(got) != fmt.Sprint(v) {
			t.Errorf("%s = %v (present %v), want %v", member, got, ok, v)
		}
	}
	e := h.lastEvent(t, "carry")
	if e.ResourceType != "patch" || e.ResourceID != id || e.ActorType != "api_key" || str(e.ActorID) != h.owner {
		t.Errorf("the event is %+v (actor %s), want resource patch %s by the owner's api_key", e, str(e.ActorID), id)
	}
	if wantMeta := map[string]any{"branch_name": "p1", "position": float64(1), "branch_resolution": "local"}; fmt.Sprint(e.Metadata) != fmt.Sprint(wantMeta) {
		t.Errorf("the event's metadata = %v, want %v", e.Metadata, wantMeta)
	}

	if a := h.mustAdd(t, "carry", map[string]any{"branch_name": "p2"}); a.patch["position"] != float64(2) {
		t.Errorf("POST p2 has position %v, want 2", a.patch["position"])
	}
	replay := h.post(t, "carry", first, key)
	if replay.status != http.StatusOK || !bytes.Equal(replay.raw, r.raw) {
		t.Errorf("the replay = %d %s, want 200 with the first body %s", replay.status, replay.raw, r.raw)
	}
	if n := h.count(t, "carry"); n != 2 {
		t.Errorf("after the replay there are %d patches, want 2", n)
	}
}

func TestAdd_RejectsInvalidBody(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	dir := h.carry(t, "carry", nil)
	integ := h.carry(t, "integ", func(w *workspace.Workspace) { w.IntegrationBranch = new("integration") })
	refs, integRefs := gitrepotest.Refs(t, dir, "refs/"), gitrepotest.Refs(t, integ, "refs/")
	cases := []struct {
		name, slug string
		body       any
	}{
		{"a JSON string", "carry", `"x"`},
		{"a JSON number", "carry", `7`},
		{"no branch_name", "carry", map[string]any{}},
		{"a null branch_name", "carry", map[string]any{"branch_name": nil}},
		{"an empty branch_name", "carry", map[string]any{"branch_name": ""}},
		{"a branch_name with ..", "carry", map[string]any{"branch_name": "a..b"}},
		{"a branch_name with a leading dash", "carry", map[string]any{"branch_name": "-x"}},
		{"the default integration branch", "carry", map[string]any{"branch_name": "deploy"}},
		{"the stored integration branch", "integ", map[string]any{"branch_name": "integration"}},
		{"position 0", "carry", map[string]any{"branch_name": "p1", "position": 0}},
		{"position -1", "carry", map[string]any{"position": -1}},
		{"position 1.5", "carry", map[string]any{"branch_name": "p1", "position": 1.5}},
		{"position as a string", "carry", map[string]any{"branch_name": "p1", "position": "2"}},
		{"skip_branch_check as a string", "carry", map[string]any{"branch_name": "p1", "skip_branch_check": "yes"}},
		{"if_not_exists as a number", "carry", map[string]any{"branch_name": "p1", "if_not_exists": 1}},
		{"description as a number", "carry", map[string]any{"branch_name": "p1", "description": 5}},
		{"upstream_pr_url as an object", "carry", map[string]any{"branch_name": "p1", "upstream_pr_url": map[string]any{}}},
		{"branch_name as a number", "carry", map[string]any{"branch_name": 5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if a := h.post(t, c.slug, c.body, nil); a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" {
				t.Errorf("POST %v = %d %s, want 400 invalid_request", c.body, a.status, a.raw)
			}
		})
	}
	for slug, before := range map[string]map[string]string{"carry": refs, "integ": integRefs} {
		if n := h.count(t, slug); n != 0 {
			t.Errorf("%s has %d patches, want none", slug, n)
		}
		if after := gitrepotest.Refs(t, h.repoPath(slug), "refs/"); fmt.Sprint(after) != fmt.Sprint(before) {
			t.Errorf("the refs of %s are %v, want them unchanged: %v", slug, after, before)
		}
		if all, _ := h.events(t, slug); all != 0 {
			t.Errorf("%s has %d events, want none", slug, all)
		}
	}

	t.Run("unknown members are ignored and null is absent", func(t *testing.T) {
		h.mustAdd(t, "carry", map[string]any{"branch_name": "p1", "unknown_member": 1})
		a := h.mustAdd(t, "carry", map[string]any{"branch_name": "p2", "position": nil, "description": nil,
			"upstream_pr_url": nil, "skip_branch_check": nil, "if_not_exists": nil})
		if a.patch["position"] != float64(2) || a.patch["description"] != nil {
			t.Errorf("POST p2 with null members = %s, want position 2 and description null", a.raw)
		}
	})
}

func TestAdd_Position(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	cases := []struct {
		name     string
		start    []string
		branch   string
		position any // nil: none given
		want     []string
		wantPos  int
	}{
		{"at 1", []string{"p1", "p2", "p3"}, "p4", 1, []string{"p4", "p1", "p2", "p3"}, 1},
		{"at 2", []string{"p1", "p2", "p3"}, "p4", 2, []string{"p1", "p4", "p2", "p3"}, 2},
		{"at 3", []string{"p1", "p2", "p3"}, "p4", 3, []string{"p1", "p2", "p4", "p3"}, 3},
		{"at N+1", []string{"p1", "p2", "p3"}, "p4", 4, []string{"p1", "p2", "p3", "p4"}, 4},
		{"past N+1", []string{"p1", "p2", "p3"}, "p4", 5, []string{"p1", "p2", "p3", "p4"}, 4},
		{"far past N+1", []string{"p1", "p2", "p3"}, "p4", 900, []string{"p1", "p2", "p3", "p4"}, 4},
		{"past every int", []string{"p1", "p2", "p3"}, "p4", json.Number("99999999999999999999999"), []string{"p1", "p2", "p3", "p4"}, 4},
		{"none", []string{"p1", "p2", "p3"}, "p4", nil, []string{"p1", "p2", "p3", "p4"}, 4},
		{"past the end of an empty list", nil, "p1", 3, []string{"p1"}, 1},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("pos%d", i)
			h.carry(t, slug, nil)
			for n, b := range c.start {
				h.put(t, livePatch(slug, b, n+1))
			}
			body := map[string]any{"branch_name": c.branch}
			if c.position != nil {
				body["position"] = c.position
			}
			a := h.mustAdd(t, slug, body)
			if a.patch["position"] != float64(c.wantPos) {
				t.Errorf("the answer's position = %v, want %d", a.patch["position"], c.wantPos)
			}
			if got := h.order(t, slug); !slices.Equal(got, c.want) {
				t.Errorf("the order is %v, want %v", got, c.want)
			}
			if got := h.lastEvent(t, slug).Metadata["position"]; got != float64(c.wantPos) {
				t.Errorf("the event's position = %v, want %d", got, c.wantPos)
			}
		})
	}
}

func TestAdd_ExistingBranchIsRefused(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	dir := h.carry(t, "carry", nil)
	h.mustAdd(t, "carry", map[string]any{"branch_name": "p1"})
	disabled := livePatch("carry", "p2", 2)
	disabled.Status = carrypatch.StatusDisabled
	h.put(t, disabled)
	// The branches are gone from the copy and origin still has them: a
	// resolution would create them again.
	for _, b := range []string{"p1", "p2"} {
		gitrepotest.Git(t, dir, "update-ref", "refs/remotes/origin/"+b, "refs/heads/"+b)
		gitrepotest.Git(t, dir, "update-ref", "-d", "refs/heads/"+b)
	}
	events, _ := h.events(t, "carry")
	for _, body := range []map[string]any{
		{"branch_name": "p1"},
		{"branch_name": "p1", "if_not_exists": false},
		{"branch_name": "p2"},
	} {
		a := h.post(t, "carry", body, nil)
		if a.status != http.StatusConflict || a.Error.ErrorType != "patch_exists" {
			t.Errorf("POST %v = %d %s, want 409 patch_exists", body, a.status, a.raw)
		}
		b := body["branch_name"].(string)
		if id := refIn(dir, "refs/heads/"+b); id != "" {
			t.Errorf("POST %v created refs/heads/%s at %s", body, b, id)
		}
	}
	if n := h.count(t, "carry"); n != 2 {
		t.Errorf("there are %d patches, want 2", n)
	}
	if after, _ := h.events(t, "carry"); after != events {
		t.Errorf("there are %d events, want %d", after, events)
	}
}

func TestAdd_IfNotExistsReturnsExisting(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	dir := h.carry(t, "carry", nil)
	first := h.mustAdd(t, "carry", map[string]any{"branch_name": "p1", "description": "first"})
	h.advance(time.Hour)
	refs := gitrepotest.Refs(t, dir, "refs/")
	events, _ := h.events(t, "carry")

	a := h.post(t, "carry", map[string]any{"branch_name": "p1", "if_not_exists": true, "description": "other", "position": 5}, nil)
	if a.status != http.StatusOK || fmt.Sprint(a.patch) != fmt.Sprint(first.patch) {
		t.Errorf("POST p1 if_not_exists = %d %s, want 200 with the stored patch %s", a.status, a.raw, first.raw)
	}
	if n := h.count(t, "carry"); n != 1 {
		t.Errorf("there are %d patches, want 1", n)
	}
	if after, _ := h.events(t, "carry"); after != events {
		t.Errorf("there are %d events, want %d", after, events)
	}
	if after := gitrepotest.Refs(t, dir, "refs/"); fmt.Sprint(after) != fmt.Sprint(refs) {
		t.Errorf("the refs are %v, want them unchanged: %v", after, refs)
	}
	if b := h.post(t, "carry", map[string]any{"branch_name": "p2", "if_not_exists": true}, nil); b.status != http.StatusCreated || b.patch["position"] != float64(2) {
		t.Errorf("POST p2 if_not_exists = %d %s, want 201 at position 2", b.status, b.raw)
	}
}

func TestAdd_ReplacesSoftDeletedPatch(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	// seed stores slug with the patch of p1 deleted and its backup ref.
	seed := func(slug string) (dir string, old carrypatch.Patch) {
		dir = h.carry(t, slug, nil)
		old = deletedPatch(slug, "p1", at(10, 0))
		h.put(t, old)
		gitrepotest.Git(t, dir, "update-ref", carrypatch.ReplacedRefPrefix+"p1", commitIn(t, dir, "replaced"))
		return dir, old
	}

	dir, old := seed("carry")
	a := h.mustAdd(t, "carry", map[string]any{"branch_name": "p1"})
	if a.patch["id"] == old.ID || a.patch["position"] != float64(1) || a.patch["status"] != "active" {
		t.Errorf("POST p1 = %s, want a new id (not %s) at position 1, active", a.raw, old.ID)
	}
	if g := h.get(t, "/workspaces/carry/patches/"+old.ID); g.status != http.StatusNotFound {
		t.Errorf("GET the removed patch = %d %s, want 404", g.status, g.raw)
	}
	if id := refIn(dir, carrypatch.ReplacedRefPrefix+"p1"); id != "" {
		t.Errorf("the backup ref of p1 is still at %s", id)
	}
	if l := h.get(t, "/workspaces/carry/patches?include_deleted=true"); !slices.Equal(l.branches(), []string{"p1"}) {
		t.Errorf("the list with the deleted patches is %v, want [p1]", l.branches())
	}

	t.Run("a backup ref that cannot be deleted is logged", func(t *testing.T) {
		dir, _ := seed("locked")
		// git deletes a ref only under its lock, which another writer holds.
		lock := filepath.Join(dir, carrypatch.ReplacedRefPrefix+"p1.lock")
		if err := os.WriteFile(lock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		h.mustAdd(t, "locked", map[string]any{"branch_name": "p1"})
		var lines []string
		for line := range strings.Lines(h.Log.String()) {
			if strings.Contains(line, `"locked"`) && strings.Contains(line, carrypatch.ReplacedRefPrefix+"p1") {
				lines = append(lines, line)
			}
		}
		if len(lines) != 1 {
			t.Errorf("the log has %d lines naming the backup ref of locked, want 1:\n%s", len(lines), h.Log.String())
		}
	})
}

func TestDeleteBackupRef_OutlivesTheRequest(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "carry.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	gitrepotest.Git(t, dir, "update-ref", carrypatch.ReplacedRefPrefix+"p1", commitIn(t, dir, "replaced"))
	comp := &carrypatch.Component{}
	comp.Bind(carrypatch.Deps{RepoPath: func(string) string { return dir }})
	// The client left right after the add's transaction committed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	carrypatch.DeleteBackupRef(ctx, comp, "carry", "p1")
	if id := refIn(dir, carrypatch.ReplacedRefPrefix+"p1"); id != "" {
		t.Errorf("the backup ref of p1 is still at %s after the request ended", id)
	}
}

func TestAdd_Concurrent(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	h.carry(t, "carry", nil)
	// race posts each body at once and returns the answers.
	race := func(bodies []map[string]any) []answer {
		out := make([]answer, len(bodies))
		var wg sync.WaitGroup
		for i, body := range bodies {
			wg.Go(func() { out[i] = h.post(t, "carry", body, nil) })
		}
		wg.Wait()
		return out
	}

	same := make([]map[string]any, 8)
	for i := range same {
		same[i] = map[string]any{"branch_name": "p1"}
	}
	created, refused := 0, 0
	for _, a := range race(same) {
		switch {
		case a.status == http.StatusCreated:
			created++
		case a.status == http.StatusConflict && a.Error.ErrorType == "patch_exists":
			refused++
		default:
			t.Errorf("a concurrent add of p1 = %d %s, want 201 or 409 patch_exists", a.status, a.raw)
		}
	}
	if created != 1 || refused != 7 {
		t.Errorf("of 8 concurrent adds of p1, %d answered 201 and %d 409, want 1 and 7", created, refused)
	}

	var different []map[string]any
	for i := 1; i <= 8; i++ {
		body := map[string]any{"branch_name": fmt.Sprintf("b%d", i), "skip_branch_check": true}
		if i%2 == 0 {
			body["position"] = 1
		}
		different = append(different, body)
	}
	for i, a := range race(different) {
		if a.status != http.StatusCreated {
			t.Errorf("the concurrent add of b%d = %d %s, want 201", i+1, a.status, a.raw)
		}
	}
	if got := h.order(t, "carry"); len(got) != 9 {
		t.Errorf("the list is %v, want 9 patches", got)
	}
}

func TestAdd_ResolutionRefusals(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	dir := h.carry(t, "carry", nil)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	h.carry(t, "from-origin", func(w *workspace.Workspace) {
		w.GitURL = new(gone.URL + "/repo.git")
		w.Settings = map[string]json.RawMessage{"patch_branch_source": json.RawMessage(`"origin"`)}
	})

	cases := []struct {
		slug       string
		body       map[string]any
		status     int
		errorType  string
		resolution string
	}{
		{"carry", map[string]any{"branch_name": "nowhere"}, http.StatusNotFound, "branch_not_found", ""},
		{"from-origin", map[string]any{"branch_name": "nowhere"}, http.StatusBadGateway, "origin_fetch_failed", ""},
		{"from-origin", map[string]any{"branch_name": "nowhere", "skip_branch_check": true}, http.StatusCreated, "", carrypatch.ResolutionSkipped},
		{"carry", map[string]any{"branch_name": "o1"}, http.StatusCreated, "", carrypatch.ResolutionOriginTracking},
	}
	gitrepotest.Git(t, dir, "update-ref", "refs/remotes/origin/o1", commitIn(t, dir, "o1"))
	for _, c := range cases {
		before := h.count(t, c.slug)
		events, _ := h.events(t, c.slug)
		a := h.post(t, c.slug, c.body, nil)
		if c.errorType != "" {
			if a.status != c.status || a.Error.ErrorType != c.errorType {
				t.Errorf("POST %v to %s = %d %s, want %d %s", c.body, c.slug, a.status, a.raw, c.status, c.errorType)
			}
			after, _ := h.events(t, c.slug)
			if n := h.count(t, c.slug); n != before || after != events {
				t.Errorf("after the refusal %s has %d patches and %d events, want %d and %d", c.slug, n, after, before, events)
			}
			continue
		}
		if a.status != c.status {
			t.Errorf("POST %v to %s = %d %s, want %d", c.body, c.slug, a.status, a.raw, c.status)
			continue
		}
		if got := h.lastEvent(t, c.slug).Metadata["branch_resolution"]; got != c.resolution {
			t.Errorf("POST %v: branch_resolution = %v, want %s", c.body, got, c.resolution)
		}
	}
	if refIn(dir, "refs/heads/o1") == "" {
		t.Error("refs/heads/o1 was not created from the origin-tracking branch")
	}
}

func TestPatchRoutes_ScopeAndAccess(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	h.carry(t, "carry", nil)
	member := h.CreateUser(t, "member")
	h.AddMember(t, h.org, member)
	memberKey := h.IssueKey(t, member)
	outsiderKey := h.IssueKey(t, h.CreateUser(t, "outsider"))
	patchesRead := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"patches:read"}})
	workspacesRead := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"workspaces:read"}})

	invalid := map[string]any{"branch_name": "a..b"}
	cases := []struct {
		name       string
		credential string
		method     string
		slug       string
		body       any
		status     int
		errorType  string
	}{
		{"a PAT with patches:read only adds", patchesRead, http.MethodPost, "carry", map[string]any{"branch_name": "p1"}, http.StatusForbidden, "insufficient_scope"},
		{"a PAT with workspaces:read only lists", workspacesRead, http.MethodGet, "carry", nil, http.StatusForbidden, "insufficient_scope"},
		{"the same PAT lists an unknown workspace", workspacesRead, http.MethodGet, "nowhere", nil, http.StatusForbidden, "insufficient_scope"},
		{"an outsider lists", outsiderKey, http.MethodGet, "carry", nil, http.StatusNotFound, "not_found"},
		{"a member with read access adds an invalid body", memberKey, http.MethodPost, "carry", invalid, http.StatusForbidden, "forbidden"},
		{"a member with read access lists", memberKey, http.MethodGet, "carry", nil, http.StatusOK, ""},
		{"the owner adds", h.key, http.MethodPost, "carry", map[string]any{"branch_name": "p1"}, http.StatusCreated, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, raw := h.Call(t, c.method, "/api/v1/workspaces/"+c.slug+"/patches", c.credential, c.body)
			var a answer
			_ = json.Unmarshal(raw, &a)
			if status != c.status || a.Error.ErrorType != c.errorType {
				t.Errorf("%s = %d %s, want %d %s", c.method, status, raw, c.status, c.errorType)
			}
		})
	}
}
