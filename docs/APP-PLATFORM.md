# EngramOS — the app platform

*How third parties build for Engram One.*

---

## 1. The decision that shapes everything

MentraOS is the closest prior art and worth studying: apps register in a console,
the cloud POSTs a webhook when a session starts, and the app runs **on the
developer's server** with a WebSocket back to the cloud. It works, and their SDK
is MIT.

EngramOS makes the opposite call, because our topology is different: **every
Engram user already has a box.** The Cloud tier provisions a Linux machine to run
their agents, and self-hosters run the same thing on hardware they own.

So Engram apps run **on the user's own box**, not on the developer's server.

| | MentraOS | EngramOS |
|---|---|---|
| App code runs on | developer's server | **the user's box** |
| Developer sees user data | yes, it flows through them | **no** |
| App is distributed as | a registered URL | an installable package |
| Works offline / self-hosted | no | yes |
| Developer pays for hosting | yes | no |

For a device that records someone's entire working day, "the app author never
receives your transcript" is not a feature note — it is the difference between a
product people will wear and one they won't. It also removes the developer's
hosting bill, which for a hobbyist app is the difference between shipping and not.

The cost is real and worth stating: we cannot silently update a broken app, we
cannot see aggregate usage, and a malicious package runs on the user's machine.
§5 is about containing that.

---

## 2. Shape of an app

An app is an npm package with a manifest.

```
standup-notes/
  engram.json          manifest — identity, permissions, triggers
  src/index.ts         the app itself
  package.json
```

```json
{
  "id": "dev.alexis.standup-notes",
  "name": "Standup Notes",
  "version": "1.0.0",
  "description": "Turns the standup you just had into notes and commitments.",
  "author": { "name": "Alexis Massicotte", "url": "https://github.com/luthor007" },
  "permissions": [
    { "scope": "memory.read", "reason": "To find the meeting you just left." },
    { "scope": "memory.write", "reason": "To save the notes it extracts." },
    { "scope": "glasses.speaker", "reason": "To read the summary back to you." }
  ],
  "triggers": [
    { "type": "phrase", "match": "wrap up the standup" },
    { "type": "memory", "event": "meeting.ended" }
  ]
}
```

Permission `reason` strings are mandatory and are shown verbatim at install. A
scope with a vague reason is a review rejection — the same rule MentraOS applies,
and for the same reason: "Microphone access" tells a user nothing.

```ts
import { defineApp } from "@engram/sdk";

export default defineApp({
  async onTrigger(ctx) {
    const meeting = await ctx.memory.recentEpisode({ kind: "meeting" });
    if (!meeting) return ctx.say("I can't find a meeting in the last hour.");

    // Runs on the user's own box, against whatever model they configured.
    const summary = await ctx.agent.ask(
      `Summarise this standup into decisions and commitments:\n\n${meeting.transcript}`,
    );

    await ctx.memory.write({
      kind: "note",
      title: `Standup — ${meeting.startedAt.toDateString()}`,
      body: summary,
      commitments: await ctx.memory.extractCommitments(meeting),
    });

    await ctx.say("Saved. Three commitments — want them read back?");
  },
});
```

---

## 3. Permission scopes

Narrow on purpose. Every scope is surface that has to keep working and a
sentence the user has to accept.

| Scope | Grants |
|---|---|
| `glasses.audio` | live microphone during an open voice session |
| `glasses.camera` | capture a still; **never** silent capture — the LEDs light |
| `glasses.speaker` | speak through the glasses |
| `glasses.touch` | tap and gesture events |
| `memory.read` | search and read the user's episodes and transcripts |
| `memory.write` | add notes, commitments and tags |
| `agent.session` | send prompts to the user's agent and read replies |
| `net.fetch` | outbound HTTP, to a host allowlist declared in the manifest |
| `schedule` | wake on a cron-like schedule |

Two rules the runtime enforces rather than trusting apps to honour:

- **`net.fetch` is allowlisted per host in the manifest.** An app with
  `memory.read` and unrestricted network access is an exfiltration tool. If it
  wants to talk to `api.example.com`, it says so at install time.
- **There is no "record without indication" scope, and there never will be.**
  The LEDs are wired to capture and apps cannot address them.

---

## 4. Triggers

Apps do not poll. They are woken by something that happened.

| Trigger | Fires on |
|---|---|
| `phrase` | a wake phrase in the live transcript |
| `touch` | a gesture — `doubleTap`, `tripleTap`, `longPress` |
| `memory` | a pipeline event: `meeting.ended`, `commitment.detected`, `day.synced` |
| `schedule` | cron expression, in the user's timezone |
| `tool` | the agent decides to call the app as a tool |

`tool` is the interesting one. An installed app is automatically exposed to the
user's agent as an MCP tool, so "wrap up the standup" works without a wake
phrase — the agent just calls it. Apps get to be both a thing you invoke and a
capability your agent has.

---

## 5. Containing untrusted code

Apps run on the user's machine, so the sandbox is the whole safety story.

- **Process isolation per app.** Each app runs in its own container with a
  read-only root, its own writable scratch, and no access to the agent's
  workspace or other apps' data.
- **The SDK is the only interface.** No filesystem, no `child_process`, no raw
  sockets. Capability objects on `ctx` are the entire API, and each is minted
  against the app's granted scopes.
- **Egress is default-deny.** Outbound traffic goes through a proxy that enforces
  the manifest's host allowlist.
- **Memory access is scoped and logged.** Every read is recorded, and the user can
  see exactly which app touched which episode. An app that reads the whole
  archive on install is visible.
- **Resource caps.** CPU, memory and wall-clock per invocation; an app that hangs
  is killed, not left holding the box.

Review posture at launch: the registry is a git repository and every listed app
is a reviewed pull request. That does not scale, and it does not have to yet —
it scales far enough to learn what the sandbox actually needs.

---

## 6. Installation and distribution

```
engram install dev.alexis.standup-notes
engram list
engram logs standup-notes
engram remove standup-notes
```

Install resolves the package, shows the permission sheet with each `reason`,
waits for consent, then provisions the container. The phone app does the same
through UI, against the same API on the box.

The registry is `github.com/uulab/engram-apps` — a directory of manifests
pointing at source repositories. No central build service, no proprietary
publishing pipeline, and forking the registry is a supported thing to do.

---

## 7. What the phone app is

Worth stating plainly, because "one app that contains apps" invites the wrong
mental model: **third-party code never runs on the phone.**

The host app is three things:
1. the BLE bridge to the glasses (the spine — see `ARCHITECTURE.md` §2)
2. a viewer onto apps running on the box
3. the consent surface — pairing, permissions, capture control

Apps render through a small declarative vocabulary — a card, a list, a
confirmation, a spoken response — which the host renders natively on both
platforms. That is what makes one iOS app and one Android app enough, and it is
why an app author does not need to know Swift or Kotlin.

The tradeoff is honest: an app cannot draw arbitrary pixels on your phone. In
exchange, it works identically on both platforms, cannot phone home with your
data, and gets reviewed as a manifest instead of a binary.

---

## 8. Build order

1. `@engram/sdk` — types, `defineApp`, the capability interfaces *(started)*
2. App runtime on the box — container-per-app, capability minting, egress proxy
3. `engram` CLI — install, list, logs, remove
4. MCP bridge so installed apps become agent tools
5. Host-app rendering for the declarative vocabulary
6. Registry repo and review process

1 and 4 are the ones that make the platform feel different. An app that is
automatically a tool your agent can call is a better primitive than an app you
have to remember to open.
