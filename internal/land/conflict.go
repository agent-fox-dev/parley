package land

import (
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

// collided is a landed change a conflict report names, as its
// landed_changes list shows it.
type collided struct {
	Number    int64   `json:"number"`
	Title     string  `json:"title"`
	LandedSHA string  `json:"landed_sha"`
	SessionID *string `json:"session_id"`
}

// Attribute gives a conflict report of the workspace its landed changes:
// LandedChanges, each change landed between the report's base and target
// that touched a conflicted file, in the order they landed, and in each
// conflicted file LandedBy, the numbers of those that touched it. A landed
// change is one the landed-commit mapping of the change component names
// for a commit, and a file is touched by a commit that changes a path one
// of its sides has. Each starts empty, so that a file no landed change
// touched reads landed_by [] whichever operation found the conflict, and
// both stay empty for a report without a base, or whose base is not an
// ancestor of its target, and when the mapping or the history cannot be
// read, which is the error. The files and their sides stay as the replay
// of internal/gitrepo classified them, since the report has one type and
// one classification for every producer.
func (c *Component) Attribute(ctx context.Context, workspace string, report *gitrepo.ConflictReport) error {
	if report == nil {
		return nil
	}
	report.LandedChanges = []any{}
	for i := range report.Files {
		report.Files[i].LandedBy = []any{}
	}
	if report.BaseSHA == "" || len(report.Files) == 0 {
		return nil
	}
	touched, order, err := c.landedTouching(ctx, workspace, report)
	if err != nil || len(order) == 0 {
		return err
	}
	changes := make([]any, 0, len(order))
	for _, n := range order {
		ch, err := c.deps.Changes.ReadChange(ctx, nil, workspace, n)
		if err != nil {
			return err
		}
		landed := collided{Number: n, Title: ch.Title, SessionID: ch.SessionID}
		if ch.Landed != nil {
			landed.LandedSHA = ch.Landed.SHA
		}
		changes = append(changes, landed)
	}
	report.LandedChanges = changes
	for i := range report.Files {
		for _, n := range order {
			if slices.Contains(touched[n], i) {
				report.Files[i].LandedBy = append(report.Files[i].LandedBy, n)
			}
		}
	}
	return nil
}

// landedTouching reads, for the commits between the report's base and
// target, oldest first, the landed changes that touched a conflicted file:
// for each change the indexes of the files it touched, and the changes in
// the order they first did; none when the base is not an ancestor of the
// target.
func (c *Component) landedTouching(ctx context.Context, workspace string, report *gitrepo.ConflictReport) (map[int64][]int, []int64, error) {
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
	touched, order := map[int64][]int{}, []int64{}
	for _, id := range commits {
		cc, found, err := c.deps.Changes.ChangeOfCommit(ctx, nil, workspace, id)
		if err != nil {
			return nil, nil, err
		}
		if !found || !cc.Landed {
			continue
		}
		paths, err := repo.ChangedPaths(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range paths {
			for _, i := range files[p] {
				if !slices.Contains(touched[cc.Number], i) {
					if len(touched[cc.Number]) == 0 {
						order = append(order, cc.Number)
					}
					touched[cc.Number] = append(touched[cc.Number], i)
				}
			}
		}
	}
	return touched, order, nil
}
