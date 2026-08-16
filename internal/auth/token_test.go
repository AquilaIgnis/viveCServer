package auth

import (
	"strings"
	"testing"
)

func TestMintDeviceTokenProducesMatchingHash(t *testing.T) {
	plainToken, tokenHash, err := MintDeviceToken()
	if err != nil {
		t.Fatalf("minting failed: %v", err)
	}

	if !strings.HasPrefix(plainToken, TokenPrefix) {
		t.Fatalf("token %q does not carry the %q prefix", plainToken, TokenPrefix)
	}
	if len(tokenHash) != 32 {
		t.Fatalf("token hash is %d bytes, want a 32-byte SHA-256", len(tokenHash))
	}

	if string(HashDeviceToken(plainToken)) != string(tokenHash) {
		t.Fatal("hashing the minted token does not reproduce the stored hash, so nothing would authenticate")
	}
}

func TestMintDeviceTokenIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		plainToken, _, err := MintDeviceToken()
		if err != nil {
			t.Fatalf("minting failed: %v", err)
		}
		if seen[plainToken] {
			t.Fatal("minted the same token twice, so the randomness is not random")
		}
		seen[plainToken] = true
	}
}

func TestParseBearerToken(t *testing.T) {
	accepted := map[string]string{
		"Bearer vive_abc": "vive_abc",
		// RFC 7235 makes the scheme case-insensitive, and clients differ on how they spell it.
		"bearer vive_abc":  "vive_abc",
		"BEARER vive_abc":  "vive_abc",
		"Bearer  vive_abc": "vive_abc",
	}
	for header, want := range accepted {
		got, ok := ParseBearerToken(header)
		if !ok || got != want {
			t.Fatalf("ParseBearerToken(%q) = (%q, %v), want (%q, true)", header, got, ok, want)
		}
	}

	rejected := []string{
		"",
		"vive_abc",
		"Basic dXNlcjpwYXNz",
		"Bearer",
		"Bearer ",
		"Bearer    ",
	}
	for _, header := range rejected {
		if got, ok := ParseBearerToken(header); ok {
			t.Fatalf("ParseBearerToken(%q) accepted %q, want rejection", header, got)
		}
	}
}
