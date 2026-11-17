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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := gitrepo.SanitizeGitText(c.text, c.secrets...); got != c.want {
				t.Errorf("SanitizeGitText(%q, %q) = %q, want %q", c.text, c.secrets, got, c.want)
			}
		})
	}
}

func TestCheckTransport(t *testing.T) {
	t.Parallel()
	for _, url := range []string{
		"https://git.example/team/r.git",
		"HTTPS://git.example/team/r.git",
		"ssh://git@git.example/team/r.git",
		"ssh://git.example:2222/team/r.git",
		"git@git.example:team/r.git",
		"git.example:team/r.git",
		"git@[::1]:team/r.git",
	} {
		if err := gitrepo.CheckTransport(url); err != nil {
			t.Errorf("CheckTransport(%q) = %v, want nil", url, err)
		}
	}
	for _, c := range []struct{ url, scheme string }{
		{"http://git.example/r.git", "http"},
		{"git://git.example/r.git", "git"},
		{"ftp://git.example/r.git", "ftp"},
		{"git+ssh://git.example/r.git", "git+ssh"},
		{"file:///srv/r.git", "file"},
		{"/srv/r.git", "file"},
		{"./r.git", "file"},
		{"r.git", "file"},
		{"./a:b.git", "file"},
		{"ext::sh -c touch% /tmp/x", "ext"},
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

	// An https origin and an origin in ssh's user@host:path form pass the
	// check: the second fails later, when ssh connects.
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
