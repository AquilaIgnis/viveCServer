// Package auth owns credentials: how a password becomes a hash, how a device token is minted and
// recognised, and how an authenticated caller travels through a request.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. Deliberately named rather than inlined, because they are the whole security
// margin of a stored password and somebody will eventually want to raise them.
//
// Raising them is safe and does not invalidate anything: every hash carries the parameters it was
// made with (see the PHC string below), so old passwords keep verifying under the old cost and are
// re-hashed at their next successful sign-in. That is what NeedsRehash is for.
//
// 64 MiB is a deliberate choice for a server whose sign-ins are rare — a device signs in once and
// then holds a token — so the cost lands on an attacker with a stolen database rather than on
// ordinary use. It is also, on its own, a meaningful brake on online guessing: each attempt costs
// the attacker 64 MiB and tens of milliseconds.
const (
	argonMemoryKiB  uint32 = 64 * 1024
	argonIterations uint32 = 3
	argonThreads    uint8  = 2
	argonSaltBytes  int    = 16
	argonKeyBytes   uint32 = 32
)

// argonVersion is the algorithm version constant Argon2 stamps into its own PHC strings.
const argonVersion = 19

var (
	// ErrPasswordMismatch is returned when a password does not verify. It is deliberately the same
	// error whatever went wrong, so nothing about the stored hash leaks through the difference.
	ErrPasswordMismatch = errors.New("password does not match")

	ErrHashMalformed = errors.New("stored password hash is not a valid argon2id PHC string")
)

// HashPassword returns a PHC-format `$argon2id$...` string safe to store verbatim.
//
// The format carries the version, the cost parameters and the salt alongside the digest. Storing
// them together is what makes the cost a per-row property rather than a global constant that can
// never change without locking everyone out.
func HashPassword(plainPassword string) (string, error) {
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating a password salt: %w", err)
	}

	digest := argon2.IDKey(
		[]byte(plainPassword), salt,
		argonIterations, argonMemoryKiB, argonThreads, argonKeyBytes,
	)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, argonMemoryKiB, argonIterations, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

// VerifyPassword reports whether plainPassword produced encodedHash.
//
// The comparison is constant time. A byte-by-byte comparison that returns early leaks, through
// timing, how much of a guess was right, which turns an infeasible search into a feasible one.
func VerifyPassword(encodedHash string, plainPassword string) error {
	parameters, salt, expectedDigest, err := parsePHCString(encodedHash)
	if err != nil {
		return err
	}

	actualDigest := argon2.IDKey(
		[]byte(plainPassword), salt,
		parameters.iterations, parameters.memoryKiB, parameters.threads, uint32(len(expectedDigest)),
	)

	if subtle.ConstantTimeCompare(actualDigest, expectedDigest) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash was made with weaker parameters than the current ones,
// so a caller who has just verified a password can quietly upgrade it.
func NeedsRehash(encodedHash string) bool {
	parameters, _, _, err := parsePHCString(encodedHash)
	if err != nil {
		// An unreadable hash cannot verify anything, so replacing it is the only useful answer.
		return true
	}
	return parameters.memoryKiB < argonMemoryKiB ||
		parameters.iterations < argonIterations ||
		parameters.threads < argonThreads
}

type argonParameters struct {
	memoryKiB  uint32
	iterations uint32
	threads    uint8
}

func parsePHCString(encodedHash string) (argonParameters, []byte, []byte, error) {
	// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<digest> splits into six, the first being empty.
	fields := strings.Split(encodedHash, "$")
	if len(fields) != 6 || fields[0] != "" || fields[1] != "argon2id" {
		return argonParameters{}, nil, nil, ErrHashMalformed
	}

	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil || version != argonVersion {
		return argonParameters{}, nil, nil, ErrHashMalformed
	}

	var parameters argonParameters
	if _, err := fmt.Sscanf(
		fields[3], "m=%d,t=%d,p=%d",
		&parameters.memoryKiB, &parameters.iterations, &parameters.threads,
	); err != nil {
		return argonParameters{}, nil, nil, ErrHashMalformed
	}
	if parameters.memoryKiB == 0 || parameters.iterations == 0 || parameters.threads == 0 {
		return argonParameters{}, nil, nil, ErrHashMalformed
	}

	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil || len(salt) == 0 {
		return argonParameters{}, nil, nil, ErrHashMalformed
	}

	digest, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil || len(digest) == 0 {
		return argonParameters{}, nil, nil, ErrHashMalformed
	}

	return parameters, salt, digest, nil
}

// decoyHash is a real argon2id hash of a value nobody knows.
//
// sync.OnceValue rather than a plain variable: two failed sign-ins arriving together would race on
// a lazily assigned package variable, and `go test -race` would rightly fail the build for it.
// Built on first use rather than at init so a process that never sees a failed sign-in never pays
// the 64 MiB.
var decoyHash = sync.OnceValue(func() string {
	generated, err := HashPassword("password-that-matches-nothing")
	if err != nil {
		return ""
	}
	return generated
})

// SpendVerificationTime does the work of a password check against a decoy hash and throws the
// result away. Call it on the path where no account was found.
//
// Without it, "no such email" returns in microseconds while "wrong password" takes tens of
// milliseconds, and that difference is a working account-enumeration oracle for anyone with a
// stopwatch and a word list.
func SpendVerificationTime(plainPassword string) {
	hash := decoyHash()
	if hash == "" {
		return
	}
	_ = VerifyPassword(hash, plainPassword)
}
