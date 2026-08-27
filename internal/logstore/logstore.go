// Package logstore captures, persists and streams service output.
//
// Every line is a structured record (timestamp, project, service, stream,
// message), never a raw byte blob. Records are appended to an NDJSON file so a
// restarted daemon can still serve history with full fidelity, kept in a
// per-service ring buffer for instant replay to new subscribers, and fanned out
// live to SSE subscribers.
package logstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Stream identifies which output channel a record came from.
type Stream string

const (
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
	// StreamSystem carries DevMan's own annotations, such as "starting" or
	// "exited with code 1". It is never produced by the service itself.
	StreamSystem Stream = "system"
)

// Record is one structured log line.
type Record struct {
	Seq       uint64    `json:"seq"`
	Timestamp time.Time `json:"timestamp"`
	Project   string    `json:"project"`
	Service   string    `json:"service"`
	Stream    Stream    `json:"stream"`
	Message   string    `json:"message"`
}

// Options configures rotation and in-memory retention.
type Options struct {
	MaxSizeBytes int64
	MaxBackups   int
	RingCapacity int
	// MaxLineBytes bounds a single record so a service that never emits a
	// newline cannot exhaust memory.
	MaxLineBytes int
}

// DefaultOptions matches the global settings defaults.
func DefaultOptions() Options {
	return Options{
		MaxSizeBytes: 10 * 1024 * 1024,
		MaxBackups:   5,
		RingCapacity: 2000,
		MaxLineBytes: 64 * 1024,
	}
}

func (o Options) normalise() Options {
	d := DefaultOptions()
	if o.MaxSizeBytes <= 0 {
		o.MaxSizeBytes = d.MaxSizeBytes
	}
	if o.MaxBackups < 0 {
		o.MaxBackups = d.MaxBackups
	}
	if o.RingCapacity <= 0 {
		o.RingCapacity = d.RingCapacity
	}
	if o.MaxLineBytes <= 0 {
		o.MaxLineBytes = d.MaxLineBytes
	}
	return o
}

// Query filters a history request.
type Query struct {
	// Tail limits the result to the last N records. Zero means no limit.
	Tail int
	// Stream restricts the result to one stream. Empty means all streams.
	Stream Stream
	// Since returns only records strictly newer than this time.
	Since time.Time
}

func (q Query) matches(r Record) bool {
	if q.Stream != "" && r.Stream != q.Stream {
		return false
	}
	if !q.Since.IsZero() && !r.Timestamp.After(q.Since) {
		return false
	}
	return true
}

// ServiceLog is the log of a single service.
type ServiceLog struct {
	project string
	service string
	path    string
	opts    Options

	mu       sync.Mutex
	file     *os.File
	size     int64
	seq      uint64
	ring     []Record
	ringNext int
	ringLen  int

	subs   map[int]chan Record
	nextID int
	closed bool
}

// Manager owns one ServiceLog per project/service pair.
type Manager struct {
	root string
	opts Options

	mu   sync.Mutex
	logs map[string]*ServiceLog
}

// NewManager creates a manager rooted at the DevMan logs directory.
func NewManager(root string, opts Options) *Manager {
	return &Manager{root: root, opts: opts.normalise(), logs: map[string]*ServiceLog{}}
}

func logKey(project, service string) string { return project + "/" + service }

// Service returns (creating if needed) the log for one service.
func (m *Manager) Service(project, service string) (*ServiceLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := logKey(project, service)
	if log, ok := m.logs[key]; ok {
		return log, nil
	}
	dir := filepath.Join(m.root, project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create log directory: %w", err)
	}
	log := &ServiceLog{
		project: project,
		service: service,
		path:    filepath.Join(dir, service+".log"),
		opts:    m.opts,
		ring:    make([]Record, m.opts.RingCapacity),
		subs:    map[int]chan Record{},
	}
	if err := log.open(); err != nil {
		return nil, err
	}
	log.warmRing()
	m.logs[key] = log
	return log, nil
}

// Close flushes and closes every open log.
func (m *Manager) Close() {
	m.mu.Lock()
	logs := make([]*ServiceLog, 0, len(m.logs))
	for _, log := range m.logs {
		logs = append(logs, log)
	}
	m.logs = map[string]*ServiceLog{}
	m.mu.Unlock()
	for _, log := range logs {
		log.Close()
	}
}

func (l *ServiceLog) open() error {
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("cannot open log file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	l.file = file
	l.size = info.Size()
	return nil
}

// warmRing preloads the ring buffer from disk so a restarted daemon can still
// answer `devman logs` without re-reading the file on every request.
func (l *ServiceLog) warmRing() {
	records := l.readFileTail(l.opts.RingCapacity)
	for _, record := range records {
		if record.Seq > l.seq {
			l.seq = record.Seq
		}
		l.pushRing(record)
	}
}

// Append records one message.
func (l *ServiceLog) Append(stream Stream, message string) Record {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return Record{}
	}
	l.seq++
	record := Record{
		Seq:       l.seq,
		Timestamp: time.Now().UTC(),
		Project:   l.project,
		Service:   l.service,
		Stream:    stream,
		Message:   message,
	}
	l.pushRing(record)
	l.writeLocked(record)
	subs := make([]chan Record, 0, len(l.subs))
	for _, ch := range l.subs {
		subs = append(subs, ch)
	}
	l.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- record:
		default:
			// A slow subscriber must never stall the service it is watching.
		}
	}
	return record
}

func (l *ServiceLog) pushRing(record Record) {
	l.ring[l.ringNext] = record
	l.ringNext = (l.ringNext + 1) % len(l.ring)
	if l.ringLen < len(l.ring) {
		l.ringLen++
	}
}

