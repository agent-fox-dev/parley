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
	"io"
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
// earlier entry at index from, or, for from -1, of the commit parent the
// repository holds already (a root commit when parent is ""), whose tree
// it takes with files holding the content given and deleted removed; refs
// are the full names of the refs that end at it, and msg its message,
// c<index> when "".
type histCommit struct {
	from    int
	parent  string
	msg     string
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
		msg := c.msg
		if msg == "" {
			msg = fmt.Sprintf("c%d", i)
		}
		fmt.Fprintf(&s, "commit refs/history/%d\nmark :%d\ncommitter T <t@example.invalid> 1700000000 +0000\n%s", i, i+1, data(msg+"\n"))
		switch {
		case c.from >= 0:
			fmt.Fprintf(&s, "from :%d\n", c.from+1)
		case c.parent != "":
			fmt.Fprintf(&s, "from %s\n", c.parent)
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

// syncFixture is the rebuild fixture for the sync of its workspace, whose
// upstream fetches it counts and lets a test look in on.
type syncFixture struct {
	*rebuildFixture
	fetches *upstreamFetches
}

// upstreamFetches counts the upstream fetches of the carry-patch
// component, and the most that ran at once, and calls onFetch, when set, at
// the start of each, before git runs.
type upstreamFetches struct {
	mu      sync.Mutex
	calls   int
	running int
	most    int
	onFetch func()
}

// mostAtOnce is the most upstream fetches that ran at the same time since
// the fixture started.
func (u *upstreamFetches) mostAtOnce() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.most
}

// count is the number of upstream fetches since the fixture started.
func (u *upstreamFetches) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// before makes fn run at the start of every upstream fetch from now on,
// on the goroutine of the fetch; nil runs nothing.
func (u *upstreamFetches) before(fn func()) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.onFetch = fn
}

// syncOptions are what a test changes of the sync fixture: the carry-patch
// component's dependencies, changed by deps after the fixture's own
// changes, and the composition, changed by compose after the fixture's own.
type syncOptions struct {
	deps    func(d *carrypatch.Deps)
	compose func(c *hub.Composition)
}

// startSyncFixture starts the sync fixture, with the workspace's record
// changed by edit before it is stored.
func startSyncFixture(t *testing.T, edit ...func(w *workspace.Workspace)) *syncFixture {
	t.Helper()
	return startSyncFixtureWith(t, syncOptions{}, edit...)
}

// startSyncFixtureWith starts the sync fixture with o, with the
// workspace's record changed by edit before it is stored.
func startSyncFixtureWith(t *testing.T, o syncOptions, edit ...func(w *workspace.Workspace)) *syncFixture {
	t.Helper()
	u := &upstreamFetches{}
	f := &syncFixture{fetches: u}
	f.rebuildFixture = startFixture(t, fixtureOptions{
		compose: o.compose,
		deps: func(d *carrypatch.Deps) {
			fetch := d.FetchRemote
			d.FetchRemote = func(ctx context.Context, dir, url, name string, cred gitrepo.Credential, deadline time.Duration) (map[string]string, error) {
				u.mu.Lock()
				u.calls++
				u.running++
				u.most = max(u.most, u.running)
				fn := u.onFetch
				u.mu.Unlock()
				defer func() {
					u.mu.Lock()
					u.running--
					u.mu.Unlock()
				}()
				if fn != nil {
					fn()
				}
				return fetch(ctx, dir, url, name, cred, deadline)
			}
			if o.deps != nil {
				o.deps(d)
			}
		},
	}, edit...)
	// The set-up of the fixture fetched upstream; the count starts after it.
	u.mu.Lock()
	u.calls, u.most = 0, 0
	u.mu.Unlock()
	return f
}

// syncAnswer is a decoded answer of the sync route: the members of a
// completed sync in body, or an error.
type syncAnswer struct {
	status int
	raw    []byte
	body   map[string]any
	Error  struct {
		ErrorType string `json:"error_type"`
		Message   string `json:"message"`
	} `json:"error"`
}

