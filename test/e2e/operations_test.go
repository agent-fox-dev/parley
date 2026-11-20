package e2e

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
)

func TestOperations_AHubRefusesAnOldGitAndStartsOnceAUsableOneIsOnThePath(t *testing.T) {
	// The script is written before the test runs beside others, so that no
	// process they fork holds it open for writing when the hub runs it.
	oldGit := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldGit, "git"), []byte("#!/bin/sh\necho 'git version 2.30.2'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	parallel(t)
	dir := t.TempDir()
	writeConfig(t, dir, "[server]\nbind = \"127.0.0.1\"\nport = 0\n")

	// A hub that started anyway is stopped after a while, so that the test
	// fails instead of waiting for it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	code := hub.Run(ctx, hub.Options{WorkDir: dir, Env: config.MapEnv(map[string]string{"PATH": oldGit}), Stderr: &stderr})
	if code != 1 {
		t.Errorf("the start with git 2.30.2 exited %d, want 1", code)
	}
	if text := stderr.String(); !strings.Contains(text, "git 2.30.2 is too old; parleyd needs git 2.38 or later") || strings.Contains(text, "listening") {
		t.Errorf("the start with git 2.30.2 wrote %q, want the git version refused and no listening line", text)
	}

	served := hubtest.Serve(t, hub.Options{WorkDir: dir, Env: config.MapEnv(map[string]string{"PATH": os.Getenv("PATH")})})
	log := served.Log.String()
	if checked, listening := strings.Index(log, `"msg":"git version checked"`), strings.Index(log, `"msg":"listening"`); checked < 0 || checked > listening {
		t.Errorf("the git version was not checked before the hub listened; stderr = %q", log)
	}
	res, err := http.Get(served.URL("/readyz"))
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200", res.StatusCode)
	}
	served.Stop()
	if code := served.ExitCode(t, 10*time.Second); code != 0 {
		t.Errorf("the hub exited %d after its stop, want 0", code)
	}
}
