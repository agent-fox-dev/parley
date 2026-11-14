package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// secretItem is a secret as the hub lists it, without a value.
const secretItem = `{"key":"A","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:30:15Z"}`

// The secret routes of the scopes the tests use, as sent.
const (
	userSecrets      = "/api/v1/user/secrets"
	orgSecrets       = "/api/v1/orgs/o/secrets"
	workspaceSecrets = "/api/v1/workspaces/w/secrets"
)

// runSecrets runs parley secrets with args against hub, with stdin as the
// standard input.
func runSecrets(t *testing.T, hub *stubHub, stdin string, args ...string) (int, string, string) {
	t.Helper()
	return runWithStdin(t, stdin, onHub(hub.URL, append([]string{"secrets"}, args...)...)...)
}

// createdHub is a stub hub that answers a create on every scope the tests
// use with 201 and secretItem, except the paths in refused, which it
// answers with their envelope.
func createdHub(t *testing.T, refused map[string]stubAnswer) *stubHub {
	t.Helper()
	hub := newStubHub(t)
	for _, path := range []string{userSecrets, orgSecrets, workspaceSecrets} {
		a, ok := refused[path]
		if !ok {
			a = stubAnswer{http.StatusCreated, `{"items":[` + secretItem + `]}`}
		}
		hub.answer(http.MethodPost, path, a.status, a.body)
	}
	return hub
}

// createResults is the document parley secrets create prints for the
// scopes that succeeded against a createdHub.
func createResults(scopes ...string) string {
	results := []string{}
	for _, s := range scopes {
		results = append(results, `{"scope":"`+s+`","items":[`+secretItem+`]}`)
	}
	return `{"results":[` + strings.Join(results, ",") + `]}`
}

// envelope is the error envelope the hub answers with status and errorType.
func envelope(status int, errorType string) string {
	return fmt.Sprintf(`{"error":{"code":%d,"message":"refused","error_type":%q}}`, status, errorType)
}

