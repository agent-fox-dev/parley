package cli

import (
	"net/http"
	"strings"
	"testing"
)

// The test sets environment variables, so it must not run in parallel.
func TestAdminTokensCommands_SendOneRequestAndPrintTheAnswer(t *testing.T) {
	tests := []struct {
		args []string
		want sentRequest
	}{
		{[]string{"admin", "tokens", "list", "alice"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users/alice/tokens"}},
		{[]string{"admin", "tokens", "list", "a/b"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users/a%2Fb/tokens"}},
		{[]string{"admin", "tokens", "revoke", "alice", "k3x9q2mz"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/users/alice/tokens/k3x9q2mz"}},
		{[]string{"admin", "tokens", "revoke", "a/b", "k?1"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/users/a%2Fb/tokens/k%3F1"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, tt.want)
		})
	}
}
