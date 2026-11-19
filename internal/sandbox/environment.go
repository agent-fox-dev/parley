package sandbox

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// The variables the hub sets in every sandbox's environment, besides
// HubCredentialVariable, whose placeholder the runtime sets.
const (
	envEndpointURL = "PARLEY_ENDPOINT_URL"
	envWorkspace   = "PARLEY_WORKSPACE"
	envRemote      = "PARLEY_REMOTE"
	envSandboxID   = "PARLEY_SANDBOX_ID"
	envSessionID   = "PARLEY_SESSION_ID"
)

// environment is the environment the sandbox rec is created with at the
// runtime, later entries replacing earlier ones of the same name: the
// caller's env, the profile's env, each exposed secret res placed under its
// own name with its value, then the PARLEY_ variables: the workspace, the
// sandbox id, the session when there is one and, with a hub credential, the
// hub's URL and the workspace's git remote on it.
//
// A credential's placeholder is the runtime's: it puts the placeholder of
// each attached provider into the variables the provider's profile
// declares, read here through client, and that of the hub credential into
// HubCredentialVariable. So those names are left out, whatever an earlier
// entry set, and the placeholders are what the sandbox sees under them.
func (s *Service) environment(ctx context.Context, client runtime.Client, rec Record, res Resolution) (map[string]string, error) {
	env := map[string]string{}
	maps.Copy(env, rec.Env)
	maps.Copy(env, rec.Snapshot.Env)
	maps.Copy(env, res.Exposed)
	read := map[string]bool{}
	for _, name := range rec.Credentials {
		id := rec.Snapshot.ProviderProfiles[name]
		if read[id] {
			continue
		}
		read[id] = true
		profile, err := client.GetProviderProfile(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("the provider profile %s of credential %s: %w", id, name, err)
		}
		for _, v := range profile.Variables {
			delete(env, v)
		}
	}
	env[envWorkspace], env[envSandboxID] = rec.Workspace, rec.ID
	if rec.SessionID != nil {
		env[envSessionID] = *rec.SessionID
	}
	if rec.HubAccess != nil {
		remote, err := s.remoteOf(ctx, rec)
		if err != nil {
			return nil, err
		}
		env[envEndpointURL], env[envRemote] = strings.TrimSuffix(s.deps.ExternalURL, "/"), remote
		delete(env, HubCredentialVariable)
	}
	return env, nil
}

// remoteOf is the hub's git URL of the workspace of rec, the remote its
// checkout clones: by the rule of a workspace's hub_url
// (internal/workspace), the external URL without a trailing slash, then
// /git/<org slug>/<slug>.git.
func (s *Service) remoteOf(ctx context.Context, rec Record) (string, error) {
	org, err := s.orgSlug(ctx, rec.OrgID)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(s.deps.ExternalURL, "/") + "/git/" + org + "/" + rec.Workspace + ".git", nil
}

// orgSlug is the slug of the organization orgID; "" for a workspace without
// one.
func (s *Service) orgSlug(ctx context.Context, orgID string) (string, error) {
	if orgID == "" {
		return "", nil
	}
	if s.deps.OrgSlug == nil {
		return "", fmt.Errorf("the sandbox service cannot read the slug of org %s", orgID)
	}
	slug, err := s.deps.OrgSlug(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("the slug of org %s: %w", orgID, err)
	}
	return slug, nil
}
