package gitrepo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// fetchNamespace is where a fetch writes origin's branches and tags for a
// moment. Git hands over a tag's object only for a ref it writes, over every
// transport, and a fetch must not write the copy's own tags; and git cannot
// replace an origin head by one whose name it is a directory of (dev by
// dev/next) in the transaction of a fetch. So each fetch writes origin's
// refs below a fresh namespace of its own, which no other fetch shares and
// in which no name is in the way, reads it and removes it, so that no such
// ref outlives the fetch, and only then sets the origin heads from what it
// read.
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
// backwards too, and removed for a branch origin no longer has (see
// setOriginHeads). A fetch that fails, or whose fetched refs cannot be
// removed, changes no origin head. No branch and no tag of the copy is
// written: origin's tags come back in Fetched, with their objects in the
// copy, for the caller to create those it wants. When deadline passes first,
// git is killed with every process it started and the error matches
// ErrDeadline; when ctx ends first, the error wraps ctx's error.
func (g Git) FetchOrigin(ctx context.Context, dir, url string, cred Credential, deadline time.Duration) (Fetched, error) {
	repo, err := g.Open(ctx, dir)
	if err != nil {
		return Fetched{}, err
	}
	ns := fetchNamespace + randomName() + "/"
	fetched, err := repo.fetch(ctx, url, cred, deadline, ns)
	// The namespace goes whatever the fetch did, even one cut off after its
	// refs were written, and before any origin head is set, so that a
	// removal that fails leaves the origin heads as they were.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	cleanErr := repo.deleteUnder(cleanup, ns)
	if err != nil {
		return Fetched{}, err
	}
	if cleanErr != nil {
		return Fetched{}, fmt.Errorf("remove the fetched refs: %w", cleanErr)
	}
	if err := repo.setOriginHeads(ctx, fetched.Branches); err != nil {
		return Fetched{}, err
	}
	return fetched, nil
}

// fetch runs the git fetch of FetchOrigin, origin's branches and tags
// written below ns, and returns them.
func (r *Repo) fetch(ctx context.Context, url string, cred Credential, deadline time.Duration, ns string) (Fetched, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	_, err := r.g.run(fetchCtx, access{cred, url}, "-c", "credential.helper=", "--git-dir="+r.dir,
		// Atomic, so that a fetch that fails writes no ref; --no-tags, so
		// that git writes no tag of the copy on its own.
		"fetch", "--quiet", "--atomic", "--force", "--no-tags", "--no-write-fetch-head",
		"--", url, "+refs/heads/*:"+ns+"heads/*", "+refs/tags/*:"+ns+"tags/*")
	if err != nil {
		if ctx.Err() == nil && errors.Is(fetchCtx.Err(), context.DeadlineExceeded) {
			return Fetched{}, fmt.Errorf("git fetch: %w (%v)", ErrDeadline, deadline)
		}
		return Fetched{}, err
	}
	branches, err := r.refs(ctx, ns+"heads/")
	if err != nil {
		return Fetched{}, err
	}
	tags, err := r.refs(ctx, ns+"tags/")
	if err != nil {
		return Fetched{}, err
	}
	return Fetched{Branches: branches, Tags: tags}, nil
}

// setOriginHeads makes the copy's origin heads exactly heads: those of
// branches heads lacks are removed, the others created or moved, in one
// transaction of git's, so that they change together or not at all. Git
// refuses that transaction when a new head's name is a directory of a
// removed one, or the other way round (dev and dev/next); then the removals
// are made first and the rest after them, since an origin head origin no
// longer has is to go in any case.
func (r *Repo) setOriginHeads(ctx context.Context, heads map[string]string) error {
	current, err := r.OriginHeads(ctx)
	if err != nil {
		return err
	}
	var removals, writes strings.Builder
	for name := range current {
		if _, kept := heads[name]; !kept {
			fmt.Fprintf(&removals, "delete %s%s\n", originPrefix, name)
		}
	}
	for name, id := range heads {
		if current[name] != id {
			fmt.Fprintf(&writes, "update %s%s %s\n", originPrefix, name, id)
		}
	}
	if removals.Len()+writes.Len() == 0 {
		return nil
	}
	if r.updateRefs(ctx, removals.String()+writes.String()) == nil {
		return nil
	}
	if err := r.updateRefs(ctx, removals.String()); err != nil {
		return fmt.Errorf("remove the origin heads of branches origin no longer has: %w", err)
	}
	if err := r.updateRefs(ctx, writes.String()); err != nil {
		return fmt.Errorf("record the origin heads: %w", err)
	}
	return nil
}

// updateRefs runs the update-ref --stdin instructions of script, one
// transaction of git's; an empty script does nothing.
func (r *Repo) updateRefs(ctx context.Context, script string) error {
	if script == "" {
		return nil
	}
	_, err := r.g.runInput(ctx, access{}, []byte(script), "--git-dir="+r.dir, "update-ref", "--stdin")
	return err
}

// randomName is 16 random hex digits.
func randomName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}
