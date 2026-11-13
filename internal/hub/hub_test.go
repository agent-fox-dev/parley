package hub_test

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/store"
)

func TestRun_RunsTheStartStagesInOrderAndListensLast(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	dir := workDir(t, "[server]\nbind = \"127.0.0.1\"\nport = "+itoa(port)+"\n")
	addr := net.JoinHostPort("127.0.0.1", itoa(port))

	var log callLog
	pinged := make(chan error, 2)
	releaseHook, releaseTask := make(chan struct{}), make(chan struct{})
	compose := func(c *hub.Composition) {
		c.AddStartHook("h1", func(ctx context.Context, rt *hub.Runtime) error {
			log.add("h1")
			pinged <- rt.Store.Ping(ctx)
			return nil
		})
		c.AddStartHook("h2", func(ctx context.Context, rt *hub.Runtime) error {
			log.add("h2")
			pinged <- rt.Store.Ping(ctx)
			<-releaseHook
			return nil
		})
		c.AddMaintenanceTask("fork_expiry", func(ctx context.Context, _ *hub.Runtime) error {
			log.add("pass")
			select {
			case <-releaseTask:
			case <-ctx.Done():
			}
			return nil
		})
	}
	r := runHub(t, hub.Options{WorkDir: dir, Compose: compose})

	eventually(t, "the second start hook runs", func() bool { return len(log.calls()) == 2 })
	if conn, err := net.Dial("tcp", addr); err == nil {
		conn.Close()
		t.Errorf("a connection to %s was accepted while a start hook was running", addr)
	}
	if got := log.calls(); !slices.Equal(got, []string{"h1", "h2"}) {
		t.Errorf("calls while the second hook runs = %v, want [h1 h2]", got)
	}

	close(releaseHook)
	r.listening = r.stderr.waitForMsg(t, "listening")
	r.addr = r.listening["addr"].(string)
	eventually(t, "the first maintenance pass starts", func() bool { return len(log.calls()) >= 3 })
	if got := log.calls(); !slices.Equal(got[:3], []string{"h1", "h2", "pass"}) {
		t.Errorf("calls = %v, want h1, h2, pass first", got)
	}
	var doc struct{ Status string }
	if status := getJSON(t, r.url("/healthz"), &doc); status != http.StatusOK {
		t.Errorf("GET /healthz while the first pass runs = %d, want 200", status)
	}
	for range 2 {
		if err := <-pinged; err != nil {
			t.Errorf("a start hook found the stores not answering: %v", err)
		}
	}

	close(releaseTask)
	if code := r.stop(t); code != 0 {
		t.Errorf("Run returned %d after a stop, want 0", code)
	}
}

func TestRun_WritesOneListeningLineWithTheAddressConfigAndDataDirectory(t *testing.T) {
	t.Parallel()
	zero := 0
	for _, tc := range []struct {
		name string
		// arrange returns the options and the expected config and data_dir.
		arrange func(t *testing.T) (hub.Options, string, string)
	}{
		{"config file in XDG_CONFIG_HOME", func(t *testing.T) (hub.Options, string, string) {
			work, cfgDir := t.TempDir(), t.TempDir()
			writeFile(t, filepath.Join(cfgDir, "config.toml"), localConfig+"[database]\npath = \"store/hub.db\"\n")
			env := config.MapEnv(map[string]string{"XDG_CONFIG_HOME": cfgDir})
			return hub.Options{WorkDir: work, Env: env},
				filepath.Join(cfgDir, "config.toml"), filepath.Join(work, "store")
		}},
		{"no config file and XDG_DATA_HOME", func(t *testing.T) (hub.Options, string, string) {
			work, data := t.TempDir(), t.TempDir()
			env := config.MapEnv(map[string]string{"XDG_DATA_HOME": data})
			return hub.Options{WorkDir: work, Env: env, Port: &zero},
				filepath.Join(work, "config.toml"), data
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, wantConfig, wantDataDir := tc.arrange(t)
			r := startHub(t, opts)

			host, port, err := net.SplitHostPort(r.addr)
			if err != nil || host == "" || port == "0" {
				t.Errorf("addr = %q, want a host and the port the system chose", r.addr)
			}
			if r.listening["config"] != wantConfig {
				t.Errorf("config = %v, want %s", r.listening["config"], wantConfig)
			}
			if r.listening["data_dir"] != wantDataDir {
				t.Errorf("data_dir = %v, want %s", r.listening["data_dir"], wantDataDir)
			}
			var doc struct{ Status string }
			if status := getJSON(t, r.url("/healthz"), &doc); status != http.StatusOK {
				t.Errorf("GET /healthz = %d, want 200", status)
			}
			if code := r.stop(t); code != 0 {
				t.Errorf("Run returned %d after a stop, want 0", code)
			}
			if n := strings.Count(r.stderr.String(), `"msg":"listening"`); n != 1 {
				t.Errorf("stderr holds %d listening lines, want 1: %q", n, r.stderr.String())
			}
		})
	}
}

