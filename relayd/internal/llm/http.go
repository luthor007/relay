package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// HTTPError is a non-2xx answer from a provider, carrying the provider's own
// message so a probe can report what it actually said.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	if body == "" {
		return fmt.Sprintf("http %d", e.Status)
	}
	return fmt.Sprintf("http %d: %s", e.Status, body)
}

// post sends a JSON body and returns the response, leaving the body open on
// success so a stream can read it.
func post(ctx context.Context, cfg Config, path string, payload any, headers func(h http.Header, key string)) (*http.Response, error) {
	key, err := cfg.Credential.Resolve(ctx, cfg.Lookup)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(cfg.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	headers(req.Header, key)

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(b)}
	}
	return resp, nil
}

// sseReader reads text/event-stream frames. Both provider APIs stream this way,
// and both terminate differently enough that the caller decides what a frame
// means; this only splits them.
type sseReader struct {
	body io.ReadCloser
	sc   *bufio.Scanner
}

func newSSEReader(body io.ReadCloser) *sseReader {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return &sseReader{body: body, sc: sc}
}

// next returns the next data payload. It returns io.EOF at the end of the
// stream.
func (r *sseReader) next() (event string, data string, err error) {
	for r.sc.Scan() {
		line := strings.TrimRight(r.sc.Text(), "\r")
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		case strings.HasPrefix(line, "data:"):
			return event, strings.TrimSpace(strings.TrimPrefix(line, "data:")), nil
		}
	}
	if err := r.sc.Err(); err != nil {
		return "", "", err
	}
	return "", "", io.EOF
}

func (r *sseReader) Close() error { return r.body.Close() }
