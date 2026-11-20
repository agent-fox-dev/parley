package gitrepo_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	fetchDir := filepath.Join(t.TempDir(), "fetch.git")
	if err := g.Clone(ctx, o.URL(), gitrepo.Credential{Username: "x-token-auth", Password: token}, fetchDir); err != nil {
		t.Fatal(err)
	}

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
			if _, err := g.FetchOrigin(ctx, fetchDir, o.URL(), c.cred, time.Minute); (err == nil) != c.ok {
				t.Errorf("FetchOrigin = %v, want success %v", err, c.ok)
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
	for range 2 { // unsetting a key that is not set is no error
		if err := repo.UnsetConfig(ctx, "parley.workspace"); err != nil {
			t.Fatal(err)
		}
		if got, found, err := repo.Config(ctx, "parley.workspace"); err != nil || found {
			t.Errorf("Config after UnsetConfig = %q, %v, %v; want not found", got, found, err)
		}
	}

	if err := repo.SetDefaultBranch(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.DefaultBranch(ctx); err != nil || got != "dev" {
		t.Errorf("DefaultBranch after setting it = %q, %v; want dev", got, err)
	}
}

// configRepo is a bare repository whose own configuration holds what git
// init wrote and then extra, opened with a count of the git processes it
// runs.
func configRepo(t *testing.T, extra string) (dir string, repo *gitrepo.Repo, calls *atomic.Int32) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, filepath.Dir(dir), "init", "--quiet", "--bare", "--initial-branch=main", dir)
	f, err := os.OpenFile(filepath.Join(dir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	calls = new(atomic.Int32)
	repo, err = gitrepo.Git{Trace: func([]string) { calls.Add(1) }}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, repo, calls
}

// gitConfig is what git config --local --get says of key in dir.
func gitConfig(t *testing.T, dir, key string) (string, bool) {
	t.Helper()
	out, err := gitrepotest.Run(dir, "", "config", "--local", "--get", key)
	if err != nil {
		return "", false
	}
	return strings.TrimSuffix(out, "\n"), true
}

func TestConfig_ReadsTheRepositorysOwnConfigurationWithoutRunningGit(t *testing.T) {
	t.Parallel()
	dir, repo, calls := configRepo(t, `[remote "origin"]
	url = https://example.invalid/a.git # where it came from
	fetch = +refs/heads/*:refs/remotes/origin/*
; a comment
[parley]
	workspace = abc
	upstreamDefaultBranch = main
[Parley]
	UPSTREAMDEFAULTBRANCH  =   trunk   line   ; the last one counts
	empty =
[parley.sub]
	key = in a subsection
[parley "sub"]
	key = in a subsection too
[other]
	workspace = not this one
`)
	for _, c := range []struct {
		key, want string
		found     bool
	}{
		{"parley.workspace", "abc", true},
		{"parley.upstreamDefaultBranch", "trunk   line", true},
		{"PARLEY.UpstreamDefaultBranch", "trunk   line", true},
		{"parley.empty", "", true},
		{"parley.key", "", false},
		{"parley.missing", "", false},
		{"core.bare", "true", true},
	} {
		want, wantFound := gitConfig(t, dir, c.key)
		if want != c.want || wantFound != c.found {
			t.Fatalf("git config --get %s = %q, %v; the case wants %q, %v", c.key, want, wantFound, c.want, c.found)
		}
		calls.Store(0)
		got, found, err := repo.Config(context.Background(), c.key)
		if err != nil || found != c.found || got != c.want {
			t.Errorf("Config(%s) = %q, %v, %v; want %q, %v", c.key, got, found, err, c.want, c.found)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("Config(%s) ran %d git processes, want none", c.key, n)
		}
	}
}

func TestConfig_AsksGitAboutWhatItDoesNotReadItself(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ extra, key string }{
		"a quoted value":      {"[parley]\n\tworkspace = \"a # b\"\n", "parley.workspace"},
		"an escape":           {"[parley]\n\tworkspace = a\\tb\n", "parley.workspace"},
		"a continued line":    {"[parley]\n\tworkspace = a\\\n b\n", "parley.workspace"},
		"a key with no value": {"[parley]\n\tworkspace\n", "parley.workspace"},
		"an include":          {"[include]\n\tpath = other\n", "parley.workspace"},
		"a header and a key":  {"[parley] workspace = abc\n", "parley.workspace"},
	} {
		dir, repo, _ := configRepo(t, c.extra)
		if name == "an include" {
			if err := os.WriteFile(filepath.Join(dir, "other"), []byte("[parley]\n\tworkspace = included\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, wantFound := gitConfig(t, dir, c.key)
		got, found, err := repo.Config(context.Background(), c.key)
		if err != nil || found != wantFound || got != want {
			t.Errorf("%s: Config(%s) = %q, %v, %v; want git's %q, %v", name, c.key, got, found, err, want, wantFound)
		}
	}
}

func TestSetConfig_LeavesAKeyThatHoldsTheValueAlready(t *testing.T) {
	t.Parallel()
	dir, repo, calls := configRepo(t, "[parley]\n\tworkspace = abc\n\ttwice = a\n\ttwice = a\n")
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		write func() error
		key   string
		want  string
		found bool
		git   bool
	}{
		{"the value it holds", func() error { return repo.SetConfig(ctx, "parley.workspace", "abc") }, "parley.workspace", "abc", true, false},
		{"another value", func() error { return repo.SetConfig(ctx, "parley.workspace", "def") }, "parley.workspace", "def", true, true},
		{"a key that is not set", func() error { return repo.SetConfig(ctx, "parley.new", "x") }, "parley.new", "x", true, true},
		{"the removal of a key that is not set", func() error { return repo.UnsetConfig(ctx, "parley.gone") }, "parley.gone", "", false, false},
		{"the removal of a key that is set", func() error { return repo.UnsetConfig(ctx, "parley.new") }, "parley.new", "", false, true},
	} {
		calls.Store(0)
		if err := c.write(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got, found := gitConfig(t, dir, c.key); got != c.want || found != c.found {
			t.Errorf("%s: git config --get %s = %q, %v; want %q, %v", c.name, c.key, got, found, c.want, c.found)
		}
		if ran := calls.Load() != 0; ran != c.git {
			t.Errorf("%s: ran git = %v, want %v", c.name, ran, c.git)
		}
	}
	// A key set twice is git's to refuse.
	if err := repo.SetConfig(ctx, "parley.twice", "a"); err == nil {
		t.Errorf("SetConfig of a key set twice = nil, want git's refusal")
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
	// partial is a bare repository with one of its parts taken away or
	// broken by change.
	partial := func(name string, change func(dir string) error) string {
		dir := filepath.Join(root, name)
		gitrepotest.Git(t, root, "init", "--quiet", "--bare", dir)
		if err := change(dir); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for name, dir := range map[string]string{
		"a plain file":       file,
		"an empty directory": empty,
		"nothing":            filepath.Join(root, "missing.git"),
		"no objects directory": partial("no-objects.git", func(dir string) error {
			return os.RemoveAll(filepath.Join(dir, "objects"))
		}),
		"no refs directory": partial("no-refs.git", func(dir string) error {
			return os.RemoveAll(filepath.Join(dir, "refs"))
		}),
		"no HEAD": partial("no-head.git", func(dir string) error {
			return os.Remove(filepath.Join(dir, "HEAD"))
		}),
		"a HEAD that is neither a ref nor an id": partial("bad-head.git", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "HEAD"), []byte("main\n"), 0o644)
		}),
		"a HEAD that names a ref outside refs/": partial("outside-head.git", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: heads/main\n"), 0o644)
		}),
	} {
		if _, err := (gitrepo.Git{}).Open(context.Background(), dir); err == nil {
			t.Errorf("Open(%s) = nil error, want an error", name)
		}
	}
}

func TestOpen_OpensAGitDirectoryWithoutRunningGit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	unborn := filepath.Join(root, "unborn.git")
	gitrepotest.Git(t, root, "init", "--quiet", "--bare", "--initial-branch=main", unborn)
	detached := filepath.Join(root, "detached.git")
	gitrepotest.Git(t, root, "init", "--quiet", "--bare", "--initial-branch=main", detached)
	if _, err := gitrepotest.Run(detached, "commit refs/heads/main\ncommitter T <t@example.invalid> 1700000000 +0000\ndata 1\nc\n\n", "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, detached, "update-ref", "--no-deref", "HEAD", "main")
	gitrepotest.Git(t, detached, "pack-refs", "--all")
	for name, dir := range map[string]string{
		"a bare repository whose branch has no commit yet": unborn,
		"a bare repository with a detached HEAD":           detached,
	} {
		var calls atomic.Int32
		repo, err := gitrepo.Git{Trace: func([]string) { calls.Add(1) }}.Open(context.Background(), dir)
		if err != nil {
			t.Errorf("Open(%s) = %v, want a repository", name, err)
			continue
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("Open(%s) ran %d git processes, want none", name, n)
		}
		if repo.Dir() != dir {
			t.Errorf("Open(%s).Dir() = %q, want %q", name, repo.Dir(), dir)
		}
	}
}

func TestResolveBranch_ReadsTheBranchWithoutRunningGit(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, filepath.Dir(dir), "init", "--quiet", "--bare", "--initial-branch=main", dir)
	who := "committer T <t@example.invalid> 1700000000 +0000\n"
	if _, err := gitrepotest.Run(dir, "commit refs/heads/main\n"+who+"data 2\nc1\n\n"+
		"commit refs/heads/packed\n"+who+"data 2\nc2\nfrom refs/heads/main\n\n"+
		"commit refs/heads/feature/moved\n"+who+"data 2\nc3\nfrom refs/heads/main\n\n", "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, dir, "pack-refs", "--all")
	main := gitrepotest.Git(t, dir, "rev-parse", "main")
	// A loose ref written after pack-refs takes the place of the packed one.
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/feature/moved", main)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/loose", main)
	packed := gitrepotest.Git(t, dir, "rev-parse", "packed")
	var calls atomic.Int32
	repo, err := gitrepo.Git{Trace: func([]string) { calls.Add(1) }}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		branch, want string
		found        bool
	}{
		{"main", main, true},
		{"packed", packed, true},
		{"feature/moved", main, true},
		{"loose", main, true},
		{"nope", "", false},
		{"feature", "", false},
	} {
		calls.Store(0)
		got, found, err := repo.ResolveBranch(context.Background(), c.branch)
		if err != nil || found != c.found || got != c.want {
			t.Errorf("ResolveBranch(%s) = %q, %v, %v; want %q, %v", c.branch, got, found, err, c.want, c.found)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("ResolveBranch(%s) ran %d git processes, want none", c.branch, n)
		}
	}
}