// sync sends POST /api/v1/workspaces/<slug>/sync, with the query given
// when it is not "", as the owner and decodes the answer.
func (f *syncFixture) sync(t *testing.T, query string) syncAnswer {
	t.Helper()
	path := "/api/v1/workspaces/" + f.slug + "/sync"
	if query != "" {
		path += "?" + query
	}
	status, body := f.Call(t, http.MethodPost, path, f.key, nil)
	return decodeSync(t, status, body)
}

// decodeSync decodes the answer of the sync route, status and body.
func decodeSync(t *testing.T, status int, body []byte) syncAnswer {
	t.Helper()
	a := syncAnswer{status: status, raw: body}
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("sync = %d %s: %v", status, body, err)
	}
	if err := json.Unmarshal(body, &a.body); err != nil {
		t.Fatalf("sync = %d %s: %v", status, body, err)
	}
	return a
}

// syncRequest is POST /api/v1/workspaces/<slug>/sync as the owner, sent
// with ctx.
func (f *syncFixture) syncRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.URL("/api/v1/workspaces/"+f.slug+"/sync"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	return req
}

// startSync sends POST /api/v1/workspaces/<slug>/sync as the owner in the
// background and returns the function that waits for its answer.
func (f *syncFixture) startSync(t *testing.T) (wait func() syncAnswer) {
	t.Helper()
	type answer struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan answer, 1)
	req := f.syncRequest(t, context.Background())
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- answer{err: err}
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		done <- answer{res.StatusCode, body, err}
	}()
	return func() syncAnswer {
		t.Helper()
		a := <-done
		if a.err != nil {
			t.Fatalf("sync %s: %v", f.slug, a.err)
		}
		return decodeSync(t, a.status, a.body)
	}
}

// mustSync syncs the workspace and fails the test unless the sync
// completed.
func (f *syncFixture) mustSync(t *testing.T) syncAnswer {
	t.Helper()
	a := f.sync(t, "")
	if a.status != http.StatusOK {
		t.Fatalf("sync %s = %d %s, want 200", f.slug, a.status, a.raw)
	}
	return a
}

// originBranch sets branch on origin to the commit id, which the hub's
// copy holds.
func (f *syncFixture) originBranch(t *testing.T, branch, id string) {
	t.Helper()
	gitrepotest.Git(t, f.copy, "push", "--quiet", "--force", f.origin.Dir(), id+":refs/heads/"+branch)
}

// originAhead gives origin's branch a commit, labelled label, on the hub's
// tip of branch, creating the branch in the hub at the upstream base when
// the hub has none, and returns the commit: origin is ahead of the hub.
func (f *syncFixture) originAhead(t *testing.T, branch, label string) string {
	t.Helper()
	tip := refIn(f.copy, "refs/heads/"+branch)
	if tip == "" {
		tip = f.base
		gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/"+branch, tip)
	}
	file := strings.ReplaceAll(branch, "/", "-") + "-" + label + ".txt"
	id := f.commitOn(t, tip, branch+" "+label, map[string]string{file: label + "\n"})
	f.originBranch(t, branch, id)
	return id
}

// upstreamTo moves the upstream's main to the commit id, which the hub's
// copy holds, whether or not that is a fast-forward.
func (f *syncFixture) upstreamTo(t *testing.T, id string) {
	t.Helper()
	gitrepotest.Git(t, f.copy, "push", "--quiet", "--force", f.upstream.Dir(), id+":refs/heads/main")
	gitrepotest.Git(t, f.upstream.Dir(), "update-server-info")
}

// wantMerged fails the test unless the answer's patches_merged is want.
func wantMerged(t *testing.T, a syncAnswer, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := answerList[string](t, a, "patches_merged"); !slices.Equal(got, want) {
		t.Errorf("patches_merged = %q, want %q", got, want)
	}
}

// commitOn writes into the hub's copy a commit on parent with msg that
// writes the files given, and returns it.
func (f *syncFixture) commitOn(t *testing.T, parent, msg string, files map[string]string) string {
	t.Helper()
	return gitrepotest.Commit(t, f.copy, msg, files, parent)
}

