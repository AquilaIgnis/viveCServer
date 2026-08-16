package auth

import (
	"fmt"
	"net/mail"
	"strings"
)

// Bounds on what the server is willing to consider. These are limits on input, not opinions about
// what makes a good password.
const (
	maxEmailChars = 320 // RFC 3696's practical ceiling: 64-character local part, 255-character domain.

	// NIST SP 800-63B: a length floor and nothing else. Composition rules ("one capital, one
	// digit") make passwords harder for people to remember and barely harder for anything to
	// guess, so the server does not impose them.
	MinPasswordChars = 8

	// A ceiling exists only because Argon2id hashes whatever it is given, and an unbounded password
	// is an unbounded amount of work an unauthenticated caller can ask for.
	maxPasswordChars = 1024
)

// NormaliseEmail validates an address and reduces it to the single form stored and compared.
//
// It lives here, in one place, because two callers need it — the HTTP registration endpoint and the
// create-account subcommand — and an account created one way has to sign in the other way. Two
// copies of this rule that drift apart produce an account nobody can log into.
//
// Lowercased in full, including the local part. RFC 5321 permits a case-sensitive local part, but
// no mail provider in practice treats one that way, and users do not believe in it either — someone
// who registers as Sam@example.com and signs in as sam@example.com has not made a mistake and
// should not be told they have.
func NormaliseEmail(rawEmail string) (string, error) {
	trimmed := strings.TrimSpace(rawEmail)
	if trimmed == "" {
		return "", fmt.Errorf("an email address is required")
	}
	if len(trimmed) > maxEmailChars {
		return "", fmt.Errorf("email address is too long")
	}

	// net/mail rather than a regular expression. Every regular expression for an email address is
	// either wrong or unreadable, and this one is the parser the standard library already ships.
	parsed, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", fmt.Errorf("email address is not valid")
	}

	return strings.ToLower(parsed.Address), nil
}

// ValidatePassword enforces the length bounds and nothing else.
func ValidatePassword(plainPassword string) error {
	if len(plainPassword) < MinPasswordChars {
		return fmt.Errorf("password must be at least %d characters", MinPasswordChars)
	}
	if len(plainPassword) > maxPasswordChars {
		return fmt.Errorf("password must be at most %d characters", maxPasswordChars)
	}
	return nil
}
