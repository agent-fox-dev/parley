package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// txStep creates table a, whose rows carry the transaction that wrote them.
var txStep = store.Step{Name: "create_a", Target: store.Main, Apply: func(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE a (txid INTEGER NOT NULL, n INTEGER NOT NULL)`)
	return err
}}

// insertTx commits one transaction of rows rows tagged txid into table a of
// db.
func insertTx(ctx context.Context, db *sql.DB, txid int64, rows int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for n := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO a (txid, n) VALUES (?, ?)`, txid, n); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func TestSnapshot_CopiesWholeTransactionsWhileAnotherConnectionWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := options(t.TempDir())
	st := mustOpen(t, opts, []store.Step{txStep})
	for id := range int64(50) {
		if err := st.Tx(ctx, func(tx *sql.Tx) error {
			for n := range 10 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO a (txid, n) VALUES (?, ?)`, id, n); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("commit transaction %d: %v", id, err)
		}
	}

	// Writers in connections of their own, as another process's would be.
	writer, err := sql.Open("sqlite", "file:"+opts.DatabasePath+"?_pragma=busy_timeout(10000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	var (
		next     atomic.Int64
		stop     = make(chan struct{})
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
	)
	next.Store(50)
	for range 2 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := insertTx(ctx, writer, next.Add(1), 10); err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
					return
				}
			}
		})
	}
	// Let the writers commit a few transactions before the copy starts.
	for deadline := time.Now().Add(5 * time.Second); next.Load() < 55 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}

	dst := filepath.Join(t.TempDir(), "copy.db")
	err = store.Snapshot(ctx, opts.DatabasePath, dst)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(failures) > 0 {
		t.Fatalf("a writer's transaction failed during the copy: %v", errors.Join(failures...))
	}

	db := rawDB(t, dst)
	if n := count(t, db, "a"); n%10 != 0 || n < 500 {
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

func TestSnapshot_LeavesTheSourceFilesAsTheyWere(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		open bool
	}{
		{"store open", true},
		{"store closed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			opts := options(t.TempDir())
			st := mustOpen(t, opts, []store.Step{txStep})
			if err := insertTx(ctx, st.Main(), 1, 10); err != nil {
				t.Fatal(err)
			}
			if !tc.open {
				st.Close()
			}
			dataDir := filepath.Dir(opts.DatabasePath)
			before := storeFiles(t, dataDir)

			if err := store.Snapshot(ctx, opts.DatabasePath, filepath.Join(t.TempDir(), "copy.db")); err != nil {
				t.Fatalf("Snapshot: %v", err)
			}

			after := storeFiles(t, dataDir)
			if !slices.Equal(keys(after), keys(before)) {
				t.Errorf("store files after the copy = %v, before = %v", keys(after), keys(before))
			}
			for name, b := range before {
				// A reader records where it reads in the shared-memory index of
				// an open store; the data is in the other files.
				if strings.HasSuffix(name, "-shm") {
					continue
				}
				if !bytes.Equal(after[name], b) {
					t.Errorf("%s changed during the copy", name)
				}
			}
		})
	}
}

func TestSnapshot_FailsWithoutCreatingOrChangingAFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	opts := options(t.TempDir())
	st := mustOpen(t, opts, []store.Step{txStep})
	st.Close()

	t.Run("source missing", func(t *testing.T) {
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "missing.db"), filepath.Join(dir, "copy.db")
		err := store.Snapshot(ctx, src, dst)
		if err == nil || !strings.Contains(err.Error(), src) {
			t.Errorf("Snapshot = %v, want an error naming %s", err, src)
		}
		if names := dirEntries(t, dir); len(names) != 0 {
			t.Errorf("the directory holds %v after the failure, want nothing", names)
		}
	})
	t.Run("copy exists", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "copy.db")
		writeFile(t, dst, []byte("keep"))
		err := store.Snapshot(ctx, opts.DatabasePath, dst)
		if err == nil || !strings.Contains(err.Error(), dst) {
			t.Errorf("Snapshot = %v, want an error naming %s", err, dst)
		}
		if b, _ := os.ReadFile(dst); string(b) != "keep" {
			t.Errorf("the existing file holds %q after the failure, want %q", b, "keep")
		}
	})
}

// dirEntries lists the names in dir.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
