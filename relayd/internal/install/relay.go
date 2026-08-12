package install

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/luthor007/relay/relayd/internal/config"
)

// Step 7b — reaching this machine from outside the house. SYSTEM.md §7.
//
// Until this step existed, `relay setup` wrote every section of the config
// except `[relay]`, so the relay was a feature you could only turn on by editing
// a TOML file you had to already know about. The daemon reported "no relay is
// configured, so this machine is reachable only on its own network" and was
// telling the truth about a decision nobody had been offered.
//
// # Why it is a question rather than a default
//
// The relay is one we run, and it is free even for self-hosters — so the easy
// thing would be to switch it on and say nothing. That would be wrong twice
// over. It is the one part of a self-hosted install where traffic leaves the
// house, and §7's whole claim is that the relay cannot read it; a claim like
// that is worth nothing if the person it protects was never told it applied to
// them. And a box on a LAN the owner never leaves genuinely does not need it, so
// "no" is a real answer rather than a talked-out-of one.
//
// What the copy has to be exact about is the difference between *cannot read*
// and *knows nothing*. The relay sees that a box id is online and that somebody
// connected to it. It cannot see what they said. Overstating that would be the
// same mistake as hiding it.

// DefaultRelay is the relay we run.
//
// It lives here rather than in the config package because it is a product fact
// — the hostname of a service we operate — and `config` should stay a
// description of the file's shape. A self-hoster pointing at their own relay
// overrides it and nothing else changes.
//
// It must match what `deploy/relay/fly.toml` actually serves. Changing the
// deployment's hostname without changing this constant silently breaks every
// box already installed, because they dial what they were told at install time
// and nothing re-reads it.
const DefaultRelay = "wss://rz.relay.glass"

// RelayOutcome is what the relay step decided.
type RelayOutcome struct {
	// URL is empty when the machine will only ever be reached on its own
	// network, which is a supported and common state rather than a failure.
	URL string
	// Chosen is the menu id, so a scripted run and a test can assert on the
	// decision rather than on the string it produced.
	Chosen   string
	Warnings []string
}

// Enabled reports whether this install turned the relay on.
func (r RelayOutcome) Enabled() bool { return strings.TrimSpace(r.URL) != "" }

// Line is what the installer prints about it, and what `relay doctor` echoes.
func (r RelayOutcome) Line() string {
	if !r.Enabled() {
		return "off — this machine is reachable on its own network only"
	}
	return "on, through " + r.URL
}

const relayBody = "Your phone can always reach this machine when they are on the same network. " +
	"Away from home — on cellular, at work — it cannot, because this machine is behind a " +
	"router that does not accept incoming connections.\n\n" +
	"The relay fixes that without any router configuration: both sides dial out to it and it " +
	"passes bytes between them. It is ours, it is free, and there is nothing to run.\n\n" +
	"What it can see: that a machine with your box's identifier is online, and that something " +
	"connected to it. What it cannot see: anything you or your agents say. The traffic is " +
	"end-to-end encrypted between your phone and this machine, and the relay holds no logs of " +
	"it and nothing on disk at all.\n\n" +
	"If your phone and this machine are always on the same network, you do not need this."

