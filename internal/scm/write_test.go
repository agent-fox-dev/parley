package scm_test

import (
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// TestWriteGate checks the gate every write route passes before its handler
// runs, around a probe route that answers what the gate handed it: the
// scope a PAT needs, the access the caller needs and the state the
// workspace, and a fork's parent, must be in. A request the gate lets
// through is reported to the recorder once; one it refuses is not.
func TestWriteGate(t *testing.T) {
	t.Parallel()
	var ran atomic.Int64
	w := newComposedWorld(t, "", func(c *hub.Composition) { c.AddRoute(scm.WriteProbeRoute(c.SCM(), &ran)) })
	now := time.Now().UTC()
	boundPAT := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:write"}, Workspace: "demo", CreatedAt: now})

	type row struct {
		name, slug, credential string
		status                 int
		errorType              string
		// want are members the probe's answer holds, when the probe runs.
		want map[string]any
	}
	check := func(t *testing.T, rows []row) {
		t.Helper()
		for _, tc := range rows {
			before := ran.Load()
			r := w.send(t, http.MethodPost, scmPath(tc.slug, "/probe"), tc.credential)
			runs := ran.Load() - before
			observed := w.observed.reset()
			if r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("%s: %d %s, want %d %q", tc.name, r.status, r.body, tc.status, tc.errorType)
			}
			if tc.status == http.StatusOK {
				if runs != 1 || !slices.Equal(observed, []string{"probe ok"}) {
					t.Errorf("%s: the probe ran %d times and the recorder saw %q, want once and [probe ok]", tc.name, runs, observed)
				}
				for k, v := range tc.want {
					if r.json[k] != v {
						t.Errorf("%s: the probe was handed %s %v, want %v", tc.name, k, r.json[k], v)
					}
				}
				continue
			}
			if runs != 0 || len(observed) != 0 {
				t.Errorf("%s: the probe ran %d times and the recorder saw %q, want neither", tc.name, runs, observed)
			}
		}
	}

	t.Run("refuses a PAT without git:write", func(t *testing.T) {
		check(t, []row{
			{name: "git:read PAT", slug: "demo", credential: w.readPAT, status: http.StatusForbidden, errorType: "insufficient_scope"},
			{name: "git:read PAT on a slug no workspace has", slug: "nope", credential: w.readPAT, status: http.StatusForbidden, errorType: "insufficient_scope"},
			{name: "git:write PAT", slug: "demo", credential: w.writePAT, status: http.StatusOK,
				want: map[string]any{"slug": "demo", "credential": identity.TypePAT, "bound": false}},
			{name: "git:write PAT bound to the workspace", slug: "demo", credential: boundPAT, status: http.StatusOK,
				want: map[string]any{"credential": identity.TypePAT, "bound": true}},
			{name: "API key", slug: "demo", credential: w.ownerKey, status: http.StatusOK,
				want: map[string]any{"credential": identity.TypeAPIKey, "bound": false}},
		})
	})

	t.Run("refuses a caller who cannot write", func(t *testing.T) {
		check(t, []row{
			{name: "no access", slug: "demo", credential: w.outsiderKey, status: http.StatusNotFound, errorType: "not_found"},
			{name: "read access", slug: "demo", credential: w.memberKey, status: http.StatusForbidden, errorType: "forbidden"},
			{name: "owner", slug: "demo", credential: w.ownerKey, status: http.StatusOK, want: map[string]any{"level": "owner"}},
			{name: "owner of a fork", slug: "demo--t1", credential: w.memberKey, status: http.StatusOK, want: map[string]any{"slug": "demo--t1", "level": "owner"}},
			{name: "admin token", slug: "demo", credential: w.Token, status: http.StatusOK,
				want: map[string]any{"level": "admin", "credential": identity.TypeAdmin}},
		})
	})

	t.Run("refuses a workspace that cannot take a write", func(t *testing.T) {
		const stream = "commit refs/heads/main\n" + testCommitter + "data 2\nc1\nM 100644 inline f.txt\ndata 3\nc1\n\n"
		for slug, status := range map[string]workspace.Status{
			"pending": workspace.StatusPending, "cloning": workspace.StatusCloning, "failed": workspace.StatusFailed,
		} {
			w.addWorkspace(t, slug, w.org, stream)
			w.set(t, slug, "status", string(status))
		}
		w.addWorkspace(t, "archiving", w.org, stream)
		w.set(t, "archiving", "maintenance", string(workspace.MarkerArchive))
		w.addWorkspace(t, "fresh", w.org, stream)
		w.addFork(t, "fresh", w.org, []string{"main"}, nil)
		w.set(t, "fresh", "status", string(workspace.StatusCloning))
		check(t, []row{
			{name: "archived", slug: "old", credential: w.ownerKey, status: http.StatusConflict, errorType: "workspace_archived"},
			{name: "a fork whose parent is archived", slug: "old--t1", credential: w.memberKey, status: http.StatusConflict, errorType: "parent_archived"},
			{name: "pending", slug: "pending", credential: w.ownerKey, status: http.StatusConflict, errorType: "workspace_busy"},
			{name: "cloning", slug: "cloning", credential: w.ownerKey, status: http.StatusConflict, errorType: "workspace_busy"},
			{name: "ready with an archive running", slug: "archiving", credential: w.ownerKey, status: http.StatusConflict, errorType: "workspace_busy"},
			{name: "a fork whose parent is cloning", slug: "fresh--t1", credential: w.memberKey, status: http.StatusConflict, errorType: "workspace_busy"},
			{name: "failed", slug: "failed", credential: w.ownerKey, status: http.StatusConflict, errorType: "workspace_not_ready"},
			{name: "ready", slug: "demo", credential: w.ownerKey, status: http.StatusOK, want: map[string]any{"slug": "demo"}},
			{name: "a fork whose parent is ready", slug: "demo--t1", credential: w.memberKey, status: http.StatusOK, want: map[string]any{"slug": "demo--t1"}},
		})
	})
}
