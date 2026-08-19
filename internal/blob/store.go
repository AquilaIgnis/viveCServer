// Package blob is the content-addressed store the attachment bytes live in.
//
// It holds files and nothing else: what an account is allowed to read, what a page references and
// what a sweep may remove are decisions made in `store` and `httpapi` against the database. This
// package's whole contract is that a file named by a digest holds exactly the bytes that hash to
// it, and that a file only appears once all of its bytes are durable.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DigestChars is the length of a SHA-256 written as lowercase hex, which is how a digest travels in
// a URL, in `blobRefs`, and as an attachment's id.
const DigestChars = sha256.Size * 2

// stagingDirectory holds partial uploads. A separate directory rather than a suffix in the tree, so
// that a crashed upload can never be mistaken for stored content by anything walking the store, and
// so that clearing it at startup is one call rather than a walk.
const stagingDirectory = "staging"

var (
	// ErrNotFound means the store holds no bytes under that digest.
	ErrNotFound = errors.New("blob not found")

	// ErrDigestMismatch means the bytes that arrived do not hash to the digest they were sent under.
	ErrDigestMismatch = errors.New("content does not match its digest")
)

// Store is the byte store the HTTP handlers talk to.
//
// Shaped so an object-store implementation could replace [FileStore] without the handlers changing
// (syncPlan.md §8): every method is addressed by digest alone, nothing returns a path, and Open's
// result is a stream rather than a buffer. What an S3-backed version would have to keep is the
// ordering promise below — bytes durable before the row that claims them.
type Store interface {
	// Stage reads source to completion under expectedDigest without publishing it.
	//
	// It returns [ErrDigestMismatch] if the bytes do not hash to expectedDigest, which is what turns
	// a body truncated in transit into a rejected upload rather than a stored file that will fail
	// to decode on every device that fetches it.
	Stage(expectedDigest string, source io.Reader) (*StagedContent, error)

	// Exists reports whether the store already holds these bytes.
	Exists(digest string) (bool, error)

	// Open returns the stored bytes for reading. The caller closes the result.
	Open(digest string) (io.ReadSeekCloser, error)

	// Remove deletes the stored bytes. Removing what is not there is not an error: the caller is a
	// sweeper, and a file already gone is the outcome it wanted.
	Remove(digest string) error
}

// FileStore keeps blobs at `<root>/<aa>/<bb>/<sha256>` (syncPlan.md §8).
//
// The two-level fan-out is not decoration. A self-hosted account with a few thousand pictures puts
// a few thousand entries in one directory, which is survivable; the same directory on a shared
// server with every account's pictures in it is where `readdir` and directory-index lookups start
// to cost. Splitting on the first two byte-pairs of the digest spreads them over 65 536 leaves for
// the price of two `mkdir`s per upload, and the digest is uniformly distributed by construction.
type FileStore struct {
	root string
}

// OpenFileStore prepares the directory and proves it is writable before the server accepts traffic.
//
// Checked at startup rather than on the first upload, for the same reason config.Load validates
// everything up front: a self-hoster who mounted the volume read-only should be told by a line at
// boot, not by one device failing to sync a picture a week later.
func OpenFileStore(root string) (*FileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("a blob directory is required")
	}

	// Every failure below is the same failure, and the message has to explain it at whichever one
	// fires first — which is this mkdir, not the write probe further down. That ordering cost a real
	// debugging session on 2026-08-19: the explanation existed and the operator never saw it.
	//
	// The cause is almost always a mount whose ownership does not match the user this process runs
	// as. The Docker daemon runs as root and creates a missing bind-mount path as root; the server
	// runs as nonroot and cannot write into it. A named volume avoids it only because Docker seeds a
	// *new* volume's ownership from the image, which a bind mount never gets.
	unwritable := func(err error) error {
		if !errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("preparing the blob directory %s: %w", root, err)
		}
		return fmt.Errorf("the blob directory %s is not writable by this user (uid %d): %w; "+
			"if it is a bind-mounted host directory, the Docker daemon created it as root — "+
			"`chown %d` it on the host, or mount a Docker volume instead",
			root, os.Getuid(), err, os.Getuid())
	}

	staging := filepath.Join(root, stagingDirectory)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, unwritable(err)
	}

	// Anything in staging is the remains of an upload that was interrupted, by a crash or by a
	// client that hung up. Clearing it at startup keeps a directory of dead partials from growing
	// for ever on a server that restarts more often than it is inspected.
	if err := clearDirectory(staging); err != nil {
		return nil, unwritable(err)
	}

	// A directory that exists and can be listed but not written to — the mkdir above succeeds when
	// `staging` is already there, so this is not redundant with it.
	probe, err := os.CreateTemp(staging, "writable-*")
	if err != nil {
		return nil, unwritable(err)
	}
	probeName := probe.Name()
	_ = probe.Close()
	if err := os.Remove(probeName); err != nil {
		return nil, unwritable(err)
	}

	return &FileStore{root: root}, nil
}

// Root is where the store keeps its files, for a startup log line.
func (store *FileStore) Root() string { return store.root }

// StagedContent is a fully received upload that has not been published yet.
//
// Staging is separate from publishing because publishing has to happen under the same lock as the
// database row that will claim these bytes, and the upload itself must not hold that lock: a 32 MB
// picture over a mobile connection is minutes, and a sweeper waiting minutes behind it is a sweeper
// that never runs.
type StagedContent struct {
	store     *FileStore
	path      string
	digest    string
	byteCount int64
}

// ByteCount is how many bytes arrived. It is what the attachment row's declared size is checked
// against and what the account's storage figure is charged, and it is measured rather than believed.
func (staged *StagedContent) ByteCount() int64 { return staged.byteCount }

