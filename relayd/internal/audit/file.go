package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// File is an append-only JSONL audit log on disk.
//
// JSONL and not SQLite, deliberately. The vault is already a database and the
// index is another; a third would make "the evidence exists in a place the user
// can see without our help" depend on our tooling to read. A line-per-event
// text file is greppable at 3 a.m. by somebody who does not trust us, which is
// the situation this file is for. It also keeps appending possible while the
// databases are locked, which is exactly when a mutation is most interesting.
//
// The handle is opened O_APPEND, so every write lands at the end even if two
// processes hold the file; there is no seek and no truncate anywhere in this
// type.
type File struct {
	// Now and NewID are injectable so a test can assert timestamps and ids.
	Now   func() time.Time
	NewID func() string

	path string

	mu   sync.Mutex
	f    *os.File
	prev string
	seq  int64
}

var _ Log = (*File)(nil)

// MaxLineBytes bounds one entry. An entry is a handful of short strings; a line
// larger than this is a corrupt file, not a big event.
const MaxLineBytes = 1 << 20

// OpenFile opens (creating if needed) an audit log and resumes its hash chain.
//
// The directory is created 0700 and the file 0600: the log names services and
// provenance, which is not secret but is nobody else's business either.
func OpenFile(path string) (*File, error) {
	if path == "" {
		return nil, fmt.Errorf("audit: empty log path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit: create log dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}

	l := &File{path: path, f: f}
	last, err := lastEntry(path)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	l.prev, l.seq = last.Hash, last.Seq
	return l, nil
}

func (l *File) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *File) newID() string {
	if l.NewID != nil {
		return l.NewID()
	}
	return NewID()
}

// Append writes one entry and flushes it to disk before returning.
//
// The fsync is not paranoia about throughput — credential mutations happen a
// few times a month. It is that the interesting case for this file is a machine
// that stopped abruptly, and an entry sitting in the page cache at that moment
// is an entry that never existed.
func (l *File) Append(_ context.Context, e Entry) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		return Entry{}, fmt.Errorf("audit: %s is closed", l.path)
	}

	out := seal(e, l.prev, l.seq+1, l.now, l.newID)
	b, err := json.Marshal(out)
	if err != nil {
		return Entry{}, fmt.Errorf("audit: encode entry: %w", err)
	}
	if len(b)+1 > MaxLineBytes {
		return Entry{}, fmt.Errorf("audit: entry is %d bytes, over the %d limit", len(b)+1, MaxLineBytes)
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return Entry{}, fmt.Errorf("audit: write %s: %w", l.path, err)
	}
	if err := l.f.Sync(); err != nil {
		return Entry{}, fmt.Errorf("audit: sync %s: %w", l.path, err)
	}

	l.prev, l.seq = out.Hash, out.Seq
	return out, nil
}

// List scans the log and returns matching entries, oldest first.
//
// It streams rather than slurping. The file is small in every expected case,
// but "expected" is doing a lot of work in a security log and a bounded reader
// costs nothing.
func (l *File) List(_ context.Context, f Filter) ([]Entry, error) {
	fh, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit: read %s: %w", l.path, err)
	}
	defer fh.Close()

	limit := f.limit()
	out := make([]Entry, 0, limit)

	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			// A line we cannot parse is not a reason to hide the rest of the log.
			// It is itself evidence, so it is surfaced as a synthetic entry rather
			// than skipped in silence.
			e = Entry{Outcome: OutcomeFailed, Reason: "unparseable audit line: " + err.Error()}
			if !f.match(e) {
				continue
			}
			out = append(out, e)
			if len(out) > limit {
				out = out[len(out)-limit:]
			}
			continue
		}
		if !f.match(e) {
			continue
		}
		out = append(out, e)
		if len(out) > limit {
			out = out[len(out)-limit:]
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("audit: scan %s: %w", l.path, err)
	}
	return out, nil
}

// All reads every entry in write order, which is what [Verify] needs.
func (l *File) All(ctx context.Context) ([]Entry, error) {
	return l.List(ctx, Filter{Limit: MaxLimit})
}

// Durable is true: this log is on disk.
func (l *File) Durable() bool { return true }

// Path is the file.
func (l *File) Path() string { return l.path }

// Close closes the handle. The log is not deleted and cannot be.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// tailBytes is how much of the end of the file is read to resume the chain.
// Comfortably more than one entry, and bounded so a huge log opens instantly.
const tailBytes = 128 * 1024

// lastEntry reads the final complete line of a log. A missing or empty file
// gives the zero entry, which starts a fresh chain.
func lastEntry(path string) (Entry, error) {
	fh, err := os.Open(path)
	if os.IsNotExist(err) {
		return Entry{}, nil
	}
	if err != nil {
		return Entry{}, fmt.Errorf("audit: read %s: %w", path, err)
	}
	defer fh.Close()

	info, err := fh.Stat()
	if err != nil {
		return Entry{}, fmt.Errorf("audit: stat %s: %w", path, err)
	}
	size := info.Size()
	if size == 0 {
		return Entry{}, nil
	}

	n := int64(tailBytes)
	if size < n {
		n = size
	}
	buf := make([]byte, n)
	if _, err := fh.ReadAt(buf, size-n); err != nil && err != io.EOF {
		return Entry{}, fmt.Errorf("audit: read %s: %w", path, err)
	}

	// Walk backwards over complete lines until one parses. A half-written final
	// line — the machine died mid-append — is skipped rather than fatal, and the
	// chain resumes from the last entry that is actually there.
	end := len(buf)
	for end > 0 {
		if buf[end-1] == '\n' {
			end--
			continue
		}
		break
	}
	for end > 0 {
		start := end - 1
		for start > 0 && buf[start-1] != '\n' {
			start--
		}
		var e Entry
		if err := json.Unmarshal(buf[start:end], &e); err == nil && e.Hash != "" {
			return e, nil
		}
		end = start
		for end > 0 && buf[end-1] == '\n' {
			end--
		}
	}
	return Entry{}, nil
}
