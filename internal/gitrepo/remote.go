package gitrepo

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// remotesPrefix is where a copy keeps the branches of a remote it follows
// besides origin, below one directory per remote.
const remotesPrefix = "refs/remotes/"

// FetchRemote fetches every branch of the remote at url into the copy
// whose git directory is dir, presenting cred when the remote asks for one,
// in one git fetch, and returns the branches it found, each name without
// refs/heads/ mapped to its head. Afterwards the refs
// refs/remotes/<name>/<branch> are the remote's branches exactly: created,
// moved, backwards too, and removed for a branch the remote no longer has.
// name is one ref name component, such as upstream; the copy's own refs
// and those of every other remote are left alone.
//
// It fetches no tag and writes no remote section and no credential into
// the copy's configuration: the URL is given to git after the end of its
// options, so that no value is read as an option. A fetch that fails, or
// whose fetched refs cannot be removed, changes no ref. When deadline
// passes first, git is killed with every process it started and the error
// matches ErrDeadline; when ctx ends first, the error wraps ctx's error.
func (g Git) FetchRemote(ctx context.Context, dir, url, name string, cred Credential, deadline time.Duration) (map[string]string, error) {
	if strings.Contains(name, "/") || !ValidRefName(remotesPrefix+name) {
		return nil, fmt.Errorf("fetch into %q: not a remote name", name)
	}
	repo, err := g.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	// Written below a namespace of its own first, as FetchOrigin does, so
	// that a branch renamed into a directory of its old name (dev to
	// dev/next) does not fail the fetch.
	ns := fetchNamespace + randomName() + "/"
	fetched, err := repo.fetch(ctx, url, cred, deadline, ns, false)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	cleanErr := repo.deleteUnder(cleanup, ns)
	if err != nil {
		return nil, err
	}
	if cleanErr != nil {
		return nil, fmt.Errorf("remove the fetched refs: %w", cleanErr)
	}
	if err := repo.setHeads(ctx, remotesPrefix+name+"/", fetched.Branches); err != nil {
		return nil, err
	}
	return fetched.Branches, nil
}
