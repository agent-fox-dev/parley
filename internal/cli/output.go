package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// showOnceWarning is the line a command whose result carries a secret the
// hub shows only once writes after the result.
const showOnceWarning = "this value is shown once and will not be shown again"

// writeResult writes the JSON document body to w, indented by 2 spaces and
// followed by one newline. The body is re-indented, not decoded and encoded
// again, so the order of its members and the text of its numbers stay as
// the hub sent them. A body that is not one JSON document is refused and
// nothing is written.
func writeResult(w io.Writer, body []byte) error {
	out, err := indentJSON(body)
	if err != nil {
		return err
	}
	_, err = w.Write(out)
	return err
}

// indentJSON is the JSON document body, its white space removed and then
// indented by 2 spaces, with one newline after it.
func indentJSON(body []byte) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// warn writes one warning line to w, stderr.
func warn(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "warning: "+format+"\n", args...)
}

// progress writes one progress line to w, stderr.
func progress(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

// sendAndPrint is the run step of a command that declares none: it sends
// the command's method to its path, its :params filled in from the
// arguments, and prints the hub's answer.
func sendAndPrint(c *call) error {
	if c.client == nil {
		// Only a command whose auth is none has no client, and each of
		// those declares its own run step.
		return fmt.Errorf("%s has no run step", c.cmd.CommandPath())
	}
	resp, err := c.client.request(c.ctx, c.decl.Method, routePath(c.decl.Path, c.args), nil, nil)
	if err != nil {
		return err
	}
	return c.printAnswer(resp)
}

// routePath is path with each :param segment replaced by the argument of
// that name, encoded as one path segment.
func routePath(path string, args map[string]string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		if name, ok := strings.CutPrefix(s, ":"); ok {
			segments[i] = url.PathEscape(args[name])
		}
	}
	return strings.Join(segments, "/")
}

// printAnswer prints the hub's answer to the command's request: a success
// as the result, an error envelope as a hubError, which run prints on
// stderr.
func (c *call) printAnswer(resp *response) error {
	switch {
	case resp.status >= 400:
		if isHubEnvelope(resp.body) {
			return &hubError{status: resp.status, body: resp.body}
		}
		return clientError("the hub answered %d without an error envelope", resp.status)
	case resp.status < 200 || resp.status >= 300:
		return clientError("the hub answered %d, which is neither a success nor an error", resp.status)
	}
	body := resp.body
	if len(bytes.TrimSpace(body)) == 0 {
		// A success without a body still prints one document, so a script
		// reads one either way.
		body = []byte("{}")
		if c.decl.Confirm != nil {
			// The confirmation is built by the CLI, so encoding it loses
			// nothing the hub sent.
			confirmation, err := json.Marshal(c.decl.Confirm(c))
			if err != nil {
				return clientError("cannot encode the confirmation: %v", err)
			}
			body = confirmation
		}
	}
	if !json.Valid(body) {
		return clientError("the hub answered %d with a body that is not JSON", resp.status)
	}
	return c.printResult(body)
}

// printResult prints result, one JSON document, on stdout, then the
// show-once warning of a command that declares it, and returns
// condition_not_met for the first --fail-on-… flag given whose condition
// holds in result.
func (c *call) printResult(result []byte) error {
	if err := writeResult(c.stdout, result); err != nil {
		return clientError("cannot print the result: %v", err)
	}
	if c.decl.ShowsOnce {
		warn(c.stderr, showOnceWarning)
	}
	for _, f := range c.decl.FailOn {
		if given, _ := c.cmd.Flags().GetBool(f.flag()); !given {
			continue
		}
		if holds, message := f.Check(result); holds {
			return conditionNotMet("%s", message)
		}
	}
	return nil
}

// flag is the name of the --fail-on-… flag.
func (f failOn) flag() string { return "fail-on-" + f.Name }
