// Package runtimetest holds the in-memory runtime, Fake, the only test
// double of the sandbox runtime in the repository, and RunContract, the
// contract tests both runtime clients pass. It is imported by test code
// only.
package runtimetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// Fake is an in-memory sandbox runtime. Its clients (Client) act in one
// workspace each, and it keeps per workspace the provider profiles, the
// providers with their labels and values, the sandboxes with their status,
// the policy values it received, files, log entries and proposals.
//
// A sandbox's files are held in memory, or with WithHostDirs in a directory
// of the host in which Exec and RunMain run real commands. Otherwise
// commands are scripted (SetExec): a command's result is the script's, and
// without a script a command exits 0 with no output, and a main process runs
// until EndMain ends it. A terminal echoes its input.
//
// The other methods are the test's controls and its view of the runtime;
// each control that names a sandbox reports an error to the test when the
// sandbox does not exist. Everything stops when the test ends. A Fake is
// safe for concurrent use.
type Fake struct {
	t        testing.TB
	now      func() time.Time
	hostRoot string
	bg       context.Context // the context of the main processes
	stop     context.CancelFunc
	wg       sync.WaitGroup

	mu           sync.Mutex
	closed       bool
	unreachable  bool
	failures     map[string]failure
	release      runtime.Release
	reject       func(runtime.Policy) bool
	rejectReason string
	validations  map[string]runtime.PolicyValidation
	script       ExecFunc
	spaces       map[string]*space
	calls        []Call
	streams      map[int]*tracked
	nextStream   int
	nextProposal int
	changed      chan struct{} // closed and replaced on every sandbox change
}

// ExecFunc runs a command of a sandbox in place of the runtime: it writes
// the command's output to stdout and stderr as the command would and
// returns its exit code. req.Env holds the sandbox's environment and the
// request's, and req.WorkDir is the sandbox's working directory when the
// request names none. It returns when ctx ends.
type ExecFunc func(ctx context.Context, sandbox string, req runtime.ExecRequest, stdout, stderr io.Writer) int

// Option configures a Fake.
type Option func(*Fake)

// WithHostDirs keeps every sandbox's files in a directory of the host
// (HostDir), in which Exec and RunMain run real commands unless a script is
// set. A sandbox path /a/b is the file a/b of that directory.
func WithHostDirs() Option {
	return func(f *Fake) { f.hostRoot = f.t.TempDir() }
}

// WithClock sets the clock that stamps log entries; the default is
// time.Now.
func WithClock(now func() time.Time) Option {
	return func(f *Fake) { f.now = now }
}

// Call is an operation a client of the fake received.
type Call struct {
	// Op is the operation's name, one of the runtime.Op… constants.
	Op        string
	Workspace string
	// Target is the sandbox an operation on a sandbox names, the provider
	// of EnsureProvider and DeleteProvider, the provider profile of
	// GetProviderProfile and InstallProviderProfile, and "" otherwise.
	Target string
}

// Stream is an open stream: a watch, a followed log, an exec stream or a
// terminal.
type Stream struct {
	ID        int
	Op        string
	Workspace string
	// Sandbox is "" for a watch.
	Sandbox string
}

type failure struct {
	kind error
	text string
}

type space struct {
	profiles  map[string]runtime.ProviderProfile
	providers map[string]runtime.Provider
	sandboxes map[string]*sandbox
	proposals []runtime.Proposal
}

type sandbox struct {
	spec       runtime.SandboxSpec
	state      runtime.State
	env        map[string]string
	policy     runtime.Policy
	revision   uint64
	providers  []string
	mainExited bool
	exitCode   int
	mainRun    int // which run of the main process may report its end
	mainStop   context.CancelFunc
	workDir    bool
	files      map[string]*file // in memory
	host       string           // with WithHostDirs
	logs       []runtime.LogEntry
}

// New returns an empty runtime that reports a healthy gateway of release
// 0.1.8. Its processes and open streams end when the test ends.
func New(t testing.TB, opts ...Option) *Fake {
	f := &Fake{
		t:           t,
		now:         time.Now,
		failures:    map[string]failure{},
		release:     runtime.Release{Healthy: true, Version: "0.1.8"},
		validations: map[string]runtime.PolicyValidation{},
		spaces:      map[string]*space{},
		streams:     map[int]*tracked{},
		changed:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(f)
	}
	f.bg, f.stop = context.WithCancel(context.Background())
	t.Cleanup(f.shutdown)
	return f
}

// Client returns a client of the fake that acts in workspace and reports
// its reachability to rec; nil reports it nowhere.
func (f *Fake) Client(workspace string, rec runtime.Recorder) runtime.Client {
	return &client{f: f, ws: workspace, reach: runtime.NewReachability(rec)}
}

