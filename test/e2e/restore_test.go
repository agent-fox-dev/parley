package e2e

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
)

// Restoring a snapshot is copying it: parleyd has no restore command. The
// tests of this file copy what parleyd backup wrote where a new working
// directory's configuration expects the data, and start parleyd there with
// no flags.

func TestRestore_AHubStartsOnTheCopyOfAnIdleHubsSnapshotWithItsDataAndTakesNewWork(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	src := t.TempDir()
	writeConfig(t, src, "[server]\nbind = \"127.0.0.1\"\nport = 0\n")
	env := config.MapEnv(nil)
	tableA := func(c *hub.Composition) {
		c.AddSchemaStep(store.Step{Name: "test_a", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "CREATE TABLE a (txn INTEGER NOT NULL, n INTEGER NOT NULL)")
			return err
		}})
	}

	// The source hub: bootstrapped, 100 rows in a, and r.git with two
	// commits.
	if code := hub.Run(ctx, hub.Options{Args: []string{"--admin-email", "a@x"}, Env: env, WorkDir: src, Compose: tableA}); code != 0 {
		t.Fatalf("the bootstrap of the source hub returned %d, want 0", code)
	}
	if err := os.Remove(filepath.Join(src, "admin_token")); err != nil {
		t.Fatal(err)
	}
	var rt1 *hub.Runtime
	source := hubtest.Serve(t, hub.Options{WorkDir: src, Env: env, Compose: func(c *hub.Composition) {
		rt1 = c.Runtime()
		tableA(c)
	}})
	for txn := range 10 {
		err := rt1.Store.Tx(ctx, func(tx *sql.Tx) error {
			for n := range 10 {
				if _, err := tx.Exec("INSERT INTO a (txn, n) VALUES (?, ?)", txn, n); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("commit to the source hub: %v", err)
		}
	}
	bareRepo(t, filepath.Join(src, "data", "workspaces", "r.git"))
	tokenHash := adminTokenHash(t, rt1.Store)
	applied := appliedSteps(t, rt1.Store)
	snapshot, code, stderr := backup(t, src, env)
	if code != 0 {
		t.Fatalf("parleyd backup returned %d, want 0; stderr = %q", code, stderr)
	}
	source.Stop()
	if code := source.ExitCode(t, 5*time.Second); code != 0 {
		t.Fatalf("the source hub returned %d after a stop, want 0", code)
	}
	snapshotHashes := hashFiles(t, snapshot)

	r := startRestored(t, snapshot, env, tableA)

	if got := r.Listening["addr"]; got != r.addr {
		t.Errorf("the restored hub listens on %v, want %s", got, r.addr)
	}
	if got := status(t, r.URL("/readyz")); got != http.StatusOK {
		t.Errorf("GET /readyz of the restored hub = %d, want 200", got)
	}
	if r.calls.Load() != 1 || r.bound.Load() || r.rows.Load() != 100 {
		t.Errorf("the start hook ran %d times, with the listener bound: %t, and read %d rows; want once, before the bind, 100 rows",
			r.calls.Load(), r.bound.Load(), r.rows.Load())
	}
	if got := adminTokenHash(t, r.rt.Store); got != tokenHash {
		t.Errorf("the restored admin token hash = %q, want the source's %q", got, tokenHash)
	}
	if got := appliedSteps(t, r.rt.Store); !slices.Equal(got[0], applied[0]) || !slices.Equal(got[1], applied[1]) {
		t.Errorf("the restored hub applied schema steps: the stores record %v, the source's recorded %v", got, applied)
	}
	err := r.rt.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO a (txn, n) VALUES (10, 0)")
		return err
	})
	if err != nil {
		t.Errorf("a transaction on the restored store: %v", err)
	}
	if err := pushNewCommit(t, filepath.Join(r.dir, "data", "workspaces", "r.git")); err != nil {
		t.Errorf("a push to the restored repository: %v", err)
	}

	r.Stop()
	if code := r.ExitCode(t, 5*time.Second); code != 0 {
		t.Errorf("the restored hub returned %d after a stop, want 0", code)
	}
	if got := hashFiles(t, snapshot); !maps.Equal(got, snapshotHashes) {
		t.Errorf("the snapshot changed: its files were %v and are %v", snapshotHashes, got)
	}
}

