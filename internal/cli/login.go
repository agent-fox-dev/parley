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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// loginRequestTimeout bounds the login's requests to the hub other than the
// callback: GET /api/v1/user and GET /api/v1/auth/providers.
const loginRequestTimeout = 30 * time.Second

// callbackRequestTimeout bounds POST /api/v1/auth/callback. Before it answers,
// the hub may make up to three calls to the identity provider (token, user,
// e-mails) of up to 15 seconds each, so the 30 seconds of the other requests
// could give up on a sign-in the hub then completes.
const callbackRequestTimeout = 60 * time.Second

// maxLoginBody bounds how much of the hub's answer, and of stdin, is read.
const maxLoginBody = 1 << 20

// hubErrorExitStatus is what parley login returns when the hub refused it.
const hubErrorExitStatus = 1

// loginError is how parley login fails: run writes envelope to stderr as it
// is and exits with status. A hub's refusal carries the hub's own envelope
// bytes; a failure on the client's side carries one built by envelopeError.
type loginError struct {
	status   int
	envelope []byte
}

func (e *loginError) Error() string { return string(bytes.TrimSpace(e.envelope)) }

// errorEnvelope is the shape of the hub's error answers, which the client
// reuses for its own failures with code 0.
type errorEnvelope struct {
	Error struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		ErrorType string `json:"error_type"`
	} `json:"error"`
}

// envelopeError is a failure on the client's side, exit status 2, with an
// envelope of code 0 and errorType.
func envelopeError(errorType, format string, args ...any) *loginError {
	var env errorEnvelope
	env.Error.Message = fmt.Sprintf(format, args...)
	env.Error.ErrorType = errorType
	body, _ := json.Marshal(env)
	return &loginError{status: usageExitStatus, envelope: append(body, '\n')}
}

// clientError is a failure the client cannot blame on its arguments: no hub
// URL, an unreadable config file or answer, an unreachable hub.
func clientError(format string, args ...any) *loginError {
	return envelopeError("client_error", format, args...)
}

// usageError is a failure caused by the arguments.
func usageError(format string, args ...any) *loginError {
	return envelopeError("usage_error", format, args...)
}

func newLoginCommand(endpointURL *string, opts options) *cobra.Command {
	var provider, expires, apiKey string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to the hub and save the hub URL and the credential",
		// A bad flag or argument fails like a bad --expires: a usage_error
		// envelope, not the one error line of the other commands.
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.NoArgs(cmd, args); err != nil {
				return usageError("%v", err)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Only the flag selects the key flow; --provider and --expires
			// belong to the browser flow.
			browser := !cmd.Flags().Changed("api-key")
			var days int
			if browser {
				var err error
				if days, err = loginExpires(expires); err != nil {
					return err
				}
			}
			// The file is read before anything else is sent, so that a file
			// the save would fail on cannot cost a credential the hub just
			// accepted or issued.
			path, err := configPath()
			if err != nil {
				return clientError("%v", err)
			}
			cfg, err := readConfig(path)
			if err != nil {
				return clientError("%v", err)
			}
			hubURL := loginHubURL(*endpointURL, cfg)
			if hubURL == "" {
				return clientError("no hub URL: pass --endpoint-url, set %s or set %s in %s", endpointURLEnv, configEndpointURL, path)
			}

			var cred string
			var user loginUser
			if browser {
				cred, user, err = browserLogin(cmd.Context(), hubURL, provider, days, opts, cmd.ErrOrStderr())
			} else if cred, err = loginCredential(apiKey, opts.stdin); err == nil {
				user, err = hubUser(cmd.Context(), hubURL, cred)
			}
			if err != nil {
				return err
			}

			cfg[configEndpointURL] = hubURL
			cfg[configAPIKey] = cred
			cfg[configUserID] = user.id
			if err := saveConfig(path, cfg); err != nil {
				if browser {
					return clientError("the hub issued an API key, but it was not saved to %s: %v", path, err)
				}
				return clientError("the hub accepted the credential, but it was not saved to %s: %v", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", user.indented)
			fmt.Fprintf(cmd.ErrOrStderr(), "Logged in as %s\n", user.username)
			return nil
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "github", "identity provider of the browser login")
	cmd.Flags().StringVar(&expires, "expires", "90", "days the browser login's API key lives: 0, 30, 60 or 90 (0: no expiry)")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "store this credential instead of signing in through the browser (- reads it from stdin)")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError("%v", err) })
	return cmd
}

