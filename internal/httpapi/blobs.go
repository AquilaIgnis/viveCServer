package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/blob"
	"github.com/AquilaIgnis/viveCServer/internal/store"
)

// blobPathPrefix is where the byte endpoints live. Named because the compression middleware has to
// recognise them: see withCompression.
const blobPathPrefix = "/v1/blobs/"

// blobTransferDeadline replaces the server-wide read and write timeouts for the length of one
// transfer.
//
// The 60 s defaults in cmd/vivecserver are sized for a sync request, which is a few hundred
// kilobytes of JSON. They are the correct answer there and the wrong one here: a 32 MB picture over
// a slow mobile connection is minutes of legitimate transfer, and a client that is uploading
// steadily at 200 kbit/s would be cut off at 60 seconds every single time, for ever, with no way to
// make progress. Extended per request rather than raised globally, so the cheap defence against a
// connection that opens and then says nothing still applies to every other route.
const blobTransferDeadline = 30 * time.Minute

// blobCacheControl is what makes a content-addressed URL worth having.
//
// `immutable` is exactly true here and almost nowhere else (RFC 8246): the bytes under a digest
// cannot change, because a different byte would be a different digest. A client that has fetched a
// picture once never has to revalidate it. `private` because the URL is account-scoped and
// bearer-authenticated — a shared cache holding this response would be holding one account's
// picture where another account's request could reach it.
const blobCacheControl = "private, max-age=31536000, immutable"

