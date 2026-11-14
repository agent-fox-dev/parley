package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/scrub"
)

// StartCheckBudget is how long the start-up check may take in all, from its
// first call to its last.
const StartCheckBudget = 30 * time.Second

// HubProviderProfile is the id of the provider profile of the hub
// credential, which the start-up check installs at every start.
const HubProviderProfile = "parley-hub"

// HubCredentialVariable is the variable that holds the hub credential's
// placeholder in a sandbox.
const HubCredentialVariable = "PARLEY_API_KEY"

// hubPaths are the path prefixes of the hub the hub credential is added on
// requests to: the REST API and git over HTTP.
var hubPaths = []string{"/api/v1/", "/git/"}

// CheckOptions are the inputs of RunStartCheck besides the settings and the
// runtime client.
type CheckOptions struct {
	// ExternalURL is [server].external_url, the URL a sandbox reaches the hub
	// at; the hub credential's provider profile authorizes its scheme, host
	// and port.
	ExternalURL string
	// Now is the clock the check's deadline is counted from; nil is
	// time.Now. The deadline is that of a context, which follows the real
	// clock, so a replacement must follow it too.
	Now func() time.Time
	// Budget replaces StartCheckBudget when it is not zero.
	Budget time.Duration
	// Releases replaces SupportedReleases when it is not the zero value.
	Releases ReleaseRange
	// Recorder hears parley_sandbox_runtime_up 1 when the check succeeds;
	// nil records nothing.
	Recorder Recorder
}

// RunStartCheck checks, once at start and before the hub serves, that the
// runtime behind client can serve the sandboxes s configures. In this order
// it reads the gateway's release and checks it against the supported range,
// makes sure the client's workspace exists, installs the provider profile
// of the hub credential (HubProviderProfile), reads every distinct provider
// profile the credentials name, refuses an exposed secret whose name is a
// variable of a provider profile attached to the same profile, and sends
// every profile's policy file for validation. It stops at the first failure,
// because each step needs the ones before it, and returns an error that
// names the key to look at: sandbox.gateway_url for a runtime that cannot
// be used or does not answer within the budget,
// sandbox.credentials[<name>].provider_profile,
// sandbox.profiles[<name>].expose_secrets or optional_expose_secrets, or
// sandbox.profiles[<name>].policy. The error's text is scrubbed of the
// gateway token and the OIDC client secret.
//
// On success it keeps on each profile with a policy file the hosts the
// runtime reported the file allows (Profile.PolicyHosts), sets
// parley_sandbox_runtime_up to 1 and returns the release it read.
func RunStartCheck(ctx context.Context, s *Settings, client runtime.Client, opts CheckOptions) (runtime.Release, error) {
	scrubber := scrub.New()
	scrubber.Add(s.Token())
	scrubber.Add(s.OIDCClientSecret())
	c := &startCheck{s: s, client: client, opts: opts}
	release, err := c.run(ctx)
	if err != nil {
		return runtime.Release{}, errors.New(scrubber.Scrub(err.Error()))
	}
	rec := opts.Recorder
	if rec == nil {
		rec = NopRecorder{}
	}
	// A metric that cannot be recorded does not fail the start.
	_ = rec.Set(runtime.MetricUp, nil, 1)
	return release, nil
}

// startCheck is one run of RunStartCheck.
type startCheck struct {
	s      *Settings
	client runtime.Client
	opts   CheckOptions
	ctx    context.Context // the check's, with its deadline
}

func (c *startCheck) run(parent context.Context) (runtime.Release, error) {
	now, budget, releases := c.opts.Now, c.opts.Budget, c.opts.Releases
	if now == nil {
		now = time.Now
	}
	if budget == 0 {
		budget = StartCheckBudget
	}
	if releases == (ReleaseRange{}) {
		releases = SupportedReleases
	}
	ctx, cancel := context.WithDeadline(parent, now().Add(budget))
	defer cancel()
	c.ctx = ctx

	release, err := c.client.Release(ctx)
	if err != nil {
		return release, c.gatewayFailure(budget, "cannot read the gateway's release", err)
	}
	if !release.Healthy {
		return release, errors.New("sandbox.gateway_url: the gateway reports that it is not healthy")
	}
	if !releases.Contains(release.Version) {
		return release, fmt.Errorf("sandbox.gateway_url: the gateway runs OpenShell release %q, outside the supported range %s", release.Version, releases)
	}
	if err := c.client.EnsureWorkspace(ctx); err != nil {
		if c.gatewayLost(err) {
			return release, c.gatewayFailure(budget, "cannot make sure the workspace exists", err)
		}
		return release, fmt.Errorf("sandbox.openshell_workspace: the runtime refused the workspace %q: %s", c.s.OpenShellWorkspace, reason(err))
	}
	if err := c.installHubProfile(budget); err != nil {
		return release, err
	}
	variables, err := c.readProviderProfiles(budget)
	if err != nil {
		return release, err
	}
	if err := c.checkExposedSecrets(variables); err != nil {
		return release, err
	}
	hosts, err := c.validatePolicies(budget)
	if err != nil {
		return release, err
	}
	for i := range c.s.profiles {
		if h, ok := hosts[c.s.profiles[i].Name]; ok {
			c.s.profiles[i].PolicyHosts = h
		}
	}
	return release, nil
}

// gatewayLost reports whether err leaves the runtime unusable: an
// unavailable runtime, or the check's deadline passed.
func (c *startCheck) gatewayLost(err error) bool {
	return errors.Is(err, runtime.ErrUnavailable) || c.ctx.Err() != nil
}

