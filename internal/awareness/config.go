package awareness

import (
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// ConfigSection is the configuration section of the awareness.
const ConfigSection = "awareness"

// The keys of [awareness].
const (
	keyDefaultClaimTTL       = "default_claim_ttl"
	keyMaxClaimTTL           = "max_claim_ttl"
	keyMaxClaimsPerWorkspace = "max_claims_per_workspace"
)

// Config is the [awareness] section: the lifetime of a claim and how many
// active claims a workspace may hold.
type Config struct {
	// DefaultClaimTTL is the lifetime of a claim created without a ttl.
	DefaultClaimTTL time.Duration
	// MaxClaimTTL is the longest ttl a claim may ask for; never shorter
	// than DefaultClaimTTL.
	MaxClaimTTL time.Duration
	// MaxClaimsPerWorkspace is how many active claims one workspace may
	// hold; at least 1.
	MaxClaimsPerWorkspace int
}

// DefaultConfig is [awareness] when the file does not set it.
func DefaultConfig() Config {
	return Config{DefaultClaimTTL: time.Hour, MaxClaimTTL: 24 * time.Hour, MaxClaimsPerWorkspace: 1000}
}

// ConfigKeys are the keys of [awareness], for the configuration loader:
// two positive durations in the product's form (a whole number and one
// unit of s, m, h or d) and a positive integer. That the default lifetime
// is not longer than the maximum is a rule over two keys, which ConfigFrom
// checks.
func ConfigKeys() []config.Key {
	return []config.Key{
		{Section: ConfigSection, Name: keyDefaultClaimTTL, Type: config.String, Default: "1h", Validate: positiveDuration},
		{Section: ConfigSection, Name: keyMaxClaimTTL, Type: config.String, Default: "24h", Validate: positiveDuration},
		{Section: ConfigSection, Name: keyMaxClaimsPerWorkspace, Type: config.Int, Default: 1000, Validate: func(v any) error {
			if v.(int) < 1 {
				return errors.New("must be at least 1")
			}
			return nil
		}},
	}
}

// ConfigFrom reads [awareness] from a configuration loaded with
// ConfigKeys registered, and refuses a default lifetime longer than the
// maximum with an error that names awareness.default_claim_ttl.
func ConfigFrom(cfg *config.Config) (Config, error) {
	def, err := timefmt.ParseDuration(cfg.String(ConfigSection, keyDefaultClaimTTL))
	if err != nil {
		return Config{}, fmt.Errorf("%s.%s: %w", ConfigSection, keyDefaultClaimTTL, err)
	}
	maxTTL, err := timefmt.ParseDuration(cfg.String(ConfigSection, keyMaxClaimTTL))
	if err != nil {
		return Config{}, fmt.Errorf("%s.%s: %w", ConfigSection, keyMaxClaimTTL, err)
	}
	if def > maxTTL {
		return Config{}, fmt.Errorf("%s.%s: must not be longer than %s.%s",
			ConfigSection, keyDefaultClaimTTL, ConfigSection, keyMaxClaimTTL)
	}
	return Config{DefaultClaimTTL: def, MaxClaimTTL: maxTTL, MaxClaimsPerWorkspace: cfg.Int(ConfigSection, keyMaxClaimsPerWorkspace)}, nil
}

// positiveDuration checks a duration key's value: the product's duration
// form, longer than zero.
func positiveDuration(v any) error {
	d, err := timefmt.ParseDuration(v.(string))
	if err != nil {
		return fmt.Errorf("must be a duration with a unit of s, m, h or d, such as 1h: %w", err)
	}
	if d <= 0 {
		return errors.New("must be longer than 0s")
	}
	return nil
}
