package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DASHBOARD.md §3.1's second requirement: let you **open the raw transcript**,
// since the index holds a pointer into the original file rather than a copy.
//
// The measured corpus is 3.6 GB, one runtime is 70% of it, and a single Hermes
// store is 2.5 GB (MEMORY.md §1). So this is a **range read and only a range
// read**. There is no code path here that reads a whole file, and the size of
// the buffer is fixed before the file is opened rather than derived from
// anything the file says about itself.
//
// The response is JSON rather than a raw byte stream on purpose: the console
// needs the size, the next offset and the secret markers for this session
// alongside the text, and a body that is only bytes would need three more
// round trips to render one screen.

// TranscriptChunk is one window into a transcript.
type TranscriptChunk struct {
	Runtime string `json:"runtime"`
	Session string `json:"session"`
	Path    string `json:"path"`

	// Offset is relative to the session's start in the file, not the file's
	// start. Hermes and OpenClaw put many sessions in one store, and an offset
	// that meant "into the file" would have the console paging through other
	// people's conversations to reach this one.
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
	// Size is the whole file right now, and SessionOffset where this session
	// begins in it. Both are shown because "you are 4 KB into a 2.5 GB store" is
	// the fact that stops somebody expecting a scrollbar.
	Size          int64 `json:"size"`
	SessionOffset int64 `json:"session_offset"`
	// NextOffset is where to ask for the following window, or -1 at the end.
	NextOffset int64 `json:"next_offset"`
	EOF        bool  `json:"eof"`

	Text string `json:"text"`
	// Truncated is set when the window ended mid-rune and the tail byte was
	// dropped rather than emitted as a replacement character.
	Truncated bool `json:"truncated,omitempty"`

	// Markers are the secrets detection found in this session. They are carried
	// with the text because the console is about to display a file that is known
	// to have contained a credential, and MEMORY.md §6's whole ordering argument
	// is that the user should be told before rather than after.
	Markers []TranscriptMarker `json:"markers,omitempty"`
}

// TranscriptMarker is one detection in this session.
type TranscriptMarker struct {
	ID         string `json:"id"`
	Detector   string `json:"detector"`
	Service    string `json:"service,omitempty"`
	ByteOffset int64  `json:"byte_offset"`
	// Captured is true once the credential was moved into the vault, which is
	// what MEMORY.md §6's proposal flow does on accept.
	Captured bool  `json:"captured"`
	At       int64 `json:"at"`
}

// TranscriptWindow is the default read size, and MaxTranscriptWindow the
// ceiling. 64 KiB is a comfortable screenful of JSONL and four orders of
// magnitude below the smallest store we measured.
const (
	TranscriptWindow    = 64 * 1024
	MaxTranscriptWindow = 1 << 20
)

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	id := sessionKey(r)

	ref, ok, err := s.transcriptFor(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, CodeNotFound,
			"no indexed transcript for this session. Backfill (MEMORY.md §4) records "+
				"where each runtime keeps its own file; until it has run there is no pointer to follow")
		return
	}

	offset, length, err := window(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadPayload, err.Error())
		return
	}

	chunk, err := readTranscript(ref, offset, length)
	switch {
	case errors.Is(err, errUnsafePath):
		writeErr(w, http.StatusForbidden, CodeForbidden, err.Error())
		return
	case errors.Is(err, os.ErrNotExist):
		writeErr(w, http.StatusNotFound, CodeNotFound,
			"the transcript this session points at is no longer on disk: "+ref.Path)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, CodeFailed, err.Error())
		return
	}

	chunk.Markers = s.transcriptMarkers(r.Context(), ref)
	writeJSON(w, http.StatusOK, chunk)
}

func window(r *http.Request) (offset, length int64, err error) {
	q := r.URL.Query()
	if v := q.Get("offset"); v != "" {
		offset, err = strconv.ParseInt(v, 10, 64)
		if err != nil || offset < 0 {
			return 0, 0, errors.New("offset must be a non-negative integer")
		}
	}
	length = TranscriptWindow
	if v := q.Get("limit"); v != "" {
		length, err = strconv.ParseInt(v, 10, 64)
		if err != nil || length <= 0 {
			return 0, 0, errors.New("limit must be a positive integer")
		}
	}
	if length > MaxTranscriptWindow {
		// Clamped rather than refused: a console asking for too much should get a
		// screenful and a next offset, not an error it has to handle.
		length = MaxTranscriptWindow
	}
	return offset, length, nil
}

var errUnsafePath = errors.New("api: refusing to read a transcript path that is not absolute and clean")

// readTranscript reads one bounded window.
//
// It allocates exactly the window, seeks straight to it, and never asks for
// more. The 786 MB Claude Code store and the 2.5 GB Hermes one are read the
// same way as a 4 KB one.
func readTranscript(ref TranscriptRef, offset, length int64) (TranscriptChunk, error) {
	if !safePath(ref.Path) {
		return TranscriptChunk{}, errUnsafePath
	}

	f, err := os.Open(ref.Path)
	if err != nil {
		return TranscriptChunk{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return TranscriptChunk{}, err
	}
	if !info.Mode().IsRegular() {
		return TranscriptChunk{}, errUnsafePath
	}
	size := info.Size()

	chunk := TranscriptChunk{
		Runtime:       ref.Runtime,
		Session:       ref.Session,
		Path:          ref.Path,
		Offset:        offset,
		Size:          size,
		SessionOffset: ref.ByteOffset,
		NextOffset:    -1,
	}

	start := ref.ByteOffset + offset
	if start >= size {
		chunk.EOF = true
		return chunk, nil
	}
	if remaining := size - start; length > remaining {
		length = remaining
	}

	buf := make([]byte, length)
	n, err := f.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return TranscriptChunk{}, err
	}
	buf = buf[:n]

	// A window can end mid-rune. Dropping the partial tail and telling the
	// console where to resume beats emitting U+FFFD into the middle of somebody's
	// source code.
	for len(buf) > 0 && !utf8.Valid(buf) {
		buf = buf[:len(buf)-1]
		chunk.Truncated = true
	}

	chunk.Text = string(buf)
	chunk.Length = int64(len(buf))
	if start+chunk.Length >= size {
		chunk.EOF = true
	} else {
		chunk.NextOffset = offset + chunk.Length
	}
	return chunk, nil
}

// safePath refuses anything but an absolute, cleaned path.
//
// The path comes out of our own index, which came out of detection, so this is
// belt and braces — but the index is a database file on a machine the user also
// uses, and a range read is exactly the primitive that turns a tampered row
// into "read me /etc/shadow".
func safePath(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	if p != filepath.Clean(p) {
		return false
	}
	return !strings.Contains(p, "\x00")
}

func (s *Server) transcriptMarkers(ctx context.Context, ref TranscriptRef) []TranscriptMarker {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id, detector, service, byte_offset, vault_id, at
		FROM secret_marker WHERE runtime = ? AND session_id = ? ORDER BY byte_offset`,
		ref.Runtime, ref.Session)
	if err != nil {
		s.log.Warn("api: read secret markers", "session", ref.Session, "error", err)
		return nil
	}
	defer rows.Close()

	var out []TranscriptMarker
	for rows.Next() {
		var m TranscriptMarker
		var vaultID string
		if err := rows.Scan(&m.ID, &m.Detector, &m.Service, &m.ByteOffset, &vaultID, &m.At); err != nil {
			return out
		}
		m.Captured = vaultID != ""
		out = append(out, m)
	}
	return out
}
