package sandbox

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
)

// errorTypePathOutside answers, with 400, a transfer path or a tar entry
// that resolves to a place that is not the working directory or below it.
const errorTypePathOutside = "path_outside_workdir"

// defaultFileMode is the mode of an uploaded file whose request names none.
const defaultFileMode fs.FileMode = 0o644

// The media types an upload's body may have; a tar stream may also be sent
// as plain octets.
const (
	mediaOctets = "application/octet-stream"
	mediaTar    = "application/x-tar"
)

// filesRequest is the query of a file transfer once it is judged.
type filesRequest struct {
	// path is absolute, clean and inside the working directory.
	path string
	// tar is set by archive=tar.
	tar bool
	// mode is the mode of an uploaded file.
	mode fs.FileMode
}

// resolvePath is p as an absolute path in the sandbox: a relative path is
// taken from the working directory, and . and .. are removed. It is refused
// with 400 path_outside_workdir unless it is the working directory or below
// it. The resolution is lexical and follows no symbolic link: what a path
// reaches through a link is confined by the runtime.
func resolvePath(p string) (string, error) {
	if !path.IsAbs(p) {
		p = path.Join(WorkDir, p)
	}
	p = path.Clean(p)
	if !within(p, WorkDir) {
		return "", server.NewError(errorTypePathOutside, http.StatusBadRequest, "path must be inside the working directory "+WorkDir)
	}
	return p, nil
}

// within reports whether the clean absolute path p is dir or below it.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// readFilePath reads the path of a transfer's query: present, not empty
// and inside the working directory.
func readFilePath(r *http.Request) (string, error) {
	p := r.URL.Query().Get("path")
	if p == "" {
		return "", server.InvalidRequest("path: is required")
	}
	return resolvePath(p)
}

// validateDownload reads the query of a download into u.
func validateDownload(r *http.Request, u *useRequest) (err error) {
	u.files.path, err = readFilePath(r)
	return err
}

// validateUpload reads the query and the content type of an upload into u.
// It reads none of the body.
func validateUpload(r *http.Request, u *useRequest) error {
	p, err := readFilePath(r)
	if err != nil {
		return err
	}
	req := filesRequest{path: p, mode: defaultFileMode}
	q := r.URL.Query()
	if q.Has("archive") {
		if q.Get("archive") != "tar" {
			return server.InvalidRequest("archive: must be tar")
		}
		req.tar = true
	}
	if q.Has("mode") {
		if req.tar {
			return server.InvalidRequest("mode: must not be sent with archive=tar, whose entries carry their modes")
		}
		if req.mode, err = parseMode(q.Get("mode")); err != nil {
			return err
		}
	}
	allowed := []string{mediaOctets}
	if req.tar {
		allowed = append(allowed, mediaTar)
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(a, mt) }) {
		return server.UnsupportedMediaType("request body must be " + strings.Join(allowed, " or "))
	}
	u.files = req
	return nil
}

// parseMode reads an octal mode from 0000 to 0777.
func parseMode(s string) (fs.FileMode, error) {
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n > 0o777 {
		return 0, server.InvalidRequest("mode: must be an octal number from 0000 to 0777")
	}
	return fs.FileMode(n), nil
}

// errTransferTooLarge ends the read of a body past [sandbox].max_transfer_size.
var errTransferTooLarge = errors.New("the body exceeds the transfer limit")

// limitedBody reads a body up to a limit and fails with errTransferTooLarge
// once it would pass it. It keeps the first error its source gave, so that
// a failed read of the body is told apart from a failure of the runtime
// that reads it.
type limitedBody struct {
	r io.Reader
	// left is what may still be read; read is what was.
	left, read int64
	err        error
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	// One byte past the limit tells a body of exactly the limit from a
	// longer one.
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.r.Read(p)
	if int64(n) > b.left {
		n, err = int(b.left), errTransferTooLarge
	}
	b.left -= int64(n)
	b.read += int64(n)
	if err != nil {
		b.err = err
	}
	return n, err
}

// failed returns what answers an upload whose body read stopped with an
// error other than its end, and nil when it did not.
func (b *limitedBody) failed() error {
	switch {
	case b.err == nil || errors.Is(b.err, io.EOF):
		return nil
	case errors.Is(b.err, errTransferTooLarge):
		return tooLarge()
	}
	return server.InvalidRequest("request body could not be read")
}

