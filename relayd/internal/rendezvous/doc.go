// Package rendezvous is the relay that solves SYSTEM.md §7's NAT problem.
//
// A Mac mini in someone's house is not reachable from a phone on cellular. That
// decides whether self-hosting works at all, and §7 already settled the answer:
// **we run a relay, and it is free even for self-hosters.** Both sides dial out;
// the relay pipes bytes it cannot read.
//
// This package is the server half. The client half of *pairing* already exists
// and is tested — `glasses/bridge/src/pairing.ts` — but nothing in the tree ever
// dialled a relay, because there was none to dial.
//
// # What it is, stated as narrowly as possible
//
// It is a pipe. It joins two dial-outs and copies bytes between them. It is not
// an authenticator, not a session store, not a proxy that understands relayd's
// API, and it holds no user data at rest. Everything it refuses to do is a thing
// that would put it in a position to read or forge traffic.
//
// The security argument has three legs and the relay is only responsible for the
// third:
//
//  1. **Pairing is safe against the relay** because the code authenticates a PAKE
//     rather than travelling over one. `pairing.ts` says it outright: "'cannot
//     read' has to be true against us, not merely intended by us." The relay sees
//     the PAKE messages and learns nothing it could offline-attack.
//  2. **The link is safe against the relay** because `relayd.ts` seals every
//     envelope with `SealedChannel` when the route is the relay, under a key
//     derived at pairing that never crossed the wire.
//  3. **The relay is responsible for availability and for isolation**: that one
//     box's traffic cannot reach another's, that a stranger cannot occupy a slot
//     somebody is mid-pairing on, and that a hostile client cannot exhaust it.
//
// Leg 3 is what this package implements. Legs 1 and 2 are why it is allowed to be
// this simple.
//
// # Two modes, because they address differently
//
// **Pairing** uses the ephemeral slot from the printed code — two Crockford
// base32 characters, so 1024 of them. That is a tiny space, and it is only safe
// because a slot lives for minutes and holds exactly one host: a second host on
// an occupied slot is refused rather than queued, so a collision is an honest
// "try again" instead of two boxes silently sharing a rendezvous. The secret half
// of the code never reaches the relay.
//
// **Linking** uses the durable box id, which is what a paired phone knows. A box
// registers once and holds the registration open; guests ask for it by id.
//
// # Why a guest gets its own socket instead of a multiplexed stream
//
// The obvious design is one box connection carrying many logical streams behind a
// small frame header. This does the opposite: the relay tells the box "somebody
// wants you, here is a stream id", and the box **dials out again** for that one
// stream. Two reasons, and the second is the one that decided it.
//
// A multiplexer is a framing layer, and a framing layer is a thing that can be
// wrong about boundaries. Every byte would pass through code that has an opinion
// about where a message ends, which is exactly the position this package exists
// to avoid being in.
//
// And a per-stream socket is an *ordinary* socket. relayd's existing WebSocket
// handler serves it unchanged, its own authentication happens end to end inside
// it, and the relay never has to be taught anything about the protocol it
// carries. The cost is one extra dial per connection, on a link that already
// tolerates reconnection by design.
//
// # What it deliberately does not do
//
//   - **No payload logging, at any level.** There is no debug flag that prints
//     bytes. A relay that can be switched into recording is a relay whose
//     operator can be compelled to switch it on.
//   - **No authentication of guests.** It cannot: the device token and signing
//     key are derived at pairing and never leave the two endpoints, which is what
//     makes leg 2 true. relayd authenticates, and a guest that reaches a box
//     without a credential gets exactly as far as an unauthenticated request
//     would have on the LAN.
//   - **No queueing.** A slot or a box that is busy answers busy. Holding a
//     second guest until the first leaves would make the failure mode a hang
//     rather than a message.
//   - **No persistence.** Restarting the relay drops every stream, and both sides
//     already reconnect. A relay with a database is a relay with a breach.
package rendezvous
