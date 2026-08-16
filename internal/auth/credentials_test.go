package auth

import (
	"strings"
	"testing"
)

func TestNormaliseEmailLowercasesAndTrims(t *testing.T) {
	cases := map[string]string{
		"Sam@Example.COM":     "sam@example.com",
		"  sam@example.com  ": "sam@example.com",
		"SAM@EXAMPLE.COM":     "sam@example.com",

		// net/mail accepts a display name; only the address is stored, so registering as
		// "Sam <sam@example.com>" and signing in as "sam@example.com" reach the same account.
		"Sam <sam@example.com>": "sam@example.com",
	}

	for input, want := range cases {
		got, err := NormaliseEmail(input)
		if err != nil {
			t.Fatalf("NormaliseEmail(%q) failed: %v", input, err)
		}
		if got != want {
			t.Fatalf("NormaliseEmail(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormaliseEmailRejectsBadInput(t *testing.T) {
	rejected := []string{
		"",
		"   ",
		"not-an-email",
		"@example.com",
		"sam@",
		strings.Repeat("a", 320) + "@example.com",
	}

	for _, input := range rejected {
		if _, err := NormaliseEmail(input); err == nil {
			t.Fatalf("NormaliseEmail(%q) should have failed", input)
		}
	}
}

func TestValidatePasswordEnforcesLengthOnly(t *testing.T) {
	if err := ValidatePassword("1234567"); err == nil {
		t.Fatal("a seven-character password should be rejected")
	}
	if err := ValidatePassword("12345678"); err != nil {
		t.Fatalf("an eight-character password should be accepted: %v", err)
	}

	// No composition rules: all-lowercase, all-digits and a passphrase are equally acceptable.
	for _, password := range []string{"aaaaaaaaaa", "1234567890", "correct horse battery staple"} {
		if err := ValidatePassword(password); err != nil {
			t.Fatalf("ValidatePassword(%q) should not impose composition rules: %v", password, err)
		}
	}

	if err := ValidatePassword(strings.Repeat("x", 1025)); err == nil {
		t.Fatal("an unbounded password is unbounded hashing work and should be rejected")
	}
}
