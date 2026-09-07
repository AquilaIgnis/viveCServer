// Package httpapi is the server's entire network surface: routing, middleware, and the handlers
// that implement the sync protocol.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/blob"
	"github.com/AquilaIgnis/viveCServer/internal/changefeed"
	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/livelog"
)

// Options is what the HTTP layer needs to know about the server's configuration.
//
// A narrow struct rather than the whole config, so the database URL and its password are not in
// reach of a request handler that has no business with them.
type Options struct {
	SignupMode config.SignupMode

	// How long a push response stays replayable for the device that sent it.
	BatchReplayWindow time.Duration

	// Blobs is where attachment bytes are kept. A handler that finds it nil serves the sync
	// protocol without the byte routes, which is what the admin surface and any test that does not
	// care about attachments get.
	Blobs blob.Store

	BlobLimits BlobLimits

	// ChangeEvents wakes connected devices after a committed account write. Nil creates a broker
	// private to this handler, which keeps small tests and embedded uses functional.
	ChangeEvents *changefeed.Broker
}

// BlobLimits bounds attachment storage.
//
// One number, and deliberately not two: there is no per-account quota, because this is a personal
// server (syncPlan.md §12 decision 1). The disk is the limit, and a quota would only turn "full
// disk" into "refused earlier" for the one person who owns both.
type BlobLimits struct {
	// MaxBlobBytes caps one attachment. Mirrors NotebookTransferManager's own 32 MB ceiling, so a
	// notebook that imports from a `.vive` bundle also syncs (syncPlan.md §6).
	MaxBlobBytes int64
}

// NewSyncHandler builds the device/sync routing tree with its middleware already wrapped around it.
//
// Routes use Go's method-and-pattern syntax, so a request with the right path and the wrong method
// gets a 405 from the standard library rather than a hand-written check in every handler.
func NewSyncHandler(pool *pgxpool.Pool, logger *slog.Logger, options Options) http.Handler {
	mux := http.NewServeMux()
	authenticated := requireAuthentication(pool, logger)
	changeEvents := options.ChangeEvents
	if changeEvents == nil {
		changeEvents = changefeed.NewBroker()
	}

	mux.HandleFunc("GET /healthz", handleLiveness())
	mux.HandleFunc("GET /readyz", handleReadiness(pool, logger))

	// Unauthenticated by necessity: these are how a caller obtains a credential in the first place.
	mux.HandleFunc("POST /v1/accounts", handleCreateAccount(pool, logger, options.SignupMode))
	mux.HandleFunc("POST /v1/devices", handleRegisterDevice(pool, logger))

	// Everything else requires a device token.
	mux.Handle("GET /v1/devices", authenticated(handleListDevices(pool, logger)))
	mux.Handle("PATCH /v1/devices/{deviceID}", authenticated(handleRenameDevice(pool, logger)))
	mux.Handle("DELETE /v1/devices/{deviceID}", authenticated(handleRevokeDevice(pool, logger, changeEvents)))

	// Attachment bytes. Registered only when there is somewhere to put them, so a build without a
	// blob directory answers 404 rather than 500.
	//
	// The HEAD pattern is registered beside the GET one although Go's mux already routes HEAD to a
	// GET pattern. It matches a strict subset of that pattern's requests, so it takes precedence
	// without conflicting — and it exists because presence is a different question from content:
	// the answer is 204 or 404 and it must never open the file or stream a byte.
	if options.Blobs != nil {
		mux.Handle("HEAD "+blobPathPrefix+"{digest}", authenticated(handleBlobPresence(pool, logger, options.Blobs)))
		mux.Handle("PUT "+blobPathPrefix+"{digest}", authenticated(handleUploadBlob(pool, logger, options.Blobs, options.BlobLimits)))
		mux.Handle("GET "+blobPathPrefix+"{digest}", authenticated(handleDownloadBlob(pool, logger, options.Blobs)))
	}

	mux.Handle("GET /v1/cursor", authenticated(handleReadCursor(pool, logger)))
	mux.Handle("GET /v1/changes", authenticated(handlePullChanges(pool, logger)))
	mux.Handle("GET /v1/changes/watch", authenticated(handleWatchChanges(pool, changeEvents, logger)))
	mux.Handle("POST /v1/changes", authenticated(handlePushChanges(pool, logger, options.BatchReplayWindow, changeEvents)))

	// Compression sits inside the logger so the log records the status the handler chose, and
	// inside recovery so a panic in a compressed response still becomes a 500.
	return withPanicRecovery(withRequestLogging(withCompression(mux, logger), logger), logger)
}

// NewAdminHandler builds the browser-only setup and administration surface. It intentionally does
// not register /v1 routes: publishing port 8080 must never accidentally publish the sync API too.
func NewAdminHandler(
	pool *pgxpool.Pool,
	logger *slog.Logger,
	liveLogs *livelog.Broker,
	changeEvents *changefeed.Broker,
) http.Handler {
	application := adminApplication{
		store:        postgresAdminStore{pool: pool},
		logger:       logger,
		liveLogs:     liveLogs,
		changeEvents: changeEvents,
	}
	return application.handler(pool)
}

// writeJSON is the single place a response body is produced, so every response is encoded the same
// way and the Content-Type can never be forgotten.
func writeJSON(w http.ResponseWriter, logger *slog.Logger, statusCode int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Reaching here means a handler passed something unencodable, which is a bug rather than a
		// bad request. The header is not written yet, so a 500 is still possible.
		logger.Error("encoding a response failed", "error", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if _, err := w.Write(body); err != nil {
		// The status line is already sent, so there is nothing to do but say so. A client that hung
		// up mid-response is ordinary, which is why this is not an error level.
		logger.Debug("writing a response failed", "error", err)
	}
}
