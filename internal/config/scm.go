package config

import (
	"errors"
	"time"
)

// SCM holds the keys of [scm] the loader owns: the bounds of a read of a
// workspace's repository through the source control API.
type SCM struct {
	// ReadTimeout is how long a read may run before it is ended and
	// answered as timed out; above zero.
	ReadTimeout time.Duration
	// MaxFileSize is the size in bytes of the largest file a read serves;
	// above zero.
	MaxFileSize int64
}

// scmKeys are the keys of [scm] the loader owns. The section stays open to
// keys other packages register, such as the limits of writes.
var scmKeys = []Key{
	{Section: "scm", Name: "read_timeout", Type: String, Default: "30s", Validate: func(v any) error {
		d, err := durationValue(v)
		if err == nil && d <= 0 {
			err = errors.New("must be above zero")
		}
		return err
	}},
	{Section: "scm", Name: "max_file_size", Type: String, Default: "32MB", Validate: func(v any) error {
		_, err := ParseSize(v.(string))
		return err
	}},
}

// scmFrom returns the [scm] bounds of checked values.
func scmFrom(values map[string]map[string]any) SCM {
	keys := values["scm"]
	// Each value is checked by its key's rule or is the key's default.
	timeout, _ := durationValue(keys["read_timeout"])
	size, _ := ParseSize(keys["max_file_size"].(string))
	return SCM{ReadTimeout: timeout, MaxFileSize: size}
}
