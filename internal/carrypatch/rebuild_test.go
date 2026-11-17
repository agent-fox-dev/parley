package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

// The kinds of an attempt's outcome, as carrypatch.RunRebuild names
// them.
const (
	attemptCompleted = "completed"
	attemptRetryable = "retryable"
	attemptPermanent = "permanent"
	attemptEnded     = "ended"
)

// rebuildFixture is the carry-patch workspace acme, ready and set up, in a
// hub of its own whose clock stands still. Its upstream is a test origin
// whose branch main holds one commit at the start, its origin another one
// that takes pushes, and its copy a bare repository made by git init that
// the set-up gave the upstream's branches and the integration branch deploy
// at the upstream base; the record's head_sha is that base too, as a clone
// of an origin at the upstream's head would leave it. The hub's job queue passes only when the test runs
// an attempt, and its rebuild jobs run the hub's rebuild attempt.
type rebuildFixture struct {
	*addHub
	slug     string
	upstream *gitrepotest.Origin
	origin   *gitrepotest.Origin
	// copy is the hub's repository of the workspace; base is the upstream
	// base the set-up wrote the integration branch at.
	copy, base string
	comp       *carrypatch.Component
	queue      *jobs.Queue
	secrets    *rebuildSecrets
	runner     *attemptRunner
	hooks      rebuildHooks
	// stopFetch, once set, makes every upstream fetch cancel the attempt
	// that runs and fail with its context's error.
	stopFetch atomic.Bool
}

// rebuildHooks are what a test runs at the points of an attempt the
// carry-patch component calls its test hooks at: before the run takes up
// its i-th patch, from 0, before the publish, and between the publish's
// read of the integration branch and its swap, while the publish's store
// transaction is open.
type rebuildHooks struct {
	mu        sync.Mutex
	onPatch   func(i int)
	onPublish func()
	onSwap    func()
}

// beforePatch sets fn to run before the run takes up each patch.
func (h *rebuildHooks) beforePatch(fn func(i int)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onPatch = fn
}

// beforePublish sets fn to run before the publish.
func (h *rebuildHooks) beforePublish(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onPublish = fn
}

// beforeSwap sets fn to run between the publish's read and its swap; fn
// must not write through the store's Tx.
func (h *rebuildHooks) beforeSwap(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onSwap = fn
}

func (h *rebuildHooks) patch(_ string, i int) {
	h.mu.Lock()
	fn := h.onPatch
	h.mu.Unlock()
	if fn != nil {
		fn(i)
	}
}

