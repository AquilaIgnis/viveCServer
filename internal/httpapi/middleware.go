package httpapi

import (
	"io"
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

// ReadFrom keeps the zero-copy path reachable through this wrapper.
//
// `io.Copy` chooses `sendfile(2)` only when its destination implements io.ReaderFrom, and a wrapper
// that does not implement it silently removes that choice: an attachment download would still be
// correct, and every byte of every picture would be copied through this process instead of going
// from the page cache to the socket. Wrapping a response is exactly the kind of change that would
// cost that without anything failing, so the forwarding lives here rather than being remembered at
// each call site.
func (w *statusRecordingWriter) ReadFrom(source io.Reader) (int64, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}

	readerFrom, canReadFrom := w.ResponseWriter.(io.ReaderFrom)
	if !canReadFrom {
		written, err := io.Copy(w.ResponseWriter, source)
		w.bytesWritten += int(written)
		return written, err
	}

	written, err := readerFrom.ReadFrom(source)
	w.bytesWritten += int(written)
	return written, err
}

// withRequestLogging records one line per request.
//
// Health checks log at debug rather than info on purpose. Compose probes readiness every couple of
// seconds, so they would otherwise be nearly all of the idle log volume and bury the requests an
// operator actually wants to see. A failing probe still reports itself from the handler.
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
