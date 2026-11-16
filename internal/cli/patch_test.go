package cli

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// patchesPath is the patches route of ws, as sent, and patchID the id of one
// of its patches.
const (
	patchesPath = "/api/v1/workspaces/ws/patches"
	patchID     = "5c0e0000-0000-4000-8000-000000000001"
)

// patchAnswer is a patch as the hub serves it, with a member a client could
// drop.
const patchAnswer = `{"id": "` + patchID + `", "branch_name": "p1", "position": 1, "status": "active", "replaced_sha": "0123456789abcdef0123456789abcdef01234567"}`

// wantHubFailure checks a command the hub refused: exit status 1, nothing on
// stdout and the hub's envelope on stderr as received.
func wantHubFailure(t *testing.T, code int, stdout, stderr, envelope string) {
	t.Helper()
	if code != 1 || stdout != "" || stderr != indented(t, envelope) {
		t.Errorf("exit status %d, stdout %q, stderr %q; want 1, nothing and the hub's envelope %q", code, stdout, stderr, envelope)
	}
}

// patchCase is one run of a parley patch command against a stub hub that
// answers status and answer to the request the command must send.
type patchCase struct {
	name          string
	args          []string
	status        int
	answer        string
	rawPath       string
	query         url.Values
	body          string
	wantExitCode1 bool
	// printed is what stdout must hold when it is not the answer, as for a
	// confirmation printed on an answer without a body.
	printed string
}

// runPatchCase runs tt and checks the one request sent and what was printed.
func runPatchCase(t *testing.T, method string, tt patchCase) {
	t.Helper()
	path, err := url.PathUnescape(tt.rawPath)
	if err != nil {
		t.Fatal(err)
	}
	hub := newStubHub(t).answer(method, path, tt.status, tt.answer)

	code, stdout, stderr, sent := runOnHub(t, hub, append([]string{"patch"}, tt.args...)...)

	got := wantOnlyRequest(t, sent, method, tt.rawPath)
	wantQuery(t, got.query, tt.query)
	if tt.body == "" {
		if got.body != "" {
			t.Errorf("body = %q, want none", got.body)
		}
	} else {
		wantJSONBody(t, got.body, tt.body)
	}
	if tt.wantExitCode1 {
		wantHubFailure(t, code, stdout, stderr, tt.answer)
		return
	}
	printed := tt.answer
	if tt.printed != "" {
		printed = tt.printed
	}
	wantResult(t, code, stdout, stderr, printed)
}

