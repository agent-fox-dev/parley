package carrypatch_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// itemsOf are the branch names and positions of a batch answer's items, in
// order.
func itemsOf(a answer) (branches []string, positions []any) {
	for _, it := range a.Items {
		branches = append(branches, fmt.Sprint(it["branch_name"]))
		positions = append(positions, it["position"])
	}
	return branches, positions
}

// floats are ns as the positions of decoded JSON.
func floats(ns ...int) []any {
	out := make([]any, len(ns))
	for i, n := range ns {
		out[i] = float64(n)
	}
	return out
}

func TestBatch_CreatesAllInRequestOrder(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	dir := h.carry(t, "carry", nil)
	h.put(t, deletedPatch("carry", "p1", at(10, 0)))
	rootsAt(t, dir, map[string]string{carrypatch.ReplacedRefPrefix + "p1": "replaced"})

	a := h.post(t, "carry", []map[string]any{
		{"branch_name": "p1"}, {"branch_name": "p2", "description": "two"}, {"branch_name": "p3", "skip_branch_check": true},
	}, nil)
	if a.status != http.StatusCreated {
		t.Fatalf("POST the batch = %d %s, want 201", a.status, a.raw)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(a.raw, &members); err != nil || len(members) != 1 || members["items"] == nil {
		t.Errorf("the answer is %s, want the one member items", a.raw)
	}
	branches, positions := itemsOf(a)
	if !slices.Equal(branches, []string{"p1", "p2", "p3"}) || fmt.Sprint(positions) != fmt.Sprint(floats(1, 2, 3)) {
		t.Errorf("the items are %v at %v, want [p1 p2 p3] at [1 2 3]", branches, positions)
	}
	for _, it := range a.Items {
		if it["status"] != "active" {
			t.Errorf("the patch of %v is %v, want active", it["branch_name"], it["status"])
		}
	}
	if a.Items[1]["description"] != "two" {
		t.Errorf("the patch of p2 has description %v, want two", a.Items[1]["description"])
	}
	_, created := h.events(t, "carry")
	if len(created) != 3 {
		t.Fatalf("there are %d parley.patch.create, want 3", len(created))
	}
	for i, e := range created {
		want := []string{carrypatch.ResolutionLocal, carrypatch.ResolutionLocal, carrypatch.ResolutionSkipped}[i]
		if e.ResourceID != a.Items[i]["id"] || e.Metadata["position"] != a.Items[i]["position"] || e.Metadata["branch_resolution"] != want {
			t.Errorf("event %d is %+v, want resource %v at position %v resolved %s", i, e, a.Items[i]["id"], a.Items[i]["position"], want)
		}
	}
	if id := refIn(dir, carrypatch.ReplacedRefPrefix+"p1"); id != "" || h.count(t, "carry") != 3 {
		t.Errorf("the deleted patch of p1 left %d rows and its backup ref at %q, want 3 rows and no ref", h.count(t, "carry"), id)
	}

	one := h.post(t, "carry", []map[string]any{{"branch_name": "p4"}}, nil)
	if _, positions := itemsOf(one); one.status != http.StatusCreated || fmt.Sprint(positions) != fmt.Sprint(floats(4)) {
		t.Errorf("POST a batch of p4 = %d %s, want 201 with one item at position 4", one.status, one.raw)
	}
}

func TestBatch_Positions(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	// a@3 is the branch a with position 3.
	parse := func(spec string) map[string]any {
		branch, pos, found := strings.Cut(spec, "@")
		body := map[string]any{"branch_name": branch, "skip_branch_check": true}
		if found {
			body["position"] = json.Number(pos)
		}
		return body
	}
	cases := []struct {
		before, batch, after []string
		positions            []int
	}{
		{nil, []string{"a", "b@1", "c"}, []string{"b", "a", "c"}, []int{2, 1, 3}},
		{[]string{"x", "y"}, []string{"a@1", "b@1"}, []string{"b", "a", "x", "y"}, []int{2, 1}},
		{[]string{"x", "y"}, []string{"a", "b@2", "c@9"}, []string{"x", "b", "y", "c", "a"}, []int{5, 2, 4}},
		{[]string{"x"}, []string{"a@3", "b@3"}, []string{"x", "a", "b"}, []int{2, 3}},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("%v onto %v", c.batch, c.before), func(t *testing.T) {
			slug := fmt.Sprintf("pos%d", i)
			h.carry(t, slug, nil)
			for n, b := range c.before {
				h.put(t, livePatch(slug, b, n+1))
			}
			var body []map[string]any
			for _, spec := range c.batch {
				body = append(body, parse(spec))
			}
			a := h.post(t, slug, body, nil)
			if a.status != http.StatusCreated {
				t.Fatalf("POST %v = %d %s, want 201", c.batch, a.status, a.raw)
			}
			if _, positions := itemsOf(a); fmt.Sprint(positions) != fmt.Sprint(floats(c.positions...)) {
				t.Errorf("the answer's positions are %v, want %v", positions, c.positions)
			}
			if got := h.order(t, slug); !slices.Equal(got, c.after) {
				t.Errorf("the order is %v, want %v", got, c.after)
			}
			_, created := h.events(t, slug)
			if len(created) != len(a.Items) {
				t.Fatalf("there are %d parley.patch.create, want %d", len(created), len(a.Items))
			}
			for n, e := range created {
				if e.ResourceID != a.Items[n]["id"] || e.Metadata["position"] != a.Items[n]["position"] {
					t.Errorf("the event of %v has position %v, want %v", a.Items[n]["branch_name"], e.Metadata["position"], a.Items[n]["position"])
				}
			}
		})
	}
}

