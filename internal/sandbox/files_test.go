package sandbox_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

// uploaded is the body of an upload's answer.
type uploaded struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Entries int    `json:"entries"`
}

// uploadOf decodes the answer of an upload that must have succeeded.
func uploadOf(t *testing.T, a answer) uploaded {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("upload = %d %s, want 200", a.status, a.body)
	}
	var u uploaded
	if err := json.Unmarshal(a.body, &u); err != nil {
		t.Fatalf("the answer %s is not an upload's: %v", a.body, err)
	}
	return u
}

func TestUpload_WritesTheBodyAsTheFileAtThePath(t *testing.T) {
	h, id := startFilesHub(t)
	for _, tc := range []struct {
		name, query, body string
		path              string
		mode              fs.FileMode
	}{
		{"relative path, default mode", "path=a.txt", "hello", "/workspace/a.txt", 0o644},
		{"missing parents, mode given", "path=d/e/f.bin&mode=0755", "\x00\xff", "/workspace/d/e/f.bin", 0o755},
		{"an existing file replaced", "path=/workspace/a.txt&mode=600", "new", "/workspace/a.txt", 0o600},
		{"dot segments, empty body", "path=./x/../y.txt", "", "/workspace/y.txt", 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := uploadOf(t, h.files(t, http.MethodPut, id, tc.query, "application/octet-stream", strings.NewReader(tc.body)))
			if want := (uploaded{Path: tc.path, Bytes: int64(len(tc.body)), Entries: 1}); got != want {
				t.Errorf("upload = %+v, want %+v", got, want)
			}
			data, mode, ok := h.file(t, id, tc.path)
			if !ok || string(data) != tc.body || mode != tc.mode {
				t.Errorf("%s = %q mode %04o (exists %v), want %q mode %04o", tc.path, data, mode, ok, tc.body, tc.mode)
			}
		})
	}
}

func TestUpload_UnpacksATarStreamUnderThePath(t *testing.T) {
	h, id := startFilesHub(t)
	archive := tarOf(t,
		tarEntry{name: "src/", mode: 0o755},
		tarEntry{name: "src/main.go", mode: 0o644, body: "package main"},
		tarEntry{name: "run.sh", mode: 0o755, body: "#!/bin/sh"},
	)
	for _, contentType := range []string{"application/octet-stream", "application/x-tar"} {
		t.Run(contentType, func(t *testing.T) {
			got := uploadOf(t, h.files(t, http.MethodPut, id, "path=proj&archive=tar", contentType, bytes.NewReader(archive)))
			if want := (uploaded{Path: "/workspace/proj", Bytes: 21, Entries: 3}); got != want {
				t.Errorf("upload = %+v, want %+v", got, want)
			}
			for _, f := range []struct {
				path, body string
				mode       fs.FileMode
			}{
				{"/workspace/proj/src/main.go", "package main", 0o644},
				{"/workspace/proj/run.sh", "#!/bin/sh", 0o755},
			} {
				data, mode, ok := h.file(t, id, f.path)
				if !ok || string(data) != f.body || mode != f.mode {
					t.Errorf("%s = %q mode %04o (exists %v), want %q mode %04o", f.path, data, mode, ok, f.body, f.mode)
				}
			}
		})
	}
	t.Run("an empty archive", func(t *testing.T) {
		got := uploadOf(t, h.files(t, http.MethodPut, id, "path=empty&archive=tar", "application/x-tar", bytes.NewReader(tarOf(t))))
		if want := (uploaded{Path: "/workspace/empty"}); got != want {
			t.Errorf("upload = %+v, want %+v", got, want)
		}
	})
}

