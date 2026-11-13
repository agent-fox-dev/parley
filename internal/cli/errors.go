package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// The exit statuses of parley, one meaning each on every command.
const (
	// exitHubError: the hub answered an error, or a wait failed or timed out.
	exitHubError = 1
	// exitClientError: a usage error, or a failure on the client's side.
	exitClientError = 2
	// exitConditionNotMet: a --fail-on-… condition held in the result.
	exitConditionNotMet = 3
)

// The error types of the errors the CLI raises itself.
const (
	usageErrorType      = "usage_error"
	clientErrorType     = "client_error"
	waitFailedType      = "wait_failed"
	waitTimeoutType     = "wait_timeout"
	conditionNotMetType = "condition_not_met"
)

// hubError is an error envelope the hub answered, kept as received so that
// its member order and extra members reach stderr unchanged.
type hubError struct {
	status int
	body   []byte
}

func (e *hubError) Error() string {
	return fmt.Sprintf("the hub answered %d: %s", e.status, bytes.TrimSpace(e.body))
}

// cliError is an error the CLI raises itself, of one of the five CLI error
// types.
type cliError struct {
	errorType string
	message   string
}

func (e *cliError) Error() string { return e.message }

func newCLIError(errorType, format string, args []any) error {
	return &cliError{errorType: errorType, message: fmt.Sprintf(format, args...)}
}

// usageError is a command line parley cannot accept: an unknown command or
// flag, a wrong number of arguments, a value that does not parse.
func usageError(format string, args ...any) error {
	return newCLIError(usageErrorType, format, args)
}

// clientError is a failure on the client's side that is not the arguments'
// fault: a missing setting, an unreadable file, an unreachable hub, an answer
// that is not the hub's.
func clientError(format string, args ...any) error {
	return newCLIError(clientErrorType, format, args)
}

// waitFailed is a --wait that ended in a terminal state that is not a success.
func waitFailed(format string, args ...any) error {
	return newCLIError(waitFailedType, format, args)
}

// waitTimeout is a --wait whose --timeout passed before a terminal state.
func waitTimeout(format string, args ...any) error {
	return newCLIError(waitTimeoutType, format, args)
}

// conditionNotMet is a --fail-on-… condition that holds in the result.
func conditionNotMet(format string, args ...any) error {
	return newCLIError(conditionNotMetType, format, args)
}

// exitCode is the exit status err ends parley with. An error of no named
// kind counts as a client error, because the status must be one of 0 to 3.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var he *hubError
	if errors.As(err, &he) {
		return exitHubError
	}
	var ce *cliError
	if errors.As(err, &ce) {
		switch ce.errorType {
		case waitFailedType, waitTimeoutType:
			return exitHubError
		case conditionNotMetType:
			return exitConditionNotMet
		}
	}
	return exitClientError
}

// isHubEnvelope tells whether body is the hub's error envelope: a JSON
// object whose error member is an object holding error_type.
func isHubEnvelope(body []byte) bool {
	var doc struct {
		Error *struct {
			ErrorType *string `json:"error_type"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &doc) == nil && doc.Error != nil && doc.Error.ErrorType != nil
}

// cliEnvelope is the error envelope of an error the CLI raises itself, in the
// hub's shape with code 0.
type cliEnvelope struct {
	Error struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		ErrorType string `json:"error_type"`
	} `json:"error"`
}

// printError writes err to w as one error envelope: the hub's as received, or
// the CLI's own, indented by 2 spaces and followed by a newline. An error of
// no named kind is written as a client_error with its text as the message.
func printError(w io.Writer, err error) {
	var he *hubError
	if errors.As(err, &he) {
		// Re-indented, not decoded and encoded again, so that the member
		// order and the extra members stay as the hub sent them.
		out, err := indentJSON(he.body)
		if err != nil {
			out = he.body
		}
		fmt.Fprintf(w, "%s", out)
		return
	}
	var env cliEnvelope
	env.Error.ErrorType = clientErrorType
	env.Error.Message = err.Error()
	var ce *cliError
	if errors.As(err, &ce) {
		env.Error.ErrorType = ce.errorType
	}
	if env.Error.Message == "" {
		// The envelope promises a message, and an empty one says nothing.
		env.Error.Message = env.Error.ErrorType
	}
	out, _ := json.MarshalIndent(env, "", "  ")
	fmt.Fprintf(w, "%s\n", out)
}
