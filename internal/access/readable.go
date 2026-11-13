package access

import (
	"context"
	"fmt"
	"slices"
)

// ReadableSet is the set of workspaces a caller can read, which the
// cross-workspace reads (the workspace list, events, audit) filter by, so
// that they never show a workspace that would answer 404. A workspace is in
// it exactly when the caller's level on it is not LevelNone.
type ReadableSet struct {
	// All marks the admin token's set, which holds every workspace at
	// LevelAdmin.
	All bool
	// Entries are the caller's level on each workspace of the set, by slug;
	// empty when All is set.
	Entries map[string]Level
}

// Contains reports whether the set holds the workspace slug. The admin
// token's set holds every slug.
func (s ReadableSet) Contains(slug string) bool {
	return s.Level(slug) != LevelNone
}

// Level is the caller's level on the workspace slug: LevelAdmin for every
// slug in the admin token's set, otherwise its entry, and LevelNone for a
// slug the set does not hold.
func (s ReadableSet) Level(slug string) Level {
	if s.All {
		return LevelAdmin
	}
	return s.Entries[slug]
}

// Readable builds the readable set of the caller c. The admin token's is
// the set of all workspaces. A user's holds, each once and in every status,
// the workspaces they own, the forks of the repositories they own, and the
// workspaces, forks included, of the active orgs they are a member of, each
// at the level Level resolves for it; those at LevelNone, such as a fork
// whose owner has no level on its parent, are left out. A caller who is
// neither gets the empty set. A failed read returns the error and the empty
// set, never part of one.
func (s *Service) Readable(ctx context.Context, c Caller) (ReadableSet, error) {
	if c.Admin {
		return ReadableSet{All: true}, nil
	}
	if c.UserID == "" {
		return ReadableSet{}, nil
	}
	orgs, err := s.deps.Memberships.ActiveOrgIDs(ctx, c.UserID)
	if err != nil {
		return ReadableSet{}, fmt.Errorf("read the active orgs of the caller: %w", err)
	}
	recs, err := s.deps.Records.OwnedOrIn(ctx, c.UserID, orgs)
	if err != nil {
		return ReadableSet{}, err
	}
	var owned []string
	for _, rec := range recs {
		if rec.Kind == KindRepository && rec.OwnerID == c.UserID {
			owned = append(owned, rec.Slug)
		}
	}
	forks, err := s.deps.Records.ForksOf(ctx, owned)
	if err != nil {
		return ReadableSet{}, err
	}
	collected := make(map[string]Record, len(recs)+len(forks))
	for _, rec := range slices.Concat(recs, forks) {
		collected[rec.Slug] = rec
	}
	levels := userLevels{
		userID: c.UserID,
		orgs:   func(context.Context) ([]string, error) { return orgs, nil },
		// Every repository the user has a level on is collected, because
		// they own it or it is in one of their active orgs. A parent that
		// is not collected is one the user has no level on, which the
		// resolver answers as it answers a parent with no record: no level
		// on the fork. So no record is read twice.
		parent: func(_ context.Context, slug string) (Record, bool, error) {
			rec, ok := collected[slug]
			return rec, ok, nil
		},
	}
	entries := make(map[string]Level, len(collected))
	for slug, rec := range collected {
		level, err := levels.on(ctx, rec)
		if err != nil {
			return ReadableSet{}, err
		}
		if level != LevelNone {
			entries[slug] = level
		}
	}
	return ReadableSet{Entries: entries}, nil
}