func TestPatchAdd_SendsOnlyGivenFlags(t *testing.T) {
	t.Parallel()
	exists := `{"error": {"code": 409, "message": "a patch of the branch p1 exists", "error_type": "patch_exists"}}`
	tests := []patchCase{
		{
			name: "only the branch", args: []string{"add", "ws", "--branch", "p1"},
			status: http.StatusCreated, answer: patchAnswer, rawPath: patchesPath, body: `{"branch_name": "p1"}`,
		},
		{
			name: "every flag, an empty description among them",
			args: []string{"add", "ws", "--branch", "p1", "--position", "2", "--upstream-pr", "https://example.com/pull/7",
				"--description", "", "--skip-branch-check", "--if-not-exists"},
			status: http.StatusCreated, answer: patchAnswer, rawPath: patchesPath,
			body: `{"branch_name": "p1", "position": 2, "upstream_pr_url": "https://example.com/pull/7", "description": "",
				"skip_branch_check": true, "if_not_exists": true}`,
		},
		{
			name: "an existing patch answered 200", args: []string{"add", "ws", "--branch", "p1", "--if-not-exists"},
			status: http.StatusOK, answer: patchAnswer, rawPath: patchesPath, body: `{"branch_name": "p1", "if_not_exists": true}`,
		},
		{
			name: "the hub refuses", args: []string{"add", "ws", "--branch", "p1"},
			status: http.StatusConflict, answer: exists, rawPath: patchesPath, body: `{"branch_name": "p1"}`, wantExitCode1: true,
		},
		{
			name: "a slug with a slash", args: []string{"add", "a/b", "--branch", "p1"},
			status: http.StatusCreated, answer: patchAnswer, rawPath: "/api/v1/workspaces/a%2Fb/patches", body: `{"branch_name": "p1"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runPatchCase(t, http.MethodPost, tt)
		})
	}
}

func TestPatchList_QueriesIncludeDeleted(t *testing.T) {
	t.Parallel()
	list := `{"items": [` + patchAnswer + `], "next_cursor": null}`
	mismatch := `{"error": {"code": 400, "message": "the workspace ws is not in carry-patch mode", "error_type": "workspace_mode_mismatch"}}`
	tests := []patchCase{
		{name: "without the flag", args: []string{"list", "ws"}, status: http.StatusOK, answer: list, rawPath: patchesPath},
		{
			name: "with --include-deleted", args: []string{"list", "ws", "--include-deleted"},
			status: http.StatusOK, answer: list, rawPath: patchesPath, query: url.Values{"include_deleted": {"true"}},
		},
		{
			name: "the hub refuses", args: []string{"list", "ws"},
			status: http.StatusBadRequest, answer: mismatch, rawPath: patchesPath, wantExitCode1: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runPatchCase(t, http.MethodGet, tt)
		})
	}
}

func TestPatchShow_GetsOnePatch(t *testing.T) {
	t.Parallel()
	notFound := `{"error": {"code": 404, "message": "no patch ` + patchID + `", "error_type": "not_found"}}`
	tests := []patchCase{
		{
			name: "a patch", args: []string{"show", "ws", patchID},
			status: http.StatusOK, answer: patchAnswer, rawPath: patchesPath + "/" + patchID,
		},
		{
			name: "arguments that need encoding", args: []string{"show", "a b", "p/1"},
			status: http.StatusOK, answer: patchAnswer, rawPath: "/api/v1/workspaces/a%20b/patches/p%2F1",
		},
		{
			name: "an unknown patch", args: []string{"show", "ws", patchID},
			status: http.StatusNotFound, answer: notFound, rawPath: patchesPath + "/" + patchID, wantExitCode1: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runPatchCase(t, http.MethodGet, tt)
		})
	}
}

func TestPatchUpdate_SendsOnlyGivenFlags(t *testing.T) {
	t.Parallel()
	path := patchesPath + "/" + patchID
	deleted := `{"error": {"code": 409, "message": "the patch is deleted", "error_type": "patch_deleted"}}`
	tests := []patchCase{
		{name: "--status", args: []string{"--status", "disabled"}, body: `{"status": "disabled"}`},
		{name: "--position", args: []string{"--position", "2"}, body: `{"position": 2}`},
		{name: "an empty --description", args: []string{"--description", ""}, body: `{"description": ""}`},
		{name: "--upstream-pr", args: []string{"--upstream-pr", "https://example.test/pull/7"}, body: `{"upstream_pr_url": "https://example.test/pull/7"}`},
		{
			name: "every flag", args: []string{"--status", "active", "--position", "1", "--description", "d", "--upstream-pr", ""},
			body: `{"status": "active", "position": 1, "description": "d", "upstream_pr_url": ""}`,
		},
		{name: "a status the hub decides on", args: []string{"--status", "bogus"}, body: `{"status": "bogus"}`},
		{
			name: "the hub refuses", args: []string{"--status", "disabled"}, body: `{"status": "disabled"}`,
			status: http.StatusConflict, answer: deleted, wantExitCode1: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.args = append([]string{"update", "ws", patchID}, tt.args...)
			tt.rawPath = path
			if tt.status == 0 {
				tt.status, tt.answer = http.StatusOK, patchAnswer
			}
			runPatchCase(t, http.MethodPatch, tt)
		})
	}
}

func TestPatchRemove_PrintsConfirmation(t *testing.T) {
	t.Parallel()
	notFound := `{"error": {"code": 404, "message": "patch not found", "error_type": "not_found"}}`
	tests := []patchCase{
		{
			name: "a patch", args: []string{"remove", "ws", patchID},
			status: http.StatusNoContent, rawPath: patchesPath + "/" + patchID,
			printed: `{"status": "removed", "patch_id": "` + patchID + `"}`,
		},
		{
			name: "the id as typed", args: []string{"remove", "ws", "p/1"},
			status: http.StatusNoContent, rawPath: patchesPath + "/p%2F1",
			printed: `{"status": "removed", "patch_id": "p/1"}`,
		},
		{
			name: "the hub refuses", args: []string{"remove", "ws", patchID},
			status: http.StatusNotFound, answer: notFound, rawPath: patchesPath + "/" + patchID, wantExitCode1: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runPatchCase(t, http.MethodDelete, tt)
		})
	}
}

func TestPatchRestore_PrintsPatch(t *testing.T) {
	t.Parallel()
	notDeleted := `{"error": {"code": 409, "message": "the patch is not deleted", "error_type": "patch_not_deleted"}}`
	tests := []patchCase{
		{
			name: "a deleted patch", args: []string{"restore", "ws", patchID},
			status: http.StatusOK, answer: patchAnswer, rawPath: patchesPath + "/" + patchID + "/restore",
		},
		{
			name: "the hub refuses", args: []string{"restore", "ws", patchID},
			status: http.StatusConflict, answer: notDeleted, rawPath: patchesPath + "/" + patchID + "/restore", wantExitCode1: true,
		},
		{
			name: "a slug with a slash", args: []string{"restore", "a/b", patchID},
			status: http.StatusOK, answer: patchAnswer, rawPath: "/api/v1/workspaces/a%2Fb/patches/" + patchID + "/restore",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runPatchCase(t, http.MethodPost, tt)
		})
	}
}

func TestPatchReorder_SendsIdsInOrder(t *testing.T) {
	t.Parallel()
	list := `{"items": [` + patchAnswer + `], "next_cursor": null}`
	incomplete := `{"error": {"code": 400, "message": "patch_ids names 3 patches, the workspace has 4", "error_type": "invalid_request"}}`
	tests := []patchCase{
		{
			name: "ids in argument order", args: []string{"reorder", "ws", "C", "A", "B"},
			status: http.StatusOK, answer: list, rawPath: patchesPath + "/reorder", body: `{"patch_ids": ["C", "A", "B"]}`,
		},
		{
			name: "one id", args: []string{"reorder", "ws", "A"},
			status: http.StatusOK, answer: list, rawPath: patchesPath + "/reorder", body: `{"patch_ids": ["A"]}`,
		},
		{
			name: "the hub refuses", args: []string{"reorder", "ws", "C", "A", "B"},
			status: http.StatusBadRequest, answer: incomplete, rawPath: patchesPath + "/reorder",
			body: `{"patch_ids": ["C", "A", "B"]}`, wantExitCode1: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runPatchCase(t, http.MethodPost, tt)
		})
	}
}

func TestPatch_UsageErrors(t *testing.T) {
	t.Parallel()
	hub := newStubHub(t)
	tests := []struct {
		args      []string
		inMessage string
	}{
		{[]string{"add", "ws"}, "parley patch add needs the flag --branch"},
		{[]string{"add", "--branch", "p1"}, "parley patch add needs the argument <slug>"},
		{[]string{"add", "ws", "extra", "--branch", "p1"}, `got the extra argument "extra"`},
		{[]string{"add", "ws", "--branch", "p1", "--position", "two"}, `"two" for "--position" flag`},
		{[]string{"list"}, "parley patch list needs the argument <slug>"},
		{[]string{"list", "a", "b"}, `got the extra argument "b"`},
		{[]string{"show", "ws"}, "parley patch show needs the argument <id>"},
		{[]string{"show", "ws", "id", "extra"}, `got the extra argument "extra"`},
		{[]string{"update", "ws", "id"}, "parley patch update needs at least one of --status, --position, --description, --upstream-pr"},
		{[]string{"update", "ws", "id", "--position", "two"}, `"two" for "--position" flag`},
		{[]string{"update", "ws"}, "parley patch update needs the argument <id>"},
		{[]string{"update", "ws", "id", "extra", "--status", "active"}, `got the extra argument "extra"`},
		{[]string{"remove", "ws"}, "parley patch remove needs the argument <id>"},
		{[]string{"remove", "ws", "id", "extra"}, `got the extra argument "extra"`},
		{[]string{"restore", "ws"}, "parley patch restore needs the argument <id>"},
		{[]string{"restore", "ws", "id", "extra"}, `got the extra argument "extra"`},
		{[]string{"reorder", "ws"}, "parley patch reorder needs the argument <id>"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			code, stdout, stderr, _ := runOnHub(t, hub, append([]string{"patch"}, tt.args...)...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.inMessage) {
				t.Errorf("message = %q, want it to contain %q", message, tt.inMessage)
			}
		})
	}
	t.Run("patch alone", func(t *testing.T) {
		code, stdout, stderr, _ := runOnHub(t, hub, "patch")

		if code != 0 || stderr != "" || !strings.Contains(stdout, "add") || !strings.Contains(stdout, "list") || !strings.Contains(stdout, "show") {
			t.Errorf("exit status %d, stdout %q, stderr %q; want 0 and the help naming add, list and show", code, stdout, stderr)
		}
	})
	if sent := hub.recorded(); len(sent) != 0 {
		t.Errorf("the hub got %v, want no request", sent)
	}
}
