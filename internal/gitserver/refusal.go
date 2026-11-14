package gitserver

import (
	"io"
	"net/http"
)

// The messages of the refusals the checks answer with.
const (
	msgAuthRequired = "authentication required"
	msgUserBlocked  = "user is blocked"
	msgRateLimited  = "rate limit exceeded"
	msgInternal     = "internal error"
	msgNotFound     = "repository not found"
	// msgLacksScope is followed by the scope the request needs.
	msgLacksScope     = "token lacks scope "
	msgReadOnly       = "org members have read-only access to this workspace; create a task fork to contribute"
	msgInvalidService = "invalid or missing service parameter"
	// msgNotAvailable is followed by the workspace's status, or busy.
	msgNotAvailable   = "repository not available: workspace is "
	msgParentBusy     = "repository not available: parent workspace is busy"
	msgParentArchived = "repository is read-only: parent workspace is archived"
)

// basicChallenge asks git for a credential: git prompts for one, or asks
// its credential helpers, only on a 401 that carries it.
const basicChallenge = `Basic realm="parley"`

// refuse answers a git request with status and message as one line of plain
// text. Every refusal of the git server goes through it, so that git, which
// shows the body to its user, always gets one line.
func refuse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}

// challenge answers 401 with the Basic challenge. It is the one answer to a
// request whose credential is missing, malformed or does not pass, whatever
// the reason and whatever the request addresses, so that it tells nothing
// about a credential or a repository.
func challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", basicChallenge)
	refuse(w, http.StatusUnauthorized, msgAuthRequired)
}
