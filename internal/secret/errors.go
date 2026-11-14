package secret

import (
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// The errors the package's operations match with errors.Is. Their messages
// never carry a value, so they can be wrapped into any error that leaves the
// hub; the messages of ErrInvalid, ErrConflict and ErrNotFound name the key
// at fault and are what a route answers.
var (
	// ErrUnavailable is returned by an operation that must seal a value while
	// the operator has configured no current key: the hub runs without one and
	// refuses only to store.
	ErrUnavailable = errors.New("no encryption key configured")
	// ErrUnreadable is returned when a sealed value cannot be opened: its key
	// id matches neither configured key, or it fails authentication. A value
	// that cannot be opened is never reported as absent, so the hub does not
	// act as if a credential were missing.
	ErrUnreadable = errors.New("cannot be decrypted")
	// ErrInvalid is matched by the error for a key or a value outside the
	// limits every secret is held to.
	ErrInvalid = errors.New("invalid secret")
	// ErrLimitReached is returned when a scope would hold more than
	// MaxEntries secrets. Its message is the one the routes answer.
	ErrLimitReached = errors.New("maximum of 100 entries per scope exceeded")
	// ErrConflict is matched by the error for a created key that the scope
	// already holds, ignoring ASCII case.
	ErrConflict = errors.New("secret already exists")
	// ErrNotFound is matched by the error for a key the scope does not hold.
	ErrNotFound = errors.New("secret not found")
)

// UnavailableError is how the REST API answers ErrUnavailable: 503
// secrets_unavailable. Every part of the hub that stores a secret on a
// caller's request answers it the same way.
var UnavailableError = server.NewError("secrets_unavailable", http.StatusServiceUnavailable,
	"secrets cannot be stored: no encryption key configured")

// keyError is an error about one key: its message names the key, and
// errors.Is matches it with its kind, one of the sentinels above.
type keyError struct {
	kind error
	msg  string
}

func (e *keyError) Error() string { return e.msg }

// Is reports whether target is the error's kind.
func (e *keyError) Is(target error) bool { return target == e.kind }
