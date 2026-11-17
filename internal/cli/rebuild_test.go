package cli

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The routes parley rebuild submit my-project and parley rebuild requeue
// my-project j1 post to, as sent; both wait at jobPath. rollbackPath is the
// route of parley rebuild rollback my-project j1, previewPath the one of
// parley rebuild preview my-project.
const (
	submitPath   = "/api/v1/workspaces/my-project/rebuild"
	requeuePath  = jobPath + "/requeue"
	rollbackPath = jobPath + "/rollback"
	previewPath  = "/api/v1/workspaces/my-project/rebuild-preview"
)

// rebuildWaits are the two commands that can wait for a rebuild job, each
// with the route it starts the job at, followed by more.
func rebuildWaits(more ...string) []struct {
	args      []string
	startPath string
} {
	return []struct {
		args      []string
		startPath string
	}{
		{append([]string{"rebuild", "submit", "my-project"}, more...), submitPath},
		{append([]string{"rebuild", "requeue", "my-project", "j1"}, more...), requeuePath},
	}
}

// rebuildHub is a stub hub that starts the rebuild job j1 of my-project as
// queued at startPath and answers the n-th read of the job with jobs[n-1],
// the last of jobs answering every later one too.
func rebuildHub(t *testing.T, startPath string, jobs ...http.HandlerFunc) *stubHub {
	t.Helper()
	return newStubHub(t).
		answer(http.MethodPost, startPath, http.StatusAccepted, rebuildJob("queued", 0)).
		handle(http.MethodGet, jobPath, inTurn(jobs...))
}

func TestRebuild_SendsTheCommandsRequestAndPrintsTheAnswer(t *testing.T) {
	const jobs = "/api/v1/workspaces/w/rebuilds"
	tests := []struct {
		args []string
		want sentRequest
	}{
		{[]string{"rebuild", "submit", "w"}, sentRequest{http.MethodPost, "/api/v1/workspaces/w/rebuild", "", `{}`}},
		{[]string{"rebuild", "submit", "w", "--strategy", "merge"}, sentRequest{http.MethodPost, "/api/v1/workspaces/w/rebuild", "", `{"strategy":"merge"}`}},
		{[]string{"rebuild", "submit", "w", "--fail-mode", "continue"}, sentRequest{http.MethodPost, "/api/v1/workspaces/w/rebuild", "", `{"fail_mode":"continue"}`}},
		{
			[]string{"rebuild", "submit", "w", "--strategy", "rebase", "--fail-mode", "fail_fast"},
			sentRequest{http.MethodPost, "/api/v1/workspaces/w/rebuild", "", `{"strategy":"rebase","fail_mode":"fail_fast"}`},
		},
		{[]string{"rebuild", "list", "w"}, sentRequest{http.MethodGet, jobs, "", ""}},
		{[]string{"rebuild", "list", "w", "--status", "failed"}, sentRequest{http.MethodGet, jobs, "status=failed", ""}},
		{[]string{"rebuild", "status", "w", "j1"}, sentRequest{http.MethodGet, jobs + "/j1", "", ""}},
		{[]string{"rebuild", "cancel", "w", "j1"}, sentRequest{http.MethodDelete, jobs + "/j1", "", ""}},
		{[]string{"rebuild", "requeue", "w", "j1"}, sentRequest{http.MethodPost, jobs + "/j1/requeue", "", ""}},
		{[]string{"rebuild", "rollback", "w", "j1"}, sentRequest{http.MethodPost, jobs + "/j1/rollback", "", ""}},
		{[]string{"rebuild", "preview", "w"}, sentRequest{http.MethodGet, "/api/v1/workspaces/w/rebuild-preview", "", ""}},
		{[]string{"rebuild", "status", "a/b", "j/1"}, sentRequest{http.MethodGet, "/api/v1/workspaces/a%2Fb/rebuilds/j%2F1", "", ""}},
		{[]string{"rebuild", "rollback", "a/b", "j/1"}, sentRequest{http.MethodPost, "/api/v1/workspaces/a%2Fb/rebuilds/j%2F1/rollback", "", ""}},
		{[]string{"rebuild", "preview", "a/b"}, sentRequest{http.MethodGet, "/api/v1/workspaces/a%2Fb/rebuild-preview", "", ""}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, tt.want)
		})
	}
	t.Run("rebuild submit answered 202", func(t *testing.T) {
		hub := rebuildHub(t, submitPath)

		code, stdout, stderr := runWorkspace(t, hub, "rebuild", "submit", "my-project")

		if code != 0 || stdout != indented(t, rebuildJob("queued", 0)) {
			t.Errorf("exit status %d, stdout %q (stderr %q); want 0 and the queued job's record", code, stdout, stderr)
		}
		if n := len(hub.recorded()); n != 1 {
			t.Errorf("the hub got %d requests, want 1", n)
		}
	})
}

