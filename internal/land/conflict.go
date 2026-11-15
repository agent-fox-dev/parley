package land

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// Attribute gives a conflict report of the workspace its landed changes:
// LandedChanges, and in each conflicted file LandedBy, the numbers of the
// landed changes that touched the file. Each starts empty, so that a file
// no landed change touched reads landed_by [] whichever operation found
// the conflict; the files and their sides stay as the replay of
// internal/gitrepo classified them, since the report has one type and one
// classification for every producer.
func (c *Component) Attribute(ctx context.Context, workspace string, report *gitrepo.ConflictReport) error {
	if report == nil {
		return nil
	}
	report.LandedChanges = []any{}
	for i := range report.Files {
		report.Files[i].LandedBy = []any{}
	}
	return nil
}
