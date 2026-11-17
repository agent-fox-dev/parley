package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// hangingRemote is an https server that takes every request and never
// answers it: it holds each one until its client goes away, or until the
// test ends, and records when each arrived and when its client left.
type hangingRemote struct {
	srv  *httptest.Server
	cert []byte
	stop chan struct{}

	mu   sync.Mutex
	held []heldRequest
}

// heldRequest is a request the hanging remote held: when it arrived, and
// when its client went away, zero while it has not.
type heldRequest struct {
	arrived, left time.Time
}

// startHangingRemote starts a hanging remote, closed when the test ends.
func startHangingRemote(t *testing.T) *hangingRemote {
	t.Helper()
	h := &hangingRemote{stop: make(chan struct{})}
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		i := len(h.held)
		h.held = append(h.held, heldRequest{arrived: time.Now()})
		h.mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-h.stop:
		}
		h.mu.Lock()
		h.held[i].left = time.Now()
		h.mu.Unlock()
	}))
	t.Cleanup(func() {
		close(h.stop)
		h.srv.Close()
	})
	h.cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: h.srv.Certificate().Raw})
	return h
}

// URL is the URL of a repository on the hanging remote.
func (h *hangingRemote) URL() string { return h.srv.URL + "/upstream.git" }

// trustedBy is env, the environment of a git, with GIT_SSL_CAINFO naming
// a bundle of the certificates the bundle it named holds and the hanging
// remote's own. It fails no test with Fatal, so that a start hook may call
// it.
func (h *hangingRemote) trustedBy(t *testing.T, env []string) []string {
	return trusting(t, env, h.cert)
}

// trusting is env, the environment of a git, with GIT_SSL_CAINFO naming a
// bundle of the certificates the bundle it named holds and the PEM
// certificate cert. It fails no test with Fatal, so that a start hook may
// call it.
func trusting(t *testing.T, env []string, cert []byte) []string {
	var pems []byte
	for _, kv := range env {
		if file, ok := strings.CutPrefix(kv, "GIT_SSL_CAINFO="); ok {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Errorf("read the certificates git trusts: %v", err)
			}
			pems = b
		}
	}
	bundle := filepath.Join(t.TempDir(), "ca-bundle.pem")
	if err := os.WriteFile(bundle, append(slices.Clone(pems), cert...), 0o600); err != nil {
		t.Errorf("write the certificates git trusts: %v", err)
	}
	return append(slices.Clone(env), "GIT_SSL_CAINFO="+bundle)
}

// heldCount is the number of requests the remote has held.
func (h *hangingRemote) heldCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.held)
}

