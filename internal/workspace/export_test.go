package workspace

import "context"

// SafetyCopy runs the safety copy of w, for the tests of the external test
// package; the operations that make one are its callers in production.
func (s *Service) SafetyCopy(ctx context.Context, w Workspace) string { return s.safetyCopy(ctx, w) }
