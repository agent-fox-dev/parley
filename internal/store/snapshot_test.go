package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSnapshot_CopiesWholeTransactionsWhileWritersCommit(t *testing.T) {
	st := storeWithTableA(t)
	tenRows := func(tx *sql.Tx) error {
		for range 10 {
			if err := insertOne(tx); err != nil {
				return err
			}
		}
		return nil
	}
	for range 50 {
		if err := st.Tx(ctx, tenRows); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 4)
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
				if err := st.Tx(ctx, tenRows); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	dst := filepath.Join(t.TempDir(), "copy.db")
	err := Snapshot(ctx, st.DatabasePath(), dst)
	close(stop)
	wg.Wait()
	close(errs)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for e := range errs {
		t.Errorf("a writer failed: %v", e)
	}
	copyDB := rawDB(t, dst)
	var n int
	var check string
	if err := copyDB.QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := copyDB.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Errorf("integrity_check = %q, %v", check, err)
	}
	if n%10 != 0 || n < 500 {
		t.Errorf("copy holds %d rows, want a multiple of 10 and at least 500", n)
	}
}

func TestSnapshot_RefusesAMissingSourceWithoutCreatingIt(t *testing.T) {
	src := filepath.Join(t.TempDir(), "nothing.db")
	err := Snapshot(ctx, src, filepath.Join(t.TempDir(), "copy.db"))
	if err == nil || !strings.Contains(err.Error(), src) {
		t.Fatalf("got %v, want an error naming %s", err, src)
	}
	if _, statErr := os.Stat(src); statErr == nil {
		t.Error("the missing source was created")
	}
}
