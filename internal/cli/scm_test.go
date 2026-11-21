package cli

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// scmPath is the path of the scm route under the workspace demo, as sent.
func scmPath(route string) string { return "/api/v1/workspaces/demo/scm/" + route }

// scmCommand is parley scm sub followed by args.
func scmCommand(sub string, args ...string) []string {
	return append([]string{"scm", sub}, args...)
}

// The tests of this file set environment variables, so they must not run in
// parallel.

func TestSCM_ReadsSendOneGetAndPrintTheAnswer(t *testing.T) {
	tests := []struct {
		args           []string
		rawPath, query string
	}{
		{scmCommand("refs", "demo"), scmPath("refs"), ""},
		{scmCommand("refs", "demo", "--prefix", "refs/tags/", "--limit", "5", "--cursor", "c1"), scmPath("refs"), "cursor=c1&limit=5&prefix=refs%2Ftags%2F"},
		{scmCommand("refs", "a/b"), "/api/v1/workspaces/a%2Fb/scm/refs", ""},
		{scmCommand("log", "demo"), scmPath("commits"), ""},
		{
			scmCommand("log", "demo", "--ref", "main", "--path", "docs/a.md", "--follow", "--since", "2026-01-01T00:00:00Z",
				"--until", "2026-02-01T00:00:00Z", "--author", "ada", "--stats", "--limit", "10", "--cursor", "c1"),
			scmPath("commits"),
			"author=ada&cursor=c1&follow=true&limit=10&path=docs%2Fa.md&ref=main&since=2026-01-01T00%3A00%3A00Z&stats=true&until=2026-02-01T00%3A00%3A00Z",
		},
		{scmCommand("show", "demo", "main"), scmPath("commits/main"), ""},
		{scmCommand("show", "demo", "feature/x", "--files"), scmPath("commits/feature%2Fx"), "files=true"},
		{scmCommand("ls", "demo"), scmPath("tree"), ""},
		{scmCommand("ls", "demo", "docs", "--ref", "v1", "--recursive"), scmPath("tree"), "path=docs&recursive=true&ref=v1"},
		{scmCommand("diff", "demo", "main", "feature"), scmPath("diff"), "base=main&head=feature"},
		{scmCommand("diff", "demo", "a", "b", "--mode", "direct", "--path", "docs/**", "--path", "*.md"), scmPath("diff"), "base=a&head=b&mode=direct&path=docs%2F%2A%2A&path=%2A.md"},
		{scmCommand("compare", "demo", "main", "feature"), scmPath("compare"), "base=main&head=feature"},
		{scmCommand("blame", "demo", "a.go"), scmPath("blame"), "path=a.go"},
		{scmCommand("blame", "demo", "a.go", "--ref", "v1", "--lines", "3:9"), scmPath("blame"), "end_line=9&path=a.go&ref=v1&start_line=3"},
		{scmCommand("outline", "demo", "internal/api/router.go"), scmPath("outline"), "path=internal%2Fapi%2Frouter.go"},
		{scmCommand("outline", "demo", "internal/api/router.go", "--ref", "v1"), scmPath("outline"), "path=internal%2Fapi%2Frouter.go&ref=v1"},
		{scmCommand("code-index", "demo"), scmPath("code-index"), ""},
		{scmCommand("code-search", "demo", "sym:Handler lang:go"), scmPath("code-search"), "q=sym%3AHandler+lang%3Ago"},
		{scmCommand("code-search", "demo", "sym:Handler lang:go", "--limit", "5"), scmPath("code-search"), "limit=5&q=sym%3AHandler+lang%3Ago"},
		{scmCommand("symbols", "demo", "NewRouter"), scmPath("symbols"), "name=NewRouter"},
		{
			scmCommand("symbols", "demo", "NewRouter", "--exact", "--kind", "func", "--path", "internal/api", "--limit", "5"),
			scmPath("symbols"), "exact=true&kind=func&limit=5&name=NewRouter&path=internal%2Fapi",
		},
		{scmCommand("search", "demo", "TODO"), scmPath("search"), "q=TODO"},
		{
			scmCommand("search", "demo", "to do", "--ref", "v1", "--path", "src/**", "--path", "*.go", "--regex", "--ignore-case"),
			scmPath("search"), "case_sensitive=false&path=src%2F%2A%2A&path=%2A.go&q=to+do&ref=v1&regex=true",
		},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, sentRequest{http.MethodGet, tt.rawPath, tt.query, ""})
		})
	}
}

