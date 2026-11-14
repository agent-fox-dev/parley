package runtimetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// Backend is one runtime backend under the contract tests: the clients of
// an implementation over one shared store, and the controls the tests need.
type Backend struct {
	// Client returns a client of the backend that acts in workspace and
	// reports its reachability to rec.
	Client func(workspace string, rec runtime.Recorder) runtime.Client
	// SetUnreachable makes every later call of the backend's clients fail
	// as when the gateway cannot be reached (true), or ends that (false). It
	// is safe to call while operations run.
	SetUnreachable func(unreachable bool)
	// RejectPolicies, when not nil, makes the backend refuse every policy
	// value pred matches, with reason as the runtime's reason.
	RejectPolicies func(pred func(runtime.Policy) bool, reason string)
	// AddProposal, when not nil, adds a pending proposal to a sandbox of
	// workspace and returns its id.
	AddProposal func(workspace string, p runtime.Proposal) string
}

// The workspaces the contract's clients act in.
const (
	contractWorkspace = "parley"
	otherWorkspace    = "other"
)

// RunContract runs the contract tests of the runtime client against the
// backends newBackend returns, a new one for every test. An operation that
// returns an error of kind ErrUnsupported skips the assertions about it, so
// the contract holds for a runtime that lacks the operation. The tests whose
// subject is not what a sandbox is created with create theirs without a
// working directory or a base document, so that they also hold for a
// runtime that cannot create a sandbox with those.
func RunContract(t *testing.T, newBackend func(t *testing.T) Backend) {
	t.Helper()
	for _, test := range []struct {
		name string
		run  func(t *testing.T, b Backend)
	}{
		{"a created sandbox is listed until it is deleted", contractCreateListDelete},
		{"wait, stop and start change the state get reports", contractWaitStopStart},
		{"a watch delivers the changes of the workspace's sandboxes and ends with the context", contractWatch},
		{"exec returns the exit code, stdout and stderr the command produced", contractExec},
		{"an exec stream delivers the output and the exit code", contractExecStream},
		{"a terminal takes input, resizes and closes", contractTerminal},
		{"an uploaded file is downloaded byte for byte and described by stat and walk", contractFiles},
		{"the main process's end, exit code and output are reported", contractMainProcess},
		{"a followed log delivers new lines and ends with the context", contractFollowedLog},
		{"the policy value a sandbox was created with is the one get reports", contractPolicyValue},
		{"an ensured provider can be listed, attached and detached", contractProviders},
		{"an installed provider profile is returned by get", contractProviderProfile},
		{"a proposal is listed, then approved or rejected once", contractProposals},
		{"a client acts in its workspace and in no other", contractWorkspaces},
		{"every failure is of exactly one kind", contractErrorKinds},
		{"the reachability flag follows the results and reports every change", contractReachability},
		{"the reachability flag is safe to read while operations run", contractReachabilityConcurrency},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, newBackend(t)) })
	}
}

func contractCreateListDelete(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	spec := plainSpec("sb-1")
	created, err := c.CreateSandbox(ctx, spec)
	skipIfUnsupported(t, err, "CreateSandbox")
	require(t, err, "CreateSandbox")
	if created.Name != spec.Name || !reflect.DeepEqual(created.Labels, spec.Labels) {
		t.Fatalf("CreateSandbox = %+v, want name %q and labels %v", created, spec.Name, spec.Labels)
	}
	got, err := c.GetSandbox(ctx, spec.Name)
	require(t, err, "GetSandbox")
	if got.Name != spec.Name {
		t.Fatalf("GetSandbox name = %q, want %q", got.Name, spec.Name)
	}
	if names := sandboxNames(t, ctx, c); !slices.Equal(names, []string{spec.Name}) {
		t.Fatalf("ListSandboxes names = %v, want [%s]", names, spec.Name)
	}
	require(t, c.DeleteSandbox(ctx, spec.Name), "DeleteSandbox")
	_, err = c.GetSandbox(ctx, spec.Name)
	wantKind(t, err, runtime.ErrNotFound, "GetSandbox after DeleteSandbox")
	if names := sandboxNames(t, ctx, c); len(names) != 0 {
		t.Fatalf("ListSandboxes names after DeleteSandbox = %v, want none", names)
	}
}

