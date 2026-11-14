// Package fork creates task forks: workspaces taken from another workspace
// for one task, which share their parent's history without copying it.
package fork

import (
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// ResolveTTL is the one rule for a requested ttl, at a fork's creation and
// when its lifetime is extended: ttl must be a whole number above zero
// followed by one unit m, h or d, at least five minutes and at most
// l.MaxTTL. It returns the fork's expires_at, now plus ttl in whole seconds
// UTC, or a 400 invalid_request naming ttl.
func ResolveTTL(ttl string, now time.Time, l config.Forks) (time.Time, error) {
	d, err := workspace.ParseDuration(ttl)
	switch {
	case err != nil:
		return time.Time{}, server.InvalidRequest("ttl: " + err.Error())
	case d < config.MinForkTTL:
		return time.Time{}, server.InvalidRequest("ttl: must be at least 5m")
	case d > l.MaxTTL:
		return time.Time{}, server.InvalidRequest(fmt.Sprintf("ttl: must be at most %s, the hub's [forks].max_ttl", wholeUnits(l.MaxTTL)))
	}
	return expiry(now, d), nil
}

// DefaultExpiry returns the expires_at of a fork created without a ttl: now
// plus l.DefaultTTL, in whole seconds UTC.
func DefaultExpiry(now time.Time, l config.Forks) time.Time {
	return expiry(now, l.DefaultTTL)
}

func expiry(now time.Time, d time.Duration) time.Time {
	return now.Add(d).UTC().Truncate(time.Second)
}

// wholeUnits writes d in the largest of d, h, m and s that divides it, the
// form a configuration file gives it in.
func wholeUnits(d time.Duration) string {
	for _, u := range []struct {
		unit time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if d%u.unit == 0 {
			return fmt.Sprintf("%d%s", d/u.unit, u.name)
		}
	}
	return fmt.Sprintf("%ds", d/time.Second)
}