func (h *rebuildHooks) publish(string) {
	h.mu.Lock()
	fn := h.onPublish
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (h *rebuildHooks) swap(string) {
	h.mu.Lock()
	fn := h.onSwap
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// startRebuildFixture starts the rebuild fixture, with the workspace's
// record changed by edit before it is stored.
func startRebuildFixture(t *testing.T, edit ...func(w *workspace.Workspace)) *rebuildFixture {
	t.Helper()
	return startRebuildFixtureWith(t, nil, edit...)
}

// startRebuildFixtureWith starts the rebuild fixture with the carry-patch
// component's dependencies changed by deps, when it is not nil, after the
// fixture's own changes, and the workspace's record changed by edit before
// it is stored.
func startRebuildFixtureWith(t *testing.T, deps func(d *carrypatch.Deps), edit ...func(w *workspace.Workspace)) *rebuildFixture {
	t.Helper()
	return startRebuildFixtureOn(t, "", deps, edit...)
}

// startRebuildFixtureOn is startRebuildFixtureWith with the hub's
// configuration given toml, after its [server] section.
func startRebuildFixtureOn(t *testing.T, toml string, deps func(d *carrypatch.Deps), edit ...func(w *workspace.Workspace)) *rebuildFixture {
	t.Helper()
	return startRebuildFixtureFrom(t, nil, toml, deps, edit...)
}

// startRebuildFixtureFrom is startRebuildFixtureOn with the upstream's
// first commit holding files, when it is not nil, rather than the test
// origin's own file.
func startRebuildFixtureFrom(t *testing.T, files map[string]string, toml string, deps func(d *carrypatch.Deps), edit ...func(w *workspace.Workspace)) *rebuildFixture {
	t.Helper()
	return startFixture(t, fixtureOptions{files: files, toml: toml, deps: deps}, edit...)
}

// fixtureOptions are what a test changes of the rebuild fixture: the files
// of the upstream's first commit, when not nil, the hub's configuration
// after its [server] section, the carry-patch component's dependencies,
// changed by deps after the fixture's own changes, and the composition,
// changed by compose after the fixture's own changes.
type fixtureOptions struct {
	files   map[string]string
	toml    string
	deps    func(d *carrypatch.Deps)
	compose func(c *hub.Composition)
}

// startFixture starts the rebuild fixture with o, with the workspace's
// record changed by edit before it is stored. Every git of the hub, the
// workspace service's that fetches origin for a sync included, trusts the
// upstream and the origin.
func startFixture(t *testing.T, o fixtureOptions, edit ...func(w *workspace.Workspace)) *rebuildFixture {
	t.Helper()
	files, toml, deps := o.files, o.toml, o.deps
	up := gitrepotest.NewOrigin(t)
	var u1 string
	if files == nil {
		u1 = up.Commit("main", "U1")
	} else {
		u1 = up.Branch("main", gitrepotest.Commit(t, up.Dir(), "U1", files))
	}
	origin := gitrepotest.SmartOrigin(t)
	bundle := filepath.Join(t.TempDir(), "ca-bundle.pem")
	var pems []byte
	for _, o := range []*gitrepotest.Origin{up, origin} {
		b, err := os.ReadFile(o.CAFile())
		if err != nil {
			t.Fatal(err)
		}
		pems = append(pems, b...)
	}
	if err := os.WriteFile(bundle, pems, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &rebuildFixture{
		addHub:   &addHub{readHub: &readHub{}, now: time.Now().UTC().Truncate(time.Second)},
		slug:     "acme",
		upstream: up,
		origin:   origin,
		secrets:  &rebuildSecrets{values: map[string]string{}},
		runner:   &attemptRunner{runs: map[string]*attemptRun{}},
	}
	// The queue's timers never fire: the test does not advance its clock.
	clock := hubtest.NewJobClock(f.clock)
	f.Hub = hubtest.StartWith(t, hubtest.Config{
		Now:  f.clock,
		TOML: toml,
		Env:  map[string]string{"GIT_SSL_CAINFO": bundle},
		Compose: func(c *hub.Composition) {
			f.comp, f.queue = c.CarryPatch(), c.Jobs()
			f.runner.comp = f.comp
			f.queue.Register(carrypatch.RebuildJobType, f.runner)
			c.AdjustJobs(func(d *jobs.Deps) { d.Clock, d.NoWake = clock, true })
			c.AdjustCarryPatch(func(d *carrypatch.Deps) {
				f.repoPath = d.RepoPath
				f.secrets.SecretReader = d.Secrets
				d.Secrets = f.secrets
				d.Git.Env = append(slices.Clone(d.Git.Env), "GIT_SSL_CAINFO="+bundle)
				d.BeforeRebuildPatch, d.BeforeRebuildPublish, d.BeforeRebuildSwap = f.hooks.patch, f.hooks.publish, f.hooks.swap
				g := d.Git
				d.FetchRemote = func(ctx context.Context, dir, url, name string, cred gitrepo.Credential, deadline time.Duration) (map[string]string, error) {
					if f.stopFetch.Load() {
						f.runner.cancelAttempt()
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return g.FetchRemote(ctx, dir, url, name, cred, deadline)
				}
				if deps != nil {
					deps(d)
				}
			})
			if o.compose != nil {
				o.compose(c)
			}
		},
	})
	// The dispatcher's own pass at its start could claim the first job; from
	// here on only the test's passes do.
	hubtest.WaitClosed(t, clock.Armed(jobs.DefaultInterval), "the job queue's first pass")
	f.owner = f.CreateUser(t, "owner")
	f.key = f.IssueKey(t, f.owner)
	f.org = f.CreateOrg(t, "acme")
	f.AddMember(t, f.org, f.owner)

	f.add(t, f.slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, append([]func(w *workspace.Workspace){
		func(w *workspace.Workspace) {
			w.IntegrationBranch = new("deploy")
			w.UpstreamURL = new(up.URL())
			w.GitURL = new(origin.URL())
			w.HeadSHA = new(u1)
		},
	}, edit...)...)
	f.copy = f.repoPath(f.slug)
	gitrepotest.Git(t, f.copy, "init", "--quiet", "--bare", f.copy)
	if err := f.comp.EnsureSetup(context.Background(), f.record(t)); err != nil {
		t.Fatalf("set up the workspace %s: %v", f.slug, err)
	}
	f.base = refIn(f.copy, "refs/heads/deploy")
	if f.base == "" || f.base != up.Head("main") {
		t.Fatalf("the set-up wrote deploy at %q, want the upstream's main %s", f.base, up.Head("main"))
	}
	return f
}

// record reads the fixture's workspace.
func (f *rebuildFixture) record(t *testing.T) workspace.Workspace {
	t.Helper()
	w, found, err := workspace.BySlug(context.Background(), f.Store.Main(), f.slug)
	if err != nil || !found {
		t.Fatalf("read the workspace %s: found %v, %v", f.slug, found, err)
	}
	return w
}

// addPatch stores an active patch of branch at the end of the patch list,
// changed by edit before it is stored, whose branch in the hub's copy holds
// one commit on the upstream base that adds a file of its own, and returns
// it.
func (f *rebuildFixture) addPatch(t *testing.T, branch string, edit ...func(p *carrypatch.Patch)) carrypatch.Patch {
	t.Helper()
	file := strings.ReplaceAll(branch, "/", "-") + ".txt"
	return f.putPatch(t, branch, gitrepotest.Commit(t, f.copy, branch, map[string]string{file: branch + "\n"}, f.base), edit...)
}

// putPatch stores an active patch of branch at the end of the patch list,
// changed by edit before it is stored, with its branch in the hub's copy at
// tip, and returns it.
func (f *rebuildFixture) putPatch(t *testing.T, branch, tip string, edit ...func(p *carrypatch.Patch)) carrypatch.Patch {
	t.Helper()
	live, err := carrypatch.ListLivePatches(context.Background(), f.Store.Main(), f.slug)
	if err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/"+branch, tip)
	p := livePatch(f.slug, branch, len(live)+1)
	for _, fn := range edit {
		fn(&p)
	}
	f.put(t, p)
	return p
}

// withStatus is an edit of a patch that gives it status and the conflict
// files given.
func withStatus(status string, conflictFiles ...string) func(p *carrypatch.Patch) {
	return func(p *carrypatch.Patch) { p.Status, p.ConflictFiles = status, conflictFiles }
}

// moveUpstream gives the upstream's main a commit on top of its head that
// writes the files given, and returns it: the next attempt's upstream
// base.
func (f *rebuildFixture) moveUpstream(t *testing.T, msg string, files map[string]string) string {
	t.Helper()
	return f.upstream.Branch("main", gitrepotest.Commit(t, f.upstream.Dir(), msg, files, f.upstream.Head("main")))
}

// stored is the stored patch id of the workspace.
func (f *rebuildFixture) stored(t *testing.T, id string) carrypatch.Patch {
	t.Helper()
	return (&patchHub{addHub: f.addHub}).stored(t, f.slug, id)
}

// patchEvents are the stored events of the workspace of type typ, every
// parley.patch.* event when typ is "", oldest first.
func (f *rebuildFixture) patchEvents(t *testing.T, typ string) []patchEvent {
	t.Helper()
	return (&patchHub{addHub: f.addHub}).patchEvents(t, f.slug, typ)
}

// patchList is every stored patch of the workspace: those that are not
// deleted in position order, then the deleted ones.
func (f *rebuildFixture) patchList(t *testing.T) []carrypatch.Patch {
	t.Helper()
	ctx := context.Background()
	live, err := carrypatch.ListLivePatches(ctx, f.Store.Main(), f.slug)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := carrypatch.ListDeletedPatches(ctx, f.Store.Main(), f.slug)
	if err != nil {
		t.Fatal(err)
	}
	return append(live, deleted...)
}

// attempt queues a rebuild of the workspace and runs one attempt of its job
// through the job queue, calling before, when it is not nil, at the start of
// the attempt; it waits until the attempt's outcome is settled, and returns
// the outcome and the job's id.
func (f *rebuildFixture) attempt(t *testing.T, before func(ctx context.Context, j jobs.Job)) (carrypatch.AttemptOutcome, string) {
	t.Helper()
	return f.attemptWith(t, carrypatch.RebuildOptions{}, before)
}

// attemptWith is attempt with the rebuild queued with the strategy and
// the fail mode of opts.
func (f *rebuildFixture) attemptWith(t *testing.T, opts carrypatch.RebuildOptions, before func(ctx context.Context, j jobs.Job)) (carrypatch.AttemptOutcome, string) {
	t.Helper()
	ctx := context.Background()
	opts.ActorType, opts.SubmittedBy = event.ActorSystem, event.ActorSystem
	res, err := f.comp.EnqueueRebuild(ctx, f.slug, opts)
	if err != nil || !res.Queued {
		t.Fatalf("queue a rebuild of %s = %+v, %v, want a queued job", f.slug, res, err)
	}
	return f.rerun(t, res.JobID, before, nil), res.JobID
}

// rerun runs one attempt of the job id, queued or due for its next attempt,
// through the job queue, calling before, when it is not nil, at the start
// of the attempt and atSettle, when it is not nil, in the settlement's
// transaction after the rebuild handler's settlement hook; it waits until
// the attempt's outcome is settled, and returns the outcome.
func (f *rebuildFixture) rerun(t *testing.T, id string, before func(ctx context.Context, j jobs.Job), atSettle func(j jobs.Job)) carrypatch.AttemptOutcome {
	t.Helper()
	ctx := context.Background()
	run := f.runner.expect(id, before)
	run.atSettle = atSettle
	if err := f.queue.Pass(ctx); err != nil {
		t.Fatalf("pass the job queue: %v", err)
	}
	// An attempt runs git against the test origins, which may take longer
	// than a step of the queue under the load of the whole suite.
	waitAttempt(t, run.ran, "the attempt of the job "+id+" to end")
	if run.out.Kind != attemptEnded {
		waitAttempt(t, run.settled, "the outcome of the job "+id+" to be settled")
		// The store runs its write transactions one at a time, in order: an
		// empty one asked for once the settlement hook ran ends after the
		// settlement committed.
		if err := f.Store.Tx(ctx, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	return run.out
}

// waitAttempt waits up to a minute for ch to be closed, and fails the test
// then.
func waitAttempt(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Minute):
		t.Fatalf("waited a minute for %s", what)
	}
}

// earlierResult is a start of an attempt that stores, as the job's result,
// what an earlier attempt of it would have left there.
func (f *rebuildFixture) earlierResult(t *testing.T) func(ctx context.Context, j jobs.Job) {
	return func(ctx context.Context, j jobs.Job) {
		doc := fmt.Sprintf(`{"upstream_head_sha":%q,"patches_applied":1,"patch_results":[{"patch_id":"earlier"}]}`, strings.Repeat("e", 40))
		if err := f.queue.SetResult(ctx, j.ID, json.RawMessage(doc)); err != nil {
			t.Errorf("store an earlier result of the job %s: %v", j.ID, err)
		}
	}
}

// result is the result document of the job id.
func (f *rebuildFixture) result(t *testing.T, id string) map[string]any {
	t.Helper()
	j, err := f.queue.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read the job %s: %v", id, err)
	}
	m := map[string]any{}
	if j.Result != nil {
		if err := json.Unmarshal(j.Result, &m); err != nil {
			t.Fatalf("the result of the job %s is not a JSON object: %v", id, err)
		}
	}
	return m
}

// patchResults is the patch_results of the result document of the job id.
func (f *rebuildFixture) patchResults(t *testing.T, id string) []map[string]any {
	t.Helper()
	raw, ok := f.result(t, id)["patch_results"].([]any)
	if !ok {
		t.Fatalf("the result of the job %s holds no patch_results list", id)
	}
	results := make([]map[string]any, len(raw))
	for i, r := range raw {
		if results[i], ok = r.(map[string]any); !ok {
			t.Fatalf("patch_results[%d] of the job %s = %v, not an object", i, id, r)
		}
	}
	return results
}

// commitAs writes into the repository dir a commit with the message msg,
// the author line author (name <email> unix-time zone) and the parents
// given, the first parent first, whose tree is the first parent's with the
// files given, and returns its id.
func commitAs(t *testing.T, dir, msg, author string, files map[string]string, parents ...string) string {
	t.Helper()
	first := ""
	if len(parents) > 0 {
		first = parents[0]
	}
	var obj strings.Builder
	obj.WriteString("tree " + gitrepotest.Tree(t, dir, first, files) + "\n")
	for _, p := range parents {
		obj.WriteString("parent " + p + "\n")
	}
	obj.WriteString("author " + author + "\ncommitter Origin <origin@example.invalid> 1500000000 +0000\n\n" + msg + "\n")
	id, err := gitrepotest.Run(dir, obj.String(), "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// commitsIn are the commits of the repository dir that head reaches and
// base does not, oldest first, each shown with the git log format given.
func commitsIn(t *testing.T, dir, base, head, format string) []string {
	t.Helper()
	out := gitrepotest.Git(t, dir, "log", "--reverse", "--date=raw", "--format="+format, base+".."+head)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// rebuildSecrets are the workspace secrets of the rebuild fixture, which the
// test sets; every other secret is read from the hub's reader. While fail
// is set, a read of an upstream credential's secret fails with it.
type rebuildSecrets struct {
	workspace.SecretReader
	mu     sync.Mutex
	values map[string]string
	fail   error
}

// failUpstream makes every read of an upstream credential's secret fail
// with err from now on; nil makes them succeed again.
func (s *rebuildSecrets) failUpstream(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// set stores value as the workspace secret name.
func (s *rebuildSecrets) set(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = value
}

func (s *rebuildSecrets) Get(ctx context.Context, scope secret.Scope, scopeID, name string) (string, bool, error) {
	if scope != secret.ScopeWorkspace {
		return s.SecretReader.Get(ctx, scope, scopeID, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil && strings.HasPrefix(name, "UPSTREAM_") {
		return "", false, s.fail
	}
	v, ok := s.values[name]
	return v, ok, nil
}

// attemptRunner is the handler of the fixture's rebuild jobs: an attempt of
// a job a test expects runs as the hub's rebuild handler runs it, which
// also settles it, and the runner keeps its outcome; an attempt of any
// other job completes at once, with no event.
type attemptRunner struct {
	comp *carrypatch.Component
	mu   sync.Mutex
	runs map[string]*attemptRun
	// cancel cancels the context of the attempt that runs, as a stop of the
	// hub would.
	cancel context.CancelFunc
	// afterRun, when set, is called once an expected attempt has its
	// outcome, before the job queue settles it.
	afterRun func(j jobs.Job)
}

// cancelAttempt cancels the context of the attempt that runs.
func (r *attemptRunner) cancelAttempt() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
}

// attemptRun is the attempt of a job a test expects.
type attemptRun struct {
	// before, when set, is called at the start of the attempt.
	before func(ctx context.Context, j jobs.Job)
	// out is the attempt's outcome once ran is closed.
	out carrypatch.AttemptOutcome
	// ran is closed once the attempt has its outcome, or has panicked,
	// settled once that is being settled.
	ran, settled chan struct{}
	settledOnce  sync.Once
	// atSettle, when set, is called in the settlement's transaction once the
	// rebuild handler's settlement hook has run, before the transaction
	// commits.
	atSettle func(j jobs.Job)
}

// expect makes the next attempt of the job id one the test expects, with
// before called at its start.
func (r *attemptRunner) expect(id string, before func(ctx context.Context, j jobs.Job)) *attemptRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := &attemptRun{before: before, ran: make(chan struct{}), settled: make(chan struct{})}
	r.runs[id] = run
	return run
}

func (r *attemptRunner) run(id string) *attemptRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[id]
}

func (r *attemptRunner) Run(ctx context.Context, j jobs.Job) error {
	run := r.run(j.ID)
	if run == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	// A panic of the attempt reaches the queue, which settles it.
	defer close(run.ran)
	if run.before != nil {
		run.before(ctx, j)
	}
	var err error
	run.out, err = carrypatch.RunRebuild(ctx, r.comp, j)
	r.mu.Lock()
	after := r.afterRun
	r.mu.Unlock()
	if after != nil {
		after(j)
	}
	return err
}

func (r *attemptRunner) Settled(ctx context.Context, tx *sql.Tx, j jobs.Job, willRetry bool) error {
	run := r.run(j.ID)
	if run == nil {
		return nil
	}
	defer run.settledOnce.Do(func() { close(run.settled) })
	err := r.comp.RebuildHandler().Settled(ctx, tx, j, willRetry)
	if run.atSettle != nil {
		run.atSettle(j)
	}
	return err
}

// visibleRefs maps every ref of the repository dir under refs/heads/,
// refs/tags/ and refs/parley/, the refs a client sees or the hub keeps, by
// its full name to its id. It fails no test, so that a hook may call it.
func visibleRefs(dir string) (map[string]string, error) {
	out, err := gitrepotest.Run(dir, "", "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/", "refs/tags/", "refs/parley/")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for line := range strings.Lines(out) {
		name, id, _ := strings.Cut(strings.TrimSpace(line), " ")
		refs[name] = id
	}
	return refs, nil
}

// deleteNow archives the fixture's workspace and deletes it, with its jobs
// and its copy, through the workspace's delete route. It fails no test, so
// that a hook may call it.
func (f *rebuildFixture) deleteNow() error {
	if err := f.storeState(f.slug, workspace.StatusArchived, workspace.MarkerNone); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodDelete, f.URL("/api/v1/workspaces/"+f.slug), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE /workspaces/%s = %d %s, want 204", f.slug, res.StatusCode, body)
	}
	return nil
}

// lastEvent is the sequence number of the newest stored event of the
// fixture's workspace, 0 when there is none.
func (f *rebuildFixture) lastEvent() (int64, error) {
	var seq sql.NullInt64
	err := f.Store.Main().QueryRow(`SELECT max(seq) FROM hub_events WHERE workspace = ?`, f.slug).Scan(&seq)
	return seq.Int64, err
}

// recordingDescriber is a conflict-file describer that records every call
// and returns, for each path it is given, a content entry whose ours and
// theirs are the path's blobs in the two commits it is given.
type recordingDescriber struct {
	mu    sync.Mutex
	calls []describeCall
}

// describeCall is one call of a conflict-file describer.
type describeCall struct {
	base, target, commit string
	paths                []string
}

func (d *recordingDescriber) ConflictFiles(_ context.Context, repo *gitrepo.Repo, base, target, commit string, paths []string) ([]gitrepo.ConflictFile, error) {
	d.mu.Lock()
	d.calls = append(d.calls, describeCall{base, target, commit, slices.Clone(paths)})
	d.mu.Unlock()
	files := make([]gitrepo.ConflictFile, 0, len(paths))
	for _, p := range paths {
		files = append(files, describedFile(repo.Dir(), target, commit, p))
	}
	return files, nil
}

// recorded are the calls the describer had.
func (d *recordingDescriber) recorded() []describeCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

// describedFile is the entry the recording describer gives for path.
func describedFile(dir, target, commit, path string) gitrepo.ConflictFile {
	return gitrepo.ConflictFile{Path: path, Kind: gitrepo.ConflictContent, Ours: blobSide(dir, target, path), Theirs: blobSide(dir, commit, path)}
}

// blobSide is the file path of the commit rev of the repository dir as a
// side of a conflict, nil when rev holds none.
func blobSide(dir, rev, path string) *gitrepo.ConflictSide {
	out, err := gitrepotest.Run(dir, "", "ls-tree", rev, "--", path)
	if fields := strings.Fields(out); err == nil && len(fields) == 4 {
		return &gitrepo.ConflictSide{SHA: fields[2], Mode: fields[0], Path: path}
	}
	return nil
}

// conflictFixture is the rebuild fixture with the recording describer,
// whose upstream's main holds a.txt and b.txt of three lines each; cut is
// that commit, in the copy too, which conflicting patches are cut from.
type conflictFixture struct {
	*rebuildFixture
	describer *recordingDescriber
	cut       string
}

// startConflictFixture starts the conflict fixture with the carry-patch
// component's dependencies changed by deps, when it is not nil.
func startConflictFixture(t *testing.T, deps func(d *carrypatch.Deps)) *conflictFixture {
	t.Helper()
	desc := &recordingDescriber{}
	f := startRebuildFixtureFrom(t, map[string]string{"a.txt": "1\n2\n3\n", "b.txt": "1\n2\n3\n"}, "", func(d *carrypatch.Deps) {
		d.ConflictFiles = desc
		if deps != nil {
			deps(d)
		}
	})
	return &conflictFixture{rebuildFixture: f, describer: desc, cut: f.base}
}

// conflictingPatch stores an active patch of branch, cut from the upstream's
// main, of two commits: the first adds a file of its own, the second
// changes line 3 of each of files, as moveOn does on upstream. It returns
// the patch and its two commits.
func (f *conflictFixture) conflictingPatch(t *testing.T, branch string, files ...string) (carrypatch.Patch, [2]string) {
	t.Helper()
	name := strings.ReplaceAll(branch, "/", "-")
	first := gitrepotest.Commit(t, f.copy, name+" adds", map[string]string{name + ".txt": branch + "\n"}, f.cut)
	changes := map[string]string{}
	for _, file := range files {
		changes[file] = "1\n2\n3 " + branch + "\n"
	}
	second := gitrepotest.Commit(t, f.copy, name+" changes", changes, first)
	return f.putPatch(t, branch, second), [2]string{first, second}
}

// moveOn gives the upstream's main a commit that changes line 3 of a.txt
// and b.txt, and returns it: the upstream base of the next attempt.
func (f *conflictFixture) moveOn(t *testing.T) string {
	t.Helper()
	return f.moveUpstream(t, "U3", map[string]string{"a.txt": "1\n2\n3 upstream\n", "b.txt": "1\n2\n3 upstream\n"})
}

// continueRebase are the options of a rebuild by rebase that goes on past
// a conflicting patch.
var continueRebase = carrypatch.RebuildOptions{Strategy: new("rebase"), FailMode: new("continue")}

// slowGitLimit is the per-operation limit of the attempt whose replay
// sleeps: far below the filter's sleep, and above what any other git
// operation of the attempt takes under load.
const slowGitLimit = 5 * time.Second

// slowFilter gives slow.txt, in the repository dir's own configuration, a
// smudge filter that writes its process id to a file and then sleeps for a
// minute, and returns that file.
func slowFilter(t *testing.T, dir string) string {
	t.Helper()
	tmp := t.TempDir()
	pid, script, attrs := filepath.Join(tmp, "pid"), filepath.Join(tmp, "slow.sh"), filepath.Join(tmp, "attributes")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho $$ > "+pid+"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attrs, []byte("slow.txt filter=slow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, dir, "config", "core.attributesFile", attrs)
	gitrepotest.Git(t, dir, "config", "filter.slow.smudge", script)
	return pid
}

// waitGone waits for the process whose id the file pidFile holds to be
// gone, and fails the test when it is still there after ten seconds: the
// process sleeps for a minute unless it is killed.
func waitGone(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the filter of slow.txt did not run: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !alive(pid) {
			return
		}
	}
	t.Errorf("the git child process %d is still alive after the attempt", pid)
}

// alive reports whether the process pid runs: one that exited and is not
// yet reaped by its parent does not.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	stat := strings.TrimSpace(string(out))
	return err == nil && stat != "" && !strings.HasPrefix(stat, "Z")
}

// resolvedA is the content a.txt is resolved to.
const resolvedA = "1\n2\n3 resolved\n"

// seedResolution records in the repository dir the resolution of the
// conflict that replaying commit onto onto meets, as a developer resolving
// it once in a working tree of the repository with rerere on would: the
// files of resolved get the content given, every other conflicted path is
// left unresolved.
func seedResolution(t *testing.T, dir, onto, commit string, resolved map[string]string) {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "seed")
	gitrepotest.Git(t, dir, "worktree", "add", "--quiet", "--detach", wt, onto)
	defer gitrepotest.Git(t, dir, "worktree", "remove", "--force", wt)
	rerere := func(args ...string) error {
		_, err := gitrepotest.Run(filepath.Join(dir, "worktrees", "seed"), "", append([]string{"-C", wt, "-c", "rerere.enabled=true"}, args...)...)
		return err
	}
	if err := rerere("cherry-pick", "--no-commit", commit); err == nil {
		t.Fatalf("%s applies onto %s; want a conflict to resolve", commit, onto)
	}
	for name, content := range resolved {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := rerere("rerere"); err != nil {
		t.Fatal(err)
	}
}

// fetch fetches the upstream into the hub's copy, as an attempt would.
func (f *conflictFixture) fetch(t *testing.T) {
	t.Helper()
	if err := f.comp.EnsureSetup(context.Background(), f.record(t)); err != nil {
		t.Fatal(err)
	}
}

// entries are the remembered-resolution entries of the fixture's workspace.
func (f *conflictFixture) entries(t *testing.T) []carrypatch.RerereEntry {
	t.Helper()
	e, err := f.comp.RerereEntries(context.Background(), f.slug)
	if err != nil {
		t.Fatalf("the remembered-resolution entries of %s: %v", f.slug, err)
	}
	return e
}

// rrCacheIDs are the ids of the conflicts the rr-cache of the repository
// dir holds.
func rrCacheIDs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "rr-cache"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Name())
	}
	return ids
}

// sameEntries reports whether got are the entries want.
func sameEntries(got, want []carrypatch.RerereEntry) bool {
	return slices.EqualFunc(got, want, func(a, b carrypatch.RerereEntry) bool {
		return a.ID == b.ID && a.Path == b.Path && a.Resolved == b.Resolved && a.RecordedAt.Equal(b.RecordedAt)
	})
}

// fileIn is the content of path in rev of the repository dir.
func fileIn(t *testing.T, dir, rev, path string) string {
	t.Helper()
	return string(gitrepotest.Blob(t, dir, rev+":"+path))
}

// another is a second carry-patch workspace slug of the fixture's hub, set
// up like the fixture's own, with its own copy, which the fixture's attempts
// rebuild when they are run through it.
func (f *conflictFixture) another(t *testing.T, slug string) *conflictFixture {
	t.Helper()
	w := f.record(t)
	f.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, func(n *workspace.Workspace) {
		n.IntegrationBranch, n.UpstreamURL, n.GitURL = w.IntegrationBranch, w.UpstreamURL, w.GitURL
	})
	g := &rebuildFixture{addHub: f.addHub, slug: slug, upstream: f.upstream, origin: f.origin,
		copy: f.repoPath(slug), comp: f.comp, queue: f.queue, secrets: f.secrets, runner: f.runner}
	gitrepotest.Git(t, g.copy, "init", "--quiet", "--bare", g.copy)
	if err := g.comp.EnsureSetup(context.Background(), g.record(t)); err != nil {
		t.Fatalf("set up the workspace %s: %v", slug, err)
	}
	g.base = refIn(g.copy, "refs/heads/deploy")
	return &conflictFixture{rebuildFixture: g, describer: f.describer, cut: refIn(g.copy, "refs/remotes/upstream/main")}
}

