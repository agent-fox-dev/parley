package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrRefMoved is the error of a compare-and-swap that found the ref at
// another value than the one it expected, or held by another writer that is
// moving it at that moment. The ref is left as the other writer has it; the
// caller reads it again and decides anew.
var ErrRefMoved = errors.New("the ref is not at the expected value")

// CompareAndSwapRef points ref, a full ref name such as refs/heads/main, at
// newID only if it points at expectedOld now, as one atomic step of git's
// ref transaction; expectedOld "" means that ref must not exist, so that the
// call creates it. When ref is elsewhere, exists though expected absent, or
// is locked by another writer, nothing changes and the error matches
// ErrRefMoved; any other error is git's.
func (r *Repo) CompareAndSwapRef(ctx context.Context, ref, newID, expectedOld string) error {
	// An empty old value is git's own "must not exist yet".
	_, err := r.git(ctx, "update-ref", ref, newID, expectedOld)
	if err == nil || ctx.Err() != nil {
		return err
	}
	current, found, readErr := r.peek(ctx, ref)
	moved := readErr == nil && (found != (expectedOld != "") || current != expectedOld)
	if moved || strings.Contains(err.Error(), ".lock': File exists") {
		return fmt.Errorf("move %s: %w", ref, ErrRefMoved)
	}
	return err
}

// IsAncestor reports whether the commit ancestor is reachable from the
// commit descendant, which a commit is from itself.
func (r *Repo) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	_, err := r.git(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if exitCode(err) == 1 {
		return false, nil
	}
	return err == nil, err
}

// Tags maps every tag of the repository to the object id it points at: the
// tag object of an annotated tag, the commit of a lightweight one.
func (r *Repo) Tags(ctx context.Context) (map[string]string, error) {
	return r.refs(ctx, "refs/tags/")
}

// peek is the object id ref points at, whatever its type; found is false
// when there is no such ref.
func (r *Repo) peek(ctx context.Context, ref string) (string, bool, error) {
	out, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
	if exitCode(err) == 1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(out)), true, nil
}

// deleteUnder removes every ref below prefix, all in one transaction of
// git's.
func (r *Repo) deleteUnder(ctx context.Context, prefix string) error {
	out, err := r.git(ctx, "for-each-ref", "--format=delete %(refname)", prefix)
	if err != nil || len(out) == 0 {
		return err
	}
	_, err = r.g.runInput(ctx, access{}, out, "--git-dir="+r.dir, "update-ref", "--stdin")
	return err
}
