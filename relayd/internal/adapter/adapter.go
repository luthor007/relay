// Package adapter is the one interface all three runtime adapters implement.
//
// Five runtimes, three protocols (ADAPTERS.md §1): Claude Code speaks
// stream-json over stdio, Codex speaks app-server JSON-RPC over NDJSON, and
// OpenClaw, Hermes and OpenCode all speak ACP — so one adapter covers three of
// the five. Nothing here parses terminal output; if an implementation finds
// itself reading prose it is on the wrong path.
//
// The interface has two levels because the runtimes do. An [Adapter] owns a
// runtime — it knows how to launch it, what protocol it speaks, and what it can
// and cannot do. A [Session] is one conversation: Codex's app-server hosts many
// threads on one connection, Claude Code runs one process per session, and both
// shapes fit.
//
// The third thing this interface expresses, and the reason it exists rather
// than being three separate packages, is [Capabilities]. ADAPTERS.md §5's
// coverage table is uneven — mid-turn steering is verified absent on ACP,
// PlanUpdated is native on Codex and ACP and absent on Claude Code, per-turn
// cost is USD on one runtime, tokens on another and structurally missing on the
// third. An adapter never emits an event it cannot observe, so the orchestrator
// has to be able to read what is missing before it asks. That is data, not a
// runtime panic.
package adapter

import (
	"context"

	"github.com/luthor007/relay/relayd/internal/event"
)

// Runtime names one of the five agent runtimes Relay drives.
type Runtime string

const (
	ClaudeCode Runtime = "claude-code"
	Codex      Runtime = "codex"
	OpenClaw   Runtime = "openclaw"
	Hermes     Runtime = "hermes"
	OpenCode   Runtime = "opencode"
)

// Runtimes lists all five.
func Runtimes() []Runtime { return []Runtime{ClaudeCode, Codex, OpenClaw, Hermes, OpenCode} }

// Protocol is the wire protocol a runtime is driven over.
type Protocol string

const (
	ProtocolStreamJSON Protocol = "stream-json" // Claude Code
	ProtocolAppServer  Protocol = "app-server"  // Codex, JSON-RPC over NDJSON
	ProtocolACP        Protocol = "acp"         // OpenClaw, Hermes, OpenCode
)

// Protocol returns the wire protocol for a runtime, or "" if unknown.
func (r Runtime) Protocol() Protocol {
	switch r {
	case ClaudeCode:
		return ProtocolStreamJSON
	case Codex:
		return ProtocolAppServer
	case OpenClaw, Hermes, OpenCode:
		return ProtocolACP
	}
	return ""
}

func (r Runtime) String() string { return string(r) }

// MCPServer is one entry of MEMORY.md §7's shared registry, injected into a
// session at creation. Grant once, works in all five, revoke once.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     []string
	// URL is set instead of Command for HTTP/SSE servers.
	URL string
}

// SessionOptions is what it takes to open a conversation on any runtime.
type SessionOptions struct {
	// ID is Relay's session id. Claude Code takes it directly as --session-id,
	// so the orchestrator names the session rather than discovering its name
	// afterwards. Adapters that cannot name a session record the runtime's own
	// id on the returned Session and map between them.
	ID string

	// Workspace must be absolute — ACP requires it and the others assume it.
	Workspace string

	Model string

	MCPServers []MCPServer

	// PermissionMode must not be an auto or bypass mode. Both Claude Code and
	// Codex silently stop asking for approval when it is: Claude Code's
	// permissions.defaultMode "auto" makes --permission-prompt-tool never fire,
	// and Codex's approvalPolicy "never" or approvalsReviewer "auto_review"
	// does the same to its five approval requests. In both cases the failure
	// presents as "the glasses never ask me anything", which reads as a feature
	// until something destructive runs unattended. An adapter that cannot
	// verify permission checks are on must report CapNeedsInput as SupportNo
	// for that session rather than a capability it cannot observe.
	PermissionMode string

	// Env is extra environment for the runtime process, in "K=V" form.
	Env []string

	// Extra carries runtime-specific settings that have no cross-runtime
	// meaning — OpenClaw's --require-existing, Codex's experimentalApi.
	Extra map[string]string
}

