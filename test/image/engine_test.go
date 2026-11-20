package image_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/test/image"
)

// The build values every image of a test run is built with.
const (
	buildVersion = "9.9.9-test"
	buildCommit  = "abc1234"
	buildTime    = "2026-01-02T03:04:05Z"
)

// commandLimit bounds one engine command other than a build.
const commandLimit = 2 * time.Minute

// buildLimit bounds one image build, which may pull a base image and
// download the module's dependencies.
const buildLimit = 30 * time.Minute

func TestEngine_SkipsUnlessAskedForAndNamesAnEngineThatIsMissing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		skip bool
		// engine is the engine returned; errNames the command an error must
		// name. A row with both accepts either outcome.
		engine, errNames string
	}{
		{name: "nothing set", skip: true},
		{name: "not asked for", env: map[string]string{"PARLEY_IMAGE_TESTS": "0"}, skip: true},
		{name: "not asked for, with an engine", env: map[string]string{"PARLEY_CONTAINER_ENGINE": "sh"}, skip: true},
		{
			name:     "an engine that is not on the PATH",
			env:      map[string]string{"PARLEY_IMAGE_TESTS": "1", "PARLEY_CONTAINER_ENGINE": "no-such-engine"},
			errNames: "no-such-engine",
		},
		{
			name:   "an engine on the PATH",
			env:    map[string]string{"PARLEY_IMAGE_TESTS": "1", "PARLEY_CONTAINER_ENGINE": "sh"},
			engine: "sh",
		},
		{
			name:   "no engine named",
			env:    map[string]string{"PARLEY_IMAGE_TESTS": "1"},
			engine: "docker", errNames: "docker",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lookup := func(name string) (string, bool) {
				v, ok := tt.env[name]
				return v, ok
			}
			engine, skip, err := image.Engine(lookup)
			switch {
			case tt.skip:
				if !skip || err != nil || engine != "" {
					t.Fatalf("Engine = %q, %t, %v; want a skip", engine, skip, err)
				}
			case skip:
				t.Fatalf("Engine = %q, skip, %v; want no skip", engine, err)
			case err != nil:
				if tt.errNames == "" || !strings.Contains(err.Error(), tt.errNames) {
					t.Fatalf("Engine = error %q; want engine %q", err, tt.engine)
				}
			case tt.engine == "" || engine != tt.engine:
				t.Fatalf("Engine = %q; want engine %q or an error naming %q", engine, tt.engine, tt.errNames)
			}
		})
	}
}

// removals holds the commands that remove what the run built; TestMain runs
// them after the tests.
var (
	removalsMu sync.Mutex
	removals   []*exec.Cmd
)

func TestMain(m *testing.M) {
	code := m.Run()
	for _, cmd := range removals {
		_ = cmd.Run()
	}
	os.Exit(code)
}

// engineFor returns the container engine of the run: it skips t unless the
// environment asks for the image tests, and fails it when the engine is
// missing.
func engineFor(t *testing.T) string {
	t.Helper()
	engine, skip, err := image.Engine(os.LookupEnv)
	switch {
	case skip:
		t.Skip("the image tests run with PARLEY_IMAGE_TESTS=1")
	case err != nil:
		t.Fatal(err)
	}
	return engine
}

// result is what one engine command did.
type result struct {
	stdout, stderr string
	code           int
}

// run runs the engine with args, up to commandLimit. It fails t only when
// the command cannot run at all.
func run(t *testing.T, engine string, args ...string) result {
	t.Helper()
	return runFor(t, commandLimit, engine, args...)
}

func runFor(t *testing.T, limit time.Duration, engine string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, engine, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("%s %s: %v", engine, strings.Join(args, " "), err)
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), code: cmd.ProcessState.ExitCode()}
}

// must runs the engine with args and returns its trimmed output; it fails t
// unless the command exits with 0.
func must(t *testing.T, engine string, args ...string) string {
	t.Helper()
	r := run(t, engine, args...)
	if r.code != 0 {
		t.Fatalf("%s %s: exit code %d\n%s%s", engine, strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return strings.TrimSpace(r.stdout)
}

// builds holds one build per Dockerfile, so that each image is built once
// per test run.
var (
	buildsMu sync.Mutex
	builds   = map[string]*build{}
)

type build struct {
	once sync.Once
	ref  string
	err  error
}

// buildImage builds the image of dockerfile, a path from the repository
// root, with the repository root as its context and the run's build values,
// once per test run, and returns its reference. The image is removed after
// the tests.
func buildImage(t *testing.T, engine, dockerfile, name string) string {
	t.Helper()
	buildsMu.Lock()
	b, ok := builds[dockerfile]
	if !ok {
		b = &build{}
		builds[dockerfile] = b
	}
	buildsMu.Unlock()
	b.once.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			b.err = err
			return
		}
		ref := fmt.Sprintf("%s:test-%d", name, os.Getpid())
		start := time.Now()
		r := runFor(t, buildLimit, engine, "build",
			"-f", filepath.Join(root, dockerfile),
			"--build-arg", "VERSION="+buildVersion,
			"--build-arg", "COMMIT="+buildCommit,
			"--build-arg", "BUILD_TIME="+buildTime,
			"-t", ref, root)
		if r.code != 0 {
			b.err = fmt.Errorf("%s build -f %s: exit code %d\n%s", engine, dockerfile, r.code, tail(r.stdout+r.stderr, 40))
			return
		}
		t.Logf("built %s from %s in %s", ref, dockerfile, time.Since(start).Round(time.Second))
		b.ref = ref
		removalsMu.Lock()
		removals = append(removals, exec.Command(engine, "rmi", "-f", ref))
		removalsMu.Unlock()
	})
	if b.err != nil {
		t.Fatal(b.err)
	}
	return b.ref
}

// volume creates an empty named volume that is removed when t ends.
func volume(t *testing.T, engine string) string {
	t.Helper()
	name := fmt.Sprintf("parley-image-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	must(t, engine, "volume", "create", name)
	t.Cleanup(func() { run(t, engine, "volume", "rm", "-f", name) })
	return name
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
