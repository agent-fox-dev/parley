package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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
)

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

// runParleyd runs the hub as cmd/parleyd does: SIGTERM and SIGINT end it.
func runParleyd(args []string, env map[string]string, dir string, stdout, stderr *lockedBuffer, compose func(*hub.Composition)) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return hub.Run(ctx, hub.Options{Args: args, Env: config.MapEnv(env), WorkDir: dir, Stdout: stdout, Stderr: stderr, Compose: compose})
}

// listeningLine waits for the listening line on stderr.
func listeningLine(t *testing.T, stderr *lockedBuffer, exited <-chan int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range strings.Split(stderr.String(), "\n") {
			var rec map[string]any
			if json.Unmarshal([]byte(l), &rec) == nil && rec["msg"] == "listening" {
				return rec
			}
		}
		select {
		case c := <-exited:
			t.Fatalf("the hub exited %d before listening: %s", c, stderr.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("no listening line: %s", stderr.String())
	return nil
}

func statusOf(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestLifecycle_FirstBootTokenRunAndStop(t *testing.T) {
	dir := t.TempDir()
	conf := "[server]\nbind = \"127.0.0.1\"\nport = 0\n[[oauth.providers]]\nname = \"github\"\nclient_id = \"id\"\nclient_secret = \"${GH_SECRET}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir, "GH_SECRET": "gh-secret-value-1234"}
	var hookCalls, taskCalls atomic.Int32
	compose := func(c *hub.Composition) {
		c.AddStartHook("resume", func(context.Context, *hub.Runtime) error { hookCalls.Add(1); return nil })
		c.AddTask("fork_expiry", func(context.Context, *hub.Runtime) error { taskCalls.Add(1); return nil })
	}
	var outputs []*lockedBuffer
	newBuf := func() *lockedBuffer { b := &lockedBuffer{}; outputs = append(outputs, b); return b }

	if code := runParleyd([]string{"--admin-email", "a@x"}, env, dir, newBuf(), newBuf(), compose); code != 0 {
		t.Fatalf("first boot exit %d: %s", code, outputs[1].String())
	}
	tokPath := filepath.Join(dir, "admin_token")
	raw, err := os.ReadFile(tokPath)
	if err != nil {
		t.Fatal(err)
	}
	tok := string(raw)
	if fi, _ := os.Stat(tokPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %o", fi.Mode().Perm())
	}

	refused := newBuf()
	if code := runParleyd(nil, env, dir, newBuf(), refused, compose); code != 1 || !strings.Contains(refused.String(), "admin_token") {
		t.Fatalf("second run = %d %s, want 1 naming the token file", code, refused.String())
	}
	os.Remove(tokPath)

	stderr := newBuf()
	exited := make(chan int, 1)
	go func() { exited <- runParleyd(nil, env, dir, newBuf(), stderr, compose) }()
	rec := listeningLine(t, stderr, exited)
	if rec["config"] != filepath.Join(dir, "config.toml") || rec["data_dir"] != dir {
		t.Errorf("listening line = %v", rec)
	}
	addr := rec["addr"].(string)
	if code := statusOf(t, "http://"+addr+"/readyz"); code != 200 {
		t.Errorf("/readyz = %d", code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for taskCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hookCalls.Load() != 1 || taskCalls.Load() != 1 {
		t.Errorf("hook calls %d, task calls %d; want 1 and 1", hookCalls.Load(), taskCalls.Load())
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exited:
		if code != 0 {
			t.Errorf("exit after SIGTERM = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the hub did not stop within 2 s")
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Error("the address still accepts connections")
	}

	st, err := store.Open(context.Background(), store.Options{DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "workspaces")}, []store.Step{identity.AdminSettingsStep})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, _ := identity.AdminSettings(context.Background(), st)
	sum := sha256.Sum256(raw)
	if s.TokenHash != hex.EncodeToString(sum[:]) {
		t.Error("the stored hash is not the token file's")
	}
	for _, b := range outputs {
		if strings.Contains(b.String(), strings.TrimPrefix(tok, "parley_admin_")) || strings.Contains(b.String(), env["GH_SECRET"]) {
			t.Errorf("an output carries the token or the client secret: %s", b.String())
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLifecycle_BackupOfARunningHub(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[server]\nbind = \"127.0.0.1\"\nport = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tableA := func(c *hub.Composition) {
		c.AddStep(store.Step{Name: "test.a", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TABLE a (v INTEGER)`)
			return err
		}})
	}
	var st atomic.Pointer[store.Store]
	stderr := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)
	go func() {
		exited <- hub.Run(ctx, hub.Options{Env: config.MapEnv(nil), WorkDir: dir, Stdout: &lockedBuffer{}, Stderr: stderr, Compose: tableA,
			OnRuntime: func(rt *hub.Runtime) { st.Store(rt.Store) }})
	}()
	rec := listeningLine(t, stderr, exited)
	addr := rec["addr"].(string)
	tenRows := func(tx *sql.Tx) error {
		for i := range 10 {
			if _, err := tx.Exec(`INSERT INTO a VALUES (?)`, i); err != nil {
				return err
			}
		}
		return nil
	}
	for range 10 {
		if err := st.Load().Tx(context.Background(), tenRows); err != nil {
			t.Fatal(err)
		}
	}
	repo := filepath.Join(dir, "data", "workspaces", "r.git")
	os.MkdirAll(repo, 0o700)
	git(t, repo, "init", "--bare", "-b", "main")
	work := t.TempDir()
	git(t, work, "clone", repo, "w")
	for i := range 2 {
		os.WriteFile(filepath.Join(work, "w", "f"), []byte(fmt.Sprint(i)), 0o600)
		git(t, filepath.Join(work, "w"), "add", "f")
		git(t, filepath.Join(work, "w"), "commit", "-m", "c")
	}
	git(t, filepath.Join(work, "w"), "push", "origin", "HEAD:main")

	stop := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				writerErr <- nil
				return
			default:
			}
			if err := st.Load().Tx(context.Background(), tenRows); err != nil {
				writerErr <- err
				return
			}
		}
	}()
	target := filepath.Join(t.TempDir(), "today")
	backupErr := &lockedBuffer{}
	code := runParleyd([]string{"backup", target}, nil, dir, &lockedBuffer{}, backupErr, nil)
	if code != 0 {
		t.Fatalf("backup exit %d: %s", code, backupErr.String())
	}
	if c := statusOf(t, "http://"+addr+"/readyz"); c != 200 {
		t.Errorf("/readyz of the running hub = %d", c)
	}
	close(stop)
	if err := <-writerErr; err != nil {
		t.Errorf("the writer failed: %v", err)
	}
	cancel()
	<-exited

	dir2 := t.TempDir()
	conf2 := fmt.Sprintf("[server]\nbind = \"127.0.0.1\"\nport = 0\n[database]\npath = %q\n[workspace]\npath = %q\n",
		filepath.Join(target, "parley.db"), filepath.Join(target, "workspaces"))
	os.WriteFile(filepath.Join(dir2, "config.toml"), []byte(conf2), 0o600)
	stderr2 := &lockedBuffer{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	exited2 := make(chan int, 1)
	go func() {
		exited2 <- hub.Run(ctx2, hub.Options{Env: config.MapEnv(nil), WorkDir: dir2, Stdout: &lockedBuffer{}, Stderr: stderr2, Compose: tableA})
	}()
	rec2 := listeningLine(t, stderr2, exited2)
	if c := statusOf(t, "http://"+rec2["addr"].(string)+"/readyz"); c != 200 {
		t.Errorf("/readyz of the hub on the snapshot = %d", c)
	}
	cancel2()
	<-exited2
	db, err := sql.Open("sqlite", filepath.Join(target, "parley.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	db.QueryRow(`SELECT count(*) FROM a`).Scan(&n)
	if n%10 != 0 || n < 100 {
		t.Errorf("snapshot holds %d rows, want a multiple of 10 and at least 100", n)
	}
	git(t, filepath.Join(target, "workspaces", "r.git"), "fsck")
}
