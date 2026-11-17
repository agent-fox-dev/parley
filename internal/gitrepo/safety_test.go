package gitrepo_test

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestSanitizeGitText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		text    string
		secrets []string
		want    string
	}{
		{"user and password of a URL",
			"fatal: unable to access 'https://bob:hunter2@git.example/x.git/': 401", nil,
			"fatal: unable to access 'https://***@git.example/x.git/': 401"},
		{"a user name alone",
			"fetching http://bob@git.example:8080/x.git", nil,
			"fetching http://***@git.example:8080/x.git"},
		{"every URL of the text",
			"https://a:b@one.example and ssh://git@two.example/r", nil,
			"https://***@one.example and ssh://***@two.example/r"},
		{"an @ in a URL's path is no user information",
			"https://git.example/@scope/pkg", nil,
			"https://git.example/@scope/pkg"},
		{"each secret value removed",
			"sent tok-1 then tok-2, tok-1 again", []string{"tok-1", "tok-2"},
			"sent  then ,  again"},
		{"an empty secret removes nothing",
			"nothing secret", []string{""},
			"nothing secret"},
		{"a secret within a URL",
			"https://git.example/x.git?token=tok-9", []string{"tok-9"},
			"https://git.example/x.git?token="},
		{"text without either is kept", "error: exit status 128", []string{"tok-3"}, "error: exit status 128"},
		{"a user and password of an access error",
			"fatal: unable to access https://alice:hunter2@example.com/r.git/", nil,
			"fatal: unable to access https://***@example.com/r.git/"},
		{"a token alone and an ssh user",
			"https://tok123@h/x and ssh://bob@h2/y", nil,
			"https://***@h/x and ssh://***@h2/y"},
		{"a URL without user information", "https://example.com/r.git", nil, "https://example.com/r.git"},
		{"ssh's user@host:path form is no URL", "git@example.com:team/r.git", nil, "git@example.com:team/r.git"},
		{"a mail address is no URL", "mail bob@example.com", nil, "mail bob@example.com"},
		{"empty text", "", []string{"hunter2"}, ""},
		{"a credential outside any URL",
			"remote: alice:hunter2 is not allowed", []string{"alice", "hunter2"},
			"remote: : is not allowed"},
		// The password holds a '/', which ends a URL's user information:
		// removed first, it leaves the user name, which must not stay.
		{"a password that cuts the URL's user information short",
			"fatal: unable to access 'https://alice:pa/ss@example.com/r.git/'", []string{"pa/ss"},
			"fatal: unable to access 'https://***@example.com/r.git/'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := gitrepo.SanitizeGitText(c.text, c.secrets...)
			if got != c.want {
				t.Errorf("SanitizeGitText(%q, %q) = %q, want %q", c.text, c.secrets, got, c.want)
			}
			if again := gitrepo.SanitizeGitText(got, c.secrets...); again != got {
				t.Errorf("SanitizeGitText applied twice to %q gives %q, once %q", c.text, again, got)
			}
		})
	}
}

