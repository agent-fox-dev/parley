package carrypatch_test

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// TestMain runs the package's tests at most hubtest.HubParallel at a time:
// they start many hubs, which contend for one lock past that (see
// hubtest.HubParallel).
func TestMain(m *testing.M) {
	hubtest.LimitParallel(hubtest.HubParallel)
	os.Exit(m.Run())
}

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
// fetch from origin makes first to resolve its credential, takes the origin
// credentials from origins, records the rebuilds it is asked to queue, and
// writes its events through an emitter the test can make fail, which rolls
// back the transaction of the change. Its git trusts the origins given to
// trust.
type patchHub struct {
	*addHub
	rebuilds    *recordingRebuilds
	secretReads atomic.Int64
	failEvents  atomic.Bool
	origins     *originSecrets

	// caBundle is the file GIT_SSL_CAINFO names for the hub's git; cas are
	// the certificates it holds.
	caBundle string
	caMu     sync.Mutex
	cas      []byte
}

// startPatchHub starts the patch hub, with the carry-patch dependencies
// changed further by adjust.
func startPatchHub(t *testing.T, adjust ...func(d *carrypatch.Deps)) *patchHub {
	t.Helper()
	h := &patchHub{
		addHub:   &addHub{readHub: &readHub{}, now: time.Now().UTC().Truncate(time.Second).Add(700 * time.Millisecond)},
		rebuilds: &recordingRebuilds{},
		caBundle: filepath.Join(t.TempDir(), "ca-bundle.pem"),
	}
	if err := os.WriteFile(h.caBundle, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Now: h.clock,
		// The order tests send thousands of requests as one caller, more
		// than the rate limit lets through.
		TOML: "[limits]\nrequests_per_minute = 0\n",
		Compose: func(c *hub.Composition) {
			c.CarryPatch().RegisterRebuildEnqueuer(h.rebuilds)
			c.AdjustCarryPatch(func(d *carrypatch.Deps) {
				h.repoPath = d.RepoPath
				h.origins = &originSecrets{SecretReader: d.Secrets, tokens: map[string]string{}, fail: map[string]error{}, reads: map[string]int{}}
				d.Secrets = countingSecrets{SecretReader: h.origins, n: &h.secretReads}
				d.Events = failingEmitter{Emitter: d.Events, fail: &h.failEvents}
				d.Git.Env = append(slices.Clone(d.Git.Env), "GIT_SSL_CAINFO="+h.caBundle)
				for _, fn := range adjust {
					fn(d)
				}
			})
		},
	})
	h.owner = h.CreateUser(t, "owner")
	h.key = h.IssueKey(t, h.owner)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, h.owner)
	h.rebuilds.observe = func(slug string) rebuildSeen {
		var resets int
		if err := h.Store.Main().QueryRow(`SELECT count(*) FROM hub_events WHERE workspace = ? AND type = ?`,
			slug, carrypatch.EventPatchReset).Scan(&resets); err != nil {
			t.Errorf("count the reset events of %s: %v", slug, err)
		}
		return rebuildSeen{head: refIn(h.repoPath(slug), "refs/heads/patch/a"), resets: resets}
	}
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

// carryOf stores the ready carry-patch workspace slug of the owner holding
// n active patches, of the branches patch/1 to patch/n, at positions 1 to n,
// and returns them in position order.
func (h *patchHub) carryOf(t *testing.T, slug string, n int) []carrypatch.Patch {
	t.Helper()
	h.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone)
	ps := make([]carrypatch.Patch, n)
	for i := range ps {
		ps[i] = livePatch(slug, fmt.Sprintf("patch/%d", i+1), i+1)
	}
	h.put(t, ps...)
	return ps
}

