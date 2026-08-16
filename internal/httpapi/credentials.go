package httpapi

import (
	"fmt"
	"strings"
)

// These describe the shape of a registration request rather than a credential, which is why they
// live here and email/password validation lives in internal/auth beside the hashing.
const (
	maxDeviceNameChars = 128
	maxPlatformChars   = 64
)

func validateDeviceName(rawName string) (string, error) {
	trimmed := strings.TrimSpace(rawName)
	if trimmed == "" {
		return "", fmt.Errorf("a device name is required")
	}
	if len(trimmed) > maxDeviceNameChars {
		return "", fmt.Errorf("device name is too long")
	}
	return trimmed, nil
}

// validatePlatform allows an empty platform: it is a label for a settings screen, not something the
// server acts on, and refusing a client that does not send one buys nothing.
func validatePlatform(rawPlatform string) (string, error) {
	trimmed := strings.TrimSpace(rawPlatform)
	if len(trimmed) > maxPlatformChars {
		return "", fmt.Errorf("platform is too long")
	}
	return trimmed, nil
}
