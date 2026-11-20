package upgrade_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/test/upgrade"
)

// manifest is the manifest.json of a recorded archive.
type manifest struct {
	Release string `json:"release"`
	Steps   struct {
		Transactional []string `json:"transactional"`
		Analytical    []string `json:"analytical"`
	} `json:"steps"`
	AdminTokenSHA256 string `json:"admin_token_sha256"`
	Repositories     []struct {
		Path string            `json:"path"`
		Refs map[string]string `json:"refs"`
	} `json:"repositories"`
}

// stores are the two stores, with the name a message gives each and the
// recorded steps of each in a manifest.
var stores = []struct {
	name     string
	target   store.Target
	recorded func(m manifest) []string
}{
	{"transactional", store.Main, func(m manifest) []string { return m.Steps.Transactional }},
	{"analytical", store.Analytics, func(m manifest) []string { return m.Steps.Analytical }},
}

// The test sends SIGTERM to its own process to stop each hub, as an operator
// stops parleyd; so it does not run in parallel.
func TestUpgrade_TheCurrentHubOpensTheDataOfEveryRecordedRelease(t *testing.T) {
	archives, err := filepath.Glob(filepath.Join("testdata", "*.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) == 0 {
		t.Fatal("testdata holds no recorded archive: record one with go run ./test/upgrade/record <release>")
	}
	registered := registeredSteps(t)
	for _, archive := range archives {
		t.Run(strings.TrimSuffix(filepath.Base(archive), ".tar.gz"), func(t *testing.T) {
			d := t.TempDir()
			if out, err := exec.Command("tar", "-xzf", archive, "-C", d).CombinedOutput(); err != nil {
				t.Fatalf("tar -xzf %s: %v\n%s", archive, err, out)
			}
			m := readManifest(t, d)
			for _, s := range stores {
				if err := upgrade.Compatible(s.recorded(m), registered[s.target]); err != nil {
					t.Fatalf("the %s store of %s: %v", s.name, m.Release, err)
				}
			}

			opts := hubOptions(t, d)
			serveAndStop(t, opts)
			first, adminHash := readStores(t, d)
			for _, s := range stores {
				applied, want := first[s.target], s.recorded(m)
				if len(applied) < len(want) || !slices.Equal(applied[:len(want)], want) {
					t.Errorf("applied steps of the %s store = %v, want them to begin with the recorded %v", s.name, applied, want)
				}
				if got, want := sorted(applied), sorted(registered[s.target]); !slices.Equal(got, want) {
					t.Errorf("applied steps of the %s store = %v, want every registered step %v", s.name, got, want)
				}
			}
			if adminHash != m.AdminTokenSHA256 {
				t.Errorf("stored admin token hash = %q, want the recorded %q", adminHash, m.AdminTokenSHA256)
			}
			for _, r := range m.Repositories {
				repo := filepath.Join(d, "workspaces", r.Path)
				git(t, repo, "fsck")
				for ref, id := range r.Refs {
					if got := git(t, repo, "rev-parse", ref); got != id {
						t.Errorf("%s %s = %s, want the recorded %s", r.Path, ref, got, id)
					}
				}
			}

			serveAndStop(t, opts)
			second, _ := readStores(t, d)
			for _, s := range stores {
				if !slices.Equal(second[s.target], first[s.target]) {
					t.Errorf("a second start changed the applied steps of the %s store: %v, were %v",
						s.name, second[s.target], first[s.target])
				}
			}
		})
	}
}

// errRegistered stops the run that only reads the registered steps.
var errRegistered = errors.New("registered steps read")

// registeredSteps returns the names of the schema steps the current hub
// registers for each store. It starts the hub on empty data and stops it in
// a start hook, before it serves, since only an open store tells them.
func registeredSteps(t *testing.T) map[store.Target][]string {
	t.Helper()
	dir := t.TempDir()
	steps := map[store.Target][]string{}
	var log hubtest.LogBuffer
	code := hub.Run(context.Background(), hub.Options{
		Env:     config.MapEnv(map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir}),
		WorkDir: dir,
		Stderr:  &log,
		Compose: func(c *hub.Composition) {
			c.AddStartHook("registered-steps", func(_ context.Context, rt *hub.Runtime) error {
				for _, s := range stores {
					steps[s.target] = rt.Store.RegisteredSteps(s.target)
				}
				return errRegistered
			})
		},
	})
	if code != 1 || !strings.Contains(log.String(), errRegistered.Error()) || len(steps[store.Main]) == 0 {
		t.Fatalf("reading the registered steps: Run returned %d with %v; stderr = %q", code, steps, log.String())
	}
	return steps
}

// readManifest reads the manifest of the archive unpacked in d.
func readManifest(t *testing.T, d string) manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(d, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	return m
}

// hubOptions are the options of parleyd started with no flags on the data
// in d, with its configuration in a directory of its own.
func hubOptions(t *testing.T, d string) hub.Options {
	t.Helper()
	cfgDir := t.TempDir()
	toml := fmt.Sprintf("[server]\nbind = \"127.0.0.1\"\nport = 0\n\n[database]\npath = %q\n\n[workspace]\npath = %q\n",
		filepath.Join(d, "parley.db"), filepath.Join(d, "workspaces"))
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return hub.Options{Env: config.MapEnv(map[string]string{"XDG_CONFIG_HOME": cfgDir}), WorkDir: cfgDir}
}

// serveAndStop starts the hub, checks that it is ready, stops it with
// SIGTERM and checks that it exited with status 0.
func serveAndStop(t *testing.T, opts hub.Options) {
	t.Helper()
	s := hubtest.Serve(t, opts)
	res, err := http.Get(s.URL("/readyz"))
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200; stderr = %q", res.StatusCode, s.Log.String())
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("parleyd exited with %d after SIGTERM, want 0; stderr = %q", code, s.Log.String())
	}
}

// readStores opens the stopped hub's stores in d and returns the applied
// steps of each and the stored admin token hash.
func readStores(t *testing.T, d string) (map[store.Target][]string, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		DatabasePath:  filepath.Join(d, "parley.db"),
		WorkspaceRoot: filepath.Join(d, "workspaces"),
	}, nil)
	if err != nil {
		t.Fatalf("open the stores: %v", err)
	}
	defer st.Close()
	applied := map[store.Target][]string{}
	for _, s := range stores {
		if applied[s.target], err = st.AppliedSteps(ctx, s.target); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := identity.AdminSettings(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	return applied, admin.TokenHash
}

// git runs stock git in repo with no user or system configuration and
// returns its trimmed output, failing the test when it exits non-zero.
func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, repo, err, out)
	}
	return strings.TrimSpace(string(out))
}

func sorted(names []string) []string {
	return slices.Sorted(slices.Values(names))
}
