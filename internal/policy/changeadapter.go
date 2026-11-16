package policy

import (
	"context"
	"slices"

	"github.com/agent-fox-dev/parley/internal/change"
)

// Files reads, through q, the files the current version of the change
// number of the workspace touches: those that differ between the change's
// base and its head, both paths of a rename or a copy, each once, in byte
// order, with no cap. They are read in the repository of the change's
// target workspace, which holds the head of every version, a fork's
// included. The base is the change's own, which for a change stacked on
// another is the head of the change below, so that a stacked change is
// judged by its own commits only. Component is the evaluation's
// ChangeFiles.
func (c *Component) Files(ctx context.Context, q Querier, workspace string, number int64) ([]string, error) {
	ch, err := c.deps.Changes.ReadChange(ctx, q, workspace, number)
	if err != nil {
		return nil, err
	}
	base := ""
	if ch.BaseSHA != nil {
		base = *ch.BaseSHA
	}
	return c.filesBetween(ctx, ch.WorkspaceSlug, base, ch.HeadSHA)
}

// filesBetween are the files that differ between base ("" for the empty
// tree) and head in the repository of the workspace, as Files reads them.
func (c *Component) filesBetween(ctx context.Context, workspace, base, head string) ([]string, error) {
	repo, err := c.deps.Repos(ctx, workspace)
	if err != nil {
		return nil, err
	}
	changes, err := repo.DiffChanges(ctx, base, head)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(changes))
	for _, f := range changes {
		files = append(files, f.Path)
		if f.OldPath != "" {
			files = append(files, f.OldPath)
		}
	}
	slices.Sort(files)
	return slices.Compact(files), nil
}

// listedFiles are the files of a version the caller read already.
type listedFiles []string

func (l listedFiles) Files(context.Context, Querier, string, int64) ([]string, error) { return l, nil }

// factsOf is what the evaluator knows of the change ch at its version
// version, with cause the reason it cannot land for whatever the policy
// requires (nil for none).
func factsOf(ch change.Change, version int64, cause *string) ChangeFacts {
	return ChangeFacts{
		Workspace: ch.WorkspaceSlug, Number: ch.Number, Version: version, State: ch.State,
		TargetBranch: ch.TargetBranch, Mergeable: ch.Mergeable, Cause: cause,
	}
}
