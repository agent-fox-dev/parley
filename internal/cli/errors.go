package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The types of the errors the CLI raises itself.
const (
	TypeUsage       = "usage_error"
	TypeClient      = "client_error"
	TypeWaitFailed  = "wait_failed"
	TypeWaitTimeout = "wait_timeout"
	TypeCondition   = "condition_not_met"
)

// HubError is an error envelope the hub answered with an HTTP status of 400
// or above. It is printed as received and ends the command with status 1.
type HubError struct {
	Status int
	Body   []byte
}

func (e *HubError) Error() string { return fmt.Sprintf("the hub answered %d", e.Status) }

// Error is an error the CLI raises itself, printed as an envelope with code
// 0 and one of the five types.
type Error struct {
	Type    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func newError(errorType, format string, args []any) error {
	return &Error{Type: errorType, Message: fmt.Sprintf(format, args...)}
}

// UsageError is an unknown command or flag, a wrong argument count or an
// invalid flag value: exit status 2.
func UsageError(format string, args ...any) error { return newError(TypeUsage, format, args) }

// ClientError is a missing setting, an unreadable configuration file, a
// failed connection, a timeout or an undecodable answer: exit status 2.
func ClientError(format string, args ...any) error { return newError(TypeClient, format, args) }

// WaitFailed is a --wait that ended in an unsuccessful state: exit status 1.
func WaitFailed(format string, args ...any) error { return newError(TypeWaitFailed, format, args) }

// WaitTimeout is a --wait that passed its --timeout: exit status 1.
func WaitTimeout(format string, args ...any) error { return newError(TypeWaitTimeout, format, args) }

// ConditionNotMet is a --fail-on-… condition that held: exit status 3.
func ConditionNotMet(format string, args ...any) error { return newError(TypeCondition, format, args) }

// ExitCode maps the outcome of a command to its exit status: 0 for none; 1
// for a hub error, wait_failed and wait_timeout; 2 for usage_error,
// client_error and any error of no named kind; 3 for condition_not_met.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var hub *HubError
	if errors.As(err, &hub) {
		return 1
	}
	var ce *Error
	if errors.As(err, &ce) {
		switch ce.Type {
		case TypeWaitFailed, TypeWaitTimeout:
			return 1
		case TypeCondition:
			return 3
		}
	}
	return 2
}

// PrintError writes an error to stderr as one JSON document indented by 2
// spaces with a newline: a hub error's envelope as received, member order
// and extra members kept; a CLI error as {"error":{"code":0,"message":…,
// "error_type":…}}; an error of no named kind as a client_error with its
// text as the message.
func PrintError(w io.Writer, err error) {
	var hub *HubError
	if errors.As(err, &hub) {
		var out bytes.Buffer
		if json.Indent(&out, bytes.TrimSpace(hub.Body), "", "  ") == nil {
			fmt.Fprintf(w, "%s\n", out.Bytes())
			return
		}
	}
	ce := &Error{Type: TypeClient, Message: err.Error()}
	errors.As(err, &ce)
	doc := struct {
		Error struct {
			Code      int    `json:"code"`
			Message   string `json:"message"`
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}{}
	doc.Error.Message, doc.Error.ErrorType = ce.Message, ce.Type
	b, _ := json.MarshalIndent(doc, "", "  ")
	fmt.Fprintf(w, "%s\n", b)
}
