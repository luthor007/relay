package transcript

import (
	"fmt"
	"sort"
	"strings"
)

// Choosing a recogniser.
//
// SYSTEM.md §8's STT row is the whole specification: "**streaming, ours** — the
// glasses have no recogniser (§7b). Phone-native first because it is free and
// offline; cloud STT when a noisy room beats it. Streaming either way, so the
// prompt is ready the moment they stop talking."
//
// Three things that follow, and each is a branch below rather than a preference:
//
//   - **Phone-native first.** It costs nothing, works on a plane, and the audio
//     never leaves the handset. It is the default whenever the phone actually
//     recognised something and the conditions did not fight it.
//   - **Cloud only when it earns it, and only when it is allowed.** Sending
//     someone's conversation to a vendor is a consent decision (ARCHITECTURE.md
//     §6, MEMORY.md §6), so [Conditions.CloudAllowed] gates it and the router
//     never infers it from a key being configured.
//   - **The nightly bulk sync is not the phone's job.** The handset recognised a
//     voice turn, not sixteen hours; §7b's latency argument does not apply to a
//     file that arrives at 3 a.m., and the right recogniser there is whichever
//     is most accurate.
//
// And it degrades visibly: with nothing configured the answer is
// [ErrNoRecognizer] and a reason, never a silent zero-length transcript. A box
// that stops producing transcripts should say so, because the failure is
// invisible until someone asks what they said last Tuesday.

// Conditions describe one recognition job.
type Conditions struct {
	// PhoneText is what the handset recognised, empty if it did not.
	PhoneText string
	// PhoneConfidence is what it reported, 0 where it reported nothing.
	PhoneConfidence float64
	// NoisyRoom is the phone saying its own recogniser is fighting the
	// environment. It is a signal from the client rather than something the box
	// measures, and it is named for what it is.
	NoisyRoom bool
	// Offline means no network. A cloud recogniser cannot be chosen.
	Offline bool
	// Bulk marks the nightly sync — a file, not a turn.
	Bulk bool
	// CloudAllowed is whether this audio may leave the machine. It is an
	// explicit grant, never inferred from a configured key.
	CloudAllowed bool
	// Codec is what the audio is, so a recogniser that cannot take it is not
	// chosen and then failed.
	Codec string
	// Diarize asks for speaker labels.
	Diarize bool
}

// Choice is the router's answer.
type Choice struct {
	Source Source
	// Recognizer is nil when Source is [SourcePhone]: there is nothing to run,
	// the text already exists.
	Recognizer Recognizer
	// Why is the reason, in words. It reaches the console, and — when the
	// answer is a degraded one — the user.
	Why string
	// Degraded marks a choice that is worse than the one the conditions asked
	// for, with Why naming what was unavailable.
	Degraded bool
}

// MinPhoneConfidence is the floor below which a phone-side result is treated as
// a guess rather than a transcript.
//
// 0.5 rather than something tuned: nobody has measured this path on the
// hardware, and a threshold presented as tuned would be a claim §7b explicitly
// leaves open ("whether phone-side ASR is good enough for a noisy room, or cloud
// STT is needed"). It is a starting point with its own uncertainty written down.
const MinPhoneConfidence = 0.5

// Router picks a recogniser.
type Router struct {
	local []Recognizer
	cloud []Recognizer
}

// NewRouter builds a router over whatever is configured. Nothing configured is
// a supported state, not a broken one — the same rule MEMORY.md §3 states for
// the embedder — and it produces [ErrNoRecognizer] with a reason at the point of
// use rather than a failure to start.
func NewRouter(recognizers ...Recognizer) *Router {
	r := &Router{}
	for _, rec := range recognizers {
		if rec == nil {
			continue
		}
		switch rec.Source() {
		case SourceCloud:
			r.cloud = append(r.cloud, rec)
		default:
			r.local = append(r.local, rec)
		}
	}
	return r
}

// Recognizers lists what is configured, local first, for the health surface.
func (r *Router) Recognizers() []string {
	var out []string
	for _, rec := range append(append([]Recognizer{}, r.local...), r.cloud...) {
		out = append(out, rec.Name()+" ("+string(rec.Source())+")")
	}
	sort.Strings(out)
	return out
}

