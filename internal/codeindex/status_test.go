package codeindex_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// status is the answer of the status route.
type status struct {
	Workspace    string  `json:"workspace"`
	Enabled      bool    `json:"enabled"`
	State        string  `json:"state"`
	HeadSHA      *string `json:"head_sha"`
	IndexedSHA   *string `json:"indexed_sha"`
	Partial      bool    `json:"partial"`
	FilesIndexed int     `json:"files_indexed"`
	BuiltAt      *string `json:"built_at"`
	Error        *string `json:"error"`
}

// statusOf is the status of the workspace slug as the owner reads it.
func (w *world) statusOf(t *testing.T, slug string) status {
	t.Helper()
	code, body := w.Call(t, http.MethodGet, "/api/v1/workspaces/"+slug+"/scm/code-index", w.key, nil)
	var s status
	// Every member is there, null or not.
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); code != http.StatusOK || err != nil || json.Unmarshal(body, &s) != nil {
		t.Fatalf("GET scm/code-index of %s = %d %s, want 200 with the status", slug, code, body)
	}
	for _, m := range []string{"workspace", "enabled", "state", "head_sha", "indexed_sha", "partial", "files_indexed", "built_at", "error"} {
		if _, has := members[m]; !has {
			t.Errorf("the status of %s has no member %s: %s", slug, m, body)
		}
	}
	return s
}

// str is the string p points at, "<null>" for nil.
func str(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

func TestStatus_AnswersTheStateOfTheIndex(t *testing.T) {
	t.Parallel()
	t.Run("ready, then building", func(t *testing.T) {
		t.Parallel()
		s := newSeam()
		w := newWorld(t, enabled, s.adjust)
		head := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n", "b.go": "package a\n"})
		rec := w.indexed(t, "demo")
		got := w.statusOf(t, "demo")
		built, err := time.Parse(time.RFC3339, str(got.BuiltAt))
		if got.Workspace != "demo" || !got.Enabled || got.State != "ready" || str(got.HeadSHA) != head || str(got.IndexedSHA) != head ||
			got.Partial || got.FilesIndexed != 2 || got.Error != nil || err != nil || built.Sub(*rec.BuiltAt).Abs() > time.Second {
			t.Errorf("the status = %+v (built_at %s, %v), want ready at %s with 2 files, built at %s", got, str(got.BuiltAt), err, head, rec.BuiltAt)
		}

		held := string(filepath.Separator) + "demo" + string(filepath.Separator)
		t.Cleanup(s.hold(held))
		next := w.commit(t, "demo", "main", map[string]string{"c.go": "package a\n"})
		s.waitBegan(t, held)
		got = w.statusOf(t, "demo")
		if got.State != "building" || str(got.HeadSHA) != next || str(got.IndexedSHA) != head || got.FilesIndexed != 2 || got.BuiltAt == nil {
			t.Errorf("while the build is held the status = %+v, want building, the head %s and the index there is, of %s", got, next, head)
		}
	})
	t.Run("queued, then failed", func(t *testing.T) {
		t.Parallel()
		s := newSeam()
		w := newClockedWorld(t, s)
		head := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
		w.queue(t, "demo")
		w.run(t, "demo")
		next := w.noticed(t, "demo", map[string]string{"b.go": "package a\n"})
		got := w.statusOf(t, "demo")
		if got.State != "queued" || str(got.HeadSHA) != next || str(got.IndexedSHA) != head || got.Error != nil {
			t.Errorf("before the build is claimed the status = %+v, want queued, the head %s and the index of %s", got, next, head)
		}

		s.failWith(func(string) error { return errors.New("the disk went away") })
		w.run(t, "demo")
		got = w.statusOf(t, "demo")
		if got.State != "failed" || str(got.IndexedSHA) != head || got.FilesIndexed != 1 || !strings.Contains(str(got.Error), "the disk went away") {
			t.Errorf("after the failed build the status = %+v (error %s), want failed with its error and the index of %s", got, str(got.Error), head)
		}
	})
	t.Run("a partial index", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, "[code_index]\nmax_files = 1\n")
		head := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n", "b.go": "package a\n", "c.go": "package a\n"})
		w.indexed(t, "demo")
		if got := w.statusOf(t, "demo"); got.State != "ready" || !got.Partial || got.FilesIndexed != 1 || str(got.IndexedSHA) != head {
			t.Errorf("the status = %+v, want ready and partial with 1 file at %s", got, head)
		}
	})
	t.Run("no repository yet", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, enabled)
		ctx, now := context.Background(), time.Now().UTC()
		rec := workspace.Workspace{
			Slug: "soon", Kind: access.KindRepository, Mode: workspace.ModeStandard, GitURL: new("https://origin.test/soon.git"),
			OwnerID: w.owner, OrgID: w.org, Status: workspace.StatusPending, DisplayName: "soon",
			CreatedAt: now, UpdatedAt: now, InstanceID: workspace.NewInstanceID(),
		}
		if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { return workspace.Insert(ctx, tx, rec) }); err != nil {
			t.Fatal(err)
		}
		if got := w.statusOf(t, "soon"); !got.Enabled || got.State != "queued" || got.HeadSHA != nil || got.IndexedSHA != nil ||
			got.Partial || got.FilesIndexed != 0 || got.BuiltAt != nil || got.Error != nil {
			t.Errorf("the status = %+v, want enabled and queued with no head and no index", got)
		}
	})
}