func TestFiles_RefuseAPathOutsideTheWorkingDirectory(t *testing.T) {
	h, id := startFilesHub(t)
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		var body io.Reader
		if method == http.MethodPut {
			body = strings.NewReader("x")
		}
		for _, p := range []string{"../x", "/etc/passwd", "a/../../b", "/workspace/../etc/x", "/workspace2/x"} {
			t.Run(method+" "+p, func(t *testing.T) {
				before := h.fileCalls()
				a := h.files(t, method, id, "path="+p, "application/octet-stream", body)
				if a.status != http.StatusBadRequest || a.err.ErrorType != "path_outside_workdir" {
					t.Errorf("%s %s = %d %s, want 400 path_outside_workdir", method, p, a.status, a.body)
				}
				if n := h.fileCalls() - before; n != 0 {
					t.Errorf("the runtime got %d file calls, want none", n)
				}
			})
		}
		for _, p := range []string{"/workspace", ".", "a/../b"} {
			t.Run(method+" "+p+" inside", func(t *testing.T) {
				if a := h.files(t, method, id, "path="+p, "application/octet-stream", body); a.err.ErrorType == "path_outside_workdir" {
					t.Errorf("%s %s = %d %s, want it not refused as outside the working directory", method, p, a.status, a.body)
				}
			})
		}
	}
	for _, tc := range []struct {
		name    string
		archive []byte
		absent  string
	}{
		{"an entry that climbs out", tarOf(t, tarEntry{name: "ok.txt", mode: 0o644, body: "ok"}, tarEntry{name: "../evil.txt", mode: 0o644, body: "evil"}), "/workspace/evil.txt"},
		{"an absolute entry", tarOf(t, tarEntry{name: "/abs.txt", mode: 0o644, body: "abs"}), "/abs.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.files(t, http.MethodPut, id, "path=proj&archive=tar", "application/x-tar", bytes.NewReader(tc.archive))
			if a.status != http.StatusBadRequest || a.err.ErrorType != "path_outside_workdir" {
				t.Errorf("upload = %d %s, want 400 path_outside_workdir", a.status, a.body)
			}
			if _, _, ok := h.file(t, id, tc.absent); ok {
				t.Errorf("%s exists, want no file at the escaped location", tc.absent)
			}
		})
	}
	if _, _, ok := h.file(t, id, "/workspace/proj/ok.txt"); !ok {
		t.Error("/workspace/proj/ok.txt is missing, want the entry before the refused one kept")
	}
}

func TestUpload_RefusesAnInvalidRequest(t *testing.T) {
	h, id := startFilesHub(t)
	const octets, xtar = "application/octet-stream", "application/x-tar"
	oneThenLink := tarOf(t, tarEntry{name: "one", mode: 0o644, body: "1"}, tarEntry{name: "l", link: "/etc"})
	hardLink := tarOf(t, tarEntry{name: "one", mode: 0o644, body: "1"}, tarEntry{name: "two", hardLink: "one"})
	for _, tc := range []struct {
		name, query, contentType string
		body                     []byte
		status                   int
		errorType                string
		// files says whether the runtime may get file calls: a refused
		// query or content type reaches none.
		files bool
	}{
		{"no path", "", octets, []byte("x"), 400, "invalid_request", false},
		{"an empty path", "path=", octets, []byte("x"), 400, "invalid_request", false},
		{"an archive other than tar", "path=a&archive=zip", octets, []byte("x"), 400, "invalid_request", false},
		{"a mode that is not octal", "path=a&mode=999", octets, []byte("x"), 400, "invalid_request", false},
		{"a mode that is not a number", "path=a&mode=rwx", octets, []byte("x"), 400, "invalid_request", false},
		{"a mode above 0777", "path=a&mode=1777", octets, []byte("x"), 400, "invalid_request", false},
		{"a mode with a tar archive", "path=a&archive=tar&mode=644", octets, tarOf(t), 400, "invalid_request", false},
		{"a body that is not a tar stream", "path=a&archive=tar", octets, []byte("not a tar"), 400, "invalid_request", true},
		{"a symbolic link entry", "path=a&archive=tar", xtar, oneThenLink, 400, "invalid_request", true},
		{"a hard link entry", "path=a&archive=tar", xtar, hardLink, 400, "invalid_request", true},
		{"a JSON body", "path=a", "application/json", []byte(`{}`), 415, "unsupported_media_type", false},
		{"a tar content type without an archive", "path=a", xtar, []byte("x"), 415, "unsupported_media_type", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := h.fileCalls()
			a := h.files(t, http.MethodPut, id, tc.query, tc.contentType, bytes.NewReader(tc.body))
			if a.status != tc.status || a.err.ErrorType != tc.errorType {
				t.Errorf("upload = %d %s, want %d %s", a.status, a.body, tc.status, tc.errorType)
			}
			if n := h.fileCalls() - before; !tc.files && n != 0 {
				t.Errorf("the runtime got %d file calls, want none", n)
			}
		})
	}
	if _, _, ok := h.file(t, id, "/workspace/a/one"); !ok {
		t.Error("/workspace/a/one is missing, want the entry before the refused link kept")
	}
	if _, _, ok := h.file(t, id, "/workspace/a/l"); ok {
		t.Error("/workspace/a/l exists, want no link written")
	}
}

