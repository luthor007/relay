// Package api is relayd's one HTTP surface: the phone's WebSocket, the
// console's REST and SSE, and nothing else.
//
// One API for both deployments (DASHBOARD.md §5), so Relay Cloud is a proxy plus
// an auth layer rather than a second backend. One place for the authorization
// checks is the only way they stay consistent, and the vault sits behind them.
//
// The phone contract is SYSTEM.md §6.1, exactly: one authenticated WebSocket,
// JSON envelopes, both directions.
//
//	{ v: 1, id: "<uuid>", type: "<name>", at: <unix_ms>, payload: {...} }
//
// Chosen over gRPC or plain HTTP because the traffic is small, bidirectional and
// long-lived, and because a WebSocket survives a phone that sleeps and wakes far
// more gracefully than a stream that has to be re-established with state.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Version is the envelope version. A frame with any other value is refused
// rather than best-guessed: a phone from a future release talking to an old
// daemon should be told so, once, instead of half-working.
const Version = 1

// Envelope is SYSTEM.md §6.1's frame, in both directions.
type Envelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	At      int64           `json:"at"` // unix milliseconds
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Phone → server. SYSTEM.md §6.1's list, complete.
const (
	TypeUtterance       = "utterance"
	TypeTouch           = "touch"
	TypeWear            = "wear"
	TypeAudioChunk      = "audio.chunk"
	TypePhoto           = "photo"
	TypeSessionCommand  = "session.command"
	TypeConsentDecision = "consent.decision"
	TypeSyncOffer       = "sync.offer"
)

// Server → phone. SYSTEM.md §6.1's list, plus three frames named below.
const (
	TypeSpeak             = "speak"
	TypeUIRender          = "ui.render"
	TypeSessionList       = "session.list"
	TypeConfirmRequest    = "confirm.request"
	TypeConnectorProposal = "connector.proposal"
	TypeDigest            = "digest"

	// TypeAck and TypeError are transport frames rather than product ones: every
	// phone→server frame carries an id, and something has to say whether it
	// landed. Without them a phone cannot distinguish "delivered" from "the
	// socket is up but the daemon dropped it", which is the failure a
	// store-and-forward queue exists to handle.
	TypeAck   = "ack"
	TypeError = "error"

	// TypeAuth is phone → server, and only over a relayed socket. On the LAN
	// the credential rides the handshake as `Authorization: Bearer …`; through
	// the rendezvous relay that handshake terminates at the relay, which is a
	// pipe and not this daemon, so the header never arrives. The token becomes
	// the first frame instead. See [Server.ServeRelayedSocket].
	TypeAuth = "auth"

	// TypeNotify is the silent-but-present notification. ADAPTERS.md §7 requires
	// a channel that reaches the phone without speaking — quiet hours hold the
	// speech and keep the notification — and SYSTEM.md §4 already lists "push
	// notification {title, body, session_id?}" as an output. §6.1's list omitted
	// the frame that carries it.
	TypeNotify = "notify"

	// TypeConfirmResolved retracts a confirm.request that is no longer true: the
	// approval was answered in a terminal (Codex's serverRequest/resolved), or
	// the turn was cancelled. A ping that outlives its question wakes someone to
	// approve what is already approved.
	TypeConfirmResolved = "confirm.resolved"
)

// Error codes carried by TypeError.
const (
	CodeBadEnvelope    = "bad_envelope"
	CodeBadVersion     = "unsupported_version"
	CodeUnknownType    = "unknown_type"
	CodeBadPayload     = "bad_payload"
	CodeNotImplemented = "not_implemented"
	CodeNoSuchSession  = "no_such_session"
	CodeUnsupported    = "unsupported"
	CodeFailed         = "failed"

	// Console codes. The console renders each of these differently, so they are
	// distinct values rather than one generic refusal: "you may not" and "prove
	// it again" lead to opposite user actions.
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	// CodeReauthenticate is DASHBOARD.md §4's cloud rule: every vault write is
	// re-authenticated regardless of session age.
	CodeReauthenticate = "reauthenticate"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	// CodeUnavailable is a surface that exists but has nothing wired behind it
	// yet — no vault, no prober, no detection pass.
	CodeUnavailable = "unavailable"
	// CodeSelfHosted is a cloud-only route asked for on the free tier. It is not
	// an error the user caused, and the message says so.
	CodeSelfHosted = "self_hosted"
)

