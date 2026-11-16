package carrypatch_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
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
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// originToken is the GIT_PAT of the workspaces of the resolution tests,
// the token their origins take.
const originToken = "origin-token-7d3a"

// resolveFixture is a carry-patch workspace whose copy is a bare repository
// made with git init, and a component bound to it whose git counts its
// invocations and records their arguments.
type resolveFixture struct {
	comp *carrypatch.Component
	ws   workspace.Workspace
	copy string
	deps carrypatch.Deps

	runs atomic.Int32
	mu   sync.Mutex
	args [][]string
}

// newResolveFixture binds a component to the workspace acme, whose git_url
// is url, with env added to git's environment, GIT_PAT originToken and
// patch_branch_source source stored in its settings ("" stores none).
func newResolveFixture(t *testing.T, url string, env []string, source string) *resolveFixture {
	t.Helper()
	f := &resolveFixture{copy: filepath.Join(t.TempDir(), "acme.git")}
	gitrepotest.Git(t, f.copy, "init", "--quiet", "--bare", f.copy)
	branch := "main"
	f.ws = workspace.Workspace{Slug: "acme", Mode: workspace.ModeCarryPatch, Status: workspace.StatusReady,
		GitURL: &url, DefaultBranch: &branch, Settings: map[string]json.RawMessage{}}
	if source != "" {
		f.ws.Settings["patch_branch_source"] = json.RawMessage(`"` + source + `"`)
	}
	settings := &workspace.SettingsRegistry{}
	if err := workspace.RegisterSettingKeys(settings); err != nil {
		t.Fatal(err)
	}
	f.deps = carrypatch.Deps{
		Secrets: secretMap{slug: "acme", values: map[string]string{"GIT_PAT": originToken}},
		Git: gitrepo.Git{Env: env, Trace: func(args []string) {
			f.runs.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			f.args = append(f.args, args)
		}},
		RepoPath: func(string) string { return f.copy },
		Settings: settings,
	}
	f.comp = &carrypatch.Component{}
	f.comp.Bind(f.deps)
	return f
}

// rebind binds the component again with its dependencies changed by adjust.
func (f *resolveFixture) rebind(adjust func(d *carrypatch.Deps)) {
	adjust(&f.deps)
	f.comp.Bind(f.deps)
}

// setRef points ref of the copy at id.
func (f *resolveFixture) setRef(t *testing.T, ref, id string) {
	t.Helper()
	gitrepotest.Git(t, f.copy, "update-ref", ref, id)
}

// commit writes a commit with the message msg into the copy, on no ref.
func (f *resolveFixture) commit(t *testing.T, msg string) string {
	t.Helper()
	return commitIn(t, f.copy, msg)
}

// tokenOrigin is an origin that takes the GIT_PAT originToken alone.
func tokenOrigin(t *testing.T) *gitrepotest.Origin {
	return gitrepotest.NewOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", originToken))
}

// assertServerError fails the test unless err is the error of the REST API
// with status and errorType.
func assertServerError(t *testing.T, err error, status int, errorType string) *server.Error {
	t.Helper()
	var e *server.Error
	if !errors.As(err, &e) || e.Status() != status || e.Type() != errorType {
		t.Fatalf("err = %v, want %d %s", err, status, errorType)
	}
	return e
}

