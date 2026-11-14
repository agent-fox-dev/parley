package openshell_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell"
	"github.com/agent-fox-dev/parley/internal/scrub"
)

// failingSandboxes answers Get and Create with err.
func failingSandboxes(s *gateway, err error) {
	s.sandboxes = sandboxesStub{
		SandboxInterface: s.Fake().Sandboxes(),
		get:              func(context.Context, string, string) (*v1.Sandbox, error) { return nil, err },
		create: func(context.Context, string, string, *v1.SandboxSpec) (*v1.Sandbox, error) {
			return nil, err
		},
	}
}

// errorKindRow is an operation's result of one kind: the gateway set up to
// give it, the operation, the kind and a part of the runtime's reason the
// error keeps ("" for none).
type errorKindRow struct {
	name  string
	setup func(s *gateway)
	op    func(ctx context.Context, c runtime.Client) error
	want  error
	text  string
}

func TestClient_ReturnsAnErrorOfExactlyOneKind(t *testing.T) {
	t.Parallel()
	for _, row := range errorKindRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newGateway()
			if row.setup != nil {
				row.setup(s)
			}
			c := openshell.NewWithClient(s, "parley", openshell.Deps{})
			err := row.op(t.Context(), c)
			wantOneKind(t, err, row.want)
			if row.text != "" && !strings.Contains(err.Error(), row.text) {
				t.Errorf("error %q does not keep the runtime's reason %q", err, row.text)
			}
		})
	}
}

// errorKindRows are the results of every kind the client gives, with the
// gateway's answers that map to each.
func errorKindRows() []errorKindRow {
	getSandbox := func(ctx context.Context, c runtime.Client) error { _, err := c.GetSandbox(ctx, "none"); return err }
	createSandbox := func(ctx context.Context, c runtime.Client) error {
		_, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1", Image: "registry.test/no such image"})
		return err
	}
	return []errorKindRow{
		{name: "get of an unknown sandbox", op: getSandbox, want: runtime.ErrNotFound},
		{
			name: "delete of an unknown provider",
			op:   func(ctx context.Context, c runtime.Client) error { return c.DeleteProvider(ctx, "none") },
			want: runtime.ErrNotFound,
		},
		{
			name: "get of an unknown provider profile",
			op: func(ctx context.Context, c runtime.Client) error {
				_, err := c.GetProviderProfile(ctx, "none")
				return err
			},
			want: runtime.ErrNotFound,
		},
		{
			name: "create with a policy the backend rejects",
			setup: func(s *gateway) {
				failingSandboxes(s, &v1.StatusError{Code: v1.ErrorInvalidArgument, Message: "policy: bad rule"})
			},
			op:   createSandbox,
			want: runtime.ErrInvalid,
			text: "bad rule",
		},
		{
			name: "create with an image reference the backend refuses",
			setup: func(s *gateway) {
				failingSandboxes(s, &v1.StatusError{Code: v1.ErrorInvalidArgument, Message: "invalid image reference", FieldViolations: []v1.FieldViolation{{Field: "template.image", Description: "not a reference"}}})
			},
			op:   createSandbox,
			want: runtime.ErrInvalid,
			text: "template.image: not a reference",
		},
		{
			name: "create of a sandbox that exists",
			setup: func(s *gateway) {
				_, _ = s.Fake().Sandboxes().Create(context.Background(), "parley", "sb-1", &v1.SandboxSpec{})
			},
			op:   createSandbox,
			want: runtime.ErrInvalid,
			text: "already exists",
		},
		{
			name: "backend answers unauthenticated",
			setup: func(s *gateway) {
				failingSandboxes(s, &v1.StatusError{Code: v1.ErrorUnauthenticated, Message: "token expired"})
			},
			op:   getSandbox,
			want: runtime.ErrUnavailable,
		},
		{
			name: "backend refuses the hub credential",
			setup: func(s *gateway) {
				failingSandboxes(s, &v1.StatusError{Code: v1.ErrorPermissionDenied, Message: "denied"})
			},
			op:   getSandbox,
			want: runtime.ErrUnavailable,
		},
		{
			name:  "backend answers a server error",
			setup: func(s *gateway) { failingSandboxes(s, &v1.StatusError{Code: v1.ErrorInternal, Message: "boom"}) },
			op:    getSandbox,
			want:  runtime.ErrUnavailable,
		},
		{
			name:  "backend is unavailable",
			setup: func(s *gateway) { s.SetUnreachable(true) },
			op:    getSandbox,
			want:  runtime.ErrUnavailable,
		},
		{
			name:  "backend fails without a status",
			setup: func(s *gateway) { failingSandboxes(s, errors.New("stream reset")) },
			op:    getSandbox,
			want:  runtime.ErrUnavailable,
		},
		{
			name: "an operation the backend lacks",
			op: func(ctx context.Context, c runtime.Client) error {
				_, err := c.Exec(ctx, "sb-1", runtime.ExecRequest{Command: []string{"true"}})
				return err
			},
			want: runtime.ErrUnsupported,
		},
		{
			name: "an operation the SDK lacks",
			op: func(ctx context.Context, c runtime.Client) error {
				_, err := c.Stat(ctx, "sb-1", "/w/none")
				return err
			},
			want: runtime.ErrUnsupported,
		},
	}
}

