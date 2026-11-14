package gitrepo_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// token is the origin's password in the credential tests.
const token = "tok-123"

// argvLog records the arguments of every git invocation.
type argvLog struct {
	mu   sync.Mutex
	argv [][]string
}

func (l *argvLog) trace(args []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.argv = append(l.argv, append([]string(nil), args...))
}

// containing returns the first argument that holds s, or "".
func (l *argvLog) containing(s string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, argv := range l.argv {
		for _, a := range argv {
			if strings.Contains(a, s) {
				return a
			}
		}
	}
	return ""
}

func TestCredential_ReachesGitOnlyThroughTheEnvironment(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", token))
	mainHead, _, _ := o.History()
	log := &argvLog{}
	g := gitrepo.Git{Env: o.Env(), Trace: log.trace}
	ctx := context.Background()

	cases := []struct {
		name string
		cred gitrepo.Credential
		ok   bool
	}{
		{"the token", gitrepo.Credential{Username: "x-token-auth", Password: token}, true},
		{"no credential", gitrepo.Credential{}, false},
		{"another token", gitrepo.Credential{Username: "x-token-auth", Password: "tok-999"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remote, err := g.ListRemote(ctx, o.URL(), c.cred)
			if (err == nil) != c.ok {
				t.Fatalf("ListRemote = %v, want success %v", err, c.ok)
			}
			if c.ok && remote.Branches["main"] != mainHead {
				t.Errorf("ListRemote main = %q, want %s", remote.Branches["main"], mainHead)
			}
			dir := filepath.Join(t.TempDir(), "copy.git")
			err = g.Clone(ctx, o.URL(), c.cred, dir)
			if (err == nil) != c.ok {
				t.Fatalf("Clone = %v, want success %v", err, c.ok)
			}
			if c.ok {
				assertNoTokenInCopy(t, g, dir, o.URL())
			}
		})
	}
	if a := log.containing(token); a != "" {
		t.Errorf("a git argument holds the token: %q", a)
	}
	if len(log.argv) == 0 {
		t.Error("the trace recorded no git invocation")
	}
}

// assertNoTokenInCopy checks that the token is nowhere in the copy's files
// or its configuration and that its origin URL is the URL without user
// information.
func assertNoTokenInCopy(t *testing.T, g gitrepo.Git, dir, url string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), token) {
			t.Errorf("the copy's file %s holds the token", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := exec.Command("git", "--git-dir", dir, "config", "--list").CombinedOutput()
	if err != nil {
		t.Fatalf("git config --list: %v: %s", err, list)
	}
	if strings.Contains(string(list), token) {
		t.Errorf("git config --list of the copy holds the token:\n%s", list)
	}
	repo, err := g.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := repo.OriginURL(context.Background()); err != nil || got != url {
		t.Errorf("OriginURL = %q, %v; want %q", got, err, url)
	}
}

func TestCredential_IsNotGivenToTheHostOriginRedirectsTo(t *testing.T) {
	t.Parallel()
	// The other server is on origin's host at another port: a different
	// origin all the same, which asks for a credential and keeps what it
	// is sent.
	var mu sync.Mutex
	var received []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			mu.Lock()
			received = append(received, auth)
			mu.Unlock()
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="other"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(other.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	g := gitrepo.Git{}
	ctx := context.Background()
	url := origin.URL + "/repo.git"
	cred := gitrepo.Credential{Username: "x-token-auth", Password: token}
	if _, err := g.ListRemote(ctx, url, cred); err == nil {
		t.Error("ListRemote succeeded against an origin that redirects to a server refusing it")
	}
	if err := g.Clone(ctx, url, cred, filepath.Join(t.TempDir(), "copy.git")); err == nil {
		t.Error("Clone succeeded against an origin that redirects to a server refusing it")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, auth := range received {
		t.Errorf("the host origin redirected to received the credential: Authorization %q", auth)
	}
}

func TestListRemote_ReportsBranchesTagsAndTheDefaultBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, defaultBranch string
	}{
		{"main is the default", "main"},
		{"dev is the default", "dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := gitrepotest.NewOrigin(t)
			mainHead, devHead, v1 := o.History()
			o.SetDefaultBranch(c.defaultBranch)
			got, err := gitrepo.Git{Env: o.Env()}.ListRemote(context.Background(), o.URL(), gitrepo.Credential{})
			if err != nil {
				t.Fatal(err)
			}
			if got.DefaultBranch != c.defaultBranch {
				t.Errorf("DefaultBranch = %q, want %q", got.DefaultBranch, c.defaultBranch)
			}
			assertRefs(t, "Branches", got.Branches, map[string]string{"main": mainHead, "dev": devHead})
			assertRefs(t, "Tags", got.Tags, map[string]string{"v1": v1})
		})
	}
}

