package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// codeIndexSection is the section that turns the code index on.
const codeIndexSection = "code_index"

// CodeIndex holds [code_index]: whether the hub keeps a code index of its
// workspaces, and the bounds of one build. The zero value is a hub without
// the section, which keeps none.
type CodeIndex struct {
	// Enabled is true when the file holds the section, even an empty one.
	Enabled bool
	// MaxFiles is the most files one index holds; 1 or more.
	MaxFiles int
	// MaxSize is the most bytes of content one index holds; above zero.
	MaxSize int64
	// BuildTimeout is the longest one build may run; above zero.
	BuildTimeout time.Duration
	// Workers is how many builds run at once; 1 to 16.
	Workers int
}

// The defaults of [code_index].
const (
	defaultCodeIndexMaxFiles     = 100000
	defaultCodeIndexMaxSize      = 1 << 30
	defaultCodeIndexBuildTimeout = 5 * time.Minute
	defaultCodeIndexWorkers      = 2
	maxCodeIndexWorkers          = 16
)

// codeIndexFrom reads the section's table, with each key's default in place
// of a key the table does not hold, and returns every failure of the section
// together, each naming its key as code_index.<key>.
func codeIndexFrom(table map[string]any) (CodeIndex, error) {
	c := CodeIndex{Enabled: true, MaxFiles: defaultCodeIndexMaxFiles, MaxSize: defaultCodeIndexMaxSize,
		BuildTimeout: defaultCodeIndexBuildTimeout, Workers: defaultCodeIndexWorkers}
	var failures []string
	fail := func(key, reason string) { failures = append(failures, codeIndexSection+"."+key+": "+reason) }
	integer := func(key string, lowest, highest int64, into *int) {
		v, given := table[key]
		if !given {
			return
		}
		n, ok := v.(int64)
		switch {
		case !ok || n < lowest || (highest > 0 && n > highest):
			if highest > 0 {
				fail(key, fmt.Sprintf("must be an integer from %d to %d", lowest, highest))
			} else {
				fail(key, fmt.Sprintf("must be an integer of %d or more", lowest))
			}
		default:
			*into = int(n)
		}
	}
	for _, key := range sortedKeys(table) {
		if !slices.Contains([]string{"max_files", "max_size", "build_timeout", "workers"}, key) {
			fail(key, "unknown key")
		}
	}
	integer("max_files", 1, 0, &c.MaxFiles)
	integer("workers", 1, maxCodeIndexWorkers, &c.Workers)
	if v, given := table["max_size"]; given {
		s, _ := v.(string)
		if size, err := ParseSize(s); err != nil {
			fail("max_size", err.Error())
		} else {
			c.MaxSize = size
		}
	}
	if v, given := table["build_timeout"]; given {
		s, isString := v.(string)
		d, err := durationValue(s)
		switch {
		case !isString || err != nil:
			fail("build_timeout", "must be a duration with a unit of s, m, h or d, such as 5m")
		case d <= 0:
			fail("build_timeout", "must be above zero")
		default:
			c.BuildTimeout = d
		}
	}
	if len(failures) > 0 {
		return CodeIndex{}, errors.New(strings.Join(failures, "; "))
	}
	return c, nil
}
