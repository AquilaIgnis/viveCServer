// Package changefeed fans committed account writes out to connected devices.
//
// It deliberately carries only a wakeup. The database cursor and delta remain the durable source
// of truth, so notifications may be coalesced without losing a change.
package changefeed

import "sync"

type subscriber struct {
	accountID string
	deviceID  string
	events    chan Event
}

// Event is the complete vocabulary of the hint stream.
type Event uint8

const (
	Changes Event = iota
	Revoked
)

// Broker is an in-memory, non-blocking fan-out for the personal server's live process.
type Broker struct {
	mutex       sync.Mutex
	subscribers map[*subscriber]struct{}
}

func NewBroker() *Broker {
	return &Broker{subscribers: make(map[*subscriber]struct{})}
}

// Subscribe installs one device listener and returns its future wakeups plus an idempotent cancel.
func (broker *Broker) Subscribe(accountID string, deviceID string) (<-chan Event, func()) {
	listener := &subscriber{
		accountID: accountID,
		deviceID:  deviceID,
		events:    make(chan Event, 1),
	}
	broker.mutex.Lock()
	broker.subscribers[listener] = struct{}{}
	broker.mutex.Unlock()

	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			broker.mutex.Lock()
			delete(broker.subscribers, listener)
			close(listener.events)
			broker.mutex.Unlock()
		})
	}
	return listener.events, cancel
}

// Publish wakes this account's other devices. An empty excludedDeviceID wakes every device.
func (broker *Broker) Publish(accountID string, excludedDeviceID string) {
	if broker == nil {
		return
	}

	broker.mutex.Lock()
	defer broker.mutex.Unlock()
	for listener := range broker.subscribers {
		if listener.accountID != accountID ||
			(excludedDeviceID != "" && listener.deviceID == excludedDeviceID) {
			continue
		}
		select {
		case listener.events <- Changes:
		default:
			// One pending wakeup already says everything the hint channel can say. The pull cursor
			// describes all commits behind it when the client handles that wakeup.
		}
	}
}

// Revoke tells every live connection for one device to stop before its token can be used again.
func (broker *Broker) Revoke(accountID string, deviceID string) {
	if broker == nil {
		return
	}

	broker.mutex.Lock()
	defer broker.mutex.Unlock()
	for listener := range broker.subscribers {
		if listener.accountID != accountID || listener.deviceID != deviceID {
			continue
		}
		// Revocation outranks a queued content wakeup. The client cannot pull it anymore, and the
		// explicit event lets it forget the now-useless token without a reconnect round trip.
		select {
		case <-listener.events:
		default:
		}
		listener.events <- Revoked
	}
}