// gatewayFailure is the error of a step that found the runtime unusable.
func (c *startCheck) gatewayFailure(budget time.Duration, step string, err error) error {
	if errors.Is(c.ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("sandbox.gateway_url: the runtime did not answer in time: the start-up check did not finish within %v", budget)
	}
	return fmt.Errorf("sandbox.gateway_url: %s: %v", step, err)
}

// installHubProfile installs the provider profile of the hub credential:
// its placeholder in HubCredentialVariable, added on requests from any
// program to the hub's REST API and git paths at the scheme, host and port
// of the external URL.
func (c *startCheck) installHubProfile(budget time.Duration) error {
	endpoint, err := hubEndpoint(c.opts.ExternalURL)
	if err != nil {
		return err
	}
	err = c.client.InstallProviderProfile(c.ctx, runtime.ProviderProfile{
		ID:        HubProviderProfile,
		Variables: []string{HubCredentialVariable},
		Endpoints: []runtime.Endpoint{endpoint},
	})
	switch {
	case err == nil:
		return nil
	case c.gatewayLost(err):
		return c.gatewayFailure(budget, "cannot install the provider profile "+HubProviderProfile, err)
	}
	return fmt.Errorf("sandbox.gateway_url: the runtime refused the provider profile %s for server.external_url: %s", HubProviderProfile, reason(err))
}

// hubEndpoint is the endpoint of the external URL: its scheme, host and
// port, the scheme's default port when it names none.
func hubEndpoint(externalURL string) (runtime.Endpoint, error) {
	u, err := url.Parse(externalURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return runtime.Endpoint{}, errors.New("server.external_url: must be an http or https URL with a host")
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return runtime.Endpoint{}, errors.New("server.external_url: has a port that is not a number from 1 to 65535")
		}
		port = n
	}
	return runtime.Endpoint{Scheme: u.Scheme, Host: u.Hostname(), Port: port, Paths: slices.Clone(hubPaths)}, nil
}

// readProviderProfiles reads each provider profile the credentials name,
// once, and returns the variables each declares, by profile id.
func (c *startCheck) readProviderProfiles(budget time.Duration) (map[string][]string, error) {
	variables := map[string][]string{}
	for _, cred := range c.s.credentials {
		if _, read := variables[cred.ProviderProfile]; read {
			continue
		}
		p, err := c.client.GetProviderProfile(c.ctx, cred.ProviderProfile)
		switch {
		case err == nil:
			variables[cred.ProviderProfile] = p.Variables
			continue
		case c.gatewayLost(err):
			return nil, c.gatewayFailure(budget, "cannot read the provider profile "+strconv.Quote(cred.ProviderProfile), err)
		case errors.Is(err, runtime.ErrNotFound):
			return nil, fmt.Errorf("sandbox.credentials[%s].provider_profile: the runtime has no provider profile %q", cred.Name, cred.ProviderProfile)
		}
		return nil, fmt.Errorf("sandbox.credentials[%s].provider_profile: cannot read the provider profile %q: %s", cred.Name, cred.ProviderProfile, reason(err))
	}
	return variables, nil
}

// checkExposedSecrets refuses an exposed secret of a profile whose name is a
// variable of the provider profile of a credential the same profile
// attaches: the runtime puts the credential's placeholder there, and the
// two values would collide in the sandbox's environment.
func (c *startCheck) checkExposedSecrets(variables map[string][]string) error {
	for _, p := range c.s.profiles {
		attached := map[string]string{} // variable → credential
		for _, name := range slices.Concat(p.Credentials, p.OptionalCredentials) {
			cred, ok := c.s.Credential(name)
			if !ok {
				continue
			}
			for _, v := range variables[cred.ProviderProfile] {
				if _, seen := attached[v]; !seen {
					attached[v] = cred.Name
				}
			}
		}
		for _, list := range []struct {
			key   string
			names []string
		}{{"expose_secrets", p.ExposeSecrets}, {"optional_expose_secrets", p.OptionalExposeSecrets}} {
			for _, n := range list.names {
				if cred, ok := attached[n]; ok {
					return fmt.Errorf("sandbox.profiles[%s].%s: %q is a variable of the provider profile of the credential %q, which the profile attaches", p.Name, list.key, n, cred)
				}
			}
		}
	}
	return nil
}

// validatePolicies sends the policy file of each profile that names one for
// validation and returns the hosts each allows, sorted, by profile name. A
// runtime that cannot validate a policy refuses the profile, because a
// policy nobody checked must not reach a sandbox. The built-in policy is the
// hub's own and is not sent.
func (c *startCheck) validatePolicies(budget time.Duration) (map[string][]string, error) {
	hosts := map[string][]string{}
	for _, p := range c.s.profiles {
		if p.PolicyPath == "" {
			continue
		}
		key := "sandbox.profiles[" + p.Name + "].policy"
		base, err := os.ReadFile(p.PolicyPath)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot read the policy file: %v", key, err)
		}
		v, err := c.client.ValidatePolicy(c.ctx, runtime.Policy{Base: base})
		switch {
		case err == nil:
			h := append([]string{}, v.Hosts...)
			slices.Sort(h)
			hosts[p.Name] = slices.Compact(h)
			continue
		case c.gatewayLost(err):
			return nil, c.gatewayFailure(budget, "cannot validate the policy file of the profile "+strconv.Quote(p.Name), err)
		case errors.Is(err, runtime.ErrUnsupported):
			return nil, fmt.Errorf("%s: the runtime cannot validate a policy file, so the profile cannot use one", key)
		}
		return nil, fmt.Errorf("%s: the runtime rejected the policy file: %s", key, reason(err))
	}
	return hosts, nil
}

// reason is the runtime's reason in err, or err's text when it has none.
func reason(err error) string {
	var e *runtime.Error
	if errors.As(err, &e) && e.Reason != "" {
		return e.Reason
	}
	return err.Error()
}
