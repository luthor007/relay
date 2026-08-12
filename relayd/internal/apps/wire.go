package apps

import "encoding/json"

// The capability channel — the only thing that crosses the sandbox boundary.
//
// Newline-delimited JSON on two pipes: fd 3 carries host→app, fd 4 carries
// app→host. Pipes and not a socket, deliberately: the strongest sandbox this
// package builds puts the app in an empty network namespace, where a socket to
// relayd would be a socket the app does not have. A capability channel that only
// works when the app has a network is a capability channel that argues for
// giving it one.
//
// The vocabulary is small enough to read in one screen, which is the point: this
// is the entire attack surface between untrusted code and the box.

// Frame types, host→app.
const (
	// frameStart carries the trigger and the minted capability list. It is the
	// first thing the app process receives and it is what unblocks the runner —
	// which is how the resource limits come to be applied before any app code
	// has loaded.
	frameStart = "start"
	// frameOK is one call's result.
	frameOK = "ok"
	// frameErr is one call's failure.
	frameErr = "err"
	// frameChunk is one piece of a streaming result. A stream is closed by the
	// frameOK that answers the same call id — there is no separate end frame,
	// because a second way to say "that is all" is a second thing a client can
	// get wrong.
	frameChunk = "chunk"
)

// Frame types, app→host.
const (
	// frameCall is a capability invocation.
	frameCall = "call"
	// frameDone says onTrigger resolved.
	frameDone = "done"
	// frameFailed says onTrigger threw, with the app's own error attached.
	frameFailed = "failed"
)

// startFrame is what the runner needs to build `ctx` and run.
type startFrame struct {
	T       string       `json:"t"`
	App     appIdentity  `json:"app"`
	Trigger TriggerFrame `json:"trigger"`
	// Capabilities is the minted set. The runner builds `ctx` from exactly this
	// and nothing else, so a capability whose scope was not granted is not a
	// property on the object the app receives.
	Capabilities []Descriptor `json:"capabilities"`
	// Granted is `ctx.granted` in the SDK: the scopes actually in force, which
	// may be narrower than the manifest requested.
	Granted []Scope `json:"granted"`
	// Declined is what was asked for and refused, so an app can say "you have
	// not given me the camera" rather than guessing why a property is missing.
	Declined []Scope `json:"declined"`
	// DeadlineMs is the wall-clock budget, so a well-behaved app can finish
	// early rather than be killed. It is information, not enforcement: the
	// supervisor kills regardless.
	DeadlineMs int64  `json:"deadlineMs"`
	Entry      string `json:"entry"`
}

type appIdentity struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// TriggerFrame is what woke the app — the SDK's `TriggerContext`.
type TriggerFrame struct {
	Type TriggerType `json:"type"`
	// Transcript is set for a phrase trigger.
	Transcript string `json:"transcript,omitempty"`
	// Gesture is set for a touch trigger.
	Gesture Gesture `json:"gesture,omitempty"`
	// Event and EpisodeID are set for a memory trigger.
	Event     MemoryEvent `json:"event,omitempty"`
	EpisodeID string      `json:"episodeId,omitempty"`
	// Arguments is set for a tool trigger: what the agent passed.
	Arguments map[string]any `json:"arguments,omitempty"`
}

// callFrame is one capability invocation from the app.
type callFrame struct {
	T      string          `json:"t"`
	ID     int64           `json:"id"`
	Method Method          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
	// Error carries the app's own failure on a frameFailed.
	Error *appError `json:"error,omitempty"`
}

type appError struct {
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
	Name    string `json:"name,omitempty"`
}

// resultFrame is one answer.
type resultFrame struct {
	T      string     `json:"t"`
	ID     int64      `json:"id"`
	Result any        `json:"result,omitempty"`
	Value  any        `json:"value,omitempty"`
	Error  *wireError `json:"error,omitempty"`
}

// wireError is a refusal the app can read. Code is stable and machine-readable;
// Message is the sentence a user would recognise, because an app that has to
// explain a refusal out loud should be able to quote it.
type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes on the capability channel.
const (
	// CodeNoCapability is a method the grant did not mint. It is deliberately
	// not "denied": from outside the grant there is nothing there to deny, and
	// an app that can tell "exists but refused" from "does not exist" has been
	// handed a probe of what the user declined.
	CodeNoCapability = "no_such_capability"
	// CodeBadArgs is a malformed call.
	CodeBadArgs = "bad_arguments"
	// CodeDenied is the egress allowlist refusing a host.
	CodeDenied = "denied"
	// CodeUnavailable is a capability that exists but has nothing behind it on
	// this box — no glasses paired, no note store wired. Distinct from
	// CodeNoCapability on purpose: this one is about the box, not the grant.
	CodeUnavailable = "unavailable"
	// CodeFailed is the call running and failing.
	CodeFailed = "failed"
)
