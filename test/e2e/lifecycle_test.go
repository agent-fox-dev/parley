package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"

	_ "modernc.org/sqlite"
)

// The tests of this file start hubs in-process through hub.Run, which holds
// the flag and signal handling cmd/parleyd hands the process's inputs to.
// They are not parallel: the first-boot flow sends SIGTERM to the test
// process, and every running hub catches it.

// lockedBuffer is a bytes.Buffer a hub may write to while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// served is a hub running in a goroutine.
type served struct {
	stderr *lockedBuffer
	code   chan int
	cancel context.CancelFunc
	// listening is the hub's listening line.
	listening map[string]any
}

// serve runs hub.Run with opts and waits up to 5 s for its listening line.
func serve(t *testing.T, opts hub.Options) *served {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{stderr: &lockedBuffer{}, code: make(chan int, 1), cancel: cancel}
	opts.Stderr = s.stderr
	go func() { s.code <- hub.Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.code:
		case <-time.After(5 * time.Second):
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for s.listening == nil {
		for line := range strings.SplitSeq(s.stderr.String(), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "listening" {
				s.listening = rec
			}
		}
		if s.listening != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no listening line within 5 s; stderr = %q", s.stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s
}

// url is the hub's URL for path.
func (s *served) url(path string) string {
	addr, _ := s.listening["addr"].(string)
	return "http://" + addr + path
}

// exitCode waits up to within for Run to return and gives its status.
func (s *served) exitCode(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case code := <-s.code:
		s.code <- code // the cleanup reads it again
		return code
	case <-time.After(within):
		t.Fatalf("Run did not return within %v", within)
		return -1
	}
}

// status sends GET url and returns the response status.
func status(t *testing.T, url string) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	res.Body.Close()
	return res.StatusCode
}

