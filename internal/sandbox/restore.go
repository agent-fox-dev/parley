package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// Restore restores the latest checkpoint of the sandbox source into the
// working directory of the sandbox rec, whose checkout is done: each branch
// of the checkpoint's refs is set to its checkpointed commit, a dirty one to
// the commit under the extra one. The checked-out branch's working tree and
// index move to its new head; when its ref is dirty, the extra commit's
// tree is then written into the working tree alone, so that its changes
// are uncommitted again, as they were. A dirty ref of another branch leaves
// its changes in the checkpoint ref.
//
// The checkpoint's commits travel as one git bundle of its refs, made in
// the source's workspace repository, written into the sandbox through the
// runtime's file upload and fetched there; it is removed on both sides. A
// checkpoint without refs restores nothing and calls no runtime. A ref the
// repository no longer holds, or that holds another commit than the
// checkpoint says, fails the restore with an error naming it, as does a
// git command that fails in the sandbox; the source is left as it is.
func (s *Service) Restore(ctx context.Context, rec, source Record) error {
	refs := lastRefs(source.Checkpoint)
	if len(refs) == 0 {
		return nil
	}
	if s.client == nil {
		return errors.New("the sandbox runtime is not connected")
	}
	repo, err := s.deps.Repos(ctx, source.Workspace)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		name := CheckpointRefPrefix + source.ID + "/" + ref.Branch
		id, err := repo.ResolveRef(ctx, name)
		switch {
		case errors.Is(err, gitrepo.ErrNotFound):
			return fmt.Errorf("the checkpoint ref %s is no longer in the repository of %s", name, source.Workspace)
		case err != nil:
			return fmt.Errorf("the checkpoint ref %s: %w", name, err)
		case id != ref.SHA:
			return fmt.Errorf("the checkpoint ref %s holds %s, not the checkpointed %s", name, id, ref.SHA)
		}
		names = append(names, name)
	}
	run := &checkpointRun{s: s, client: s.client, rec: rec, suffix: randomSuffix()}
	err = run.restore(ctx, repo, names, refs)
	run.cleanup(ctx)
	return err
}

// restore brings the commits of the checkpoint refs names, of the hub's
// repository hub, into the working directory and sets its branches by refs.
func (r *checkpointRun) restore(ctx context.Context, hub *gitrepo.Repo, names []string, refs []CheckpointRef) error {
	repo, err := r.repository(ctx)
	if err != nil {
		return err
	}
	if !repo {
		return errors.New("the working directory is not a git work tree")
	}
	bundle, err := r.upload(ctx, hub, names)
	if err != nil {
		return err
	}
	// Fetched for their objects alone: the branches are set below, and no
	// ref of the fetch's own is left to remove.
	if _, err := r.git(ctx, gitCall{args: append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false",
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", bundle}, names...)}); err != nil {
		return err
	}
	_, current, err := r.heads(ctx)
	if err != nil {
		return err
	}
	var (
		updates strings.Builder
		// head is the ref of the checked-out branch, at, its new head.
		head *CheckpointRef
		at   string
	)
	for _, ref := range refs {
		commit := ref.SHA
		if ref.Dirty {
			parent, err := r.git(ctx, gitCall{args: []string{"rev-parse", "--verify", "--end-of-options", ref.SHA + "^1^{commit}"}})
			if err != nil {
				return fmt.Errorf("the commit under the checkpointed %s: %w", ref.SHA, err)
			}
			commit = strings.TrimSpace(parent)
		}
		switch {
		case ref.Branch == current:
			head, at = &ref, commit
		case ref.Branch != "HEAD":
			// Uncommitted changes with no branch checked out were saved as
			// HEAD, which is no branch to set in a clone that has one
			// checked out.
			fmt.Fprintf(&updates, "update refs/heads/%s %s\n", ref.Branch, commit)
		}
	}
	if updates.Len() > 0 {
		if _, err := r.git(ctx, gitCall{args: []string{"update-ref", "--stdin"}, stdin: []byte(updates.String())}); err != nil {
			return err
		}
	}
	if head == nil {
		return nil
	}
	if _, err := r.git(ctx, gitCall{args: []string{"reset", "--quiet", "--hard", at}}); err != nil {
		return err
	}
	if !head.Dirty {
		return nil
	}
	// The extra commit's tree into the working tree alone, the index left
	// at the head: git restore without --staged removes a file the tree
	// lacks and writes one the index lacks as untracked.
	_, err = r.git(ctx, gitCall{args: []string{"restore", "--source=" + head.SHA, "--worktree", "--", "."}})
	return err
}

// upload makes one bundle of the refs names in the hub's repository hub,
// writes it into the sandbox's git directory as the attempt's file of kind
// bundle, which cleanup removes, and returns that file's name relative to
// the working directory.
func (r *checkpointRun) upload(ctx context.Context, hub *gitrepo.Repo, names []string) (string, error) {
	dir, err := os.MkdirTemp("", checkpointName+"*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	local := filepath.Join(dir, "checkpoint.bundle")
	if err := hub.CreateBundle(ctx, local, names); err != nil {
		return "", err
	}
	f, err := os.Open(local)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	bundle := r.file("bundle")
	if err := r.client.Upload(ctx, r.rec.ID, path.Join(WorkDir, bundle), 0o600, f); err != nil {
		return "", fmt.Errorf("write the checkpoint bundle into the sandbox: %w", err)
	}
	return bundle, nil
}
