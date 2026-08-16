package httpapi

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecordingWriter remembers what a handler sent, so the logging middleware can report it.
//
// Unwrap is what keeps this transparent: http.ResponseController walks it to reach the real
// writer's Flush and Hijack, so wrapping every response here does not quietly disable streaming for
// a handler added later.
type statusRecordingWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
}

func (w *statusRecordingWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *statusRecordingWriter) Write(body []byte) (int, error) {
	// A handler that writes without calling WriteHeader has implicitly sent a 200.
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	written, err := w.ResponseWriter.Write(body)
	w.bytesWritten += written
	return written, err
}

func (w *statusRecordingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// withRequestLogging records one line per request.
//
// Health checks log at debug rather than info on purpose. Compose probes readiness every couple of
// seconds and every client polls on a 60 s cycle (syncPlan.md SD6), so these two paths would
// otherwise be nearly all of the log volume and would bury the requests an operator actually wants
// to see. A failing probe still reports itself from the handler.
func withRequestLogging(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		recorder := &statusRecordingWriter{ResponseWriter: w}

		next.ServeHTTP(recorder, r)

		if recorder.statusCode == 0 {
			recorder.statusCode = http.StatusOK
		}

		level := slog.LevelInfo
		if isProbePath(r.URL.Path) && recorder.statusCode < http.StatusBadRequest {
			level = slog.LevelDebug
		}

		logger.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.statusCode,
			"bytes", recorder.bytesWritten,
			"duration_ms", time.Since(startedAt).Milliseconds(),
		)
	})
}

func isProbePath(requestPath string) bool {
	return requestPath == "/healthz" || requestPath == "/readyz"
}

// withPanicRecovery keeps one bad request from taking the process down.
//
// The stack is logged rather than returned: a panic message can carry a query fragment or a value
// from the request, and the client is the least appropriate place to send it.
func withPanicRecovery(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			panicValue := recover()
			if panicValue == nil {
				return
			}

			// http.ErrAbortHandler is the standard library's documented way for a handler to
			// abandon a response on purpose. Swallowing it here would turn a deliberate abort into
			// a logged crash on every dropped connection.
			if panicValue == http.ErrAbortHandler {
				panic(panicValue)
			}

			logger.Error("handler panicked",
				"method", r.Method,
				"path", r.URL.Path,
				"panic", panicValue,
				"stack", string(debug.Stack()),
			)

			// If the handler already wrote a status this is a no-op with a warning in the standard
			// library's own log; there is no correct way to un-send a response.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal"}`))
		}()

		next.ServeHTTP(w, r)
	})
}