// bareCopy makes the hub's copy of slug a bare repository made by git init
// holding the branches patch/a, patch/b and patch/c of carryList and the
// backup ref of patch/a, and returns its directory.
func (h *patchHub) bareCopy(t *testing.T, slug string) string {
	t.Helper()
	dir := h.repoPath(slug)
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	for _, b := range []string{"patch/a", "patch/b", "patch/c"} {
		gitrepotest.Git(t, dir, "update-ref", "refs/heads/"+b, commitIn(t, dir, b))
	}
	gitrepotest.Git(t, dir, "update-ref", carrypatch.ReplacedRefPrefix+"patch/a", commitIn(t, dir, "replaced a"))
	return dir
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

// remove sends DELETE /workspaces/<slug>/patches/<id> as the owner with
// body, nil for none.
func (h *patchHub) remove(t *testing.T, slug, id string, body any) answer {
	t.Helper()
	return h.send(t, h.key, http.MethodDelete, "/workspaces/"+slug+"/patches/"+id, body)
}

// restore sends POST /workspaces/<slug>/patches/<id>/restore as the owner
// with body, nil for none.
func (h *patchHub) restore(t *testing.T, slug, id string, body any) answer {
	t.Helper()
	return h.send(t, h.key, http.MethodPost, "/workspaces/"+slug+"/patches/"+id+"/restore", body)
}

// reorder sends POST /workspaces/<slug>/patches/reorder as the owner with
// body.
func (h *patchHub) reorder(t *testing.T, slug string, body any) answer {
	t.Helper()
	return h.send(t, h.key, http.MethodPost, "/workspaces/"+slug+"/patches/reorder", body)
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

// trust makes the hub's git trust the certificate in the PEM file caFile,
// such as an origin's CAFile, besides those it trusts already. The bundle
// is replaced in one step, so that a git reading it meanwhile sees the old
// or the new one.
func (h *patchHub) trust(t *testing.T, caFile string) {
	t.Helper()
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	h.caMu.Lock()
	defer h.caMu.Unlock()
	h.cas = append(h.cas, pem...)
	next := h.caBundle + ".next"
	if err := os.WriteFile(next, h.cas, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, h.caBundle); err != nil {
		t.Fatal(err)
	}
}

// resetCase is a carry-patch workspace of the reset tests: its origin,
// which takes the GIT_PAT originToken alone, the hub's copy of it, and its
// patch A of the branch patch/a.
type resetCase struct {
	slug   string
	origin *gitrepotest.Origin
	copy   string
	patch  carrypatch.Patch
}

// resetCarry stores the ready carry-patch workspace slug of the owner,
// changed by edit when it is not nil, whose git_url is a new origin that
// holds the branches patch/a and other at one commit each, whose GIT_PAT is
// originToken, and whose copy, the bare copy of carry, also holds
// origin's branches and origin heads of them, as a clone of origin would.
// It holds patch A of patch/a, active, at position 1. The hub's git trusts
// the origin.
func (h *patchHub) resetCarry(t *testing.T, slug string, edit func(w *workspace.Workspace)) *resetCase {
	t.Helper()
	o := tokenOrigin(t)
	o.Commit("patch/a", "a1")
	o.Commit("other", "o1")
	h.trust(t, o.CAFile())
	url := o.URL()
	dir := h.carry(t, slug, func(w *workspace.Workspace) {
		w.GitURL = &url
		if edit != nil {
			edit(w)
		}
	})
	gitrepotest.Git(t, dir, "fetch", "--quiet", "--no-tags", o.Dir(), "+refs/heads/*:refs/heads/*", "+refs/heads/*:refs/remotes/origin/*")
	h.origins.set(slug, originToken, nil)
	p := livePatch(slug, "patch/a", 1)
	h.put(t, p)
	return &resetCase{slug: slug, origin: o, copy: dir, patch: p}
}

// autoRebuild is an edit of resetCarry that stores the setting
// auto_rebuild_after_sync on.
func autoRebuild(on bool) func(w *workspace.Workspace) {
	return func(w *workspace.Workspace) {
		w.Settings = map[string]json.RawMessage{"auto_rebuild_after_sync": json.RawMessage(fmt.Sprint(on))}
	}
}

// withSettings is an edit of resetCarry that stores the settings
// patch_branch_source source and patch_divergence_policy policy, "" for
// none.
func withSettings(source, policy string) func(w *workspace.Workspace) {
	return func(w *workspace.Workspace) {
		w.Settings = map[string]json.RawMessage{}
		if source != "" {
			w.Settings["patch_branch_source"] = json.RawMessage(`"` + source + `"`)
		}
		if policy != "" {
			w.Settings["patch_divergence_policy"] = json.RawMessage(`"` + policy + `"`)
		}
	}
}

// resetToOrigin sends POST /workspaces/<slug>/patches/<id>/reset-to-origin
// as the owner with no body.
func (h *patchHub) resetToOrigin(t *testing.T, slug, id string) answer {
	t.Helper()
	return h.send(t, h.key, http.MethodPost, "/workspaces/"+slug+"/patches/"+id+"/reset-to-origin", nil)
}

// originSecrets is the origin credential resolver of the patch hub: the
// GIT_PAT of a workspace given to set is its token, or its error; the
// secrets of every other workspace are read from the hub's reader. It
// counts the reads per workspace.
type originSecrets struct {
	workspace.SecretReader
	mu     sync.Mutex
	tokens map[string]string
	fail   map[string]error
	reads  map[string]int
}

// set makes token the GIT_PAT of slug, or, when err is not nil, every read
// of its secrets fail with err.
func (s *originSecrets) set(slug, token string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[slug], s.fail[slug] = token, err
}

// readsOf is how often the secrets of slug were read.
func (s *originSecrets) readsOf(slug string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[slug]
}

func (s *originSecrets) Get(ctx context.Context, scope secret.Scope, scopeID, name string) (string, bool, error) {
	s.mu.Lock()
	token, set := s.tokens[scopeID]
	err := s.fail[scopeID]
	s.reads[scopeID]++
	s.mu.Unlock()
	switch {
	case !set || scope != secret.ScopeWorkspace:
		return s.SecretReader.Get(ctx, scope, scopeID, name)
	case err != nil:
		return "", false, err
	case name == workspace.SecretGitPAT:
		return token, true, nil
	}
	return "", false, nil
}

// recordingRebuilds is a rebuild enqueuer that records each call with
// what the hub held when it was made, and answers a workspace as answer
// set it: by default, that nothing was queued.
type recordingRebuilds struct {
	mu      sync.Mutex
	made    []rebuildCall
	answers map[string]rebuildAnswer
	// observe, when set, reads what the hub holds for a workspace when a
	// rebuild of it is asked for.
	observe func(slug string) rebuildSeen
}

// rebuildCall is a call of the recording enqueuer.
type rebuildCall struct {
	slug string
	opts carrypatch.RebuildOptions
	seen rebuildSeen
}

// rebuildSeen is what the hub held when a rebuild of a workspace was asked
// for: the tip of patch/a in its copy, the branch of the reset cases, and
// how many parley.patch.reset events it had.
type rebuildSeen struct {
	head   string
	resets int
}

// rebuildAnswer is what the recording enqueuer answers a workspace.
type rebuildAnswer struct {
	result carrypatch.RebuildResult
	err    error
}

// answer makes every later call for slug answer result and err.
func (r *recordingRebuilds) answer(slug string, result carrypatch.RebuildResult, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.answers == nil {
		r.answers = map[string]rebuildAnswer{}
	}
	r.answers[slug] = rebuildAnswer{result, err}
}

func (r *recordingRebuilds) EnqueueRebuild(_ context.Context, slug string, opts carrypatch.RebuildOptions) (carrypatch.RebuildResult, error) {
	var seen rebuildSeen
	if r.observe != nil {
		seen = r.observe(slug)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.made = append(r.made, rebuildCall{slug: slug, opts: opts, seen: seen})
	a := r.answers[slug]
	return a.result, a.err
}

// calls is how often a rebuild was asked for.
func (r *recordingRebuilds) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.made)
}

// callsOf are the calls that asked for a rebuild of slug, oldest first.
func (r *recordingRebuilds) callsOf(slug string) []rebuildCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []rebuildCall
	for _, c := range r.made {
		if c.slug == slug {
			out = append(out, c)
		}
	}
	return out
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

// rebuildHub is an addHub for the rebuild routes. Its job queue stamps jobs
// with the hub's clock and passes only when a test calls pass: its
// interval timer never fires and nothing wakes it. Its runner is the
// handler of rebuild jobs: an attempt of a job a test scripted (drive)
// does what the test says, any other completes at once. Its carry-patch
// component writes events through an emitter the test can make fail, and
// its git records every command it runs.
type rebuildHub struct {
	*addHub
	comp       *carrypatch.Component
	queue      *jobs.Queue
	runner     *rebuildRunner
	failEvents atomic.Bool
	// rollbackHooks are the hooks onRollbackSwap gave workspaces, by slug.
	rollbackHooks sync.Map
	// trialMerges are the failures onTrialMerge gave copies, by directory.
	trialMerges sync.Map
	// gitRun are the arguments of every git command of the carry-patch
	// component, in the order they ran.
	gitMu  sync.Mutex
	gitRun [][]string
}

// startRebuildHub starts a rebuild hub with an owner who holds an API key
// and whose organization owns the workspaces it stores.
func startRebuildHub(t *testing.T) *rebuildHub {
	t.Helper()
	h := &rebuildHub{
		addHub: &addHub{readHub: &readHub{}, now: time.Now().UTC().Truncate(time.Second).Add(700 * time.Millisecond)},
		runner: &rebuildRunner{attempts: map[string]*rebuildAttempt{}, calls: map[string]int{}, done: make(chan struct{})},
	}
	// The queue's timers never fire: the test does not advance its clock.
	clock := hubtest.NewJobClock(h.clock)
	h.Hub = hubtest.StartWith(t, hubtest.Config{
		Now:  h.clock,
		TOML: "[limits]\nrequests_per_minute = 0\n",
		Compose: func(c *hub.Composition) {
			h.comp, h.queue = c.CarryPatch(), c.Jobs()
			h.queue.Register(carrypatch.RebuildJobType, h.runner)
			c.AdjustJobs(func(d *jobs.Deps) { d.Clock, d.NoWake = clock, true })
			c.AdjustCarryPatch(func(d *carrypatch.Deps) {
				h.repoPath = d.RepoPath
				d.Events = failingEmitter{Emitter: d.Events, fail: &h.failEvents}
				d.BeforeRollbackSwap = h.beforeRollbackSwap
				d.TrialMerge = h.trialMerge
				trace := d.Git.Trace
				d.Git.Trace = func(args []string) {
					if trace != nil {
						trace(args)
					}
					h.gitMu.Lock()
					defer h.gitMu.Unlock()
					h.gitRun = append(h.gitRun, args)
				}
			})
		},
	})
	// Registered after the hub's own clean-up, so it runs first: the
	// attempts still running end before the hub's stop waits for them.
	t.Cleanup(h.runner.stop)
	// The dispatcher's own pass at its start could claim a test's first
	// job; from here on only the test's passes do.
	hubtest.WaitClosed(t, clock.Armed(jobs.DefaultInterval), "the job queue's first pass")
	h.owner = h.CreateUser(t, "owner")
	h.key = h.IssueKey(t, h.owner)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, h.owner)
	return h
}

