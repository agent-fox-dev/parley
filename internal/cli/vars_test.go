package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// varItem is a variable as the hub answers it, value included.
const varItem = `{"key":"A","value":"1","created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:30:15Z"}`

// The variable routes of the scopes the tests use, as sent.
const (
	userVars      = "/api/v1/user/vars"
	orgVars       = "/api/v1/orgs/o/vars"
	workspaceVars = "/api/v1/workspaces/w/vars"
	resolvedVars  = "/api/v1/workspaces/w/vars/resolved"
)

// runVars runs parley vars with args against hub, with stdin as the
// standard input.
func runVars(t *testing.T, hub *stubHub, stdin string, args ...string) (int, string, string) {
	t.Helper()
	return runWithStdin(t, stdin, onHub(hub.URL, append([]string{"vars"}, args...)...)...)
}

// varsCreatedHub is a stub hub that answers a variable create on every
// scope the tests use with 201 and varItem, except the paths in refused,
// which it answers with their answer.
func varsCreatedHub(t *testing.T, refused map[string]stubAnswer) *stubHub {
	t.Helper()
	hub := newStubHub(t)
	for _, path := range []string{userVars, orgVars, workspaceVars} {
		a, ok := refused[path]
		if !ok {
			a = stubAnswer{http.StatusCreated, `{"items":[` + varItem + `]}`}
		}
		hub.answer(http.MethodPost, path, a.status, a.body)
	}
	return hub
}

// varsResults is the document parley vars create prints for the scopes
// that succeeded against a varsCreatedHub.
func varsResults(scopes ...string) string {
	results := []string{}
	for _, s := range scopes {
		results = append(results, `{"scope":"`+s+`","items":[`+varItem+`]}`)
	}
	return `{"results":[` + strings.Join(results, ",") + `]}`
}

