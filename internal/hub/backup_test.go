package hub_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/store"
)

// dataHub is a hub started in a working directory of its own, with table a
// in the transactional store and table b in the analytical store.
type dataHub struct {
	dir string
	rt  *hub.Runtime
	hub *running
}

// execStep returns a schema step that runs stmt.
func execStep(name string, target store.Target, stmt string) store.Step {
	return store.Step{Name: name, Target: target, Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, stmt)
		return err
	}}
}

// startDataHub starts a hub on a new data directory and waits until it
// listens.
func startDataHub(t *testing.T) *dataHub {
	t.Helper()
	d := &dataHub{dir: workDir(t, localConfig)}
	compose := func(c *hub.Composition) {
		c.AddSchemaStep(execStep("create_a", store.Main, `CREATE TABLE a (txid INTEGER NOT NULL, n INTEGER NOT NULL)`))
		c.AddSchemaStep(execStep("create_b", store.Analytics, `CREATE TABLE b (n INTEGER NOT NULL)`))
		d.rt = c.Runtime()
	}
	d.hub = startHub(t, hub.Options{WorkDir: d.dir, Compose: compose})
	return d
}

// stopped stops the hub and fails the test when it does not exit 0.
func (d *dataHub) stopped(t *testing.T) *dataHub {
	t.Helper()
	if code := d.hub.stop(t); code != 0 {
		t.Fatalf("the hub exited %d, want 0", code)
	}
	return d
}

func (d *dataHub) dbPath() string        { return filepath.Join(d.dir, "data", "parley.db") }
func (d *dataHub) workspaceRoot() string { return filepath.Join(d.dir, "data", "workspaces") }