func TestRebuild_PrintsTheHubsRefusalAsReceived(t *testing.T) {
	t.Parallel()
	refusal := func(code int, errorType string) string {
		return `{"error":{"code":` + strconv.Itoa(code) + `,"message":"refused","error_type":"` + errorType + `"}}`
	}
	tests := []struct {
		args         []string
		method, path string
		status       int
		errorType    string
	}{
		{[]string{"rebuild", "submit", "my-project"}, http.MethodPost, submitPath, http.StatusConflict, "concurrent_rebuild"},
		{[]string{"rebuild", "list", "my-project"}, http.MethodGet, "/api/v1/workspaces/my-project/rebuilds", http.StatusNotFound, "not_found"},
		{[]string{"rebuild", "status", "my-project", "j1"}, http.MethodGet, jobPath, http.StatusNotFound, "not_found"},
		{[]string{"rebuild", "cancel", "my-project", "j1"}, http.MethodDelete, jobPath, http.StatusConflict, "job_not_cancellable"},
		{[]string{"rebuild", "requeue", "my-project", "j1"}, http.MethodPost, requeuePath, http.StatusConflict, "job_not_requeueable"},
		{[]string{"rebuild", "rollback", "my-project", "j1"}, http.MethodPost, rollbackPath, http.StatusConflict, "rollback_unavailable"},
		{[]string{"rebuild", "preview", "my-project"}, http.MethodGet, previewPath, http.StatusConflict, "upstream_unknown"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			answer := refusal(tt.status, tt.errorType)
			hub := newStubHub(t).answer(tt.method, tt.path, tt.status, answer)

			code, stdout, stderr := runWorkspace(t, hub, tt.args...)

			if code != 1 || stdout != "" {
				t.Errorf("exit status %d, stdout %q; want 1 and nothing", code, stdout)
			}
			if stderr != indented(t, answer) {
				t.Errorf("stderr = %q, want the hub's envelope as received", stderr)
			}
			if n := len(hub.recorded()); n != 1 {
				t.Errorf("the hub got %d requests, want 1", n)
			}
		})
	}
}

func TestRebuildWait_PollsThroughFailedUntilTheJobIsCompleted(t *testing.T) {
	t.Parallel()
	for _, tt := range rebuildWaits("--wait") {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			hub := rebuildHub(t, tt.startPath, jobsInStatus("running", "failed", "queued", "running", "completed")...)

			code, stdout, stderr := runWorkspace(t, hub, tt.args...)

			if code != 0 {
				t.Errorf("exit status = %d, want 0 (stderr %q)", code, stderr)
			}
			if want := indented(t, rebuildJob("completed", 5)); stdout != want {
				t.Errorf("stdout = %q, want only the completed job %q", stdout, want)
			}
			wantProgress(t, stderr, "waiting: queued", "waiting: running", "waiting: failed", "waiting: queued", "waiting: running", "waiting: completed")
			if got := hub.count(http.MethodGet); got != 5 {
				t.Errorf("the hub got %d job reads, want 5", got)
			}
			sent := hub.recorded()
			if len(sent) == 0 || sent[0].method != http.MethodPost || sent[0].rawPath != tt.startPath {
				t.Fatalf("the hub got %v, want POST %s first", sent, tt.startPath)
			}
		})
	}
	for _, name := range []string{"submit", "requeue"} {
		code, stdout, stderr := runCLI(t, "help", "rebuild", name, "--json")
		if code != 0 {
			t.Fatalf("help rebuild %s exited %d (stderr %q)", name, code, stderr)
		}
		var d commandHelp
		oneJSONDocument(t, stdout, &d)
		for flag, def := range map[string]string{"wait": "false", "timeout": "5m", "poll-interval": "5s"} {
			if got := flagNamed(d, flag); got.Default != def {
				t.Errorf("rebuild %s --%s defaults to %q, want %q", name, flag, got.Default, def)
			}
		}
	}
}