func TestStatus_AnswersOffWhereNoIndexIsKept(t *testing.T) {
	t.Parallel()
	t.Run("a hub without the section", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, "")
		head := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
		if got := w.statusOf(t, "demo"); got.Enabled || got.State != "off" || got.IndexedSHA != nil || str(got.HeadSHA) != head || got.Workspace != "demo" {
			t.Errorf("the status = %+v, want not enabled, off, no index and the head %s", got, head)
		}
	})
	t.Run("a workspace whose setting is off", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, enabled)
		w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
		w.indexed(t, "demo")
		if code, body := w.Call(t, http.MethodPatch, "/api/v1/workspaces/demo", w.key, map[string]any{"settings": map[string]any{"code_index": false}}); code != http.StatusOK {
			t.Fatalf("PATCH code_index false = %d %s, want 200", code, body)
		}
		if got := w.statusOf(t, "demo"); got.Enabled || got.State != "off" {
			t.Errorf("right after the setting was turned off the status = %+v, want not enabled and off", got)
		}
		w.pass(t)
		if got := w.statusOf(t, "demo"); got.Enabled || got.State != "off" || got.IndexedSHA != nil || got.FilesIndexed != 0 || got.BuiltAt != nil {
			t.Errorf("after a maintenance pass the status = %+v, want off with nothing left of the index", got)
		}
	})
}

func TestStatus_AnswersForTheParentOfAFork(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	head := w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "demo")
	if code, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/demo/forks", w.key, map[string]any{"name": "x", "auto_change": false}); code != http.StatusCreated {
		t.Fatalf("create the fork demo--x = %d %s, want 201", code, body)
	}
	w.commit(t, "demo--x", "work", map[string]string{"x.go": "package a\n"})

	fork, parent := w.statusOf(t, "demo--x"), w.statusOf(t, "demo")
	if fork.Workspace != "demo" || fork.State != "ready" || str(fork.IndexedSHA) != head || str(fork.HeadSHA) != head {
		t.Errorf("the fork's status = %+v, want its parent's: demo, ready at %s", fork, head)
	}
	if fork.State != parent.State || str(fork.BuiltAt) != str(parent.BuiltAt) || fork.FilesIndexed != parent.FilesIndexed || fork.Enabled != parent.Enabled {
		t.Errorf("the fork's status = %+v, want every member its parent's, %+v", fork, parent)
	}
}

func TestStatus_ShowsAFailureScrubbedAndCut(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-very-secret-value"
	s := newSeam()
	s.failWith(func(string) error {
		return errors.New("cannot reach https://example.invalid with the token " + secret + ": " + strings.Repeat("é", 2500))
	})
	w := newComposedWorld(t, hubtest.Config{TOML: enabled, Compose: func(c *hub.Composition) {
		c.AddStartHook("code-index-test-secret", func(_ context.Context, rt *hub.Runtime) error {
			rt.Scrubber.Add(secret)
			return nil
		})
	}}, s.adjust)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	w.queue(t, "demo")
	hubtest.Eventually(t, "the build to fail", func() bool { return w.record(t, "demo").Error != "" })

	got := w.statusOf(t, "demo")
	text := str(got.Error)
	if got.State != "failed" || got.Error == nil || len(text) > 1024 || len(text) < 512 || strings.Contains(text, secret) || !json.Valid([]byte(`"`+strings.ReplaceAll(text, `"`, ``)+`"`)) {
		t.Errorf("the status = %+v with an error of %d bytes, want failed with an error of at most 1,024 bytes without the secret", got, len(text))
	}
	if !strings.Contains(text, "é") || strings.ToValidUTF8(text, "") != text {
		t.Errorf("the error %q is not cut between two characters", text[len(text)-8:])
	}
}

