package cli

import (
	"os/exec"
	"runtime"
)

// openBrowser opens a URL with the system's opener: open on macOS,
// xdg-open on Linux and rundll32 url.dll,FileProtocolHandler on Windows.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// The pages the browser shows after the redirect.
const (
	successPage = `<!doctype html><html><head><meta charset="utf-8"><title>Parley</title></head><body><p>You are signed in to Parley. You can close this tab.</p></body></html>`
	failurePage = `<!doctype html><html><head><meta charset="utf-8"><title>Parley</title></head><body><p>The sign-in to Parley failed. Return to the terminal.</p></body></html>`
)
