package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// configDirEnv names the variable that moves the client's config directory
// away from ~/.parley.
const configDirEnv = "PARLEY_CONFIG_DIR"

// configFileName is the client configuration file in the config directory.
const configFileName = "config.toml"

// The keys parley login sets in the configuration file.
const (
	configEndpointURL = "endpoint_url"
	configAPIKey      = "api_key"
	configUserID      = "user_id"
)

// configPath is the client configuration file: config.toml in
// $PARLEY_CONFIG_DIR when it is set and not empty, else in ~/.parley.
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

// readConfig reads the configuration file at path into a map, so that a save
// keeps the keys this client does not know. A missing file is an empty map.
func readConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the config file: %w", err)
	}
	cfg := map[string]any{}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		// Only the position: the parser's message can quote the text at the
		// mistake, and this file holds a credential.
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("the config file %s is not valid TOML: line %d, column %d", path, pe.Position.Line, pe.Position.Col)
		}
		return nil, fmt.Errorf("the config file %s is not valid TOML", path)
	}
	return cfg, nil
}

// saveConfig writes cfg to path. It writes a temporary file with mode 0600 in
// the same directory and renames it over path, so a reader sees the old file
// or the new one and never a part of either; the directory is created with
// mode 0700 when it is missing. On failure the temporary file is removed.
func saveConfig(path string, cfg map[string]any) (err error) {
	data, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// CreateTemp creates the file with mode 0600.
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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
