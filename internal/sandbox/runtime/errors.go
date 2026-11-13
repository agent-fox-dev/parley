package runtime

import "errors"

// The four kinds of failure. Every error a Client returns is of exactly one
// of them, which a caller tests with errors.Is.
var (
	// ErrNotFound: the runtime has no such sandbox, provider, provider
	// profile, proposal or path.
	ErrNotFound = errors.New("not found")
	// ErrInvalid: the runtime understood the request and refused it; the
	// error's text keeps the runtime's reason.
	ErrInvalid = errors.New("invalid")
	// ErrUnsupported: the runtime in use has no such operation.
	ErrUnsupported = errors.New("unsupported")
	// ErrUnavailable: every other failure, a gateway that cannot be reached,
	// refuses the hub's credential, answers with a server error or does not
	// answer in time included.
	ErrUnavailable = errors.New("unavailable")
)

// Error is a failed operation of a Client. It keeps text only, not the
// error it was made from, so that what an implementation scrubbed from its
// text cannot be reached through errors.Unwrap, and so that errors.Is finds
// exactly one kind.
type Error struct {
	// Kind is ErrNotFound, ErrInvalid, ErrUnsupported or ErrUnavailable.
	Kind error
	// Op is the operation's name (OpGetSandbox, …), "" when unknown.
	Op string
	// Reason is the runtime's reason or what went wrong, "" when there is
	// nothing to add to the kind.
	Reason string
}

// NewError returns an error of kind for an operation. A kind that is not
// one of the four is taken for ErrUnavailable, every other failure.
func NewError(kind error, op, reason string) *Error {
	e := &Error{Kind: kind, Op: op, Reason: reason}
	e.Kind = e.kind()
	return e
}

// Error reads "sandbox runtime: <op>: <kind>: <reason>", leaving out an
// empty op or reason.
func (e *Error) Error() string {
	text := "sandbox runtime: "
	if e.Op != "" {
		text += e.Op + ": "
	}
	text += e.kind().Error()
	if e.Reason != "" {
		text += ": " + e.Reason
	}
	return text
}

// Is reports whether target is the error's kind.
func (e *Error) Is(target error) bool {
	return target == e.kind()
}

// kind is the error's kind, ErrUnavailable for an Error built without one
// of the four.
func (e *Error) kind() error {
	switch e.Kind {
	case ErrNotFound, ErrInvalid, ErrUnsupported:
		return e.Kind
	}
	return ErrUnavailable
}

// KindOf returns the kind of err: ErrNotFound, ErrInvalid, ErrUnsupported or
// ErrUnavailable. An error of none of them is ErrUnavailable, every other
// failure; nil is nil.
func KindOf(err error) error {
	if err == nil {
		return nil
	}
	for _, kind := range []error{ErrNotFound, ErrInvalid, ErrUnsupported} {
		if errors.Is(err, kind) {
			return kind
		}
	}
	return ErrUnavailable
}