func TestUpload_RefusesABodyOverTheTransferLimit(t *testing.T) {
	h, id := startFilesHub(t)
	const limit = 1 << 20

	t.Run("a declared length over the limit", func(t *testing.T) {
		before := h.fileCalls()
		a := h.files(t, http.MethodPut, id, "path=big", "application/octet-stream", bytes.NewReader(make([]byte, limit+1)))
		if a.status != http.StatusRequestEntityTooLarge || a.err.ErrorType != "payload_too_large" {
			t.Errorf("upload = %d %s, want 413 payload_too_large", a.status, a.body)
		}
		if n := h.fileCalls() - before; n != 0 {
			t.Errorf("the runtime got %d file calls, want none", n)
		}
	})

	big := tarOf(t, tarEntry{name: "big.bin", mode: 0o644, body: strings.Repeat("x", 2<<20)})
	for _, tc := range []struct {
		name, query string
		body        io.Reader
	}{
		{"a file without a declared length", "path=big", bytes.NewReader(make([]byte, 4<<20))},
		{"a tar stream without a declared length", "path=t&archive=tar", bytes.NewReader(big)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The body gives no more than the limit and 64 KiB until the
			// answer came: a hub that read past them would wait for more
			// until the deadline.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			body := &gatedBody{ctx: ctx, r: tc.body, limit: limit + 65536}
			a := h.filesWith(t, ctx, http.MethodPut, id, tc.query, "application/octet-stream", body)
			cancel()
			if a.status != http.StatusRequestEntityTooLarge || a.err.ErrorType != "payload_too_large" {
				t.Errorf("upload = %d %s, want 413 payload_too_large", a.status, a.body)
			}
			if taken := body.taken.Load(); taken > limit+65536 {
				t.Errorf("the hub read %d bytes, want at most %d", taken, limit+65536)
			}
		})
	}

	t.Run("a body of exactly the limit", func(t *testing.T) {
		got := uploadOf(t, h.files(t, http.MethodPut, id, "path=ok", "application/octet-stream", bytes.NewReader(make([]byte, limit))))
		if got.Bytes != limit {
			t.Errorf("upload wrote %d bytes, want %d", got.Bytes, limit)
		}
	})
}

// untarred is an entry of a downloaded tar stream.
type untarred struct {
	dir  bool
	mode fs.FileMode
	body string
}

// untar reads the tar stream a by entry name.
func untar(t *testing.T, a answer) map[string]untarred {
	t.Helper()
	if a.status != http.StatusOK || a.header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("download = %d %q %s, want 200 application/x-tar", a.status, a.header.Get("Content-Type"), a.body)
	}
	out := map[string]untarred{}
	tr := tar.NewReader(bytes.NewReader(a.body))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read the tar stream: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read the tar entry %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = untarred{dir: hdr.Typeflag == tar.TypeDir, mode: hdr.FileInfo().Mode().Perm(), body: string(body)}
	}
}

func TestDownload_SendsTheBytesOfAFile(t *testing.T) {
	h, ready := startFilesHub(t)
	ids := map[string]string{sandbox.StateReady: ready}
	for _, state := range []string{sandbox.StateCompleted, sandbox.StateFailed} {
		ids[state] = h.sandbox(t, func(r *sandbox.Record) { r.State = state })
	}
	for state, id := range ids {
		h.put(t, id,
			tarEntry{name: "/workspace/out.txt", body: "result"},
			tarEntry{name: "/workspace/bin/data", body: "\x00\xff\x10"},
			tarEntry{name: "/workspace/empty"},
		)
		for _, tc := range []struct{ path, body string }{
			{"out.txt", "result"},
			{"/workspace/bin/data", "\x00\xff\x10"},
			{"empty", ""},
		} {
			t.Run(state+" "+tc.path, func(t *testing.T) {
				a, err := h.download(t, id, "path="+tc.path)
				if err != nil {
					t.Fatalf("read the answer: %v", err)
				}
				if a.status != http.StatusOK || a.header.Get("Content-Type") != "application/octet-stream" {
					t.Fatalf("download = %d %q %s, want 200 application/octet-stream", a.status, a.header.Get("Content-Type"), a.body)
				}
				if got, want := a.header.Get("Content-Length"), strconv.Itoa(len(tc.body)); got != want {
					t.Errorf("Content-Length = %q, want %q", got, want)
				}
				if string(a.body) != tc.body {
					t.Errorf("body = %q, want %q", a.body, tc.body)
				}
			})
		}
	}
}

