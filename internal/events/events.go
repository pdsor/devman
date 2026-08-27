// Package events is the daemon's internal event bus.
//
// Every state change is published as a structured event from the moment the
// supervisor exists, rather than being retrofitted when SSE was added. The GUI
// subscribes, the CLI can tail it, and a future MCP server gets it for free.
package events

import (
	"sync"
	"time"

	"github.com/devman-project/devman/pkg/dto"
)

// Persister optionally records events durably.
type Persister func(dto.Event)

// Bus fans structured events out to subscribers.
type Bus struct {
	persist Persister

	mu       sync.Mutex
	subs     map[int]chan dto.Event
	nextID   int
	seq      uint64
	recent   []dto.Event
	capacity int
}

// New creates a bus. persist may be nil.
func New(persist Persister) *Bus {
	return &Bus{
		persist:  persist,
		subs:     map[int]chan dto.Event{},
		capacity: 500,
	}
}

// Publish stamps and delivers an event.
//
// Delivery never blocks: a subscriber that stops reading loses events rather
// than stalling the supervisor that is trying to report a state change.
func (b *Bus) Publish(event dto.Event) dto.Event {
	b.mu.Lock()
	b.seq++
	event.Seq = b.seq
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	b.recent = append(b.recent, event)
	if len(b.recent) > b.capacity {
		b.recent = b.recent[len(b.recent)-b.capacity:]
	}
	targets := make([]chan dto.Event, 0, len(b.subs))
	for _, ch := range b.subs {
		targets = append(targets, ch)
	}
	persist := b.persist
	b.mu.Unlock()

	if persist != nil {
		persist(event)
	}
	for _, ch := range targets {
		select {
		case ch <- event:
		default:
		}
	}
	return event
}

// Emit is a shorthand for the common project/service event.
func (b *Bus) Emit(kind dto.EventType, project, service, message string, data map[string]any) dto.Event {
	return b.Publish(dto.Event{
		Type:    kind,
		Project: project,
		Service: service,
		Message: message,
		Data:    data,
	})
}

// Subscribe returns a channel of events plus a cancel function.
func (b *Bus) Subscribe(buffer int) (<-chan dto.Event, func()) {
	if buffer <= 0 {
		buffer = 128
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan dto.Event, buffer)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
	}
}

// Recent returns the most recent events, oldest first.
func (b *Bus) Recent(limit int) []dto.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 || limit > len(b.recent) {
		limit = len(b.recent)
	}
	out := make([]dto.Event, limit)
	copy(out, b.recent[len(b.recent)-limit:])
	return out
}

// Close disconnects every subscriber.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}
