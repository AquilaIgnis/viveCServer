package changefeed

import "testing"

func TestPublishExcludesTheAuthorAndOtherAccounts(t *testing.T) {
	broker := NewBroker()
	author, cancelAuthor := broker.Subscribe("account-a", "tablet")
	defer cancelAuthor()
	peer, cancelPeer := broker.Subscribe("account-a", "phone")
	defer cancelPeer()
	stranger, cancelStranger := broker.Subscribe("account-b", "phone")
	defer cancelStranger()

	broker.Publish("account-a", "tablet")

	assertNoEvent(t, author, "author")
	assertEvent(t, peer, "peer")
	assertNoEvent(t, stranger, "other account")
}

func TestPendingWakeupsAreCoalesced(t *testing.T) {
	broker := NewBroker()
	events, cancel := broker.Subscribe("account", "phone")
	defer cancel()

	broker.Publish("account", "")
	broker.Publish("account", "")
	assertEvent(t, events, "first coalesced event")
	assertNoEvent(t, events, "duplicate event")
}

func TestRevocationReplacesAPendingContentWakeup(t *testing.T) {
	broker := NewBroker()
	events, cancel := broker.Subscribe("account", "phone")
	defer cancel()

	broker.Publish("account", "")
	broker.Revoke("account", "phone")
	select {
	case event := <-events:
		if event != Revoked {
			t.Fatalf("event = %v, want Revoked", event)
		}
	default:
		t.Fatal("revoked device received no event")
	}
}

func assertEvent(t *testing.T, events <-chan Event, label string) {
	t.Helper()
	select {
	case <-events:
	default:
		t.Fatalf("%s received no event", label)
	}
}

func assertNoEvent(t *testing.T, events <-chan Event, label string) {
	t.Helper()
	select {
	case <-events:
		t.Fatalf("%s unexpectedly received an event", label)
	default:
	}
}
