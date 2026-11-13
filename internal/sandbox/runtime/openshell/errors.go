package openshell

import (
	"context"
	"errors"
	"strings"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

// classify returns the kind of an SDK failure and the reason to keep with
// it. ctx is the context the call ran with: a call it ended is unavailable,
// whatever the SDK made of it.
//
// The SDK's not-found is not found. A request the gateway understood and
// refused (an invalid argument, a resource that exists already, a conflict
// with the resource's state) is invalid with the gateway's reason. A call
// the SDK or the gateway does not implement, and a file transfer in an SDK
// built without its transport, are unsupported. Everything else is
// unavailable: a gateway that cannot be reached or does not answer in time,
// one that refuses the hub's credential, and one that fails with a server
// error.
func classify(ctx context.Context, err error) (kind error, reason string) {
	var re *runtime.Error
	if errors.As(err, &re) {
		return re.Kind, re.Reason
	}
	if errors.Is(err, v1.ErrTransportNotAvailable) {
		return runtime.ErrUnsupported, "the SDK in use has no file transport"
	}
	var se *v1.StatusError
	isStatus := errors.As(err, &se)
	if ctxErr := ctx.Err(); ctxErr != nil && (!isStatus || ended(se.Code)) {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return runtime.ErrUnavailable, "the gateway did not answer in time"
		}
		return runtime.ErrUnavailable, "the call was canceled"
	}
	if !isStatus {
		return runtime.ErrUnavailable, err.Error()
	}
	switch se.Code {
	case v1.ErrorNotFound:
		return runtime.ErrNotFound, se.Message
	case v1.ErrorInvalidArgument, v1.ErrorAlreadyExists, v1.ErrorConflict:
		return runtime.ErrInvalid, statusReason(se)
	case v1.ErrorUnimplemented:
		return runtime.ErrUnsupported, se.Message
	}
	return runtime.ErrUnavailable, se.Error()
}

// ended reports whether code is what the SDK reports for a call whose
// context ended or that could not reach the gateway before it did.
func ended(code v1.ErrorCode) bool {
	switch code {
	case v1.ErrorDeadlineExceeded, v1.ErrorCancelled, v1.ErrorUnavailable:
		return true
	}
	return false
}

// statusReason is the gateway's message followed by the fields it names.
func statusReason(se *v1.StatusError) string {
	parts := []string{se.Message}
	for _, fv := range se.FieldViolations {
		parts = append(parts, fv.Field+": "+fv.Description)
	}
	return strings.Join(parts, "; ")
}

// unsupported is the error of an operation the SDK in use has no call for.
func unsupported(op, reason string) error {
	return runtime.NewError(runtime.ErrUnsupported, op, reason)
}

// invalid is the error of a request the client refuses before it reaches
// the gateway.
func invalid(op, reason string) error {
	return runtime.NewError(runtime.ErrInvalid, op, reason)
}

// notFound is the error of a lookup the client makes itself that finds
// nothing.
func notFound(op, reason string) error {
	return runtime.NewError(runtime.ErrNotFound, op, reason)
}
