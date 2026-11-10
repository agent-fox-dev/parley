package hub

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/store"
)

// backupSteps create table a in the transactional store and b in the
// analytical one.
func backupSteps(c *Composition) {
	c.AddStep(store.Step{Name: "test.a", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE a (tx INTEGER, v INTEGER)`)
		return err
	}})
	c.AddStep(store.Step{Name: "test.b", Target: store.Analytical, Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE b (v INTEGER)`)
		return err
	}})
}

func insertRows(t *testing.T, st *store.Store, main, analytics int) {
	t.Helper()
	ctx := context.Background()
	for i := range main {
		if err := st.Tx(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(`INSERT INTO a VALUES (?, ?)`, -1, i); return err }); err != nil {
			t.Fatal(err)
		}
	}
	for i := range analytics {
		if err := st.AnalyticsTx(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(`INSERT INTO b VALUES (?)`, i); return err }); err != nil {
			t.Fatal(err)
		}
	}
}

func countIn(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return n
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestBackup_WritesBothStoresAndTheWorkspaceRoot(t *testing.T) {
	for _, running := range []bool{true, false} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("running=%v existing=%v", running, existing), func(t *testing.T) {
				dir := hubDir(t, loopbackConfig)
				h := startHub(t, dir, nil, withCompose(backupSteps))
				insertRows(t, h.rt.Load().Store, 10, 3)
				if !running {
					h.stop()
				}
				target := filepath.Join(t.TempDir(), "today")
				if existing {
					if err := os.Mkdir(target, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				code, stdout, stderr := runHub(t, dir, nil, []string{"backup", target}, withCompose(backupSteps))
				if code != 0 {
					t.Fatalf("exit %d: %s", code, stderr)
				}
				if stdout != "" {
					t.Errorf("stdout = %q", stdout)
				}
				if got := names(t, target); strings.Join(got, ",") != "analytics.db,parley.db,workspaces" {
					t.Errorf("target holds %v", got)
				}
				if n := countIn(t, filepath.Join(target, "parley.db"), "a"); n != 10 {
					t.Errorf("copy of a holds %d rows, want 10", n)
				}
				if n := countIn(t, filepath.Join(target, "analytics.db"), "b"); n != 3 {
					t.Errorf("copy of b holds %d rows, want 3", n)
				}
				if running {
					if code, _ := h.get("/readyz"); code != 200 {
						t.Errorf("/readyz after the backup = %d", code)
					}
				}
			})
		}
	}
}

func TestBackup_RefusesATargetThatIsNotAnEmptyDirectory(t *testing.T) {
	dir := hubDir(t, loopbackConfig)
	startHub(t, dir, nil).stop()
	base := t.TempDir()
	withFile := filepath.Join(base, "with-file")
	withDir := filepath.Join(base, "with-dir")
	regular := filepath.Join(base, "regular")
	os.MkdirAll(withFile, 0o700)
	os.WriteFile(filepath.Join(withFile, "keep"), []byte("keep"), 0o600)
	os.MkdirAll(filepath.Join(withDir, "sub"), 0o700)
	os.WriteFile(regular, []byte("file"), 0o600)
	for _, target := range []string{withFile, withDir, regular} {
		before := snapshotTree(t, target)
		code, _, stderr := runHub(t, dir, nil, []string{"backup", target})
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", target, code)
		}
		if lines := nonEmptyLines(stderr); len(lines) != 1 || !strings.Contains(stderr, target) {
			t.Errorf("%s: stderr = %s", target, stderr)
		}
		if after := snapshotTree(t, target); after != before {
			t.Errorf("%s changed", target)
		}
	}
}

// snapshotTree describes the names and bytes under a path.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		data := ""
		if !fi.IsDir() {
			raw, _ := os.ReadFile(p)
			data = string(raw)
		}
		fmt.Fprintf(&b, "%s %v %q\n", p, fi.IsDir(), data)
		return nil
	})
	return b.String()
}

