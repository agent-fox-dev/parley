package config

import (
	"errors"
	"math"
	"time"
)

// Land holds the keys of [land]: how a land tries, how many lands run at
// once, how long one may take, and the identity of the commits it makes.
type Land struct {
	// MaxAttempts is how many times one land tries to move its target
	// before it gives up as contention; at least 1.
	MaxAttempts int
	// Timeout is how long a land may take from its creation, or from its
	// re-run at a start, before it ends as timed out; above zero.
	Timeout time.Duration
	// Workers is how many lands are computed at once; at least 1.
	Workers int
	// CommitterName and CommitterEmail are the committer of every commit a
	// land makes; neither is empty.
	CommitterName, CommitterEmail string
}

// landKeys are the keys of [land]. The section stays open to keys other
// packages register.
var landKeys = []Key{
	{Section: "land", Name: "max_attempts", Type: Int, Default: 10, Validate: intBetween(1, math.MaxInt)},
	{Section: "land", Name: "timeout", Type: String, Default: "5m", Validate: func(v any) error {
		d, err := durationValue(v)
		if err == nil && d <= 0 {
			err = errors.New("must be above zero")
		}
		return err
	}},
	{Section: "land", Name: "workers", Type: Int, Default: 8, Validate: intBetween(1, math.MaxInt)},
	{Section: "land", Name: "committer_name", Type: String, Default: "Parley", Validate: nonEmpty},
	{Section: "land", Name: "committer_email", Type: String, Default: "parley@localhost", Validate: nonEmpty},
}

// landFrom returns the [land] values of checked values.
func landFrom(values map[string]map[string]any) Land {
	keys := values["land"]
	// Each value is checked by its key's rule or is the key's default.
	timeout, _ := durationValue(keys["timeout"])
	return Land{
		MaxAttempts: keys["max_attempts"].(int), Timeout: timeout, Workers: keys["workers"].(int),
		CommitterName: keys["committer_name"].(string), CommitterEmail: keys["committer_email"].(string),
	}
}
