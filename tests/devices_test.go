package tests

import (
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

// registerInstallation signs in through the real endpoint with an installation id, the way the app
// does, and returns the device it was given along with the status.
func (fixture *syncFixture) registerInstallation(name string, installationID string) (*deviceClient, int) {
	fixture.t.Helper()

	var registered struct {
		DeviceID string `json:"deviceId"`
		Token    string `json:"token"`
	}
	status := fixture.request(http.MethodPost, "/v1/devices", "", map[string]any{
		"email":          fixture.email,
		"password":       testAccountPassword,
		"name":           name,
		"platform":       "Test",
		"installationId": installationID,
	}, &registered)
	return &deviceClient{
		t: fixture.t, fixture: fixture, name: name, token: registered.Token, deviceID: registered.DeviceID,
	}, status
}

type listedDevice struct {
	DeviceID      string     `json:"deviceId"`
	Name          string     `json:"name"`
	LastPulledSeq int64      `json:"lastPulledSeq"`
	RevokedAt     *time.Time `json:"revokedAt"`
}

func (device *deviceClient) listDevices() []listedDevice {
	device.t.Helper()
	var listed struct {
		Devices []listedDevice `json:"devices"`
	}
	if status := device.fixture.request(http.MethodGet, "/v1/devices", device.token, nil, &listed); status != http.StatusOK {
		device.t.Fatalf("listing devices: status %d", status)
	}
	return listed.Devices
}

// Signing in again from the same installation is the same tablet, so it keeps its one row and only
// its token changes. Before installation ids every sign-in added a row, and the device list grew
// one stale entry per re-login.
func TestSigningInAgainFromOneInstallationReusesItsDeviceRow(t *testing.T) {
	fixture := newSyncFixture(t)
	const installationID = "6f1d2c3b-4a59-4e68-9d7c-0b1a2c3d4e5f"

	first, status := fixture.registerInstallation("Studio tablet", installationID)
	if status != http.StatusCreated {
		t.Fatalf("first sign-in = %d, want 201", status)
	}

	// Move the row's acknowledged cursor off zero, so the reset on reuse is observable.
	first.push(notebookChange("nb-installation", 0, "Installation"))
	first.pull(first.cursor(), 0)
	if before := first.listDevices(); len(before) != 1 || before[0].LastPulledSeq == 0 {
		t.Fatalf("the fixture meant to leave one device with an acknowledged cursor, got %+v", before)
	}

	second, status := fixture.registerInstallation("Studio tablet (renamed)", installationID)
	if status != http.StatusCreated {
		t.Fatalf("second sign-in = %d, want 201", status)
	}
	if second.deviceID != first.deviceID {
		t.Fatalf("second sign-in got device %s, want the installation's existing %s", second.deviceID, first.deviceID)
	}
	if second.token == first.token {
		t.Fatal("signing in again returned the same token; it must be replaced")
	}

	if status := fixture.request(http.MethodGet, "/v1/cursor", first.token, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("the replaced token still answers %d, want 401", status)
	}

	devices := second.listDevices()
	if len(devices) != 1 {
		t.Fatalf("the account lists %d devices after two sign-ins from one installation, want 1", len(devices))
	}
	if devices[0].Name != "Studio tablet (renamed)" {
		t.Fatalf("device name = %q, want the one sent with the latest sign-in", devices[0].Name)
	}
	if devices[0].LastPulledSeq != 0 {
		t.Fatalf("lastPulledSeq = %d after signing in again, want 0 like a new device", devices[0].LastPulledSeq)
	}
}

// A client that sends no installation id keeps the old behaviour: one row per sign-in.
func TestSignInsWithoutAnInstallationIDEachAddARow(t *testing.T) {
	fixture := newSyncFixture(t)
	fixture.registerDevice("first")
	second := fixture.registerDevice("second")

	if devices := second.listDevices(); len(devices) != 2 {
		t.Fatalf("two sign-ins without an installation id left %d devices, want 2", len(devices))
	}
}

// An installation revoked from another device and then signed in again with the password is live
// again on its old row, as a fresh registration would have been on a new one.
func TestARevokedInstallationThatSignsInAgainIsLiveAgain(t *testing.T) {
	fixture := newSyncFixture(t)
	const installationID = "0a1b2c3d-4e5f-4a6b-8c7d-8e9f0a1b2c3d"

	tablet, _ := fixture.registerInstallation("tablet", installationID)
	phone := fixture.registerDevice("phone")
	if status := fixture.request(http.MethodDelete, "/v1/devices/"+tablet.deviceID, phone.token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoking the tablet = %d, want 204", status)
	}

	again, status := fixture.registerInstallation("tablet", installationID)
	if status != http.StatusCreated || again.deviceID != tablet.deviceID {
		t.Fatalf("signing in again = %d on device %s, want 201 on %s", status, again.deviceID, tablet.deviceID)
	}
	if status := fixture.request(http.MethodGet, "/v1/cursor", again.token, nil, nil); status != http.StatusOK {
		t.Fatalf("the new token answers %d, want 200", status)
	}
	for _, device := range phone.listDevices() {
		if device.DeviceID == tablet.deviceID && device.RevokedAt != nil {
			t.Fatal("the tablet's row is still revoked after it signed in again")
		}
	}
}

func TestAMalformedInstallationIDIsRefused(t *testing.T) {
	fixture := newSyncFixture(t)
	if _, status := fixture.registerInstallation("tablet", "not-a-uuid"); status != http.StatusBadRequest {
		t.Fatalf("a malformed installation id = %d, want 400", status)
	}
}

// The stream a replaced token opened must not outlive it. It ends without a `revoked` event,
// because the client that just signed in holds the new token and that event tells it to forget the
// account.
func TestSigningInAgainClosesTheStreamTheOldTokenOpened(t *testing.T) {
	fixture := newSyncFixture(t)
	const installationID = "5e4d3c2b-1a09-4f8e-9d7c-6b5a4f3e2d1c"

	tablet, _ := fixture.registerInstallation("tablet", installationID)
	stream := openChangeStream(t, tablet)
	defer stream.close()
	if event := stream.read(t); event != "ready" {
		t.Fatalf("first stream event = %q, want ready", event)
	}

	if _, status := fixture.registerInstallation("tablet", installationID); status != http.StatusCreated {
		t.Fatalf("signing in again = %d, want 201", status)
	}

	ended := make(chan error, 1)
	go func() {
		frame, err := readChangeStreamEvent(stream.reader)
		if err == nil {
			err = errors.New("received event " + frame.name)
		}
		ended <- err
	}()
	select {
	case err := <-ended:
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("the old token's stream = %v, want it closed with no event", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old token's stream was still open five seconds after the token was replaced")
	}
}
