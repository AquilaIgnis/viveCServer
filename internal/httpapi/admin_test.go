package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/livelog"
	"github.com/AquilaIgnis/viveCServer/internal/store"
)

type fakeAdminStore struct {
	accountCount        int64
	account             store.Account
	sessions            map[string]string
	devices             []store.Device
	notebooks           []store.NotebookSummary
	cloudNotebooks      []store.NotebookSummary
	archivedNotebooks   []store.NotebookSummary
	deleteNotebookErr   error
	deletedNotebookIDs  []string
	unhostNotebookErr   error
	unhostedNotebookIDs []string
	revokedID           string
	renamedID           string
	removedIDs          []string
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

func (database *fakeAdminStore) RenameDevice(_ context.Context, accountID string, deviceID string, name string) error {
	if accountID != database.account.ID {
		return store.ErrDeviceNotFound
	}
	for index := range database.devices {
		// Revoked rows are not renameable: history that can be relabelled is worse than history.
		if database.devices[index].ID == deviceID && database.devices[index].RevokedAt == nil {
			database.devices[index].Name = name
			database.renamedID = deviceID
			return nil
		}
	}
	return store.ErrDeviceNotFound
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

func (database *fakeAdminStore) DeleteRevokedDevice(_ context.Context, accountID string, deviceID string) error {
	if accountID != database.account.ID {
		return store.ErrDeviceNotFound
	}
	for index := range database.devices {
		if database.devices[index].ID != deviceID {
			continue
		}
		if database.devices[index].RevokedAt == nil {
			return store.ErrDeviceStillActive
		}
		database.devices = append(database.devices[:index], database.devices[index+1:]...)
		database.removedIDs = append(database.removedIDs, deviceID)
		return nil
	}
	return store.ErrDeviceNotFound
}

func (database *fakeAdminStore) DeleteRevokedDevices(_ context.Context, accountID string) (int64, error) {
	if accountID != database.account.ID {
		return 0, errors.New("wrong account")
	}
	kept := make([]store.Device, 0, len(database.devices))
	var removedCount int64
	for _, device := range database.devices {
		if device.RevokedAt == nil {
			kept = append(kept, device)
			continue
		}
		database.removedIDs = append(database.removedIDs, device.ID)
		removedCount++
	}
	database.devices = kept
	return removedCount, nil
}

func (database *fakeAdminStore) NotebookOverview(_ context.Context, accountID string, limit int) (store.NotebookOverviewResult, error) {
	if accountID != database.account.ID {
		return store.NotebookOverviewResult{}, errors.New("wrong account")
	}
	overview := store.NotebookOverviewResult{
		NotebookCount:         int64(len(database.notebooks)),
		CloudNotebookCount:    int64(len(database.cloudNotebooks)),
		ArchivedNotebookCount: int64(len(database.archivedNotebooks)),
	}
	overview.Notebooks = database.notebooks[:min(len(database.notebooks), limit)]
	overview.CloudNotebooks = database.cloudNotebooks[:min(len(database.cloudNotebooks), limit)]
	overview.ArchivedNotebooks = database.archivedNotebooks[:min(len(database.archivedNotebooks), limit)]
	return overview, nil
}

func (database *fakeAdminStore) DeleteArchivedNotebook(_ context.Context, accountID string, notebookID string) error {
	if accountID != database.account.ID {
		return store.ErrNotebookNotFound
	}
	if database.deleteNotebookErr != nil {
		return database.deleteNotebookErr
	}
	for index, notebook := range database.archivedNotebooks {
		if notebook.ID != notebookID {
			continue
		}
		database.archivedNotebooks = append(database.archivedNotebooks[:index], database.archivedNotebooks[index+1:]...)
		database.deletedNotebookIDs = append(database.deletedNotebookIDs, notebookID)
		return nil
	}
	for _, notebook := range database.notebooks {
		if notebook.ID == notebookID {
			return store.ErrNotebookNotArchived
		}
	}
	return store.ErrNotebookNotFound
}

func (database *fakeAdminStore) StopHostingCloudNotebook(_ context.Context, accountID string, notebookID string) error {
	if accountID != database.account.ID {
		return store.ErrNotebookNotFound
	}
	if database.unhostNotebookErr != nil {
		return database.unhostNotebookErr
	}
	for index, notebook := range database.cloudNotebooks {
		if notebook.ID != notebookID {
			continue
		}
		database.cloudNotebooks = append(database.cloudNotebooks[:index], database.cloudNotebooks[index+1:]...)
		database.archivedNotebooks = append(database.archivedNotebooks, notebook)
		database.unhostedNotebookIDs = append(database.unhostedNotebookIDs, notebookID)
		return nil
	}
	for _, notebook := range append(append([]store.NotebookSummary{}, database.notebooks...), database.archivedNotebooks...) {
		if notebook.ID == notebookID {
			return store.ErrNotebookNotCloudHosted
		}
	}
	return store.ErrNotebookNotFound
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
		!strings.Contains(dashboard.Body.String(), `src="`+adminScriptPath+`"`) {
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

// TestDevicesCanBeRenamedFromTheDashboard: the dashboard is the one place that shows every device
// at once, which is where two rows reporting the same `Build.MODEL` become a problem — and the
// only place to fix it, since the name a client registers with is all it knew about itself.
func TestDevicesCanBeRenamedFromTheDashboard(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		devices: []store.Device{
			{ID: "20000000-0000-0000-0000-000000000002", Name: "Pixel Tablet", Platform: "Android 15"},
		},
		sessions: map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	form := url.Values{"csrf_token": {adminCSRFToken(plainToken)}, "name": {"Studio tablet"}}
	request := httptest.NewRequest(http.MethodPost, "/admin/devices/20000000-0000-0000-0000-000000000002/rename", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther || database.devices[0].Name != "Studio tablet" {
		t.Fatalf("rename response = %d, stored name = %q", response.Code, database.devices[0].Name)
	}

	// Without the CSRF token it is somebody else's form post, and the name must not move.
	forged := url.Values{"name": {"Renamed by a stranger"}}
	forgedRequest := httptest.NewRequest(http.MethodPost, "/admin/devices/20000000-0000-0000-0000-000000000002/rename", strings.NewReader(forged.Encode()))
	forgedRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	forgedRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	forgedResponse := httptest.NewRecorder()
	handler.ServeHTTP(forgedResponse, forgedRequest)

	if forgedResponse.Code != http.StatusForbidden || database.devices[0].Name != "Studio tablet" {
		t.Fatalf("forged rename = %d, stored name = %q", forgedResponse.Code, database.devices[0].Name)
	}

	// An empty name would leave a row nothing can be said about, so it is refused rather than stored.
	blank := url.Values{"csrf_token": {adminCSRFToken(plainToken)}, "name": {"   "}}
	blankRequest := httptest.NewRequest(http.MethodPost, "/admin/devices/20000000-0000-0000-0000-000000000002/rename", strings.NewReader(blank.Encode()))
	blankRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	blankRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	blankResponse := httptest.NewRecorder()
	handler.ServeHTTP(blankResponse, blankRequest)

	if blankResponse.Code != http.StatusBadRequest || database.devices[0].Name != "Studio tablet" {
		t.Fatalf("blank rename = %d, stored name = %q", blankResponse.Code, database.devices[0].Name)
	}
}

// TestRevokedDevicesCanBeRemovedFromTheDashboard: revocation is permanent on its own, so the row
// that survives it is a record, not a control. Once the operator has read it, the only thing left
// to do with a phone they no longer own is stop looking at it.
func TestRevokedDevicesCanBeRemovedFromTheDashboard(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	revokedAt := time.Now()
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		devices: []store.Device{
			{ID: "20000000-0000-0000-0000-000000000002", Name: "Old phone", RevokedAt: &revokedAt},
			{ID: "20000000-0000-0000-0000-000000000003", Name: "Studio tablet"},
		},
		sessions: map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	dashboard := httptest.NewRecorder()
	dashboardRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	dashboardRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(dashboard, dashboardRequest)
	if !strings.Contains(dashboard.Body.String(), `/admin/devices/20000000-0000-0000-0000-000000000002/remove`) {
		t.Fatal("the revoked device has no Remove control on the dashboard")
	}
	if strings.Contains(dashboard.Body.String(), `/admin/devices/20000000-0000-0000-0000-000000000003/remove`) {
		t.Fatal("an active device was offered a Remove control")
	}

	// A stale page's forged post must not delete anything, in either direction.
	forged := httptest.NewRequest(http.MethodPost, "/admin/devices/20000000-0000-0000-0000-000000000002/remove", strings.NewReader(""))
	forged.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	forged.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	forgedResponse := httptest.NewRecorder()
	handler.ServeHTTP(forgedResponse, forged)
	if forgedResponse.Code != http.StatusForbidden || len(database.devices) != 2 {
		t.Fatalf("forged removal = %d, devices left = %d", forgedResponse.Code, len(database.devices))
	}

	// An active device is refused rather than quietly disconnected.
	form := url.Values{"csrf_token": {adminCSRFToken(plainToken)}}
	activeRequest := httptest.NewRequest(http.MethodPost, "/admin/devices/20000000-0000-0000-0000-000000000003/remove", strings.NewReader(form.Encode()))
	activeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	activeRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	activeResponse := httptest.NewRecorder()
	handler.ServeHTTP(activeResponse, activeRequest)
	if activeResponse.Code != http.StatusConflict || len(database.devices) != 2 {
		t.Fatalf("removing an active device = %d, devices left = %d", activeResponse.Code, len(database.devices))
	}

	removeRequest := httptest.NewRequest(http.MethodPost, "/admin/devices/20000000-0000-0000-0000-000000000002/remove", strings.NewReader(form.Encode()))
	removeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	removeRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	removeResponse := httptest.NewRecorder()
	handler.ServeHTTP(removeResponse, removeRequest)
	if removeResponse.Code != http.StatusSeeOther || len(database.devices) != 1 {
		t.Fatalf("removal = %d, devices left = %d", removeResponse.Code, len(database.devices))
	}
	if database.devices[0].ID != "20000000-0000-0000-0000-000000000003" {
		t.Fatalf("removal took the wrong row: %q survived", database.devices[0].ID)
	}

	// Removing it a second time is a 404 rather than a 500: the row is already gone.
	repeatRequest := httptest.NewRequest(http.MethodPost, "/admin/devices/20000000-0000-0000-0000-000000000002/remove", strings.NewReader(form.Encode()))
	repeatRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	repeatRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	repeatResponse := httptest.NewRecorder()
	handler.ServeHTTP(repeatResponse, repeatRequest)
	if repeatResponse.Code != http.StatusNotFound {
		t.Fatalf("repeat removal = %d, want 404", repeatResponse.Code)
	}
}

// TestAllRevokedDevicesCanBeClearedAtOnce: one button per row means a list nobody finishes
// clearing, so the panel heading offers to take all of them and leaves every live device alone.
func TestAllRevokedDevicesCanBeClearedAtOnce(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	revokedAt := time.Now()
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		devices: []store.Device{
			{ID: "20000000-0000-0000-0000-000000000002", Name: "Old phone", RevokedAt: &revokedAt},
			{ID: "20000000-0000-0000-0000-000000000003", Name: "Studio tablet"},
			{ID: "20000000-0000-0000-0000-000000000004", Name: "Lost tablet", RevokedAt: &revokedAt},
		},
		sessions: map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	dashboard := httptest.NewRecorder()
	dashboardRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	dashboardRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(dashboard, dashboardRequest)
	if !strings.Contains(dashboard.Body.String(), "Remove 2 revoked") {
		t.Fatal("the dashboard did not offer to clear the revoked devices, or miscounted them")
	}

	form := url.Values{"csrf_token": {adminCSRFToken(plainToken)}}
	request := httptest.NewRequest(http.MethodPost, "/admin/devices/remove-revoked", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther || len(database.devices) != 1 {
		t.Fatalf("bulk removal = %d, devices left = %d", response.Code, len(database.devices))
	}
	if database.devices[0].ID != "20000000-0000-0000-0000-000000000003" {
		t.Fatalf("bulk removal took a live device: %q survived", database.devices[0].ID)
	}

	// With nothing revoked the offer disappears, and the endpoint stays harmless if it is posted
	// to anyway -- the operator asked for a list with no revoked rows and that is already true.
	clearedDashboard := httptest.NewRecorder()
	clearedRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	clearedRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(clearedDashboard, clearedRequest)
	if strings.Contains(clearedDashboard.Body.String(), "revoked</button>") {
		t.Fatal("the bulk removal button survived an empty revoked list")
	}

	repeat := httptest.NewRecorder()
	repeatRequest := httptest.NewRequest(http.MethodPost, "/admin/devices/remove-revoked", strings.NewReader(form.Encode()))
	repeatRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	repeatRequest.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(repeat, repeatRequest)
	if repeat.Code != http.StatusSeeOther || len(database.devices) != 1 {
		t.Fatalf("repeat bulk removal = %d, devices left = %d", repeat.Code, len(database.devices))
	}
}

// TestDashboardCountsAndNamesNotebooks: the count is the question the operator asks first and the
// names are the follow-up, so both are on the page and the names are one click away rather than
// one request away.
func TestDashboardCountsAndNamesNotebooks(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC)
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		notebooks: []store.NotebookSummary{
			{ID: "nb-1", Name: "Field notes", ServerUpdatedAt: updatedAt},
			{ID: "nb-2", Name: `<img src=x onerror="alert(1)">`, ServerUpdatedAt: updatedAt},
		},
		sessions: map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(response, request)
	body := response.Body.String()

	if response.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", response.Code)
	}
	if !strings.Contains(body, `data-tab="notebooks">Notebooks <span class="count-badge">2</span>`) {
		t.Fatal("the notebooks tab did not show the count")
	}
	if !strings.Contains(body, "Field notes") {
		t.Fatal("the notebook names are absent from the dashboard")
	}
	// The devices tab is what the dashboard opens on, so the notebook panel ships hidden.
	if !strings.Contains(body, `<div data-panel="notebooks" hidden>`) {
		t.Fatal("the notebooks panel is not hidden on the devices tab")
	}
	// A notebook name is user content that reached the server over sync, so it gets escaped like
	// any other -- naming a notebook after a script tag must not make it one.
	if strings.Contains(body, "<img src=x") {
		t.Fatal("a notebook name was written without HTML escaping")
	}
	if !strings.Contains(body, "&lt;img src=x") {
		t.Fatal("the escaped notebook name is missing")
	}
}

// TestDashboardShowsClientDeletesInTheArchive: a client deletion remains a sync tombstone in the
// database, and the dashboard gives that durable row a place of its own instead of making it look
// as though the server discarded it.
// TestNotebooksTabDividesSyncedFromCloudHosted: the two groups differ in where the bytes are, which
// is the difference the operator is looking at the tab to see. A single list would have to say it in
// prose on every row.
func TestNotebooksTabDividesSyncedFromCloudHosted(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		notebooks: []store.NotebookSummary{
			{ID: "nb-1", Name: "Field notes", ServerUpdatedAt: updatedAt},
			{ID: "nb-2", Name: "Old sketches", ServerUpdatedAt: updatedAt, Closed: true},
		},
		cloudNotebooks: []store.NotebookSummary{
			{ID: "nb-3", Name: "Archive 2019", ServerUpdatedAt: updatedAt, Closed: true},
		},
		sessions: map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin?tab=notebooks", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(response, request)
	body := response.Body.String()

	if response.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", response.Code)
	}
	// The badge counts the tab, not one of the two groups under it. Matched without the opening
	// tag because a selected tab carries an aria-current attribute the devices tab does not.
	if !strings.Contains(body, `>Notebooks <span class="count-badge">3</span>`) {
		t.Fatal("the notebooks tab badge does not count both groups")
	}
	syncedHeading := strings.Index(body, `<h3>Synced <span class="count-badge">2</span>`)
	cloudHeading := strings.Index(body, `<h3>On cloud <span class="count-badge">1</span>`)
	if syncedHeading < 0 || cloudHeading < 0 {
		t.Fatal("the notebooks tab is missing one of its two dividers")
	}
	if syncedHeading > cloudHeading {
		t.Fatal("the cloud group is above the synced one, which reads as the exception being the rule")
	}
	if !strings.Contains(body, "Archive 2019") || !strings.Contains(body, "Field notes") {
		t.Fatal("a notebook name is missing from the tab")
	}
	// Only a cloud-hosted notebook can be unhosted, so only its rows carry the form.
	if strings.Count(body, `action="/admin/notebooks/stop-hosting"`) != 1 {
		t.Fatalf("stop-hosting forms = %d, want exactly the one cloud notebook",
			strings.Count(body, `action="/admin/notebooks/stop-hosting"`))
	}
	// Closed is worth saying on the synced list, where it distinguishes two rows. It is not worth
	// saying on the cloud list, where every row is closed by definition.
	if strings.Count(body, `<span class="record-state">Closed</span>`) != 1 {
		t.Fatal("the Closed badge is not on exactly the one shelved-but-present notebook")
	}
}

// TestNotebooksTabWithNothingOnTheCloudShowsNoDivider: a divider appears when there is something to
// divide. With every notebook on the devices the distinction is one the operator cannot act on.
func TestNotebooksTabWithNothingOnTheCloudShowsNoDivider(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		notebooks:    []store.NotebookSummary{{ID: "nb-1", Name: "Field notes"}},
		sessions:     map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin?tab=notebooks", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(response, request)
	body := response.Body.String()

	if strings.Contains(body, "On cloud") {
		t.Fatal("an empty cloud group was still given a heading")
	}
	if strings.Contains(body, "/admin/notebooks/stop-hosting") {
		t.Fatal("a stop-hosting form was rendered with nothing on the cloud")
	}
}

// TestStopHostingRequiresCSRFAndReturnsToTheNotebooks: the same shape as every other destructive
// admin action, and it lands back on the tab it acted on rather than at the top of the page.
func TestStopHostingRequiresCSRFAndReturnsToTheNotebooks(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount:   1,
		account:        store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		notebooks:      []store.NotebookSummary{{ID: "nb-live", Name: "Work"}},
		cloudNotebooks: []store.NotebookSummary{{ID: "nb-cloud", Name: "Archive 2019", Closed: true}},
		sessions:       map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	post := func(notebookID string, csrf string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"notebook_id": {notebookID}, "csrf_token": {csrf}}
		request := httptest.NewRequest(http.MethodPost, "/admin/notebooks/stop-hosting", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	if response := post("nb-cloud", "wrong"); response.Code != http.StatusForbidden {
		t.Fatalf("stop hosting without valid CSRF = %d, want 403", response.Code)
	}
	if len(database.cloudNotebooks) != 1 {
		t.Fatal("invalid CSRF deleted the notebook anyway")
	}

	// A notebook the devices still hold is refused by the store, and the panel says why.
	if response := post("nb-live", adminCSRFToken(plainToken)); response.Code != http.StatusConflict {
		t.Fatalf("stop hosting a synced notebook = %d, want 409", response.Code)
	}

	response := post("nb-cloud", adminCSRFToken(plainToken))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin?tab=notebooks#contents" {
		t.Fatalf("successful stop hosting = %d %q, want 303 back to Notebooks", response.Code, response.Header().Get("Location"))
	}
	if fmt.Sprint(database.unhostedNotebookIDs) != "[nb-cloud]" || len(database.cloudNotebooks) != 0 {
		t.Fatalf("unhosted ids = %v with %d cloud rows left, want only nb-cloud", database.unhostedNotebookIDs, len(database.cloudNotebooks))
	}
}

func TestDashboardShowsClientDeletesInTheArchive(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	archivedAt := time.Date(2026, 8, 18, 14, 45, 0, 0, time.UTC)
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		archivedNotebooks: []store.NotebookSummary{
			{ID: "nb-old", Name: `<script>alert("archived")</script>`, ServerUpdatedAt: archivedAt},
		},
		sessions: map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin?tab=archived", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(response, request)
	body := response.Body.String()

	if response.Code != http.StatusOK {
		t.Fatalf("archive status = %d, want 200", response.Code)
	}
	if !strings.Contains(body, `data-tab="archived" aria-current="page">Archived <span class="count-badge">1</span>`) {
		t.Fatal("the archived tab did not show its count or selected state")
	}
	if !strings.Contains(body, `<div data-panel="archived">`) || !strings.Contains(body, `<div data-panel="notebooks" hidden>`) {
		t.Fatal("the archived panel was not the only visible notebook panel")
	}
	if !strings.Contains(body, "Archived Aug 18, 2026") {
		t.Fatal("the archived row did not show the server-confirmed archive date")
	}
	if strings.Contains(body, `<script>alert("archived")</script>`) || !strings.Contains(body, `&lt;script&gt;alert`) {
		t.Fatal("an archived notebook name was not HTML-escaped")
	}
	if !strings.Contains(body, `action="/admin/notebooks/delete"`) || !strings.Contains(body, `name="notebook_id" value="nb-old"`) {
		t.Fatal("the archived row has no permanent-delete form for its notebook id")
	}
	if !strings.Contains(body, `data-confirm-message="Permanently delete this archived notebook and all of its contents? Every device drops its copy the next time it connects. This cannot be undone."`) {
		t.Fatal("the permanent-delete form does not ask for destructive confirmation")
	}
	// No device can hold this back and the markup must not suggest one can: the deletion happens
	// now and the devices are told about it afterwards.
	if strings.Contains(body, `class="button-danger" disabled`) {
		t.Fatal("the archive offered a disabled permanent-delete button")
	}
}

func TestPermanentNotebookDeletionRequiresCSRFAndReturnsToTheArchive(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount:      1,
		account:           store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		archivedNotebooks: []store.NotebookSummary{{ID: "nb-old", Name: "Old field notes"}},
		sessions:          map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	post := func(csrf string) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"notebook_id": {"nb-old"}, "csrf_token": {csrf}}
		request := httptest.NewRequest(http.MethodPost, "/admin/notebooks/delete", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	if response := post("wrong"); response.Code != http.StatusForbidden {
		t.Fatalf("delete without valid CSRF = %d, want 403", response.Code)
	}
	if len(database.archivedNotebooks) != 1 {
		t.Fatal("invalid CSRF permanently deleted the notebook")
	}

	response := post(adminCSRFToken(plainToken))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin?tab=archived#contents" {
		t.Fatalf("successful delete = %d %q, want 303 back to Archived", response.Code, response.Header().Get("Location"))
	}
	if len(database.archivedNotebooks) != 0 || fmt.Sprint(database.deletedNotebookIDs) != "[nb-old]" {
		t.Fatalf("deleted ids = %v with %d archive rows left, want only nb-old removed", database.deletedNotebookIDs, len(database.archivedNotebooks))
	}
}

// TestPermanentNotebookDeletionRefusesALiveNotebook: the archive's action is housekeeping that
// follows a deletion, never a second way to perform one. The markup that offered it is stale, and
// markup is not an authority boundary.
func TestPermanentNotebookDeletionRefusesALiveNotebook(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount:      1,
		account:           store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		archivedNotebooks: []store.NotebookSummary{{ID: "nb-old", Name: "Old field notes"}},
		deleteNotebookErr: store.ErrNotebookNotArchived,
		sessions:          map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	form := url.Values{"notebook_id": {"nb-old"}, "csrf_token": {adminCSRFToken(plainToken)}}
	request := httptest.NewRequest(http.MethodPost, "/admin/notebooks/delete", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "Only a notebook deleted by a client") {
		t.Fatalf("deleting a live notebook = %d %q, want an explanatory 409", response.Code, response.Body.String())
	}
}

// TestDashboardSaysHowManyNotebookNamesItLeftOut: the list is a disclosure in a statistics tile,
// not an inventory screen, so it stops -- and says that it stopped rather than showing a prefix of
// the truth as if it were all of it.
func TestDashboardSaysHowManyNotebookNamesItLeftOut(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	notebooks := make([]store.NotebookSummary, maxDashboardNotebooks+3)
	for index := range notebooks {
		notebooks[index] = store.NotebookSummary{ID: fmt.Sprintf("nb-%d", index), Name: fmt.Sprintf("Notebook %d", index)}
	}
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		notebooks:    notebooks,
		sessions:     map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(response, request)
	body := response.Body.String()

	if !strings.Contains(body, fmt.Sprintf(`<span class="count-badge">%d</span>`, len(notebooks))) {
		t.Fatal("the count reported only the rendered names rather than every notebook")
	}
	if !strings.Contains(body, "3 more not shown.") {
		t.Fatal("the dashboard silently truncated the notebook list")
	}
}

// TestTabsWorkWithoutTheBrowserScript: the tabs are links the server answers, not scripted
// buttons, so switching works with the script blocked and the script only removes the round trip.
func TestTabsWorkWithoutTheBrowserScript(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount:      1,
		account:           store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		devices:           []store.Device{{ID: "20000000-0000-0000-0000-000000000002", Name: "Studio tablet"}},
		notebooks:         []store.NotebookSummary{{ID: "nb-1", Name: "Field notes"}},
		archivedNotebooks: []store.NotebookSummary{{ID: "nb-old", Name: "Old field notes"}},
		sessions:          map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	fetch := func(path string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, response.Code)
		}
		return response.Body.String()
	}

	devicesTab := fetch("/admin")
	if !strings.Contains(devicesTab, `<div data-panel="devices">`) || !strings.Contains(devicesTab, `<div data-panel="notebooks" hidden>`) {
		t.Fatal("/admin did not open on the devices tab")
	}

	notebooksTab := fetch("/admin?tab=notebooks")
	if !strings.Contains(notebooksTab, `<div data-panel="notebooks">`) || !strings.Contains(notebooksTab, `<div data-panel="devices" hidden>`) {
		t.Fatal("?tab=notebooks did not switch the visible panel")
	}
	// Removing revoked devices is a device action, so its button has no business being on screen
	// while the notebooks are.
	if !strings.Contains(notebooksTab, `<div class="panel-actions" data-panel="devices" hidden>`) {
		t.Fatal("the device actions stayed visible on the notebooks tab")
	}

	archivedTab := fetch("/admin?tab=archived")
	if !strings.Contains(archivedTab, `<div data-panel="archived">`) || !strings.Contains(archivedTab, `<div data-panel="notebooks" hidden>`) || !strings.Contains(archivedTab, `<div data-panel="devices" hidden>`) {
		t.Fatal("?tab=archived did not switch the visible panel")
	}

	// A value nobody meant lands on the devices tab rather than on an error page.
	if nonsense := fetch("/admin?tab=wat"); !strings.Contains(nonsense, `<div data-panel="devices">`) {
		t.Fatal("an unrecognised tab did not fall back to devices")
	}
}

