package hubtest

import (
	"testing"
	"time"
)

// Eventually waits for cond to hold, checking it every few milliseconds, and
// fails the test when it does not within 20 seconds, a bound long enough for
// a busy machine. what names the awaited state in the failure.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	const within = 20 * time.Second
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
