package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	liveLogHeartbeatInterval = 15 * time.Second
	liveLogWriteTimeout      = 30 * time.Second
)

// handleLiveLogs streams future server log records to an authenticated admin browser. The broker
// has no replay buffer, database table, or file: refreshing the page starts a new empty stream.
func (application adminApplication) handleLiveLogs(w http.ResponseWriter, r *http.Request) {
	account, _, ok := application.requireAdminSession(w, r)
	if !ok {
		return
	}
	if application.liveLogs == nil {
		application.writeAdminError(w, "Live logs are unavailable.", "live log broker is not configured", fmt.Errorf("missing live log broker"))
		return
	}

	events, cancelSubscription := application.liveLogs.Subscribe(account.ID)
	defer cancelSubscription()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	responseController := http.NewResponseController(w)
	if !writeServerSentEvent(responseController, w, "ready", []byte(`{"status":"connected"}`)) {
		return
	}

	heartbeat := time.NewTicker(liveLogHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case event, open := <-events:
			if !open {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				application.logger.Warn("encoding a live log event failed", "error", err)
				continue
			}
			if !writeServerSentEvent(responseController, w, "log", payload) {
				return
			}

		case <-heartbeat.C:
			if err := responseController.SetWriteDeadline(time.Now().Add(liveLogWriteTimeout)); err != nil {
				return
			}
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			if err := responseController.Flush(); err != nil {
				return
			}

		case <-r.Context().Done():
			return
		}
	}
}

func writeServerSentEvent(controller *http.ResponseController, w http.ResponseWriter, eventName string, payload []byte) bool {
	if err := controller.SetWriteDeadline(time.Now().Add(liveLogWriteTimeout)); err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, payload); err != nil {
		return false
	}
	return controller.Flush() == nil
}
