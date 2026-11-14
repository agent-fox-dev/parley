package gitrepo

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// RefCopy names the refs CopyRefs brings from one repository into another.
// A ref of the destination that it does not name is left alone.
type RefCopy struct {
	// Names are full ref names, such as refs/heads/main: each is set in the
	// destination to the source's value, or deleted from the destination
	// when the source has no such ref.
	Names []string
	// Prefixes are namespaces, each ending in "/", such as
	// refs/parley/changes/7/: every ref of the source below one is set in
	// the destination.
	Prefixes []string
	// Prune is a namespace of the destination, ending in "/": every ref
	// below it that the copy does not set is deleted. "" deletes nothing
	// but the Names the source lacks.
	Prune string
}

// Copied is what CopyRefs changed in the destination, each list sorted.
type Copied struct {
	// Set are the refs it created or moved.
	Set []string
	// Deleted are the refs it removed.
	Deleted []string
}

// CopyRefs makes the refs c names in the repository at dst what they are in
// the repository at src, both git directories on this host: a ref whose
// value differs is set with a forced local fetch, all of them in one atomic
// transaction, and the refs c says go are deleted, in one transaction of
// their own, before it, so that a deleted name is never in the way of a new
// one. A ref already at the source's value is not written, so that a copy
// that changes nothing writes nothing. When dst borrows src's objects
// through its alternates, as a fork does, the fetch transfers no object.
//
// An error leaves dst's refs as they were or with the deletions alone
// made; a later copy completes it. Two copies into one dst at once may get
// in each other's way on a ref's lock, and the caller keeps them apart.
func (g Git) CopyRefs(ctx context.Context, src, dst string, c RefCopy) (Copied, error) {
	from, err := g.Open(ctx, src)
	if err != nil {
		return Copied{}, err
	}
	into, err := g.Open(ctx, dst)
	if err != nil {
		return Copied{}, err
	}
	have, err := from.named(ctx, c.Names, c.Prefixes)
	if err != nil {
		return Copied{}, fmt.Errorf("read the refs of %s: %w", src, err)
	}
	var prune []string
	if c.Prune != "" {
		prune = []string{c.Prune}
	}
	had, err := into.named(ctx, c.Names, prune)
	if err != nil {
		return Copied{}, fmt.Errorf("read the refs of %s: %w", dst, err)
	}
	var done Copied
	for name, id := range have {
		if had[name] != id {
			done.Set = append(done.Set, name)
		}
	}
	for name := range had {
		if _, kept := have[name]; !kept {
			done.Deleted = append(done.Deleted, name)
		}
	}
	slices.Sort(done.Set)
	slices.Sort(done.Deleted)
	if len(done.Deleted) > 0 {
		var stdin strings.Builder
		for _, name := range done.Deleted {
			fmt.Fprintf(&stdin, "delete %s\n", name)
		}
		if _, err := g.runInput(ctx, access{}, []byte(stdin.String()), "--git-dir="+into.dir, "update-ref", "--stdin"); err != nil {
			return Copied{}, err
		}
	}
	if len(done.Set) > 0 {
		args := []string{"--git-dir=" + into.dir, "fetch", "--quiet", "--atomic", "--no-tags",
			"--no-write-fetch-head", "--no-auto-maintenance", "--", from.dir}
		for _, name := range done.Set {
			args = append(args, "+"+name+":"+name)
		}
		if _, err := g.run(ctx, access{}, args...); err != nil {
			return Copied{}, err
		}
	}
	return done, nil
}

// named maps each ref of the repository that is one of names or lies below
// one of prefixes to the object id it points at.
func (r *Repo) named(ctx context.Context, names, prefixes []string) (map[string]string, error) {
	refs := map[string]string{}
	if len(names)+len(prefixes) == 0 {
		// for-each-ref without a pattern lists every ref.
		return refs, nil
	}
	// A pattern also matches the refs below its name; only the exact names
	// are kept.
	args := append(append([]string{"for-each-ref", "--format=%(objectname) %(refname)", "--"}, names...), prefixes...)
	out, err := r.git(ctx, args...)
	if err != nil {
		return nil, err
	}
	for line := range strings.Lines(string(out)) {
		id, name, ok := strings.Cut(strings.TrimRight(line, "\n"), " ")
		below := slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
		if ok && (below || slices.Contains(names, name)) {
			refs[name] = id
		}
	}
	return refs, nil
}
