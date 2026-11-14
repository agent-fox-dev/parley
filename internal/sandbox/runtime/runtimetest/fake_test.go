package runtimetest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
)

func TestFake_PassesTheContract(t *testing.T) {
	t.Parallel()
	modes := []struct {
		name  string
		opts  []runtimetest.Option
		setup func(t *testing.T, f *runtimetest.Fake)
	}{
		{
			name: "files and commands in host directories",
			opts: []runtimetest.Option{runtimetest.WithHostDirs()},
		},
		{
			name:  "files in memory, commands scripted",
			setup: func(t *testing.T, f *runtimetest.Fake) { f.SetExec(shellScript(t.TempDir())) },
		},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			runtimetest.RunContract(t, func(t *testing.T) runtimetest.Backend {
				f := runtimetest.New(t, mode.opts...)
				if mode.setup != nil {
					mode.setup(t, f)
				}
				return f.Backend()
			})
		})
	}
}

func TestRunContract_SkipsTheOperationsABackendLacks(t *testing.T) {
	t.Parallel()
	lacking := []string{
		runtime.OpGetProviderProfile, runtime.OpInstallProviderProfile, runtime.OpListProviders,
		runtime.OpAttachProvider, runtime.OpValidatePolicy, runtime.OpGetPolicy, runtime.OpAddPolicyRule,
		runtime.OpListProposals, runtime.OpStopSandbox, runtime.OpStartSandbox, runtime.OpWatchSandboxes,
		runtime.OpRunMain, runtime.OpExec, runtime.OpExecStream, runtime.OpExecInteractive,
		runtime.OpUpload, runtime.OpMakeDir, runtime.OpStat, runtime.OpDownload, runtime.OpWalk, runtime.OpLogs,
	}
	runtimetest.RunContract(t, func(t *testing.T) runtimetest.Backend {
		f := runtimetest.New(t)
		for _, op := range lacking {
			f.FailOperation(op, runtime.ErrUnsupported, "the runtime has no "+op)
		}
		b := f.Backend()
		b.RejectPolicies, b.AddProposal = nil, nil
		return b
	})
}

func TestFake_FailsANamedOperationWithTheChosenKindUntilCleared(t *testing.T) {
	t.Parallel()
	for _, kind := range []error{runtime.ErrNotFound, runtime.ErrInvalid, runtime.ErrUnsupported, runtime.ErrUnavailable} {
		t.Run(kind.Error(), func(t *testing.T) {
			t.Parallel()
			f := runtimetest.New(t)
			c := f.Client("parley", nil)
			ctx := t.Context()
			f.FailOperation(runtime.OpListSandboxes, kind, "boom")
			_, err := c.ListSandboxes(ctx)
			if runtime.KindOf(err) != kind || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("ListSandboxes = %v, want an error of kind %q with the text boom", err, kind)
			}
			if want := kind != runtime.ErrUnavailable; c.Reachable() != want {
				t.Fatalf("Reachable() = %v after an error of kind %q, want %v", c.Reachable(), kind, want)
			}
			if _, err := c.Release(ctx); err != nil {
				t.Fatalf("Release while another operation fails: %v", err)
			}
			f.FailOperation(runtime.OpListSandboxes, nil, "")
			if _, err := c.ListSandboxes(ctx); err != nil {
				t.Fatalf("ListSandboxes after the failure was cleared: %v", err)
			}
		})
	}
}

func TestFake_ReportsTheReleaseAndProviderProfilesItWasGiven(t *testing.T) {
	t.Parallel()
	f := runtimetest.New(t)
	c := f.Client("parley", nil)
	ctx := t.Context()
	if r, err := c.Release(ctx); err != nil || r != (runtime.Release{Healthy: true, Version: "0.1.8"}) {
		t.Fatalf("default Release = %+v, %v; want a healthy 0.1.8", r, err)
	}
	f.SetRelease(runtime.Release{Healthy: true, Version: "0.9.0"})
	if r, _ := c.Release(ctx); r.Version != "0.9.0" {
		t.Fatalf("Release version = %q, want 0.9.0", r.Version)
	}
	p := runtime.ProviderProfile{ID: "anthropic", Variables: []string{"ANTHROPIC_API_KEY"}, Endpoints: []runtime.Endpoint{{Host: "api.anthropic.test", Port: 443}}}
	f.AddProviderProfile("parley", p)
	got, err := c.GetProviderProfile(ctx, "anthropic")
	if err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("GetProviderProfile = %+v, %v; want %+v", got, err, p)
	}
	if _, err := f.Client("other", nil).GetProviderProfile(ctx, "anthropic"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("GetProviderProfile in another workspace = %v, want not found", err)
	}
}

