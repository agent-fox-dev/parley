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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// DefaultLoginWait bounds the wait for the browser's redirect.
const DefaultLoginWait = 120 * time.Second

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
			lf.apiKey = flags.apiKey
			l := &login{flags: flags, lf: lf, opts: opts, cmd: cmd, keyFlow: cmd.Flags().Changed("api-key")}
			return l.run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&lf.provider, "provider", "github", "the identity provider to sign in with")
	cmd.Flags().StringVar(&lf.expires, "expires", "90", "the new key's lifetime in days: 0, 30, 60 or 90")
	return cmd
}

// login is one run of parley login.
type login struct {
	flags   *globalFlags
	lf      loginFlags
	opts    Options
	cmd     *cobra.Command
	keyFlow bool

	settings Settings
	hub      string
}

// run reads the configuration file before any request, resolves the hub
// URL, and runs the key flow or the browser flow.
func (l *login) run(ctx context.Context) error {
	settings, err := readConfig()
	if err != nil {
		return err
	}
	l.settings = settings
	l.hub = strings.TrimRight(l.hubURL(), "/")
	if l.hub == "" {
		path, _ := configPath()
		return ClientError("no hub URL: give --endpoint-url, set PARLEY_ENDPOINT_URL or set endpoint_url in %s", path)
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
	return l.settings.EndpointURL
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
			return ClientError("cannot read the credential from stdin: %v", err)
		}
		cred = string(b)
	}
	cred = strings.TrimSpace(cred)
	if cred == "" {
		return UsageError("--api-key needs a credential")
	}
	user, err := l.request(ctx, http.MethodGet, "/user", cred, nil)
	if err != nil {
		return err
	}
	return l.finish(cred, user)
}

// request sends one request through the shared client — path relative to
// /api/v1, the credential only when one is given — and decodes the answer
// as every command does.
func (l *login) request(ctx context.Context, method, path, cred string, body any) ([]byte, error) {
	c, err := newClient(l.hub, cred)
	if err != nil {
		return nil, err
	}
	resp, err := c.Send(ctx, method, path, nil, body)
	if err != nil {
		return nil, err
	}
	return resp.Result()
}

// finish saves the hub URL, the credential and the user's id, prints the
// user on stdout and greets on stderr.
func (l *login) finish(cred string, user []byte) error {
	var u struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(user, &u); err != nil || u.ID == "" {
		return ClientError("the hub's answer carries no user")
	}
	if err := SaveSettings(Settings{EndpointURL: l.hub, APIKey: cred, UserID: u.ID}); err != nil {
		return ClientError("the hub accepted the credential but it was not saved: %v", err)
	}
	if err := printResult(l.cmd.OutOrStdout(), user); err != nil {
		return err
	}
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
		return UsageError("--expires must be 0, 30, 60 or 90")
	}
	authorizeURL, err := l.pickProvider(ctx)
	if err != nil {
		return err
	}
	code, redirectURI, err := l.awaitRedirect(ctx, authorizeURL)
	if err != nil {
		return err
	}
	answer, err := l.request(ctx, http.MethodPost, "/auth/callback", "", map[string]any{
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
		return ClientError("the hub's login answer carries no key")
	}
	return l.finish(out.APIKey.Key, out.User)
}

// pickProvider fetches the providers the hub lists, without a credential,
// and returns the authorize URL of the one --provider names.
func (l *login) pickProvider(ctx context.Context) (string, error) {
	data, err := l.request(ctx, http.MethodGet, "/auth/providers", "", nil)
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
		return "", ClientError("the hub's provider list is not readable")
	}
	var names []string
	for _, it := range list.Items {
		if it.Name == l.lf.provider {
			return it.AuthorizeURL, nil
		}
		names = append(names, it.Name)
	}
	if len(names) == 0 {
		return "", ClientError("the hub offers no identity provider")
	}
	return "", ClientError("the hub does not offer the provider %s; it offers %s", l.lf.provider, strings.Join(names, ", "))
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
		return "", "", ClientError("%v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", ClientError("cannot listen for the browser's redirect: %v", err)
	}
	redirectURI = "http://" + ln.Addr().String() + "/callback"
	target, err := withQuery(authorizeURL, map[string]string{"redirect_uri": redirectURI, "state": state})
	if err != nil {
		_ = ln.Close()
		return "", "", ClientError("the provider's authorize URL is not usable: %v", err)
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
		return "", "", ClientError("no sign-in arrived from the browser within %s", wait)
	case <-ctx.Done():
		return "", "", ClientError("the login was cancelled")
	}
}

// checkRedirect accepts a redirect that carries a code and the state that
// was sent, compared in constant time.
func checkRedirect(q url.Values, state string) redirectResult {
	switch {
	case q.Get("error") != "":
		return redirectResult{err: ClientError("the provider refused the sign-in")}
	case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1:
		return redirectResult{err: ClientError("the sign-in's state does not match")}
	case q.Get("code") == "":
		return redirectResult{err: ClientError("the sign-in brought no code")}
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