func contractWaitStopStart(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	name := createSandbox(t, ctx, c, "sb-1")
	st, err := c.WaitReady(ctx, name)
	skipIfUnsupported(t, err, "WaitReady")
	require(t, err, "WaitReady")
	if st.State != runtime.StateRunning {
		t.Fatalf("WaitReady state = %q, want %q", st.State, runtime.StateRunning)
	}
	err = c.StopSandbox(ctx, name)
	skipIfUnsupported(t, err, "StopSandbox")
	require(t, err, "StopSandbox")
	if st := getSandbox(t, ctx, c, name); st.State != runtime.StateStopped {
		t.Fatalf("state after StopSandbox = %q, want %q", st.State, runtime.StateStopped)
	}
	err = c.StartSandbox(ctx, name, runtime.StartSpec{Policy: plainSpec(name).Policy})
	skipIfUnsupported(t, err, "StartSandbox")
	require(t, err, "StartSandbox")
	st, err = c.WaitReady(ctx, name)
	require(t, err, "WaitReady after StartSandbox")
	if st.State != runtime.StateRunning {
		t.Fatalf("state after StartSandbox = %q, want %q", st.State, runtime.StateRunning)
	}
}

func contractWatch(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w, err := c.WatchSandboxes(wctx)
	skipIfUnsupported(t, err, "WatchSandboxes")
	require(t, err, "WatchSandboxes")
	defer w.Close()
	name := createSandbox(t, ctx, c, "sb-1")
	ev, err := w.Next()
	require(t, err, "Next after CreateSandbox")
	if ev.Sandbox.Name != name || ev.Deleted {
		t.Fatalf("first event = %+v, want sandbox %q not deleted", ev, name)
	}
	require(t, c.DeleteSandbox(ctx, name), "DeleteSandbox")
	for {
		ev, err := w.Next()
		require(t, err, "Next after DeleteSandbox")
		if ev.Deleted {
			if ev.Sandbox.Name != name {
				t.Fatalf("deleted event names %q, want %q", ev.Sandbox.Name, name)
			}
			break
		}
	}
	cancel()
	for {
		_, err := w.Next()
		if err == io.EOF {
			break
		}
		require(t, err, "Next after the context ended")
	}
}

func contractExec(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	name := readySandbox(t, ctx, c, "sb-1")
	r, err := c.Exec(ctx, name, runtime.ExecRequest{Command: []string{"sh", "-c", "echo hi; echo oops >&2; exit 3"}})
	skipIfUnsupported(t, err, "Exec")
	require(t, err, "Exec")
	if r.ExitCode != 3 || string(r.Stdout) != "hi\n" || string(r.Stderr) != "oops\n" {
		t.Fatalf("Exec = {%d %q %q}, want {3 \"hi\\n\" \"oops\\n\"}", r.ExitCode, r.Stdout, r.Stderr)
	}
	r, err = c.Exec(ctx, name, runtime.ExecRequest{Command: []string{"sh", "-c", `printf %s "$X"`}, Env: map[string]string{"X": "y"}})
	require(t, err, "Exec with an environment")
	if string(r.Stdout) != "y" {
		t.Fatalf("Exec with X=y printed %q, want %q", r.Stdout, "y")
	}
	r, err = c.Exec(ctx, name, runtime.ExecRequest{Command: []string{"cat"}, Stdin: []byte("in")})
	if !errors.Is(err, runtime.ErrUnsupported) {
		require(t, err, "Exec with stdin")
		if string(r.Stdout) != "in" {
			t.Fatalf("Exec of cat with stdin %q printed %q", "in", r.Stdout)
		}
	}
}

func contractExecStream(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	name := readySandbox(t, ctx, c, "sb-1")
	s, err := c.ExecStream(ctx, name, runtime.ExecRequest{Command: []string{"sh", "-c", "echo hi; echo oops >&2; exit 4"}})
	skipIfUnsupported(t, err, "ExecStream")
	require(t, err, "ExecStream")
	defer s.Close()
	var stdout, stderr bytes.Buffer
	for {
		out, err := s.Next()
		if err == io.EOF {
			break
		}
		require(t, err, "Next")
		switch out.Stream {
		case runtime.Stdout:
			stdout.Write(out.Data)
		case runtime.Stderr:
			stderr.Write(out.Data)
		default:
			t.Fatalf("output of stream %q", out.Stream)
		}
	}
	code, err := s.ExitCode()
	require(t, err, "ExitCode")
	if code != 4 || stdout.String() != "hi\n" || stderr.String() != "oops\n" {
		t.Fatalf("ExecStream = {%d %q %q}, want {4 \"hi\\n\" \"oops\\n\"}", code, stdout.String(), stderr.String())
	}
}

func contractTerminal(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	name := readySandbox(t, ctx, c, "sb-1")
	term, err := c.ExecInteractive(ctx, name, runtime.ExecRequest{Command: []string{"cat"}}, runtime.TerminalSize{Cols: 80, Rows: 24})
	skipIfUnsupported(t, err, "ExecInteractive")
	require(t, err, "ExecInteractive")
	_, err = term.Write([]byte("hello\n"))
	require(t, err, "Write")
	var seen []byte
	buf := make([]byte, 256)
	for !bytes.Contains(seen, []byte("hello")) {
		n, err := term.Read(buf)
		require(t, err, "Read")
		seen = append(seen, buf[:n]...)
	}
	require(t, term.Resize(runtime.TerminalSize{Cols: 100, Rows: 30}), "Resize")
	require(t, term.Close(), "Close")
}

