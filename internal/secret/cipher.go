package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
)

// keyIDSize is the length of a key id: 8 bytes in hex.
const keyIDSize = 16

// Sealed is a value sealed under one of the operator's keys: AES-256-GCM
// with a fresh 12-byte nonce, the binding of its owner as additional
// authenticated data.
type Sealed struct {
	// KeyID is the KeyID of the key that sealed the value.
	KeyID string
	// Bytes are the nonce followed by the ciphertext and its tag.
	Bytes []byte
}

// Seal seals value under the current key, bound to binding, so that only the
// same binding opens it. Without a current key it returns ErrUnavailable.
func Seal(keys Keys, binding, value string) (Sealed, error) {
	if keys.Current == nil {
		return Sealed{}, ErrUnavailable
	}
	aead, err := newAEAD(keys.Current)
	if err != nil {
		return Sealed{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, fmt.Errorf("draw a nonce: %w", err)
	}
	return Sealed{
		KeyID: KeyID(keys.Current),
		Bytes: aead.Seal(nonce, nonce, []byte(value), []byte(binding)),
	}, nil
}

// Open opens a sealed value with the configured key whose id it carries, the
// current or the previous one. It returns ErrUnreadable when neither key has
// that id or the value fails authentication, a different binding included.
func Open(keys Keys, binding string, sealed Sealed) (string, error) {
	key := keyByID(keys, sealed.KeyID)
	if key == nil {
		return "", ErrUnreadable
	}
	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}
	n := aead.NonceSize()
	if len(sealed.Bytes) < n {
		return "", ErrUnreadable
	}
	plain, err := aead.Open(nil, sealed.Bytes[:n], sealed.Bytes[n:], []byte(binding))
	if err != nil {
		return "", ErrUnreadable
	}
	return string(plain), nil
}

// SealValue seals a value that is not a named secret, such as the signing
// secret of an event webhook, under the current key and bound to the
// caller's own binding string. The result carries its key id in front of the
// sealed bytes, so it can be stored anywhere and opened and rotated by the
// same rules as a named secret. Without a current key it returns
// ErrUnavailable.
func SealValue(keys Keys, binding, value string) ([]byte, error) {
	s, err := Seal(keys, binding, value)
	if err != nil {
		return nil, err
	}
	return append([]byte(s.KeyID), s.Bytes...), nil
}

// OpenValue opens what SealValue returned with the same binding. It returns
// an error matching ErrUnreadable when neither configured key fits, the
// binding differs or the bytes were changed.
func OpenValue(keys Keys, binding string, sealed []byte) (string, error) {
	s, ok := splitValue(sealed)
	if !ok {
		return "", fmt.Errorf("sealed value %w", ErrUnreadable)
	}
	v, err := Open(keys, binding, s)
	if err != nil {
		return "", fmt.Errorf("sealed value %w", err)
	}
	return v, nil
}

// NeedsReseal reports whether what SealValue returned is under a key other
// than the current one, which the re-encryption after a rotation converts.
func NeedsReseal(keys Keys, sealed []byte) bool {
	s, ok := splitValue(sealed)
	return !ok || keys.Current == nil || s.KeyID != KeyID(keys.Current)
}

// splitValue splits what SealValue returned into its key id and its bytes.
func splitValue(sealed []byte) (Sealed, bool) {
	if len(sealed) < keyIDSize {
		return Sealed{}, false
	}
	return Sealed{KeyID: string(sealed[:keyIDSize]), Bytes: sealed[keyIDSize:]}, true
}

// keyByID returns the configured key with the given id, or nil.
func keyByID(keys Keys, id string) Key {
	for _, k := range []Key{keys.Current, keys.Previous} {
		if k != nil && KeyID(k) == id {
			return k
		}
	}
	return nil
}

// newAEAD returns AES-256-GCM under key.
func newAEAD(key Key) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// entryBinding is the binding of a named secret: its scope, its scope id and
// its key in lowercase, each prefixed with its length so that no two entries
// share one. A sealed value copied to another entry therefore fails
// authentication.
func entryBinding(scope, scopeID, key string) string {
	var b strings.Builder
	b.WriteString("secret")
	for _, part := range []string{scope, scopeID, asciiLower(key)} {
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
	}
	return b.String()
}

// asciiLower lowers A to Z and leaves every other byte as it is: keys are
// ASCII, and their case is folded byte by byte.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
