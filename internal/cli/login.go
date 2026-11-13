package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// loginRequestTimeout bounds the login's request to the hub.
const loginRequestTimeout = 30 * time.Second

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
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The file is read before anything else, so that a file the save
			// would fail on cannot cost a credential the hub just accepted.
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
			// Only the flag selects the key flow; --provider and --expires
			// belong to the browser flow.
			if !cmd.Flags().Changed("api-key") {
				return clientError("the browser login is not available in this build: pass the credential with --api-key")
			}
			cred, err := loginCredential(apiKey, opts.stdin)
			if err != nil {
				return err
			}
			user, err := hubUser(cmd.Context(), hubURL, cred)
			if err != nil {
				return err
			}

			cfg[configEndpointURL] = hubURL
			cfg[configAPIKey] = cred
			cfg[configUserID] = user.id
			if err := saveConfig(path, cfg); err != nil {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/api/v1/user", nil)
	if err != nil {
		return loginUser{}, clientError("invalid hub URL %q: %v", hubURL, err)
	}
	req.Header.Set("Authorization", "Bearer "+cred)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: loginRequestTimeout}).Do(req)
	if err != nil {
		return loginUser{}, clientError("cannot reach the hub: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLoginBody))
	if err != nil {
		return loginUser{}, clientError("cannot read the hub's answer: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		if isErrorEnvelope(body) {
			return loginUser{}, &loginError{status: hubErrorExitStatus, envelope: body}
		}
		return loginUser{}, clientError("the hub answered %s without an error envelope", resp.Status)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return loginUser{}, clientError("the hub's answer is not a JSON object")
	}
	id, _ := fields["id"].(string)
	if id == "" {
		return loginUser{}, clientError("the hub's answer has no user id")
	}
	username, _ := fields["username"].(string)
	var indented bytes.Buffer
	if err := json.Indent(&indented, bytes.TrimSpace(body), "", "  "); err != nil {
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
