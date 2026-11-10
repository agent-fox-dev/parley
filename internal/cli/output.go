package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// showOnceWarning follows the result of a command that prints a secret
// shown once.
const showOnceWarning = "this value is shown once and will not be shown again"

// printResult writes a JSON body to stdout as one document indented by 2
// spaces with one trailing newline, without decoding it into values, so
// member order and number text stay as received; an empty body prints {}.
func printResult(w io.Writer, body []byte) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		_, err := io.WriteString(w, "{}\n")
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err != nil {
		return ClientError("the hub's answer is not JSON: %v", err)
	}
	_, err := fmt.Fprintf(w, "%s\n", out.Bytes())
	return err
}

// printDocument writes a value as one JSON document indented by 2 spaces.
func printDocument(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return ClientError("cannot encode the result: %v", err)
	}
	return printResult(w, b)
}

// warnf writes one warning line to stderr; a warning begins with
// "warning: ".
func warnf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "warning: "+format+"\n", args...)
}

// progressf writes one progress line of a wait to stderr; a progress line
// begins with "waiting: ".
func progressf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "waiting: "+format+"\n", args...)
}
