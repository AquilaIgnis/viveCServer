package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// TokenPrefix marks a string as one of this server's device tokens.
//
// A prefix costs nothing and earns two things: a token that leaks into a log or a paste is
// recognisable for what it is, and secret scanners can be taught one pattern that matches it.
const TokenPrefix = "vive_"

// AdminSessionTokenPrefix distinguishes browser sessions from device credentials. Both are
// random bearer secrets, but they are deliberately rejected outside the surface that minted them.
const AdminSessionTokenPrefix = "vive_admin_"

// tokenRandomBytes is 32 bytes — 256 bits — of randomness. A device token is a bearer credential
// with no expiry, so it is sized to be permanently infeasible to guess rather than merely
// expensive.
const tokenRandomBytes = 32

// MintDeviceToken returns a new token and the hash to store for it.
//
// The plaintext is shown once, at registration, and is never recoverable afterwards. That is the
// point: a database dump must not be enough to authenticate as somebody.
func MintDeviceToken() (plainToken string, tokenHash []byte, err error) {
	return mintToken(TokenPrefix)
}

// MintAdminSessionToken returns the browser cookie value and the digest stored in PostgreSQL.
func MintAdminSessionToken() (plainToken string, tokenHash []byte, err error) {
	return mintToken(AdminSessionTokenPrefix)
}

func mintToken(prefix string) (plainToken string, tokenHash []byte, err error) {
	randomBytes := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", nil, fmt.Errorf("generating a token: %w", err)
	}

	plainToken = prefix + base64.RawURLEncoding.EncodeToString(randomBytes)
	return plainToken, HashToken(plainToken), nil
}

// HashDeviceToken reduces a token to what is stored and compared.
//
// SHA-256, not Argon2id, and the difference from HashPassword is deliberate. A password is
// low-entropy and chosen by a human, so it must be expensive to hash or it can be guessed offline.
// A token is 256 bits from crypto/rand, so there is no guessing to deter — and this runs on every
// authenticated request, where a 64 MiB key derivation would be a self-inflicted denial of service.
func HashDeviceToken(plainToken string) []byte {
	return HashToken(plainToken)
}

// HashToken is shared by device and admin-session tokens. Both have 256 bits of entropy, so a fast
// one-way digest is appropriate; Argon2id is reserved for human-chosen passwords.
func HashToken(plainToken string) []byte {
	digest := sha256.Sum256([]byte(plainToken))
	return digest[:]
}

// ParseAdminSessionToken rejects malformed or cross-surface cookie values before a database read.
func ParseAdminSessionToken(candidate string) (string, bool) {
	if !strings.HasPrefix(candidate, AdminSessionTokenPrefix) || len(candidate) <= len(AdminSessionTokenPrefix) {
		return "", false
	}
	return candidate, true
}

// ParseBearerToken extracts a token from an Authorization header value, returning false if the
// header is absent or not a well-formed bearer credential.
func ParseBearerToken(authorizationHeader string) (string, bool) {
	const bearerScheme = "Bearer "

	// The scheme is case-insensitive per RFC 7235, and clients differ on how they spell it.
	if len(authorizationHeader) <= len(bearerScheme) ||
		!strings.EqualFold(authorizationHeader[:len(bearerScheme)], bearerScheme) {
		return "", false
	}

	token := strings.TrimSpace(authorizationHeader[len(bearerScheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
