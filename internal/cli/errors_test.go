package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestPrintError_WritesTheEnvelopeOfAnErrorTheCLIRaises(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		errorType string
	}{
		{"usage_error", usageError("unknown flag: --%s", "bogus"), "usage_error"},
		{"client_error", clientError("cannot reach the hub at %s", "127.0.0.1:1"), "client_error"},
		{"wait_failed", waitFailed("the job ended in state %s", "failed"), "wait_failed"},
		{"wait_timeout", waitTimeout("the job is still running after %s", "5m"), "wait_timeout"},
		{"condition_not_met", conditionNotMet("n is %d, an odd number", 3), "condition_not_met"},
		{"an error of no named kind", errors.New("disk full"), "client_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer

			printError(&buf, tt.err)

			var doc map[string]json.RawMessage
			oneJSONDocument(t, buf.String(), &doc)
			if got := sortedKeys(doc); fmt.Sprint(got) != "[error]" {
				t.Fatalf("members %v, want only error", got)
			}
			var inner map[string]any
			if err := json.Unmarshal(doc["error"], &inner); err != nil {
				t.Fatalf("error is not an object: %s", doc["error"])
			}
			if got := sortedKeys(inner); fmt.Sprint(got) != "[code error_type message]" {
				t.Errorf("error has the members %v, want code, message and error_type", got)
			}
			if inner["code"] != float64(0) {
				t.Errorf("code = %v, want 0", inner["code"])
			}
			if m, _ := inner["message"].(string); m == "" {
				t.Errorf("message = %v, want a non-empty string", inner["message"])
			}
			if inner["error_type"] != tt.errorType {
				t.Errorf("error_type = %v, want %q", inner["error_type"], tt.errorType)
			}
			if want := indented(t, buf.String()); buf.String() != want {
				t.Errorf("printed %q, want it indented by 2 spaces with a newline, %q", buf.String(), want)
			}
		})
	}
}

func TestExitCode_GivesEachOutcomeItsStatus(t *testing.T) {
	t.Parallel()
	envelope := []byte(`{"error":{"code":404,"message":"workspace not found","error_type":"not_found"}}`)
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, 0},
		{"hub error 404", &hubError{status: 404, body: envelope}, 1},
		{"hub error 500", &hubError{status: 500, body: envelope}, 1},
		{"wait_failed", waitFailed("failed"), 1},
		{"wait_timeout", waitTimeout("timed out"), 1},
		{"usage_error", usageError("bad"), 2},
		{"client_error", clientError("bad"), 2},
		{"an error of no named kind", errors.New("other"), 2},
		{"condition_not_met", conditionNotMet("odd"), 3},
		{"a wrapped condition_not_met", fmt.Errorf("sync: %w", conditionNotMet("diverged")), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCode(tt.err); got != tt.want {
				t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
