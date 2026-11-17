package cli

import (
	"net/http"
	"strings"
	"testing"
)

// wantSends sets the environment, so the test does not run in parallel.
func TestWorkspacePatchStatus_PrintsTheSummaryOfAWorkspace(t *testing.T) {
	wantSends(t, []string{"workspace", "patch-status", "demo"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/workspaces/demo/patch-status"})

	code, stdout, stderr := runCLI(t, "workspace", "--help")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "patch-status") {
		t.Errorf("parley workspace --help: exit status %d, stdout %q, stderr %q; want 0 and the help listing patch-status", code, stdout, stderr)
	}
}
