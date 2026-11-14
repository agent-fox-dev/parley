package sandbox

import (
	"context"
	"errors"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/scrub"
)

// RuntimeDeps is what a RuntimeFactory builds a client with besides the
// settings: the hub's scrubber, for the client's error texts, and the
// recorder its reachability flag reports to.
type RuntimeDeps struct {
	Scrubber *scrub.Scrubber
	Recorder Recorder
}

// RuntimeFactory builds the runtime client of checked settings with
// sandboxes on. The hub's default builds the OpenShell implementation; a
// test passes one that builds a client of a fake.
type RuntimeFactory func(settings *Settings, deps RuntimeDeps) (runtime.Client, error)

// RuntimeVersion is the sandbox_runtime member of the document of GET
// /version: the supported range and the release read at start.
type RuntimeVersion struct {
	Supported string `json:"supported"`
	Connected string `json:"connected"`
}

// Service is the sandbox feature of one hub: the loaded [sandbox] settings
// and, with sandboxes on, the runtime client the start-up check succeeded
// through. The hub creates it while it composes, before the configuration
// is loaded, so that the parts composed after it can keep it; they read it
// once the hub serves. Configure and Start are called once, in that order,
// before the first request; after that the service does not change.
type Service struct {
	settings    *Settings
	externalURL string
	client      runtime.Client
	release     runtime.Release
	releases    ReleaseRange
	scrubber    *scrub.Scrubber
	// source is what the hub-wide list reads; nil until one is registered.
	source Source
}

// NewService returns the service of a hub whose configuration is not loaded
// yet: sandboxes are off until Configure says otherwise.
func NewService() *Service { return &Service{settings: &Settings{}} }

// Configure checks the [sandbox] section of cfg (see FromConfig) and keeps
// its settings and [server].external_url. It returns the section's error
// and then keeps nothing.
func (s *Service) Configure(cfg *config.Config) error {
	settings, err := FromConfig(cfg)
	if err != nil {
		return err
	}
	s.settings, s.externalURL = settings, cfg.Server.ExternalURL
	return nil
}

// Settings returns the loaded settings; Enabled is false while sandboxes
// are off.
func (s *Service) Settings() *Settings { return s.settings }

// Enabled reports whether the configuration holds a [sandbox] section.
func (s *Service) Enabled() bool { return s.settings.Enabled }

// ExternalURL returns [server].external_url as the configuration gives it.
func (s *Service) ExternalURL() string { return s.externalURL }

// Start runs the start-up check (RunStartCheck) through client, with the
// external URL Configure kept when opts names none, and keeps the client
// and the release once the check has succeeded. It refuses to run while
// sandboxes are off.
func (s *Service) Start(ctx context.Context, client runtime.Client, opts CheckOptions) error {
	if !s.Enabled() {
		return errors.New("sandboxes are not enabled")
	}
	if opts.ExternalURL == "" {
		opts.ExternalURL = s.externalURL
	}
	release, err := RunStartCheck(ctx, s.settings, client, opts)
	if err != nil {
		return err
	}
	s.client, s.release, s.releases = client, release, opts.Releases
	if s.releases == (ReleaseRange{}) {
		s.releases = SupportedReleases
	}
	return nil
}

// Client returns the runtime client; nil while sandboxes are off or before
// the start-up check has succeeded.
func (s *Service) Client() runtime.Client { return s.client }

// Version returns the sandbox_runtime member of GET /version: the
// supported range and the release the start-up check read. ok is false
// while sandboxes are off or before the check has succeeded. It makes no
// call, so it stays the same while the runtime is unreachable.
func (s *Service) Version() (v RuntimeVersion, ok bool) {
	if s.client == nil {
		return RuntimeVersion{}, false
	}
	return RuntimeVersion{Supported: s.releases.String(), Connected: s.release.Version}, true
}
