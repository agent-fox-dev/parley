package workspace_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The event types a create, an update and a sync write.
const (
	createEvent = "parley.workspace.create"
	updateEvent = "parley.workspace.update"
	syncEvent   = "parley.workspace.sync"
	pushEvent   = "parley.workspace.push"
)

// world is a hub the workspace route tests ask: the users u and v, each with
// an API key and a personal org, the hub's secret and event components, a
// fake origin that records every call, the hub's git for clone work behind a
// recorder, and the clone notifications, which are recorded and dropped so
// that no clone runs unless the test runs it.
type world struct {
	*hubtest.Hub
	svc     *workspace.Service
	secrets *secret.Component
	events  *event.Component
	origin  *fakeOrigin
	git     *recordingGit

	// u and v are the users' ids, uKey and vKey their API keys, uOrg and
	// vOrg their personal orgs.
	u, v       string
	uKey, vKey string
	uOrg, vOrg identity.Org

	// mu guards now, the hub's time, which a test may advance while the hub
	// reads it, notified, slugs, fault and availableCalls.
	mu       sync.Mutex
	now      time.Time
	notified []string
	// slugs are the workspace slugs a create named or a secret was stored
	// for: the scopes a test can find secrets in.
	slugs map[string]bool
	// fault names the write of a create that fails, "" for none: one of
	// faultInsert, faultSecret and faultEvent.
	fault string
	// availableCalls counts the asks whether secrets can be stored.
	availableCalls int
	// gitOrigin keeps the hub's own origin lister, which runs git, in place
	// of the fake origin.
	gitOrigin bool
	// cloneWorkers keeps the hub's clone notification, so that the hub's
	// own workers run the clone work of what is created.
	cloneWorkers bool
}

// The writes of a create a test can make fail.
const (
	faultInsert = "workspace insert"
	faultSecret = "secret write"
	faultEvent  = "event write"
	// faultSecretRead fails every read of a secret, not only in a create.
	faultSecretRead = "secret read"
)

// option changes how newWorld starts the hub.
type option func(*hubtest.Config, *world)

// withExternalURL sets [server].external_url.
func withExternalURL(url string) option {
	return func(c *hubtest.Config, _ *world) { c.Server += "external_url = \"" + url + "\"\n" }
}

// withoutSecretKey starts the hub with no secret key, so that no secret can
// be stored.
func withoutSecretKey() option {
	return func(c *hubtest.Config, _ *world) { delete(c.Env, secret.EnvCurrentKey) }
}

// withGitOrigin keeps the hub's own origin lister and gives the hub the
// environment that makes its git trust the test origin o.
func withGitOrigin(o *gitrepotest.Origin) option {
	return func(c *hubtest.Config, w *world) {
		w.gitOrigin = true
		c.Env["GIT_SSL_CAINFO"] = o.CAFile()
	}
}

// withCloneWorkers lets the hub's own clone workers run the clone work of
// every workspace created.
func withCloneWorkers() option {
	return func(_ *hubtest.Config, w *world) { w.cloneWorkers = true }
}

// withLog makes the workspace service log to buf, as text.
func withLog(buf *hubtest.LogBuffer) option {
	return withCompose(func(c *hub.Composition) {
		c.AdjustWorkspace(func(d *workspace.Deps) { d.Logger = slog.New(slog.NewTextHandler(buf, nil)) })
	})
}

// withCompose runs fn on the hub's composition, after the world's own.
func withCompose(fn func(*hub.Composition)) option {
	return func(c *hubtest.Config, _ *world) {
		prev := c.Compose
		c.Compose = func(comp *hub.Composition) { prev(comp); fn(comp) }
	}
}

// newWorld starts a hub with a secret key and a clock that stands halfway
// through a second, and builds the world in it.
func newWorld(t *testing.T, opts ...option) *world {
	t.Helper()
	w := &world{origin: &fakeOrigin{}, git: &recordingGit{}, now: time.Now().UTC().Truncate(time.Second).Add(time.Second / 2)}
	cfg := hubtest.Config{
		Env: map[string]string{secret.EnvCurrentKey: testKey},
		Now: w.clock,
		Compose: func(c *hub.Composition) {
			w.svc, w.secrets, w.events = c.Workspaces(), c.Secrets(), c.Events()
			c.AdjustWorkspace(func(d *workspace.Deps) {
				if !w.gitOrigin {
					d.Lister = w.origin
				}
				if !w.cloneWorkers {
					d.Notify = w.notify
				}
				d.InsertWorkspace = func(ctx context.Context, tx *sql.Tx, ws workspace.Workspace) error {
					if err := w.faulted(faultInsert); err != nil {
						return err
					}
					return workspace.Insert(ctx, tx, ws)
				}
				d.Secrets = faultySecrets{d.Secrets, w}
				d.Events = faultyEvents{d.Events, w}
				w.git.Git = d.Git
				d.Git = w.git
			})
		},
	}
	for _, o := range opts {
		o(&cfg, w)
	}
	w.Hub = hubtest.StartWith(t, cfg)
	w.u, w.v = w.CreateUser(t, "ursula"), w.CreateUser(t, "victor")
	w.uKey, w.vKey = w.IssueKey(t, w.u), w.IssueKey(t, w.v)
	w.uOrg, w.vOrg = w.personalOrg(t, w.u), w.personalOrg(t, w.v)
	w.origin.stored = w.stored
	return w
}