// TestDeviceActionsReturnToTheRowTheyActedOn: a bare /admin redirect reloads at the top of the
// page, so an operator working down a list loses their place on every click. This is the bug that
// makes the panel annoying to actually use.
func TestDeviceActionsReturnToTheRowTheyActedOn(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	revokedAt := time.Now()
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		devices: []store.Device{
			{ID: "20000000-0000-0000-0000-000000000002", Name: "Studio tablet"},
			{ID: "20000000-0000-0000-0000-000000000003", Name: "Old phone", RevokedAt: &revokedAt},
		},
		sessions: map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	post := func(path string, form url.Values) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusSeeOther {
			t.Fatalf("POST %s = %d, want 303", path, response.Code)
		}
		return response.Header().Get("Location")
	}

	csrf := url.Values{"csrf_token": {adminCSRFToken(plainToken)}}
	rename := url.Values{"csrf_token": {adminCSRFToken(plainToken)}, "name": {"Studio tablet"}}

	// Rename and revoke leave the row in place, so they land on it.
	if location := post("/admin/devices/20000000-0000-0000-0000-000000000002/rename", rename); location != "/admin?tab=devices#device-20000000-0000-0000-0000-000000000002" {
		t.Fatalf("rename redirected to %q, want the row it renamed", location)
	}
	if location := post("/admin/devices/20000000-0000-0000-0000-000000000002/revoke", csrf); location != "/admin?tab=devices#device-20000000-0000-0000-0000-000000000002" {
		t.Fatalf("revoke redirected to %q, want the row it revoked", location)
	}

	// Removal deletes the row, so there is nothing to land on but the list.
	if location := post("/admin/devices/20000000-0000-0000-0000-000000000003/remove", csrf); location != "/admin?tab=devices#contents" {
		t.Fatalf("removal redirected to %q, want the device list", location)
	}
	if location := post("/admin/devices/remove-revoked", csrf); location != "/admin?tab=devices#contents" {
		t.Fatalf("bulk removal redirected to %q, want the device list", location)
	}
}