func TestFake_RejectsThePolicyValuesAPredicateMatches(t *testing.T) {
	t.Parallel()
	bad := runtime.Policy{Base: []byte("bad\n")}
	rows := []struct {
		name string
		call func(ctx context.Context, c runtime.Client) error
	}{
		{"validate", func(ctx context.Context, c runtime.Client) error { _, err := c.ValidatePolicy(ctx, bad); return err }},
		{"create", func(ctx context.Context, c runtime.Client) error {
			_, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-2", Policy: bad})
			return err
		}},
		{"start", func(ctx context.Context, c runtime.Client) error {
			return c.StartSandbox(ctx, "sb-1", runtime.StartSpec{Policy: bad})
		}},
		{"add a rule", func(ctx context.Context, c runtime.Client) error {
			return c.AddPolicyRule(ctx, "sb-1", runtime.Rule{Name: "bad"})
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := runtimetest.New(t)
			c := f.Client("parley", nil)
			ctx := t.Context()
			if _, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1", Policy: runtime.Policy{Base: []byte("good\n")}}); err != nil {
				t.Fatal(err)
			}
			f.RejectPolicies(func(p runtime.Policy) bool {
				return bytes.Contains(p.Base, []byte("bad")) || slices.ContainsFunc(p.Added, func(r runtime.Rule) bool { return r.Name == "bad" })
			}, "bad rule")
			err := row.call(ctx, c)
			if runtime.KindOf(err) != runtime.ErrInvalid || !strings.Contains(err.Error(), "bad rule") {
				t.Fatalf("error = %v, want invalid with the reason bad rule", err)
			}
		})
	}
}

func TestFake_ReportsWhatABaseDocumentAllowsAndRefersTo(t *testing.T) {
	t.Parallel()
	f := runtimetest.New(t)
	c := f.Client("parley", nil)
	want := runtime.PolicyValidation{Hosts: []string{"api.anthropic.test"}, Providers: []string{"anthropic"}}
	f.SetPolicyValidation([]byte("base-a\n"), want)
	got, err := c.ValidatePolicy(t.Context(), runtime.Policy{Base: []byte("base-a\n")})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ValidatePolicy = %+v, %v; want %+v", got, err, want)
	}
	got, err = c.ValidatePolicy(t.Context(), runtime.Policy{Base: []byte("base-b\n")})
	if err != nil || len(got.Hosts) != 0 || len(got.Providers) != 0 {
		t.Fatalf("ValidatePolicy of another base = %+v, %v; want nothing reported", got, err)
	}
}

func TestFake_ChangesASandboxAsTheTestTellsIt(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name   string
		change func(f *runtimetest.Fake)
		want   func(st runtime.SandboxStatus) bool
	}{
		{"state", func(f *runtimetest.Fake) { f.SetState("parley", "sb-1", runtime.StateRestarting) },
			func(st runtime.SandboxStatus) bool { return st.State == runtime.StateRestarting }},
		{"end of the main process", func(f *runtimetest.Fake) { f.EndMain("parley", "sb-1", 7) },
			func(st runtime.SandboxStatus) bool { return st.MainExited && st.ExitCode == 7 }},
		{"working directory dropped", func(f *runtimetest.Fake) { f.DropWorkDir("parley", "sb-1") },
			func(st runtime.SandboxStatus) bool { return !st.WorkDirPresent }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			f := runtimetest.New(t)
			c := f.Client("parley", nil)
			ctx := t.Context()
			if _, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1", WorkDir: "/w"}); err != nil {
				t.Fatal(err)
			}
			if err := c.RunMain(ctx, "sb-1", []string{"agent"}); err != nil {
				t.Fatal(err)
			}
			before, _ := c.GetSandbox(ctx, "sb-1")
			if row.want(before) || !before.WorkDirPresent || before.MainExited {
				t.Fatalf("status before the change = %+v", before)
			}
			w, err := c.WatchSandboxes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			row.change(f)
			if st, _ := c.GetSandbox(ctx, "sb-1"); !row.want(st) {
				t.Fatalf("GetSandbox after the change = %+v", st)
			}
			if ev, err := w.Next(); err != nil || !row.want(ev.Sandbox) {
				t.Fatalf("watched change = %+v, %v", ev, err)
			}
		})
	}
}

