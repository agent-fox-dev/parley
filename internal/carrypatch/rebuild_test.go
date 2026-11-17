package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The kinds of an attempt's outcome, as carrypatch.RunRebuildAttempt names
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
// at the upstream base. The hub's job queue passes only when the test runs
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
}

// startRebuildFixture starts the rebuild fixture, with the workspace's
// record changed by edit before it is stored.
func startRebuildFixture(t *testing.T, edit ...func(w *workspace.Workspace)) *rebuildFixture {
	t.Helper()
	up := gitrepotest.NewOrigin(t)
	up.Commit("main", "U1")
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
		Now: f.clock,
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
			})
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
// whose branch in the hub's copy holds one commit on the upstream base that
// adds a file of its own, and returns it.
func (f *rebuildFixture) addPatch(t *testing.T, branch string) carrypatch.Patch {
	t.Helper()
	live, err := carrypatch.ListLivePatches(context.Background(), f.Store.Main(), f.slug)
	if err != nil {
		t.Fatal(err)
	}
	file := strings.ReplaceAll(branch, "/", "-") + ".txt"
	tip := commitFiles(t, f.copy, f.base, branch, map[string]string{file: branch + "\n"})
	gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/"+branch, tip)
	p := livePatch(f.slug, branch, len(live)+1)
	f.put(t, p)
	return p
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
	ctx := context.Background()
	res, err := f.comp.EnqueueRebuild(ctx, f.slug, carrypatch.RebuildOptions{ActorType: event.ActorSystem, SubmittedBy: event.ActorSystem})
	if err != nil || !res.Queued {
		t.Fatalf("queue a rebuild of %s = %+v, %v, want a queued job", f.slug, res, err)
	}
	run := f.runner.expect(res.JobID, before)
	if err := f.queue.Pass(ctx); err != nil {
		t.Fatalf("pass the job queue: %v", err)
	}
	// An attempt runs git against the test origins, which may take longer
	// than a step of the queue under the load of the whole suite.
	waitAttempt(t, run.ran, "the attempt of the job "+res.JobID+" to end")
	if run.out.Kind != attemptEnded {
		waitAttempt(t, run.settled, "the outcome of the job "+res.JobID+" to be settled")
		// The store runs its write transactions one at a time, in order: an
		// empty one asked for once the settlement hook ran ends after the
		// settlement committed.
		if err := f.Store.Tx(ctx, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	return run.out, res.JobID
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

// commitFiles writes into the repository dir a commit with the message msg
// on parent ("" for a root commit) whose tree is parent's with the files,
// at the top level, holding the content given, and returns its id.
func commitFiles(t *testing.T, dir, parent, msg string, files map[string]string) string {
	t.Helper()
	entries := map[string]string{}
	if parent != "" {
		for line := range strings.Lines(gitrepotest.Git(t, dir, "ls-tree", parent)) {
			meta, name, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
			entries[name] = meta
		}
	}
	for name, content := range files {
		blob, err := gitrepotest.Run(dir, content, "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		entries[name] = "100644 blob " + blob
	}
	var tree strings.Builder
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		fmt.Fprintf(&tree, "%s\t%s\n", entries[name], name)
	}
	id, err := gitrepotest.Run(dir, tree.String(), "mktree")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"commit-tree", id, "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return gitrepotest.Git(t, dir, args...)
}

// rebuildSecrets are the workspace secrets of the rebuild fixture, which the
// test sets; every other secret is read from the hub's reader.
type rebuildSecrets struct {
	workspace.SecretReader
	mu     sync.Mutex
	values map[string]string
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
	v, ok := s.values[name]
	return v, ok, nil
}

// attemptRunner is the handler of the fixture's rebuild jobs: an attempt of
// a job a test expects runs the hub's rebuild attempt and returns its
// outcome to the queue; an attempt of any other job completes at once.
type attemptRunner struct {
	comp *carrypatch.Component
	mu   sync.Mutex
	runs map[string]*attemptRun
}

// attemptRun is the attempt of a job a test expects.
type attemptRun struct {
	// before, when set, is called at the start of the attempt.
	before func(ctx context.Context, j jobs.Job)
	// out is the attempt's outcome once ran is closed.
	out carrypatch.AttemptOutcome
	// ran is closed once the attempt has its outcome, settled once that is
	// being settled.
	ran, settled chan struct{}
	settledOnce  sync.Once
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
	if run.before != nil {
		run.before(ctx, j)
	}
	run.out = carrypatch.RunRebuildAttempt(ctx, r.comp, j)
	close(run.ran)
	switch run.out.Kind {
	case attemptRetryable:
		return errors.New(run.out.Text)
	case attemptPermanent:
		return jobs.Permanent(errors.New(run.out.Text))
	}
	return nil
}

func (r *attemptRunner) Settled(_ context.Context, _ *sql.Tx, j jobs.Job, _ bool) error {
	if run := r.run(j.ID); run != nil {
		run.settledOnce.Do(func() { close(run.settled) })
	}
	return nil
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
	cases := []struct {
		name   string
		breaks func(t *testing.T, f *rebuildFixture)
		reason string
	}{
		{"the upstream repository is gone", func(t *testing.T, f *rebuildFixture) {
			if err := os.RemoveAll(f.upstream.Dir()); err != nil {
				t.Fatal(err)
			}
		}, "upstream_fetch_failed"},
		{"no upstream branch to start from", func(t *testing.T, f *rebuildFixture) {
			gitrepotest.Git(t, f.upstream.Dir(), "update-ref", "-d", "refs/heads/main")
			f.upstream.Commit("trunk", "T1")
			f.upstream.SetDefaultBranch("gone")
		}, "upstream_unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			f.addPatch(t, "patch/1")
			f.addPatch(t, "patch/2")
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
				t.Errorf("deploy = %s, want it where it was, %s", got, deploy)
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
