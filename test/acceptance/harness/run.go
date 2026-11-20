package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Result is how a process the harness ran ended.
type Result struct {
	// Code is the exit status; -1 when the process could not start or was
	// stopped by a signal, a cancelled context included.
	Code           int
	Stdout, Stderr string
}

// Must fails the test unless the process exited 0.
func (r Result) Must(tb testing.TB) Result {
	tb.Helper()
	if r.Code != 0 {
		tb.Fatalf("exit status %d\nstdout:\n%s\nstderr:\n%s", r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// Lines are the lines of the standard output, without their ends; none
// when it is empty.
func (r Result) Lines() []string {
	if r.Stdout == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(r.Stdout, "\n"), "\n")
}

// JSON is the standard output decoded as a JSON object, nil when it is not
// one.
func (r Result) JSON() map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(r.Stdout), &m) != nil {
		return nil
	}
	return m
}

// run runs name with args in dir with exactly the environment env.
func run(ctx context.Context, dir string, env []string, name string, args ...string) Result {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	r := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		r.Code = exit.ExitCode()
	case err != nil:
		r.Code, r.Stderr = -1, r.Stderr+err.Error()
	}
	return r
}

// CLI runs the built parley against the hub with credential as its API key
// ("" for none), in a home and a configuration directory of its own. A
// command that waits polls every second rather than every five.
func (h *Hub) CLI(credential string, args ...string) Result {
	return h.CLIContext(context.Background(), credential, args...)
}

// CLIContext is CLI for a command that does not end by itself, such as
// parley events tail: the process is killed when ctx is done, and the
// Result holds what it printed until then.
func (h *Hub) CLIContext(ctx context.Context, credential string, args ...string) Result {
	if slices.Contains(args, "--wait") && !slices.Contains(args, "--poll-interval") {
		at := len(args)
		if i := slices.Index(args, "--"); i >= 0 {
			at = i
		}
		args = slices.Insert(slices.Clone(args), at, "--poll-interval", "1s")
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + h.cliHome, "PARLEY_CONFIG_DIR=" + h.cliConfig,
		"PARLEY_ENDPOINT_URL=" + h.URL(""),
	}
	if credential != "" {
		env = append(env, "PARLEY_API_KEY="+credential)
	}
	return run(ctx, h.cliHome, env, built.parley, args...)
}

// Git runs stock git with args in dir, away from the user's configuration,
// trusting the test origins, with a fixed author. With a credential, git
// asks the built parley's credential-helper for the hub's URL, which
// answers with it; without one, a credential can still come in the remote
// URL (AuthURL).
func (h *Hub) Git(dir, credential string, args ...string) Result {
	env := h.gitEnv()
	if credential != "" {
		env = append(env, "PARLEY_ENDPOINT_URL="+h.URL(""), "PARLEY_API_KEY="+credential)
		args = append([]string{"-c", "credential." + h.URL("") + ".helper=!" + built.parley + " credential-helper"}, args...)
	}
	return run(context.Background(), dir, env, "git", args...)
}

// gitEnv is the whole environment of the scenario's git.
func (h *Hub) gitEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + h.gitHome, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_SSL_CAINFO=" + built.caFile,
		"GIT_AUTHOR_NAME=Acceptance", "GIT_AUTHOR_EMAIL=acceptance@example.test",
		"GIT_COMMITTER_NAME=Acceptance", "GIT_COMMITTER_EMAIL=acceptance@example.test",
	}
}

// GitEnv runs stock git as Git does without a credential, with the
// NAME=value variables env added to its environment, such as those a
// credential helper the scenario configures itself reads.
func (h *Hub) GitEnv(dir string, env []string, args ...string) Result {
	return run(context.Background(), dir, append(h.gitEnv(), env...), "git", args...)
}

// AuthURL is remote with credential as the password of its user info.
func AuthURL(remote, credential string) string {
	u, err := url.Parse(remote)
	if err != nil {
		return remote
	}
	u.User = url.UserPassword("x", credential)
	return u.String()
}

// CommitFiles writes files, by path relative to the clone dir, and commits
// every change of the clone with msg, failing the test on an error.
func (h *Hub) CommitFiles(dir, msg string, files map[string]string) {
	h.t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	h.Git(dir, "", "add", "-A").Must(h.t)
	h.Git(dir, "", "commit", "-q", "-m", msg).Must(h.t)
}

// PushChange pushes the clone dir's HEAD to branch of its remote origin,
// asking the hub to open or update a change with intent, which the hub
// requires by default, and with the push options opts (such as
// parley.depends-on=1), each sent as -o.
func (h *Hub) PushChange(dir, credential, branch, intent string, opts ...string) Result {
	args := []string{"push", "-o", "parley.change", "-o", "parley.intent=" + intent}
	for _, o := range opts {
		args = append(args, "-o", o)
	}
	return h.Git(dir, credential, append(args, "origin", "HEAD:refs/heads/"+branch)...)
}

// Response is the hub's answer to a Call.
type Response struct {
	// Status is 0 when no answer came; Err says why.
	Status int
	Header http.Header
	Raw    []byte
	// Body is Raw decoded, when it is a JSON object.
	Body map[string]any
	Err  error
}

// Want fails the test unless the answer has status.
func (r Response) Want(tb testing.TB, status int) Response {
	tb.Helper()
	if r.Err != nil {
		tb.Fatalf("no answer, want %d: %v", status, r.Err)
	}
	if r.Status != status {
		tb.Fatalf("answer %d, want %d: %s", r.Status, status, r.Raw)
	}
	return r
}

// ErrorType is the error_type of an error envelope, "" for another body.
func (r Response) ErrorType() string {
	e, _ := r.Body["error"].(map[string]any)
	s, _ := e["error_type"].(string)
	return s
}

// Items are the objects of a list's items.
func (r Response) Items() []map[string]any {
	raw, _ := r.Body["items"].([]any)
	items := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, _ := it.(map[string]any)
		items = append(items, m)
	}
	return items
}

// client answers with what the hub answered: it follows no redirect and
// retries nothing.
var client = &http.Client{
	Timeout:       2 * time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Call sends method path to the hub with credential as a bearer token (""
// for none), body as JSON (nil for none; a string or []byte is sent as it
// is), and the header given as name, value pairs, which may replace the
// Content-Type.
func (h *Hub) Call(method, path, credential string, body any, header ...string) Response {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewReader([]byte(b))
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return Response{Err: err}
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.URL(path), rd)
	if err != nil {
		return Response{Err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := client.Do(req)
	if err != nil {
		return Response{Err: err}
	}
	defer res.Body.Close()
	r := Response{Status: res.StatusCode, Header: res.Header}
	r.Raw, r.Err = io.ReadAll(res.Body)
	_ = json.Unmarshal(r.Raw, &r.Body)
	return r
}
