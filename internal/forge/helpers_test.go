package forge_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestMain(m *testing.M) {
	hubtest.LimitParallel(hubtest.HubParallel)
	os.Exit(m.Run())
}

// The probe routes: forge routes that make one issue read on the forge
// through the shared checks, so that the checks are tested on routes of
// their own. /forge-probe reads like a list (it takes state and limit),
// /forge-probe/:number like a read of one issue.
const (
	probeList   = "/workspaces/:slug/forge-probe"
	probeNumber = "/workspaces/:slug/forge-probe/:number"
)

// brokenSlug is the workspace whose secrets cannot be read, and
// deniedParent the parent on which the access decider grants nobody
// anything.
const (
	brokenSlug   = "broken"
	deniedParent = "pd"
)

// world is a hub with the probe routes and the watched write routes, its
// forge component recording its events through a recorder, its forge client sending to a fake
// GitHub over TLS, an owner and a member of the org acme, and an outsider.
type world struct {
	*hubtest.Hub
	fake    *forgetest.Fake
	secrets *secret.Component
	decider *decider
	// forge is the hub's forge component, events its event log, and rec
	// the recorder the component records its events through.
	forge  *forge.Component
	events *event.Component
	rec    *recorder

	org                         string
	owner, member, outsider     string
	ownerKey, memberKey, outKey string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return newWorldWith(t, hubtest.Config{}, nil)
}

// newWorldWith is newWorld with the server TOML, the TOML and the
// environment of cfg added to the hub's, the clock of cfg as the hub's, and adjust, when it is not nil, run on the forge
// component's dependencies after the world's own changes.
func newWorldWith(t *testing.T, cfg hubtest.Config, adjust func(w *world, d *forge.Deps)) *world {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	w := &world{fake: forgetest.New(t), decider: &decider{refused: deniedParent}}
	env := map[string]string{secret.EnvCurrentKey: base64.StdEncoding.EncodeToString(key)}
	maps.Copy(env, cfg.Env)
	w.Hub = hubtest.StartWith(t, hubtest.Config{
		Server: cfg.Server,
		TOML:   cfg.TOML,
		Env:    env,
		Now:    cfg.Now,
		Compose: func(c *hub.Composition) {
			w.secrets, w.forge, w.events = c.Secrets(), c.Forge(), c.Events()
			for _, pattern := range []string{probeList, probeNumber} {
				c.AddRoute(c.Forge().Route(probe(pattern)))
			}
			for _, rt := range c.Forge().Routes() {
				if rt.Scope == forge.ScopeWrite {
					c.AddRoute(watched(rt))
				}
			}
			c.AdjustForge(func(d *forge.Deps) {
				d.Client.Transport = w.fake.Transport()
				w.rec = &recorder{next: d.Events}
				d.Events = w.rec
				d.Secrets = brokenSecrets{d.Secrets}
				w.decider.real = d.Access
				d.Access = w.decider
				if adjust != nil {
					adjust(w, d)
				}
			})
		},
	})
	w.owner, w.member, w.outsider = w.CreateUser(t, "owner"), w.CreateUser(t, "member"), w.CreateUser(t, "outsider")
	w.ownerKey, w.memberKey, w.outKey = w.IssueKey(t, w.owner), w.IssueKey(t, w.member), w.IssueKey(t, w.outsider)
	w.org = w.CreateOrg(t, "acme")
	w.AddMember(t, w.org, w.member)
	return w
}

// The routes the forge failure tests read through: a repository, an
// issue thread, which reads the issue and then its comments, and an
// issue's comments.
const (
	repositoryPath = "/forge/repository"
	issuePath      = "/issues/1"
	commentsPath   = "/issues/1/comments"
)

// probe is the probe route of pattern: it reads the issues, or the issue
// :number, of the target's repository on GitHub or GitLab and answers the
// forge's body, which must be JSON and for an issue an object, as the
// member forge. Its Check refuses a request with the query parameter
// refuse, 409 probe_refused.
func probe(pattern string) forge.Route {
	numbered := pattern == probeNumber
	return forge.Route{
		Method: http.MethodGet, Pattern: pattern, Scope: forge.ScopeRead, Need: access.NeedRead,
		Params: func(r *http.Request, call *forge.Call) error {
			if numbered {
				n, err := forge.PathNumber(r)
				call.Params = n
				return err
			}
			switch r.URL.Query().Get("state") {
			case "", "open", "closed", "all":
			default:
				return server.InvalidRequest("state must be open, closed or all")
			}
			_, err := server.ReadPage(r)
			return err
		},
		Check: func(r *http.Request, _ *forge.Call) error {
			if r.URL.Query().Has("refuse") {
				return server.NewError("probe_refused", http.StatusConflict, "refused by the route's check")
			}
			return nil
		},
		Serve: func(w http.ResponseWriter, r *http.Request, call *forge.Call) error {
			repo := call.Resolution.Repository
			path := "/repos/" + repo.Owner + "/" + repo.Name + "/issues"
			if call.Resolution.Kind == forge.KindGitLab {
				path = "/projects/" + url.PathEscape(repo.Owner+"/"+repo.Name) + "/issues"
			}
			var answer any = new(any)
			if n, ok := call.Params.(int64); ok {
				path += "/" + strconv.FormatInt(n, 10)
				answer = new(map[string]any)
			}
			resp, err := call.Get(r.Context(), path)
			if err != nil {
				return err
			}
			if err := forge.Decode(resp, answer); err != nil {
				return err
			}
			server.WriteJSON(w, http.StatusOK, map[string]any{"forge": answer})
			return nil
		},
	}
}