// loginHubURL is the first non-empty of the flag --endpoint-url, the variable
// PARLEY_ENDPOINT_URL and the file's endpoint_url, without trailing slashes.
func loginHubURL(flag string, cfg map[string]any) string {
	hubURL := flag
	if hubURL == "" {
		hubURL = os.Getenv(endpointURLEnv)
	}
	if hubURL == "" {
		hubURL, _ = cfg[configEndpointURL].(string)
	}
	return strings.TrimRight(hubURL, "/")
}

// loginCredential is the value of --api-key, or what stdin holds when the
// value is -, without its surrounding white space.
func loginCredential(value string, stdin io.Reader) (string, error) {
	if value == "-" {
		data, err := io.ReadAll(io.LimitReader(stdin, maxLoginBody))
		if err != nil {
			return "", clientError("cannot read the credential from stdin: %v", err)
		}
		value = string(data)
	}
	cred := strings.TrimSpace(value)
	if cred == "" {
		return "", usageError("--api-key needs a credential, given as its value or on stdin with -")
	}
	return cred, nil
}

// loginUser is what the login takes from the hub's User.
type loginUser struct {
	id, username string
	// indented is the User as the hub answered it, indented by 2 spaces.
	indented []byte
}

// hubUser asks the hub at hubURL who cred belongs to, with GET /api/v1/user.
func hubUser(ctx context.Context, hubURL, cred string) (loginUser, error) {
	body, err := hubCall(ctx, http.MethodGet, hubURL+"/api/v1/user", cred, nil, loginRequestTimeout)
	if err != nil {
		return loginUser{}, err
	}
	return parseUser(body)
}

// hubCall sends a request to the hub, with cred as the bearer credential
// unless it is empty and with body as JSON unless it is nil, waits up to
// timeout, and returns the body of a 200 answer. A hub's error envelope comes
// back as a loginError with the envelope's bytes; any other failure is a
// client_error.
func hubCall(ctx context.Context, method, target, cred string, body []byte, timeout time.Duration) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return nil, clientError("invalid hub URL %q: %v", target, err)
	}
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, clientError("cannot reach the hub: %v", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxLoginBody))
	if err != nil {
		return nil, clientError("cannot read the hub's answer: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		if isErrorEnvelope(answer) {
			return nil, &loginError{status: hubErrorExitStatus, envelope: answer}
		}
		return nil, clientError("the hub answered %s without an error envelope", resp.Status)
	}
	return answer, nil
}

// parseUser reads the hub's User from raw.
func parseUser(raw []byte) (loginUser, error) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return loginUser{}, clientError("the hub's answer is not a JSON object")
	}
	id, _ := fields["id"].(string)
	if id == "" {
		return loginUser{}, clientError("the hub's answer has no user id")
	}
	username, _ := fields["username"].(string)
	var indented bytes.Buffer
	if err := json.Indent(&indented, bytes.TrimSpace(raw), "", "  "); err != nil {
		return loginUser{}, clientError("the hub's answer is not a JSON object")
	}
	return loginUser{id: id, username: username, indented: indented.Bytes()}, nil
}

// isErrorEnvelope tells whether body is the hub's error envelope, a JSON
// object whose error member is an object.
func isErrorEnvelope(body []byte) bool {
	var doc struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	return json.Unmarshal(body, &doc) == nil && doc.Error != nil
}

// loginExpires reads --expires: the days the issued API key lives, one of
// the lifetimes the hub accepts.
func loginExpires(value string) (int, error) {
	switch value {
	case "0", "30", "60", "90":
		days, _ := strconv.Atoi(value)
		return days, nil
	}
	return 0, usageError("--expires must be 0, 30, 60 or 90, got %q", value)
}

