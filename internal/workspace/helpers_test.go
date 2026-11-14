package workspace_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
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

// The event types a create and an update write.
const (
	createEvent = "parley.workspace.create"
	updateEvent = "parley.workspace.update"
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
// hub's fetcher records every fetch.
type syncWorld struct {
	*world
	origin *gitrepotest.Origin
	// copy is the git directory of the hub's copy.
	copy    string
	fetches *recordingFetcher
}

// newSyncWorld starts a world with opts and builds the sync world in it.
func newSyncWorld(t *testing.T, opts ...option) *syncWorld {
	t.Helper()
	o := gitrepotest.NewOrigin(t)
	o.History()
	f := &recordingFetcher{}
	w := newWorld(t, append(opts, withCompose(func(c *hub.Composition) {
		c.AdjustWorkspace(func(d *workspace.Deps) { f.Fetcher, d.Fetcher = d.Fetcher, f })
	}))...)
	url := o.Dir()
	w.put(t, workspace.Workspace{Slug: syncSlug, GitURL: &url})
	if rec := w.clone(t, syncSlug); rec.Status != workspace.StatusReady {
		t.Fatalf("the clone of the sync workspace left it %s (%v)", rec.Status, deref(rec.CloneError))
	}
	return &syncWorld{world: w, origin: o, copy: w.svc.RepoPath(syncSlug), fetches: f}
}

// sync runs a sync of the workspace with opts and fails the test when it
// does not complete.
func (s *syncWorld) sync(t *testing.T, opts workspace.SyncOptions) workspace.SyncResult {
	t.Helper()
	r, err := s.svc.Sync(context.Background(), syncSlug, opts)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return r
}

// refsUnder maps every ref below prefix of the repository at dir, by its
// name without the prefix, to the object id it points at.
func refsUnder(t *testing.T, dir, prefix string) map[string]string {
	t.Helper()
	refs := map[string]string{}
	for line := range strings.Lines(gitrepotest.Git(t, dir, "for-each-ref", "--format=%(refname) %(objectname)", prefix)) {
		name, id, _ := strings.Cut(strings.TrimSpace(line), " ")
		refs[strings.TrimPrefix(name, prefix)] = id
	}
	return refs
}

// deref is the value p points at, "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// recordingFetcher is the hub's fetcher for syncs: it records every fetch
// and runs it.
type recordingFetcher struct {
	workspace.Fetcher

	mu    sync.Mutex
	calls []fetchCall
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
	f.mu.Unlock()
	return f.Fetcher.FetchOrigin(ctx, dir, url, cred, deadline)
}

// recorded are the fetches so far.
func (f *recordingFetcher) recorded() []fetchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fetchCall(nil), f.calls...)
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