// watched is the write route rt at /workspaces/:slug/watched/..., in place
// of /workspaces/:slug/..., whose answer notes whether it has been written,
// so that the recorder can tell whether an event came before the answer.
func watched(rt server.Route) server.Route {
	rt.Pattern = strings.Replace(rt.Pattern, "/:slug/", "/:slug/watched/", 1)
	serve := rt.Handler
	rt.Handler = func(w http.ResponseWriter, r *http.Request) error {
		ww := &watchedWriter{ResponseWriter: w}
		return serve(ww, r.WithContext(context.WithValue(r.Context(), watchedKey{}, ww)))
	}
	return rt
}

// watchedKey is the context key of a watched route's answer.
type watchedKey struct{}

// watchedWriter is a watched route's answer, which notes whether anything
// has been written to it.
type watchedWriter struct {
	http.ResponseWriter
	written atomic.Bool
}

func (w *watchedWriter) WriteHeader(status int) {
	w.written.Store(true)
	w.ResponseWriter.WriteHeader(status)
}

func (w *watchedWriter) Write(b []byte) (int, error) {
	w.written.Store(true)
	return w.ResponseWriter.Write(b)
}

func (w *watchedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recorder is the forge component's event emitter in the tests: it
// records each event in the hub's event log and keeps it, or fails every
// event once failWith has been called.
type recorder struct {
	next forge.EventEmitter

	mu     sync.Mutex
	fail   error
	events []recorded
}

// recorded is an event the recorder recorded, and whether the answer to
// the request had been written then, which only a watched route tells.
type recorded struct {
	event.Record
	answered bool
}

func (r *recorder) EmitNow(ctx context.Context, in event.Input) (event.Record, error) {
	ww, _ := ctx.Value(watchedKey{}).(*watchedWriter)
	answered := ww != nil && ww.written.Load()
	r.mu.Lock()
	fail := r.fail
	r.mu.Unlock()
	if fail != nil {
		return event.Record{}, fail
	}
	rec, err := r.next.EmitNow(ctx, in)
	if err != nil {
		return rec, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recorded{rec, answered})
	return rec, nil
}

// failWith makes every later event fail with err.
func (r *recorder) failWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = err
}

// count is how many events the recorder recorded.
func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// since are the events recorded after the first n.
func (r *recorder) since(n int) []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events[n:])
}

// meta is the event's metadata, decoded.
func (r recorded) meta(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Metadata, &m); err != nil {
		t.Fatalf("the metadata %s of %s: %v", r.Metadata, r.Type, err)
	}
	return m
}

// waitUntil waits for done to hold, failing the test when it does not
// within ten seconds.
func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting for %s", what)
		}
	}
}

// brokenSecrets is the hub's secret reader, except that the secrets of
// brokenSlug cannot be read.
type brokenSecrets struct{ workspace.SecretReader }

func (s brokenSecrets) Get(ctx context.Context, scope secret.Scope, scopeID, name string) (string, bool, error) {
	if scopeID == brokenSlug {
		return "", true, errors.New("the secret store cannot be read")
	}
	return s.SecretReader.Get(ctx, scope, scopeID, name)
}

// asked is one question the forge component put to the access decider.
type asked struct {
	caller access.Caller
	slug   string
	need   access.Need
}

// decider is the hub's access rule, which records what it is asked, except
// that it grants nobody anything on the workspace refused.
type decider struct {
	real    forge.AccessDecider
	refused string

	mu    sync.Mutex
	asked []asked
}

func (d *decider) Decide(ctx context.Context, c access.Caller, slug string, need access.Need) (access.Decision, error) {
	d.mu.Lock()
	d.asked = append(d.asked, asked{c, slug, need})
	d.mu.Unlock()
	if slug == d.refused {
		return access.Decision{Outcome: access.NotFound}, nil
	}
	return d.real.Decide(ctx, c, slug, need)
}

func (d *decider) questions() []asked {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]asked(nil), d.asked...)
}

// settings is a stored settings document of name and value pairs.
func settings(pairs ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v, _ := json.Marshal(pairs[i+1])
		out[pairs[i]] = v
	}
	return out
}

