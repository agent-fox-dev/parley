package gitserver

import (
	"errors"
	"fmt"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

func TestGitText_GivesGitsReasonWithoutAPathOfTheHub(t *testing.T) {
	t.Parallel()
	const repo = "/var/lib/parley/repos/demo"
	failed := func(stderr string) error {
		return &gitrepo.ServeError{Command: "upload-pack", Stderr: stderr, Err: errors.New("exit status 128")}
	}
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"git's last line without its prefix", failed("warning: x\nfatal: protocol error: bad line length 9"), "protocol error: bad line length 9"},
		{"a ref name", failed("fatal: unknown ref refs/heads/gone"), "unknown ref refs/heads/gone"},
		{"the repository's place", failed("fatal: '" + repo + "' does not appear to be a git repository"), "'repository' does not appear to be a git repository"},
		{"another place of the hub", failed("error: object directory /var/lib/parley/repos/upstream/objects does not exist"), "upload-pack failed"},
		{"a quoted place of the hub", failed("fatal: unable to read '/var/lib/parley/repos/upstream/objects/info/alternates'"), "upload-pack failed"},
		{"git wrote nothing", failed(""), "upload-pack failed"},
		{"no failure of git", errors.New("keep the request: disk full"), "upload-pack failed"},
		{"a request too large", fmt.Errorf("git upload-pack: %w", gitrepo.ErrRequestTooLarge), "request too large"},
	} {
		if got := gitText(c.err, ServiceUploadPack, repo); got != c.want {
			t.Errorf("%s: gitText = %q, want %q", c.name, got, c.want)
		}
	}
}
