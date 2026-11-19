package audit

import (
	"context"
	"time"
)

// SessionStore is the session store as the session steps reach it;
// *session.Component satisfies it. Each operation runs in its own
// transaction and returns the number of sessions it changed.
type SessionStore interface {
	// TimeOutActiveBefore ends every active session started before cutoff
	// as timed out at now.
	TimeOutActiveBefore(ctx context.Context, cutoff, now time.Time) (int64, error)
	// DeleteTerminalBefore deletes every ended session started before
	// cutoff, with its usage records.
	DeleteTerminalBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// SessionSteps returns the steps stale_sessions, which times out the
// sessions active for longer than settings.SessionMaxActiveAge, and
// sessions, which deletes the ended sessions started longer than
// settings.SessionMaxAge ago. Each step reads clock once, so that its
// cutoff and the end it writes are the same instant; the store's error is
// the step's.
func SessionSteps(s SessionStore, settings Settings, clock func() time.Time) map[string]Step {
	return map[string]Step{
		"stale_sessions": func(ctx context.Context) error {
			now := clock()
			_, err := s.TimeOutActiveBefore(ctx, now.Add(-settings.SessionMaxActiveAge), now)
			return err
		},
		"sessions": func(ctx context.Context) error {
			_, err := s.DeleteTerminalBefore(ctx, clock().Add(-settings.SessionMaxAge))
			return err
		},
	}
}
