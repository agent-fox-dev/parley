package land

import (
	"cmp"
	"context"
	"slices"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// conflict completes the report of a commit of the covered change cov of
// the attempt a that did not apply on the target head onto: it names the
// target head the attempt replayed onto, the head and base of the version
// it lands, and, in a stack, the change, and attributes the conflict to
// the landed changes. An attribution that cannot be read leaves the report
// without it rather than failing the attempt, whose conflict stands.
func (c *Component) conflict(ctx context.Context, a Attempt, cov CoveredChange, onto string, report *gitrepo.ConflictReport) *gitrepo.ConflictReport {
	report.TargetSHA, report.HeadSHA, report.BaseSHA, report.Change = onto, cov.Head, cov.Base, nil
	if len(a.Covered) > 1 {
		report.Change = cov.Number
	}
	if err := c.Attribute(ctx, a.Workspace, report); err != nil {
		c.deps.Logger.Warn("land attempt: conflict not attributed", "workspace", a.Workspace, "land_id", a.ID, "error", err)
	}
	return report
}

// Attribute gives a conflict report of the workspace its landed changes:
// LandedChanges, each landed change with a commit between the report's
// base and target that touched a conflicted file, once and by number, and
// in each conflicted file LandedBy, the numbers, ascending, of those that
// touched it. A file is touched by a commit that changes a path one of its
// sides has, and a commit belongs to the landed change the component's
// lookup (landed) names for it; a commit of no change, as a direct push,
// names none. Each list starts empty, so that a file no landed change touched reads landed_by []
// whichever operation found the conflict, and both stay empty for a
// report without a base, or whose base is not an ancestor of its target,
// and when the lookup or the history cannot be read, which is the error.
// The files and their sides stay as the replay of internal/gitrepo
// classified them, since the report has one type and one classification
// for every producer.
func (c *Component) Attribute(ctx context.Context, workspace string, report *gitrepo.ConflictReport) error {
	if report == nil {
		return nil
	}
	report.LandedChanges = []any{}
	for i := range report.Files {
		report.Files[i].LandedBy = []any{}
	}
	lookup := c.landed()
	if lookup == nil || report.BaseSHA == "" || len(report.Files) == 0 {
		return nil
	}
	landed, touched, err := c.landedTouching(ctx, workspace, lookup, report)
	if err != nil || len(landed) == 0 {
		return err
	}
	// By number, so that two reports of one conflict read the same.
	slices.SortFunc(landed, func(a, b CollidedChange) int { return cmp.Compare(a.Number, b.Number) })
	for _, ch := range landed {
		report.LandedChanges = append(report.LandedChanges, ch)
		for _, i := range touched[ch.Number] {
			report.Files[i].LandedBy = append(report.Files[i].LandedBy, ch.Number)
		}
	}
	return nil
}

// landedTouching reads, for the commits between the report's base and
// target, oldest first, the landed changes that touched a conflicted file:
// the changes, each once, and for each change the indexes of the files it
// touched; none when the base is not an ancestor of the target. A commit
// is looked up only once it touched a conflicted file.
func (c *Component) landedTouching(ctx context.Context, workspace string, lookup LandedLookup, report *gitrepo.ConflictReport) ([]CollidedChange, map[int64][]int, error) {
	repo, err := c.deps.Repos(ctx, workspace)
	if err != nil {
		return nil, nil, err
	}
	if ok, err := repo.IsAncestor(ctx, report.BaseSHA, report.TargetSHA); err != nil || !ok {
		return nil, nil, err
	}
	commits, err := repo.CommitsBetween(ctx, report.BaseSHA, report.TargetSHA, 0)
	if err != nil {
		return nil, nil, err
	}
	files := map[string][]int{}
	for i, f := range report.Files {
		for _, side := range []*gitrepo.ConflictSide{f.Base, f.Ours, f.Theirs} {
			if side != nil && !slices.Contains(files[side.Path], i) {
				files[side.Path] = append(files[side.Path], i)
			}
		}
		if !slices.Contains(files[f.Path], i) {
			files[f.Path] = append(files[f.Path], i)
		}
	}
	var landed []CollidedChange
	touched := map[int64][]int{}
	for _, id := range commits {
		paths, err := repo.ChangedPaths(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		var hit []int
		for _, p := range paths {
			for _, i := range files[p] {
				if !slices.Contains(hit, i) {
					hit = append(hit, i)
				}
			}
		}
		if len(hit) == 0 {
			continue
		}
		ch, found, err := lookup.LandedChange(ctx, workspace, id)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			continue
		}
		if _, seen := touched[ch.Number]; !seen {
			landed = append(landed, ch)
			touched[ch.Number] = []int{}
		}
		for _, i := range hit {
			if !slices.Contains(touched[ch.Number], i) {
				touched[ch.Number] = append(touched[ch.Number], i)
			}
		}
	}
	return landed, touched, nil
}
