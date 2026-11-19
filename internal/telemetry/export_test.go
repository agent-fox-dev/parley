package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
)

// NormaliseUUID is normaliseUUID, for the tests of package telemetry_test.
func NormaliseUUID(s string) (string, bool) { return normaliseUUID(s) }

// NormaliseTimestamp parses the client timestamp s and renders it as a
// response does.
func NormaliseTimestamp(s string) (string, bool) {
	t, ok := parseTimestamp(s)
	if !ok {
		return "", false
	}
	return clientTime(t).String(), true
}

// Count reads the JSON value raw as the integer member duration_ms of a
// body.
func Count(raw string) (*int64, error) {
	return fields{"duration_ms": json.RawMessage(raw)}.count("duration_ms")
}

// Seed stores the record body, of the kind the single-record ingestion
// route path takes (/events, /sessions/outcomes, /tools/calls,
// /tools/errors or /traces), under the workspace slug and the run runID as
// that route reads and stores it, whatever the workspace's status, and
// returns the stored record as the route answers it.
func (c *Component) Seed(ctx context.Context, path, slug, runID string, body map[string]any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var f fields
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	t := target{workspace: slug, runID: runID}
	switch path {
	case "/events":
		return seed(ctx, c, runEvents, c.readRunEvent, f, t)
	case "/sessions/outcomes":
		return seed(ctx, c, outcomes, c.readOutcome, f, t)
	case "/tools/calls":
		return seed(ctx, c, toolCalls, c.readToolCall, f, t)
	case "/tools/errors":
		return seed(ctx, c, toolErrors, c.readToolError, f, t)
	case "/traces":
		return seed(ctx, c, traces, c.readTrace, f, t)
	}
	return nil, fmt.Errorf("no record kind is stored through %s", path)
}

func seed[R record](ctx context.Context, c *Component, k kind[R], read func(fields, target) (R, error), f fields, t target) ([]byte, error) {
	rec, err := read(f, t)
	if err != nil {
		return nil, err
	}
	if err := c.write(ctx, func(ctx context.Context, tx Tx) error { return k.insert(ctx, tx, rec) }); err != nil {
		return nil, err
	}
	return json.Marshal(rec)
}
