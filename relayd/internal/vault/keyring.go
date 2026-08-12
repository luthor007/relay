package vault

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// Keyring is the OS keychain, behind an interface so tests do not need one and
// so a build container without dbus is a supported environment rather than a
// failure.
type Keyring interface {
	Set(service, user, secret string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

// ErrKeyringUnavailable means this machine has no usable keychain. On Linux
// that is usually no D-Bus session and no secret service — which is exactly the
// case in a container, and on a headless always-on box, which is the machine
// this product is installed on. Degrading has to work.
var ErrKeyringUnavailable = errors.New("vault: no usable OS keychain")

// OSKeyring is the real keychain: Keychain on macOS, libsecret on Linux,
// Credential Manager on Windows.
type OSKeyring struct{}

func (OSKeyring) Set(service, user, secret string) error {
	return keyring.Set(service, user, secret)
}

func (OSKeyring) Get(service, user string) (string, error) {
	v, err := keyring.Get(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return v, err
}

func (OSKeyring) Delete(service, user string) error {
	err := keyring.Delete(service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// probeKeyring writes, reads and deletes a canary. Nothing about a keychain can
// be assumed from the platform alone: macOS in a CI runner has one that refuses
// to unlock, and Linux may or may not have a secret service running.
func probeKeyring(kr Keyring, service string) error {
	if kr == nil {
		return ErrKeyringUnavailable
	}
	const canary = "relay-keychain-probe"
	if err := kr.Set(service, canary, "ok"); err != nil {
		return fmt.Errorf("%w: %v", ErrKeyringUnavailable, err)
	}
	got, err := kr.Get(service, canary)
	if err != nil {
		_ = kr.Delete(service, canary)
		return fmt.Errorf("%w: %v", ErrKeyringUnavailable, err)
	}
	_ = kr.Delete(service, canary)
	if got != "ok" {
		return fmt.Errorf("%w: canary read back as %q", ErrKeyringUnavailable, got)
	}
	return nil
}

// MemoryKeyring is an in-process keychain for tests.
type MemoryKeyring struct {
	items map[string]string
	// FailAll makes every operation fail, which is how a test reproduces a
	// machine with no keychain.
	FailAll bool
}

// NewMemoryKeyring builds an empty in-process keychain.
func NewMemoryKeyring() *MemoryKeyring { return &MemoryKeyring{items: map[string]string{}} }

func (m *MemoryKeyring) key(service, user string) string { return service + "\x00" + user }

func (m *MemoryKeyring) Set(service, user, secret string) error {
	if m.FailAll {
		return ErrKeyringUnavailable
	}
	if m.items == nil {
		m.items = map[string]string{}
	}
	m.items[m.key(service, user)] = secret
	return nil
}

func (m *MemoryKeyring) Get(service, user string) (string, error) {
	if m.FailAll {
		return "", ErrKeyringUnavailable
	}
	v, ok := m.items[m.key(service, user)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *MemoryKeyring) Delete(service, user string) error {
	if m.FailAll {
		return ErrKeyringUnavailable
	}
	delete(m.items, m.key(service, user))
	return nil
}
