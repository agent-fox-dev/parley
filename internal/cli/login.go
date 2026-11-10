package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// DefaultLoginWait bounds the wait for the browser's redirect.
const DefaultLoginWait = 120 * time.Second

// loginTimeout bounds each request of parley login.
const loginTimeout = 30 * time.Second

// exitError ends a command with an exit status and an error envelope on
// stderr. Package 09 replaces it with the CLI's shared errors.
type exitError struct {
	code     int
	envelope []byte
}

func (e *exitError) Error() string { return string(e.envelope) }

func envelopeError(code int, errorType, message string) *exitError {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 0, "message": message, "error_type": errorType}})
	return &exitError{code: code, envelope: append(b, '\n')}
}

// clientError is a failure on the client's side: exit status 2.
func clientError(format string, args ...any) *exitError {
	return envelopeError(2, "client_error", fmt.Sprintf(format, args...))
}

// usageError is a bad flag or argument: exit status 2.
func usageError(format string, args ...any) *exitError {
	return envelopeError(2, "usage_error", fmt.Sprintf(format, args...))
}

// hubError is the hub's own error, printed as received: exit status 1.
func hubError(body []byte) *exitError {
	b := bytes.TrimSpace(body)
	return &exitError{code: 1, envelope: append(append([]byte(nil), b...), '\n')}
}

// loginFlags are the flags of parley login.
type loginFlags struct {
	provider string
	expires  string
	apiKey   string
}

func newLoginCommand(flags *globalFlags, opts Options) *cobra.Command {
	var lf loginFlags
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in through the browser, or store a credential with --api-key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l := &login{flags: flags, lf: lf, opts: opts, cmd: cmd, keyFlow: cmd.Flags().Changed("api-key")}
			return l.run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&lf.provider, "provider", "github", "the identity provider to sign in with")
	cmd.Flags().StringVar(&lf.expires, "expires", "90", "the new key's lifetime in days: 0, 30, 60 or 90")
	cmd.Flags().StringVar(&lf.apiKey, "api-key", "", "store this credential instead of signing in; - reads it from stdin")
	return cmd
}

// login is one run of parley login.
type login struct {
	flags   *globalFlags
	lf      loginFlags
	opts    Options
	cmd     *cobra.Command
	keyFlow bool

	path string
	file map[string]any
	hub  string
}

// run reads the configuration file before any request, resolves the hub
// URL, and runs the key flow or the browser flow.
func (l *login) run(ctx context.Context) error {
	path, err := configPath()
	if err != nil {
		return clientError("%v", err)
	}
	file, err := readConfig(path)
	if err != nil {
		return clientError("%v", err)
	}
	l.path, l.file = path, file
	l.hub = strings.TrimRight(l.hubURL(), "/")
	if l.hub == "" {
		return clientError("no hub URL: give --endpoint-url, set PARLEY_ENDPOINT_URL or set endpoint_url in %s", path)
	}
	if l.keyFlow {
		return l.withKey(ctx)
	}
	return l.withBrowser(ctx)
}

// hubURL is the first non-empty value of --endpoint-url,
// PARLEY_ENDPOINT_URL and the file key endpoint_url.
func (l *login) hubURL() string {
	if l.flags.endpointURL != "" {
		return l.flags.endpointURL
	}
	if v := os.Getenv("PARLEY_ENDPOINT_URL"); v != "" {
		return v
	}
	s, _ := l.file[keyEndpointURL].(string)
	return s
}

// withKey is the key flow: GET /user with the credential and, on 200, save
// it. No key is created or revoked.
func (l *login) withKey(ctx context.Context) error {
	cred := l.lf.apiKey
	if cred == "-" {
		stdin := l.opts.Stdin
		if stdin == nil {
			stdin = os.Stdin
		}
		b, err := io.ReadAll(stdin)
		if err != nil {
			return clientError("cannot read the credential from stdin: %v", err)
		}
		cred = string(b)
	}
	cred = strings.TrimSpace(cred)
	if cred == "" {
		return usageError("--api-key needs a credential")
	}
	user, err := l.request(ctx, http.MethodGet, "/api/v1/user", cred, nil)
	if err != nil {
		return err
	}
	return l.finish(cred, user)
}

