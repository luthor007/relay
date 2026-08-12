package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// `ctx.storage` and `ctx.agent` — the two capabilities that are not about the
// glasses and not about memory.

// AgentSession is the user's agent, on their box, with their model
// configuration. APP-PLATFORM.md §2: apps do not bring their own API keys and
// the user is never billed twice.
type AgentSession interface {
	Ask(ctx context.Context, appID, prompt, model string) (string, error)
	// Stream is the same thing in pieces, for anything long enough to be worth
	// reading aloud while it arrives. An implementation that cannot stream must
	// not fake it by chunking a finished answer — the point of streaming is
	// latency, and a fake one reports a latency it did not achieve. Returning
	// [ErrNoStreaming] is the honest answer.
	Stream(ctx context.Context, appID, prompt string, emit func(string) error) error
}

// ErrNoStreaming is an agent that answers but cannot stream.
var ErrNoStreaming = errors.New("apps: this agent cannot stream; use ask")

// AgentCap serves the `agent.*` methods.
type AgentCap struct {
	agent  AgentSession
	appID  string
	redact Redactor
}

// NewAgent builds the agent capability. The prompt an app sends is text the app
// composed out of the user's memory, so it goes through the detector before it
// leaves for a model — including a model that is not on this machine.
func NewAgent(agent AgentSession, appID string, redact Redactor) (*AgentCap, error) {
	if agent == nil {
		return nil, errors.New("apps: agent capability needs an agent")
	}
	if redact == nil {
		return nil, ErrNoRedactor
	}
	return &AgentCap{agent: agent, appID: appID, redact: redact}, nil
}

// Ask sends one prompt.
func (a *AgentCap) Ask(ctx context.Context, prompt, model string) (string, error) {
	clean, _ := a.redact.Redact(prompt)
	return a.agent.Ask(ctx, a.appID, clean, model)
}

// Stream sends one prompt and emits the reply in pieces.
func (a *AgentCap) Stream(ctx context.Context, prompt string, emit func(string) error) error {
	clean, _ := a.redact.Redact(prompt)
	return a.agent.Stream(ctx, a.appID, clean, emit)
}

// ------------------------------------------------------------------ storage --

// Storage is `ctx.storage`: private to this app, on this user's box.
//
// It is served by the host rather than mounted into the sandbox, which is the
// whole reason it can be described as private. A directory the app can write to
// is a directory whose path the app knows, and §5's "no access to … other apps'
// data" then depends on the app not walking up one level. Served over the
// capability channel, an app has no path at all — it has a key/value interface
// and the keys are namespaced by the id it was installed under.
type Storage interface {
	Get(ctx context.Context, appID, key string) (json.RawMessage, error)
	Set(ctx context.Context, appID, key string, value json.RawMessage) error
	Delete(ctx context.Context, appID, key string) error
}

// MaxStorageValue caps one stored value. An app is free to keep state; it is not
// free to fill the user's disk one `set` at a time.
const MaxStorageValue = 1 << 20 // 1 MiB

// MaxStorageKeys caps how many keys one app may hold.
const MaxStorageKeys = 1024

// ErrStorageFull is a set that would exceed [MaxStorageKeys].
var ErrStorageFull = errors.New("apps: this app has used all of its storage keys")

// FileStorage is a one-file-per-key store under a directory relayd owns.
type FileStorage struct {
	mu   sync.Mutex
	root string
}

// NewFileStorage opens a per-app store. dir is [Layout.Data], which is on
// relayd's side of the sandbox boundary.
func NewFileStorage(dir string) (*FileStorage, error) {
	if dir == "" {
		return nil, errors.New("apps: storage needs a directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("apps: storage directory: %w", err)
	}
	return &FileStorage{root: dir}, nil
}

// ValidStorageKey refuses anything that could leave the directory a file-backed
// implementation would use.
//
// It is checked at the *boundary* — [Host] runs it before any [Storage] sees the
// key — rather than inside one implementation, because the key arrives from
// untrusted code and every implementation would otherwise have to remember. The
// app never sees a path, but it does choose the key, and "../../../etc/passwd"
// is the first thing anybody tries.
func ValidStorageKey(key string) error {
	if key == "" {
		return errors.New("apps: storage key cannot be empty")
	}
	if len(key) > 128 {
		return errors.New("apps: storage key is too long")
	}
	for _, r := range key {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '-' || r == '_' || r == ':'
		if !ok {
			return fmt.Errorf("apps: storage key %q may use letters, digits and . - _ : only", key)
		}
	}
	if strings.Contains(key, "..") {
		return fmt.Errorf("apps: storage key %q is not a path", key)
	}
	return nil
}

// keyPath maps a key to a file. It re-checks the key: defence in depth against a
// caller that reached this implementation without going through [Host].
func (s *FileStorage) keyPath(appID, key string) (string, error) {
	if err := ValidStorageKey(key); err != nil {
		return "", err
	}
	return filepath.Join(s.root, Slug(appID)+"."+key+".json"), nil
}

func (s *FileStorage) Get(_ context.Context, appID, key string) (json.RawMessage, error) {
	p, err := s.keyPath(appID, key)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

func (s *FileStorage) Set(_ context.Context, appID, key string, value json.RawMessage) error {
	p, err := s.keyPath(appID, key)
	if err != nil {
		return err
	}
	if len(value) > MaxStorageValue {
		return fmt.Errorf("apps: storage value is %d bytes, over the %d-byte limit", len(value), MaxStorageValue)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		entries, err := os.ReadDir(s.root)
		if err == nil && len(entries) >= MaxStorageKeys {
			return ErrStorageFull
		}
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, value, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *FileStorage) Delete(_ context.Context, appID, key string) error {
	p, err := s.keyPath(appID, key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// MemoryStorage is the in-memory store, for tests.
type MemoryStorage struct {
	mu   sync.Mutex
	vals map[string]json.RawMessage
}

func (s *MemoryStorage) key(appID, key string) string { return Slug(appID) + "\x00" + key }

func (s *MemoryStorage) Get(_ context.Context, appID, key string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vals[s.key(appID, key)], nil
}

func (s *MemoryStorage) Set(_ context.Context, appID, key string, value json.RawMessage) error {
	if len(value) > MaxStorageValue {
		return fmt.Errorf("apps: storage value is over the %d-byte limit", MaxStorageValue)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vals == nil {
		s.vals = map[string]json.RawMessage{}
	}
	s.vals[s.key(appID, key)] = append(json.RawMessage(nil), value...)
	return nil
}

func (s *MemoryStorage) Delete(_ context.Context, appID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vals, s.key(appID, key))
	return nil
}
