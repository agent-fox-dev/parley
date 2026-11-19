package gitrepo

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"container/heap"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The reader of this file reads commits from git's object store in process
// and walks their history as git merge-base and rev-list do, for the
// questions the hub asks of a repository thousands of times, each of which
// would otherwise start a git process: IsAncestor, MergeBase,
// CommitsBetween, ReadCommits and the commits a rebase replays. Like the
// readers of objectfile.go and refsfile.go, its ok is false for what it does
// not read the way git does, and its caller asks git then: an object it does
// not find in the repository's object store or its alternates, a pack index
// other than version 2, a repository whose history git reads otherwise (a
// shallow one, grafts, replace refs), a git environment that changes where
// or how git reads objects in a way it does not follow, a walk of more than
// maxWalkCommits commits, and an answer that only git's own order settles:
// several merge bases, or a range that holds a merge commit.

const (
	// maxWalkCommits bounds the commits one walk reads; a longer walk is
	// left to git, which may have a commit-graph to answer it with.
	maxWalkCommits = 2048
	// maxCommitSize bounds an object read, delta or result, and
	// maxDeltaBytes the deltas of one object together; a commit is a few
	// hundred bytes.
	maxCommitSize = 1 << 20
	maxDeltaBytes = 8 << 20
	// maxAlternateDepth is how deep git follows the alternates of
	// alternates.
	maxAlternateDepth = 5
)

// commitReader reads the commits of one git directory for one call.
type commitReader struct {
	objects []string // object directories: the repository's, then its alternates
	indexes []string // pack indexes of objects, found on the first packed read
	packs   []*packFiles
	globbed bool
	commits map[string]*Commit
}

// packFiles are the open index and pack of one pack.
type packFiles struct{ index, pack *os.File }

// newCommitReader is the reader of the commits of the git directory dir as
// a git process with the environment entries env reads them; ok is false
// when it would not read them the way git does.
func newCommitReader(dir string, env []string) (*commitReader, bool) {
	if !filesBackend(dir) {
		return nil, false
	}
	for _, name := range []string{"shallow", filepath.Join("info", "grafts"), filepath.Join("refs", "replace")} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			return nil, false
		}
	}
	if replace, ok := readPackedRefs(dir, "refs/replace/"); !ok || len(replace) > 0 {
		return nil, false
	}
	primary := filepath.Join(dir, "objects")
	var alternates []string
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		switch key {
		case "GIT_OBJECT_DIRECTORY":
			primary = value
		case "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			// git reads an entry in double quotes as a C string.
			if strings.Contains(value, `"`) {
				return nil, false
			}
			alternates = append(alternates, filepath.SplitList(value)...)
		case "GIT_DIR", "GIT_COMMON_DIR", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE", "GIT_REPLACE_REF_BASE":
			return nil, false
		}
	}
	r := &commitReader{commits: map[string]*Commit{}}
	for _, objects := range append([]string{primary}, alternates...) {
		if !filepath.IsAbs(objects) || !r.addObjects(objects, 0) {
			return nil, false
		}
	}
	return r, true
}

// addObjects adds the object directory objects and, depth levels deep at
// most, the alternates its info/alternates file names; ok is false for an
// alternates file it does not read the way git does.
func (r *commitReader) addObjects(objects string, depth int) bool {
	objects = filepath.Clean(objects)
	if slices.Contains(r.objects, objects) {
		return true
	}
	r.objects = append(r.objects, objects)
	raw, err := os.ReadFile(filepath.Join(objects, "info", "alternates"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil, depth >= maxAlternateDepth:
		return false
	}
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "", line[0] == '#':
			continue
		case line[0] == '"':
			return false
		case !filepath.IsAbs(line):
			line = filepath.Join(objects, line)
		}
		if !r.addObjects(line, depth+1) {
			return false
		}
	}
	return true
}

// close closes the packs the reader opened.
func (r *commitReader) close() {
	for _, p := range r.packs {
		if p != nil {
			_ = p.index.Close()
			_ = p.pack.Close()
		}
	}
}

