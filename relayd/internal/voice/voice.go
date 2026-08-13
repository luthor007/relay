// Package voice is ORCHESTRATOR.md §2a: choosing a voice.
//
// Speech is the output channel, so this is a step with a recommendation, not an
// advanced setting. The shape is a primary provider plus an automatic fallback,
// and two rows carry copy that is load-bearing rather than decorative:
//
//   - **The keyless default exists so the device is never mute.** A buyer who
//     skips this step must still have a device that talks. SYSTEM.md §7c calls
//     "mute out of the box" the worst possible first hour for a voice product,
//     and this fallback is what replaced the starter-allowance idea entirely —
//     it costs nothing, it never runs out mid-month, and it makes upgrading to
//     Simba a quality decision rather than a rescue from silence.
//   - **Phone-native is the FASTEST option and the worst-sounding one.**
//     Synthesis happens on the handset with no round trip at all. Describing it
//     as "slower and worse" is wrong in a way a user notices in the first
//     minute, so the hint says what is actually true: free and instant, and it
//     sounds like a robot in your ear all day.
//
// The catalog is data. internal/install renders it; it does not invent it.
package voice

import (
	"fmt"
	"strings"
)

// Where synthesis physically happens, which is what actually decides latency.
type Synthesis string

const (
	// SynthHosted is the provider's servers: a network round trip.
	SynthHosted Synthesis = "hosted"
	// SynthDevice is the handset itself. No network hop, so it is the fastest
	// option available and the reason the phone row must not be described as
	// slow.
	SynthDevice Synthesis = "device"
	// SynthLocal is this machine — a Piper or Kokoro process next to relayd.
	SynthLocal Synthesis = "local"
)

// API is the request shape a hosted provider speaks.
type API string

const (
	APISimba      API = "simba"
	APIOpenAI     API = "openai"
	APIElevenLabs API = "elevenlabs"
	APICartesia   API = "cartesia"
	APIDeepgram   API = "deepgram"
	// APIEdge is the keyless hosted default. Synthesis runs over a websocket to
	// an undocumented Microsoft endpoint (SYSTEM.md §7c names that as a
	// dependency we do not control), so what this package can check is
	// reachability, and it says so rather than implying more.
	APIEdge API = "edge"
	// APINone is a row this machine cannot exercise at all.
	APINone API = ""
	// APILocal is an OpenAI-shaped endpoint on localhost, which is what both
	// Piper and Kokoro's usual servers expose.
	APILocal API = "local"
)

// Option is one row of the voice menu.
type Option struct {
	ID     string
	Label  string
	Vendor string

	// Recommended marks Simba 3.2. Exactly one row carries it.
	Recommended bool
	// Keyless marks the row that exists so a skipped step still talks. Exactly
	// one row carries it, and [Fallback] returns that one.
	Keyless bool
	// Default marks the row a fresh install starts on.
	Default bool

	Synthesis Synthesis
	// Streams is whether first audio arrives before the sentence ends, which is
	// SYSTEM.md §7b's largest perceived-latency win.
	Streams bool

	// The three columns of ORCHESTRATOR.md §2a's table.
	Quality string
	Latency string
	Cost    string

	// Hint is the sentence under the row. On two rows it is the whole point.
	Hint string
	// Note is a paragraph printed when this row is chosen.
	Note string

	NeedsCredential bool
	API             API
	BaseURL         string
	DefaultModel    string
	DefaultVoice    string

	// Probeable is whether this machine can make a real call to test the
	// choice. False on the phone row, because the synthesiser is not here.
	Probeable bool
	// ProbeNote says what a probe does or does not prove for this row.
	ProbeNote string
}