// setStatus stores status on the patch id, as an update would leave it.
func (f *syncFixture) setStatus(t *testing.T, id, status string) {
	t.Helper()
	if _, err := f.Store.Main().Exec(`UPDATE patches SET status = ? WHERE id = ?`, status, id); err != nil {
		t.Fatal(err)
	}
}

// upstreamChain writes into the hub's copy a line of commits on parent (a
// root commit first when parent is ""), the k-th of them, from 0, the
// pull request commit with the subject "Fix the dialer (#7)", and returns
// the last; all are written in one run of git fast-import.
func (f *syncFixture) upstreamChain(t *testing.T, label, parent string, n, k int) string {
	t.Helper()
	commits := make([]histCommit, n)
	for i := range commits {
		commits[i] = histCommit{from: i - 1, msg: fmt.Sprintf("%s: upstream %d", label, i),
			files: map[string]string{"up.txt": fmt.Sprintf("%s %d\n", label, i)}}
		if i == 0 {
			commits[i].parent = parent
		}
		if i == k {
			commits[i].msg = "Fix the dialer (#7)"
		}
	}
	ids := writeHistory(t, f.copy, commits)
	return ids[n-1]
}

// addPatchOn stores an active patch of branch at the end of the patch
// list, changed by edit, whose branch in the hub's copy holds one commit
// on base that adds a file of its own, and returns it.
func (f *syncFixture) addPatchOn(t *testing.T, branch, base string, edit ...func(p *carrypatch.Patch)) carrypatch.Patch {
	t.Helper()
	file := strings.ReplaceAll(branch, "/", "-") + ".txt"
	return f.putPatch(t, branch, f.commitOn(t, base, branch, map[string]string{file: branch + "\n"}), edit...)
}

// setSettings sets the workspace's settings given through the workspace
// update route, as the owner.
func (f *syncFixture) setSettings(t *testing.T, settings map[string]any) {
	t.Helper()
	if status, raw := f.Call(t, http.MethodPatch, "/api/v1/workspaces/"+f.slug, f.key, map[string]any{"settings": settings}); status != http.StatusOK {
		t.Fatalf("set the settings %v of %s = %d %s, want 200", settings, f.slug, status, raw)
	}
}

// setColumn stores value as the column of the workspace's record, as the
// hub's own writes would leave it.
func (f *syncFixture) setColumn(t *testing.T, column string, value any) {
	t.Helper()
	if _, err := f.Store.Main().Exec(`UPDATE workspaces SET `+column+` = ? WHERE slug = ?`, value, f.slug); err != nil {
		t.Fatalf("set %s of %s: %v", column, f.slug, err)
	}
}

// heads are the branches of the hub's copy, by name without refs/heads/.
func (f *syncFixture) heads(t *testing.T) map[string]string {
	t.Helper()
	heads, err := headsIn(f.copy)
	if err != nil {
		t.Fatalf("read the branches of %s: %v", f.copy, err)
	}
	return heads
}