func TestClient_TakesARefusedConnectionForAnUnavailableRuntime(t *testing.T) {
	// Serial: the loopback port it frees must stay closed until the client dials it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	settings := loadSettings(t, "gateway_url = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\ntoken_file = \"token\"", map[string]string{"token": "tok"})
	c, err := openshell.New(settings, openshell.Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Release(t.Context())
	wantOneKind(t, err, runtime.ErrUnavailable)
	if c.Reachable() {
		t.Error("Reachable() = true after a refused connection")
	}
}

func TestClient_AnswersWhatTheSDKCannotDoAsUnsupportedWithoutCallingIt(t *testing.T) {
	t.Parallel()
	s := newGateway()
	// Every call that reaches the SDK fails as unavailable, so an
	// operation that answers unsupported has not called it.
	s.SetUnreachable(true)
	c := openshell.NewWithClient(s, "parley", openshell.Deps{Scrubber: scrub.New()})
	ctx := t.Context()
	ops := map[string]func() error{
		runtime.OpValidatePolicy: func() error {
			_, err := c.ValidatePolicy(ctx, runtime.Policy{Base: []byte("version: 1\n")})
			return err
		},
		runtime.OpGetPolicy:      func() error { _, err := c.GetPolicy(ctx, "sb-1"); return err },
		runtime.OpWatchSandboxes: func() error { _, err := c.WatchSandboxes(ctx); return err },
		runtime.OpRunMain:        func() error { return c.RunMain(ctx, "sb-1", []string{"true"}) },
		runtime.OpMakeDir:        func() error { return c.MakeDir(ctx, "sb-1", "/w/d", 0o755) },
		runtime.OpStat:           func() error { _, err := c.Stat(ctx, "sb-1", "/w"); return err },
		runtime.OpWalk:           func() error { _, err := c.Walk(ctx, "sb-1", "/w"); return err },
		runtime.OpLogs:           func() error { _, err := c.Logs(ctx, "sb-1", runtime.LogQuery{}); return err },
		runtime.OpExec + " with stdin": func() error {
			_, err := c.Exec(ctx, "sb-1", runtime.ExecRequest{Command: []string{"cat"}, Stdin: []byte("in")})
			return err
		},
		runtime.OpCreateSandbox + " with a base document": func() error {
			_, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1", Policy: runtime.Policy{Base: []byte("version: 1\n")}})
			return err
		},
		runtime.OpCreateSandbox + " in a working directory": func() error {
			_, err := c.CreateSandbox(ctx, runtime.SandboxSpec{Name: "sb-1", WorkDir: "/w"})
			return err
		},
		runtime.OpStartSandbox + " with a policy value": func() error {
			return c.StartSandbox(ctx, "sb-1", runtime.StartSpec{Policy: runtime.Policy{Added: []runtime.Rule{{Name: "r", Body: json.RawMessage(`{}`)}}}})
		},
		runtime.OpStartSandbox + " with an environment": func() error {
			return c.StartSandbox(ctx, "sb-1", runtime.StartSpec{Env: map[string]string{"A": "1"}})
		},
		runtime.OpExecStream + " with stdin": func() error {
			_, err := c.ExecStream(ctx, "sb-1", runtime.ExecRequest{Command: []string{"cat"}, Stdin: []byte("in")})
			return err
		},
	}
	// The subtests stay serial: they share the client, whose flag is read
	// once they have all run.
	for op, call := range ops {
		t.Run(op, func(t *testing.T) {
			err := call()
			wantOneKind(t, err, runtime.ErrUnsupported)
		})
	}
	if !c.Reachable() {
		t.Error("Reachable() = false after results of kind unsupported only")
	}
}

// wantOneKind fails unless err is of exactly one of the four kinds, and that
// kind is want.
func wantOneKind(t *testing.T, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want one of kind %q", want)
	}
	var held []string
	for _, kind := range []error{runtime.ErrNotFound, runtime.ErrInvalid, runtime.ErrUnsupported, runtime.ErrUnavailable} {
		if errors.Is(err, kind) {
			held = append(held, kind.Error())
		}
	}
	if len(held) != 1 || !errors.Is(err, want) {
		t.Fatalf("error %q is of kinds %q, want exactly %q", err, held, want)
	}
}
