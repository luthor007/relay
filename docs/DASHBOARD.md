# The console — one UI, two deployments

*Referenced by `MEMORY.md` §5, §6 and §10 as "the app or the cloud dashboard",
and never specified until now. This closes that gap.*

---

## 1. The problem it solves

A cloud-tier customer's Mac mini has no screen. They cannot open a terminal on
it, cannot read a `relayd` log, and cannot type an API key into a machine sitting
in our rack. Everything the product knows about them — sessions, facts,
credentials, connectors — is reachable only through a surface we have not built.

The phone app covers the common path (`ORCHESTRATOR.md` §5). It does not cover
pasting a service account JSON, reading a fact list of two hundred entries, or
debugging why a runtime will not start.

## 2. One codebase, two hosts

The temptation is to build a cloud dashboard and tell self-hosters to use the
CLI. Don't — it splits the UI in two and gives the free tier the worse half of
the product, which contradicts the whole positioning in `CLOUD.md` §1.

Instead, **the same web app, served from two places**:

| | Self-hosted | Relay Cloud |
|---|---|---|
| Served by | `relayd`, on `127.0.0.1` | us |
| Auth | a token `relayd` prints, like the pairing code | account + session |
| Reaches | that machine only | that customer's box, via the relay |
| Vault writes | land in the OS keychain, locally | land in our KMS |

This preserves the claim that matters commercially: **on the free tier, nothing
about you reaches our infrastructure, including through the UI.** The self-hoster
gets the identical screens; we are simply not in the path.

`relayd` embeds the built assets in the binary (Go's `embed`), so there is no
second thing to install and no static host to run. That is also why the console
is plain TypeScript and not a framework that needs a Node server at runtime.

---

## 3. Screens

Six, in build order. Each maps onto something already specified.

### 3.1 Sessions — *the default view*

Every session across all five runtimes, live and historical, from the registry
and index (`MEMORY.md` §2). Title, runtime, repo, last activity, status, cost.

Two things it must do that a list normally does not: show **which sessions are
blocked on input** at the top, unmissable, because a blocked session is the one
failure mode that silently stops all work; and let you **open the raw transcript**,
since the index holds a pointer into the original file rather than a copy.

### 3.2 Credentials

The vault (`MEMORY.md` §6). Add, validate, rotate, revoke. Per entry: service,
when it was added, where it came from, when it was last used, and by which
runtime.

**Never displays a secret after it is stored.** Last four characters, and a
re-validate button. A UI that shows the key back to you is a UI that gets
screenshotted into a support ticket.

Pending proposals live here too — the "I found what looks like a Twilio token in
a session from March" flow from §6 needs somewhere to be accepted or dismissed
that is not a voice prompt at 2 a.m.

### 3.3 Facts

`MEMORY.md` §5 requires this screen and calls it "what makes the whole tier
defensible". Every inferred fact, its evidence with dates, confidence, and a
delete button. Superseded facts visible under a toggle, because "you used to use
Firebase" is a real thing to be able to answer.

Editable, not just deletable. A wrong fact that the user can correct in one field
is better than one they can only remove.

### 3.4 Connectors and MCP

The union registry from `MEMORY.md` §7 and the revocation screen already required
by `ORCHESTRATOR.md` §4b: every connector, its scope in plain words, when it was
last used and for what, and one place to revoke it across all five runtimes.

Unused connectors say so. Access nobody has touched in a month is the kind that
gets forgotten and then exploited.

### 3.5 Runtimes and health

Which runtimes are installed, authenticated, and running. Which model each is
configured for. The voice provider. The two orchestrator models from
`ORCHESTRATOR.md` §2b, with a re-probe button — because "my glasses stopped
talking" is almost always an expired credential, and the fastest support answer
is a page that already says which one.

Cloud tier adds machine health: uptime, disk, last backup.

### 3.6 Billing — cloud only

Stripe's customer portal, linked rather than rebuilt. Plan, next charge, invoices,
cancel. Do not reimplement any of it.

---

## 4. Auth, because this is the sensitive surface

The console can write to the vault. That makes it the highest-value target in the
system, above the glasses and above `relayd`'s own API.

**Self-hosted.** Bound to `127.0.0.1` by default — not `0.0.0.0`, and the
difference is not a preference. A token is printed on start, same pattern as the
pairing code. Exposing it on a LAN is a deliberate flag with a warning, not a
config default someone flips without reading.

**Cloud.** Real accounts, TOTP available, and every vault write re-authenticated
regardless of session age. Sessions expire.

**Both.** Every credential and connector mutation is written to an audit log the
console itself displays. If something reads keys it should not, the evidence
exists in a place the user can see without our help.

---

## 5. Stack

Plain TypeScript, Vite, no framework runtime — the assets are embedded in a Go
binary, so anything needing a Node process at serve time is disqualified by
construction. Server-rendered would be simpler still, but the sessions view wants
live updates and the credential flows want optimistic UI.

Same `relayd` HTTP API in both deployments, so the cloud host is a proxy plus an
auth layer rather than a second backend. One API means one place for the
authorization checks, which is the only way they stay consistent.

---

## 6. Build order

Slots after the orchestrator milestone, before the app platform.

| | Ships | Proves |
|---|---|---|
| 1 | Sessions view, read-only, self-hosted | the registry and index are real and correct |
| 2 | Runtimes and health + re-probe | the top support question answers itself |
| 3 | Credentials + audit log | agents can act without the user fetching keys |
| 4 | Facts, editable | the inference layer is defensible |
| 5 | Connectors and MCP | grant once, revoke once |
| 6 | Cloud host: auth, proxy, billing | the $39 tier has a front door |

**Self-hosted first, cloud second.** The cloud deployment is the same app behind
auth, so building it first would mean building the auth layer before there is
anything to protect.

---

## 7. Open

1. ~~**Live updates.**~~ **Settled — SSE, and it is built.** `GET /v1/events` on
   the `relayd` API streams three named events: `session` when a row moves,
   `incident` when something goes wrong, `ping` when the user is about to hear
   from us, plus an opening `sessions` frame so a console that connects
   mid-flight renders immediately instead of waiting for the next change. The
   sessions view never needed bidirectional traffic, as suspected. The phone's
   socket stays a WebSocket for the reason `SYSTEM.md` §6.1 gives — that one is
   genuinely two-way — so the two live channels are different on purpose rather
   than by accident.
2. **Whether the phone app should embed this** as a webview rather than
   reimplementing the same screens natively. It would halve the work and cost
   some polish; the native-versus-webview call in `APPS-SCOPE.md` §6 should be
   revisited with this in scope.
3. **Multi-machine.** One console, several `relayd` instances, for someone with a
   laptop and a server. Not needed at launch, and the auth model changes if it
   ever is.
