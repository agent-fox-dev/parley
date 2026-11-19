package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The service is the sandbox part of a workspace's archive and deletion
// (ADR 07) and the sandbox lifecycle of a fork's removal.
var (
	_ workspace.SandboxRemover  = (*Service)(nil)
	_ workspace.ArchiveObserver = (*Service)(nil)
	_ fork.SandboxLifecycle     = (*Service)(nil)
)

// RemoveSandboxes deletes by the system every sandbox of the workspaces
// slugs that is not deleted, with reason DeleteWorkspaceDeleted and no
// checkpoint, since the workspaces and their refs are going: the sandbox
// remover a workspace's deletion calls before it removes anything. What the
// runtime cannot delete, because it cannot be reached or fails, is left to
// the reconciliation, as for every deletion. It returns an error only when
// a deletion could not be recorded, naming the sandbox, after trying the
// others.
func (s *Service) RemoveSandboxes(ctx context.Context, slugs []string) error {
	return s.deleteIn(ctx, slugs, DeleteWorkspaceDeleted, false)
}

// DeleteSandboxes deletes by the system every sandbox of the workspace slug
// that is not deleted, giving reason, with a checkpoint of each ready one
// first when checkpoint is set and reason is not DeleteWorkspaceDeleted: the
// deletion a fork's removal asks for. Its error is that of RemoveSandboxes.
func (s *Service) DeleteSandboxes(ctx context.Context, slug, reason string, checkpoint bool) error {
	return s.deleteIn(ctx, []string{slug}, reason, checkpoint)
}

// deleteIn deletes by the system every sandbox of the workspaces slugs that
// is not deleted, for reason, with the checkpoint switch given.
func (s *Service) deleteIn(ctx context.Context, slugs []string, reason string, checkpoint bool) error {
	if s.deps.Store == nil || len(slugs) == 0 {
		return nil
	}
	recs, err := listIn(ctx, s.deps.Store.Main(), slugs, liveStates...)
	if err != nil {
		return err
	}
	var errs []error
	for _, rec := range recs {
		if err := s.Delete(ctx, rec, reason, SystemActor, checkpoint); err != nil {
			errs = append(errs, fmt.Errorf("delete sandbox %s: %w", rec.ID, err))
		}
	}
	return errors.Join(errs...)
}

