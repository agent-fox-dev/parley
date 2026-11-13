package cli

import (
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// callbackRequestTimeout bounds POST /api/v1/auth/callback. Before it answers,
// the hub may make up to three calls to the identity provider (token, user,
// e-mails) of up to 15 seconds each, so the 30 seconds of the other requests
// could give up on a sign-in the hub then completes.
const callbackRequestTimeout = 60 * time.Second

// maxStdinCredential bounds how much of stdin is read for the credential.
const maxStdinCredential = 1 << 20

func newLoginCommand() *cobra.Command {
	return declare(command{
		Name:        "login",
		Description: "Sign in to the hub and save the hub URL and the credential",
		// login needs a hub URL but takes no credential from the
		// environment or the file: it obtains the one it saves.
		Auth: authNone,
		Flags: func(fs *pflag.FlagSet) {
			fs.String("provider", "github", "identity provider of the browser login")
			fs.String("expires", "90", "days the browser login's API key lives: 0, 30, 60 or 90 (0: no expiry)")
		},
		Run: login,
	})
}

// login is the run step of parley login: it signs in through the browser,
// or checks the credential --api-key gives, saves the hub URL, the
// credential and the user's id, and prints the user.
func login(c *call) error {
	// Only the global flag --api-key selects the key flow, not
	// PARLEY_API_KEY or the file; --provider and --expires belong to the
	// browser flow.
	browser := !c.cmd.Flags().Changed("api-key")
	// What the command line gives is checked first, so that a usage error
	// is reported before any setting is read.
	var days int
	var cred string
	var err error
	if browser {
		expires, _ := c.cmd.Flags().GetString("expires")
		days, err = loginExpires(expires)
	} else {
		cred, err = loginCredential(globalFlag(c.cmd, "api-key"), c.opts.stdin)
	}
	if err != nil {
		return err
	}
	// The file is read, and its path found, before anything is sent, so
	// that a file or a missing home directory the save would fail on cannot
	// cost a credential the hub just accepted or issued.
	if _, err := configPath(); err != nil {
		return clientError("%v", err)
	}
	file, err := readConfig()
	if err != nil {
		return err
	}
	hubURL := firstNonEmpty(givenHubURL(c.cmd), file.EndpointURL)
	if hubURL == "" {
		return clientError("%s", noHubURL())
	}
	if err := checkHubURL(hubURL); err != nil {
		return err
	}

	// The requests before the credential is known carry none.
	hub := c.newClient(hubURL, "")
	var user loginUser
	if browser {
		provider, _ := c.cmd.Flags().GetString("provider")
		cred, user, err = browserLogin(c.ctx, hub, provider, days, c.opts, c.stderr)
	} else {
		user, err = hubUser(c.ctx, c.newClient(hubURL, cred))
	}
	if err != nil {
		return err
	}

	if err := saveConfig(configSettings{EndpointURL: hub.hubURL, APIKey: cred, UserID: user.id}); err != nil {
		if browser {
			return clientError("the hub issued an API key, but it was not saved: %v", err)
		}
		return clientError("the hub accepted the credential, but it was not saved: %v", err)
	}
	if err := c.printResult(user.raw); err != nil {
		return err
	}
	fmt.Fprintf(c.stderr, "Logged in as %s\n", user.username)
	return nil
}

// loginCredential is the value of --api-key, or what stdin holds when the
// value is -, without its surrounding white space.
func loginCredential(value string, stdin io.Reader) (string, error) {
	if value == "-" {
		data, err := io.ReadAll(io.LimitReader(stdin, maxStdinCredential))
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
	// raw is the User as the hub answered it.
	raw []byte
}

// hubUser asks the hub who the credential of hub belongs to, with GET
// /api/v1/user.
func hubUser(ctx context.Context, hub *client) (loginUser, error) {
	resp, err := hub.request(ctx, http.MethodGet, "/user", nil, nil)
	if err != nil {
		return loginUser{}, err
	}
	return parseUser(resp.body)
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
	return loginUser{id: id, username: username, raw: raw}, nil
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
func browserLogin(ctx context.Context, hub *client, provider string, days int, opts options, stderr io.Writer) (string, loginUser, error) {
	authorize, err := hubProvider(ctx, hub, provider)
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

	signIn := struct {
		Provider    string `json:"provider"`
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
		Expires     int    `json:"expires"`
	}{provider, code, redirectURI, days}
	resp, err := hub.send(ctx, apiRequest{method: http.MethodPost, path: "/auth/callback", body: signIn, timeout: callbackRequestTimeout})
	if err != nil {
		return "", loginUser{}, err
	}
	var answer struct {
		User   json.RawMessage `json:"user"`
		APIKey struct {
			Key string `json:"key"`
		} `json:"api_key"`
	}
	if json.Unmarshal(resp.body, &answer) != nil || answer.APIKey.Key == "" {
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
func hubProvider(ctx context.Context, hub *client, name string) (string, error) {
	resp, err := hub.request(ctx, http.MethodGet, "/auth/providers", nil, nil)
	if err != nil {
		return "", err
	}
	var list struct {
		Items []struct {
			Name         string `json:"name"`
			AuthorizeURL string `json:"authorize_url"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.body, &list); err != nil {
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
