package logstore

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	return NewManager(t.TempDir(), opts)
}

func TestLineWriterSplitsRecords(t *testing.T) {
	m := newTestManager(t, DefaultOptions())
	log, err := m.Service("proj", "backend")
	if err != nil {
		t.Fatalf("Service: %v", err)
	}
	defer m.Close()

	w := log.Writer(StreamStdout)
	// Deliberately split a line across writes and use CRLF.
	w.Write([]byte("first\r\nsec"))
	w.Write([]byte("ond\nthird without newline"))
	w.Flush()

	records := log.History(Query{})
	if len(records) != 3 {
		t.Fatalf("records = %d: %+v", len(records), records)
	}
	want := []string{"first", "second", "third without newline"}
	for i, expected := range want {
		if records[i].Message != expected {
			t.Fatalf("record %d = %q, want %q", i, records[i].Message, expected)
		}
		if records[i].Stream != StreamStdout {
			t.Fatalf("record %d stream = %q", i, records[i].Stream)
		}
		if records[i].Project != "proj" || records[i].Service != "backend" {
			t.Fatalf("record %d not tagged: %+v", i, records[i])
		}
		if records[i].Timestamp.IsZero() {
			t.Fatalf("record %d has no timestamp", i)
		}
		if records[i].Seq != uint64(i+1) {
			t.Fatalf("record %d seq = %d", i, records[i].Seq)
		}
	}
}

func TestLineWriterBoundsUnterminatedOutput(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxLineBytes = 16
	m := newTestManager(t, opts)
	log, err := m.Service("proj", "chatty")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	w := log.Writer(StreamStdout)
	// 40 bytes with no newline at all.
	w.Write([]byte(strings.Repeat("x", 40)))

	records := log.History(Query{})
	if len(records) != 2 {
		t.Fatalf("expected the writer to flush at the line limit, got %d records", len(records))
	}
	if len(records[0].Message) != 16 {
		t.Fatalf("record 0 length = %d", len(records[0].Message))
	}
	w.Flush()
	if got := len(log.History(Query{})); got != 3 {
		t.Fatalf("records after flush = %d", got)
	}
}

func TestStreamsAreQueryable(t *testing.T) {
	m := newTestManager(t, DefaultOptions())
	log, _ := m.Service("proj", "svc")
	defer m.Close()

	log.Append(StreamStdout, "out 1")
	log.Append(StreamStderr, "err 1")
	log.Append(StreamStdout, "out 2")
	log.Append(StreamStderr, "err 2")

	if got := log.History(Query{Stream: StreamStderr}); len(got) != 2 ||
		got[0].Message != "err 1" || got[1].Message != "err 2" {
		t.Fatalf("stderr filter = %+v", got)
	}
	if got := log.History(Query{Tail: 1}); len(got) != 1 || got[0].Message != "err 2" {
		t.Fatalf("tail = %+v", got)
	}
	errors := log.LastErrors(1)
	if len(errors) != 1 || errors[0].Message != "err 2" {
		t.Fatalf("LastErrors = %+v", errors)
	}
}

func TestSinceFilter(t *testing.T) {
	m := newTestManager(t, DefaultOptions())
	log, _ := m.Service("proj", "svc")
	defer m.Close()

	first := log.Append(StreamStdout, "before")
	time.Sleep(5 * time.Millisecond)
	log.Append(StreamStdout, "after")

	got := log.History(Query{Since: first.Timestamp})
	if len(got) != 1 || got[0].Message != "after" {
		t.Fatalf("since filter = %+v", got)
	}
}

func TestSubscribeReceivesLiveRecords(t *testing.T) {
	m := newTestManager(t, DefaultOptions())
	log, _ := m.Service("proj", "svc")
	defer m.Close()

	stream, cancel := log.Subscribe(8)
	log.Append(StreamStdout, "live")

	select {
	case record := <-stream:
		if record.Message != "live" {
			t.Fatalf("record = %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no record delivered to subscriber")
	}

	cancel()
	if _, open := <-stream; open {
		t.Fatal("channel must be closed after cancel")
	}
	// Appending after cancellation must not panic on a closed channel.
	log.Append(StreamStdout, "after cancel")
}

func TestSlowSubscriberDoesNotBlockAppend(t *testing.T) {
	m := newTestManager(t, DefaultOptions())
	log, _ := m.Service("proj", "svc")
	defer m.Close()

	_, cancel := log.Subscribe(1)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			log.Append(StreamStdout, fmt.Sprintf("line %d", i))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber that never reads blocked the writer")
	}
}