// HasLiveSandbox reports whether the workspace slug has a sandbox in state
// provisioning or ready, one that holds compute or is about to.
func (s *Service) HasLiveSandbox(ctx context.Context, slug string) (bool, error) {
	if s.deps.Store == nil {
		return false, nil
	}
	var live bool
	err := s.deps.Store.Main().QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM sandboxes WHERE workspace = ? AND state IN (?, ?))`,
		slug, StateProvisioning, StateReady).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("read the sandboxes of workspace %s: %w", slug, err)
	}
	return live, nil
}

// WorkspaceArchived stops by the system, with reason StopArchived, every
// ready sandbox of the archived workspace slug and of its forks, each with a
// checkpoint first when its profile asks for one, as every stop does; the
// archive observer. A sandbox whose stop fails stays ready and the others
// are still stopped; the error then names every sandbox not stopped.
// Without a runtime client no ready sandbox can be stopped.
func (s *Service) WorkspaceArchived(ctx context.Context, slug string, forks []string) error {
	if s.deps.Store == nil {
		return nil
	}
	recs, err := listIn(ctx, s.deps.Store.Main(), append([]string{slug}, forks...), StateReady)
	if err != nil {
		return err
	}
	var (
		failed []string
		errs   []error
	)
	for _, rec := range recs {
		var err error
		if s.client == nil {
			err = errors.New("sandboxes are off: no runtime can stop it")
		} else {
			_, err = s.Stop(ctx, rec, StopArchived, SystemActor)
		}
		if err != nil {
			failed = append(failed, rec.ID)
			errs = append(errs, fmt.Errorf("%s: %w", rec.ID, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("the sandboxes %s of archived workspace %s were not stopped: %w",
			strings.Join(failed, ", "), slug, errors.Join(errs...))
	}
	return nil
}

// RemoveWorkspaceRecords removes, in the transaction tx that removes the
// workspace slug, every sandbox record of it, running the purge hooks for
// each in tx first, and reports the states after; for a fork it also deletes
// the fork's checkpoint refs, since a later fork with the same slug must not
// find them. A repository workspace's refs go with its repository, which
// its deletion removes. The tokens of the sandboxes are the workspace's
// token revoker's to revoke. The remover of owned data of a workspace's
// deletion and of a fork's removal; an error rolls tx back, but the refs
// it deleted stay deleted.
func (s *Service) RemoveWorkspaceRecords(ctx context.Context, tx *sql.Tx, slug string) error {
	recs, err := listIn(ctx, tx, []string{slug})
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if err := s.removeRecord(ctx, tx, rec.ID); err != nil {
			return err
		}
	}
	if len(recs) > 0 && s.deps.Recorder != nil {
		if err := s.ReportStates(ctx, tx); err != nil {
			return err
		}
	}
	w, found, err := workspace.BySlug(ctx, tx, slug)
	if err != nil {
		return fmt.Errorf("read workspace %s: %w", slug, err)
	}
	if !found || w.Kind != access.KindFork || s.deps.Repos == nil {
		return nil
	}
	return s.deleteCheckpointRefs(ctx, slug)
}

// removeRecord runs the purge hooks for the sandbox id in tx and removes
// its record there.
func (s *Service) removeRecord(ctx context.Context, tx *sql.Tx, id string) error {
	if err := s.RunPurgeHooks(ctx, tx, id); err != nil {
		return fmt.Errorf("purge sandbox %s: %w", id, err)
	}
	return Remove(ctx, tx, id)
}

// deleteCheckpointRefs deletes every ref under CheckpointRefPrefix of the
// repository of the workspace slug; a repository that does not exist has
// none.
func (s *Service) deleteCheckpointRefs(ctx context.Context, slug string) error {
	repo, err := s.deps.Repos(ctx, slug)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("open the repository of workspace %s: %w", slug, err)
	}
	refs, err := repo.ListRefs(ctx, CheckpointRefPrefix)
	if err != nil {
		return fmt.Errorf("list the checkpoint refs of workspace %s: %w", slug, err)
	}
	for _, ref := range refs {
		if err := repo.DeleteRef(ctx, ref.Name); err != nil {
			return fmt.Errorf("delete the checkpoint ref %s of workspace %s: %w", ref.Name, slug, err)
		}
	}
	return nil
}

// listIn reads through q the sandboxes of the workspaces slugs in one of
// states, or in any state without states, by created_at and then by id.
func listIn(ctx context.Context, q access.Querier, slugs []string, states ...string) ([]Record, error) {
	if len(slugs) == 0 {
		return nil, nil
	}
	query := `SELECT ` + recordColumns + ` FROM sandboxes WHERE workspace IN (` + sqlMarks(len(slugs)) + `)`
	args := make([]any, 0, len(slugs)+len(states))
	for _, slug := range slugs {
		args = append(args, slug)
	}
	if len(states) > 0 {
		query += ` AND state IN (` + sqlMarks(len(states)) + `)`
		for _, state := range states {
			args = append(args, state)
		}
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list the sandboxes of %s: %w", strings.Join(slugs, ", "), err)
	}
	defer func() { _ = rows.Close() }()
	var out []Record
	for rows.Next() {
		rec, err := scanRecord(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list the sandboxes of %s: %w", strings.Join(slugs, ", "), err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the sandboxes of %s: %w", strings.Join(slugs, ", "), err)
	}
	return out, nil
}

// sqlMarks is n query placeholders separated by commas.
func sqlMarks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
