package gitrepo

import (
	"context"
	"errors"
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

// ErrNoSuchBranch is the error of FetchOriginBranch for a branch origin does
// not have.
var ErrNoSuchBranch = errors.New("origin has no such branch")

// FetchOriginBranch fetches the one branch branch of the origin at url into
// the copy whose git directory is dir, presenting cred when origin asks for
// one, and returns its head on origin. The copy's origin head of the branch,
// refs/remotes/origin/<branch>, is set to it, created or moved, backwards
// too; no other ref changes, no tag is fetched, and no remote section and
// no credential is written into the copy's configuration.
//
// The branch reaches git only in full ref form, and the URL and the
// refspec only after the end of git's options, so that no value is read as
// an option; a branch that begins with '-' is ErrInvalidArgument, and a
// name git would not take for a branch is refused, both without running
// git. When origin has no such branch, the error matches
// ErrNoSuchBranch; when another ref of the copy is in the way of the
// origin head's name (refs/remotes/origin/feat for feat/x), it matches
// ErrRefNameConflict and origin is not asked. A fetch that fails changes no
// ref. When deadline passes first, git is killed with every process it
// started and the error matches ErrDeadline; when ctx ends first, the
// error wraps ctx's error.
func (g Git) FetchOriginBranch(ctx context.Context, dir, url, branch string, cred Credential, deadline time.Duration) (string, error) {
	if err := refuseOption("branch", branch); err != nil {
		return "", err
	}
	src, dst := "refs/heads/"+branch, originPrefix+branch
	if !ValidRefName(src) {
		return "", fmt.Errorf("fetch the branch %q: not a branch name", branch)
	}
	repo, err := g.Open(ctx, dir)
	if err != nil {
		return "", err
	}
	if name, blocked, err := repo.RefInTheWay(ctx, dst); err != nil {
		return "", err
	} else if blocked {
		return "", fmt.Errorf("fetch %s into %s: %s: %w", src, dst, name, ErrRefNameConflict)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	_, stderr, err := g.runOutput(fetchCtx, access{cred, url}, nil, "-c", "credential.helper=", "--git-dir="+repo.dir,
		"fetch", "--quiet", "--atomic", "--force", "--no-tags", "--no-write-fetch-head",
		"--", url, "+"+src+":"+dst)
	if err != nil {
		switch {
		case ctx.Err() == nil && errors.Is(fetchCtx.Err(), context.DeadlineExceeded):
			return "", fmt.Errorf("git fetch: %w (%v)", ErrDeadline, deadline)
		// Git's own words for a ref the remote does not advertise, fixed
		// by LC_ALL=C, after which it exits with 128.
		case exitCode(err) == 128 && strings.Contains(stderr, "couldn't find remote ref"):
			return "", fmt.Errorf("fetch %s: %w", src, ErrNoSuchBranch)
		}
		return "", err
	}
	ref, found, err := repo.LookupRef(ctx, dst)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("fetch %s: git wrote no %s", src, dst)
	}
	return ref.ID, nil
}
