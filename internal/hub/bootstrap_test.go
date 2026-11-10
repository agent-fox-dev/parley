package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
)

var adminTokenForm = regexp.MustCompile(`^parley_admin_[0-9a-f]{64}$`)

// settingsOf reads the admin settings of the hub whose working directory is
// dir, with the hub stopped.
func settingsOf(t *testing.T, dir string) identity.Settings {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(dir, "data", "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "data", "workspaces"),
	}, []store.Step{identity.AdminSettingsStep})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := identity.AdminSettings(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tokenFile(dir string) string { return filepath.Join(dir, "admin_token") }

// readToken reads the token file and checks its form and mode.
func readToken(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(tokenFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !adminTokenForm.Match(b) {
		t.Fatalf("token file holds %q", b)
	}
	fi, _ := os.Stat(tokenFile(dir))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %o, want 600", fi.Mode().Perm())
	}
	return string(b)
}

func hashOf(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func warnLine(t *testing.T, stderr string) string {
	t.Helper()
	for _, l := range nonEmptyLines(stderr) {
		if strings.Contains(l, `"level":"WARN"`) {
			return l
		}
	}
	t.Fatalf("no WARN line in %s", stderr)
	return ""
}

// bootstrapped runs --admin-email a@x on a new hub in dir and returns the
// token.
func bootstrapped(t *testing.T, dir string) string {
	t.Helper()
	if code, _, stderr := runHub(t, dir, nil, []string{"--admin-email", "a@x"}); code != 0 {
		t.Fatalf("bootstrap exit %d: %s", code, stderr)
	}
	return readToken(t, dir)
}

func TestBootstrap_ANewHubStartsWithoutAnAdminToken(t *testing.T) {
	dir := hubDir(t, loopbackConfig)
	h := startHub(t, dir, nil)
	if _, err := os.Stat(tokenFile(dir)); err == nil {
		t.Error("a token file was created")
	}
	h.stop()
	if s := settingsOf(t, dir); s.Stored || s.Email != "" || s.TokenHash != "" {
		t.Errorf("settings = %+v, want none", s)
	}
}

func TestBootstrap_AdminEmailWritesTheFirstTokenAndExits(t *testing.T) {
	for _, stale := range []bool{false, true} {
		dir := hubDir(t, loopbackConfig)
		if stale {
			if err := os.WriteFile(tokenFile(dir), []byte("stale"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		code, _, stderr := runHub(t, dir, nil, []string{"--admin-email", "a@x"})
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		tok := readToken(t, dir)
		if s := settingsOf(t, dir); s.Email != "a@x" || s.TokenHash != hashOf(tok) {
			t.Errorf("settings = %+v", s)
		}
		if !strings.Contains(warnLine(t, stderr), tokenFile(dir)) {
			t.Errorf("the WARN line does not name %s", tokenFile(dir))
		}
		if strings.Contains(stderr, "listening") {
			t.Error("the hub listened")
		}
	}
}

func TestBootstrap_ResetWritesANewTokenInEveryState(t *testing.T) {
	tests := []struct {
		name       string
		bootstrap  bool
		removeFile bool
		args       []string
		wantEmail  string
	}{
		{"not bootstrapped", false, false, nil, ""},
		{"bootstrapped, file deleted", true, true, nil, "a@x"},
		{"bootstrapped, file present", true, false, nil, "a@x"},
		{"not bootstrapped, with an e-mail", false, false, []string{"--admin-email", "b@x"}, "b@x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := hubDir(t, loopbackConfig)
			old := ""
			if tt.bootstrap {
				old = hashOf(bootstrapped(t, dir))
				if tt.removeFile {
					os.Remove(tokenFile(dir))
				}
			}
			code, _, stderr := runHub(t, dir, nil, append([]string{"--reset-admin-token"}, tt.args...))
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			tok := readToken(t, dir)
			s := settingsOf(t, dir)
			if s.TokenHash != hashOf(tok) || s.TokenHash == old {
				t.Errorf("stored hash %s, want the new token's", s.TokenHash)
			}
			if s.Email != tt.wantEmail {
				t.Errorf("e-mail = %q, want %q", s.Email, tt.wantEmail)
			}
			if strings.Contains(stderr, "listening") {
				t.Error("the hub listened")
			}
		})
	}
}

func TestBootstrap_RefusesToStartWhileTheTokenFileExists(t *testing.T) {
	for _, args := range [][]string{nil, {"--admin-email", "c@x"}} {
		dir := hubDir(t, loopbackConfig)
		tok := bootstrapped(t, dir)
		before := settingsOf(t, dir)
		hookCalled := false
		code, _, stderr := runHub(t, dir, nil, args, withCompose(func(c *Composition) {
			c.AddStartHook("h", func(context.Context, *Runtime) error { hookCalled = true; return nil })
		}))
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if lines := nonEmptyLines(stderr); len(lines) != 1 || !strings.Contains(lines[0], tokenFile(dir)) {
			t.Errorf("stderr = %s, want one line naming the token file", stderr)
		}
		if b, _ := os.ReadFile(tokenFile(dir)); string(b) != tok {
			t.Error("the token file changed")
		}
		if s := settingsOf(t, dir); s != before {
			t.Errorf("settings changed: %+v", s)
		}
		if hookCalled {
			t.Error("a start hook was called")
		}
	}
}

func TestBootstrap_ABootstrappedHubWithoutTheFileStarts(t *testing.T) {
	for _, args := range [][]string{nil, {"--admin-email", "c@x"}} {
		dir := hubDir(t, loopbackConfig)
		tok := bootstrapped(t, dir)
		os.Remove(tokenFile(dir))
		h := startHub(t, dir, nil, func(o *Options) { o.Args = args })
		if _, err := os.Stat(tokenFile(dir)); err == nil {
			t.Error("a token file was created")
		}
		h.stop()
		if s := settingsOf(t, dir); s.Email != "a@x" || s.TokenHash != hashOf(tok) {
			t.Errorf("settings = %+v", s)
		}
	}
}

func TestBootstrap_TheTokenAppearsOnlyInItsFile(t *testing.T) {
	dir := hubDir(t, loopbackConfig+"[logging]\nlevel = \"trace\"\n")
	var tokens []string
	var out strings.Builder
	for _, args := range [][]string{{"--admin-email", "a@x"}, {"--reset-admin-token"}} {
		code, stdout, stderr := runHub(t, dir, nil, args)
		if code != 0 {
			t.Fatalf("%v: exit %d", args, code)
		}
		out.WriteString(stdout + stderr)
		tokens = append(tokens, readToken(t, dir))
	}
	if tokens[0] == tokens[1] {
		t.Error("two runs produced the same token")
	}
	for _, tok := range tokens {
		hexPart := strings.TrimPrefix(tok, "parley_admin_")
		if strings.Contains(out.String(), hexPart) {
			t.Error("the token reached stdout or stderr")
		}
		filepath.WalkDir(filepath.Join(dir, "data"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(hexPart)) {
					t.Errorf("%s holds the token", p)
				}
			}
			return nil
		})
	}
}

func TestBootstrap_AFileThatCannotBeWrittenStoresNothing(t *testing.T) {
	for _, reset := range []bool{false, true} {
		dir := hubDir(t, loopbackConfig)
		args := []string{"--admin-email", "a@x"}
		if reset {
			bootstrapped(t, dir)
			os.Remove(tokenFile(dir))
			args = []string{"--reset-admin-token"}
		}
		if err := os.MkdirAll(filepath.Join(tokenFile(dir), "inside"), 0o700); err != nil {
			t.Fatal(err)
		}
		before := settingsOf(t, dir)
		code, _, stderr := runHub(t, dir, nil, args)
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		if lines := nonEmptyLines(stderr); len(lines) != 1 || !strings.Contains(stderr, "admin_token") {
			t.Errorf("stderr = %s", stderr)
		}
		if s := settingsOf(t, dir); s != before {
			t.Errorf("settings changed to %+v from %+v", s, before)
		}
	}
}
