package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// readHub is a hub with an owner who holds an API key and whose
// organization owns the workspaces add stores; repoPath is where the hub
// looks for each workspace's copy.
type readHub struct {
	*hubtest.Hub
	owner, org, key string
	repoPath        func(slug string) string
}

func startReadHub(t *testing.T) *readHub {
	t.Helper()
	h := &readHub{}
	h.Hub = hubtest.Start(t, func(c *hub.Composition) {
		c.AdjustCarryPatch(func(d *carrypatch.Deps) { h.repoPath = d.RepoPath })
	})
	h.owner = h.CreateUser(t, "owner")
	h.key = h.IssueKey(t, h.owner)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, h.owner)
	return h
}

// add stores the workspace slug of the owner in mode, with status and the
// maintenance marker given, changed further by edit.
func (h *readHub) add(t *testing.T, slug, mode string, status workspace.Status, marker workspace.Marker, edit ...func(w *workspace.Workspace)) {
	t.Helper()
	now := time.Now().UTC()
	w := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: mode, GitURL: new("https://origin.test/" + slug + ".git"),
		DefaultBranch: new("main"), OwnerID: h.owner, OrgID: h.org, Status: status, DisplayName: slug,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(), Marker: marker,
	}
	for _, fn := range edit {
		fn(&w)
	}
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, w) }); err != nil {
		t.Fatalf("store workspace %s: %v", slug, err)
	}
}

// put stores the patches through the patch store.
func (h *readHub) put(t *testing.T, patches ...carrypatch.Patch) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, p := range patches {
			if err := carrypatch.InsertPatch(ctx, tx, p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("store the patches: %v", err)
	}
}

// at is 2026-11-16 at hh:mm UTC.
func at(hh, mm int) time.Time { return time.Date(2026, 11, 16, hh, mm, 0, 0, time.UTC) }

// livePatch is the patch of branch at position pos of the workspace slug.
func livePatch(slug, branch string, pos int) carrypatch.Patch {
	return carrypatch.Patch{ID: uuid.New().String(), Workspace: slug, BranchName: branch, Position: new(pos),
		Status: carrypatch.StatusActive, AddedAt: at(9, 0), UpdatedAt: at(9, 0)}
}

// deletedPatch is the patch of branch of the workspace slug, deleted at
// deleted.
func deletedPatch(slug, branch string, deleted time.Time) carrypatch.Patch {
	return carrypatch.Patch{ID: uuid.New().String(), Workspace: slug, BranchName: branch, Status: carrypatch.StatusDeleted,
		AddedAt: at(9, 0), UpdatedAt: deleted, DeletedAt: &deleted}
}

// seed stores the carry-patch workspace slug, ready, with the patches p1,
// p2 and p3 at positions 1 to 3 and the deleted patches d1 (deleted at
// 10:00) and d2 (deleted at 11:00), and returns them by branch.
func (h *readHub) seed(t *testing.T, slug string) map[string]carrypatch.Patch {
	t.Helper()
	h.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	ps := map[string]carrypatch.Patch{
		"p1": livePatch(slug, "p1", 1), "p2": livePatch(slug, "p2", 2), "p3": livePatch(slug, "p3", 3),
		"d1": deletedPatch(slug, "d1", at(10, 0)), "d2": deletedPatch(slug, "d2", at(11, 0)),
	}
	for _, b := range []string{"p1", "p2", "p3", "d1", "d2"} {
		h.put(t, ps[b])
	}
	return ps
}

// answer is a decoded answer of the hub: a patch, a list or an error.
type answer struct {
	status int
	raw    []byte
	Items  []map[string]any `json:"items"`
	Next   *string          `json:"next_cursor"`
	Error  struct {
		ErrorType string `json:"error_type"`
		Message   string `json:"message"`
	} `json:"error"`
	patch map[string]any
}

// get sends GET /api/v1+path as the owner and decodes the answer.
func (h *readHub) get(t *testing.T, path string) answer {
	t.Helper()
	status, body := h.Call(t, http.MethodGet, "/api/v1"+path, h.key, nil)
	a := answer{status: status, raw: body}
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("GET %s = %d %s: %v", path, status, body, err)
	}
	_ = json.Unmarshal(body, &a.patch)
	return a
}

// branches are the branch names of the list's items, in order.
func (a answer) branches() []string {
	out := make([]string, 0, len(a.Items))
	for _, it := range a.Items {
		out = append(out, fmt.Sprint(it["branch_name"]))
	}
	return out
}