// Catalog is the voice menu, in the order ORCHESTRATOR.md §2a lists it:
// the recommendation first, the paid alternatives, the aggregator, then the
// three free rows — keyless, phone, and self-hosted.
func Catalog() []Option {
	return []Option{
		{
			ID: "simba", Label: "Simba 3.2", Vendor: "simba",
			Recommended: true, Default: true,
			Synthesis: SynthHosted, Streams: true,
			Quality:         "best of the list",
			Latency:         "streams; first audio before the sentence ends",
			Cost:            "$10 / M chars",
			Hint:            "best on the list, streams, about $1.47 a month",
			NeedsCredential: true, API: APISimba,
			BaseURL: "https://api.simba.audio/v1", DefaultModel: "simba-3.2", DefaultVoice: "aria",
			Probeable: true, ProbeNote: "synthesises one word before the installer exits",
		},
		{
			ID: "elevenlabs", Label: "ElevenLabs", Vendor: "elevenlabs",
			Synthesis: SynthHosted, Streams: true,
			Quality: "good to very good", Latency: "streams",
			Cost:            "per-character, check the provider",
			Hint:            "very good, streams, priced per character",
			NeedsCredential: true, API: APIElevenLabs,
			BaseURL: "https://api.elevenlabs.io/v1", DefaultModel: "eleven_turbo_v2_5",
			DefaultVoice: "21m00Tcm4TlvDq8ikWAM",
			Probeable:    true, ProbeNote: "synthesises one word before the installer exits",
		},
		{
			ID: "cartesia", Label: "Cartesia", Vendor: "cartesia",
			Synthesis: SynthHosted, Streams: true,
			Quality: "good to very good", Latency: "streams",
			Cost:            "per-character, check the provider",
			Hint:            "very good, low-latency streaming, per character",
			NeedsCredential: true, API: APICartesia,
			BaseURL: "https://api.cartesia.ai", DefaultModel: "sonic-2",
			Probeable: true, ProbeNote: "synthesises one word before the installer exits",
		},
		{
			ID: "deepgram", Label: "Deepgram", Vendor: "deepgram",
			Synthesis: SynthHosted, Streams: true,
			Quality: "good to very good", Latency: "streams",
			Cost:            "per-character, check the provider",
			Hint:            "Aura voices, streams, per character",
			NeedsCredential: true, API: APIDeepgram,
			BaseURL: "https://api.deepgram.com/v1", DefaultModel: "aura-2-thalia-en",
			Probeable: true, ProbeNote: "synthesises one word before the installer exits",
		},
		{
			ID: "openai", Label: "OpenAI", Vendor: "openai",
			Synthesis: SynthHosted, Streams: true,
			Quality: "good to very good", Latency: "streams",
			Cost:            "per-character, check the provider",
			Hint:            "per character — one fewer account if you have an OpenAI key",
			NeedsCredential: true, API: APIOpenAI,
			BaseURL: "https://api.openai.com/v1", DefaultModel: "gpt-4o-mini-tts", DefaultVoice: "alloy",
			Probeable: true, ProbeNote: "synthesises one word before the installer exits",
		},
		{
			ID: "openrouter", Label: "OpenRouter", Vendor: "openrouter",
			Synthesis: SynthHosted, Streams: true,
			Quality: "whatever it fronts", Latency: "varies",
			Cost:            "one key, many voices",
			Hint:            "one key, many voices — the same key covers both models",
			NeedsCredential: true, API: APIOpenAI,
			BaseURL: "https://openrouter.ai/api/v1", DefaultModel: "openai/gpt-4o-mini-tts", DefaultVoice: "alloy",
			Probeable: true, ProbeNote: "synthesises one word before the installer exits",
		},
		{
			ID: "edge", Label: "Keyless default (Edge TTS)", Vendor: "edge",
			Keyless:   true,
			Synthesis: SynthHosted, Streams: true,
			Quality: "acceptable", Latency: "hosted, so a round trip",
			Cost:            "free, no credential",
			Hint:            "free, no key — skipping this step is safe, it still talks",
			NeedsCredential: false, API: APIEdge,
			BaseURL:   "https://speech.platform.bing.com/consumer/speech/synthesize/readaloud",
			Probeable: true,
			ProbeNote: "checks that the keyless service answers; synthesis itself runs over a " +
				"websocket to the same undocumented endpoint and is not exercised here",
		},
		{
			ID: "phone", Label: "Phone-native (iOS / Android)", Vendor: "phone",
			Synthesis: SynthDevice, Streams: true,
			Quality: "noticeably synthetic",
			Latency: "fastest — no network hop",
			Cost:    "free",
			// The one sentence in this file that must not be paraphrased.
			Hint:            "free and instant, but it sounds like a robot in your ear all day",
			NeedsCredential: false, API: APINone,
			Probeable: false,
			ProbeNote: "nothing on this machine can test it: the synthesiser is the handset. " +
				"The phone app reports its available voices when it pairs.",
		},
		{
			ID: "local", Label: "Self-hosted (Piper, Kokoro)", Vendor: "local",
			Synthesis: SynthLocal, Streams: true,
			Quality: "decent, not great", Latency: "fast and private",
			Cost:            "free, your GPU",
			Hint:            "Nothing leaves the machine. Point it at a local endpoint.",
			NeedsCredential: false, API: APILocal,
			DefaultModel: "piper", Probeable: true,
			ProbeNote: "synthesises one word against the local endpoint you configure",
		},
	}
}

