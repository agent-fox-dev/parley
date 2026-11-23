//go:build !(darwin && cgo)

package spawn

import (
	"errors"
	"os"
	"syscall"
)

// available is false: os/exec starts every command. On Linux it starts
// them without copying the caller already.
const available = false

func supported(*syscall.SysProcAttr) bool { return false }

func spawn(string, []string, []string, string, [3]*os.File, bool) (int, error) {
	return 0, errors.New("spawn is not available on this platform")
}