// commit is the commit of the full, lower-case id id; ok is false for an
// object that is not a commit, one it does not find or read, and past
// maxWalkCommits commits.
func (r *commitReader) commit(id string) (*Commit, bool) {
	if c, ok := r.commits[id]; ok {
		return c, true
	}
	if len(r.commits) >= maxWalkCommits {
		return nil, false
	}
	kind, data, ok := r.read(id)
	if !ok || kind != packCommit || !gitLayout(data) {
		return nil, false
	}
	c, err := parseCommit(id, data)
	if err != nil {
		return nil, false
	}
	r.commits[id] = &c
	return &c, true
}

// gitLayout reports whether the commit object raw is laid out as git reads
// a commit's tree and parents: its tree line first, then its parent lines,
// and no tree or parent line among the headers after them, each id a full
// lower-case one. git takes a parent line elsewhere for another header,
// which parseCommit does not, and refuses a commit whose first line is not
// its tree.
func gitLayout(raw []byte) bool {
	headers, _, _ := bytes.Cut(raw, []byte("\n\n"))
	lines := strings.Split(string(headers), "\n")
	if !lowerIDLine(lines[0], "tree ") {
		return false
	}
	i := 1
	for ; i < len(lines) && strings.HasPrefix(lines[i], "parent "); i++ {
		if !lowerIDLine(lines[i], "parent ") {
			return false
		}
	}
	return !slices.ContainsFunc(lines[i:], func(l string) bool {
		return strings.HasPrefix(l, "tree ") || strings.HasPrefix(l, "parent ")
	})
}

// lowerIDLine reports whether line is key followed by a full lower-case id.
func lowerIDLine(line, key string) bool {
	id, ok := strings.CutPrefix(line, key)
	return ok && len(id) == idLen && strings.Trim(id, "0123456789abcdef") == ""
}

// read is the kind, as a pack entry gives it, and the content of the object
// id: loose in one of the object directories, or in one of their packs.
func (r *commitReader) read(id string) (kind int, data []byte, ok bool) {
	for _, objects := range r.objects {
		kind, data, found, ok := readLoose(objects, id)
		if found || !ok {
			return kind, data, ok
		}
	}
	raw, err := hex.DecodeString(id)
	if err != nil {
		return 0, nil, false
	}
	if !r.globbed {
		// git writes a pack before its index, and a pack's objects before
		// it removes their loose files, so a pack whose index is listed
		// now is whole, and an object packed since the loose read is in it.
		for _, objects := range r.objects {
			indexes, err := filepath.Glob(filepath.Join(objects, "pack", "pack-*.idx"))
			if err != nil {
				return 0, nil, false
			}
			r.indexes = append(r.indexes, indexes...)
		}
		r.packs, r.globbed = make([]*packFiles, len(r.indexes)), true
	}
	for i, idx := range r.indexes {
		if r.packs[i] == nil {
			index, err := os.Open(idx)
			if err != nil {
				return 0, nil, false
			}
			pack, err := os.Open(strings.TrimSuffix(idx, ".idx") + ".pack")
			if err != nil {
				_ = index.Close()
				return 0, nil, false
			}
			r.packs[i] = &packFiles{index: index, pack: pack}
		}
		offset, found, ok := packOffset(r.packs[i].index, raw)
		if !ok {
			return 0, nil, false
		}
		if found {
			return readPacked(r.packs[i], offset)
		}
	}
	return 0, nil, false
}

// readLoose reads the loose object id below objects; found is false when
// there is no such file.
func readLoose(objects, id string) (kind int, data []byte, found, ok bool) {
	f, err := os.Open(filepath.Join(objects, id[:2], id[2:]))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil, false, true
	}
	if err != nil {
		return 0, nil, false, false
	}
	defer func() { _ = f.Close() }()
	z, err := zlib.NewReader(bufio.NewReader(f))
	if err != nil {
		return 0, nil, false, false
	}
	defer func() { _ = z.Close() }()
	raw, err := io.ReadAll(io.LimitReader(z, maxCommitSize+64))
	if err != nil {
		return 0, nil, false, false
	}
	header, data, cut := bytes.Cut(raw, []byte{0})
	typ, size, _ := strings.Cut(string(header), " ")
	if n, err := strconv.Atoi(size); !cut || err != nil || n != len(data) || n > maxCommitSize {
		return 0, nil, false, false
	}
	kind = map[string]int{ObjectCommit: packCommit, ObjectTree: packTree, ObjectBlob: packBlob, "tag": packTag}[typ]
	return kind, data, true, kind != 0
}

