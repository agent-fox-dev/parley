package workspace

import "context"

// SafetyCopy runs the safety copy of w, for the tests of the external test
// package; the operations that make one are its callers in production.
func (s *Service) SafetyCopy(ctx context.Context, w Workspace) string { return s.safetyCopy(ctx, w) }

// WithMarker runs fn holding the maintenance marker m of the workspace
// slug, for the tests of the external test package; the operations that
// hold one are its callers in production.
func (s *Service) WithMarker(ctx context.Context, slug string, m Marker, fn func() error) error {
	return s.withMarker(ctx, slug, m, fn)
}

// Clear removes every collaborator from t, the ones the hub's composition
// registered included, so that a test runs with the empty set or with its
// own stubs alone.
func (t *Teardown) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.revoker, t.removers, t.closer, t.sandboxes, t.observers, t.forks = nil, nil, nil, nil, nil, nil
}
