package scm

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The limits of a diff answer: the files it lists and the patch text it
// holds. A diff over either is cut and says so.
const (
	maxDiffFiles  = 5000
	maxPatchBytes = 8 << 20
)

// The lines of context of a diff's patches: the default and the most a
// request may ask for.
const (
	defaultContext = 3
	maxContext     = 20
)

// fileChange is how one file differs between two trees, as the diff,
// compare and commit routes answer it.
type fileChange struct {
	Path       string  `json:"path"`
	OldPath    *string `json:"old_path"`
	Status     string  `json:"status"`
	Insertions int     `json:"insertions"`
	Deletions  int     `json:"deletions"`
	Binary     bool    `json:"binary"`
	OldSHA     *string `json:"old_sha"`
	NewSHA     *string `json:"new_sha"`
	// Patch is the file's unified diff, only on the diff route.
	Patch *string `json:"patch,omitempty"`
}

// fileChanges are changes as the read routes answer them: a path, id or
// old path a change has no value for is null.
func fileChanges(changes []gitrepo.FileChange) []fileChange {
	orNull := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	files := make([]fileChange, 0, len(changes))
	for _, ch := range changes {
		files = append(files, fileChange{
			Path: ch.Path, OldPath: orNull(ch.OldPath), Status: ch.Status, Insertions: ch.Insertions,
			Deletions: ch.Deletions, Binary: ch.Binary, OldSHA: orNull(ch.OldID), NewSHA: orNull(ch.NewID),
		})
	}
	return files
}

