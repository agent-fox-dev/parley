package carrypatch_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
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

// patchHub is an addHub for the routes that maintain a patch list. Its
// carry-patch component counts the reads of workspace secrets, which every
// fetch from origin makes first to resolve its credential, records the
// rebuilds it is asked to queue, and writes its events through an emitter
// the test can make fail, which rolls back the transaction of the change.
type patchHub struct {
	*addHub
	rebuilds    *recordingRebuilds
	secretReads atomic.Int64
	failEvents  atomic.Bool
}

func startPatchHub(t *testing.T) *patchHub {
	t.Helper()
	h := &patchHub{
		addHub:   &addHub{readHub: &readHub{}, now: time.Now().UTC().Truncate(time.Second).Add(700 * time.Millisecond)},
		rebuilds: &recordingRebuilds{},
	}
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Now: h.clock,
		Compose: func(c *hub.Composition) {
			c.CarryPatch().RegisterRebuildEnqueuer(h.rebuilds)
			c.AdjustCarryPatch(func(d *carrypatch.Deps) {
				h.repoPath = d.RepoPath
				d.Secrets = countingSecrets{SecretReader: d.Secrets, n: &h.secretReads}
				d.Events = failingEmitter{Emitter: d.Events, fail: &h.failEvents}
			})
		},
	})
	h.owner = h.CreateUser(t, "owner")
	h.key = h.IssueKey(t, h.owner)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, h.owner)
	return h
}

// carryList stores the carry-patch workspace slug of the owner, in status
// and with the maintenance marker given, holding the active patches A, B
// and C of the branches patch/a, patch/b and patch/c at positions 1 to 3,
// and returns them by letter.
func (h *patchHub) carryList(t *testing.T, slug string, status workspace.Status, marker workspace.Marker) map[string]carrypatch.Patch {
	t.Helper()
	h.add(t, slug, workspace.ModeCarryPatch, status, marker)
	ps := map[string]carrypatch.Patch{}
	for i, letter := range []string{"A", "B", "C"} {
		ps[letter] = livePatch(slug, "patch/"+strings.ToLower(letter), i+1)
		h.put(t, ps[letter])
	}
	return ps
}

// patches are the stored patches of slug: those that are not deleted in
// position order, then the deleted ones.
func (h *patchHub) patches(t *testing.T, slug string) []carrypatch.Patch {
	t.Helper()
	ctx := context.Background()
	live, err := carrypatch.ListLivePatches(ctx, h.Store.Main(), slug)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := carrypatch.ListDeletedPatches(ctx, h.Store.Main(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return append(live, deleted...)
}

// stored is the stored patch id of slug.
func (h *patchHub) stored(t *testing.T, slug, id string) carrypatch.Patch {
	t.Helper()
	p, found, err := carrypatch.ReadPatch(context.Background(), h.Store.Main(), slug, id)
	if err != nil || !found {
		t.Fatalf("read the patch %s of %s: found %v, %v", id, slug, found, err)
	}
	return p
}

// alter changes the stored patch id of slug by edit, as an earlier change
// would have left it.
func (h *patchHub) alter(t *testing.T, slug, id string, edit func(p *carrypatch.Patch)) {
	t.Helper()
	ctx := context.Background()
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		p, found, err := carrypatch.ReadPatch(ctx, tx, slug, id)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%s has no patch %s", slug, id)
		}
		edit(&p)
		if err := carrypatch.DeletePatch(ctx, tx, slug, id); err != nil {
			return err
		}
		return carrypatch.InsertPatch(ctx, tx, p)
	}); err != nil {
		t.Fatalf("alter the patch %s of %s: %v", id, slug, err)
	}
}

// send sends method /api/v1+path with credential and body, and decodes the
// answer.
func (h *patchHub) send(t *testing.T, credential, method, path string, body any) answer {
	t.Helper()
	status, raw := h.Call(t, method, "/api/v1"+path, credential, body)
	a := answer{status: status, raw: raw}
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(raw, &a.patch)
	return a
}

// update sends PATCH /workspaces/<slug>/patches/<id> as the owner with body.
func (h *patchHub) update(t *testing.T, slug, id string, body any) answer {
	t.Helper()
	return h.send(t, h.key, http.MethodPatch, "/workspaces/"+slug+"/patches/"+id, body)
}

// patchEvents are the stored events of slug of type typ, every
// parley.patch.* event when typ is "", oldest first.
func (h *patchHub) patchEvents(t *testing.T, slug, typ string) []patchEvent {
	t.Helper()
	if typ == "" {
		typ = "parley.patch.%"
	}
	rows, err := h.Store.Main().Query(`SELECT resource_type, resource_id, actor_type, actor_id, metadata
FROM hub_events WHERE workspace = ? AND type LIKE ? ORDER BY seq`, slug, typ)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []patchEvent
	for rows.Next() {
		var e patchEvent
		var meta string
		if err := rows.Scan(&e.ResourceType, &e.ResourceID, &e.ActorType, &e.ActorID, &meta); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(meta), &e.Metadata); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// recordingRebuilds is a rebuild enqueuer that records each call and
// queues nothing.
type recordingRebuilds struct {
	mu    sync.Mutex
	slugs []string
	opts  []carrypatch.RebuildOptions
}

func (r *recordingRebuilds) EnqueueRebuild(_ context.Context, slug string, opts carrypatch.RebuildOptions) (carrypatch.RebuildResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slugs, r.opts = append(r.slugs, slug), append(r.opts, opts)
	return carrypatch.RebuildResult{}, nil
}

// calls is how often a rebuild was asked for.
func (r *recordingRebuilds) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.slugs)
}

// countingSecrets is a secret reader that counts its reads.
type countingSecrets struct {
	workspace.SecretReader
	n *atomic.Int64
}

func (s countingSecrets) Get(ctx context.Context, scope secret.Scope, scopeID, name string) (string, bool, error) {
	s.n.Add(1)
	return s.SecretReader.Get(ctx, scope, scopeID, name)
}

// failingEmitter is an emitter that refuses every write while fail is set.
type failingEmitter struct {
	carrypatch.Emitter
	fail *atomic.Bool
}

func (e failingEmitter) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	if e.fail.Load() {
		return event.Record{}, errors.New("the event log refuses the write")
	}
	return e.Emitter.Emit(ctx, tx, in)
}