// ---------------------------------------------------------- phone → server --

// Utterance is recognised speech. The glasses have no recogniser (SYSTEM.md
// §7b) — the phone does the recognition and sends the text.
type Utterance struct {
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence,omitempty"`
	Source     string  `json:"source,omitempty"` // glasses | phone
	// Final distinguishes a streaming partial from the finished utterance.
	// Streaming ASR is the point (§7b): the prompt is ready the moment they
	// stop, rather than starting a 400 ms job at that point.
	Final bool `json:"final"`
}

// Touch is a gesture on the glasses.
type Touch struct {
	Gesture string `json:"gesture"` // tap1 | tap2 | tap3 | long | swipe+ | swipe-
}

// Wear is the glasses going on or off a face.
type Wear struct {
	Worn bool `json:"worn"`
}

// AudioChunk is Opus from the mic. M4.
type AudioChunk struct {
	Seq   int64  `json:"seq"`
	Codec string `json:"codec"`
	Data  []byte `json:"data"`
}

// Photo is an image from the glasses. M4.
type Photo struct {
	ID   string `json:"id"`
	MIME string `json:"mime"`
	Data []byte `json:"data,omitempty"`
	// Stored means the photo is still on the glasses. SYSTEM.md §3: photos stay
	// on the device and transfer on demand, not on capture.
	Stored bool `json:"stored"`
}

// SyncOffer is the phone offering a night's audio. M4.
type SyncOffer struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// OnLAN is whether the phone and the machine share a network. If they do
	// not, bulk sync waits rather than silently burning a data plan (§7).
	OnLAN bool `json:"on_lan"`
}

// SessionCommand is every session-directed action the phone can take.
type SessionCommand struct {
	// Command is list | send | cancel | steer | close | answer.
	Command string `json:"command"`
	Session string `json:"session,omitempty"`
	Turn    string `json:"turn,omitempty"`
	Text    string `json:"text,omitempty"`

	// Question, Option, Decision and Interrupt answer a blocked session.
	Question  string `json:"question,omitempty"`
	Option    string `json:"option,omitempty"`
	Decision  string `json:"decision,omitempty"` // allow | deny | cancelled
	Interrupt bool   `json:"interrupt,omitempty"`
}

