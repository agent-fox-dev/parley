package forge

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
)

// labelsAdd is the number of the issue labels are added to, and their
// names.
type labelsAdd struct {
	number int64
	names  []string
}

// readLabelsAdd reads the number and the body of a label addition: a bare
// JSON array of 1 to maxNames names, none blank.
func readLabelsAdd(r *http.Request, call *Call) error {
	n, err := PathNumber(r)
	if err != nil {
		return err
	}
	var raw []byte
	if r.Body != nil {
		if raw, err = io.ReadAll(r.Body); err != nil {
			return server.InvalidRequest("the body cannot be read")
		}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return server.InvalidRequest("the body must be a JSON array of label names")
	}
	names, err := readNames("labels", raw)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return server.InvalidRequest("labels must hold at least one name")
	}
	call.Params = labelsAdd{number: n, names: names}
	return nil
}

// addLabels adds the names to the issue and leaves the labels it carries in
// place; the answer has no body. Its event has the number and the names as
// labels.
func addLabels(ctx context.Context, call *Call) (written, error) {
	in := call.Params.(labelsAdd)
	ev := writeEvent{number: in.number, extra: map[string]any{"labels": in.names}}
	if err := writerOf(call.Resolution.Kind).addLabels(ctx, call, in.number, in.names); err != nil {
		return written{event: ev}, err
	}
	return written{status: http.StatusNoContent, event: ev}, nil
}

// labelRemove is the number of the issue a label is removed from, and the
// label's name.
type labelRemove struct {
	number int64
	name   string
}

// readLabelRemove reads the number and the label name of a removal: the
// last path segment, which the router has decoded once, so that type%2Fbug
// names type/bug. A blank name is refused.
func readLabelRemove(r *http.Request, call *Call) error {
	n, err := PathNumber(r)
	if err != nil {
		return err
	}
	name := server.Param(r, "name")
	if blank(name) {
		return server.InvalidRequest("the label name must not be blank")
	}
	call.Params = labelRemove{number: n, name: name}
	return nil
}

// removeLabel removes the label from the issue; an issue that does not
// carry it is left as it is, and the answer is the same, with no body. Its
// event has the number and the name as label.
func removeLabel(ctx context.Context, call *Call) (written, error) {
	in := call.Params.(labelRemove)
	ev := writeEvent{number: in.number, extra: map[string]any{"label": in.name}}
	if err := writerOf(call.Resolution.Kind).removeLabel(ctx, call, in.number, in.name); err != nil {
		return written{event: ev}, err
	}
	return written{status: http.StatusNoContent, event: ev}, nil
}

// defaultLabelColor is the colour of a label created without one.
const defaultLabelColor = "ededed"

// labelEnsure is the label an ensure creates when the forge does not hold
// it: its name, its colour in lower case without #, and its description,
// nil when it is not set.
type labelEnsure struct {
	name, color string
	description *string
}

// readLabelEnsure reads the body of a label ensure: a name, required and
// not blank; a colour, six hex digits in either case with or without a
// leading #; a description, a string.
func readLabelEnsure(r *http.Request, call *Call) error {
	m, err := readObject(r)
	if err != nil {
		return err
	}
	var name, color, description *string
	if err := cmp.Or(m.required("name", &name), m.text("color", &color), m.text("description", &description)); err != nil {
		return err
	}
	in := labelEnsure{name: *name, color: defaultLabelColor, description: description}
	if color != nil {
		c, ok := labelColor(*color)
		if !ok {
			return server.InvalidRequest("color must be six hex digits, with or without a leading #")
		}
		in.color = c
	}
	call.Params = in
	return nil
}

// labelColor is c in lower case without its leading #, and false when it
// is not six hex digits.
func labelColor(c string) (string, bool) {
	c = strings.ToLower(strings.TrimPrefix(c, "#"))
	if len(c) != 6 || strings.Trim(c, "0123456789abcdef") != "" {
		return "", false
	}
	return c, true
}

// ensureLabel answers the label of the name the request gives as the forge
// holds it, unchanged, or creates it when the forge holds none. A label
// another caller created between the lookup and the create is read again
// and answered as found. The answer is 201 when this call created the
// label, else 200. Its event has the name as label and whether this call
// created it.
func ensureLabel(ctx context.Context, call *Call) (written, error) {
	in := call.Params.(labelEnsure)
	ev := writeEvent{id: in.name, extra: map[string]any{"label": in.name, "created": false}}
	f := writerOf(call.Resolution.Kind)
	label, err := f.label(ctx, call, in.name)
	switch {
	case err == nil:
		return written{status: http.StatusOK, body: label, event: ev}, nil
	case !forgeAnswered(err, http.StatusNotFound):
		return written{event: ev}, err
	}
	label, err = f.createLabel(ctx, call, in)
	if err == nil {
		ev.extra["created"] = true
		return written{status: http.StatusCreated, body: label, event: ev}, nil
	}
	if !forgeAnswered(err, labelExists(call.Resolution.Kind)) {
		return written{event: ev}, err
	}
	label, again := f.label(ctx, call, in.name)
	switch {
	case again == nil:
		return written{status: http.StatusOK, body: label, event: ev}, nil
	case forgeAnswered(again, http.StatusNotFound):
		// The refusal was not about a label of that name after all.
		return written{event: ev}, err
	}
	return written{event: ev}, again
}

