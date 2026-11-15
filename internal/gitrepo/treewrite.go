package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The kinds of a file operation.
const (
	// OpPut writes Content to the file Path with Mode.
	OpPut = "put"
	// OpDelete removes the file Path.
	OpDelete = "delete"
	// OpMove renames the file From to To, keeping its content and mode.
	OpMove = "move"
	// OpChmod sets the mode of the file Path to Mode, keeping its content.
	OpChmod = "chmod"
)

// The modes of a file an operation writes.
const (
	ModeFile       = "100644"
	ModeExecutable = "100755"
	ModeSymlink    = "120000"
	modeSubmodule  = "160000"
)

// Operation is one change to the files of a tree. Each path is relative to
// the root of the tree, its segments separated by "/".
type Operation struct {
	// Op is OpPut, OpDelete, OpMove or OpChmod.
	Op string
	// Path is the file a put, delete or chmod changes.
	Path string
	// From and To are the file a move renames and its new path.
	From, To string
	// Content is the bytes a put writes: a symlink's target for
	// ModeSymlink.
	Content []byte
	// Mode is the mode a put writes, ModeFile, ModeExecutable or
	// ModeSymlink, "" being ModeFile; and the mode a chmod sets, ModeFile
	// or ModeExecutable.
	Mode string
}

// The reasons an operation does not apply, which an OperationError wraps.
var (
	// ErrInvalidOperation is an operation of no known kind, a path that is
	// empty, absolute or has an empty, ".", ".." or ".git" segment, a
	// missing field or a mode the operation does not take.
	ErrInvalidOperation = errors.New("the operation is malformed")
	// ErrNoFile is a delete, move or chmod of a path that is no file: it
	// is missing or a directory.
	ErrNoFile = errors.New("no file at the path")
	// ErrPathExists is a move onto a file or a directory.
	ErrPathExists = errors.New("the path exists")
	// ErrUnderFile is a path one of whose parent directories is a file.
	ErrUnderFile = errors.New("a parent directory of the path is a file")
	// ErrIsDirectory is a put onto a directory.
	ErrIsDirectory = errors.New("the path is a directory")
	// ErrSubmodule is any operation on a submodule entry or below one.
	ErrSubmodule = errors.New("the path is a submodule or lies in one")
)

// OperationError is the error of the first operation of a list that does
// not apply to the tree the operations before it left.
type OperationError struct {
	// Index is the operation's zero-based position in the list.
	Index int
	// Path is the path the operation could not apply to.
	Path string
	// Err is the reason, one of the errors an OperationError wraps.
	Err error
}

func (e *OperationError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("operation %d: %v", e.Index, e.Err)
	}
	return fmt.Sprintf("operation %d: %s: %v", e.Index, e.Path, e.Err)
}

func (e *OperationError) Unwrap() error { return e.Err }