// chooseRelay asks whether this box should be reachable from outside.
func chooseRelay(ctx context.Context, opts Options) (RelayOutcome, error) {
	p := opts.Prompt
	p.Section("Reaching this machine from anywhere", relayBody)

	// Already configured wins without a question. Re-running the installer on a
	// working box should not offer to turn off something that is on — the
	// second run is usually about something else entirely, and a menu whose
	// default silently differs from the current state is how a re-install
	// breaks a feature nobody was thinking about.
	if existing := strings.TrimSpace(opts.Config.Relay.URL); existing != "" {
		p.Say("  Already configured: %s. Leaving it alone.", existing)
		return RelayOutcome{URL: existing, Chosen: "existing"}, nil
	}

	choice, err := p.Select(Question{
		ID:    "relay",
		Title: "Reach this machine from anywhere?",
		Choices: []Choice{
			{ID: "hosted", Label: "Yes — use the relay we run", Hint: DefaultRelay, Recommended: true},
			{ID: "own", Label: "Yes — use my own relay", Hint: "you run cmd/relay-rendezvous somewhere"},
			{ID: "off", Label: "No — this machine is only reached on its own network", Last: true},
		},
		Default: "hosted",
	})
	if err != nil {
		return RelayOutcome{}, err
	}

	switch choice {
	case "off":
		return RelayOutcome{Chosen: "off"}, nil

	case "own":
		raw, err := p.Input(Input{
			ID:     "relay-url",
			Prompt: "Relay URL",
			Body:   "ws:// or wss://, with no path. For example wss://relay.example.",
		})
		if err != nil {
			return RelayOutcome{}, err
		}
		clean, problem := normaliseRelayURL(raw)
		if problem != "" {
			// Refused here rather than at the next start. A URL that fails
			// `config.validate` makes the daemon refuse to load the whole file,
			// so a typo typed at this prompt would present as a box that no
			// longer starts — with the error naming a config file the user has
			// never opened.
			return RelayOutcome{
				Chosen:   "own",
				Warnings: []string{"the relay was left off: " + problem},
			}, nil
		}
		return RelayOutcome{URL: clean, Chosen: "own"}, nil

	default:
		return RelayOutcome{URL: DefaultRelay, Chosen: "hosted"}, nil
	}
}

// normaliseRelayURL trims what a person types into what the daemon accepts, or
// says why it cannot.
//
// The forgiveness is deliberate and bounded. A trailing slash and a pasted
// `https://` are the two things people actually type, and both have one obvious
// reading — but only the slash is silently fixed. An `https://` is *named*,
// because somebody writing it is assuming the relay is an HTTP API, and quietly
// rewriting the scheme would hide that they are two protocols with different
// failure modes. That is the same argument `config.Relay.validate` makes, and
// this exists so the message arrives while the person is still sitting there.
func normaliseRelayURL(raw string) (string, string) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", "no URL was given"
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Sprintf("%q is not a URL", raw)
	}
	if u.Scheme == "" {
		// A bare hostname is the most likely thing anyone types, and "that is
		// not a URL" is true and useless. Say what to add.
		if u.Host == "" && u.Path != "" && !strings.Contains(u.Path, " ") {
			return "", fmt.Sprintf("%q has no scheme; the relay's address starts with ws:// or wss:// — try wss://%s", raw, u.Path)
		}
		return "", fmt.Sprintf("%q has no scheme; try ws:// or wss://", raw)
	}
	if u.Host == "" {
		return "", fmt.Sprintf("%q is not a URL", raw)
	}
	switch u.Scheme {
	case "ws", "wss":
	case "http", "https":
		return "", fmt.Sprintf("%q is %s://, and the relay speaks ws:// or wss:// — "+
			"try %s", raw, u.Scheme, "wss://"+u.Host)
	default:
		return "", fmt.Sprintf("%q is not a ws:// or wss:// URL", raw)
	}
	if u.Path != "" && u.Path != "/" {
		// The daemon appends `/rz/v1/...` itself, so a path here produces a URL
		// that is wrong in a way that only shows up as a connection that never
		// completes.
		return "", fmt.Sprintf("%q has a path on it; the relay's address is just the host", raw)
	}

	// The same refusal config.validate makes, made early: an unencrypted hop to
	// a public host leaks who is talking to whom, which is the metadata §7 is
	// otherwise careful about.
	if u.Scheme == "ws" && !isLoopback(u.Hostname()) {
		return "", fmt.Sprintf("%q is ws:// to a public host; use wss://", raw)
	}

	cfg := config.Relay{URL: trimmed}
	if !cfg.Enabled() {
		return "", fmt.Sprintf("%q is empty after trimming", raw)
	}
	return trimmed, ""
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

// ctx is unused today and is in the signature on purpose: every other step
// takes one, and the relay step is the obvious place a future check ("can this
// machine actually reach that relay?") would go. Leaving it out would mean
// changing every caller to add it back.
var _ = context.Background
