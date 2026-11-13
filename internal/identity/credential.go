package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// The wire formats of the credentials: an API key is parley_<id>_<secret>
// over apiKeyAlphabet, a PAT parley_pat_<id>_<secret> over patAlphabet, and
// the admin token AdminTokenPrefix and 64 lowercase hex characters. Their
// lengths differ, so a string has at most one of them.
const (
	apiKeyPrefix   = "parley_"
	patPrefix      = "parley_pat_"
	apiKeyAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	patAlphabet    = "abcdefghijklmnopqrstuvwxyz0123456789"
	idLength       = 8
	secretLength   = 32
	adminHexLength = 2 * adminTokenBytes
)

// presented is a credential split by its format.
type presented struct {
	// typ is TypeAdmin, TypeAPIKey or TypePAT.
	typ        string
	id, secret string
}

// parseCredential splits s by the format it has, and reports false when it
// has none of the three.
func parseCredential(s string) (presented, bool) {
	switch {
	case len(s) == len(AdminTokenPrefix)+adminHexLength && strings.HasPrefix(s, AdminTokenPrefix):
		if !over(s[len(AdminTokenPrefix):], "0123456789abcdef") {
			return presented{}, false
		}
		return presented{typ: TypeAdmin, secret: s}, true
	case len(s) == len(patPrefix)+idLength+1+secretLength && strings.HasPrefix(s, patPrefix):
		return splitIDSecret(TypePAT, s[len(patPrefix):], patAlphabet)
	case len(s) == len(apiKeyPrefix)+idLength+1+secretLength && strings.HasPrefix(s, apiKeyPrefix):
		return splitIDSecret(TypeAPIKey, s[len(apiKeyPrefix):], apiKeyAlphabet)
	}
	return presented{}, false
}

// splitIDSecret splits rest, <id>_<secret>, whose length is already known
// to be right.
func splitIDSecret(typ, rest, alphabet string) (presented, bool) {
	id, secret := rest[:idLength], rest[idLength+1:]
	if rest[idLength] != '_' || !over(id, alphabet) || !over(secret, alphabet) {
		return presented{}, false
	}
	return presented{typ: typ, id: id, secret: secret}, true
}

// over reports whether every byte of s is in alphabet.
func over(s, alphabet string) bool {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// randomString returns n characters of alphabet drawn uniformly from the
// system's random source. A byte at or above the largest multiple of the
// alphabet's length is drawn again, so no character is more likely than
// another.
func randomString(alphabet string, n int) (string, error) {
	limit := 256 - 256%len(alphabet)
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("read random bytes: %w", err)
		}
		for _, b := range buf {
			if int(b) < limit && len(out) < n {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}

// newCredential generates an id and a secret over alphabet and returns the
// credential prefix<id>_<secret> with them.
func newCredential(prefix, alphabet string) (credential, id, secret string, err error) {
	if id, err = randomString(alphabet, idLength); err != nil {
		return "", "", "", err
	}
	if secret, err = randomString(alphabet, secretLength); err != nil {
		return "", "", "", err
	}
	return prefix + id + "_" + secret, id, secret, nil
}

// hashSecret returns what the hub stores for the secret part of an API key
// or PAT: its lowercase hex SHA-256. The secrets are 32 random characters,
// so a fast hash is enough.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// dummyHash is compared with the hash of a presented secret when its id is
// not stored, so that an unknown id takes the same path as a wrong secret.
// Finding a secret with this hash would mean breaking SHA-256.
var dummyHash = strings.Repeat("0", sha256.Size*2)