func TestRefOperations_RefuseAValueThatBeginsWithADash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "copy.git")
	gitrepotest.InitBare(t, dir, "main")
	c := gitrepotest.Commit(t, dir, "c", map[string]string{"a.txt": "a\n"})
	gitrepotest.SetRef(t, dir, "refs/heads/main", c)
	o := gitrepotest.NewOrigin(t)
	o.Commit("feature/x", "x")
	pwned := filepath.Join(tmp, "pwned")

	var starts atomic.Int64
	g := gitrepo.Git{Env: o.Env(), Trace: func([]string) { starts.Add(1) }}
	repo, err := g.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	const deadline = 10 * time.Second
	// Each operation takes the value v where carry-patch code passes a
	// value a caller gave: a branch, a ref of a branch, a revision.
	ops := []struct {
		name string
		run  func(v string) error
	}{
		{"ancestry, the revision as the ancestor", func(v string) error { _, err := repo.IsAncestor(ctx, v, c); return err }},
		{"ancestry, the revision as the descendant", func(v string) error { _, err := repo.IsAncestor(ctx, c, v); return err }},
		{"read a ref", func(v string) error { _, _, err := repo.LookupRef(ctx, v); return err }},
		{"create a branch, the value as the ref", func(v string) error { return repo.CompareAndSwapRef(ctx, v, c, "") }},
		{"create a branch, the value as the commit", func(v string) error { return repo.CompareAndSwapRef(ctx, "refs/heads/made", v, "") }},
		{"move a branch, the value as the expected commit", func(v string) error { return repo.CompareAndSwapRef(ctx, "refs/heads/main", c, v) }},
		{"point a ref, the value as the ref", func(v string) error { return repo.SetRef(ctx, v, c) }},
		{"point a ref, the value as the commit", func(v string) error { return repo.SetRef(ctx, "refs/heads/made", v) }},
		{"single-branch fetch", func(v string) error {
			_, err := g.FetchOriginBranch(ctx, dir, o.URL(), v, gitrepo.Credential{}, deadline)
			return err
		}},
		{"delete a ref", func(v string) error { return repo.DeleteRef(ctx, v) }},
	}
	refs := gitrepotest.Refs(t, dir, "")
	for _, v := range []string{"--upload-pack=touch " + pwned, "-c", "--output=" + pwned, "--exec=touch " + pwned, "-"} {
		// No branch begins with '-': resolving one finds none, as for
		// any other branch that does not exist.
		starts.Store(0)
		if id, found, err := repo.ResolveBranch(ctx, v); err != nil || found {
			t.Errorf("resolve a branch %q = %s, %v, %v, want not found and no error", v, id, found, err)
		}
		if n := starts.Load(); n != 0 {
			t.Errorf("resolve a branch %q started %d git processes, want none", v, n)
		}
		for _, op := range ops {
			starts.Store(0)
			err := op.run(v)
			if !errors.Is(err, gitrepo.ErrInvalidArgument) {
				t.Errorf("%s with %q = %v, want ErrInvalidArgument", op.name, v, err)
			}
			if n := starts.Load(); n != 0 {
				t.Errorf("%s with %q started %d git processes, want none", op.name, v, n)
			}
			if _, err := os.Stat(pwned); !os.IsNotExist(err) {
				t.Fatalf("after %s with %q the injected option wrote %s: %v", op.name, v, pwned, err)
			}
		}
	}
	if got := gitrepotest.Refs(t, dir, ""); !maps.Equal(got, refs) {
		t.Errorf("the refusals changed the refs to %v, want %v", got, refs)
	}

	// A branch name a caller may give passes each of them.
	const branch, ref = "feature/x", "refs/heads/feature/x"
	if err := repo.CompareAndSwapRef(ctx, ref, c, ""); err != nil {
		t.Errorf("create %s = %v", ref, err)
	}
	if err := repo.SetRef(ctx, ref, c); err != nil {
		t.Errorf("point %s = %v", ref, err)
	}
	if got, found, err := repo.LookupRef(ctx, ref); err != nil || !found || got.ID != c {
		t.Errorf("read %s = %v, %v, %v, want it at %s", ref, got, found, err, c)
	}
	if id, found, err := repo.ResolveBranch(ctx, branch); err != nil || !found || id != c {
		t.Errorf("resolve %s = %s, %v, %v, want %s", branch, id, found, err, c)
	}
	if ok, err := repo.IsAncestor(ctx, c, ref); err != nil || !ok {
		t.Errorf("is %s an ancestor of %s = %v, %v, want true", c, ref, ok, err)
	}
	if err := repo.DeleteRef(ctx, ref); err != nil {
		t.Errorf("delete %s = %v", ref, err)
	}
	if id, err := g.FetchOriginBranch(ctx, dir, o.URL(), branch, gitrepo.Credential{}, deadline); err != nil || id != o.Head(branch) {
		t.Errorf("fetch %s = %s, %v, want %s", branch, id, err, o.Head(branch))
	}
}

func TestCheckTransport(t *testing.T) {
	t.Parallel()
	for _, url := range []string{
		"HTTPS://git.example/team/r.git",
		"ssh://git@git.example/team/r.git",
		"ssh://git.example:2222/team/r.git",
		"git.example:team/r.git",
		"git@[::1]:team/r.git",
	} {
		if err := gitrepo.CheckTransport(url); err != nil {
			t.Errorf("CheckTransport(%q) = %v, want nil", url, err)
		}
	}
	for _, c := range []struct{ url, scheme string }{
		{"ftp://git.example/r.git", "ftp"},
		{"git+ssh://git.example/r.git", "git+ssh"},
		{"./r.git", "file"},
		{"r.git", "file"},
		{"./a:b.git", "file"},
		{"fd::0", "fd"},
		{"https::https://git.example/r.git", "https"},
		{"-oProxyCommand=touch:r.git", "ssh"},
		{"git@-oProxyCommand=x:r.git", "ssh"},
		{"", "file"},
	} {
		err := gitrepo.CheckTransport(c.url)
		if !errors.Is(err, gitrepo.ErrTransportRefused) || !strings.Contains(err.Error(), `"`+c.scheme+`"`) {
			t.Errorf("CheckTransport(%q) = %v, want ErrTransportRefused naming %q", c.url, err, c.scheme)
		}
	}
}