// The load runs from before each backup starts until after it has ended.
// Which of the transactions and pushes that overlap the backup a snapshot
// holds is open; each one it holds, it holds whole, and it holds every one
// that ended before the backup began.
func TestRestore_ABackupTakenUnderLoadRestoresToAHubWithSoundStoresAndCompleteRepositories(t *testing.T) {
	parallel(t)
	src := t.TempDir()
	writeConfig(t, src, "[server]\nbind = \"127.0.0.1\"\nport = 0\n")
	env := config.MapEnv(nil)
	steps := func(rt **hub.Runtime) func(*hub.Composition) {
		return func(c *hub.Composition) {
			*rt = c.Runtime()
			for _, s := range loadSteps {
				c.AddSchemaStep(s)
			}
		}
	}
	var rt1 *hub.Runtime
	source := hubtest.Serve(t, hub.Options{WorkDir: src, Env: env, Compose: steps(&rt1)})
	repos := loadRepos(t, filepath.Join(src, "data", "workspaces"))

	for round := range 3 {
		l := startLoad(t, rt1.Store, repos)
		// What the stores held as the backup began: a count is one read, so
		// it holds every transaction committed before it.
		committed := [2]int{rowCount(t, rt1.Store.Main(), "load"), rowCount(t, rt1.Store.Analytics(), "load_a")}
		began := time.Now()
		snapshot, code, stderr := backup(t, src, env)
		ready := status(t, source.URL("/readyz"))
		errs := l.end()
		if code != 0 {
			t.Fatalf("backup %d returned %d, want 0; stderr = %q", round, code, stderr)
		}
		if len(errs) > 0 {
			t.Errorf("backup %d: the load failed: %v", round, errs)
		}
		if ready != http.StatusOK {
			t.Errorf("backup %d: GET /readyz of the running hub = %d after the backup, want 200", round, ready)
		}

		dir := restore(t, snapshot, 0)
		var rt2 *hub.Runtime
		restored := hubtest.Serve(t, hub.Options{WorkDir: dir, Env: env, Compose: steps(&rt2)})
		if got := status(t, restored.URL("/readyz")); got != http.StatusOK {
			t.Errorf("backup %d: GET /readyz of the restored hub = %d, want 200", round, got)
		}
		for i, db := range []struct {
			name, table string
			db          *sql.DB
		}{{"parley.db", "load", rt2.Store.Main()}, {"analytics.db", "load_a", rt2.Store.Analytics()}} {
			var check string
			if err := db.db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
				t.Errorf("backup %d: the integrity check of the restored %s answered %q, %v; want ok", round, db.name, check, err)
			}
			if n := rowCount(t, db.db, db.table); n%loadBatch != 0 || n < committed[i] {
				t.Errorf("backup %d: the restored %s holds %d rows of %s, want a multiple of %d and at least the %d committed before the backup began",
					round, db.name, n, db.table, loadBatch, committed[i])
			}
		}
		for _, repo := range repos {
			copied := filepath.Join(dir, "data", "workspaces", filepath.Base(repo))
			if _, err := runGit(copied, "fsck", "--no-progress", "--no-dangling"); err != nil {
				t.Errorf("backup %d: %v", round, err)
				continue
			}
			head := git(t, copied, "rev-parse", "refs/heads/main")
			before, last := l.lastPushed(repo, began)
			if _, err := runGit(repo, "merge-base", "--is-ancestor", head, last); err != nil {
				t.Errorf("backup %d: the restored main of %s, %s, is not the source's last commit %s or an ancestor of it: %v",
					round, filepath.Base(repo), head, last, err)
			}
			if _, err := runGit(copied, "merge-base", "--is-ancestor", before, head); err != nil {
				t.Errorf("backup %d: the restored main of %s, %s, does not hold %s, pushed before the backup began: %v",
					round, filepath.Base(repo), head, before, err)
			}
			if err := pushNewCommit(t, copied); err != nil {
				t.Errorf("backup %d: a push to the restored %s: %v", round, filepath.Base(repo), err)
			}
		}
		restored.Stop()
		if code := restored.ExitCode(t, 5*time.Second); code != 0 {
			t.Errorf("backup %d: the restored hub returned %d after a stop, want 0", round, code)
		}
	}
}

// backup runs parleyd backup on the hub of workDir into a new directory and
// returns the directory, the exit status and what it wrote to stderr.
func backup(t *testing.T, workDir string, env config.Env) (dir string, code int, stderr string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "snapshot")
	var errb hubtest.LogBuffer
	code = hub.Run(context.Background(), hub.Options{Args: []string{"backup", dir}, Env: env, WorkDir: workDir, Stderr: &errb})
	return dir, code, errb.String()
}

