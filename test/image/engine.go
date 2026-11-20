// Package image holds the tests of the container images the repository
// builds. They need a container engine and are skipped unless the
// environment asks for them, so that an ordinary test run compiles and lints
// them without running any container.
package image

import (
	"fmt"
	"os/exec"
)

// The variables that ask for the image tests and name their engine.
const (
	testsVar  = "PARLEY_IMAGE_TESTS"
	engineVar = "PARLEY_CONTAINER_ENGINE"
)

// defaultEngine is the engine used when PARLEY_CONTAINER_ENGINE is unset or
// empty. Any command that takes docker's arguments will do.
const defaultEngine = "docker"

// Engine returns the container engine the image tests run, read from the
// environment through lookupEnv. It reports skip, and looks nothing up,
// unless PARLEY_IMAGE_TESTS is 1. Asked for, the engine is the command
// PARLEY_CONTAINER_ENGINE names, docker by default, and an error naming the
// command when it is not on the PATH.
func Engine(lookupEnv func(string) (string, bool)) (engine string, skip bool, err error) {
	if v, _ := lookupEnv(testsVar); v != "1" {
		return "", true, nil
	}
	engine = defaultEngine
	if v, _ := lookupEnv(engineVar); v != "" {
		engine = v
	}
	if _, err := exec.LookPath(engine); err != nil {
		return "", false, fmt.Errorf("the container engine %q (%s) is not on the PATH: %w", engine, engineVar, err)
	}
	return engine, false, nil
}
