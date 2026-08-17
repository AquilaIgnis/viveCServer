package tests

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/httpapi"
	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/internal/syncengine"
	"github.com/AquilaIgnis/viveCServer/migrations"
)

const testDatabaseURLSetting = "VIVE_TEST_DATABASE_URL"

// testAccountPassword is shared by every test account. These accounts exist for the length of one
// `go test` run against a throwaway database.
const testAccountPassword = "correct horse battery staple"

var (
	sharedPoolOnce  sync.Once
	sharedPool      *pgxpool.Pool
	sharedPoolError error
)

func TestMain(m *testing.M) {
	exitCode := m.Run()
	if sharedPool != nil {
		sharedPool.Close()
	}
	os.Exit(exitCode)
}

// testDatabaseURL finds the database to run against, or explains how to start one.
//
// Skipping locally and failing in CI is deliberate. A suite that quietly skips is a suite that
// stops running the day someone's Docker daemon is not started, and nobody notices until the
// property it was protecting has already been broken.
func testDatabaseURL(t *testing.T) string {
	t.Helper()

	databaseURL := os.Getenv(testDatabaseURLSetting)
	if databaseURL != "" {
		return databaseURL
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("%s must be set in CI: the integration suite is not allowed to skip there", testDatabaseURLSetting)
	}
	t.Skipf(`%s is not set, so the integration suite is skipped. To run it:

  docker run --rm -d --name vive-test-postgres -e POSTGRES_PASSWORD=postgres -p 5433:5432 postgres:18
  %s='postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable' go test ./tests/...
  docker rm -f vive-test-postgres
`, testDatabaseURLSetting, testDatabaseURLSetting)
	return ""
}

// testPool migrates the database once per run and hands out one shared pool.
//
// Tests are not isolated by truncating tables between them. They are isolated by account, which is
// the boundary the server enforces in production: if two tests could see each other's rows, that
// would itself be the bug worth failing on.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testDatabaseURL(t)

	sharedPoolOnce.Do(func() {
		ctx := context.Background()
		if sharedPoolError = store.ApplyMigrations(ctx, databaseURL, migrations.Files, discardingLogger()); sharedPoolError != nil {
			return
		}
		sharedPool, sharedPoolError = store.OpenPool(ctx, databaseURL)
	})
	if sharedPoolError != nil {
		t.Fatalf("preparing the test database: %v", sharedPoolError)
	}
	return sharedPool
}

func discardingLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// syncFixture is one running sync listener and one account on it.
type syncFixture struct {
	t         *testing.T
	pool      *pgxpool.Pool
	baseURL   string
	accountID string
	email     string
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	pool := testPool(t)

	handler := httpapi.NewSyncHandler(pool, discardingLogger(), httpapi.Options{
		SignupMode: config.SignupModeClosed,

		// Long enough that no test can age out of it while running, short enough to be a real value
		// rather than "forever".
		BatchReplayWindow: time.Hour,
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	passwordHash, err := auth.HashPassword(testAccountPassword)
	if err != nil {
		t.Fatalf("hashing the test password: %v", err)
	}
	email := "s2-" + randomToken(t) + "@example.test"
	accountID, err := store.CreateAccount(context.Background(), pool, email, passwordHash)
	if err != nil {
		t.Fatalf("creating a test account: %v", err)
	}

	return &syncFixture{t: t, pool: pool, baseURL: server.URL, accountID: accountID, email: email}
}

// registerDevice goes through the real registration endpoint rather than inserting a row, so every
// test authenticates with a token the server actually minted.
func (fixture *syncFixture) registerDevice(name string) *deviceClient {
	fixture.t.Helper()

	var registered struct {
		DeviceID  string `json:"deviceId"`
		AccountID string `json:"accountId"`
		Token     string `json:"token"`
	}
	status := fixture.request(http.MethodPost, "/v1/devices", "", map[string]any{
		"email":    fixture.email,
		"password": testAccountPassword,
		"name":     name,
		"platform": "Test",
	}, &registered)
	if status != http.StatusCreated {
		fixture.t.Fatalf("registering device %q: status %d", name, status)
	}

	return &deviceClient{t: fixture.t, fixture: fixture, name: name, token: registered.Token, deviceID: registered.DeviceID}
}

// request performs one HTTP call and decodes the body, returning the status so a caller can assert
// on failures as easily as on successes.
func (fixture *syncFixture) request(method string, path string, token string, body any, into any) int {
	fixture.t.Helper()
	status, err := fixture.tryRequest(method, path, token, body, into)
	if err != nil {
		fixture.t.Fatalf("%s %s: %v", method, path, err)
	}
	return status
}

// tryRequest is the error-returning form, for the concurrency tests: t.Fatalf may only be called
// from the goroutine running the test.
func (fixture *syncFixture) tryRequest(method string, path string, token string, body any, into any) (int, error) {
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encoding the request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, fixture.baseURL+path, requestBody)
	if err != nil {
		return 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, err
	}
	if into != nil && len(responseBody) > 0 {
		if err := json.Unmarshal(responseBody, into); err != nil {
			return response.StatusCode, fmt.Errorf("decoding %s: %w", responseBody, err)
		}
	}
	return response.StatusCode, nil
}

// deviceClient is one registered device talking to the sync listener.
type deviceClient struct {
	t        *testing.T
	fixture  *syncFixture
	name     string
	token    string
	deviceID string
}

// push sends a batch under a fresh idempotency key and fails the test if the server refuses the
// request outright. Per-entity rejections are part of the result, not a failure.
func (device *deviceClient) push(changes ...map[string]any) syncengine.PushResult {
	device.t.Helper()
	return device.pushBatch(newBatchID(device.t), changes...)
}

func (device *deviceClient) pushBatch(batchID string, changes ...map[string]any) syncengine.PushResult {
	device.t.Helper()
	result, status, err := device.tryPush(batchID, changes...)
	if err != nil {
		device.t.Fatalf("%s push: %v", device.name, err)
	}
	if status != http.StatusOK {
		device.t.Fatalf("%s push: status %d", device.name, status)
	}
	return result
}

func (device *deviceClient) tryPush(batchID string, changes ...map[string]any) (syncengine.PushResult, int, error) {
	if changes == nil {
		changes = []map[string]any{}
	}
	var result syncengine.PushResult
	status, err := device.fixture.tryRequest(http.MethodPost, "/v1/changes", device.token, map[string]any{
		"batchId": batchID,
		"changes": changes,
	}, &result)
	return result, status, err
}

func (device *deviceClient) pull(sinceCursor int64, limit int) syncengine.PullResult {
	device.t.Helper()
	result, status, err := device.tryPull(sinceCursor, limit)
	if err != nil {
		device.t.Fatalf("%s pull: %v", device.name, err)
	}
	if status != http.StatusOK {
		device.t.Fatalf("%s pull: status %d", device.name, status)
	}
	return result
}

func (device *deviceClient) tryPull(sinceCursor int64, limit int) (syncengine.PullResult, int, error) {
	path := fmt.Sprintf("/v1/changes?since=%d", sinceCursor)
	if limit > 0 {
		path += fmt.Sprintf("&limit=%d", limit)
	}
	var result syncengine.PullResult
	status, err := device.fixture.tryRequest(http.MethodGet, path, device.token, nil, &result)
	return result, status, err
}

func (device *deviceClient) cursor() int64 {
	device.t.Helper()
	var result struct {
		Cursor int64 `json:"cursor"`
	}
	status := device.fixture.request(http.MethodGet, "/v1/cursor", device.token, nil, &result)
	if status != http.StatusOK {
		device.t.Fatalf("%s cursor: status %d", device.name, status)
	}
	return result.Cursor
}

// pullEverything drains the delta from a cursor, following hasMore the way a real client does.
func (device *deviceClient) pullEverything(sinceCursor int64, limit int) ([]map[string]any, int64) {
	device.t.Helper()

	collected := make([]map[string]any, 0)
	cursor := sinceCursor
	for range 100 {
		page := device.pull(cursor, limit)
		for _, raw := range page.Changes {
			collected = append(collected, decodeChangeObject(device.t, raw))
		}
		cursor = page.Cursor
		if !page.HasMore {
			return collected, cursor
		}
	}
	device.t.Fatal("pulling did not finish within 100 pages")
	return nil, 0
}

func decodeChangeObject(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decoding a change: %v", err)
	}
	return object
}

// changesByID indexes a pulled delta so assertions can name what they are looking for.
func changesByID(changes []map[string]any) map[string]map[string]any {
	byID := make(map[string]map[string]any, len(changes))
	for _, change := range changes {
		id, _ := change["id"].(string)
		byID[id] = change
	}
	return byID
}

func randomToken(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("reading random bytes: %v", err)
	}
	return hex.EncodeToString(raw)
}

