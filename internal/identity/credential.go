package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"math/big"
	"regexp"
)

// The three credential types.
const (
	TypeAdmin  = "admin"
	TypeAPIKey = "api_key"
	TypePAT    = "pat"
)

// AdminCredentialID is the credential id of the admin token.
const AdminCredentialID = "admin"

const (
	alnum      = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	lowerAlnum = "0123456789abcdefghijklmnopqrstuvwxyz"
)

// The wire formats. Their lengths make them disjoint.
var (
	adminForm  = regexp.MustCompile(`^parley_admin_[0-9a-f]{64}$`)
	patForm    = regexp.MustCompile(`^parley_pat_([a-z0-9]{8})_([a-z0-9]{32})$`)
	apiKeyForm = regexp.MustCompile(`^parley_([0-9A-Za-z]{8})_([0-9A-Za-z]{32})$`)
)

// parsed is a credential recognised by its exact format.
type parsed struct {
	kind   string
	id     string
	secret string
	whole  string
}

// parseCredential recognises a credential by its exact format.
func parseCredential(s string) (parsed, bool) {
	switch {
	case adminForm.MatchString(s):
		return parsed{kind: TypeAdmin, id: AdminCredentialID, whole: s}, true
	case patForm.MatchString(s):
		m := patForm.FindStringSubmatch(s)
		return parsed{kind: TypePAT, id: m[1], secret: m[2], whole: s}, true
	case apiKeyForm.MatchString(s):
		m := apiKeyForm.FindStringSubmatch(s)
		return parsed{kind: TypeAPIKey, id: m[1], secret: m[2], whole: s}, true
	}
	return parsed{}, false
}

// randomString draws n characters of alphabet from crypto/rand.
func randomString(alphabet string, n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[k.Int64()]
	}
	return string(out), nil
}

// HashSecret is the stored form of an API key's or PAT's secret part: its
// lowercase hexadecimal SHA-256.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// secretMatches compares a secret with a stored hash in constant time.
func secretMatches(secret, storedHash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashSecret(secret)), []byte(storedHash)) == 1
}

// dummyHash is compared against when a credential id is unknown, so that an
// unknown id and a wrong secret take the same path.
var dummyHash = HashSecret("parley-identity-dummy-secret")
