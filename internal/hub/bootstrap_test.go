package hub_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
)

var adminTokenFormat = regexp.MustCompile(`^parley_admin_[0-9a-f]{64}$`)

// result is what one run of the hub returned and wrote.
type result struct {
	code           int
	stdout, stderr string
}

// runToEnd runs the hub with opts until Run returns, and fails the test when
// it does not return within 5 s, as a hub that serves would not.
func runToEnd(t *testing.T, opts hub.Options) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, stderr := newSyncBuffer(), newSyncBuffer()
	opts.Stdout, opts.Stderr = stdout, stderr
	code := hub.Run(ctx, opts)
	if ctx.Err() != nil {
		t.Fatalf("Run did not return within 5 s; stderr = %q", stderr.String())
	}
	return result{code, stdout.String(), stderr.String()}
}

// countHooks adds a start hook that counts its calls in n.
func countHooks(n *atomic.Int32) func(*hub.Composition) {
	return func(c *hub.Composition) {
		c.AddStartHook("count", func(context.Context, *hub.Runtime) error {
			n.Add(1)
			return nil
		})
	}
}

// storedAdmin reads the admin settings of the hub whose working directory is
// dir and whose data directory is the default, dir/data.
func storedAdmin(t *testing.T, dir string) identity.Admin {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{
		DatabasePath:  filepath.Join(dir, "data", "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "data", "workspaces"),
	}, []store.Step{identity.AdminSettingsStep})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer st.Close()
	s, err := identity.AdminSettings(ctx, st)
	if err != nil {
		t.Fatalf("read the admin settings: %v", err)
	}
	return s
}

// readToken reads the token file at path and checks its format and mode.
func readToken(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the token file: %v", err)
	}
	if !adminTokenFormat.Match(b) {
		t.Errorf("token file holds %q, want a match of %s and no trailing newline", b, adminTokenFormat)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the token file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("token file mode = %o, want 600", mode)
	}
	return string(b)
}

func hashOf(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// bootstrapHub runs a first bootstrap with --admin-email a@x in dir and
// returns its token.
func bootstrapHub(t *testing.T, dir string) string {
	t.Helper()
	if res := runToEnd(t, hub.Options{WorkDir: dir, Args: []string{"--admin-email", "a@x"}}); res.code != 0 {
		t.Fatalf("first bootstrap returned %d; stderr = %q", res.code, res.stderr)
	}
	return readToken(t, filepath.Join(dir, "admin_token"))
}

// checkWritten checks a run that wrote the token file at path: status 0, one
// warn line naming the path, no listening line and no start hook.
func checkWritten(t *testing.T, res result, path string, hooks int32) {
	t.Helper()
	if res.code != 0 {
		t.Errorf("Run returned %d, want 0; stderr = %q", res.code, res.stderr)
	}
	var warned bool
	for _, line := range nonEmptyLines(res.stderr) {
		if decodeLine(t, line)["level"] == "WARN" && strings.Contains(line, path) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("stderr holds no WARN line naming %s: %q", path, res.stderr)
	}
	if strings.Contains(res.stderr, "listening") {
		t.Errorf("stderr holds a listening line: %q", res.stderr)
	}
	if hooks != 0 {
		t.Errorf("the start hook was called %d times, want 0", hooks)
	}
}

func TestRun_StartsWithoutAnAdminTokenWhenNoneIsAskedFor(t *testing.T) {
	t.Parallel()
	dir := workDir(t, localConfig)
	r := startHub(t, hub.Options{WorkDir: dir})
	if _, err := os.Stat(filepath.Join(dir, "admin_token")); !os.IsNotExist(err) {
		t.Errorf("stat admin_token = %v, want it not to exist", err)
	}
	if code := r.stop(t); code != 0 {
		t.Errorf("Run returned %d after a stop, want 0", code)
	}
	if s := storedAdmin(t, dir); s != (identity.Admin{}) {
		t.Errorf("admin settings = %+v, want none", s)
	}
}

func TestRun_BootstrapsANewHubWithTheAdminEmail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		before string // the token file's content before the run; "" for none
	}{
		{"no token file before", ""},
		{"a stale token file before", "stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := workDir(t, localConfig)
			path := filepath.Join(dir, "admin_token")
			if tc.before != "" {
				writeFile(t, path, tc.before)
			}
			var hooks atomic.Int32
			res := runToEnd(t, hub.Options{WorkDir: dir, Args: []string{"--admin-email", "a@x"}, Compose: countHooks(&hooks)})

			checkWritten(t, res, path, hooks.Load())
			tok := readToken(t, path)
			if s := storedAdmin(t, dir); s.Email != "a@x" || s.TokenHash != hashOf(tok) {
				t.Errorf("admin settings = %+v, want e-mail a@x and hash %s", s, hashOf(tok))
			}
		})
	}
}

