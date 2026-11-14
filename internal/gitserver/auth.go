package gitserver

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// authenticate reads the credential from HTTP Basic, the only way git sends
// one over HTTP, and checks it as the REST API checks a bearer credential.
// The password is the credential and the user name is never read: git
// sends whatever name the remote URL or the user gave, so the same
// credential passes under every name. A missing or malformed header, an
// empty password and every 401 of the credential check get the one
// challenge; a blocked user gets 403 without it, since another credential
// of the same user would not pass either. A request that does not pass is
// counted against its client address, as the REST API counts it, and is
// answered by authentication even when the address is over its limit.
func (c *Component) authenticate(w http.ResponseWriter, r *http.Request, x *Exchange) bool {
	p, err := c.verify(r)
	if err != nil {
		c.count(server.LimitKey(r, ""))
		var e *server.Error
		switch {
		case errors.As(err, &e) && e.Status() == http.StatusUnauthorized:
			challenge(w)
		case errors.As(err, &e) && e.Status() == http.StatusForbidden:
			refuse(w, http.StatusForbidden, msgUserBlocked)
		default:
			c.fail(w, r, err)
		}
		return false
	}
	x.Principal = p
	x.Caller = server.Caller{CredentialID: p.CredentialID, Admin: p.Type == identity.TypeAdmin, Detail: &p}
	if p.User != nil {
		x.Caller.UserID = p.User.ID
	}
	return true
}

// verify checks the Basic password of r with the credential check.
// r.BasicAuth matches the scheme Basic in any letter case, decodes the
// base64 data and splits it at the first colon.
func (c *Component) verify(r *http.Request) (identity.Principal, error) {
	_, password, ok := r.BasicAuth()
	if !ok || password == "" || c.deps.Credentials == nil {
		return identity.Principal{}, server.Unauthenticated(msgAuthRequired)
	}
	return c.deps.Credentials.VerifyCredential(r.Context(), password)
}

// rateLimit counts an authenticated request against its credential, the key
// the REST API counts the same credential with, and refuses it 429 with
// Retry-After when the credential is over its limit, before the request is
// looked at any further.
func (c *Component) rateLimit(w http.ResponseWriter, r *http.Request, x *Exchange) bool {
	ok, retryAfter := c.count(server.LimitKey(r, x.Caller.CredentialID))
	if ok {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	refuse(w, http.StatusTooManyRequests, msgRateLimited)
	return false
}

// count counts one request against key; without a limiter every request is
// admitted.
func (c *Component) count(key string) (ok bool, retryAfter int) {
	if c.deps.Limiter == nil {
		return true, 0
	}
	return c.deps.Limiter.Allow(key)
}
