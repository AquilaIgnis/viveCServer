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

	// Disconnected ends a stream without telling the client its credential is gone. It is for a
	// device whose token was replaced rather than withdrawn: the client already holds the new one,
	// and a `revoked` event racing it there would make the app forget the account it just signed
	// in to. A plain close makes it reconnect with whatever token it holds now.
	Disconnected
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
		// The explicit event lets the client forget the now-useless token without a reconnect
		// round trip.
		deliverUrgently(listener, Revoked)
	}
}

// Disconnect closes every live connection for one device whose token has just been replaced, so
// that no stream outlives the credential it was authenticated with.
func (broker *Broker) Disconnect(accountID string, deviceID string) {
	if broker == nil {
		return
	}

	broker.mutex.Lock()
	defer broker.mutex.Unlock()
	for listener := range broker.subscribers {
		if listener.accountID != accountID || listener.deviceID != deviceID {
			continue
		}
		deliverUrgently(listener, Disconnected)
	}
}

// deliverUrgently replaces whatever the listener had queued with an event that outranks it.
//
// Revocation and disconnection outrank a queued content wakeup: the connection cannot be trusted to
// pull that content any more. Draining before sending is what makes the send safe to write as a
// blocking one — the buffer holds a single event, so it always has room immediately afterwards — and
// both callers hold the broker mutex throughout, which keeps this from racing Subscribe's cancel
// closing the channel.
func deliverUrgently(listener *subscriber, event Event) {
	select {
	case <-listener.events:
	default:
	}
	listener.events <- event
}
