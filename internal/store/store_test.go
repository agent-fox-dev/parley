package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var ctx = context.Background()

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func open(t *testing.T, opts Options, steps []Step) *Store {
	t.Helper()
	st, err := Open(ctx, opts, steps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

func exec(stmt string) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, stmt)
		return err
	}
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpen_CreatesTheDirectoriesAndBothStores(t *testing.T) {
	tmp := t.TempDir()
	st := open(t, Options{DatabasePath: filepath.Join(tmp, "a/b/parley.db"), WorkspaceRoot: filepath.Join(tmp, "c/workspaces")}, nil)
	if m := mode(t, filepath.Join(tmp, "a/b")); m != 0o700 {
		t.Errorf("data directory mode = %o, want 700", m)
	}
	if m := mode(t, filepath.Join(tmp, "c/workspaces")); m != 0o700 {
		t.Errorf("workspace root mode = %o, want 700", m)
	}
	for _, f := range []string{"a/b/parley.db", "a/b/analytics.db"} {
		if _, err := os.Stat(filepath.Join(tmp, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if err := st.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	st.Close()

	existing := filepath.Join(tmp, "d")
	if err := os.Mkdir(existing, 0o750); err != nil {
		t.Fatal(err)
	}
	open(t, Options{DatabasePath: filepath.Join(existing, "parley.db"), WorkspaceRoot: filepath.Join(tmp, "e")}, nil).Close()
	if m := mode(t, existing); m != 0o750 {
		t.Errorf("existing directory mode = %o, want 750 kept", m)
	}
}

func TestOpen_AppliesEachSchemaStepOnceInOrder(t *testing.T) {
	tmp := t.TempDir()
	opts := Options{DatabasePath: filepath.Join(tmp, "parley.db"), WorkspaceRoot: filepath.Join(tmp, "ws")}
	var ran []string
	step := func(name string, target Target, stmt string) Step {
		return Step{Name: name, Target: target, Apply: func(ctx context.Context, tx *sql.Tx) error {
			ran = append(ran, name)
			return exec(stmt)(ctx, tx)
		}}
	}
	steps := []Step{
		step("t1", Main, `CREATE TABLE a (x INTEGER)`),
		step("t2", Analytical, `CREATE TABLE b (y INTEGER)`),
		step("t3", Main, `INSERT INTO a VALUES (7)`),
	}
	open(t, opts, steps).Close()
	if strings.Join(ran, ",") != "t1,t2,t3" {
		t.Fatalf("first open ran %v", ran)
	}
	ran = nil
	open(t, opts, steps).Close()
	if len(ran) != 0 {
		t.Fatalf("second open ran %v", ran)
	}
	st := open(t, opts, append(steps, step("t4", Main, `ALTER TABLE a ADD COLUMN z TEXT`)))
	defer st.Close()
	if strings.Join(ran, ",") != "t4" {
		t.Fatalf("third open ran %v", ran)
	}
	var x int
	if err := st.DB().QueryRow(`SELECT x FROM a`).Scan(&x); err != nil || x != 7 {
		t.Errorf("row of t3 = %d, %v", x, err)
	}
	if !tableExists(t, st.Analytics(), "b") || tableExists(t, st.DB(), "b") {
		t.Error("table b is not in the analytical store alone")
	}
}

func TestOpen_AFailingStepIsRolledBackAndStopsTheOpen(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "parley.db")
	opts := Options{DatabasePath: db, WorkspaceRoot: filepath.Join(tmp, "ws")}
	var ran []string
	fail := true
	boom := Step{Name: "boom", Target: Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
		ran = append(ran, "boom")
		if _, err := tx.ExecContext(ctx, `CREATE TABLE x (v INTEGER)`); err != nil {
			return err
		}
		if fail {
			return errors.New("step broke")
		}
		return nil
	}}
	steps := []Step{
		{Name: "ok1", Target: Main, Apply: func(ctx context.Context, tx *sql.Tx) error { ran = append(ran, "ok1"); return exec(`CREATE TABLE a (v)`)(ctx, tx) }},
		boom,
		{Name: "ok3", Target: Main, Apply: func(ctx context.Context, tx *sql.Tx) error { ran = append(ran, "ok3"); return exec(`CREATE TABLE c (v)`)(ctx, tx) }},
	}
	st, err := Open(ctx, opts, steps)
	if st != nil || err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v, %v; want an error naming boom", st, err)
	}
	raw := rawDB(t, db)
	if !tableExists(t, raw, "a") || tableExists(t, raw, "x") || tableExists(t, raw, "c") {
		t.Error("want table a only")
	}
	raw.Close()

	fail, ran = false, nil
	open(t, opts, steps).Close()
	if strings.Join(ran, ",") != "boom,ok3" {
		t.Errorf("corrected open ran %v", ran)
	}

	fresh := t.TempDir()
	dup := []Step{
		{Name: "dup", Target: Main, Apply: exec(`CREATE TABLE d1 (v)`)},
		{Name: "dup", Target: Main, Apply: exec(`CREATE TABLE d2 (v)`)},
	}
	st, err = Open(ctx, Options{DatabasePath: filepath.Join(fresh, "parley.db"), WorkspaceRoot: filepath.Join(fresh, "ws")}, dup)
	if st != nil || err == nil || !strings.Contains(err.Error(), "dup") {
		t.Fatalf("duplicate names: got %v, %v", st, err)
	}
	if _, statErr := os.Stat(filepath.Join(fresh, "parley.db")); statErr == nil {
		raw := rawDB(t, filepath.Join(fresh, "parley.db"))
		if tableExists(t, raw, "d1") || tableExists(t, raw, "d2") {
			t.Error("a step ran although two steps share a name")
		}
	}
}

