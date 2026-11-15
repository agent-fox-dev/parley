package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrRefMoved is the error of a compare-and-swap that found the ref at
// another value than the one it expected, or held by another writer that is
// moving it at that moment. The ref is left as the other writer has it; the
// caller reads it again and decides anew.
var ErrRefMoved = errors.New("the ref is not at the expected value")

// ErrRefNameConflict is the error of a compare-and-swap that would create a
// ref whose name another ref's name is a directory of, or that is itself a
// directory of another ref's name, such as refs/heads/feat/x beside
// refs/heads/feat: git keeps both names in one tree of ref files and cannot
// hold the two. Nothing changes.
var ErrRefNameConflict = errors.New("another ref is in the way of the name")

// CompareAndSwapRef points ref, a full ref name such as refs/heads/main, at
// newID only if it points at expectedOld now, as one atomic step of git's
// ref transaction; expectedOld "" means that ref must not exist, so that the
// call creates it. When ref is elsewhere, exists though expected absent, or
// is locked by another writer, nothing changes and the error matches
// ErrRefMoved; when ref is to be created and another ref's name is in the
// way of its name, nothing changes and the error matches
// ErrRefNameConflict; any other error is git's.
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
	if expectedOld == "" {
		// The refs are read rather than git's message matched, whose wording
		// changes between versions.
		if _, blocked, blockErr := r.RefInTheWay(ctx, ref); blockErr == nil && blocked {
			return fmt.Errorf("create %s: %w", ref, ErrRefNameConflict)
		}
	}
	return err
}

// SetRef points ref, a full ref name, at the commit id, whatever it
// points at now and whether or not it exists: for a ref only the hub
// writes, such as a change's version ref, where a value left by an earlier
// write that was undone is to be overwritten.
func (r *Repo) SetRef(ctx context.Context, ref, id string) error {
	_, err := r.git(ctx, "update-ref", "--no-deref", ref, id)
	return err
}

// DeleteRef removes ref, a full ref name, whatever it points at: for a
// ref only the hub writes, such as a change's version ref, which a write
// that was then refused must not leave behind. A ref that does not exist
// is no error.
func (r *Repo) DeleteRef(ctx context.Context, ref string) error {
	_, err := r.git(ctx, "update-ref", "--no-deref", "-d", ref)
	return err
}

// SetRefFrom is SetRef for a commit that may be missing from the
// repository and that the repository at the git directory src holds, as a
// fork holds its own commits apart from its parent: the commit and what it
// reaches that the repository lacks are copied from src first. A commit the
// repository holds already is pointed at without a copy.
func (r *Repo) SetRefFrom(ctx context.Context, src, ref, id string) error {
	missing, err := r.Missing(ctx, []string{id})
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return r.SetRef(ctx, ref, id)
	}
	// A push from src sends exactly what this repository lacks and sets
	// the ref in one step; a fetch by id would need src to allow wants of
	// unadvertised objects.
	_, err = r.g.run(ctx, access{}, "--git-dir="+src, "push", "--quiet", "--no-verify", "--", r.dir, "+"+id+":"+ref)
	return err
}

// RefInTheWay is the ref whose name keeps ref from being created: one
// whose name is a directory of ref's, such as refs/heads/feat for
// refs/heads/feat/x, or one that lies below ref's name, such as
// refs/heads/topic/a for refs/heads/topic; the first by name when there
// are several. found is false when no ref is in the way.
func (r *Repo) RefInTheWay(ctx context.Context, ref string) (name string, found bool, err error) {
	parts := strings.Split(ref, "/")
	var dirs []string
	// refs/heads and the like are namespaces, never refs.
	for i := 3; i < len(parts); i++ {
		dirs = append(dirs, strings.Join(parts[:i], "/"))
	}
	// A pattern matches its own name and every ref below it.
	out, err := r.git(ctx, append([]string{"for-each-ref", "--format=%(refname)", ref}, dirs...)...)
	if err != nil {
		return "", false, err
	}
	for name := range strings.Lines(string(out)) {
		name = strings.TrimRight(name, "\n")
		if strings.HasPrefix(name, ref+"/") || slices.Contains(dirs, name) {
			return name, true, nil
		}
	}
	return "", false, nil
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

// RefValues maps each of refs, full ref names, that exists to the object id
// it points at; a ref that does not exist is absent from the map.
func (r *Repo) RefValues(ctx context.Context, refs []string) (map[string]string, error) {
	values := map[string]string{}
	if len(refs) == 0 {
		return values, nil
	}
	// for-each-ref matches a pattern as a prefix of whole name components,
	// so only the exact names are kept.
	out, err := r.git(ctx, append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, refs...)...)
	if err != nil {
		return nil, err
	}
	for line := range strings.Lines(string(out)) {
		if id, ref, ok := strings.Cut(strings.TrimRight(line, "\n"), " "); ok && slices.Contains(refs, ref) {
			values[ref] = id
		}
	}
	return values, nil
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