func contractFiles(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	name := readySandbox(t, ctx, c, "sb-1")
	data := []byte("line one\nline two\x00\xff\n")
	err := c.Upload(ctx, name, "/w/a", 0o644, bytes.NewReader(data))
	skipIfUnsupported(t, err, "Upload")
	require(t, err, "Upload")

	rc, err := c.Download(ctx, name, "/w/a")
	if !skipped(t, err, "Download") {
		require(t, err, "Download")
		got, err := io.ReadAll(rc)
		require(t, err, "reading the download")
		require(t, rc.Close(), "closing the download")
		if !bytes.Equal(got, data) {
			t.Errorf("Download = %q, want %q", got, data)
		}
	}

	fi, err := c.Stat(ctx, name, "/w/a")
	if !skipped(t, err, "Stat") {
		require(t, err, "Stat")
		want := runtime.FileInfo{Path: "/w/a", Size: int64(len(data)), Mode: 0o644}
		if fi != want {
			t.Errorf("Stat = %+v, want %+v", fi, want)
		}
		_, err = c.Stat(ctx, name, "/w/none")
		wantKind(t, err, runtime.ErrNotFound, "Stat of a missing path")
	}

	infos, err := c.Walk(ctx, name, "/w")
	if !skipped(t, err, "Walk") {
		require(t, err, "Walk")
		if names := baseNames(infos); !slices.Equal(names, []string{"a"}) {
			t.Errorf("Walk names = %v, want [a]", names)
		}
	}

	err = c.MakeDir(ctx, name, "/w/d/e", 0o755)
	if !skipped(t, err, "MakeDir") {
		require(t, err, "MakeDir")
		fi, err := c.Stat(ctx, name, "/w/d/e")
		if !skipped(t, err, "Stat") {
			require(t, err, "Stat of the new directory")
			if !fi.Dir || fi.Path != "/w/d/e" {
				t.Errorf("Stat of the new directory = %+v, want a directory at /w/d/e", fi)
			}
		}
	}
}

func contractMainProcess(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	name := readySandbox(t, ctx, c, "sb-1")
	err := c.RunMain(ctx, name, []string{"sh", "-c", "echo out; exit 2"})
	skipIfUnsupported(t, err, "RunMain")
	require(t, err, "RunMain")
	st := eventually(t, ctx, c, name, func(st runtime.SandboxStatus) bool { return st.MainExited })
	if st.ExitCode != 2 {
		t.Fatalf("main exit code = %d, want 2", st.ExitCode)
	}
	ls, err := c.Logs(ctx, name, runtime.LogQuery{Source: runtime.SourceProcess})
	skipIfUnsupported(t, err, "Logs")
	require(t, err, "Logs")
	defer ls.Close()
	var texts []string
	for {
		e, err := ls.Next()
		if err == io.EOF {
			break
		}
		require(t, err, "Next")
		if e.Source != runtime.SourceProcess || e.Stream != runtime.Stdout {
			t.Fatalf("log entry %+v, want a process line of stdout", e)
		}
		texts = append(texts, e.Text)
	}
	if !slices.Equal(texts, []string{"out"}) {
		t.Fatalf("process log texts = %q, want [out]", texts)
	}
}

func contractFollowedLog(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	name := readySandbox(t, ctx, c, "sb-1")
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ls, err := c.Logs(fctx, name, runtime.LogQuery{Source: runtime.SourceProcess, Follow: true})
	skipIfUnsupported(t, err, "Logs")
	require(t, err, "Logs")
	defer ls.Close()
	err = c.RunMain(ctx, name, []string{"sh", "-c", "echo one; sleep 0.1; echo two"})
	skipIfUnsupported(t, err, "RunMain")
	require(t, err, "RunMain")
	var texts []string
	for !slices.Contains(texts, "two") {
		e, err := ls.Next()
		require(t, err, "Next")
		texts = append(texts, e.Text)
	}
	if !slices.Equal(texts, []string{"one", "two"}) {
		t.Fatalf("followed texts = %q, want [one two]", texts)
	}
	cancel()
	for {
		_, err := ls.Next()
		if err == io.EOF {
			break
		}
		require(t, err, "Next after the context ended")
	}
}