// git runs git in dir with no user or system configuration and returns its
// trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLifecycle_FirstBootHandsOutTheAdminTokenThenServesAndStopsOnSIGTERM(t *testing.T) {
	dir := t.TempDir()
	const secret = "gh-secret-value-0451"
	writeConfig(t, dir, "[server]\nbind = \"127.0.0.1\"\nport = 0\n\n"+
		"[[oauth.providers]]\nname = \"github\"\nclient_id = \"cid\"\nclient_secret = \"${GH_SECRET}\"\n")
	env := config.MapEnv(map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir, "GH_SECRET": secret})
	var hooks, tasks atomic.Int32
	opts := func(args ...string) hub.Options {
		return hub.Options{Args: args, Env: env, WorkDir: dir, Compose: func(c *hub.Composition) {
			c.AddStartHook("resume", func(context.Context, *hub.Runtime) error {
				hooks.Add(1)
				return nil
			})
			c.AddMaintenanceTask("fork_expiry", func(context.Context, *hub.Runtime) error {
				tasks.Add(1)
				return nil
			})
		}}
	}
	var outputs []string

	// First run: bootstrap.
	var out1, err1 lockedBuffer
	o := opts("--admin-email", "a@x")
	o.Stdout, o.Stderr = &out1, &err1
	if code := hub.Run(context.Background(), o); code != 0 {
		t.Fatalf("parleyd --admin-email a@x returned %d, want 0; stderr = %q", code, err1.String())
	}
	outputs = append(outputs, out1.String(), err1.String())
	tokenFile := filepath.Join(dir, "admin_token")
	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatalf("the admin token file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("admin_token has mode %o, want 600", mode)
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	token := string(raw)
	sum := sha256.Sum256(raw)
	if got := storedTokenHash(t, dir); got != hex.EncodeToString(sum[:]) {
		t.Errorf("stored hash = %q, want the SHA-256 of the token file", got)
	}

	// Second run: refused while the file exists.
	var out2, err2 lockedBuffer
	o = opts()
	o.Stdout, o.Stderr = &out2, &err2
	if code := hub.Run(context.Background(), o); code != 1 {
		t.Fatalf("parleyd with the token file present returned %d, want 1", code)
	}
	outputs = append(outputs, out2.String(), err2.String())
	if !strings.Contains(err2.String(), "admin_token") || strings.Contains(err2.String(), "listening") {
		t.Errorf("refusal stderr = %q, want a line naming admin_token and no listening line", err2.String())
	}

	// Third run: serves once the file is gone, and stops on SIGTERM.
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	var out3 lockedBuffer
	o = opts()
	o.Stdout = &out3
	s := serve(t, o)
	if got, want := s.listening["config"], filepath.Join(dir, "config.toml"); got != want {
		t.Errorf("listening config = %v, want %s", got, want)
	}
	if got := s.listening["data_dir"]; got != dir {
		t.Errorf("listening data_dir = %v, want %s", got, dir)
	}
	if got := status(t, s.url("/readyz")); got != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for tasks.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if hooks.Load() != 1 || tasks.Load() != 1 {
		t.Errorf("start hook called %d times and maintenance task %d times, want 1 and 1", hooks.Load(), tasks.Load())
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.exitCode(t, 2*time.Second); code != 0 {
		t.Errorf("Run returned %d after SIGTERM, want 0", code)
	}
	addr, _ := s.listening["addr"].(string)
	if conn, err := net.Dial("tcp", addr); err == nil {
		conn.Close()
		t.Errorf("%s accepts connections after the stop", addr)
	}
	outputs = append(outputs, out3.String(), s.stderr.String())

	hexPart := strings.TrimPrefix(token, "parley_admin_")
	for _, text := range outputs {
		if strings.Contains(text, hexPart) || strings.Contains(text, secret) {
			t.Errorf("an output holds the token or the client secret: %q", text)
		}
	}
}

func TestLifecycle_BacksUpARunningHubAndAHubStartsOnTheSnapshot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is needed: %v", err)
	}
	dir := t.TempDir()
	writeConfig(t, dir, "[server]\nbind = \"127.0.0.1\"\nport = 0\n")
	env := config.MapEnv(nil)
	tableA := store.Step{Name: "test_a", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "CREATE TABLE a (txn INTEGER NOT NULL, n INTEGER NOT NULL)")
		return err
	}}
	var applied atomic.Int32
	compose := func(rt **hub.Runtime) func(*hub.Composition) {
		return func(c *hub.Composition) {
			*rt = c.Runtime()
			step := tableA
			step.Apply = func(ctx context.Context, tx *sql.Tx) error {
				applied.Add(1)
				return tableA.Apply(ctx, tx)
			}
			c.AddSchemaStep(step)
		}
	}

	var rt1 *hub.Runtime
	hub1 := serve(t, hub.Options{WorkDir: dir, Env: env, Compose: compose(&rt1)})
	commit := func(txn int) error {
		return rt1.Store.Tx(context.Background(), func(tx *sql.Tx) error {
			for n := range 10 {
				if _, err := tx.Exec("INSERT INTO a (txn, n) VALUES (?, ?)", txn, n); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for txn := range 10 {
		if err := commit(txn); err != nil {
			t.Fatalf("commit before the backup: %v", err)
		}
	}
	repo := filepath.Join(dir, "data", "workspaces", "r.git")
	if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(repo), "init", "--quiet", "--bare", "--initial-branch=main", repo)
	clone := t.TempDir()
	git(t, clone, "init", "--quiet", "--initial-branch=main")
	for _, msg := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(clone, "file"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, clone, "add", "file")
		git(t, clone, "commit", "--quiet", "-m", msg)
	}
	git(t, clone, "push", "--quiet", repo, "main")

	stopWriter := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		for txn := 10; ; txn++ {
			select {
			case <-stopWriter:
				writerErr <- nil
				return
			default:
			}
			if err := commit(txn); err != nil {
				writerErr <- err
				return
			}
		}
	}()

	target := filepath.Join(t.TempDir(), "snapshot")
	var out, errb lockedBuffer
	code := hub.Run(context.Background(), hub.Options{
		Args: []string{"backup", target}, Env: env, WorkDir: dir, Stdout: &out, Stderr: &errb,
		Compose: compose(new(*hub.Runtime)),
	})
	if code != 0 {
		t.Fatalf("parleyd backup returned %d, want 0; stderr = %q", code, errb.String())
	}
	if got := status(t, hub1.url("/readyz")); got != http.StatusOK {
		t.Errorf("GET /readyz of the running hub = %d after the backup, want 200", got)
	}
	close(stopWriter)
	if err := <-writerErr; err != nil {
		t.Errorf("the writer failed during the backup: %v", err)
	}
	for _, name := range []string{"parley.db", "analytics.db", "workspaces"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Errorf("the snapshot lacks %s: %v", name, err)
		}
	}

	dir2 := t.TempDir()
	writeConfig(t, dir2, "[server]\nbind = \"127.0.0.1\"\nport = 0\n\n[database]\npath = \""+
		filepath.Join(target, "parley.db")+"\"\n\n[workspace]\npath = \""+filepath.Join(target, "workspaces")+"\"\n")
	appliedBefore := applied.Load()
	hub2 := serve(t, hub.Options{WorkDir: dir2, Env: env, Compose: compose(new(*hub.Runtime))})
	if got := status(t, hub2.url("/readyz")); got != http.StatusOK {
		t.Errorf("GET /readyz of the hub on the snapshot = %d, want 200", got)
	}
	if applied.Load() != appliedBefore {
		t.Errorf("the hub on the snapshot applied a schema step")
	}
	hub2.cancel()
	if code := hub2.exitCode(t, 5*time.Second); code != 0 {
		t.Errorf("the hub on the snapshot returned %d after a stop, want 0", code)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.Join(target, "parley.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM a").Scan(&n); err != nil {
		t.Fatalf("count the rows of the snapshot: %v", err)
	}
	if n%10 != 0 || n < 100 {
		t.Errorf("the snapshot holds %d rows, want a multiple of 10 and at least 100", n)
	}
	git(t, filepath.Join(target, "workspaces", "r.git"), "fsck", "--no-progress")

	hub1.cancel()
	if code := hub1.exitCode(t, 5*time.Second); code != 0 {
		t.Errorf("the first hub returned %d after a stop, want 0", code)
	}
}

// writeConfig writes config.toml with text into dir.
func writeConfig(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// storedTokenHash reads the admin token hash of the hub whose data directory
// is dir.
func storedTokenHash(t *testing.T, dir string) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		DatabasePath:  filepath.Join(dir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{identity.AdminSettingsStep})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	admin, err := identity.AdminSettings(ctx, st)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read the admin settings: %v", err)
	}
	return admin.TokenHash
}