func TestFake_DroppedWorkingDirectoryLosesItsFiles(t *testing.T) {
	t.Parallel()
	f := runtimetest.New(t)
	c := f.Client("parley", nil)
	ctx := t.Context()
	if _, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1", WorkDir: "/w"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(ctx, "sb-1", "/w/a", 0o644, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	f.DropWorkDir("parley", "sb-1")
	if _, err := c.Stat(ctx, "sb-1", "/w/a"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Stat after the working directory was dropped = %v, want not found", err)
	}
}

func TestFake_LogsTheEntriesItWasToldToEmit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	f := runtimetest.New(t, runtimetest.WithClock(func() time.Time { return now }))
	c := f.Client("parley", nil)
	ctx := t.Context()
	if _, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1"}); err != nil {
		t.Fatal(err)
	}
	f.EmitOutput("parley", "sb-1", runtime.Stdout, "first")
	now = now.Add(time.Minute)
	f.EmitOutput("parley", "sb-1", runtime.Stderr, "second")
	now = now.Add(time.Minute)
	deny := runtime.LogEntry{Action: "deny", Program: "/usr/bin/curl", Host: "evil.test", Port: 443, Method: "GET", Path: "/x"}
	f.EmitPolicyLog("parley", "sb-1", deny)

	rows := []struct {
		name  string
		query runtime.LogQuery
		want  []string
	}{
		{"process lines", runtime.LogQuery{Source: runtime.SourceProcess}, []string{"stdout first", "stderr second"}},
		{"policy entries", runtime.LogQuery{Source: runtime.SourcePolicy}, []string{"deny evil.test"}},
		{"both sources", runtime.LogQuery{}, []string{"stdout first", "stderr second", "deny evil.test"}},
		{"tail", runtime.LogQuery{Tail: 2}, []string{"stderr second", "deny evil.test"}},
		{"since", runtime.LogQuery{Since: now.Add(-time.Minute)}, []string{"stderr second", "deny evil.test"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			ls, err := c.Logs(ctx, "sb-1", row.query)
			if err != nil {
				t.Fatal(err)
			}
			defer ls.Close()
			var got []string
			for {
				e, err := ls.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if e.Source == runtime.SourcePolicy {
					if e.Program != deny.Program || e.Port != 443 || e.Method != "GET" || e.Path != "/x" || !e.Time.Equal(now) {
						t.Fatalf("policy entry = %+v", e)
					}
					got = append(got, e.Action+" "+e.Host)
				} else {
					got = append(got, string(e.Stream)+" "+e.Text)
				}
			}
			if !slices.Equal(got, row.want) {
				t.Fatalf("entries = %q, want %q", got, row.want)
			}
		})
	}
}

