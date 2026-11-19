//go:build unix

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize has ch told when the terminal's size changes.
func notifyResize(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGWINCH)
}
