package e2e

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"

	_ "modernc.org/sqlite"
)

// The tests of this file start hubs in-process through hub.Run, which holds
// the flag and signal handling cmd/parleyd hands the process's inputs to.
// They are not parallel: the first-boot flow sends SIGTERM to the test
// process, and every running hub catches it.

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
	var out1, err1 hubtest.LogBuffer
	o := opts("--admin-email", "a@x")
	o.Stdout, o.Stderr = &out1, &err1
	if code := hub.Run(context.Background(), o); code != 0 {
		t.Fatalf("parleyd --admin-email a@x returned %d, want 0; stderr = %q", code, err1.String())
	}
	outputs = append(outputs, out1.String(), err1.String())
	tokenFile := filepath.Join(dir, "admin_token")
	token := checkTokenFile(t, tokenFile, dir)

	// Second run: refused while the file exists.
	var out2, err2 hubtest.LogBuffer
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
	var out3 hubtest.LogBuffer
	o = opts()
	o.Stdout = &out3
	s := hubtest.Serve(t, o)
	if got, want := s.Listening["config"], filepath.Join(dir, "config.toml"); got != want {
		t.Errorf("listening config = %v, want %s", got, want)
	}
	if got := s.Listening["data_dir"]; got != dir {
		t.Errorf("listening data_dir = %v, want %s", got, dir)
	}
	if got := status(t, s.URL("/readyz")); got != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200", got)
	}
	waitUntil(5*time.Second, func() bool { return tasks.Load() >= 1 })
	if hooks.Load() != 1 || tasks.Load() != 1 {
		t.Errorf("start hook called %d times and maintenance task %d times, want 1 and 1", hooks.Load(), tasks.Load())
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := s.ExitCode(t, 2*time.Second); code != 0 {
		t.Errorf("Run returned %d after SIGTERM, want 0", code)
	}
	addr, _ := s.Listening["addr"].(string)
	if conn, err := net.Dial("tcp", addr); err == nil {
		conn.Close()
		t.Errorf("%s accepts connections after the stop", addr)
	}
	outputs = append(outputs, out3.String(), s.Log.String())

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
	hub1 := hubtest.Serve(t, hub.Options{WorkDir: dir, Env: env, Compose: compose(&rt1)})
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
	bareRepo(t, filepath.Join(dir, "data", "workspaces", "r.git"))
	stopWriter := keepCommitting(10, commit)

	target := filepath.Join(t.TempDir(), "snapshot")
	var out, errb hubtest.LogBuffer
	code := hub.Run(context.Background(), hub.Options{
		Args: []string{"backup", target}, Env: env, WorkDir: dir, Stdout: &out, Stderr: &errb,
		Compose: compose(new(*hub.Runtime)),
	})
	if code != 0 {
		t.Fatalf("parleyd backup returned %d, want 0; stderr = %q", code, errb.String())
	}
	if got := status(t, hub1.URL("/readyz")); got != http.StatusOK {
		t.Errorf("GET /readyz of the running hub = %d after the backup, want 200", got)
	}
	if err := stopWriter(); err != nil {
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
	hub2 := hubtest.Serve(t, hub.Options{WorkDir: dir2, Env: env, Compose: compose(new(*hub.Runtime))})
	if got := status(t, hub2.URL("/readyz")); got != http.StatusOK {
		t.Errorf("GET /readyz of the hub on the snapshot = %d, want 200", got)
	}
	if applied.Load() != appliedBefore {
		t.Errorf("the hub on the snapshot applied a schema step")
	}
	hub2.Stop()
	if code := hub2.ExitCode(t, 5*time.Second); code != 0 {
		t.Errorf("the hub on the snapshot returned %d after a stop, want 0", code)
	}

	if n := countRows(t, filepath.Join(target, "parley.db"), "a"); n%10 != 0 || n < 100 {
		t.Errorf("the snapshot holds %d rows, want a multiple of 10 and at least 100", n)
	}
	git(t, filepath.Join(target, "workspaces", "r.git"), "fsck", "--no-progress")

	hub1.Stop()
	if code := hub1.ExitCode(t, 5*time.Second); code != 0 {
		t.Errorf("the first hub returned %d after a stop, want 0", code)
	}
}

// checkTokenFile checks the admin token file at path, written by the hub
// whose data directory is dataDir: mode 0600 and a content whose SHA-256 is
// the stored hash. It returns the token.
func checkTokenFile(t *testing.T, path, dataDir string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the admin token file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("admin_token has mode %o, want 600", mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := storedTokenHash(t, dataDir); got != hex.EncodeToString(sum[:]) {
		t.Errorf("stored hash = %q, want the SHA-256 of the token file", got)
	}
	return string(raw)
}

// waitUntil polls cond until it holds or within has passed.
func waitUntil(within time.Duration, cond func() bool) {
	for deadline := time.Now().Add(within); !cond() && time.Now().Before(deadline); {
		time.Sleep(2 * time.Millisecond)
	}
}

// bareRepo creates a bare repository at path holding two commits on main.
func bareRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, filepath.Dir(path), "init", "--quiet", "--bare", "--initial-branch=main", path)
	clone := t.TempDir()
	git(t, clone, "init", "--quiet", "--initial-branch=main")
	for _, msg := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(clone, "file"), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, clone, "add", "file")
		git(t, clone, "commit", "--quiet", "-m", msg)
	}
	git(t, clone, "push", "--quiet", path, "main")
}

// keepCommitting calls commit with first, first+1, ... in a goroutine until
// the returned function is called, which returns the first error of commit.
func keepCommitting(first int, commit func(txn int) error) (stop func() error) {
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		for txn := first; ; txn++ {
			select {
			case <-done:
				result <- nil
				return
			default:
			}
			if err := commit(txn); err != nil {
				result <- err
				return
			}
		}
	}()
	return func() error {
		close(done)
		return <-result
	}
}

// countRows counts the rows of table in the store file at path, opened
// read-only.
func countRows(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count the rows of %s in %s: %v", table, path, err)
	}
	return n
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
