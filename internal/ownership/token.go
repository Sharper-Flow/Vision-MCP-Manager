package ownership

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

// GenerateOwnerToken creates a cryptographically random URL-safe token.
func GenerateOwnerToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// TokenHash returns the lowercase hexadecimal SHA-256 digest of token.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ConfigHash hashes only the explicit server identity fields. In particular,
// environment values are not a configuration identity input.
func ConfigHash(identity ServerIdentity) string {
	canonical := struct {
		Name      string   `json:"name"`
		Command   string   `json:"command"`
		Args      []string `json:"args"`
		URL       string   `json:"url"`
		Transport string   `json:"transport"`
	}{identity.Name, identity.Command, identity.Args, identity.URL, identity.Transport}
	b, _ := json.Marshal(canonical)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
