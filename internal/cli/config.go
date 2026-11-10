package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// The keys of the client configuration file that parley login writes.
const (
	keyEndpointURL = "endpoint_url"
	keyAPIKey      = "api_key"
	keyUserID      = "user_id"
)

// configPath is $PARLEY_CONFIG_DIR/config.toml when the variable is set and
// not empty, otherwise .parley/config.toml in the user's home directory.
func configPath() (string, error) {
	if dir := os.Getenv("PARLEY_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find the home directory: %w", err)
	}
	return filepath.Join(home, ".parley", "config.toml"), nil
}

// readConfig reads the configuration file into a map, so keys the CLI does
// not know survive a save; a missing file is an empty map.
func readConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the configuration file %s: %w", path, err)
	}
	m := map[string]any{}
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("the configuration file %s is not valid TOML: %w", path, err)
	}
	return m, nil
}

// saveConfig replaces the configuration file atomically with mode 0600,
// creating its directory with mode 0700 when missing: it writes a temporary
// file beside it and renames it over the file.
func saveConfig(path string, m map[string]any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config.toml.*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}