func storeWithTableA(t *testing.T) *Store {
	t.Helper()
	tmp := t.TempDir()
	st := open(t, Options{DatabasePath: filepath.Join(tmp, "parley.db"), WorkspaceRoot: filepath.Join(tmp, "ws")},
		[]Step{{Name: "a", Target: Main, Apply: exec(`CREATE TABLE a (v INTEGER)`)}})
	t.Cleanup(func() { st.Close() })
	return st
}

func count(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func insertOne(tx *sql.Tx) error {
	_, err := tx.Exec(`INSERT INTO a VALUES (1)`)
	return err
}

func TestTx_CommitsOrLeavesNoTrace(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name  string
		fn    func(tx *sql.Tx) error
		check func(err error, n int) bool
	}{
		{"returns nil", func(tx *sql.Tx) error { _ = insertOne(tx); return insertOne(tx) }, func(err error, n int) bool { return err == nil && n == 2 }},
		{"returns an error", func(tx *sql.Tx) error { _ = insertOne(tx); _ = insertOne(tx); return errBoom }, func(err error, n int) bool { return errors.Is(err, errBoom) && n == 0 }},
		{"panics", func(tx *sql.Tx) error { _ = insertOne(tx); _ = insertOne(tx); panic("x") }, func(err error, n int) bool { return err != nil && strings.Contains(err.Error(), "x") && n == 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := storeWithTableA(t)
			err := st.Tx(ctx, tt.fn)
			if n := count(t, st); !tt.check(err, n) {
				t.Errorf("err %v, rows %d", err, n)
			}
			if err := st.Tx(ctx, insertOne); err != nil {
				t.Errorf("the next transaction failed: %v", err)
			}
		})
	}
}

func TestTx_ConcurrentTransactionsAllComplete(t *testing.T) {
	st := storeWithTableA(t)
	var wg sync.WaitGroup
	errs := make(chan error, 800)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if err := st.Tx(ctx, insertOne); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("transaction failed: %v", err)
	}
	if n := count(t, st); n != 800 {
		t.Errorf("rows = %d, want 800", n)
	}
}

func TestOpen_FailsOnAPathItCannotUseWithoutChangingAFile(t *testing.T) {
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	notDB := filepath.Join(tmp, "notdb", "parley.db")
	if err := os.MkdirAll(filepath.Dir(notDB), 0o700); err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("not a database"), 300)[:4096]
	if err := os.WriteFile(notDB, content, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		opts    Options
		failing string
	}{
		{"database under a file", Options{DatabasePath: filepath.Join(blocker, "data/parley.db"), WorkspaceRoot: filepath.Join(tmp, "ws1")}, filepath.Join(blocker, "data")},
		{"workspace root under a file", Options{DatabasePath: filepath.Join(tmp, "ok/parley.db"), WorkspaceRoot: filepath.Join(blocker, "ws")}, filepath.Join(blocker, "ws")},
		{"not a database", Options{DatabasePath: notDB, WorkspaceRoot: filepath.Join(tmp, "ws3")}, notDB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := Open(ctx, tt.opts, nil)
			if st != nil || err == nil || !strings.Contains(err.Error(), tt.failing) {
				t.Fatalf("got %v, %v; want an error naming %s", st, err, tt.failing)
			}
		})
	}
	if got, _ := os.ReadFile(notDB); !bytes.Equal(got, content) {
		t.Error("the file that was not a database was changed")
	}
}

func TestClose_LaterCallsFailWithAClosedStoreError(t *testing.T) {
	st := storeWithTableA(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if err := st.Ping(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Ping after Close = %v, want ErrClosed", err)
	}
	if err := st.Tx(ctx, insertOne); !errors.Is(err, ErrClosed) {
		t.Errorf("Tx after Close = %v, want ErrClosed", err)
	}
}
