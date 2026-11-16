package cli

import (
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// policyURL is the path of the policy route of the workspace api, as sent.
const policyURL = "/api/v1/workspaces/api/policy"

// policyJSON is a document with a rule for docs, one for billing and one
// that blocks secrets.
const policyJSON = `{
  "default": {"approvals": 0},
  "rules": [
    {"name": "docs", "paths": ["docs/**"], "require": {}},
    {"name": "billing", "paths": ["billing/**"], "require": {"approvals": 1, "checks": ["test", "billing-e2e"]}},
    {"name": "secrets", "paths": ["deploy/secrets/**"], "require": {"block": true}}
  ]
}`

// policyTOML is a document in TOML, and policyFromTOML the same document
// in JSON.
const (
	policyTOML = `[default]
approvals = 0
checks = ["test"]

[[rules]]
name = "docs"
paths = ["docs/**"]

[rules.require]
approvals = 0

[[rules]]
name = "billing"
paths = ["billing/**"]

[rules.require]
approvals = 1
block = false
`
	policyFromTOML = `{"default": {"approvals": 0, "checks": ["test"]}, "rules": [
  {"name": "docs", "paths": ["docs/**"], "require": {"approvals": 0}},
  {"name": "billing", "paths": ["billing/**"], "require": {"approvals": 1, "block": false}}]}`
)

// policyHub is a stub hub that answers the three policy routes of api with
// 200 and hubAnswer.
func policyHub(t *testing.T) *stubHub {
	t.Helper()
	return newStubHub(t).
		answer(http.MethodGet, policyURL, http.StatusOK, hubAnswer).
		answer(http.MethodPut, policyURL, http.StatusOK, hubAnswer).
		answer(http.MethodPost, policyURL+"/evaluate", http.StatusOK, hubAnswer)
}

// runPolicy runs parley policy with args against a new policyHub, with
// stdin as the standard input, and returns what it printed and the
// requests it sent.
func runPolicy(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string, sent []stubRequest) {
	t.Helper()
	hub := policyHub(t)
	opts := defaultOptions()
	opts.stdin = strings.NewReader(stdin)
	code, stdout, stderr = runCLIWith(t, opts, onHub(hub.URL, append([]string{"policy"}, args...)...)...)
	return code, stdout, stderr, hub.recorded()
}

func TestPolicyGet_AsksForTheRevisionGivenAndPrintsTheAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		flags []string
		query url.Values
	}{
		{nil, nil},
		{[]string{"--revision", "3"}, url.Values{"revision": {"3"}}},
		{[]string{"--revision", "0"}, url.Values{"revision": {"0"}}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.flags, " "), func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr, sent := runPolicy(t, "", append([]string{"get", "api"}, tt.flags...)...)

			got := wantOnlyRequest(t, sent, http.MethodGet, policyURL)
			wantQuery(t, got.query, tt.query)
			if got.body != "" {
				t.Errorf("body = %q, want none", got.body)
			}
			wantResult(t, code, stdout, stderr, hubAnswer)
		})
	}
}

func TestPolicySet_SendsTheFilesDocumentUnderTheRevisionCondition(t *testing.T) {
	t.Parallel()
	file := localFile(t, "policy.json", []byte(policyJSON))
	tests := []struct {
		flags   []string
		ifMatch []string
	}{
		{nil, nil},
		{[]string{"--if-revision", "7"}, []string{`"7"`}},
		{[]string{"--if-revision", "0"}, []string{`"0"`}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.flags, " "), func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr, sent := runPolicy(t, "", append([]string{"set", "api", "--file", file}, tt.flags...)...)

			got := wantOnlyRequest(t, sent, http.MethodPut, policyURL)
			if ct := got.header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if ifMatch := got.header.Values("If-Match"); strings.Join(ifMatch, ",") != strings.Join(tt.ifMatch, ",") {
				t.Errorf("If-Match = %q, want %q", ifMatch, tt.ifMatch)
			}
			wantJSONBody(t, got.body, policyJSON)
			wantResult(t, code, stdout, stderr, hubAnswer)
		})
	}
}

