package scm_test

import (
	"encoding/base64"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// The files the file tests add to the tree workspace's main, beside what
// addTree writes there.
var (
	binDat = func() string {
		b := make([]byte, 256)
		for i := range b {
			b[i] = byte(i)
		}
		return string(b)
	}()
	utf8Txt   = "Grüße, 世界 👋\n"
	digitsTxt = strings.Repeat("0123456789", 10)
)

// fileWorld is a world whose tree workspace also holds bin.dat (the bytes
// 0x00 to 0xff), empty.txt, utf8.txt (multi-byte text), digits.txt (100
// bytes), edge.dat (1,024 bytes) and big.dat (1,025 bytes), and the git
// directory of that workspace.
func fileWorld(t *testing.T, adjust ...func(*scm.Deps)) (*world, string) {
	t.Helper()
	w := newWorld(t, adjust...)
	return w, w.addTree(t, "bin.dat", binDat, "empty.txt", "", "utf8.txt", utf8Txt, "digits.txt", digitsTxt,
		"edge.dat", strings.Repeat("e", 1024), "big.dat", strings.Repeat("b", 1025))
}

func fileURL(path string) string {
	return scmPath("tree", "/file?ref=main&path="+url.QueryEscape(path))
}

func TestFile_AnswersTheBytesOfAFileAtARef(t *testing.T) {
	w, dir := fileWorld(t)
	id := func(rev string) string { return strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", rev)) }
	for _, c := range []struct{ path, body, mode string }{
		{"README.md", treeReadme, "100644"},
		{"run.sh", "#!/bin/sh\necho run\n", "100755"},
		{"bin.dat", binDat, "100644"},
		{"empty.txt", "", "100644"},
		{"link", "README.md", "120000"},
	} {
		t.Run(c.path, func(t *testing.T) {
			r := w.get(t, fileURL(c.path), w.ownerKey)
			if r.status != http.StatusOK || string(r.body) != c.body {
				t.Fatalf("%d %q, want 200 %q", r.status, r.body, c.body)
			}
			for name, want := range map[string]string{
				"Content-Type": "application/octet-stream", "Content-Length": strconv.Itoa(len(c.body)),
				"X-Parley-Blob-Sha": id("main:" + c.path), "X-Parley-Resolved-Sha": id("main"), "X-Parley-File-Mode": c.mode,
			} {
				if got := r.header.Get(name); got != want {
					t.Errorf("%s: %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestFile_AnswersOneRangeOfAFile(t *testing.T) {
	w, dir := fileWorld(t)
	head := strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "main"))
	for _, c := range []struct {
		rng, body, contentRange string
		status                  int
	}{
		{"bytes=0-9", digitsTxt[0:10], "bytes 0-9/100", http.StatusPartialContent},
		{"bytes=90-", digitsTxt[90:], "bytes 90-99/100", http.StatusPartialContent},
		{"bytes=-5", digitsTxt[95:], "bytes 95-99/100", http.StatusPartialContent},
		{"bytes=50-500", digitsTxt[50:], "bytes 50-99/100", http.StatusPartialContent},
		{"bytes=-500", digitsTxt, "bytes 0-99/100", http.StatusPartialContent},
		{"bytes=0-1,5-6", digitsTxt, "", http.StatusOK},
		{"bytes=9-2", digitsTxt, "", http.StatusOK},
		{"lines=0-9", digitsTxt, "", http.StatusOK},
	} {
		t.Run(c.rng, func(t *testing.T) {
			r := w.get(t, fileURL("digits.txt"), w.ownerKey, "Range", c.rng)
			if r.status != c.status || string(r.body) != c.body || r.header.Get("Content-Range") != c.contentRange {
				t.Errorf("%d %q, Content-Range %q; want %d %q, %q", r.status, r.body, r.header.Get("Content-Range"), c.status, c.body, c.contentRange)
			}
			if r.header.Get("X-Parley-Resolved-Sha") != head || r.header.Get("Content-Length") != strconv.Itoa(len(c.body)) {
				t.Errorf("X-Parley-Resolved-Sha %q, Content-Length %q; want %s, %d", r.header.Get("X-Parley-Resolved-Sha"), r.header.Get("Content-Length"), head, len(c.body))
			}
		})
	}
	if r := w.get(t, fileURL("digits.txt"), w.ownerKey, "Range", "bytes=0-9", "Accept", "application/json"); r.status != http.StatusOK || r.json["size"] != float64(100) || r.json["content"] != digitsTxt {
		t.Errorf("a range with the JSON form: %d %s, want 200 with the whole file", r.status, r.body)
	}
}

func TestFile_AnswersAFileAsJSON(t *testing.T) {
	w, dir := fileWorld(t)
	id := func(rev string) string { return strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", rev)) }
	for _, c := range []struct{ path, body, encoding string }{
		{"README.md", treeReadme, "utf8"},
		{"utf8.txt", utf8Txt, "utf8"},
		{"bin.dat", binDat, "base64"},
		{"empty.txt", "", "utf8"},
	} {
		t.Run(c.path, func(t *testing.T) {
			r := w.get(t, fileURL(c.path), w.ownerKey, "Accept", "application/json")
			if r.status != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
				t.Fatalf("%d %s (Content-Type %q), want 200 JSON", r.status, r.body, r.header.Get("Content-Type"))
			}
			if keys, want := slices.Sorted(maps.Keys(r.json)), []string{"content", "encoding", "mode", "path", "resolved_sha", "sha", "size"}; !slices.Equal(keys, want) {
				t.Errorf("members %v, want %v", keys, want)
			}
			content, _ := r.json["content"].(string)
			if c.encoding == "base64" {
				b, err := base64.StdEncoding.DecodeString(content)
				if err != nil {
					t.Fatalf("content %q: %v", content, err)
				}
				content = string(b)
			}
			if r.json["encoding"] != c.encoding || content != c.body || r.json["size"] != float64(len(c.body)) || r.json["mode"] != "100644" ||
				r.json["path"] != c.path || r.json["sha"] != id("main:"+c.path) || r.json["resolved_sha"] != id("main") {
				t.Errorf("%s, want encoding %s, the file's bytes, size %d, mode 100644 and the blob and commit ids", r.body, c.encoding, len(c.body))
			}
		})
	}
}

func TestFile_RefusesAPathOrRangeItCannotServe(t *testing.T) {
	w, _ := fileWorld(t)
	for _, c := range []struct {
		query, rng string
		status     int
		want       string
	}{
		{"", "", http.StatusBadRequest, "invalid_request"},
		{"path=", "", http.StatusBadRequest, "invalid_request"},
		{"path=docs", "", http.StatusBadRequest, "invalid_request"},
		{"path=vendor/sub", "", http.StatusBadRequest, "invalid_request"},
		{"path=../x", "", http.StatusBadRequest, "invalid_request"},
		{"path=.git/config", "", http.StatusBadRequest, "invalid_request"},
		{"path=nope.txt", "", http.StatusNotFound, "path_not_found"},
		{"path=docs/nope.md", "", http.StatusNotFound, "path_not_found"},
		{"path=README.md", "bytes=99999-", http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable"},
		{"path=empty.txt", "bytes=-5", http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable"},
	} {
		for _, accept := range []string{"", "application/json"} {
			if c.rng != "" && accept != "" {
				continue
			}
			t.Run(c.query+" "+c.rng+" "+accept, func(t *testing.T) {
				r := w.get(t, scmPath("tree", "/file?ref=main&"+c.query), w.ownerKey, "Range", c.rng, "Accept", accept)
				e, _ := r.json["error"].(map[string]any)
				if r.status != c.status || r.errorType() != c.want || e["code"] != float64(c.status) || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
					t.Errorf("%d %s, want the %d %s envelope", r.status, r.body, c.status, c.want)
				}
				if size := strconv.Itoa(len(map[string]string{"path=README.md": treeReadme, "path=empty.txt": ""}[c.query])); c.rng != "" && r.header.Get("Content-Range") != "bytes */"+size {
					t.Errorf("Content-Range %q, want bytes */%s", r.header.Get("Content-Range"), size)
				}
			})
		}
	}
}

func TestFile_RefusesAFileOverTheSizeLimit(t *testing.T) {
	w, _ := fileWorld(t, func(d *scm.Deps) { d.MaxFileSize = 1024 })
	for _, accept := range []string{"", "application/json"} {
		for _, rng := range []string{"", "bytes=0-9"} {
			r := w.get(t, fileURL("big.dat"), w.ownerKey, "Accept", accept, "Range", rng)
			e, _ := r.json["error"].(map[string]any)
			if r.status != http.StatusRequestEntityTooLarge || r.errorType() != "payload_too_large" || e["size"] != float64(1025) {
				t.Errorf("big.dat (Accept %q, Range %q): %d %s, want 413 payload_too_large with size 1025", accept, rng, r.status, r.body)
			}
		}
	}
	if r := w.get(t, fileURL("edge.dat"), w.ownerKey); r.status != http.StatusOK || len(r.body) != 1024 {
		t.Errorf("edge.dat, exactly the limit: %d with %d bytes, want 200 with 1,024", r.status, len(r.body))
	}
}

// The resolver's forms of a revision, as the file route's ref parameter
// takes them, the raw form naming the commit in a header.
func TestFile_ReadsTheCommitItsRefNames(t *testing.T) {
	w := newWorld(t)
	unreachable := commit(t, w.repo, w.id["c2"], "only.txt", "only\n")
	w.set(t, "demo", "default_branch", "dev")
	for _, c := range []struct{ query, sha, body string }{
		{"path=f.txt", w.id["c1"], "c1\n"},
		{"ref=main&path=f.txt", w.id["c2"], "c2\n"},
		{"ref=v2&path=f.txt", w.id["c2"], "c2\n"},
		{"ref=" + w.id["c3"][:7] + "&path=x.txt", w.id["c3"], "c3\n"},
		{"ref=" + unreachable + "&path=only.txt", unreachable, "only\n"},
	} {
		r := w.get(t, scmPath("demo", "/file?"+c.query), w.ownerKey)
		if r.status != http.StatusOK || r.header.Get("X-Parley-Resolved-Sha") != c.sha || string(r.body) != c.body {
			t.Errorf("GET /file?%s = %d %q (X-Parley-Resolved-Sha %q), want 200 %q from %s", c.query, r.status, r.body, r.header.Get("X-Parley-Resolved-Sha"), c.body, c.sha)
		}
	}
	if r := w.get(t, scmPath("demo", "/file?ref=main~1&path=f.txt"), w.ownerKey); r.status != http.StatusNotFound || r.errorType() != "ref_not_found" {
		t.Errorf("ref=main~1: %d %s, want 404 ref_not_found", r.status, r.body)
	}
}
