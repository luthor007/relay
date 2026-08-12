package audit

import (
	"context"
	"sync"
	"time"
)

// Memory is an audit log that does not survive a restart.
//
// It exists so the mutation path is never unlogged: relayd defaults to one when
// no data directory is writable, and [Memory.Durable] reports false so the
// console says "this machine is not keeping an audit trail" instead of showing
// an empty list that looks like "nothing has happened".
type Memory struct {
	// Now and NewID are injectable so a test can assert timestamps and ids.
	Now   func() time.Time
	NewID func() string

	mu      sync.Mutex
	entries []Entry
	prev    string
	seq     int64
	// cap bounds the ring. Zero means MemoryCap.
	cap int
}

// MemoryCap is how many entries a memory log keeps. Well past a year of a
// personal machine's credential mutations, and small enough to be free.
const MemoryCap = 10_000

var _ Log = (*Memory)(nil)

// NewMemory builds an in-memory log.
func NewMemory() *Memory { return &Memory{} }

func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Memory) newID() string {
	if m.NewID != nil {
		return m.NewID()
	}
	return NewID()
}

// Append writes one entry.
func (m *Memory) Append(_ context.Context, e Entry) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seq++
	out := seal(e, m.prev, m.seq, m.now, m.newID)
	m.prev = out.Hash
	m.entries = append(m.entries, out)

	limit := m.cap
	if limit <= 0 {
		limit = MemoryCap
	}
	if len(m.entries) > limit {
		m.entries = m.entries[len(m.entries)-limit:]
	}
	return out, nil
}

// List returns matching entries, oldest first.
func (m *Memory) List(_ context.Context, f Filter) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		if f.match(e) {
			out = append(out, e)
		}
	}
	if n := f.limit(); len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// Durable is false: this is the honest degradation, and the console prints it.
func (m *Memory) Durable() bool { return false }

// Path is empty because there is no file.
func (m *Memory) Path() string { return "" }

// Close is a no-op.
func (m *Memory) Close() error { return nil }