// addWorkspace stores ws, a repository of the owner in acme, ready and in
// standard mode unless ws says otherwise, or a task fork when it names a
// parent. Its settings are stored as given, past the settings route's
// validation.
func (w *world) addWorkspace(t *testing.T, ws workspace.Workspace) {
	t.Helper()
	ctx, now := context.Background(), time.Now().UTC()
	if ws.Kind == "" {
		ws.Kind = access.KindRepository
	}
	if ws.Mode == "" {
		ws.Mode = workspace.ModeStandard
	}
	if ws.Status == "" {
		ws.Status = workspace.StatusReady
	}
	if ws.OwnerID == "" {
		ws.OwnerID = w.owner
	}
	ws.OrgID, ws.DisplayName, ws.DefaultBranch = w.org, ws.Slug, new("main")
	ws.CreatedAt, ws.UpdatedAt, ws.InstanceID = now, now, workspace.NewInstanceID()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		if ws.ParentSlug == nil {
			return workspace.Insert(ctx, tx, ws)
		}
		ws.Kind, ws.ForkPoint = access.KindFork, &workspace.ForkPoint{Ref: "main", SHA: "0123456789012345678901234567890123456789"}
		ws.ExpiresAt, ws.AutoChange = new(now.Add(24*time.Hour)), new(false)
		return fork.Insert(ctx, tx, fork.Fork{Workspace: ws})
	})
	if err != nil {
		t.Fatalf("store workspace %s: %v", ws.Slug, err)
	}
}

// putSecret stores the workspace secret name of slug.
func (w *world) putSecret(t *testing.T, slug, name, value string) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		return w.secrets.Put(ctx, tx, secret.ScopeWorkspace, slug, name, value)
	}); err != nil {
		t.Fatalf("store secret %s of %s: %v", name, slug, err)
	}
}

// pat issues a PAT of user with the permissions given, bound to the
// workspace bound unless it is "".
func (w *world) pat(t *testing.T, user, bound string, permissions ...string) string {
	t.Helper()
	return w.IssuePAT(t, identity.PATSpec{UserID: user, Permissions: permissions, Workspace: bound})
}

// get sends GET /api/v1/workspaces/<slug><path> with credential and
// returns the answer and how many requests the forge received meanwhile.
func (w *world) get(t *testing.T, slug, path, credential string) (status int, body []byte, sent int) {
	t.Helper()
	before := w.fake.Count()
	status, body = w.Call(t, http.MethodGet, "/api/v1/workspaces/"+slug+path, credential, nil)
	return status, body, w.fake.Count() - before
}

// send sends method /api/v1/workspaces/<slug><path> with credential and
// body, as hubtest's Call sends it, and returns the answer and how many
// requests the forge received meanwhile.
func (w *world) send(t *testing.T, method, slug, path, credential string, body any) (status int, answer []byte, sent int) {
	t.Helper()
	before := w.fake.Count()
	status, answer = w.Call(t, method, "/api/v1/workspaces/"+slug+path, credential, body)
	return status, answer, w.fake.Count() - before
}

// forgeEvents is how many forge events the hub's event log holds.
func (w *world) forgeEvents(t *testing.T) int {
	t.Helper()
	records, _, err := w.events.ReadAfter(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("read the event log: %v", err)
	}
	n := 0
	for _, r := range records {
		if strings.HasPrefix(r.Type, "parley.forge.") {
			n++
		}
	}
	return n
}

// hubError is the error member of an error answer.
type hubError struct {
	Code    int    `json:"code"`
	Type    string `json:"error_type"`
	Message string `json:"message"`
}

// list is a list answer: its items, each as a JSON object, and its
// next_cursor.
type list struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// list sends GET /api/v1/workspaces/<slug><path> with the admin token and
// returns the list it answers, failing the test unless the answer is 200
// with exactly the members items, an array, and next_cursor, a string or
// null. sent is how many requests the forge received meanwhile.
func (w *world) list(t *testing.T, slug, path string) (l list, sent int) {
	t.Helper()
	status, body, sent := w.get(t, slug, path, w.Token)
	var members map[string]json.RawMessage
	if status != http.StatusOK || json.Unmarshal(body, &members) != nil || len(members) != 2 ||
		!strings.HasPrefix(string(members["items"]), "[") || json.Unmarshal(body, &l) != nil {
		t.Fatalf("GET %s%s = %d %.300s, want 200 with items, an array, and next_cursor", slug, path, status, body)
	}
	if l.NextCursor != nil && *l.NextCursor == "" {
		t.Fatalf("GET %s%s: next_cursor is an empty string", slug, path)
	}
	return l, sent
}

// lastQuery is the query of the latest request the fake forge received.
func (w *world) lastQuery(t *testing.T) url.Values {
	t.Helper()
	q, err := url.ParseQuery(w.fake.Last(t).RawQuery)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// withQuery adds the query parameters q to path.
func withQuery(path, q string) string {
	if strings.Contains(path, "?") {
		return path + "&" + q
	}
	return path + "?" + q
}

// sameJSON reports whether got and want are the same JSON value.
func sameJSON(got []byte, want string) bool {
	var g, w any
	return json.Unmarshal(got, &g) == nil && json.Unmarshal([]byte(want), &w) == nil && reflect.DeepEqual(g, w)
}

func errorOf(t *testing.T, body []byte) hubError {
	t.Helper()
	var e struct {
		Error hubError `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("the answer %s is no error: %v", body, err)
	}
	return e.Error
}
