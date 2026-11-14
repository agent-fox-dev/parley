package workspace_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/store"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// openStore opens a fresh store with the workspace table as the hub builds
// it: the access rule's steps, then this package's.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(dir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{access.SchemaStep, access.CreatingTokenStep, workspace.SchemaStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestStore_KeepsEveryFieldAndUpdatesTheCloneFieldsInATransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t)
	t0 := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	repo := workspace.Workspace{
		Slug: "acme", Kind: access.KindRepository, Mode: workspace.ModeCarryPatch,
		GitURL: new("https://example.invalid/acme.git"), DefaultBranch: new("main"),
		OwnerID: "u1", OrgID: "o1", Status: workspace.StatusPending,
		DisplayName: "Acme", Description: "the acme repository",
		CloneError: new("earlier failure"), LastSyncAt: new(t0.Add(-time.Hour)), SyncError: new("sync failed"),
		UpstreamURL: new("https://example.invalid/up.git"), IntegrationBranch: new("deploy"), UpstreamHeadSHA: new("u0"),
		Settings:  map[string]json.RawMessage{"sync_enabled": json.RawMessage(`false`)},
		CreatedAt: t0, UpdatedAt: t0, InstanceID: workspace.NewInstanceID(), Marker: workspace.MarkerReclone,
	}
	fork := workspace.Workspace{
		Slug: "acme--f1", Kind: access.KindFork, Mode: workspace.ModeStandard, DefaultBranch: new("main"),
		OwnerID: "u2", OrgID: "o1", Status: workspace.StatusReady, DisplayName: "acme--f1",
		HeadSHA: new("abc"), ParentSlug: new("acme"), ForkPoint: &workspace.ForkPoint{Ref: "main", SHA: "abc"},
		ExpiresAt: new(t0.Add(7 * 24 * time.Hour)), AutoChange: new(true), Purpose: new("fix a bug"),
		SessionID: new("s1"), Settings: map[string]json.RawMessage{}, CreatedAt: t0, UpdatedAt: t0,
		InstanceID: workspace.NewInstanceID(), CreatingTokenID: "tok-1",
	}
	tx := func(fn func(*sql.Tx) error) error { return st.Tx(ctx, fn) }
	for _, w := range []workspace.Workspace{repo, fork} {
		if err := tx(func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, w) }); err != nil {
			t.Fatalf("Insert(%s) = %v", w.Slug, err)
		}
	}
	read := func(want workspace.Workspace) {
		t.Helper()
		got, found, err := workspace.BySlug(ctx, st.Main(), want.Slug)
		if err != nil || !found || !reflect.DeepEqual(got, want) {
			t.Errorf("BySlug(%s) = %+v, %v, %v, want %+v", want.Slug, got, found, err, want)
		}
	}
	read(repo)
	read(fork)
	if repo.InstanceID == fork.InstanceID || len(repo.InstanceID) < 32 {
		t.Errorf("instance ids %q and %q, want two distinct random ids", repo.InstanceID, fork.InstanceID)
	}
	if rec, _, err := access.RecordBySlug(ctx, st.Main(), "acme--f1"); err != nil || rec.ParentSlug != "acme" || rec.CreatingTokenID != "tok-1" {
		t.Errorf("the access rule reads %+v, %v, want the fork's record", rec, err)
	}

	clone := workspace.CloneFields{Status: workspace.StatusReady, DefaultBranch: new("main"),
		HeadSHA: new("def"), OriginHeadSHA: new("def"), UpdatedAt: t0.Add(time.Minute)}
	if err := tx(func(tx *sql.Tx) error { return workspace.UpdateClone(ctx, tx, "acme", clone) }); err != nil {
		t.Fatalf("UpdateClone = %v", err)
	}
	repo.Status, repo.HeadSHA, repo.OriginHeadSHA, repo.CloneError, repo.UpdatedAt = workspace.StatusReady, new("def"), new("def"), nil, t0.Add(time.Minute)
	read(repo)

	if err := tx(func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, fork) }); !errors.Is(err, access.ErrDuplicateSlug) {
		t.Errorf("a second Insert(acme--f1) = %v, want ErrDuplicateSlug", err)
	}
	if err := tx(func(tx *sql.Tx) error { return workspace.UpdateClone(ctx, tx, "nope", clone) }); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("UpdateClone(nope) = %v, want ErrNotFound", err)
	}
	if _, found, err := workspace.BySlug(ctx, st.Main(), "nope"); found || err != nil {
		t.Errorf("BySlug(nope) = found %v, %v, want not found and no error", found, err)
	}
}