// noScratchLeft fails the test when the repository dir holds a scratch
// working tree, or a record of one.
func noScratchLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, gitrepo.ScratchParent))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s holds %q, want no entry", gitrepo.ScratchParent, names)
	}
	if got := gitrepotest.Git(t, dir, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("git worktree list = %q, want the repository alone", got)
	}
}

// job is the stored job id.
func (f *rebuildFixture) job(t *testing.T, id string) jobs.Job {
	t.Helper()
	j, err := f.queue.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read the job %s: %v", id, err)
	}
	return j
}

// pushSetting is the workspace setting that makes a rebuild push its
// integration branch to origin.
const pushSetting = "rebuild_push_integration_branch"

// pushOn is an edit of the workspace that turns the push of the integration
// branch on.
func pushOn(w *workspace.Workspace) {
	w.Settings = map[string]json.RawMessage{pushSetting: json.RawMessage("true")}
}

// recordingPusher pushes as the hub does, through push, and keeps every
// request it was given.
type recordingPusher struct {
	push func(ctx context.Context, req gitrepo.PushRequest) (gitrepo.PushResult, error)
	mu   sync.Mutex
	reqs []gitrepo.PushRequest
}

// pushWith makes the component push through a recording pusher whose
// pushes run fn, given the hub's git; nil is the hub's own push.
func (p *recordingPusher) pushWith(fn func(g gitrepo.Git, ctx context.Context, req gitrepo.PushRequest) (gitrepo.PushResult, error)) func(d *carrypatch.Deps) {
	return func(d *carrypatch.Deps) {
		g := d.Git
		if fn == nil {
			fn = func(g gitrepo.Git, ctx context.Context, req gitrepo.PushRequest) (gitrepo.PushResult, error) {
				return g.Push(ctx, req)
			}
		}
		p.push = func(ctx context.Context, req gitrepo.PushRequest) (gitrepo.PushResult, error) { return fn(g, ctx, req) }
		d.Push = p.Push
	}
}

