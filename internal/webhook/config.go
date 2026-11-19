package webhook

import (
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The [webhooks] section of the configuration file.
const (
	// SectionName is the section the webhook keys live in.
	SectionName = "webhooks"
	// DefaultMaxPerWorkspace is [webhooks].max_per_workspace when the file
	// does not set it.
	DefaultMaxPerWorkspace = 20
	// DefaultTimeout is [webhooks].timeout when the file does not set it.
	DefaultTimeout = 10 * time.Second
	// minTimeout and maxTimeout bound the timeout the file may set: a
	// receiver needs a moment to answer, and one slow receiver must not
	// hold its subscription's loop for long.
	minTimeout = time.Second
	maxTimeout = time.Minute
)

// Settings are the values of the [webhooks] section.
type Settings struct {
	// MaxPerWorkspace is the most subscriptions a workspace holds, in any
	// state ([webhooks].max_per_workspace).
	MaxPerWorkspace int
	// Timeout bounds one attempt from the connect to the response's status
	// ([webhooks].timeout).
	Timeout time.Duration
	// AllowPrivate lets a destination resolve to an address of the hub's
	// own networks ([webhooks].allow_private).
	AllowPrivate bool
	// AllowInsecure lets a destination on a loopback host use http
	// ([webhooks].allow_insecure).
	AllowInsecure bool
}

// Register registers the keys of [webhooks] with r: max_per_workspace, an
// integer of at least 0 (default 20); timeout, a duration from 1s to 60s
// (default 10s); and allow_private and allow_insecure, bools (default
// false). A value the file sets that breaks its rule, and a key the section
// does not have, fail the load with an error naming the key, which stops the
// start.
func Register(r *config.Registry) error {
	return errors.Join(
		r.Register(config.Key{Section: SectionName, Name: "max_per_workspace", Type: config.Int,
			Default: DefaultMaxPerWorkspace, Validate: validMaxPerWorkspace}),
		r.Register(config.Key{Section: SectionName, Name: "timeout", Type: config.String, Default: "10s",
			Validate: validTimeout}),
		r.Register(config.Key{Section: SectionName, Name: "allow_private", Type: config.Bool, Default: false}),
		r.Register(config.Key{Section: SectionName, Name: "allow_insecure", Type: config.Bool, Default: false}),
	)
}

func validMaxPerWorkspace(v any) error {
	if v.(int) < 0 {
		return errors.New("must be at least 0")
	}
	return nil
}

func validTimeout(v any) error {
	d, err := timefmt.ParseDuration(v.(string))
	if err != nil {
		return fmt.Errorf("must be a duration with a unit of s, m, h or d, such as 10s: %w", err)
	}
	if d < minTimeout || d > maxTimeout {
		return errors.New("must be from 1s to 60s")
	}
	return nil
}

// SettingsFrom reads the [webhooks] section of cfg, loaded with the keys
// Register registers; a key that was not loaded has its default.
func SettingsFrom(cfg *config.Config) Settings {
	s := Settings{
		MaxPerWorkspace: DefaultMaxPerWorkspace,
		Timeout:         DefaultTimeout,
		AllowPrivate:    cfg.Bool(SectionName, "allow_private"),
		AllowInsecure:   cfg.Bool(SectionName, "allow_insecure"),
	}
	// 0 is a limit the file may set, so the key's presence tells it from
	// a key that was not loaded.
	if n, ok := cfg.Value(SectionName, "max_per_workspace"); ok {
		s.MaxPerWorkspace, _ = n.(int)
	}
	if d, err := timefmt.ParseDuration(cfg.String(SectionName, "timeout")); err == nil && d > 0 {
		s.Timeout = d
	}
	return s
}