// errorTypes are the error types of the envelopes stderr holds, in order.
func errorTypes(t *testing.T, stderr string) []string {
	t.Helper()
	types := []string{}
	dec := json.NewDecoder(strings.NewReader(stderr))
	for {
		var doc struct {
			Error struct {
				ErrorType string `json:"error_type"`
			} `json:"error"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return types
		}
		if err != nil {
			t.Fatalf("stderr is not a sequence of envelopes (%v): %q", err, stderr)
		}
		types = append(types, doc.Error.ErrorType)
	}
}

// wantPaths fails the test unless hub got method on exactly paths, as sent,
// in order.
func wantPaths(t *testing.T, hub *stubHub, method string, paths ...string) []stubRequest {
	t.Helper()
	sent := hub.recorded()
	got := []string{}
	for _, r := range sent {
		got = append(got, r.method+" "+r.rawPath)
	}
	want := []string{}
	for _, p := range paths {
		want = append(want, method+" "+p)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	return sent
}

func TestSecretsCreate_PostsEveryEntryToEachScopeInOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		paths   []string
		scopes  []string
		entries string
	}{
		{"one entry, to the user without a flag", []string{"A=1"},
			[]string{userSecrets}, []string{"user"}, `[{"key":"A","value":"1"}]`},
		{"arguments split at commas and entries at the first =", []string{"A=1,B=x=y", "C="},
			[]string{userSecrets}, []string{"user"}, `[{"key":"A","value":"1"},{"key":"B","value":"x=y"},{"key":"C","value":""}]`},
		{"every scope, in the order user, org, workspace", []string{"A=1", "--workspace", "w", "--org", "o", "--user"},
			[]string{userSecrets, orgSecrets, workspaceSecrets}, []string{"user", "org", "workspace"}, `[{"key":"A","value":"1"}]`},
		{"the org alone", []string{"A=1", "--org", "o"},
			[]string{orgSecrets}, []string{"org"}, `[{"key":"A","value":"1"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := createdHub(t, nil)

			code, stdout, stderr := runSecrets(t, hub, "", append([]string{"create"}, tt.args...)...)

			if code != 0 || stderr != "" {
				t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
			}
			for _, r := range wantPaths(t, hub, http.MethodPost, tt.paths...) {
				wantJSONBody(t, r.body, `{"entries":`+tt.entries+`}`)
			}
			if want := indented(t, createResults(tt.scopes...)); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
		})
	}
}

func TestSecretsCreate_GoesOnAfterAScopeFailsAndPrintsEachErrorOnce(t *testing.T) {
	t.Parallel()
	unavailable := stubAnswer{http.StatusServiceUnavailable, envelope(503, "secrets_unavailable")}
	tests := []struct {
		name    string
		refused map[string]stubAnswer
		scopes  []string
		errors  []string
	}{
		{"one scope refused",
			map[string]stubAnswer{orgSecrets: {http.StatusConflict, envelope(409, "conflict")}},
			[]string{"user", "workspace"}, []string{"conflict"}},
		{"every scope refused",
			map[string]stubAnswer{userSecrets: unavailable, orgSecrets: unavailable, workspaceSecrets: unavailable},
			nil, []string{"secrets_unavailable", "secrets_unavailable", "secrets_unavailable"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := createdHub(t, tt.refused)

			code, stdout, stderr := runSecrets(t, hub, "", "create", "A=1", "--user", "--org", "o", "--workspace", "w")

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			wantPaths(t, hub, http.MethodPost, userSecrets, orgSecrets, workspaceSecrets)
			if want := indented(t, createResults(tt.scopes...)); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			if got := errorTypes(t, stderr); !reflect.DeepEqual(got, tt.errors) {
				t.Errorf("stderr holds the envelopes %q, want %q:\n%s", got, tt.errors, stderr)
			}
		})
	}
}

func TestSecrets_ReadTheValueFromStdinWithoutOneLineEnding(t *testing.T) {
	t.Parallel()
	values := []struct{ stdin, value string }{
		{"abc\n", "abc"},
		{"abc\r\n", "abc"},
		{"abc\n\n", "abc\n"},
		{"a,b=c", "a,b=c"},
		{"", ""},
		{"line1\nline2", "line1\nline2"},
	}
	commands := []struct {
		name, method, rawPath string
		body                  func(value string) map[string]any
	}{
		{"create", http.MethodPost, userSecrets, func(v string) map[string]any {
			return map[string]any{"entries": []map[string]string{{"key": "K", "value": v}}}
		}},
		{"update", http.MethodPatch, userSecrets + "/K", func(v string) map[string]any {
			return map[string]any{"value": v}
		}},
	}
	for _, c := range commands {
		for _, v := range values {
			t.Run(fmt.Sprintf("%s %q", c.name, v.stdin), func(t *testing.T) {
				t.Parallel()
				hub := newStubHub(t).
					answer(http.MethodPost, userSecrets, http.StatusCreated, `{"items":[`+secretItem+`]}`).
					answer(http.MethodPatch, userSecrets+"/K", http.StatusOK, secretItem)

				code, _, stderr := runSecrets(t, hub, v.stdin, c.name, "K", "--from-stdin")

				if code != 0 {
					t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
				}
				sent := wantPaths(t, hub, c.method, c.rawPath)
				want, _ := json.Marshal(c.body(v.value))
				wantJSONBody(t, sent[0].body, string(want))
			})
		}
	}
}

func TestSecretsList_PrintsTheItemsOfEveryPageAsOneDocument(t *testing.T) {
	t.Parallel()
	item := func(n int) string {
		return fmt.Sprintf(`{"key":"K%d","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:3%d:15Z"}`, n, n)
	}
	pages := map[string]string{
		"":   `{"items":[` + item(1) + `,` + item(2) + `],"next_cursor":"c1"}`,
		"c1": `{"items":[` + item(3) + `,` + item(4) + `],"next_cursor":"c2"}`,
		"c2": `{"items":[` + item(5) + `,` + item(6) + `],"next_cursor":null}`,
	}
	paged := func(w http.ResponseWriter, r *http.Request) {
		answerWith(http.StatusOK, pages[r.URL.Query().Get("cursor")])(w, r)
	}
	tests := []struct {
		name    string
		flags   []string
		rawPath string
		queries []string
		items   []int
	}{
		{"three pages of a workspace", []string{"--workspace", "w"}, workspaceSecrets,
			[]string{"limit=200", "cursor=c1&limit=200", "cursor=c2&limit=200"}, []int{1, 2, 3, 4, 5, 6}},
		{"the user without a flag", nil, userSecrets, []string{"limit=200"}, []int{5, 6}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).handle(http.MethodGet, workspaceSecrets, paged).
				answer(http.MethodGet, userSecrets, http.StatusOK, pages["c2"])

			code, stdout, stderr := runSecrets(t, hub, "", append([]string{"list"}, tt.flags...)...)

			if code != 0 || stderr != "" {
				t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
			}
			paths := make([]string, len(tt.queries))
			for i := range paths {
				paths[i] = tt.rawPath
			}
			queries := []string{}
			for _, r := range wantPaths(t, hub, http.MethodGet, paths...) {
				queries = append(queries, r.query)
			}
			if !reflect.DeepEqual(queries, tt.queries) {
				t.Errorf("queries = %q, want %q", queries, tt.queries)
			}
			items := []string{}
			for _, n := range tt.items {
				items = append(items, item(n))
			}
			if want := indented(t, `{"items":[`+strings.Join(items, ",")+`]}`); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
		})
	}
}