func contractPolicyValue(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	spec := contractSpec("sb-1")
	_, err := c.ValidatePolicy(ctx, spec.Policy)
	if !skipped(t, err, "ValidatePolicy") {
		require(t, err, "ValidatePolicy")
	}
	_, err = c.CreateSandbox(ctx, spec)
	skipIfUnsupported(t, err, "CreateSandbox with a working directory and a base document")
	require(t, err, "CreateSandbox")
	name := spec.Name
	got, err := c.GetPolicy(ctx, name)
	skipIfUnsupported(t, err, "GetPolicy")
	require(t, err, "GetPolicy")
	if !samePolicy(got.Policy, spec.Policy) {
		t.Fatalf("GetPolicy = %+v, want %+v", got.Policy, spec.Policy)
	}
	rule := runtime.Rule{Name: "added", Body: json.RawMessage(`{"endpoints":[{"host":"pkg.test","port":443}]}`)}
	err = c.AddPolicyRule(ctx, name, rule)
	skipIfUnsupported(t, err, "AddPolicyRule")
	require(t, err, "AddPolicyRule")
	after, err := c.GetPolicy(ctx, name)
	require(t, err, "GetPolicy after AddPolicyRule")
	want := spec.Policy
	want.Added = append(slices.Clone(want.Added), rule)
	if !samePolicy(after.Policy, want) {
		t.Fatalf("GetPolicy after AddPolicyRule = %+v, want %+v", after.Policy, want)
	}
	if after.Revision <= got.Revision {
		t.Fatalf("revision after AddPolicyRule = %d, want more than %d", after.Revision, got.Revision)
	}
}

func contractProviders(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	name := createSandbox(t, ctx, c, "sb-1")
	labels := map[string]string{"parley.sandbox": name}
	err := c.EnsureProvider(ctx, runtime.Provider{Name: "p", Profile: "anthropic", Value: "v1", Labels: labels})
	skipIfUnsupported(t, err, "EnsureProvider")
	require(t, err, "EnsureProvider")
	require(t, c.EnsureProvider(ctx, runtime.Provider{Name: "p", Profile: "anthropic", Value: "v2", Labels: labels}), "EnsureProvider again")
	list, err := c.ListProviders(ctx)
	if !skipped(t, err, "ListProviders") {
		require(t, err, "ListProviders")
		want := []runtime.Provider{{Name: "p", Profile: "anthropic", Labels: labels}}
		if !reflect.DeepEqual(list, want) {
			t.Errorf("ListProviders = %+v, want %+v", list, want)
		}
	}
	err = c.AttachProvider(ctx, name, "p")
	if !skipped(t, err, "AttachProvider") {
		require(t, err, "AttachProvider")
		wantProviders(t, ctx, c, name, []string{"p"})
		require(t, c.DetachProvider(ctx, name, "p"), "DetachProvider")
		wantProviders(t, ctx, c, name, nil)
		wantKind(t, c.AttachProvider(ctx, name, "none"), runtime.ErrNotFound, "AttachProvider of an unknown provider")
	}
	require(t, c.DeleteProvider(ctx, "p"), "DeleteProvider")
	wantKind(t, c.DeleteProvider(ctx, "p"), runtime.ErrNotFound, "DeleteProvider of a deleted provider")
}

func contractProviderProfile(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	p := runtime.ProviderProfile{
		ID:        "parley-hub",
		Variables: []string{"PARLEY_API_KEY"},
		Endpoints: []runtime.Endpoint{{Scheme: "https", Host: "hub.test", Port: 443, Paths: []string{"/api/v1/", "/git/"}}},
	}
	err := c.InstallProviderProfile(ctx, p)
	skipIfUnsupported(t, err, "InstallProviderProfile")
	require(t, err, "InstallProviderProfile")
	p.Variables = []string{"PARLEY_API_KEY", "PARLEY_TOKEN"}
	require(t, c.InstallProviderProfile(ctx, p), "InstallProviderProfile replacing it")
	got, err := c.GetProviderProfile(ctx, p.ID)
	skipIfUnsupported(t, err, "GetProviderProfile")
	require(t, err, "GetProviderProfile")
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("GetProviderProfile = %+v, want %+v", got, p)
	}
}