func TestResolveBranch_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		source string
		skip   bool
		// prepare writes the case's refs into the copy and the origin, and
		// returns the refs the copy holds afterwards, those of other
		// branches included.
		prepare        func(t *testing.T, f *resolveFixture, o *gitrepotest.Origin) map[string]string
		branch         string
		want           string
		originRequests bool
	}{
		{"a branch of the copy is taken as it is", "origin", false, func(t *testing.T, f *resolveFixture, o *gitrepotest.Origin) map[string]string {
			a, b := f.commit(t, "a"), f.commit(t, "b")
			f.setRef(t, "refs/heads/p1", a)
			f.setRef(t, "refs/remotes/origin/p1", b)
			o.Commit("p1", "on origin")
			return map[string]string{"refs/heads/p1": a, "refs/remotes/origin/p1": b}
		}, "p1", carrypatch.ResolutionLocal, false},
		{"an origin-tracking branch becomes a branch of the copy", "hub", false, func(t *testing.T, f *resolveFixture, _ *gitrepotest.Origin) map[string]string {
			b := f.commit(t, "b")
			f.setRef(t, "refs/remotes/origin/p2", b)
			return map[string]string{"refs/heads/p2": b, "refs/remotes/origin/p2": b}
		}, "p2", carrypatch.ResolutionOriginTracking, false},
		{"a branch only origin has is fetched with patch_branch_source origin", "origin", false, func(t *testing.T, f *resolveFixture, o *gitrepotest.Origin) map[string]string {
			c := o.Commit("p3", "c")
			o.Commit("other", "moved on")
			stale := f.commit(t, "stale")
			f.setRef(t, "refs/remotes/origin/other", stale)
			return map[string]string{"refs/heads/p3": c, "refs/remotes/origin/p3": c, "refs/remotes/origin/other": stale}
		}, "p3", carrypatch.ResolutionOriginFetch, true},
		{"a skipped check looks nothing up", "origin", true, func(_ *testing.T, _ *resolveFixture, o *gitrepotest.Origin) map[string]string {
			o.Commit("p4", "d")
			return map[string]string{}
		}, "p4", carrypatch.ResolutionSkipped, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := tokenOrigin(t)
			f := newResolveFixture(t, o.URL(), o.Env(), c.source)
			want := c.prepare(t, f, o)
			runs, requests := f.runs.Load(), o.Requests()

			got, err := f.comp.ResolveBranch(context.Background(), f.ws, c.branch, c.skip)
			if err != nil || got != c.want {
				t.Fatalf("ResolveBranch(%s) = %q, %v; want %q", c.branch, got, err, c.want)
			}
			if refs := gitrepotest.Refs(t, f.copy, ""); !maps.Equal(refs, want) {
				t.Errorf("the copy's refs = %v, want %v", refs, want)
			}
			if n := o.Requests() - requests; (n > 0) != c.originRequests {
				t.Errorf("origin received %d requests, want any: %v", n, c.originRequests)
			}
			if c.skip && f.runs.Load() != runs {
				t.Errorf("git ran %d times for a skipped check, want never", f.runs.Load()-runs)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, args := range f.args {
				if i := slices.Index(args, "--"); slices.Contains(args, c.branch) || (i < 0 && slices.Contains(args, o.URL())) {
					t.Errorf("git ran with the branch name bare or the URL before the end of its options: %q", args)
				}
			}
		})
	}
}

func TestResolveBranch_NotFound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		source         string
		originRequests bool
	}{
		{"patch_branch_source hub never asks origin", "hub", false},
		{"no setting reads as hub", "", false},
		{"patch_branch_source origin and origin has no such branch", "origin", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := tokenOrigin(t)
			o.Commit("main", "one")
			if !c.originRequests {
				o.Commit("p5", "only on origin")
			}
			f := newResolveFixture(t, o.URL(), o.Env(), c.source)
			other := f.commit(t, "other")
			f.setRef(t, "refs/heads/main", other)
			before := gitrepotest.Refs(t, f.copy, "")

			_, err := f.comp.ResolveBranch(context.Background(), f.ws, "p5", false)
			assertServerError(t, err, http.StatusNotFound, "branch_not_found")
			if refs := gitrepotest.Refs(t, f.copy, ""); !maps.Equal(refs, before) {
				t.Errorf("the copy's refs = %v, want them as before %v", refs, before)
			}
			if n := o.Requests(); (n > 0) != c.originRequests {
				t.Errorf("origin received %d requests, want any: %v", n, c.originRequests)
			}
		})
	}
}

func TestResolveBranch_OriginFetchFailed(t *testing.T) {
	t.Parallel()
	if carrypatch.DefaultBranchFetchDeadline != 2*time.Minute {
		t.Errorf("DefaultBranchFetchDeadline = %v, want 2 minutes", carrypatch.DefaultBranchFetchDeadline)
	}
	// The deadline leaves git time to start and reach origin also on a
	// loaded machine.
	const deadline = time.Second
	cases := []struct {
		name string
		// origin is the URL of the workspace's origin and the variables
		// git trusts it with; open, when not nil, counts the requests
		// origin still holds.
		origin func(t *testing.T) (url string, env []string, open *atomic.Int32)
		adjust func(d *carrypatch.Deps)
		// why is a part of the message that names the cause, "" for git's
		// own words.
		why string
	}{
		{"origin is stopped", func(t *testing.T) (string, []string, *atomic.Int32) {
			o := tokenOrigin(t)
			o.Close()
			return o.URL(), o.Env(), nil
		}, nil, ""},
		{"origin with a password in its URL is stopped", func(t *testing.T) (string, []string, *atomic.Int32) {
			o := tokenOrigin(t)
			o.Close()
			return strings.Replace(o.URL(), "https://", "https://someone:url-secret@", 1), o.Env(), nil
		}, nil, ""},
		{"origin answers 401 to GIT_PAT", func(t *testing.T) (string, []string, *atomic.Int32) {
			o := gitrepotest.NewOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", "another-token"))
			o.Commit("p6", "on origin")
			return o.URL(), o.Env(), nil
		}, nil, ""},
		{"the secret reader fails", func(t *testing.T) (string, []string, *atomic.Int32) {
			o := tokenOrigin(t)
			o.Commit("p6", "on origin")
			return o.URL(), o.Env(), nil
		}, func(d *carrypatch.Deps) {
			d.Secrets = secretMap{slug: "acme", fail: map[string]bool{"GIT_PAT": true}, err: errors.New("the secret store cannot be read")}
		}, "the secret store cannot be read"},
		{"origin never answers", func(t *testing.T) (string, []string, *atomic.Int32) {
			// Takes each request and never answers it: a request ends only
			// when the git process holding its connection is gone.
			var open atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				open.Add(1)
				defer open.Add(-1)
				<-r.Context().Done()
			}))
			t.Cleanup(srv.Close)
			ca := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}
			return srv.URL + "/origin.git", []string{"GIT_SSL_CAINFO=" + ca}, &open
		}, func(d *carrypatch.Deps) { d.BranchFetchDeadline = deadline }, "deadline"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			url, env, open := c.origin(t)
			f := newResolveFixture(t, url, env, "origin")
			if c.adjust != nil {
				f.rebind(c.adjust)
			}

			start := time.Now()
			_, err := f.comp.ResolveBranch(context.Background(), f.ws, "p6", false)
			e := assertServerError(t, err, http.StatusBadGateway, "origin_fetch_failed")
			if d := time.Since(start); d > deadline+2*time.Second {
				t.Errorf("ResolveBranch returned after %v, want within 2s of the deadline %v", d, deadline)
			}
			if !strings.Contains(e.Message(), c.why) {
				t.Errorf("the message %q does not name the cause %q", e.Message(), c.why)
			}
			if strings.Contains(e.Message(), originToken) || strings.Contains(e.Message(), "another-token") || strings.Contains(e.Message(), "url-secret") {
				t.Errorf("the message shows a credential: %s", e.Message())
			}
			if refs := gitrepotest.Refs(t, f.copy, ""); len(refs) != 0 {
				t.Errorf("the copy's refs = %v, want none", refs)
			}
			for wait := time.Now(); open != nil && open.Load() != 0; time.Sleep(10 * time.Millisecond) {
				if time.Since(wait) > 2*time.Second {
					t.Fatal("a request to origin is still open 2s after the return: a git process is left")
				}
			}
		})
	}
}