func TestRotationKeepsBackups(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxSizeBytes = 512
	opts.MaxBackups = 2
	m := newTestManager(t, opts)
	log, _ := m.Service("proj", "noisy")
	defer m.Close()

	for i := 0; i < 200; i++ {
		log.Append(StreamStdout, fmt.Sprintf("line %03d with some padding to grow the file", i))
	}

	dir := filepath.Dir(log.Path())
	matches, err := filepath.Glob(filepath.Join(dir, "noisy.log*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 3 {
		t.Fatalf("expected the active file plus 2 backups, got %v", matches)
	}
	if _, err := filepath.Glob(filepath.Join(dir, "noisy.log.3")); err != nil {
		t.Fatal(err)
	}
	for _, path := range matches {
		if strings.HasSuffix(path, ".3") {
			t.Fatalf("backup beyond max_backups was kept: %s", path)
		}
	}

	// History must still work across rotation.
	if got := log.History(Query{Tail: 5}); len(got) != 5 {
		t.Fatalf("tail after rotation = %d", len(got))
	}
}

func TestHistorySurvivesReopen(t *testing.T) {
	root := t.TempDir()
	first := NewManager(root, DefaultOptions())
	log, err := first.Service("proj", "svc")
	if err != nil {
		t.Fatal(err)
	}
	log.Append(StreamStdout, "persisted line")
	log.Append(StreamStderr, "persisted error")
	first.Close()

	// A restarted daemon must still be able to serve the history, including the
	// stream tags and timestamps, which is why records are stored as NDJSON.
	second := NewManager(root, DefaultOptions())
	defer second.Close()
	reopened, err := second.Service("proj", "svc")
	if err != nil {
		t.Fatal(err)
	}
	records := reopened.History(Query{})
	if len(records) != 2 || records[0].Message != "persisted line" {
		t.Fatalf("records after reopen = %+v", records)
	}
	if records[1].Stream != StreamStderr {
		t.Fatalf("stream tag lost: %+v", records[1])
	}

	// Sequence numbers must continue rather than restart.
	next := reopened.Append(StreamStdout, "new line")
	if next.Seq != 3 {
		t.Fatalf("seq after reopen = %d, want 3", next.Seq)
	}
}

func TestHistoryReadsBeyondRingCapacity(t *testing.T) {
	opts := DefaultOptions()
	opts.RingCapacity = 10
	m := newTestManager(t, opts)
	log, _ := m.Service("proj", "svc")
	defer m.Close()

	for i := 1; i <= 100; i++ {
		log.Append(StreamStdout, fmt.Sprintf("line %d", i))
	}

	if got := log.History(Query{Tail: 5}); len(got) != 5 || got[4].Message != "line 100" {
		t.Fatalf("tail from ring = %+v", got)
	}
	// More than the ring holds: the file must be consulted.
	got := log.History(Query{Tail: 50})
	if len(got) != 50 {
		t.Fatalf("tail beyond ring = %d records", len(got))
	}
	if got[0].Message != "line 51" || got[49].Message != "line 100" {
		t.Fatalf("unexpected window: %q .. %q", got[0].Message, got[49].Message)
	}
}

func TestManagerReturnsSameLogPerService(t *testing.T) {
	m := newTestManager(t, DefaultOptions())
	defer m.Close()

	a, _ := m.Service("proj", "svc")
	b, _ := m.Service("proj", "svc")
	if a != b {
		t.Fatal("Service must return a single log per project/service")
	}
	other, _ := m.Service("proj", "other")
	if other == a {
		t.Fatal("different services must not share a log")
	}
	if filepath.Base(other.Path()) != "other.log" {
		t.Fatalf("log path = %q", other.Path())
	}
}