func contractProposals(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	list, err := c.ListProposals(ctx)
	skipIfUnsupported(t, err, "ListProposals")
	require(t, err, "ListProposals")
	if len(list) != 0 {
		t.Fatalf("ListProposals on a new backend = %+v, want none", list)
	}
	if b.AddProposal == nil {
		t.Skip("the backend cannot add a proposal")
	}
	name := createSandbox(t, ctx, c, "sb-1")
	rule := runtime.Rule{Name: "pypi", Body: json.RawMessage(`{"endpoints":[{"host":"pypi.test","port":443}]}`)}
	first := b.AddProposal(contractWorkspace, runtime.Proposal{Sandbox: name, Rule: rule, Summary: "allow pypi.test", Findings: []runtime.Finding{{Severity: "low", Message: "package index"}}})
	second := b.AddProposal(contractWorkspace, runtime.Proposal{Sandbox: name, Rule: rule, Summary: "allow pypi.test again"})
	list, err = c.ListProposals(ctx)
	require(t, err, "ListProposals")
	if ids := proposalIDs(list); !slices.Equal(ids, []string{first, second}) {
		t.Fatalf("ListProposals ids = %v, want [%s %s]", ids, first, second)
	}
	if p := list[0]; p.Sandbox != name || p.Rule.Name != rule.Name || p.Summary != "allow pypi.test" || len(p.Findings) != 1 {
		t.Fatalf("ListProposals[0] = %+v", p)
	}
	require(t, c.ApproveProposal(ctx, name, first), "ApproveProposal")
	wantKind(t, c.ApproveProposal(ctx, name, first), runtime.ErrNotFound, "ApproveProposal a second time")
	require(t, c.RejectProposal(ctx, name, second, "not needed"), "RejectProposal")
	wantKind(t, c.RejectProposal(ctx, name, second, "not needed"), runtime.ErrNotFound, "RejectProposal a second time")
	list, err = c.ListProposals(ctx)
	require(t, err, "ListProposals after the decisions")
	if len(list) != 0 {
		t.Fatalf("ListProposals after the decisions = %+v, want none", list)
	}
}

func contractWorkspaces(t *testing.T, b Backend) {
	a, other := b.Client(contractWorkspace, nil), b.Client(otherWorkspace, nil)
	ctx := contractContext(t)
	name := createSandbox(t, ctx, a, "sb-1")
	_, err := other.GetSandbox(ctx, name)
	wantKind(t, err, runtime.ErrNotFound, "GetSandbox of another workspace's sandbox")
	if names := sandboxNames(t, ctx, other); len(names) != 0 {
		t.Errorf("ListSandboxes of another workspace = %v, want none", names)
	}
	err = a.EnsureProvider(ctx, runtime.Provider{Name: "p", Profile: "anthropic", Value: "v"})
	if !skipped(t, err, "EnsureProvider") {
		require(t, err, "EnsureProvider")
		wantKind(t, other.DeleteProvider(ctx, "p"), runtime.ErrNotFound, "DeleteProvider of another workspace's provider")
	}
	err = a.InstallProviderProfile(ctx, runtime.ProviderProfile{ID: "pp", Variables: []string{"TOKEN"}})
	if !skipped(t, err, "InstallProviderProfile") {
		require(t, err, "InstallProviderProfile")
		_, err = other.GetProviderProfile(ctx, "pp")
		if !skipped(t, err, "GetProviderProfile") {
			wantKind(t, err, runtime.ErrNotFound, "GetProviderProfile of another workspace's profile")
		}
	}
}

func contractErrorKinds(t *testing.T, b Backend) {
	c := b.Client(contractWorkspace, nil)
	ctx := contractContext(t)
	name := createSandbox(t, ctx, c, "sb-1")
	rows := []struct {
		name string
		op   func() error
		want error
	}{
		{"get of an unknown sandbox", func() error { _, err := c.GetSandbox(ctx, "none"); return err }, runtime.ErrNotFound},
		{"delete of an unknown provider", func() error { return c.DeleteProvider(ctx, "none") }, runtime.ErrNotFound},
		{"get of an unknown provider profile", func() error { _, err := c.GetProviderProfile(ctx, "none"); return err }, runtime.ErrNotFound},
		{"stat of a missing path", func() error { _, err := c.Stat(ctx, name, "/w/none"); return err }, runtime.ErrNotFound},
		{"approve of an unknown proposal", func() error { return c.ApproveProposal(ctx, name, "none") }, runtime.ErrNotFound},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			err := row.op()
			skipIfUnsupported(t, err, row.name)
			wantKind(t, err, row.want, row.name)
		})
	}

	t.Run("validate of a policy the backend rejects", func(t *testing.T) {
		if b.RejectPolicies == nil {
			t.Skip("the backend cannot reject a policy")
		}
		b.RejectPolicies(func(p runtime.Policy) bool { return bytes.Contains(p.Base, []byte("deny-all")) }, "bad rule")
		_, err := c.ValidatePolicy(ctx, runtime.Policy{Base: []byte("deny-all\n")})
		skipIfUnsupported(t, err, "ValidatePolicy")
		wantKind(t, err, runtime.ErrInvalid, "ValidatePolicy of a rejected policy")
		if !strings.Contains(err.Error(), "bad rule") {
			t.Errorf("error %q does not keep the runtime's reason %q", err, "bad rule")
		}
		spec := contractSpec("sb-2")
		spec.Policy.Base = []byte("deny-all\n")
		_, err = c.CreateSandbox(ctx, spec)
		wantKind(t, err, runtime.ErrInvalid, "CreateSandbox with a rejected policy")
	})

	t.Run("every operation of an unreachable backend", func(t *testing.T) {
		b.SetUnreachable(true)
		defer b.SetUnreachable(false)
		for op, call := range everyOperation(c, name) {
			err := call(ctx)
			if errors.Is(err, runtime.ErrUnsupported) {
				continue
			}
			wantKind(t, err, runtime.ErrUnavailable, op)
		}
	})
}

