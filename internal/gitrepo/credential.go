package gitrepo

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// Credential is what git presents to origin: a user name and a password or
// token, for Basic authentication. The zero value is no credential: git
// then asks origin anonymously and fails where origin wants one.
type Credential struct {
	Username string
	Password string
}

// IsZero reports whether c is no credential.
func (c Credential) IsZero() bool { return c == Credential{} }

// The variables the askpass program reads the credential from. They are set
// only in the environment of a git process run with a credential, so that
// the credential is in no URL, no argument and no configuration file.
const (
	envUsername = "PARLEY_GIT_USERNAME"
	envPassword = "PARLEY_GIT_PASSWORD"
	envOrigin   = "PARLEY_GIT_ORIGIN"
)

// askpassScript answers git's prompts ("Username for 'https://host': ",
// "Password for 'https://user@host': ", fixed by LC_ALL=C) only when the
// prompt names origin's own scheme, host and port: git follows a redirect
// of its first request, and the host it is sent to must not receive the
// credential. It answers the user name for the prompt that asks for it, the
// password for every other one, and fails, so that git fails, for a prompt
// about any other host. It holds no secret.
const askpassScript = `#!/bin/sh
url=${1#*\'}
url=${url%\'*}
case "$url" in
*://*@*) url=${url%%://*}://${url##*@} ;;
esac
[ -n "$` + envOrigin + `" ] && [ "$url" = "$` + envOrigin + `" ] || exit 1
case "$1" in
Username*) printf '%s\n' "$` + envUsername + `" ;;
*) printf '%s\n' "$` + envPassword + `" ;;
esac
`

// privateDir makes a private directory, mode 0700, for one git process: an
// empty home directory, so that no user configuration, credential store or
// key of the account running the hub takes part, and, when withAskpass, the
// askpass program. The caller removes it with os.RemoveAll once git has
// returned, so that no hub or test process leaves one behind.
func privateDir(withAskpass bool) (string, error) {
	dir, err := os.MkdirTemp("", "parley-git-")
	if err != nil {
		return "", fmt.Errorf("make git's private directory: %w", err)
	}
	if err := os.Mkdir(homePath(dir), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("make git's home directory: %w", err)
	}
	if withAskpass {
		if err := os.WriteFile(askpassPath(dir), []byte(askpassScript), 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("write git's askpass program: %w", err)
		}
	}
	return dir, nil
}

func askpassPath(dir string) string { return filepath.Join(dir, "askpass") }

func homePath(dir string) string { return filepath.Join(dir, "home") }

// credentialEnv is the environment that hands cred to git through the
// askpass program for the origin at rawURL alone, nil for no credential.
func credentialEnv(dir string, cred Credential, rawURL string) []string {
	if cred.IsZero() {
		return nil
	}
	return []string{
		"GIT_ASKPASS=" + askpassPath(dir),
		envUsername + "=" + cred.Username,
		envPassword + "=" + cred.Password,
		envOrigin + "=" + promptOrigin(rawURL),
	}
}

// promptOrigin is the scheme, host and port of rawURL as git's prompts name
// them, "" for a URL with no host, about which the askpass program answers
// nothing.
func promptOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