// notTar answers a tar upload whose stream could not be read, which may be
// because its body passed the limit.
func (b *limitedBody) notTar() error {
	if errors.Is(b.err, errTransferTooLarge) {
		return tooLarge()
	}
	return server.InvalidRequest("request body is not a readable tar stream")
}

// watchedReader keeps the first error, other than its end, that its source
// gave, which a runtime reading it may report as its own.
type watchedReader struct {
	r   io.Reader
	err error
}

func (w *watchedReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && w.err == nil {
		w.err = err
	}
	return n, err
}

// tooLarge answers a transfer over [sandbox].max_transfer_size.
func tooLarge() error {
	return server.PayloadTooLarge("the transfer exceeds [sandbox].max_transfer_size")
}

// uploaded is the answer of an upload.
type uploaded struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Entries int    `json:"entries"`
}

// upload writes the body of r into the sandbox of u: as the file at the
// request's path, or, with archive=tar, as the entries of a tar stream
// under it. The body is streamed, so what was written before a refused tar
// entry or before the limit stays in the sandbox.
func (s *Service) upload(w http.ResponseWriter, r *http.Request, u useRequest) error {
	res, err := s.uploadBody(r, u)
	if err != nil {
		// The rest of a refused body is not wanted: the server would
		// otherwise read up to 256 KiB of it before answering.
		w.Header().Set("Connection", "close")
		return err
	}
	server.WriteJSON(w, http.StatusOK, res)
	return nil
}

func (s *Service) uploadBody(r *http.Request, u useRequest) (uploaded, error) {
	limit := s.settings.MaxTransferSize
	if r.ContentLength > limit {
		return uploaded{}, tooLarge()
	}
	body := &limitedBody{r: r.Body, left: limit}
	if u.files.tar {
		return s.unpack(r, u, body)
	}
	err := s.client.Upload(r.Context(), u.rec.ID, u.files.path, u.files.mode, body)
	// A runtime that stopped on the body's error reports it as its own.
	if failed := body.failed(); failed != nil {
		return uploaded{}, failed
	}
	if err != nil {
		return uploaded{}, err
	}
	return uploaded{Path: u.files.path, Bytes: body.read, Entries: 1}, nil
}

// unpack writes the directories and regular files of the tar stream body
// under the request's path, which it creates when the stream is readable,
// and stops at the first entry it refuses.
func (s *Service) unpack(r *http.Request, u useRequest, body *limitedBody) (uploaded, error) {
	ctx, id, top := r.Context(), u.rec.ID, u.files.path
	res := uploaded{Path: top}
	made := false
	makeTop := func() error {
		if made {
			return nil
		}
		made = true
		return s.client.MakeDir(ctx, id, top, 0o755)
	}
	tr := tar.NewReader(body)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return res, makeTop()
		}
		if err != nil {
			return res, body.notTar()
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			// Metadata for the entries after it, not an entry.
			continue
		}
		target, err := entryPath(top, hdr.Name)
		if err != nil {
			return res, err
		}
		mode := hdr.FileInfo().Mode().Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := makeTop(); err != nil {
				return res, err
			}
			if err := s.client.MakeDir(ctx, id, target, mode); err != nil {
				return res, err
			}
		case tar.TypeReg:
			if err := makeTop(); err != nil {
				return res, err
			}
			src := &watchedReader{r: tr}
			err := s.client.Upload(ctx, id, target, mode, src)
			if src.err != nil {
				return res, body.notTar()
			}
			if err != nil {
				return res, err
			}
			res.Bytes += hdr.Size
		default:
			return res, server.InvalidRequest(fmt.Sprintf("tar entry %q is neither a regular file nor a directory", cut(hdr.Name)))
		}
		res.Entries++
	}
}

// entryPath is the place of the tar entry name under the directory top,
// refused with 400 path_outside_workdir when the name is absolute or leaves
// top.
func entryPath(top, name string) (string, error) {
	p := path.Join(top, name)
	if strings.HasPrefix(name, "/") || !within(p, top) {
		return "", server.NewError(errorTypePathOutside, http.StatusBadRequest, fmt.Sprintf("tar entry %q is outside the upload's path", cut(name)))
	}
	return p, nil
}

// cut shortens a name a client sent to what an error message quotes.
func cut(name string) string {
	const most = 256
	if len(name) <= most {
		return name
	}
	return name[:most] + "…"
}