// carryWith stores the ready carry-patch workspace slug of the owner, whose
// integration branch is deploy and whose stored settings are settings
// (none when nil), holding one patch of the branch patch/<n> in each status
// of statuses, in order.
func (h *rebuildHub) carryWith(t *testing.T, slug string, settings map[string]string, statuses ...string) {
	t.Helper()
	h.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, func(w *workspace.Workspace) {
		w.IntegrationBranch = new("deploy")
		if settings != nil {
			w.Settings = map[string]json.RawMessage{}
			for k, v := range settings {
				w.Settings[k] = json.RawMessage(`"` + v + `"`)
			}
		}
	})
	var ps []carrypatch.Patch
	for i, status := range statuses {
		branch := fmt.Sprintf("patch/%d", i+1)
		if status == carrypatch.StatusDeleted {
			ps = append(ps, deletedPatch(slug, branch, at(10, 0)))
			continue
		}
		p := livePatch(slug, branch, len(ps)+1)
		p.Status = status
		ps = append(ps, p)
	}
	h.put(t, ps...)
}

// onRollbackSwap makes fn run when a rollback of slug has read the head of
// the integration branch and is about to move it.
func (h *rebuildHub) onRollbackSwap(slug string, fn func()) { h.rollbackHooks.Store(slug, fn) }

