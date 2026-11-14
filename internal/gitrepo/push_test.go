package gitrepo_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestPush_SendsOnlyTheOneBranch(t *testing.T) {
	t.Parallel()
	o := gitrepotest.SmartOrigin(t)
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	tree := gitrepotest.Git(t, dir, "mktree")
	a := gitrepotest.Git(t, dir, "commit-tree", tree, "-m", "on a")
	b := gitrepotest.Git(t, dir, "commit-tree", tree, "-p", a, "-m", "on b")
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/a", a)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/b", b)
	gitrepotest.Git(t, dir, "update-ref", "refs/tags/v1", b)

	g := gitrepo.Git{Env: o.Env()}
	got, err := g.Push(context.Background(), gitrepo.PushRequest{Dir: dir, URL: o.URL(), Commit: a, Branch: "a"})
	if err != nil || got != gitrepo.PushPushed {
		t.Fatalf("Push = %q, %v; want pushed", got, err)
	}
	if refs, want := gitrepotest.Refs(t, o.Dir(), ""), map[string]string{"refs/heads/a": a}; !maps.Equal(refs, want) {
		t.Errorf("origin's refs after the push = %v, want %v", refs, want)
	}
}

func TestPush_ClassifiesTheFailuresOfTheSSHTransport(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "copy.git")
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", dir)
	commit := gitrepotest.Git(t, dir, "commit-tree", gitrepotest.Git(t, dir, "mktree"), "-m", "a")
	// The ssh git starts for a git@ origin writes what a real one or the
	// forge behind it writes, and fails.
	ssh := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\nprintf '%s\\n' \"$SSH_SAYS\" >&2\nexit 255\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		says string
		want error
	}{
		{"git@example.invalid: Permission denied (publickey).", gitrepo.ErrUnauthenticated},
		{"ERROR: Permission to team/repo.git denied to deploy-key.", gitrepo.ErrForbidden},
		{"ERROR: Repository not found.", gitrepo.ErrRepositoryNotFound},
		{"ssh: Could not resolve hostname example.invalid: Name or service not known", gitrepo.ErrUnreachable},
		{"ssh: connect to host example.invalid port 22: Connection refused", gitrepo.ErrUnreachable},
		{"Host key verification failed.", gitrepo.ErrUnreachable},
		// A proxy command that cannot run: the shell's "not found" is no
		// repository's.
		{"/bin/sh: 1: exec: nc: not found\nkex_exchange_identification: Connection closed by remote host", gitrepo.ErrUnreachable},
		{"the forge is having a bad day", gitrepo.ErrPushFailed},
	} {
		g := gitrepo.Git{Env: []string{"GIT_SSH_COMMAND=" + ssh, "SSH_SAYS=" + c.says}}
		got, err := g.Push(context.Background(), gitrepo.PushRequest{
			Dir: dir, URL: "git@example.invalid:team/repo.git", Commit: commit, Branch: "main",
		})
		var pushErr *gitrepo.PushError
		if !errors.Is(err, c.want) || !errors.As(err, &pushErr) || !strings.Contains(pushErr.Reason, c.says) {
			t.Errorf("ssh says %q: Push = %q, %v; want a PushError matching %v that gives the reason", c.says, got, err, c.want)
		}
	}
}
