package carrypatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// reasonRebuildConflict is the reason of the parley.patch.update of a
// patch a rebuild found in conflict.
const reasonRebuildConflict = "rebuild_conflict"

// ConflictFileDescriber gives the file entries of a conflict report: for
// each of paths, left in conflict when the commit commit was applied onto
// the commit target in the repository repo, the file's path, kind and its
// sides, ours being the file in target and theirs the file in commit.
type ConflictFileDescriber interface {
	ConflictFiles(ctx context.Context, repo *gitrepo.Repo, target, commit string, paths []string) ([]gitrepo.ConflictFile, error)
}

// GitConflictFiles is the conflict-file describer that reads the entries
// from the repository with gitrepo's ConflictFiles.
type GitConflictFiles struct{}

// ConflictFiles is repo.ConflictFiles.
func (GitConflictFiles) ConflictFiles(ctx context.Context, repo *gitrepo.Repo, target, commit string, paths []string) ([]gitrepo.ConflictFile, error) {
	return repo.ConflictFiles(ctx, target, commit, paths)
}

// ConflictReport is the conflict report of a patch that did not apply in
// a rebuild: the hub's one conflict report, with the patch's id.
type ConflictReport struct {
	PatchID string
	gitrepo.ConflictReport
}

// MarshalJSON writes patch_id, then every member of the hub's conflict
// report as that report writes them; the report's own MarshalJSON, which
// the embedding would promote, would leave out patch_id.
func (r ConflictReport) MarshalJSON() ([]byte, error) {
	id, err := json.Marshal(r.PatchID)
	if err != nil {
		return nil, err
	}
	rest, err := json.Marshal(r.ConflictReport)
	if err != nil {
		return nil, err
	}
	// rest is an object with members: {"target_sha":…}.
	out := append([]byte(`{"patch_id":`), id...)
	out = append(out, ',')
	return append(out, rest[1:]...), nil
}

// PatchConflict is what a conflict report of a patch is built from.
type PatchConflict struct {
	// PatchID is the patch's id, Source the tip of its branch the run read.
	PatchID, Source string
	// Target is the head under construction before the patch.
	Target string
	// Commit is the commit that did not apply: with strategy merge, Source.
	Commit string
	// Paths are the paths left in conflict.
	Paths []string
}

// BuildConflictReport builds the conflict report of the patch conflict in
// describes, in the workspace's repository repo: the head under
// construction before the patch is target_sha, the patch's source
// head_sha, their merge base base_sha (null when they share none), the
// commit that did not apply commit_sha, and the files are the describer's
// entries for the paths, in path order, each with landed_by []. A rebuild
// knows of no landed change, so landed_changes is [] and change null.
func (c *Component) BuildConflictReport(ctx context.Context, repo *gitrepo.Repo, in PatchConflict) (ConflictReport, error) {
	paths := slices.Clone(in.Paths)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	base, err := repo.MergeBase(ctx, in.Source, in.Target)
	if err != nil {
		return ConflictReport{}, err
	}
	files, err := c.describer().ConflictFiles(ctx, repo, in.Target, in.Commit, paths)
	if err != nil {
		return ConflictReport{}, fmt.Errorf("describe the conflicted files of %s: %w", in.Commit, err)
	}
	files = slices.Clone(files)
	for i := range files {
		files[i].LandedBy = []any{}
	}
	return ConflictReport{PatchID: in.PatchID, ConflictReport: gitrepo.ConflictReport{
		TargetSHA: in.Target, HeadSHA: in.Source, BaseSHA: base, CommitSHA: in.Commit,
		Files: files, LandedChanges: []any{},
	}}, nil
}

// describer is the component's conflict-file describer.
func (c *Component) describer() ConflictFileDescriber {
	if c.d.ConflictFiles == nil {
		return GitConflictFiles{}
	}
	return c.d.ConflictFiles
}

// conflictAt is where a patch stopped applying in a run: the commit that
// did not apply, with strategy merge the patch's source, and the paths it
// left in conflict, in path order.
type conflictAt struct {
	commit string
	paths  []string
}

