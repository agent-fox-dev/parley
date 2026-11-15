package config

import (
	"errors"
	"time"
)

// SCM holds the keys of [scm] the loader owns: the bounds of a read of a
// workspace's repository through the source control API, and of the body of
// a commit request.
type SCM struct {
	// ReadTimeout is how long a read may run before it is ended and
	// answered as timed out; above zero.
	ReadTimeout time.Duration
	// MaxFileSize is the size in bytes of the largest file a read serves;
	// above zero.
	MaxFileSize int64
	// MaxCommitSize is the size in bytes of the largest body a commit
	// request may have; above zero.
	MaxCommitSize int64
}

// scmKeys are the keys of [scm] the loader owns. The section stays open to
// keys other packages register.
var scmKeys = []Key{
	{Section: "scm", Name: "read_timeout", Type: String, Default: "30s", Validate: func(v any) error {
		d, err := durationValue(v)
		if err == nil && d <= 0 {
			err = errors.New("must be above zero")
		}
		return err
	}},
	{Section: "scm", Name: "max_file_size", Type: String, Default: "32MB", Validate: sizeValue},
	{Section: "scm", Name: "max_commit_size", Type: String, Default: "32MB", Validate: sizeValue},
}

// sizeValue checks a size such as "32MB".
func sizeValue(v any) error {
	_, err := ParseSize(v.(string))
	return err
}

// scmFrom returns the [scm] bounds of checked values.
func scmFrom(values map[string]map[string]any) SCM {
	keys := values["scm"]
	// Each value is checked by its key's rule or is the key's default.
	timeout, _ := durationValue(keys["read_timeout"])
	size, _ := ParseSize(keys["max_file_size"].(string))
	commit, _ := ParseSize(keys["max_commit_size"].(string))
	return SCM{ReadTimeout: timeout, MaxFileSize: size, MaxCommitSize: commit}
}