// eventCount is the number of stored events of the workspace whose type
// is like typ, a pattern of SQL's LIKE.
func (f *syncFixture) eventCount(t *testing.T, typ string) int {
	t.Helper()
	var n int
	if err := f.Store.Main().QueryRow(`SELECT COUNT(*) FROM hub_events WHERE workspace = ? AND type LIKE ?`, f.slug, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// rebuildJobs are the rebuild jobs of the workspace, newest first.
func (f *syncFixture) rebuildJobs(t *testing.T) []jobs.Job {
	t.Helper()
	js, err := f.queue.List(context.Background(), carrypatch.RebuildJobType, f.slug, "", nil, 200)
	if err != nil {
		t.Fatalf("list the rebuild jobs of %s: %v", f.slug, err)
	}
	return js
}

// headsIn are the branches of the repository dir, by name without
// refs/heads/, for a caller off the test's goroutine.
func headsIn(dir string) (map[string]string, error) {
	out, err := gitrepotest.Run(dir, "", "for-each-ref", "--format=%(refname:lstrip=2) %(objectname)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	heads := map[string]string{}
	for line := range strings.Lines(out) {
		if name, id, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			heads[name] = id
		}
	}
	return heads, nil
}

// branches are the results of the answer's branch list, by branch name.
func (a syncAnswer) branches(t *testing.T) map[string]string {
	t.Helper()
	list, ok := a.body["branches"].([]any)
	if !ok {
		t.Fatalf("the answer %s has no branch list", a.raw)
	}
	results := map[string]string{}
	for _, e := range list {
		entry, _ := e.(map[string]any)
		name, _ := entry["name"].(string)
		results[name], _ = entry["result"].(string)
	}
	return results
}

// answerList is the answer's list member, in the answer's order; the test
// fails when the answer has no such list.
func answerList[T any](t *testing.T, a syncAnswer, member string) []T {
	t.Helper()
	var list *[]T
	err := json.Unmarshal(a.members(t)[member], &list)
	if err != nil || list == nil {
		t.Fatalf("the answer %s has no %s list: %v", a.raw, member, err)
	}
	return *list
}

// eventBranches are the branch_name of each event of ev, in order.
func eventBranches(ev []patchEvent) []string {
	names := make([]string, 0, len(ev))
	for _, e := range ev {
		name, _ := e.Metadata["branch_name"].(string)
		names = append(names, name)
	}
	return names
}

// syncedEntry is one entry of a sync answer's patches_synced.
type syncedEntry struct {
	BranchName  string  `json:"branch_name"`
	Action      string  `json:"action"`
	State       string  `json:"state"`
	LocalSHA    *string `json:"local_sha"`
	OriginSHA   *string `json:"origin_sha"`
	ReplacedSHA *string `json:"replaced_sha"`
}

// syncedNames are the branch names of the answer's patches_synced, in the
// answer's order.
func (a syncAnswer) syncedNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, e := range answerList[syncedEntry](t, a, "patches_synced") {
		names = append(names, e.BranchName)
	}
	return names
}

// syncedOf is the entry of the answer's patches_synced for branch.
func (a syncAnswer) syncedOf(t *testing.T, branch string) syncedEntry {
	t.Helper()
	for _, e := range answerList[syncedEntry](t, a, "patches_synced") {
		if e.BranchName == branch {
			return e
		}
	}
	t.Fatalf("patches_synced of %s has no entry for %s", a.raw, branch)
	return syncedEntry{}
}

// moveRecorder is a branch-move observer that records every move it is
// told of and answers err.
type moveRecorder struct {
	err   error
	mu    sync.Mutex
	moves []workspace.BranchMove
}

func (o *moveRecorder) BranchMoved(_ context.Context, m workspace.BranchMove) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.moves = append(o.moves, m)
	return o.err
}

// movesUnder are the moves told so far of the branches whose name starts
// with prefix, in the order told.
func (o *moveRecorder) movesUnder(prefix string) []workspace.BranchMove {
	o.mu.Lock()
	defer o.mu.Unlock()
	var moves []workspace.BranchMove
	for _, m := range o.moves {
		if strings.HasPrefix(m.Branch, prefix) {
			moves = append(moves, m)
		}
	}
	return moves
}

// dropRebuilds removes the rebuild jobs of the workspace, as if none had
// been queued, so that the next sync's rebuild is not a duplicate.
func (f *syncFixture) dropRebuilds(t *testing.T) {
	t.Helper()
	if _, err := f.Store.Main().Exec(`DELETE FROM jobs WHERE type = ? AND workspace_slug = ?`, carrypatch.RebuildJobType, f.slug); err != nil {
		t.Fatalf("remove the rebuild jobs of %s: %v", f.slug, err)
	}
}

