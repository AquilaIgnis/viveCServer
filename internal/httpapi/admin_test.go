package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/livelog"
	"github.com/AquilaIgnis/viveCServer/internal/store"
)

type fakeAdminStore struct {
	accountCount int64
	account      store.Account
	sessions     map[string]string
	devices      []store.Device
	revokedID    string
}

func (database *fakeAdminStore) CountAccounts(context.Context) (int64, error) {
	return database.accountCount, nil
}

func (database *fakeAdminStore) CreateInitialAccount(_ context.Context, email string, passwordHash string) (string, error) {
	if database.accountCount > 0 {
		return "", store.ErrSetupAlreadyComplete
	}
	database.accountCount = 1
	database.account = store.Account{
		ID:           "10000000-0000-0000-0000-000000000001",
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	}
	return database.account.ID, nil
}

func (database *fakeAdminStore) FindAccountByEmail(_ context.Context, email string) (store.Account, error) {
	if database.accountCount == 0 || database.account.Email != email {
		return store.Account{}, store.ErrAccountNotFound
	}
	return database.account, nil
}

func (database *fakeAdminStore) UpdatePasswordHash(_ context.Context, _ string, passwordHash string) error {
	database.account.PasswordHash = passwordHash
	return nil
}

func (database *fakeAdminStore) CreateAdminSession(_ context.Context, accountID string, tokenHash []byte, _ time.Time) error {
	if database.sessions == nil {
		database.sessions = make(map[string]string)
	}
	database.sessions[string(tokenHash)] = accountID
	return nil
}

func (database *fakeAdminStore) AuthenticateAdminSession(_ context.Context, tokenHash []byte) (store.Account, error) {
	if database.sessions[string(tokenHash)] != database.account.ID {
		return store.Account{}, store.ErrAdminSessionNotFound
	}
	return database.account, nil
}

func (database *fakeAdminStore) DeleteAdminSession(_ context.Context, tokenHash []byte) error {
	delete(database.sessions, string(tokenHash))
	return nil
}

func (database *fakeAdminStore) ListDevices(_ context.Context, accountID string) ([]store.Device, error) {
	if accountID != database.account.ID {
		return nil, errors.New("wrong account")
	}
	return database.devices, nil
}

func (database *fakeAdminStore) RevokeDevice(_ context.Context, accountID string, deviceID string) error {
	if accountID != database.account.ID {
		return store.ErrDeviceNotFound
	}
	for index := range database.devices {
		if database.devices[index].ID == deviceID {
			now := time.Now()
			database.devices[index].RevokedAt = &now
			database.revokedID = deviceID
			return nil
		}
	}
	return store.ErrDeviceNotFound
}

func newAdminTestHandler(database adminStore) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return adminApplication{store: database, logger: logger}.handler(nil)
}

func TestFirstRunSetupCreatesOwnerAndSignsIn(t *testing.T) {
	database := &fakeAdminStore{}
	handler := newAdminTestHandler(database)

	setupPage := httptest.NewRecorder()
	handler.ServeHTTP(setupPage, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if setupPage.Code != http.StatusOK {
		t.Fatalf("GET /setup status = %d, want 200", setupPage.Code)
	}
	csrfCookie := responseCookie(t, setupPage.Result(), formCSRFCookieName)
	if !strings.Contains(setupPage.Body.String(), `name="csrf_token" value="`+csrfCookie.Value+`"`) {
		t.Fatal("setup page did not bind the CSRF cookie into the form")
	}

	form := url.Values{
		"csrf_token":            {csrfCookie.Value},
		"email":                 {"Owner@Example.com"},
		"password":              {"correct horse battery staple"},
		"password_confirmation": {"correct horse battery staple"},
	}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(csrfCookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin" {
		t.Fatalf("POST /setup = %d %q, want 303 /admin", response.Code, response.Header().Get("Location"))
	}
	if database.account.Email != "owner@example.com" {
		t.Fatalf("stored email = %q, want normalised address", database.account.Email)
	}
	if database.account.PasswordHash == form.Get("password") {
		t.Fatal("setup stored the plaintext password")
	}
	if err := auth.VerifyPassword(database.account.PasswordHash, form.Get("password")); err != nil {
		t.Fatalf("stored Argon2id hash does not verify: %v", err)
	}

	adminCookie := responseCookie(t, response.Result(), adminSessionCookieName)
	if !adminCookie.HttpOnly || adminCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("admin cookie security attributes = HttpOnly:%v SameSite:%v", adminCookie.HttpOnly, adminCookie.SameSite)
	}

	dashboardRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	dashboardRequest.AddCookie(adminCookie)
	dashboard := httptest.NewRecorder()
	handler.ServeHTTP(dashboard, dashboardRequest)
	if dashboard.Code != http.StatusOK || !strings.Contains(dashboard.Body.String(), "owner@example.com") {
		t.Fatalf("authenticated dashboard = %d body %q", dashboard.Code, dashboard.Body.String())
	}
	if dashboard.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("admin response is missing its Content-Security-Policy")
	}
	if !strings.Contains(dashboard.Body.String(), `id="live-log-output"`) ||
		!strings.Contains(dashboard.Body.String(), `src="/assets/admin.js"`) {
		t.Fatal("dashboard is missing the live log panel or its browser script")
	}
}

