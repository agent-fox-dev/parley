package jobs

import (
	"encoding/json"
	"errors"
)

// ErrNotRunning is the error of an operation that needs a running job, on a
// job that is not running or does not exist.
var ErrNotRunning = errors.New("job not running")

// permanentError marks the error it wraps as one a retry cannot cure.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }

func (e *permanentError) Unwrap() error { return e.err }

// Permanent marks err permanent: an attempt that returns it dead-letters
// its job at once instead of retrying it. Any error a handler returns that
// is not marked is retryable. Permanent(nil) is nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// detailError attaches a detail document to the error it wraps.
type detailError struct {
	err error
	doc json.RawMessage
}

func (e *detailError) Error() string { return e.err.Error() }

func (e *detailError) Unwrap() error { return e.err }

// WithDetail attaches the JSON document doc to err: the settlement of an
// attempt that returns it stores doc as the job's failure detail. Its text
// is err's. WithDetail(nil, doc) is nil.
func WithDetail(err error, doc json.RawMessage) error {
	if err == nil {
		return nil
	}
	return &detailError{err: err, doc: doc}
}

// isPermanent reports whether err is marked permanent.
func isPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// detailOf is the detail document err carries, nil when it carries none.
func detailOf(err error) json.RawMessage {
	var d *detailError
	if errors.As(err, &d) {
		return d.doc
	}
	return nil
}
