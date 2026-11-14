//go:build !unix

package gitrepo

import "os/exec"

// killGroupOnCancel leaves cmd as it is: without process groups, the end of
// its context kills git alone.
func killGroupOnCancel(*exec.Cmd) {}
