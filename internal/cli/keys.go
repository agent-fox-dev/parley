package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// The parts of an API key, parley_<key id>_<secret>, each part over
// apiKeyAlphabet. The hub defines the format; the CLI repeats it because it
// talks to the hub only over HTTP.
const (
	apiKeyPrefix       = "parley_"
	apiKeyIDLength     = 8
	apiKeySecretLength = 32
	apiKeyAlphabet     = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// newKeysCommand builds parley keys: the caller's own API keys.
func newKeysCommand() *cobra.Command {
	return group("keys", "List, refresh and revoke your API keys",
		declare(command{
			Name:        "list",
			Description: "List your API keys, newest first",
			Method:      http.MethodGet,
			Path:        "/user/keys",
			Auth:        authUser,
		}),
		declare(command{
			Name:        "refresh",
			Description: "Replace the secret of the API key you are using and save the new key in the config file",
			Method:      http.MethodPost,
			Path:        "/user/keys/:key_id/refresh",
			Auth:        authUser,
			ShowsOnce:   true,
			Run:         refreshKey,
		}),
		declare(command{
			Name:        "revoke",
			Description: "Revoke the API key you are using and remove it from the config file",
			Method:      http.MethodDelete,
			Path:        "/user/keys/:key_id",
			Auth:        authUser,
			Run:         revokeKey,
		}),
	)
}

// apiKeyID is the key id of credential when credential has the form of an
// API key. A PAT or the admin token does not: their prefixes and lengths
// differ.
func apiKeyID(credential string) (string, bool) {
	rest, ok := strings.CutPrefix(credential, apiKeyPrefix)
	if !ok || len(rest) != apiKeyIDLength+1+apiKeySecretLength || rest[apiKeyIDLength] != '_' {
		return "", false
	}
	id, secret := rest[:apiKeyIDLength], rest[apiKeyIDLength+1:]
	if strings.Trim(id, apiKeyAlphabet) != "" || strings.Trim(secret, apiKeyAlphabet) != "" {
		return "", false
	}
	return id, true
}

// sendForConfiguredKey sends the command's request for the API key the
// command presents, the only key it can name, and returns the hub's answer.
// A credential that is not an API key is a client error, and nothing is
// sent. The message does not quote the credential, which is a secret.
func sendForConfiguredKey(c *call) (*response, error) {
	id, ok := apiKeyID(c.client.credential)
	if !ok {
		return nil, clientError("%s needs an API key (%s<key id>_<secret>) as the credential, and the credential given is not one", c.cmd.CommandPath(), apiKeyPrefix)
	}
	return c.client.request(c.ctx, c.decl.Method, routePath(c.decl.Path, map[string]string{"key_id": id}), nil, nil)
}

// refreshKey is the run step of parley keys refresh: it asks the hub for a
// new secret of the key, prints the answer with the show-once warning, and
// saves the new key as api_key of the config file. The hub has replaced the
// key once it answers, so a key that cannot be saved is still printed, and
// the command then fails with a client error.
func refreshKey(c *call) error {
	resp, err := sendForConfiguredKey(c)
	if err != nil {
		return err
	}
	if err := c.printAnswer(resp); err != nil {
		return err
	}
	var answer struct {
		Key string `json:"key"`
	}
	err = json.Unmarshal(resp.body, &answer)
	if err == nil && answer.Key == "" {
		err = errors.New("the hub's answer holds no key")
	}
	if err == nil {
		err = saveRefreshedKey(answer.Key)
	}
	if err != nil {
		warn(c.stderr, "the new key was not saved; it is printed above, and parley login --api-key - saves it")
		return clientError("the new key was not saved: %v", err)
	}
	return nil
}

// saveRefreshedKey sets api_key of the config file to key and keeps every
// other key. Only a file that exists is written: a credential given by the
// flag or the environment has no file to go to, and creating one is not
// the refresh's to do.
func saveRefreshedKey(key string) error {
	exists, err := configFileExists()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("there is no config file %s", configPathForMessage())
	}
	file, err := readConfig()
	if err != nil {
		return err
	}
	return saveConfig(configSettings{EndpointURL: file.EndpointURL, APIKey: key, UserID: file.UserID})
}

// revokeKey is the run step of parley keys revoke: it revokes the key, prints
// the hub's answer, clears api_key and user_id of a config file that exists,
// which now hold a key that no longer works, and tells the user to sign in
// again. An error the hub answers leaves the file as it was.
func revokeKey(c *call) error {
	resp, err := sendForConfiguredKey(c)
	if err != nil {
		return err
	}
	if err := c.printAnswer(resp); err != nil {
		return err
	}
	exists, err := configFileExists()
	if err == nil && exists {
		var file configSettings
		if file, err = readConfig(); err == nil {
			err = saveConfig(configSettings{EndpointURL: file.EndpointURL})
		}
	}
	warn(c.stderr, "the API key is revoked; run parley login to sign in again")
	return err
}

// configFileExists tells whether the config file exists. Without a path for
// it, because there is no home directory, there is no file.
func configFileExists() (bool, error) {
	path, err := configPath()
	if err != nil {
		return false, nil
	}
	_, err = os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, clientError("cannot read the config file %s: %v", path, withoutPath(err))
}
