package policy_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/store"
)

// openStore opens a store with nothing but the schema of the policy
// revisions.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(context.Background(), store.Options{
		DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{policy.SchemaStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWorkspaceDeleted_DropsTheRevisionsOfTheWorkspaceOnly(t *testing.T) {
	s, ctx, at := openStore(t), context.Background(), time.Now()
	by := policy.Writer{ActorType: "admin_token"}
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		for _, slug := range []string{"api", "web"} {
			for n := int64(1); n <= 2; n++ {
				rev := policy.Revision{Number: n, Document: policy.NeverSet, UpdatedAt: &at, UpdatedBy: &by}
				if err := policy.InsertRevision(ctx, tx, slug, rev); err != nil {
					return err
				}
			}
		}
		return (&policy.Component{}).WorkspaceDeleted(ctx, tx, "api")
	})
	if err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]int64{"api": 0, "web": 2} {
		rev, err := policy.CurrentRevision(ctx, s.Main(), slug)
		if err != nil || rev.Number != want {
			t.Errorf("current revision of %s = %d, %v; want %d", slug, rev.Number, err, want)
		}
		if _, found, err := policy.RevisionAt(ctx, s.Main(), slug, 1); err != nil || found != (want > 0) {
			t.Errorf("revision 1 of %s found = %v, %v; want %v", slug, found, err, want > 0)
		}
	}
}
