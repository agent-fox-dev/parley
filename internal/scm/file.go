package scm

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
)

// DefaultMaxFileSize is the size in bytes of the largest file the file
// route serves when nothing else is configured: 32 MiB.
const DefaultMaxFileSize = 32 << 20

// The encodings of a file's content in the JSON form.
const (
	encodingUTF8   = "utf8"
	encodingBase64 = "base64"
)

// fileObject is a file as the JSON form of the file route answers it.
type fileObject struct {
	Path        string `json:"path"`
	SHA         string `json:"sha"`
	Mode        string `json:"mode"`
	Size        int64  `json:"size"`
	Encoding    string `json:"encoding"`
	Content     string `json:"content"`
	ResolvedSHA string `json:"resolved_sha"`
}

// getFile answers GET …/scm/file: the file or symlink path at ref, the raw
// bytes (a symlink's being its target) or, when Accept names
// application/json, a JSON object. The raw form honours a Range header of
// one byte range. A file over the size limit is refused before any of it is
// read, whole or in part.
func (c *Component) getFile(w http.ResponseWriter, r *http.Request) error {
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
	file, err := rd.repo.LookupFile(ctx, res.SHA, path)
	switch {
	case errors.Is(err, gitrepo.ErrNotFound):
		return server.NewError("path_not_found", http.StatusNotFound, fmt.Sprintf("path %q does not exist at %s", path, res.SHA))
	case errors.Is(err, gitrepo.ErrNotFile):
		return server.InvalidRequest(fmt.Sprintf("path %q is a directory or a submodule at %s, not a file", path, res.SHA))
	case err != nil:
		return err
	}
	if file.Size > c.deps.MaxFileSize {
		return server.PayloadTooLarge(fmt.Sprintf("file %q is %d bytes, over the limit of %d", path, file.Size, c.deps.MaxFileSize)).
			With("size", file.Size)
	}

	if acceptsJSON(r) {
		var content bytes.Buffer
		if err := rd.repo.WriteBlob(ctx, file.ID, &content); err != nil {
			return err
		}
		obj := fileObject{Path: path, SHA: file.ID, Mode: file.Mode, Size: file.Size, Encoding: encodingUTF8, ResolvedSHA: res.SHA}
		if utf8.Valid(content.Bytes()) {
			obj.Content = content.String()
		} else {
			obj.Encoding, obj.Content = encodingBase64, base64.StdEncoding.EncodeToString(content.Bytes())
		}
		server.WriteJSON(w, http.StatusOK, obj)
		return nil
	}

	first, last, ranged := byteRange(r.Header.Get("Range"), file.Size)
	if ranged && first < 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", file.Size))
		return server.NewError("range_not_satisfiable", http.StatusRequestedRangeNotSatisfiable,
			fmt.Sprintf("the range %q does not start inside the file's %d bytes", r.Header.Get("Range"), file.Size))
	}
	status := http.StatusPartialContent
	if !ranged {
		status, first, last = http.StatusOK, 0, file.Size-1
	}
	body := &window{w: w, skip: first, left: last - first + 1, begin: func() {
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.FormatInt(last-first+1, 10))
		h.Set("X-Parley-Blob-Sha", file.ID)
		h.Set("X-Parley-Resolved-Sha", res.SHA)
		h.Set("X-Parley-File-Mode", file.Mode)
		if ranged {
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, file.Size))
		}
		w.WriteHeader(status)
	}}
	if err := rd.repo.WriteBlob(ctx, file.ID, body); err != nil {
		return err
	}
	// An empty answer has no first byte to send the headers with.
	body.start()
	return nil
}

// acceptsJSON reports whether the request's Accept header names
// application/json.
func acceptsJSON(r *http.Request) bool {
	for _, field := range r.Header.Values("Accept") {
		for media := range strings.SplitSeq(field, ",") {
			if t, _, err := mime.ParseMediaType(strings.TrimSpace(media)); err == nil && t == "application/json" {
				return true
			}
		}
	}
	return false
}

// byteRange reads the Range header h of a file of size bytes. ranged is
// false when h asks for no single byte range: absent, malformed, or several
// ranges, each of which is answered with the whole file. Otherwise the range
// runs from first to last, both inclusive, its end cut to the file's last
// byte; first is -1 when the range does not start inside the file.
func byteRange(h string, size int64) (first, last int64, ranged bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	from, to, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return 0, 0, false
	}
	parse := func(s string) (int64, bool) {
		if s == "" || strings.Trim(s, "0123456789") != "" {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	if from == "" {
		// The last n bytes; none of an empty file, and no bytes at all, start
		// inside it.
		n, ok := parse(to)
		switch {
		case !ok:
			return 0, 0, false
		case n == 0 || size == 0:
			return -1, 0, true
		}
		return max(size-n, 0), size - 1, true
	}
	first, ok = parse(from)
	if !ok {
		return 0, 0, false
	}
	last = size - 1
	if to != "" {
		end, ok := parse(to)
		if !ok || end < first {
			return 0, 0, false
		}
		last = min(end, last)
	}
	if first >= size {
		return -1, 0, true
	}
	return first, last, true
}

// window passes on to w the left bytes that follow the first skip bytes
// written to it and drops the others. Before it passes on the first byte it
// calls begin, once, so that an error before any byte still gets its own
// answer.
type window struct {
	w          io.Writer
	skip, left int64
	begin      func()
	begun      bool
}

func (v *window) Write(p []byte) (int, error) {
	n := len(p)
	drop := min(v.skip, int64(len(p)))
	p, v.skip = p[drop:], v.skip-drop
	p = p[:min(int64(len(p)), v.left)]
	if len(p) == 0 {
		return n, nil
	}
	v.start()
	if _, err := v.w.Write(p); err != nil {
		return 0, err
	}
	v.left -= int64(len(p))
	return n, nil
}

// start sends the answer's headers, once.
func (v *window) start() {
	if !v.begun {
		v.begun = true
		v.begin()
	}
}
