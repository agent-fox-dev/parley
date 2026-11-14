package workspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// safetyCopy runs the safety copy of the workspace slug as the store holds
// it and returns its outcome.
func (w *world) safetyCopy(t *testing.T, slug string) string {
	t.Helper()
	rec, found := w.workspace(t, slug)
	if !found {
		t.Fatalf("no workspace %s", slug)
	}
	return w.svc.SafetyCopy(context.Background(), rec)
}

func TestSafetyCopy_OffersEveryBranchAsAFastForward(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	o := s.origin
	mainHead, devHead := o.Head("main"), o.Head("dev")
	o.Branch("stale", "dev")
	o.Branch("old", "main")
	s.setOriginHead(t, "stale", devHead)
	ahead := grow(t, s.copy, "main", grow(t, s.copy, "main", mainHead, "hub"), "hub")
	feat := grow(t, s.copy, "feat", mainHead, "hub")
	grow(t, s.copy, "stale", mainHead, "hub")
	gitrepotest.Git(t, s.copy, "update-ref", "refs/tags/v-hub", ahead)
	// Origin takes only the stored token, presented as x-token-auth.
	s.storeSecrets(t, map[string]string{workspace.SecretGitPAT: "s3cr3t-token"})
	o.Accept("x-token-auth", "s3cr3t-token")
	before := gitrepotest.Refs(t, o.Dir(), "refs/tags/")

	if got := s.safetyCopy(t, syncSlug); got != "partial" {
		t.Errorf("outcome %q, want partial", got)
	}
	for branch, want := range map[string]string{"main": ahead, "feat": feat, "stale": devHead, "old": mainHead, "dev": devHead} {
		if head := o.Head(branch); head != want {
			t.Errorf("origin's %s is %q, want %s", branch, head, want)
		}
	}
	if tags := gitrepotest.Refs(t, o.Dir(), "refs/tags/"); len(tags) != len(before) || tags["v-hub"] != "" {
		t.Errorf("origin's tags are %v, want %v", tags, before)
	}
	for branch, want := range map[string]string{"main": ahead, "feat": feat, "stale": devHead} {
		if got := ref(t, s.copy, "refs/remotes/origin/"+branch); got != want {
			t.Errorf("the origin head of %s is %q, want %s", branch, got, want)
		}
	}
}

func TestSafetyCopy_LeavesTheIntegrationBranchOut(t *testing.T) {
	t.Parallel()
	s := newSyncWorld(t)
	mainHead := s.origin.Head("main")
	s.origin.Branch("deploy", "main")
	s.setMode(t, workspace.ModeCarryPatch, "deploy")
	grow(t, s.copy, "deploy", mainHead, "hub")
	patch := grow(t, s.copy, "patch/a", mainHead, "hub")

	if got := s.safetyCopy(t, syncSlug); got != "ok" {
		t.Errorf("outcome %q, want ok", got)
	}
	if head := s.origin.Head("deploy"); head != mainHead {
		t.Errorf("origin's deploy is %s, want it unchanged at %s", head, mainHead)
	}
	if head := s.origin.Head("patch/a"); head != patch {
		t.Errorf("origin's patch/a is %q, want %s", head, patch)
	}
}

// stubBranchesPusher is the hub's safety-copy pusher: it records the heads
// of every push and the error of its context once the push returns, and
// answers what answer, given the push, returns, without running git.
type stubBranchesPusher struct {
	mu     sync.Mutex
	heads  []map[string]string
	ctxErr []error
	answer func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error)
}

func (p *stubBranchesPusher) PushBranches(ctx context.Context, req gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
	p.mu.Lock()
	answer := p.answer
	p.mu.Unlock()
	got, err := answer(ctx, req)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.heads, p.ctxErr = append(p.heads, req.Heads), append(p.ctxErr, ctx.Err())
	return got, err
}

// calls are the pushes so far: their heads and the error of their context.
func (p *stubBranchesPusher) calls() ([]map[string]string, []error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.heads, p.ctxErr
}

