package workspace_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// createEvent is the event type a create writes.
const createEvent = "parley.workspace.create"

// world is a hub the workspace route tests ask: the users u and v, each with
// an API key and a personal org, the hub's secret and event components, a
// fake origin that records every call, and the clone notifications, which
// are recorded and dropped so that no clone runs.
type world struct {
	*hubtest.Hub
	svc     *workspace.Service
	secrets *secret.Component
	events  *event.Component
	origin  *fakeOrigin

	// u and v are the users' ids, uKey and vKey their API keys, uOrg and
	// vOrg their personal orgs.
	u, v       string
	uKey, vKey string
	uOrg, vOrg identity.Org

	// mu guards now, the hub's time, which a test may advance while the hub
	// reads it, notified, fault and availableCalls.
	mu       sync.Mutex
	now      time.Time
	notified []string
	// fault names the write of a create that fails, "" for none: one of
	// faultInsert, faultSecret and faultEvent.
	fault string
	// availableCalls counts the asks whether secrets can be stored.
	availableCalls int
	// gitOrigin keeps the hub's own origin lister, which runs git, in place
	// of the fake origin.
	gitOrigin bool
}

// The writes of a create a test can make fail.
const (
	faultInsert = "workspace insert"
	faultSecret = "secret write"
	faultEvent  = "event write"
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
	w := &world{origin: &fakeOrigin{}, now: time.Now().UTC().Truncate(time.Second).Add(time.Second / 2)}
	cfg := hubtest.Config{
		Env: map[string]string{secret.EnvCurrentKey: testKey},
		Now: w.clock,
		Compose: func(c *hub.Composition) {
			w.svc, w.secrets, w.events = c.Workspaces(), c.Secrets(), c.Events()
			c.AdjustWorkspace(func(d *workspace.Deps) {
				if !w.gitOrigin {
					d.Lister = w.origin
				}
				d.Notify = w.notify
				d.InsertWorkspace = func(ctx context.Context, tx *sql.Tx, ws workspace.Workspace) error {
					if err := w.faulted(faultInsert); err != nil {
						return err
					}
					return workspace.Insert(ctx, tx, ws)
				}
				d.Secrets = faultySecrets{d.Secrets, w}
				d.Events = faultyEvents{d.Events, w}
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
	status, b := w.Call(t, http.MethodPost, "/api/v1/workspaces", cred, body)
	return answer{status, b}
}

// object decodes a, which must be 201, as a workspace object.
func (a answer) object(t *testing.T) map[string]any {
	t.Helper()
	if a.status != http.StatusCreated {
		t.Fatalf("answer = %d %s, want 201", a.status, a.body)
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

// stored is the number of workspace records and of secrets the store holds,
// -1 for a count that cannot be read; it may run outside the test's
// goroutine.
func (w *world) stored() (workspaces, secrets int) {
	workspaces, secrets = -1, -1
	_ = w.Store.Main().QueryRow(`SELECT count(*) FROM workspaces`).Scan(&workspaces)
	_ = w.Store.Main().QueryRow(`SELECT count(*) FROM secrets`).Scan(&secrets)
	return workspaces, secrets
}

// fail makes the write named fault of every create fail until it is called
// with "".
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
// records, the secrets of every scope, the create events and the calls to
// origin.
type footprint struct{ workspaces, secrets, events, originCalls int }

// footprint counts what the hub holds and what origin was asked.
func (w *world) footprint(t *testing.T) footprint {
	t.Helper()
	var secrets int
	if err := w.Store.Main().QueryRow(`SELECT count(*) FROM secrets`).Scan(&secrets); err != nil {
		t.Fatal(err)
	}
	return footprint{w.workspaces(t), secrets, len(w.createEvents(t)), w.origin.called()}
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

// createEvents are the parley.workspace.create events of the log.
func (w *world) createEvents(t *testing.T) []event.Record {
	t.Helper()
	recs, _, err := w.events.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.Record
	for _, r := range recs {
		if r.Type == createEvent {
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
