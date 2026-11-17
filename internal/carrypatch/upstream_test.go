package carrypatch_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io/fs"
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
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// upstreamFixture is a carry-patch workspace whose copy is a bare
// repository made with git init, and a component bound to it whose git
// records the arguments of every invocation.
type upstreamFixture struct {
	comp *carrypatch.Component
	ws   workspace.Workspace
	copy string

	mu   sync.Mutex
	args [][]string
}

// newUpstreamFixture binds a component to the workspace acme, whose
// upstream_url is url and default_branch main, with the secrets of reader,
// env added to git's environment and deadline as the upstream fetch's
// deadline (0 for the default).
func newUpstreamFixture(t *testing.T, url string, env []string, reader workspace.SecretReader, deadline time.Duration) *upstreamFixture {
	t.Helper()
	f := &upstreamFixture{copy: filepath.Join(t.TempDir(), "acme.git")}
	gitrepotest.InitBare(t, f.copy, "")
	branch := "main"
	f.ws = workspace.Workspace{Slug: "acme", Mode: workspace.ModeCarryPatch, UpstreamURL: &url, DefaultBranch: &branch}
	f.comp = &carrypatch.Component{}
	f.comp.Bind(carrypatch.Deps{
		Secrets: reader,
		Git: gitrepo.Git{Env: env, Trace: func(args []string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.args = append(f.args, args)
		}},
		RepoPath:              func(string) string { return f.copy },
		UpstreamFetchDeadline: deadline,
	})
	return f
}

// gitRuns is the arguments of every git invocation so far.
func (f *upstreamFixture) gitRuns() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.args)
}

// recordedDefault is the copy's parley.upstreamDefaultBranch, "" when it is
// not set.
func (f *upstreamFixture) recordedDefault(t *testing.T) string {
	t.Helper()
	v, err := gitrepotest.Run(f.copy, "", "config", "--local", "--get", "parley.upstreamDefaultBranch")
	if err != nil {
		return ""
	}
	return v
}

// commitIn writes a root commit with the message msg and an empty tree into
// the repository dir and returns its id.
func commitIn(t *testing.T, dir, msg string) string {
	t.Helper()
	return gitrepotest.Commit(t, dir, msg, nil)
}