func TestListRemote_AnswersAnEmptyOriginWithNoBranches(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	got, err := gitrepo.Git{Env: o.Env()}.ListRemote(context.Background(), o.URL(), gitrepo.Credential{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Branches) != 0 || len(got.Tags) != 0 {
		t.Errorf("ListRemote of an empty origin = %+v, want no branches and no tags", got)
	}
}

func TestClone_MakesABareCopyWithEveryBranchTagAndFullHistory(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	mainHead, devHead, v1 := o.History()
	g := gitrepo.Git{Env: o.Env()}
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "copy.git")
	if err := g.Clone(ctx, o.URL(), gitrepo.Credential{}, dir); err != nil {
		t.Fatal(err)
	}
	repo, err := g.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := repo.DefaultBranch(ctx); err != nil || got != "main" {
		t.Errorf("DefaultBranch = %q, %v; want main", got, err)
	}
	branches, err := repo.Branches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertRefs(t, "Branches", branches, map[string]string{"main": mainHead, "dev": devHead})
	if got, found, err := repo.ResolveBranch(ctx, "dev"); err != nil || !found || got != devHead {
		t.Errorf("ResolveBranch(dev) = %q, %v, %v; want %s", got, found, err, devHead)
	}
	if got, found, err := repo.ResolveBranch(ctx, "nope"); err != nil || found {
		t.Errorf("ResolveBranch(nope) = %q, %v, %v; want not found", got, found, err)
	}
	for _, c := range []struct{ args, want string }{
		{"rev-parse --is-bare-repository", "true"},
		{"rev-parse --is-shallow-repository", "false"},
		{"rev-list --count main", "3"},
		{"rev-parse refs/tags/v1", v1},
	} {
		if got := gitrepotest.Git(t, dir, strings.Fields(c.args)...); got != c.want {
			t.Errorf("git %s = %q, want %q", c.args, got, c.want)
		}
	}
	if err := gitrepo.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("after Remove, stat = %v; want the copy gone", err)
	}
}

func TestRepo_WritesOriginHeadsConfigAndTheDefaultBranch(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	mainHead, devHead, _ := o.History()
	g := gitrepo.Git{Env: o.Env()}
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "copy.git")
	if err := g.Clone(ctx, o.URL(), gitrepo.Credential{}, dir); err != nil {
		t.Fatal(err)
	}
	repo, err := g.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	if heads, err := repo.OriginHeads(ctx); err != nil || len(heads) != 0 {
		t.Errorf("OriginHeads of a fresh clone = %v, %v; want none", heads, err)
	}
	for b, id := range map[string]string{"main": mainHead, "dev": devHead} {
		if err := repo.SetOriginHead(ctx, b, id); err != nil {
			t.Fatal(err)
		}
	}
	heads, err := repo.OriginHeads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertRefs(t, "OriginHeads", heads, map[string]string{"main": mainHead, "dev": devHead})
	if got := gitrepotest.Git(t, dir, "rev-parse", "refs/remotes/origin/dev"); got != devHead {
		t.Errorf("refs/remotes/origin/dev = %q, want %s", got, devHead)
	}
	if err := repo.DeleteOriginHead(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if got, found, err := repo.OriginHead(ctx, "dev"); err != nil || found {
		t.Errorf("OriginHead(dev) after the delete = %q, %v, %v; want not found", got, found, err)
	}
	if got, found, err := repo.OriginHead(ctx, "main"); err != nil || !found || got != mainHead {
		t.Errorf("OriginHead(main) = %q, %v, %v; want %s", got, found, err, mainHead)
	}

	if got, found, err := repo.Config(ctx, "parley.workspace"); err != nil || found {
		t.Errorf("Config before it is set = %q, %v, %v; want not found", got, found, err)
	}
	if err := repo.SetConfig(ctx, "parley.workspace", "abc"); err != nil {
		t.Fatal(err)
	}
	if got, found, err := repo.Config(ctx, "parley.workspace"); err != nil || !found || got != "abc" {
		t.Errorf("Config = %q, %v, %v; want abc", got, found, err)
	}

	if err := repo.SetDefaultBranch(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.DefaultBranch(ctx); err != nil || got != "dev" {
		t.Errorf("DefaultBranch after setting it = %q, %v; want dev", got, err)
	}
}

func TestOpen_RefusesWhatIsNotARepository(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	file := filepath.Join(root, "file.git")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(root, "empty.git")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"a plain file":       file,
		"an empty directory": empty,
		"nothing":            filepath.Join(root, "missing.git"),
	} {
		if _, err := (gitrepo.Git{}).Open(context.Background(), dir); err == nil {
			t.Errorf("Open(%s) = nil error, want an error", name)
		}
	}
}