// forEachRef is what git for-each-ref lists below prefix in dir, by name
// without the prefix.
func forEachRef(t *testing.T, dir, prefix string) map[string]string {
	t.Helper()
	refs := map[string]string{}
	for line := range strings.Lines(gitrepotest.Git(t, dir, "for-each-ref", "--format=%(objectname) %(refname)", prefix)) {
		id, name, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name, under := strings.CutPrefix(name, prefix); under && name != "HEAD" {
			refs[name] = id
		}
	}
	return refs
}

func TestBranchesTagsAndOriginHeads_ReadTheRefsWithoutRunningGit(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, filepath.Dir(dir), "init", "--quiet", "--bare", "--initial-branch=main", dir)
	who := "committer T <t@example.invalid> 1700000000 +0000\n"
	if _, err := gitrepotest.Run(dir, "commit refs/heads/main\n"+who+"data 2\nc1\n\n"+
		"commit refs/heads/packed\n"+who+"data 2\nc2\nfrom refs/heads/main\n\n"+
		"commit refs/heads/feature/moved\n"+who+"data 2\nc3\nfrom refs/heads/main\n\n"+
		"commit refs/heads/gone\n"+who+"data 2\nc4\nfrom refs/heads/main\n\n"+
		"tag v1\nfrom refs/heads/main\ntagger T <t@example.invalid> 1700000000 +0000\ndata 2\nv1\n", "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, dir, "pack-refs", "--all")
	main := gitrepotest.Git(t, dir, "rev-parse", "main")
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/feature/moved", main)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/loose/deep/name", main)
	gitrepotest.Git(t, dir, "update-ref", "-d", "refs/heads/gone")
	gitrepotest.Git(t, dir, "tag", "v2", main)
	gitrepotest.Git(t, dir, "update-ref", "refs/remotes/origin/main", main)
	var calls atomic.Int32
	repo, err := gitrepo.Git{Trace: func([]string) { calls.Add(1) }}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []struct {
		prefix string
		read   func() (map[string]string, error)
	}{
		{"refs/heads/", func() (map[string]string, error) { return repo.Branches(ctx) }},
		{"refs/tags/", func() (map[string]string, error) { return repo.Tags(ctx) }},
		{"refs/remotes/origin/", func() (map[string]string, error) { return repo.OriginHeads(ctx) }},
	} {
		want := forEachRef(t, dir, c.prefix)
		calls.Store(0)
		got, err := c.read()
		if err != nil {
			t.Fatalf("%s: %v", c.prefix, err)
		}
		assertRefs(t, c.prefix, got, want)
		if n := calls.Load(); n != 0 {
			t.Errorf("reading %s ran %d git processes, want none", c.prefix, n)
		}
	}
	if got := forEachRef(t, dir, "refs/heads/"); len(got) != 4 || got["gone"] != "" {
		t.Errorf("the fixture's branches are %v; want main, packed, feature/moved and loose/deep/name", got)
	}
}

