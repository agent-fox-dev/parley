package access

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// CoveredSet returns, sorted, the workspaces the binding of the bound PAT c
// reaches in full: for a token bound to a repository, the repository and
// every fork of it that the token created; for a token bound to a fork, the
// fork alone. A bound slug with no record covers that slug alone, so that a
// request naming it passes the binding and gets the access rule's answer. A
// caller without a binding covers nothing: nil. A failed read is nil and the
// error.
func (s *Service) CoveredSet(ctx context.Context, c Caller) ([]string, error) {
	if c.Workspace == "" {
		return nil, nil
	}
	bound, found, err := s.deps.Records.Record(ctx, c.Workspace)
	if err != nil {
		return nil, fmt.Errorf("read the bound workspace: %w", err)
	}
	covered := []string{c.Workspace}
	if !found || bound.Kind == KindFork {
		return covered, nil
	}
	forks, err := s.deps.Records.ForksOf(ctx, []string{c.Workspace})
	if err != nil {
		return nil, fmt.Errorf("read the forks of the bound workspace: %w", err)
	}
	for _, f := range forks {
		if createdBy(f, c.TokenID) {
			covered = append(covered, f.Slug)
		}
	}
	slices.Sort(covered)
	return covered, nil
}

// RelationOf answers how the workspace slug stands to the binding of the
// bound PAT c: RelationBound for the bound workspace, whether it has a
// record or not; RelationCreatedFork for a fork of it that the token
// created; RelationParentOfBoundFork for the parent of the fork the token is
// bound to; and none otherwise, and for a caller without a binding. The
// records are read on every call, so that a fork the token has just created
// is covered at once and a binding never outlives a record's change. A
// failed read is none and the error.
func (s *Service) RelationOf(ctx context.Context, c Caller, slug string) (Relation, error) {
	switch {
	case c.Workspace == "":
		return "", nil
	case slug == c.Workspace:
		return RelationBound, nil
	}
	named, found, err := s.deps.Records.Record(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("read the named workspace: %w", err)
	}
	if found && named.Kind == KindFork && named.ParentSlug == c.Workspace && createdBy(named, c.TokenID) {
		return RelationCreatedFork, nil
	}
	bound, found, err := s.deps.Records.Record(ctx, c.Workspace)
	if err != nil {
		return "", fmt.Errorf("read the bound workspace: %w", err)
	}
	if found && bound.Kind == KindFork && bound.ParentSlug == slug {
		return RelationParentOfBoundFork, nil
	}
	return "", nil
}

// createdBy reports whether the token tokenID created the fork rec. A fork
// with no recorded creator was created by no token.
func createdBy(rec Record, tokenID string) bool {
	return rec.Kind == KindFork && rec.CreatingTokenID != "" && rec.CreatingTokenID == tokenID
}

// boundReadable is the readable set of the bound PAT c: the readable set of
// its user cut down to the workspaces its binding covers, each at the
// user's level, so that a bound token never sees in a cross-workspace read a
// workspace its binding does not reach.
func (s *Service) boundReadable(ctx context.Context, c Caller) (ReadableSet, error) {
	covered, err := s.CoveredSet(ctx, c)
	if err != nil {
		return ReadableSet{}, err
	}
	set, err := s.Readable(ctx, Caller{UserID: c.UserID})
	if err != nil {
		return ReadableSet{}, err
	}
	maps.DeleteFunc(set.Entries, func(slug string, _ Level) bool { return !slices.Contains(covered, slug) })
	return set, nil
}