func TestRebuildWait_FailsWhenTheJobEndsUnsuccessfullyOrTheTimeoutPasses(t *testing.T) {
	t.Parallel()
	ends := []struct {
		name    string
		jobs    []string
		timeout string
		// final is the record printed, "" for none; errorType the one of
		// the envelope on stderr.
		final, errorType string
	}{
		{"dead_letter", []string{"running", "dead_letter"}, "", rebuildJob("dead_letter", 2), "wait_failed"},
		{"cancelled", []string{"cancelled"}, "", rebuildJob("cancelled", 1), "wait_failed"},
		{"failed until the timeout", []string{"failed"}, "1s", "", "wait_timeout"},
	}
	for _, end := range ends {
		for _, tt := range rebuildWaits("--wait") {
			t.Run(end.name+" "+strings.Join(tt.args, " "), func(t *testing.T) {
				t.Parallel()
				hub := rebuildHub(t, tt.startPath, jobsInStatus(end.jobs...)...)
				args := tt.args
				if end.timeout != "" {
					args = append(args, "--timeout", end.timeout)
				}

				code, stdout, stderr := runWorkspace(t, hub, args...)

				if code != 1 {
					t.Errorf("exit status = %d, want 1 (stderr %q)", code, stderr)
				}
				want := ""
				if end.final != "" {
					want = indented(t, end.final)
				}
				if stdout != want {
					t.Errorf("stdout = %q, want %q", stdout, want)
				}
				wantErrorType(t, stderr, end.errorType)
				if n := hub.count(http.MethodDelete); n != 0 {
					t.Errorf("the hub got %d DELETE requests, want none", n)
				}
			})
		}
	}
}

func TestRebuild_RefusesAWrongCommandLineWithoutARequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		// inMessage is what the usage error must say.
		inMessage string
	}{
		{[]string{"submit", "w", "--strategy", "squash"}, `takes --strategy rebase or merge, got "squash"`},
		{[]string{"submit", "w", "--strategy", ""}, `takes --strategy rebase or merge, got ""`},
		{[]string{"submit", "w", "--fail-mode", "stop"}, `takes --fail-mode fail_fast or continue, got "stop"`},
		{[]string{"list", "w", "--status", "done"}, `got "done"`},
		{[]string{"list", "w", "--status", ""}, `got ""`},
		{[]string{"submit"}, "needs the argument <slug>"},
		{[]string{"submit", "w", "x"}, `got the extra argument "x"`},
		{[]string{"list"}, "needs the argument <slug>"},
		{[]string{"status", "w"}, "needs the argument <id>"},
		{[]string{"cancel", "w"}, "needs the argument <id>"},
		{[]string{"requeue", "w"}, "needs the argument <id>"},
		{[]string{"cancel", "w", "j1", "x"}, `got the extra argument "x"`},
		{[]string{"rollback"}, "needs the argument <slug>"},
		{[]string{"rollback", "w"}, "needs the argument <id>"},
		{[]string{"rollback", "w", "j1", "x"}, `got the extra argument "x"`},
		{[]string{"preview"}, "needs the argument <slug>"},
		{[]string{"preview", "w", "x"}, `got the extra argument "x"`},
		{[]string{"submit", "w", "--wait", "--timeout", "0s"}, `"0s" for "--timeout" flag: want a whole number above 0`},
		{[]string{"requeue", "w", "j1", "--poll-interval", "-1s"}, `"-1s" for "--poll-interval" flag: want a whole number above 0`},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t)

			code, stdout, stderr := runWorkspace(t, hub, append([]string{"rebuild"}, tt.args...)...)

			if message := wantClientFailure(t, code, stdout, stderr, "usage_error"); !strings.Contains(message, tt.inMessage) {
				t.Errorf("message = %q, want it to say %q", message, tt.inMessage)
			}
			if n := len(hub.recorded()); n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