// testKey is a secret key in the form an operator configures it: 32 bytes
// in standard base64.
const testKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// personalOrg is the one org a freshly created user belongs to.
func (w *world) personalOrg(t *testing.T, userID string) identity.Org {
	t.Helper()
	ctx := context.Background()
	ids, err := identity.ActiveOrgIDs(ctx, w.Store.Main(), userID)
	if err != nil || len(ids) != 1 {
		t.Fatalf("the orgs of %s = %v, %v; want exactly the personal org", userID, ids, err)
	}
	org, err := identity.OrgByID(ctx, w.Store.Main(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	return org
}

// clock is the hub's time.
func (w *world) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

// advance moves the hub's time on by d.
func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
}

// pat stores a PAT of the user with the permissions given and returns it.
func (w *world) pat(t *testing.T, userID string, perms ...string) string {
	t.Helper()
	return w.IssuePAT(t, identity.PATSpec{UserID: userID, Permissions: perms})
}

// notify records a clone notification and drops it.
func (w *world) notify(slug string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notified = append(w.notified, slug)
}

// notifications are the slugs the clone workers were told about.
func (w *world) notifications() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.notified...)
}

// answer is the status and the body of a hub's answer.
type answer struct {
	status int
	body   []byte
}

// create posts body to POST /workspaces with cred as the bearer credential.
func (w *world) create(t *testing.T, cred string, body any) answer {
	t.Helper()
	if m, ok := body.(map[string]any); ok {
		if slug, ok := m["slug"].(string); ok {
			w.useSlug(slug)
		}
	}
	status, b := w.Call(t, http.MethodPost, "/api/v1/workspaces", cred, body)
	return answer{status, b}
}

// get asks GET /workspaces/<slug> with cred as the bearer credential.
func (w *world) get(t *testing.T, cred, slug string) answer {
	t.Helper()
	status, b := w.Call(t, http.MethodGet, "/api/v1/workspaces/"+slug, cred, nil)
	return answer{status, b}
}

// patch sends body to PATCH /workspaces/<slug> with cred as the bearer
// credential.
func (w *world) patch(t *testing.T, cred, slug string, body any) answer {
	t.Helper()
	status, b := w.Call(t, http.MethodPatch, "/api/v1/workspaces/"+slug, cred, body)
	return answer{status, b}
}

// postSync sends POST /workspaces/<slug>/sync?<query>, with no body, with
// cred as the bearer credential.
func (w *world) postSync(t *testing.T, cred, slug, query string) answer {
	t.Helper()
	status, b := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/sync?"+query, cred, nil)
	return answer{status, b}
}

// postSyncAndLeave sends POST /workspaces/<slug>/sync with cred as the
// bearer credential and, once started is closed, cancels the request as a
// caller who disconnects; it returns when the request has ended so.
func (w *world) postSyncAndLeave(t *testing.T, cred, slug string, started <-chan struct{}) {
	t.Helper()
	w.postAndLeave(t, cred, "/api/v1/workspaces/"+slug+"/sync", started)
}

// postAndLeave sends POST path, with no body, as postSyncAndLeave does.
func (w *world) postAndLeave(t *testing.T, cred, path string, started <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+cred)
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
		t.Fatal("the sync did not reach the point to disconnect at within 10 s")
	}
	cancel()
	if err := <-sent; !errors.Is(err, context.Canceled) {
		t.Fatalf("the request ended with %v, want it cancelled by the caller", err)
	}
}

// list asks GET /workspaces?<query> with cred as the bearer credential.
func (w *world) list(t *testing.T, cred, query string) answer {
	t.Helper()
	status, b := w.Call(t, http.MethodGet, "/api/v1/workspaces?"+query, cred, nil)
	return answer{status, b}
}

// page decodes a, which must be 200, as a page of a list: the slug and the
// access of each item in order, and next_cursor. items must be a JSON
// array, never null, of workspace objects.
func (a answer) page(t *testing.T) (slugs, levels []string, next *string) {
	t.Helper()
	var p struct {
		Items      json.RawMessage `json:"items"`
		NextCursor *string         `json:"next_cursor"`
	}
	if err := json.Unmarshal(a.body, &p); err != nil || a.status != http.StatusOK || len(p.Items) == 0 || p.Items[0] != '[' {
		t.Fatalf("answer = %d %s, want 200 with an items array", a.status, a.body)
	}
	var items []map[string]any
	if err := json.Unmarshal(p.Items, &items); err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if len(it) != len(objectMembers) {
			t.Errorf("item %v has %d members, want the %d of a workspace object", it["slug"], len(it), len(objectMembers))
		}
		slug, _ := it["slug"].(string)
		level, _ := it["access"].(string)
		slugs, levels = append(slugs, slug), append(levels, level)
	}
	return slugs, levels, p.NextCursor
}

// objectMembers are the members of every workspace object.
var objectMembers = []string{"slug", "kind", "workspace_mode", "git_url", "default_branch", "owner_id", "org_id",
	"status", "display_name", "description", "head_sha", "origin_head_sha", "clone_error", "last_sync_at",
	"sync_error", "upstream_url", "integration_branch", "upstream_head_sha", "parent_slug", "fork_point",
	"expires_at", "auto_change", "purpose", "session_id", "settings", "created_at", "updated_at", "org_slug",
	"hub_url", "access"}

// object decodes a, which must be 201, as a workspace object.
func (a answer) object(t *testing.T) map[string]any {
	t.Helper()
	return a.decode(t, http.StatusCreated)
}