func TestResolveBranch_LosesCreateRace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		source string
		// prepare writes the branch p7 where the case finds it.
		prepare func(t *testing.T, f *resolveFixture, o *gitrepotest.Origin)
		want    string
	}{
		{"from the origin-tracking branch", "hub", func(t *testing.T, f *resolveFixture, _ *gitrepotest.Origin) {
			f.setRef(t, "refs/remotes/origin/p7", f.commit(t, "b"))
		}, carrypatch.ResolutionOriginTracking},
		{"from origin", "origin", func(_ *testing.T, _ *resolveFixture, o *gitrepotest.Origin) {
			o.Commit("p7", "c")
		}, carrypatch.ResolutionOriginFetch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := tokenOrigin(t)
			f := newResolveFixture(t, o.URL(), o.Env(), c.source)
			c.prepare(t, f, o)
			a := f.commit(t, "the other writer's")
			f.rebind(func(d *carrypatch.Deps) {
				d.BeforeBranchCreate = func(branch string) { f.setRef(t, "refs/heads/"+branch, a) }
			})

			got, err := f.comp.ResolveBranch(context.Background(), f.ws, "p7", false)
			if err != nil || got != c.want {
				t.Fatalf("ResolveBranch = %q, %v; want %q", got, err, c.want)
			}
			if id := refIn(f.copy, "refs/heads/p7"); id != a {
				t.Errorf("refs/heads/p7 = %s, want the other writer's %s", id, a)
			}
		})
	}
}

func TestResolveBranch_RefusesANameAnotherBranchIsInTheWayOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		source  string
		prepare func(t *testing.T, f *resolveFixture, o *gitrepotest.Origin)
	}{
		{"from the origin-tracking branch", "hub", func(t *testing.T, f *resolveFixture, _ *gitrepotest.Origin) {
			f.setRef(t, "refs/remotes/origin/feat/x", f.commit(t, "b"))
		}},
		{"from origin", "origin", func(_ *testing.T, _ *resolveFixture, o *gitrepotest.Origin) {
			o.Commit("feat/x", "c")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := tokenOrigin(t)
			f := newResolveFixture(t, o.URL(), o.Env(), c.source)
			f.setRef(t, "refs/heads/feat", f.commit(t, "feat"))
			c.prepare(t, f, o)
			before := gitrepotest.Refs(t, f.copy, "")

			_, err := f.comp.ResolveBranch(context.Background(), f.ws, "feat/x", false)
			e := assertServerError(t, err, http.StatusConflict, "conflict")
			if !strings.Contains(e.Message(), "refs/heads/feat ") {
				t.Errorf("the message %q does not name the branch in the way", e.Message())
			}
			if refs := gitrepotest.Refs(t, f.copy, ""); !maps.Equal(refs, before) {
				t.Errorf("the copy's refs = %v, want them as before %v", refs, before)
			}
			if n := o.Requests(); n != 0 {
				t.Errorf("origin received %d requests, want none", n)
			}
		})
	}
}
