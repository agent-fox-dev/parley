package telemetry

import (
	"context"
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// runPrefix is the pattern every ingestion route starts with.
const runPrefix = "/workspaces/:slug/runs/:run_id"

// msgArchived is the refusal of a valid request to an archived workspace.
const msgArchived = "the workspace is archived and accepts no telemetry"

// msgWriteTimeout is the refusal of a write that did not finish within the
// write limit, and retryAfter the seconds after which the caller may send
// it again.
const (
	msgWriteTimeout = "the telemetry store did not finish the write in time; nothing was stored, send it again"
	retryAfter      = "5"
)

// errNoDecision is a request at an ingestion route without the access
// decision the pipeline's access check attaches: a wiring fault, which
// grants nothing.
var errNoDecision = errors.New("no workspace access decision at an ingestion route")

// target is the workspace and run an ingestion request names in its path.
type target struct {
	workspace, runID string
}

// act stores what a request validated and answers it.
type act func(ctx context.Context, w http.ResponseWriter) error

// validator reads and checks an ingestion request to t and returns the act
// that stores it; a refusal is the error to answer.
type validator func(r *http.Request, t target) (act, error)

// ingest is the ingestion route POST runPrefix+suffix: create marks a
// creating POST, which takes an Idempotency-Key. Every ingestion route is
// built here, so that all of them decide in one order. The pipeline checks
// the scope audit:write of a PAT, then write access to the workspace; the
// handler then checks the run id of the path, runs validate, refuses an
// archived workspace and acts: a write the write limit ends is answered 503
// unavailable with Retry-After. A workspace in any other status, a task fork
// included, accepts ingestion: it needs no repository.
func (c *Component) ingest(suffix string, create bool, validate validator) server.Route {
	return server.Route{
		Method:  http.MethodPost,
		Pattern: runPrefix + suffix,
		Scope:   "audit:write",
		Marks:   map[string]string{access.MarkNeed: string(access.NeedWrite)},
		Create:  create,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			d, ok := access.DecisionOf(r)
			if !ok {
				return errNoDecision
			}
			t := target{workspace: server.Param(r, "slug"), runID: server.Param(r, "run_id")}
			if !runIDForm.MatchString(t.runID) {
				return server.InvalidRequest(msgRunID)
			}
			act, err := validate(r, t)
			if err != nil {
				return err
			}
			if d.Record.Status == access.StatusArchived {
				return server.WorkspaceArchived(msgArchived)
			}
			err = act(r.Context(), w)
			if errors.Is(err, errWriteTimeout) {
				w.Header().Set("Retry-After", retryAfter)
				return server.Unavailable(msgWriteTimeout)
			}
			return err
		},
	}
}

// single is the validator of a single-record route: its body is one JSON
// object, whose run_id member, when present, must equal the path's, and
// whose members read checks.
func single(read func(f fields, t target) (act, error)) validator {
	return func(r *http.Request, t target) (act, error) {
		var f fields
		if err := server.DecodeJSON(r, &f); err != nil {
			return nil, err
		}
		if err := f.runID(t.runID); err != nil {
			return nil, err
		}
		return read(f, t)
	}
}
