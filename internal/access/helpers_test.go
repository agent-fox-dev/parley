package access_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
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

// memberships is a membership source stub: the orgs each user is a member
// of, of which it answers only those in active, as the real source answers
// only active orgs.
type memberships struct {
	of     map[string][]string
	active []string
}

func (m memberships) ActiveOrgIDs(_ context.Context, userID string) ([]string, error) {
	var ids []string
	for _, id := range m.of[userID] {
		if slices.Contains(m.active, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// errRead is the failure of the failing sources.
var errRead = errors.New("read failed")

// failingRecords reads through RecordReader, except the record of a slug in
// fail, whose read fails with errRead.
type failingRecords struct {
	access.RecordReader
	fail []string
}

func (f failingRecords) Record(ctx context.Context, slug string) (access.Record, bool, error) {
	if slices.Contains(f.fail, slug) {
		return access.Record{}, false, errRead
	}
	return f.RecordReader.Record(ctx, slug)
}

// failingMemberships is a membership source whose every read fails with
// errRead.
var failingMemberships = access.MembershipsFunc(func(context.Context, string) ([]string, error) {
	return nil, errRead
})

// newService is a service bound to records and m.
func newService(records access.RecordReader, m access.Memberships) *access.Service {
	svc := &access.Service{}
	svc.Bind(access.Deps{Records: records, Memberships: m})
	return svc
}