func TestFake_BreaksAnOpenStream(t *testing.T) {
	t.Parallel()
	rows := []struct {
		op   string
		open func(ctx context.Context, c runtime.Client) (next func() error, err error)
	}{
		{runtime.OpLogs, func(ctx context.Context, c runtime.Client) (func() error, error) {
			s, err := c.Logs(ctx, "sb-1", runtime.LogQuery{Follow: true})
			return func() error { _, err := s.Next(); return err }, err
		}},
		{runtime.OpWatchSandboxes, func(ctx context.Context, c runtime.Client) (func() error, error) {
			s, err := c.WatchSandboxes(ctx)
			return func() error { _, err := s.Next(); return err }, err
		}},
		{runtime.OpExecStream, func(ctx context.Context, c runtime.Client) (func() error, error) {
			s, err := c.ExecStream(ctx, "sb-1", runtime.ExecRequest{Command: []string{"block"}})
			return func() error { _, err := s.Next(); return err }, err
		}},
		{runtime.OpExecInteractive, func(ctx context.Context, c runtime.Client) (func() error, error) {
			s, err := c.ExecInteractive(ctx, "sb-1", runtime.ExecRequest{Command: []string{"sh"}}, runtime.TerminalSize{Cols: 80, Rows: 24})
			return func() error { _, err := s.Read(make([]byte, 8)); return err }, err
		}},
	}
	for _, row := range rows {
		t.Run(row.op, func(t *testing.T) {
			t.Parallel()
			f := runtimetest.New(t)
			f.SetExec(func(ctx context.Context, _ string, _ runtime.ExecRequest, _, _ io.Writer) int {
				<-ctx.Done()
				return 0
			})
			c := f.Client("parley", nil)
			ctx := t.Context()
			if _, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1"}); err != nil {
				t.Fatal(err)
			}
			next, err := row.open(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			open := f.Streams()
			if len(open) != 1 || open[0].Op != row.op || open[0].Workspace != "parley" {
				t.Fatalf("Streams() = %+v, want one %s stream of parley", open, row.op)
			}
			f.BreakStream(open[0].ID)
			if err := next(); runtime.KindOf(err) != runtime.ErrUnavailable {
				t.Fatalf("reading a broken stream = %v, want an error of kind unavailable", err)
			}
			if c.Reachable() {
				t.Fatal("Reachable() = true after a stream broke")
			}
			if open := f.Streams(); len(open) != 0 {
				t.Fatalf("Streams() after the break = %+v, want none", open)
			}
		})
	}
}

func TestFake_ForgetsAStreamOnceItEnds(t *testing.T) {
	t.Parallel()
	f := runtimetest.New(t)
	c := f.Client("parley", nil)
	ctx, cancel := context.WithCancel(t.Context())
	if _, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1"}); err != nil {
		t.Fatal(err)
	}
	ls, err := c.Logs(ctx, "sb-1", runtime.LogQuery{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	w, err := c.WatchSandboxes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.Streams()); n != 2 {
		t.Fatalf("open streams = %d, want 2", n)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if open := f.Streams(); len(open) != 1 || open[0].Op != runtime.OpLogs || open[0].Sandbox != "sb-1" {
		t.Fatalf("Streams() after closing the watch = %+v, want the log stream of sb-1", open)
	}
	cancel()
	if _, err := ls.Next(); err != io.EOF {
		t.Fatalf("Next after the context ended = %v, want io.EOF", err)
	}
	if open := f.Streams(); len(open) != 0 {
		t.Fatalf("Streams() after the context ended = %+v, want none", open)
	}
}

func TestFake_RecordsTheCallsItReceivedInOrder(t *testing.T) {
	t.Parallel()
	f := runtimetest.New(t)
	c, other := f.Client("parley", nil), f.Client("other", nil)
	ctx := t.Context()
	_, _ = c.Release(ctx)
	_ = c.EnsureWorkspace(ctx)
	_ = c.InstallProviderProfile(ctx, runtime.ProviderProfile{ID: "parley-hub"})
	_, _ = c.GetProviderProfile(ctx, "anthropic")
	_, _ = c.ValidatePolicy(ctx, runtime.Policy{})
	f.SetUnreachable(true)
	_, _ = other.GetSandbox(ctx, "sb-1")
	_ = c.Reachable()
	want := []runtimetest.Call{
		{Op: runtime.OpRelease, Workspace: "parley"},
		{Op: runtime.OpEnsureWorkspace, Workspace: "parley"},
		{Op: runtime.OpInstallProviderProfile, Workspace: "parley", Target: "parley-hub"},
		{Op: runtime.OpGetProviderProfile, Workspace: "parley", Target: "anthropic"},
		{Op: runtime.OpValidatePolicy, Workspace: "parley"},
		{Op: runtime.OpGetSandbox, Workspace: "other", Target: "sb-1"},
	}
	if got := f.Calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Calls() = %+v, want %+v", got, want)
	}
}

