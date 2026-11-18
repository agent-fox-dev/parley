// Package forge is the hub's client of the forge, GitHub or GitLab, that
// hosts a workspace's remote repository: it works out which forge a remote
// target uses, where its API is and which stored credential to send, so that
// a caller reads the forge through the hub and never holds a forge token.
package forge

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