func TestGit_EndsWithTheContextAndLeavesNoGitProcess(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(ctx context.Context, url string) error
	}{
		{"ListRemote", func(ctx context.Context, url string) error {
			_, err := gitrepo.Git{}.ListRemote(ctx, url, gitrepo.Credential{Username: "u", Password: "p"})
			return err
		}},
		{"Clone", func(ctx context.Context, url string) error {
			return gitrepo.Git{}.Clone(ctx, url, gitrepo.Credential{}, filepath.Join(t.TempDir(), "copy.git"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ln := newSilentListener(t)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := c.run(ctx, ln.url)
			if err == nil {
				t.Fatal("err = nil, want an error")
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("returned after %v, want within 2s", d)
			}
			// A connection is held by git's transport helper, a child of git:
			// it closes only when that process is gone, and a helper left
			// running that had not connected yet would connect now.
			if !ln.quiet(500*time.Millisecond, 2*time.Second) {
				t.Errorf("a connection to origin is still open 2s after the return (err %v): a git process is left", err)
			}
		})
	}
}

// Not parallel: it puts a fake ssh first on the PATH of the test process,
// which git finds through the PATH it is given.
func TestGit_RunsSSHInBatchModeUnlessTheCallerGivesItsOwnCommand(t *testing.T) {
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script := "#!/bin/sh\necho \"$*\" > '" + argsFile + "'\nexit 255\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, c := range []struct {
		name            string
		env             []string
		want, wantNever string
	}{
		{"the minimal environment", nil, "-o BatchMode=yes", ""},
		{"a caller's GIT_SSH_COMMAND", []string{"GIT_SSH_COMMAND=ssh -o ConnectTimeout=1"}, "-o ConnectTimeout=1", "BatchMode"},
	} {
		_ = os.Remove(argsFile)
		if _, err := (gitrepo.Git{Env: c.env}).ListRemote(context.Background(), "git@example.invalid:a/b.git", gitrepo.Credential{}); err == nil {
			t.Errorf("%s: ListRemote over the failing ssh = nil, want an error", c.name)
		}
		got, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatalf("%s: git never ran ssh: %v", c.name, err)
		}
		if !strings.Contains(string(got), c.want) || (c.wantNever != "" && strings.Contains(string(got), c.wantNever)) {
			t.Errorf("%s: ssh ran with %q, want %q and no %q", c.name, got, c.want, c.wantNever)
		}
	}
}

// silentListener accepts connections and never answers them.
type silentListener struct {
	url  string
	mu   sync.Mutex
	open int
}

// newSilentListener listens on a loopback port until the test ends; its url
// is an https URL that points at it.
func newSilentListener(t *testing.T) *silentListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	l := &silentListener{url: "https://" + ln.Addr().String() + "/x.git"}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			l.add(1)
			go func() {
				defer conn.Close()
				_, _ = io.Copy(io.Discard, conn)
				l.add(-1)
			}()
		}
	}()
	return l
}

func (l *silentListener) add(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open += n
}

// quiet reports whether no connection is open at some moment after at
// least watch and before limit have passed.
func (l *silentListener) quiet(watch, limit time.Duration) bool {
	start := time.Now()
	for time.Since(start) < limit {
		l.mu.Lock()
		open := l.open
		l.mu.Unlock()
		if open == 0 && time.Since(start) >= watch {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// assertRefs compares refs by name.
func assertRefs(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
		return
	}
	for name, id := range want {
		if got[name] != id {
			t.Errorf("%s[%s] = %q, want %s", what, name, got[name], id)
		}
	}
}
