package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/store"
)

// requireAuthentication resolves the bearer token on every protected route and puts the caller's
// identity into the request context.
//
// It is applied per route rather than globally, because the credential endpoints beneath it are
// exactly the ones that cannot require a credential. Wrapping the whole mux and then carving out
// exceptions is the version of this that eventually leaks a route.
func requireAuthentication(pool *pgxpool.Pool, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			plainToken, wellFormed := auth.ParseBearerToken(r.Header.Get("Authorization"))
			if !wellFormed {
				// WWW-Authenticate is what makes a 401 a protocol answer rather than a bare
				// refusal; a client can tell "you sent no credential" from "your credential is
				// wrong" without guessing.
				w.Header().Set("WWW-Authenticate", `Bearer realm="vivecserver"`)
				writeError(w, logger, http.StatusUnauthorized, codeUnauthenticated, "a bearer token is required")
				return
			}

			deviceID, accountID, err := store.AuthenticateDeviceToken(r.Context(), pool, auth.HashDeviceToken(plainToken))
			switch {
			case errors.Is(err, store.ErrDeviceNotFound):
				// Unknown and revoked are the same answer on purpose: revocation then takes effect
				// on the next request, with no session cache anywhere to invalidate.
				w.Header().Set("WWW-Authenticate", `Bearer realm="vivecserver", error="invalid_token"`)
				writeError(w, logger, http.StatusUnauthorized, codeUnauthenticated, "unknown or revoked token")
				return

			case errors.Is(err, store.ErrLastSeenNotRecorded):
				// Authentication succeeded; only the diagnostic timestamp did not. Failing the
				// request over bookkeeping would be a worse outcome than a stale column.
				logger.Warn("could not record device last_seen_at", "device_id", deviceID, "error", err)

			case err != nil:
				writeInternalError(w, logger, "authenticating a device failed", err)
				return
			}

			authenticated := auth.AuthenticatedDevice{DeviceID: deviceID, AccountID: accountID}
			next.ServeHTTP(w, r.WithContext(auth.WithAuthenticatedDevice(r.Context(), authenticated)))
		})
	}
}

// authenticatedDeviceOrFail reads the identity the middleware stored.
//
// Its absence means a route was registered without the middleware, which is a wiring bug rather
// than anything the caller did — hence a 500 and a loud log, not a 401 that would quietly look like
// ordinary auth failure while a protected route sat open.
func authenticatedDeviceOrFail(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (auth.AuthenticatedDevice, bool) {
	device, ok := auth.DeviceFrom(r.Context())
	if !ok {
		logger.Error("protected route reached without the authentication middleware", "path", r.URL.Path)
		writeError(w, logger, http.StatusInternalServerError, codeInternal, "something went wrong")
		return auth.AuthenticatedDevice{}, false
	}
	return device, true
}
