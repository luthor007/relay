/**
 * Standup Notes — a complete Relay app in 80 lines.
 *
 * Shows the four things that make the platform different:
 *
 *   1. It reads the user's memory directly. No integration, no OAuth, no
 *      "connect your calendar" — the transcript is already there because the
 *      glasses were on.
 *   2. It asks *the user's* agent, on *their* box, with *their* model. The app
 *      author ships no API key and pays for no inference.
 *   3. Declaring the `tool` trigger makes it callable by the agent, so "wrap up
 *      the standup" works without the app owning a wake phrase.
 *   4. It draws a card on the phone without a line of Swift or Kotlin. This
 *      file runs on the user's box; `ctx.ui` yields a view the host app draws
 *      natively. Nothing third-party executes on the handset.
 */

import { card, defineApp, list } from "@relay/sdk";

export default defineApp({
  // The same four scopes relay.json asks for. Declaring them here narrows
  // `ctx` to exactly these capabilities, so reaching for a fifth does not
  // compile — and a manifest that loses one fails on the first invocation
  // saying so, instead of throwing on an undefined property.
  scopes: ["memory.read", "memory.write", "agent.session", "glasses.speaker"],

  async onTrigger(ctx) {
    const meeting = await ctx.memory.recentEpisode({ kind: "meeting", within: 60 * 60 * 1000 });

    if (!meeting) {
      // Say what is wrong and what would fix it, rather than apologising.
      await ctx.say("I can't find a meeting in the last hour. Ask me again right after one.");
      return;
    }

    ctx.log("summarising meeting", { id: meeting.id, minutes: durationMinutes(meeting) });

    const summary = await ctx.agent.ask(
      [
        "Summarise this meeting for the person who was wearing the glasses.",
        "Lead with decisions. Keep it under 120 words. No preamble.",
        "",
        meeting.transcript,
      ].join("\n"),
    );

    const commitments = await ctx.memory.extractCommitments(meeting);

    const { id } = await ctx.memory.write({
      kind: "note",
      title: `Standup — ${meeting.startedAt.toLocaleDateString()}`,
      body: summary,
      commitments,
      tags: ["standup", "meeting"],
    });

    ctx.log("saved note", { id, commitments: commitments.length });

    // The card and the list are data, not pixels: the phone draws them with its
    // own native controls, identically on iOS and Android.
    //
    // `ctx.ui` is optional for the same reason every capability is optional —
    // a box with no phone paired has nowhere to draw, and an absent capability
    // is honest where a silently-dropped frame is not.
    await ctx.ui?.render({
      vocabulary: 1,
      blocks: [
        card("Standup", {
          body: summary,
          fields: [{ label: "Length", value: `${durationMinutes(meeting)} min` }],
        }),
        ...(commitments.length > 0
          ? [
              list(
                commitments.slice(0, 20).map((c) => ({
                  title: c.text,
                  subtitle: c.to,
                  detail: c.dueAt?.toLocaleDateString(),
                })),
                { title: plural(commitments.length, "commitment") },
              ),
            ]
          : []),
      ],
    });

    // Only offer to read them back if there is something worth hearing. A
    // spoken "zero commitments" is noise in someone's ear.
    if (commitments.length === 0) {
      await ctx.say("Saved the summary. Nothing you committed to.");
      return;
    }

    const mine = commitments.filter((c) => c.to);
    await ctx.say(
      `Saved. ${plural(commitments.length, "commitment")}` +
        (mine.length > 0 ? `, ${mine.length} to other people. Want them read back?` : "."),
    );
  },
});

function durationMinutes(episode: { startedAt: Date; endedAt: Date }): number {
  return Math.round((episode.endedAt.getTime() - episode.startedAt.getTime()) / 60_000);
}

function plural(count: number, word: string): string {
  return `${count} ${word}${count === 1 ? "" : "s"}`;
}
