package carrypatch_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// carryHub is a hub whose clone workers run the clone work of every
// workspace created, with its repository root in a directory of the test's
// own, a secret key, a git that trusts the test origins it was started
// for, and a user with an API key. It records, per workspace, the status
// the carry-patch set-up observes when it first reads the workspace's
// secrets.
type carryHub struct {
	*hubtest.Hub
	root string
	key  string
	comp *carrypatch.Component
	// secrets is the hub's secret component.
	secrets *secret.Component

	mu       sync.Mutex
	observed map[string]workspace.Status
}

// startCarryHub starts a hub whose git trusts every origin of origins.
func startCarryHub(t *testing.T, origins ...*gitrepotest.Origin) *carryHub {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), "ca-bundle.pem")
	var pems []byte
	for _, o := range origins {
		b, err := os.ReadFile(o.CAFile())
		if err != nil {
			t.Fatal(err)
		}
		pems = append(pems, b...)
	}
	if err := os.WriteFile(bundle, pems, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	root, err := json.Marshal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &carryHub{observed: map[string]workspace.Status{}}
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		TOML: "[workspace]\npath = " + string(root) + "\nworkers = 2\n",
		Env:  map[string]string{"GIT_SSL_CAINFO": bundle, secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(raw)},
		Compose: func(c *hub.Composition) {
			h.comp, h.secrets = c.CarryPatch(), c.Secrets()
			c.AdjustCarryPatch(func(d *carrypatch.Deps) {
				d.Secrets = observingSecrets{SecretReader: d.Secrets, store: d.Store.Main(), h: h}
			})
		},
	})
	_ = json.Unmarshal(root, &h.root)
	user := h.CreateUser(t, "dev")
	h.key = h.IssueKey(t, user)
	return h
}

// observingSecrets is the set-up's secret reader, which records the
// status of the workspace it is first asked about.
type observingSecrets struct {
	workspace.SecretReader
	store *sql.DB
	h     *carryHub
}

func (s observingSecrets) Get(ctx context.Context, scope secret.Scope, scopeID, name string) (string, bool, error) {
	if w, found, err := workspace.BySlug(ctx, s.store, scopeID); err == nil && found {
		s.h.mu.Lock()
		if _, seen := s.h.observed[scopeID]; !seen {
			s.h.observed[scopeID] = w.Status
		}
		s.h.mu.Unlock()
	}
	return s.SecretReader.Get(ctx, scope, scopeID, name)
}

// observedStatus is the status the set-up of slug observed, "" when it
// never ran.
func (h *carryHub) observedStatus(slug string) workspace.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.observed[slug]
}

// create creates the workspace slug of origin through the hub's route,
// with members added to the body.
func (h *carryHub) create(t *testing.T, slug string, origin *gitrepotest.Origin, members map[string]any) {
	t.Helper()
	body := map[string]any{"slug": slug, "git_url": origin.URL()}
	for k, v := range members {
		body[k] = v
	}
	if status, answer := h.Call(t, http.MethodPost, "/api/v1/workspaces", h.key, body); status != http.StatusCreated {
		t.Fatalf("create %s = %d %s, want 201", slug, status, answer)
	}
}

// carryPatch is the body of a carry-patch workspace whose upstream is up,
// with members added.
func carryPatch(up *gitrepotest.Origin, members map[string]any) map[string]any {
	body := map[string]any{"workspace_mode": "carry_patch", "upstream_url": up.URL()}
	for k, v := range members {
		body[k] = v
	}
	return body
}

// putSecret stores the workspace secret name of slug.
func (h *carryHub) putSecret(t *testing.T, slug, name, value string) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		return h.secrets.Put(ctx, tx, secret.ScopeWorkspace, slug, name, value)
	}); err != nil {
		t.Fatalf("store the secret %s of %s: %v", name, slug, err)
	}
}

// record reads the workspace slug.
func (h *carryHub) record(t *testing.T, slug string) workspace.Workspace {
	t.Helper()
	w, found, err := workspace.BySlug(context.Background(), h.Store.Main(), slug)
	if err != nil || !found {
		t.Fatalf("read workspace %s: found %v, %v", slug, found, err)
	}
	return w
}

// waitReady waits up to 20 s for the workspace slug to leave pending and
// cloning, and returns its record.
func (h *carryHub) waitReady(t *testing.T, slug string) workspace.Workspace {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		w := h.record(t, slug)
		if w.Status != workspace.StatusPending && w.Status != workspace.StatusCloning {
			if w.Status != workspace.StatusReady {
				t.Fatalf("workspace %s is %s with clone_error %v, want ready", slug, w.Status, str(w.CloneError))
			}
			return w
		}
		if time.Now().After(deadline) {
			t.Fatalf("workspace %s is still %s after 20 s", slug, w.Status)
		}
	}
}

// copyOf is the directory of the copy of the workspace slug.
func (h *carryHub) copyOf(slug string) string {
	return filepath.Join(h.root, slug+".git")
}

// eventTypes are the types of the events of the workspace slug, oldest
// first.
func (h *carryHub) eventTypes(t *testing.T, slug string) []string {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT type FROM hub_events WHERE workspace = ? ORDER BY seq`, slug)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		types = append(types, typ)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return types
}

// refIn is the commit id ref points at in the repository dir, "" when it
// does not exist.
func refIn(dir, ref string) string {
	id, err := gitrepotest.Run(dir, "", "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return id
}

// configIn is the value of key in the repository dir's own configuration,
// "" when it is not set.
func configIn(dir, key string) string {
	v, err := gitrepotest.Run(dir, "", "config", "--local", "--get", key)
	if err != nil {
		return ""
	}
	return v
}

// str is *s, or <nil>.
func str(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
