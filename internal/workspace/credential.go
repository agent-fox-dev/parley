package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/server"
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

// credentialCheckTimeout is how long the check of a given credential may ask
// origin before the create is refused.
const credentialCheckTimeout = 10 * time.Second

// errCredentialCheck answers a create whose credential origin did not take.
// Its message is fixed: git's own text may hold the credential or the
// internals of the hub's network, so it is never shown to the caller.
var errCredentialCheck = server.NewError("credential_check_failed", http.StatusBadRequest,
	"the credential was refused or origin could not be reached")

// credential is the credential the create request gives, and whether it
// gives one: a token is presented as the user name x-token-auth.
func (r createRequest) credential() (Credential, bool) {
	switch {
	case r.GitPAT != nil:
		return Credential{Username: tokenUsername, Password: *r.GitPAT}, true
	case r.GitUsername != nil && r.GitPassword != nil:
		return Credential{Username: *r.GitUsername, Password: *r.GitPassword}, true
	}
	return Credential{}, false
}

// checkCredential refuses the create of slug unless its credential can be
// stored and origin takes it: without a secret key it answers 503 before
// origin is asked, since the credential could not be kept; then it lists
// url's refs with cred under a deadline, so that a mistyped token costs one
// refused request rather than a clone that fails later. The refusal is
// logged with git's text scrubbed of the credential.
func (s *Service) checkCredential(ctx context.Context, slug, url string, cred Credential) error {
	if !s.deps.Secrets.Available() {
		return secret.UnavailableError
	}
	ctx, cancel := context.WithTimeout(ctx, credentialCheckTimeout)
	defer cancel()
	err := s.deps.Lister.ListOrigin(ctx, url, cred)
	if err == nil {
		return nil
	}
	attrs := []any{"slug", slug, "git_url", url}
	if s.deps.Scrubber != nil {
		attrs = append(attrs, "error", s.deps.Scrubber.Scrub(err.Error(), cred.Password))
	}
	s.deps.Logger.Warn("workspace credential check refused", attrs...)
	return errCredentialCheck
}

// storeCredential writes the credential of the create request req as the
// workspace secrets of slug inside tx, the create transaction, so that an
// error rolls the whole create back: GIT_PAT for a token, GIT_USERNAME and
// GIT_PASSWORD for a user name and password, nothing without a credential.
func (s *Service) storeCredential(ctx context.Context, tx *sql.Tx, slug string, req createRequest) error {
	for _, v := range []struct {
		name  string
		value *string
	}{{SecretGitPAT, req.GitPAT}, {SecretGitUsername, req.GitUsername}, {SecretGitPassword, req.GitPassword}} {
		if v.value == nil {
			continue
		}
		if err := s.deps.Secrets.Put(ctx, tx, secret.ScopeWorkspace, slug, v.name, *v.value); err != nil {
			return fmt.Errorf("store the origin credential of %s: %w", slug, err)
		}
	}
	return nil
}