func TestDownload_SendsADirectoryOrAFileAsATarStream(t *testing.T) {
	h, id := startFilesHub(t)
	h.put(t, id,
		tarEntry{name: "/workspace/proj/src/main.go", body: "package main"},
		tarEntry{name: "/workspace/proj/run.sh", mode: 0o755, body: "#!/bin/sh"},
		tarEntry{name: "/workspace/out.txt", body: "result"},
		tarEntry{name: "/workspace/emptydir/"},
	)
	t.Run("a directory", func(t *testing.T) {
		got := untar(t, h.mustDownload(t, id, "path=proj&archive=tar"))
		delete(got, "./")
		want := map[string]untarred{
			"src/":        {dir: true, mode: 0o755},
			"src/main.go": {mode: 0o644, body: "package main"},
			"run.sh":      {mode: 0o755, body: "#!/bin/sh"},
		}
		if !maps.Equal(got, want) {
			t.Errorf("entries = %+v, want %+v", got, want)
		}
	})
	t.Run("a file", func(t *testing.T) {
		got := untar(t, h.mustDownload(t, id, "path=out.txt&archive=tar"))
		if want := map[string]untarred{"out.txt": {mode: 0o644, body: "result"}}; !maps.Equal(got, want) {
			t.Errorf("entries = %+v, want %+v", got, want)
		}
	})
	t.Run("an empty directory", func(t *testing.T) {
		for name, e := range untar(t, h.mustDownload(t, id, "path=emptydir&archive=tar")) {
			if !e.dir {
				t.Errorf("entry %s is a file, want no file in the archive of an empty directory", name)
			}
		}
	})
}

func TestDownload_RefusesARequestItCannotServe(t *testing.T) {
	h, id := startFilesHub(t)
	h.put(t, id, tarEntry{name: "/workspace/proj/"}, tarEntry{name: "/workspace/out.txt", body: "result"})
	gone := h.sandbox(t, func(r *sandbox.Record) { r.State = sandbox.StateFailed })
	h.put(t, gone, tarEntry{name: "/workspace/out.txt", body: "result"})
	h.rt.DropWorkDir("parley", gone)
	for _, tc := range []struct {
		name, sandbox, query string
		status               int
		errorType            string
	}{
		{"no path", id, "", 400, "invalid_request"},
		{"an empty path", id, "path=", 400, "invalid_request"},
		{"an archive other than tar", id, "path=out.txt&archive=zip", 400, "invalid_request"},
		{"a directory without an archive", id, "path=proj", 400, "path_is_directory"},
		{"the working directory without an archive", id, "path=.", 400, "path_is_directory"},
		{"a missing file", id, "path=missing.txt", 404, "path_not_found"},
		{"a missing directory as a tar stream", id, "path=missing&archive=tar", 404, "path_not_found"},
		{"a sandbox whose working directory is gone", gone, "path=out.txt", 404, "path_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.mustDownload(t, tc.sandbox, tc.query)
			if a.status != tc.status || a.err.ErrorType != tc.errorType {
				t.Errorf("download = %d %s, want %d %s", a.status, a.body, tc.status, tc.errorType)
			}
			if ct := a.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want the JSON error envelope", ct)
			}
		})
	}
}

func TestDownload_RefusesMoreThanTheTransferLimit(t *testing.T) {
	h, id := startFilesHub(t)
	const limit = 1 << 20
	h.put(t, id,
		tarEntry{name: "/workspace/big.bin", body: strings.Repeat("b", limit+1)},
		tarEntry{name: "/workspace/exact.bin", body: strings.Repeat("e", limit)},
		tarEntry{name: "/workspace/bigdir/1", body: strings.Repeat("1", 600000)},
		tarEntry{name: "/workspace/bigdir/2", body: strings.Repeat("2", 600000)},
		tarEntry{name: "/workspace/bigdir/3", body: strings.Repeat("3", 600000)},
	)
	t.Run("a file over the limit", func(t *testing.T) {
		a := h.mustDownload(t, id, "path=big.bin")
		if a.status != http.StatusRequestEntityTooLarge || a.err.ErrorType != "payload_too_large" {
			t.Errorf("download = %d %s, want 413 payload_too_large", a.status, a.body)
		}
	})
	t.Run("a file of exactly the limit", func(t *testing.T) {
		a := h.mustDownload(t, id, "path=exact.bin")
		if a.status != http.StatusOK || len(a.body) != limit {
			t.Errorf("download = %d with %d bytes, want 200 with %d", a.status, len(a.body), limit)
		}
	})
	t.Run("a tar stream that passes the limit", func(t *testing.T) {
		a, err := h.download(t, id, "path=bigdir&archive=tar")
		if a.status != http.StatusOK {
			t.Fatalf("download = %d %s, want 200", a.status, a.body)
		}
		if err == nil {
			t.Error("the body ended cleanly, want the read to end with an error")
		}
		if len(a.body) > limit {
			t.Errorf("received %d bytes, want at most %d", len(a.body), limit)
		}
	})
}
