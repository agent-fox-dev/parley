package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
)

// The keys of the client configuration file the CLI knows.
const (
	keyEndpointURL = "endpoint_url"
	keyAPIKey      = "api_key"
	keyUserID      = "user_id"
)

// Settings are the values of the keys the CLI knows.
type Settings struct {
	EndpointURL string
	APIKey      string
	UserID      string
}

// configPath is $PARLEY_CONFIG_DIR/config.toml when the variable is set and
// not empty, otherwise .parley/config.toml in the user's home directory. ok
// is false when neither the variable nor a home directory gives a place;
// path is then ~/.parley/config.toml, for messages.
func configPath() (path string, ok bool) {
	if dir := os.Getenv("PARLEY_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "config.toml"), true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join("~", ".parley", "config.toml"), false
	}
	return filepath.Join(home, ".parley", "config.toml"), true
}

// decodeConfig decodes the file at path into a map; a missing file is an
// empty map and exists false. A file that cannot be read, is not valid
// TOML, or holds a known key whose value is not a string is unparseable.
func decodeConfig(path string) (m map[string]any, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, true, ClientError("cannot read the configuration file %s: %v", path, err)
	}
	m = map[string]any{}
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, true, ClientError("the configuration file %s is not valid TOML: %v", path, err)
	}
	for _, k := range []string{keyEndpointURL, keyAPIKey, keyUserID} {
		if v, ok := m[k]; ok {
			if _, isString := v.(string); !isString {
				return nil, true, ClientError("the configuration file %s holds %s that is not a string", path, k)
			}
		}
	}
	return m, true, nil
}

// readConfig reads the settings of the configuration file. A missing file,
// or no place for it, gives empty settings; reading creates nothing.
func readConfig() (Settings, error) {
	path, ok := configPath()
	if !ok {
		return Settings{}, nil
	}
	m, _, err := decodeConfig(path)
	if err != nil {
		return Settings{}, err
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	return Settings{EndpointURL: str(keyEndpointURL), APIKey: str(keyAPIKey), UserID: str(keyUserID)}, nil
}

// saveSettings saves the settings, as parley login does: it creates the
// directory with mode 0700 when missing, keeps every key of the existing
// file it does not know, writes a temporary file beside it, syncs it, sets
// mode 0600 and renames it over config.toml, so a reader sees the old file
// or the new one. Any failure is a client error naming the path, leaves the
// previous file as it was and removes the temporary file.
func saveSettings(s Settings) error {
	path, ok := configPath()
	if !ok {
		return ClientError("cannot save the configuration file %s: no home directory and PARLEY_CONFIG_DIR is not set", path)
	}
	m, _, err := decodeConfig(path)
	if err != nil {
		return ClientError("cannot save the configuration file %s: its content cannot be kept (%v)", path, err)
	}
	m[keyEndpointURL], m[keyAPIKey], m[keyUserID] = s.EndpointURL, s.APIKey, s.UserID
	data, err := toml.Marshal(m)
	if err != nil {
		return ClientError("cannot save the configuration file %s: %v", path, err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ClientError("cannot save the configuration file %s: %v", path, err)
	}
	if err := replaceFile(dir, path, data); err != nil {
		return ClientError("cannot save the configuration file %s: %v", path, err)
	}
	return nil
}

// replaceFile writes data to a new file with mode 0600 in dir and renames it
// over path, removing it on any failure.
func replaceFile(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".config.toml.*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	merr := tmp.Chmod(0o600)
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, merr, cerr); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

// flagValue reads a global flag the command inherited.
func flagValue(cmd *cobra.Command, name string) string {
	if f := cmd.Flag(name); f != nil {
		return f.Value.String()
	}
	return ""
}

// resolveSettings takes the hub URL and the credential each from the first
// non-empty value of its flag, its variable and the configuration file,
// reading the file only when a flag and the environment leave one empty. A
// missing setting is a client error naming, for each one missing, its
// flag, its variable and the file's path.
func resolveSettings(cmd *cobra.Command) (hubURL, credential string, err error) {
	hubURL = firstNonEmpty(flagValue(cmd, "endpoint-url"), os.Getenv("PARLEY_ENDPOINT_URL"))
	credential = firstNonEmpty(flagValue(cmd, "api-key"), os.Getenv("PARLEY_API_KEY"))
	if hubURL != "" && credential != "" {
		return hubURL, credential, nil
	}
	file, err := readConfig()
	if err != nil {
		return "", "", err
	}
	hubURL = firstNonEmpty(hubURL, file.EndpointURL)
	credential = firstNonEmpty(credential, file.APIKey)
	path, _ := configPath()
	var missing []string
	if hubURL == "" {
		missing = append(missing, "no hub URL: give --endpoint-url, set PARLEY_ENDPOINT_URL or set endpoint_url in "+path)
	}
	if credential == "" {
		missing = append(missing, "no credential: give --api-key, set PARLEY_API_KEY or set api_key in "+path)
	}
	if len(missing) > 0 {
		return "", "", ClientError("%s", strings.Join(missing, "; "))
	}
	return hubURL, credential, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
