package config

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// MinForkTTL is the shortest lifetime a fork may have: the shortest
// [forks].default_ttl the file may set, and the shortest ttl a fork may ask
// for.
const MinForkTTL = 5 * time.Minute

// Forks holds the keys of [forks] the loader owns: the lifetime of a fork at
// its creation, how many forks one workspace may have, and whether a fork
// goes early once its work has landed.
type Forks struct {
	// DefaultTTL is the lifetime of a fork created without a ttl.
	DefaultTTL time.Duration
	// MaxTTL is the longest ttl a fork may ask for; never shorter than
	// DefaultTTL.
	MaxTTL time.Duration
	// MaxPerWorkspace is how many forks one workspace may have; at least 1.
	MaxPerWorkspace int
	// ExpireOnLand is whether the sweep removes a fork a day after all its
	// changes have landed or been abandoned, before its lifetime ends.
	ExpireOnLand bool
}

// forkKeys are the keys of [forks] the loader owns. The section stays open
// to keys other packages register.
var forkKeys = []Key{
	{Section: "forks", Name: "default_ttl", Type: String, Default: "7d", Validate: func(v any) error {
		d, err := forkDuration(v)
		if err == nil && d < MinForkTTL {
			err = errors.New("must be at least 5m")
		}
		return err
	}},
	{Section: "forks", Name: "max_ttl", Type: String, Default: "30d", Validate: func(v any) error {
		_, err := forkDuration(v)
		return err
	}},
	{Section: "forks", Name: "max_per_workspace", Type: Int, Default: 200, Validate: intBetween(1, math.MaxInt)},
	{Section: "forks", Name: "expire_on_land", Type: Bool, Default: true},
}

// forkDuration reads a [forks] duration in the loader's duration form.
func forkDuration(v any) (time.Duration, error) {
	d, err := timefmt.ParseDuration(v.(string))
	if err != nil {
		return 0, fmt.Errorf("must be a duration with a unit of s, m, h or d, such as 7d: %w", err)
	}
	return d, nil
}

// forksFrom returns the [forks] limits of checked values, or the error of a
// max_ttl shorter than default_ttl: a contradiction no single key's rule sees.
func forksFrom(values map[string]map[string]any) (Forks, error) {
	keys := values["forks"]
	// Each value is checked by its key's rule or is the key's default.
	def, _ := forkDuration(keys["default_ttl"])
	maxTTL, _ := forkDuration(keys["max_ttl"])
	if maxTTL < def {
		return Forks{}, errors.New("forks.max_ttl: must not be shorter than forks.default_ttl")
	}
	return Forks{
		DefaultTTL:      def,
		MaxTTL:          maxTTL,
		MaxPerWorkspace: keys["max_per_workspace"].(int),
		ExpireOnLand:    keys["expire_on_land"].(bool),
	}, nil
}
