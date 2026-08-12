// Package mcpbridge is APP-PLATFORM.md §8 steps 4 and 5 — the two the same
// section says are what "make the platform feel different".
//
// # 1. Every installed app is automatically a tool
//
// §8 states the primitive in one sentence: *an app that is automatically a tool
// your agent can call is a better primitive than an app you have to remember to
// open.* §4 says what that buys — "wrap up the standup" works with no wake
// phrase, because the agent just calls it. Apps get to be both a thing you
// invoke and a capability your agent has.
//
// There is no second mechanism for it. [Provider] is an [mcp.Provider] on the
// same gateway every connector is on, which is SYSTEM.md §6.3 taken literally:
// "memory and installed apps are tools on the same bus, not special cases".
// One grant table, one confirmation path, one audit trail, one tools/list. An
// app tool that had its own would have to re-earn all four and would get one of
// them wrong.
//
// Three rules follow from being on that bus rather than beside it:
//
//   - **An app that cannot be run is not offered.** [Provider.Tools] returns
//     nothing at all when there is no [Invoker] wired, and drops any app whose
//     manifest has no `tool` trigger. A tool an agent can see is a tool it will
//     call; offering one that cannot run teaches the user that Relay is broken.
//   - **The manifest is the grant.** [Grants] answers for app connectors out of
//     the install consent — the human decision APP-PLATFORM.md §6 already
//     records, taken against a sheet that showed the app's own reasons. It
//     never invents one, and it delegates every connector that is not an app to
//     internal/connector.
//   - **An app that can leave the machine confirms out loud.** An app granted
//     `net.fetch` gets a non-empty [mcp.Tool.Consequence], so ORCHESTRATOR.md
//     §4b rule 3 applies and the gateway asks at the glasses before it runs —
//     every time, and it refuses outright when no confirmation path is wired.
//     §3's sentence is the reason: "an app with memory.read and unrestricted
//     network access is an exfiltration tool", and here it is the *agent*
//     deciding to run it.
//
// # 2. The declarative vocabulary
//
// ORCHESTRATOR.md §5 is the half of APP-PLATFORM.md that explains where UI
// renders, and it looks like a contradiction until both halves are stated:
//
//   - **App code runs on the server**, sandboxed, never on the phone. That is
//     what keeps the author from ever seeing your transcript.
//   - **App UI renders in the phone app**, through a small declarative
//     vocabulary the host draws natively — a card, a list, a confirmation, a
//     spoken reply.
//
// [View] is that vocabulary in Go; `apps/sdk/src/ui.ts` is the same format in
// TypeScript, and TestVocabularyDoesNotDriftFromSDK reads that file rather than
// trusting this sentence. Two implementations of one wire format is how a phone
// ends up drawing something the SDK said was invalid, so the block kinds, the
// caps and the version are pinned across the two.
//
// The format is deliberately small, and [APP-PLATFORM.md §7] says what the
// smallness buys: *an app cannot draw arbitrary pixels on your phone. In
// exchange, it works identically on both platforms, cannot phone home with your
// data, and gets reviewed as a manifest instead of a binary.* All three are
// properties of the size — identical rendering needs one obvious native drawing
// per block, "cannot phone home" needs there to be nowhere to put a URL, and
// "reviewed as a manifest" needs a reviewer to be able to hold the whole
// vocabulary in their head. A vocabulary that grows until it is a rendering
// engine loses all three at once.
//
// [Renderer] is the server side of it: validate, refuse a block the app was not
// granted, stamp the app id the app cannot forge, and hand SYSTEM.md §6.1's
// `ui.render` frame to a [ViewSink]. A [Renderer] with no sink refuses rather
// than resolving — a box with no phone paired has nowhere to draw, and
// pretending otherwise is emitting an event nobody can observe.
//
// # What the agent gets back
//
// The two halves meet in [Provider]'s handler. An app woken by a `tool` trigger
// renders to the phone and speaks through the glasses as usual; the agent that
// called it has neither, so it is handed [ViewText] — the same view, projected
// to plain text. One vocabulary, two renderings, and the app wrote neither.
package mcpbridge
