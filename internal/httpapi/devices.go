package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/changefeed"
	"github.com/AquilaIgnis/viveCServer/internal/store"
)

type registerDeviceRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

type registerDeviceResponse struct {
	DeviceID  string `json:"deviceId"`
	AccountID string `json:"accountId"`

	// Returned exactly once. Nothing on the server can produce it again — only its SHA-256 is
	// stored — so a client that loses it registers a new device.
	Token string `json:"token"`
}

type deviceSummary struct {
	DeviceID      string     `json:"deviceId"`
	Name          string     `json:"name"`
	Platform      string     `json:"platform"`
	CreatedAt     time.Time  `json:"createdAt"`
	LastSeenAt    *time.Time `json:"lastSeenAt,omitempty"`
	LastPulledSeq int64      `json:"lastPulledSeq"`
	RevokedAt     *time.Time `json:"revokedAt,omitempty"`
}

type listDevicesResponse struct {
	Devices []deviceSummary `json:"devices"`
}

// handleRegisterDevice exchanges an account password for a device token.
//
// This is the only endpoint that takes a password, and the only one that returns a token. Every
// other request authenticates with the token, so the password is typed once per device rather than
// held anywhere.
func handleRegisterDevice(pool *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request registerDeviceRequest
		if !decodeJSONRequest(w, r, logger, maxAuthRequestBytes, &request) {
			return
		}

		email, err := auth.NormaliseEmail(request.Email)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
		deviceName, err := validateDeviceName(request.Name)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}
		platform, err := validatePlatform(request.Platform)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}

		account, err := store.FindAccountByEmail(r.Context(), pool, email)
		if errors.Is(err, store.ErrAccountNotFound) {
			// Hash anyway, so an unregistered address takes as long to reject as a wrong password
			// does. Skipping it here is what turns this endpoint into an account-enumeration
			// oracle, and the wasted work is the entire point.
			auth.SpendVerificationTime(request.Password)
			writeInvalidCredentials(w, logger)
			return
		}
		if err != nil {
			writeInternalError(w, logger, "looking up an account failed", err)
			return
		}

		if err := auth.VerifyPassword(account.PasswordHash, request.Password); err != nil {
			if !errors.Is(err, auth.ErrPasswordMismatch) {
				// A stored hash that will not parse is corruption, not a bad password. The caller
				// still learns nothing, but an operator needs to see it.
				logger.Error("stored password hash is unusable", "account_id", account.ID, "error", err)
			}
			writeInvalidCredentials(w, logger)
			return
		}

		// The password was right, so this is the one moment the plaintext is available to re-hash
		// under stronger parameters. Failing to upgrade must not fail the sign-in.
		if auth.NeedsRehash(account.PasswordHash) {
			if upgraded, err := auth.HashPassword(request.Password); err == nil {
				if err := store.UpdatePasswordHash(r.Context(), pool, account.ID, upgraded); err != nil {
					logger.Warn("could not upgrade a password hash", "account_id", account.ID, "error", err)
				} else {
					logger.Info("upgraded a password hash", "account_id", account.ID)
				}
			}
		}

		plainToken, tokenHash, err := auth.MintDeviceToken()
		if err != nil {
			writeInternalError(w, logger, "minting a device token failed", err)
			return
		}

		deviceID, err := store.RegisterDevice(r.Context(), pool, account.ID, deviceName, platform, tokenHash)
		if err != nil {
			writeInternalError(w, logger, "registering a device failed", err)
			return
		}

		logger.Info("device registered", "account_id", account.ID, "device_id", deviceID, "platform", platform)
		writeJSON(w, logger, http.StatusCreated, registerDeviceResponse{
			DeviceID:  deviceID,
			AccountID: account.ID,
			Token:     plainToken,
		})
	}
}

