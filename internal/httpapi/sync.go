package httpapi

import (
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/internal/syncengine"
)

// maxPushRequestBytes is the other half of syncPlan.md §7's batch cap: 512 entities or 4 MB,
// whichever comes first. The entity count is checked below; this is what stops one very large
// entity from arriving instead of many small ones.
const maxPushRequestBytes = 4 * 1024 * 1024

// Pull page sizes. The default matches the push cap so that, in the ordinary case, one push turns
// into one pull. The ceiling exists because a client asking for everything at once is how a first
// sync of a large notebook turns into an allocation the server cannot bound (R1).
const (
	defaultPullLimit = 512
	maxPullLimit     = 2048
)

type cursorResponse struct {
	Cursor int64 `json:"cursor"`
}

type pushChangesRequest struct {
	BatchID string            `json:"batchId"`
	Changes []json.RawMessage `json:"changes"`
}

// handleReadCursor answers the idle poll.
//
// This is the request a client makes every 60 seconds with nothing to do (SD6), so it stays a
// single primary-key read: no delta, no join, and nothing written. A client whose cursor matches
// and whose outbox is empty makes this one request and goes back to sleep.
func handleReadCursor(pool *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := authenticatedDeviceOrFail(w, r, logger)
		if !ok {
			return
		}

		cursor, err := store.ReadChangeSeq(r.Context(), pool, caller.AccountID)
		if err != nil {
			writeInternalError(w, logger, "reading a change cursor failed", err)
			return
		}

		writeJSON(w, logger, http.StatusOK, cursorResponse{Cursor: cursor})
	}
}

// handlePullChanges returns the changes an account accumulated after the client's cursor, and the
// ids it erased for good in the same window.
func handlePullChanges(pool *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := authenticatedDeviceOrFail(w, r, logger)
		if !ok {
			return
		}

		sinceCursor, err := readIntegerQuery(r, "since", 0, 0, math.MaxInt64)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
		limit, err := readIntegerQuery(r, "limit", defaultPullLimit, 1, maxPullLimit)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}

		result, err := syncengine.PullChanges(r.Context(), pool, caller.AccountID, sinceCursor, int(limit))
		if err != nil {
			writeInternalError(w, logger, "reading a change delta failed", err)
			return
		}

		// `since` is the cursor the client presented, so it is proof that an earlier response was
		// received and committed locally. The cursor in this response is not proof yet: recording it
		// before writing the body would let a dropped connection acknowledge a purge the client
		// never saw. This is what decides when a purge stops being worth keeping
		// (store.PrunePurges); it is housekeeping and never allowed to fail the pull.
		if err := store.RecordAcknowledgedSeq(r.Context(), pool, caller.DeviceID, sinceCursor); err != nil {
			logger.Warn("could not record a device acknowledged cursor",
				"account_id", caller.AccountID, "device_id", caller.DeviceID, "error", err)
		}

		logger.Debug("changes pulled",
			"account_id", caller.AccountID,
			"device_id", caller.DeviceID,
			"since", sinceCursor,
			"returned", len(result.Changes),
			"purged", len(result.Purges),
			"cursor", result.Cursor,
			"has_more", result.HasMore,
		)
		writeJSON(w, logger, http.StatusOK, result)
	}
}

// handlePushChanges applies a batch of client changes.
func handlePushChanges(pool *pgxpool.Pool, logger *slog.Logger, replayWindow time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := authenticatedDeviceOrFail(w, r, logger)
		if !ok {
			return
		}

		var request pushChangesRequest
		if !decodeJSONRequest(w, r, logger, maxPushRequestBytes, &request) {
			return
		}

		// The idempotency key is required rather than optional. A push whose response was lost is
		// ordinary on a mobile network, and a client that cannot name its retry has no way to be
		// told "this already happened" instead of applying the work twice.
		if !store.IsUUID(request.BatchID) {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, "batchId must be a UUID")
			return
		}
		if len(request.Changes) > syncengine.MaxChangesPerBatch {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest,
				"a batch holds at most "+strconv.Itoa(syncengine.MaxChangesPerBatch)+" changes")
			return
		}

		response, err := syncengine.ApplyChangeBatch(r.Context(), pool, syncengine.PushRequest{
			Caller:       caller,
			BatchID:      request.BatchID,
			Changes:      request.Changes,
			ReplayWindow: replayWindow,
		})
		if err != nil {
			writeInternalError(w, logger, "applying a change batch failed", err)
			return
		}

		// Decoded back only to log counts. The response itself is passed through as the bytes the
		// engine produced, because those are the bytes a retry of this batch will replay.
		var summary syncengine.PushResult
		if err := json.Unmarshal(response, &summary); err == nil {
			logger.Info("change batch applied",
				"account_id", caller.AccountID,
				"device_id", caller.DeviceID,
				"batch_id", request.BatchID,
				"applied", len(summary.Applied),
				"rejected", len(summary.Rejected),
				"cursor", summary.Cursor,
			)
		}

		writeJSON(w, logger, http.StatusOK, response)
	}
}

// readIntegerQuery parses one bounded integer query parameter.
//
// An unparsable or out-of-range value is refused rather than clamped. A client that asked for
// `limit=100000` and silently received 2048 would page through a notebook believing it had seen all
// of it.
func readIntegerQuery(r *http.Request, name string, fallback int64, minimum int64, maximum int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, &queryParameterError{name: name, detail: "must be an integer"}
	}
	if value < minimum || value > maximum {
		return 0, &queryParameterError{
			name:   name,
			detail: "must be between " + strconv.FormatInt(minimum, 10) + " and " + strconv.FormatInt(maximum, 10),
		}
	}
	return value, nil
}

type queryParameterError struct {
	name   string
	detail string
}

func (err *queryParameterError) Error() string {
	return err.name + " " + err.detail
}