func TestRun_StopsTheStartAtTheFirstFailingStage(t *testing.T) {
	t.Parallel()
	type arrangement struct {
		dir     string
		compose func(*hub.Composition)
		want    string
	}
	var log callLog // every row's calls, prefixed by the row's name
	hooks := func(row string) func(*hub.Composition) {
		return func(c *hub.Composition) {
			c.AddStartHook(row+"-hook", func(context.Context, *hub.Runtime) error {
				log.add(row + "-hook")
				return nil
			})
			c.AddMaintenanceTask(row+"-task", func(context.Context, *hub.Runtime) error {
				log.add(row + "-task")
				return nil
			})
		}
	}
	for _, tc := range []struct {
		name string
		// mustNotExist is a path, under the row's directory, that a stage
		// which must not have run would have created.
		mustNotExist string
		arrange      func(t *testing.T, row string) arrangement
	}{
		{"invalid configuration", "data", func(t *testing.T, row string) arrangement {
			return arrangement{dir: workDir(t, "[server]\nprot = 1\n"), compose: hooks(row), want: "server.prot"}
		}},
		{"data directory under a regular file", "", func(t *testing.T, row string) arrangement {
			dir := t.TempDir()
			blocker := filepath.Join(dir, "blocker")
			writeFile(t, blocker, "a file, not a directory")
			writeFile(t, filepath.Join(dir, "config.toml"), localConfig+"[database]\npath = \""+filepath.Join(blocker, "parley.db")+"\"\n")
			return arrangement{dir: dir, compose: hooks(row), want: blocker}
		}},
		{"failing schema step", "", func(t *testing.T, row string) arrangement {
			return arrangement{dir: workDir(t, localConfig), want: "boom", compose: func(c *hub.Composition) {
				c.AddSchemaStep(store.Step{Name: "boom", Target: store.Main, Apply: func(context.Context, *sql.Tx) error {
					return errors.New("no such table")
				}})
				hooks(row)(c)
			}}
		}},
		{"failing start hook", "", func(t *testing.T, row string) arrangement {
			return arrangement{dir: workDir(t, localConfig), want: "resume-clones", compose: func(c *hub.Composition) {
				c.AddStartHook("resume-clones", func(context.Context, *hub.Runtime) error {
					return errors.New("disk gone")
				})
				hooks(row)(c)
			}}
		}},
		{"listen address in use", "", func(t *testing.T, row string) arrangement {
			held, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("hold a port: %v", err)
			}
			t.Cleanup(func() { held.Close() })
			port := held.Addr().(*net.TCPAddr).Port
			// The hook runs before the bind, so only the task must not run.
			return arrangement{
				dir:     workDir(t, "[server]\nbind = \"127.0.0.1\"\nport = "+itoa(port)+"\n"),
				compose: func(c *hub.Composition) { hooks(row)(c) },
				want:    held.Addr().String(),
			}
		}},
		{"start hook registered twice", "data", func(t *testing.T, row string) arrangement {
			return arrangement{dir: workDir(t, localConfig), want: "twice", compose: func(c *hub.Composition) {
				noop := func(context.Context, *hub.Runtime) error { return nil }
				c.AddStartHook("twice", noop)
				c.AddStartHook("twice", noop)
				hooks(row)(c)
			}}
		}},
		{"maintenance task registered twice", "data", func(t *testing.T, row string) arrangement {
			return arrangement{dir: workDir(t, localConfig), want: "retention", compose: func(c *hub.Composition) {
				noop := func(context.Context, *hub.Runtime) error { return nil }
				c.AddMaintenanceTask("retention", noop)
				c.AddMaintenanceTask("retention", noop)
				hooks(row)(c)
			}}
		}},
		{"background work registered twice", "data", func(t *testing.T, row string) arrangement {
			return arrangement{dir: workDir(t, localConfig), want: "event_streams", compose: func(c *hub.Composition) {
				noop := func(context.Context, *hub.Runtime) {}
				c.AddBackgroundWork("event_streams", time.Second, noop)
				c.AddBackgroundWork("event_streams", time.Second, noop)
				hooks(row)(c)
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := tc.arrange(t, tc.name)
			var stdout, stderr syncBuffer
			code := hub.Run(context.Background(), hub.Options{
				WorkDir: a.dir, Stdout: &stdout, Stderr: &stderr, Compose: a.compose,
			})

			if code != 1 {
				t.Errorf("Run returned %d, want 1", code)
			}
			lines := nonEmptyLines(stderr.String())
			if len(lines) != 1 {
				t.Fatalf("stderr holds %d lines, want 1: %q", len(lines), stderr.String())
			}
			if rec := decodeLine(t, lines[0]); rec["level"] != "ERROR" {
				t.Errorf("level = %v, want ERROR", rec["level"])
			}
			if !strings.Contains(lines[0], a.want) {
				t.Errorf("error line %q does not name %q", lines[0], a.want)
			}
			if strings.Contains(stderr.String(), "listening") {
				t.Errorf("stderr %q has a listening line", stderr.String())
			}
			for _, call := range log.calls() {
				if call == tc.name+"-task" || (call == tc.name+"-hook" && tc.name != "listen address in use") {
					t.Errorf("%s was called after the start failed", call)
				}
			}
			if tc.mustNotExist != "" {
				if _, err := os.Stat(filepath.Join(a.dir, tc.mustNotExist)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s exists after the start failed (stat: %v)", tc.mustNotExist, err)
				}
			}
		})
	}
}

func TestRun_RunsAMaintenancePassAtEachTickOfItsInterval(t *testing.T) {
	t.Parallel()
	var passes atomic.Int32
	r := startHub(t, hub.Options{
		WorkDir:             workDir(t, localConfig),
		MaintenanceInterval: 10 * time.Millisecond,
		Compose: func(c *hub.Composition) {
			c.AddMaintenanceTask("retention", func(context.Context, *hub.Runtime) error {
				passes.Add(1)
				return nil
			})
		},
	})

	eventually(t, "three passes run", func() bool { return passes.Load() >= 3 })
	if code := r.stop(t); code != 0 {
		t.Errorf("Run returned %d after a stop, want 0", code)
	}
	n := passes.Load()
	time.Sleep(30 * time.Millisecond)
	if got := passes.Load(); got != n {
		t.Errorf("%d passes ran after Run returned", got-n)
	}
}

func TestRun_ScrubsAConfiguredClientSecretFromTheStartError(t *testing.T) {
	t.Parallel()
	dir := workDir(t, localConfig+
		"[[oauth.providers]]\nname = \"github\"\nclient_id = \"id\"\nclient_secret = \"${GH}\"\n")
	var stdout, stderr syncBuffer
	code := hub.Run(context.Background(), hub.Options{
		WorkDir: dir,
		Env:     config.MapEnv(map[string]string{"GH": "topsecret-value"}),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Compose: func(c *hub.Composition) {
			c.AddStartHook("login", func(context.Context, *hub.Runtime) error {
				return errors.New("provider said: topsecret-value")
			})
		},
	})

	if code != 1 {
		t.Errorf("Run returned %d, want 1", code)
	}
	if strings.Contains(stderr.String(), "topsecret-value") {
		t.Errorf("stderr %q holds the client secret", stderr.String())
	}
	if !strings.Contains(stderr.String(), "[redacted]") {
		t.Errorf("stderr %q does not show [redacted] in the secret's place", stderr.String())
	}
}

func TestServerOptions_CarriesTheConfiguredLimitsAndOneReadinessProbe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		file                string
		maxBody             int64
		perMinute           int
		rateLimitDisabled   bool
		logHealthProbes     bool
		readinessProbeCount int
	}{
		{"defaults", "", 1048576, 600, false, false, 1},
		{"configured", "[server]\nmax_body_size = \"2KB\"\n[limits]\nrequests_per_minute = 7\n[logging]\nlog_health_probes = true\n",
			2048, 7, false, true, 1},
		{"rate limit off", "[limits]\nrequests_per_minute = 0\n", 1048576, 0, true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load(nil, workDir(t, tc.file))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			st, err := store.Open(context.Background(), store.Options{
				DatabasePath: cfg.Database.Path, WorkspaceRoot: cfg.Workspace.Path,
			}, nil)
			if err != nil {
				t.Fatalf("open the stores: %v", err)
			}
			defer st.Close()

			opts := hub.ServerOptions(cfg, st)
			if opts.MaxBodySize != tc.maxBody {
				t.Errorf("MaxBodySize = %d, want %d", opts.MaxBodySize, tc.maxBody)
			}
			if opts.RequestsPerMinute != tc.perMinute {
				t.Errorf("RequestsPerMinute = %d, want %d", opts.RequestsPerMinute, tc.perMinute)
			}
			if opts.RateLimitDisabled != tc.rateLimitDisabled {
				t.Errorf("RateLimitDisabled = %v, want %v", opts.RateLimitDisabled, tc.rateLimitDisabled)
			}
			if opts.LogHealthProbes != tc.logHealthProbes {
				t.Errorf("LogHealthProbes = %v, want %v", opts.LogHealthProbes, tc.logHealthProbes)
			}
			if len(opts.ReadinessProbes) != tc.readinessProbeCount {
				t.Fatalf("%d readiness probes, want %d", len(opts.ReadinessProbes), tc.readinessProbeCount)
			}
			if err := opts.ReadinessProbes[0](context.Background()); err != nil {
				t.Errorf("readiness probe on open stores = %v, want nil", err)
			}
			if opts.IdempotencyStore == nil {
				t.Error("IdempotencyStore is nil, want the store-backed records")
			}
		})
	}
}

