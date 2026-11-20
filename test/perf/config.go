// Package perf measures the performance targets of a running hub: it runs
// fourteen scenarios against the hub an operator names, one after the
// other, and writes a report of each target's measured value and verdict.
//
// The harness is a black-box HTTP client of the hub. It imports no server
// code, so that what it measures is what any client sees.
package perf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The environment variables that configure a run.
const (
	EnvURL            = "PARLEY_PERF_URL"
	EnvToken          = "PARLEY_PERF_TOKEN"
	EnvWorkspace      = "PARLEY_PERF_WORKSPACE"
	EnvCapacity       = "PARLEY_PERF_CAPACITY"
	EnvSandboxProfile = "PARLEY_PERF_SANDBOX_PROFILE"
	EnvReport         = "PARLEY_PERF_REPORT"
)

// The defaults of the durations a test may shorten.
const (
	DefaultScenarioDeadline = 10 * time.Minute
	DefaultCapacityDeadline = 60 * time.Minute
	DefaultEventWait        = 10 * time.Second
)

// reportName is the report's file name in the system's temporary directory
// when PARLEY_PERF_REPORT does not name a path.
const reportName = "parley-perf-report.json"

// Config is a run's configuration.
type Config struct {
	// URL is the hub's base URL, such as https://hub.example.
	URL string
	// Token is the credential every request carries as a bearer token.
	Token string
	// Workspaces are the slugs of the workspaces a run uses: the first by
	// every scenario, the others only by active_forks.
	Workspaces []string
	// Capacity runs the capacity scenarios.
	Capacity bool
	// SandboxProfile is the profile of the sandboxes a run creates; empty
	// skips the sandbox scenarios.
	SandboxProfile string
	// ReportPath is where the report is written.
	ReportPath string
	// ScenarioDeadline is how long a scenario may run, and CapacityDeadline
	// how long a capacity scenario may; zero is the default.
	ScenarioDeadline time.Duration
	CapacityDeadline time.Duration
	// EventWait is how long a scenario waits for an event after the
	// response that causes it; zero is the default.
	EventWait time.Duration
}

// ConfigFromEnv reads a run's configuration through lookup. skip is the
// reason not to run, non-empty when PARLEY_PERF_URL is not set; the error
// names a variable a run needs that is not set. A variable set to the empty
// string counts as not set.
func ConfigFromEnv(lookup func(string) (string, bool)) (cfg Config, skip string, err error) {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	cfg.URL = strings.TrimRight(get(EnvURL), "/")
	if cfg.URL == "" {
		return Config{}, EnvURL + " is not set: set it to the URL of a running hub to measure its performance targets", nil
	}
	if cfg.Token = get(EnvToken); cfg.Token == "" {
		return Config{}, "", errors.New(EnvToken + " is not set: set it to a credential with write access to the workspaces")
	}
	for _, slug := range strings.Split(get(EnvWorkspace), ",") {
		if slug = strings.TrimSpace(slug); slug != "" {
			cfg.Workspaces = append(cfg.Workspaces, slug)
		}
	}
	if len(cfg.Workspaces) == 0 {
		return Config{}, "", errors.New(EnvWorkspace + " is not set: set it to the slugs of cloned workspaces, separated by commas")
	}
	cfg.Capacity = get(EnvCapacity) == "1"
	cfg.SandboxProfile = get(EnvSandboxProfile)
	if cfg.ReportPath = get(EnvReport); cfg.ReportPath == "" {
		cfg.ReportPath = filepath.Join(os.TempDir(), reportName)
	}
	return cfg.withDefaults(), "", nil
}

// withDefaults is cfg with the default of each duration that is zero.
func (cfg Config) withDefaults() Config {
	if cfg.ScenarioDeadline <= 0 {
		cfg.ScenarioDeadline = DefaultScenarioDeadline
	}
	if cfg.CapacityDeadline <= 0 {
		cfg.CapacityDeadline = DefaultCapacityDeadline
	}
	if cfg.EventWait <= 0 {
		cfg.EventWait = DefaultEventWait
	}
	return cfg
}
