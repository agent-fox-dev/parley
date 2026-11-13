package cli

import (
	"net/http"
	"strings"
	"testing"
)

// The test sets environment variables, so it must not run in parallel.
func TestAdminKeysCommands_SendOneRequestAndPrintTheAnswer(t *testing.T) {
	tests := []struct {
		args []string
		want sentRequest
	}{
		{[]string{"admin", "keys", "list", "alice"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users/alice/keys"}},
		{[]string{"admin", "keys", "list", "a/b"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/users/a%2Fb/keys"}},
		{[]string{"admin", "keys", "revoke", "alice", "Ab3dE6gH"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/users/alice/keys/Ab3dE6gH"}},
		{[]string{"admin", "keys", "revoke", "a/b", "k?1"}, sentRequest{method: http.MethodDelete, rawPath: "/api/v1/users/a%2Fb/keys/k%3F1"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, tt.want)
		})
	}
}