func TestRun_AnswersReadinessFromBothStores(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		close func(*store.Store) error
	}{
		{"analytical store closed", func(st *store.Store) error { return st.Analytics().Close() }},
		{"transactional store closed", func(st *store.Store) error { return st.Main().Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtimes := make(chan *hub.Runtime, 1)
			r := startHub(t, hub.Options{WorkDir: workDir(t, localConfig), Compose: func(c *hub.Composition) {
				c.AddStartHook("capture", func(_ context.Context, rt *hub.Runtime) error {
					runtimes <- rt
					return nil
				})
			}})
			rt := <-runtimes

			var doc map[string]any
			if status := getJSON(t, r.url("/readyz"), &doc); status != http.StatusOK || doc["status"] != "ready" {
				t.Errorf("GET /readyz = %d %v, want 200 ready", status, doc)
			}
			if err := tc.close(rt.Store); err != nil {
				t.Fatalf("close: %v", err)
			}
			doc = nil
			if status := getJSON(t, r.url("/readyz"), &doc); status != http.StatusServiceUnavailable || doc["status"] != "not ready" || len(doc) != 1 {
				t.Errorf("GET /readyz = %d %v, want 503 {status: not ready}", status, doc)
			}
			if status := getJSON(t, r.url("/healthz"), &doc); status != http.StatusOK {
				t.Errorf("GET /healthz = %d, want 200", status)
			}
		})
	}
}