// newBatchID makes a version 4 UUID without a dependency for it: the server only requires that the
// value parses as a UUID and is stable across a retry.
func newBatchID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("reading random bytes: %v", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// Change builders. They spell out every field a client sends so a test reads like the wire.

func notebookChange(id string, baseVersion int64, name string) map[string]any {
	return map[string]any{
		"kind":        "notebook",
		"id":          id,
		"baseVersion": baseVersion,
		"updatedAt":   1_700_000_000_000,
		"name":        name,
		"colorArgb":   -16777216,
		"sortIndex":   0,
		"expanded":    true,
		"createdAt":   1_700_000_000_000,
	}
}

func sectionChange(id string, baseVersion int64, notebookID string, name string) map[string]any {
	return map[string]any{
		"kind":        "section",
		"id":          id,
		"baseVersion": baseVersion,
		"updatedAt":   1_700_000_000_000,
		"notebookId":  notebookID,
		"name":        name,
		"colorArgb":   -16711936,
		"sortIndex":   0,
		"createdAt":   1_700_000_000_000,
	}
}

func pageChange(id string, baseVersion int64, sectionID string, title string) map[string]any {
	return map[string]any{
		"kind":        "page",
		"id":          id,
		"baseVersion": baseVersion,
		"updatedAt":   1_700_000_000_000,
		"sectionId":   sectionID,
		"title":       title,
		"sortIndex":   0,
		"preview":     "",
		"createdAt":   1_700_000_000_000,
	}
}

// pageContentChange addresses a body by its page's id, which is what the app does: `pageId` is the
// primary key of its own `page_content` table. `doc` is a []byte so that `encoding/json` base64s it
// exactly as a real client's serializer will.
func pageContentChange(pageID string, baseVersion int64, doc []byte) map[string]any {
	return map[string]any{
		"kind":        "pageContent",
		"id":          pageID,
		"baseVersion": baseVersion,
		"updatedAt":   1_700_000_000_000,
		"pageId":      pageID,
		"doc":         doc,
		"format":      "json/1",
	}
}

// inkStrokeChange is one stroke as a client sends it.
//
// `drawOrder` and not `seq`: the envelope owns `seq`, and a stroke that sent its draw order under
// that name would have it stripped as an envelope field and stored nowhere.
func inkStrokeChange(id string, baseVersion int64, pageID string, drawOrder int, points []byte) map[string]any {
	return map[string]any{
		"kind":          "inkStroke",
		"id":            id,
		"baseVersion":   baseVersion,
		"updatedAt":     1_700_000_000_000,
		"pageId":        pageID,
		"drawOrder":     drawOrder,
		"brushFamily":   "pressure-pen",
		"brushVersion":  1,
		"sizeDp":        3.5,
		"colorArgb":     -16777216,
		"epsilon":       0.1,
		"stabilization": 2,
		"minX":          1.5,
		"minY":          2.5,
		"maxX":          3.5,
		"maxY":          4.5,
		"points":        points,
		"enc":           "ink/v1",
		"createdAt":     1_700_000_000_000,
	}
}

func inkEraseChange(id string, baseVersion int64, pageID string, targetIDs []string) map[string]any {
	return map[string]any{
		"kind":        "inkErase",
		"id":          id,
		"baseVersion": baseVersion,
		"updatedAt":   1_700_000_000_000,
		"pageId":      pageID,
		"mode":        "Normal",
		"sizeDp":      8.0,
		"points":      []byte{1, 2, 3},
		"enc":         "ink/v1",
		"createdAt":   1_700_000_000_000,
		"targetIds":   targetIDs,
	}
}

func inkMoveChange(id string, baseVersion int64, pageID string, targetIDs []string) map[string]any {
	return map[string]any{
		"kind":        "inkMove",
		"id":          id,
		"baseVersion": baseVersion,
		"updatedAt":   1_700_000_000_000,
		"pageId":      pageID,
		"dxDp":        12.5,
		"dyDp":        -4.25,
		"scaleX":      1.0,
		"scaleY":      1.0,
		"anchorX":     0.0,
		"anchorY":     0.0,
		"points":      []byte{4, 5, 6, 7},
		"enc":         "ink/v1",
		"createdAt":   1_700_000_000_000,
		"targetIds":   targetIDs,
	}
}

// changeOfKind finds the one change of a kind in a delta. Bodies share their page's id, so indexing
// a delta by id alone cannot tell a page from the document hanging off it.
func changeOfKind(t *testing.T, changes []map[string]any, kind string) map[string]any {
	t.Helper()
	for _, change := range changes {
		if change["kind"] == kind {
			return change
		}
	}
	t.Fatalf("no %q change in the delta", kind)
	return nil
}
