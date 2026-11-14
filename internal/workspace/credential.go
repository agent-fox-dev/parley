package workspace

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/secret"
)

// The workspace secrets that hold a workspace's origin credential.
const (
	SecretGitPAT      = "GIT_PAT"
	SecretGitUsername = "GIT_USERNAME"
	SecretGitPassword = "GIT_PASSWORD"
)

// tokenUsername is the user name a token is presented with.
const tokenUsername = "x-token-auth"

// SecretReader reads one secret of one scope: found is false, with no
// error, when the scope holds no such secret. *secret.Component satisfies
// it.
type SecretReader interface {
	Get(ctx context.Context, scope secret.Scope, scopeID, name string) (value string, found bool, err error)
}

// ResolveOriginCredential returns the credential git presents to the origin
// of the workspace slug, read from the workspace's secrets on every call so
// that a changed secret counts at its next use: a non-empty GIT_PAT is the
// user name x-token-auth with the token as password; otherwise GIT_USERNAME
// and GIT_PASSWORD when both are set and not empty; otherwise no credential.
// A secret that cannot be read is an error, never no credential, so that a
// broken secret store does not turn into an anonymous request to origin.
func ResolveOriginCredential(ctx context.Context, secrets SecretReader, slug string) (Credential, error) {
	read := func(name string) (string, error) {
		v, _, err := secrets.Get(ctx, secret.ScopeWorkspace, slug, name)
		if err != nil {
			return "", fmt.Errorf("read the origin credential of %s: %w", slug, err)
		}
		return v, nil
	}
	token, err := read(SecretGitPAT)
	if err != nil {
		return Credential{}, err
	}
	if token != "" {
		return Credential{Username: tokenUsername, Password: token}, nil
	}
	user, err := read(SecretGitUsername)
	if err != nil {
		return Credential{}, err
	}
	password, err := read(SecretGitPassword)
	if err != nil || user == "" || password == "" {
		return Credential{}, err
	}
	return Credential{Username: user, Password: password}, nil
}