func TestBackup_CopiesWholeTransactionsOnly(t *testing.T) {
	dir := hubDir(t, loopbackConfig)
	h := startHub(t, dir, nil, withCompose(backupSteps))
	st := h.rt.Load().Store
	var txID atomic.Int64
	commit := func() error {
		id := txID.Add(1)
		return st.Tx(context.Background(), func(tx *sql.Tx) error {
			for i := range 10 {
				if _, err := tx.Exec(`INSERT INTO a VALUES (?, ?)`, id, i); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for range 50 {
		if err := commit(); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writerErr atomic.Value
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := commit(); err != nil {
					writerErr.Store(err)
					return
				}
			}
		}()
	}
	target := filepath.Join(t.TempDir(), "snap")
	code, _, stderr := runHub(t, dir, nil, []string{"backup", target}, withCompose(backupSteps))
	close(stop)
	wg.Wait()
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if err := writerErr.Load(); err != nil {
		t.Errorf("a writer failed: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(target, "parley.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	var check string
	db.QueryRow(`SELECT count(*) FROM a`).Scan(&n)
	db.QueryRow(`PRAGMA integrity_check`).Scan(&check)
	if n%10 != 0 || n < 500 || check != "ok" {
		t.Errorf("copy holds %d rows, integrity %q", n, check)
	}
	var partial int
	db.QueryRow(`SELECT count(*) FROM (SELECT tx FROM a GROUP BY tx HAVING count(*) != 10)`).Scan(&partial)
	if partial != 0 {
		t.Errorf("%d transactions are in the copy in part", partial)
	}
}

// gitRun runs git with a fixed identity and no user configuration.
func gitRun(t *testing.T, dir string, args ...string) string {
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

// bareRepo creates a bare repository at path with two commits on main.
func bareRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, path, "init", "--bare", "-b", "main")
	work := t.TempDir()
	gitRun(t, work, "clone", path, "w")
	w := filepath.Join(work, "w")
	for i := range 2 {
		os.WriteFile(filepath.Join(w, "f"), []byte(fmt.Sprint(i)), 0o600)
		gitRun(t, w, "add", "f")
		gitRun(t, w, "commit", "-m", fmt.Sprint("c", i))
	}
	gitRun(t, w, "push", "origin", "HEAD:main")
}

func TestBackup_CopiesTheRepositories(t *testing.T) {
	dir := hubDir(t, loopbackConfig)
	startHub(t, dir, nil).stop()
	ws := filepath.Join(dir, "data", "workspaces")
	repos := []string{"acme/one.git", "two.git"}
	for _, r := range repos {
		bareRepo(t, filepath.Join(ws, r))
	}
	target := filepath.Join(t.TempDir(), "snap")
	if code, _, stderr := runHub(t, dir, nil, []string{"backup", target}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, r := range repos {
		copied := filepath.Join(target, "workspaces", r)
		gitRun(t, copied, "fsck")
		if got, want := gitRun(t, copied, "rev-parse", "refs/heads/main"), gitRun(t, filepath.Join(ws, r), "rev-parse", "refs/heads/main"); got != want {
			t.Errorf("%s: main = %s, want %s", r, got, want)
		}
	}

	empty := hubDir(t, loopbackConfig)
	startHub(t, empty, nil).stop()
	target2 := filepath.Join(t.TempDir(), "snap")
	if code, _, stderr := runHub(t, empty, nil, []string{"backup", target2}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := names(t, filepath.Join(target2, "workspaces")); len(got) != 0 {
		t.Errorf("empty workspace root copied as %v", got)
	}
}

func TestBackup_AFailureRemovesWhatItWrote(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T) (dir, want string)
	}{
		{"invalid configuration", func(t *testing.T) (string, string) {
			return hubDir(t, "[server]\nprot = 1\n"), "server.prot"
		}},
		{"no hub was ever started", func(t *testing.T) (string, string) {
			dir := hubDir(t, loopbackConfig)
			return dir, filepath.Join(dir, "data", "parley.db")
		}},
	}
	if os.Geteuid() != 0 {
		tests = append(tests, struct {
			name    string
			prepare func(t *testing.T) (dir, want string)
		}{"a file of the workspace root cannot be read", func(t *testing.T) (string, string) {
			dir := hubDir(t, loopbackConfig)
			startHub(t, dir, nil).stop()
			locked := filepath.Join(dir, "data", "workspaces", "locked")
			os.WriteFile(locked, []byte("x"), 0o600)
			os.Chmod(locked, 0)
			t.Cleanup(func() { os.Chmod(locked, 0o600) })
			return dir, locked
		}})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, want := tt.prepare(t)
			target := filepath.Join(t.TempDir(), "snap")
			code, _, stderr := runHub(t, dir, nil, []string{"backup", target})
			if code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			if lines := nonEmptyLines(stderr); len(lines) != 1 || !strings.Contains(stderr, want) {
				t.Errorf("stderr = %s, want one line naming %s", stderr, want)
			}
			if _, err := os.Stat(target); err == nil && len(names(t, target)) != 0 {
				t.Errorf("target left with %v", names(t, target))
			}
			if tt.name == "no hub was ever started" {
				if _, err := os.Stat(want); err == nil {
					t.Error("the database was created")
				}
			}
		})
	}
}

func TestBackup_LeavesTheHubAlone(t *testing.T) {
	dir := hubDir(t, loopbackConfig)
	startHub(t, dir, nil).stop()
	var calls []string
	compose := withCompose(func(c *Composition) {
		c.AddStartHook("hook", func(context.Context, *Runtime) error { calls = append(calls, "hook"); return nil })
		c.AddTask("retention", func(context.Context, *Runtime) error { calls = append(calls, "task"); return nil })
		c.AddStep(store.Step{Name: "later", Target: store.Main, Apply: func(context.Context, *sql.Tx) error {
			calls = append(calls, "step")
			return nil
		}})
	})
	target := filepath.Join(t.TempDir(), "snap")
	code, _, stderr := runHub(t, dir, nil, []string{"backup", target}, compose)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(calls) != 0 || strings.Contains(stderr, "listening") {
		t.Errorf("calls %v, stderr %s", calls, stderr)
	}
	if _, err := os.Stat(tokenFile(dir)); err == nil {
		t.Error("an admin token file was created")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "data", "parley.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	db.QueryRow(`SELECT count(*) FROM schema_steps WHERE name = 'later'`).Scan(&n)
	if n != 0 {
		t.Error("the step 'later' was applied to the source")
	}
	for _, args := range [][]string{{"--reset-admin-token", "backup", target}, {"--admin-email", "a@x", "backup", target}} {
		if code, _, _ := runHub(t, dir, nil, args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