// TestAdminAssetsAreContentAddressed pins the fix for a genuinely nasty upgrade bug: the pages are
// no-store, so a rebuilt server serves new markup at once, while a stylesheet at a fixed path stays
// in the browser cache. New class names against the old stylesheet is not a subtle degradation, it
// is an unstyled page, and it lasts until the cache entry expires.
func TestAdminAssetsAreContentAddressed(t *testing.T) {
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
	}
	handler := newAdminTestHandler(database)

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/login", nil))
	linked := regexp.MustCompile(`href="(/assets/[^"]+\.css)"`).FindStringSubmatch(page.Body.String())
	if linked == nil {
		t.Fatalf("no stylesheet link in the page: %s", page.Body.String())
	}
	if linked[1] == "/assets/admin.css" {
		t.Fatal("the stylesheet is still at a fixed path, so a stale cache can outlive an upgrade")
	}

	// Whatever the markup links to is what the router serves: the two are built from one value, and
	// this is the assertion that keeps them that way.
	asset := httptest.NewRecorder()
	handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, linked[1], nil))
	if asset.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", linked[1], asset.Code)
	}
	if !strings.Contains(asset.Body.String(), ".record-row") {
		t.Fatal("the served stylesheet is not the one the pages are written against")
	}
	if cacheControl := asset.Header().Get("Cache-Control"); !strings.Contains(cacheControl, "immutable") {
		t.Fatalf("asset Cache-Control = %q, want an immutable cache now that the path carries a digest", cacheControl)
	}
}

