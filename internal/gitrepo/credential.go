package gitrepo

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
)

// askpassScript answers git's prompts: the user name for the prompt that
// asks for it ("Username for 'https://…': ", fixed by LC_ALL=C), the
// password for every other one. It holds no secret.
const askpassScript = `#!/bin/sh
case "$1" in
Username*) printf '%s\n' "$` + envUsername + `" ;;
*) printf '%s\n' "$` + envPassword + `" ;;
esac
`

// private is the process's private directory for git: the askpass program
// and an empty home directory, so that no user configuration, credential
// store or key of the account running the hub takes part.
var private struct {
	mu   sync.Mutex
	dir  string
	made bool
}

// privateDir returns the private directory, making it, mode 0700, on first
// use and again when it has gone (a temporary directory may be cleaned).
func privateDir() (string, error) {
	private.mu.Lock()
	defer private.mu.Unlock()
	if private.made {
		if _, err := os.Stat(askpassPath(private.dir)); err == nil {
			return private.dir, nil
		}
	}
	dir, err := os.MkdirTemp("", "parley-git-")
	if err != nil {
		return "", fmt.Errorf("make git's private directory: %w", err)
	}
	if err := os.Mkdir(homePath(dir), 0o700); err != nil {
		return "", fmt.Errorf("make git's home directory: %w", err)
	}
	if err := os.WriteFile(askpassPath(dir), []byte(askpassScript), 0o700); err != nil {
		return "", fmt.Errorf("write git's askpass program: %w", err)
	}
	private.dir, private.made = dir, true
	return dir, nil
}

func askpassPath(dir string) string { return filepath.Join(dir, "askpass") }

func homePath(dir string) string { return filepath.Join(dir, "home") }

// credentialEnv is the environment that hands cred to git through the
// askpass program, nil for no credential.
func credentialEnv(dir string, cred Credential) []string {
	if cred.IsZero() {
		return nil
	}
	return []string{
		"GIT_ASKPASS=" + askpassPath(dir),
		envUsername + "=" + cred.Username,
		envPassword + "=" + cred.Password,
	}
}