// pages follows next_cursor from GET path with query and limit, and
// returns the branch names of each page.
func (h *readHub) pages(t *testing.T, path, query string, limit int) [][]string {
	t.Helper()
	var out [][]string
	cursor := ""
	for range 10 {
		q := fmt.Sprintf("limit=%d", limit)
		if query != "" {
			q += "&" + query
		}
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		a := h.get(t, path+"?"+q)
		if a.status != http.StatusOK {
			t.Fatalf("GET %s?%s = %d %s, want 200", path, q, a.status, a.raw)
		}
		out = append(out, a.branches())
		if a.Next == nil {
			return out
		}
		cursor = *a.Next
	}
	t.Fatalf("GET %s?%s: more than 10 pages", path, query)
	return nil
}

func TestList_PositionOrderAndPaging(t *testing.T) {
	t.Parallel()
	h := startReadHub(t)
	h.seed(t, "carry")
	h.add(t, "empty", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)

	a := h.get(t, "/workspaces/carry/patches")
	if a.status != http.StatusOK || !slices.Equal(a.branches(), []string{"p1", "p2", "p3"}) || a.Next != nil {
		t.Fatalf("GET patches = %d %v next %v (%s), want 200 [p1 p2 p3] and no next_cursor", a.status, a.branches(), a.Next, a.raw)
	}
	for _, it := range a.Items {
		if len(it) != 14 {
			t.Errorf("the item of %v has %d members, want 14: %v", it["branch_name"], len(it), it)
		}
	}
	if got := h.pages(t, "/workspaces/carry/patches", "", 2); !slices.EqualFunc(got, [][]string{{"p1", "p2"}, {"p3"}}, slices.Equal) {
		t.Errorf("the pages of limit 2 are %v, want [[p1 p2] [p3]]", got)
	}

	empty := h.get(t, "/workspaces/empty/patches")
	if empty.status != http.StatusOK || !strings.Contains(string(empty.raw), `"items":[]`) || empty.Next != nil {
		t.Errorf("GET patches of a workspace without patches = %d %s, want 200 with items [] and next_cursor null", empty.status, empty.raw)
	}

	for _, limit := range []string{"0", "201"} {
		a := h.get(t, "/workspaces/carry/patches?limit="+limit)
		if a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" {
			t.Errorf("GET patches?limit=%s = %d %s, want 400 invalid_request", limit, a.status, a.Error.ErrorType)
		}
	}
}

func TestList_IncludeDeleted(t *testing.T) {
	t.Parallel()
	h := startReadHub(t)
	ps := h.seed(t, "carry")

	a := h.get(t, "/workspaces/carry/patches?include_deleted=true")
	if a.status != http.StatusOK || !slices.Equal(a.branches(), []string{"p1", "p2", "p3", "d2", "d1"}) {
		t.Fatalf("GET patches?include_deleted=true = %d %v (%s), want [p1 p2 p3 d2 d1]", a.status, a.branches(), a.raw)
	}
	for _, it := range a.Items[3:] {
		if it["status"] != carrypatch.StatusDeleted || it["position"] != nil || it["deleted_at"] == nil {
			t.Errorf("the deleted item %v has status %v, position %v, deleted_at %v; want deleted, null and set",
				it["branch_name"], it["status"], it["position"], it["deleted_at"])
		}
	}
	want := [][]string{{"p1", "p2"}, {"p3", "d2"}, {"d1"}}
	if got := h.pages(t, "/workspaces/carry/patches", "include_deleted=true", 2); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("the pages of limit 2 are %v, want %v", got, want)
	}
	if a := h.get(t, "/workspaces/carry/patches?include_deleted=false"); !slices.Equal(a.branches(), []string{"p1", "p2", "p3"}) {
		t.Errorf("GET patches?include_deleted=false = %d %v, want [p1 p2 p3]", a.status, a.branches())
	}

	// A third patch deleted at the same time as d2: the two are ordered by
	// id, and a page boundary between them repeats and skips nothing.
	d3 := deletedPatch("carry", "d3", at(11, 0))
	h.put(t, d3)
	tied := []string{"d3", "d2"}
	if ps["d2"].ID < d3.ID {
		tied = []string{"d2", "d3"}
	}
	full := slices.Concat([]string{"p1", "p2", "p3"}, tied, []string{"d1"})
	if a := h.get(t, "/workspaces/carry/patches?include_deleted=true"); !slices.Equal(a.branches(), full) {
		t.Errorf("GET patches?include_deleted=true = %v, want %v (deleted at the same time, by id)", a.branches(), full)
	}
	if got := slices.Concat(h.pages(t, "/workspaces/carry/patches", "include_deleted=true", 4)...); !slices.Equal(got, full) {
		t.Errorf("the pages of limit 4 hold %v, want %v", got, full)
	}
}