func (h *rebuildHub) beforeRollbackSwap(slug, _ string) {
	if fn, ok := h.rollbackHooks.Load(slug); ok {
		fn.(func())()
	}
}

// onTrialMerge makes fail decide each trial merge of a preview of the copy
// whose git directory is dir: the merge of theirs onto ours fails with the
// error fail returns, and runs when that is nil.
func (h *rebuildHub) onTrialMerge(dir string, fail func(ours, theirs string) error) {
	h.trialMerges.Store(filepath.Clean(dir), fail)
}

func (h *rebuildHub) trialMerge(ctx context.Context, repo *gitrepo.Repo, ours, theirs string) (gitrepo.TrialMergeResult, error) {
	if fail, ok := h.trialMerges.Load(repo.Dir()); ok {
		if err := fail.(func(string, string) error)(ours, theirs); err != nil {
			return gitrepo.TrialMergeResult{}, err
		}
	}
	return repo.TrialMerge(ctx, ours, theirs)
}

// gitCommands are the git commands the carry-patch component ran so far,
// each its arguments joined by spaces.
func (h *rebuildHub) gitCommands() []string {
	h.gitMu.Lock()
	defer h.gitMu.Unlock()
	out := make([]string, 0, len(h.gitRun))
	for _, args := range h.gitRun {
		out = append(out, strings.Join(args, " "))
	}
	return out
}

