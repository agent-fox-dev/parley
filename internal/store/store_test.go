package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/store"
)

// recorder collects the names of the schema steps that ran, in order.
type recorder struct {
	mu  sync.Mutex
	ran []string
}

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ran)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = nil
}

// step returns a schema step that records its name and then runs sql in its
// transaction.
func (r *recorder) step(name string, target store.Target, stmt string) store.Step {
	return store.Step{Name: name, Target: target, Apply: func(ctx context.Context, tx *sql.Tx) error {
		r.mu.Lock()
		r.ran = append(r.ran, name)
		r.mu.Unlock()
		_, err := tx.ExecContext(ctx, stmt)
		return err
	}}
}

func options(dir string) store.Options {
	return store.Options{
		DatabasePath:  filepath.Join(dir, "data", "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "data", "workspaces"),
	}
}

func mustOpen(t *testing.T, opts store.Options, steps []store.Step) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), opts, steps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// openWithTableA opens a store whose one schema step creates table a.
func openWithTableA(t *testing.T) *store.Store {
	t.Helper()
	return mustOpen(t, options(t.TempDir()), []store.Step{{
		Name:   "create_a",
		Target: store.Main,
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)`)
			return err
		},
	}})
}

// rawDB opens a store file directly, bypassing the store package.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	if err != nil {
		t.Fatalf("look up table %s: %v", name, err)
	}
	return n == 1
}

func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestOpen_CreatesTheDirectoriesAndBothStores(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		DatabasePath:  filepath.Join(tmp, "a", "b", "parley.db"),
		WorkspaceRoot: filepath.Join(tmp, "c", "workspaces"),
	}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, dir := range []string{filepath.Join(tmp, "a", "b"), filepath.Join(tmp, "c", "workspaces")} {
		if got := modeOf(t, dir); got != 0o700 {
			t.Errorf("mode of %s = %o, want 700", dir, got)
		}
	}
	for _, f := range []string{"parley.db", "analytics.db"} {
		if _, err := os.Stat(filepath.Join(tmp, "a", "b", f)); err != nil {
			t.Errorf("store file %s: %v", f, err)
		}
	}
	if err := st.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	existing := filepath.Join(tmp, "d")
	if err := os.Mkdir(existing, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o750); err != nil {
		t.Fatal(err)
	}
	mustOpen(t, store.Options{
		DatabasePath:  filepath.Join(existing, "parley.db"),
		WorkspaceRoot: filepath.Join(existing, "workspaces"),
	}, nil)
	if got := modeOf(t, existing); got != 0o750 {
		t.Errorf("mode of an existing data directory = %o, want 750 as before", got)
	}
}

func TestOpen_OpensBothStoresInWALModeWithForeignKeys(t *testing.T) {
	t.Parallel()
	st := mustOpen(t, options(t.TempDir()), nil)
	for name, db := range map[string]*sql.DB{"transactional": st.Main(), "analytical": st.Analytics()} {
		var mode string
		if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatalf("%s journal_mode: %v", name, err)
		}
		if mode != "wal" {
			t.Errorf("%s journal_mode = %q, want wal", name, mode)
		}
		var fk int
		if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatalf("%s foreign_keys: %v", name, err)
		}
		if fk != 1 {
			t.Errorf("%s foreign_keys = %d, want 1", name, fk)
		}
	}
}

func TestOpen_AppliesOnlyTheSchemaStepsNotYetRecorded(t *testing.T) {
	t.Parallel()
	opts := options(t.TempDir())
	ctx := context.Background()
	rec := &recorder{}
	steps := []store.Step{
		rec.step("t1", store.Main, `CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)`),
		rec.step("t2", store.Analytics, `CREATE TABLE b (id INTEGER PRIMARY KEY)`),
		rec.step("t3", store.Main, `INSERT INTO a (v) VALUES ('kept')`),
	}

	reopen := func(steps []store.Step) {
		t.Helper()
		st, err := store.Open(ctx, opts, steps)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	reopen(steps)
	if got, want := rec.names(), []string{"t1", "t2", "t3"}; !slices.Equal(got, want) {
		t.Fatalf("first open ran %v, want %v", got, want)
	}

	rec.reset()
	reopen(steps)
	if got := rec.names(); len(got) != 0 {
		t.Fatalf("second open ran %v, want none", got)
	}

	rec.reset()
	reopen(append(slices.Clone(steps), rec.step("t4", store.Main, `ALTER TABLE a ADD COLUMN w TEXT`)))
	if got, want := rec.names(), []string{"t4"}; !slices.Equal(got, want) {
		t.Fatalf("open with an appended step ran %v, want %v", got, want)
	}

	main := rawDB(t, opts.DatabasePath)
	var v string
	if err := main.QueryRow(`SELECT v FROM a WHERE w IS NULL`).Scan(&v); err != nil || v != "kept" {
		t.Errorf("row written by t3 = %q, %v; want kept", v, err)
	}
	analytics := rawDB(t, filepath.Join(filepath.Dir(opts.DatabasePath), "analytics.db"))
	if !hasTable(t, analytics, "b") {
		t.Error("table b is missing from the analytical store")
	}
	if hasTable(t, main, "b") {
		t.Error("table b is in the transactional store")
	}
}

func TestOpen_StopsAtAFailingSchemaStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		apply func(ctx context.Context, tx *sql.Tx) error
	}{
		{"returns an error", func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `CREATE TABLE x (id INTEGER)`); err != nil {
				return err
			}
			return errors.New("step failed")
		}},
		{"panics", func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `CREATE TABLE x (id INTEGER)`); err != nil {
				return err
			}
			panic("step blew up")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := options(t.TempDir())
			ctx := context.Background()
			rec := &recorder{}
			boom := store.Step{Name: "boom", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
				rec.mu.Lock()
				rec.ran = append(rec.ran, "boom")
				rec.mu.Unlock()
				return tt.apply(ctx, tx)
			}}
			ok1 := rec.step("ok1", store.Main, `CREATE TABLE a (id INTEGER)`)
			ok3 := rec.step("ok3", store.Main, `CREATE TABLE c (id INTEGER)`)

			st, err := store.Open(ctx, opts, []store.Step{ok1, boom, ok3})
			if st != nil {
				st.Close()
				t.Fatal("Open returned a store after a failing step")
			}
			if err == nil || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("Open error = %v, want one naming boom", err)
			}
			db := rawDB(t, opts.DatabasePath)
			if !hasTable(t, db, "a") {
				t.Error("table a of the step before the failure is missing")
			}
			if hasTable(t, db, "x") {
				t.Error("table x of the failing step was kept")
			}
			if hasTable(t, db, "c") {
				t.Error("table c of the step after the failure was created")
			}
			db.Close()

			rec.reset()
			fixed := rec.step("boom", store.Main, `CREATE TABLE x (id INTEGER)`)
			mustOpen(t, opts, []store.Step{ok1, fixed, ok3})
			if got, want := rec.names(), []string{"boom", "ok3"}; !slices.Equal(got, want) {
				t.Errorf("open with the corrected list ran %v, want %v", got, want)
			}
		})
	}
}

func TestOpen_RefusesAnInvalidStepListBeforeApplyingAnything(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		steps func(rec *recorder) []store.Step
		want  string
	}{
		{"duplicated name", func(rec *recorder) []store.Step {
			return []store.Step{
				rec.step("dup", store.Main, `CREATE TABLE a (id INTEGER)`),
				rec.step("dup", store.Analytics, `CREATE TABLE b (id INTEGER)`),
			}
		}, "dup"},
		{"empty name", func(rec *recorder) []store.Step {
			return []store.Step{rec.step("", store.Main, `CREATE TABLE a (id INTEGER)`)}
		}, "name"},
		{"no function", func(*recorder) []store.Step { return []store.Step{{Name: "nofn", Target: store.Main}} }, "nofn"},
		{"unknown target", func(rec *recorder) []store.Step {
			return []store.Step{rec.step("where", store.Target(9), `CREATE TABLE a (id INTEGER)`)}
		}, "where"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			opts := options(t.TempDir())
			st, err := store.Open(context.Background(), opts, tt.steps(rec))
			if st != nil {
				st.Close()
				t.Fatal("Open returned a store for an invalid step list")
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Open error = %v, want one containing %q", err, tt.want)
			}
			if got := rec.names(); len(got) != 0 {
				t.Errorf("steps ran: %v", got)
			}
			if _, err := os.Stat(opts.DatabasePath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("store file was created (stat: %v)", err)
			}
		})
	}
}

func TestTx_CommitsOnNilAndDiscardsOnErrorOrPanic(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	insertTwo := func(ctx context.Context, tx *sql.Tx) error {
		for range 2 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO a (v) VALUES ('r')`); err != nil {
				return fmt.Errorf("insert: %w", err)
			}
		}
		return nil
	}
	tests := []struct {
		name      string
		fn        func(ctx context.Context) func(*sql.Tx) error
		wantCount int
		check     func(t *testing.T, err error)
	}{
		{"returns nil", func(ctx context.Context) func(*sql.Tx) error {
			return func(tx *sql.Tx) error { return insertTwo(ctx, tx) }
		}, 2, func(t *testing.T, err error) {
			if err != nil {
				t.Errorf("Tx = %v, want nil", err)
			}
		}},
		{"returns an error", func(ctx context.Context) func(*sql.Tx) error {
			return func(tx *sql.Tx) error {
				if err := insertTwo(ctx, tx); err != nil {
					return err
				}
				return errBoom
			}
		}, 0, func(t *testing.T, err error) {
			if !errors.Is(err, errBoom) {
				t.Errorf("Tx = %v, want the function's error", err)
			}
		}},
		{"panics", func(ctx context.Context) func(*sql.Tx) error {
			return func(tx *sql.Tx) error {
				if err := insertTwo(ctx, tx); err != nil {
					return err
				}
				panic("x marks the spot")
			}
		}, 0, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "x marks the spot") {
				t.Errorf("Tx = %v, want an error carrying the panic value", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := openWithTableA(t)
			tt.check(t, st.Tx(ctx, tt.fn(ctx)))
			if got := count(t, st.Main(), "a"); got != tt.wantCount {
				t.Errorf("rows in a = %d, want %d", got, tt.wantCount)
			}
			err := st.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO a (v) VALUES ('next')`)
				return err
			})
			if err != nil {
				t.Errorf("next transaction: %v", err)
			}
		})
	}
}

func TestTx_ConcurrentTransactionsAllComplete(t *testing.T) {
	t.Parallel()
	const workers, perWorker = 32, 25
	ctx := context.Background()
	st := openWithTableA(t)

	errs := make(chan error, workers*perWorker)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				errs <- st.Tx(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO a (v) VALUES (?)`, fmt.Sprintf("%d-%d", w, i))
					return err
				})
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Tx: %v", err)
		}
	}
	if got := count(t, st.Main(), "a"); got != workers*perWorker {
		t.Errorf("rows in a = %d, want %d", got, workers*perWorker)
	}
}

