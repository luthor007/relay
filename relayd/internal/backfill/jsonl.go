package backfill

import (
	"bufio"
	"errors"
	"io"
	"os"
)

// maxJSONLLine caps one transcript line. A single Claude Code line can carry a
// whole file's contents as a tool result, so the cap is generous; what it
// prevents is one pathological line taking the process down mid-backfill.
const maxJSONLLine = 64 << 20

// jsonlStats is what a streaming pass over a JSONL file could not use.
type jsonlStats struct {
	Lines     int
	Malformed int
	Oversized int
}

// scanJSONL streams a JSONL file, calling fn with each line and the byte offset
// that line starts at.
//
// It streams rather than reading the file in: Claude Code's store was 786 MB
// and Codex's 295 MB on the measured machine, and backfill has to walk both
// without holding either in memory. A line longer than maxJSONLLine is counted
// and skipped rather than fatal — one unreadable line must not cost the other
// 4,378 messages.
func scanJSONL(path string, fn func(line []byte, offset int64) error) (jsonlStats, error) {
	var st jsonlStats

	f, err := os.Open(path)
	if err != nil {
		return st, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 256<<10)

	var start int64  // byte offset the current line begins at
	var length int64 // bytes consumed for the current line, newline included
	var buf []byte
	dropping := false

	for {
		frag, err := r.ReadSlice('\n')
		switch {
		case err == nil, errors.Is(err, bufio.ErrBufferFull), errors.Is(err, io.EOF):
		default:
			return st, err
		}
		if errors.Is(err, io.EOF) && len(frag) == 0 {
			break
		}

		length += int64(len(frag))
		if int64(len(buf))+int64(len(frag)) > maxJSONLLine {
			dropping = true
		} else {
			buf = append(buf, frag...)
		}

		if errors.Is(err, bufio.ErrBufferFull) {
			continue // the line is longer than the read buffer; keep going
		}

		line := trimEOL(buf)
		switch {
		case dropping:
			st.Oversized++
		case len(line) == 0:
			// a blank line is not a record
		default:
			st.Lines++
			if cbErr := fn(line, start); cbErr != nil {
				st.Malformed++
			}
		}

		start += length
		length, buf, dropping = 0, buf[:0], false

		if errors.Is(err, io.EOF) {
			break
		}
	}
	return st, nil
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