func TestFetchUpstream_BringsBranchesWithoutTags(t *testing.T) {
	t.Parallel()
	const token = "upstream-token-4f1c"
	up := gitrepotest.NewOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", token))
	mainHead := up.Commit("main", "one")
	release := up.Commit("release", "r1")
	up.Tag("v9", "main")
	f := newUpstreamFixture(t, up.URL(), up.Env(), secretMap{slug: "acme", values: map[string]string{"UPSTREAM_GIT_PAT": token}}, 0)
	ctx := context.Background()

	if err := f.comp.FetchUpstream(ctx, f.ws); err != nil {
		t.Fatalf("FetchUpstream: %v", err)
	}
	want := map[string]string{"refs/remotes/upstream/main": mainHead, "refs/remotes/upstream/release": release}
	if got := gitrepotest.Refs(t, f.copy, ""); !maps.Equal(got, want) {
		t.Errorf("the copy's refs = %v, want the upstream branches alone %v", got, want)
	}
	if got := f.recordedDefault(t); got != "main" {
		t.Errorf("parley.upstreamDefaultBranch = %q, want main", got)
	}
	if out, err := gitrepotest.Run(f.copy, "", "config", "--local", "--get-regexp", `^remote\.`); err == nil {
		t.Errorf("the copy's configuration has a remote section: %s", out)
	}
	err := filepath.WalkDir(f.copy, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, []byte(token)) {
			t.Errorf("the token is in the file %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range f.gitRuns() {
		if strings.Contains(strings.Join(args, " "), token) {
			t.Errorf("the token is in the git arguments %q", args)
		}
	}

	// Upstream deletes release and moves main on: the next fetch follows.
	gitrepotest.Git(t, up.Dir(), "update-ref", "-d", "refs/heads/release")
	newMain := up.Commit("main", "two")
	if err := f.comp.FetchUpstream(ctx, f.ws); err != nil {
		t.Fatalf("second FetchUpstream: %v", err)
	}
	want = map[string]string{"refs/remotes/upstream/main": newMain}
	if got := gitrepotest.Refs(t, f.copy, ""); !maps.Equal(got, want) {
		t.Errorf("the copy's refs after upstream deleted release = %v, want %v", got, want)
	}

	// An upstream whose HEAD names no branch it has reports no default
	// branch, and the recorded one goes.
	up.SetDefaultBranch("gone")
	if err := f.comp.FetchUpstream(ctx, f.ws); err != nil {
		t.Fatalf("third FetchUpstream: %v", err)
	}
	if got := f.recordedDefault(t); got != "" {
		t.Errorf("parley.upstreamDefaultBranch with no upstream default = %q, want it unset", got)
	}
}

func TestFetchUpstream_FailureChangesNothing(t *testing.T) {
	t.Parallel()
	const token = "upstream-token-9b2e"
	unreadable := errors.New("the secret store cannot be read")
	cases := []struct {
		name string
		// fail breaks the next fetch of f from upstream up.
		fail func(f *upstreamFixture, up *gitrepotest.Origin)
	}{
		{"the upstream URL names no repository", func(f *upstreamFixture, up *gitrepotest.Origin) {
			missing := strings.TrimSuffix(up.URL(), "origin.git") + "missing.git"
			f.ws.UpstreamURL = &missing
		}},
		{"the upstream answers 401 to the credential", func(_ *upstreamFixture, up *gitrepotest.Origin) {
			up.Accept("x-token-auth", "another-token")
		}},
		{"the secret reader fails", func(f *upstreamFixture, up *gitrepotest.Origin) {
			f.comp.Bind(carrypatch.Deps{
				Secrets:  secretMap{slug: "acme", fail: map[string]bool{"UPSTREAM_GIT_PAT": true}, err: unreadable},
				Git:      gitrepo.Git{Env: up.Env()},
				RepoPath: func(string) string { return f.copy },
			})
		}},
		{"the upstream URL with a password in it is stopped", func(f *upstreamFixture, up *gitrepotest.Origin) {
			up.Close()
			withUser := strings.Replace(up.URL(), "https://", "https://someone:url-secret@", 1)
			f.ws.UpstreamURL = &withUser
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			up := gitrepotest.NewOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", token))
			up.Commit("main", "one")
			up.Commit("release", "r1")
			f := newUpstreamFixture(t, up.URL(), up.Env(), secretMap{slug: "acme", values: map[string]string{"UPSTREAM_GIT_PAT": token}}, 0)
			ctx := context.Background()
			if err := f.comp.FetchUpstream(ctx, f.ws); err != nil {
				t.Fatalf("the first fetch: %v", err)
			}
			before, beforeDefault := gitrepotest.Refs(t, f.copy, ""), f.recordedDefault(t)
			// Upstream moves on, so that a fetch that got through would
			// change the copy.
			up.Commit("main", "two")
			up.Commit("next", "n1")
			up.SetDefaultBranch("release")

			c.fail(f, up)
			err := f.comp.FetchUpstream(ctx, f.ws)
			if err == nil {
				t.Fatal("FetchUpstream succeeded, want an error")
			}
			if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "another-token") || strings.Contains(err.Error(), "url-secret") {
				t.Errorf("the error shows a credential: %v", err)
			}
			if got := gitrepotest.Refs(t, f.copy, ""); !maps.Equal(got, before) {
				t.Errorf("the copy's refs after the failure = %v, want them as before %v", got, before)
			}
			if got := f.recordedDefault(t); got != beforeDefault {
				t.Errorf("parley.upstreamDefaultBranch after the failure = %q, want %q", got, beforeDefault)
			}
		})
	}
}

func TestFetchUpstream_NoAnonymousFallback(t *testing.T) {
	t.Parallel()
	// An upstream that takes anonymous requests, so that a fetch made
	// without a credential would get through.
	up := gitrepotest.NewOrigin(t)
	up.Commit("main", "one")
	every := map[string]bool{
		"UPSTREAM_GIT_PAT": true, "UPSTREAM_GIT_USERNAME": true, "UPSTREAM_GIT_PASSWORD": true,
		"GIT_PAT": true, "GIT_USERNAME": true, "GIT_PASSWORD": true,
	}
	f := newUpstreamFixture(t, up.URL(), up.Env(), secretMap{slug: "acme", fail: every, err: errors.New("unreadable")}, 0)

	if err := f.comp.FetchUpstream(context.Background(), f.ws); err == nil {
		t.Error("FetchUpstream succeeded with no readable secret, want an error")
	}
	if runs := f.gitRuns(); len(runs) != 0 {
		t.Errorf("git ran %d times, want never: %q", len(runs), runs)
	}
	if n := up.Requests(); n != 0 {
		t.Errorf("upstream received %d requests, want none", n)
	}
}