// browserLogin signs in through the browser: it sends the browser to the
// identity provider named provider, waits for the provider to send it back
// to a listener on the loopback interface with a code, and exchanges the
// code at the hub for a new API key of days days. It returns the key and
// the user.
func browserLogin(ctx context.Context, hubURL, provider string, days int, opts options, stderr io.Writer) (string, loginUser, error) {
	authorize, err := hubProvider(ctx, hubURL, provider)
	if err != nil {
		return "", loginUser{}, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", loginUser{}, clientError("cannot listen for the browser's redirect: %v", err)
	}
	// Closed on every way out, so the port takes no connection once the
	// login is over.
	defer ln.Close()
	redirectURI := "http://" + ln.Addr().String() + "/callback"
	state, err := newState()
	if err != nil {
		return "", loginUser{}, clientError("cannot draw the sign-in's state: %v", err)
	}
	target, err := url.Parse(authorize)
	if err != nil || !target.IsAbs() {
		return "", loginUser{}, clientError("the hub's authorize_url for %s is not an absolute URL", provider)
	}
	q := target.Query()
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	target.RawQuery = q.Encode()

	code, err := awaitRedirect(ctx, ln, state, opts.loginWait, func() {
		if err := opts.openBrowser(target.String()); err != nil {
			fmt.Fprintf(stderr, "Cannot open a browser (%v). Open this URL to sign in:\n%s\n", err, target)
		}
	})
	if err != nil {
		return "", loginUser{}, err
	}

	payload, err := json.Marshal(struct {
		Provider    string `json:"provider"`
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
		Expires     int    `json:"expires"`
	}{provider, code, redirectURI, days})
	if err != nil {
		return "", loginUser{}, clientError("cannot encode the sign-in: %v", err)
	}
	body, err := hubCall(ctx, http.MethodPost, hubURL+"/api/v1/auth/callback", "", payload, callbackRequestTimeout)
	if err != nil {
		return "", loginUser{}, err
	}
	var answer struct {
		User   json.RawMessage `json:"user"`
		APIKey struct {
			Key string `json:"key"`
		} `json:"api_key"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.APIKey.Key == "" {
		return "", loginUser{}, clientError("the hub's answer to the sign-in holds no API key")
	}
	user, err := parseUser(answer.User)
	if err != nil {
		return "", loginUser{}, err
	}
	return answer.APIKey.Key, user, nil
}

// hubProvider returns the authorize_url of the identity provider name from
// the hub's GET /api/v1/auth/providers, which takes no credential.
func hubProvider(ctx context.Context, hubURL, name string) (string, error) {
	body, err := hubCall(ctx, http.MethodGet, hubURL+"/api/v1/auth/providers", "", nil, loginRequestTimeout)
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Name         string `json:"name"`
			AuthorizeURL string `json:"authorize_url"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", clientError("the hub's list of identity providers is not readable")
	}
	var names []string
	for _, item := range list.Items {
		if item.Name == name {
			return item.AuthorizeURL, nil
		}
		names = append(names, item.Name)
	}
	if len(names) == 0 {
		return "", clientError("the hub offers no identity provider, so %q cannot be used", name)
	}
	return "", clientError("the hub does not offer the identity provider %q; it offers %s", name, strings.Join(names, ", "))
}

// newState draws the sign-in's state: 32 hexadecimal characters.
func newState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// The pages the browser shows when it comes back to the listener.
const (
	signedInPage = "<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><title>Parley</title></head>" +
		"<body><p>You are signed in to Parley. You can close this window and return to the terminal.</p></body></html>\n"
	failedPage = "<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><title>Parley</title></head>" +
		"<body><p>The sign-in to Parley failed. The terminal says why.</p></body></html>\n"
)

// redirect is what came back to the listener: the code, or why the sign-in
// failed.
type redirect struct {
	code string
	err  error
}

// awaitRedirect serves GET /callback on ln once, calls open, and waits up to
// wait for the browser to come back with a code and state. It answers the
// browser with a page saying how the sign-in went, and stops serving before
// it returns.
func awaitRedirect(ctx context.Context, ln net.Listener, state string, wait time.Duration, open func()) (string, error) {
	got := make(chan redirect, 1)
	var once sync.Once
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			first := false
			once.Do(func() { first = true })
			if !first {
				http.Error(w, "this sign-in is over", http.StatusGone)
				return
			}
			res := checkRedirect(r.URL.Query(), state)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			page := signedInPage
			if res.err != nil {
				page = failedPage
				w.WriteHeader(http.StatusBadRequest)
			}
			w.Write([]byte(page))
			got <- res
		}),
	}
	// Serve's error is ErrServerClosed once the deferred Shutdown stops it.
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		// Shutdown lets the page under way reach the browser.
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			_ = srv.Close()
		}
	}()

	open()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case res := <-got:
		return res.code, res.err
	case <-timer.C:
		return "", clientError("the browser did not come back from the sign-in within %v", wait)
	case <-ctx.Done():
		return "", clientError("the login was stopped before the browser came back: %v", ctx.Err())
	}
}

// checkRedirect checks the query the browser came back with: it must carry
// the state this login sent, compared in constant time, and a code, and no
// error.
func checkRedirect(q url.Values, state string) redirect {
	switch {
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1:
		return redirect{err: clientError("the sign-in failed: the browser came back without the state this login sent")}
	case q.Get("error") != "":
		return redirect{err: clientError("the sign-in failed: the identity provider answered %q", q.Get("error"))}
	case q.Get("code") == "":
		return redirect{err: clientError("the sign-in failed: the browser came back without a code")}
	}
	return redirect{code: q.Get("code")}
}