// Choose picks a recogniser for one job.
func (r *Router) Choose(c Conditions) (Choice, error) {
	// The bulk path first, because the phone's answer is not on offer there.
	if c.Bulk {
		return r.chooseMachine(c, "the nightly sync is a file rather than a turn, so accuracy beats latency")
	}

	phoneUsable := strings.TrimSpace(c.PhoneText) != "" &&
		(c.PhoneConfidence == 0 || c.PhoneConfidence >= MinPhoneConfidence) &&
		!c.NoisyRoom

	if phoneUsable {
		return Choice{
			Source: SourcePhone,
			Why:    "the phone already recognised this, which is free, offline and never leaves the handset",
		}, nil
	}

	// The phone either did not recognise, was not confident, or said the room
	// was against it. That is when SYSTEM.md §8 says to spend something.
	reason := "the phone did not recognise this turn"
	switch {
	case c.NoisyRoom && strings.TrimSpace(c.PhoneText) != "":
		reason = "the phone reported a noisy room, which is the case §8 says cloud STT is for"
	case strings.TrimSpace(c.PhoneText) != "":
		reason = fmt.Sprintf("the phone's confidence was %.2f, below the %.2f floor", c.PhoneConfidence, MinPhoneConfidence)
	}

	choice, err := r.chooseMachine(c, reason)
	if err != nil {
		// Nothing to run, but the phone did send *something*. A low-confidence
		// transcript that says it is low-confidence beats no transcript at all,
		// and it beats a silent one by more.
		if strings.TrimSpace(c.PhoneText) != "" {
			return Choice{
				Source:   SourcePhone,
				Degraded: true,
				Why:      reason + ", and no recogniser is configured here, so its own text is used as-is",
			}, nil
		}
		return Choice{}, err
	}
	return choice, nil
}

// chooseMachine picks between the recognisers that actually run.
func (r *Router) chooseMachine(c Conditions, because string) (Choice, error) {
	cloudBlocked := ""
	switch {
	case len(r.cloud) == 0:
		cloudBlocked = "no cloud recogniser is configured"
	case c.Offline:
		cloudBlocked = "there is no network"
	case !c.CloudAllowed:
		cloudBlocked = "this audio has no grant to leave the machine"
	}

	// Cloud is preferred exactly where §8 says it earns its place: a noisy room.
	// Everywhere else local wins, because it is free and the audio stays put.
	if c.NoisyRoom && cloudBlocked == "" {
		if rec := pick(r.cloud, c); rec != nil {
			return Choice{Source: SourceCloud, Recognizer: rec,
				Why: because + ", so " + rec.Name() + " is running it in the cloud"}, nil
		}
	}
	if rec := pick(r.local, c); rec != nil {
		ch := Choice{Source: rec.Source(), Recognizer: rec,
			Why: because + ", so " + rec.Name() + " is running it here"}
		if c.NoisyRoom && cloudBlocked != "" {
			ch.Degraded = true
			ch.Why += " — a noisy room is where cloud STT wins, but " + cloudBlocked
		}
		return ch, nil
	}
	if cloudBlocked == "" {
		if rec := pick(r.cloud, c); rec != nil {
			return Choice{Source: SourceCloud, Recognizer: rec,
				Why: because + ", so " + rec.Name() + " is running it in the cloud"}, nil
		}
	}

	detail := "no local recogniser is configured"
	if len(r.local) > 0 {
		detail = "no configured recogniser accepts " + c.Codec
	}
	if cloudBlocked != "" {
		detail += " and " + cloudBlocked
	}
	return Choice{}, fmt.Errorf("%w: %s (%s)", ErrNoRecognizer, detail, because)
}

// pick returns the first recogniser that can actually do the job. Capability is
// checked rather than assumed, so a codec mismatch is a routing decision instead
// of a failure halfway through a stream.
func pick(rs []Recognizer, c Conditions) Recognizer {
	var fallback Recognizer
	for _, rec := range rs {
		if err := CheckCodec(rec, c.Codec); err != nil {
			continue
		}
		if c.Offline && !rec.Capabilities().Offline {
			continue
		}
		if c.Diarize && !rec.Capabilities().Diarization {
			// Kept as a fallback rather than skipped: a transcript with no
			// speaker labels is worth having, and [Pipeline] records that the
			// labels are missing rather than inventing them.
			if fallback == nil {
				fallback = rec
			}
			continue
		}
		return rec
	}
	return fallback
}
