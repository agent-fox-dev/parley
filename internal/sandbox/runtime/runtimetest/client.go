package runtimetest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// client is a client of the fake that acts in one workspace.
type client struct {
	f     *Fake
	ws    string
	reach *runtime.Reachability
}

var _ runtime.Client = (*client)(nil)

// begin records a call and answers for an unreachable runtime, a failing
// operation and an ended context.
func (c *client) begin(ctx context.Context, op, target string) error {
	f := c.f
	f.mu.Lock()
	f.calls = append(f.calls, Call{Op: op, Workspace: c.ws, Target: target})
	unreachable, closed := f.unreachable, f.closed
	fail, failing := f.failures[op]
	f.mu.Unlock()
	switch {
	case unreachable:
		return runtime.NewError(runtime.ErrUnavailable, op, "connection refused")
	case closed:
		return runtime.NewError(runtime.ErrUnavailable, op, "the runtime has stopped")
	case failing:
		return runtime.NewError(fail.kind, op, fail.text)
	case ctx.Err() != nil:
		return runtime.NewError(runtime.ErrUnavailable, op, ctx.Err().Error())
	}
	return nil
}

// end names the operation in its error and moves the reachability flag.
func (c *client) end(op string, err error) error {
	if err != nil {
		var e *runtime.Error
		if errors.As(err, &e) {
			if e.Op == "" {
				e.Op = op
			}
		} else {
			err = runtime.NewError(runtime.ErrUnavailable, op, err.Error())
		}
	}
	return c.reach.Observe(err)
}

// do runs an operation that only reads or changes the fake's state.
func (c *client) do(ctx context.Context, op, target string, fn func(s *space) error) error {
	err := c.begin(ctx, op, target)
	if err == nil {
		c.f.mu.Lock()
		err = fn(c.f.space(c.ws))
		c.f.mu.Unlock()
	}
	return c.end(op, err)
}

func (s *space) sandbox(name string) (*sandbox, error) {
	sb := s.sandboxes[name]
	if sb == nil {
		return nil, notFound("no sandbox %q", name)
	}
	return sb, nil
}

func (c *client) Reachable() bool { return c.reach.Reachable() }

func (c *client) Release(ctx context.Context) (runtime.Release, error) {
	var r runtime.Release
	err := c.do(ctx, runtime.OpRelease, "", func(*space) error {
		r = c.f.release
		return nil
	})
	return r, err
}

func (c *client) EnsureWorkspace(ctx context.Context) error {
	return c.do(ctx, runtime.OpEnsureWorkspace, "", func(*space) error { return nil })
}

func (c *client) GetProviderProfile(ctx context.Context, id string) (runtime.ProviderProfile, error) {
	var p runtime.ProviderProfile
	err := c.do(ctx, runtime.OpGetProviderProfile, id, func(s *space) error {
		got, ok := s.profiles[id]
		if !ok {
			return notFound("no provider profile %q", id)
		}
		p = cloneProfile(got)
		return nil
	})
	return p, err
}

func (c *client) InstallProviderProfile(ctx context.Context, p runtime.ProviderProfile) error {
	return c.do(ctx, runtime.OpInstallProviderProfile, p.ID, func(s *space) error {
		if p.ID == "" {
			return invalid("a provider profile needs an id")
		}
		s.profiles[p.ID] = cloneProfile(p)
		return nil
	})
}

func (c *client) EnsureProvider(ctx context.Context, p runtime.Provider) error {
	return c.do(ctx, runtime.OpEnsureProvider, p.Name, func(s *space) error {
		if p.Name == "" || p.Profile == "" {
			return invalid("a provider needs a name and a provider profile")
		}
		s.providers[p.Name] = cloneProvider(p)
		return nil
	})
}

func (c *client) DeleteProvider(ctx context.Context, name string) error {
	return c.do(ctx, runtime.OpDeleteProvider, name, func(s *space) error {
		if _, ok := s.providers[name]; !ok {
			return notFound("no provider %q", name)
		}
		delete(s.providers, name)
		for _, sb := range s.sandboxes {
			sb.providers = slices.DeleteFunc(sb.providers, func(p string) bool { return p == name })
		}
		return nil
	})
}

