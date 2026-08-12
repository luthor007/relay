package apps

import (
	"sort"
	"strings"
)

// The capability table — APP-PLATFORM.md §5's second bullet, as data.
//
// "The SDK is the only interface. No filesystem, no `child_process`, no raw
// sockets. Capability objects on `ctx` are the entire API, and each is minted
// against the app's granted scopes."
//
// This table is the entire API. It is a closed list in one place because that is
// what makes the claim checkable: to add something an app can do, you add a row
// here, and the row has to name the scope that pays for it. A method with no
// scope is one an app can do to *itself* — write to its own private storage,
// write to its own log — and there are exactly four of those.
//
// Two properties follow, and both are tested rather than asserted:
//
//   - **Absent, not refusing.** [Capabilities] is what the in-sandbox runner
//     builds `ctx` from, and a capability whose scope was not granted is not in
//     it, so the property does not exist on the object. An app cannot
//     feature-detect its way to a capability it was not given, and it cannot
//     catch a refusal and retry.
//   - **No path to the indicators.** There is no method here that addresses the
//     capture LEDs, and there is no scope that could pay for one.
//     TestNoCapabilityCanAddressTheIndicators walks this table on every run.

// Method is the wire name of one host call.
type Method string

const (
	MethodMemorySearch        Method = "memory.search"
	MethodMemoryRecentEpisode Method = "memory.recentEpisode"
	MethodMemoryGet           Method = "memory.get"
	MethodMemoryExtract       Method = "memory.extractCommitments"
	MethodMemoryWrite         Method = "memory.write"

	MethodGlassesSay     Method = "glasses.say"
	MethodGlassesCapture Method = "glasses.capture"
	MethodGlassesListen  Method = "glasses.listen"

	MethodAgentAsk    Method = "agent.ask"
	MethodAgentStream Method = "agent.stream"

	MethodUIRender Method = "ui.render"
	MethodUIAsk    Method = "ui.ask"

	MethodStorageGet    Method = "storage.get"
	MethodStorageSet    Method = "storage.set"
	MethodStorageDelete Method = "storage.delete"

	MethodFetch Method = "fetch"
	MethodLog   Method = "log"
)

// capability is one row of the table.
type capability struct {
	Method Method
	// Requires is the scope that pays for it. Empty means the app is acting on
	// itself and nothing of the user's is reachable.
	Requires Scope
	// Object and Member are where it lands on `ctx`. An object whose members are
	// all ungranted is never created.
	Object string
	Member string
	// Streams marks a method whose result arrives as a sequence of frames rather
	// than one value.
	Streams bool
}