// fileBytes is every byte value once, the file the stub hub serves raw.
func fileBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// fileHub is a stub hub that answers GET …/scm/file of demo with status and
// body, as the hub answers the raw form.
func fileHub(t *testing.T, status int, body []byte) *stubHub {
	t.Helper()
	return newStubHub(t).handle(http.MethodGet, scmPath("file"), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		w.WriteHeader(status)
		w.Write(body)
	})
}

func TestSCMCat_WritesTheFileBytes(t *testing.T) {
	hub := fileHub(t, http.StatusOK, fileBytes())

	code, stdout, stderr := runAgainst(t, hub, scmCommand("cat", "demo", "bin.dat", "--ref", "v1")...)

	if code != 0 || stderr != "" {
		t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
	}
	if !bytes.Equal([]byte(stdout), fileBytes()) {
		t.Errorf("stdout = %q, want the file's bytes", stdout)
	}
	sent := hub.recorded()
	if len(sent) != 1 || sent[0].method != http.MethodGet || sent[0].query != "path=bin.dat&ref=v1" {
		t.Fatalf("the hub got %v, want one GET ?path=bin.dat&ref=v1", sent)
	}
	if accept := sent[0].header.Get("Accept"); strings.Contains(accept, "application/json") {
		t.Errorf("Accept = %q, want the raw form asked for", accept)
	}

	out := filepath.Join(t.TempDir(), "f")
	code, stdout, stderr = runAgainst(t, hub, scmCommand("cat", "demo", "bin.dat", "--output", out)...)

	if code != 0 || stdout != "" || stderr != "" {
		t.Errorf("exit status %d, stdout %q, stderr %q; want 0, nothing and nothing", code, stdout, stderr)
	}
	if got, err := os.ReadFile(out); err != nil || !bytes.Equal(got, fileBytes()) {
		t.Errorf("%s holds %q (%v), want the file's bytes", out, got, err)
	}
}

func TestSCMCat_WritesNothingOnAFailure(t *testing.T) {
	notFound := []byte(`{"error":{"code":404,"message":"path \"nope\" does not exist","error_type":"path_not_found"}}`)
	dir := t.TempDir()
	tests := []struct {
		name string
		// existing is what the output file holds before the run, nil for no file.
		existing  []byte
		status    int
		body      []byte
		out       string
		wantCode  int
		errorType string
	}{
		{"a hub error, no file", nil, http.StatusNotFound, notFound, filepath.Join(dir, "new"), 1, "path_not_found"},
		{"a hub error, a file", []byte("keep"), http.StatusNotFound, notFound, filepath.Join(dir, "kept"), 1, "path_not_found"},
		{"an output that cannot be written", nil, http.StatusOK, fileBytes(), filepath.Join(dir, "missing-dir", "f"), 2, "client_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.existing != nil {
				if err := os.WriteFile(tt.out, tt.existing, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			hub := fileHub(t, tt.status, tt.body)

			code, stdout, stderr := runAgainst(t, hub, scmCommand("cat", "demo", "nope", "--output", tt.out)...)

			if code != tt.wantCode || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want %d and nothing", code, stdout, tt.wantCode)
			}
			if _, _, errorType := clientEnvelope(t, stderr); errorType != tt.errorType {
				t.Errorf("error_type = %q, want %q", errorType, tt.errorType)
			}
			if tt.existing == nil {
				wantAbsent(t, tt.out)
			} else if got, err := os.ReadFile(tt.out); err != nil || !bytes.Equal(got, tt.existing) {
				t.Errorf("%s holds %q (%v), want %q unchanged", tt.out, got, err, tt.existing)
			}
		})
	}
}