func TestRun_ResetsTheAdminTokenInAnyState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// arrange prepares dir and returns the token of a first bootstrap,
		// "" when there was none.
		arrange   func(t *testing.T, dir string) string
		extraArgs []string
		wantEmail string // "" means the first bootstrap's, a@x, when there was one
	}{
		{"not bootstrapped and no file", func(*testing.T, string) string { return "" }, nil, ""},
		{"bootstrapped with the file deleted", func(t *testing.T, dir string) string {
			tok := bootstrapHub(t, dir)
			if err := os.Remove(filepath.Join(dir, "admin_token")); err != nil {
				t.Fatal(err)
			}
			return tok
		}, nil, "a@x"},
		{"bootstrapped with the file still there", bootstrapHub, nil, "a@x"},
		{"not bootstrapped with an admin e-mail", func(*testing.T, string) string { return "" }, []string{"--admin-email", "b@x"}, "b@x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := workDir(t, localConfig)
			path := filepath.Join(dir, "admin_token")
			first := tc.arrange(t, dir)
			old := storedAdmin(t, dir).TokenHash

			var hooks atomic.Int32
			args := append([]string{"--reset-admin-token"}, tc.extraArgs...)
			res := runToEnd(t, hub.Options{WorkDir: dir, Args: args, Compose: countHooks(&hooks)})

			checkWritten(t, res, path, hooks.Load())
			tok := readToken(t, path)
			if tok == first {
				t.Errorf("the reset wrote the token of the first bootstrap again")
			}
			s := storedAdmin(t, dir)
			if s.TokenHash != hashOf(tok) || s.TokenHash == old {
				t.Errorf("stored hash = %s, want %s (and not the earlier %s)", s.TokenHash, hashOf(tok), old)
			}
			if s.Email != tc.wantEmail {
				t.Errorf("stored e-mail = %q, want %q", s.Email, tc.wantEmail)
			}
		})
	}
}

func TestRun_RefusesToStartWhileTheTokenFileExists(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"--admin-email", "c@x"}} {
		t.Run(strings.Join(append([]string{"args"}, args...), " "), func(t *testing.T) {
			t.Parallel()
			dir := workDir(t, localConfig)
			path := filepath.Join(dir, "admin_token")
			tok := bootstrapHub(t, dir)
			before := storedAdmin(t, dir)

			var hooks atomic.Int32
			res := runToEnd(t, hub.Options{WorkDir: dir, Args: args, Compose: countHooks(&hooks)})

			if res.code != 1 {
				t.Errorf("Run returned %d, want 1", res.code)
			}
			if lines := nonEmptyLines(res.stderr); len(lines) != 1 || !strings.Contains(lines[0], path) {
				t.Errorf("stderr = %q, want one line naming %s", res.stderr, path)
			}
			if got, _ := os.ReadFile(path); string(got) != tok {
				t.Errorf("token file holds %q, want the first bootstrap's token", got)
			}
			if s := storedAdmin(t, dir); s != before {
				t.Errorf("admin settings = %+v, want them unchanged, %+v", s, before)
			}
			if n := hooks.Load(); n != 0 {
				t.Errorf("the start hook was called %d times, want 0", n)
			}
		})
	}
}

func TestRun_StartsABootstrappedHubOnceTheTokenFileIsDeleted(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"--admin-email", "c@x"}} {
		t.Run(strings.Join(append([]string{"args"}, args...), " "), func(t *testing.T) {
			t.Parallel()
			dir := workDir(t, localConfig)
			path := filepath.Join(dir, "admin_token")
			tok := bootstrapHub(t, dir)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}

			r := startHub(t, hub.Options{WorkDir: dir, Args: args})
			if code := r.stop(t); code != 0 {
				t.Errorf("Run returned %d after a stop, want 0", code)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("stat admin_token = %v, want it not to exist", err)
			}
			if s := storedAdmin(t, dir); s.Email != "a@x" || s.TokenHash != hashOf(tok) {
				t.Errorf("admin settings = %+v, want those of the bootstrap", s)
			}
		})
	}
}

