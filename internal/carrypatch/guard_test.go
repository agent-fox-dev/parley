package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// guardHub is a hub with two probe routes declared through carrypatch.Route
// and guarded by the component's Guard after their own validation: GET
// /workspaces/:slug/patch-probe reads, POST /workspaces/:slug/patch-probe
// writes and refuses a body whose member valid is not true. A probe that
// passes the guard counts the call for its workspace and answers 200.
type guardHub struct {
	*hubtest.Hub
	owner, org, key string

	mu    sync.Mutex
	calls map[string]int
}

func startGuardHub(t *testing.T) *guardHub {
	t.Helper()
	h := &guardHub{calls: map[string]int{}}
	h.Hub = hubtest.Start(t, func(c *hub.Composition) {
		comp := c.CarryPatch()
		probe := func(w http.ResponseWriter, r *http.Request) error {
			if r.Method == http.MethodPost {
				var body struct {
					Valid bool `json:"valid"`
				}
				if err := server.DecodeJSON(r, &body); err != nil {
					return err
				}
				if !body.Valid {
					return server.InvalidRequest("valid must be true")
				}
			}
			ws, err := comp.Guard(r)
			if err != nil {
				return err
			}
			h.mu.Lock()
			h.calls[r.Method+" "+ws.Slug]++
			h.mu.Unlock()
			server.WriteJSON(w, http.StatusOK, map[string]string{"slug": ws.Slug})
			return nil
		}
		c.AddRoute(carrypatch.Route(carrypatch.ReadRoute, server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/patch-probe", Handler: probe}))
		c.AddRoute(carrypatch.Route(carrypatch.WriteRoute, server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/patch-probe", Handler: probe}))
	})
	h.owner = h.CreateUser(t, "owner")
	h.key = h.IssueKey(t, h.owner)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, h.owner)
	return h
}

// add stores the workspace slug of the owner, in mode, status and with the
// maintenance marker given; a fork is added when parent is not "".
func (h *guardHub) add(t *testing.T, slug, mode string, status workspace.Status, marker workspace.Marker, parent string) {
	t.Helper()
	now := time.Now().UTC()
	w := workspace.Workspace{
		Slug: slug, Kind: access.KindRepository, Mode: mode, GitURL: new("https://origin.test/" + slug + ".git"),
		DefaultBranch: new("main"), OwnerID: h.owner, OrgID: h.org, Status: status, DisplayName: slug,
		CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(), Marker: marker,
	}
	if parent != "" {
		w.Kind, w.ParentSlug, w.GitURL = access.KindFork, &parent, nil
	}
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, w) }); err != nil {
		t.Fatalf("store workspace %s: %v", slug, err)
	}
}

// probe calls method on the probe route of slug as the owner, with body,
// and returns the status and the answer's error_type.
func (h *guardHub) probe(t *testing.T, method, slug string, body any) (int, string) {
	t.Helper()
	status, answer := h.Call(t, method, "/api/v1/workspaces/"+slug+"/patch-probe", h.key, body)
	var env struct {
		Error struct {
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(answer, &env)
	return status, env.Error.ErrorType
}

// called is how often the probe of method passed the guard on slug.
func (h *guardHub) called(method, slug string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[method+" "+slug]
}

func TestGuard_ModeMismatch(t *testing.T) {
	t.Parallel()
	h := startGuardHub(t)
	h.add(t, "std-ready", workspace.ModeStandard, workspace.StatusReady, workspace.MarkerNone, "")
	h.add(t, "std-failed", workspace.ModeStandard, workspace.StatusFailed, workspace.MarkerNone, "")
	h.add(t, "carry", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, "")
	// A task fork is always in mode standard, whatever its parent's mode.
	h.add(t, "carry-fork", workspace.ModeStandard, workspace.StatusReady, workspace.MarkerNone, "carry")
	valid := map[string]bool{"valid": true}
	for _, slug := range []string{"std-ready", "std-failed", "carry-fork"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(slug+" "+method, func(t *testing.T) {
				var body any
				if method == http.MethodPost {
					body = valid
				}
				status, typ := h.probe(t, method, slug, body)
				if status != http.StatusBadRequest || typ != "workspace_mode_mismatch" {
					t.Errorf("%s %s = %d %s, want 400 workspace_mode_mismatch", method, slug, status, typ)
				}
				if n := h.called(method, slug); n != 0 {
					t.Errorf("the handler behind the guard ran %d times, want never", n)
				}
			})
		}
	}
	t.Run("an invalid body is refused before the mode", func(t *testing.T) {
		status, typ := h.probe(t, http.MethodPost, "std-ready", map[string]bool{"valid": false})
		if status != http.StatusBadRequest || typ != "invalid_request" {
			t.Errorf("POST std-ready with an invalid body = %d %s, want 400 invalid_request", status, typ)
		}
	})
	t.Run("a carry-patch workspace passes", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if status, typ := h.probe(t, method, "carry", valid); status != http.StatusOK {
				t.Errorf("%s carry = %d %s, want 200", method, status, typ)
			}
		}
	})
}

func TestGuard_WriteNeedsReady(t *testing.T) {
	t.Parallel()
	h := startGuardHub(t)
	cases := []struct {
		slug   string
		status workspace.Status
		marker workspace.Marker
		want   string // the write's error_type, "" for none
	}{
		{"pending", workspace.StatusPending, workspace.MarkerNone, "workspace_busy"},
		{"cloning", workspace.StatusCloning, workspace.MarkerNone, "workspace_busy"},
		{"reclone", workspace.StatusReady, workspace.MarkerReclone, "workspace_busy"},
		{"failed", workspace.StatusFailed, workspace.MarkerNone, "workspace_not_ready"},
		{"archived", workspace.StatusArchived, workspace.MarkerNone, "workspace_archived"},
		{"ready", workspace.StatusReady, workspace.MarkerNone, ""},
	}
	for _, c := range cases {
		h.add(t, c.slug, workspace.ModeCarryPatch, c.status, c.marker, "")
	}
	for _, c := range cases {
		t.Run(c.slug, func(t *testing.T) {
			status, typ := h.probe(t, http.MethodPost, c.slug, map[string]bool{"valid": true})
			calls := h.called(http.MethodPost, c.slug)
			switch {
			case c.want == "" && (status != http.StatusOK || calls != 1):
				t.Errorf("POST %s = %d %s with %d handler calls, want 200 and one call", c.slug, status, typ, calls)
			case c.want != "" && (status != http.StatusConflict || typ != c.want):
				t.Errorf("POST %s = %d %s, want 409 %s", c.slug, status, typ, c.want)
			case c.want != "" && calls != 0:
				t.Errorf("the write handler behind the guard ran %d times on %s, want never", calls, c.slug)
			}
			if status, typ := h.probe(t, http.MethodGet, c.slug, nil); status != http.StatusOK {
				t.Errorf("GET %s = %d %s, want 200: a read checks no state", c.slug, status, typ)
			}
		})
	}
}
