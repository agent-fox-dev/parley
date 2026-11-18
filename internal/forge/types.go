// Package forge is the hub's client of the forge, GitHub or GitLab, that
// hosts a workspace's remote repository: it works out which forge a remote
// target uses, where its API is and which stored credential to send, so that
// a caller reads the forge through the hub and never holds a forge token.
package forge

import (
	"context"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// Repository is the forge repository a remote URL names: Host is in lower
// case, without a leading www. and without a port; Owner is every path
// segment before the last one joined by "/" (a GitLab group path may have
// several), and Name is the last segment without one trailing .git.
type Repository struct {
	Host  string
	Owner string
	Name  string
}

// ForgeKind is the forge a remote target uses.
type ForgeKind string

// The forge kinds.
const (
	KindGitHub  ForgeKind = "github"
	KindGitLab  ForgeKind = "gitlab"
	KindUnknown ForgeKind = "unknown"
)

// Target is the remote a forge route works on: a workspace's origin, or
// the upstream a carry-patch workspace tracks.
type Target string

// The remote targets.
const (
	TargetOrigin   Target = "origin"
	TargetUpstream Target = "upstream"
)

// The workspace settings that name a target's forge API URL.
const (
	SettingAPIURL         = "forge_api_url"
	SettingUpstreamAPIURL = "upstream_forge_api_url"
)

// SettingName is the name of the workspace setting that names the API URL
// of the target t.
func (t Target) SettingName() string {
	if t == TargetUpstream {
		return SettingUpstreamAPIURL
	}
	return SettingAPIURL
}

// CredentialKind is what the hub sends to the forge for a target.
type CredentialKind string

// The credential kinds: a token, the password of a user name and password,
// or nothing, for an anonymous request.
const (
	CredentialPAT   CredentialKind = "pat"
	CredentialBasic CredentialKind = "basic"
	CredentialNone  CredentialKind = "none"
)

// Credential is the credential of a target: Token is what the forge gets,
// empty for CredentialNone. It is read from the workspace's secrets on every
// resolution and is never kept.
type Credential struct {
	Kind  CredentialKind
	Token string
}

// Settings are a workspace's two forge settings, as stored; "" means the
// default derived from the remote's host.
type Settings struct {
	APIURL         string
	UpstreamAPIURL string
}

// For is the setting of the target t.
func (s Settings) For(t Target) string {
	if t == TargetUpstream {
		return s.UpstreamAPIURL
	}
	return s.APIURL
}

// Resolution is everything a forge request for a remote target needs.
type Resolution struct {
	Target     Target
	Repository Repository
	Kind       ForgeKind
	APIURL     string
	Credential Credential
}

// The error types of a resolution that fails.
const (
	ErrorTypeAPIURLInvalid = "forge_api_url_invalid"
	ErrorTypeUnknown       = "forge_unknown"
)

// ResolveError is a resolution the caller's request cannot succeed with:
// Type is one of the ErrorType constants and Message says what to change.
// A failure of the hub itself, such as a secret that cannot be read, is
// another error.
type ResolveError struct {
	Type    string
	Message string
}

func (e *ResolveError) Error() string { return e.Type + ": " + e.Message }

// RepositoryDetails is a forge repository as the hub answers it, in one
// shape for both forges.
type RepositoryDetails struct {
	FullName      string      `json:"full_name"`
	DefaultBranch string      `json:"default_branch"`
	Private       bool        `json:"private"`
	Archived      bool        `json:"archived"`
	Permissions   Permissions `json:"permissions"`
	// The ways of merging the repository allows; nil when the forge does
	// not say.
	AllowMergeCommit *bool `json:"allow_merge_commit"`
	AllowSquashMerge *bool `json:"allow_squash_merge"`
	AllowRebaseMerge *bool `json:"allow_rebase_merge"`
}

// Permissions are what the target's credential may do on a repository.
type Permissions struct {
	Pull  bool `json:"pull"`
	Push  bool `json:"push"`
	Admin bool `json:"admin"`
}

// The states of an issue.
const (
	IssueOpen   = "open"
	IssueClosed = "closed"
)

// Issue is a forge issue as the hub answers it, in one shape for both
// forges. A timestamp the forge does not give is nil, written as null.
type Issue struct {
	// Number is GitHub's number or GitLab's iid.
	Number int64  `json:"number"`
	Title  string `json:"title"`
	// Body is "" when the forge gives none.
	Body string `json:"body"`
	// State is IssueOpen or IssueClosed.
	State  string `json:"state"`
	URL    string `json:"url"`
	Author Author `json:"author"`
	// Labels are the label names, never nil.
	Labels    []string      `json:"labels"`
	CreatedAt *timefmt.Time `json:"created_at"`
	UpdatedAt *timefmt.Time `json:"updated_at"`
	ClosedAt  *timefmt.Time `json:"closed_at"`
	// IsPR is true when the forge's issue is a pull request, as GitHub's
	// issues may be.
	IsPR bool `json:"is_pr"`
}

// Author is the forge user who wrote something.
type Author struct {
	Login string `json:"login"`
}

// stamp is a forge timestamp as the hub writes it, nil when the forge gave
// none.
func stamp(t *time.Time) *timefmt.Time {
	if t == nil {
		return nil
	}
	return new(timefmt.Time(*t))
}

// api is one forge's REST API: the reads the forge routes make, each
// answering in the hub's shape.
type api interface {
	repository(ctx context.Context, call *Call) (RepositoryDetails, error)
	// issues reads the page p of the issues that match f, and the position
	// of the forge's next page, nil when it names none.
	issues(ctx context.Context, call *Call, f IssueFilter, p Paging) ([]Issue, *Position, error)
}

// apiOf is the API of the forge kind, which resolution has found to be
// GitHub or GitLab.
func apiOf(kind ForgeKind) api {
	if kind == KindGitLab {
		return gitlab{}
	}
	return github{}
}
