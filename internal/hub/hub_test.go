package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/store"
)

func TestReadyz_FollowsBothStores(t *testing.T) {
	for _, closeStore := range []string{"analytical", "transactional"} {
		t.Run(closeStore, func(t *testing.T) {
			h := startHub(t, hubDir(t, loopbackConfig), nil)
			if code, body := h.get("/readyz"); code != 200 || body != `{"status":"ready"}` {
				t.Fatalf("/readyz = %d %s, want 200 ready", code, body)
			}
			rt := h.rt.Load()
			db := rt.Store.Analytics()
			if closeStore == "transactional" {
				db = rt.Store.DB()
			}
			db.Close()
			if code, body := h.get("/readyz"); code != 503 || body != `{"status":"not ready"}` {
				t.Errorf("/readyz after closing the %s store = %d %s, want 503 not ready", closeStore, code, body)
			}
			if code, _ := h.get("/healthz"); code != 200 {
				t.Errorf("/healthz = %d", code)
			}
		})
	}
}

// freePort returns a port nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// orderLog is a log of stage names safe for concurrent use.
type orderLog struct {
	mu  sync.Mutex
	got []string
}

func (l *orderLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, s)
}

func (l *orderLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.got...)
}

func TestRun_StartsInOneFixedOrder(t *testing.T) {
	port := freePort(t)
	dir := hubDir(t, fmt.Sprintf("[server]\nbind = \"127.0.0.1\"\nport = %d\n", port))
	log := &orderLog{}
	releaseHook := make(chan struct{})
	releaseTask := make(chan struct{})
	defer close(releaseTask)
	hookRan := make(chan struct{})
	ping := func(ctx context.Context, rt *Runtime) error {
		if err := rt.Store.Ping(ctx); err != nil {
			return fmt.Errorf("stores not open: %w", err)
		}
		return nil
	}
	compose := withCompose(func(c *Composition) {
		c.AddStartHook("h1", func(ctx context.Context, rt *Runtime) error { log.add("h1"); return ping(ctx, rt) })
		c.AddStartHook("h2", func(ctx context.Context, rt *Runtime) error {
			log.add("h2")
			close(hookRan)
			<-releaseHook
			return ping(ctx, rt)
		})
		c.AddTask("fork_expiry", func(ctx context.Context, rt *Runtime) error {
			log.add("pass")
			select {
			case <-releaseTask:
			case <-ctx.Done():
			}
			return nil
		})
	})

	go func() {
		select {
		case <-hookRan:
		case <-time.After(10 * time.Second):
			t.Error("start hook h2 was not called")
			close(releaseHook)
			return
		}
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			t.Error("a connection was accepted while a start hook ran")
		}
		if got := log.list(); strings.Join(got, ",") != "h1,h2" {
			t.Errorf("log while h2 blocks = %v", got)
		}
		close(releaseHook)
	}()
	h := startHub(t, dir, nil, compose)
	deadline := time.Now().Add(2 * time.Second)
	for len(log.list()) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := log.list(); len(got) < 3 || strings.Join(got[:3], ",") != "h1,h2,pass" {
		t.Errorf("log = %v, want h1, h2, pass", got)
	}
	if code, _ := h.get("/healthz"); code != 200 {
		t.Errorf("/healthz while the task blocks = %d", code)
	}
}