// ConsentDecision answers a confirm.request. ActionID is the ping id, which is
// stable across the two-minute re-ping so an answer to either lands on the same
// question.
type ConsentDecision struct {
	ActionID  string `json:"action_id"`
	Approved  bool   `json:"approved"`
	Option    string `json:"option,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Message   string `json:"message,omitempty"`
}

// ---------------------------------------------------------- server → phone --

// Speak is something to say out loud.
type Speak struct {
	Text string `json:"text"`
	// Interrupt is the hard stop: a blocked session may speak over narration.
	Interrupt bool   `json:"interrupt,omitempty"`
	Session   string `json:"session,omitempty"`
	Ping      string `json:"ping,omitempty"`
}

// Notify is a phone notification. Silent is quiet hours: present, soundless.
type Notify struct {
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Sessions []string `json:"sessions,omitempty"`
	Silent   bool     `json:"silent,omitempty"`
	Ping     string   `json:"ping,omitempty"`
}

// SessionSummary is one row of the list, the same shape the console renders.
type SessionSummary struct {
	ID string `json:"id"`
	// NativeID is the runtime's own id, which is what the index keys on. It is
	// the join between the live tier and the historical one.
	NativeID   string   `json:"native_id,omitempty"`
	Runtime    string   `json:"runtime"`
	Subject    string   `json:"subject"`
	Workspace  string   `json:"workspace"`
	State      string   `json:"state"`
	LastActive int64    `json:"last_active"`
	CreatedAt  int64    `json:"created_at"`
	CostUSD    *float64 `json:"cost_usd"` // nil, never 0, where the runtime cannot report it
	Tokens     *int64   `json:"tokens"`
	// Blocked hoists a session waiting on a human. DASHBOARD.md §3.1 puts these
	// at the top, unmissable, because a blocked session is the one failure mode
	// that silently stops all work.
	Blocked   bool `json:"blocked"`
	Questions int  `json:"questions,omitempty"`
	Live      bool `json:"live"`

	// Everything below is the console's half of DASHBOARD.md §3.1 — "live and
	// historical, from the registry and the index" — and is absent on the
	// phone's frame, which only ever carries sessions the orchestrator drives.

	// Source is registry | index | both.
	Source string `json:"source,omitempty"`
	// Title is the runtime's own title where it wrote one (MEMORY.md §4: Claude
	// Code and Hermes both do). Subject is what the user called it.
	Title     string `json:"title,omitempty"`
	Model     string `json:"model,omitempty"`
	Messages  int64  `json:"messages,omitempty"`
	ToolCalls int64  `json:"tool_calls,omitempty"`
	// Transcript is a POINTER into the runtime's own file, never a copy
	// (MEMORY.md §3).
	Transcript *TranscriptRef `json:"transcript,omitempty"`
}

// TranscriptRef locates a session's transcript where it already lives.
//
// MEMORY.md §3 keeps the measured 3.6 GB on disk, in place, unmoved: the index
// holds a pointer, not a copy. So does this — the console opens the file
// through a bounded range read (GET /v1/sessions/{id}/transcript) rather than
// ever receiving it whole.
type TranscriptRef struct {
	Runtime string `json:"runtime"`
	Session string `json:"session"`
	Path    string `json:"path"`
	// ByteOffset is where this session starts in the file. Hermes and OpenClaw
	// keep every session in one store, so it is frequently not zero.
	ByteOffset int64 `json:"byte_offset"`
	// Size is the file's size as backfill last saw it.
	Size int64 `json:"size,omitempty"`
}

// SessionList is every session across every runtime.
type SessionList struct {
	Sessions []SessionSummary `json:"sessions"`
	At       int64            `json:"at"`
}

// ConfirmOption is one answer the agent will accept, spoken by name.
type ConfirmOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Standing means choosing it grants something beyond the action in front of
	// us — "allow always". ORCHESTRATOR.md §4b: the orchestrator must never
	// select one on the user's behalf, so the phone marks it and a human picks.
	Standing bool `json:"standing"`
}

// ConfirmRequest is a session blocked on a human.
type ConfirmRequest struct {
	ActionID string          `json:"action_id"`
	Session  string          `json:"session"`
	Runtime  string          `json:"runtime"`
	Ask      string          `json:"ask"` // permission | tool_value | elicitation
	Prompt   string          `json:"prompt"`
	Options  []ConfirmOption `json:"options,omitempty"`
	Tool     string          `json:"tool,omitempty"`
	Target   string          `json:"target,omitempty"`
	// Consequential is an action with effects outside the machine. Not
	// suppressible by batching or quiet hours.
	Consequential bool  `json:"consequential"`
	Deadline      int64 `json:"deadline,omitempty"`
	Repeat        int   `json:"repeat,omitempty"`
}

// UIRender is a mini-app's view on its way to the phone.
//
// ORCHESTRATOR.md §5: app code runs on the server, sandboxed; app UI renders in
// the phone app through a small declarative vocabulary the host draws natively.
// This is the frame between those two halves.
//
// View is [json.RawMessage] on purpose, and it is the boundary that keeps this
// package honest: `internal/api` is transport and does not know the vocabulary.
// `internal/apps` owns it, validates every view against it before one gets here,
// and is the only place the caps and the block kinds live on this side. If this
// struct grew a `Blocks []Block` field, the vocabulary would be defined in two
// packages in one binary and the transport would start having opinions about
// what an app may draw.
type UIRender struct {
	// ActionID is set only when an answer is expected, and it is what the phone
	// puts in the consent.decision it sends back — the same field, the same
	// route and the same server bookkeeping a confirm.request uses. An app's
	// question is a question like any other.
	ActionID string `json:"action_id,omitempty"`
	// App and AppName say who drew it. "Which of my apps is asking me this" is
	// the first question a confirmation raises and the view cannot answer it.
	App     string `json:"app"`
	AppName string `json:"appName,omitempty"`
	// Deadline is when the question stops standing, in unix milliseconds. The
	// phone dismisses it rather than leaving a button that no longer does
	// anything — an app that stopped waiting has already treated it as a no.
	Deadline int64           `json:"deadline,omitempty"`
	View     json.RawMessage `json:"view"`
}

// ConfirmResolved retracts a confirm.request.
type ConfirmResolved struct {
	ActionID string `json:"action_id"`
	Reason   string `json:"reason"`
}

// Ack says a frame landed.
type Ack struct {
	Re string `json:"re"`
	OK bool   `json:"ok"`
}

// ErrorPayload says a frame did not land, and why.
type ErrorPayload struct {
	Re      string `json:"re,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
	// Milestone names where the unimplemented half lives, so "not implemented"
	// is a schedule rather than a shrug.
	Milestone string `json:"milestone,omitempty"`
}

