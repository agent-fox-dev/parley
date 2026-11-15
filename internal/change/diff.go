package change

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/server"
)

// diffFile is a file of a diff answer: the file change of the source
// control API's diff and, in an interdiff, whether it changed between the
// two versions.
type diffFile struct {
	scm.FileChange
	Changed *bool `json:"changed,omitempty"`
}

// diffAnswer is the JSON answer of GET …/changes/:number/diff.
type diffAnswer struct {
	BaseSHA      string        `json:"base_sha"`
	HeadSHA      string        `json:"head_sha"`
	MergeBaseSHA *string       `json:"merge_base_sha"`
	Files        []diffFile    `json:"files"`
	Stats        scm.DiffStats `json:"stats"`
	Truncated    bool          `json:"truncated"`
	Version      int64         `json:"version"`
	Against      *int64        `json:"against"`
}

// diffQuery is what a diff request asks for: the version (0 for the
// current one), the version to compare it with (0 for none, the plain
// diff), the path globs, and the form of the answer.
type diffQuery struct {
	version, against int64
	globs            []string
	asPatch, patches bool
}

// readDiffQuery reads the query of a diff request; a value the route does
// not take is 400 invalid_request.
func readDiffQuery(q url.Values) (diffQuery, error) {
	d := diffQuery{globs: q["path"], patches: true}
	if q.Has("version") && q.Get("version") != "latest" {
		n, ok := positive(q.Get("version"))
		if !ok {
			return d, server.InvalidRequest("version must be a positive integer or latest")
		}
		d.version = n
	}
	if q.Has("against") {
		n, ok := positive(q.Get("against"))
		if !ok {
			return d, server.InvalidRequest("against must be a positive integer")
		}
		d.against = n
	}
	switch q.Get("format") {
	case "", "json":
	case "patch":
		d.asPatch = true
	default:
		return d, server.InvalidRequest("format must be json or patch")
	}
	if q.Has("patches") {
		switch q.Get("patches") {
		case "true":
		case "false":
			d.patches = false
		default:
			return d, server.InvalidRequest("patches must be true or false")
		}
	}
	return d, nil
}

// getDiff answers the diff of a version of the change, from its base_sha
// to its head_sha, or, with against, the interdiff of the version and
// against, with the limits of the source control API's diff. A version
// number the change does not have is 404 not_found; a version whose base or
// head commit is no longer in the workspace's repository, such as after a
// reclone, 404 ref_not_found, while the change and version records stay
// readable.
func (c *Component) getDiff(w http.ResponseWriter, r *http.Request) error {
	dq, err := readDiffQuery(r.URL.Query())
	if err != nil {
		return err
	}
	ctx, q := r.Context(), c.deps.Store.Main()
	ch, err := c.changeOf(r, q)
	if err != nil {
		return err
	}
	v, err := versionOf(ctx, q, ch, dq.version)
	if err != nil {
		return err
	}
	named := []Version{v}
	if dq.against != 0 {
		a, err := versionOf(ctx, q, ch, dq.against)
		if err != nil {
			return err
		}
		named = append(named, a)
	}
	repo, err := c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return err
	}
	if err := present(ctx, repo, ch, named); err != nil {
		return err
	}
	if len(named) == 1 {
		return plainDiff(ctx, w, repo, v, dq)
	}
	return interdiff(ctx, w, repo, named[1], v, dq)
}

// versionOf is the version number of the change, 0 for the current one;
// 404 not_found when the change has no such version.
func versionOf(ctx context.Context, q Querier, ch Change, number int64) (Version, error) {
	if number == 0 {
		number = ch.Version
	}
	vs, err := readVersionsAfter(ctx, q, ch.WorkspaceSlug, ch.Number, number-1, 1)
	if err != nil {
		return Version{}, err
	}
	if len(vs) == 0 || vs[0].Number != number {
		return Version{}, server.NotFound(fmt.Sprintf("%s has no version %d", ch.Ref, number))
	}
	return vs[0], nil
}

// present answers 404 ref_not_found unless the base and head commits of
// every version are in the repository.
func present(ctx context.Context, repo *gitrepo.Repo, ch Change, versions []Version) error {
	for _, v := range versions {
		ids := []string{v.HeadSHA}
		if v.BaseSHA != "" {
			ids = append(ids, v.BaseSHA)
		}
		missing, err := repo.Missing(ctx, ids)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return server.NewError("ref_not_found", http.StatusNotFound,
				fmt.Sprintf("version %d of %s: the commit %s is not in the repository", v.Number, ch.Ref, missing[0]))
		}
	}
	return nil
}

// plainDiff answers the diff of the version from its base_sha to its
// head_sha, computed by the source control API's diff.
func plainDiff(ctx context.Context, w http.ResponseWriter, repo *gitrepo.Repo, v Version, dq diffQuery) error {
	d, err := scm.ComputeDiff(ctx, repo, v.BaseSHA, v.HeadSHA, scm.DiffQuery{
		Globs: dq.globs, Patches: dq.patches, AsPatch: dq.asPatch, Context: scm.DefaultContext,
	})
	if err != nil {
		return err
	}
	if dq.asPatch {
		return scm.WritePatchText(w, d.Text, d.Truncated)
	}
	files := make([]diffFile, 0, len(d.Files))
	for _, f := range d.Files {
		files = append(files, diffFile{FileChange: f})
	}
	server.WriteJSON(w, http.StatusOK, diffAnswer{
		BaseSHA: v.BaseSHA, HeadSHA: v.HeadSHA, MergeBaseSHA: &v.BaseSHA, Files: files, Stats: d.Stats,
		Truncated: d.Truncated, Version: v.Number,
	})
	return nil
}