// decode decodes a, which must have the status want, as a JSON object.
func (a answer) decode(t *testing.T, want int) map[string]any {
	t.Helper()
	if a.status != want {
		t.Fatalf("answer = %d %s, want %d", a.status, a.body, want)
	}
	var obj map[string]any
	if err := json.Unmarshal(a.body, &obj); err != nil {
		t.Fatalf("decode the workspace object %s: %v", a.body, err)
	}
	return obj
}

// errorType is the error_type of a's error envelope.
func (a answer) errorType(t *testing.T) string {
	t.Helper()
	errorType, _ := a.refusal(t)
	return errorType
}

// refusal is the error_type and the message of a's error envelope.
func (a answer) refusal(t *testing.T) (errorType, message string) {
	t.Helper()
	var env struct {
		Error struct {
			ErrorType string `json:"error_type"`
			Message   string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(a.body, &env); err != nil {
		t.Fatalf("decode the error envelope %s: %v", a.body, err)
	}
	return env.Error.ErrorType, env.Error.Message
}

// workspaces is the number of workspace records the store holds.
func (w *world) workspaces(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.Store.Main().QueryRow(`SELECT count(*) FROM workspaces`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// stored is the number of workspace records and of secrets the store holds
// (see secretCount), -1 for a count that cannot be read; it may run outside
// the test's goroutine.
func (w *world) stored() (workspaces, secrets int) {
	workspaces = -1
	_ = w.Store.Main().QueryRow(`SELECT count(*) FROM workspaces`).Scan(&workspaces)
	secrets, err := w.secretCount()
	if err != nil {
		secrets = -1
	}
	return workspaces, secrets
}

// useSlug adds slug to the scopes secretCount counts.
func (w *world) useSlug(slug string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.slugs == nil {
		w.slugs = map[string]bool{}
	}
	w.slugs[slug] = true
}

// secretCount is the number of secrets the secret component holds in the
// scopes the tests use: the workspace scope of every slug a create named or
// a secret was stored for. The component counts one scope at a time, and a
// create stores secrets in no other.
func (w *world) secretCount() (int, error) {
	w.mu.Lock()
	slugs := slices.Collect(maps.Keys(w.slugs))
	w.mu.Unlock()
	total := 0
	for _, slug := range slugs {
		n, err := w.secrets.Count(context.Background(), secret.ScopeWorkspace, slug)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// fail makes the write named fault of every create, and of every update for
// the event write, fail until it is called with "".
func (w *world) fail(fault string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fault = fault
}

// faulted is the error of the write named fault, nil when it does not fail.
func (w *world) faulted(fault string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fault == fault {
		return errors.New("the " + fault + " failed")
	}
	return nil
}

// availableAsked is how often the service asked whether secrets can be
// stored.
func (w *world) availableAsked() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.availableCalls
}

// faultySecrets is the hub's secret component, counting the asks whether
// secrets can be stored and failing its writes while the world says so.
type faultySecrets struct {
	workspace.Secrets
	w *world
}

func (s faultySecrets) Available() bool {
	s.w.mu.Lock()
	s.w.availableCalls++
	s.w.mu.Unlock()
	return s.Secrets.Available()
}

func (s faultySecrets) Put(ctx context.Context, tx *sql.Tx, scope secret.Scope, scopeID, name, value string) error {
	if err := s.w.faulted(faultSecret); err != nil {
		return err
	}
	return s.Secrets.Put(ctx, tx, scope, scopeID, name, value)
}

func (s faultySecrets) Get(ctx context.Context, scope secret.Scope, scopeID, name string) (string, bool, error) {
	if err := s.w.faulted(faultSecretRead); err != nil {
		return "", false, err
	}
	return s.Secrets.Get(ctx, scope, scopeID, name)
}

// faultyEvents is the hub's event component, failing its writes while the
// world says so.
type faultyEvents struct {
	workspace.Events
	w *world
}

func (e faultyEvents) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	if err := e.w.faulted(faultEvent); err != nil {
		return event.Record{}, err
	}
	return e.Events.Emit(ctx, tx, in)
}

// footprint is what a refused create leaves as it found it: the workspace
// records, the secrets (see secretCount), the create events and the calls
// to origin.
type footprint struct{ workspaces, secrets, events, originCalls int }

// footprint counts what the hub holds and what origin was asked.
func (w *world) footprint(t *testing.T) footprint {
	t.Helper()
	secrets, err := w.secretCount()
	if err != nil {
		t.Fatal(err)
	}
	return footprint{w.workspaces(t), secrets, len(w.eventsOf(t, createEvent)), w.origin.called()}
}

// workspace reads the record slug from the store; found is false when there
// is none.
func (w *world) workspace(t *testing.T, slug string) (workspace.Workspace, bool) {
	t.Helper()
	rec, found, err := workspace.BySlug(context.Background(), w.Store.Main(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return rec, found
}

// deleteRecord removes the record slug from the store, so that the slug can
// be created again.
func (w *world) deleteRecord(t *testing.T, slug string) {
	t.Helper()
	if _, err := w.Store.Main().Exec(`DELETE FROM workspaces WHERE slug = ?`, slug); err != nil {
		t.Fatal(err)
	}
}

// workspaceSecrets is the number of secrets the workspace slug holds.
func (w *world) workspaceSecrets(t *testing.T, slug string) int {
	t.Helper()
	n, err := w.secrets.Count(context.Background(), secret.ScopeWorkspace, slug)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// eventCount is the number of events of the log.
func (w *world) eventCount(t *testing.T) int {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(recs)
}

// eventsOf are the events of the log of type typ, in the log's order.
func (w *world) eventsOf(t *testing.T, typ string) []event.Record {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Record
	for _, r := range recs {
		if r.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

// fakeOrigin is the origin lister of the tests: it records each call and
// answers err.
type fakeOrigin struct {
	// stored counts what the store holds at a call; nil counts nothing.
	stored func() (workspaces, secrets int)

	mu    sync.Mutex
	calls []originCall
	err   error
}

// originCall is one call of the fake origin: the URL and the credential it
// was given, the time its context had left, 0 without a deadline, and the
// workspaces and secrets the store held when it was called.
type originCall struct {
	url                 string
	cred                workspace.Credential
	left                time.Duration
	workspaces, secrets int
}

func (o *fakeOrigin) ListOrigin(ctx context.Context, url string, cred workspace.Credential) error {
	call := originCall{url: url, cred: cred}
	if deadline, ok := ctx.Deadline(); ok {
		call.left = time.Until(deadline)
	}
	if o.stored != nil {
		call.workspaces, call.secrets = o.stored()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, call)
	return o.err
}

// answer makes every later call answer err.
func (o *fakeOrigin) answer(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.err = err
}

// recorded are the calls the fake origin had.
func (o *fakeOrigin) recorded() []originCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]originCall(nil), o.calls...)
}

// called is the number of calls the fake origin had.
func (o *fakeOrigin) called() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.calls)
}

// putSecret stores the workspace secret name of slug through the secret
// component.
func (w *world) putSecret(t *testing.T, slug, name, value string) {
	t.Helper()
	w.useSlug(slug)
	err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		return w.secrets.Put(context.Background(), tx, secret.ScopeWorkspace, slug, name, value)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// failingReader is a secret reader whose read of the secret fail answers
// err; it answers the others from values.
type failingReader struct {
	fail   string
	err    error
	values map[string]string
}

func (r failingReader) Get(_ context.Context, _ secret.Scope, _, name string) (string, bool, error) {
	if name == r.fail {
		return "", false, r.err
	}
	v, ok := r.values[name]
	return v, ok, nil
}

// recordingGit is the hub's git for clone work: it counts every invocation
// and, while cloneErr is set, answers every clone with it without running
// git.
type recordingGit struct {
	workspace.Git

	mu       sync.Mutex
	calls    int
	cloneErr error
}

func (g *recordingGit) Clone(ctx context.Context, url string, cred workspace.Credential, dir string) error {
	g.mu.Lock()
	g.calls++
	err := g.cloneErr
	g.mu.Unlock()
	if err != nil {
		return err
	}
	return g.Git.Clone(ctx, url, cred, dir)
}

func (g *recordingGit) Open(ctx context.Context, dir string) (*gitrepo.Repo, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	return g.Git.Open(ctx, dir)
}

// failClone makes every later clone answer err; nil runs git again.
func (g *recordingGit) failClone(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cloneErr = err
}

// invocations is the number of clones and opens the clone work asked for.
func (g *recordingGit) invocations() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// startWorkers starts and launches clone workers of size on the world's
// service that run work, nil for the service's clone work, and stops them
// when the test ends.
func (w *world) startWorkers(t *testing.T, size int, work func(context.Context, string) error) *workspace.Workers {
	t.Helper()
	wk, err := workspace.NewWorkers(size, w.svc, work)
	if err != nil {
		t.Fatal(err)
	}
	if err := wk.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := wk.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := wk.Stop(ctx); err != nil {
			t.Errorf("stop the clone workers: %v", err)
		}
	})
	return wk
}

// runLog records the clone work workers run through it: the slugs in the
// order their work started and ended, and how many ran at once at most.
// While hold is set, each run waits for a value from it (or its context's
// end) before it does the work.
type runLog struct {
	mu                sync.Mutex
	started, finished []string
	running, most     int
	hold              chan struct{}
}

// around returns work that records each run in l, waits at l.hold, then
// runs clone.
func (l *runLog) around(clone func(context.Context, string) error) func(context.Context, string) error {
	return func(ctx context.Context, slug string) error {
		l.mu.Lock()
		l.started = append(l.started, slug)
		l.running++
		l.most = max(l.most, l.running)
		hold := l.hold
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.running--
			l.finished = append(l.finished, slug)
		}()
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return clone(ctx, slug)
	}
}

// snapshot is the slugs started and finished so far and the most runs seen
// at once.
func (l *runLog) snapshot() (started, finished []string, most int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.started...), append([]string(nil), l.finished...), l.most
}

// eventually waits up to 10 s for cond to hold and fails the test, naming
// what, when it does not.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 10 s", what)
		}
	}
}

