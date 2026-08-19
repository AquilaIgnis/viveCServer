package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("opening a store: %v", err)
	}
	return store
}

// TestStagedContentIsInvisibleUntilPublished: staging and publishing are separate because the
// publish has to happen under the same lock as the row that will claim the bytes, and the upload
// itself must not hold that lock.
func TestStagedContentIsInvisibleUntilPublished(t *testing.T) {
	store := newTestStore(t)
	content := []byte("a picture, more or less")
	digest := digestOf(content)

	staged, err := store.Stage(digest, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if staged.ByteCount() != int64(len(content)) {
		t.Fatalf("staged %d bytes, want %d", staged.ByteCount(), len(content))
	}

	if exists, _ := store.Exists(digest); exists {
		t.Fatal("a staged upload is already readable, so a crash between staging and the row that claims it would leave content nothing tracks")
	}

	if err := staged.Publish(); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if exists, _ := store.Exists(digest); !exists {
		t.Fatal("a published upload is not readable")
	}

	// The fan-out layout of §8, asserted because it is what keeps one directory from holding every
	// picture on the server.
	expectedPath := filepath.Join(store.Root(), digest[0:2], digest[2:4], digest)
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("the blob is not at <aa>/<bb>/<digest>: %v", err)
	}

	reader, err := store.Open(digest)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer reader.Close()

	read, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !bytes.Equal(read, content) {
		t.Fatalf("read %q, want %q", read, content)
	}
}

// TestOpenReturnsAFileSoDownloadsCanUseSendfile guards a property that is invisible when it breaks.
//
// http.ServeContent copies through io.Copy, which reaches the socket's ReadFrom, which uses
// sendfile(2) only when the source's concrete type is *os.File. Wrapping the handle in a type of
// our own would still compile and still serve every byte correctly — it would just move all of them
// through user space, and nothing would fail to say so.
func TestOpenReturnsAFileSoDownloadsCanUseSendfile(t *testing.T) {
	store := newTestStore(t)
	content := []byte("bytes that should reach the socket without being copied through this process")
	digest := digestOf(content)

	staged, err := store.Stage(digest, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if err := staged.Publish(); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	reader, err := store.Open(digest)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer reader.Close()

	if _, isFile := reader.(*os.File); !isFile {
		t.Fatalf("Open returned %T, want *os.File: anything else silently costs the sendfile path", reader)
	}
}

// TestContentThatDoesNotMatchItsDigestIsRefusedAndLeavesNothing.
func TestContentThatDoesNotMatchItsDigestIsRefusedAndLeavesNothing(t *testing.T) {
	store := newTestStore(t)
	content := []byte("the bytes that were promised")
	wrongDigest := digestOf([]byte("some other bytes entirely"))

	_, err := store.Stage(wrongDigest, bytes.NewReader(content))
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("staging mismatched content: %v, want ErrDigestMismatch", err)
	}

	staging := filepath.Join(store.Root(), stagingDirectory)
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("reading the staging directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused upload left %d files in staging", len(entries))
	}
}

// TestPublishingContentTheStoreAlreadyHasIsANoOp is the dedup path: the same picture from a second
// device costs the upload and then costs no disk at all.
func TestPublishingContentTheStoreAlreadyHasIsANoOp(t *testing.T) {
	store := newTestStore(t)
	content := []byte("one picture, two devices")
	digest := digestOf(content)

	for attempt := range 2 {
		staged, err := store.Stage(digest, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("staging %d: %v", attempt, err)
		}
		if err := staged.Publish(); err != nil {
			t.Fatalf("publishing %d: %v", attempt, err)
		}
	}

	if stored := countStoredFiles(t, store); stored != 1 {
		t.Fatalf("the store holds %d files, want one", stored)
	}

	reader, err := store.Open(digest)
	if err != nil {
		t.Fatalf("opening after a second publish: %v", err)
	}
	defer reader.Close()
	read, _ := io.ReadAll(reader)
	if !bytes.Equal(read, content) {
		t.Fatal("the second publish damaged the stored content")
	}
}

// TestDiscardingAStagedUploadRemovesIt, which is what a failed transaction downstream does.
func TestDiscardingAStagedUploadRemovesIt(t *testing.T) {
	store := newTestStore(t)
	content := []byte("an upload that will not be kept")

	staged, err := store.Stage(digestOf(content), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("staging: %v", err)
	}
	if err := staged.Discard(); err != nil {
		t.Fatalf("discarding: %v", err)
	}
	// Twice, because a discard after a publish that already consumed the staged file is an ordinary
	// path rather than an error.
	if err := staged.Discard(); err != nil {
		t.Fatalf("discarding twice: %v", err)
	}
}

