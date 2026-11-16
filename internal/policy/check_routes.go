package policy

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The limits of a check report: summary and url in Unicode code points,
// details in bytes of its JSON text as received.
const (
	maxSummaryChars = 2000
	maxURLChars     = 2048
	maxDetailsBytes = 16384
)

// eventCheck is the type of the event a check report records.
const eventCheck = "parley.change.check"

// checkRoutes are the routes of the check results. Reporting one needs the
// scope checks:write, which no other scope implies, so that a runner's
// token can do nothing else, and write access to the workspace. The
// pattern is not in the contributor table of internal/access, so that a
// token bound to a task fork cannot report on the fork's parent a result
// for the work it pushed.
func (c *Component) checkRoutes() []server.Route {
	return []server.Route{{
		Method: http.MethodPut, Pattern: "/workspaces/:slug/changes/:number/checks/:name", Scope: "checks:write",
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: c.putCheck,
	}}
}

// checkRequest is the body of a check report: version is 0 for the change's
// current version, url and details nil when the report has none.
type checkRequest struct {
	status, summary string
	url             *string
	details         json.RawMessage
	version         int64
}

// putCheck stores the result of the check :name on a version of the change
// and answers it, 200 for a first report as for one that replaces the last.
// The refusals come in this order: a change the workspace does not have, a
// name or a body that is not valid, an archived workspace, a closed change
// and a version the change does not have. The admin token may report; it is
// named as the reporter with no user. The result, the landing it
// re-evaluates and its event are written in one store transaction, so that
// a failure of any of them writes none.
func (c *Component) putCheck(w http.ResponseWriter, r *http.Request) error {
	ctx, slug, name := r.Context(), server.Param(r, "slug"), server.Param(r, "name")
	found, err := c.existingChange(ctx, r)
	if err != nil {
		return err
	}
	// A result is reported under the names a policy document may require,
	// so the name has their form (checkName of the document).
	if !checkName.MatchString(name) {
		return server.InvalidRequest(fmt.Sprintf("check name %q must be 1 to 63 of a-z, 0-9, '_', '.' and '-', starting with a letter or digit", name))
	}
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	req, err := parseCheck(body)
	if err != nil {
		return err
	}
	p, _ := identity.PrincipalOf(ctx)
	by := writerOf(p)
	var res checkResult
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		ch, err := c.writableChange(ctx, tx, slug, found.Number)
		if err != nil {
			return err
		}
		s := storedCheck{
			Workspace: slug, Number: ch.Number, Version: req.version, Name: name, Status: req.status,
			Summary: req.summary, URL: req.url, Details: req.details, By: by, UpdatedAt: c.deps.Now(),
		}
		if s.Version == 0 {
			s.Version = ch.Version
		}
		if s.Version > ch.Version {
			return server.NotFound(fmt.Sprintf("%s has no version %d", ch.Ref, s.Version))
		}
		if err := upsertCheck(ctx, tx, s); err != nil {
			return err
		}
		if _, _, err := c.ReevaluateIn(ctx, tx, slug, ch.Number); err != nil {
			return err
		}
		res = checkOf(s)
		_, err = c.deps.Events.Emit(ctx, tx, event.Input{
			Type: eventCheck, Workspace: &slug, ActorType: by.ActorType, ActorID: by.ActorID,
			ResourceType: "change", ResourceID: strconv.FormatInt(ch.Number, 10),
			Metadata: map[string]any{"version": s.Version, "name": name, "status": s.Status,
				"number": ch.Number, "ref": ch.Ref},
		})
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, res)
	return nil
}

// parseCheck reads the members of a check report, refusing with 400
// invalid_request a member other than status, summary, url, details and
// version, a status other than pending, passed and failed, a summary that
// is not a string of at most maxSummaryChars code points, a url that is not
// an absolute http or https URL of at most maxURLChars, details that is not
// a JSON object of at most maxDetailsBytes as received, and a version that
// is not a positive integer.
func parseCheck(body map[string]json.RawMessage) (checkRequest, error) {
	var req checkRequest
	for name := range body {
		switch name {
		case "status", "summary", "url", "details", "version":
		default:
			return req, server.InvalidRequest(fmt.Sprintf("unknown member %q; a check report has status, summary, url, details and version", name))
		}
	}
	if json.Unmarshal(body["status"], &req.status) != nil ||
		(req.status != checkPending && req.status != checkPassed && req.status != checkFailed) {
		return req, server.InvalidRequest("status must be pending, passed or failed")
	}
	if raw, given := body["summary"]; given {
		var summary *string
		if json.Unmarshal(raw, &summary) != nil || summary == nil || utf8.RuneCountInString(*summary) > maxSummaryChars {
			return req, server.InvalidRequest(fmt.Sprintf("summary must be a string of at most %d characters", maxSummaryChars))
		}
		req.summary = *summary
	}
	if raw, given := body["url"]; given {
		if json.Unmarshal(raw, &req.url) != nil || req.url == nil || !webURL(*req.url) {
			return req, server.InvalidRequest(fmt.Sprintf("url must be an absolute http or https URL of at most %d characters", maxURLChars))
		}
	}
	if raw, given := body["details"]; given {
		// The member's bytes are its text as the request holds it, so the
		// limit is on what the runner sent, not on a re-encoding.
		if len(raw) == 0 || raw[0] != '{' || len(raw) > maxDetailsBytes {
			return req, server.InvalidRequest(fmt.Sprintf("details must be a JSON object of at most %d bytes", maxDetailsBytes))
		}
		req.details = raw
	}
	if raw, given := body["version"]; given {
		if json.Unmarshal(raw, &req.version) != nil || req.version <= 0 {
			return req, server.InvalidRequest("version must be a positive integer")
		}
	}
	return req, nil
}

// webURL reports whether s is an absolute http or https URL with a host, of
// at most maxURLChars characters.
func webURL(s string) bool {
	if utf8.RuneCountInString(s) > maxURLChars {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