func TestSecretsUpdateAndDelete_SendTheKeyAsOneSegmentOfTheScope(t *testing.T) {
	t.Parallel()
	notFound := envelope(404, "not_found")
	tests := []struct {
		name, method, rawPath string
		args                  []string
		status                int
		answer, body          string
		// stdout is the document printed, "" for none; errorType that of
		// the envelope on stderr, "" for none.
		stdout, errorType string
		code              int
	}{
		{"update an org's secret", http.MethodPatch, orgSecrets + "/A", []string{"update", "A=x=y", "--org", "o"},
			http.StatusOK, secretItem, `{"value":"x=y"}`, secretItem, "", 0},
		{"delete a workspace's secret", http.MethodDelete, workspaceSecrets + "/A", []string{"delete", "A", "--workspace", "w"},
			http.StatusNoContent, "", "", `{"deleted":"A"}`, "", 0},
		{"delete a key holding a slash", http.MethodDelete, userSecrets + "/a%2Fb", []string{"delete", "a/b", "--user"},
			http.StatusNoContent, "", "", `{"deleted":"a/b"}`, "", 0},
		{"update a key the scope does not hold", http.MethodPatch, userSecrets + "/A", []string{"update", "A=1"},
			http.StatusNotFound, notFound, `{"value":"1"}`, "", "not_found", 1},
		{"delete a key the scope does not hold", http.MethodDelete, orgSecrets + "/A", []string{"delete", "A", "--org", "o"},
			http.StatusNotFound, notFound, "", "", "not_found", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, _ := url.PathUnescape(tt.rawPath)
			hub := newStubHub(t).answer(tt.method, path, tt.status, tt.answer)

			code, stdout, stderr := runSecrets(t, hub, "", tt.args...)

			if code != tt.code {
				t.Errorf("exit status = %d, want %d (stderr %q)", code, tt.code, stderr)
			}
			sent := wantPaths(t, hub, tt.method, tt.rawPath)
			if tt.body != "" {
				wantJSONBody(t, sent[0].body, tt.body)
			}
			want := ""
			if tt.stdout != "" {
				want = indented(t, tt.stdout)
			}
			if stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			wantErrors := []string{}
			if tt.errorType != "" {
				wantErrors = append(wantErrors, tt.errorType)
			}
			if got := errorTypes(t, stderr); !reflect.DeepEqual(got, wantErrors) {
				t.Errorf("stderr holds the envelopes %q, want %q", got, wantErrors)
			}
		})
	}
}

func TestSecrets_RefuseMalformedArgumentsBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	tests := [][]string{
		{"create"},
		{"create", "A"},
		{"create", "=v"},
		{"create", "A=1,B"},
		{"create", "A", "B", "--from-stdin"},
		{"create", "A=1", "--from-stdin"},
		{"create", "A,B", "--from-stdin"},
		{"create", "A=1", "--workspace", ""},
		{"update", "A=1,B=2"},
		{"update", "A=1", "B=2"},
		{"update", "A"},
		{"delete"},
		{"delete", "A", "B"},
		{"list", "--user", "--org", "o"},
		{"update", "A=1", "--org", "o", "--workspace", "w"},
		{"delete", "A", "--user", "--workspace", "w"},
		{"list", "--org", ""},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			code, stdout, stderr := runSecrets(t, hub, "v", args...)

			wantClientFailure(t, code, stdout, stderr, usageErrorType)
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}