// TestNotebookIconsAreServedAndAllowedByTheCSP: the two notebook rows carry drawings rather than
// glyphs, which means three things have to agree -- the markup, the router, and the policy header.
// The last one is the trap: `default-src 'none'` covers images, so an <img> added without amending
// the CSP renders an alt-text stub and a console warning on a page that still passes every other
// test.
func TestNotebookIconsAreServedAndAllowedByTheCSP(t *testing.T) {
	plainToken, tokenHash, err := auth.MintAdminSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	database := &fakeAdminStore{
		accountCount: 1,
		account:      store.Account{ID: "10000000-0000-0000-0000-000000000001", Email: "owner@example.com"},
		notebooks:    []store.NotebookSummary{{ID: "nb-live", Name: "Work"}},
		cloudNotebooks: []store.NotebookSummary{
			{ID: "nb-cloud", Name: "Archive 2019", Closed: true},
		},
		sessions: map[string]string{string(tokenHash): "10000000-0000-0000-0000-000000000001"},
	}
	handler := newAdminTestHandler(database)

	page := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin?tab=notebooks", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: plainToken})
	handler.ServeHTTP(page, request)
	body := page.Body.String()

	policy := page.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "img-src 'self'") {
		t.Fatalf("CSP = %q, which blocks the notebook drawings the page just asked for", policy)
	}
	if strings.Contains(policy, "img-src *") || strings.Contains(policy, "img-src 'self' data:") {
		t.Fatalf("CSP = %q, wider than it needs to be: every image is served from this binary", policy)
	}

	for _, icon := range []struct {
		name   string
		path   string
		marker string
	}{
		{name: "synced notebook", path: notebookIcon.Path, marker: `class="record-icon notebook"`},
		{name: "cloud notebook", path: cloudNotebookIcon.Path, marker: `class="record-icon cloud"`},
	} {
		if !strings.Contains(body, icon.marker+`><img src="`+icon.path+`"`) {
			t.Fatalf("the %s row does not draw %s", icon.name, icon.path)
		}
		if strings.HasSuffix(icon.path, "/assets/notebook.svg") {
			t.Fatalf("the %s icon is at a fixed path, so a stale cache can outlive an upgrade", icon.name)
		}

		// Whatever the markup asks for is what the router answers. The two come from one value, and
		// this is the assertion that keeps them that way.
		asset := httptest.NewRecorder()
		handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, icon.path, nil))
		if asset.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", icon.path, asset.Code)
		}
		if contentType := asset.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "image/svg+xml") {
			t.Fatalf("%s Content-Type = %q, want image/svg+xml", icon.path, contentType)
		}
		if !strings.Contains(asset.Body.String(), "<svg") {
			t.Fatalf("%s did not answer with a drawing", icon.path)
		}
		if cacheControl := asset.Header().Get("Cache-Control"); !strings.Contains(cacheControl, "immutable") {
			t.Fatalf("%s Cache-Control = %q, want an immutable cache now that the path carries a digest", icon.path, cacheControl)
		}
	}

	// The two rows must not end up drawing the same picture, which is what a copy-paste in the
	// template looks like and what nothing else here would catch.
	if notebookIcon.Path == cloudNotebookIcon.Path {
		t.Fatal("both notebook rows draw the same icon")
	}
}

// TestContentAddressedPathTracksItsContent: the digest is the whole mechanism, so it has to change
// when the bytes do and hold still when they do not.
func TestContentAddressedPathTracksItsContent(t *testing.T) {
	first := contentAddressedPath("admin", "css", "body { color: red }")
	again := contentAddressedPath("admin", "css", "body { color: red }")
	changed := contentAddressedPath("admin", "css", "body { color: blue }")

	if first != again {
		t.Fatalf("the same content produced %q and %q, so every restart would bust the cache", first, again)
	}
	if first == changed {
		t.Fatal("changed content produced the same path, which is the bug this exists to prevent")
	}
	if !strings.HasPrefix(first, "/assets/admin.") || !strings.HasSuffix(first, ".css") {
		t.Fatalf("path = %q, want it still recognisable as admin.css", first)
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