func TestSCMDiff_PrintsThePatchTextOrTheNamesOnly(t *testing.T) {
	const patch = "diff --git a/x b/x\nindex 1111111..2222222 100644\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n"
	hub := newStubHub(t).handle(http.MethodGet, scmPath("diff"), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
		w.Write([]byte(patch))
	})

	code, stdout, stderr := runAgainst(t, hub, scmCommand("diff", "demo", "main", "feature", "--patch")...)

	if code != 0 || stderr != "" || stdout != patch {
		t.Errorf("exit status %d, stdout %q, stderr %q; want 0, the patch as received and nothing", code, stdout, stderr)
	}
	if sent := hub.recorded(); len(sent) != 1 || sent[0].query != "base=main&format=patch&head=feature" {
		t.Errorf("the hub got %v, want one GET ?base=main&format=patch&head=feature", sent)
	}

	wantSends(t, scmCommand("diff", "demo", "main", "feature", "--name-only"),
		sentRequest{http.MethodGet, scmPath("diff"), "base=main&head=feature&patches=false", ""})
}

func TestSCMCompare_FailsOnAConflictWhenAsked(t *testing.T) {
	answer := func(mergeable string) string {
		return `{"base_sha": "` + strings.Repeat("a", 40) + `", "mergeable": "` + mergeable + `", "conflict": null}`
	}
	tests := []struct {
		mergeable string
		flags     []string
		want      int
	}{
		{"conflict", []string{"--fail-on-conflict"}, 3},
		{"clean", []string{"--fail-on-conflict"}, 0},
		{"conflict", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.mergeable+" "+strings.Join(tt.flags, " "), func(t *testing.T) {
			hub := newStubHub(t).answer(http.MethodGet, scmPath("compare"), http.StatusOK, answer(tt.mergeable))

			code, stdout, stderr := runAgainst(t, hub, scmCommand("compare", append([]string{"demo", "main", "feature"}, tt.flags...)...)...)

			if code != tt.want {
				t.Errorf("exit status = %d, want %d (stderr %q)", code, tt.want, stderr)
			}
			if want := indented(t, answer(tt.mergeable)); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if tt.want == 0 {
				if stderr != "" {
					t.Errorf("stderr = %q, want it empty", stderr)
				}
				return
			}
			if gotCode, message, errorType := clientEnvelope(t, stderr); gotCode != 0 || errorType != "condition_not_met" || message == "" {
				t.Errorf("envelope code %v, error_type %q, message %q; want 0, condition_not_met and a message", gotCode, errorType, message)
			}
		})
	}
}

func TestSCM_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	tests := [][]string{
		{"scm", "refs"},
		{"scm", "show", "demo"},
		{"scm", "cat", "demo"},
		{"scm", "diff", "demo", "main"},
		{"scm", "compare", "demo", "a", "b", "c"},
		{"scm", "diff", "demo", "a", "b", "--mode", "sideways"},
		{"scm", "diff", "demo", "a", "b", "--patch", "--name-only"},
		{"scm", "blame", "demo", "f", "--lines", "3"},
		{"scm", "blame", "demo", "f", "--lines", "a:b"},
		{"scm", "blame", "demo", "f", "--lines", "0:2"},
		{"scm", "outline"},
		{"scm", "outline", "demo"},
		{"scm", "outline", "demo", "a.go", "b.go"},
		{"scm", "code-index"},
		{"scm", "code-search", "demo"},
		{"scm", "code-search", "demo", "a", "b"},
		{"scm", "code-search", "demo", "a", "--limit", "many"},
		{"scm", "symbols", "demo"},
		{"scm", "symbols", "demo", "a", "--limit", "many"},
		{"scm", "code-index", "demo", "extra"},
		{"scm", "refs", "demo", "--limit", "many"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			hub := newStubHub(t)

			code, stdout, stderr := runAgainst(t, hub, args...)

			wantClientFailure(t, code, stdout, stderr, "usage_error")
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

func TestSCM_WithoutACommandPrintsItsHelp(t *testing.T) {
	code, stdout, stderr := runCLI(t, "scm")

	if code != 0 || stderr != "" {
		t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
	}
	for _, sub := range []string{"refs", "log", "show", "ls", "cat", "diff", "compare", "blame", "outline", "code-index", "code-search", "symbols", "search", "commit", "rebase"} {
		if !slices.ContainsFunc(nonEmptyLines(stdout), func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), sub+" ") }) {
			t.Errorf("stdout does not list %s:\n%s", sub, stdout)
		}
	}
}