// insertRecord stores a repository workspace of the user u with status,
// origin url and creation time created, as the store holds it after a
// create.
func (w *world) insertRecord(t *testing.T, slug string, status workspace.Status, created time.Time, url string) {
	t.Helper()
	w.put(t, workspace.Workspace{Slug: slug, Status: status, GitURL: &url, CreatedAt: created})
}

// put stores the workspace record rec, with what it leaves zero filled in
// as a create would: a standard repository of the user u in u's personal
// org, pending, created and updated at the hub's time, with an instance id.
func (w *world) put(t *testing.T, rec workspace.Workspace) {
	t.Helper()
	if rec.Kind == "" {
		rec.Kind = access.KindRepository
	}
	if rec.Mode == "" {
		rec.Mode = workspace.ModeStandard
	}
	if rec.OwnerID == "" {
		rec.OwnerID = w.u
	}
	if rec.OrgID == "" {
		rec.OrgID = w.uOrg.ID
	}
	if rec.Status == "" {
		rec.Status = workspace.StatusPending
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = w.clock()
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = rec.CreatedAt
	}
	rec.InstanceID = workspace.NewInstanceID()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, rec) }); err != nil {
		t.Fatal(err)
	}
}

// acme creates the org acme with the users u and v as members and returns
// its id.
func (w *world) acme(t *testing.T) string {
	t.Helper()
	id := w.CreateOrg(t, "acme")
	w.AddMember(t, id, w.u)
	w.AddMember(t, id, w.v)
	return id
}

