package scm

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// maxBlameSize is the size in bytes of the largest file the blame route
// blames: 2 MiB.
const maxBlameSize = 2 << 20

// binaryProbe is how many of a file's first bytes git looks at for a NUL
// byte to call the file binary.
const binaryProbe = 8000

// isBinary reports whether content is a binary file's, as git judges it: a
// NUL byte among its first binaryProbe bytes.
func isBinary(content []byte) bool {
	return bytes.IndexByte(content[:min(len(content), binaryProbe)], 0) >= 0
}

// blameRange is a range of lines of a blame answer.
type blameRange struct {
	StartLine         int          `json:"start_line"`
	EndLine           int          `json:"end_line"`
	Commit            commitObject `json:"commit"`
	OriginalPath      string       `json:"original_path"`
	OriginalStartLine int          `json:"original_start_line"`
}

// blameAnswer is the answer of the blame route.
type blameAnswer struct {
	ResolvedSHA string       `json:"resolved_sha"`
	Path        string       `json:"path"`
	Ranges      []blameRange `json:"ranges"`
}

// getBlame answers GET …/scm/blame: the lines start_line to end_line of
// the text file path at ref, by default all of them, in ranges of adjacent
// lines that one commit last changed at adjacent lines, each with that
// commit's object, without stats, and the path and line the range had
// there, following renames. A file that is not text or is over
// maxBlameSize is refused before it is blamed.
func (c *Component) getBlame(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	path, err := pathParam(q, true)
	if err != nil {
		return err
	}
	start, err := lineParam(q, "start_line", 1)
	if err != nil {
		return err
	}
	end, err := lineParam(q, "end_line", 0)
	if err != nil {
		return err
	}
	if start < 1 {
		return server.InvalidRequest("start_line must be 1 or more")
	}
	if q.Has("end_line") && start > end {
		return server.InvalidRequest("start_line must not be past end_line")
	}

	rd, err := c.open(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	res, err := rd.resolver.ResolveOptional(ctx, q.Get("ref"))
	if err != nil {
		return err
	}
	if notModified(w, r, res.Full, res.SHA) {
		return nil
	}
	file, err := rd.repo.LookupFile(ctx, res.SHA, path)
	switch {
	case errors.Is(err, gitrepo.ErrNotFound):
		return server.NewError("path_not_found", http.StatusNotFound, fmt.Sprintf("path %q does not exist at %s", path, res.SHA))
	case errors.Is(err, gitrepo.ErrNotFile):
		return server.InvalidRequest(fmt.Sprintf("path %q is a directory or a submodule at %s, not a file", path, res.SHA))
	case err != nil:
		return err
	}
	if file.Size > maxBlameSize {
		return server.PayloadTooLarge(fmt.Sprintf("file %q is %d bytes, over the blame limit of %d", path, file.Size, maxBlameSize)).
			With("size", file.Size)
	}
	var content bytes.Buffer
	if err := rd.repo.WriteBlob(ctx, file.ID, &content); err != nil {
		return err
	}
	if isBinary(content.Bytes()) {
		return server.InvalidRequest(fmt.Sprintf("file %q is binary at %s, and a binary file has no lines to blame", path, res.SHA))
	}
	lines := lineCount(content.Bytes())
	answer := blameAnswer{ResolvedSHA: res.SHA, Path: path, Ranges: []blameRange{}}
	if lines == 0 && !q.Has("start_line") {
		server.WriteJSON(w, http.StatusOK, answer)
		return nil
	}
	if start > lines {
		return server.InvalidRequest(fmt.Sprintf("start_line %d is past the end of %q, which has %d lines", start, path, lines))
	}
	if end == 0 || end > lines {
		end = lines
	}

	ranges, err := rd.repo.Blame(ctx, res.SHA, path, start, end)
	if err != nil {
		return err
	}
	// Each commit is read, and its lookups asked, once however many ranges
	// it has.
	var ids []string
	seen := map[string]bool{}
	for _, g := range ranges {
		if !seen[g.Commit] {
			seen[g.Commit] = true
			ids = append(ids, g.Commit)
		}
	}
	commits, err := rd.repo.ReadCommits(ctx, ids)
	if err != nil {
		return err
	}
	objs, err := c.commitObjects(ctx, rd, commits, nil)
	if err != nil {
		return err
	}
	byID := make(map[string]commitObject, len(objs))
	for _, o := range objs {
		byID[o.SHA] = o
	}
	for _, g := range ranges {
		answer.Ranges = append(answer.Ranges, blameRange{StartLine: g.StartLine, EndLine: g.EndLine, Commit: byID[g.Commit],
			OriginalPath: g.OriginalPath, OriginalStartLine: g.OriginalStartLine})
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// lineParam is the query parameter name of q as a line number, def when q
// has none; a value that is not an integer is 400 invalid_request.
func lineParam(q url.Values, name string, def int) (int, error) {
	if !q.Has(name) {
		return def, nil
	}
	n, err := strconv.Atoi(q.Get(name))
	if err != nil {
		return 0, server.InvalidRequest(fmt.Sprintf("%s must be an integer", name))
	}
	return n, nil
}

// lineCount is the number of lines of content as git counts them: a last
// line without a newline counts.
func lineCount(content []byte) int {
	n := bytes.Count(content, []byte{'\n'})
	if len(content) > 0 && content[len(content)-1] != '\n' {
		n++
	}
	return n
}