func contractReachability(t *testing.T, b Backend) {
	rec := &Metrics{}
	c := b.Client(contractWorkspace, rec)
	ctx := contractContext(t)
	_, err := c.GetSandbox(ctx, "none")
	wantKind(t, err, runtime.ErrNotFound, "GetSandbox of an unknown sandbox")
	if !c.Reachable() {
		t.Fatal("Reachable() = false after a not found result")
	}
	b.SetUnreachable(true)
	_, err = c.ListSandboxes(ctx)
	wantKind(t, err, runtime.ErrUnavailable, "ListSandboxes of an unreachable backend")
	if c.Reachable() {
		t.Fatal("Reachable() = true after an unavailable result")
	}
	b.SetUnreachable(false)
	_, err = c.Release(ctx)
	require(t, err, "Release")
	if !c.Reachable() {
		t.Fatal("Reachable() = false after a successful Release")
	}
	want := []Metric{
		{Method: "set", Name: runtime.MetricUp, Value: 0},
		{Method: "set", Name: runtime.MetricUp, Value: 1},
	}
	if got := withoutLabels(t, rec.Calls()); !reflect.DeepEqual(got, want) {
		t.Fatalf("recorder calls = %+v, want %+v", got, want)
	}
}

func contractReachabilityConcurrency(t *testing.T, b Backend) {
	rec := &Metrics{}
	c := b.Client(contractWorkspace, rec)
	ctx := contractContext(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = c.Reachable()
			}
		}
	}()
	for i := range 50 {
		wg.Go(func() {
			for j := range 10 {
				if i == 0 && j%2 == 0 {
					b.SetUnreachable(j%4 == 0)
				}
				_, _ = c.GetSandbox(ctx, "none")
				_, _ = c.ListSandboxes(ctx)
				_, _ = c.Release(ctx)
			}
		})
	}
	wg.Wait()
	close(stop)
	<-readerDone
	b.SetUnreachable(false)
	_, err := c.Release(ctx)
	require(t, err, "Release")
	if !c.Reachable() {
		t.Fatal("Reachable() = false after a successful Release")
	}
	calls := withoutLabels(t, rec.Calls())
	for i, m := range calls {
		if m.Method != "set" || m.Name != runtime.MetricUp || (m.Value != 0 && m.Value != 1) {
			t.Fatalf("recorder call %d = %+v, want a set of %s to 0 or 1", i, m, runtime.MetricUp)
		}
		if i > 0 && calls[i-1].Value == m.Value {
			t.Fatalf("recorder calls %d and %d both set %v: a change was reported twice", i-1, i, m.Value)
		}
	}
	if n := len(calls); n > 0 && calls[n-1].Value != 1 {
		t.Fatalf("last recorder call = %+v, want a set to 1", calls[n-1])
	}
}