// SessionRef identifies an existing session well enough to reattach to it.
type SessionRef struct {
	Runtime Runtime
	// ID is Relay's id.
	ID string
	// Native is the runtime's own id, when it differs: OpenClaw's ACP session
	// keys look like "agent:main:main", Codex uses thread ids, Claude Code uses
	// the UUID we gave it.
	Native string
	// Workspace is the absolute cwd the session belongs to.
	Workspace string
}

// BlockKind is a piece of prompt content. Anything past BlockText is gated by
// the runtime's advertised prompt capabilities — a photo from the glasses
// cannot enter a prompt on a runtime that did not advertise image support.
type BlockKind string

const (
	BlockText            BlockKind = "text"
	BlockResourceLink    BlockKind = "resource_link"
	BlockImage           BlockKind = "image"
	BlockAudio           BlockKind = "audio"
	BlockEmbeddedContext BlockKind = "embedded_context"
)

// Block is one piece of a turn's content.
type Block struct {
	Kind     BlockKind
	Text     string
	URI      string
	MIMEType string
	Data     []byte
}

// Turn is one thing to say to a session.
type Turn struct {
	// ID is optional; the adapter assigns one when it is empty and returns it.
	ID     string
	Text   string
	Blocks []Block
}

// Requires reports the prompt capabilities this turn needs. Text and
// resource_link are the ACP baseline and need nothing.
func (t Turn) Requires() []Capability {
	var seen map[Capability]bool
	var out []Capability
	add := func(c Capability) {
		if seen == nil {
			seen = map[Capability]bool{}
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, b := range t.Blocks {
		switch b.Kind {
		case BlockImage:
			add(CapPromptImage)
		case BlockAudio:
			add(CapPromptAudio)
		case BlockEmbeddedContext:
			add(CapPromptEmbeddedContext)
		}
	}
	return out
}

// Adapter drives one runtime. One Adapter may own many Sessions.
type Adapter interface {
	// Runtime is which of the five this drives.
	Runtime() Runtime

	// Capabilities is what this runtime can be observed to do, before any
	// session-specific negotiation narrows it.
	Capabilities() Capabilities

	// Start opens a new session.
	Start(ctx context.Context, opts SessionOptions) (Session, error)

	// Resume reattaches to an existing one. It returns an *UnsupportedError
	// for CapResume where the runtime cannot do it — ACP's loadSession is
	// per-runtime and per-version, and the registry must fall back to starting
	// a new session and saying so rather than failing silently.
	Resume(ctx context.Context, ref SessionRef, opts SessionOptions) (Session, error)

	// Close shuts down the runtime and every session under it.
	Close(ctx context.Context) error
}

// Session is one conversation with one runtime.
//
// Events is the normalized stream from ADAPTERS.md §5. It is closed exactly
// once, when the session ends, and an implementation must not block on it
// forever — a consumer that stops reading is a bug in the consumer, but a
// deadlocked runtime is a bug in the adapter.
type Session interface {
	// ID is Relay's session id.
	ID() string

	// Native is the runtime's own id, for reattaching and for reconciling
	// against the runtime's own store.
	Native() string

	Runtime() Runtime

	// Capabilities is this session's capabilities, which may be narrower than
	// the Adapter's: an ACP handshake reports agentCapabilities per connection,
	// and a Claude Code session running under an auto permission mode has no
	// observable needs-input path at all.
	Capabilities() Capabilities

	Events() <-chan event.Event

	// Send starts a new turn and returns its id. It does not wait for the turn
	// to finish; TurnCompleted on the event stream is the boundary.
	Send(ctx context.Context, t Turn) (turnID string, err error)

	// Steer injects an utterance into a turn already running. Verified present
	// on Claude Code and Codex, verified absent on ACP — where it returns an
	// *UnsupportedError for CapSteer and the caller must Cancel and re-prompt
	// instead (ADAPTERS.md §4).
	//
	// turnID is a precondition, not a convenience: Codex's turn/steer requires
	// a matching expectedTurnId and fails if that turn is no longer the active
	// one. ErrTurnNotActive is that failure.
	Steer(ctx context.Context, turnID string, t Turn) error

	// Cancel stops a turn. On ACP the agent flushes pending session/update
	// notifications before resolving, so the implementation must keep reading
	// events after cancelling and must resolve every outstanding NeedsInput
	// with DecisionCancelled or the agent's turn cannot unwind.
	Cancel(ctx context.Context, turnID string) error

	// Close ends the session and closes Events.
	Close(ctx context.Context) error
}