// labelExists is the status a forge of kind refuses the create of a label
// with when one of that name exists.
func labelExists(kind ForgeKind) int {
	if kind == KindGitLab {
		return http.StatusConflict
	}
	return http.StatusUnprocessableEntity
}

// forgeAnswered reports whether err is the forge's answer with status.
func forgeAnswered(err error, status int) bool {
	f, ok := errors.AsType[*Failure](err)
	return ok && f.Kind == FailureStatus && f.Status == status
}

// labelWrite is the body of a label create on both forges: the colour in
// the form the forge takes, and the description when it is set.
type labelWrite struct {
	Name        string  `json:"name"`
	Color       string  `json:"color"`
	Description *string `json:"description,omitempty"`
}

// forgeLabel is a label as both forges give it, GitLab's colour with a
// leading #.
type forgeLabel struct {
	Name        string  `json:"name"`
	Color       string  `json:"color"`
	Description *string `json:"description"`
}

func (g forgeLabel) label() Label {
	l := Label{Name: g.Name, Color: strings.ToLower(strings.TrimPrefix(g.Color, "#"))}
	if g.Description != nil {
		l.Description = *g.Description
	}
	return l
}

// labelPath is the API path of the label name of the repository or project
// at repo: the name is one escaped segment, so that a name holding a slash
// names one label.
func labelPath(repo, name string) string {
	return repo + "/labels/" + url.PathEscape(name)
}

// readLabel reads the label name below the repository or project at repo.
func readLabel(ctx context.Context, call *Call, repo, name string) (Label, error) {
	var g forgeLabel
	if err := getJSON(ctx, call, labelPath(repo, name), &g); err != nil {
		return Label{}, err
	}
	return g.label(), nil
}

// createLabel creates the label body describes below the repository or
// project at repo.
func createLabel(ctx context.Context, call *Call, repo string, body labelWrite) (Label, error) {
	var g forgeLabel
	if err := call.sendJSON(ctx, http.MethodPost, repo+"/labels", body, &g); err != nil {
		return Label{}, err
	}
	return g.label(), nil
}

func (github) addLabels(ctx context.Context, call *Call, number int64, names []string) error {
	var labels []forgeLabel
	path := githubIssuePath(call.Resolution.Repository, number) + "/labels"
	return call.sendJSON(ctx, http.MethodPost, path, struct {
		Labels []string `json:"labels"`
	}{names}, &labels)
}

// removeLabel deletes the label from the issue. GitHub answers 404 both for
// an issue that does not exist and for one that does not carry the label,
// so a 404 is told apart by reading the issue: when it exists, the label is
// already gone.
func (github) removeLabel(ctx context.Context, call *Call, number int64, name string) error {
	var labels []forgeLabel
	path := githubIssuePath(call.Resolution.Repository, number) + "/labels/" + url.PathEscape(name)
	err := call.sendJSON(ctx, http.MethodDelete, path, nil, &labels)
	if !forgeAnswered(err, http.StatusNotFound) {
		return err
	}
	_, err = github{}.issue(ctx, call, number)
	return err
}

func (github) label(ctx context.Context, call *Call, name string) (Label, error) {
	return readLabel(ctx, call, githubRepo(call.Resolution.Repository), name)
}

func (github) createLabel(ctx context.Context, call *Call, in labelEnsure) (Label, error) {
	return createLabel(ctx, call, githubRepo(call.Resolution.Repository),
		labelWrite{Name: in.name, Color: in.color, Description: in.description})
}

// gitlabLabelChange is the body of an issue update on GitLab that adds or
// removes labels and leaves the others in place, each list one string of
// names separated by commas.
type gitlabLabelChange struct {
	AddLabels    string `json:"add_labels,omitempty"`
	RemoveLabels string `json:"remove_labels,omitempty"`
}

func (gitlab) addLabels(ctx context.Context, call *Call, number int64, names []string) error {
	var g gitlabIssue
	return call.sendJSON(ctx, http.MethodPut, gitlabIssuePath(call.Resolution.Repository, number),
		gitlabLabelChange{AddLabels: strings.Join(names, ",")}, &g)
}

// removeLabel takes the label off the issue; GitLab answers alike whether
// or not the issue carried it, and 404 when the issue does not exist.
func (gitlab) removeLabel(ctx context.Context, call *Call, number int64, name string) error {
	var g gitlabIssue
	return call.sendJSON(ctx, http.MethodPut, gitlabIssuePath(call.Resolution.Repository, number),
		gitlabLabelChange{RemoveLabels: name}, &g)
}

func (gitlab) label(ctx context.Context, call *Call, name string) (Label, error) {
	return readLabel(ctx, call, gitlabProject(call.Resolution.Repository), name)
}

func (gitlab) createLabel(ctx context.Context, call *Call, in labelEnsure) (Label, error) {
	return createLabel(ctx, call, gitlabProject(call.Resolution.Repository),
		labelWrite{Name: in.name, Color: "#" + in.color, Description: in.description})
}