// Backend returns the fake as the contract tests take it.
func (f *Fake) Backend() Backend {
	return Backend{
		Client:         f.Client,
		SetUnreachable: f.SetUnreachable,
		RejectPolicies: f.RejectPolicies,
		AddProposal:    f.AddProposal,
	}
}

// SetUnreachable makes every later call fail with an error of kind
// ErrUnavailable, as when the gateway cannot be reached (true), or ends
// that (false).
func (f *Fake) SetUnreachable(unreachable bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unreachable = unreachable
}

// FailOperation makes every later call of the operation op (a runtime.Op…
// name) fail with an error of kind and text, until a call with kind nil
// ends it.
func (f *Fake) FailOperation(op string, kind error, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if kind == nil {
		delete(f.failures, op)
		return
	}
	f.failures[op] = failure{kind: kind, text: text}
}

// SetRelease sets what Release reports.
func (f *Fake) SetRelease(r runtime.Release) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.release = r
}

// AddProviderProfile puts a provider profile in a workspace, as one the
// runtime knows.
func (f *Fake) AddProviderProfile(workspace string, p runtime.ProviderProfile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.space(workspace).profiles[p.ID] = cloneProfile(p)
}

// RejectPolicies makes the runtime refuse every policy value pred matches,
// with reason, in ValidatePolicy, CreateSandbox, StartSandbox and
// AddPolicyRule (the policy with the rule added); nil pred ends it.
func (f *Fake) RejectPolicies(pred func(runtime.Policy) bool, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reject, f.rejectReason = pred, reason
}

// SetPolicyValidation sets what ValidatePolicy reports for a policy value
// with the base document base: the hosts it allows and the providers it
// refers to. Another base document reports nothing.
func (f *Fake) SetPolicyValidation(base []byte, v runtime.PolicyValidation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.validations[string(base)] = runtime.PolicyValidation{Hosts: slices.Clone(v.Hosts), Providers: slices.Clone(v.Providers)}
}

// SetExec makes fn run every later command: those of Exec, ExecStream and
// RunMain. fn is called from other goroutines.
func (f *Fake) SetExec(fn ExecFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = fn
}

// SetState changes the state of a sandbox.
func (f *Fake) SetState(workspace, name string, state runtime.State) {
	f.change(workspace, name, func(sb *sandbox) { sb.state = state })
}

// EndMain ends the main process of a sandbox with an exit code.
func (f *Fake) EndMain(workspace, name string, code int) {
	f.change(workspace, name, func(sb *sandbox) {
		sb.stopMain()
		sb.mainExited, sb.exitCode = true, code
	})
}

// DropWorkDir makes the runtime lose a sandbox's working directory and the
// files in it.
func (f *Fake) DropWorkDir(workspace, name string) {
	f.change(workspace, name, func(sb *sandbox) {
		sb.workDir = false
		f.removeTree(sb, workDirOf(sb))
	})
}

// EmitOutput adds a line of the main process's output to a sandbox's log.
func (f *Fake) EmitOutput(workspace, name string, stream runtime.OutputStream, text string) {
	f.emit(workspace, name, runtime.LogEntry{Source: runtime.SourceProcess, Stream: stream, Text: text}, true)
}

// EmitPolicyLog adds an entry to a sandbox's policy log; its Source is set
// to policy and a zero Time to the clock's.
func (f *Fake) EmitPolicyLog(workspace, name string, e runtime.LogEntry) {
	e.Source = runtime.SourcePolicy
	f.emit(workspace, name, e, true)
}

// AddProposal adds a pending proposal to the sandbox p.Sandbox of a
// workspace and returns its id, the one p carries or else a new one.
func (f *Fake) AddProposal(workspace string, p runtime.Proposal) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.space(workspace)
	if s.sandboxes[p.Sandbox] == nil {
		f.t.Errorf("runtimetest: AddProposal: no sandbox %q in workspace %q", p.Sandbox, workspace)
		return ""
	}
	if p.ID == "" {
		f.nextProposal++
		p.ID = strconv.Itoa(f.nextProposal)
	}
	s.proposals = append(s.proposals, cloneProposal(p))
	return p.ID
}

// BreakStream breaks an open stream: its reader gets an error of kind
// ErrUnavailable.
func (f *Fake) BreakStream(id int) {
	f.mu.Lock()
	tr := f.streams[id]
	f.mu.Unlock()
	if tr == nil {
		f.t.Errorf("runtimetest: BreakStream: no open stream %d", id)
		return
	}
	tr.end(broken(tr.info.Op))
}