// handleBlobPresence answers whether this account already has these bytes.
//
// The cheap half of deduplication, and the reason an upload of a picture the server already holds
// costs one round trip instead of 32 MB. The client asks before every upload; the app inserts the
// same screenshot on nine pages and uploads it once.
//
// Presence means *both* the row and the file. A row whose bytes are missing — a restored database
// without its blob directory, an operator who cleared a volume — must answer 404, or the client
// skips the upload that would repair it and the picture is lost for good.
func handleBlobPresence(pool *pgxpool.Pool, logger *slog.Logger, blobs blob.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, digest, ok := blobRequest(w, r, logger)
		if !ok {
			return
		}

		held, _, err := blobHeldByAccount(r, pool, blobs, caller.AccountID, digest)
		if err != nil {
			writeInternalError(w, logger, "checking for a stored blob failed", err)
			return
		}
		if !held {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Header().Set("ETag", `"`+digest+`"`)
		w.Header().Set("Cache-Control", blobCacheControl)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleUploadBlob stores attachment bytes under the digest in the URL.
//
// Three properties, in the order they matter:
//
//   - **The digest is verified, never trusted.** The bytes are hashed as they stream in and refused
//     if they do not match the name they were sent under. A content-addressed store that accepts an
//     unverified name is not content-addressed; it is a filesystem with a confusing API, and the
//     first truncated upload puts a file in it that every device will fetch and fail to decode.
//   - **Nothing is buffered.** The body streams to a staging file, so a 32 MB upload costs a 32 KB
//     buffer rather than 32 MB of heap on however many connections arrive at once.
//   - **The bytes are durable before the row that claims them exists.** Publishing fsyncs the file
//     and then its directory; only then is the row written. The reverse order leaves a row pointing
//     at bytes a power cut ate, which is the one inconsistency this design refuses to allow.
func handleUploadBlob(
	pool *pgxpool.Pool,
	logger *slog.Logger,
	blobs blob.Store,
	limits BlobLimits,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, digest, ok := blobRequest(w, r, logger)
		if !ok {
			return
		}

		// Answered before the body is read. With `Expect: 100-continue` — which the standard
		// library honours by withholding the continuation until a handler reads the body — a
		// client that already uploaded this picture from another device sends the header and never
		// sends the megabytes. That is the whole of upload-side dedup, and it costs one index probe.
		held, _, err := blobHeldByAccount(r, pool, blobs, caller.AccountID, digest)
		if err != nil {
			writeInternalError(w, logger, "checking for a stored blob failed", err)
			return
		}
		if held {
			w.Header().Set("ETag", `"`+digest+`"`)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.ContentLength > limits.MaxBlobBytes {
			// Refused on the declared length before a byte is read. MaxBytesReader below is what
			// actually enforces the cap; this only saves an upload that was never going to be kept.
			writeError(w, logger, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				"an attachment may not exceed "+strconv.FormatInt(limits.MaxBlobBytes, 10)+" bytes")
			return
		}

		// The transfer may legitimately take longer than an ordinary request. Both directions:
		// the read deadline covers the upload, and the write deadline covers the answer to a client
		// whose connection stalls after sending.
		deadline := time.Now().Add(blobTransferDeadline)
		responseController := http.NewResponseController(w)
		_ = responseController.SetReadDeadline(deadline)
		_ = responseController.SetWriteDeadline(deadline)

		staged, err := blobs.Stage(digest, http.MaxBytesReader(w, r.Body, limits.MaxBlobBytes))
		switch {
		case errors.Is(err, blob.ErrDigestMismatch):
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest,
				"the uploaded bytes do not hash to the digest in the URL")
			return
		case isRequestTooLarge(err):
			writeError(w, logger, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				"an attachment may not exceed "+strconv.FormatInt(limits.MaxBlobBytes, 10)+" bytes")
			return
		case err != nil:
			writeInternalError(w, logger, "receiving an attachment failed", err)
			return
		}

		stored, err := publishBlob(r, pool, blobs, caller, digest, staged)
		if err != nil {
			writeInternalError(w, logger, "storing an attachment failed", err)
			return
		}

		logger.Info("attachment stored",
			"account_id", caller.AccountID,
			"device_id", caller.DeviceID,
			"digest", digest,
			"bytes", staged.ByteCount(),
			"deduplicated", !stored,
		)

		w.Header().Set("ETag", `"`+digest+`"`)
		if !stored {
			// Another device of this account got there first, between the check above and here.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}
}

// publishBlob moves staged bytes into the store and records the account's claim on them.
//
// Both steps happen under the per-digest lock, which is what keeps a concurrent sweep from
// unlinking the file between them. See store.BlobLock for the interleaving it prevents.
func publishBlob(
	r *http.Request,
	pool *pgxpool.Pool,
	blobs blob.Store,
	caller auth.AuthenticatedDevice,
	digest string,
	staged *blob.StagedContent,
) (stored bool, err error) {
	lock, err := store.AcquireBlobLock(r.Context(), pool, digest)
	if err != nil {
		_ = staged.Discard()
		return false, err
	}
	defer lock.Release(r.Context())

	if err := staged.Publish(); err != nil {
		_ = staged.Discard()
		return false, err
	}

	stored, err = store.RecordStoredBlob(r.Context(), pool, caller.AccountID, digest, staged.ByteCount())
	if err != nil {
		// The row was not written, so nothing claims these bytes. Remove them unless another
		// account's row does — the file on disk is shared by every account holding the same
		// picture, and the check runs under the same lock that keeps that answer from changing.
		if heldElsewhere, checkErr := store.AnyAccountHoldsBlob(r.Context(), pool, digest); checkErr == nil && !heldElsewhere {
			_ = blobs.Remove(digest)
		}
		return false, err
	}
	return stored, nil
}

// handleDownloadBlob streams stored bytes back.
//
// http.ServeContent rather than io.Copy, for three things this would otherwise have to implement:
// Range requests, so a 32 MB picture interrupted at 90% resumes instead of restarting;
// If-None-Match against the ETag, so a client that already has it pays a 304; and If-Range, so a
// resumed transfer is refused if the content somehow differs. The bytes themselves go out through
// the socket's own ReadFrom, which on Linux is `sendfile(2)` — the picture never enters this
// process's address space at all. That is only true while nothing wraps the ResponseWriter in a
// type without ReadFrom, which is why the compression middleware steps aside for this route and why
// statusRecordingWriter forwards ReadFrom.
func handleDownloadBlob(pool *pgxpool.Pool, logger *slog.Logger, blobs blob.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, digest, ok := blobRequest(w, r, logger)
		if !ok {
			return
		}

		presence, err := store.SelectStoredBlobs(r.Context(), pool, caller.AccountID, []string{digest})
		if err != nil {
			writeInternalError(w, logger, "reading an attachment failed", err)
			return
		}
		if _, held := presence[digest]; !held {
			// Held by another account is the same answer as held by nobody. Knowing a digest is not
			// holding a blob: the row is the capability, and an account gets one only by uploading
			// the bytes, which it can only do if it already had them.
			writeError(w, logger, http.StatusNotFound, codeNotFound, "no such attachment")
			return
		}

		content, err := blobs.Open(digest)
		if errors.Is(err, blob.ErrNotFound) {
			// A row without its bytes. Not a client error in any useful sense, so it is logged as
			// the storage fault it is; the client's repair is to upload the picture again, which
			// the 404 is what invites.
			logger.Error("an attachment row has no stored bytes",
				"account_id", caller.AccountID, "digest", digest)
			writeError(w, logger, http.StatusNotFound, codeNotFound, "no such attachment")
			return
		}
		if err != nil {
			writeInternalError(w, logger, "opening an attachment failed", err)
			return
		}
		defer content.Close()

		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(blobTransferDeadline))

		w.Header().Set("ETag", `"`+digest+`"`)
		w.Header().Set("Cache-Control", blobCacheControl)

		// Set explicitly so ServeContent does not sniff. Sniffing would report what the bytes look
		// like, and bytes that look like HTML served from an authenticated origin are a stored
		// cross-site scripting hazard the moment anything opens one of these URLs in a browser. The
		// app knows the real media type from the attachment row it pulled; nothing here needs to
		// guess it, so nothing here does.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "attachment")

		// The zero time omits Last-Modified, which would be meaningless: content addressed by its
		// own hash has no modification time, and the ETag answers every conditional request.
		http.ServeContent(w, r, "", time.Time{}, content)
	}
}

// blobRequest resolves the caller and the digest both byte routes need.
func blobRequest(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (auth.AuthenticatedDevice, string, bool) {
	caller, ok := authenticatedDeviceOrFail(w, r, logger)
	if !ok {
		return auth.AuthenticatedDevice{}, "", false
	}

	digest := r.PathValue("digest")
	if err := blob.ValidateDigest(digest); err != nil {
		writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, err.Error())
		return auth.AuthenticatedDevice{}, "", false
	}
	return caller, digest, true
}

// blobHeldByAccount reports whether the account has a row for the digest *and* the store has the
// bytes. Both, always: see handleBlobPresence.
func blobHeldByAccount(
	r *http.Request,
	pool *pgxpool.Pool,
	blobs blob.Store,
	accountID string,
	digest string,
) (held bool, byteCount int64, err error) {
	presence, err := store.SelectStoredBlobs(r.Context(), pool, accountID, []string{digest})
	if err != nil {
		return false, 0, err
	}
	stored, hasRow := presence[digest]
	if !hasRow {
		return false, 0, nil
	}

	hasBytes, err := blobs.Exists(digest)
	if err != nil {
		return false, 0, err
	}
	return hasBytes, stored.ByteCount, nil
}

// isRequestTooLarge recognises the error MaxBytesReader produces, which arrives wrapped in whatever
// the reader that hit it was doing.
func isRequestTooLarge(err error) bool {
	var tooLarge *http.MaxBytesError
	return errors.As(err, &tooLarge)
}
