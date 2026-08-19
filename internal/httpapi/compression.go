package httpapi

import (
	"compress/gzip"
	"log/slog"
	"net/http"
	"strings"
)

// gzipMinimumBytes is the response size below which compressing is a pessimisation.
//
// The idle poll answers `{"cursor":4}` in twelve bytes (SD6), and gzip's header, trailer and block
// framing alone are more than that — compressing it would make the most frequent request this
// server has bigger and slower. A response is therefore buffered until it passes roughly one
// ethernet payload, and only then does the encoder start.
const gzipMinimumBytes = 1400

// maxCompressedRequestBytes bounds a compressed push body.
//
// The cap that matters is the one on the *decoded* stream, which each handler applies itself — a
// four-kilobyte body that decompresses to four gigabytes is the whole trick, and a limit read
// before the decoder is no limit at all. This one exists so that a request which is nothing but
// compressed padding is refused before it is decompressed rather than after.
const maxCompressedRequestBytes = 4 * 1024 * 1024

// withCompression adds gzip in both directions (syncPlan.md §7).
//
// Deliberately wrapped around the sync handler alone. The admin surface streams its live log as
// `text/event-stream`, and a compressor between that and the browser buffers the very thing whose
// point is arriving immediately.
func withCompression(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Attachment bytes go past untouched, for three separate reasons that happen to agree.
		// They are already compressed — the app re-encodes every import to JPEG — so gzip would
		// spend CPU on both ends to add a little. They are served with Range support, and a
		// compressor between ServeContent and the socket makes `Content-Range` describe a body the
		// client did not receive. And the buffering writer below does not implement io.ReaderFrom,
		// so it would quietly cost the `sendfile(2)` path that keeps a 32 MB download out of this
		// process's memory entirely.
		if strings.HasPrefix(r.URL.Path, blobPathPrefix) {
			next.ServeHTTP(w, r)
			return
		}

		if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			// MaxBytesReader first, so the bound applies to what arrives rather than to what it
			// becomes. The handler's own limit then bounds the decoded stream.
			r.Body = http.MaxBytesReader(w, r.Body, maxCompressedRequestBytes)
			decoded, err := gzip.NewReader(r.Body)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest, codeInvalidRequest,
					"request body is not valid gzip")
				return
			}
			defer decoded.Close()
			r.Body = decoded
			// Or a handler downstream reads the header and decides the body is still compressed.
			r.Header.Del("Content-Encoding")
		}

		// Announced whether or not this response is compressed: a cache that saw one small answer
		// uncompressed must not serve it to a client that cannot read the compressed one.
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}

		compressing := &gzipResponseWriter{ResponseWriter: w}
		defer compressing.finish()
		next.ServeHTTP(compressing, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	for _, encoding := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, _, _ := strings.Cut(encoding, ";")
		if strings.EqualFold(strings.TrimSpace(name), "gzip") {
			return true
		}
	}
	return false
}

// gzipResponseWriter compresses a response, but only once it is worth compressing.
//
// It holds the first [gzipMinimumBytes] rather than deciding up front, because a handler that
// writes JSON knows its status before it knows its size, and `Content-Length` cannot be right for
// both a buffered and a compressed body. Nothing reaches the client until the size is known one way
// or the other, which is what makes both headers honest.
type gzipResponseWriter struct {
	http.ResponseWriter

	status    int
	buffered  []byte
	encoder   *gzip.Writer
	committed bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *gzipResponseWriter) Write(payload []byte) (int, error) {
	if w.encoder != nil {
		return w.encoder.Write(payload)
	}

	w.buffered = append(w.buffered, payload...)
	if len(w.buffered) < gzipMinimumBytes {
		return len(payload), nil
	}

	w.commitCompressed()
	buffered := w.buffered
	w.buffered = nil
	if _, err := w.encoder.Write(buffered); err != nil {
		return 0, err
	}
	return len(payload), nil
}

// Flush sends what is held. A handler that flushes is saying this much is ready now, which is worth
// more than the compression the rest of the buffer might have earned.
func (w *gzipResponseWriter) Flush() {
	if w.encoder == nil && len(w.buffered) > 0 {
		w.commitCompressed()
		buffered := w.buffered
		w.buffered = nil
		_, _ = w.encoder.Write(buffered)
	}
	if w.encoder != nil {
		_ = w.encoder.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *gzipResponseWriter) commitCompressed() {
	w.committed = true
	header := w.ResponseWriter.Header()
	header.Set("Content-Encoding", "gzip")
	// Written before compression and therefore describing the wrong body. Removing it lets the
	// standard library size the response, or chunk it.
	header.Del("Content-Length")
	w.encoder = gzip.NewWriter(w.ResponseWriter)
	w.ResponseWriter.WriteHeader(w.statusOrOK())
}

// finish writes whatever the handler left behind: a small response goes out uncompressed, and a
// large one has its encoder closed so the gzip trailer is written.
func (w *gzipResponseWriter) finish() {
	if w.encoder != nil {
		_ = w.encoder.Close()
		return
	}
	if w.committed {
		return
	}
	w.committed = true
	w.ResponseWriter.WriteHeader(w.statusOrOK())
	if len(w.buffered) > 0 {
		_, _ = w.ResponseWriter.Write(w.buffered)
	}
}

func (w *gzipResponseWriter) statusOrOK() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