func TestBatch_RejectsInvalid(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	dir := h.carry(t, "carry", nil)
	refs := gitrepotest.Refs(t, dir, "refs/")
	cases := []struct {
		body, prefix string
	}{
		{`[]`, ""},
		{`[{"branch_name": "p1"}, 5]`, "patch[1]: "},
		{`[{"branch_name": "p1"}, {}]`, "patch[1]: "},
		{`[{"branch_name": "a..b"}]`, "patch[0]: "},
		{`[{"branch_name": "p1"}, {"branch_name": "p2", "position": 0}]`, "patch[1]: "},
		{`[{"branch_name": "p1", "if_not_exists": true}]`, "patch[0]: "},
		{`[{"branch_name": "p1"}, {"branch_name": "p2"}, {"branch_name": "p1"}]`, "patch[2]: "},
		{`[{"branch_name": "deploy"}]`, "patch[0]: "},
	}
	for _, c := range cases {
		t.Run(c.body, func(t *testing.T) {
			a := h.post(t, "carry", c.body, nil)
			if a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" || !strings.HasPrefix(a.Error.Message, c.prefix) {
				t.Errorf("POST %s = %d %s, want 400 invalid_request with a message that starts with %q", c.body, a.status, a.raw, c.prefix)
			}
		})
	}
	if n := h.count(t, "carry"); n != 0 {
		t.Errorf("there are %d patches, want none", n)
	}
	if all, _ := h.events(t, "carry"); all != 0 {
		t.Errorf("there are %d events, want none", all)
	}
	if after := gitrepotest.Refs(t, dir, "refs/"); fmt.Sprint(after) != fmt.Sprint(refs) {
		t.Errorf("the refs are %v, want them unchanged: %v", after, refs)
	}
	if a := h.post(t, "carry", `[{"branch_name": "p1", "if_not_exists": false}]`, nil); a.status != http.StatusCreated {
		t.Errorf("POST a batch with if_not_exists false = %d %s, want 201", a.status, a.raw)
	}
}

func TestBatch_AllOrNothing(t *testing.T) {
	t.Parallel()
	h := startAddHub(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	fromOrigin := func(w *workspace.Workspace) {
		w.GitURL = new(gone.URL + "/repo.git")
		w.Settings = map[string]json.RawMessage{"patch_branch_source": json.RawMessage(`"origin"`)}
	}
	// The store refuses the second patch the batch of the workspace fault
	// inserts: p9 is the first row, the batch's first patch the second.
	const fault = `CREATE TRIGGER fail_second_patch BEFORE INSERT ON patches
WHEN NEW.workspace_slug = 'fault' AND (SELECT count(*) FROM patches WHERE workspace_slug = 'fault') >= 2
BEGIN SELECT RAISE(ABORT, 'the second patch fails'); END`

	cases := []struct {
		slug      string
		edit      func(w *workspace.Workspace)
		batch     []string
		status    int
		errorType string
		o1        bool // whether refs/heads/o1 exists afterwards
	}{
		{"registered", nil, []string{"o1", "p9"}, http.StatusConflict, "patch_exists", false},
		{"unresolved", nil, []string{"o1", "nowhere"}, http.StatusNotFound, "branch_not_found", true},
		{"origin-down", fromOrigin, []string{"p1", "nowhere"}, http.StatusBadGateway, "origin_fetch_failed", false},
		{"fault", nil, []string{"p1", "p2"}, http.StatusInternalServerError, "internal", false},
	}
	for _, c := range cases {
		t.Run(c.slug, func(t *testing.T) {
			dir := h.carry(t, c.slug, c.edit)
			rootsAt(t, dir, map[string]string{"refs/remotes/origin/o1": "o1"})
			h.put(t, livePatch(c.slug, "p9", 1))
			if c.slug == "fault" {
				if _, err := h.Store.Main().Exec(fault); err != nil {
					t.Fatalf("create the failing trigger: %v", err)
				}
				defer func() {
					if _, err := h.Store.Main().Exec(`DROP TRIGGER fail_second_patch`); err != nil {
						t.Errorf("drop the failing trigger: %v", err)
					}
				}()
			}
			events, _ := h.events(t, c.slug)
			var body []map[string]any
			for _, b := range c.batch {
				body = append(body, map[string]any{"branch_name": b})
			}
			a := h.post(t, c.slug, body, nil)
			if a.status != c.status || a.Error.ErrorType != c.errorType {
				t.Errorf("POST %v = %d %s, want %d %s", c.batch, a.status, a.raw, c.status, c.errorType)
			}
			if got := h.order(t, c.slug); !slices.Equal(got, []string{"p9"}) || h.count(t, c.slug) != 1 {
				t.Errorf("the patches are %v (%d rows), want [p9] alone", got, h.count(t, c.slug))
			}
			if after, _ := h.events(t, c.slug); after != events {
				t.Errorf("there are %d events, want %d", after, events)
			}
			if got := refIn(dir, "refs/heads/o1") != ""; got != c.o1 {
				t.Errorf("refs/heads/o1 exists: %v, want %v", got, c.o1)
			}
		})
	}
}
