package harness

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Issue is an issue (or, with PullRequest, a pull request) the fake forge
// holds.
type Issue struct {
	Number      int
	Title, Body string
	// State is open unless set.
	State       string
	PullRequest bool
}

// Forge is a fake GitHub API over TLS on a loopback port, serving
// repositories and their issues below URL. A workspace reaches it with the
// setting forge_api_url = URL and an origin whose path is /<owner>/<repo>.git
// (WithRepoPath); the hub must be started with Env to trust it.
type Forge struct {
	// URL is the API's base, https://127.0.0.1:<port>/github.
	URL    string
	caFile string
	mu     sync.Mutex
	issues map[string][]Issue
	got    []string
}

// NewForge starts a fake forge holding no repository; it is closed when
// the test ends.
func NewForge(tb testing.TB) *Forge {
	tb.Helper()
	f := &Forge{issues: map[string][]Issue{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /github/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		full := f.record(r)
		if !f.has(full) {
			notFound(w)
			return
		}
		writeJSON(w, map[string]any{
			"full_name": full, "name": r.PathValue("repo"), "default_branch": "main", "private": false,
			"html_url": "https://github.example/" + full, "owner": map[string]any{"login": r.PathValue("owner")},
		})
	})
	mux.HandleFunc("GET /github/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		full := f.record(r)
		if !f.has(full) {
			notFound(w)
			return
		}
		f.mu.Lock()
		issues := append([]Issue(nil), f.issues[full]...)
		f.mu.Unlock()
		items := []map[string]any{}
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		for _, is := range issues {
			state := is.State
			if state == "" {
				state = "open"
			}
			item := map[string]any{
				"number": is.Number, "title": is.Title, "body": is.Body, "state": state,
				"html_url": "https://github.example/" + full + "/issues/" + itoa(is.Number),
				"user":     map[string]any{"login": "reporter"}, "labels": []any{},
				"created_at": at, "updated_at": at,
			}
			if is.PullRequest {
				item["pull_request"] = map[string]any{"url": "https://github.example/" + full + "/pull/" + itoa(is.Number)}
			}
			items = append(items, item)
		}
		writeJSON(w, items)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		notFound(w)
	})
	srv := httptest.NewTLSServer(mux)
	tb.Cleanup(srv.Close)
	f.URL = srv.URL + "/github"
	f.caFile = filepath.Join(tb.TempDir(), "forge-ca.pem")
	if err := os.WriteFile(f.caFile, certPEM(srv), 0o644); err != nil {
		tb.Fatal(err)
	}
	return f
}

// SetIssues makes the forge hold the repository owner/repo with the issues
// given.
func (f *Forge) SetIssues(owner, repo string, issues ...Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[owner+"/"+repo] = append([]Issue{}, issues...)
}

// Env is what a hub's environment needs to trust the forge (Options.Env).
func (f *Forge) Env() []string { return []string{"SSL_CERT_FILE=" + f.caFile} }

// Requests are the requests the forge received, as "METHOD path?query".
func (f *Forge) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// record notes r and returns the owner/repo it names.
func (f *Forge) record(r *http.Request) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, r.Method+" "+r.URL.RequestURI())
	return r.PathValue("owner") + "/" + r.PathValue("repo")
}

func (f *Forge) has(full string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.issues[full]
	return ok
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"Not Found"}`))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string { return strconv.Itoa(n) }