// Calls returns the calls the fake's clients received, in order; Reachable
// is not a call.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Streams returns the open streams in the order they were opened.
func (f *Fake) Streams() []Stream {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Stream, 0, len(f.streams))
	for _, tr := range f.streams {
		out = append(out, tr.info)
	}
	slices.SortFunc(out, func(a, b Stream) int { return a.ID - b.ID })
	return out
}

// HostDir returns the host directory that holds a sandbox's files with
// WithHostDirs, and "" without.
func (f *Fake) HostDir(workspace, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb := f.space(workspace).sandboxes[name]; sb != nil {
		return sb.host
	}
	f.t.Errorf("runtimetest: HostDir: no sandbox %q in workspace %q", name, workspace)
	return ""
}

// Spec returns the spec a sandbox was created with.
func (f *Fake) Spec(workspace, name string) (runtime.SandboxSpec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.space(workspace).sandboxes[name]
	if sb == nil {
		return runtime.SandboxSpec{}, false
	}
	return cloneSpec(sb.spec), true
}

// Provider returns a provider of a workspace, its value included.
func (f *Fake) Provider(workspace, name string) (runtime.Provider, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.space(workspace).providers[name]
	return cloneProvider(p), ok
}

// shutdown ends the main processes and the open streams and waits for what
// runs them.
func (f *Fake) shutdown() {
	f.mu.Lock()
	f.closed = true
	open := slices.Collect(maps.Values(f.streams))
	f.mu.Unlock()
	f.stop()
	for _, tr := range open {
		tr.end(io.EOF)
	}
	f.wg.Wait()
}

// space returns a workspace's state, made on first use. The caller holds
// f.mu.
func (f *Fake) space(ws string) *space {
	s := f.spaces[ws]
	if s == nil {
		s = &space{
			profiles:  map[string]runtime.ProviderProfile{},
			providers: map[string]runtime.Provider{},
			sandboxes: map[string]*sandbox{},
		}
		f.spaces[ws] = s
	}
	return s
}

// change applies a control's change to a sandbox and publishes it.
func (f *Fake) change(ws, name string, fn func(sb *sandbox)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.space(ws).sandboxes[name]
	if sb == nil {
		f.t.Errorf("runtimetest: no sandbox %q in workspace %q", name, ws)
		return
	}
	fn(sb)
	f.publish(ws, sb, false)
}

// publish tells the watches of a workspace and the waiters that a sandbox
// changed. The caller holds f.mu.
func (f *Fake) publish(ws string, sb *sandbox, deleted bool) {
	ev := runtime.SandboxEvent{Sandbox: sb.status(), Deleted: deleted}
	for _, tr := range f.streams {
		if tr.watch != nil && tr.info.Workspace == ws {
			tr.watch.push(ev)
		}
	}
	close(f.changed)
	f.changed = make(chan struct{})
}

// emit adds a log entry to a sandbox and delivers it to the logs that
// follow it. A control reports a missing sandbox; a main process that
// outlived its sandbox does not.
func (f *Fake) emit(ws, name string, e runtime.LogEntry, control bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.space(ws).sandboxes[name]
	if sb == nil {
		if control {
			f.t.Errorf("runtimetest: no sandbox %q in workspace %q", name, ws)
		}
		return
	}
	if e.Time.IsZero() {
		e.Time = f.now()
	}
	sb.logs = append(sb.logs, e)
	for _, tr := range f.streams {
		if tr.follow != nil && tr.info.Workspace == ws && tr.info.Sandbox == name && (tr.source == "" || tr.source == e.Source) {
			tr.follow.push(e)
		}
	}
}

// rejected returns the error of a policy value the runtime refuses, or nil.
// The caller holds f.mu.
func (f *Fake) rejected(p runtime.Policy) error {
	if f.reject != nil && f.reject(p) {
		return invalid("policy rejected: %s", f.rejectReason)
	}
	return nil
}

// startMain starts the main process of a sandbox. With neither a script nor
// a host directory it runs until EndMain. The caller holds f.mu.
func (f *Fake) startMain(ws, name string, sb *sandbox, command []string) {
	sb.stopMain()
	sb.mainExited, sb.exitCode = false, 0
	if (f.script == nil && sb.host == "") || f.closed {
		return
	}
	run := sb.mainRun
	ctx, cancel := context.WithCancel(f.bg)
	sb.mainStop = cancel
	req := sb.request(runtime.ExecRequest{Command: command})
	script, host := f.script, sb.host
	f.wg.Go(func() {
		defer cancel()
		stdout := &lineWriter{emit: func(text string) {
			f.emit(ws, name, runtime.LogEntry{Source: runtime.SourceProcess, Stream: runtime.Stdout, Text: text}, false)
		}}
		stderr := &lineWriter{emit: func(text string) {
			f.emit(ws, name, runtime.LogEntry{Source: runtime.SourceProcess, Stream: runtime.Stderr, Text: text}, false)
		}}
		code := runCommand(ctx, script, name, host, req, stdout, stderr)
		stdout.flush()
		stderr.flush()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.space(ws).sandboxes[name] != sb || sb.mainRun != run || ctx.Err() != nil {
			return
		}
		sb.mainExited, sb.exitCode = true, code
		f.publish(ws, sb, false)
	})
}

