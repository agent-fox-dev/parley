// Package secret keeps the hub's secrets: values that their owners write and
// that leave the hub through no response, event, log line or error message.
// At rest a value lies only sealed with AES-256-GCM under a key the operator
// supplies in the environment or a file, and can rotate by keeping the former
// key as the previous one, so a copy of the hub's storage discloses no secret.
package secret

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/agent-fox-dev/parley/internal/config"
)

// The environment variables and the configuration key the operator's keys
// are read from.
const (
	// EnvCurrentKey holds the key new values are sealed under.
	EnvCurrentKey = "PARLEY_SECRETS_KEY"
	// EnvPreviousKey holds the former key during a rotation: values sealed
	// under it are still opened, and re-sealed under the current key.
	EnvPreviousKey = "PARLEY_SECRETS_KEY_PREVIOUS"
	// KeyFileSetting names the file the current key is read from when
	// EnvCurrentKey is unset, as errors spell it.
	KeyFileSetting = "[secrets].key_file"
)

// keySize is the size of an AES-256 key.
const keySize = 32

// errMalformed is the reason given for a key that is not the operator's
// encoding of one; it never quotes what was configured.
var errMalformed = errors.New("must be standard padded base64 of exactly 32 bytes")

// Key is an AES-256 key. It prints, logs and encodes as JSON as "[key]"
// whatever the verb or the handler, so a key that reaches a log line, an
// error or a JSON document by mistake discloses nothing.
type Key []byte

// Format prints "[key]" for every verb.
func (Key) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[key]")) }

// LogValue logs "[key]" in place of the key.
func (Key) LogValue() slog.Value { return slog.StringValue("[key]") }

// MarshalJSON encodes the key as the string "[key]" and an absent key as
// null, so that slog's JSON handler, which encodes a struct holding a key
// such as Keys with encoding/json, discloses whether a key is configured and
// nothing of it.
func (k Key) MarshalJSON() ([]byte, error) {
	if k == nil {
		return []byte("null"), nil
	}
	return []byte(`"[key]"`), nil
}

// Keys are the operator's keys. A nil key is absent: without a current key
// the hub runs and refuses only to store, and a previous key without a
// current one still opens the values sealed under it.
type Keys struct {
	// Current seals every new value and opens the values sealed under it.
	Current Key
	// Previous opens the values sealed under it until they are re-sealed.
	Previous Key
}

// LoadKeys reads the operator's keys: the current key from EnvCurrentKey, or
// when that is unset or empty from keyFile, and the previous key from
// EnvPreviousKey. Each is standard padded base64 of exactly 32 bytes with
// surrounding white space ignored. keyFile is the path the hub has already
// resolved from KeyFileSetting, the empty string when none is set; it is read
// only when EnvCurrentKey is unset, so an unreadable file next to a set
// variable is no error. An error names the variable or the file and never
// what it holds, so it can be logged; the caller decides whether it ends the
// process.
func LoadKeys(env config.Env, keyFile string) (Keys, error) {
	var keys Keys
	var err error
	if v, ok := env(EnvCurrentKey); ok && v != "" {
		if keys.Current, err = decodeKey(v); err != nil {
			return Keys{}, fmt.Errorf("%s: %w", EnvCurrentKey, err)
		}
	} else if keyFile != "" {
		var content []byte
		if content, err = os.ReadFile(keyFile); err != nil {
			return Keys{}, fmt.Errorf("%s %q: cannot be read: %w", KeyFileSetting, keyFile, pathError(err))
		}
		if keys.Current, err = decodeKey(string(content)); err != nil {
			return Keys{}, fmt.Errorf("%s %q: %w", KeyFileSetting, keyFile, err)
		}
	}
	if v, ok := env(EnvPreviousKey); ok && v != "" {
		if keys.Previous, err = decodeKey(v); err != nil {
			return Keys{}, fmt.Errorf("%s: %w", EnvPreviousKey, err)
		}
	}
	return keys, nil
}

// pathError keeps the reason of a failed read without the path, which the
// caller's message already names.
func pathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// decodeKey decodes one configured key.
func decodeKey(text string) (Key, error) {
	raw, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(text))
	if err != nil || len(raw) != keySize {
		return nil, errMalformed
	}
	return Key(raw), nil
}

// KeyID is the fingerprint stored with each sealed value: the first 8 bytes
// of the key's SHA-256 in lowercase hex. It decides which key opens a value,
// and lets the hub count the values no configured key opens without
// decrypting any.
func KeyID(key Key) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}
