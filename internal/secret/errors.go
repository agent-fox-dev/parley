package secret

import "errors"

// The errors the package's operations match with errors.Is. Their messages
// never carry a key or a value, so they can be wrapped into any error that
// leaves the hub.
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
)