func TestOriginHeadAndALostCompareAndSwap_ReadTheRefWithoutRunningGit(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.InitBare(t, dir, "main")
	c1 := gitrepotest.Commit(t, dir, "c1", map[string]string{"f.txt": "1\n"})
	c2 := gitrepotest.Commit(t, dir, "c2", map[string]string{"f.txt": "2\n"}, c1)
	gitrepotest.SetRef(t, dir, "refs/remotes/origin/main", c1)
	gitrepotest.SetRef(t, dir, "refs/heads/b", c2)
	var mu sync.Mutex
	var ran [][]string
	repo, err := gitrepo.Git{Trace: func(args []string) {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, args)
	}}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	took := func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		out := ran
		ran = nil
		return out
	}

	if id, found, err := repo.OriginHead(context.Background(), "main"); err != nil || !found || id != c1 {
		t.Errorf("OriginHead(main) = %q, %v, %v; want %s", id, found, err, c1)
	}
	if got := took(); len(got) != 0 {
		t.Errorf("OriginHead(main) ran git %v, want none", got)
	}
	// A swap that loses runs git update-ref, and reads where the ref went
	// itself.
	if err := repo.CompareAndSwapRef(context.Background(), "refs/heads/b", c1, c1); !errors.Is(err, gitrepo.ErrRefMoved) {
		t.Errorf("CompareAndSwapRef from a moved ref = %v, want ErrRefMoved", err)
	}
	if got := took(); len(got) != 1 {
		t.Errorf("the lost CompareAndSwapRef ran git %v, want its update-ref alone", got)
	}
}

