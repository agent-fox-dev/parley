package openshell_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/fake"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/runtimetest"
	"github.com/agent-fox-dev/parley/internal/scrub"
)

func TestClient_PassesTheContract(t *testing.T) {
	runtimetest.RunContract(t, func(t *testing.T) runtimetest.Backend {
		// Over the SDK's in-memory client alone: it keeps no provider
		// profiles, so the operations that need one are skipped here and
		// tested over the test gateway's profiles below.
		g := newGateway()
		g.noProfiles = true
		return runtimetest.Backend{
			Client: func(workspace string, rec runtime.Recorder) runtime.Client {
				return openshell.NewWithClient(g, workspace, openshell.Deps{Scrubber: scrub.New(), Recorder: rec})
			},
			SetUnreachable: g.SetUnreachable,
		}
	})
}

func TestClient_ActsOnProvidersInItsWorkspaceOnly(t *testing.T) {
	s := newGateway()
	a := openshell.NewWithClient(s, "parley", openshell.Deps{})
	b := openshell.NewWithClient(s, "other", openshell.Deps{})
	ctx := t.Context()
	require(t, a.InstallProviderProfile(ctx, runtime.ProviderProfile{
		ID:        "anthropic",
		Variables: []string{"ANTHROPIC_API_KEY"},
		Endpoints: []runtime.Endpoint{{Host: "api.anthropic.test", Port: 443}},
	}), "InstallProviderProfile")
	_, err := b.GetProviderProfile(ctx, "anthropic")
	wantOneKind(t, err, runtime.ErrNotFound)
	got, err := a.GetProviderProfile(ctx, "anthropic")
	require(t, err, "GetProviderProfile")
	want := runtime.ProviderProfile{ID: "anthropic", Variables: []string{"ANTHROPIC_API_KEY"}, Endpoints: []runtime.Endpoint{{Host: "api.anthropic.test", Port: 443}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetProviderProfile = %+v, want %+v", got, want)
	}

	require(t, a.EnsureProvider(ctx, runtime.Provider{Name: "p", Profile: "anthropic", Value: "v", Labels: map[string]string{"parley.sandbox": "sb-1"}}), "EnsureProvider")
	stored, err := s.Fake().Providers().Get(ctx, "parley", "p")
	require(t, err, "reading the provider from the SDK")
	if stored.Type != "anthropic" || !reflect.DeepEqual(stored.Spec.Credentials, map[string]string{"anthropic": "v"}) {
		t.Errorf("SDK provider = type %q credentials %v, want type anthropic with the value under the profile's credential", stored.Type, stored.Spec.Credentials)
	}
	wantOneKind(t, b.EnsureProvider(ctx, runtime.Provider{Name: "q", Profile: "anthropic", Value: "v"}), runtime.ErrNotFound)
	wantOneKind(t, b.DeleteProvider(ctx, "p"), runtime.ErrNotFound)
	if list, err := b.ListProviders(ctx); err != nil || len(list) != 0 {
		t.Errorf("ListProviders of another workspace = %+v, %v; want none", list, err)
	}

	_, err = a.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1"})
	require(t, err, "CreateSandbox")
	wantOneKind(t, b.AttachProvider(ctx, "sb-1", "p"), runtime.ErrNotFound)
	require(t, a.AttachProvider(ctx, "sb-1", "p"), "AttachProvider")
	if _, err := b.SandboxProviders(ctx, "sb-1"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("SandboxProviders of another workspace's sandbox = %v, want not found", err)
	}
}

func TestClient_InstallsAProviderProfileWithTheEndpointsHostPortAndScheme(t *testing.T) {
	s := newGateway()
	c := openshell.NewWithClient(s, "parley", openshell.Deps{})
	ctx := t.Context()
	p := runtime.ProviderProfile{
		ID:        "parley-hub",
		Variables: []string{"PARLEY_API_KEY"},
		Endpoints: []runtime.Endpoint{{Scheme: "https", Host: "hub.test", Port: 443, Paths: []string{"/api/v1/", "/git/"}}},
	}
	require(t, c.InstallProviderProfile(ctx, p), "InstallProviderProfile")
	installed, ok := s.Profile("parley", "parley-hub")
	if !ok {
		t.Fatal("the profile was not installed")
	}
	if want := []v1.NetworkEndpoint{{Host: "hub.test", Port: 443, Protocol: "https"}}; !reflect.DeepEqual(installed.Endpoints, want) {
		t.Errorf("installed endpoints = %+v, want %+v (no paths)", installed.Endpoints, want)
	}
	if want := []v1.ProfileCredential{{Name: "parley-hub", EnvVars: []string{"PARLEY_API_KEY"}, Required: true}}; !reflect.DeepEqual(installed.Credentials, want) {
		t.Errorf("installed credentials = %+v, want %+v", installed.Credentials, want)
	}
	got, err := c.GetProviderProfile(ctx, "parley-hub")
	require(t, err, "GetProviderProfile")
	want := runtime.ProviderProfile{ID: "parley-hub", Variables: []string{"PARLEY_API_KEY"}, Endpoints: []runtime.Endpoint{{Scheme: "https", Host: "hub.test", Port: 443}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetProviderProfile = %+v, want %+v", got, want)
	}

	p.Variables = []string{"PARLEY_API_KEY", "PARLEY_TOKEN"}
	require(t, c.InstallProviderProfile(ctx, p), "InstallProviderProfile replacing it")
	got, err = c.GetProviderProfile(ctx, "parley-hub")
	require(t, err, "GetProviderProfile after replacing it")
	if !reflect.DeepEqual(got.Variables, p.Variables) {
		t.Errorf("variables after replacing = %v, want %v", got.Variables, p.Variables)
	}
}

func TestClient_EnsuresListsAttachesAndDetachesAProvider(t *testing.T) {
	s := newGateway()
	s.AddProfile("parley", v1.ProviderProfile{ID: "anthropic", Credentials: []v1.ProfileCredential{{Name: "api_key", EnvVars: []string{"ANTHROPIC_API_KEY"}}}})
	c := openshell.NewWithClient(s, "parley", openshell.Deps{})
	ctx := t.Context()
	_, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1"})
	require(t, err, "CreateSandbox")
	labels := map[string]string{"parley.sandbox": "sb-1"}
	require(t, c.EnsureProvider(ctx, runtime.Provider{Name: "p", Profile: "anthropic", Value: "v1", Labels: labels}), "EnsureProvider")
	require(t, c.EnsureProvider(ctx, runtime.Provider{Name: "p", Profile: "anthropic", Value: "v2", Labels: labels}), "EnsureProvider again")
	stored, err := s.Fake().Providers().Get(ctx, "parley", "p")
	require(t, err, "reading the provider from the SDK")
	if !reflect.DeepEqual(stored.Spec.Credentials, map[string]string{"api_key": "v2"}) {
		t.Errorf("SDK credentials = %v, want the last value under the profile's credential", stored.Spec.Credentials)
	}
	list, err := c.ListProviders(ctx)
	require(t, err, "ListProviders")
	if want := []runtime.Provider{{Name: "p", Profile: "anthropic", Labels: labels}}; !reflect.DeepEqual(list, want) {
		t.Errorf("ListProviders = %+v, want %+v", list, want)
	}
	require(t, c.AttachProvider(ctx, "sb-1", "p"), "AttachProvider")
	if got, err := c.SandboxProviders(ctx, "sb-1"); err != nil || !reflect.DeepEqual(got, []string{"p"}) {
		t.Errorf("SandboxProviders = %v, %v; want [p]", got, err)
	}
	require(t, c.DetachProvider(ctx, "sb-1", "p"), "DetachProvider")
	if got, err := c.SandboxProviders(ctx, "sb-1"); err != nil || len(got) != 0 {
		t.Errorf("SandboxProviders after DetachProvider = %v, %v; want none", got, err)
	}
	wantOneKind(t, c.AttachProvider(ctx, "sb-1", "none"), runtime.ErrNotFound)
	require(t, c.DeleteProvider(ctx, "p"), "DeleteProvider")
	wantOneKind(t, c.DeleteProvider(ctx, "p"), runtime.ErrNotFound)

	s.AddProfile("parley", v1.ProviderProfile{ID: "two", Credentials: []v1.ProfileCredential{{Name: "a"}, {Name: "b"}}})
	wantOneKind(t, c.EnsureProvider(ctx, runtime.Provider{Name: "q", Profile: "two", Value: "v"}), runtime.ErrInvalid)
}

func TestClient_EnsuresItsWorkspace(t *testing.T) {
	s := newGateway()
	c := openshell.NewWithClient(s, "parley", openshell.Deps{})
	ctx := t.Context()
	for range 2 {
		require(t, c.EnsureWorkspace(ctx), "EnsureWorkspace")
	}
	if _, err := s.Fake().Workspaces().Get(ctx, "parley"); err != nil {
		t.Fatalf("workspace parley after EnsureWorkspace: %v", err)
	}
	if list, _ := s.Fake().Workspaces().ListAll(ctx); len(list) != 1 {
		t.Fatalf("workspaces = %d, want 1", len(list))
	}
}

func TestClient_ReportsTheGatewaysHealthAndRelease(t *testing.T) {
	s := newGateway(fake.WithGatewayInfo(&v1.GatewayInfo{Version: "0.1.8"}))
	c := openshell.NewWithClient(s, "parley", openshell.Deps{})
	got, err := c.Release(t.Context())
	require(t, err, "Release")
	if want := (runtime.Release{Healthy: true, Version: "0.1.8"}); got != want {
		t.Fatalf("Release = %+v, want %+v", got, want)
	}
}

func TestClient_CreatesASandboxWithWhatTheSDKCanCarry(t *testing.T) {
	s := newGateway()
	c := openshell.NewWithClient(s, "parley", openshell.Deps{})
	ctx := t.Context()
	spec := runtime.SandboxSpec{
		Name:      "sb-1",
		Image:     "registry.test/agent:1",
		CPU:       "2",
		Memory:    "4Gi",
		GPU:       1,
		WorkDir:   "/w",
		Env:       map[string]string{"PARLEY_SANDBOX": "sb-1"},
		Providers: []string{"p"},
		Labels:    map[string]string{"parley.sandbox": "sb-1"},
		Policy: runtime.Policy{
			Base:      []byte("version: 1\n"),
			Protected: []runtime.Rule{{Name: "parley-hub", Body: json.RawMessage(`{"endpoints":[{"host":"hub.test","port":443}]}`)}},
			Added:     []runtime.Rule{{Name: "pypi", Body: json.RawMessage(`{"endpoints":[{"host":"pypi.test","port":443}],"binaries":[{"path":"/usr/bin/pip"}]}`)}},
		},
	}
	st, err := c.CreateSandbox(ctx, spec)
	require(t, err, "CreateSandbox")
	if st.Name != "sb-1" || st.State != runtime.StatePending || !reflect.DeepEqual(st.Labels, spec.Labels) {
		t.Errorf("CreateSandbox = %+v", st)
	}
	sb, err := s.Fake().Sandboxes().Get(ctx, "parley", "sb-1")
	require(t, err, "reading the sandbox from the SDK")
	gpus := uint32(1)
	want := v1.SandboxSpec{
		Environment: map[string]string{"PARLEY_SANDBOX": "sb-1"},
		Template: &v1.SandboxTemplate{
			Image:     "registry.test/agent:1",
			Resources: map[string]any{"limits": map[string]any{"cpu": "2", "memory": "4Gi"}},
		},
		Providers: []string{"p"},
		GPU:       true,
		GPUCount:  &gpus,
		Policy: &v1.SandboxPolicy{NetworkPolicies: map[string]v1.NetworkPolicyRule{
			"parley-hub": {Name: "parley-hub", Endpoints: []v1.PolicyNetworkEndpoint{{Host: "hub.test", Port: 443}}},
			"pypi":       {Name: "pypi", Endpoints: []v1.PolicyNetworkEndpoint{{Host: "pypi.test", Port: 443}}, Binaries: []v1.PolicyNetworkBinary{{Path: "/usr/bin/pip"}}},
		}},
	}
	if !reflect.DeepEqual(sb.Spec, want) {
		t.Errorf("SDK spec = %+v\nwant %+v", sb.Spec, want)
	}

	rows := []struct {
		name   string
		policy runtime.Policy
		want   error
	}{
		{
			name:   "a rule the SDK's network rule cannot hold",
			policy: runtime.Policy{Protected: []runtime.Rule{{Name: "r", Body: json.RawMessage(`{"endpoints":[{"host":"x.test","port":443,"allowed_ips":["10.0.0.0/8"]}]}`)}}},
			want:   runtime.ErrUnsupported,
		},
		{
			name: "an added rule named like a protected one",
			policy: runtime.Policy{
				Protected: []runtime.Rule{{Name: "parley-hub", Body: json.RawMessage(`{}`)}},
				Added:     []runtime.Rule{{Name: "parley-hub", Body: json.RawMessage(`{}`)}},
			},
			want: runtime.ErrInvalid,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-2", Policy: row.policy})
			wantOneKind(t, err, row.want)
			if _, err := s.Fake().Sandboxes().Get(ctx, "parley", "sb-2"); !v1.IsNotFound(err) {
				t.Errorf("the sandbox reached the SDK: %v", err)
			}
		})
	}
}

func TestClient_GivesEveryCallThatDoesNotStreamAtMostThirtySeconds(t *testing.T) {
	t.Run("a call blocked until its context ends returns when the caller's context ends", func(t *testing.T) {
		s := newGateway()
		s.sandboxes = sandboxesStub{SandboxInterface: s.Fake().Sandboxes(), get: func(ctx context.Context, _, _ string) (*v1.Sandbox, error) {
			<-ctx.Done()
			return nil, &v1.StatusError{Code: v1.ErrorDeadlineExceeded, Message: ctx.Err().Error()}
		}}
		c := openshell.NewWithClient(s, "parley", openshell.Deps{})
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := c.GetSandbox(ctx, "sb-1")
		wantOneKind(t, err, runtime.ErrUnavailable)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("GetSandbox returned after %v, want within 1s of the caller's 50ms", elapsed)
		}
	})

	t.Run("a call that ignores its context is not waited for", func(t *testing.T) {
		s := newGateway()
		release := make(chan struct{})
		defer close(release)
		s.sandboxes = sandboxesStub{SandboxInterface: s.Fake().Sandboxes(), get: func(context.Context, string, string) (*v1.Sandbox, error) {
			<-release
			return nil, errors.New("too late")
		}}
		c := openshell.NewWithClient(s, "parley", openshell.Deps{})
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := c.GetSandbox(ctx, "sb-1")
		wantOneKind(t, err, runtime.ErrUnavailable)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("GetSandbox returned after %v, want within 1s of the caller's 50ms", elapsed)
		}
	})

	t.Run("without a deadline of the caller the call has 30 seconds", func(t *testing.T) {
		s := newGateway()
		var deadline time.Time
		var ok bool
		s.sandboxes = sandboxesStub{SandboxInterface: s.Fake().Sandboxes(), get: func(ctx context.Context, _, name string) (*v1.Sandbox, error) {
			deadline, ok = ctx.Deadline()
			return &v1.Sandbox{Name: name}, nil
		}}
		c := openshell.NewWithClient(s, "parley", openshell.Deps{})
		called := time.Now()
		_, err := c.GetSandbox(context.Background(), "sb-1")
		returned := time.Now()
		require(t, err, "GetSandbox")
		if !ok {
			t.Fatal("the SDK call had no deadline")
		}
		if deadline.Sub(returned) > 30*time.Second || deadline.Sub(called) < 29*time.Second {
			t.Fatalf("the SDK call's deadline was %v after the call began, want 30s", deadline.Sub(called))
		}
	})

	t.Run("a stream has no deadline and ends with the caller's context", func(t *testing.T) {
		s := newGateway()
		var mu sync.Mutex
		var hasDeadline bool
		s.exec = execStub{ExecInterface: s.Fake().Exec(), stream: func(ctx context.Context, _, _ string, _ []string) (v1.ExecStream, error) {
			mu.Lock()
			_, hasDeadline = ctx.Deadline()
			mu.Unlock()
			return &blockingStream{ctx: ctx}, nil
		}}
		c := openshell.NewWithClient(s, "parley", openshell.Deps{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stream, err := c.ExecStream(ctx, "sb-1", runtime.ExecRequest{Command: []string{"sleep", "100"}})
		require(t, err, "ExecStream")
		defer stream.Close()
		mu.Lock()
		if hasDeadline {
			t.Error("the stream's context has a deadline")
		}
		mu.Unlock()
		done := make(chan error, 1)
		go func() { _, err := stream.Next(); done <- err }()
		cancel()
		select {
		case err := <-done:
			if err != io.EOF {
				t.Fatalf("Next after the caller's context ended = %v, want io.EOF", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the stream did not end with the caller's context")
		}
	})
}

func TestClient_DeliversAStreamsOutputAndExitCode(t *testing.T) {
	s := newGateway()
	var gotCommand []string
	s.exec = execStub{ExecInterface: s.Fake().Exec(), stream: func(_ context.Context, workspace, sandbox string, command []string) (v1.ExecStream, error) {
		if workspace != "parley" || sandbox != "sb-1" {
			return nil, &v1.StatusError{Code: v1.ErrorNotFound, Message: sandbox + " not found"}
		}
		gotCommand = command
		return &scriptedStream{chunks: []v1.ExecChunk{
			{Stream: v1.StreamStdout, Data: []byte("hi\n")},
			{Stream: v1.StreamStderr, Data: []byte("oops\n")},
		}, code: 4}, nil
	}}
	c := openshell.NewWithClient(s, "parley", openshell.Deps{})
	stream, err := c.ExecStream(t.Context(), "sb-1", runtime.ExecRequest{Command: []string{"sh", "-c", "x"}})
	require(t, err, "ExecStream")
	defer stream.Close()
	var got []runtime.Output
	for {
		out, err := stream.Next()
		if err == io.EOF {
			break
		}
		require(t, err, "Next")
		got = append(got, out)
	}
	want := []runtime.Output{{Stream: runtime.Stdout, Data: []byte("hi\n")}, {Stream: runtime.Stderr, Data: []byte("oops\n")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("output = %+v, want %+v", got, want)
	}
	if code, err := stream.ExitCode(); err != nil || code != 4 {
		t.Errorf("ExitCode = %d, %v; want 4", code, err)
	}
	if !reflect.DeepEqual(gotCommand, []string{"sh", "-c", "x"}) {
		t.Errorf("SDK command = %q", gotCommand)
	}
	_, err = c.ExecStream(t.Context(), "none", runtime.ExecRequest{Command: []string{"true"}})
	wantOneKind(t, err, runtime.ErrNotFound)
}

func TestClient_TransfersAFileByteForByteThroughATemporaryFile(t *testing.T) {
	s := newGateway()
	files := newFilesStub()
	s.files = files
	c := openshell.NewWithClient(s, "parley", openshell.Deps{})
	ctx := t.Context()
	data := []byte("line one\nline two\x00\xff\n")
	require(t, c.Upload(ctx, "sb-1", "/w/a", 0o640, bytes.NewReader(data)), "Upload")
	if got := files.modes["parley/sb-1/w/a"]; got != 0o640 {
		t.Errorf("uploaded mode = %v, want 0640", got)
	}
	rc, err := c.Download(ctx, "sb-1", "/w/a")
	require(t, err, "Download")
	got, err := io.ReadAll(rc)
	require(t, err, "reading the download")
	require(t, rc.Close(), "closing the download")
	if !bytes.Equal(got, data) {
		t.Errorf("Download = %q, want %q", got, data)
	}
	for _, local := range files.locals {
		if _, err := os.Stat(local); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("temporary file %s is left behind: %v", local, err)
		}
	}
	_, err = c.Download(ctx, "sb-1", "/w/none")
	wantOneKind(t, err, runtime.ErrNotFound)
}

// blockingStream is an exec stream that produces nothing until its context
// ends.
type blockingStream struct{ ctx context.Context }

func (b *blockingStream) Next() (*v1.ExecChunk, error) {
	<-b.ctx.Done()
	return nil, &v1.StatusError{Code: v1.ErrorCancelled, Message: "context canceled", Cause: b.ctx.Err()}
}

func (b *blockingStream) ExitCode() (int, error) { return 0, errors.New("no exit code") }
func (b *blockingStream) Close() error           { return nil }

func TestClient_ScrubsTheGatewayCredentialsFromEveryError(t *testing.T) {
	is := newIssuer(t)
	rows := []struct {
		name   string
		keys   string
		files  map[string]string
		secret string
	}{
		{
			name:   "the gateway token",
			keys:   "gateway_url = \"https://gw.test:8443\"\ntoken_file = \"token\"",
			files:  map[string]string{"token": "gw-tok-SECRET\n"},
			secret: "gw-tok-SECRET",
		},
		{
			name:   "the OIDC client secret",
			keys:   "gateway_url = \"https://gw.test:8443\"\noidc_issuer = \"" + is.URL + "\"\noidc_client_id = \"parley\"\noidc_client_secret_file = \"secret\"",
			files:  map[string]string{"secret": "oidc-SECRET"},
			secret: "oidc-SECRET",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			settings := loadSettings(t, row.keys, row.files)
			s := newGateway()
			failingSandboxes(s, &v1.StatusError{Code: v1.ErrorUnauthenticated, Message: "auth " + row.secret + " refused"})
			scrubber := scrub.New()
			c, err := openshell.New(settings, openshell.Deps{
				Scrubber: scrubber,
				Dial:     func(v1.Config) (v1.ClientInterface, error) { return s, nil },
			})
			require(t, err, "New")
			_, err = c.GetSandbox(t.Context(), "sb-1")
			wantOneKind(t, err, runtime.ErrUnavailable)
			if strings.Contains(err.Error(), row.secret) {
				t.Errorf("error %q holds the secret", err)
			}
			if got := scrubber.Scrub("x " + row.secret + " y"); got != "x "+scrub.Marker+" y" {
				t.Errorf("Scrub after New = %q, want the secret replaced by %s", got, scrub.Marker)
			}

			_, err = openshell.New(settings, openshell.Deps{
				Scrubber: scrubber,
				Dial: func(v1.Config) (v1.ClientInterface, error) {
					return nil, errors.New("dial with " + row.secret + " failed")
				},
			})
			if err == nil || strings.Contains(err.Error(), row.secret) {
				t.Errorf("New with a failing dial = %v, want an error without the secret", err)
			}
		})
	}
}

func require(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