func TestPolicySet_ReadsTOMLAndStandardInputAsTheSameJSONDocument(t *testing.T) {
	t.Parallel()
	tomlFile := localFile(t, "policy.toml", []byte(policyTOML))
	txtFile := localFile(t, "policy.txt", []byte(policyTOML))
	tests := []struct {
		name, file, stdin, want string
	}{
		{"a .toml file", tomlFile, "", policyFromTOML},
		{"TOML in a file of another name", txtFile, "", policyFromTOML},
		{"TOML on stdin", "-", policyTOML, policyFromTOML},
		{"JSON on stdin", "-", policyJSON, policyJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr, sent := runPolicy(t, tt.stdin, "set", "api", "--file", tt.file)

			got := wantOnlyRequest(t, sent, http.MethodPut, policyURL)
			// Compared by value, so approvals must arrive as a JSON number
			// and block as a JSON boolean.
			wantJSONBody(t, got.body, tt.want)
			wantResult(t, code, stdout, stderr, hubAnswer)
		})
	}
}

func TestPolicyEvaluate_SendsTheMembersOfTheFlagsGiven(t *testing.T) {
	t.Parallel()
	candidate := localFile(t, "candidate.toml", []byte(policyTOML))
	tests := []struct {
		flags []string
		body  string
	}{
		{
			[]string{"--paths", "billing/a.go,docs/x.md", "--paths", "src/b.go", "--target", "main"},
			`{"paths": ["billing/a.go", "docs/x.md", "src/b.go"], "target_branch": "main"}`,
		},
		{[]string{"--change", "12"}, `{"change": 12}`},
		{[]string{"--change", "0"}, `{"change": 0}`},
		{[]string{"--paths", "a", "--policy-file", "candidate.toml"}, `{"paths": ["a"], "policy": ` + policyFromTOML + `}`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.flags, " "), func(t *testing.T) {
			t.Parallel()
			if i := slices.Index(tt.flags, "candidate.toml"); i >= 0 {
				tt.flags = slices.Clone(tt.flags)
				tt.flags[i] = candidate
			}
			code, stdout, stderr, sent := runPolicy(t, "", append([]string{"evaluate", "api"}, tt.flags...)...)

			got := wantOnlyRequest(t, sent, http.MethodPost, policyURL+"/evaluate")
			wantQuery(t, got.query, nil)
			wantJSONBody(t, got.body, tt.body)
			wantResult(t, code, stdout, stderr, hubAnswer)
		})
	}
}

func TestPolicy_RefusesABadCommandLineBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	// The rows name the local files by these words, so that a row's name
	// does not hold a temporary directory.
	good := localFile(t, "policy.json", []byte(policyJSON))
	files := map[string]string{
		"good.json":   good,
		"a-directory": filepath.Dir(good),
		"bad.json":    localFile(t, "bad.json", []byte("{not json or = toml")),
		// A .toml file is read as TOML alone.
		"json.toml": localFile(t, "json.toml", []byte(policyJSON)),
	}
	tests := [][]string{
		{"get"},
		{"set"},
		{"evaluate"},
		{"get", "api", "extra"},
		{"set", "api"},
		{"set", "api", "--file", "/no/such/file"},
		{"set", "api", "--file", "a-directory"},
		{"set", "api", "--file", "bad.json"},
		{"set", "api", "--file", "json.toml"},
		{"set", "api", "--file", "good.json", "--if-revision", "x"},
		{"set", "api", "--file", "good.json", "--if-revision", "-1"},
		{"set", "api", "--file", "good.json", "--if-revision", "1.5"},
		{"get", "api", "--revision", "abc"},
		{"get", "api", "--revision", "-1"},
		{"get", "api", "--revision", ""},
		{"evaluate", "api"},
		{"evaluate", "api", "--target", "main"},
		{"evaluate", "api", "--paths", "a", "--change", "1"},
		{"evaluate", "api", "--change", "x"},
		{"evaluate", "api", "--change", "-1"},
		{"evaluate", "api", "--paths", "a", "--policy-file", "/no/such/file"},
		{"evaluate", "api", "--paths", "a", "--policy-file", "bad.json"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			args = slices.Clone(args)
			for i, arg := range args {
				if path, ok := files[arg]; ok {
					args[i] = path
				}
			}
			code, stdout, stderr, sent := runPolicy(t, "", args...)

			wantClientFailure(t, code, stdout, stderr, "usage_error")
			if len(sent) != 0 {
				t.Errorf("the hub got %v, want no request", sent)
			}
		})
	}
}