func TestSafetyCopy_ReducesTheBranchResultsToOneOutcome(t *testing.T) {
	t.Parallel()
	p := &stubBranchesPusher{}
	s := newSyncWorld(t, withPushDeadline(200*time.Millisecond), withCompose(func(c *hub.Composition) {
		c.AdjustWorkspace(func(d *workspace.Deps) { d.SafetyPusher = p })
	}))
	url := s.origin.URL()
	s.put(t, workspace.Workspace{Slug: "broken", Status: workspace.StatusFailed, GitURL: &url})
	s.put(t, workspace.Workspace{Slug: "empty", Status: workspace.StatusReady, GitURL: &url})
	gitrepotest.Git(t, s.svc.RepoPath("empty"), "init", "--quiet", "--bare", s.svc.RepoPath("empty"))

	pushed := gitrepo.BranchPush{Result: gitrepo.PushPushed}
	upToDate := gitrepo.BranchPush{Result: gitrepo.PushUpToDate}
	rejected := gitrepo.BranchPush{Err: gitrepo.ErrNonFastForward}
	// answer gives main the first result and dev the second.
	answer := func(main, dev gitrepo.BranchPush) func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		return func(_ context.Context, req gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
			main.Branch, main.Commit, dev.Branch, dev.Commit = "main", req.Heads["main"], "dev", req.Heads["dev"]
			return []gitrepo.BranchPush{main, dev}, nil
		}
	}
	failing := func(err error) func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		return func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) { return nil, err }
	}
	stalled := func(accepted []string) func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		return func(ctx context.Context, req gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
			<-ctx.Done()
			var got []gitrepo.BranchPush
			for _, b := range accepted {
				got = append(got, gitrepo.BranchPush{Branch: b, Commit: req.Heads[b], Result: gitrepo.PushPushed})
			}
			return got, ctx.Err()
		}
	}
	for _, c := range []struct {
		name   string
		slug   string
		fault  string
		answer func(context.Context, gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error)
		want   string
		// pushed tells whether the pusher is called; cancelled whether its
		// context has ended when it returns.
		pushed, cancelled bool
	}{
		{name: "all pushed", answer: answer(pushed, pushed), want: "ok", pushed: true},
		{name: "all already up to date", answer: answer(upToDate, upToDate), want: "ok", pushed: true},
		{name: "pushed and up to date", answer: answer(pushed, upToDate), want: "ok", pushed: true},
		{name: "no branch to offer", slug: "empty", answer: answer(pushed, pushed), want: "ok"},
		{name: "failed workspace without a copy", slug: "broken", answer: answer(pushed, pushed), want: "ok"},
		{name: "pushed and refused", answer: answer(pushed, rejected), want: "partial", pushed: true},
		{name: "up to date and refused", answer: answer(upToDate, rejected), want: "partial", pushed: true},
		{name: "all refused", answer: answer(rejected, rejected), want: "failed", pushed: true},
		{name: "connection failure without a branch result", want: "failed", pushed: true,
			answer: failing(&gitrepo.PushError{Class: gitrepo.ErrUnreachable, Reason: "Connection refused"})},
		{name: "credential cannot be read", fault: faultSecretRead, answer: answer(pushed, pushed), want: "failed"},
		{name: "origin refuses the credential", want: "failed", pushed: true,
			answer: failing(&gitrepo.PushError{Class: gitrepo.ErrUnauthenticated, Reason: "Authentication failed"})},
		{name: "origin cannot be dialled", want: "failed", pushed: true,
			answer: failing(errors.New("dial tcp 127.0.0.1:1: connect: connection refused"))},
		{name: "deadline passes", answer: stalled(nil), want: "failed", pushed: true, cancelled: true},
		{name: "deadline passes after a branch was taken", answer: stalled([]string{"main"}), want: "partial", pushed: true, cancelled: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p.mu.Lock()
			p.heads, p.ctxErr, p.answer = nil, nil, c.answer
			p.mu.Unlock()
			s.fail(c.fault)
			defer s.fail("")
			slug := c.slug
			if slug == "" {
				slug = syncSlug
			}
			if got := s.safetyCopy(t, slug); got != c.want {
				t.Errorf("outcome %q, want %q", got, c.want)
			}
			heads, ctxErrs := p.calls()
			if c.pushed != (len(heads) == 1) || len(heads) > 1 {
				t.Fatalf("%d pushes, want pushed = %v", len(heads), c.pushed)
			}
			if c.pushed && (ctxErrs[0] != nil) != c.cancelled {
				t.Errorf("the push's context ended with %v, want ended = %v", ctxErrs[0], c.cancelled)
			}
		})
	}
}

func TestSafetyCopy_KeepsTheCredentialAndOriginsWordsOutOfTheArchive(t *testing.T) {
	t.Parallel()
	const said = "remote: denied for https://x-token-auth:s3cr3t-token@origin.test/r.git"
	var log hubtest.LogBuffer
	p := &stubBranchesPusher{answer: func(_ context.Context, req gitrepo.PushBranchesRequest) ([]gitrepo.BranchPush, error) {
		refused := &gitrepo.PushError{Branch: "main", Class: gitrepo.ErrOriginRejected, Reason: said}
		return []gitrepo.BranchPush{{Branch: "main", Commit: req.Heads["main"], Err: refused}}, errors.New(said)
	}}
	s := newSyncWorld(t, withLog(&log), withSafetyPusher(p))
	s.storeSecrets(t, map[string]string{workspace.SecretGitPAT: "s3cr3t-token"})

	a := s.archive(t, s.uKey, syncSlug)

	if a.status != http.StatusOK {
		t.Fatalf("archive = %d %s, want 200", a.status, a.body)
	}
	evs := s.eventsOf(t, archiveEvent)
	if len(evs) != 1 {
		t.Fatalf("%d archive events, want 1", len(evs))
	}
	ev, err := json.Marshal(evs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(evs[0].Metadata), `"origin_push":"failed"`) {
		t.Errorf("the archive event's metadata is %s, want origin_push failed", evs[0].Metadata)
	}
	if !strings.Contains(log.String(), "workspace safety copy failed") {
		t.Errorf("the log does not report the failed safety copy:\n%s", log.String())
	}
	for what, text := range map[string]string{"the answer": string(a.body), "the event": string(ev), "the log": log.String()} {
		for _, secret := range []string{"s3cr3t-token", "remote: denied"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s holds %q:\n%s", what, secret, text)
			}
		}
	}
}