// ownChange is how a version changes one file against its own base, with
// the file's patch in the form the interdiff compares.
type ownChange struct {
	change gitrepo.FileChange
	key    string
}

// ownChanges are the files the version changes against its own base, by
// path.
func ownChanges(ctx context.Context, repo *gitrepo.Repo, v Version) (map[string]ownChange, error) {
	changes, err := repo.DiffChanges(ctx, v.BaseSHA, v.HeadSHA)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ownChange, len(changes))
	opt := gitrepo.PatchOptions{Context: scm.DefaultContext, Max: scm.MaxPatchBytes}
	err = repo.DiffPatches(ctx, v.BaseSHA, v.HeadSHA, opt, func(i int, patch []byte) bool {
		if i < len(changes) {
			out[changes[i].Path] = ownChange{changes[i], patchKey(changes[i], patch)}
		}
		return true
	})
	return out, err
}

// patchKey is the patch of a file change as two versions compare it: the
// hunk bodies without the index line and the hunks' positions, which a
// rebase changes. A binary file, or a patch too long to read, compares by
// its blob ids.
func patchKey(ch gitrepo.FileChange, patch []byte) string {
	if ch.Binary || patch == nil {
		return "\x00" + ch.Status + " " + ch.OldPath + " " + ch.OldID + " " + ch.NewID
	}
	var b strings.Builder
	for line := range strings.Lines(string(patch)) {
		switch {
		case strings.HasPrefix(line, "index "):
		case strings.HasPrefix(line, "@@"):
			b.WriteString("@@\n")
		default:
			b.WriteString(line)
		}
	}
	return b.String()
}

// interdiff answers what moved from the version against to the version v:
// every file either version touches against its own base, changed when its
// patch differs between them or only one touches it. A changed file's
// patch is the diff of its content at the head of against to its content
// at the head of v, computed by the source control API's diff of the two
// heads restricted to the changed files; stats count the changed files.
func interdiff(ctx context.Context, w http.ResponseWriter, repo *gitrepo.Repo, against, v Version, dq diffQuery) error {
	before, err := ownChanges(ctx, repo, against)
	if err != nil {
		return err
	}
	after, err := ownChanges(ctx, repo, v)
	if err != nil {
		return err
	}
	var paths []string
	changed := map[string]bool{}
	for path, a := range after {
		b, ok := before[path]
		changed[path] = !ok || a.key != b.key
		paths = append(paths, path)
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			changed[path] = true
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	paths = slices.DeleteFunc(paths, func(p string) bool { return !identity.MatchAny(dq.globs, p) })

	d, err := scm.ComputeDiff(ctx, repo, against.HeadSHA, v.HeadSHA, scm.DiffQuery{
		Globs: dq.globs, Keep: func(p string) bool { return changed[p] }, Patches: dq.patches, AsPatch: dq.asPatch,
		Context: scm.DefaultContext,
	})
	if err != nil {
		return err
	}
	if dq.asPatch {
		return scm.WritePatchText(w, d.Text, d.Truncated)
	}
	direct := make(map[string]scm.FileChange, len(d.Files))
	for _, f := range d.Files {
		direct[f.Path] = f
	}
	answer := diffAnswer{
		BaseSHA: against.HeadSHA, HeadSHA: v.HeadSHA, Stats: d.Stats, Truncated: d.Truncated,
		Version: v.Number, Against: &against.Number, Files: make([]diffFile, 0, min(len(paths), scm.MaxDiffFiles)),
	}
	if len(paths) > scm.MaxDiffFiles {
		paths, answer.Truncated = paths[:scm.MaxDiffFiles], true
	}
	for _, path := range paths {
		own, ok := after[path]
		if !ok {
			own = before[path]
		}
		fc, differs := direct[path]
		switch {
		case !changed[path]:
			fc = scm.FileChanges([]gitrepo.FileChange{own.change})[0]
		case !differs:
			// Changed, yet the same at both heads: the patches differ
			// because the bases do.
			fc = sameAtBothHeads(path, own.change, dq.patches)
			answer.Stats.FilesChanged++
		}
		answer.Files = append(answer.Files, diffFile{FileChange: fc, Changed: new(changed[path])})
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// sameAtBothHeads is the file change of a changed file whose content is the
// same at both heads: modified, with no line changed and an empty patch.
func sameAtBothHeads(path string, own gitrepo.FileChange, patches bool) scm.FileChange {
	at := own.NewID
	f := scm.FileChanges([]gitrepo.FileChange{{Path: path, Status: gitrepo.ChangeModified, Binary: own.Binary, OldID: at, NewID: at}})[0]
	if patches {
		f.Patch = new("")
	}
	return f
}
