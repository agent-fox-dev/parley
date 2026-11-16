package policy_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/policy"
	"github.com/agent-fox-dev/parley/internal/store"
)

// openStore opens a store with nothing but the schema of the policy
// revisions and of the verdicts and check results.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(context.Background(), store.Options{
		DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{policy.SchemaStep, policy.ReviewStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWorkspaceDeleted_DropsThePolicyAndEvidenceOfTheWorkspaceOnly(t *testing.T) {
	s, ctx, at := openStore(t), context.Background(), time.Now()
	by := policy.Writer{ActorType: "admin_token"}
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		for _, slug := range []string{"api", "web"} {
			for n := int64(1); n <= 2; n++ {
				rev := policy.Revision{Number: n, Document: policy.NeverSet, UpdatedAt: &at, UpdatedBy: &by}
				if err := policy.InsertRevision(ctx, tx, slug, rev); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO change_approvals (id, workspace_slug, number, version, user_id,
	username, credential_type, credential_id, verdict, comment, created_at) VALUES (?, ?, 1, ?, 'u1', 'dana', 'pat', 't1', 'approve', '', 0)`,
					fmt.Sprintf("%s-%d", slug, n), slug, n); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO change_checks (workspace_slug, number, version, name, status, summary,
	actor_type, updated_at) VALUES (?, 1, ?, 'test', 'passed', '', 'admin_token', 0)`, slug, n); err != nil {
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
		for _, table := range []string{"change_approvals", "change_checks"} {
			var n int64
			if err := s.Main().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE workspace_slug = ?`, slug).Scan(&n); err != nil || n != want {
				t.Errorf("rows of %s in %s = %d, %v; want %d", slug, table, n, err, want)
			}
		}
	}
}

func TestReplace_KeepsTheFiftyHighestRevisions(t *testing.T) {
	s, ctx, at := openStore(t), context.Background(), time.Now()
	by := policy.Writer{ActorType: "admin_token"}
	for slug, times := range map[string]int{"api": 52, "web": 50} {
		for i := range times {
			err := s.Tx(ctx, func(tx *sql.Tx) error {
				rev, err := policy.Replace(ctx, tx, slug, policy.NeverSet, nil, by, at)
				if err == nil && rev.Number != int64(i+1) {
					err = errors.New("unexpected revision number")
				}
				return err
			})
			if err != nil {
				t.Fatalf("replacement %d of %s: %v", i+1, slug, err)
			}
		}
	}
	for slug, kept := range map[string][2]int64{"api": {3, 52}, "web": {1, 50}} {
		for n := int64(1); n <= kept[1]; n++ {
			if _, found, err := policy.RevisionAt(ctx, s.Main(), slug, n); err != nil || found != (n >= kept[0]) {
				t.Errorf("revision %d of %s found = %v, %v; want %v", n, slug, found, err, n >= kept[0])
			}
		}
		var rows int
		if err := s.Main().QueryRowContext(ctx, `SELECT count(*) FROM policy_revisions WHERE workspace_slug = ?`, slug).Scan(&rows); err != nil || rows != 50 {
			t.Errorf("stored revisions of %s = %d, %v; want 50", slug, rows, err)
		}
	}
}
