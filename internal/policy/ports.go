package policy

import "context"

// Users is what the validator asks about the users a document names: whether
// a user record with exactly that username exists, whatever its status. A
// failure of the lookup is returned as an error, never as false, so that a
// document is not refused for an unknown user the store could not read.
type Users interface {
	UserExists(ctx context.Context, username string) (bool, error)
}
