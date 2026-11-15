package scm_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestRead_ServesEveryReaderOnEveryReadableWorkspace(t *testing.T) {
	w := newWorld(t)
	creds := []struct{ name, cred string }{
		{"owner key", w.ownerKey}, {"member key", w.memberKey},
		{"PAT with git:read", w.readPAT}, {"PAT with git:write alone", w.writePAT}, {"admin token", w.Token},
	}
	// What old and its fork answered before old was archived.
	w.set(t, "old", "status", string(workspace.StatusReady))
	before := map[string][]byte{}
	for _, slug := range []string{"old", "old--t1"} {
		before[slug] = w.get(t, scmPath(slug, "/refs"), w.ownerKey).body
	}
	w.set(t, "old", "status", string(workspace.StatusArchived))

	for _, ws := range []struct{ name, slug string }{
		{"ready", "demo"}, {"archived", "old"}, {"fork of a ready parent", "demo--t1"}, {"fork of an archived parent", "old--t1"},
	} {
		for _, c := range creds {
			t.Run(ws.name+"/"+c.name, func(t *testing.T) {
				r := w.get(t, scmPath(ws.slug, "/refs"), c.cred)
				if _, isList := r.json["items"].([]any); r.status != http.StatusOK || !isList {
					t.Fatalf("status = %d, body %s; want 200 with a list of items", r.status, r.body)
				}
				if b, archived := before[ws.slug]; archived && !bytes.Equal(r.body, b) {
					t.Errorf("body = %s, want what it answered before archiving, %s", r.body, b)
				}
			})
		}
	}
	for _, rt := range scmRoutes {
		for _, c := range creds {
			t.Run("demo "+rt.ok+"/"+c.name, func(t *testing.T) {
				if r := w.get(t, scmPath("demo", rt.ok), c.cred); r.status != http.StatusOK {
					t.Errorf("status = %d, want 200 (body %s)", r.status, r.body)
				}
			})
		}
	}
}

func TestRead_RefusesACallerWhoMayNotReadWithoutOpeningTheRepository(t *testing.T) {
	w := newWorld(t)
	const requestID = "5f0c6a52-3a8e-4d43-9c55-0d8b1f0e2a41"
	for _, rt := range scmRoutes {
		t.Run(rt.ok, func(t *testing.T) {
			demo, nope := scmPath("demo", rt.ok), scmPath("nope", rt.ok)
			if r := w.get(t, demo, ""); r.status != http.StatusUnauthorized || r.errorType() != "unauthenticated" {
				t.Errorf("without a credential: %d %s, want 401 unauthenticated", r.status, r.body)
			}
			for _, path := range []string{demo, nope} {
				if r := w.get(t, path, w.changesPAT); r.status != http.StatusForbidden || r.errorType() != "insufficient_scope" {
					t.Errorf("%s with a PAT holding changes:read: %d %s, want 403 insufficient_scope", path, r.status, r.body)
				}
			}
			a := w.get(t, demo, w.outsiderKey, "X-Request-ID", requestID)
			b := w.get(t, nope, w.outsiderKey, "X-Request-ID", requestID)
			if a.status != http.StatusNotFound || a.errorType() != "not_found" || !bytes.Equal(a.body, b.body) {
				t.Errorf("outsider: %d %s, want 404 not_found with the body of a missing workspace, %s", a.status, a.body, b.body)
			}
		})
	}
	if n := w.opens.Load(); n != 0 {
		t.Errorf("the routes opened a repository %d times, want none", n)
	}
}

func TestRead_RefusesAWorkspaceThatIsNotReadableWithoutOpeningTheRepository(t *testing.T) {
	w := newWorld(t)
	tests := []struct {
		name, slug        string
		set, column, from string
		to, want          string
	}{
		{"pending", "demo", "demo", "status", "ready", "pending", "workspace_busy"},
		{"cloning", "demo", "demo", "status", "ready", "cloning", "workspace_busy"},
		{"reclone running", "demo", "demo", "maintenance", "", "reclone", "workspace_busy"},
		{"archive running", "demo", "demo", "maintenance", "", "archive", "workspace_busy"},
		{"delete running", "demo", "demo", "maintenance", "", "delete", "workspace_busy"},
		{"delete of an archived workspace running", "old", "old", "maintenance", "", "delete", "workspace_busy"},
		{"fork whose parent is cloning", "demo--t1", "demo", "status", "ready", "cloning", "workspace_busy"},
		{"fork whose archived parent is being deleted", "old--t1", "old", "maintenance", "", "delete", "workspace_busy"},
		{"failed", "demo", "demo", "status", "ready", "failed", "workspace_not_ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w.set(t, tt.set, tt.column, tt.to)
			defer w.set(t, tt.set, tt.column, tt.from)
			for _, rt := range scmRoutes {
				r := w.get(t, scmPath(tt.slug, rt.ok), w.ownerKey)
				if r.status != http.StatusConflict || r.errorType() != tt.want {
					t.Errorf("%s: %d %s, want 409 %s", rt.ok, r.status, r.body, tt.want)
				}
			}
		})
	}
	if n := w.opens.Load(); n != 0 {
		t.Errorf("the routes opened a repository %d times, want none", n)
	}
}