func TestBranches_AsksGitAboutWhatItDoesNotReadItself(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, filepath.Dir(dir), "init", "--quiet", "--bare", "--initial-branch=main", dir)
	if _, err := gitrepotest.Run(dir, "commit refs/heads/main\ncommitter T <t@example.invalid> 1700000000 +0000\ndata 1\nc\n\n", "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	gitrepotest.Git(t, dir, "symbolic-ref", "refs/heads/alias", "refs/heads/main")
	repo, err := gitrepo.Git{}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.Branches(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertRefs(t, "Branches with a symbolic ref", got, forEachRef(t, dir, "refs/heads/"))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Branches(context.Background()); err == nil {
		t.Errorf("Branches of a removed repository = %v, nil; want an error", got)
	}
}

func TestResolveBranch_AsksGitAboutWhatItDoesNotReadItself(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, filepath.Dir(dir), "init", "--quiet", "--bare", "--initial-branch=main", dir)
	if _, err := gitrepotest.Run(dir, "commit refs/heads/main\ncommitter T <t@example.invalid> 1700000000 +0000\ndata 1\nc\n\n", "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	main := gitrepotest.Git(t, dir, "rev-parse", "main")
	gitrepotest.Git(t, dir, "symbolic-ref", "refs/heads/alias", "refs/heads/main")
	gitrepotest.Git(t, dir, "symbolic-ref", "refs/heads/dangling", "refs/heads/gone")
	if err := os.WriteFile(filepath.Join(dir, "refs", "heads", "garbled"), []byte("not an id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := gitrepo.Git{}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		branch, want string
		found        bool
	}{
		{"alias", main, true},
		{"dangling", "", false},
		{"garbled", "", false},
		{"../HEAD", "", false},
	} {
		got, found, err := repo.ResolveBranch(context.Background(), c.branch)
		if err != nil || found != c.found || got != c.want {
			t.Errorf("ResolveBranch(%s) = %q, %v, %v; want %q, %v", c.branch, got, found, err, c.want, c.found)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if got, found, err := repo.ResolveBranch(context.Background(), "main"); err == nil {
		t.Errorf("ResolveBranch(main) of a removed repository = %q, %v, nil; want an error", got, found)
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

// A fetch ends in git's automatic housekeeping. Detached, it would outlive
// the call as an orphan of no process of the hub's, and a zombie in a
// container whose PID 1 is parleyd; run as part of the command, it has
// repacked the copy by the time the call returns.
func TestGit_FinishesTheHousekeepingACommandStartsBeforeItReturns(t *testing.T) {
	t.Parallel()
	o := gitrepotest.SmartOrigin(t)
	o.History()
	g := gitrepo.Git{Env: o.Env()}
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "copy.git")
	if err := g.Clone(ctx, o.URL(), gitrepo.Credential{}, dir); err != nil {
		t.Fatal(err)
	}
	// The fetch keeps what it receives as a second pack, and two packs are
	// more than the copy allows before git's gc repacks it on its own (gc,
	// not the geometric repack later gits run by default, so that one
	// fetch is enough).
	gitrepotest.Git(t, dir, "config", "fetch.unpackLimit", "1")
	gitrepotest.Git(t, dir, "config", "gc.autoPackLimit", "1")
	gitrepotest.Git(t, dir, "config", "maintenance.strategy", "gc")
	o.Commit("main", "fourth")

	if _, err := g.FetchOrigin(ctx, dir, o.URL(), gitrepo.Credential{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.pack"))
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 {
		t.Errorf("the copy holds %d packs when FetchOrigin returns, want 1: the housekeeping the fetch started still runs", len(packs))
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

// Not parallel: it puts a fake git first on the PATH of the test process,
// which records the home directory and the askpass program it is given.
func TestGit_RemovesItsPrivateDirectoryOnceGitHasRun(t *testing.T) {
	bin := t.TempDir()
	seen := filepath.Join(bin, "seen")
	script := "#!/bin/sh\nprintf '%s\\n%s\\n' \"$HOME\" \"$GIT_ASKPASS\" > '" + seen + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"ListRemote with a credential", func() error {
			_, err := gitrepo.Git{}.ListRemote(ctx, "https://example.invalid/a.git", gitrepo.Credential{Username: "u", Password: "p"})
			return err
		}},
		{"ListRemote without a credential", func() error {
			_, err := gitrepo.Git{}.ListRemote(ctx, "https://example.invalid/a.git", gitrepo.Credential{})
			return err
		}},
	} {
		_ = os.Remove(seen)
		if err := c.run(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, err := os.ReadFile(seen)
		if err != nil {
			t.Fatalf("%s: git never ran: %v", c.name, err)
		}
		for _, path := range strings.Fields(string(got)) {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("%s: %s, which git ran with, is still there after the call (stat: %v)", c.name, path, err)
			}
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
