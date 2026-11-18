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

// Comment is a comment on a forge issue as the hub answers it, in one shape
// for both forges.
type Comment struct {
	// ID is the forge's numeric id of the comment.
	ID int64 `json:"id"`
	// Body is "" when the forge gives none.
	Body      string        `json:"body"`
	Author    Author        `json:"author"`
	CreatedAt *timefmt.Time `json:"created_at"`
	UpdatedAt *timefmt.Time `json:"updated_at"`
	URL       string        `json:"url"`
}

// IssueThread is an issue with the first page of its comments, oldest
// first. When the comments could not be read, Comments is empty and
// CommentsError says why; the issue is answered all the same.
type IssueThread struct {
	Issue    Issue     `json:"issue"`
	Comments []Comment `json:"comments"`
	// CommentsNextCursor continues the comments on the issue's comments
	// route; nil when no comment follows.
	CommentsNextCursor *string        `json:"comments_next_cursor"`
	CommentsError      *CommentsError `json:"comments_error"`
}

// CommentsError is the failure of the comments read of an issue thread,
// as the hub's error for it (MapError).
type CommentsError struct {
	Type    string `json:"error_type"`
	Message string `json:"message"`
}

// The states of a pull request: an issue's, and merged.
const PullMerged = "merged"

// PullRequest is a forge pull request, a GitLab merge request, as the hub
// answers it, in one shape for both forges. A timestamp the forge does not
// give is nil, written as null.
type PullRequest struct {
	// Number is GitHub's number or GitLab's iid.
	Number int64  `json:"number"`
	Title  string `json:"title"`
	// Body is "" when the forge gives none.
	Body string `json:"body"`
	// State is IssueOpen, IssueClosed or PullMerged.
	State  string `json:"state"`
	URL    string `json:"url"`
	Draft  bool   `json:"draft"`
	Merged bool   `json:"merged"`
	// HeadSHA is the full id of the head commit.
	HeadSHA    string        `json:"head_sha"`
	HeadBranch string        `json:"head_branch"`
	BaseBranch string        `json:"base_branch"`
	Author     Author        `json:"author"`
	CreatedAt  *timefmt.Time `json:"created_at"`
	UpdatedAt  *timefmt.Time `json:"updated_at"`
	ClosedAt   *timefmt.Time `json:"closed_at"`
	MergedAt   *timefmt.Time `json:"merged_at"`
}

// PullRequestState is the part of a pull request that says where it
// stands.
type PullRequestState struct {
	Number  int64  `json:"number"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	HeadSHA string `json:"head_sha"`
}

// The statuses of a changed file.
const (
	FileAdded    = "added"
	FileModified = "modified"
	FileDeleted  = "deleted"
	FileRenamed  = "renamed"
)

// ChangedFile is a file a pull request changes, with the lines it adds and
// deletes.
type ChangedFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// The statuses and conclusions of a check run.
const (
	CheckQueued     = "queued"
	CheckInProgress = "in_progress"
	CheckCompleted  = "completed"

	CheckSuccess   = "success"
	CheckFailure   = "failure"
	CheckCancelled = "cancelled"
	CheckSkipped   = "skipped"
	CheckNeutral   = "neutral"
)

// CheckRun is a check run of a pull request's head, a job of a GitLab merge
// request's latest pipeline, as the hub answers it.
type CheckRun struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Conclusion is nil while the run is not completed.
	Conclusion *string `json:"conclusion"`
	// Summary is "" when the forge gives none.
	Summary string `json:"summary"`
	URL     string `json:"url"`
}

// The states of a review.
const (
	ReviewApproved         = "approved"
	ReviewChangesRequested = "changes_requested"
	ReviewCommented        = "commented"
	ReviewDismissed        = "dismissed"
)

// Review is a review of a pull request as the hub answers it: on GitLab an
// approval, which has no time, or a note.
type Review struct {
	Author Author `json:"author"`
	State  string `json:"state"`
	// Body is "" when the forge gives none.
	Body        string        `json:"body"`
	SubmittedAt *timefmt.Time `json:"submitted_at"`
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
	// issue reads the issue number.
	issue(ctx context.Context, call *Call, number int64) (Issue, error)
	// comments reads the page p of the comments of the issue number, oldest
	// first, and the position of the forge's next page, nil when it names
	// none.
	comments(ctx context.Context, call *Call, number int64, p Paging) ([]Comment, *Position, error)
	// pull reads the pull request number.
	pull(ctx context.Context, call *Call, number int64) (PullRequest, error)
	// files reads the page p of the files the pull request number changes.
	files(ctx context.Context, call *Call, number int64, p Paging) ([]ChangedFile, *Position, error)
	// checks reads the page p of the check runs of the pull request
	// number's head: the head p's anchor names, else the head the forge
	// names now, which becomes the next position's anchor.
	checks(ctx context.Context, call *Call, number int64, p Paging) ([]CheckRun, *Position, error)
	// checkAnchor reports whether anchor can be the anchor of a later
	// check run page: what the first page found, which a cursor carries
	// into a request path.
	checkAnchor(anchor string) bool
	// reviews reads the page p of the reviews of the pull request number,
	// oldest first.
	reviews(ctx context.Context, call *Call, number int64, p Paging) ([]Review, *Position, error)
}

// apiOf is the API of the forge kind, which resolution has found to be
// GitHub or GitLab.
func apiOf(kind ForgeKind) api {
	if kind == KindGitLab {
		return gitlab{}
	}
	return github{}
}
