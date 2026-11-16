// Package carrypatch keeps a carry-patch workspace: the upstream it tracks,
// its integration branch and the ordered list of patch branches the hub
// rebuilds on top of the upstream.
package carrypatch

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The workspace secrets that hold a carry-patch workspace's upstream
// credential.
const (
	SecretUpstreamGitPAT      = "UPSTREAM_GIT_PAT"
	SecretUpstreamGitUsername = "UPSTREAM_GIT_USERNAME"
	SecretUpstreamGitPassword = "UPSTREAM_GIT_PASSWORD"
)

// tokenUsername is the user name a token is presented with, the same one the
// origin's token uses.
const tokenUsername = "x-token-auth"

// ResolveUpstreamCredential returns the credential git presents to the
// upstream of the workspace slug, read from the workspace's secrets on every
// call so that a rotated secret counts at its next use: a non-empty
// UPSTREAM_GIT_PAT is the user name x-token-auth with the token as password;
// otherwise UPSTREAM_GIT_USERNAME and UPSTREAM_GIT_PASSWORD when both are set
// and not empty; otherwise the workspace's origin credential, which may be
// none. A secret that cannot be read is an error, never no credential, so
// that a broken secret store does not turn into an anonymous request to
// upstream. The credential reaches git through the process environment only
// (internal/gitrepo).
func ResolveUpstreamCredential(ctx context.Context, secrets workspace.SecretReader, slug string) (gitrepo.Credential, error) {
	read := func(name string) (string, error) {
		v, _, err := secrets.Get(ctx, secret.ScopeWorkspace, slug, name)
		if err != nil {
			return "", fmt.Errorf("read the upstream credential of %s: %w", slug, err)
		}
		return v, nil
	}
	token, err := read(SecretUpstreamGitPAT)
	if err != nil {
		return gitrepo.Credential{}, err
	}
	if token != "" {
		return gitrepo.Credential{Username: tokenUsername, Password: token}, nil
	}
	user, err := read(SecretUpstreamGitUsername)
	if err != nil {
		return gitrepo.Credential{}, err
	}
	password, err := read(SecretUpstreamGitPassword)
	if err != nil {
		return gitrepo.Credential{}, err
	}
	if user != "" && password != "" {
		return gitrepo.Credential{Username: user, Password: password}, nil
	}
	return workspace.ResolveOriginCredential(ctx, secrets, slug)
}
