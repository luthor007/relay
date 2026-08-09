import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { ManifestError, parseManifest } from "../src/manifest.ts";
import { defineApp } from "../src/app.ts";

const VALID = {
  id: "dev.alexis.standup-notes",
  name: "Standup Notes",
  version: "1.0.0",
  description: "Turns the standup you just had into notes and commitments.",
  author: { name: "Alexis Massicotte" },
  permissions: [
    { scope: "memory.read", reason: "To find the meeting you just left." },
    { scope: "memory.write", reason: "To save the notes it extracts." },
  ],
  triggers: [{ type: "phrase", match: "wrap up the standup" }],
};

describe("manifest", () => {
  test("accepts a well-formed manifest", () => {
    const manifest = parseManifest(VALID);
    assert.equal(manifest.id, "dev.alexis.standup-notes");
    assert.equal(manifest.permissions.length, 2);
    assert.equal(manifest.timeoutMs, 30_000, "should default a timeout");
  });

  test("requires reverse-DNS ids", () => {
    for (const id of ["standup", "Dev.Alexis.App", "dev..app", ""]) {
      assert.throws(() => parseManifest({ ...VALID, id }), ManifestError, `accepted ${id}`);
    }
  });

  test("requires semver", () => {
    assert.throws(() => parseManifest({ ...VALID, version: "1.0" }), /semver/);
  });

  test("rejects unknown scopes", () => {
    const bad = { ...VALID, permissions: [{ scope: "glasses.gps", reason: "Because I want it." }] };
    assert.throws(() => parseManifest(bad), /not a known scope/);
  });

  test("rejects a permission reason that just restates the scope", () => {
    // The install sheet shows these verbatim; "Microphone" tells nobody anything.
    const bad = { ...VALID, permissions: [{ scope: "glasses.audio", reason: "Audio" }] };
    assert.throws(() => parseManifest(bad), /explain why/);
  });

  test("rejects duplicate scopes", () => {
    const bad = {
      ...VALID,
      permissions: [
        { scope: "memory.read", reason: "To read your meetings." },
        { scope: "memory.read", reason: "To read them again, apparently." },
      ],
    };
    assert.throws(() => parseManifest(bad), /duplicate/);
  });

  test("an app with no triggers cannot start, so it is rejected", () => {
    assert.throws(() => parseManifest({ ...VALID, triggers: [] }), /at least one trigger/);
  });

  test("net.fetch without an allowlist is refused", () => {
    // memory.read + unrestricted egress is an exfiltration tool.
    const bad = {
      ...VALID,
      permissions: [
        ...VALID.permissions,
        { scope: "net.fetch", reason: "To post your notes to our servers." },
      ],
    };
    assert.throws(() => parseManifest(bad), /allowedHosts/);
  });

  test("net.fetch with an allowlist is accepted", () => {
    const manifest = parseManifest({
      ...VALID,
      permissions: [
        ...VALID.permissions,
        { scope: "net.fetch", reason: "To file commitments as issues on your tracker." },
      ],
      allowedHosts: ["api.linear.app"],
    });
    assert.deepEqual(manifest.allowedHosts, ["api.linear.app"]);
  });

  test("allowedHosts without net.fetch is a mistake worth surfacing", () => {
    assert.throws(
      () => parseManifest({ ...VALID, allowedHosts: ["api.linear.app"] }),
      /without the net.fetch/,
    );
  });

  test("rejects non-objects", () => {
    for (const input of [null, "manifest", 42, []]) {
      assert.throws(() => parseManifest(input), ManifestError);
    }
  });
});

describe("defineApp", () => {
  test("returns the definition", () => {
    const app = defineApp({ onTrigger: () => {} });
    assert.equal(typeof app.onTrigger, "function");
  });

  test("an app without onTrigger is a programming error", () => {
    assert.throws(() => defineApp({} as never), TypeError);
  });
});