// diffStats are the numbers of a set of file changes.
type diffStats struct {
	FilesChanged int `json:"files_changed"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
}

func newDiffStats(s gitrepo.DiffStats) diffStats {
	return diffStats{FilesChanged: s.FilesChanged, Insertions: s.Insertions, Deletions: s.Deletions}
}

// diffAnswer is the JSON answer of the diff route.
type diffAnswer struct {
	BaseSHA      string       `json:"base_sha"`
	HeadSHA      string       `json:"head_sha"`
	MergeBaseSHA *string      `json:"merge_base_sha"`
	Files        []fileChange `json:"files"`
	Stats        diffStats    `json:"stats"`
	Truncated    bool         `json:"truncated"`
}

// diffQuery is what a diff request asks for.
type diffQuery struct {
	base, head string
	direct     bool
	asPatch    bool
	patches    bool
	context    int
	globs      []string
}

// readDiffQuery reads the query of a diff request; a missing revision or a
// value the route does not take is 400 invalid_request.
func readDiffQuery(r *http.Request) (diffQuery, error) {
	q := r.URL.Query()
	d := diffQuery{base: q.Get("base"), head: q.Get("head"), context: defaultContext, globs: q["path"]}
	if d.base == "" || d.head == "" {
		return d, server.InvalidRequest("base and head are required")
	}
	switch q.Get("mode") {
	case "", "merge_base":
	case "direct":
		d.direct = true
	default:
		return d, server.InvalidRequest("mode must be merge_base or direct")
	}
	switch q.Get("format") {
	case "", "json":
	case "patch":
		d.asPatch = true
	default:
		return d, server.InvalidRequest("format must be json or patch")
	}
	if q.Has("context") {
		n, err := strconv.Atoi(q.Get("context"))
		if err != nil || n < 0 || n > maxContext {
			return d, server.InvalidRequest(fmt.Sprintf("context must be an integer from 0 to %d", maxContext))
		}
		d.context = n
	}
	var err error
	d.patches, err = boolParam(q, "patches", true)
	return d, err
}

// getDiff answers GET …/scm/diff: the file changes from the merge base of
// base and head (mode merge_base, the default) or from base itself (mode
// direct) to head, as JSON or, with format=patch, as one unified diff. The
// files are those whose path one of the path globs matches; the answer
// lists at most maxDiffFiles of them and holds at most maxPatchBytes of
// patch text, cut at a file, and says when it was cut.
func (c *Component) getDiff(w http.ResponseWriter, r *http.Request) error {
	dq, err := readDiffQuery(r)
	if err != nil {
		return err
	}
	rd, err := c.open(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	base, err := rd.resolver.Resolve(ctx, dq.base)
	if err != nil {
		return err
	}
	head, err := rd.resolver.Resolve(ctx, dq.head)
	if err != nil {
		return err
	}
	repr := reprJSON
	if dq.asPatch {
		repr = reprPatch
	}
	if notModified(w, r, base.Full && head.Full, base.SHA, head.SHA, repr) {
		return nil
	}
	mergeBase, err := rd.repo.MergeBase(ctx, base.SHA, head.SHA)
	if err != nil {
		return err
	}
	// Two commits without a common ancestor are compared from the empty
	// tree (""), since everything in head is added.
	left := mergeBase
	if dq.direct {
		left = base.SHA
	}
	changes, err := rd.repo.DiffChanges(ctx, left, head.SHA)
	if err != nil {
		return err
	}
	// Each change kept, by its index among changes, so that its patch is
	// found among the patches of every change.
	var kept []int
	var keptChanges []gitrepo.FileChange
	for i, ch := range changes {
		if matchesAny(dq.globs, ch.Path) {
			kept, keptChanges = append(kept, i), append(keptChanges, ch)
		}
	}
	answer := diffAnswer{BaseSHA: base.SHA, HeadSHA: head.SHA, Stats: newDiffStats(gitrepo.Stats(keptChanges))}
	if mergeBase != "" {
		answer.MergeBaseSHA = &mergeBase
	}
	if len(kept) > maxDiffFiles {
		kept, keptChanges, answer.Truncated = kept[:maxDiffFiles], keptChanges[:maxDiffFiles], true
	}
	answer.Files = fileChanges(keptChanges)

	var text bytes.Buffer
	if dq.asPatch || dq.patches {
		opt := gitrepo.PatchOptions{Context: dq.context, Binary: dq.asPatch, Max: maxPatchBytes}
		cut, err := readPatches(ctx, rd.repo, left, head.SHA, opt, changes, kept, func(file int, patch []byte) {
			if dq.asPatch {
				text.Write(patch)
				return
			}
			p := string(patch)
			answer.Files[file].Patch = &p
		})
		if err != nil {
			return err
		}
		answer.Truncated = answer.Truncated || cut
	}
	if !dq.asPatch {
		server.WriteJSON(w, http.StatusOK, answer)
		return nil
	}
	w.Header().Set("Content-Type", "text/x-diff")
	if answer.Truncated {
		w.Header().Set("X-Parley-Truncated", "true")
	}
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(text.Bytes())
	return err
}

// readPatches reads the patches of the kept changes, the indexes of the
// files the answer lists among changes, and gives each to add with the
// file's position in the answer, in order, until the next patch would take
// the text past maxPatchBytes; cut says that a file's patch was left out.
// A binary file has a patch only with opt.Binary, the patch form, whose
// text then applies; the JSON form gives it none.
func readPatches(ctx context.Context, repo Repository, from, to string, opt gitrepo.PatchOptions, changes []gitrepo.FileChange, kept []int,
	add func(file int, patch []byte)) (cut bool, err error) {
	if len(kept) == 0 {
		return false, nil
	}
	file, size := 0, 0
	var mismatch error
	err = repo.DiffPatches(ctx, from, to, opt, func(i int, patch []byte) bool {
		if i >= len(changes) {
			mismatch = fmt.Errorf("diff: git wrote a patch for the change %d of %d", i, len(changes))
			return false
		}
		if i != kept[file] {
			return true
		}
		if !opt.Binary && changes[i].Binary {
			file++
			return file < len(kept)
		}
		if patch == nil || size+len(patch) > maxPatchBytes {
			cut = true
			return false
		}
		add(file, patch)
		size += len(patch)
		file++
		return file < len(kept)
	})
	if err == nil {
		err = mismatch
	}
	return cut, err
}