// rollbackRepo makes the hub's copy of slug a bare repository made by git
// init whose integration branch deploy is at the commit H2, whose parent is
// H1, and returns its directory, H1 and H2.
func (h *rebuildHub) rollbackRepo(t *testing.T, slug string) (dir, h1, h2 string) {
	t.Helper()
	dir = h.repoPath(slug)
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	h1 = gitrepotest.Commit(t, dir, "H1", map[string]string{"a.txt": "1\n"})
	h2 = gitrepotest.Commit(t, dir, "H2", map[string]string{"a.txt": "2\n"}, h1)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/deploy", h2)
	return dir, h1, h2
}

// rebuiltFrom is the result document of a rebuild that moved the
// integration branch from previous, "" for null, to head.
func rebuiltFrom(previous, head string) string {
	prev := "null"
	if previous != "" {
		prev = `"` + previous + `"`
	}
	return `{"integration_head_sha":"` + head + `","previous_integration_head_sha":` + prev + `}`
}

// settledRebuild queues a rebuild of slug whose attempt stores the result
// document result, none when "", and returns err, and returns the job's id
// once the outcome is settled: completed for nil, failed for a retryable
// error, dead_letter for a permanent one.
func (h *rebuildHub) settledRebuild(t *testing.T, slug, result string, err error) string {
	t.Helper()
	id := h.queueRebuild(t, slug)
	h.drive(t, id, jobs.StatusRunning)
	if result != "" {
		if err := h.queue.SetResult(context.Background(), id, json.RawMessage(result)); err != nil {
			t.Fatalf("store the result of %s: %v", id, err)
		}
	}
	h.finish(t, id, err)
	return id
}

// rollback sends POST /workspaces/<slug>/rebuilds/<id>/rollback with
// credential.
func (h *rebuildHub) rollback(t *testing.T, credential, slug, id string) answer {
	t.Helper()
	return h.call(t, credential, http.MethodPost, "/workspaces/"+slug+"/rebuilds/"+id+"/rollback", nil, nil)
}

// configure stores settings among the settings of slug through the
// workspace's update route.
func (h *rebuildHub) configure(t *testing.T, slug string, settings map[string]string) {
	t.Helper()
	if status, raw := h.Call(t, http.MethodPatch, "/api/v1/workspaces/"+slug, h.key, map[string]any{"settings": settings}); status != http.StatusOK {
		t.Fatalf("set the settings %v of %s = %d %s, want 200", settings, slug, status, raw)
	}
}

// call sends method /api/v1+path with credential, body and the headers
// header, and decodes the answer: a record is in patch.
func (h *rebuildHub) call(t *testing.T, credential, method, path string, body any, header http.Header) answer {
	t.Helper()
	status, raw := h.CallWith(t, method, "/api/v1"+path, credential, body, header)
	a := answer{status: status, raw: raw}
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(raw, &a.patch)
	return a
}

// submit sends POST /workspaces/<slug>/rebuild as the owner with body (nil
// for none) and the headers header.
func (h *rebuildHub) submit(t *testing.T, slug string, body any, header http.Header) answer {
	t.Helper()
	return h.call(t, h.key, http.MethodPost, "/workspaces/"+slug+"/rebuild", body, header)
}

// readRebuild sends GET /workspaces/<slug>/rebuilds/<id> as the owner.
func (h *rebuildHub) readRebuild(t *testing.T, slug, id string) answer {
	t.Helper()
	return h.call(t, h.key, http.MethodGet, "/workspaces/"+slug+"/rebuilds/"+id, nil, nil)
}

// cancel sends DELETE /workspaces/<slug>/rebuilds/<id> as the owner.
func (h *rebuildHub) cancel(t *testing.T, slug, id string) answer {
	t.Helper()
	return h.call(t, h.key, http.MethodDelete, "/workspaces/"+slug+"/rebuilds/"+id, nil, nil)
}

