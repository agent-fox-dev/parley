package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
)

// configDirEnv names the variable that moves the client's config directory
// away from ~/.parley.
const configDirEnv = "PARLEY_CONFIG_DIR"

// configFileName is the client configuration file in the config directory.
const configFileName = "config.toml"

// The keys of the configuration file parley knows; parley login sets them.
const (
	configEndpointURL = "endpoint_url"
	configAPIKey      = "api_key"
	configUserID      = "user_id"
)

// apiKeyEnv names the variable that gives the credential when the flag
// --api-key does not.
const apiKeyEnv = "PARLEY_API_KEY"

// settings are what a command whose auth is user or admin sends its requests
// with.
type settings struct {
	hubURL, credential string
}

// resolveSettings takes the hub URL from the first non-empty of the flag
// --endpoint-url, PARLEY_ENDPOINT_URL and the file's endpoint_url, and the
// credential from the first non-empty of --api-key, PARLEY_API_KEY and the
// file's api_key, each value as given, white space included. The file is
// read only when the flags and the environment leave one of the two empty,
// so that a caller who passes both is not stopped by a file it never uses; a
// file that cannot be read or parsed is then a client error naming it. A
// setting none of the three gives, and a hub URL checkHubURL refuses, are
// client errors too.
func resolveSettings(cmd *cobra.Command) (settings, error) {
	s := settings{
		hubURL:     givenHubURL(cmd),
		credential: firstNonEmpty(globalFlag(cmd, "api-key"), os.Getenv(apiKeyEnv)),
	}
	if s.hubURL == "" || s.credential == "" {
		file, err := readConfig()
		if err != nil {
			return settings{}, err
		}
		s.hubURL = firstNonEmpty(s.hubURL, file.EndpointURL)
		s.credential = firstNonEmpty(s.credential, file.APIKey)
	}

	var missing []string
	if s.hubURL == "" {
		missing = append(missing, noHubURL())
	}
	if s.credential == "" {
		missing = append(missing, fmt.Sprintf("no credential: pass --api-key, set %s or set %s in %s", apiKeyEnv, configAPIKey, configPathForMessage()))
	}
	if len(missing) > 0 {
		return settings{}, clientError("%s", strings.Join(missing, "; "))
	}
	if err := checkHubURL(s.hubURL); err != nil {
		return settings{}, err
	}
	return s, nil
}

// givenHubURL is the hub URL of the first non-empty of the flag
// --endpoint-url and PARLEY_ENDPOINT_URL, or "" when both are empty; the
// config file's endpoint_url comes after them, read by the caller when it
// needs the file.
func givenHubURL(cmd *cobra.Command) string {
	return firstNonEmpty(globalFlag(cmd, "endpoint-url"), os.Getenv(endpointURLEnv))
}

// noHubURL is the message of a command that needs a hub URL and was given
// none: where a hub URL can be given.
func noHubURL() string {
	return fmt.Sprintf("no hub URL: pass --endpoint-url, set %s or set %s in %s", endpointURLEnv, configEndpointURL, configPathForMessage())
}