// ApplyOperations applies ops, in their order, each to the tree the one
// before it left, to the tree of the commit of the full id parent, and
// returns the id of the tree they leave: the parent's own tree when they
// change nothing. A directory is made for a file put below it and goes
// with its last file. When an operation does not apply, the error is an
// *OperationError naming the first such operation, and no tree is
// returned.
//
// The tree is built in a temporary index file of the call's own, removed
// before it returns, never in the repository's index or a work tree. The
// blobs and trees written stay in the repository whether or not a commit
// refers to them; git's garbage collection removes those no ref reaches.
func (r *Repo) ApplyOperations(ctx context.Context, parent string, ops []Operation) (string, error) {
	base, err := r.parentTree(ctx, parent)
	if err != nil {
		return "", err
	}
	files, err := r.treeFiles(ctx, base)
	if err != nil {
		return "", err
	}
	s := newTreeState(files)
	for i, op := range ops {
		if path, err := s.apply(op); err != nil {
			return "", &OperationError{Index: i, Path: path, Err: err}
		}
	}

	dir, err := os.MkdirTemp("", "parley-tree-")
	if err != nil {
		return "", fmt.Errorf("make the temporary index's directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := r.writeContents(ctx, dir, s.files); err != nil {
		return "", err
	}
	info := indexInfo(files, s.files)
	if len(info) == 0 {
		return base, nil
	}
	g := r.g
	g.Env = append(slices.Clip(g.Env), "GIT_INDEX_FILE="+filepath.Join(dir, "index"))
	if _, err := g.run(ctx, access{}, "--git-dir="+r.dir, "read-tree", base); err != nil {
		return "", err
	}
	if _, err := g.runInput(ctx, access{}, info, "--git-dir="+r.dir, "update-index", "-z", "--index-info"); err != nil {
		return "", err
	}
	out, err := g.run(ctx, access{}, "--git-dir="+r.dir, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// parentTree is the tree of the commit of the full id parent.
func (r *Repo) parentTree(ctx context.Context, parent string) (string, error) {
	if len(parent) != idLen || !isHexID(parent, idLen) {
		return "", fmt.Errorf("commit %q: %w", parent, ErrNotFound)
	}
	lines, err := r.batchCheck(ctx, "%(objecttype) %(objectname)", []string{parent, parent + "^{tree}"})
	if err != nil {
		return "", err
	}
	switch {
	case len(lines) != 2:
		return "", fmt.Errorf("tree of %s: cat-file answered %q", parent, lines)
	case strings.HasSuffix(lines[0], " missing"):
		return "", fmt.Errorf("commit %s: %w", parent, ErrNotFound)
	case !strings.HasPrefix(lines[0], ObjectCommit+" "):
		return "", fmt.Errorf("commit %s: %w", parent, ErrNotCommit)
	}
	tree, ok := strings.CutPrefix(lines[1], ObjectTree+" ")
	if !ok {
		return "", fmt.Errorf("tree of %s: cat-file answered %q", parent, lines[1])
	}
	return tree, nil
}

// treeFiles are the entries other than directories of every directory of
// tree, by path.
func (r *Repo) treeFiles(ctx context.Context, tree string) (map[string]stagedFile, error) {
	out, err := r.git(ctx, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, err
	}
	files := map[string]stagedFile{}
	for rec := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("ls-tree answered %q", rec)
		}
		files[path] = stagedFile{mode: f[0], id: f[2]}
	}
	return files, nil
}

// writeContents writes the content of every file of files that has no id
// yet as a blob, in one git process, and sets its id.
func (r *Repo) writeContents(ctx context.Context, dir string, files map[string]*stagedFile) error {
	var pending []*stagedFile
	var paths strings.Builder
	for _, f := range files {
		if f.id != "" {
			continue
		}
		name := filepath.Join(dir, "blob-"+strconv.Itoa(len(pending)))
		if err := os.WriteFile(name, f.content, 0o600); err != nil {
			return fmt.Errorf("write a file's content: %w", err)
		}
		pending = append(pending, f)
		paths.WriteString(name + "\n")
	}
	if len(pending) == 0 {
		return nil
	}
	// --no-filters writes the bytes as they are, whatever attributes the
	// file names would match.
	out, err := r.g.runInput(ctx, access{}, []byte(paths.String()),
		"--git-dir="+r.dir, "hash-object", "-w", "--no-filters", "--stdin-paths")
	if err != nil {
		return err
	}
	ids := strings.Fields(string(out))
	if len(ids) != len(pending) {
		return fmt.Errorf("hash-object answered %q for %d files", out, len(pending))
	}
	for i, f := range pending {
		f.id, f.content = ids[i], nil
	}
	return nil
}

// indexInfo is what git update-index -z --index-info takes to turn the
// files base into the files final: first the removals, so that a file
// that takes the place of a directory, or a directory of a file, finds the
// path free.
func indexInfo(base map[string]stagedFile, final map[string]*stagedFile) []byte {
	var removed, set []string
	for path := range base {
		if final[path] == nil {
			removed = append(removed, path)
		}
	}
	for path, f := range final {
		if b, ok := base[path]; !ok || b.mode != f.mode || b.id != f.id {
			set = append(set, path)
		}
	}
	slices.Sort(removed)
	slices.Sort(set)
	var info strings.Builder
	for _, path := range removed {
		info.WriteString("0 " + nullID + "\t" + path + "\x00")
	}
	for _, path := range set {
		info.WriteString(final[path].mode + " " + final[path].id + "\t" + path + "\x00")
	}
	return []byte(info.String())
}

// stagedFile is a file of a tree being built: its mode, and its blob's id
// or, before it is written, its content.
type stagedFile struct {
	mode    string
	id      string
	content []byte
}

// treeState is the files of a tree being built and its directories.
type treeState struct {
	files map[string]*stagedFile
	// dirs counts, for each directory, the files below it.
	dirs map[string]int
}

func newTreeState(base map[string]stagedFile) *treeState {
	s := &treeState{files: make(map[string]*stagedFile, len(base)), dirs: map[string]int{}}
	for path, f := range base {
		s.add(path, &f)
	}
	return s
}

// apply applies op, or returns the path it does not apply to and why.
func (s *treeState) apply(op Operation) (string, error) {
	switch op.Op {
	case OpPut:
		mode := op.Mode
		if mode == "" {
			mode = ModeFile
		}
		if !ValidPath(op.Path) || (mode != ModeFile && mode != ModeExecutable && mode != ModeSymlink) {
			return op.Path, ErrInvalidOperation
		}
		if err := s.placeable(op.Path); err != nil {
			return op.Path, err
		}
		if s.files[op.Path] != nil {
			s.remove(op.Path)
		}
		s.add(op.Path, &stagedFile{mode: mode, content: op.Content})
	case OpDelete:
		if !ValidPath(op.Path) {
			return op.Path, ErrInvalidOperation
		}
		if _, err := s.file(op.Path); err != nil {
			return op.Path, err
		}
		s.remove(op.Path)
	case OpChmod:
		if !ValidPath(op.Path) || (op.Mode != ModeFile && op.Mode != ModeExecutable) {
			return op.Path, ErrInvalidOperation
		}
		f, err := s.file(op.Path)
		if err != nil {
			return op.Path, err
		}
		f.mode = op.Mode
	case OpMove:
		if !ValidPath(op.From) || !ValidPath(op.To) {
			return op.From, ErrInvalidOperation
		}
		f, err := s.file(op.From)
		if err != nil {
			return op.From, err
		}
		switch {
		case s.inSubmodule(op.To):
			return op.To, ErrSubmodule
		case s.files[op.To] != nil || s.dirs[op.To] > 0:
			return op.To, ErrPathExists
		case s.underFile(op.To):
			return op.To, ErrUnderFile
		}
		s.remove(op.From)
		s.add(op.To, f)
	default:
		return op.Path, ErrInvalidOperation
	}
	return "", nil
}

// file is the file at path, which must be one and no submodule.
func (s *treeState) file(path string) (*stagedFile, error) {
	switch f := s.files[path]; {
	case s.inSubmodule(path):
		return nil, ErrSubmodule
	case f == nil:
		return nil, ErrNoFile
	default:
		return f, nil
	}
}

// placeable is nil when a file can be put at path: no directory is there,
// and none of its parent directories is a file or a submodule. path
// itself may be a file, which the put replaces, but no submodule.
//
// git update-index replaces a file by a directory, or a directory by a
// file, without a word, so these checks are the applier's own.
func (s *treeState) placeable(path string) error {
	switch {
	case s.inSubmodule(path):
		return ErrSubmodule
	case s.underFile(path):
		return ErrUnderFile
	case s.dirs[path] > 0:
		return ErrIsDirectory
	}
	return nil
}

// inSubmodule is whether path or one of its parent directories is a
// submodule entry.
func (s *treeState) inSubmodule(path string) bool {
	if f := s.files[path]; f != nil && f.mode == modeSubmodule {
		return true
	}
	for dir := range parents(path) {
		if f := s.files[dir]; f != nil && f.mode == modeSubmodule {
			return true
		}
	}
	return false
}

// underFile is whether one of path's parent directories is a file.
func (s *treeState) underFile(path string) bool {
	for dir := range parents(path) {
		if s.files[dir] != nil {
			return true
		}
	}
	return false
}

func (s *treeState) add(path string, f *stagedFile) {
	s.files[path] = f
	for dir := range parents(path) {
		s.dirs[dir]++
	}
}

func (s *treeState) remove(path string) {
	delete(s.files, path)
	for dir := range parents(path) {
		if s.dirs[dir]--; s.dirs[dir] == 0 {
			delete(s.dirs, dir)
		}
	}
}

// parents yields the parent directories of path, the outermost first.
func parents(path string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for i := range len(path) {
			if path[i] == '/' && !yield(path[:i]) {
				return
			}
		}
	}
}

