package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The errors of a read that names nothing it can answer for. Each is
// distinct, so that a caller can tell an unknown name from one that names
// several objects or an object of the wrong type.
var (
	// ErrNotFound is the error of a name that names no object or ref of the
	// repository, or that is not in a form the read takes.
	ErrNotFound = errors.New("no such object or ref")
	// ErrAmbiguous is the error of an abbreviated id that more than one
	// commit's id begins with.
	ErrAmbiguous = errors.New("the abbreviated id names more than one commit")
	// ErrNotCommit is the error of a name whose object is not a commit and
	// leads to none through annotated tags.
	ErrNotCommit = errors.New("the object is not a commit")
)

// Ref is a ref of the repository.
type Ref struct {
	// Name is the full ref name, such as refs/heads/main.
	Name string
	// ID is the object the ref points at: the tag object of an annotated
	// tag.
	ID string
	// Commit is the commit ID reaches through annotated tags, ID itself for
	// a commit, "" when it reaches none, such as a tag of a tree.
	Commit string
}

// Object is what the repository holds under a name.
type Object struct {
	// ID is the object's full id.
	ID string
	// Type is ObjectCommit, ObjectTree, ObjectBlob or "tag".
	Type string
	// Size is the object's size in bytes, as git stores it uncompressed.
	Size int64
}

// idLen is the length of a full object id written in hexadecimal.
const idLen = 40

// minAbbrev is the shortest abbreviated id the repository takes.
const minAbbrev = 4

// ListRefs lists, in name order, every ref whose name begins with one of
// prefixes, each a full ref name prefix that ends in "/" such as
// refs/heads/; with no prefix, every ref of the repository. A prefix of
// another form is an error.
func (r *Repo) ListRefs(ctx context.Context, prefixes ...string) ([]Ref, error) {
	for _, p := range prefixes {
		if !strings.HasSuffix(p, "/") || (p != "refs/" && !validRefName(strings.TrimSuffix(p, "/"))) {
			return nil, fmt.Errorf("list refs: %q is not a ref prefix", p)
		}
	}
	return r.listRefs(ctx, prefixes, func(string) bool { return true })
}

// ResolveRef is the commit the ref of the full name name reaches through
// annotated tags. A name git would not take for a ref, or that no ref has,
// is ErrNotFound and runs no git for a name of the first kind; a ref that
// reaches no commit is ErrNotCommit.
func (r *Repo) ResolveRef(ctx context.Context, name string) (string, error) {
	if !validRefName(name) {
		return "", fmt.Errorf("ref %q: %w", name, ErrNotFound)
	}
	// for-each-ref takes the name as a pattern, which also matches the refs
	// below it; rev-parse would also try other names that end in it.
	refs, err := r.listRefs(ctx, []string{name}, func(n string) bool { return n == name })
	switch {
	case err != nil:
		return "", err
	case len(refs) == 0:
		return "", fmt.Errorf("ref %s: %w", name, ErrNotFound)
	case refs[0].Commit == "":
		return "", fmt.Errorf("ref %s: %w", name, ErrNotCommit)
	}
	return refs[0].Commit, nil
}

// ResolveCommit is the full id of the one commit whose id begins with hex,
// a full or abbreviated id of 4 to 40 hexadecimal characters, whether or not
// a ref reaches it. hex names objects only, never a ref: it is ErrNotFound
// when no object's id begins with it or it is not of that form (then
// without running git), ErrAmbiguous when several commits' ids do, and
// ErrNotCommit when only objects of other types' ids do, an annotated tag
// included.
func (r *Repo) ResolveCommit(ctx context.Context, hex string) (string, error) {
	if !isHexID(hex, minAbbrev) {
		return "", fmt.Errorf("commit %q: %w", hex, ErrNotFound)
	}
	hex = strings.ToLower(hex)
	// rev-parse's own lookup of an abbreviated id would take a ref of that
	// name first; --disambiguate lists the objects alone.
	out, err := r.git(ctx, "rev-parse", "--disambiguate="+hex)
	if err != nil {
		return "", err
	}
	objects, err := r.objectTypes(ctx, strings.Fields(string(out)))
	if err != nil {
		return "", err
	}
	var commits []string
	for _, o := range objects {
		if o.Type == ObjectCommit {
			commits = append(commits, o.ID)
		}
	}
	switch {
	case len(commits) > 1:
		return "", fmt.Errorf("commit %s: %w", hex, ErrAmbiguous)
	case len(commits) == 1:
		return commits[0], nil
	case len(objects) > 0:
		return "", fmt.Errorf("commit %s: %w", hex, ErrNotCommit)
	}
	return "", fmt.Errorf("commit %s: %w", hex, ErrNotFound)
}

