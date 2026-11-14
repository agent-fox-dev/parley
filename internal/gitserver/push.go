package gitserver

import (
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Result is what a git request came to, as the request counter records it:
// ResultRefused is an answer of 401, 403 or 404; ResultError every other
// answer that is not a served exchange, and an exchange that ended with an
// error of the hub's, a failed git or a timeout; ResultOK everything else.
type Result string

// The results of a git request.
const (
	ResultOK      Result = "ok"
	ResultRefused Result = "refused"
	ResultError   Result = "error"
)

// The credential types of a PushCaller. The admin token is admin_token, the
// name the hub's events give it, not the principal's admin.
const (
	CredentialPAT        = identity.TypePAT
	CredentialAPIKey     = identity.TypeAPIKey
	CredentialAdminToken = event.ActorAdminToken
)

// PushCaller is the caller of a push that passed every check, as the checks
// established it, so that the push processor decides on facts it need not
// look up again.
type PushCaller struct {
	// CredentialType is CredentialPAT, CredentialAPIKey or
	// CredentialAdminToken.
	CredentialType string
	// CredentialID is the PAT's token_id, the API key's key_id, or admin.
	CredentialID string
	// UserID is the user the credential acts for; "" for the admin token.
	UserID string
	// Level is the caller's level on the workspace as the access rule
	// decided it: access.LevelOwner or access.LevelAdmin, since a member
	// never passes the checks of a push.
	Level access.Level
	// Binding is what the binding guard found a bound PAT reaches; nil for
	// every other credential.
	Binding *access.Binding
	// PAT is the PAT as the credential check read it, with its
	// permissions, its paths and its workspace; nil for another credential.
	// The fetch side never reads its paths; a push does.
	PAT *identity.Principal
}

// PushProcessor processes a push that passed every check of the git
// server: the one hand-off to the push rules, which live outside this
// package. ProcessPush gets the response writer, the request with its body
// unread and whole, whatever [server].max_body_size says, since the git
// routes have no body limit, the caller and the workspace record, and
// writes the whole answer. The request's Content-Encoding is its to honour:
// a gzip body is passed on as the client sent it, because the processor
// decides how much of a push it reads and how. Full duplex is enabled on
// the response before the call, so that the processor may answer while it
// reads. It returns the result of the request, which the git server counts
// once; the processor never counts it itself.
type PushProcessor interface {
	ProcessPush(w http.ResponseWriter, r *http.Request, caller PushCaller, ws workspace.Workspace) Result
}

var errNoPushProcessor = errors.New("no push processor is installed")

// push hands a push that passed every check to the push processor, which
// writes the whole answer, and returns the processor's result. Without a
// processor the push is answered 500 internal error with its body unread
// and git not run, so that nothing in the repository changes.
func (c *Component) push(w http.ResponseWriter, r *http.Request, x Exchange) Result {
	if c.deps.Push == nil {
		c.fail(w, r, errNoPushProcessor)
		return ResultError
	}
	_ = http.NewResponseController(w).EnableFullDuplex()
	return c.deps.Push.ProcessPush(w, r, pushCaller(x), x.Workspace)
}

// pushCaller is the caller the checks established for x.
func pushCaller(x Exchange) PushCaller {
	p := PushCaller{
		CredentialType: credentialType(x.Principal),
		CredentialID:   x.Principal.CredentialID,
		UserID:         x.Caller.UserID,
		Level:          x.Level,
		Binding:        x.Binding,
	}
	if x.Principal.Type == identity.TypePAT {
		pat := x.Principal
		p.PAT = &pat
	}
	return p
}

// credentialType is the credential type of p as the push processor and the
// hub's events name it: admin_token for the admin token.
func credentialType(p identity.Principal) string {
	if p.Type == identity.TypeAdmin {
		return CredentialAdminToken
	}
	return p.Type
}
