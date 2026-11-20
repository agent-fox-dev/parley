package scm

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/agentkit-go/outline"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// maxOutlineSize is the size in bytes of the largest file the outline route
// outlines: 1 MiB, which is also the most the outline package takes.
const maxOutlineSize = 1 << 20

// outlineDecl is one declaration of an outline answer.
type outlineDecl struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Container string `json:"container"`
	Signature string `json:"signature"`
	Exported  bool   `json:"exported"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// outlineAnswer is the answer of the outline route.
type outlineAnswer struct {
	Path         string        `json:"path"`
	ResolvedSHA  string        `json:"resolved_sha"`
	Language     string        `json:"language"`
	Supported    bool          `json:"supported"`
	Declarations []outlineDecl `json:"declarations"`
}

// getOutline answers GET …/scm/outline: the declarations of the source file
// path at ref, in line order. A file this build has no backend for (any
// language but Go when built without cgo, and any file of no known
// language) is answered as not supported, with no declarations, and so is a
// symbolic link, whose content is the path it points at. A file that is
// binary or over maxOutlineSize is refused before it is parsed.
func (c *Component) getOutline(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	path, err := pathParam(q, true)
	if err != nil {
		return err
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
	answer := outlineAnswer{Path: path, ResolvedSHA: res.SHA, Declarations: []outlineDecl{}}
	if file.Mode == modeSymlink {
		server.WriteJSON(w, http.StatusOK, answer)
		return nil
	}
	if file.Size > maxOutlineSize {
		return server.PayloadTooLarge(fmt.Sprintf("file %q is %d bytes, over the outline limit of %d", path, file.Size, maxOutlineSize)).
			With("size", file.Size)
	}
	var content bytes.Buffer
	if err := rd.repo.WriteBlob(ctx, file.ID, &content); err != nil {
		return err
	}
	if isBinary(content.Bytes()) {
		return server.NewError("binary_file", http.StatusUnprocessableEntity,
			fmt.Sprintf("file %q is binary at %s, and a binary file has no declarations to outline", path, res.SHA))
	}
	// The parse ends with the request's context, at the read timeout; the
	// route's wrapper answers that end.
	out, err := outline.Outline(ctx, "/"+path, content.Bytes(), outline.Options{MaxFileBytes: maxOutlineSize})
	if err != nil {
		return fmt.Errorf("outline %q at %s: %w", path, res.SHA, err)
	}
	answer.Language = out.Lang
	answer.Supported = out.Backend != outline.BackendNone
	for _, d := range out.Decls {
		answer.Declarations = append(answer.Declarations, outlineDecl{Kind: string(d.Kind), Name: d.Name, Container: d.Container,
			Signature: d.Signature, Exported: d.Exported, StartLine: d.StartLine, EndLine: d.EndLine})
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}