// checkLeftWithin checks that the remote held at least n requests and that
// the client of each went away within d of its arrival, waiting up to d
// for one that has not yet.
func (h *hangingRemote) checkLeftWithin(t *testing.T, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		h.mu.Lock()
		held := slices.Clone(h.held)
		h.mu.Unlock()
		waiting := slices.ContainsFunc(held, func(r heldRequest) bool { return r.left.IsZero() })
		if len(held) >= n && !waiting || time.Now().After(deadline) {
			if len(held) < n {
				t.Fatalf("the hanging remote held %d requests, want at least %d", len(held), n)
			}
			for i, r := range held {
				if r.left.IsZero() || r.left.Sub(r.arrived) > d {
					t.Errorf("the client of request %d left after %v, want within %v", i, r.left.Sub(r.arrived), d)
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// upstreamFetchProbe is the pattern of the test route that
// addUpstreamFetchProbe mounts, below /api/v1.
const upstreamFetchProbe = "/workspaces/:slug/upstream-fetch-probe"

// addUpstreamFetchProbe mounts on c the test route POST upstreamFetchProbe,
// a carry-patch write route that runs one fetch of the workspace's upstream
// into its copy, presenting the upstream credential, through the
// carry-patch git runner with limit, as the sync and preview handlers run
// their git, and answers a failure through the carry-patch error mapper;
// it answers 204 when the fetch succeeds.
func addUpstreamFetchProbe(c *hub.Composition, limit time.Duration) {
	var d *carrypatch.Deps
	c.AdjustCarryPatch(func(deps *carrypatch.Deps) { d = deps })
	comp := c.CarryPatch()
	c.AddRoute(carrypatch.Route(carrypatch.WriteRoute, server.Route{Method: http.MethodPost, Pattern: upstreamFetchProbe,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			ws, err := comp.Guard(r)
			if err != nil {
				return err
			}
			cred, err := carrypatch.ResolveUpstreamCredential(r.Context(), d.Secrets, ws.Slug)
			if err != nil {
				return err
			}
			err = carrypatch.RunGit(r.Context(), limit, cred, func(ctx context.Context, deadline time.Duration) error {
				_, err := d.Git.FetchRemote(ctx, d.RepoPath(ws.Slug), str(ws.UpstreamURL), carrypatch.UpstreamRemote, cred, deadline)
				return err
			})
			if err != nil {
				return carrypatch.RequestError(err)
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		}}))
}

// startFetchProbeHub starts a hub with an owner who holds an API key and
// whose organization owns the workspaces add stores, which serves the
// upstream fetch probe with limit, with the carry-patch dependencies
// changed by adjust.
func startFetchProbeHub(t *testing.T, limit time.Duration, adjust func(d *carrypatch.Deps)) *readHub {
	t.Helper()
	h := &readHub{}
	h.Hub = hubtest.Start(t, func(c *hub.Composition) {
		c.AdjustCarryPatch(func(d *carrypatch.Deps) {
			h.repoPath = d.RepoPath
			adjust(d)
		})
		addUpstreamFetchProbe(c, limit)
	})
	h.owner = h.CreateUser(t, "owner")
	h.key = h.IssueKey(t, h.owner)
	h.org = h.CreateOrg(t, "acme")
	h.AddMember(t, h.org, h.owner)
	return h
}

func TestRequest_FailsAsInternalWhenAGitOperationOutlastsTheLimit(t *testing.T) {
	t.Parallel()
	const limit = 300 * time.Millisecond
	up := startHangingRemote(t)
	h := startFetchProbeHub(t, limit, func(d *carrypatch.Deps) { d.Git.Env = up.trustedBy(t, d.Git.Env) })
	h.add(t, "demo", workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, func(w *workspace.Workspace) {
		w.IntegrationBranch, w.UpstreamURL = new("deploy"), new(up.URL())
	})
	cp := h.repoPath("demo")
	gitrepotest.InitBare(t, cp, "main")
	head := gitrepotest.Commit(t, cp, "main", map[string]string{"a.txt": "a\n"})
	gitrepotest.SetRef(t, cp, "refs/heads/main", head)
	gitrepotest.SetRef(t, cp, "refs/heads/deploy", head)
	refs := gitrepotest.Refs(t, cp, "")

	// Under the load of the whole suite git may not reach the remote before
	// the limit ends it; such a call is checked too, and made again, so
	// that one call shows the connection it held closed.
	for try := 1; ; try++ {
		start := time.Now()
		status, body := h.Call(t, http.MethodPost, "/api/v1/workspaces/demo/upstream-fetch-probe", h.key, nil)
		took := time.Since(start)

		var a answer
		if err := json.Unmarshal(body, &a); err != nil {
			t.Fatalf("the answer %d %s: %v", status, body, err)
		}
		if status != http.StatusInternalServerError || a.Error.ErrorType != "internal" || !strings.Contains(a.Error.Message, "time limit") {
			t.Errorf("the answer = %d %s, want 500 internal saying the time limit passed", status, body)
		}
		if took < limit || took > 3*time.Second {
			t.Errorf("the answer came after %v, want between %v and 3s", took, limit)
		}
		if got := gitrepotest.Refs(t, cp, ""); !maps.Equal(got, refs) {
			t.Errorf("the copy's refs = %v, want %v unchanged", got, refs)
		}
		if up.heldCount() > 0 || try == 3 {
			break
		}
	}
	up.checkLeftWithin(t, 1, limit+2*time.Second)
}

func TestRebuild_EndsAnAttemptWhoseGitOperationOutlastsTheLimitAsRetryable(t *testing.T) {
	t.Parallel()
	const limit = 300 * time.Millisecond
	up := startHangingRemote(t)
	f := startRebuildFixtureWith(t, func(d *carrypatch.Deps) {
		d.RebuildGitDeadline = limit
		d.Git.Env = up.trustedBy(t, d.Git.Env)
	})
	p := f.addPatch(t, "p1")
	// From now on the upstream takes the fetch's request and never answers.
	if _, err := f.Store.Main().Exec(`UPDATE workspaces SET upstream_url = ? WHERE slug = ?`, up.URL(), f.slug); err != nil {
		t.Fatal(err)
	}
	deploy := refIn(f.copy, "refs/heads/deploy")
	check := func(t *testing.T, id, status string, retryCount int, willRetry bool, attempts int) {
		t.Helper()
		j := f.job(t, id)
		if j.Status != status || j.RetryCount != retryCount {
			t.Errorf("the job has status %s and retry_count %d, want %s and %d", j.Status, j.RetryCount, status, retryCount)
		}
		if status == jobs.StatusFailed && j.NextAttemptAt == nil {
			t.Errorf("the failed job has no next_attempt_at")
		}
		if text := str(j.Error); !strings.Contains(text, carrypatch.ErrGitTimeout.Error()) || !strings.Contains(text, limit.String()) {
			t.Errorf("the job's error = %q, want it to say the time limit of %v passed", text, limit)
		}
		events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildFail)
		if len(events) != attempts || events[len(events)-1].Metadata["will_retry"] != willRetry {
			t.Errorf("parley.rebuild.fail events = %v, want %d, the last with will_retry %v", events, attempts, willRetry)
		}
		if got := refIn(f.copy, "refs/heads/deploy"); got != deploy {
			t.Errorf("deploy = %s, want it where it was, %s", got, deploy)
		}
		if got := f.stored(t, p.ID); got.Status != carrypatch.StatusActive {
			t.Errorf("the patch has status %s, want active", got.Status)
		}
		if results := f.patchResults(t, id); len(results) != 0 {
			t.Errorf("patch_results = %v, want none", results)
		}
		up.checkLeftWithin(t, attempts, limit+2*time.Second)
	}

	_, id := f.attempt(t, nil)
	check(t, id, jobs.StatusFailed, 1, true, 1)

	j := f.job(t, id)
	f.advance(j.NextAttemptAt.Sub(f.clock()))
	f.rerun(t, id, func(ctx context.Context, j jobs.Job) {
		if err := f.Store.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE jobs SET retry_count = ? WHERE id = ?`, jobs.MaxRetries, j.ID)
			return err
		}); err != nil {
			t.Errorf("give the job %s its last retry: %v", j.ID, err)
		}
	}, nil)
	check(t, id, jobs.StatusDeadLetter, jobs.MaxRetries, false, 2)
}

func TestRebuild_HoldsEachGitOperationToTheLimitAndNotTheirSum(t *testing.T) {
	t.Parallel()
	// The first delayed operations sum past the limit, and each keeps more
	// than a second of it for git itself, which a loaded machine needs for
	// a replay of several git processes.
	const limit, delay, delayed = 3 * time.Second, 1200 * time.Millisecond, 3
	var ops atomic.Int64
	f := startRebuildFixtureWith(t, func(d *carrypatch.Deps) {
		d.RebuildGitDeadline = limit
		d.BeforeRebuildGit = func(ctx context.Context) {
			if ops.Add(1) > delayed {
				return
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
			}
		}
	})
	for _, b := range []string{"p1", "p2", "p3"} {
		f.addPatch(t, b)
	}

	start := time.Now()
	out, id := f.attempt(t, nil)
	took := time.Since(start)

	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	if j := f.job(t, id); j.Status != jobs.StatusCompleted {
		t.Errorf("the job has status %s, want completed", j.Status)
	}
	if n := ops.Load(); n < delayed || took <= limit {
		t.Errorf("the attempt ran %d git operations, the first %d delayed by %v each, in %v; want them to sum to more than %v", n, delayed, delay, took, limit)
	}
	if res := f.result(t, id); res["patches_applied"] != float64(3) {
		t.Errorf("patches_applied = %v, want 3", res["patches_applied"])
	}
}