func (l *ServiceLog) writeLocked(record Record) {
	if l.file == nil {
		return
	}
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if l.size+int64(len(line)) > l.opts.MaxSizeBytes {
		l.rotateLocked()
	}
	written, err := l.file.Write(line)
	if err == nil {
		l.size += int64(written)
	}
}

// rotateLocked shifts <name>.log to <name>.log.1, .1 to .2 and so on, dropping
// the oldest backup.
func (l *ServiceLog) rotateLocked() {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	if l.opts.MaxBackups == 0 {
		_ = os.Remove(l.path)
	} else {
		oldest := fmt.Sprintf("%s.%d", l.path, l.opts.MaxBackups)
		_ = os.Remove(oldest)
		for i := l.opts.MaxBackups - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
		}
		_ = os.Rename(l.path, l.path+".1")
	}
	if err := l.open(); err != nil {
		l.file = nil
		l.size = 0
	}
}

// Writer returns an io.Writer that splits the stream into records. The returned
// writer must be closed (or Flushed) to emit a trailing partial line.
func (l *ServiceLog) Writer(stream Stream) *LineWriter {
	return &LineWriter{log: l, stream: stream, max: l.opts.MaxLineBytes}
}

// History returns matching records, newest last.
func (l *ServiceLog) History(query Query) []Record {
	l.mu.Lock()
	ring := l.ringSnapshotLocked()
	// While the ring is not yet full it holds every record this log has ever
	// seen (it is warmed from disk on open), so the files add nothing.
	ringHoldsEverything := l.ringLen < len(l.ring)
	l.mu.Unlock()

	source := ring
	needMore := query.Tail == 0 || query.Tail > len(ring)
	if !ringHoldsEverything && needMore {
		limit := query.Tail
		if limit == 0 {
			limit = len(ring) * 4
		}
		if fromFile := l.readFileTail(limit); len(fromFile) > len(ring) {
			source = fromFile
		}
	}

	out := make([]Record, 0, len(source))
	for _, record := range source {
		if query.matches(record) {
			out = append(out, record)
		}
	}
	if query.Tail > 0 && len(out) > query.Tail {
		out = out[len(out)-query.Tail:]
	}
	return out
}

// LastErrors returns the most recent stderr records, which is what an AI agent
// asks for after a failed start.
func (l *ServiceLog) LastErrors(limit int) []Record {
	if limit <= 0 {
		limit = 20
	}
	return l.History(Query{Tail: limit, Stream: StreamStderr})
}

func (l *ServiceLog) ringSnapshotLocked() []Record {
	out := make([]Record, 0, l.ringLen)
	start := (l.ringNext - l.ringLen + len(l.ring)) % len(l.ring)
	for i := 0; i < l.ringLen; i++ {
		out = append(out, l.ring[(start+i)%len(l.ring)])
	}
	return out
}

// Subscribe returns a channel of live records plus a cancel function. The
// channel is buffered; records are dropped for subscribers that fall behind
// rather than blocking the service.
func (l *ServiceLog) Subscribe(buffer int) (<-chan Record, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		ch := make(chan Record)
		close(ch)
		return ch, func() {}
	}
	id := l.nextID
	l.nextID++
	ch := make(chan Record, buffer)
	l.subs[id] = ch
	return ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if existing, ok := l.subs[id]; ok {
			delete(l.subs, id)
			close(existing)
		}
	}
}

// Close releases the file and disconnects subscribers.
func (l *ServiceLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	for id, ch := range l.subs {
		delete(l.subs, id)
		close(ch)
	}
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}

// Path is the active log file.
func (l *ServiceLog) Path() string { return l.path }

// readFileTail reads up to limit records from the active file and, if needed,
// from rotated backups (newest first).
func (l *ServiceLog) readFileTail(limit int) []Record {
	if limit <= 0 {
		return nil
	}
	files := []string{l.path}
	for i := 1; i <= l.opts.MaxBackups; i++ {
		files = append(files, fmt.Sprintf("%s.%d", l.path, i))
	}

	var collected [][]Record
	total := 0
	for _, path := range files {
		records := readRecords(path)
		if len(records) == 0 {
			continue
		}
		collected = append(collected, records)
		total += len(records)
		if total >= limit {
			break
		}
	}
	// collected is newest-file-first; flatten oldest-first.
	out := make([]Record, 0, total)
	for i := len(collected) - 1; i >= 0; i-- {
		out = append(out, collected[i]...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func readRecords(path string) []Record {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var out []Record
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		out = append(out, record)
	}
	return out
}

// LineWriter turns a byte stream into log records, one per line.
type LineWriter struct {
	log    *ServiceLog
	stream Stream
	max    int

	mu      sync.Mutex
	partial []byte
}

var _ io.WriteCloser = (*LineWriter)(nil)

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	consumed := len(p)
	for len(p) > 0 {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			w.partial = append(w.partial, p...)
			// A service that never emits a newline must not grow this buffer
			// without bound; flush at the line limit instead.
			for len(w.partial) >= w.max {
				w.emit(w.partial[:w.max])
				w.partial = append(w.partial[:0], w.partial[w.max:]...)
			}
			break
		}
		line := append(w.partial, p[:idx]...)
		w.partial = w.partial[:0]
		w.emit(line)
		p = p[idx+1:]
	}
	return consumed, nil
}

// Flush emits any buffered partial line. Called when a process exits so its
// last, newline-less output is not lost.
func (w *LineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		w.emit(w.partial)
		w.partial = w.partial[:0]
	}
}

// Close flushes the writer.
func (w *LineWriter) Close() error {
	w.Flush()
	return nil
}

func (w *LineWriter) emit(line []byte) {
	text := strings.TrimRight(string(line), "\r")
	w.log.Append(w.stream, text)
}