// setPending puts the workspace slug back to pending with its other clone
// fields kept, its default_branch removed when forget is true, as a
// workspace that has clone work again.
func (w *world) setPending(t *testing.T, slug string, forget bool) {
	t.Helper()
	rec, _ := w.workspace(t, slug)
	if forget {
		rec.DefaultBranch = nil
	}
	if err := workspace.UpdateClone(context.Background(), w.Store.Main(), slug, workspace.CloneFields{
		Status: workspace.StatusPending, DefaultBranch: rec.DefaultBranch, HeadSHA: rec.HeadSHA,
		OriginHeadSHA: rec.OriginHeadSHA, CloneError: rec.CloneError, UpdatedAt: w.clock(),
	}); err != nil {
		t.Fatal(err)
	}
}

// deleteSecret removes the workspace secret name of slug through the secret
// component.
func (w *world) deleteSecret(t *testing.T, slug, name string) {
	t.Helper()
	err := w.Store.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := w.secrets.Delete(context.Background(), tx, secret.ScopeWorkspace, slug, name)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// syncSlug is the workspace of a sync world.
const syncSlug = "demo"

// syncWorld is a world with the ready repository workspace syncSlug of the
// user u, whose origin is a bare repository on disk holding the origin's
// usual history (see gitrepotest.Origin.History), and the hub's copy of it at
// its place, made by the clone work, which records the origin heads. The
// hub's fetcher records every fetch, and swaps is the hub's hook between a
// branch's classification and its swap.
type syncWorld struct {
	*world
	origin *gitrepotest.Origin
	// copy is the git directory of the hub's copy.
	copy    string
	fetches *recordingFetcher
	swaps   *swapHook
}

// newSyncWorld starts a world with opts and builds the sync world in it.
func newSyncWorld(t *testing.T, opts ...option) *syncWorld {
	t.Helper()
	o := gitrepotest.NewOrigin(t)
	o.History()
	f, h := &recordingFetcher{}, &swapHook{calls: map[string]int{}, on: map[string]func(int){}}
	w := newWorld(t, append(opts, withCompose(func(c *hub.Composition) {
		c.AdjustWorkspace(func(d *workspace.Deps) { f.Fetcher, d.Fetcher, d.BeforeRefSwap = d.Fetcher, f, h.before })
	}))...)
	url := o.Dir()
	w.put(t, workspace.Workspace{Slug: syncSlug, GitURL: &url})
	if rec := w.clone(t, syncSlug); rec.Status != workspace.StatusReady {
		t.Fatalf("the clone of the sync workspace left it %s (%v)", rec.Status, deref(rec.CloneError))
	}
	return &syncWorld{world: w, origin: o, copy: w.svc.RepoPath(syncSlug), fetches: f, swaps: h}
}

// sync runs a sync of the workspace with opts, the admin token as the actor
// when opts names none, and fails the test when it does not complete.
func (s *syncWorld) sync(t *testing.T, opts workspace.SyncOptions) workspace.SyncResult {
	t.Helper()
	if opts.Actor.Type == "" {
		opts.Actor = identity.Principal{Type: identity.TypeAdmin}
	}
	r, err := s.svc.Sync(context.Background(), syncSlug, opts)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return r
}

// syncBranches decodes the branches of a, which must be 200, and checks that
// the rest of a is the workspace object.
func syncBranches(t *testing.T, a answer) (map[string]any, []workspace.BranchResult) {
	t.Helper()
	obj := a.decode(t, http.StatusOK)
	var body struct {
		Branches []workspace.BranchResult `json:"branches"`
	}
	if err := json.Unmarshal(a.body, &body); err != nil || body.Branches == nil {
		t.Fatalf("the sync answer %s has no branches list (%v)", a.body, err)
	}
	delete(obj, "branches")
	if len(obj) != len(objectMembers) {
		t.Errorf("the sync answer has %d members beside branches, want the %d of a workspace object", len(obj), len(objectMembers))
	}
	return obj, body.Branches
}

// deref is the value p points at, "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// recordingFetcher is the hub's fetcher for syncs: it records every fetch
// and runs it, or fails it with err while err is set.
type recordingFetcher struct {
	workspace.Fetcher

	mu    sync.Mutex
	calls []fetchCall
	hold  func()
	err   error
}

// fetchCall is one fetch: the copy, origin's URL, the credential and the
// deadline it was given.
type fetchCall struct {
	dir, url string
	cred     workspace.Credential
	deadline time.Duration
}

func (f *recordingFetcher) FetchOrigin(ctx context.Context, dir, url string, cred workspace.Credential, deadline time.Duration) (gitrepo.Fetched, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fetchCall{dir, url, cred, deadline})
	hold, err := f.hold, f.err
	f.mu.Unlock()
	if hold != nil {
		hold()
	}
	if err != nil {
		return gitrepo.Fetched{}, err
	}
	return f.Fetcher.FetchOrigin(ctx, dir, url, cred, deadline)
}