func TestRun_LogsAFailingMaintenanceTaskAtEveryPassAndKeepsServing(t *testing.T) {
	t.Parallel()
	insert := func(name string) func(context.Context, *hub.Runtime) error {
		return func(ctx context.Context, rt *hub.Runtime) error {
			return rt.Store.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "INSERT INTO passes (task) VALUES (?)", name)
				return err
			})
		}
	}
	var rt *hub.Runtime
	r := startHub(t, hub.Options{
		WorkDir:             workDir(t, localConfig),
		MaintenanceInterval: 100 * time.Millisecond,
		Compose: func(c *hub.Composition) {
			rt = c.Runtime()
			c.AddSchemaStep(store.Step{Name: "test_passes", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, "CREATE TABLE passes (task TEXT NOT NULL)")
				return err
			}})
			c.AddMaintenanceTask("fork_expiry", insert("fork_expiry"))
			c.AddMaintenanceTask("claim_cleanup", func(context.Context, *hub.Runtime) error {
				return errors.New("claims unreachable at https://u:p@h/x")
			})
			c.AddMaintenanceTask("retention", insert("retention"))
		},
	})

	rows := func(task string) int {
		var n int
		if err := rt.Store.Main().QueryRow("SELECT count(*) FROM passes WHERE task = ?", task).Scan(&n); err != nil {
			t.Fatalf("count the rows of %s: %v", task, err)
		}
		return n
	}
	failures := func() []map[string]any {
		var errs []map[string]any
		for _, rec := range r.stderr.records(t) {
			if rec["level"] == "ERROR" {
				errs = append(errs, rec)
			}
		}
		return errs
	}
	eventually(t, "three passes with a failing task", func() bool {
		return rows("retention") >= 3 && rows("fork_expiry") >= 3 && len(failures()) >= 3
	})

	for _, rec := range failures() {
		text, _ := rec["error"].(string)
		if rec["task"] != "claim_cleanup" || !strings.Contains(text, "https://h/x") || strings.Contains(text, "u:p") {
			t.Errorf("error line %v, want task claim_cleanup and an error holding https://h/x without u:p", rec)
		}
	}
	var health map[string]any
	if status := getJSON(t, r.url("/healthz"), &health); status != http.StatusOK {
		t.Errorf("GET /healthz = %d after failing passes, want 200", status)
	}
	if code := r.stop(t); code != 0 {
		t.Errorf("Run returned %d after a stop, want 0", code)
	}
}