// TestInterruptedUploadsAreClearedAtStartup: a crash mid-upload must not leave a partial file
// accumulating for the life of the server.
func TestInterruptedUploadsAreClearedAtStartup(t *testing.T) {
	root := t.TempDir()
	store, err := OpenFileStore(root)
	if err != nil {
		t.Fatalf("opening a store: %v", err)
	}

	content := []byte("an upload interrupted by a crash")
	if _, err := store.Stage(digestOf(content), bytes.NewReader(content)); err != nil {
		t.Fatalf("staging: %v", err)
	}

	staging := filepath.Join(root, stagingDirectory)
	before, _ := os.ReadDir(staging)
	if len(before) != 1 {
		t.Fatalf("staging holds %d files before the restart, want the interrupted one", len(before))
	}

	if _, err := OpenFileStore(root); err != nil {
		t.Fatalf("reopening a store: %v", err)
	}

	after, _ := os.ReadDir(staging)
	if len(after) != 0 {
		t.Fatalf("staging still holds %d files after a restart", len(after))
	}
}

// TestRemoveIsIdempotent: the caller is a sweeper, and a file already gone is the outcome it wanted.
func TestRemoveIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	content := []byte("a picture nothing points at any more")
	digest := digestOf(content)

	staged, _ := store.Stage(digest, bytes.NewReader(content))
	if err := staged.Publish(); err != nil {
		t.Fatalf("publishing: %v", err)
	}
	if err := store.Remove(digest); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if err := store.Remove(digest); err != nil {
		t.Fatalf("removing what is already gone: %v", err)
	}
	if _, err := store.Open(digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("opening a removed blob: %v, want ErrNotFound", err)
	}
}

// TestOnlyALowercaseHexDigestIsAccepted is what makes it safe to build a path out of a URL
// parameter: 64 characters of lowercase hex cannot contain a separator or a dot, so no request can
// name a file outside the store.
func TestOnlyALowercaseHexDigestIsAccepted(t *testing.T) {
	valid := digestOf([]byte("anything"))
	if err := ValidateDigest(valid); err != nil {
		t.Fatalf("an ordinary digest was refused: %v", err)
	}

	refused := map[string]string{
		"empty":            "",
		"too short":        valid[:63],
		"too long":         valid + "a",
		"uppercase":        strings.ToUpper(valid),
		"not hex":          strings.Repeat("z", 64),
		"traversal":        "../../../etc/passwd",
		"padded traversal": strings.Repeat(".", 64),
		"separators":       strings.Repeat("a/", 32),
	}
	for name, digest := range refused {
		if err := ValidateDigest(digest); err == nil {
			t.Errorf("%s was accepted as a digest", name)
		}
	}

	store := newTestStore(t)
	if _, err := store.Open("../../../etc/passwd"); err == nil {
		t.Fatal("a traversal path was opened")
	}
}

// TestAnUnwritableDirectoryExplainsItself locks the message rather than only the failure.
//
// A self-hoster meeting this reads one line and either understands it or opens an issue. The
// failure it describes — a bind-mounted host directory the Docker daemon created as root, under a
// server that runs as nonroot — is the single most likely way this server refuses to start, and
// "permission denied" on its own points at nothing.
func TestAnUnwritableDirectoryExplainsItself(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, which bypasses the permission this test needs to be denied")
	}

	root := filepath.Join(t.TempDir(), "blobs")
	if err := os.Mkdir(root, 0o500); err != nil {
		t.Fatalf("preparing an unwritable directory: %v", err)
	}

	_, err := OpenFileStore(root)
	if err == nil {
		t.Fatal("opening a store in an unwritable directory succeeded")
	}
	for _, expected := range []string{root, "not writable", "chown", "Docker"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("the failure %q does not mention %q", err, expected)
		}
	}
}

func countStoredFiles(t *testing.T, store *FileStore) int {
	t.Helper()

	stored := 0
	err := filepath.WalkDir(store.Root(), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == stagingDirectory {
				return filepath.SkipDir
			}
			return nil
		}
		stored++
		return nil
	})
	if err != nil {
		t.Fatalf("walking the store: %v", err)
	}
	return stored
}