// conflict deals with the patch p, which did not apply at at onto the head
// under construction before, which was its target: it undoes the patch in
// the scratch working tree, so that the run goes on from before, builds
// the conflict report, marks the patch conflict in the store and records
// res as a conflict with its files and its report. A non-nil outcome ends
// the attempt.
func (r *rebuildRun) conflict(ctx context.Context, scratch *gitrepo.Scratch, p runPatch, before string, at conflictAt, res *patchResult) *attemptOutcome {
	err := r.git(ctx, func(ctx context.Context) error { return scratch.Reset(ctx, before) })
	if err != nil {
		return r.failed(ctx, fmt.Errorf("rebuild of %s: undo %s: %w", r.ws.Slug, p.BranchName, err))
	}
	r.head = before
	var report ConflictReport
	err = r.git(ctx, func(ctx context.Context) (err error) {
		report, err = r.c.BuildConflictReport(ctx, r.repo, PatchConflict{
			PatchID: p.ID, Source: *p.source, Target: before, Commit: at.commit, Paths: at.paths,
		})
		return err
	})
	if err != nil {
		return r.failed(ctx, fmt.Errorf("rebuild of %s: report the conflict of %s: %w", r.ws.Slug, p.BranchName, err))
	}
	doc, err := json.Marshal(report)
	if err != nil {
		return r.failed(ctx, err)
	}
	if err := r.markConflict(ctx, p, at.paths); err != nil {
		return r.failed(ctx, fmt.Errorf("rebuild of %s: mark %s conflict: %w", r.ws.Slug, p.BranchName, err))
	}
	res.Status, res.ConflictFiles, res.Conflict = resultConflict, at.paths, doc
	return nil
}

// markConflict sets the stored patch p to status conflict with files as
// its conflict files, and emits parley.patch.update, in the same
// transaction, when the status changed: a patch that was conflict already
// gets its new files alone. A patch a caller deleted, disabled or found
// merged upstream while the run went on keeps what the caller gave it.
func (r *rebuildRun) markConflict(ctx context.Context, p runPatch, files []string) error {
	if r.c.d.Events == nil {
		return errNotBound
	}
	slug := r.ws.Slug
	return r.c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		cur, found, err := ReadPatch(ctx, tx, slug, p.ID)
		if err != nil || !found {
			return err
		}
		switch {
		case cur.Status == StatusConflict && slices.Equal(cur.ConflictFiles, files),
			cur.Status != StatusActive && cur.Status != StatusConflict:
			return nil
		}
		next := cur
		next.Status, next.ConflictFiles = StatusConflict, files
		next.UpdatedAt = r.c.now().UTC().Truncate(time.Second)
		if err := writeFields(ctx, tx, next); err != nil {
			return err
		}
		if cur.Status == StatusConflict {
			return nil
		}
		_, err = r.c.d.Events.Emit(ctx, tx, event.Input{
			Type: EventPatchUpdate, Workspace: &slug, ActorType: event.ActorSystem,
			ResourceType: "patch", ResourceID: p.ID,
			Metadata: map[string]any{"branch_name": cur.BranchName, "changed": []string{"status"},
				"from": cur.Status, "to": StatusConflict, "reason": reasonRebuildConflict},
		})
		return err
	})
}

// stopAtConflict is the outcome of a fail_fast run at the patch whose
// conflict result res is: a permanent failure that carries the patch's
// conflict report, so that the job is not tried again while the patch
// does not apply.
func (r *rebuildRun) stopAtConflict(res patchResult) *attemptOutcome {
	files := "1 file"
	if n := len(res.ConflictFiles); n != 1 {
		files = fmt.Sprintf("%d files", n)
	}
	return &attemptOutcome{
		kind: attemptPermanent, reason: reasonConflict, conflict: res.Conflict,
		text: fmt.Sprintf("rebuild of %s: the patch %s does not apply: %s in conflict", r.ws.Slug, res.BranchName, files),
	}
}
