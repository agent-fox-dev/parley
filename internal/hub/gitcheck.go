package hub

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// gitCheckHook is the start hook that checks git's version.
const gitCheckHook = "git-version"

// gitCheckLimit is how long the check waits for git --version.
const gitCheckLimit = 10 * time.Second

// composeGitCheck adds the start hook that refuses to start on a host
// without git 2.38 or later. It is the first part, so that its hook runs
// before every hook that may run git; the runs that end before the hooks
// (--help, a usage error, backup, the bootstrap) need no git. The version
// found is a start report, so that a start that fails in a later hook still
// writes its one error line and nothing else
// (docs/adr/06-load-the-secret-keys-before-the-stores-open.md).
func composeGitCheck(c *Composition) {
	var version string
	c.AddStartHook(gitCheckHook, func(ctx context.Context, _ *Runtime) error {
		v, err := gitrepo.CheckVersion(ctx, lookPathIn(c.env), gitCheckLimit)
		version = v.Text
		return err
	})
	c.addStartReport(func(logger *slog.Logger) {
		logger.Info("git version checked", "git_version", version)
	})
}

// lookPathIn finds an executable on the PATH of env. An env without a PATH,
// as a test's often is, falls back to the process's PATH, the one
// internal/gitrepo runs git with; in parleyd both are the same.
func lookPathIn(env config.Env) func(string) (string, error) {
	return func(name string) (string, error) {
		path, ok := env("PATH")
		if !ok {
			return exec.LookPath(name)
		}
		for _, dir := range filepath.SplitList(path) {
			// A relative entry would resolve against the working directory,
			// which exec.LookPath refuses too.
			if !filepath.IsAbs(dir) {
				continue
			}
			file := filepath.Join(dir, name)
			if fi, err := os.Stat(file); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
				return file, nil
			}
		}
		return "", exec.ErrNotFound
	}
}