func TestFetchUpstream_StopsAtDeadline(t *testing.T) {
	t.Parallel()
	if carrypatch.DefaultGitDeadline != 10*time.Minute {
		t.Errorf("DefaultGitDeadline = %v, want 10 minutes", carrypatch.DefaultGitDeadline)
	}
	// An upstream that takes each request and never answers it: a request
	// ends only when the git process holding its connection is gone.
	var seen, open atomic.Int32
	url, env := tlsUpstream(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		open.Add(1)
		defer open.Add(-1)
		<-r.Context().Done()
	}))
	// The deadline leaves git time to start and reach the upstream also on
	// a loaded machine, where 200 ms did not.
	const deadline = time.Second
	f := newUpstreamFixture(t, url, env, secretMap{slug: "acme", values: map[string]string{"UPSTREAM_GIT_PAT": "t"}}, deadline)
	rootsAt(t, f.copy, map[string]string{"refs/remotes/upstream/main": "base"})
	before := gitrepotest.Refs(t, f.copy, "")

	start := time.Now()
	err := f.comp.FetchUpstream(context.Background(), f.ws)
	if !errors.Is(err, carrypatch.ErrGitTimeout) || !errors.Is(err, gitrepo.ErrDeadline) {
		t.Errorf("err = %v, want the deadline error", err)
	}
	if d := time.Since(start); d > deadline+2*time.Second {
		t.Errorf("FetchUpstream returned after %v, want within 2s of the deadline %v", d, deadline)
	}
	if seen.Load() == 0 {
		t.Fatal("upstream received no request")
	}
	for wait := time.Now(); open.Load() != 0; time.Sleep(10 * time.Millisecond) {
		if time.Since(wait) > 2*time.Second {
			t.Fatal("a request to upstream is still open 2s after the return: a git process is left")
		}
	}
	if got := gitrepotest.Refs(t, f.copy, ""); !maps.Equal(got, before) {
		t.Errorf("the copy's refs = %v, want them as before %v", got, before)
	}
}

func TestFetchUpstream_OneDeadlineForListingAndFetch(t *testing.T) {
	t.Parallel()
	up := gitrepotest.NewOrigin(t)
	up.Commit("main", "one")
	// The upstream answers the listing late, with part of the deadline
	// left, and never answers the fetch that follows: info/refs is the
	// first request of each.
	const deadline, listing = 2 * time.Second, 1200 * time.Millisecond
	var infoRefs atomic.Int32
	files := http.StripPrefix("/upstream.git", http.FileServer(http.Dir(up.Dir())))
	url, env := tlsUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			wait := time.After(listing)
			if infoRefs.Add(1) > 1 {
				wait = nil
			}
			select {
			case <-wait:
			case <-r.Context().Done():
				return
			}
		}
		files.ServeHTTP(w, r)
	}))
	f := newUpstreamFixture(t, url, env, secretMap{slug: "acme", values: map[string]string{"UPSTREAM_GIT_PAT": "t"}}, deadline)

	start := time.Now()
	err := f.comp.FetchUpstream(context.Background(), f.ws)
	if !errors.Is(err, carrypatch.ErrGitTimeout) || !errors.Is(err, gitrepo.ErrDeadline) {
		t.Errorf("err = %v, want the deadline error", err)
	}
	// The fetch had only what the listing left of the deadline; with one
	// of its own it would end after listing+deadline.
	if d := time.Since(start); d > deadline+time.Second {
		t.Errorf("FetchUpstream returned after %v, want within 1s of the deadline %v", d, deadline)
	}
	if got := gitrepotest.Refs(t, f.copy, ""); len(got) != 0 {
		t.Errorf("the copy's refs = %v, want none", got)
	}
}

// tlsUpstream serves h over TLS until the test ends and returns the URL of
// a repository on it and the variables a git that trusts it runs with.
func tlsUpstream(t *testing.T, h http.Handler) (url string, env []string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv.URL + "/upstream.git", []string{"GIT_SSL_CAINFO=" + ca}
}

func TestUpstreamBase_Rule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		recorded string
		refs     []string // upstream branches to write, each at its own commit
		want     string   // the branch whose commit is the base; "" for none
	}{
		{"the recorded default branch first", "trunk", []string{"trunk", "main"}, "trunk"},
		{"the workspace's default branch with none recorded", "", []string{"main"}, "main"},
		{"the workspace's default branch when the recorded one is gone", "gone", []string{"main"}, "main"},
		{"no base without the default branches", "", []string{"dev"}, ""},
		{"no base without an upstream ref", "", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newUpstreamFixture(t, "https://upstream.invalid/x.git", nil, secretMap{slug: "acme"}, 0)
			msgs := map[string]string{}
			for _, b := range c.refs {
				msgs["refs/remotes/upstream/"+b] = b
			}
			ids := map[string]string{}
			for ref, id := range rootsAt(t, f.copy, msgs) {
				ids[strings.TrimPrefix(ref, "refs/remotes/upstream/")] = id
			}
			if c.recorded != "" {
				gitrepotest.Git(t, f.copy, "config", "--local", "parley.upstreamDefaultBranch", c.recorded)
			}

			got, err := f.comp.UpstreamBase(context.Background(), f.ws)
			if c.want == "" {
				if !errors.Is(err, carrypatch.ErrNoUpstreamBase) {
					t.Errorf("UpstreamBase = %q, %v; want ErrNoUpstreamBase", got, err)
				}
			} else if err != nil || got != ids[c.want] || len(got) != 40 || strings.Trim(got, "0123456789abcdef") != "" {
				t.Errorf("UpstreamBase = %q, %v; want the 40-character id %s of %s", got, err, ids[c.want], c.want)
			}
			for _, args := range f.gitRuns() {
				if slices.Contains(args, "fetch") || slices.Contains(args, "ls-remote") {
					t.Errorf("UpstreamBase ran git %q, want no fetch", args)
				}
			}
		})
	}
}
