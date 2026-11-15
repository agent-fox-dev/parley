package scm

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// treeBounds are the default and the bounds of a tree listing's limit.
var treeBounds = server.PageBounds{Default: 1000, Min: 1, Max: 10000}

// The types of a tree entry.
const (
	entryBlob    = "blob"
	entryTree    = "tree"
	entryCommit  = "commit"
	entrySymlink = "symlink"
)

// git's modes of a symlink and of a submodule entry.
const (
	modeSymlink   = "120000"
	modeSubmodule = "160000"
)

// treeEntry is an entry of a directory as the tree route answers it.
type treeEntry struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Type string `json:"type"`
	Mode string `json:"mode"`
	SHA  string `json:"sha"`
	// Size is present on entries of type blob alone.
	Size *int64 `json:"size,omitempty"`
}

// treePosition is where a tree listing continues: the commit its first page
// resolved, so that every page lists the same tree when the ref moves, and
// the path of the last entry the pages before held.
type treePosition struct {
	SHA  string `json:"sha"`
	Path string `json:"path"`
}

// listTree answers GET …/scm/tree: a page of the entries of the directory
// path (the root by default) at ref, or with recursive=true of every entry
// below it, ordered by path, with the commit and the directory's tree id it
// was read from.
func (c *Component) listTree(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r, treeBounds)
	if err != nil {
		return err
	}
	var pos treePosition
	continuing, err := page.After(&pos)
	if err != nil {
		return err
	}
	if continuing && (!isCommitID(pos.SHA) || len(pos.SHA) != commitIDLen || pos.Path == "") {
		return server.InvalidRequest("cursor is malformed or belongs to another query")
	}
	q := r.URL.Query()
	path, err := pathParam(q, false)
	if err != nil {
		return err
	}
	recursive, err := boolParam(q, "recursive", false)
	if err != nil {
		return err
	}

	rd, err := c.open(r)
	if err != nil {
		return err
	}
	res, err := listedCommit(r, rd, continuing, pos.SHA)
	if err != nil {
		return err
	}
	if notModified(w, r, res.Full, res.SHA) {
		return nil
	}
	sha := res.SHA
	// One entry past the page tells that there is a next page.
	read := gitrepo.TreeRead{Recursive: recursive, Limit: page.Limit + 1}
	if continuing {
		read.After = pos.Path
	}
	tree, err := rd.repo.ReadTree(r.Context(), sha, path, read)
	switch {
	case errors.Is(err, gitrepo.ErrNotFound):
		return server.NewError("path_not_found", http.StatusNotFound, fmt.Sprintf("path %q does not exist at %s", path, sha))
	case errors.Is(err, gitrepo.ErrNotDirectory):
		return server.InvalidRequest(fmt.Sprintf("path %q is not a directory at %s", path, sha))
	case err != nil:
		return err
	}
	items := make([]treeEntry, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		items = append(items, newTreeEntry(e))
	}
	return server.WritePageWith(w, page, items, func(e treeEntry) any { return treePosition{SHA: sha, Path: e.Path} },
		server.Member{Name: "resolved_sha", Value: sha}, server.Member{Name: "tree_sha", Value: tree.ID})
}

// newTreeEntry is git's entry e as the tree route answers it: a symlink and
// a submodule are told from other blobs and commits by their mode, and only
// a blob has a size.
func newTreeEntry(e gitrepo.TreeEntry) treeEntry {
	t := treeEntry{Path: e.Path, Name: e.Name, Mode: e.Mode, SHA: e.ID}
	switch {
	case e.Mode == modeSymlink:
		t.Type = entrySymlink
	case e.Mode == modeSubmodule:
		t.Type = entryCommit
	case e.Type == gitrepo.ObjectTree:
		t.Type = entryTree
	default:
		t.Type = entryBlob
		t.Size = &e.Size
	}
	return t
}