func TestSetupRejectsMissingCSRFToken(t *testing.T) {
	database := &fakeAdminStore{}
	handler := newAdminTestHandler(database)
	form := url.Values{
		"email":                 {"owner@example.com"},
		"password":              {"correct horse battery staple"},
		"password_confirmation": {"correct horse battery staple"},
	}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("POST /setup without CSRF status = %d, want 403", response.Code)
	}
	if database.accountCount != 0 {
		t.Fatal("a CSRF request created the owner account")
	}
}

func TestAdminCanRevokeADevice(t *testing.T) {
	passwordHash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount: 1,
		account: store.Account{
			ID:           "10000000-0000-0000-0000-000000000001",
			Email:        "owner@example.com",
			PasswordHash: passwordHash,
			CreatedAt:    time.Now(),
		},
		devices: []store.Device{{
			ID:        "20000000-0000-0000-0000-000000000002",
			AccountID: "10000000-0000-0000-0000-000000000001",
			Name:      `<script>alert("x")</script>`,
			Platform:  "Android",
			CreatedAt: time.Now(),
		}},
	}
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	database.sessions = map[string]string{string(tokenHash): database.account.ID}
	handler := newAdminTestHandler(database)

	dashboardRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	dashboardRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	dashboard := httptest.NewRecorder()
	handler.ServeHTTP(dashboard, dashboardRequest)
	if strings.Contains(dashboard.Body.String(), `<script>alert`) {
		t.Fatal("device name was written without HTML escaping")
	}
	if !strings.Contains(dashboard.Body.String(), "&lt;script&gt;") {
		t.Fatal("escaped device name is absent from the dashboard")
	}

	form := url.Values{"csrf_token": {adminCSRFToken(plainToken)}}
	revokeRequest := httptest.NewRequest(http.MethodPost, "/admin/devices/20000000-0000-0000-0000-000000000002/revoke", strings.NewReader(form.Encode()))
	revokeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revokeRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	revokeResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokeResponse, revokeRequest)
	if revokeResponse.Code != http.StatusSeeOther || database.revokedID == "" {
		t.Fatalf("revoke response = %d, revoked id = %q", revokeResponse.Code, database.revokedID)
	}
}

func TestConfiguredOwnerCanLoginAndLogout(t *testing.T) {
	passwordHash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount: 1,
		account: store.Account{
			ID:           "10000000-0000-0000-0000-000000000001",
			Email:        "owner@example.com",
			PasswordHash: passwordHash,
			CreatedAt:    time.Now(),
		},
	}
	handler := newAdminTestHandler(database)

	loginPage := httptest.NewRecorder()
	handler.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, "/login", nil))
	csrfCookie := responseCookie(t, loginPage.Result(), formCSRFCookieName)
	form := url.Values{
		"csrf_token": {csrfCookie.Value},
		"email":      {"owner@example.com"},
		"password":   {"correct horse battery staple"},
	}
	loginRequest := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRequest.AddCookie(csrfCookie)
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusSeeOther || loginResponse.Header().Get("Location") != "/admin" {
		t.Fatalf("login response = %d %q", loginResponse.Code, loginResponse.Header().Get("Location"))
	}

	adminCookie := responseCookie(t, loginResponse.Result(), adminSessionCookieName)
	logoutForm := url.Values{"csrf_token": {adminCSRFToken(adminCookie.Value)}}
	logoutRequest := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(logoutForm.Encode()))
	logoutRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	logoutRequest.AddCookie(adminCookie)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusSeeOther || logoutResponse.Header().Get("Location") != "/login" {
		t.Fatalf("logout response = %d %q", logoutResponse.Code, logoutResponse.Header().Get("Location"))
	}
	if len(database.sessions) != 0 {
		t.Fatal("logout left the browser session usable in the database")
	}
}