func TestVarsCreate_SendsEveryEntryInOneBodyPerScopeInOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		stdin   string
		paths   []string
		scopes  []string
		entries string
	}{
		{"arguments split at commas and entries at the first =", []string{"A=1,B=x=y", "C="}, "",
			[]string{userVars}, []string{"user"}, `[{"key":"A","value":"1"},{"key":"B","value":"x=y"},{"key":"C","value":""}]`},
		{"a value from stdin without its last line ending", []string{"K", "--from-stdin"}, "line1\nline2\n",
			[]string{userVars}, []string{"user"}, `[{"key":"K","value":"line1\nline2"}]`},
		{"a value from stdin without its last CRLF", []string{"K", "--from-stdin"}, "v\r\n",
			[]string{userVars}, []string{"user"}, `[{"key":"K","value":"v"}]`},
		{"a value from stdin holding a comma and =", []string{"K", "--from-stdin"}, "a,b=c",
			[]string{userVars}, []string{"user"}, `[{"key":"K","value":"a,b=c"}]`},
		{"an empty value from stdin", []string{"K", "--from-stdin"}, "",
			[]string{userVars}, []string{"user"}, `[{"key":"K","value":""}]`},
		{"every scope, in the order user, org, workspace", []string{"A=1", "--workspace", "w", "--user", "--org", "o"}, "",
			[]string{userVars, orgVars, workspaceVars}, []string{"user", "org", "workspace"}, `[{"key":"A","value":"1"}]`},
		{"the workspace alone", []string{"A=1", "--workspace", "w"}, "",
			[]string{workspaceVars}, []string{"workspace"}, `[{"key":"A","value":"1"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := varsCreatedHub(t, nil)

			code, stdout, stderr := runVars(t, hub, tt.stdin, append([]string{"create"}, tt.args...)...)

			if code != 0 || stderr != "" {
				t.Errorf("exit status %d, stderr %q; want 0 and nothing", code, stderr)
			}
			for _, r := range wantPaths(t, hub, http.MethodPost, tt.paths...) {
				wantJSONBody(t, r.body, `{"entries":`+tt.entries+`}`)
			}
			if want := indented(t, varsResults(tt.scopes...)); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
		})
	}
}

func TestVarsCreate_GoesOnAfterAScopeFailsAndPrintsEachErrorOnce(t *testing.T) {
	t.Parallel()
	refusal := func(status int, errorType string) stubAnswer { return stubAnswer{status, envelope(status, errorType)} }
	tests := []struct {
		name    string
		refused map[string]stubAnswer
		scopes  []string
		// errors are the envelopes stderr holds, in order.
		errors []string
	}{
		{"the org refused",
			map[string]stubAnswer{orgVars: refusal(http.StatusNotFound, "not_found")},
			[]string{"user", "workspace"}, []string{envelope(404, "not_found")}},
		{"every scope refused",
			map[string]stubAnswer{
				userVars:      refusal(http.StatusTooManyRequests, "secret_limit_reached"),
				orgVars:       refusal(http.StatusNotFound, "not_found"),
				workspaceVars: refusal(http.StatusConflict, "conflict"),
			},
			nil, []string{envelope(429, "secret_limit_reached"), envelope(404, "not_found"), envelope(409, "conflict")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := varsCreatedHub(t, tt.refused)

			code, stdout, stderr := runVars(t, hub, "", "create", "A=1", "--user", "--org", "o", "--workspace", "w")

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			wantPaths(t, hub, http.MethodPost, userVars, orgVars, workspaceVars)
			if want := indented(t, varsResults(tt.scopes...)); stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}
			want := ""
			for _, e := range tt.errors {
				want += indented(t, e)
			}
			if stderr != want {
				t.Errorf("stderr = %q, want each refusal once, as received: %q", stderr, want)
			}
		})
	}
}

func TestVars_RefuseMalformedArgumentsBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		// mentions is what the message must hold.
		mentions string
	}{
		{[]string{"create", "A"}, "parley vars create"},
		{[]string{"create", "=v"}, "parley vars create"},
		{[]string{"create", "A=1,,B=2"}, "parley vars create"},
		{[]string{"create", "A=1", "--from-stdin"}, "parley vars create"},
		{[]string{"create", "A", "B", "--from-stdin"}, "parley vars create"},
		{[]string{"create", "", "--from-stdin"}, "parley vars create"},
		{[]string{"create"}, "parley vars create"},
		{[]string{"create", "A=1", "--org", ""}, "parley vars create"},
		{[]string{"update", "A=1,B=2"}, "parley vars update"},
		{[]string{"update", "A=1", "B=2"}, "parley vars update"},
		{[]string{"update", "A"}, "parley vars update"},
		{[]string{"list", "--user", "--org", "o"}, "parley vars list"},
		{[]string{"list", "extra"}, "parley vars list"},
		{[]string{"update", "A=1", "--org", "o", "--workspace", "w"}, "parley vars update"},
		{[]string{"delete", "A", "--user", "--workspace", "w"}, "parley vars delete"},
		{[]string{"delete"}, "parley vars delete"},
		{[]string{"delete", ""}, "parley vars delete"},
		{[]string{"delete", "A", "B"}, "parley vars delete"},
		{[]string{"resolve", "w", "--user"}, "--user"},
		{[]string{"resolve", "w", "--workspace", "w"}, "--workspace"},
		{[]string{"resolve"}, "parley vars resolve"},
		{[]string{"resolve", ""}, "parley vars resolve"},
		{[]string{"resolve", "w", "x"}, "parley vars resolve"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.args), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			code, stdout, stderr := runVars(t, hub, "v", tt.args...)

			message := wantClientFailure(t, code, stdout, stderr, usageErrorType)
			if !strings.Contains(message, tt.mentions) {
				t.Errorf("message = %q, want it to name %q", message, tt.mentions)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

func TestVarsListAndResolve_PrintTheItemsOfEveryPageAsOneDocument(t *testing.T) {
	t.Parallel()
	item := func(key, origin string) string {
		o := ""
		if origin != "" {
			o = `,"origin":"` + origin + `"`
		}
		return `{"key":"` + key + `","value":"v-` + key + `"` + o + `,"created_at":"2026-11-14T10:30:15Z","updated_at":"2026-11-14T10:31:15Z"}`
	}
	// paged answers the first page, then second for cursor c1.
	paged := func(first, second string, secondStatus int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "c1" {
				answerWith(secondStatus, second)(w, r)
				return
			}
			answerWith(http.StatusOK, first)(w, r)
		}
	}
	listed := `{"items":[` + item("a", "") + `,` + item("b", "") + `],"next_cursor":"c1"}`
	resolved := `{"items":[` + item("A", "workspace") + `],"next_cursor":"c1"}`
	tests := []struct {
		name    string
		args    []string
		rawPath string
		handler http.HandlerFunc
		// pages is how many pages are read.
		pages int
		// stdout is the document printed, "" for none; stderr the envelope
		// on stderr, "" for none.
		stdout, stderr string
		code           int
	}{
		{"an org's list over two pages", []string{"list", "--org", "o"}, orgVars,
			paged(listed, `{"items":[`+item("c", "")+`],"next_cursor":null}`, http.StatusOK), 2,
			`{"items":[` + item("a", "") + `,` + item("b", "") + `,` + item("c", "") + `]}`, "", 0},
		{"the user's list without a flag", []string{"list"}, userVars,
			answerWith(http.StatusOK, `{"items":[`+item("a", "")+`],"next_cursor":null}`), 1,
			`{"items":[` + item("a", "") + `]}`, "", 0},
		{"a workspace's resolved variables over two pages", []string{"resolve", "w"}, resolvedVars,
			paged(resolved, `{"items":[`+item("B", "org")+`],"next_cursor":null}`, http.StatusOK), 2,
			`{"items":[` + item("A", "workspace") + `,` + item("B", "org") + `]}`, "", 0},
		{"a list whose second page fails", []string{"list", "--org", "o"}, orgVars,
			paged(listed, envelope(500, "internal"), http.StatusInternalServerError), 2,
			"", envelope(500, "internal"), 1},
		{"a resolve whose second page fails", []string{"resolve", "w"}, resolvedVars,
			paged(resolved, envelope(500, "internal"), http.StatusInternalServerError), 2,
			"", envelope(500, "internal"), 1},
		{"a resolve of a workspace the caller cannot see", []string{"resolve", "w"}, resolvedVars,
			answerWith(http.StatusNotFound, envelope(404, "not_found")), 1,
			"", envelope(404, "not_found"), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).handle(http.MethodGet, tt.rawPath, tt.handler)

			code, stdout, stderr := runVars(t, hub, "", tt.args...)

			if code != tt.code {
				t.Errorf("exit status = %d, want %d (stderr %q)", code, tt.code, stderr)
			}
			wantQueries := []string{"limit=200", "cursor=c1&limit=200"}[:tt.pages]
			paths := make([]string, len(wantQueries))
			for i := range paths {
				paths[i] = tt.rawPath
			}
			queries := []string{}
			for _, r := range wantPaths(t, hub, http.MethodGet, paths...) {
				queries = append(queries, r.query)
			}
			if !reflect.DeepEqual(queries, wantQueries) {
				t.Errorf("queries = %q, want %q", queries, wantQueries)
			}
			wantOut, wantErr := "", ""
			if tt.stdout != "" {
				wantOut = indented(t, tt.stdout)
			}
			if tt.stderr != "" {
				wantErr = indented(t, tt.stderr)
			}
			if stdout != wantOut {
				t.Errorf("stdout = %q, want %q", stdout, wantOut)
			}
			if stderr != wantErr {
				t.Errorf("stderr = %q, want %q", stderr, wantErr)
			}
		})
	}
}

func TestVarsUpdateAndDelete_SendTheKeyAsOneSegmentOfTheScope(t *testing.T) {
	t.Parallel()
	notFound := envelope(404, "not_found")
	tests := []struct {
		name, method, rawPath string
		args                  []string
		stdin                 string
		status                int
		answer, body          string
		// stdout is the document printed, "" for none; stderr the envelope
		// on stderr, "" for none.
		stdout, stderr string
		code           int
	}{
		{"update the user's variable", http.MethodPatch, userVars + "/A", []string{"update", "A=x=1"}, "",
			http.StatusOK, varItem, `{"value":"x=1"}`, varItem, "", 0},
		{"update a workspace's variable from stdin", http.MethodPatch, workspaceVars + "/A", []string{"update", "A", "--from-stdin", "--workspace", "w"}, "v\n",
			http.StatusOK, varItem, `{"value":"v"}`, varItem, "", 0},
		{"update a key holding a slash and a space", http.MethodPatch, orgVars + "/a%2Fb%20c", []string{"update", "a/b c=1", "--org", "o"}, "",
			http.StatusOK, varItem, `{"value":"1"}`, varItem, "", 0},
		{"update a key the scope does not hold", http.MethodPatch, userVars + "/A", []string{"update", "A=1"}, "",
			http.StatusNotFound, notFound, `{"value":"1"}`, "", notFound, 1},
		{"delete an org's variable", http.MethodDelete, orgVars + "/Region", []string{"delete", "Region", "--org", "o"}, "",
			http.StatusNoContent, "", "", `{"deleted":"Region"}`, "", 0},
		{"delete a key holding a slash", http.MethodDelete, workspaceVars + "/a%2Fb", []string{"delete", "a/b", "--workspace", "w"}, "",
			http.StatusNoContent, "", "", `{"deleted":"a/b"}`, "", 0},
		{"delete a key the scope does not hold", http.MethodDelete, orgVars + "/Region", []string{"delete", "Region", "--org", "o"}, "",
			http.StatusNotFound, notFound, "", "", notFound, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, _ := url.PathUnescape(tt.rawPath)
			hub := newStubHub(t).answer(tt.method, path, tt.status, tt.answer)

			code, stdout, stderr := runVars(t, hub, tt.stdin, tt.args...)

			if code != tt.code {
				t.Errorf("exit status = %d, want %d (stderr %q)", code, tt.code, stderr)
			}
			sent := wantPaths(t, hub, tt.method, tt.rawPath)
			if tt.body != "" {
				wantJSONBody(t, sent[0].body, tt.body)
			}
			wantOut, wantErr := "", ""
			if tt.stdout != "" {
				wantOut = indented(t, tt.stdout)
			}
			if tt.stderr != "" {
				wantErr = indented(t, tt.stderr)
			}
			if stdout != wantOut {
				t.Errorf("stdout = %q, want %q", stdout, wantOut)
			}
			if stderr != wantErr {
				t.Errorf("stderr = %q, want %q", stderr, wantErr)
			}
		})
	}
}