// handleListDevices returns the calling account's devices, so the app's settings screen can show
// what is registered and offer to revoke it.
func handleListDevices(pool *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := authenticatedDeviceOrFail(w, r, logger)
		if !ok {
			return
		}

		// The account id comes from the authenticated context, never from the request. That is the
		// whole of this server's tenancy model.
		devices, err := store.ListDevices(r.Context(), pool, caller.AccountID)
		if err != nil {
			writeInternalError(w, logger, "listing devices failed", err)
			return
		}

		summaries := make([]deviceSummary, 0, len(devices))
		for _, d := range devices {
			summaries = append(summaries, deviceSummary{
				DeviceID:      d.ID,
				Name:          d.Name,
				Platform:      d.Platform,
				CreatedAt:     d.CreatedAt,
				LastSeenAt:    d.LastSeenAt,
				LastPulledSeq: d.LastPulledSeq,
				RevokedAt:     d.RevokedAt,
			})
		}

		writeJSON(w, logger, http.StatusOK, listDevicesResponse{Devices: summaries})
	}
}

type renameDeviceRequest struct {
	Name string `json:"name"`
}

// handleRenameDevice relabels a device, including the calling device's own.
//
// The name a client registers with is whatever it could work out about the hardware, and that is
// regularly not enough to tell two devices apart — two Android emulators built from different
// profiles report the same `Build.MODEL`, and so do two real tablets of the same model. The field
// exists so a person can tell which row to revoke, so the fix for an ambiguous one is to let them
// write it rather than to invent a better guess.
func handleRenameDevice(pool *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := authenticatedDeviceOrFail(w, r, logger)
		if !ok {
			return
		}

		var request renameDeviceRequest
		if !decodeJSONRequest(w, r, logger, maxAuthRequestBytes, &request) {
			return
		}
		name, err := validateDeviceName(request.Name)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, codeInvalidRequest, err.Error())
			return
		}

		targetDeviceID := r.PathValue("deviceID")
		if err := store.RenameDevice(r.Context(), pool, caller.AccountID, targetDeviceID, name); err != nil {
			if errors.Is(err, store.ErrDeviceNotFound) {
				// 404 for another account's device, for the same reason revocation does it: a 403
				// would confirm that the id exists.
				writeError(w, logger, http.StatusNotFound, codeNotFound, "no such device")
				return
			}
			writeInternalError(w, logger, "renaming a device failed", err)
			return
		}

		logger.Info("device renamed",
			"account_id", caller.AccountID,
			"device_id", targetDeviceID,
			"renamed_by", caller.DeviceID,
			"self", targetDeviceID == caller.DeviceID,
		)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleRevokeDevice makes a device's token stop working, including the calling device's own.
func handleRevokeDevice(pool *pgxpool.Pool, logger *slog.Logger, changeEvents *changefeed.Broker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := authenticatedDeviceOrFail(w, r, logger)
		if !ok {
			return
		}

		targetDeviceID := r.PathValue("deviceID")

		if err := store.RevokeDevice(r.Context(), pool, caller.AccountID, targetDeviceID); err != nil {
			if errors.Is(err, store.ErrDeviceNotFound) {
				// 404 rather than 403 for a device belonging to another account. A 403 would
				// confirm the id exists, which is information about somebody else's account.
				writeError(w, logger, http.StatusNotFound, codeNotFound, "no such device")
				return
			}
			writeInternalError(w, logger, "revoking a device failed", err)
			return
		}

		logger.Info("device revoked",
			"account_id", caller.AccountID,
			"device_id", targetDeviceID,
			"revoked_by", caller.DeviceID,
			"self", targetDeviceID == caller.DeviceID,
		)
		changeEvents.Revoke(caller.AccountID, targetDeviceID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// writeInvalidCredentials is one function so that every credential failure is byte-identical.
// Two call sites with two slightly different messages is how a difference between "no such account"
// and "wrong password" gets reintroduced by accident.
func writeInvalidCredentials(w http.ResponseWriter, logger *slog.Logger) {
	writeError(w, logger, http.StatusUnauthorized, codeInvalidCredentials, "email or password is incorrect")
}