// readPacked is the kind and the content of the pack entry at offset of p,
// its deltas applied.
func readPacked(p *packFiles, offset int64) (kind int, data []byte, ok bool) {
	var deltas [][]byte
	held := int64(0)
	for range maxDeltaChain {
		h, ok := readPackHeader(p.pack, offset)
		if held += h.size; !ok || h.size > maxCommitSize || held > maxDeltaBytes {
			return 0, nil, false
		}
		z, err := zlib.NewReader(bufio.NewReader(io.NewSectionReader(p.pack, h.data, 1<<62)))
		if err != nil {
			return 0, nil, false
		}
		data := make([]byte, h.size)
		_, err = io.ReadFull(z, data)
		_ = z.Close()
		if err != nil {
			return 0, nil, false
		}
		switch h.kind {
		case packCommit, packTree, packBlob, packTag:
			for i := len(deltas) - 1; i >= 0; i-- {
				if data, ok = applyDelta(data, deltas[i]); !ok {
					return 0, nil, false
				}
			}
			return h.kind, data, true
		case packOfsDelta:
			deltas, offset = append(deltas, data), h.base
		case packRefDelta:
			deltas = append(deltas, data)
			var found bool
			if offset, found, ok = packOffset(p.index, h.baseID); !found || !ok {
				return 0, nil, false
			}
		default:
			return 0, nil, false
		}
	}
	return 0, nil, false
}

