package acceptance

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/acceptance/harness"
)

func TestMain(m *testing.M) { os.Exit(harness.Main(m)) }

func TestHarness_HubRunsFromOneBinaryConfigAndDataDir(t *testing.T) {
	t.Parallel()
	hub := harness.Start(t, harness.Options{OwnBinary: true})
	if want := "127.0.0.1:" + strconv.Itoa(hub.Port); hub.ListeningAddr != want {
		t.Errorf("the listening line names %q, want %q", hub.ListeningAddr, want)
	}
	hub.Call(http.MethodGet, "/readyz", "", nil).Want(t, http.StatusOK)

	// The fork-push-land journey, once.
	alice := hub.User("alice")
	demo := hub.Workspace(alice, "demo")
	fork := hub.Fork(alice.Key, demo.Slug, harness.AgentScopes, nil)
	dir := t.TempDir()
	hub.Git(dir, "", "clone", harness.AuthURL(fork.Remote, fork.Token), "fork").Must(t)
	clone := filepath.Join(dir, "fork")
	hub.CommitFiles(clone, "Add the agent's file", map[string]string{"agent.txt": "hello\n"})
	hub.PushChange(clone, "", "work", "add the agent's file").Must(t)
	hub.CLI(fork.Token, "change", "land", demo.Slug, "1").Must(t)
	harness.Eventually(t, "change 1 to be landed", func() bool {
		return hub.Call(http.MethodGet, "/api/v1/workspaces/demo/changes/1", alice.Key, nil).Body["state"] == "landed"
	})

	// Git runs in its own process group, so a child left running would
	// outlive the hub: look for one while the hub still runs.
	harness.Eventually(t, "the hub's git children to exit", func() bool { return len(hub.Children()) == 0 })
	hub.Stop()

	assertEntries(t, "the binary's directory", hub.WorkDir, "parleyd")
	assertEntries(t, "HOME", hub.Home)
	assertEntries(t, "TMPDIR", hub.TmpDir)
	// The admin token file is read and removed before the start: a
	// bootstrapped hub refuses to start while it exists.
	assertEntries(t, "the configuration directory", hub.ConfigDir, "config.toml")
	if data, err := os.ReadDir(hub.DataDir); err != nil || len(data) == 0 {
		t.Errorf("the data directory holds %d entries (%v), want the hub's state", len(data), err)
	}
}

// assertEntries fails t unless dir holds exactly the entries named.
func assertEntries(t *testing.T, what, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", what, err)
	}
	got := []string{}
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s holds %q, want %q", what, got, want)
	}
}

func TestHarness_GatedScenariosSkipWithAStatedReason(t *testing.T) {
	if harness.IsChild() {
		t.Skip("runs in the parent test process only")
	}
	t.Parallel()
	suite := filepath.Join(t.TempDir(), "acceptance.test")
	if out, err := exec.Command("go", "test", "-c", "-o", suite, ".").CombinedOutput(); err != nil {
		t.Fatalf("go test -c: %v\n%s", err, out)
	}
	run := func(pattern string, env []string) (int, string) {
		cmd := exec.Command(suite, "-test.run", pattern, "-test.v", "-test.count=1")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("run the suite: %v", err)
		}
		return code, string(out)
	}

	code, out := run("^Test(Sandbox|Long|LargeRepo|Performance)_", harness.ChildEnv(t))
	if code != 0 {
		t.Errorf("the gated scenarios exited %d, want 0:\n%s", code, out)
	}
	results := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "--- ") {
			continue
		}
		if !strings.HasPrefix(line, "--- SKIP: ") {
			t.Errorf("result line %q, want a skip", line)
			continue
		}
		name := strings.Fields(strings.TrimPrefix(line, "--- SKIP: "))[0]
		results[name[:strings.Index(name, "_")+1]]++
	}
	for prefix, gate := range map[string]harness.Gate{
		"TestSandbox_": harness.Gateway, "TestLong_": harness.Long,
		"TestLargeRepo_": harness.LargeRepo, "TestPerformance_": harness.ReferenceHost,
	} {
		if results[prefix] == 0 {
			t.Errorf("no %s scenario was reported skipped:\n%s", prefix, out)
		}
		if !strings.Contains(out, gate.Var) {
			t.Errorf("no skip reason names %s:\n%s", gate.Var, out)
		}
	}
	if strings.Contains(out, "--- PASS") || strings.Contains(out, "--- FAIL") {
		t.Errorf("a gated scenario passed or failed:\n%s", out)
	}

	// A set gateway variable never skips: a gateway nobody answers at fails
	// the scenario with the hub's start error.
	code, out = run("^TestSandbox_ForkWithSandbox", harness.ChildEnv(t, harness.Gateway.Var+"=http://127.0.0.1:1"))
	if code == 0 || !strings.Contains(out, "--- FAIL") || !strings.Contains(out, "sandbox.gateway_url") {
		t.Errorf("with an unreachable gateway the sandbox scenario exited %d, want a failure naming sandbox.gateway_url:\n%s", code, out)
	}
}