func TestStatus_AppliesTheAccessRuleOfTheSourceControlReads(t *testing.T) {
	t.Parallel()
	w := newWorld(t, enabled)
	w.addWorkspace(t, "demo", map[string]string{"a.go": "package a\n"})
	outsider := w.IssueKey(t, w.CreateUser(t, "outsider"))
	pat := func(perm string) string {
		return w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{perm}, CreatedAt: time.Now().UTC()})
	}
	path := "/api/v1/workspaces/demo/scm/code-index"
	for _, tc := range []struct {
		name, cred string
		code       int
		errorType  string
	}{
		{"no credential", "", http.StatusUnauthorized, "unauthenticated"},
		{"a PAT without git:read", pat("changes:read"), http.StatusForbidden, "insufficient_scope"},
		{"a caller who may not read demo", outsider, http.StatusNotFound, "not_found"},
		{"a PAT with git:read", pat("git:read"), http.StatusOK, ""},
		{"the owner", w.key, http.StatusOK, ""},
	} {
		code, body := w.Call(t, http.MethodGet, path, tc.cred, nil)
		var envelope struct {
			Error struct {
				ErrorType string `json:"error_type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &envelope)
		if code != tc.code || envelope.Error.ErrorType != tc.errorType {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, body, tc.code, tc.errorType)
		}
	}
	// The two requests the route answered are counted, the refused ones not.
	if got := w.metricsText(t); !strings.Contains(got, `parley_scm_requests_total{op="code_index",result="ok"} 2`) {
		t.Errorf("GET /metrics does not count the two answered requests under the op code_index:\n%s", grep(got, "parley_scm_requests_total"))
	}
}

func TestMetrics_CountsTheBuildsAndMeasuresTheDisk(t *testing.T) {
	t.Parallel()
	s := newSeam()
	w := newWorld(t, enabled, s.adjust)
	w.addWorkspace(t, "whole", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "whole")
	s.mu.Lock()
	s.partial = true
	s.mu.Unlock()
	w.addWorkspace(t, "part", map[string]string{"a.go": "package a\n"})
	w.indexed(t, "part")
	s.failWith(func(tree string) error {
		if strings.Contains(tree, string(filepath.Separator)+"broken"+string(filepath.Separator)) {
			return errors.New("no luck")
		}
		return nil
	})
	w.addWorkspace(t, "broken", map[string]string{"a.go": "package a\n"})
	w.queue(t, "broken")
	hubtest.Eventually(t, "the build of broken to fail", func() bool { return w.builds(t)["failed"] >= 1 })

	text := w.metricsText(t)
	// The failed build is retried after its delay, so it may have failed
	// more than once by now.
	n := w.builds(t)
	if n["ok"] != 1 || n["partial"] != 1 || n["failed"] < 1 {
		t.Errorf("builds by outcome = %v, want one ok, one partial and the failed one", n)
	}
	if !strings.Contains(text, "parley_code_index_build_duration_seconds_count ") ||
		strings.Contains(text, "parley_code_index_build_duration_seconds_count 0\n") || strings.Contains(text, "parley_code_index_build_duration_seconds_count 1\n") ||
		strings.Contains(text, "parley_code_index_build_duration_seconds_count 2\n") {
		t.Errorf("the duration histogram does not count the three builds or more:\n%s", grep(text, "parley_code_index_build_duration_seconds_count"))
	}
	if strings.Contains(text, "parley_code_index_bytes 0\n") || !strings.Contains(text, "parley_code_index_bytes ") {
		t.Errorf("parley_code_index_bytes is not above 0:\n%s", grep(text, "parley_code_index_bytes"))
	}

	for _, slug := range []string{"whole", "part", "broken"} {
		if code, body := w.Call(t, http.MethodPost, "/api/v1/workspaces/"+slug+"/archive", w.key, nil); code != http.StatusOK {
			t.Fatalf("archive %s = %d %s, want 200", slug, code, body)
		}
		if code, body := w.Call(t, http.MethodDelete, "/api/v1/workspaces/"+slug, w.key, nil); code >= 300 {
			t.Fatalf("delete %s = %d %s, want it deleted", slug, code, body)
		}
	}
	w.pass(t)
	if text := w.metricsText(t); !strings.Contains(text, "parley_code_index_bytes 0\n") {
		t.Errorf("after the workspaces are deleted and a pass ran, parley_code_index_bytes is not 0:\n%s", grep(text, "parley_code_index_bytes"))
	}
}

// grep is the lines of text that hold part.
func grep(text, part string) string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, part) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
