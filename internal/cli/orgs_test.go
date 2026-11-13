package cli

import (
	"net/http"
	"strings"
	"testing"
)

// The test sets environment variables, so it must not run in parallel.
func TestOrgsCommands_SendOneRequestAndPrintTheAnswer(t *testing.T) {
	tests := []struct {
		args []string
		want sentRequest
	}{
		{[]string{"orgs", "list"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/user/orgs"}},
		{[]string{"orgs", "show", "acme"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/orgs/acme"}},
		{[]string{"orgs", "members", "acme"}, sentRequest{method: http.MethodGet, rawPath: "/api/v1/orgs/acme/members"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			wantSends(t, tt.args, tt.want)
		})
	}
}
