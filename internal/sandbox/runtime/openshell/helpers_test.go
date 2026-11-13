package openshell_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell/openshelltest"
)

// loadSettings writes config.toml with [server].external_url, the [sandbox]
// section keys (TOML lines) and one profile into a new config dir, next to
// files (name to content), loads it with the sandbox section registered and
// returns the settings. It fails the test on any error.
func loadSettings(t *testing.T, keys string, files map[string]string) *sandbox.Settings {
	t.Helper()
	dir := t.TempDir()
	toml := "[server]\nexternal_url = \"https://hub.test\"\n[sandbox]\n" + keys +
		"\n[[sandbox.profiles]]\nname = \"agent\"\nttl = \"4h\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := config.NewRegistry()
	if err := sandbox.Register(reg); err != nil {
		t.Fatal(err)
	}
	cfg, err := reg.Load(config.MapEnv(nil), dir)
	if err != nil {
		t.Fatalf("loading %s: %v", toml, err)
	}
	s, err := sandbox.FromConfig(cfg)
	if err != nil {
		t.Fatalf("checking %s: %v", toml, err)
	}
	return s
}

// gateway is the test gateway with sub-clients a test may replace. The
// replacements answer only while the gateway is reachable.
type gateway struct {
	*openshelltest.Gateway

	// sandboxes, exec and files replace the gateway's sub-clients when they
	// are set.
	sandboxes v1.SandboxInterface
	exec      v1.ExecInterface
	files     v1.FileInterface
	// noProfiles leaves the provider profiles to the SDK's in-memory client,
	// which does not implement them.
	noProfiles bool
}

func newGateway(opts ...fake.ClientOption) *gateway {
	return &gateway{Gateway: openshelltest.New(opts...)}
}

func (g *gateway) Sandboxes() v1.SandboxInterface {
	if g.sandboxes != nil && !g.Unreachable() {
		return g.sandboxes
	}
	return g.Gateway.Sandboxes()
}

func (g *gateway) Exec() v1.ExecInterface {
	if g.exec != nil && !g.Unreachable() {
		return g.exec
	}
	return g.Gateway.Exec()
}

func (g *gateway) Files() v1.FileInterface {
	if g.files != nil && !g.Unreachable() {
		return g.files
	}
	return g.Gateway.Files()
}

func (g *gateway) Providers() v1.ProviderInterface {
	if g.noProfiles && !g.Unreachable() {
		return g.Fake().Providers()
	}
	return g.Gateway.Providers()
}

// sandboxesStub answers Get and Create with its functions when they are
// set, and every other call with the sub-client it embeds.
type sandboxesStub struct {
	v1.SandboxInterface
	get    func(ctx context.Context, workspace, name string) (*v1.Sandbox, error)
	create func(ctx context.Context, workspace, name string, spec *v1.SandboxSpec) (*v1.Sandbox, error)
}

func (s sandboxesStub) Get(ctx context.Context, workspace, name string) (*v1.Sandbox, error) {
	if s.get != nil {
		return s.get(ctx, workspace, name)
	}
	return s.SandboxInterface.Get(ctx, workspace, name)
}

func (s sandboxesStub) Create(ctx context.Context, workspace, name string, spec *v1.SandboxSpec, opts ...v1.CreateOption) (*v1.Sandbox, error) {
	if s.create != nil {
		return s.create(ctx, workspace, name, spec)
	}
	return s.SandboxInterface.Create(ctx, workspace, name, spec, opts...)
}

// execStub answers Stream with its function.
type execStub struct {
	v1.ExecInterface
	stream func(ctx context.Context, workspace, sandbox string, command []string) (v1.ExecStream, error)
}

func (e execStub) Stream(ctx context.Context, workspace, sandbox string, command []string, _ ...v1.ExecOptions) (v1.ExecStream, error) {
	return e.stream(ctx, workspace, sandbox, command)
}

// filesStub keeps uploaded files in memory by workspace, sandbox and path,
// with the mode of the local file they were uploaded from, and remembers
// the local paths it was given.
type filesStub struct {
	mu     sync.Mutex
	files  map[string][]byte
	modes  map[string]os.FileMode
	locals []string
}

func newFilesStub() *filesStub {
	return &filesStub{files: map[string][]byte{}, modes: map[string]os.FileMode{}}
}

func (f *filesStub) Upload(_ context.Context, workspace, sandbox, local, remote string) error {
	data, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	fi, err := os.Stat(local)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[workspace+"/"+sandbox+remote] = data
	f.modes[workspace+"/"+sandbox+remote] = fi.Mode().Perm()
	f.locals = append(f.locals, local)
	return nil
}

func (f *filesStub) Download(_ context.Context, workspace, sandbox, remote, local string) error {
	f.mu.Lock()
	data, ok := f.files[workspace+"/"+sandbox+remote]
	f.locals = append(f.locals, local)
	f.mu.Unlock()
	if !ok {
		return &v1.StatusError{Code: v1.ErrorNotFound, Message: remote + " not found"}
	}
	return os.WriteFile(local, data, 0o600)
}

// scriptedStream is an exec stream that delivers its chunks, then ends with
// its exit code.
type scriptedStream struct {
	chunks []v1.ExecChunk
	code   int
}

func (s *scriptedStream) Next() (*v1.ExecChunk, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	ch := s.chunks[0]
	s.chunks = s.chunks[1:]
	return &ch, nil
}

func (s *scriptedStream) ExitCode() (int, error) { return s.code, nil }
func (s *scriptedStream) Close() error           { return nil }
