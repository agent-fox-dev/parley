package e2e

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gitClient runs stock git as a developer's machine would against the hub,
// with none of this machine's git or parley configuration: an empty HOME, no
// system configuration, no terminal prompt, and the parley credential helper
// built from this tree configured for the hub's URL, with the hub URL and the
// credential in PARLEY_ENDPOINT_URL and PARLEY_API_KEY.
type gitClient struct {
	// env is git's whole environment; of a variable given twice the last
	// value counts.
	env []string
	// helper is the -c option that configures the credential helper.
	helper string
}

// newGitClient builds parley and returns the git client of the hub at hubURL
// with credential.
func newGitClient(t *testing.T, hubURL, credential string) *gitClient {
	t.Helper()
	parley := build(t, t.TempDir(), "./cmd/parley", "dev", "none", "")
	return &gitClient{
		env: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"PARLEY_ENDPOINT_URL=" + hubURL,
			"PARLEY_API_KEY=" + credential,
		},
		helper: "credential." + hubURL + ".helper=!" + parley + " credential-helper",
	}
}

// git runs git with args in dir, the test's working directory when dir is
// "", and returns its output, stdout and stderr together.
func (g *gitClient) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", g.helper}, args...)...)
	cmd.Dir, cmd.Env = dir, g.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestGit_TheCredentialHelperGivesTheHubCredentialToStockGitAndToNoOtherHost(t *testing.T) {
	h := startForkHub(t)
	remote := h.URL("/git/acme/demo.git")
	g := newGitClient(t, h.URL(""), h.key)

	out, err := g.git("", "ls-remote", remote)
	if err != nil || !strings.Contains(out, "\trefs/heads/main\n") {
		t.Fatalf("git ls-remote %s: %v, output %q; want the refs of demo", remote, err, out)
	}

	// The helper is asked for the hub's address while its settings name the
	// same hub under another host name, so it answers nothing.
	g.env = append(g.env, "PARLEY_ENDPOINT_URL="+strings.Replace(h.URL(""), "127.0.0.1", "localhost", 1))
	out, err = g.git("", "ls-remote", remote)
	if err == nil || !strings.Contains(out, "could not read Username") {
		t.Errorf("git ls-remote %s with the helper set for another host: %v, output %q; want git to find no credential", remote, err, out)
	}
}