// commit commits one transaction of rows rows tagged txid to table a.
func (d *dataHub) commit(ctx context.Context, txid int64, rows int) error {
	return d.rt.Store.Tx(ctx, func(tx *sql.Tx) error {
		for n := range rows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO a (txid, n) VALUES (?, ?)`, txid, n); err != nil {
				return err
			}
		}
		return nil
	})
}

// backup runs parleyd backup target in the hub's working directory.
func (d *dataHub) backup(t *testing.T, target string) result {
	t.Helper()
	return runToEnd(t, hub.Options{WorkDir: d.dir, Args: []string{"backup", target}})
}

// openCopy opens a store file read-only.
func openCopy(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// tree returns every path under root, relative to it, with the content of
// the regular files; a directory maps to "<dir>". A missing root is empty.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == root {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if e.IsDir() {
			files[rel] = "<dir>"
			return nil
		}
		b, err := os.ReadFile(path)
		files[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	return files
}

// checkSameTree fails the test when root's content is not want.
func checkSameTree(t *testing.T, what, root string, want map[string]string) {
	t.Helper()
	got := tree(t, root)
	if !maps.Equal(got, want) {
		t.Errorf("%s changed: paths now %v, before %v", what, slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
}

func TestRun_BacksUpBothStoresAndTheWorkspaceRootIntoAnEmptyOrMissingDirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		running       bool
		targetPresent bool
	}{
		{"hub running, target missing", true, false},
		{"hub running, target an empty directory", true, true},
		{"hub stopped, target missing", false, false},
		{"hub stopped, target an empty directory", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := startDataHub(t)
			if err := d.commit(ctx, 1, 10); err != nil {
				t.Fatalf("commit to a: %v", err)
			}
			if _, err := d.rt.Store.Analytics().ExecContext(ctx, `INSERT INTO b (n) VALUES (1), (2), (3)`); err != nil {
				t.Fatalf("insert into b: %v", err)
			}
			if !tc.running {
				d.stopped(t)
			}
			target := filepath.Join(t.TempDir(), "snapshot")
			if tc.targetPresent {
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			res := d.backup(t, target)

			if res.code != 0 {
				t.Fatalf("backup returned %d, want 0; stderr = %q", res.code, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want nothing", res.stdout)
			}
			if lines := nonEmptyLines(res.stderr); len(lines) != 1 || !strings.Contains(lines[0], target) {
				t.Errorf("stderr = %q, want one line naming %s", res.stderr, target)
			}
			if got := dirNames(t, target); !slices.Equal(got, []string{"analytics.db", "parley.db", "workspaces"}) {
				t.Errorf("the snapshot holds %v, want [analytics.db parley.db workspaces]", got)
			}
			if !tc.targetPresent {
				if mode := modeOf(t, target); mode != 0o700 {
					t.Errorf("created directory mode = %o, want 700", mode)
				}
			}
			if n := countRows(t, openCopy(t, filepath.Join(target, "parley.db")), "a"); n != 10 {
				t.Errorf("the copy of the transactional store holds %d rows in a, want 10", n)
			}
			if n := countRows(t, openCopy(t, filepath.Join(target, "analytics.db")), "b"); n != 3 {
				t.Errorf("the copy of the analytical store holds %d rows in b, want 3", n)
			}
			if tc.running {
				var doc struct{ Status string }
				if status := getJSON(t, d.hub.url("/readyz"), &doc); status != http.StatusOK {
					t.Errorf("GET /readyz after the backup = %d, want 200", status)
				}
			}
		})
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestRun_RefusesATargetThatIsNotAnEmptyDirectoryAndWritesNothing(t *testing.T) {
	t.Parallel()
	d := startDataHub(t).stopped(t)
	data := tree(t, d.dir)
	// Without symbolic links in either path, so that resolving the links of
	// the relative paths cannot make them absolute.
	cwd, err := os.Getwd()
	if err == nil {
		cwd, err = filepath.EvalSymlinks(cwd)
	}
	if err != nil {
		t.Fatal(err)
	}
	realDir, err := filepath.EvalSymlinks(d.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		// relative runs the backup from a relative working directory with
		// the target relative to it.
		relative bool
		arrange  func(t *testing.T) string
	}{
		{"a directory holding a file", false, func(t *testing.T) string {
			target := t.TempDir()
			writeFile(t, filepath.Join(target, "keep"), "kept")
			return target
		}},
		{"a directory holding an empty directory", false, func(t *testing.T) string {
			target := t.TempDir()
			if err := os.Mkdir(filepath.Join(target, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			return target
		}},
		{"a regular file", false, func(t *testing.T) string {
			target := filepath.Join(t.TempDir(), "file")
			writeFile(t, target, "not a directory")
			return target
		}},
		{"a missing directory inside the workspace root", false, func(t *testing.T) string {
			return filepath.Join(d.workspaceRoot(), "snapshot")
		}},
		{"a missing directory inside the workspace root, from a relative working directory", true, func(t *testing.T) string {
			return filepath.Join(realDir, "data", "workspaces", "snapshot")
		}},
		{"a missing directory inside the data directory", false, func(t *testing.T) string {
			return filepath.Join(filepath.Dir(d.dbPath()), "snapshot")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := tc.arrange(t)
			before := tree(t, target)

			opts := hub.Options{WorkDir: d.dir, Args: []string{"backup", target}}
			if tc.relative {
				opts.WorkDir = relativeTo(t, cwd, realDir)
				opts.Args[1] = relativeTo(t, realDir, target)
			}
			res := runToEnd(t, opts)

			if res.code != 1 {
				t.Errorf("backup returned %d, want 1", res.code)
			}
			// The refusal, not a copy that failed and was removed.
			lines := nonEmptyLines(res.stderr)
			if len(lines) != 1 || !strings.HasPrefix(fmt.Sprint(decodeLine(t, lines[0])["error"]), "backup directory "+target+" ") {
				t.Errorf("stderr = %q, want one line refusing backup directory %s", res.stderr, target)
			}
			checkSameTree(t, "the target", target, before)
			checkSameTree(t, "the hub's working directory", d.dir, data)
		})
	}
}

// relativeTo returns path relative to base.
func relativeTo(t *testing.T, base, path string) string {
	t.Helper()
	rel, err := filepath.Rel(base, path)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func TestRun_BacksUpOnlyWholeTransactionsWhileTheHubCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := startDataHub(t)
	for id := range int64(50) {
		if err := d.commit(ctx, id, 10); err != nil {
			t.Fatalf("commit transaction %d: %v", id, err)
		}
	}
	var (
		next     atomic.Int64
		stop     = make(chan struct{})
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
	)
	next.Store(50)
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := d.commit(ctx, next.Add(1), 10); err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
					return
				}
			}
		})
	}
	eventually(t, "the writers commit", func() bool { return next.Load() > 55 })

	target := filepath.Join(t.TempDir(), "snapshot")
	res := d.backup(t, target)
	close(stop)
	wg.Wait()

	if res.code != 0 {
		t.Fatalf("backup returned %d, want 0; stderr = %q", res.code, res.stderr)
	}
	if len(failures) > 0 {
		t.Fatalf("a writer's transaction failed during the backup: %v", errors.Join(failures...))
	}
	db := openCopy(t, filepath.Join(target, "parley.db"))
	if n := countRows(t, db, "a"); n%10 != 0 || n < 500 {
		t.Errorf("the copy holds %d rows, want a multiple of 10 and at least 500", n)
	}
	var partial int
	if err := db.QueryRow(`SELECT count(*) FROM (SELECT txid FROM a GROUP BY txid HAVING count(*) != 10)`).Scan(&partial); err != nil {
		t.Fatal(err)
	}
	if partial != 0 {
		t.Errorf("%d transactions are in the copy with only some of their rows", partial)
	}
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Errorf("integrity_check = %q, %v; want ok", check, err)
	}
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
		writeFile(t, filepath.Join(clone, "file"), msg)
		git(t, clone, "add", "file")
		git(t, clone, "commit", "--quiet", "-m", msg)
	}
	git(t, clone, "push", "--quiet", path, "main")
}

func TestRun_CopiesTheWorkspaceRootSoThatGitReadsEveryRepository(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is needed: %v", err)
	}
	d := startDataHub(t).stopped(t)

	empty := filepath.Join(t.TempDir(), "snapshot")
	if res := d.backup(t, empty); res.code != 0 {
		t.Fatalf("backup of an empty workspace root returned %d; stderr = %q", res.code, res.stderr)
	}
	if got := dirNames(t, filepath.Join(empty, "workspaces")); len(got) != 0 {
		t.Errorf("the copy of an empty workspace root holds %v, want nothing", got)
	}

	repos := []string{"acme/one.git", "two.git"}
	for _, r := range repos {
		bareRepo(t, filepath.Join(d.workspaceRoot(), r))
	}
	// One repository keeps its refs in packed-refs.
	git(t, filepath.Join(d.workspaceRoot(), "two.git"), "pack-refs", "--all")

	target := filepath.Join(t.TempDir(), "snapshot")
	if res := d.backup(t, target); res.code != 0 {
		t.Fatalf("backup returned %d; stderr = %q", res.code, res.stderr)
	}
	checkSameTree(t, "the copy of the workspace root", filepath.Join(target, "workspaces"), tree(t, d.workspaceRoot()))
	for _, r := range repos {
		copied := filepath.Join(target, "workspaces", r)
		git(t, copied, "fsck", "--no-progress")
		want := git(t, filepath.Join(d.workspaceRoot(), r), "rev-parse", "refs/heads/main")
		if got := git(t, copied, "rev-parse", "refs/heads/main"); got != want {
			t.Errorf("%s: refs/heads/main = %s in the copy, %s in the source", r, got, want)
		}
	}
}

func TestRun_FailsTheBackupWithOneLineAndRemovesWhatItWrote(t *testing.T) {
	t.Parallel()
	type arrangement struct {
		dir, target, want string
		// noDatabase is a path that must not exist afterwards.
		noDatabase string
	}
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T) arrangement
	}{
		{"invalid configuration", func(t *testing.T) arrangement {
			return arrangement{
				dir:    workDir(t, "[server]\nprot = 1\n"),
				target: filepath.Join(t.TempDir(), "snapshot"),
				want:   "server.prot",
			}
		}},
		{"no hub was ever started here", func(t *testing.T) arrangement {
			dir := workDir(t, localConfig)
			db := filepath.Join(dir, "data", "parley.db")
			return arrangement{dir: dir, target: filepath.Join(t.TempDir(), "snapshot"), want: db, noDatabase: db}
		}},
		{"a workspace file that cannot be read", func(t *testing.T) arrangement {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file of mode 0000")
			}
			d := startDataHub(t).stopped(t)
			locked := filepath.Join(d.workspaceRoot(), "acme", "locked")
			writeFile(t, locked, "secret")
			if err := os.Chmod(locked, 0); err != nil {
				t.Fatal(err)
			}
			// An existing empty target: the stores are copied into it before
			// the copy of the workspace root fails.
			return arrangement{dir: d.dir, target: t.TempDir(), want: locked}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := tc.arrange(t)

			res := runToEnd(t, hub.Options{WorkDir: a.dir, Args: []string{"backup", a.target}})

			if res.code != 1 {
				t.Errorf("backup returned %d, want 1", res.code)
			}
			if lines := nonEmptyLines(res.stderr); len(lines) != 1 || !strings.Contains(lines[0], a.want) {
				t.Errorf("stderr = %q, want one line naming %s", res.stderr, a.want)
			}
			if _, err := os.Stat(a.target); err == nil {
				if got := dirNames(t, a.target); len(got) != 0 {
					t.Errorf("the target holds %v after the failure, want nothing", got)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			if a.noDatabase != "" {
				if _, err := os.Stat(a.noDatabase); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s exists after the backup (%v), want no database file", a.noDatabase, err)
				}
			}
		})
	}
}

func TestRun_BacksUpWithoutStartingAnythingOrWritingToTheDataOrConfigDirectory(t *testing.T) {
	t.Parallel()
	d := startDataHub(t).stopped(t)
	// The working directory holds config.toml, so it is the config dir, and
	// the data directory, data/.
	before := tree(t, d.dir)
	var log callLog
	compose := func(c *hub.Composition) {
		c.AddStartHook("resume", func(context.Context, *hub.Runtime) error {
			log.add("start hook")
			return nil
		})
		c.AddMaintenanceTask("fork_expiry", func(context.Context, *hub.Runtime) error {
			log.add("maintenance task")
			return nil
		})
		c.AddSchemaStep(store.Step{Name: "later", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
			log.add("schema step")
			_, err := tx.ExecContext(ctx, `CREATE TABLE later (n INTEGER)`)
			return err
		}})
	}
	target := filepath.Join(t.TempDir(), "snapshot")

	res := runToEnd(t, hub.Options{WorkDir: d.dir, Args: []string{"backup", target}, Compose: compose})

	if res.code != 0 {
		t.Fatalf("backup returned %d, want 0; stderr = %q", res.code, res.stderr)
	}
	if got := log.calls(); len(got) != 0 {
		t.Errorf("the backup ran %v, want nothing", got)
	}
	if strings.Contains(res.stderr, "listening") {
		t.Errorf("stderr = %q, want no listening line", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(d.dir, "admin_token")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("admin_token exists after the backup (%v)", err)
	}
	checkSameTree(t, "the config dir and the data directory", d.dir, before)
	var later int
	if err := openCopy(t, d.dbPath()).QueryRow(`SELECT count(*) FROM schema_steps WHERE name = 'later'`).Scan(&later); err != nil {
		t.Fatal(err)
	}
	if later != 0 {
		t.Errorf("schema step 'later' is recorded in the source store")
	}

	for _, args := range [][]string{
		{"--reset-admin-token", "backup", target + "-2"},
		{"--admin-email=a@x", "backup", target + "-2"},
	} {
		if res := runToEnd(t, hub.Options{WorkDir: d.dir, Args: args}); res.code != 2 {
			t.Errorf("Run %v returned %d, want 2; stderr = %q", args, res.code, res.stderr)
		}
		if _, err := os.Stat(target + "-2"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Run %v created %s", args, target+"-2")
		}
	}
	if _, err := os.Stat(filepath.Join(d.dir, "admin_token")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("admin_token exists after the refused runs (%v)", err)
	}
}
