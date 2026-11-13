package cli

import (
	"net/http"
	"strings"
	"testing"
)

// The test sets environment variables, so it must not run in parallel.
func TestUserCommands_SendOneRequestAndPrintTheAnswer(t *testing.T) {
	tests := []struct {
		args []string
		want sentRequest
	}{
		{[]string{"user", "show"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/user"}},
		{[]string{"user", "update", "--full-name", "Alice A."}, sentRequest{method: http.MethodPatch, rawPath: "/api/v1/user", body: `{"full_name":"Alice A."}`}},
		{[]string{"user", "update", "--full-name", ""}, sentRequest{method: http.MethodPatch, rawPath: "/api/v1/user", body: `{"full_name":""}`}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, tt.want)
		})
	}
}