func TestRun_LogsOneListeningLineWithAddressConfigAndData(t *testing.T) {
	t.Run("config in XDG_CONFIG_HOME", func(t *testing.T) {
		work := t.TempDir()
		xdg := t.TempDir()
		db := filepath.Join(t.TempDir(), "store", "hub.db")
		text := loopbackConfig + "[database]\npath = \"" + db + "\"\n"
		if err := os.WriteFile(filepath.Join(xdg, "config.toml"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		h := startHub(t, work, map[string]string{"XDG_CONFIG_HOME": xdg})
		checkListening(t, h, filepath.Join(xdg, "config.toml"), filepath.Dir(db))
	})
	t.Run("no config file", func(t *testing.T) {
		work := t.TempDir()
		data := t.TempDir()
		zero := 0
		h := startHub(t, work, map[string]string{"XDG_DATA_HOME": data}, func(o *Options) { o.Port = &zero })
		checkListening(t, h, filepath.Join(work, "config.toml"), data)
	})
}

func checkListening(t *testing.T, h *runningHub, wantConfig, wantData string) {
	t.Helper()
	n := 0
	var rec map[string]any
	for _, r := range records(t, h.stderr.String()) {
		if r["msg"] == "listening" {
			n++
			rec = r
		}
	}
	if n != 1 {
		t.Fatalf("%d listening lines, want 1", n)
	}
	_, port, err := net.SplitHostPort(rec["addr"].(string))
	if err != nil || port == "0" || port == "" {
		t.Errorf("addr = %v", rec["addr"])
	}
	if rec["config"] != wantConfig || rec["data_dir"] != wantData {
		t.Errorf("config %v data_dir %v, want %s %s", rec["config"], rec["data_dir"], wantConfig, wantData)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Errorf("connecting to the listening port: %v", err)
		return
	}
	c.Close()
}

func TestRun_AFailureBeforeListeningExitsOneWithOneLine(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	heldAddr := held.Addr().String()
	heldPort := held.Addr().(*net.TCPAddr).Port

	tests := []struct {
		name    string
		config  string
		compose func(*Composition, *orderLog)
		want    string
		check   func(t *testing.T, dir string, log *orderLog)
	}{
		{"invalid configuration", "[server]\nprot = 1\n", nil, "server.prot", func(t *testing.T, dir string, _ *orderLog) {
			if _, err := os.Stat(filepath.Join(dir, "data")); err == nil {
				t.Error("the data directory was created")
			}
		}},
		{"data cannot be opened", loopbackConfig + "[database]\npath = \"" + filepath.Join(blocker, "parley.db") + "\"\n", nil, blocker, nil},
		{"a schema step fails", loopbackConfig, func(c *Composition, _ *orderLog) {
			c.AddStep(store.Step{Name: "boom", Target: store.Main, Apply: func(context.Context, *sql.Tx) error { return errors.New("broken") }})
		}, "boom", nil},
		{"a start hook fails", loopbackConfig, func(c *Composition, log *orderLog) {
			c.AddStartHook("resume-clones", func(context.Context, *Runtime) error { return errors.New("disk gone") })
			c.AddStartHook("second", func(context.Context, *Runtime) error { log.add("second"); return nil })
			c.AddTask("retention", func(context.Context, *Runtime) error { log.add("task"); return nil })
		}, "resume-clones", func(t *testing.T, _ string, log *orderLog) {
			time.Sleep(50 * time.Millisecond)
			if got := log.list(); len(got) != 0 {
				t.Errorf("ran after the failing hook: %v", got)
			}
		}},
		{"the address is taken", fmt.Sprintf("[server]\nbind = \"127.0.0.1\"\nport = %d\n", heldPort), nil, heldAddr, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := hubDir(t, tt.config)
			log := &orderLog{}
			var opts []Option
			if tt.compose != nil {
				opts = append(opts, withCompose(func(c *Composition) { tt.compose(c, log) }))
			}
			code, _, stderr := runHub(t, dir, nil, nil, opts...)
			if code != 1 {
				t.Fatalf("exit %d, want 1; stderr:\n%s", code, stderr)
			}
			lines := nonEmptyLines(stderr)
			if len(lines) != 1 {
				t.Fatalf("stderr holds %d lines, want 1:\n%s", len(lines), stderr)
			}
			if rec := records(t, stderr)[0]; rec["level"] != "ERROR" {
				t.Errorf("level = %v, want ERROR", rec["level"])
			}
			if !strings.Contains(lines[0], tt.want) {
				t.Errorf("line %s does not name %s", lines[0], tt.want)
			}
			if strings.Contains(stderr, "listening") {
				t.Error("a listening line was written")
			}
			if tt.check != nil {
				tt.check(t, dir, log)
			}
		})
	}
}

func TestServerOptions_CarryTheConfiguredLimits(t *testing.T) {
	tests := []struct {
		text      string
		body      int64
		perMinute int
		probes    bool
	}{
		{"", 1048576, 600, false},
		{"[server]\nmax_body_size = \"2KB\"\n[limits]\nrequests_per_minute = 7\n[logging]\nlog_health_probes = true\n", 2048, 7, true},
	}
	for _, tt := range tests {
		cfg, err := config.Load(config.MapEnv(nil), hubDir(t, tt.text))
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		st, err := store.Open(context.Background(), store.Options{DatabasePath: filepath.Join(dir, "p.db"), WorkspaceRoot: filepath.Join(dir, "ws")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		opts := ServerOptions(cfg, st)
		if opts.BodyLimit != tt.body || opts.RequestsPerMinute != tt.perMinute || opts.LogHealthProbes != tt.probes || len(opts.ReadinessProbes) != 1 {
			t.Errorf("%q: options = %+v", tt.text, opts)
		}
		if err := opts.ReadinessProbes[0](context.Background()); err != nil {
			t.Errorf("readiness probe on open stores: %v", err)
		}
		st.Close()
		if err := opts.ReadinessProbes[0](context.Background()); err == nil {
			t.Error("readiness probe on closed stores succeeded")
		}
	}
}

func TestRun_AFailedStartKeepsConfiguredSecretsOutOfItsLine(t *testing.T) {
	text := loopbackConfig + "[[oauth.providers]]\nname = \"github\"\nclient_id = \"id\"\nclient_secret = \"${GH}\"\n"
	compose := withCompose(func(c *Composition) {
		c.AddStartHook("call-provider", func(context.Context, *Runtime) error { return errors.New("provider said: topsecret-value") })
	})
	code, _, stderr := runHub(t, hubDir(t, text), map[string]string{"GH": "topsecret-value"}, nil, compose)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Contains(stderr, "topsecret-value") || !strings.Contains(stderr, "[redacted]") {
		t.Errorf("stderr = %s, want the secret replaced by [redacted]", stderr)
	}
}
