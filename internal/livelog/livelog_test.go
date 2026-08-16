package livelog

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestBrokerForgetsPastEventsAndScopesAttributedEvents(t *testing.T) {
	broker := NewBroker()
	broker.publish(Event{Message: "before subscription"})

	accountAEvents, cancelAccountA := broker.Subscribe("account-a")
	defer cancelAccountA()
	accountBEvents, cancelAccountB := broker.Subscribe("account-b")
	defer cancelAccountB()

	select {
	case event := <-accountAEvents:
		t.Fatalf("broker replayed an old event: %+v", event)
	default:
	}

	broker.publish(Event{Message: "for account a", AccountID: "account-a"})
	if event := receiveEvent(t, accountAEvents); event.Message != "for account a" {
		t.Fatalf("account A received %+v", event)
	}
	select {
	case event := <-accountBEvents:
		t.Fatalf("account B received account A's event: %+v", event)
	default:
	}

	broker.publish(Event{Message: "server-wide"})
	if event := receiveEvent(t, accountAEvents); event.Message != "server-wide" {
		t.Fatalf("account A missed server-wide event: %+v", event)
	}
	if event := receiveEvent(t, accountBEvents); event.Message != "server-wide" {
		t.Fatalf("account B missed server-wide event: %+v", event)
	}
}

func TestHandlerPublishesStructuredAndRedactedEvent(t *testing.T) {
	broker := NewBroker()
	events, cancel := broker.Subscribe("account-a")
	defer cancel()

	downstream := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewHandler(downstream, broker)).With("component", "test")
	logger.InfoContext(context.Background(), "device signed in",
		"account_id", "account-a",
		"device_id", "device-one",
		"token", "must-not-appear",
	)

	event := receiveEvent(t, events)
	if event.Message != "device signed in" || event.Level != "INFO" {
		t.Fatalf("unexpected event: %+v", event)
	}
	if event.AccountID != "account-a" || event.Attributes["component"] != "test" {
		t.Fatalf("event lost its structured attributes: %+v", event)
	}
	if event.Attributes["token"] != "[redacted]" {
		t.Fatalf("sensitive token was not redacted: %+v", event)
	}
}

func receiveEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a live log event")
		return Event{}
	}
}