// failWith makes every later fetch call fail with err, without fetching;
// nil makes them fetch again.
func (f *recordingFetcher) failWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// holdWith makes every later fetch call hold before it runs.
func (f *recordingFetcher) holdWith(hold func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = hold
}

// recorded are the fetches so far.
func (f *recordingFetcher) recorded() []fetchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fetchCall(nil), f.calls...)
}

// swapHook is the hub's hook between a branch's classification and its swap
// in a sync: it counts the calls per branch and runs what is set for the
// branch with the call's number, from 1.
type swapHook struct {
	mu    sync.Mutex
	calls map[string]int
	on    map[string]func(call int)
}

func (h *swapHook) before(_ context.Context, _, branch string) {
	h.mu.Lock()
	h.calls[branch]++
	call, fn := h.calls[branch], h.on[branch]
	h.mu.Unlock()
	if fn != nil {
		fn(call)
	}
}

// set makes every later call for branch run fn.
func (h *swapHook) set(branch string, fn func(call int)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.on[branch] = fn
}

// count is the number of calls for branch so far.
func (h *swapHook) count(branch string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[branch]
}

// gitFrom runs git with stdin on the repository at dir, as another writer
// does, from any goroutine: a failure fails the test without stopping the
// goroutine, and the output is "" then.
func gitFrom(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Writer", "GIT_AUTHOR_EMAIL=writer@example.invalid",
		"GIT_COMMITTER_NAME=Writer", "GIT_COMMITTER_EMAIL=writer@example.invalid")
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("git %v in %s: %v: %s", args, dir, err, out)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ref is the object id of the ref name of the repository at dir, "" when
// there is no such ref.
func ref(t *testing.T, dir, name string) string {
	t.Helper()
	return gitrepotest.Git(t, dir, "for-each-ref", "--format=%(objectname)", name)
}

// grow points branch of the repository at dir at a new commit on top of
// from, with from's tree and a message naming where, and returns its id, so
// that two sides grown from one commit differ.
func grow(t *testing.T, dir, branch, from, where string) string {
	t.Helper()
	id := gitrepotest.Git(t, dir, "commit-tree", from+"^{tree}", "-p", from, "-m", where+" on "+branch)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/"+branch, id)
	return id
}

// outcomes maps every branch of r to its result.
func outcomes(r workspace.SyncResult) map[string]workspace.BranchOutcome {
	m := map[string]workspace.BranchOutcome{}
	for _, b := range r.Branches {
		m[b.Name] = b.Result
	}
	return m
}

// branchSet is the branches of a sync world after build: from base, origin's
// main, same is equal on both sides, new-b (absent from the hub) and ff have
// a commit on origin only, ahead-b one in the hub only (hubAhead) and div one
// of its own on each side (hubDiv); main and dev are as the clone left them.
// theirs are origin's branch heads.
type branchSet struct {
	base, hubAhead, hubDiv string
	theirs                 map[string]string
}

// branchSet builds the branch set in s.
func (s *syncWorld) branchSet(t *testing.T) branchSet {
	t.Helper()
	origin := s.origin.Dir()
	b := branchSet{base: s.origin.Head("main")}
	gitrepotest.Git(t, origin, "branch", "same", b.base)
	gitrepotest.Git(t, origin, "branch", "ahead-b", b.base)
	for _, name := range []string{"new-b", "ff", "div"} {
		grow(t, origin, name, b.base, "origin")
	}
	gitrepotest.Git(t, s.copy, "branch", "same", b.base)
	gitrepotest.Git(t, s.copy, "branch", "ff", b.base)
	b.hubAhead = grow(t, s.copy, "ahead-b", b.base, "hub")
	b.hubDiv = grow(t, s.copy, "div", b.base, "hub")
	b.theirs = gitrepotest.Refs(t, origin, "refs/heads/")
	return b
}

// recordingObserver records every branch move it is told of, with the hub's
// head of the branch at that moment, and answers err.
type recordingObserver struct {
	t   *testing.T
	dir string
	err error

	mu    sync.Mutex
	moves []workspace.BranchMove
	heads []string
}

func (o *recordingObserver) BranchMoved(_ context.Context, m workspace.BranchMove) error {
	head := ref(o.t, o.dir, "refs/heads/"+m.Branch)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.moves, o.heads = append(o.moves, m), append(o.heads, head)
	return o.err
}

// recorded are the moves told so far and the heads seen at each.
func (o *recordingObserver) recorded() ([]workspace.BranchMove, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.moves), slices.Clone(o.heads)
}

// observe registers on the world's service an observer answering err.
func (s *syncWorld) observe(t *testing.T, err error) *recordingObserver {
	o := &recordingObserver{t: t, dir: s.copy, err: err}
	s.svc.RegisterBranchMoveObserver(o)
	return o
}

// setMode gives the workspace syncSlug the workspace_mode mode and the
// integration_branch integration.
func (w *world) setMode(t *testing.T, mode, integration string) {
	t.Helper()
	if _, err := w.Store.Main().Exec(`UPDATE workspaces SET workspace_mode = ?, integration_branch = ? WHERE slug = ?`,
		mode, integration, syncSlug); err != nil {
		t.Fatal(err)
	}
}

// excluding is an excluded-branch provider that names names, or fails with
// err, and records the slugs it is asked for.
type excluding struct {
	names []string
	err   error

	mu    sync.Mutex
	slugs []string
}