// ValidPath reports whether path can name a file of a tree: not empty, not
// absolute, no empty, ".", ".." or ".git" segment, the last in any case
// since git refuses it so on a file system that ignores case, and no NUL.
func ValidPath(path string) bool {
	if path == "" || strings.ContainsRune(path, 0) {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return false
		}
	}
	return true
}

// HubName and HubEmail are the identity of the hub, the committer of every
// commit it makes and the author of those no user writes.
const (
	HubName  = "parley"
	HubEmail = "parley@localhost"
)

// HubSignature is the hub's identity at the time at.
func HubSignature(at time.Time) Signature {
	return Signature{Name: HubName, Email: HubEmail, Time: at}
}

// CommitSpec is what a new commit holds. Every part of it is given:
// nothing is taken from the environment or a git configuration.
type CommitSpec struct {
	// Tree is the full id of the commit's tree.
	Tree string
	// Parents are the full ids of its parent commits, the first parent
	// first, none for a root commit.
	Parents []string
	// Message is the commit's message, ended with a newline when it is not.
	Message string
	// Author and Committer are its signatures, each with its time and
	// that time's offset. The characters <, > and newline are left out of
	// a name or an e-mail address; a name with nothing else, or a zero
	// time, is an error.
	Author, Committer Signature
}

// CreateCommit writes the commit spec describes and returns its id. It
// moves no ref. A tree or a parent that names no tree or no commit is an
// error.
func (r *Repo) CreateCommit(ctx context.Context, spec CommitSpec) (string, error) {
	author, err := signatureLine(spec.Author)
	if err != nil {
		return "", fmt.Errorf("author: %w", err)
	}
	committer, err := signatureLine(spec.Committer)
	if err != nil {
		return "", fmt.Errorf("committer: %w", err)
	}
	names := append([]string{spec.Tree}, spec.Parents...)
	for _, id := range names {
		if len(id) != idLen || !isHexID(id, idLen) {
			return "", fmt.Errorf("object %q: %w", id, ErrNotFound)
		}
	}
	types, err := r.batchCheck(ctx, "%(objecttype)", names)
	if err != nil {
		return "", err
	}
	if len(types) != len(names) {
		return "", fmt.Errorf("cat-file answered %q", types)
	}
	for i, typ := range types {
		want := ObjectCommit
		if i == 0 {
			want = ObjectTree
		}
		if typ != want {
			return "", fmt.Errorf("%s is no %s: %s", names[i], want, typ)
		}
	}

	var obj strings.Builder
	obj.WriteString("tree " + strings.ToLower(spec.Tree) + "\n")
	for _, p := range spec.Parents {
		obj.WriteString("parent " + strings.ToLower(p) + "\n")
	}
	obj.WriteString("author " + author + "\ncommitter " + committer + "\n\n" + spec.Message)
	if spec.Message != "" && !strings.HasSuffix(spec.Message, "\n") {
		obj.WriteString("\n")
	}
	out, err := r.g.runInput(ctx, access{}, []byte(obj.String()), "--git-dir="+r.dir, "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// signatureLine is s as a commit's author or committer line writes it.
func signatureLine(s Signature) (string, error) {
	clean := func(v string) string {
		return strings.TrimSpace(strings.Map(func(c rune) rune {
			if c == '<' || c == '>' || c == '\n' || c == 0 {
				return -1
			}
			return c
		}, v))
	}
	name := clean(s.Name)
	switch {
	case name == "":
		return "", fmt.Errorf("no name in %q", s.Name)
	case s.Time.IsZero():
		return "", errors.New("no time")
	}
	return fmt.Sprintf("%s <%s> %d %s", name, clean(s.Email), s.Time.Unix(), s.Time.Format("-0700")), nil
}