// requeue sends POST /workspaces/<slug>/rebuilds/<id>/requeue as the owner.
func (h *rebuildHub) requeue(t *testing.T, slug, id string) answer {
	t.Helper()
	return h.call(t, h.key, http.MethodPost, "/workspaces/"+slug+"/rebuilds/"+id+"/requeue", nil, nil)
}

// mustSubmit submits a rebuild of slug with no body, fails the test unless
// the answer is 202, and returns the job's id.
func (h *rebuildHub) mustSubmit(t *testing.T, slug string) string {
	t.Helper()
	a := h.submit(t, slug, nil, nil)
	id, _ := a.patch["id"].(string)
	if a.status != http.StatusAccepted || id == "" {
		t.Fatalf("submit a rebuild of %s = %d %s, want 202 with the job", slug, a.status, a.raw)
	}
	return id
}

// queueRebuild queues a rebuild of slug through the shared enqueue, which
// looks neither at the workspace's state nor for an active patch, and
// returns the job's id.
func (h *rebuildHub) queueRebuild(t *testing.T, slug string) string {
	t.Helper()
	res, err := h.comp.EnqueueRebuild(context.Background(), slug, carrypatch.RebuildOptions{ActorType: event.ActorSystem, SubmittedBy: event.ActorSystem})
	if err != nil || !res.Queued {
		t.Fatalf("queue a rebuild of %s = %+v, %v, want a queued job", slug, res, err)
	}
	return res.JobID
}

// setState stores status and the maintenance marker on the record of the
// workspace slug, as a clone or a maintenance operation would leave it.
func (h *readHub) setState(t *testing.T, slug string, status workspace.Status, marker workspace.Marker) {
	t.Helper()
	if err := h.storeState(slug, status, marker); err != nil {
		t.Fatalf("set the state of %s: %v", slug, err)
	}
}