// request sends one request to the hub and returns the JSON object it
// answered 200; a hub error is printed as received, anything unreadable is
// a client error.
func (l *login) request(ctx context.Context, method, path, cred string, body any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, clientError("%v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.hub+path, rd)
	if err != nil {
		return nil, clientError("the hub URL %s is not usable: %v", l.hub, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, clientError("cannot reach the hub at %s: %v", l.hub, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, clientError("cannot read the hub's answer: %v", err)
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return nil, clientError("the hub's answer to %s %s is not a JSON object", method, path)
	}
	if resp.StatusCode != http.StatusOK {
		if _, ok := obj["error"]; ok {
			return nil, hubError(data)
		}
		return nil, clientError("the hub answered %s %s with status %d", method, path, resp.StatusCode)
	}
	return data, nil
}

// finish saves the hub URL, the credential and the user's id, prints the
// user on stdout and greets on stderr.
func (l *login) finish(cred string, user []byte) error {
	var u struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(user, &u); err != nil || u.ID == "" {
		return clientError("the hub's answer carries no user")
	}
	l.file[keyEndpointURL] = l.hub
	l.file[keyAPIKey] = cred
	l.file[keyUserID] = u.ID
	if err := saveConfig(l.path, l.file); err != nil {
		return clientError("the hub accepted the credential but it was not saved to %s: %v", l.path, err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimSpace(user), "", "  "); err != nil {
		return clientError("%v", err)
	}
	fmt.Fprintf(l.cmd.OutOrStdout(), "%s\n", out.Bytes())
	fmt.Fprintf(l.cmd.ErrOrStderr(), "Logged in as %s\n", u.Username)
	return nil
}

// validExpires are the key lifetimes a login may ask for, in days.
var validExpires = map[string]int{"0": 0, "30": 30, "60": 60, "90": 90}

// withBrowser is the browser flow: list the providers, open the provider's
// authorize URL with a loopback redirect and a random state, wait for the
// redirect, and exchange its code at the hub.
func (l *login) withBrowser(ctx context.Context) error {
	expires, ok := validExpires[l.lf.expires]
	if !ok {
		return usageError("--expires must be 0, 30, 60 or 90")
	}
	authorizeURL, err := l.pickProvider(ctx)
	if err != nil {
		return err
	}
	code, redirectURI, err := l.awaitRedirect(ctx, authorizeURL)
	if err != nil {
		return err
	}
	answer, err := l.request(ctx, http.MethodPost, "/api/v1/auth/callback", "", map[string]any{
		"provider": l.lf.provider, "code": code, "redirect_uri": redirectURI, "expires": expires,
	})
	if err != nil {
		return err
	}
	var out struct {
		User   json.RawMessage `json:"user"`
		APIKey struct {
			Key string `json:"key"`
		} `json:"api_key"`
	}
	if err := json.Unmarshal(answer, &out); err != nil || out.APIKey.Key == "" {
		return clientError("the hub's login answer carries no key")
	}
	return l.finish(out.APIKey.Key, out.User)
}

// pickProvider fetches the providers the hub lists, without a credential,
// and returns the authorize URL of the one --provider names.
func (l *login) pickProvider(ctx context.Context) (string, error) {
	data, err := l.request(ctx, http.MethodGet, "/api/v1/auth/providers", "", nil)
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Name         string `json:"name"`
			AuthorizeURL string `json:"authorize_url"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return "", clientError("the hub's provider list is not readable")
	}
	var names []string
	for _, it := range list.Items {
		if it.Name == l.lf.provider {
			return it.AuthorizeURL, nil
		}
		names = append(names, it.Name)
	}
	if len(names) == 0 {
		return "", clientError("the hub offers no identity provider")
	}
	return "", clientError("the hub does not offer the provider %s; it offers %s", l.lf.provider, strings.Join(names, ", "))
}

// redirectResult is what the one redirect brought.
type redirectResult struct {
	code string
	err  error
}

// awaitRedirect listens on 127.0.0.1, opens the authorize URL with
// redirect_uri and state added, and waits for the redirect at most the
// login wait. The listener serves GET /callback once and is closed on every
// way out.
func (l *login) awaitRedirect(ctx context.Context, authorizeURL string) (code, redirectURI string, err error) {
	state, err := randomState()
	if err != nil {
		return "", "", clientError("%v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", clientError("cannot listen for the browser's redirect: %v", err)
	}
	redirectURI = "http://" + ln.Addr().String() + "/callback"
	target, err := withQuery(authorizeURL, map[string]string{"redirect_uri": redirectURI, "state": state})
	if err != nil {
		_ = ln.Close()
		return "", "", clientError("the provider's authorize URL is not usable: %v", err)
	}

	results := make(chan redirectResult, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		res := checkRedirect(r.URL.Query(), state)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := successPage
		if res.err != nil {
			w.WriteHeader(http.StatusBadRequest)
			page = failurePage
		}
		// A page the browser does not receive changes nothing: the
		// result below decides the login.
		_, _ = io.WriteString(w, page)
		once.Do(func() { results <- res })
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if srv.Shutdown(shutdownCtx) != nil {
			_ = srv.Close()
		}
	}()

	opener := l.opts.Opener
	if opener == nil {
		opener = openBrowser
	}
	if err := opener(target); err != nil {
		fmt.Fprintf(l.cmd.ErrOrStderr(), "Open this URL in a browser to sign in:\n%s\n", target)
	}
	wait := l.opts.LoginWait
	if wait <= 0 {
		wait = DefaultLoginWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case res := <-results:
		if res.err != nil {
			return "", "", res.err
		}
		return res.code, redirectURI, nil
	case <-timer.C:
		return "", "", clientError("no sign-in arrived from the browser within %s", wait)
	case <-ctx.Done():
		return "", "", clientError("the login was cancelled")
	}
}

// checkRedirect accepts a redirect that carries a code and the state that
// was sent, compared in constant time.
func checkRedirect(q url.Values, state string) redirectResult {
	switch {
	case q.Get("error") != "":
		return redirectResult{err: clientError("the provider refused the sign-in")}
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1:
		return redirectResult{err: clientError("the sign-in's state does not match")}
	case q.Get("code") == "":
		return redirectResult{err: clientError("the sign-in brought no code")}
	}
	return redirectResult{code: q.Get("code")}
}

// randomState is 32 hexadecimal characters from crypto/rand.
func randomState() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// withQuery adds query parameters to a URL, keeping the ones it has.
func withQuery(raw string, add map[string]string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, v := range add {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
