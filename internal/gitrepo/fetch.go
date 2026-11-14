package gitrepo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// fetchNamespace is where a fetch writes origin's tags for a moment. Git
// hands over a tag's object only for a ref it writes, over every transport,
// and a fetch must not write the copy's own tags; so each fetch writes them
// below a namespace of its own, which no other fetch shares, and removes it
// before it returns, so that no such ref outlives the fetch.
const fetchNamespace = "refs/parley/fetch/"

// cleanupTimeout bounds the removal of a fetch's namespace, which runs also
// after the fetch's context ended.
const cleanupTimeout = 10 * time.Second

// ErrDeadline is matched by the error of a fetch that did not end before its
// deadline.
var ErrDeadline = errors.New("origin did not answer within the deadline")

// Fetched is what one fetch found on origin.
type Fetched struct {
	// Branches maps each branch origin has, without refs/heads/, to its
	// head: the origin heads the copy keeps after the fetch.
	Branches map[string]string
	// Tags maps each tag origin has, without refs/tags/, to the object id it
	// points at, the tag object of an annotated tag. The objects are in the
	// copy; the tags are not, unless the copy had them already.
	Tags map[string]string
}

// FetchOrigin fetches every branch and every tag of the origin at url into
// the copy whose git directory is dir, presenting cred when origin asks for
// one, in one git fetch. Afterwards the copy's origin heads (the refs
// refs/remotes/origin/<name>) are origin's branches exactly: created, moved,
// backwards too, and removed for a branch origin no longer has, all of them
// or, when the fetch fails, none. No branch and no tag of the copy is
// written: origin's tags come back in Fetched, with their objects in the
// copy, for the caller to create those it wants. When deadline passes
// first, git is killed with every process it started and the error matches
// ErrDeadline; when ctx ends first, the error wraps ctx's error.
func (g Git) FetchOrigin(ctx context.Context, dir, url string, cred Credential, deadline time.Duration) (Fetched, error) {
	repo, err := g.Open(ctx, dir)
	if err != nil {
		return Fetched{}, err
	}
	ns := fetchNamespace + randomName() + "/"
	tags, err := repo.fetch(ctx, url, cred, deadline, ns)
	// The namespace goes whatever the fetch did, even one cut off after its
	// refs were written.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	cleanErr := repo.deleteUnder(cleanup, ns)
	if err != nil {
		return Fetched{}, err
	}
	if cleanErr != nil {
		return Fetched{}, fmt.Errorf("remove the fetched tags' refs: %w", cleanErr)
	}
	branches, err := repo.OriginHeads(ctx)
	if err != nil {
		return Fetched{}, err
	}
	return Fetched{Branches: branches, Tags: tags}, nil
}

// fetch runs the git fetch of FetchOrigin, origin's tags written below ns,
// and returns those tags.
func (r *Repo) fetch(ctx context.Context, url string, cred Credential, deadline time.Duration, ns string) (map[string]string, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	_, err := r.g.run(fetchCtx, access{cred, url}, "-c", "credential.helper=", "--git-dir="+r.dir,
		// Forced and pruned, so that the origin heads follow origin wherever
		// it went; atomic, so that they change together or not at all;
		// --no-tags, so that git writes no tag of the copy on its own.
		"fetch", "--quiet", "--atomic", "--prune", "--force", "--no-tags", "--no-write-fetch-head",
		"--", url, "+refs/heads/*:"+originPrefix+"*", "+refs/tags/*:"+ns+"*")
	if err != nil {
		if ctx.Err() == nil && errors.Is(fetchCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git fetch: %w (%v)", ErrDeadline, deadline)
		}
		return nil, err
	}
	return r.refs(ctx, ns)
}

// randomName is 16 random hex digits.
func randomName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}