func (p *excluding) ExcludedBranches(_ context.Context, slug string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slugs = append(p.slugs, slug)
	return p.names, p.err
}

// asked are the slugs the provider was asked for so far.
func (p *excluding) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.slugs)
}

// pushWorld is a world with the ready repository workspace syncSlug of the
// user u, whose origin takes pushes over https (gitrepotest.SmartOrigin) and
// holds the origin's usual history (see gitrepotest.Origin.History), and the
// hub's copy of it at its place, made by the clone work, which records the
// origin heads. The hub's push operation records every push.
type pushWorld struct {
	*world
	origin *gitrepotest.Origin
	// copy is the git directory of the hub's copy.
	copy   string
	pushes *recordingPusher
}

// newPushWorld builds the push world with an origin built with originOpts,
// storing the workspace secrets secrets before the clone.
func newPushWorld(t *testing.T, secrets map[string]string, originOpts ...gitrepotest.Option) *pushWorld {
	t.Helper()
	return newPushWorldWith(t, secrets, nil, originOpts...)
}

// newPushWorldWith is newPushWorld with the hub started with opts too.
func newPushWorldWith(t *testing.T, secrets map[string]string, opts []option, originOpts ...gitrepotest.Option) *pushWorld {
	t.Helper()
	o := gitrepotest.SmartOrigin(t, originOpts...)
	o.History()
	p := &recordingPusher{}
	w := newWorld(t, append([]option{withGitOrigin(o), withCompose(func(c *hub.Composition) {
		c.AdjustWorkspace(func(d *workspace.Deps) {
			g, _ := d.Pusher.(gitrepo.Git)
			g.Trace = p.trace
			p.git, d.Pusher = g, p
		})
	})}, opts...)...)
	url := o.URL()
	w.put(t, workspace.Workspace{Slug: syncSlug, GitURL: &url})
	for name, value := range secrets {
		w.putSecret(t, syncSlug, name, value)
	}
	if rec := w.clone(t, syncSlug); rec.Status != workspace.StatusReady {
		t.Fatalf("the clone of the push workspace left it %s (%v)", rec.Status, deref(rec.CloneError))
	}
	return &pushWorld{world: w, origin: o, copy: w.svc.RepoPath(syncSlug), pushes: p}
}

// seen points branch of the hub's copy and of origin at one new commit on
// top of from and records it as the branch's origin head, as a sync that
// brought it in leaves them, and returns the commit.
func (p *pushWorld) seen(t *testing.T, branch, from string) string {
	t.Helper()
	id := grow(t, p.copy, branch, from, "seen")
	gitrepotest.Git(t, p.copy, "push", "--quiet", p.origin.Dir(), id+":refs/heads/"+branch)
	p.setOriginHead(t, branch, id)
	return id
}

// setOriginHead records id as the origin head of branch in the hub's copy,
// or removes the record when id is "".
func (p *pushWorld) setOriginHead(t *testing.T, branch, id string) {
	t.Helper()
	ctx := context.Background()
	repo, err := gitrepo.Git{}.Open(ctx, p.copy)
	if err == nil && id == "" {
		err = repo.DeleteOriginHead(ctx, branch)
	} else if err == nil {
		err = repo.SetOriginHead(ctx, branch, id)
	}
	if err != nil {
		t.Fatalf("setting the origin head of %s to %q: %v", branch, id, err)
	}
}