// PeelCommit is the commit the object of the full id id reaches through
// annotated tags, id itself for a commit. An id that is not a full id, or
// that names no object, is ErrNotFound; an object that reaches no commit is
// ErrNotCommit.
func (r *Repo) PeelCommit(ctx context.Context, id string) (string, error) {
	if len(id) != idLen || !isHexID(id, idLen) {
		return "", fmt.Errorf("object %q: %w", id, ErrNotFound)
	}
	id = strings.ToLower(id)
	// cat-file answers each line on its own, so one run tells a missing
	// object from one that peels to no commit.
	out, err := r.batchCheck(ctx, "%(objectname)", []string{id, id + "^{commit}"})
	if err != nil {
		return "", err
	}
	switch {
	case len(out) != 2:
		return "", fmt.Errorf("peel %s: cat-file answered %q", id, out)
	case strings.HasSuffix(out[0], " missing"):
		return "", fmt.Errorf("object %s: %w", id, ErrNotFound)
	case strings.HasSuffix(out[1], " missing"):
		return "", fmt.Errorf("object %s: %w", id, ErrNotCommit)
	}
	return out[1], nil
}

// ObjectInfo is the object name names: a full id, or a full id, a colon and
// the path of an entry of that commit's or tree's tree, which git takes
// literally. A name of another form is ErrNotFound without running git, as
// is a name that names nothing.
func (r *Repo) ObjectInfo(ctx context.Context, name string) (Object, error) {
	id, _, _ := strings.Cut(name, ":")
	if len(id) != idLen || !isHexID(id, idLen) || strings.ContainsAny(name, "\n\x00") {
		return Object{}, fmt.Errorf("object %q: %w", name, ErrNotFound)
	}
	objects, err := r.objectTypes(ctx, []string{name})
	if err != nil {
		return Object{}, err
	}
	if len(objects) == 0 {
		return Object{}, fmt.Errorf("object %q: %w", name, ErrNotFound)
	}
	return objects[0], nil
}

// objectTypes is the type and size of each of names, in their order, the
// names that name no one object left out.
func (r *Repo) objectTypes(ctx context.Context, names []string) ([]Object, error) {
	if len(names) == 0 {
		return nil, nil
	}
	lines, err := r.batchCheck(ctx, "%(objectname) %(objecttype) %(objectsize)", names)
	if err != nil {
		return nil, err
	}
	var objects []Object
	for i, line := range lines {
		if i < len(names) && (line == names[i]+" missing" || line == names[i]+" ambiguous") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("cat-file answered %q", line)
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cat-file answered %q", line)
		}
		objects = append(objects, Object{ID: f[0], Type: f[1], Size: size})
	}
	return objects, nil
}

// batchCheck runs git cat-file --batch-check with format on names, given on
// its standard input so that git reads none of them as an option, and
// returns its lines.
func (r *Repo) batchCheck(ctx context.Context, format string, names []string) ([]string, error) {
	out, err := r.g.runInput(ctx, access{}, []byte(strings.Join(names, "\n")+"\n"),
		"--git-dir="+r.dir, "cat-file", "--batch-check="+format)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"), nil
}

// listRefs lists, in name order, the refs for-each-ref matches with
// patterns that keep accepts.
func (r *Repo) listRefs(ctx context.Context, patterns []string, keep func(name string) bool) ([]Ref, error) {
	// %(*objectname) is the object an annotated tag leads to, empty for any
	// other ref.
	args := append([]string{"for-each-ref", "--format=%(objectname) %(objecttype) %(*objectname) %(*objecttype) %(refname)"}, patterns...)
	out, err := r.git(ctx, args...)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for line := range strings.Lines(string(out)) {
		f := strings.SplitN(strings.TrimRight(line, "\n"), " ", 5)
		if len(f) != 5 || !keep(f[4]) {
			continue
		}
		ref := Ref{Name: f[4], ID: f[0]}
		switch {
		case f[1] == ObjectCommit:
			ref.Commit = f[0]
		case f[3] == ObjectCommit:
			ref.Commit = f[2]
		case f[1] == "tag" && f[3] == "tag":
			// A tag of a tag, which an older git dereferences one step only.
			if ref.Commit, err = r.PeelCommit(ctx, f[0]); errors.Is(err, ErrNotCommit) {
				err = nil
			}
			if err != nil {
				return nil, err
			}
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// isHexID reports whether s is min to 40 hexadecimal characters.
func isHexID(s string, min int) bool {
	if len(s) < min || len(s) > idLen {
		return false
	}
	for i := range len(s) {
		if !isHex(s[i]) {
			return false
		}
	}
	return true
}

// validRefName reports whether name is a full ref name git takes for a
// ref, by the rules of git check-ref-format, below refs/: no component
// that begins with "." or ends with ".lock", no "..", "@{", "//", control
// character, space or any of ~^:?*[\, not ending in "/" or ".".
func validRefName(name string) bool {
	rest, ok := strings.CutPrefix(name, "refs/")
	if !ok || rest == "" || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	for _, c := range []byte(name) {
		if c < 0x20 || c == 0x7f || strings.IndexByte(" ~^:?*[\\", c) >= 0 {
			return false
		}
	}
	for comp := range strings.SplitSeq(rest, "/") {
		if comp == "" || strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return false
		}
	}
	return true
}