// checkHubURL refuses a hub URL that is not an absolute URL with scheme http
// or https and a host, and one with a query or a fragment, which would come
// before the path of every request.
func checkHubURL(hubURL string) error {
	u, err := url.Parse(hubURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(hubURL, "?#") {
		return clientError("the hub URL %q is not an absolute http or https URL with a host and without a query or fragment", hubURL)
	}
	return nil
}

// firstNonEmpty is the first of values that is not empty, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// configPathForMessage is the configuration file's path for a message: the
// path configPath gives, or ~/.parley/config.toml when there is no home
// directory to put in its place.
func configPathForMessage() string {
	if path, err := configPath(); err == nil {
		return path
	}
	return filepath.Join("~", ".parley", configFileName)
}

// configSettings are the values of the keys of the configuration file parley
// knows; a key the file does not hold is empty.
type configSettings struct {
	EndpointURL, APIKey, UserID string
}

// configPath is the client configuration file: config.toml in
// $PARLEY_CONFIG_DIR when it is set and not empty, else in ~/.parley. It
// fails when it needs the home directory and none can be determined.
func configPath() (string, error) {
	if dir := os.Getenv(configDirEnv); dir != "" {
		return filepath.Join(dir, configFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find the config directory: set %s or HOME: %w", configDirEnv, err)
	}
	return filepath.Join(home, ".parley", configFileName), nil
}

// readConfig reads the configuration file. A missing file, or no path for
// it because there is no home directory, gives empty settings: the file is
// optional and reading it creates nothing. A file that cannot be read or
// parsed is a client error that names it.
func readConfig() (configSettings, error) {
	path, err := configPath()
	if err != nil {
		return configSettings{}, nil
	}
	cfg, err := parseConfig(path)
	if err != nil {
		return configSettings{}, err
	}
	// parseConfig has checked that the known keys hold strings.
	endpointURL, _ := cfg[configEndpointURL].(string)
	apiKey, _ := cfg[configAPIKey].(string)
	userID, _ := cfg[configUserID].(string)
	return configSettings{EndpointURL: endpointURL, APIKey: apiKey, UserID: userID}, nil
}

// saveConfig sets endpoint_url, api_key and user_id of the configuration
// file to s and keeps every other key and table of the file with its value.
// The new file replaces the old one by a rename, so a reader sees one or the
// other and never a part of either, and it has mode 0600. A file it cannot
// parse is not replaced, because its other keys could not be kept. On any
// failure it returns a client error that names the file and leaves the old
// file as it was.
func saveConfig(s configSettings) error {
	path, err := configPath()
	if err != nil {
		return clientError("cannot save the config file: %v", err)
	}
	cfg, err := parseConfig(path)
	if err != nil {
		return err
	}
	cfg[configEndpointURL] = s.EndpointURL
	cfg[configAPIKey] = s.APIKey
	cfg[configUserID] = s.UserID
	data, err := toml.Marshal(cfg)
	if err != nil {
		return clientError("cannot write the config file %s: %v", path, err)
	}
	if err := replaceFile(path, data); err != nil {
		return clientError("cannot write the config file %s: %v", path, withoutPath(err))
	}
	return nil
}

// parseConfig decodes the configuration file at path into a map, so that a
// save keeps the keys this client does not know. A missing file is an empty
// map. A file that cannot be read, is not valid TOML, or holds a known key
// whose value is not a string is a client error that names path; the message
// never quotes the file, which holds a credential.
func parseConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, clientError("cannot read the config file %s: %v", path, withoutPath(err))
	}
	cfg := map[string]any{}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		// Only the position: the parser's message can quote the text at the
		// mistake.
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, clientError("the config file %s is not valid TOML: line %d, column %d", path, pe.Position.Line, pe.Position.Col)
		}
		return nil, clientError("the config file %s is not valid TOML", path)
	}
	for _, key := range []string{configEndpointURL, configAPIKey, configUserID} {
		if value, ok := cfg[key]; ok {
			if _, ok := value.(string); !ok {
				return nil, clientError("the config file %s is not valid: %s is not a string", path, key)
			}
		}
	}
	return cfg, nil
}

// replaceFile writes data to a temporary file with mode 0600 in the
// directory of path, creating the directory with mode 0700 when it is
// missing, and renames it over path. On failure the temporary file and the
// directories it created are removed and path is left as it was.
func replaceFile(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	missing := missingDirs(dir)
	defer func() {
		if err != nil {
			for _, d := range missing {
				// Remove only takes an empty directory, so this never
				// removes what someone else put there meanwhile.
				_ = os.Remove(d)
			}
		}
	}()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+configFileName+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	// Explicitly, so that neither the umask nor the old file's mode decides.
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// missingDirs is dir and each of its ancestors that does not exist, the
// deepest first: the directories a save would create.
func missingDirs(dir string) []string {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); !errors.Is(err, fs.ErrNotExist) {
			return missing
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			return missing
		}
	}
}

// withoutPath is err without the operation and path a file system error adds,
// for a message that names the file itself.
func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}
