package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/store"
)

type createAccountRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type createAccountResponse struct {
	AccountID string `json:"accountId"`
}

// handleCreateAccount creates an account over HTTP, if the server is configured to allow it.
func handleCreateAccount(pool *pgxpool.Pool, logger *slog.Logger, signupMode config.SignupMode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if signupMode != config.SignupModeOpen {
			writeError(w, logger, http.StatusForbidden, codeSignupClosed,
				"this server does not accept registrations; the operator creates accounts with the create-account command")
			return
		}

		var request createAccountRequest
		if !decodeJSONRequest(w, r, logger, maxAuthRequestBytes, &request) {
			return
		}

		email, err := auth.NormaliseEmail(request.Email)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
		if err := auth.ValidatePassword(request.Password); err != nil {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}

		passwordHash, err := auth.HashPassword(request.Password)
		if err != nil {
			writeInternalError(w, logger, "hashing a password failed", err)
			return
		}

		accountID, err := store.CreateInitialAccount(r.Context(), pool, email, passwordHash)
		if errors.Is(err, store.ErrEmailTaken) || errors.Is(err, store.ErrSetupAlreadyComplete) {
			// The community server has exactly one account. Keep this generic rather than saying
			// whether the submitted address is the one it holds.
			writeError(w, logger, http.StatusConflict, codeSignupClosed, "this server already has its account")
			return
		}
		if err != nil {
			writeInternalError(w, logger, "creating an account failed", err)
			return
		}

		logger.Info("account created", "account_id", accountID)
		writeJSON(w, logger, http.StatusCreated, createAccountResponse{AccountID: accountID})
	}
}