// capabilities is the whole API. Order is the order `ctx` is built in.
var capabilities = []capability{
	{Method: MethodMemorySearch, Requires: ScopeMemoryRead, Object: "memory", Member: "search"},
	{Method: MethodMemoryRecentEpisode, Requires: ScopeMemoryRead, Object: "memory", Member: "recentEpisode"},
	{Method: MethodMemoryGet, Requires: ScopeMemoryRead, Object: "memory", Member: "get"},
	{Method: MethodMemoryExtract, Requires: ScopeMemoryRead, Object: "memory", Member: "extractCommitments"},
	{Method: MethodMemoryWrite, Requires: ScopeMemoryWrite, Object: "memory", Member: "write"},

	{Method: MethodGlassesSay, Requires: ScopeGlassesSpeaker, Object: "glasses", Member: "say"},
	{Method: MethodGlassesCapture, Requires: ScopeGlassesCamera, Object: "glasses", Member: "capture"},
	{Method: MethodGlassesListen, Requires: ScopeGlassesAudio, Object: "glasses", Member: "listen"},

	{Method: MethodAgentAsk, Requires: ScopeAgentSession, Object: "agent", Member: "ask"},
	{Method: MethodAgentStream, Requires: ScopeAgentSession, Object: "agent", Member: "stream", Streams: true},

	// Drawing on the phone of the person who installed the app. No scope, and
	// this is the one row where that deserves an argument rather than an
	// assertion: it is the only scope-free capability that reaches *outside*
	// relayd, so it does not fit the "an app can do this to itself" shape the
	// other four have.
	//
	// It is still scope-free, because a scope pays for access to something of
	// the user's and this reaches nothing: a view cannot read, cannot fetch,
	// cannot capture, carries no URL, and returns the app nothing about the
	// phone — not whether one is connected, not whether anybody looked. The
	// user already consented to the app existing, and an app that cannot say
	// anything is an app that can only speak, which is the more invasive of the
	// two. Speech is the part that costs: a `speak` block inside a view needs
	// `glasses.speaker`, enforced per block in [CheckScopes], so `ui` being
	// mintable does not mint the speaker with it.
	{Method: MethodUIRender, Object: "ui", Member: "render"},
	{Method: MethodUIAsk, Object: "ui", Member: "ask"},

	// Private to this app, on this user's box. No scope: the app is the only
	// thing that can see it, and the bytes never leave relayd's side of the
	// boundary — `ctx.storage` is served by the host, not by a directory the
	// sandbox mounted.
	{Method: MethodStorageGet, Object: "storage", Member: "get"},
	{Method: MethodStorageSet, Object: "storage", Member: "set"},
	{Method: MethodStorageDelete, Object: "storage", Member: "delete"},

	// `net.fetch` is a bare function on ctx, matching the SDK's `ctx.fetch?`.
	{Method: MethodFetch, Requires: ScopeNetFetch, Object: "", Member: "fetch"},

	// The app's own log. No scope, and redacted through the same detector
	// everything else is: an app that reads a credential out of a transcript and
	// prints it must not turn relayd's log into where that credential ends up.
	{Method: MethodLog, Object: "", Member: "log"},
}

// Descriptor is one capability as the runner receives it.
type Descriptor struct {
	Method  Method `json:"method"`
	Object  string `json:"object,omitempty"`
	Member  string `json:"member"`
	Streams bool   `json:"streams,omitempty"`
}

// Capabilities is what the runner builds `ctx` from, for one set of granted
// scopes.
//
// This is the whole of the minting rule. A capability whose scope is not in
// granted is not returned, so the runner never defines the property, so the app
// sees an object that does not have it — which is the difference between "the
// camera refused" and "this app has no camera".
func Capabilities(granted []Scope) []Descriptor {
	has := map[Scope]bool{}
	for _, s := range granted {
		has[s] = true
	}
	out := make([]Descriptor, 0, len(capabilities))
	for _, c := range capabilities {
		if c.Requires != "" && !has[c.Requires] {
			continue
		}
		out = append(out, Descriptor{Method: c.Method, Object: c.Object, Member: c.Member, Streams: c.Streams})
	}
	return out
}

// Methods is the set of host methods a set of scopes may call, sorted.
//
// [Host] builds its dispatch table from exactly this, so a runner that was
// tampered with — the app is untrusted code and the runner runs next to it —
// still cannot reach a method the scopes did not pay for. The check exists twice
// on purpose: once so the property is absent, once so calling it anyway fails.
func Methods(granted []Scope) []Method {
	out := make([]Method, 0, len(capabilities))
	for _, d := range Capabilities(granted) {
		out = append(out, d.Method)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RequiredScope returns the scope a method costs, and whether the method exists
// at all.
func RequiredScope(m Method) (Scope, bool) {
	for _, c := range capabilities {
		if c.Method == m {
			return c.Requires, true
		}
	}
	return "", false
}

// Objects lists the `ctx` sub-objects a grant produces, for the console's "what
// this app can do" view.
func Objects(granted []Scope) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range Capabilities(granted) {
		if d.Object == "" || seen[d.Object] {
			continue
		}
		seen[d.Object] = true
		out = append(out, d.Object)
	}
	sort.Strings(out)
	return out
}

// Describe renders a grant as the sentence the console shows. It never mentions
// a capability the app does not have, because "cannot use the camera" invites
// the reader to wonder whether the app asked.
func Describe(granted []Scope) string {
	if len(granted) == 0 {
		return "nothing but its own storage and its own log"
	}
	parts := make([]string, 0, len(granted))
	for _, s := range granted {
		parts = append(parts, s.Grants())
	}
	return strings.Join(parts, "; ")
}