// storeState is setState for a caller that must not fail the test, such
// as a hook the code under test calls from another goroutine.
func (h *readHub) storeState(slug string, status workspace.Status, marker workspace.Marker) error {
	ctx := context.Background()
	return h.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ?, maintenance = ? WHERE slug = ?`, status, marker, slug)
		return err
	})
}

// job is the stored job id.
func (h *rebuildHub) job(t *testing.T, id string) jobs.Job {
	t.Helper()
	j, err := h.queue.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read the job %s: %v", id, err)
	}
	return j
}

// jobsOf are the rebuild jobs of slug, newest first.
func (h *rebuildHub) jobsOf(t *testing.T, slug string) []jobs.Job {
	t.Helper()
	js, err := h.queue.List(context.Background(), carrypatch.RebuildJobType, slug, "", nil, 200)
	if err != nil {
		t.Fatalf("list the rebuild jobs of %s: %v", slug, err)
	}
	return js
}

// recordOf is the stored job id as the API shows a rebuild job.
func (h *rebuildHub) recordOf(t *testing.T, id string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(carrypatch.RebuildRecordOf(h.job(t, id)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// rebuildEvent is a stored parley.rebuild.* event.
type rebuildEvent struct {
	ResourceType, ResourceID, ActorType string
	ActorID, SessionID                  *string
	Metadata                            map[string]any
}

// rebuildEvents are the stored events of slug of type typ, oldest first.
func (h *readHub) rebuildEvents(t *testing.T, slug, typ string) []rebuildEvent {
	t.Helper()
	rows, err := h.Store.Main().Query(`SELECT resource_type, resource_id, actor_type, actor_id, session_id, metadata
FROM hub_events WHERE workspace = ? AND type = ? ORDER BY seq`, slug, typ)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rebuildEvent
	for rows.Next() {
		var e rebuildEvent
		var meta string
		if err := rows.Scan(&e.ResourceType, &e.ResourceID, &e.ActorType, &e.ActorID, &e.SessionID, &meta); err != nil {
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

// pass runs one pass of the job queue.
func (h *rebuildHub) pass(t *testing.T) {
	t.Helper()
	if err := h.queue.Pass(context.Background()); err != nil {
		t.Fatalf("pass the job queue: %v", err)
	}
}

// drive moves the queued job id to status through the queue: a pass claims
// it and its attempt runs on (running, until finish or the end of the
// test), completes (completed), fails and waits for its retry (failed) or
// fails for good (dead_letter); a cancel cancels it (cancelled).
func (h *rebuildHub) drive(t *testing.T, id, status string) {
	t.Helper()
	switch status {
	case jobs.StatusCancelled:
		ctx := context.Background()
		if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
			_, err := h.queue.Cancel(ctx, tx, id)
			return err
		}); err != nil {
			t.Fatalf("cancel the job %s: %v", id, err)
		}
	case jobs.StatusRunning:
		a := h.runner.expect(id)
		// Ends with the test, unless the test finishes it first.
		t.Cleanup(func() {
			select {
			case a.outcome <- nil:
			default:
			}
		})
		h.pass(t)
		hubtest.WaitClosed(t, a.entered, "the attempt of the job "+id+" to begin")
	case jobs.StatusCompleted:
		h.attempt(t, id, nil)
	case jobs.StatusFailed:
		h.attempt(t, id, errors.New("the upstream is unreachable"))
	case jobs.StatusDeadLetter:
		h.attempt(t, id, jobs.Permanent(errors.New("the patch list cannot be applied")))
	default:
		t.Fatalf("drive the job %s to %s: not a status a test drives a job to", id, status)
	}
	h.wantStatus(t, id, status)
}

// attempt runs an attempt of the job id, queued or due for its retry, that
// returns err, and waits until its outcome is settled.
func (h *rebuildHub) attempt(t *testing.T, id string, err error) {
	t.Helper()
	a := h.runner.expect(id)
	a.outcome <- err
	h.pass(t)
	hubtest.WaitClosed(t, a.entered, "the attempt of the job "+id+" to begin")
	h.settled(t, a)
}

// fail runs n attempts of the job id that fail with a retryable error, and
// moves the clock on to the next attempt after each: the job is then due
// for that attempt with the retry count n.
func (h *rebuildHub) fail(t *testing.T, id string, n int) {
	t.Helper()
	for range n {
		h.attempt(t, id, errors.New("fetch failed"))
		h.advance(h.job(t, id).NextAttemptAt.Sub(h.clock()))
	}
}

// finish ends the running attempt of the job id, which drive started, with
// err, and waits until its outcome is settled.
func (h *rebuildHub) finish(t *testing.T, id string, err error) {
	t.Helper()
	a := h.runner.expect(id)
	a.outcome <- err
	h.settled(t, a)
}

// settled waits until the outcome of the attempt a is settled and the
// settlement has committed: the store runs its write transactions one at a
// time, in the order they are asked for, so an empty one asked for once
// the settlement hook ran ends after the settlement.
func (h *rebuildHub) settled(t *testing.T, a *rebuildAttempt) {
	t.Helper()
	hubtest.WaitClosed(t, a.settled, "the outcome of the attempt to be settled")
	if err := h.Store.Tx(context.Background(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// wantStatus fails the test unless the job id is in status.
func (h *rebuildHub) wantStatus(t *testing.T, id, status string) {
	t.Helper()
	if got := h.job(t, id).Status; got != status {
		t.Fatalf("the job %s is %s, want %s", id, got, status)
	}
}

// rebuildRunner is the handler of rebuild jobs of the rebuild hub. An
// attempt of a job expect scripted returns what the test sends on its
// outcome, or nil once the runner stops; an attempt of any other job
// completes at once, so that a pass a test runs for its own job does not
// leave another test's job holding a worker. It counts the attempts of
// every job.
type rebuildRunner struct {
	mu       sync.Mutex
	attempts map[string]*rebuildAttempt
	calls    map[string]int
	done     chan struct{}
	stopOnce sync.Once
}

// rebuildAttempt is the script of a job's attempt.
type rebuildAttempt struct {
	// outcome takes what the attempt returns.
	outcome chan error
	// entered is closed once the attempt has begun, settled once its
	// outcome is being settled.
	entered, settled         chan struct{}
	enteredOnce, settledOnce sync.Once
	// payload is the payload the attempt was given.
	payload json.RawMessage
}

// expect scripts the attempt of the job id, and returns its script: that
// of the attempt scripted last, unless it has been settled, when it scripts
// the next.
func (r *rebuildRunner) expect(id string) *rebuildAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.attempts[id]
	if ok {
		select {
		case <-a.settled:
			ok = false
		default:
		}
	}
	if !ok {
		a = &rebuildAttempt{outcome: make(chan error, 1), entered: make(chan struct{}), settled: make(chan struct{})}
		r.attempts[id] = a
	}
	return a
}

// scripted is the script of the job id, nil when it has none.
func (r *rebuildRunner) scripted(id string) *rebuildAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts[id]
}

// payloadOf is the payload the attempt of the scripted job id was given.
func (r *rebuildRunner) payloadOf(id string) json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.attempts[id]; a != nil {
		return a.payload
	}
	return nil
}

// callsOf is the number of attempts of the job id the runner was called
// for.
func (r *rebuildRunner) callsOf(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[id]
}

// stop ends every attempt still running.
func (r *rebuildRunner) stop() { r.stopOnce.Do(func() { close(r.done) }) }

func (r *rebuildRunner) Run(ctx context.Context, j jobs.Job) error {
	r.mu.Lock()
	r.calls[j.ID]++
	a := r.attempts[j.ID]
	if a != nil {
		a.payload = j.Payload
	}
	r.mu.Unlock()
	if a == nil {
		return nil
	}
	a.enteredOnce.Do(func() { close(a.entered) })
	select {
	case err := <-a.outcome:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return nil
	}
}

func (r *rebuildRunner) Settled(_ context.Context, _ *sql.Tx, j jobs.Job, _ bool) error {
	if a := r.scripted(j.ID); a != nil {
		a.settledOnce.Do(func() { close(a.settled) })
	}
	return nil
}

// histCommit is a commit writeHistory writes: on top of the commit of the
// earlier entry at index from (-1 for a root commit), whose tree it takes
// with files holding the content given and deleted removed; refs are the
// full names of the refs that end at it.
type histCommit struct {
	from    int
	files   map[string]string
	deleted []string
	refs    []string
}

// writeHistory writes commits, and the refs they name, into the
// repository whose git directory is dir in one run of git fast-import,
// rather than one git process per object, and returns their ids in order.
func writeHistory(t *testing.T, dir string, commits []histCommit) []string {
	t.Helper()
	data := func(content string) string { return fmt.Sprintf("data %d\n%s\n", len(content), content) }
	var s strings.Builder
	for i, c := range commits {
		// Each commit has a branch of its own, so that a root commit has no
		// parent; the resets at the end leave those branches unwritten.
		fmt.Fprintf(&s, "commit refs/history/%d\nmark :%d\ncommitter T <t@example.invalid> 1700000000 +0000\n%s", i, i+1, data(fmt.Sprintf("c%d\n", i)))
		if c.from >= 0 {
			fmt.Fprintf(&s, "from :%d\n", c.from+1)
		}
		for _, name := range slices.Sorted(maps.Keys(c.files)) {
			fmt.Fprintf(&s, "M 100644 inline %s\n%s", name, data(c.files[name]))
		}
		for _, name := range c.deleted {
			fmt.Fprintf(&s, "D %s\n", name)
		}
		s.WriteString("\n")
	}
	for i, c := range commits {
		fmt.Fprintf(&s, "reset refs/history/%d\n\n", i)
		for _, ref := range c.refs {
			fmt.Fprintf(&s, "reset %s\nfrom :%d\n\n", ref, i+1)
		}
		fmt.Fprintf(&s, "get-mark :%d\n", i+1)
	}
	out, err := gitrepotest.Run(dir, s.String(), "fast-import", "--quiet")
	if err != nil {
		t.Fatalf("write the history into %s: %v", dir, err)
	}
	ids := strings.Fields(out)
	if len(ids) != len(commits) {
		t.Fatalf("fast-import answered %q for %d commits", out, len(commits))
	}
	return ids
}

// blobID is the id git gives a blob of content.
func blobID(content string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content))))
}

// previewCopy makes the hub's copy of slug a bare repository made by git
// init that holds commits, written by writeHistory, and returns its
// directory and the commits' ids.
func (h *rebuildHub) previewCopy(t *testing.T, slug string, commits []histCommit) (dir string, ids []string) {
	t.Helper()
	dir = h.repoPath(slug)
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	return dir, writeHistory(t, dir, commits)
}

// preview sends GET /workspaces/<slug>/rebuild-preview as the owner.
func (h *rebuildHub) preview(t *testing.T, slug string) answer {
	t.Helper()
	return h.call(t, h.key, http.MethodGet, "/workspaces/"+slug+"/rebuild-preview", nil, nil)
}