// previewAnswer is a preview of my-project whose patches, given as
// branch name and status, sit at positions 1, 2 and on.
func previewAnswer(patches ...[2]string) string {
	results := make([]string, len(patches))
	for i, p := range patches {
		results[i] = fmt.Sprintf(`{"patch_id":"p%d","branch_name":%q,"position":%d,"status":%q,"conflict_files":[],"conflict":null}`, i+1, p[0], i+1, p[1])
	}
	return `{"upstream_head_sha":"` + strings.Repeat("a", 40) + `","patch_results":[` + strings.Join(results, ",") + `]}`
}

func TestRebuildPreview_FailOnConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		flag   bool
		status int
		answer string
		// code is the exit status; names are what the condition_not_met
		// message must name, and others what it must not.
		code          int
		names, others []string
	}{
		{
			"a conflicting patch after a clean one", true, http.StatusOK,
			previewAnswer([2]string{"patch/a", "would_succeed"}, [2]string{"patch/b", "would_conflict"}),
			3, []string{"patch/b"}, []string{"patch/a"},
		},
		{
			"two conflicting patches", true, http.StatusOK,
			previewAnswer([2]string{"patch/a", "would_conflict"}, [2]string{"patch/b", "would_skip"}, [2]string{"patch/c", "would_conflict"}),
			3, []string{"patch/a", "patch/c"}, []string{"patch/b"},
		},
		{
			"clean and skipped patches only", true, http.StatusOK,
			previewAnswer([2]string{"patch/a", "would_succeed"}, [2]string{"patch/b", "would_skip"}),
			0, nil, nil,
		},
		{"no patch to test", true, http.StatusOK, previewAnswer(), 0, nil, nil},
		{
			"a conflicting patch without the flag", false, http.StatusOK,
			previewAnswer([2]string{"patch/a", "would_succeed"}, [2]string{"patch/b", "would_conflict"}),
			0, nil, nil,
		},
		{
			"the hub's refusal", true, http.StatusConflict,
			`{"error":{"code":409,"message":"refused","error_type":"upstream_unknown"}}`,
			1, nil, nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hub := newStubHub(t).answer(http.MethodGet, previewPath, tt.status, tt.answer)
			args := []string{"rebuild", "preview", "my-project"}
			if tt.flag {
				args = append(args, "--fail-on-conflict")
			}

			code, stdout, stderr := runWorkspace(t, hub, args...)

			if code != tt.code {
				t.Errorf("exit status = %d, want %d (stderr %q)", code, tt.code, stderr)
			}
			if n := len(hub.recorded()); n != 1 {
				t.Errorf("the hub got %d requests, want 1", n)
			}
			if tt.status != http.StatusOK {
				if stdout != "" || stderr != indented(t, tt.answer) {
					t.Errorf("stdout %q, stderr %q; want nothing and the hub's envelope as received", stdout, stderr)
				}
				return
			}
			if want := indented(t, tt.answer); stdout != want {
				t.Errorf("stdout = %q, want the whole answer %q", stdout, want)
			}
			if tt.code == 0 {
				if stderr != "" {
					t.Errorf("stderr = %q, want it empty", stderr)
				}
				return
			}
			gotCode, message, errorType := clientEnvelope(t, stderr)
			if gotCode != 0 || errorType != "condition_not_met" {
				t.Errorf("envelope code %v, error_type %q; want 0 and condition_not_met", gotCode, errorType)
			}
			for _, name := range tt.names {
				if !strings.Contains(message, name) {
					t.Errorf("message = %q, want it to name %s", message, name)
				}
			}
			for _, name := range tt.others {
				if strings.Contains(message, name) {
					t.Errorf("message = %q, want it not to name %s", message, name)
				}
			}
		})
	}
}