// everyOperation calls every operation of the client but Reachable once,
// by name, on the sandbox name.
func everyOperation(c runtime.Client, name string) map[string]func(ctx context.Context) error {
	policy := contractSpec(name).Policy
	req := runtime.ExecRequest{Command: []string{"true"}}
	return map[string]func(ctx context.Context) error{
		runtime.OpRelease:         func(ctx context.Context) error { _, err := c.Release(ctx); return err },
		runtime.OpEnsureWorkspace: c.EnsureWorkspace,
		runtime.OpGetProviderProfile: func(ctx context.Context) error {
			_, err := c.GetProviderProfile(ctx, "pp")
			return err
		},
		runtime.OpInstallProviderProfile: func(ctx context.Context) error {
			return c.InstallProviderProfile(ctx, runtime.ProviderProfile{ID: "pp"})
		},
		runtime.OpEnsureProvider: func(ctx context.Context) error {
			return c.EnsureProvider(ctx, runtime.Provider{Name: "p", Profile: "pp", Value: "v"})
		},
		runtime.OpDeleteProvider: func(ctx context.Context) error { return c.DeleteProvider(ctx, "p") },
		runtime.OpAttachProvider: func(ctx context.Context) error { return c.AttachProvider(ctx, name, "p") },
		runtime.OpDetachProvider: func(ctx context.Context) error { return c.DetachProvider(ctx, name, "p") },
		runtime.OpSandboxProviders: func(ctx context.Context) error {
			_, err := c.SandboxProviders(ctx, name)
			return err
		},
		runtime.OpListProviders: func(ctx context.Context) error { _, err := c.ListProviders(ctx); return err },
		runtime.OpValidatePolicy: func(ctx context.Context) error {
			_, err := c.ValidatePolicy(ctx, policy)
			return err
		},
		runtime.OpGetPolicy: func(ctx context.Context) error { _, err := c.GetPolicy(ctx, name); return err },
		runtime.OpAddPolicyRule: func(ctx context.Context) error {
			return c.AddPolicyRule(ctx, name, runtime.Rule{Name: "r", Body: json.RawMessage(`{}`)})
		},
		runtime.OpListProposals:   func(ctx context.Context) error { _, err := c.ListProposals(ctx); return err },
		runtime.OpApproveProposal: func(ctx context.Context) error { return c.ApproveProposal(ctx, name, "1") },
		runtime.OpRejectProposal:  func(ctx context.Context) error { return c.RejectProposal(ctx, name, "1", "no") },
		runtime.OpCreateSandbox: func(ctx context.Context) error {
			_, err := c.CreateSandbox(ctx, plainSpec("sb-new"))
			return err
		},
		runtime.OpGetSandbox:     func(ctx context.Context) error { _, err := c.GetSandbox(ctx, name); return err },
		runtime.OpListSandboxes:  func(ctx context.Context) error { _, err := c.ListSandboxes(ctx); return err },
		runtime.OpDeleteSandbox:  func(ctx context.Context) error { return c.DeleteSandbox(ctx, name) },
		runtime.OpStopSandbox:    func(ctx context.Context) error { return c.StopSandbox(ctx, name) },
		runtime.OpStartSandbox:   func(ctx context.Context) error { return c.StartSandbox(ctx, name, runtime.StartSpec{}) },
		runtime.OpWaitReady:      func(ctx context.Context) error { _, err := c.WaitReady(ctx, name); return err },
		runtime.OpWatchSandboxes: func(ctx context.Context) error { return closeIfOpen(c.WatchSandboxes(ctx)) },
		runtime.OpRunMain:        func(ctx context.Context) error { return c.RunMain(ctx, name, []string{"true"}) },
		runtime.OpExec:           func(ctx context.Context) error { _, err := c.Exec(ctx, name, req); return err },
		runtime.OpExecStream:     func(ctx context.Context) error { return closeIfOpen(c.ExecStream(ctx, name, req)) },
		runtime.OpExecInteractive: func(ctx context.Context) error {
			return closeIfOpen(c.ExecInteractive(ctx, name, req, runtime.TerminalSize{Cols: 80, Rows: 24}))
		},
		runtime.OpUpload: func(ctx context.Context) error {
			return c.Upload(ctx, name, "/w/a", 0o644, strings.NewReader("x"))
		},
		runtime.OpMakeDir:  func(ctx context.Context) error { return c.MakeDir(ctx, name, "/w/d", 0o755) },
		runtime.OpStat:     func(ctx context.Context) error { _, err := c.Stat(ctx, name, "/w"); return err },
		runtime.OpDownload: func(ctx context.Context) error { return closeIfOpen(c.Download(ctx, name, "/w/a")) },
		runtime.OpWalk:     func(ctx context.Context) error { _, err := c.Walk(ctx, name, "/w"); return err },
		runtime.OpLogs: func(ctx context.Context) error {
			return closeIfOpen(c.Logs(ctx, name, runtime.LogQuery{}))
		},
	}
}

func closeIfOpen[S interface{ Close() error }](s S, err error) error {
	if err == nil {
		_ = s.Close()
	}
	return err
}

// contractSpec is the sandbox as the hub creates one: plainSpec in the
// working directory /w, with a base document in its policy value. The tests
// of what a sandbox is created with, and of its files, use it.
func contractSpec(name string) runtime.SandboxSpec {
	spec := plainSpec(name)
	spec.WorkDir = "/w"
	spec.Policy.Base = []byte("version: 1\n")
	return spec
}

// plainSpec is a sandbox with an environment, labels and a policy value of
// one protected rule, and neither a working directory nor a base document:
// the sandbox of the tests whose subject is not what a sandbox is created
// with, so that they hold for a runtime that cannot create it in a working
// directory or with a base document.
func plainSpec(name string) runtime.SandboxSpec {
	return runtime.SandboxSpec{
		Name: name,
		Env:  map[string]string{"PARLEY_SANDBOX": name},
		Policy: runtime.Policy{
			Protected: []runtime.Rule{{
				Name: "parley-hub",
				Body: json.RawMessage(`{"endpoints":[{"host":"hub.test","port":443}]}`),
			}},
		},
		Labels: map[string]string{"parley.sandbox": name},
	}
}

func contractContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// createSandbox creates the sandbox plainSpec(name) and returns its name.
func createSandbox(t *testing.T, ctx context.Context, c runtime.Client, name string) string {
	t.Helper()
	_, err := c.CreateSandbox(ctx, plainSpec(name))
	skipIfUnsupported(t, err, "CreateSandbox")
	require(t, err, "CreateSandbox")
	return name
}

// readySandbox creates the sandbox contractSpec(name), in the working
// directory the file tests use, waits until it is ready and returns its
// name.
func readySandbox(t *testing.T, ctx context.Context, c runtime.Client, name string) string {
	t.Helper()
	_, err := c.CreateSandbox(ctx, contractSpec(name))
	skipIfUnsupported(t, err, "CreateSandbox with a working directory and a base document")
	require(t, err, "CreateSandbox")
	_, err = c.WaitReady(ctx, name)
	if !skipped(t, err, "WaitReady") {
		require(t, err, "WaitReady")
	}
	return name
}

func getSandbox(t *testing.T, ctx context.Context, c runtime.Client, name string) runtime.SandboxStatus {
	t.Helper()
	st, err := c.GetSandbox(ctx, name)
	require(t, err, "GetSandbox")
	return st
}

// eventually polls a sandbox's status until cond holds or ctx ends.
func eventually(t *testing.T, ctx context.Context, c runtime.Client, name string, cond func(runtime.SandboxStatus) bool) runtime.SandboxStatus {
	t.Helper()
	for {
		st := getSandbox(t, ctx, c, name)
		if cond(st) {
			return st
		}
		select {
		case <-ctx.Done():
			t.Fatalf("sandbox %q never reached the awaited status; last %+v", name, st)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func sandboxNames(t *testing.T, ctx context.Context, c runtime.Client) []string {
	t.Helper()
	list, err := c.ListSandboxes(ctx)
	require(t, err, "ListSandboxes")
	names := make([]string, len(list))
	for i, st := range list {
		names[i] = st.Name
	}
	return names
}

func wantProviders(t *testing.T, ctx context.Context, c runtime.Client, sandbox string, want []string) {
	t.Helper()
	got, err := c.SandboxProviders(ctx, sandbox)
	if skipped(t, err, "SandboxProviders") {
		return
	}
	require(t, err, "SandboxProviders")
	if !slices.Equal(got, want) {
		t.Errorf("SandboxProviders = %v, want %v", got, want)
	}
}

func baseNames(infos []runtime.FileInfo) []string {
	names := make([]string, len(infos))
	for i, fi := range infos {
		names[i] = path.Base(fi.Path)
	}
	return names
}

func proposalIDs(list []runtime.Proposal) []string {
	ids := make([]string, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	return ids
}

// withoutLabels checks that no call carries a label and drops the empty
// label maps, so calls compare by method, name and value.
func withoutLabels(t *testing.T, calls []Metric) []Metric {
	t.Helper()
	for i := range calls {
		if len(calls[i].Labels) != 0 {
			t.Fatalf("recorder call %+v carries labels", calls[i])
		}
		calls[i].Labels = nil
	}
	return calls
}

// samePolicy compares policy values: the base documents byte for byte, the
// rules by name and by the JSON value of their bodies.
func samePolicy(a, b runtime.Policy) bool {
	return bytes.Equal(a.Base, b.Base) && sameRules(a.Protected, b.Protected) && sameRules(a.Added, b.Added)
}

func sameRules(a, b []runtime.Rule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || !sameJSON(a[i].Body, b[i].Body) {
			return false
		}
	}
	return true
}

func sameJSON(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(va, vb)
}

func require(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// wantKind fails unless err is of exactly one of the four kinds, and that
// kind is want.
func wantKind(t *testing.T, err, want error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error, want one of kind %q", what, want)
	}
	var held []string
	for _, kind := range []error{runtime.ErrNotFound, runtime.ErrInvalid, runtime.ErrUnsupported, runtime.ErrUnavailable} {
		if errors.Is(err, kind) {
			held = append(held, kind.Error())
		}
	}
	if len(held) != 1 || !errors.Is(err, want) {
		t.Fatalf("%s: error %q is of kinds %q, want exactly %q", what, err, held, want)
	}
}

// skipIfUnsupported skips the rest of the test when err says the backend
// lacks the operation.
func skipIfUnsupported(t *testing.T, err error, op string) {
	t.Helper()
	if errors.Is(err, runtime.ErrUnsupported) {
		t.Skipf("the backend lacks %s: %v", op, err)
	}
}

// skipped reports whether err says the backend lacks the operation, so the
// caller leaves out the assertions about it.
func skipped(t *testing.T, err error, op string) bool {
	t.Helper()
	if errors.Is(err, runtime.ErrUnsupported) {
		t.Logf("the backend lacks %s: %v", op, err)
		return true
	}
	return false
}
