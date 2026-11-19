//go:build !unix

package cli

import "os"

// notifyResize does nothing where the system sends no signal when the
// terminal's size changes: the terminal keeps the size it was opened at.
func notifyResize(chan<- os.Signal) {}
