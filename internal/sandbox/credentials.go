package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Resolution is what resolving a profile's credentials and exposed secrets
// along a workspace's chain found. It holds values: it is kept in memory
// only, and only the names are stored or shown.
type Resolution struct {
	// Credentials maps the name of each attached credential to the value of
	// its secret.
	Credentials map[string]string
	// Exposed maps the name of each placed secret to its value.
	Exposed map[string]string
	// MissingCredentials are the required credentials whose secret resolved
	// nowhere, and MissingSecrets the required exposed secrets that did not,
	// both sorted.
	MissingCredentials, MissingSecrets []string
	// Searched are the origins of the steps searched, in chain order; empty
	// when no name was asked.
	Searched []string
}

// CredentialNames returns the names of the attached credentials, sorted.
func (r Resolution) CredentialNames() []string { return sortedKeys(r.Credentials) }

// SecretNames returns the names of the placed secrets, sorted.
func (r Resolution) SecretNames() []string { return sortedKeys(r.Exposed) }

// Values returns the values the resolution found, for the scrubbing of the
// sandbox's text.
func (r Resolution) Values() Values {
	return Values{Credentials: slices.Sorted(maps.Values(r.Credentials)), Exposed: slices.Sorted(maps.Values(r.Exposed))}
}

// Missing returns the refusal of a resolution in which a required name
// resolved nowhere, nil when none: 422 credential_missing, naming the
// missing credentials and secrets and the scopes searched.
func (r Resolution) Missing() error {
	if len(r.MissingCredentials) == 0 && len(r.MissingSecrets) == 0 {
		return nil
	}
	var parts []string
	if len(r.MissingCredentials) > 0 {
		parts = append(parts, "credentials "+strings.Join(r.MissingCredentials, ", "))
	}
	if len(r.MissingSecrets) > 0 {
		parts = append(parts, "secrets "+strings.Join(r.MissingSecrets, ", "))
	}
	msg := fmt.Sprintf("missing %s; searched %s", strings.Join(parts, " and "), strings.Join(r.Searched, ", "))
	return server.NewError("credential_missing", http.StatusUnprocessableEntity, msg).
		With("credentials", nonNil(r.MissingCredentials)).
		With("secrets", nonNil(r.MissingSecrets)).
		With("scopes_searched", nonNil(r.Searched))
}

// errSecretsUnreadable answers a resolution that met a secret store it
// cannot read: no encryption key, or a value that cannot be decrypted.
var errSecretsUnreadable = server.NewError("secrets_unavailable", http.StatusServiceUnavailable,
	"the profile's secrets cannot be read: the secret store has no encryption key or holds a value it cannot decrypt")

// Resolve resolves, along the chain of the workspace slug, the secret of
// every credential snap lists, required and optional, and every secret it
// exposes, required and optional, in one call of the hub's resolution. A
// credential's secret is the one its configuration names; a credential the
// configuration no longer has resolves nowhere. An optional name that
// resolves nowhere is left out; a required one is listed as missing, which
// Missing turns into the refusal.
//
// When snap lists nothing the resolution is not asked at all. A secret store
// that cannot be read is 503 secrets_unavailable (a *server.Error); any
// other failure is an error that names no value.
func (s *Service) Resolve(ctx context.Context, slug string, snap Snapshot) (Resolution, error) {
	return s.resolveIn(ctx, nil, slug, snap)
}

// resolveIn is Resolve with the workspace records of slug's chain read
// through q, nil for the secret component's own reads: a create reads them
// in its transaction, in which the fork create has stored the fork it asks
// a sandbox for and not committed it.
func (s *Service) resolveIn(ctx context.Context, q access.Querier, slug string, snap Snapshot) (Resolution, error) {
	out := Resolution{Credentials: map[string]string{}, Exposed: map[string]string{}, Searched: []string{}}
	credentials := slices.Concat(snap.Credentials, snap.OptionalCredentials)
	exposed := slices.Concat(snap.ExposeSecrets, snap.OptionalExposeSecrets)
	if len(credentials) == 0 && len(exposed) == 0 {
		return out, nil
	}
	secretOf := map[string]string{}
	for _, name := range credentials {
		if c, ok := s.settings.Credential(name); ok {
			secretOf[name] = c.Secret
		}
	}
	asked := slices.Concat(slices.Collect(maps.Values(secretOf)), exposed)
	if s.deps.ResolveSecrets == nil {
		return Resolution{}, errors.New("the sandbox service has no secret resolution")
	}
	res, err := s.deps.ResolveSecrets(ctx, q, slug, asked)
	switch {
	case errors.Is(err, secret.ErrUnavailable), errors.Is(err, secret.ErrUnreadable):
		return Resolution{}, errSecretsUnreadable
	case err != nil:
		return Resolution{}, fmt.Errorf("resolve the secrets of sandbox profile on %s: %w", slug, err)
	}
	// The resolution answers each name once, compared ignoring ASCII case.
	found := map[string]string{}
	for _, r := range res.Secrets {
		if r.Found {
			found[strings.ToLower(r.Name)] = r.Value
		}
	}
	lookup := func(name string) (string, bool) {
		v, ok := found[strings.ToLower(name)]
		return v, ok
	}
	for _, name := range credentials {
		v, ok := "", false
		if sec, known := secretOf[name]; known {
			v, ok = lookup(sec)
		}
		switch {
		case ok:
			out.Credentials[name] = v
		case slices.Contains(snap.Credentials, name):
			out.MissingCredentials = append(out.MissingCredentials, name)
		}
	}
	for _, name := range exposed {
		v, ok := lookup(name)
		switch {
		case ok:
			out.Exposed[name] = v
		case slices.Contains(snap.ExposeSecrets, name):
			out.MissingSecrets = append(out.MissingSecrets, name)
		}
	}
	slices.Sort(out.MissingCredentials)
	slices.Sort(out.MissingSecrets)
	out.MissingCredentials, out.MissingSecrets = slices.Compact(out.MissingCredentials), slices.Compact(out.MissingSecrets)
	for _, step := range res.Searched {
		out.Searched = append(out.Searched, string(step.Origin))
	}
	return out, nil
}

// nonNil is list, or an empty list for nil, so that JSON shows [].
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