func TestList_RejectsBadIncludeDeleted(t *testing.T) {
	t.Parallel()
	h := startReadHub(t)
	h.seed(t, "carry")
	for _, query := range []string{"include_deleted=1", "include_deleted=yes", "include_deleted=TRUE", "include_deleted=",
		"include_deleted=true&include_deleted=true"} {
		a := h.get(t, "/workspaces/carry/patches?"+query)
		if a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" {
			t.Errorf("GET patches?%s = %d %s, want 400 invalid_request", query, a.status, a.Error.ErrorType)
		}
	}
}

func TestGet_ReturnsAnyPatch(t *testing.T) {
	t.Parallel()
	h := startReadHub(t)
	ps := h.seed(t, "carry")
	copyDir := h.repoPath("carry")
	gitrepotest.InitBare(t, copyDir, "")

	p2 := h.get(t, "/workspaces/carry/patches/"+ps["p2"].ID)
	if p2.status != http.StatusOK || p2.patch["branch_name"] != "p2" || p2.patch["position"] != float64(2) || len(p2.patch) != 14 {
		t.Errorf("GET the patch p2 = %d %s, want 200 with branch_name p2, position 2 and 14 members", p2.status, p2.raw)
	}
	if _, ok := p2.patch["replaced_sha"]; ok {
		t.Errorf("GET the patch p2 without a backup ref = %s, want no replaced_sha", p2.raw)
	}
	d1 := h.get(t, "/workspaces/carry/patches/"+ps["d1"].ID)
	if d1.status != http.StatusOK || d1.patch["status"] != carrypatch.StatusDeleted || d1.patch["position"] != nil {
		t.Errorf("GET the deleted patch d1 = %d %s, want 200 with status deleted and position null", d1.status, d1.raw)
	}

	tree, err := gitrepotest.Run(copyDir, "", "mktree")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"p2", "d1"} {
		r := gitrepotest.Git(t, copyDir, "commit-tree", tree, "-m", "replaced "+b)
		gitrepotest.SetRef(t, copyDir, "refs/parley/replaced/"+b, r)
		a := h.get(t, "/workspaces/carry/patches/"+ps[b].ID)
		if got, _ := a.patch["replaced_sha"].(string); a.status != http.StatusOK || got != r || len(got) != 40 {
			t.Errorf("GET the patch %s with a backup ref at %s = %d %s, want replaced_sha %s", b, r, a.status, a.raw, r)
		}
	}
}

func TestGet_UnknownIdIsNotFound(t *testing.T) {
	t.Parallel()
	h := startReadHub(t)
	h.seed(t, "carry")
	other := h.seed(t, "other")
	for name, id := range map[string]string{
		"a random UUID":                     uuid.New().String(),
		"the id of another workspace's one": other["p1"].ID,
		"abc":                               "abc",
		"300 characters":                    strings.Repeat("a", 300),
	} {
		a := h.get(t, "/workspaces/carry/patches/"+id)
		if a.status != http.StatusNotFound || a.Error.ErrorType != "not_found" || a.Error.Message != "patch not found" {
			t.Errorf("GET %s = %d %s %q, want 404 not_found %q", name, a.status, a.Error.ErrorType, a.Error.Message, "patch not found")
		}
	}
}

func TestRead_WorksInEveryWorkspaceState(t *testing.T) {
	t.Parallel()
	h := startReadHub(t)
	cases := []struct {
		slug   string
		status workspace.Status
		marker workspace.Marker
	}{
		{"pending", workspace.StatusPending, workspace.MarkerNone},
		{"cloning", workspace.StatusCloning, workspace.MarkerNone},
		{"failed", workspace.StatusFailed, workspace.MarkerNone},
		{"archived", workspace.StatusArchived, workspace.MarkerNone},
		{"deleting", workspace.StatusReady, workspace.MarkerDelete},
	}
	for _, c := range cases {
		h.add(t, c.slug, workspace.ModeCarryPatch, c.status, c.marker)
		p := livePatch(c.slug, "p1", 1)
		h.put(t, p)
		if a := h.get(t, "/workspaces/"+c.slug+"/patches"); a.status != http.StatusOK || len(a.Items) != 1 {
			t.Errorf("GET the patches of %s = %d %s, want 200 with one item", c.slug, a.status, a.raw)
		}
		a := h.get(t, "/workspaces/"+c.slug+"/patches/"+p.ID)
		if _, replaced := a.patch["replaced_sha"]; a.status != http.StatusOK || replaced {
			t.Errorf("GET the patch of %s = %d %s, want 200 without replaced_sha", c.slug, a.status, a.raw)
		}
	}
}
