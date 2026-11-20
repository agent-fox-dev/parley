package perf

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// sandboxReadySamples is how many sandboxes sandbox_ready creates.
const sandboxReadySamples = 5

// sandboxReady creates sandboxes of the configured profile in the first
// workspace one after the other and measures for each the seconds from
// its create response to the first read that shows it ready. The target
// says "within", so it reports the largest; a sandbox that fails ends the
// scenario in error naming its state_reason.
func sandboxReady(ctx context.Context, e *env) (outcome, error) {
	var largest float64
	for range sandboxReadySamples {
		id, err := e.sandbox(ctx)
		if err != nil {
			return outcome{}, err
		}
		created := time.Now()
		ready, err := e.waitReady(ctx, id)
		if err != nil {
			return outcome{}, err
		}
		largest = max(largest, ready.Sub(created).Seconds())
	}
	return outcome{measured: &largest, samples: sandboxReadySamples}, nil
}

// sandboxState reads the sandbox id of the first workspace and returns its
// state and state_reason, empty when it has none.
func (e *env) sandboxState(ctx context.Context, id string) (string, string, error) {
	var sb struct {
		State       string  `json:"state"`
		StateReason *string `json:"state_reason"`
	}
	path := workspacePath(e.cfg.Workspaces[0], "/sandboxes/"+url.PathEscape(id))
	if _, err := e.client.json(ctx, http.MethodGet, path, nil, &sb); err != nil {
		return "", "", err
	}
	reason := ""
	if sb.StateReason != nil {
		reason = *sb.StateReason
	}
	return sb.State, reason, nil
}
