package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
		t.Parallel()
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
		t.Parallel()
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
