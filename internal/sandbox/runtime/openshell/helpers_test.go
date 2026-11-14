package openshell_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
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

	// sandboxes, exec, files and policy replace the gateway's sub-clients
	// when they are set.
	sandboxes v1.SandboxInterface
	exec      v1.ExecInterface
	files     v1.FileInterface
	policy    v1.PolicyInterface
}

func newGateway(opts ...fake.ClientOption) *gateway {
	return &gateway{Gateway: openshelltest.New(opts...)}
}

// newContractGateway is a test gateway that does what the SDK's in-memory
// client does not: it runs commands as processes of the host, keeps files
// and policy drafts, and holds the provider profile anthropic, with one
// credential, in the workspace parley, as a gateway that ships it does. The
// drafts it returns add the proposals.
func newContractGateway(t *testing.T) (*gateway, *drafts) {
	g := newGateway()
	g.exec = processExec{sandboxes: g.Fake().Sandboxes(), dir: t.TempDir()}
	g.files = newFilesStub()
	d := &drafts{PolicyInterface: g.Fake().Policy(), sandboxes: g.Fake().Sandboxes(), chunks: map[string][]v1.PolicyChunk{}}
	g.policy = d
	g.AddProfile("parley", v1.ProviderProfile{ID: "anthropic", Credentials: []v1.ProfileCredential{{Name: "api_key", EnvVars: []string{"ANTHROPIC_API_KEY"}}}})
	return g, d
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

func (g *gateway) Policy() v1.PolicyInterface {
	if g.policy != nil && !g.Unreachable() {
		return g.policy
	}
	return g.Gateway.Policy()
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

// processExec runs the commands of the sandboxes the SDK's in-memory client
// holds as processes of the host, in dir, with the environment of the
// options added to the test's. A stream delivers the output once the
// command has ended, stdout before stderr.
type processExec struct {
	sandboxes v1.SandboxInterface
	dir       string
}

func (e processExec) command(ctx context.Context, workspace, sandbox string, command []string, opts []v1.ExecOptions) (*exec.Cmd, error) {
	if _, err := e.sandboxes.Get(ctx, workspace, sandbox); err != nil {
		return nil, err
	}
	if len(command) == 0 {
		return nil, &v1.StatusError{Code: v1.ErrorInvalidArgument, Message: "no command"}
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = e.dir
	cmd.Env = os.Environ()
	for _, o := range opts {
		for k, v := range o.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	return cmd, nil
}

// exitCode is the exit code of a command that ended with err, and the error
// of one that could not run.
func exitCode(err error) (int, error) {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}

func (e processExec) Run(ctx context.Context, workspace, sandbox string, command []string, opts ...v1.ExecOptions) (*v1.ExecResult, error) {
	cmd, err := e.command(ctx, workspace, sandbox, command, opts)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code, err := exitCode(cmd.Run())
	if err != nil {
		return nil, err
	}
	return &v1.ExecResult{ExitCode: code, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

func (e processExec) Stream(ctx context.Context, workspace, sandbox string, command []string, opts ...v1.ExecOptions) (v1.ExecStream, error) {
	r, err := e.Run(ctx, workspace, sandbox, command, opts...)
	if err != nil {
		return nil, err
	}
	s := &scriptedStream{code: r.ExitCode}
	if len(r.Stdout) > 0 {
		s.chunks = append(s.chunks, v1.ExecChunk{Stream: v1.StreamStdout, Data: r.Stdout})
	}
	if len(r.Stderr) > 0 {
		s.chunks = append(s.chunks, v1.ExecChunk{Stream: v1.StreamStderr, Data: r.Stderr})
	}
	return s, nil
}

// Interactive runs the command with its input from Write and its stdout and
// stderr to Read; the terminal size is not used.
func (e processExec) Interactive(ctx context.Context, workspace, sandbox string, command []string, _, _ uint32, opts ...v1.ExecOptions) (v1.InteractiveSession, error) {
	cmd, err := e.command(ctx, workspace, sandbox, command, opts)
	if err != nil {
		return nil, err
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		_ = out.Close()
		return nil, err
	}
	return &processSession{cmd: cmd, in: in, out: out}, nil
}

type processSession struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *os.File
	once sync.Once
	code int
	err  error
}

func (s *processSession) Read(p []byte) (int, error)  { return s.out.Read(p) }
func (s *processSession) Write(p []byte) (int, error) { return s.in.Write(p) }
func (s *processSession) Resize(uint32, uint32) error { return nil }

// ExitCode ends the input and waits for the command.
func (s *processSession) ExitCode() (int, error) {
	s.once.Do(func() {
		_ = s.in.Close()
		s.code, s.err = exitCode(s.cmd.Wait())
	})
	return s.code, s.err
}

func (s *processSession) Close() error {
	_ = s.in.Close()
	_ = s.cmd.Process.Kill()
	_, _ = s.ExitCode()
	return s.out.Close()
}

// drafts keeps the policy draft chunks of the sandboxes the SDK's in-memory
// client holds, by workspace and sandbox; the rest of the policy interface
// is the in-memory client's. GetDraft returns every chunk whatever its
// status.
type drafts struct {
	v1.PolicyInterface
	sandboxes v1.SandboxInterface

	mu     sync.Mutex
	chunks map[string][]v1.PolicyChunk // by workspace + "/" + sandbox
	last   int
}

// add adds a pending chunk proposing p's rule to p's sandbox, with p's
// summary as its rationale and p's findings as its security notes, and
// returns its id.
func (d *drafts) add(workspace string, p runtime.Proposal) string {
	var rule v1.NetworkPolicyRule
	if err := json.Unmarshal(p.Rule.Body, &rule); err != nil {
		panic("proposal rule " + p.Rule.Name + ": " + err.Error())
	}
	rule.Name = p.Rule.Name
	notes := make([]string, len(p.Findings))
	for i, f := range p.Findings {
		notes[i] = f.Message
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.last++
	id := strconv.Itoa(d.last)
	key := workspace + "/" + p.Sandbox
	d.chunks[key] = append(d.chunks[key], v1.PolicyChunk{
		ID:            id,
		Status:        "pending",
		RuleName:      p.Rule.Name,
		ProposedRule:  &rule,
		Rationale:     p.Summary,
		SecurityNotes: strings.Join(notes, "; "),
		ReviewToken:   "review-" + id,
	})
	return id
}

func (d *drafts) GetDraft(ctx context.Context, workspace, sandbox string, _ ...v1.GetDraftOption) (*v1.DraftPolicy, error) {
	if _, err := d.sandboxes.Get(ctx, workspace, sandbox); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return &v1.DraftPolicy{Chunks: slices.Clone(d.chunks[workspace+"/"+sandbox])}, nil
}

// decide gives a pending chunk its decision.
func (d *drafts) decide(workspace, sandbox, id string, decide func(*v1.PolicyChunk) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, ch := range d.chunks[workspace+"/"+sandbox] {
		if ch.ID != id {
			continue
		}
		if ch.Status != "pending" {
			return &v1.StatusError{Code: v1.ErrorConflict, Message: "chunk " + id + " is " + ch.Status}
		}
		return decide(&d.chunks[workspace+"/"+sandbox][i])
	}
	return &v1.StatusError{Code: v1.ErrorNotFound, Message: "chunk " + id + " not found"}
}

func (d *drafts) ApproveDraftChunk(_ context.Context, workspace, sandbox, id, token string) (*v1.ApproveResult, error) {
	err := d.decide(workspace, sandbox, id, func(ch *v1.PolicyChunk) error {
		if token != ch.ReviewToken {
			return &v1.StatusError{Code: v1.ErrorConflict, Message: "stale review token"}
		}
		ch.Status = "approved"
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &v1.ApproveResult{}, nil
}

func (d *drafts) RejectDraftChunk(_ context.Context, workspace, sandbox, id, reason string) error {
	return d.decide(workspace, sandbox, id, func(ch *v1.PolicyChunk) error {
		ch.Status, ch.RejectionReason = "rejected", reason
		return nil
	})
}
