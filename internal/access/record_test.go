package access_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
)

func TestInsertRecord_KeepsTheSixValuesAcrossAReopenOfTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st := openStore(t, dir)
	var recs []access.Record
	for _, status := range []string{access.StatusPending, access.StatusCloning, access.StatusReady, access.StatusFailed, access.StatusArchived} {
		recs = append(recs, repo("acme-"+status, "u1", "o1", status))
	}
	recs = append(recs, fork("acme-ready--f1", "u2", "o1", "acme-ready"))
	seed(t, st, recs...)
	for _, rec := range recs {
		if got, found, err := access.RecordBySlug(ctx, st.Main(), rec.Slug); err != nil || !found || got != rec {
			t.Errorf("RecordBySlug(%q) = %+v, %v, %v, want %+v", rec.Slug, got, found, err, rec)
		}
	}

	st.Close()
	// The reopened store is read through the reader the rule uses.
	reader := access.StoreRecords(openStore(t, dir).Main())
	for _, rec := range recs {
		if got, found, err := reader.Record(ctx, rec.Slug); err != nil || !found || got != rec {
			t.Errorf("after a reopen, Record(%q) = %+v, %v, %v, want %+v", rec.Slug, got, found, err, rec)
		}
	}
}

func TestInsertRecord_RefusesADuplicateSlugAndKeepsTheFirstRecord(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	first := repo("acme", "u1", "o1", access.StatusReady)
	seed(t, st, first)
	if err := insert(st, repo("acme", "u2", "o1", access.StatusPending)); !errors.Is(err, access.ErrDuplicateSlug) {
		t.Errorf("second insert of acme = %v, want ErrDuplicateSlug", err)
	}
	if got, found, err := access.RecordBySlug(context.Background(), st.Main(), "acme"); err != nil || !found || got != first {
		t.Errorf("RecordBySlug(acme) = %+v, %v, %v, want the first record %+v", got, found, err, first)
	}
}

func TestInsertRecord_RefusesAnInvalidRecordAndWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	seed(t, st, repo("acme", "u1", "o1", access.StatusReady), fork("acme--f1", "u2", "o1", "acme"))
	with := func(rec access.Record, change func(*access.Record)) access.Record {
		change(&rec)
		return rec
	}
	valid := repo("new", "u1", "o1", access.StatusReady)
	for _, c := range []struct {
		name string
		rec  access.Record
	}{
		{"an empty slug", with(valid, func(r *access.Record) { r.Slug = "" })},
		{"an empty owner", with(valid, func(r *access.Record) { r.OwnerID = "" })},
		{"an empty org", with(valid, func(r *access.Record) { r.OrgID = "" })},
		{"an unknown kind", with(valid, func(r *access.Record) { r.Kind = "mirror" })},
		{"an unknown status", with(valid, func(r *access.Record) { r.Status = "deleted" })},
		{"a repository with a parent", with(valid, func(r *access.Record) { r.ParentSlug = "acme" })},
		{"a fork without a parent", fork("new", "u1", "o1", "")},
		{"a fork of no record", fork("new", "u1", "o1", "nope")},
		{"a fork of a fork", fork("new", "u1", "o1", "acme--f1")},
		{"a fork in another org than its parent", fork("new", "u1", "o2", "acme")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := insert(st, c.rec); !errors.Is(err, access.ErrInvalidRecord) {
				t.Errorf("insert %+v = %v, want ErrInvalidRecord", c.rec, err)
			}
			if _, found, err := access.RecordBySlug(ctx, st.Main(), c.rec.Slug); err != nil || found {
				t.Errorf("RecordBySlug(%q) after the refusal = %v, %v, want not found", c.rec.Slug, found, err)
			}
			var n int
			if err := st.Main().QueryRowContext(ctx, `SELECT count(*) FROM workspaces`).Scan(&n); err != nil || n != 2 {
				t.Errorf("stored records = %d, %v, want 2", n, err)
			}
		})
	}
}

func TestRecordBySlug_AnswersNotFoundWithoutAnErrorForAStringNoRecordHas(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	seed(t, st, repo("acme", "u1", "o1", access.StatusReady))
	for _, slug := range []string{"", "nope", "ACME", "acme--x", strings.Repeat("a", 300)} {
		if got, found, err := access.RecordBySlug(context.Background(), st.Main(), slug); err != nil || found {
			t.Errorf("RecordBySlug(%q) = %+v, %v, %v, want not found and no error", slug, got, found, err)
		}
	}
}