func TestRemoteOperations_RefuseTransportsOtherThanHTTPSAndSSH(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	cp := filepath.Join(tmp, "copy.git")
	gitrepotest.InitBare(t, cp, "main")
	c := gitrepotest.Commit(t, cp, "c", map[string]string{"a.txt": "a\n"})
	gitrepotest.SetRef(t, cp, "refs/heads/main", c)
	src := filepath.Join(tmp, "src.git")
	gitrepotest.InitBare(t, src, "main")
	gitrepotest.SetRef(t, src, "refs/heads/main", gitrepotest.Commit(t, src, "s", map[string]string{"s.txt": "s\n"}))
	pwned := filepath.Join(tmp, "pwned")

	var plainRequests atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainRequests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(plain.Close)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	ca := filepath.Join(tmp, "redirect-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirect.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	g := gitrepo.Git{Env: []string{"GIT_SSL_CAINFO=" + ca}}
	const deadline = 10 * time.Second
	none := gitrepo.Credential{}

	ops := []struct {
		name string
		run  func(url string) error
	}{
		{"fetch", func(url string) error { _, err := g.FetchOrigin(ctx, cp, url, none, deadline); return err }},
		{"single-branch fetch", func(url string) error {
			_, err := g.FetchOriginBranch(ctx, cp, url, "main", none, deadline)
			return err
		}},
		{"fetch into a remote's refs", func(url string) error {
			_, err := g.FetchRemote(ctx, cp, url, "upstream", none, deadline)
			return err
		}},
		{"push", func(url string) error {
			_, err := g.Push(ctx, gitrepo.PushRequest{Dir: cp, URL: url, Commit: c, Branch: "pushed"})
			return err
		}},
		{"push of branches", func(url string) error {
			res, err := g.PushBranches(ctx, gitrepo.PushBranchesRequest{Dir: cp, URL: url, Heads: map[string]string{"pushed": c}})
			if err == nil && (len(res) != 1 || res[0].Err != nil) {
				err = fmt.Errorf("the push did not take the branch: %+v", res)
			}
			return err
		}},
		{"list", func(url string) error { _, err := g.ListRemote(ctx, url, none); return err }},
		{"clone", func(url string) error { return g.Clone(ctx, url, none, filepath.Join(t.TempDir(), "clone.git")) }},
	}
	rows := []struct {
		name, url string
		// scheme is the transport the check refuses, "" for a URL it lets
		// through to a remote that redirects elsewhere.
		scheme string
	}{
		{"a plain-HTTP server", plain.URL + "/r.git", "http"},
		{"the git protocol", "git://127.0.0.1/r.git", "git"},
		{"a file URL", "file://" + src, "file"},
		{"a local path", src, "file"},
		{"a remote helper", `ext::sh -c "touch ` + pwned + `"`, "ext"},
		{"an https remote that redirects to plain HTTP", redirect.URL + "/r.git", ""},
	}
	copyRefs, srcRefs := gitrepotest.Refs(t, cp, ""), gitrepotest.Refs(t, src, "")
	for _, row := range rows {
		for _, op := range ops {
			err := op.run(row.url)
			switch {
			case err == nil:
				t.Errorf("%s from %s = nil, want an error", op.name, row.name)
			case row.scheme != "" && (!errors.Is(err, gitrepo.ErrTransportRefused) || !strings.Contains(err.Error(), `"`+row.scheme+`"`)):
				t.Errorf("%s from %s = %v, want ErrTransportRefused naming %q", op.name, row.name, err, row.scheme)
			}
			if got := gitrepotest.Refs(t, cp, ""); !maps.Equal(got, copyRefs) {
				t.Errorf("after the %s from %s the copy's refs are %v, want %v", op.name, row.name, got, copyRefs)
				copyRefs = got
			}
			if got := gitrepotest.Refs(t, src, ""); !maps.Equal(got, srcRefs) {
				t.Errorf("after the %s from %s the local repository's refs are %v, want %v", op.name, row.name, got, srcRefs)
				srcRefs = got
			}
			if _, err := os.Stat(pwned); !os.IsNotExist(err) {
				t.Fatalf("after the %s from %s the remote helper's command ran: %v", op.name, row.name, err)
			}
		}
	}
	if n := plainRequests.Load(); n != 0 {
		t.Errorf("the plain-HTTP server received %d requests, want none", n)
	}
}

// An https origin and an origin in ssh's user@host:path form pass the
// check: the second fails later, when ssh connects.
func TestRemoteOperations_ReachAnHTTPSOrSSHRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	cp := filepath.Join(tmp, "copy.git")
	gitrepotest.InitBare(t, cp, "main")
	const deadline = 10 * time.Second
	none := gitrepo.Credential{}
	o := gitrepotest.NewOrigin(t)
	o.Commit("main", "o1")
	if _, err := (gitrepo.Git{Env: o.Env()}).FetchOrigin(ctx, cp, o.URL(), none, deadline); err != nil {
		t.Errorf("fetch from an https origin = %v, want nil", err)
	}
	reached := filepath.Join(tmp, "ssh-ran")
	ssh := filepath.Join(tmp, "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\ntouch "+reached+"\necho 'ssh: connect to host 127.0.0.1 port 22: Connection refused' >&2\nexit 255\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (gitrepo.Git{Env: []string{"GIT_SSH_COMMAND=" + ssh}}).FetchOrigin(ctx, cp, "git@127.0.0.1:team/r.git", none, deadline)
	if err == nil || errors.Is(err, gitrepo.ErrTransportRefused) {
		t.Errorf("fetch from git@127.0.0.1:team/r.git = %v, want the connection's error", err)
	}
	if _, err := os.Stat(reached); err != nil {
		t.Errorf("ssh was not started for git@127.0.0.1:team/r.git: %v", err)
	}
}