func (p *recordingPusher) Push(ctx context.Context, req gitrepo.PushRequest) (gitrepo.PushResult, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	return p.push(ctx, req)
}

// requests are the requests the pusher was given.
func (p *recordingPusher) requests() []gitrepo.PushRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]gitrepo.PushRequest(nil), p.reqs...)
}

// setSettingNow stores value as the workspace setting name of the fixture's
// workspace, as a caller changing the settings during an attempt would. It
// fails no test, so that a hook may call it.
func (f *rebuildFixture) setSettingNow(name string, value any) error {
	ctx := context.Background()
	return f.Store.Tx(ctx, func(tx *sql.Tx) error {
		v, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE workspaces SET settings = json_set(settings, ?, json(?)) WHERE slug = ?`,
			"$."+name, string(v), f.slug)
		return err
	})
}

// onOrigin gives origin's deploy a commit of the hub's copy on the upstream
// base, which the hub records as origin's head of deploy when recorded is
// set, and returns it.
func (f *rebuildFixture) onOrigin(t *testing.T, recorded bool) string {
	t.Helper()
	x := gitrepotest.Commit(t, f.copy, "deployed elsewhere", map[string]string{"deployed.txt": "x\n"}, f.base)
	gitrepotest.Git(t, f.copy, "push", "--quiet", f.origin.Dir(), x+":refs/heads/deploy")
	if recorded {
		gitrepotest.Git(t, f.copy, "update-ref", "refs/remotes/origin/deploy", x)
	}
	return x
}

// cleanUpdates are the events of updates that are parley.patch.update with
// the reason rebuild_clean, of a patch a rebuild applied again.
func cleanUpdates(updates []patchEvent) []patchEvent {
	var out []patchEvent
	for _, e := range updates {
		if e.Metadata["reason"] == "rebuild_clean" {
			out = append(out, e)
		}
	}
	return out
}

// resultMember is the member name of the job's result document doc, nil
// when it has none or doc does not decode.
func resultMember(doc []byte, name string) any {
	m := map[string]any{}
	_ = json.Unmarshal(doc, &m)
	return m[name]
}

// position is the position p as a test prints it, "none" when nil.
func position(p *int) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprint(*p)
}

// followUpSettings is an edit of the workspace that stores the rebuild
// options rebase and fail_fast and the two settings that decide which
// stale inputs queue a follow-up rebuild.
func followUpSettings(afterPush, afterSync bool) func(w *workspace.Workspace) {
	return func(w *workspace.Workspace) {
		w.Settings = map[string]json.RawMessage{
			"rebuild_strategy":        json.RawMessage(`"rebase"`),
			"rebuild_fail_mode":       json.RawMessage(`"fail_fast"`),
			"auto_rebuild_after_push": json.RawMessage(boolJSON(afterPush)),
			"auto_rebuild_after_sync": json.RawMessage(boolJSON(afterSync)),
		}
	}
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// setRefNow points ref of the hub's copy at id, or deletes it when id is
// "", as another writer would while an attempt runs. It fails no test
// fatally, so that a hook may call it.
func (f *rebuildFixture) setRefNow(t *testing.T, ref, id string) {
	args := []string{"update-ref", ref, id}
	if id == "" {
		args = []string{"update-ref", "-d", ref}
	}
	if _, err := gitrepotest.Run(f.copy, "", args...); err != nil {
		t.Errorf("git %v: %v", args, err)
	}
}

// softDeleteNow deletes the patch id as a caller's removal would while an
// attempt runs. It fails no test fatally, so that a hook may call it.
func (f *rebuildFixture) softDeleteNow(t *testing.T, id string) {
	ctx := context.Background()
	err := f.Store.Tx(ctx, func(tx *sql.Tx) error {
		p, found, err := carrypatch.ReadPatch(ctx, tx, f.slug, id)
		if err != nil || !found {
			return fmt.Errorf("read the patch: found %v, %w", found, err)
		}
		return carrypatch.MarkDeleted(ctx, tx, p, f.clock())
	})
	if err != nil {
		t.Errorf("delete the patch %s: %v", id, err)
	}
}

// rebuildJobs are the rebuild jobs of the fixture's workspace in status,
// every one when status is "", newest first.
func (f *rebuildFixture) rebuildJobs(t *testing.T, status string) []jobs.Job {
	t.Helper()
	js, err := f.queue.List(context.Background(), carrypatch.RebuildJobType, f.slug, status, nil, 200)
	if err != nil {
		t.Fatalf("list the rebuild jobs of %s: %v", f.slug, err)
	}
	return js
}

// payloadOf is the payload of the job j as a JSON object.
func payloadOf(t *testing.T, j jobs.Job) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(j.Payload, &m); err != nil {
		t.Fatalf("the payload of the job %s is not a JSON object: %v", j.ID, err)
	}
	return m
}

// checkFollowUp checks that the ended job id queued exactly one follow-up
// rebuild, still queued, whose payload holds want, and wrote one
// parley.rebuild.followup about id with the stale inputs given, and
// returns the follow-up job.
func (f *rebuildFixture) checkFollowUp(t *testing.T, id string, want map[string]any, stalePatches []any, upstreamStale bool) jobs.Job {
	t.Helper()
	queued := f.rebuildJobs(t, jobs.StatusQueued)
	if len(queued) != 1 || queued[0].ID == id {
		t.Fatalf("the queued rebuild jobs are %v, want one follow-up of %s", queued, id)
	}
	next := queued[0]
	payload := payloadOf(t, next)
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("the follow-up's %s = %v, want %v", k, payload[k], v)
		}
	}
	events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFollowUp)
	if len(events) != 1 {
		t.Fatalf("%d parley.rebuild.followup events, want one", len(events))
	}
	e := events[0]
	if e.ResourceType != "rebuild" || e.ResourceID != id || e.ActorType != event.ActorSystem || e.ActorID != nil {
		t.Errorf("the event is about %s %s by %s %v, want rebuild %s by system", e.ResourceType, e.ResourceID, e.ActorType, e.ActorID, id)
	}
	meta := map[string]any{"follow_up_job_id": next.ID, "stale_patches": stalePatches, "upstream_stale": upstreamStale}
	if !reflect.DeepEqual(e.Metadata, meta) {
		t.Errorf("the event's metadata = %v, want %v", e.Metadata, meta)
	}
	return next
}

// show is the commit rev of the repository dir with the git log format
// given.
func show(t *testing.T, dir, rev, format string) string {
	t.Helper()
	return gitrepotest.Git(t, dir, "log", "-1", "--date=raw", "--format="+format, rev)
}

func TestRebuild_FetchesUpstreamAndRecordsBase(t *testing.T) {
	t.Parallel()
	t.Run("every upstream branch, no tag, and the base", func(t *testing.T) {
		t.Parallel()
		f := startRebuildFixture(t)
		u2 := f.upstream.Commit("main", "U2")
		release := f.upstream.Commit("release", "R1")
		f.upstream.Tag("v9", "main")

		out, id := f.attempt(t, f.earlierResult(t))
		if out.Kind != attemptCompleted {
			t.Fatalf("the attempt = %+v, want completed", out)
		}
		res := f.result(t, id)
		if res["upstream_head_sha"] != u2 {
			t.Errorf("upstream_head_sha = %v, want the upstream's main %s", res["upstream_head_sha"], u2)
		}
		if got, ok := res["patch_results"].([]any); !ok || len(got) != 0 {
			t.Errorf("patch_results = %v, want [] in place of the earlier attempt's", res["patch_results"])
		}
		want := map[string]string{"main": u2, "release": release}
		if got := gitrepotest.Refs(t, f.copy, "refs/remotes/upstream/"); !maps.Equal(got, want) {
			t.Errorf("the copy's upstream branches = %v, want %v", got, want)
		}
		if got := gitrepotest.Refs(t, f.copy, "refs/tags/"); len(got) != 0 {
			t.Errorf("the copy's tags = %v, want none", got)
		}
	})

	cases := []struct {
		name           string
		secrets        map[string]string
		user, password string
	}{
		{"the upstream token before every other credential", map[string]string{
			carrypatch.SecretUpstreamGitPAT: "upstream-token", carrypatch.SecretUpstreamGitUsername: "upstream-user",
			carrypatch.SecretUpstreamGitPassword: "upstream-password", workspace.SecretGitPAT: "origin-token",
		}, "x-token-auth", "upstream-token"},
		{"the upstream user and password before the origin credential", map[string]string{
			carrypatch.SecretUpstreamGitUsername: "upstream-user", carrypatch.SecretUpstreamGitPassword: "upstream-password",
			workspace.SecretGitPAT: "origin-token",
		}, "upstream-user", "upstream-password"},
		{"the origin credential without an upstream one", map[string]string{
			workspace.SecretGitPAT: "origin-token",
		}, "x-token-auth", "origin-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			for name, value := range tc.secrets {
				f.secrets.set(name, value)
			}
			// The upstream takes this credential alone: a fetch that presents
			// another one fails.
			f.upstream.Accept(tc.user, tc.password)
			u2 := f.upstream.Commit("main", "U2")

			out, id := f.attempt(t, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed with the fetch presenting %s", out, tc.user)
			}
			if got := f.result(t, id)["upstream_head_sha"]; got != u2 {
				t.Errorf("upstream_head_sha = %v, want %s", got, u2)
			}
		})
	}
}

func TestRebuild_BaseFallsBackToWorkspaceDefaultBranch(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t, func(w *workspace.Workspace) { w.DefaultBranch = new("release") })
	gitrepotest.Git(t, f.upstream.Dir(), "update-ref", "-d", "refs/heads/main")
	f.upstream.Commit("trunk", "T1")
	release := f.upstream.Commit("release", "R1")
	f.upstream.SetDefaultBranch("gone")

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	if got := f.result(t, id)["upstream_head_sha"]; got != release {
		t.Errorf("upstream_head_sha = %v, want the head of the upstream's release %s", got, release)
	}
}

func TestRebuild_UpstreamFailuresAreRetryable(t *testing.T) {
	t.Parallel()
	gone := func(t *testing.T, f *rebuildFixture) {
		if err := os.RemoveAll(f.upstream.Dir()); err != nil {
			t.Fatal(err)
		}
	}
	unknown := func(t *testing.T, f *rebuildFixture) {
		gitrepotest.Git(t, f.upstream.Dir(), "update-ref", "-d", "refs/heads/main")
		f.upstream.Commit("trunk", "T1")
		f.upstream.SetDefaultBranch("gone")
	}
	// stop makes the fetch stop the hub, as a stop during the fetch would,
	// and fail with the cancelled context.
	stop := func(t *testing.T, f *rebuildFixture) { f.stopFetch.Store(true) }
	cases := []struct {
		name   string
		breaks func(t *testing.T, f *rebuildFixture)
		// noDeploy deletes the integration branch before the attempt, which
		// must not create it.
		noDeploy bool
		reason   string
	}{
		{"the upstream repository is gone and deploy does not exist", gone, true, "upstream_fetch_failed"},
		{"no upstream branch to start from and deploy does not exist", unknown, true, "upstream_unknown"},
		{"the hub stops during the fetch", stop, false, "shutdown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			f.addPatch(t, "patch/1")
			f.addPatch(t, "patch/2")
			if tc.noDeploy {
				gitrepotest.Git(t, f.copy, "update-ref", "-d", "refs/heads/deploy")
			}
			deploy := refIn(f.copy, "refs/heads/deploy")
			patches := f.patchList(t)
			tips := gitrepotest.Refs(t, f.copy, "refs/heads/patch/")
			tc.breaks(t, f)

			out, id := f.attempt(t, f.earlierResult(t))
			if out.Kind != attemptRetryable || out.Reason != tc.reason {
				t.Errorf("the attempt = %+v, want retryable with reason %s", out, tc.reason)
			}
			res := f.result(t, id)
			if got, ok := res["patch_results"].([]any); !ok || len(got) != 0 {
				t.Errorf("patch_results = %v, want []", res["patch_results"])
			}
			if res["upstream_head_sha"] != nil {
				t.Errorf("upstream_head_sha = %v, want null", res["upstream_head_sha"])
			}
			if got := refIn(f.copy, "refs/heads/deploy"); got != deploy {
				t.Errorf("deploy = %q, want it as it was, %q", got, deploy)
			}
			if got := f.patchList(t); !reflect.DeepEqual(got, patches) {
				t.Errorf("the patches = %+v, want them as they were, %+v", got, patches)
			}
			if got := gitrepotest.Refs(t, f.copy, "refs/heads/patch/"); !maps.Equal(got, tips) {
				t.Errorf("the patch branches = %v, want them as they were, %v", got, tips)
			}
		})
	}
}

func TestRebuild_WorkspaceStateAtStart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status       workspace.Status
		kind, reason string
	}{
		{workspace.StatusArchived, attemptPermanent, "workspace_archived"},
		{workspace.StatusFailed, attemptPermanent, "workspace_not_ready"},
		{workspace.StatusPending, attemptRetryable, "workspace_busy"},
		{workspace.StatusCloning, attemptRetryable, "workspace_busy"},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			f.upstream.Commit("main", "U2")
			f.setState(t, f.slug, tc.status, workspace.MarkerNone)
			requests := f.upstream.Requests()

			out, _ := f.attempt(t, nil)
			if out.Kind != tc.kind || out.Reason != tc.reason {
				t.Errorf("the attempt = %+v, want %s with reason %s", out, tc.kind, tc.reason)
			}
			if got := f.upstream.Requests(); got != requests {
				t.Errorf("the upstream got %d requests during the attempt, want none", got-requests)
			}
			if got := refIn(f.copy, "refs/remotes/upstream/main"); got != f.base {
				t.Errorf("the copy's upstream main = %s, want it unfetched at %s", got, f.base)
			}
		})
	}
}

func TestRebuild_ListsPatchesInPositionOrder(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	tips := map[string]string{}
	for _, b := range []string{"p1", "p2", "p3", "p4"} {
		tips[b] = gitrepotest.Commit(t, f.copy, b, map[string]string{b + ".txt": b + "\n"}, f.base)
		gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/"+b, tips[b])
	}
	// The list's order is not the order the patches were stored in.
	p1, p2, p3 := livePatch(f.slug, "p1", 1), livePatch(f.slug, "p2", 2), livePatch(f.slug, "p3", 3)
	f.put(t, p3, p1, deletedPatch(f.slug, "p4", at(10, 0)), p2)

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	got := f.patchResults(t, id)
	want := []carrypatch.Patch{p1, p2, p3}
	if len(got) != len(want) {
		t.Fatalf("patch_results = %v, want one entry for each of p1, p2 and p3", got)
	}
	for i, p := range want {
		r := got[i]
		if r["patch_id"] != p.ID || r["branch_name"] != p.BranchName || r["position"] != float64(*p.Position) || r["source_sha"] != tips[p.BranchName] {
			t.Errorf("patch_results[%d] = %v, want patch %s of %s at position %d from %s", i, r, p.ID, p.BranchName, *p.Position, tips[p.BranchName])
		}
	}
}

func TestRebuild_WorkspaceLeavesReadyDuringRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// leave makes the workspace leave ready at the attempt's step its
		// hooks give.
		leave        func(t *testing.T, f *rebuildFixture, left *int64)
		kind, reason string
	}{
		{"archived before the second patch", func(t *testing.T, f *rebuildFixture, _ *int64) {
			f.hooks.beforePatch(func(i int) {
				if i == 1 {
					if err := f.storeState(f.slug, workspace.StatusArchived, workspace.MarkerNone); err != nil {
						t.Errorf("archive the workspace: %v", err)
					}
				}
			})
		}, attemptPermanent, "workspace_archived"},
		{"recloned before the publish", func(t *testing.T, f *rebuildFixture, _ *int64) {
			f.hooks.beforePublish(func() {
				if err := f.storeState(f.slug, workspace.StatusCloning, workspace.MarkerNone); err != nil {
					t.Errorf("reclone the workspace: %v", err)
				}
			})
		}, attemptRetryable, "workspace_busy"},
		{"deleted with its jobs before the second patch", func(t *testing.T, f *rebuildFixture, left *int64) {
			f.hooks.beforePatch(func(i int) {
				if i != 1 {
					return
				}
				if err := f.deleteNow(); err != nil {
					t.Errorf("delete the workspace: %v", err)
				}
				seq, err := f.lastEvent()
				if err != nil {
					t.Errorf("read the workspace's events: %v", err)
				}
				*left = seq
			})
		}, attemptEnded, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			f.addPatch(t, "p1")
			f.addPatch(t, "p2")
			deploy := refIn(f.copy, "refs/heads/deploy")
			var left int64
			tc.leave(t, f, &left)

			out, id := f.attempt(t, nil)
			if out.Kind != tc.kind || out.Reason != tc.reason {
				t.Errorf("the attempt = %+v, want %s with reason %q", out, tc.kind, tc.reason)
			}
			if _, err := os.Stat(f.copy); err == nil {
				if got := refIn(f.copy, "refs/heads/deploy"); got != deploy {
					t.Errorf("deploy = %s, want it where it was, %s", got, deploy)
				}
			}
			if tc.kind != attemptEnded {
				return
			}
			if _, err := f.queue.Get(context.Background(), id); !errors.Is(err, jobs.ErrNotFound) {
				t.Errorf("read the job %s after the delete = %v, want %v", id, err, jobs.ErrNotFound)
			}
			if seq, err := f.lastEvent(); err != nil || seq != left {
				t.Errorf("the newest event of %s = %d, %v; want none after the delete's, %d", f.slug, seq, err, left)
			}
		})
	}
}

func TestRebuild_PushDuringRunDoesNotChangeTheResult(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	f.addPatch(t, "p1")
	f.addPatch(t, "p2")
	tip := refIn(f.copy, "refs/heads/p2")
	f.hooks.beforePatch(func(i int) {
		if i != 0 {
			return
		}
		x, err := gitrepotest.Run(f.copy, "", "commit-tree", tip+"^{tree}", "-p", tip, "-m", "pushed during the run")
		if err != nil {
			t.Errorf("write the commit X: %v", err)
			return
		}
		ctx := context.Background()
		repo, err := gitrepo.Git{}.Open(ctx, f.copy)
		if err == nil {
			err = repo.CompareAndSwapRef(ctx, "refs/heads/p2", x, tip)
		}
		if err != nil {
			t.Errorf("move p2 from %s to %s during the run = %v, want it moved at once", tip, x, err)
		}
	})

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	if got := f.patchResults(t, id)[1]["source_sha"]; got != tip {
		t.Errorf("source_sha of p2 = %v, want the tip read at the listing, %s", got, tip)
	}
	deploy := refIn(f.copy, "refs/heads/deploy")
	if log := commitsIn(t, f.copy, f.base, deploy, "%s"); slices.Contains(log, "pushed during the run") || !slices.Contains(log, "p2") {
		t.Errorf("base..deploy = %q, want p2's commit and not the one pushed during the run", log)
	}
	if got := gitrepotest.Git(t, f.copy, "show", deploy+":p2.txt"); got != "p2" {
		t.Errorf("p2.txt in deploy = %q, want p2's change", got)
	}
}

func TestRebuild_SkipsPatchesThatDoNotApplyToTheRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status string
		// gone deletes the patch's branch before the attempt.
		gone   bool
		reason string
	}{
		{"disabled", carrypatch.StatusDisabled, false, "disabled"},
		{"merged upstream", carrypatch.StatusMergedUpstream, false, "merged_upstream"},
		{"active without its branch", carrypatch.StatusActive, true, "branch_not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			f.addPatch(t, "p1", withStatus(tc.status))
			var source any = refIn(f.copy, "refs/heads/p1")
			if tc.gone {
				gitrepotest.Git(t, f.copy, "update-ref", "-d", "refs/heads/p1")
				source = nil
			}
			base := f.moveUpstream(t, "U2", map[string]string{"u2.txt": "u2\n"})

			out, id := f.attempt(t, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			results := f.patchResults(t, id)
			if len(results) != 1 {
				t.Fatalf("patch_results = %v, want one", results)
			}
			want := map[string]any{"status": "skipped", "skipped_reason": tc.reason, "source_sha": source,
				"new_head_sha": nil, "conflict_files": []any{}, "conflict": nil}
			for k, v := range want {
				if got := results[0][k]; !reflect.DeepEqual(got, v) {
					t.Errorf("%s of p1's result = %v, want %v", k, got, v)
				}
			}
			if got := refIn(f.copy, "refs/heads/deploy"); got != base {
				t.Errorf("deploy = %s, want the upstream base %s", got, base)
			}
		})
	}
}

func TestRebuild_TriesConflictPatchesAgain(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	f.addPatch(t, "p1", withStatus(carrypatch.StatusConflict, "f.txt"))

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	deploy := refIn(f.copy, "refs/heads/deploy")
	if r := f.patchResults(t, id)[0]; r["status"] != "success" || r["new_head_sha"] != deploy || deploy == f.base {
		t.Errorf("p1's result = %v, deploy = %s; want success with new_head_sha the published head", r, deploy)
	}
}

func TestRebuild_StoresProgressAfterEveryPatch(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	for _, b := range []string{"p1", "p2", "p3"} {
		f.addPatch(t, b)
	}
	var id string
	var seen []jobs.Job
	f.hooks.beforePatch(func(i int) {
		if i == 0 {
			return
		}
		j, err := f.queue.Get(context.Background(), id)
		if err != nil {
			t.Errorf("read the job before patch %d: %v", i, err)
			return
		}
		seen = append(seen, j)
	})

	out, _ := f.attempt(t, func(_ context.Context, j jobs.Job) { id = j.ID })
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	final := f.patchResults(t, id)
	if len(seen) != 2 {
		t.Fatalf("read the job %d times between patches, want 2", len(seen))
	}
	for n, j := range seen {
		var res struct {
			Applied      int              `json:"patches_applied"`
			Skipped      int              `json:"patches_skipped"`
			Conflicted   int              `json:"patches_conflicted"`
			Removed      int              `json:"patches_removed"`
			PatchResults []map[string]any `json:"patch_results"`
		}
		if err := json.Unmarshal(j.Result, &res); err != nil {
			t.Fatalf("the job's result before patch %d = %s: %v", n+2, j.Result, err)
		}
		if j.Status != jobs.StatusRunning {
			t.Errorf("the job before patch %d is %s, want running", n+2, j.Status)
		}
		if !reflect.DeepEqual(res.PatchResults, final[:n+1]) {
			t.Errorf("patch_results before patch %d = %v, want the first %d of %v", n+2, res.PatchResults, n+1, final)
		}
		if res.Applied+res.Skipped+res.Conflicted+res.Removed != 0 {
			t.Errorf("the counts before patch %d = %+v, want 0", n+2, res)
		}
	}
}