// applyDelta is the object git's delta delta makes of base.
func applyDelta(base, delta []byte) ([]byte, bool) {
	srcSize, n := binary.Uvarint(delta)
	if n <= 0 || srcSize != uint64(len(base)) {
		return nil, false
	}
	delta = delta[n:]
	dstSize, n := binary.Uvarint(delta)
	if n <= 0 || dstSize > maxCommitSize {
		return nil, false
	}
	delta = delta[n:]
	out := make([]byte, 0, dstSize)
	for len(delta) > 0 {
		op := delta[0]
		delta = delta[1:]
		switch {
		case op&0x80 != 0: // copy from base: offset and size bytes as op's bits say
			var offset, size uint64
			for i := range 7 {
				if op&(1<<i) == 0 {
					continue
				}
				if len(delta) == 0 {
					return nil, false
				}
				if i < 4 {
					offset |= uint64(delta[0]) << (8 * i)
				} else {
					size |= uint64(delta[0]) << (8 * (i - 4))
				}
				delta = delta[1:]
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > uint64(len(base)) || uint64(len(out))+size > dstSize {
				return nil, false
			}
			out = append(out, base[offset:offset+size]...)
		case op != 0: // insert the op bytes that follow
			if int(op) > len(delta) || uint64(len(out))+uint64(op) > dstSize {
				return nil, false
			}
			out = append(out, delta[:op]...)
			delta = delta[op:]
		default:
			return nil, false
		}
	}
	return out, uint64(len(out)) == dstSize
}

// The flags of paintDown, as git's commit-reach.c names them.
const (
	paintOne    = 1 << iota // reached from one
	paintTwo                // reached from two
	paintStale              // reached from a commit both reach
	paintResult             // a common commit found
)

// paintDown is git's paint_down_to_common for one and two, full lower-case
// ids: it walks back from both, newest commit first, until every commit
// still to walk is reached from a commit both reach, and returns the flags
// of the commits it reached and, in the order found, the commits both reach
// that it found. A merge base is among them unless it is stale; one is an
// ancestor of two when two reaches it.
func (r *commitReader) paintDown(one, two string) (flags map[string]uint8, found []string, ok bool) {
	var q commitQueue
	flags = map[string]uint8{}
	for _, end := range []struct {
		id   string
		flag uint8
	}{{one, paintOne}, {two, paintTwo}} {
		c, ok := r.commit(end.id)
		if !ok {
			return nil, nil, false
		}
		flags[end.id] |= end.flag
		q.add(c)
	}
	for q.nonStale(flags) {
		c := q.next()
		f := flags[c.ID] & (paintOne | paintTwo | paintStale)
		if f == paintOne|paintTwo {
			if flags[c.ID]&paintResult == 0 {
				flags[c.ID] |= paintResult
				found = append(found, c.ID)
			}
			f |= paintStale
		}
		for _, id := range c.Parents {
			if flags[id]&f == f {
				continue
			}
			p, ok := r.commit(id)
			if !ok {
				return nil, nil, false
			}
			flags[id] |= f
			q.add(p)
		}
	}
	return flags, found, true
}

// isAncestor is IsAncestor of the full lower-case ids ancestor and
// descendant.
func (r *commitReader) isAncestor(ancestor, descendant string) (yes, ok bool) {
	flags, _, ok := r.paintDown(ancestor, descendant)
	return ok && flags[ancestor]&paintTwo != 0, ok
}

// mergeBase is MergeBase of the full lower-case ids a and b; ok is false
// when they have several, of which git picks one by its own order.
func (r *commitReader) mergeBase(a, b string) (string, bool) {
	if a == b {
		_, ok := r.commit(a)
		return a, ok
	}
	flags, found, ok := r.paintDown(a, b)
	if !ok {
		return "", false
	}
	var bases []string
	for _, id := range found {
		if flags[id]&paintStale == 0 {
			bases = append(bases, id)
		}
	}
	switch len(bases) {
	case 0:
		return "", true
	case 1:
		return bases[0], true
	}
	return "", false
}

// between is the commits head reaches and from does not, every commit head
// reaches for a from of "", oldest first, of full lower-case ids; ok is
// false unless each of them has one parent at most, the only history in
// which the order is the same for every order git lists commits in.
func (r *commitReader) between(from, head string) ([]*Commit, bool) {
	stop := ""
	if from != "" {
		base, ok := r.mergeBase(from, head)
		if !ok {
			return nil, false
		}
		stop = base
	}
	var commits []*Commit
	for id := head; id != stop; {
		c, ok := r.commit(id)
		if !ok || len(c.Parents) > 1 {
			return nil, false
		}
		commits = append(commits, c)
		if len(c.Parents) == 0 {
			if stop != "" {
				return nil, false
			}
			break
		}
		id = c.Parents[0]
	}
	slices.Reverse(commits)
	return commits, true
}

// commitQueue is git's commit queue without generation numbers: the newest
// committer time first, and of equal times the one added first.
type commitQueue struct {
	items []queuedCommit
	added int
}

type queuedCommit struct {
	c   *Commit
	seq int
}

func (q *commitQueue) add(c *Commit) {
	heap.Push((*commitHeap)(q), queuedCommit{c: c, seq: q.added})
	q.added++
}

func (q *commitQueue) next() *Commit { return heap.Pop((*commitHeap)(q)).(queuedCommit).c }

// nonStale reports whether a commit still to walk is not stale.
func (q *commitQueue) nonStale(flags map[string]uint8) bool {
	return slices.ContainsFunc(q.items, func(e queuedCommit) bool { return flags[e.c.ID]&paintStale == 0 })
}

// commitHeap is commitQueue as a container/heap.
type commitHeap commitQueue

func (h *commitHeap) Len() int { return len(h.items) }
func (h *commitHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	if ta, tb := a.c.Committer.Time, b.c.Committer.Time; !ta.Equal(tb) {
		return ta.After(tb)
	}
	return a.seq < b.seq
}
func (h *commitHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *commitHeap) Push(x any)    { h.items = append(h.items, x.(queuedCommit)) }
func (h *commitHeap) Pop() any {
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return last
}

// commitReader is the reader of r's commits for one call under ctx; ok is
// false when it would not read them the way git does, and when ctx has
// ended, which git then reports.
func (r *Repo) commitReader(ctx context.Context) (*commitReader, bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	return newCommitReader(r.dir, r.g.Env)
}

// readCommits is ReadCommits of the full lower-case ids ids read in
// process; ok is false for what it leaves to git.
func (r *Repo) readCommits(ctx context.Context, ids []string) ([]Commit, bool) {
	cr, ok := r.commitReader(ctx)
	if !ok {
		return nil, false
	}
	defer cr.close()
	commits := make([]Commit, 0, len(ids))
	for _, id := range ids {
		c, ok := cr.commit(id)
		if !ok {
			return nil, false
		}
		commits = append(commits, *c)
	}
	return commits, true
}

// commitsBetween is between of r's commits read in process; ok is false
// for what it leaves to git.
func (r *Repo) commitsBetween(ctx context.Context, from, head string) ([]*Commit, bool) {
	cr, ok := r.commitReader(ctx)
	if !ok {
		return nil, false
	}
	defer cr.close()
	return cr.between(from, head)
}