func (c *client) AttachProvider(ctx context.Context, sandbox, provider string) error {
	return c.do(ctx, runtime.OpAttachProvider, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		if _, ok := s.providers[provider]; !ok {
			return notFound("no provider %q", provider)
		}
		if !slices.Contains(sb.providers, provider) {
			sb.providers = append(sb.providers, provider)
			slices.Sort(sb.providers)
		}
		return nil
	})
}

func (c *client) DetachProvider(ctx context.Context, sandbox, provider string) error {
	return c.do(ctx, runtime.OpDetachProvider, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		i := slices.Index(sb.providers, provider)
		if i < 0 {
			return notFound("provider %q is not attached to sandbox %q", provider, sandbox)
		}
		sb.providers = slices.Delete(sb.providers, i, i+1)
		return nil
	})
}

func (c *client) SandboxProviders(ctx context.Context, sandbox string) ([]string, error) {
	var out []string
	err := c.do(ctx, runtime.OpSandboxProviders, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		out = slices.Clone(sb.providers)
		return nil
	})
	return out, err
}

func (c *client) ListProviders(ctx context.Context) ([]runtime.Provider, error) {
	var out []runtime.Provider
	err := c.do(ctx, runtime.OpListProviders, "", func(s *space) error {
		for _, name := range slices.Sorted(maps.Keys(s.providers)) {
			p := cloneProvider(s.providers[name])
			p.Value = ""
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

func (c *client) ValidatePolicy(ctx context.Context, p runtime.Policy) (runtime.PolicyValidation, error) {
	var v runtime.PolicyValidation
	err := c.do(ctx, runtime.OpValidatePolicy, "", func(*space) error {
		if err := c.f.rejected(p); err != nil {
			return err
		}
		got := c.f.validations[string(p.Base)]
		v = runtime.PolicyValidation{Hosts: slices.Clone(got.Hosts), Providers: slices.Clone(got.Providers)}
		return nil
	})
	return v, err
}

func (c *client) GetPolicy(ctx context.Context, sandbox string) (runtime.SandboxPolicy, error) {
	var out runtime.SandboxPolicy
	err := c.do(ctx, runtime.OpGetPolicy, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		out = runtime.SandboxPolicy{Policy: clonePolicy(sb.policy), Revision: sb.revision}
		return nil
	})
	return out, err
}

func (c *client) AddPolicyRule(ctx context.Context, sandbox string, rule runtime.Rule) error {
	return c.do(ctx, runtime.OpAddPolicyRule, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		return c.f.addRule(sb, rule)
	})
}

// addRule adds a rule to a sandbox's policy unless the runtime refuses the
// result. The caller holds f.mu.
func (f *Fake) addRule(sb *sandbox, rule runtime.Rule) error {
	p := clonePolicy(sb.policy)
	p.Added = append(p.Added, cloneRules([]runtime.Rule{rule})...)
	if err := f.rejected(p); err != nil {
		return err
	}
	sb.policy = p
	sb.revision++
	return nil
}

func (c *client) ListProposals(ctx context.Context) ([]runtime.Proposal, error) {
	var out []runtime.Proposal
	err := c.do(ctx, runtime.OpListProposals, "", func(s *space) error {
		for _, p := range s.proposals {
			out = append(out, cloneProposal(p))
		}
		return nil
	})
	return out, err
}

// ApproveProposal adds the proposal's rule to the sandbox's policy, as the
// runtime does, and takes the proposal off the pending list.
func (c *client) ApproveProposal(ctx context.Context, sandbox, id string) error {
	return c.do(ctx, runtime.OpApproveProposal, sandbox, func(s *space) error {
		i, err := s.proposal(sandbox, id)
		if err != nil {
			return err
		}
		if err := c.f.addRule(s.sandboxes[sandbox], s.proposals[i].Rule); err != nil {
			return err
		}
		s.proposals = slices.Delete(s.proposals, i, i+1)
		return nil
	})
}

func (c *client) RejectProposal(ctx context.Context, sandbox, id, _ string) error {
	return c.do(ctx, runtime.OpRejectProposal, sandbox, func(s *space) error {
		i, err := s.proposal(sandbox, id)
		if err != nil {
			return err
		}
		s.proposals = slices.Delete(s.proposals, i, i+1)
		return nil
	})
}

func (s *space) proposal(sandbox, id string) (int, error) {
	i := slices.IndexFunc(s.proposals, func(p runtime.Proposal) bool { return p.Sandbox == sandbox && p.ID == id })
	if i < 0 || s.sandboxes[sandbox] == nil {
		return 0, notFound("no pending proposal %q of sandbox %q", id, sandbox)
	}
	return i, nil
}

func (c *client) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (runtime.SandboxStatus, error) {
	var st runtime.SandboxStatus
	err := c.do(ctx, runtime.OpCreateSandbox, spec.Name, func(s *space) error {
		switch {
		case spec.Name == "" || strings.ContainsAny(spec.Name, `/\`) || spec.Name == "." || spec.Name == "..":
			return invalid("sandbox name %q is not valid", spec.Name)
		case s.sandboxes[spec.Name] != nil:
			return invalid("sandbox %q exists", spec.Name)
		}
		if err := c.f.rejected(spec.Policy); err != nil {
			return err
		}
		sb := &sandbox{
			spec:     cloneSpec(spec),
			state:    runtime.StatePending,
			env:      maps.Clone(spec.Env),
			policy:   clonePolicy(spec.Policy),
			revision: 1,
			workDir:  true,
			files:    map[string]*file{},
		}
		if err := c.f.makeRoot(c.ws, sb); err != nil {
			return err
		}
		s.sandboxes[spec.Name] = sb
		c.f.publish(c.ws, sb, false)
		st = sb.status()
		return nil
	})
	return st, err
}

func (c *client) GetSandbox(ctx context.Context, name string) (runtime.SandboxStatus, error) {
	var st runtime.SandboxStatus
	err := c.do(ctx, runtime.OpGetSandbox, name, func(s *space) error {
		sb, err := s.sandbox(name)
		if err != nil {
			return err
		}
		st = sb.status()
		return nil
	})
	return st, err
}

func (c *client) ListSandboxes(ctx context.Context) ([]runtime.SandboxStatus, error) {
	var out []runtime.SandboxStatus
	err := c.do(ctx, runtime.OpListSandboxes, "", func(s *space) error {
		for _, name := range slices.Sorted(maps.Keys(s.sandboxes)) {
			out = append(out, s.sandboxes[name].status())
		}
		return nil
	})
	return out, err
}

func (c *client) DeleteSandbox(ctx context.Context, name string) error {
	return c.do(ctx, runtime.OpDeleteSandbox, name, func(s *space) error {
		sb, err := s.sandbox(name)
		if err != nil {
			return err
		}
		sb.stopMain()
		delete(s.sandboxes, name)
		s.proposals = slices.DeleteFunc(s.proposals, func(p runtime.Proposal) bool { return p.Sandbox == name })
		c.f.publish(c.ws, sb, true)
		return nil
	})
}

func (c *client) StopSandbox(ctx context.Context, name string) error {
	return c.do(ctx, runtime.OpStopSandbox, name, func(s *space) error {
		sb, err := s.sandbox(name)
		if err != nil {
			return err
		}
		sb.stopMain()
		sb.state = runtime.StateStopped
		c.f.publish(c.ws, sb, false)
		return nil
	})
}

func (c *client) StartSandbox(ctx context.Context, name string, spec runtime.StartSpec) error {
	return c.do(ctx, runtime.OpStartSandbox, name, func(s *space) error {
		sb, err := s.sandbox(name)
		if err != nil {
			return err
		}
		if err := c.f.rejected(spec.Policy); err != nil {
			return err
		}
		sb.policy = clonePolicy(spec.Policy)
		sb.revision++
		sb.env = maps.Clone(spec.Env)
		sb.state = runtime.StateRunning
		c.f.publish(c.ws, sb, false)
		return nil
	})
}

// WaitReady makes a pending or restarting sandbox running and returns it;
// for a stopped sandbox it waits until a change makes it running.
func (c *client) WaitReady(ctx context.Context, name string) (runtime.SandboxStatus, error) {
	op := runtime.OpWaitReady
	if err := c.begin(ctx, op, name); err != nil {
		return runtime.SandboxStatus{}, c.end(op, err)
	}
	f := c.f
	for {
		f.mu.Lock()
		sb, err := f.space(c.ws).sandbox(name)
		if err != nil {
			f.mu.Unlock()
			return runtime.SandboxStatus{}, c.end(op, err)
		}
		if sb.state != runtime.StateStopped {
			if sb.state != runtime.StateRunning {
				sb.state = runtime.StateRunning
				f.publish(c.ws, sb, false)
			}
			st := sb.status()
			f.mu.Unlock()
			return st, c.end(op, nil)
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return runtime.SandboxStatus{}, c.end(op, runtime.NewError(runtime.ErrUnavailable, op, ctx.Err().Error()))
		}
	}
}

func (c *client) WatchSandboxes(ctx context.Context) (runtime.SandboxWatch, error) {
	op := runtime.OpWatchSandboxes
	if err := c.begin(ctx, op, ""); err != nil {
		return nil, c.end(op, err)
	}
	out := newPipe[runtime.SandboxEvent]()
	track(c.f, ctx, &tracked{info: Stream{Op: op, Workspace: c.ws}, watch: out}, out)
	return &watch{c: c, out: out}, c.end(op, nil)
}

func (c *client) RunMain(ctx context.Context, sandbox string, command []string) error {
	return c.do(ctx, runtime.OpRunMain, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		if err := sb.runnable(command); err != nil {
			return err
		}
		c.f.startMain(c.ws, sandbox, sb, command)
		return nil
	})
}

// runnable refuses a command a sandbox cannot run.
func (sb *sandbox) runnable(command []string) error {
	switch {
	case len(command) == 0:
		return invalid("the command is empty")
	case sb.state == runtime.StateStopped:
		return invalid("sandbox %q is stopped", sb.spec.Name)
	}
	return nil
}

// command looks up a sandbox for a command and returns what runs it.
func (c *client) command(ctx context.Context, op, sandbox string, req runtime.ExecRequest) (runtime.ExecRequest, ExecFunc, string, error) {
	if err := c.begin(ctx, op, sandbox); err != nil {
		return req, nil, "", err
	}
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, err := f.space(c.ws).sandbox(sandbox)
	if err != nil {
		return req, nil, "", err
	}
	if err := sb.runnable(req.Command); err != nil {
		return req, nil, "", err
	}
	return sb.request(req), f.script, sb.host, nil
}

func (c *client) Exec(ctx context.Context, sandbox string, req runtime.ExecRequest) (runtime.ExecResult, error) {
	op := runtime.OpExec
	req, script, host, err := c.command(ctx, op, sandbox, req)
	if err != nil {
		return runtime.ExecResult{}, c.end(op, err)
	}
	var stdout, stderr bytes.Buffer
	code := runCommand(ctx, script, sandbox, host, req, &stdout, &stderr)
	if ctx.Err() != nil {
		return runtime.ExecResult{}, c.end(op, runtime.NewError(runtime.ErrUnavailable, op, ctx.Err().Error()))
	}
	return runtime.ExecResult{ExitCode: code, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, c.end(op, nil)
}

func (c *client) ExecStream(ctx context.Context, sandbox string, req runtime.ExecRequest) (runtime.ExecStream, error) {
	op := runtime.OpExecStream
	req, script, host, err := c.command(ctx, op, sandbox, req)
	if err != nil {
		return nil, c.end(op, err)
	}
	out := newPipe[runtime.Output]()
	work, cancel := context.WithCancel(ctx)
	out.whenEnded(cancel)
	track(c.f, ctx, &tracked{info: Stream{Op: op, Workspace: c.ws, Sandbox: sandbox}}, out)
	s := &execStream{c: c, out: out, finished: make(chan struct{})}
	f := c.f
	f.mu.Lock()
	closed := f.closed
	if !closed {
		f.wg.Go(func() {
			defer cancel()
			s.code = runCommand(work, script, sandbox, host, req, outputWriter{out, runtime.Stdout}, outputWriter{out, runtime.Stderr})
			s.ended = work.Err() == nil
			close(s.finished)
			out.end(io.EOF)
		})
	}
	f.mu.Unlock()
	if closed {
		close(s.finished)
		out.end(io.EOF)
	}
	return s, c.end(op, nil)
}

func (c *client) ExecInteractive(ctx context.Context, sandbox string, req runtime.ExecRequest, _ runtime.TerminalSize) (runtime.Terminal, error) {
	op := runtime.OpExecInteractive
	if _, _, _, err := c.command(ctx, op, sandbox, req); err != nil {
		return nil, c.end(op, err)
	}
	out := newPipe[[]byte]()
	track(c.f, ctx, &tracked{info: Stream{Op: op, Workspace: c.ws, Sandbox: sandbox}}, out)
	return &terminal{c: c, out: out}, c.end(op, nil)
}

func (c *client) Upload(ctx context.Context, sandbox, p string, mode fs.FileMode, r io.Reader) error {
	op := runtime.OpUpload
	if err := c.begin(ctx, op, sandbox); err != nil {
		return c.end(op, err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return c.end(op, invalid("reading the upload: %v", err))
	}
	f := c.f
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, err := f.space(c.ws).sandbox(sandbox)
	if err == nil {
		err = f.writeFile(sb, p, mode, data)
	}
	return c.end(op, err)
}

func (c *client) MakeDir(ctx context.Context, sandbox, p string, mode fs.FileMode) error {
	return c.do(ctx, runtime.OpMakeDir, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		return c.f.makeDir(sb, p, mode)
	})
}

func (c *client) Stat(ctx context.Context, sandbox, p string) (runtime.FileInfo, error) {
	var fi runtime.FileInfo
	err := c.do(ctx, runtime.OpStat, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		fi, err = c.f.stat(sb, p)
		return err
	})
	return fi, err
}

func (c *client) Download(ctx context.Context, sandbox, p string) (io.ReadCloser, error) {
	var data []byte
	err := c.do(ctx, runtime.OpDownload, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		data, err = c.f.readFile(sb, p)
		return err
	})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (c *client) Walk(ctx context.Context, sandbox, dir string) ([]runtime.FileInfo, error) {
	var out []runtime.FileInfo
	err := c.do(ctx, runtime.OpWalk, sandbox, func(s *space) error {
		sb, err := s.sandbox(sandbox)
		if err != nil {
			return err
		}
		out, err = c.f.walk(sb, dir)
		return err
	})
	return out, err
}

// Logs reads the entries already written; a followed log is listed as open
// under the same lock, so that an entry written meanwhile comes once.
func (c *client) Logs(ctx context.Context, sandbox string, q runtime.LogQuery) (runtime.LogStream, error) {
	op := runtime.OpLogs
	if err := c.begin(ctx, op, sandbox); err != nil {
		return nil, c.end(op, err)
	}
	f := c.f
	out := newPipe[runtime.LogEntry]()
	tr := &tracked{info: Stream{Op: op, Workspace: c.ws, Sandbox: sandbox}, follow: out, source: q.Source}
	if q.Follow {
		prepare(f, tr, out)
	}
	f.mu.Lock()
	sb, err := f.space(c.ws).sandbox(sandbox)
	if err == nil {
		var entries []runtime.LogEntry
		for _, e := range sb.logs {
			if (q.Source == "" || e.Source == q.Source) && !e.Time.Before(q.Since) {
				entries = append(entries, e)
			}
		}
		if q.Tail > 0 && len(entries) > q.Tail {
			entries = entries[len(entries)-q.Tail:]
		}
		for _, e := range entries {
			out.push(e)
		}
		if q.Follow {
			f.register(tr)
		}
	}
	f.mu.Unlock()
	if err != nil || !q.Follow {
		out.end(io.EOF)
	} else {
		endWithContext(ctx, out)
	}
	if err != nil {
		return nil, c.end(op, err)
	}
	return &logStream{c: c, out: out}, c.end(op, nil)
}
