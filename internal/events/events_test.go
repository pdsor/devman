package events

import (
	"sync"
	"testing"
	"time"

	"github.com/devman-project/devman/pkg/dto"
)

func TestPublishStampsSequenceAndTimestamp(t *testing.T) {
	bus := New(nil)
	defer bus.Close()

	first := bus.Emit(dto.EventServiceStarted, "p_1", "web", "started", nil)
	second := bus.Emit(dto.EventServiceStopped, "p_1", "web", "stopped", nil)

	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("sequence numbers must be monotonic, got %d and %d", first.Seq, second.Seq)
	}
	if first.Timestamp.IsZero() {
		t.Fatal("events must be timestamped")
	}
}

func TestSubscribersReceiveEventsAndCancelCleanly(t *testing.T) {
	bus := New(nil)
	defer bus.Close()

	stream, cancel := bus.Subscribe(4)
	bus.Emit(dto.EventDaemonReady, "", "", "ready", nil)

	select {
	case event := <-stream:
		if event.Type != dto.EventDaemonReady {
			t.Fatalf("unexpected event %s", event.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("the subscriber received nothing")
	}

	cancel()
	if _, open := <-stream; open {
		t.Fatal("cancelling a subscription must close its channel")
	}
	// Publishing after a cancellation must not panic on a closed channel.
	bus.Emit(dto.EventDaemonReady, "", "", "still fine", nil)
}

func TestSlowSubscriberLosesEventsInsteadOfBlockingTheSupervisor(t *testing.T) {
	bus := New(nil)
	defer bus.Close()

	// A subscriber that never reads must not be able to stall a service state
	// change: dropping events is the lesser evil.
	_, cancel := bus.Subscribe(1)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			bus.Emit(dto.EventHealthChanged, "p_1", "web", "changed", nil)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing blocked on a subscriber that stopped reading")
	}
}

func TestRecentReturnsTheNewestEventsOldestFirst(t *testing.T) {
	bus := New(nil)
	defer bus.Close()

	for i := 0; i < 5; i++ {
		bus.Emit(dto.EventServiceStarted, "p_1", "web", "started", map[string]any{"i": i})
	}

	recent := bus.Recent(3)
	if len(recent) != 3 {
		t.Fatalf("expected 3 events, got %d", len(recent))
	}
	if recent[0].Seq != 3 || recent[2].Seq != 5 {
		t.Fatalf("expected sequences 3..5, got %d..%d", recent[0].Seq, recent[2].Seq)
	}
}

func TestPersisterReceivesEveryEvent(t *testing.T) {
	var persisted []dto.Event
	bus := New(func(event dto.Event) { persisted = append(persisted, event) })
	defer bus.Close()

	bus.Emit(dto.EventPortReserved, "p_1", "web", "reserved", map[string]any{"port": 3000})
	if len(persisted) != 1 {
		t.Fatalf("expected one persisted event, got %d", len(persisted))
	}
	if persisted[0].Data["port"] != 3000 {
		t.Fatalf("event data was not preserved: %+v", persisted[0].Data)
	}
}

// TestPublishDuringCloseDoesNotPanic is a regression test for a crash seen under
// load: Publish used to snapshot the subscriber channels, release the lock and
// then send, so a health probe that landed while the daemon was shutting down
// sent on a channel Close had already closed and took the process down.
func TestPublishDuringCloseDoesNotPanic(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		bus := New(nil)
		for i := 0; i < 4; i++ {
			// Unread subscribers with a tiny buffer, so sends are the interesting
			// path rather than a no-op.
			bus.Subscribe(1)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				bus.Emit(dto.EventHealthChanged, "p_1", "web", "health is HEALTHY", nil)
			}
		}()
		go func() {
			defer wg.Done()
			bus.Close()
		}()
		wg.Wait()

		// Publishing after Close must still be safe and must still be recorded.
		event := bus.Emit(dto.EventServiceStopped, "p_1", "web", "stopped", nil)
		if event.Seq == 0 {
			t.Fatal("an event published after Close must still be stamped")
		}
	}
}

// TestSubscribeAfterCloseEndsImmediately keeps a late subscriber from waiting
// forever for a bus that will never publish to it again.
func TestSubscribeAfterCloseEndsImmediately(t *testing.T) {
	bus := New(nil)
	bus.Close()

	stream, cancel := bus.Subscribe(4)
	defer cancel()
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("a subscriber created after Close must not receive events")
		}
	case <-time.After(time.Second):
		t.Fatal("a subscriber created after Close must get a closed channel")
	}
}