func TestFake_ScriptsExecResults(t *testing.T) {
	t.Parallel()
	f := runtimetest.New(t)
	c := f.Client("parley", nil)
	ctx := t.Context()
	if _, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1"}); err != nil {
		t.Fatal(err)
	}
	if r, err := c.Exec(ctx, "sb-1", runtime.ExecRequest{Command: []string{"anything"}}); err != nil || r.ExitCode != 0 || len(r.Stdout)+len(r.Stderr) != 0 {
		t.Fatalf("Exec without a script = %+v, %v; want exit 0 and no output", r, err)
	}
	var seen []string
	f.SetExec(func(_ context.Context, sandbox string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
		seen = append(seen, sandbox+" "+strings.Join(req.Command, " ")+" "+req.Env["K"])
		_, _ = io.WriteString(stdout, "o")
		_, _ = io.WriteString(stderr, "e")
		return 5
	})
	req := runtime.ExecRequest{Command: []string{"git", "status"}, Env: map[string]string{"K": "v"}}
	r, err := c.Exec(ctx, "sb-1", req)
	if err != nil || r.ExitCode != 5 || string(r.Stdout) != "o" || string(r.Stderr) != "e" {
		t.Fatalf("scripted Exec = %+v, %v", r, err)
	}
	s, err := c.ExecStream(ctx, "sb-1", req)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for {
		o, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(o.Stream)+" "+string(o.Data))
	}
	if code, err := s.ExitCode(); err != nil || code != 5 || !slices.Equal(out, []string{"stdout o", "stderr e"}) {
		t.Fatalf("scripted ExecStream = %q, exit %d, %v", out, code, err)
	}
	if want := []string{"sb-1 git status v", "sb-1 git status v"}; !slices.Equal(seen, want) {
		t.Fatalf("script saw %q, want %q", seen, want)
	}
}

func TestFake_BacksASandboxWithAHostDirectory(t *testing.T) {
	t.Parallel()
	f := runtimetest.New(t, runtimetest.WithHostDirs())
	c := f.Client("parley", nil)
	ctx := t.Context()
	if _, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1", WorkDir: "/w"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(ctx, "sb-1", "/w/a", 0o600, strings.NewReader("up")); err != nil {
		t.Fatal(err)
	}
	dir := f.HostDir("parley", "sb-1")
	if data, err := os.ReadFile(filepath.Join(dir, "w", "a")); err != nil || string(data) != "up" {
		t.Fatalf("host file = %q, %v; want up", data, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "w", "b"), []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := c.Exec(ctx, "sb-1", runtime.ExecRequest{Command: []string{"cat", "b"}})
	if err != nil || string(r.Stdout) != "host" {
		t.Fatalf("Exec of cat b in the working directory = %+v, %v; want host", r, err)
	}
}

func TestFake_KeepsWhatItWasGiven(t *testing.T) {
	t.Parallel()
	f := runtimetest.New(t)
	c := f.Client("parley", nil)
	ctx := t.Context()
	spec := runtime.SandboxSpec{Name: "sb-1", Image: "agent:1", Env: map[string]string{"A": "1"}, Providers: []string{"p"}}
	if _, err := c.CreateSandbox(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.Spec("parley", "sb-1"); !ok || !reflect.DeepEqual(got, spec) {
		t.Fatalf("Spec = %+v, %v; want %+v", got, ok, spec)
	}
	if err := c.EnsureProvider(ctx, runtime.Provider{Name: "p", Profile: "anthropic", Value: "secret"}); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.Provider("parley", "p"); !ok || got.Value != "secret" {
		t.Fatalf("Provider = %+v, %v; want the value secret", got, ok)
	}
	if _, ok := f.Spec("other", "sb-1"); ok {
		t.Fatal("Spec found the sandbox in another workspace")
	}
}

// shellScript runs every command for real, with sh, in dir, so that the
// contract's commands give the same results in memory as on a runtime.
func shellScript(dir string) runtimetest.ExecFunc {
	return func(ctx context.Context, _ string, req runtime.ExecRequest, stdout, stderr io.Writer) int {
		cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		for k, v := range req.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdin = bytes.NewReader(req.Stdin)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		err := cmd.Run()
		if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
			return exit.ExitCode()
		} else if err != nil {
			return -1
		}
		return 0
	}
}
