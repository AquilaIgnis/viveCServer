package httpapi

import (
	"log/slog"
	"net/http"
)

// Error codes are a closed set, so a client can branch on them without parsing prose. The message
// beside a code is for a human reading a log, and is never the thing a client should switch on.
const (
	codeInvalidRequest     = "invalid_request"
	codeInvalidCredentials = "invalid_credentials"
	codeSignupClosed       = "signup_closed"
	codeEmailTaken         = "email_taken"
	codeUnauthenticated    = "unauthenticated"
	codeNotFound           = "not_found"
	codePayloadTooLarge    = "payload_too_large"
	codeInternal           = "internal"
)

type errorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// writeError sends one of the closed-set codes with a human-readable message.
//
// The message must never carry anything the caller did not already know — no stored hash, no
// database error text, no hint about whether an account exists. Those go to the log instead.
func writeError(w http.ResponseWriter, logger *slog.Logger, statusCode int, code string, message string) {
	writeJSON(w, logger, statusCode, errorResponse{Error: code, Message: message})
}

// writeInternalError logs the real cause and tells the caller nothing about it.
func writeInternalError(w http.ResponseWriter, logger *slog.Logger, context string, err error) {
	logger.Error(context, "error", err)
	writeError(w, logger, http.StatusInternalServerError, codeInternal, "something went wrong")
}
