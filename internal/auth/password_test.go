package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	encoded, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}

	if err := VerifyPassword(encoded, password); err != nil {
		t.Fatalf("the password it was made from did not verify: %v", err)
	}
}

func TestPasswordRejectsWrongPassword(t *testing.T) {
	encoded, err := HashPassword("the right one")
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}

	if err := VerifyPassword(encoded, "the wrong one"); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("expected ErrPasswordMismatch, got %v", err)
	}
}

// Two hashes of one password must differ, or the salt is not doing anything and identical passwords
// become visible to anyone reading the table.
func TestPasswordHashesAreSalted(t *testing.T) {
	const password = "same password twice"

	first, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}

	if first == second {
		t.Fatal("two hashes of the same password are identical, so the salt is not random")
	}
	if err := VerifyPassword(second, password); err != nil {
		t.Fatalf("the second hash did not verify: %v", err)
	}
}

func TestPasswordHashIsPHCFormatted(t *testing.T) {
	encoded, err := HashPassword("whatever")
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash does not carry its own parameters: %q", encoded)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	malformed := map[string]string{
		"empty":            "",
		"not PHC":          "just-a-string",
		"wrong algorithm":  "$argon2i$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ$ZGlnZXN0",
		"missing digest":   "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHQ",
		"bad base64 salt":  "$argon2id$v=19$m=65536,t=3,p=2$!!!!$ZGlnZXN0",
		"zero cost":        "$argon2id$v=19$m=0,t=0,p=0$c2FsdHNhbHQ$ZGlnZXN0",
		"unknown version":  "$argon2id$v=16$m=65536,t=3,p=2$c2FsdHNhbHQ$ZGlnZXN0",
		"parameters short": "$argon2id$v=19$m=65536,t=3$c2FsdHNhbHQ$ZGlnZXN0",
	}

	for name, hash := range malformed {
		t.Run(name, func(t *testing.T) {
			if err := VerifyPassword(hash, "anything"); !errors.Is(err, ErrHashMalformed) {
				t.Fatalf("expected ErrHashMalformed, got %v", err)
			}
		})
	}
}

// A hash made under weaker parameters must still verify — that is what makes raising the cost safe
// — and must report that it wants upgrading.
func TestNeedsRehashDetectsWeakerParameters(t *testing.T) {
	const weaker = "$argon2id$v=19$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	if !NeedsRehash(weaker) {
		t.Fatal("a hash made with less memory and fewer iterations should want rehashing")
	}

	current, err := HashPassword("whatever")
	if err != nil {
		t.Fatalf("hashing failed: %v", err)
	}
	if NeedsRehash(current) {
		t.Fatal("a hash made with the current parameters should not want rehashing")
	}
}

func TestSpendVerificationTimeIsSafeToCallConcurrently(t *testing.T) {
	// The decoy hash is built lazily on first use, so the thing being checked here is that two
	// simultaneous failed sign-ins do not race on it. Meaningful only under -race.
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			SpendVerificationTime("guess")
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}