// restore copies the snapshot's stores and workspace root into the data
// directory of a new working directory whose config.toml says where they
// are and binds 127.0.0.1 on port, and returns the working directory.
func restore(t *testing.T, snapshot string, port int) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.CopyFS(filepath.Join(data, "workspaces"), os.DirFS(filepath.Join(snapshot, "workspaces"))); err != nil {
		t.Fatalf("copy the snapshot's workspace root: %v", err)
	}
	for _, name := range []string{"parley.db", store.AnalyticsFile} {
		raw, err := os.ReadFile(filepath.Join(snapshot, name))
		if err == nil {
			err = os.WriteFile(filepath.Join(data, name), raw, 0o600)
		}
		if err != nil {
			t.Fatalf("copy the snapshot's %s: %v", name, err)
		}
	}
	writeConfig(t, dir, fmt.Sprintf("[server]\nbind = \"127.0.0.1\"\nport = %d\n\n"+
		"[database]\npath = \"data/parley.db\"\n\n[workspace]\npath = \"data/workspaces\"\n", port))
	return dir
}

// hashFiles returns the SHA-256 of every regular file under dir by its
// path relative to dir.
func hashFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	hashes := map[string]string{}
	err := fs.WalkDir(os.DirFS(dir), ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		hashes[path] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("hash the files of %s: %v", dir, err)
	}
	return hashes
}

// adminTokenHash reads the stored admin token hash from st.
func adminTokenHash(t *testing.T, st *store.Store) string {
	t.Helper()
	admin, err := identity.AdminSettings(context.Background(), st)
	if err != nil {
		t.Fatalf("read the admin settings: %v", err)
	}
	return admin.TokenHash
}

// appliedSteps returns the schema steps st's transactional and analytical
// stores record.
func appliedSteps(t *testing.T, st *store.Store) [2][]string {
	t.Helper()
	var steps [2][]string
	for i, target := range []store.Target{store.Main, store.Analytics} {
		names, err := st.AppliedSteps(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		steps[i] = names
	}
	return steps
}

// rowCount counts the rows of table in db.
func rowCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count the rows of %s: %v", table, err)
	}
	return n
}

// restoredHub is a hub started on the copy of a snapshot, and what its
// start hook saw.
type restoredHub struct {
	*hubtest.Served
	// dir is its working directory and addr the address its config.toml
	// names.
	dir, addr string
	rt        *hub.Runtime
	// calls counts the runs of the start hook, bound says whether the hook
	// reached a listener on addr, rows is the count of a it read.
	calls, rows atomic.Int64
	bound       atomic.Bool
}

// startRestored copies snapshot into a new working directory whose
// config.toml binds a port of 127.0.0.1 and starts a hub there with the
// parts of compose and a start hook that reads table a and dials the
// address.
//
// The test holds the port until the hook runs: the hub binds the address
// of its config.toml itself, which a listener handed to it would bypass,
// so the port is reserved by a listener of the test that the hook closes
// before it dials. Another process can then take the port only in the
// moment between the hook and the bind; and a hub that bound before its
// hooks ran finds the port taken and does not start.
func startRestored(t *testing.T, snapshot string, env config.Env, compose func(*hub.Composition)) *restoredHub {
	t.Helper()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	t.Cleanup(func() { reserved.Close() })
	port := reserved.Addr().(*net.TCPAddr).Port
	r := &restoredHub{dir: restore(t, snapshot, port), addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	r.Served = hubtest.Serve(t, hub.Options{WorkDir: r.dir, Env: env, Compose: func(c *hub.Composition) {
		r.rt = c.Runtime()
		compose(c)
		c.AddStartHook("read_restored", func(ctx context.Context, rt *hub.Runtime) error {
			r.calls.Add(1)
			if err := reserved.Close(); err != nil {
				return err
			}
			if conn, err := net.DialTimeout("tcp", r.addr, time.Second); err == nil {
				conn.Close()
				r.bound.Store(true)
			}
			var n int64
			if err := rt.Store.Main().QueryRowContext(ctx, "SELECT count(*) FROM a").Scan(&n); err != nil {
				return err
			}
			r.rows.Store(n)
			return nil
		})
	}})
	return r
}

// pushNewCommit pushes one new commit onto main of the bare repository
// repo from a clone of its own, and fails unless git exits with 0 and main
// is the commit afterwards.
func pushNewCommit(t *testing.T, repo string) error {
	t.Helper()
	work := t.TempDir()
	if _, err := runGit(work, "clone", "--quiet", "--bare", "--shared", repo, "."); err != nil {
		return err
	}
	out, err := runGit(work, "rev-parse", "main^{tree}", "main")
	if err != nil {
		return err
	}
	tree, parent, _ := strings.Cut(out, "\n")
	commit, err := runGit(work, "commit-tree", tree, "-p", parent, "-m", "after the restore")
	if err != nil {
		return err
	}
	if _, err := runGit(work, "push", "--quiet", repo, commit+":refs/heads/main"); err != nil {
		return err
	}
	if head, err := runGit(repo, "rev-parse", "refs/heads/main"); err != nil || head != commit {
		return fmt.Errorf("main is %s after the push of %s (%v)", head, commit, err)
	}
	return nil
}