func TestOpen_FailsWithoutCreatingOrChangingAStoreFile(t *testing.T) {
	t.Parallel()
	notADatabase := bytes.Repeat([]byte("not a database"), 4096/len("not a database")+1)[:4096]
	tests := []struct {
		name string
		// setup prepares tmp and returns the options and the path the error
		// must name.
		setup func(t *testing.T, tmp string) (store.Options, string)
	}{
		{"data directory under a regular file", func(t *testing.T, tmp string) (store.Options, string) {
			writeFile(t, filepath.Join(tmp, "file"), []byte("x"))
			dir := filepath.Join(tmp, "file", "data")
			return store.Options{
				DatabasePath:  filepath.Join(dir, "parley.db"),
				WorkspaceRoot: filepath.Join(tmp, "workspaces"),
			}, dir
		}},
		{"workspace root under a regular file", func(t *testing.T, tmp string) (store.Options, string) {
			writeFile(t, filepath.Join(tmp, "file"), []byte("x"))
			root := filepath.Join(tmp, "file", "workspaces")
			return store.Options{
				DatabasePath:  filepath.Join(tmp, "data", "parley.db"),
				WorkspaceRoot: root,
			}, root
		}},
		{"transactional store is not a database", func(t *testing.T, tmp string) (store.Options, string) {
			db := filepath.Join(tmp, "data", "parley.db")
			writeFile(t, db, notADatabase)
			return store.Options{DatabasePath: db, WorkspaceRoot: filepath.Join(tmp, "workspaces")}, db
		}},
		{"analytical store is not a database", func(t *testing.T, tmp string) (store.Options, string) {
			analytics := filepath.Join(tmp, "data", "analytics.db")
			writeFile(t, analytics, notADatabase)
			return store.Options{
				DatabasePath:  filepath.Join(tmp, "data", "parley.db"),
				WorkspaceRoot: filepath.Join(tmp, "workspaces"),
			}, analytics
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			opts, failing := tt.setup(t, tmp)
			before := storeFiles(t, filepath.Dir(opts.DatabasePath))

			st, err := store.Open(context.Background(), opts, nil)
			if st != nil {
				st.Close()
				t.Fatal("Open returned a store")
			}
			if err == nil || !strings.Contains(err.Error(), failing) {
				t.Fatalf("Open error = %v, want one naming %s", err, failing)
			}
			after := storeFiles(t, filepath.Dir(opts.DatabasePath))
			if len(after) != len(before) {
				t.Errorf("store files after the failed open = %v, before = %v", keys(after), keys(before))
			}
			for name, b := range before {
				if !bytes.Equal(after[name], b) {
					t.Errorf("%s changed", name)
				}
			}
		})
	}
}

func TestStore_PingAndCloseReportAStoreThatNoLongerAnswers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name  string
		close func(st *store.Store) error
		want  error
	}{
		{"transactional store closed", func(st *store.Store) error { return st.Main().Close() }, nil},
		{"analytical store closed", func(st *store.Store) error { return st.Analytics().Close() }, nil},
		{"store closed", func(st *store.Store) error { return st.Close() }, store.ErrClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := openWithTableA(t)
			if err := st.Ping(ctx); err != nil {
				t.Fatalf("Ping before closing: %v", err)
			}
			if err := tt.close(st); err != nil {
				t.Fatalf("close: %v", err)
			}
			err := st.Ping(ctx)
			if err == nil {
				t.Fatal("Ping answered nil after a store was closed")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("Ping = %v, want %v", err, tt.want)
			}
		})
	}

	st := openWithTableA(t)
	if err := st.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if err := st.Tx(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Tx after Close = %v, want ErrClosed", err)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// storeFiles returns the content of every store file in dir, keyed by name.
func storeFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, base := range []string{"parley.db", "analytics.db"} {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			b, err := os.ReadFile(filepath.Join(dir, base+suffix))
			if err == nil {
				files[base+suffix] = b
			}
		}
	}
	return files
}

func keys(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