func TestLiveLogsRequireAnAdminSession(t *testing.T) {
	database := &fakeAdminStore{
		accountCount: 1,
		account: store.Account{
			ID:        "10000000-0000-0000-0000-000000000001",
			Email:     "owner@example.com",
			CreatedAt: time.Now(),
		},
	}
	application := adminApplication{
		store:    database,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		liveLogs: livelog.NewBroker(),
	}
	response := httptest.NewRecorder()
	application.handler(nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/logs", nil))

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/login" {
		t.Fatalf("unauthenticated live log response = %d %q, want 303 /login", response.Code, response.Header().Get("Location"))
	}
}

func TestAuthenticatedAdminReceivesFutureLiveLogs(t *testing.T) {
	account := store.Account{
		ID:        "10000000-0000-0000-0000-000000000001",
		Email:     "owner@example.com",
		CreatedAt: time.Now(),
	}
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount: 1,
		account:      account,
		sessions:     map[string]string{string(tokenHash): account.ID},
	}
	broker := livelog.NewBroker()
	logger := slog.New(livelog.NewHandler(slog.NewTextHandler(io.Discard, nil), broker))
	application := adminApplication{store: database, logger: logger, liveLogs: broker}
	requestContext, cancelRequest := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/admin/logs", nil).WithContext(requestContext)
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	response := newSSETestWriter()
	handlerFinished := make(chan struct{})
	go func() {
		defer close(handlerFinished)
		application.handler(nil).ServeHTTP(response, request)
	}()
	defer func() {
		cancelRequest()
		select {
		case <-handlerFinished:
		case <-time.After(time.Second):
			t.Error("live log handler did not stop after its request was cancelled")
		}
	}()

	ready := readSSEFrame(t, response.writes)
	if response.statusCode != http.StatusOK {
		t.Fatalf("authenticated live log status = %d, want 200", response.statusCode)
	}
	if contentType := response.header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("live log content type = %q, want text/event-stream", contentType)
	}
	if ready.event != "ready" {
		t.Fatalf("first SSE event = %q, want ready", ready.event)
	}

	logger.Info("device registered",
		"account_id", account.ID,
		"device_id", "20000000-0000-0000-0000-000000000002",
	)
	logFrame := readSSEFrame(t, response.writes)
	if logFrame.event != "log" {
		t.Fatalf("SSE event = %q, want log", logFrame.event)
	}
	var event livelog.Event
	if err := json.Unmarshal([]byte(logFrame.data), &event); err != nil {
		t.Fatalf("decoding live log event: %v", err)
	}
	if event.Message != "device registered" || event.Attributes["device_id"] == "" {
		t.Fatalf("unexpected live log event: %+v", event)
	}
}

type sseFrame struct {
	event string
	data  string
}

type sseTestWriter struct {
	header     http.Header
	statusCode int
	writes     chan []byte
}

func newSSETestWriter() *sseTestWriter {
	return &sseTestWriter{
		header: make(http.Header),
		writes: make(chan []byte, 8),
	}
}

func (writer *sseTestWriter) Header() http.Header {
	return writer.header
}

func (writer *sseTestWriter) WriteHeader(statusCode int) {
	writer.statusCode = statusCode
}

func (writer *sseTestWriter) Write(body []byte) (int, error) {
	copyOfBody := append([]byte(nil), body...)
	writer.writes <- copyOfBody
	return len(body), nil
}

func (writer *sseTestWriter) Flush() {}

func (writer *sseTestWriter) SetWriteDeadline(time.Time) error {
	return nil
}

func readSSEFrame(t *testing.T, writes <-chan []byte) sseFrame {
	t.Helper()
	select {
	case body := <-writes:
		var frame sseFrame
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "event: ") {
				frame.event = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				frame.data = strings.TrimPrefix(line, "data: ")
			}
		}
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE frame")
		return sseFrame{}
	}
}

func TestSetupRoutesAreAbsentFromSyncPort(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewSyncHandler(nil, logger, Options{SignupMode: config.SignupModeClosed})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("sync GET /setup status = %d, want 404", response.Code)
	}
}

func responseCookie(t *testing.T, response *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == name && cookie.MaxAge >= 0 {
			return cookie
		}
	}
	t.Fatalf("response did not set cookie %q", name)
	return nil
}
