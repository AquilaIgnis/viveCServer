package httpapi

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func compressionTestHandler(body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			received, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(received)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

// TestASmallResponseIsNotCompressed guards the cursor catch-up. It answers twelve bytes, and gzip's
// own framing is larger than that.
func TestASmallResponseIsNotCompressed(t *testing.T) {
	handler := withCompression(compressionTestHandler([]byte(`{"cursor":4}`)), discardLogger())

	request := httptest.NewRequest(http.MethodGet, "/v1/cursor", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Header().Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding = %q, want a small response sent as it is", response.Header().Get("Content-Encoding"))
	}
	if response.Body.String() != `{"cursor":4}` {
		t.Fatalf("body = %q", response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatal("Vary must be announced whether or not the response was compressed")
	}
}

func TestALargeResponseIsCompressedAndDecodesToTheSameBytes(t *testing.T) {
	// Compressible on purpose: a delta is JSON with the same keys on every row.
	original := []byte(strings.Repeat(`{"kind":"page","title":"Monday standup"},`, 400))
	handler := withCompression(compressionTestHandler(original), discardLogger())

	request := httptest.NewRequest(http.MethodGet, "/v1/changes", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", response.Header().Get("Content-Encoding"))
	}
	if response.Body.Len() >= len(original) {
		t.Fatalf("compressed to %d bytes from %d, which is no saving", response.Body.Len(), len(original))
	}

	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatalf("response is not gzip: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the compressed body: %v", err)
	}
	if !bytes.Equal(decoded, original) {
		t.Fatal("the decompressed body is not what the handler wrote")
	}
}

func TestAClientThatCannotDecompressGetsPlainBytes(t *testing.T) {
	original := []byte(strings.Repeat("a", 4000))
	handler := withCompression(compressionTestHandler(original), discardLogger())

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/changes", nil))

	if response.Header().Get("Content-Encoding") != "" || response.Body.Len() != len(original) {
		t.Fatalf("encoding = %q, %d bytes: a client that did not ask must not be sent gzip",
			response.Header().Get("Content-Encoding"), response.Body.Len())
	}
}

// TestACompressedRequestIsDecodedForTheHandler is the push half of §7.
func TestACompressedRequestIsDecodedForTheHandler(t *testing.T) {
	payload := []byte(`{"batchId":"11111111-1111-1111-1111-111111111111","changes":[]}`)
	var compressed bytes.Buffer
	encoder := gzip.NewWriter(&compressed)
	if _, err := encoder.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}

	handler := withCompression(compressionTestHandler(nil), discardLogger())
	request := httptest.NewRequest(http.MethodPost, "/v1/changes", bytes.NewReader(compressed.Bytes()))
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != string(payload) {
		t.Fatalf("status %d, body %q: the handler must see the decoded body", response.Code, response.Body.String())
	}
}

func TestARequestThatClaimsGzipAndIsNotIsRefused(t *testing.T) {
	handler := withCompression(compressionTestHandler(nil), discardLogger())
	request := httptest.NewRequest(http.MethodPost, "/v1/changes", strings.NewReader("not gzip at all"))
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

// TestACompressedRequestIsStillBoundedByTheHandlersLimit is the trap this whole middleware has to
// avoid: a body limited before decompression is not limited at all, so a few kilobytes of zeroes
// could become gigabytes in memory.
func TestACompressedRequestIsStillBoundedByTheHandlersLimit(t *testing.T) {
	// Valid JSON all the way down, so the decoder is stopped by the size limit rather than by the
	// first byte it cannot parse. That distinction is the test: a bomb made of NUL bytes is refused
	// for being malformed and proves nothing about the cap.
	var compressed bytes.Buffer
	encoder := gzip.NewWriter(&compressed)
	if _, err := encoder.Write([]byte(`{"a":"`)); err != nil {
		t.Fatal(err)
	}
	for range 64 {
		if _, err := encoder.Write(bytes.Repeat([]byte("a"), 1<<20)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := encoder.Write([]byte(`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if compressed.Len() > 1<<20 {
		t.Fatalf("the bomb is %d bytes compressed, which does not test what it is meant to", compressed.Len())
	}

	// The handler applies the same cap the real push path does, on the body it is handed.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var target map[string]any
		if !decodeJSONRequest(w, r, discardLogger(), maxPushRequestBytes, &target) {
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/changes", bytes.NewReader(compressed.Bytes()))
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	withCompression(inner, discardLogger()).ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: 64 MiB of zeroes must not be decoded into memory", response.Code)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
