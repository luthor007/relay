// Package pairing is the three facts a phone needs to reach this machine, and
// the one string that carries them.
//
// The phone's problem is not the protocol — that has worked for as long as the
// two ends have agreed on a socket — it is that nothing durable existed to point
// it at. relayd minted a fresh API token on every start and printed it to a log
// nobody reads, so a phone paired on Tuesday was unauthorized on Wednesday, and
// the `relay pair` command printed a decorative code that no daemon has ever
// checked.
//
// So: the token becomes durable, in the same way and for the same reason the box
// identity already is, and both are readable by the CLI that has to tell the
// user about them.
package pairing

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Scheme is the pairing link's scheme. It is registered by the iOS app, so a
// link scanned from a QR code or tapped in a message opens the app with the box
// already in hand.
const Scheme = "relay"

// TokenFile and BoxIDFile live beside the databases, 0600.
const (
	TokenFile = "api-token"
	BoxIDFile = "box-id"
)

// ErrNoDataDir is returned when there is nowhere durable to keep these.
var ErrNoDataDir = errors.New("pairing: no data directory")

// Token returns this machine's API token, durably.
//
// Configured wins — `relayd -token` is how somebody pins one deliberately, and
// how a test gets a predictable value. Otherwise it is generated once and kept,
// because a token that changes on restart is a phone that unpairs itself at
// every reboot, which is indistinguishable from the app being broken.
func Token(configured, dataDir string) (string, error) {
	return durable(configured, dataDir, TokenFile, newSecret)
}

// BoxID returns this machine's durable name at the relay.
//
// Not a secret: config.Relay says at length that anyone who learns it gets as
// far as a stranger on the LAN, which is nowhere, because the API
// authenticates — see api.Server.ServeRelayedSocket, which is what makes that
// sentence true over the relay too.
func BoxID(configured, dataDir string) (string, error) {
	return durable(configured, dataDir, BoxIDFile, newBoxID)
}

// newBoxID is a secret with a prefix, so that an id read aloud, pasted into a
// support message or found in a log is recognisable as a box and not mistaken
// for the token — which is the same shape and is not public.
func newBoxID() (string, error) {
	s, err := newSecret()
	if err != nil {
		return "", err
	}
	return "box-" + s, nil
}

// durable reads a value, or mints and keeps one.
func durable(configured, dataDir, name string, mint func() (string, error)) (string, error) {
	if v := strings.TrimSpace(configured); v != "" {
		return v, nil
	}
	if strings.TrimSpace(dataDir) == "" {
		return "", ErrNoDataDir
	}
	path := filepath.Join(dataDir, name)

	if b, err := os.ReadFile(path); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
		// An empty file is a half-finished write from a previous start. Falling
		// through regenerates rather than running with the empty string, which
		// as a token would authenticate nobody and as an id would collide with
		// every other box that did the same.
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	v, err := mint()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	// Through a temporary file, so a torn write is only ever a power cut rather
	// than a crash mid-start.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(v+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return v, nil
}

// Read returns a durable value without minting one. It is what a command that
// only reports on state uses, so `relay pair` on a machine that has never run
// relayd says so rather than inventing a token nothing accepts.
func Read(dataDir, name string) (string, bool) {
	if strings.TrimSpace(dataDir) == "" {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(dataDir, name))
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(b))
	return v, v != ""
}

// newSecret mints 80 bits, Crockford-ish base32 without padding, so it survives
// being read aloud, pasted into a URL, or typed by somebody who has given up on
// the QR code.
func newSecret() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

// Link is the one string a phone needs: relay://<box>:<token>@<relay host>.
//
// One string rather than three fields because the token is forty random
// characters, and the difference between one paste and three is the difference
// between a feature and a thing nobody uses.
func Link(relayURL, boxID, token string) (string, error) {
	if strings.TrimSpace(relayURL) == "" {
		return "", errors.New("pairing: no relay is configured, so there is nothing for a phone " +
			"outside this network to reach")
	}
	if boxID == "" || token == "" {
		return "", errors.New("pairing: this machine has no durable identity yet; start relayd once")
	}
	u, err := url.Parse(relayURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("pairing: %q is not a relay address", relayURL)
	}
	return (&url.URL{
		Scheme: Scheme,
		User:   url.UserPassword(boxID, token),
		Host:   u.Host,
	}).String(), nil
}
