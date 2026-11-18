package forge

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/agent-fox-dev/parley/internal/secret"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The workspace secrets that hold a carry-patch workspace's upstream
// credential, the names internal/carrypatch gives them.
const (
	secretUpstreamGitPAT      = "UPSTREAM_GIT_PAT"
	secretUpstreamGitUsername = "UPSTREAM_GIT_USERNAME"
	secretUpstreamGitPassword = "UPSTREAM_GIT_PASSWORD"
)

// remoteSchemes are the URL schemes a remote URL may have; a remote without
// a scheme is in git's scp form, user@host:owner/name.
var remoteSchemes = map[string]bool{"https": true, "http": true, "ssh": true, "git": true}

// ParseRemote derives the forge repository the remote URL raw names, and
// whether it names one: it does when it has a host and at least two path
// segments that are not empty once one trailing .git is removed from the
// last. User information and a port are ignored.
func ParseRemote(raw string) (Repository, bool) {
	var host, path string
	if scheme, _, found := strings.Cut(raw, "://"); found {
		u, err := url.Parse(raw)
		if err != nil || !remoteSchemes[strings.ToLower(scheme)] {
			return Repository{}, false
		}
		host, path = u.Hostname(), u.Path
	} else {
		// The scp form: the host ends at the first colon, which comes
		// before any slash; a local path has none.
		colon := strings.IndexByte(raw, ':')
		if colon < 0 || strings.IndexByte(raw[:colon], '/') >= 0 {
			return Repository{}, false
		}
		host, path = raw[:colon], raw[colon+1:]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	var segments []string
	for s := range strings.SplitSeq(path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	if host == "" || len(segments) < 2 {
		return Repository{}, false
	}
	name := strings.TrimSuffix(segments[len(segments)-1], ".git")
	if name == "" {
		return Repository{}, false
	}
	return Repository{Host: host, Owner: strings.Join(segments[:len(segments)-1], "/"), Name: name}, true
}

// Kind decides the forge of a remote target from its API URL setting and
// its remote's host, in that order: the first of the two that contains
// exactly one of the words github and gitlab, ignoring case, names the
// forge.
func Kind(setting, host string) ForgeKind {
	for _, s := range []string{setting, host} {
		s = strings.ToLower(s)
		switch github, gitlab := strings.Contains(s, "github"), strings.Contains(s, "gitlab"); {
		case github && !gitlab:
			return KindGitHub
		case gitlab && !github:
			return KindGitLab
		}
	}
	return KindUnknown
}

// APIURL is the API URL of a forge of the given kind: the setting without
// one trailing "/" when it is set, otherwise the forge's default for the
// remote's host. A GitLab API URL always ends in /api/v4. It is "" for an
// unknown kind.
func APIURL(kind ForgeKind, setting, host string) string {
	var api string
	switch {
	case kind != KindGitHub && kind != KindGitLab:
		return ""
	case setting != "":
		api = strings.TrimSuffix(setting, "/")
	case kind == KindGitHub && host == "github.com":
		return "https://api.github.com"
	case kind == KindGitHub:
		return "https://" + host + "/api/v3"
	default:
		api = "https://" + host
	}
	if kind == KindGitLab && !strings.HasSuffix(api, "/api/v4") {
		api += "/api/v4"
	}
	return api
}

// CheckAPIURL accepts "", which means the default, and an absolute https
// URL with a host and without user information, query or fragment. It is
// the rule the settings route stores the value by, checked again at use
// because a stored value is not the forge's to trust.
func CheckAPIURL(setting string) error {
	return workspace.ValidateForgeURL(setting)
}

// ResolveCredential reads the credential of the target from the secrets of
// the workspace slug, anew on every call so that a changed secret counts at
// its next use. For origin: a non-empty GIT_PAT is a token; otherwise
// GIT_USERNAME and GIT_PASSWORD, both set and not empty, are basic with the
// password as token; otherwise none. For upstream the UPSTREAM_ secrets are
// tried in the same order first, then the origin's. A secret that cannot be
// read is an error, never no credential, so that a broken secret store does
// not turn into an anonymous request.
func ResolveCredential(ctx context.Context, secrets workspace.SecretReader, slug string, target Target) (Credential, error) {
	if target == TargetUpstream {
		c, found, err := readCredential(ctx, secrets, slug, secretUpstreamGitPAT, secretUpstreamGitUsername, secretUpstreamGitPassword)
		if err != nil || found {
			return c, err
		}
	}
	c, found, err := readCredential(ctx, secrets, slug, workspace.SecretGitPAT, workspace.SecretGitUsername, workspace.SecretGitPassword)
	if err != nil || found {
		return c, err
	}
	return Credential{Kind: CredentialNone}, nil
}

// readCredential reads one token secret and one user name and password
// pair, and reports whether they make a credential.
func readCredential(ctx context.Context, secrets workspace.SecretReader, slug, patName, userName, passwordName string) (Credential, bool, error) {
	var values [3]string
	for i, name := range []string{patName, userName, passwordName} {
		v, _, err := secrets.Get(ctx, secret.ScopeWorkspace, slug, name)
		if err != nil {
			return Credential{}, false, fmt.Errorf("read the forge credential of %s: %w", slug, err)
		}
		if i == 0 && v != "" {
			return Credential{Kind: CredentialPAT, Token: v}, true, nil
		}
		values[i] = v
	}
	if values[1] != "" && values[2] != "" {
		return Credential{Kind: CredentialBasic, Token: values[2]}, true, nil
	}
	return Credential{}, false, nil
}

// Resolve derives everything a forge request for the target of the
// workspace ws needs: the repository its remote URL names (git_url for
// origin, upstream_url for upstream), the forge kind, the API URL and the
// credential. A request that cannot succeed is a *ResolveError, checked in
// this order: the target's setting is invalid (forge_api_url_invalid); the
// URL names no repository, or the kind is unknown (forge_unknown). The
// credential is read only then; a secret that cannot be read is another
// error. Which workspace's record, settings and secrets apply, such as a
// fork's parent's, is the caller's choice.
func Resolve(ctx context.Context, secrets workspace.SecretReader, ws workspace.Workspace, target Target, settings Settings) (Resolution, error) {
	setting, remote := settings.For(target), ws.GitURL
	if target == TargetUpstream {
		remote = ws.UpstreamURL
	}
	if err := CheckAPIURL(setting); err != nil {
		return Resolution{}, &ResolveError{Type: ErrorTypeAPIURLInvalid,
			Message: fmt.Sprintf("the setting %s %s", target.SettingName(), err)}
	}
	var repo Repository
	ok := remote != nil
	if ok {
		repo, ok = ParseRemote(*remote)
	}
	if !ok {
		return Resolution{}, &ResolveError{Type: ErrorTypeUnknown,
			Message: fmt.Sprintf("the %s URL of the workspace names no forge repository", target)}
	}
	kind := Kind(setting, repo.Host)
	if kind == KindUnknown {
		return Resolution{}, &ResolveError{Type: ErrorTypeUnknown,
			Message: fmt.Sprintf("cannot tell whether %s is GitHub or GitLab; set %s to the forge's API URL",
				repo.Host, target.SettingName())}
	}
	cred, err := ResolveCredential(ctx, secrets, ws.Slug, target)
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{Target: target, Repository: repo, Kind: kind, APIURL: APIURL(kind, setting, repo.Host), Credential: cred}, nil
}
