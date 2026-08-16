package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// maxAuthRequestBytes caps the credential endpoints. They carry an email, a password and two short
// strings; anything approaching this is either a mistake or an attempt to make the server allocate.
const maxAuthRequestBytes = 64 * 1024

// decodeJSONRequest reads a JSON body into target, having first bounded it.
//
// Unknown fields are deliberately *not* rejected. A newer client must be able to send a field this
// build has never heard of and still be understood — the same forward compatibility the document
// model gets from `ignoreUnknownKeys`, and the same reason the sync tables carry `extra jsonb`
// (syncPlan.md SD5). Rejecting unknown fields would make every client change a lockstep deploy.
//
// It reports whether decoding succeeded, having already written the response if it did not.
func decodeJSONRequest(w http.ResponseWriter, r *http.Request, logger *slog.Logger, maxBytes int64, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, logger, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "request body is too large")
			return false
		}

		// The decoder's message describes the caller's own JSON, so it is safe to return and is the
		// single most useful thing to tell someone whose client is malformed.
		writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, "request body is not valid JSON: "+err.Error())
		return false
	}

	return true
}