// Get returns one option by id.
func Get(id string) (Option, bool) {
	for _, o := range Catalog() {
		if o.ID == id {
			return o, true
		}
	}
	return Option{}, false
}

// Recommended is Simba 3.2.
func Recommended() Option {
	for _, o := range Catalog() {
		if o.Recommended {
			return o
		}
	}
	panic("voice: no recommended option — ORCHESTRATOR.md §2a requires one")
}

// Fallback is the keyless row. It is the automatic fallback under every other
// choice, which is the whole reason it exists.
func Fallback() Option {
	for _, o := range Catalog() {
		if o.Keyless {
			return o
		}
	}
	panic("voice: no keyless option — a device that can go mute is not shippable")
}

// Plan is what the installer writes into the config: a primary and the fallback
// beneath it.
type Plan struct {
	Primary  string
	Fallback string
}

// DefaultPlan is Simba over the keyless default.
func DefaultPlan() Plan {
	return Plan{Primary: Recommended().ID, Fallback: Fallback().ID}
}

// ErrWouldBeMute is returned by Validate for a plan that could leave the device
// silent. SYSTEM.md §7c: "mute out of the box" is the worst possible first hour
// for a voice product, so this is an error and not a warning.
type ErrWouldBeMute struct{ Reason string }

func (e *ErrWouldBeMute) Error() string {
	return "voice: this configuration can leave the device mute — " + e.Reason
}

// Validate checks a plan can always produce audio.
func (p Plan) Validate() error {
	if p.Primary == "" && p.Fallback == "" {
		return &ErrWouldBeMute{Reason: "no primary voice and no fallback"}
	}
	if p.Primary != "" {
		if _, ok := Get(p.Primary); !ok {
			return fmt.Errorf("voice: unknown primary %q", p.Primary)
		}
	}
	if p.Fallback == "" {
		return &ErrWouldBeMute{Reason: "no fallback, so an expired key means silence"}
	}
	fb, ok := Get(p.Fallback)
	if !ok {
		return fmt.Errorf("voice: unknown fallback %q", p.Fallback)
	}
	if fb.NeedsCredential {
		return &ErrWouldBeMute{
			Reason: fb.Label + " needs a credential, so it cannot be the fallback: the " +
				"fallback exists precisely for the case where a credential has stopped working",
		}
	}
	return nil
}

// Describe renders the row for a terminal menu.
func (o Option) Describe() string {
	var b strings.Builder
	b.WriteString(o.Label)
	switch {
	case o.Recommended:
		b.WriteString("  (recommended)")
	case o.Keyless:
		b.WriteString("  (no key needed)")
	}
	b.WriteString("\n    ")
	b.WriteString(strings.Join([]string{o.Quality, o.Latency, o.Cost}, " · "))
	if o.Hint != "" {
		b.WriteString("\n    ")
		b.WriteString(o.Hint)
	}
	return b.String()
}