// wantRebuildQueued fails the test unless the answer reports a rebuild
// queued by the sync, whose job is still queued, and returns the job's
// record as the rebuild route answers it.
func (f *syncFixture) wantRebuildQueued(t *testing.T, a syncAnswer) map[string]any {
	t.Helper()
	id, _ := a.body["rebuild_job_id"].(string)
	if a.body["rebuild_triggered"] != true || id == "" {
		t.Fatalf("rebuild_triggered, rebuild_job_id = %v, %v, want true and a job", a.body["rebuild_triggered"], a.body["rebuild_job_id"])
	}
	record := f.rebuildRecord(t, id)
	if record["status"] != "queued" {
		t.Errorf("the rebuild %s is %v, want queued", id, record["status"])
	}
	return record
}

// rebuildRecord is the record of the rebuild id of the workspace as the
// rebuild route answers it to the owner.
func (f *syncFixture) rebuildRecord(t *testing.T, id string) map[string]any {
	t.Helper()
	status, raw := f.Call(t, http.MethodGet, "/api/v1/workspaces/"+f.slug+"/rebuilds/"+id, f.key, nil)
	var record map[string]any
	if err := json.Unmarshal(raw, &record); status != http.StatusOK || err != nil {
		t.Fatalf("read the rebuild %s = %d %s, want 200 and a record", id, status, raw)
	}
	return record
}

// wantNoRebuild fails the test unless the answer reports that the sync
// queued no rebuild: rebuild_triggered false and rebuild_job_id null.
func wantNoRebuild(t *testing.T, a syncAnswer) {
	t.Helper()
	id, present := a.body["rebuild_job_id"]
	if a.body["rebuild_triggered"] != false || !present || id != nil {
		t.Errorf("rebuild_triggered, rebuild_job_id = %v, %v (present %v), want false and null", a.body["rebuild_triggered"], id, present)
	}
}

// refMover is a BeforeRefSwap hook that plays another writer: armed with a
// branch, it moves that branch of the repository dir to a commit of its
// own the next time a swap of the branch is about to happen, once, and
// sets the branch's backup ref too when armed with one; it counts the swaps
// about to happen per branch.
type refMover struct {
	mu     sync.Mutex
	dir    string
	branch string
	to     string
	backup string
	calls  map[string]int
	err    error
}

// arm makes the next swap of branch in the repository dir find it at to.
func (m *refMover) arm(dir, branch, to string) {
	m.armBackup(dir, branch, to, "")
}

// armBackup makes the next swap of branch in the repository dir find it at
// to and its backup ref at backup, as a reset to origin that replaced the
// branch in between leaves them; backup "" leaves the backup ref alone.
func (m *refMover) armBackup(dir, branch, to, backup string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dir, m.branch, m.to, m.backup = dir, branch, to, backup
}

func (m *refMover) hook(_, branch string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[branch]++
	if branch != m.branch {
		return
	}
	m.branch = ""
	if _, err := gitrepotest.Run(m.dir, "", "update-ref", "refs/heads/"+branch, m.to); err != nil {
		m.err = err
	}
	if m.backup == "" {
		return
	}
	if _, err := gitrepotest.Run(m.dir, "", "update-ref", carrypatch.ReplacedRefPrefix+branch, m.backup); err != nil {
		m.err = err
	}
}

// swaps is the number of swaps of branch that were about to happen, and
// the error of a move the mover could not make.
func (m *refMover) swaps(branch string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[branch], m.err
}

// members are the answer's top-level members as the hub wrote them.
func (a syncAnswer) members(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(a.raw, &members); err != nil {
		t.Fatalf("the answer %s is not an object: %v", a.raw, err)
	}
	return members
}

// syncAndLeave sends POST /api/v1/workspaces/<slug>/sync as the owner and
// leaves, cancelling the request, once started is closed.
func (f *syncFixture) syncAndLeave(t *testing.T, started <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := f.syncRequest(t, ctx)
	sent := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
		sent <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the sync did not reach the point to leave at within 10 s")
	}
	cancel()
	if err := <-sent; !errors.Is(err, context.Canceled) {
		t.Fatalf("the request ended with %v, want it cancelled by the caller", err)
	}
}

// waitSynced waits until the workspace records a completed sync.
func (f *syncFixture) waitSynced(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); f.record(t).LastSyncAt == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the sync recorded no completion within 20 s")
		}
	}
}