// asked is closed once origin has been sent a request more than now.
func (p *pushWorld) asked() <-chan struct{} {
	n, ch := p.origin.Requests(), make(chan struct{})
	go func() {
		for deadline := time.Now().Add(10 * time.Second); p.origin.Requests() == n && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}

// checkRecords checks what a push of the hub's head sha of branch of the
// workspace syncSlug by u, forced when force is set, recorded after the log
// held n push events: exactly one push event more, for that branch, whose
// metadata has ok true and result outcome when outcome is a push result,
// and otherwise ok false and error_type outcome; for a result also the
// branch's origin head in the hub's copy, and for the default branch main
// the workspace's origin_head_sha, at sha.
func (p *pushWorld) checkRecords(t *testing.T, what string, n int, branch, sha string, force bool, outcome string) {
	t.Helper()
	evs := p.eventsOf(t, pushEvent)
	if len(evs) != n+1 {
		t.Errorf("%s: %d push events written, want 1", what, len(evs)-n)
		return
	}
	rec, _ := p.workspace(t, syncSlug)
	want := map[string]any{"branch": branch, "sha": sha, "force": force, "remote_url": deref(rec.GitURL), "ok": false, "error_type": outcome}
	if slices.Contains([]gitrepo.PushResult{gitrepo.PushPushed, gitrepo.PushForced, gitrepo.PushUpToDate}, gitrepo.PushResult(outcome)) {
		delete(want, "error_type")
		want["ok"], want["result"] = true, outcome
		if got := ref(t, p.copy, "refs/remotes/origin/"+branch); got != sha {
			t.Errorf("%s: the recorded origin head of %s is %q, want %s", what, branch, got, sha)
		}
		if got := deref(rec.OriginHeadSHA); branch == "main" && got != sha {
			t.Errorf("%s: origin_head_sha is %q, want %s", what, got, sha)
		}
	}
	ev := evs[n]
	var meta map[string]any
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if deref(ev.Workspace) != syncSlug || ev.ActorType != event.ActorAPIKey || deref(ev.ActorID) != p.u ||
		ev.ResourceType != "branch" || ev.ResourceID != branch || !reflect.DeepEqual(meta, want) {
		t.Errorf("%s: push event of workspace %q by %s %q on %s %q with %v, want workspace %q by %s %q on branch %q with %v", what,
			deref(ev.Workspace), ev.ActorType, deref(ev.ActorID), ev.ResourceType, ev.ResourceID, meta,
			syncSlug, event.ActorAPIKey, p.u, branch, want)
	}
}

// outcome is the result of a push's answer a when it is 200, its error type
// otherwise.
func (a answer) outcome(t *testing.T) string {
	t.Helper()
	if a.status == http.StatusOK {
		r, _ := a.decode(t, http.StatusOK)["result"].(string)
		return r
	}
	return a.errorType(t)
}

// push sends POST /workspaces/<slug>/branches/<segment>/push, segment being
// the branch name as the path carries it, with body (nil for none) and cred
// as the bearer credential.
func (w *world) push(t *testing.T, cred, slug, segment string, body any) answer {
	t.Helper()
	status, b := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/branches/"+segment+"/push", cred, body)
	return answer{status, b}
}

// recordingPusher is the hub's push operation: it records every push and the
// arguments of every git process the hub's git runs for one, and runs the
// push, or answers with stub without running git while stub is set.
type recordingPusher struct {
	git gitrepo.Git

	mu    sync.Mutex
	calls []gitrepo.PushRequest
	args  [][]string
	stub  func(context.Context) (gitrepo.PushResult, error)
}

func (p *recordingPusher) Push(ctx context.Context, req gitrepo.PushRequest) (gitrepo.PushResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	stub := p.stub
	p.mu.Unlock()
	if stub != nil {
		return stub(ctx)
	}
	return p.git.Push(ctx, req)
}

func (p *recordingPusher) trace(args []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.args = append(p.args, args)
}

// stubWith makes every later push answer r without running git.
func (p *recordingPusher) stubWith(r gitrepo.PushResult) {
	p.answerWith(func(context.Context) (gitrepo.PushResult, error) { return r, nil })
}

// answerWith makes every later push answer what fn, given the push's
// context, returns, without running git.
func (p *recordingPusher) answerWith(fn func(context.Context) (gitrepo.PushResult, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stub = fn
}

// recorded are the pushes so far.
func (p *recordingPusher) recorded() []gitrepo.PushRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// gitArgs are the arguments of every git process the pushes ran so far.
func (p *recordingPusher) gitArgs() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.args)
}

// setProtected stores globs as the protected_branches setting of the
// workspace syncSlug; nil leaves the setting unset.
func (w *world) setProtected(t *testing.T, globs []string) {
	t.Helper()
	doc := []byte("{}")
	if globs != nil {
		var err error
		if doc, err = json.Marshal(map[string][]string{"protected_branches": globs}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Store.Main().Exec(`UPDATE workspaces SET settings = ? WHERE slug = ?`, string(doc), syncSlug); err != nil {
		t.Fatal(err)
	}
}

// storeSecrets stores secrets as the secrets of the workspace syncSlug,
// deleting each one whose value is "".
func (w *world) storeSecrets(t *testing.T, secrets map[string]string) {
	t.Helper()
	for name, value := range secrets {
		if value == "" {
			w.deleteSecret(t, syncSlug, name)
		} else {
			w.putSecret(t, syncSlug, name, value)
		}
	}
}

// setGitURL points the record of the workspace slug at the origin url.
func (w *world) setGitURL(t *testing.T, slug, url string) {
	t.Helper()
	if _, err := w.Store.Main().Exec(`UPDATE workspaces SET git_url = ? WHERE slug = ?`, url, slug); err != nil {
		t.Fatal(err)
	}
}

// withPushDeadline gives the hub's pushes the deadline d.
func withPushDeadline(d time.Duration) option {
	return withCompose(func(c *hub.Composition) {
		c.AdjustWorkspace(func(deps *workspace.Deps) { deps.PushDeadline = d })
	})
}

// processesNaming are the command lines of the running processes whose
// command line holds s, such as a git push to an origin's URL and the
// transport helper it started.
func processesNaming(t *testing.T, s string) []string {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "args=").Output()
	if err != nil {
		t.Fatalf("list the processes: %v", err)
	}
	var naming []string
	for line := range strings.Lines(string(out)) {
		if strings.Contains(line, s) {
			naming = append(naming, strings.TrimSpace(line))
		}
	}
	return naming
}

// patchLookup is a patch-branch lookup that reports the branches registered
// as patch branches of every workspace, and fails for the workspace failing.
type patchLookup struct {
	registered []string
	failing    string
}

func (l patchLookup) IsPatchBranch(_ context.Context, slug, branch string) (bool, error) {
	if slug == l.failing {
		return false, errors.New("the patch list cannot be read")
	}
	return slices.Contains(l.registered, branch), nil
}

// beforeEmit is the hub's event component, which runs a function before
// every event it writes.
type beforeEmit struct {
	workspace.Events

	mu sync.Mutex
	fn func()
}

func (e *beforeEmit) Emit(ctx context.Context, tx *sql.Tx, in event.Input) (event.Record, error) {
	e.mu.Lock()
	fn := e.fn
	e.mu.Unlock()
	if fn != nil {
		fn()
	}
	return e.Events.Emit(ctx, tx, in)
}

// set makes fn the function run before every event.
func (e *beforeEmit) set(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fn = fn
}
