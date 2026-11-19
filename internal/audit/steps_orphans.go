package audit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
)

// WorkspaceRecords are the workspace records the orphans step asks whether
// a workspace exists; access.StoreRecords satisfies it.
type WorkspaceRecords interface {
	// Record reads the record of exactly this slug; found is false when
	// there is none.
	Record(ctx context.Context, slug string) (rec access.Record, found bool, err error)
}

// OrphanStore is a store that keeps records of workspaces after the
// workspace is gone, as the orphans step reaches it: *session.Component,
// *telemetry.Component and *event.Component satisfy it.
type OrphanStore interface {
	// WorkspaceSlugs returns every workspace slug its records carry.
	WorkspaceSlugs(ctx context.Context) ([]string, error)
	// DeleteWorkspaceRecordsBefore deletes, in one transaction, the
	// records of the workspaces slugs older than cutoff, each by its own
	// time, and returns the number it deleted.
	DeleteWorkspaceRecordsBefore(ctx context.Context, slugs []string, cutoff time.Time) (int64, error)
}

// OrphanSteps returns the step orphans, which deletes from every store the
// records of the workspaces that are gone, once they are older than
// settings.OrphanRetention. A slug is gone when records has no workspace of
// that slug and, for a task fork's slug, none of the parent's either: the
// records of a deleted fork belong to its parent while the parent exists.
// The step reads clock once. Every store is asked even when one fails; the
// step's error joins theirs.
func OrphanSteps(records WorkspaceRecords, stores []OrphanStore, settings Settings, clock func() time.Time) map[string]Step {
	return map[string]Step{
		"orphans": func(ctx context.Context) error {
			cutoff := clock().Add(-settings.OrphanRetention)
			var slugs []string
			for _, s := range stores {
				some, err := s.WorkspaceSlugs(ctx)
				if err != nil {
					return err
				}
				slugs = append(slugs, some...)
			}
			slices.Sort(slugs)
			gone, err := goneSlugs(ctx, records, slices.Compact(slugs))
			if err != nil || len(gone) == 0 {
				return err
			}
			var errs []error
			for _, s := range stores {
				if _, err := s.DeleteWorkspaceRecordsBefore(ctx, gone, cutoff); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	}
}

// goneSlugs returns the slugs of slugs that name no existing workspace,
// directly or, for a task fork's slug, through its parent.
func goneSlugs(ctx context.Context, records WorkspaceRecords, slugs []string) ([]string, error) {
	exists := map[string]bool{}
	known := func(slug string) (bool, error) {
		if found, ok := exists[slug]; ok {
			return found, nil
		}
		_, found, err := records.Record(ctx, slug)
		if err != nil {
			return false, fmt.Errorf("read the workspace record %s: %w", slug, err)
		}
		exists[slug] = found
		return found, nil
	}
	var gone []string
	for _, slug := range slugs {
		found, err := known(slug)
		if err != nil {
			return nil, err
		}
		if !found {
			if parent, _, isFork := strings.Cut(slug, forkSeparator); isFork {
				if found, err = known(parent); err != nil {
					return nil, err
				}
			}
		}
		if !found {
			gone = append(gone, slug)
		}
	}
	return gone, nil
}