func TestRun_KeepsTheAdminTokenOutOfEverythingButTheTokenFile(t *testing.T) {
	t.Parallel()
	dir := workDir(t, localConfig)
	path := filepath.Join(dir, "admin_token")
	var output strings.Builder
	var tokens []string
	for _, run := range []struct {
		config string
		args   []string
	}{
		{localConfig, []string{"--admin-email", "a@x"}},
		{localConfig + "[logging]\nlevel = \"trace\"\n", []string{"--reset-admin-token"}},
	} {
		writeFile(t, filepath.Join(dir, "config.toml"), run.config)
		res := runToEnd(t, hub.Options{WorkDir: dir, Args: run.args})
		if res.code != 0 {
			t.Fatalf("Run %v returned %d; stderr = %q", run.args, res.code, res.stderr)
		}
		output.WriteString(res.stdout + res.stderr)
		tokens = append(tokens, readToken(t, path))
	}

	if tokens[0] == tokens[1] {
		t.Errorf("both runs produced the token %q", tokens[0])
	}
	for _, tok := range tokens {
		hexPart := strings.TrimPrefix(tok, "parley_admin_")
		if strings.Contains(output.String(), hexPart) {
			t.Errorf("stdout or stderr holds a token: %q", output.String())
		}
		err := filepath.WalkDir(filepath.Join(dir, "data"), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), hexPart) {
				t.Errorf("%s holds a token", p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk the data directory: %v", err)
		}
	}
}

func TestRun_StoresNothingWhenTheTokenFileCannotBeWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		bootstrap bool
		args      []string
	}{
		{"admin e-mail on a new hub", false, []string{"--admin-email", "a@x"}},
		{"reset on a bootstrapped hub", true, []string{"--reset-admin-token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := workDir(t, localConfig)
			path := filepath.Join(dir, "admin_token")
			if tc.bootstrap {
				bootstrapHub(t, dir)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			// A non-empty directory cannot be replaced by a file.
			writeFile(t, filepath.Join(path, "keep"), "x")
			before := storedAdmin(t, dir)
			entries := dirNames(t, dir)

			res := runToEnd(t, hub.Options{WorkDir: dir, Args: tc.args})

			if res.code != 1 {
				t.Errorf("Run returned %d, want 1", res.code)
			}
			if lines := nonEmptyLines(res.stderr); len(lines) != 1 || !strings.Contains(lines[0], "admin_token") {
				t.Errorf("stderr = %q, want one line naming admin_token", res.stderr)
			}
			if s := storedAdmin(t, dir); s != before {
				t.Errorf("admin settings = %+v, want them unchanged, %+v", s, before)
			}
			if got := dirNames(t, dir); !slices.Equal(got, entries) {
				t.Errorf("config dir holds %v after the run, want %v", got, entries)
			}
		})
	}
}

// failingCommit adds a schema step after which a change of the stored admin
// token hash breaks a deferred foreign key, so that the transaction of a
// reset fails at its commit, after the token file was written.
func failingCommit(c *hub.Composition) {
	c.AddSchemaStep(execStep("fail_reset_commit", store.Main, `
CREATE TABLE parent (id INTEGER PRIMARY KEY);
CREATE TABLE child (parent_id INTEGER REFERENCES parent (id) DEFERRABLE INITIALLY DEFERRED);
CREATE TRIGGER orphan AFTER UPDATE OF token_hash ON admin_settings BEGIN
	INSERT INTO child (parent_id) VALUES (1);
END`))
}

func TestRun_KeepsTheOldTokenFileWhenTheResetCannotCommit(t *testing.T) {
	t.Parallel()
	dir := workDir(t, localConfig)
	path := filepath.Join(dir, "admin_token")
	if res := runToEnd(t, hub.Options{WorkDir: dir, Args: []string{"--admin-email", "a@x"}, Compose: failingCommit}); res.code != 0 {
		t.Fatalf("first bootstrap returned %d; stderr = %q", res.code, res.stderr)
	}
	old := readToken(t, path)
	before := storedAdmin(t, dir)
	entries := dirNames(t, dir)

	res := runToEnd(t, hub.Options{WorkDir: dir, Args: []string{"--reset-admin-token"}, Compose: failingCommit})

	if res.code != 1 {
		t.Errorf("Run returned %d, want 1; stderr = %q", res.code, res.stderr)
	}
	if lines := nonEmptyLines(res.stderr); len(lines) != 1 || !strings.Contains(lines[0], "commit") {
		t.Errorf("stderr = %q, want one line naming the failed commit", res.stderr)
	}
	if got := readToken(t, path); got != old {
		t.Errorf("token file holds %q after the failed reset, want the old token", got)
	}
	if s := storedAdmin(t, dir); s != before || s.TokenHash != hashOf(old) {
		t.Errorf("admin settings = %+v, want them unchanged, %+v, with the old token's hash", s, before)
	}
	if got := dirNames(t, dir); !slices.Equal(got, entries) {
		t.Errorf("config dir holds %v after the run, want %v", got, entries)
	}
}

// dirNames lists the names in dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
