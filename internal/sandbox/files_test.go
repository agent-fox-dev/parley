package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"time"
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
