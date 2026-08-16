// Package livelog mirrors structured log records to authenticated browser subscribers without
// retaining history. Standard JSON logging remains the source of truth on stdout.
package livelog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const subscriberBufferSize = 128

// Event is the browser-safe representation of one slog record.
type Event struct {
	Time       time.Time         `json:"time"`
	Level      string            `json:"level"`
	Message    string            `json:"message"`
	Attributes map[string]string `json:"attributes,omitempty"`

	// AccountID controls delivery and is deliberately absent from the JSON representation. Events
	// explicitly attributed to one account are never sent to another account's browser session.
	AccountID string `json:"-"`
}

type subscriber struct {
	accountID string
	events    chan Event
}

// Broker fans new events out to connected browsers. It has no history or replay buffer: an event
// published with no subscriber is immediately forgotten.
type Broker struct {
	mutex       sync.Mutex
	subscribers map[*subscriber]struct{}
}

func NewBroker() *Broker {
	return &Broker{subscribers: make(map[*subscriber]struct{})}
}

// Subscribe returns future events visible to accountID and a function that must be called when the
// browser disconnects.
func (broker *Broker) Subscribe(accountID string) (<-chan Event, func()) {
	listener := &subscriber{
		accountID: accountID,
		events:    make(chan Event, subscriberBufferSize),
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

func (broker *Broker) publish(event Event) {
	broker.mutex.Lock()
	defer broker.mutex.Unlock()

	for listener := range broker.subscribers {
		if event.AccountID != "" && event.AccountID != listener.accountID {
			continue
		}
		select {
		case listener.events <- event:
		default:
			// A slow browser must never stall request handling or stdout logging. Its next rendered
			// event will make the gap visible without the server keeping a backlog.
		}
	}
}

type boundAttribute struct {
	groups    []string
	attribute slog.Attr
}

// Handler writes each record to downstream and then offers a browser-safe copy to broker.
type Handler struct {
	downstream      slog.Handler
	broker          *Broker
	groups          []string
	boundAttributes []boundAttribute
}

func NewHandler(downstream slog.Handler, broker *Broker) *Handler {
	return &Handler{downstream: downstream, broker: broker}
}

func (handler *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return handler.downstream.Enabled(ctx, level)
}

func (handler *Handler) Handle(ctx context.Context, record slog.Record) error {
	downstreamError := handler.downstream.Handle(ctx, record)

	event := Event{
		Time:       record.Time,
		Level:      record.Level.String(),
		Message:    record.Message,
		Attributes: make(map[string]string),
	}
	for _, bound := range handler.boundAttributes {
		appendAttribute(event.Attributes, bound.groups, bound.attribute)
	}
	record.Attrs(func(attribute slog.Attr) bool {
		appendAttribute(event.Attributes, handler.groups, attribute)
		return true
	})
	if len(event.Attributes) == 0 {
		event.Attributes = nil
	}
	event.AccountID = event.Attributes["account_id"]
	handler.broker.publish(event)

	return downstreamError
}

func (handler *Handler) WithAttrs(attributes []slog.Attr) slog.Handler {
	boundAttributes := append([]boundAttribute(nil), handler.boundAttributes...)
	for _, attribute := range attributes {
		boundAttributes = append(boundAttributes, boundAttribute{
			groups:    append([]string(nil), handler.groups...),
			attribute: attribute,
		})
	}
	return &Handler{
		downstream:      handler.downstream.WithAttrs(attributes),
		broker:          handler.broker,
		groups:          append([]string(nil), handler.groups...),
		boundAttributes: boundAttributes,
	}
}

func (handler *Handler) WithGroup(name string) slog.Handler {
	groups := append([]string(nil), handler.groups...)
	if name != "" {
		groups = append(groups, name)
	}
	return &Handler{
		downstream:      handler.downstream.WithGroup(name),
		broker:          handler.broker,
		groups:          groups,
		boundAttributes: append([]boundAttribute(nil), handler.boundAttributes...),
	}
}

func appendAttribute(target map[string]string, groups []string, attribute slog.Attr) {
	if attribute.Equal(slog.Attr{}) {
		return
	}
	value := attribute.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		nestedGroups := append([]string(nil), groups...)
		if attribute.Key != "" {
			nestedGroups = append(nestedGroups, attribute.Key)
		}
		for _, child := range value.Group() {
			appendAttribute(target, nestedGroups, child)
		}
		return
	}

	keyParts := append([]string(nil), groups...)
	if attribute.Key != "" {
		keyParts = append(keyParts, attribute.Key)
	}
	key := strings.Join(keyParts, ".")
	if key == "" {
		return
	}
	if isSensitiveAttribute(key) {
		target[key] = "[redacted]"
		return
	}

	switch value.Kind() {
	case slog.KindTime:
		target[key] = value.Time().Format(time.RFC3339Nano)
	case slog.KindDuration:
		target[key] = value.Duration().String()
	default:
		target[key] = fmt.Sprint(value.Any())
	}
}

func isSensitiveAttribute(key string) bool {
	normalised := strings.ToLower(key)
	for _, fragment := range []string{"password", "token", "authorization", "cookie", "secret", "database_url"} {
		if strings.Contains(normalised, fragment) {
			return true
		}
	}
	return false
}