// stopMain stops the running main process, whose end is then not reported.
func (sb *sandbox) stopMain() {
	sb.mainRun++
	if sb.mainStop != nil {
		sb.mainStop()
		sb.mainStop = nil
	}
}

func (sb *sandbox) status() runtime.SandboxStatus {
	return runtime.SandboxStatus{
		Name:           sb.spec.Name,
		Labels:         maps.Clone(sb.spec.Labels),
		State:          sb.state,
		MainExited:     sb.mainExited,
		ExitCode:       sb.exitCode,
		WorkDirPresent: sb.workDir,
	}
}

// request completes a command's request with the sandbox's environment and
// working directory.
func (sb *sandbox) request(req runtime.ExecRequest) runtime.ExecRequest {
	env := maps.Clone(sb.env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, req.Env)
	req.Env = env
	if req.WorkDir == "" {
		req.WorkDir = workDirOf(sb)
	}
	req.Command = slices.Clone(req.Command)
	req.Stdin = bytes.Clone(req.Stdin)
	return req
}

func workDirOf(sb *sandbox) string {
	if sb.spec.WorkDir == "" {
		return "/"
	}
	return sb.spec.WorkDir
}

// runCommand runs a command with the script, or for real in the host
// directory, or as a command that exits 0 with no output, and returns its
// exit code.
func runCommand(ctx context.Context, script ExecFunc, name, host string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
	switch {
	case script != nil:
		return script(ctx, name, req, stdout, stderr)
	case host == "":
		return 0
	}
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = hostPath(host, req.WorkDir)
	cmd.Env = os.Environ()
	for _, k := range slices.Sorted(maps.Keys(req.Env)) {
		cmd.Env = append(cmd.Env, k+"="+req.Env[k])
	}
	cmd.Stdin = bytes.NewReader(req.Stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// A child the command leaves behind may hold its output open; the
	// command's end does not wait for it.
	cmd.WaitDelay = 100 * time.Millisecond
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return exit.ExitCode()
	case err != nil:
		_, _ = fmt.Fprintln(stderr, err)
		return 127
	}
	return 0
}

// lineWriter turns what a main process writes into log lines.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	emit func(text string)
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}

// hostPath is the host path of a sandbox path.
func hostPath(host, p string) string {
	return filepath.Join(host, filepath.FromSlash(cleanPath(p)))
}

func notFound(format string, args ...any) error {
	return runtime.NewError(runtime.ErrNotFound, "", fmt.Sprintf(format, args...))
}

func invalid(format string, args ...any) error {
	return runtime.NewError(runtime.ErrInvalid, "", fmt.Sprintf(format, args...))
}

func cloneProfile(p runtime.ProviderProfile) runtime.ProviderProfile {
	p.Variables = slices.Clone(p.Variables)
	p.Programs = slices.Clone(p.Programs)
	if p.Endpoints != nil {
		eps := make([]runtime.Endpoint, len(p.Endpoints))
		for i, e := range p.Endpoints {
			e.Paths = slices.Clone(e.Paths)
			eps[i] = e
		}
		p.Endpoints = eps
	}
	return p
}

func cloneProvider(p runtime.Provider) runtime.Provider {
	p.Labels = maps.Clone(p.Labels)
	return p
}

func cloneRules(rules []runtime.Rule) []runtime.Rule {
	if rules == nil {
		return nil
	}
	out := make([]runtime.Rule, len(rules))
	for i, r := range rules {
		r.Body = bytes.Clone(r.Body)
		out[i] = r
	}
	return out
}

func clonePolicy(p runtime.Policy) runtime.Policy {
	return runtime.Policy{Base: bytes.Clone(p.Base), Protected: cloneRules(p.Protected), Added: cloneRules(p.Added)}
}

func cloneSpec(s runtime.SandboxSpec) runtime.SandboxSpec {
	s.Env = maps.Clone(s.Env)
	s.Policy = clonePolicy(s.Policy)
	s.Providers = slices.Clone(s.Providers)
	s.Labels = maps.Clone(s.Labels)
	return s
}

func cloneProposal(p runtime.Proposal) runtime.Proposal {
	p.Rule.Body = bytes.Clone(p.Rule.Body)
	p.Findings = slices.Clone(p.Findings)
	return p
}