// ConnectorProposal is ORCHESTRATOR.md §4b's evidence-grounded suggestion.
//
//	You have mentioned your Prusa four times this week. Want me to connect it?
//	I could queue prints and tell you when they finish.
//
// One shape for both surfaces: this is the console row on the connectors screen
// and the payload of the [TypeConnectorProposal] frame the phone receives.
// DASHBOARD.md §5's "one API behind both deployments" is only true if the two
// carry the same thing, and a second near-identical struct for the console is
// how that stops being true.
//
// Access is always "read". §4b rule 2 makes the write half a second decision,
// so there is no field here for it to arrive in.
type ConnectorProposal struct {
	Connector string `json:"connector"`
	// Title is what the user is shown: "Prusa 3D printer".
	Title  string `json:"title,omitempty"`
	Access string `json:"access"`

	// Evidence is the counted sentence — "You have mentioned your Prusa four
	// times this week." — and Opens is what granting it would let the agent do
	// that it cannot now, in the connector's own words. Line is the two of them
	// as §4b writes it, which is what the glasses would speak.
	//
	// All three are built from counts and from the connector's own descriptor.
	// None of them quotes anything the user said: the proposer discards the
	// text once it has matched, so there is no path from an utterance into this
	// struct.
	Evidence string   `json:"evidence"`
	Opens    string   `json:"opens"`
	Line     string   `json:"line"`
	Scopes   []string `json:"scopes,omitempty"`

	// Episodes is separate conversations and Mentions is total sentences. Both,
	// because they are different claims and the sentence above counts episodes.
	Episodes int   `json:"episodes,omitempty"`
	Mentions int   `json:"mentions,omitempty"`
	FirstAt  int64 `json:"first_at,omitempty"`
	LastAt   int64 `json:"last_at,omitempty"`
}

// Digest is the daily summary.
type Digest struct {
	Notes       []string `json:"notes"`
	Commitments []string `json:"commitments"`
	Decisions   []string `json:"decisions"`
}

// ------------------------------------------------------------------ codec --

// ErrBadVersion is a frame from a client that speaks a different envelope.
var ErrBadVersion = errors.New("api: unsupported envelope version")

// Decode parses a frame and checks the envelope contract.
func Decode(b []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return Envelope{}, fmt.Errorf("api: %w", err)
	}
	if e.V != Version {
		return e, fmt.Errorf("%w: %d (this daemon speaks %d)", ErrBadVersion, e.V, Version)
	}
	if e.Type == "" {
		return e, errors.New("api: frame has no type")
	}
	return e, nil
}

// Bind unmarshals a frame's payload.
func Bind[T any](e Envelope) (T, error) {
	var v T
	if len(e.Payload) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		return v, fmt.Errorf("api: %s payload: %w", e.Type, err)
	}
	return v, nil
}

// Frame builds an outbound envelope.
func Frame(id, typ string, at time.Time, payload any) (Envelope, error) {
	e := Envelope{V: Version, ID: id, Type: typ, At: at.UnixMilli()}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return e, fmt.Errorf("api: encode %s: %w", typ, err)
		}
		e.Payload = b
	}
	return e, nil
}