func (store *FileStore) Stage(expectedDigest string, source io.Reader) (*StagedContent, error) {
	if err := ValidateDigest(expectedDigest); err != nil {
		return nil, err
	}

	staging, err := os.CreateTemp(filepath.Join(store.root, stagingDirectory), "upload-*")
	if err != nil {
		return nil, fmt.Errorf("staging an upload: %w", err)
	}
	stagedPath := staging.Name()

	// Every failure below removes the partial file. A staged upload that is never committed is
	// removed by its caller; one that never became a StagedContent has no caller to do it.
	discard := func() {
		_ = staging.Close()
		_ = os.Remove(stagedPath)
	}

	// io.Copy, not ReadAll: the whole point of a file-backed store is that a 32 MB upload costs a
	// 32 KB buffer rather than 32 MB of heap, on however many connections arrive at once.
	hash := sha256.New()
	byteCount, err := io.Copy(io.MultiWriter(staging, hash), source)
	if err != nil {
		discard()
		return nil, err
	}

	if digest := hex.EncodeToString(hash.Sum(nil)); digest != expectedDigest {
		discard()
		return nil, fmt.Errorf("%w: bytes hash to %s", ErrDigestMismatch, digest)
	}

	// Durable before it is publishable. Without this the rename below can be visible while the
	// content behind it is still only in the page cache, and a power loss leaves a file whose name
	// promises a digest its bytes no longer hash to — the one thing a content-addressed store may
	// never contain.
	if err := staging.Sync(); err != nil {
		discard()
		return nil, fmt.Errorf("flushing an upload: %w", err)
	}
	if err := staging.Close(); err != nil {
		_ = os.Remove(stagedPath)
		return nil, fmt.Errorf("closing an upload: %w", err)
	}

	return &StagedContent{store: store, path: stagedPath, digest: expectedDigest, byteCount: byteCount}, nil
}

// Publish moves staged bytes into the store, making them readable under their digest.
//
// A no-op when the store already holds the digest, which is the dedup path: the same picture on a
// second device costs the upload and then costs no disk at all. It stays a no-op safely because the
// stored file and the staged one are the same bytes by definition — both hash to the same digest.
func (staged *StagedContent) Publish() error {
	store := staged.store
	finalPath := store.pathFor(staged.digest)

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		return fmt.Errorf("preparing a blob directory: %w", err)
	}

	if _, err := os.Stat(finalPath); err == nil {
		return staged.Discard()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking for a stored blob: %w", err)
	}

	if err := os.Rename(staged.path, finalPath); err != nil {
		return fmt.Errorf("publishing a blob: %w", err)
	}

	// The rename is atomic but not durable: the directory entry can still be lost by a power cut
	// that the file's own contents survive. Syncing the leaf directory is what makes "the file is
	// there" true after a crash, and it is why the caller may insert the row that claims these
	// bytes only after this returns.
	return syncDirectory(filepath.Dir(finalPath))
}

// Discard removes a staged upload that will not be published.
func (staged *StagedContent) Discard() error {
	if err := os.Remove(staged.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("discarding a staged blob: %w", err)
	}
	return nil
}

func (store *FileStore) Exists(digest string) (bool, error) {
	if err := ValidateDigest(digest); err != nil {
		return false, err
	}
	switch _, err := os.Stat(store.pathFor(digest)); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("checking for a stored blob: %w", err)
	}
}

// Open returns the stored bytes.
//
// The concrete type behind the interface is `*os.File`, and that is load-bearing rather than
// incidental: `http.ServeContent` copies through `io.Copy`, which reaches the socket's `ReadFrom`,
// which uses `sendfile(2)` when — and only when — the source is a real file. Wrapping the handle
// in a type of our own would still compile, still serve, and quietly move every byte of every
// picture through user space. Do not wrap it.
func (store *FileStore) Open(digest string) (io.ReadSeekCloser, error) {
	if err := ValidateDigest(digest); err != nil {
		return nil, err
	}

	file, err := os.Open(store.pathFor(digest))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("opening a stored blob: %w", err)
	}
	return file, nil
}

func (store *FileStore) Remove(digest string) error {
	if err := ValidateDigest(digest); err != nil {
		return err
	}
	if err := os.Remove(store.pathFor(digest)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing a stored blob: %w", err)
	}
	return nil
}

// pathFor is safe to build from a request parameter only because [ValidateDigest] has already run:
// 64 characters of lowercase hex cannot contain a separator or a dot, so no digest can name a path
// outside the store however it is escaped in the URL.
func (store *FileStore) pathFor(digest string) string {
	return filepath.Join(store.root, digest[0:2], digest[2:4], digest)
}

// ValidateDigest accepts exactly the form a digest takes everywhere in this server: 64 lowercase
// hex characters.
//
// Uppercase is refused rather than folded. The digest is an identity — it is an attachment's id,
// a `blobRefs` element and a URL path segment — and two spellings of one identity is how the same
// picture becomes two rows, two files and two charges against the same account's storage.
func ValidateDigest(digest string) error {
	if len(digest) != DigestChars {
		return fmt.Errorf("a digest is %d lowercase hex characters, got %d", DigestChars, len(digest))
	}
	for index := 0; index < len(digest); index++ {
		char := digest[index]
		isHex := (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')
		if !isHex {
			return errors.New("a digest is 64 lowercase hex characters")
		}
	}
	return nil
}

// DecodeDigest turns the wire form into the 32 bytes the database column holds.
func DecodeDigest(digest string) ([]byte, error) {
	if err := ValidateDigest(digest); err != nil {
		return nil, err
	}
	return hex.DecodeString(digest)
}

func clearDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening a blob directory: %w", err)
	}
	defer directory.Close()

	if err := directory.Sync(); err != nil {
		return fmt.Errorf("flushing a blob directory: %w", err)
	}
	return nil
}
