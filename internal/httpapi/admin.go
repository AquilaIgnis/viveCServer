package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/livelog"
	"github.com/AquilaIgnis/viveCServer/internal/store"
)

const (
	adminSessionCookieName = "vive_admin_session"
	formCSRFCookieName     = "vive_form_csrf"
	adminSessionLifetime   = 30 * 24 * time.Hour
	formCSRFLifetime       = 10 * time.Minute
	maxAdminFormBytes      = 64 * 1024
)

type adminStore interface {
	CountAccounts(ctx context.Context) (int64, error)
	CreateInitialAccount(ctx context.Context, normalisedEmail string, passwordHash string) (string, error)
	FindAccountByEmail(ctx context.Context, normalisedEmail string) (store.Account, error)
	UpdatePasswordHash(ctx context.Context, accountID string, passwordHash string) error
	CreateAdminSession(ctx context.Context, accountID string, tokenHash []byte, expiresAt time.Time) error
	AuthenticateAdminSession(ctx context.Context, tokenHash []byte) (store.Account, error)
	DeleteAdminSession(ctx context.Context, tokenHash []byte) error
	ListDevices(ctx context.Context, accountID string) ([]store.Device, error)
	RenameDevice(ctx context.Context, accountID string, deviceID string, name string) error
	RevokeDevice(ctx context.Context, accountID string, deviceID string) error
}

type postgresAdminStore struct {
	pool *pgxpool.Pool
}

func (database postgresAdminStore) CountAccounts(ctx context.Context) (int64, error) {
	return store.CountAccounts(ctx, database.pool)
}

func (database postgresAdminStore) CreateInitialAccount(ctx context.Context, normalisedEmail string, passwordHash string) (string, error) {
	return store.CreateInitialAccount(ctx, database.pool, normalisedEmail, passwordHash)
}

func (database postgresAdminStore) FindAccountByEmail(ctx context.Context, normalisedEmail string) (store.Account, error) {
	return store.FindAccountByEmail(ctx, database.pool, normalisedEmail)
}

func (database postgresAdminStore) UpdatePasswordHash(ctx context.Context, accountID string, passwordHash string) error {
	return store.UpdatePasswordHash(ctx, database.pool, accountID, passwordHash)
}

func (database postgresAdminStore) CreateAdminSession(ctx context.Context, accountID string, tokenHash []byte, expiresAt time.Time) error {
	return store.CreateAdminSession(ctx, database.pool, accountID, tokenHash, expiresAt)
}

func (database postgresAdminStore) AuthenticateAdminSession(ctx context.Context, tokenHash []byte) (store.Account, error) {
	return store.AuthenticateAdminSession(ctx, database.pool, tokenHash)
}

func (database postgresAdminStore) DeleteAdminSession(ctx context.Context, tokenHash []byte) error {
	return store.DeleteAdminSession(ctx, database.pool, tokenHash)
}

func (database postgresAdminStore) ListDevices(ctx context.Context, accountID string) ([]store.Device, error) {
	return store.ListDevices(ctx, database.pool, accountID)
}

func (database postgresAdminStore) RenameDevice(ctx context.Context, accountID string, deviceID string, name string) error {
	return store.RenameDevice(ctx, database.pool, accountID, deviceID, name)
}

func (database postgresAdminStore) RevokeDevice(ctx context.Context, accountID string, deviceID string) error {
	return store.RevokeDevice(ctx, database.pool, accountID, deviceID)
}

type adminApplication struct {
	store    adminStore
	logger   *slog.Logger
	liveLogs *livelog.Broker
}

func (application adminApplication) handler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleLiveness())
	mux.HandleFunc("GET /readyz", handleReadiness(pool, application.logger))
	mux.HandleFunc("GET /assets/admin.css", application.handleStylesheet)
	mux.HandleFunc("GET /assets/admin.js", application.handleAdminScript)
	mux.HandleFunc("GET /{$}", application.handleRoot)
	mux.HandleFunc("GET /setup", application.handleSetupPage)
	mux.HandleFunc("POST /setup", application.handleSetup)
	mux.HandleFunc("GET /login", application.handleLoginPage)
	mux.HandleFunc("POST /login", application.handleLogin)
	mux.HandleFunc("POST /logout", application.handleLogout)
	mux.HandleFunc("GET /admin", application.handleDashboard)
	mux.HandleFunc("GET /admin/logs", application.handleLiveLogs)
	mux.HandleFunc("POST /admin/devices/{deviceID}/rename", application.handleRenameDevice)
	mux.HandleFunc("POST /admin/devices/{deviceID}/revoke", application.handleRevokeDevice)

	return withAdminSecurityHeaders(withPanicRecovery(withRequestLogging(mux, application.logger), application.logger))
}

