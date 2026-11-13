package cli

import (
	"os/exec"
	"runtime"
	"time"
)

// openerGrace is how long openSystemBrowser waits for the system's opener to
// report a failure. Some openers stay with the browser they started; one
// still running after this long is taken to have opened the page.
const openerGrace = 5 * time.Second

// openSystemBrowser opens url in the user's browser with the system's
// opener: open on macOS, rundll32 on Windows, xdg-open elsewhere. It fails
// when the opener cannot be started or exits with an error within
// openerGrace.
func openSystemBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(openerGrace)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return nil
	}
}
