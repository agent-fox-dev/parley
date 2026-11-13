package access_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/store"
)

// openStore opens the transactional store in dir, on disk, with the
// workspace record step, and closes it when the test ends. Opening the same
// dir again reopens the same store.
func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(dir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{access.SchemaStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// repo is a repository record.
func repo(slug, owner, org, status string) access.Record {
	return access.Record{Slug: slug, OwnerID: owner, OrgID: org, Kind: access.KindRepository, Status: status}
}

// fork is a ready fork record of parent.
func fork(slug, owner, org, parent string) access.Record {
	return access.Record{Slug: slug, OwnerID: owner, OrgID: org, Kind: access.KindFork, ParentSlug: parent, Status: access.StatusReady}
}

// insert inserts rec in a transaction of its own.
func insert(st *store.Store, rec access.Record) error {
	return st.Tx(context.Background(), func(tx *sql.Tx) error {
		return access.InsertRecord(context.Background(), tx, rec)
	})
}

// seed inserts recs in order and fails the test at the first refusal.
func seed(t *testing.T, st *store.Store, recs ...access.Record) {
	t.Helper()
	for _, rec := range recs {
		if err := insert(st, rec); err != nil {
			t.Fatalf("insert %+v: %v", rec, err)
		}
	}
}