func (application adminApplication) handleRoot(w http.ResponseWriter, r *http.Request) {
	configured, ok := application.setupCompleteOrWriteError(w, r)
	if !ok {
		return
	}
	if !configured {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (application adminApplication) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	configured, ok := application.setupCompleteOrWriteError(w, r)
	if !ok {
		return
	}
	if configured {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	csrfToken, ok := application.issueFormCSRFToken(w, r)
	if !ok {
		return
	}
	application.renderSetup(w, http.StatusOK, setupPageData{CSRFToken: csrfToken})
}

func (application adminApplication) handleSetup(w http.ResponseWriter, r *http.Request) {
	configured, ok := application.setupCompleteOrWriteError(w, r)
	if !ok {
		return
	}
	if configured {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if !parseAdminForm(w, r) {
		application.renderSetup(w, http.StatusBadRequest, setupPageData{Error: "The submitted form is not valid."})
		return
	}
	csrfToken := r.PostFormValue("csrf_token")
	if !formCSRFTokenMatches(r, csrfToken) {
		application.renderSetup(w, http.StatusForbidden, setupPageData{Error: "This form expired. Reload the page and try again."})
		return
	}

	page := setupPageData{CSRFToken: csrfToken, Email: strings.TrimSpace(r.PostFormValue("email"))}
	email, err := auth.NormaliseEmail(page.Email)
	if err != nil {
		page.Error = err.Error()
		application.renderSetup(w, http.StatusBadRequest, page)
		return
	}
	password := r.PostFormValue("password")
	if err := auth.ValidatePassword(password); err != nil {
		page.Error = err.Error()
		application.renderSetup(w, http.StatusBadRequest, page)
		return
	}
	if password != r.PostFormValue("password_confirmation") {
		page.Error = "Passwords do not match."
		application.renderSetup(w, http.StatusBadRequest, page)
		return
	}

	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		application.writeAdminError(w, "Could not secure the password.", "hashing the initial password failed", err)
		return
	}
	accountID, err := application.store.CreateInitialAccount(r.Context(), email, passwordHash)
	if errors.Is(err, store.ErrSetupAlreadyComplete) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		application.writeAdminError(w, "Could not complete setup.", "creating the initial account failed", err)
		return
	}

	if !application.createAdminSession(w, r, accountID) {
		// The durable part succeeded. Sending the operator to login is recoverable; pretending the
		// account did not get created would make a retry confusingly land on the login page anyway.
		http.Redirect(w, r, "/login?setup=complete", http.StatusSeeOther)
		return
	}
	clearCookie(w, r, formCSRFCookieName)
	application.logger.Info("initial browser setup completed", "account_id", accountID)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (application adminApplication) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	configured, ok := application.setupCompleteOrWriteError(w, r)
	if !ok {
		return
	}
	if !configured {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	if _, _, err := application.accountFromSession(r); err == nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	} else if !errors.Is(err, store.ErrAdminSessionNotFound) {
		application.writeAdminError(w, "Could not check your session.", "checking an admin session failed", err)
		return
	}

	csrfToken, ok := application.issueFormCSRFToken(w, r)
	if !ok {
		return
	}
	message := ""
	if r.URL.Query().Get("setup") == "complete" {
		message = "Setup is complete. Sign in with the account you just created."
	}
	application.renderLogin(w, http.StatusOK, loginPageData{CSRFToken: csrfToken, Message: message})
}

func (application adminApplication) handleLogin(w http.ResponseWriter, r *http.Request) {
	configured, ok := application.setupCompleteOrWriteError(w, r)
	if !ok {
		return
	}
	if !configured {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	if !parseAdminForm(w, r) {
		application.renderLogin(w, http.StatusBadRequest, loginPageData{Error: "The submitted form is not valid."})
		return
	}
	csrfToken := r.PostFormValue("csrf_token")
	page := loginPageData{
		CSRFToken: csrfToken,
		Email:     strings.TrimSpace(r.PostFormValue("email")),
	}
	if !formCSRFTokenMatches(r, csrfToken) {
		page.Error = "This form expired. Reload the page and try again."
		application.renderLogin(w, http.StatusForbidden, page)
		return
	}

	email, err := auth.NormaliseEmail(page.Email)
	if err != nil {
		// Login failures stay identical whether the address is malformed, absent, or paired with a
		// wrong password. That preserves the API's account-enumeration protection in the browser UI.
		auth.SpendVerificationTime(r.PostFormValue("password"))
		page.Error = "Email or password is incorrect."
		application.renderLogin(w, http.StatusUnauthorized, page)
		return
	}

	account, err := application.store.FindAccountByEmail(r.Context(), email)
	if errors.Is(err, store.ErrAccountNotFound) {
		auth.SpendVerificationTime(r.PostFormValue("password"))
		page.Error = "Email or password is incorrect."
		application.renderLogin(w, http.StatusUnauthorized, page)
		return
	}
	if err != nil {
		application.writeAdminError(w, "Could not sign in.", "looking up an admin account failed", err)
		return
	}

	password := r.PostFormValue("password")
	if err := auth.VerifyPassword(account.PasswordHash, password); err != nil {
		if !errors.Is(err, auth.ErrPasswordMismatch) {
			application.logger.Error("stored password hash is unusable", "account_id", account.ID, "error", err)
		}
		page.Error = "Email or password is incorrect."
		application.renderLogin(w, http.StatusUnauthorized, page)
		return
	}

	if auth.NeedsRehash(account.PasswordHash) {
		if upgradedHash, err := auth.HashPassword(password); err == nil {
			if err := application.store.UpdatePasswordHash(r.Context(), account.ID, upgradedHash); err != nil {
				application.logger.Warn("could not upgrade an admin password hash", "account_id", account.ID, "error", err)
			}
		}
	}

	if !application.createAdminSession(w, r, account.ID) {
		application.writeAdminError(w, "Could not start your session.", "creating an admin session failed", errors.New("session creation failed"))
		return
	}
	clearCookie(w, r, formCSRFCookieName)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (application adminApplication) handleDashboard(w http.ResponseWriter, r *http.Request) {
	account, plainSessionToken, ok := application.requireAdminSession(w, r)
	if !ok {
		return
	}

	devices, err := application.store.ListDevices(r.Context(), account.ID)
	if err != nil {
		application.writeAdminError(w, "Could not load registered devices.", "listing devices for admin failed", err)
		return
	}

	views := make([]adminDeviceView, 0, len(devices))
	activeDeviceCount := 0
	for _, device := range devices {
		if device.RevokedAt == nil {
			activeDeviceCount++
		}
		views = append(views, adminDeviceView{
			ID:         device.ID,
			Name:       device.Name,
			Platform:   device.Platform,
			CreatedAt:  formatAdminTime(device.CreatedAt),
			LastSeenAt: formatOptionalAdminTime(device.LastSeenAt, "Never"),
			RevokedAt:  formatOptionalAdminTime(device.RevokedAt, ""),
			Active:     device.RevokedAt == nil,
		})
	}

	application.writeAdminPage(w, http.StatusOK, adminDashboardTemplate, dashboardPageData{
		Email:             account.Email,
		CreatedAt:         formatAdminTime(account.CreatedAt),
		Storage:           formatByteCount(account.StorageBytes),
		ActiveDeviceCount: activeDeviceCount,
		DeviceCount:       len(devices),
		Devices:           views,
		CSRFToken:         adminCSRFToken(plainSessionToken),
	})
}

// handleRenameDevice relabels a device from the dashboard.
//
// Same shape as revoking — session, CSRF, account-scoped store call, redirect — because it is the
// same kind of action on the same row. It is here because the dashboard is the one place that shows
// every device at once, which is exactly where two rows called "Pixel Tablet" are a problem.
func (application adminApplication) handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	account, plainSessionToken, ok := application.requireAdminSession(w, r)
	if !ok {
		return
	}
	if !parseAdminForm(w, r) || !constantTimeTokenMatch(adminCSRFToken(plainSessionToken), r.PostFormValue("csrf_token")) {
		application.writeAdminPage(w, http.StatusForbidden, adminErrorTemplate, adminErrorPageData{
			Title:   "Request expired",
			Message: "Return to the dashboard and try again.",
		})
		return
	}

	name, err := validateDeviceName(r.PostFormValue("name"))
	if err != nil {
		application.writeAdminPage(w, http.StatusBadRequest, adminErrorTemplate, adminErrorPageData{
			Title:   "That name will not do",
			Message: err.Error() + ".",
		})
		return
	}

	deviceID := r.PathValue("deviceID")
	if err := application.store.RenameDevice(r.Context(), account.ID, deviceID, name); err != nil {
		if errors.Is(err, store.ErrDeviceNotFound) {
			application.writeAdminPage(w, http.StatusNotFound, adminErrorTemplate, adminErrorPageData{
				Title:   "Device not found",
				Message: "That device does not exist for this account, or has been revoked.",
			})
			return
		}
		application.writeAdminError(w, "Could not rename the device.", "renaming a device from admin failed", err)
		return
	}
	application.logger.Info("device renamed from admin panel", "account_id", account.ID, "device_id", deviceID)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (application adminApplication) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	account, plainSessionToken, ok := application.requireAdminSession(w, r)
	if !ok {
		return
	}
	if !parseAdminForm(w, r) || !constantTimeTokenMatch(adminCSRFToken(plainSessionToken), r.PostFormValue("csrf_token")) {
		application.writeAdminPage(w, http.StatusForbidden, adminErrorTemplate, adminErrorPageData{
			Title:   "Request expired",
			Message: "Return to the dashboard and try again.",
		})
		return
	}

	deviceID := r.PathValue("deviceID")
	if err := application.store.RevokeDevice(r.Context(), account.ID, deviceID); err != nil {
		if errors.Is(err, store.ErrDeviceNotFound) {
			application.writeAdminPage(w, http.StatusNotFound, adminErrorTemplate, adminErrorPageData{
				Title:   "Device not found",
				Message: "That device does not exist for this account.",
			})
			return
		}
		application.writeAdminError(w, "Could not revoke the device.", "revoking a device from admin failed", err)
		return
	}
	application.logger.Info("device revoked from admin panel", "account_id", account.ID, "device_id", deviceID)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (application adminApplication) handleLogout(w http.ResponseWriter, r *http.Request) {
	_, plainSessionToken, err := application.accountFromSession(r)
	if errors.Is(err, store.ErrAdminSessionNotFound) {
		clearCookie(w, r, adminSessionCookieName)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		application.writeAdminError(w, "Could not sign out.", "checking the session during logout failed", err)
		return
	}
	if !parseAdminForm(w, r) || !constantTimeTokenMatch(adminCSRFToken(plainSessionToken), r.PostFormValue("csrf_token")) {
		application.writeAdminPage(w, http.StatusForbidden, adminErrorTemplate, adminErrorPageData{
			Title:   "Request expired",
			Message: "Return to the dashboard and try again.",
		})
		return
	}

	if err := application.store.DeleteAdminSession(r.Context(), auth.HashToken(plainSessionToken)); err != nil {
		application.writeAdminError(w, "Could not sign out.", "deleting an admin session failed", err)
		return
	}
	clearCookie(w, r, adminSessionCookieName)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (application adminApplication) setupCompleteOrWriteError(w http.ResponseWriter, r *http.Request) (bool, bool) {
	accountCount, err := application.store.CountAccounts(r.Context())
	if err != nil {
		application.writeAdminError(w, "Could not check server setup.", "counting accounts for admin failed", err)
		return false, false
	}
	return accountCount > 0, true
}

func (application adminApplication) createAdminSession(w http.ResponseWriter, r *http.Request, accountID string) bool {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		application.logger.Error("minting an admin session token failed", "account_id", accountID, "error", err)
		return false
	}
	expiresAt := time.Now().Add(adminSessionLifetime)
	if err := application.store.CreateAdminSession(r.Context(), accountID, tokenHash, expiresAt); err != nil {
		application.logger.Error("persisting an admin session failed", "account_id", accountID, "error", err)
		return false
	}

	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    plainToken,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(adminSessionLifetime.Seconds()),
		HttpOnly: true,
		Secure:   requestUsesHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	return true
}

func (application adminApplication) accountFromSession(r *http.Request) (store.Account, string, error) {
	cookie, err := r.Cookie(adminSessionCookieName)
	if err != nil {
		return store.Account{}, "", store.ErrAdminSessionNotFound
	}
	plainToken, ok := auth.ParseAdminSessionToken(cookie.Value)
	if !ok {
		return store.Account{}, "", store.ErrAdminSessionNotFound
	}
	account, err := application.store.AuthenticateAdminSession(r.Context(), auth.HashToken(plainToken))
	if err != nil {
		return store.Account{}, "", err
	}
	return account, plainToken, nil
}

func (application adminApplication) requireAdminSession(w http.ResponseWriter, r *http.Request) (store.Account, string, bool) {
	account, plainToken, err := application.accountFromSession(r)
	if errors.Is(err, store.ErrAdminSessionNotFound) {
		clearCookie(w, r, adminSessionCookieName)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return store.Account{}, "", false
	}
	if err != nil {
		application.writeAdminError(w, "Could not check your session.", "authenticating an admin session failed", err)
		return store.Account{}, "", false
	}
	return account, plainToken, true
}

func (application adminApplication) issueFormCSRFToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		application.writeAdminError(w, "Could not prepare the form.", "generating a form token failed", err)
		return "", false
	}
	token := base64.RawURLEncoding.EncodeToString(randomBytes)
	expiresAt := time.Now().Add(formCSRFLifetime)
	http.SetCookie(w, &http.Cookie{
		Name:     formCSRFCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(formCSRFLifetime.Seconds()),
		HttpOnly: true,
		Secure:   requestUsesHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	return token, true
}

func (application adminApplication) writeAdminError(w http.ResponseWriter, message string, logMessage string, err error) {
	application.logger.Error(logMessage, "error", err)
	application.writeAdminPage(w, http.StatusInternalServerError, adminErrorTemplate, adminErrorPageData{
		Title:   "Something went wrong",
		Message: message,
	})
}

func parseAdminForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminFormBytes)
	return r.ParseForm() == nil
}

func formCSRFTokenMatches(r *http.Request, submittedToken string) bool {
	cookie, err := r.Cookie(formCSRFCookieName)
	if err != nil {
		return false
	}
	return constantTimeTokenMatch(cookie.Value, submittedToken)
}

func constantTimeTokenMatch(expected string, actual string) bool {
	expectedDigest := sha256.Sum256([]byte(expected))
	actualDigest := sha256.Sum256([]byte(actual))
	return subtle.ConstantTimeCompare(expectedDigest[:], actualDigest[:]) == 1 && expected != "" && actual != ""
}

func adminCSRFToken(plainSessionToken string) string {
	digest := sha256.Sum256([]byte("vive-admin-csrf\x00" + plainSessionToken))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func requestUsesHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// This only makes a cookie more restrictive, so trusting it cannot expose one. Reverse proxies
	// should replace rather than append this header before forwarding.
	return strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https")
}

func clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestUsesHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func withAdminSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func formatAdminTime(value time.Time) string {
	return value.Local().Format("Jan 2, 2006 at 3:04 PM")
}

func formatOptionalAdminTime(value *time.Time, fallback string) string {
	if value == nil {
		return fallback
	}
	return formatAdminTime(*value)
}

func formatByteCount(byteCount int64) string {
	const unit = 1024
	if byteCount < unit {
		return fmt.Sprintf("%d B", byteCount)
	}
	divisor, exponent := int64(unit), 0
	for quotient := byteCount / unit; quotient >= unit && exponent < 3; quotient /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(byteCount)/float64(divisor), "KMGT"[exponent])
}
