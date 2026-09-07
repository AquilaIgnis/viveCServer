package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/changefeed"
	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/internal/syncengine"
)

const changeStreamWriteTimeout = 30 * time.Second

// handleWatchChanges is the server-driven pull channel for one foreground device.
//
// The client supplies only the durable cursor it has already committed. The subscription is
// installed before the first database read, so a concurrent commit is either in that read or leaves
// a broker wakeup behind it. `ready` carries the first real delta page (even when it is empty), and
// every later `changes` event carries another PullResult. Nothing asks the client to make a second
// "did anything change?" request.
func handleWatchChanges(
	pool *pgxpool.Pool,
	changeEvents *changefeed.Broker,
	logger *slog.Logger,
) http.HandlerFunc {
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

		events, cancelSubscription := changeEvents.Subscribe(caller.AccountID, caller.DeviceID)
		defer cancelSubscription()

		firstPage, err := syncengine.PullChanges(
			r.Context(), pool, caller.AccountID, sinceCursor, defaultPullLimit,
		)
		if err != nil {
			writeInternalError(w, logger, "reading the initial streamed change delta failed", err)
			return
		}

		// Presenting `since` proves that everything through it was committed locally. As with the
		// ordinary pull route, the cursor being sent now is not acknowledged until a reconnect names
		// it; a connection dropped after this line can therefore retain purges longer, never lose one.
		if err := store.RecordAcknowledgedSeq(r.Context(), pool, caller.DeviceID, sinceCursor); err != nil {
			logger.Warn("could not record a streamed device cursor",
				"account_id", caller.AccountID, "device_id", caller.DeviceID, "error", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		controller := http.NewResponseController(w)
		cursor, streamed := writeChangePages(
			r, controller, w, pool, logger, caller.AccountID, sinceCursor, "ready", firstPage,
		)
		if !streamed {
			return
		}

		for {
			select {
			case event, open := <-events:
				if !open {
					return
				}
				switch event {
				case changefeed.Changes:
					page, err := syncengine.PullChanges(
						r.Context(), pool, caller.AccountID, cursor, defaultPullLimit,
					)
					if err != nil {
						logger.Error("reading a streamed change delta failed", "error", err)
						return
					}
					// Coalescing and a commit racing the initial read can leave a wakeup after
					// the stream already contains everything. Silence is the correct response.
					if page.Cursor == cursor && len(page.Changes) == 0 && len(page.Purges) == 0 {
						continue
					}
					var written bool
					cursor, written = writeChangePages(
						r, controller, w, pool, logger, caller.AccountID, cursor, "changes", page,
					)
					if !written {
						return
					}
				case changefeed.Revoked:
					_ = writeChangeStreamEvent(controller, w, "revoked", []byte(`{}`))
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	}
}

// writeChangePages sends firstPage and, when necessary, the remaining bounded pages through one
// stream. Reading one page at a time preserves the ordinary pull endpoint's memory and batch
// boundaries even for a first connection to a large account.
func writeChangePages(
	r *http.Request,
	controller *http.ResponseController,
	w http.ResponseWriter,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	accountID string,
	cursor int64,
	firstEvent string,
	firstPage syncengine.PullResult,
) (int64, bool) {
	page := firstPage
	eventName := firstEvent
	for {
		payload, err := json.Marshal(page)
		if err != nil {
			logger.Error("encoding a streamed change delta failed", "error", err)
			return cursor, false
		}
		if !writeChangeStreamEvent(controller, w, eventName, payload) {
			return cursor, false
		}

		previous := cursor
		cursor = page.Cursor
		if !page.HasMore {
			return cursor, true
		}
		if cursor <= previous {
			logger.Error("streamed change delta did not advance", "cursor", cursor)
			return cursor, false
		}

		page, err = syncengine.PullChanges(
			r.Context(), pool, accountID, cursor, defaultPullLimit,
		)
		if err != nil {
			logger.Error("reading another streamed change page failed", "error", err)
			return cursor, false
		}
		eventName = "changes"
	}
}

func writeChangeStreamEvent(
	controller *http.ResponseController,
	w http.ResponseWriter,
	eventName string,
	data []byte,
) bool {
	if err := controller.SetWriteDeadline(time.Now().Add(changeStreamWriteTimeout)); err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", eventName); err != nil {
		return false
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return false
		}
	}
	if _, err := fmt.Fprint(w, "\n"); err != nil {
		return false
	}
	if err := controller.Flush(); err != nil {
		return false
	}
	// The sync listener has a finite WriteTimeout for ordinary responses. Clear it between events:
	// silence is the expected idle state, not a request that has stopped making progress.
	return controller.SetWriteDeadline(time.Time{}) == nil
}
