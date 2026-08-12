#!/usr/bin/env python3
"""Build docs/fixtures/adapters/codex.trace.json from the vendored schemas.

    python3 relayd/testdata/codex/gen_trace.py [--check]

THIS SCRIPT DOES NOT RECORD ANYTHING. No `codex` binary exists in the build
container, so the trace it writes is *derived from the contract* rather than
captured from a run: every message is constructed here and then validated
against the definition in `docs/fixtures/adapters/{ClientRequest,
ServerNotification,ServerRequest}.json` that it claims to conform to. That makes
it a good regression fixture and a bad substitute for the real thing — the two
differ wherever codex-cli 0.140.0 does something the schema does not say.

`ADAPTERS.md` §8 carries the corresponding entry: this file must be replaced
with a genuine recording on the author's Mac.

With --check it validates the file already on disk instead of rewriting it, so
a stale fixture fails rather than being silently regenerated.

The Go side validates the same file with its own validator
(`relayd/internal/adapter/codex/schema_test.go`), because CI has Go and may not
have Python's jsonschema.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve()
REPO = HERE.parents[3]
FIX = REPO / "docs" / "fixtures" / "adapters"
OUT = FIX / "codex.trace.json"

THREAD = "01998e4a-7a1f-7c3a-9c22-6d3b1f0e5a10"
SESSION_TREE = "01998e4a-7a1f-7c3a-9c22-6d3b1f0e5a00"
CWD = "/Users/USER/src/relay"
TURN1 = "turn_01HZX8QK4T7N9G"
TURN2 = "turn_01HZX8R2M6B4KD"

records: list[dict] = []


def rec(direction, kind, msg, schema=None, method=None, note=None):
    r = {"dir": direction, "seq": len(records), "kind": kind}
    if method:
        r["method"] = method
    r["schema"] = schema
    if note:
        r["note"] = note
    r["msg"] = msg
    records.append(r)
    return r


def send_request(rid, method, params, note=None):
    rec("send", "request", {"id": rid, "method": method, "params": params},
        schema=f"ClientRequest.json#/definitions/{schema_for('ClientRequest.json', method)}",
        method=method, note=note)


def send_response(rid, result, note):
    rec("send", "response", {"id": rid, "result": result}, schema=None, note=note)


def recv_response(rid, result, note):
    rec("recv", "response", {"id": rid, "result": result}, schema=None, note=note)


def recv_note(method, params, note=None):
    rec("recv", "notification", {"method": method, "params": params},
        schema=f"ServerNotification.json#/definitions/{schema_for('ServerNotification.json', method)}",
        method=method, note=note)


def recv_request(rid, method, params, note=None):
    rec("recv", "request", {"id": rid, "method": method, "params": params},
        schema=f"ServerRequest.json#/definitions/{schema_for('ServerRequest.json', method)}",
        method=method, note=note)


_schema_cache: dict[str, dict] = {}


def load(name: str) -> dict:
    if name not in _schema_cache:
        _schema_cache[name] = json.loads((FIX / name).read_text())
    return _schema_cache[name]


def schema_for(file: str, method: str) -> str:
    """Name the params definition the schema pins to this method."""
    doc = load(file)
    for variant in doc["oneOf"]:
        props = variant.get("properties", {})
        names = props.get("method", {}).get("enum") or []
        if method in names:
            ref = props.get("params", {}).get("$ref")
            if ref:
                return ref.rsplit("/", 1)[-1]
            return "null"
    raise SystemExit(f"{method} is not a variant of {file}")


# --------------------------------------------------------------------------
# The session
# --------------------------------------------------------------------------

rec("meta", "meta", {}, note="see the provenance block")
records[0] = {
    "dir": "meta",
    "seq": 0,
    "kind": "meta",
    "provenance": "SCHEMA-DERIVED, NOT RECORDED",
    "warning": (
        "This is NOT a recorded Codex session. No codex binary existed on the machine "
        "that produced it. Every message below was constructed from the vendored "
        "codex-cli 0.140.0 schemas and validated against the definition named in its "
        "`schema` field. It is a contract fixture, not evidence of runtime behaviour. "
        "Replace it with a real `codex app-server` recording on a machine that has "
        "Codex installed and authenticated — ADAPTERS.md §8 tracks that as an open item."
    ),
    "generator": "relayd/testdata/codex/gen_trace.py",
    "contract": {
        "runtime": "codex",
        "cli_version": "0.140.0",
        "schemas": ["ClientRequest.json", "ServerNotification.json", "ServerRequest.json"],
        "transport": "NDJSON over stdio, one JSON object per line, no Content-Length headers",
        "envelope": "no `jsonrpc` field: app-server neither sends nor expects one",
    },
    "unverified": [
        "every JSON-RPC *result* payload: generate-json-schema emits params only and "
        "there is no ServerResponse.json, so the `result` values below carry schema:null "
        "and are the adapter's best reading of the contract (codex-methods.md §2)",
        "the reply to item/commandExecution/requestApproval is the bare "
        "CommandExecutionApprovalDecision value, inferred from an orphaned definition",
        "wire ordering between a response and the notifications around it: the ordering "
        "here is the one the adapter is written to tolerate, not one that was observed",
    ],
    "shape": {
        "dir": "meta | send (client to server) | recv (server to client)",
        "kind": "request | notification | response | error",
        "schema": "the definition this message was validated against, or null when the contract has none",
        "msg": "the exact object that appears on the wire, one per line",
    },
    "msg": {},
}

# --- handshake -------------------------------------------------------------

send_request(1, "initialize", {
    "clientInfo": {"name": "relay", "version": "0", "title": "Relay"},
    "capabilities": {
        "experimentalApi": True,
        "requestAttestation": False,
    },
}, note="mandatory and typed; experimentalApi gates item/tool/requestUserInput")

recv_response(1, {}, note="result shape is not in the vendored contract; the adapter reads nothing from it")

# --- open the thread -------------------------------------------------------

send_request(2, "thread/start", {
    "cwd": CWD,
    "model": "gpt-5-codex",
    "sandbox": "workspace-write",
    "approvalPolicy": "on-request",
    "approvalsReviewer": "user",
}, note="both approval settings are always sent explicitly: leaving them to config.toml is how the trap gets switched on under us")

recv_note("thread/started", {
    "thread": {
        "id": THREAD,
        "sessionId": SESSION_TREE,
        "cwd": CWD,
        "createdAt": 1786000000,
        "updatedAt": 1786000000,
        "preview": "",
        "status": {"type": "idle"},
        "turns": [],
        "source": "appServer",
        "modelProvider": "openai",
        "cliVersion": "0.140.0",
        "ephemeral": False,
    },
}, note="THIS is where the thread id comes from, not the thread/start result")

recv_response(2, {}, note="unspecified; deliberately ignored")

recv_note("thread/settings/updated", {
    "threadId": THREAD,
    "threadSettings": {
        "approvalPolicy": "on-request",
        "approvalsReviewer": "user",
        "collaborationMode": {"mode": "default", "settings": {"model": "gpt-5-codex"}},
        "cwd": CWD,
        "model": "gpt-5-codex",
        "modelProvider": "openai",
        "sandboxPolicy": {"type": "workspaceWrite", "networkAccess": False},
    },
}, note="the live trap check: approvalPolicy != never and approvalsReviewer == user")

# --- turn one --------------------------------------------------------------

send_request(3, "turn/start", {
    "threadId": THREAD,
    "input": [{"type": "text", "text": "check whether the tests pass, then tell me what broke"}],
    "clientUserMessageId": "relay-turn-1",
})

recv_note("turn/started", {
    "threadId": THREAD,
    "turn": {"id": TURN1, "status": "inProgress", "items": [], "startedAt": 1786000001},
})

recv_response(3, {}, note="unspecified; the turn id came from turn/started")

recv_note("item/started", {
    "threadId": THREAD, "turnId": TURN1, "startedAtMs": 1786000001100,
    "item": {"id": "item_r1", "type": "reasoning"},
})

recv_note("item/reasoning/summaryPartAdded", {
    "threadId": THREAD, "turnId": TURN1, "itemId": "item_r1", "summaryIndex": 0,
})

recv_note("item/reasoning/summaryTextDelta", {
    "threadId": THREAD, "turnId": TURN1, "itemId": "item_r1", "summaryIndex": 0,
    "delta": "Running the test suite",
})

recv_note("item/reasoning/summaryPartAdded", {
    "threadId": THREAD, "turnId": TURN1, "itemId": "item_r1", "summaryIndex": 1,
}, note="a part boundary: two separate thoughts, not one sentence")

recv_note("item/reasoning/summaryTextDelta", {
    "threadId": THREAD, "turnId": TURN1, "itemId": "item_r1", "summaryIndex": 1,
    "delta": "then reading whatever fails",
})

recv_note("item/reasoning/textDelta", {
    "threadId": THREAD, "turnId": TURN1, "itemId": "item_r1", "contentIndex": 0,
    "delta": "The user wants the failing tests, so run go test first.",
}, note="raw thinking: normalized to Reasoning, never spoken")

recv_note("item/completed", {
    "threadId": THREAD, "turnId": TURN1, "completedAtMs": 1786000002000,
    "item": {
        "id": "item_r1", "type": "reasoning",
        "summary": ["Running the test suite", "then reading whatever fails"],
        "content": ["The user wants the failing tests, so run go test first."],
    },
}, note="already streamed, so the adapter emits nothing here rather than repeating it")

recv_note("turn/plan/updated", {
    "threadId": THREAD, "turnId": TURN1,
    "explanation": "Find out what is failing before changing anything.",
    "plan": [
        {"step": "run the test suite", "status": "inProgress"},
        {"step": "read the failures", "status": "pending"},
        {"step": "report back", "status": "pending"},
    ],
}, note="the agent stating its own plan: the best narration source in the system")

recv_note("item/started", {
    "threadId": THREAD, "turnId": TURN1, "startedAtMs": 1786000002100,
    "item": {
        "id": "item_c1", "type": "commandExecution",
        "command": "go test ./...",
        "commandActions": [{"type": "unknown", "command": "go test ./..."}],
        "cwd": CWD,
        "status": "inProgress",
        "source": "agent",
    },
})

recv_request("srv-1", "item/commandExecution/requestApproval", {
    "threadId": THREAD, "turnId": TURN1, "itemId": "item_c1",
    "startedAtMs": 1786000002150,
    "command": "go test ./...",
    "cwd": CWD,
    "approvalId": None,
    "reason": "the command runs outside the sandbox's write roots",
}, note="blocks app-server until answered; becomes NeedsInput, PING blocking")

send_response("srv-1", "accept",
              note="CommandExecutionApprovalDecision as a bare union value — inferred from an orphaned definition, see ADAPTERS.md §8 item 7")

recv_note("item/commandExecution/outputDelta", {
    "threadId": THREAD, "turnId": TURN1, "itemId": "item_c1",
    "delta": "ok  \tgithub.com/luthor007/relay/relayd/internal/event\t0.004s\n",
})

recv_note("item/commandExecution/outputDelta", {
    "threadId": THREAD, "turnId": TURN1, "itemId": "item_c1",
    "delta": "FAIL\tgithub.com/luthor007/relay/relayd/internal/store\t0.212s\n",
})

recv_note("item/completed", {
    "threadId": THREAD, "turnId": TURN1, "completedAtMs": 1786000009000,
    "item": {
        "id": "item_c1", "type": "commandExecution",
        "command": "go test ./...",
        "commandActions": [{"type": "unknown", "command": "go test ./..."}],
        "cwd": CWD,
        "status": "failed",
        "exitCode": 1,
        "durationMs": 6850,
        "aggregatedOutput": (
            "ok  \tgithub.com/luthor007/relay/relayd/internal/event\t0.004s\n"
            "FAIL\tgithub.com/luthor007/relay/relayd/internal/store\t0.212s\n"
        ),
        "source": "agent",
    },
}, note="aggregatedOutput repeats the deltas, so the adapter carries the status and not the bytes")

recv_note("item/started", {
    "threadId": THREAD, "turnId": TURN1, "startedAtMs": 1786000009100,
    "item": {"id": "item_m1", "type": "agentMessage", "text": ""},
})

for delta in ["One package fails: ", "internal/store, ", "TestVaultIsNeverIndexed."]:
    recv_note("item/agentMessage/delta", {
        "threadId": THREAD, "turnId": TURN1, "itemId": "item_m1", "delta": delta,
    })

recv_note("item/completed", {
    "threadId": THREAD, "turnId": TURN1, "completedAtMs": 1786000010000,
    "item": {
        "id": "item_m1", "type": "agentMessage",
        "text": "One package fails: internal/store, TestVaultIsNeverIndexed.",
    },
}, note="authoritative over the deltas, but already spoken — emitting it again would say the answer twice")

recv_note("thread/tokenUsage/updated", {
    "threadId": THREAD, "turnId": TURN1,
    "tokenUsage": {
        "last": {"inputTokens": 8210, "cachedInputTokens": 6144, "outputTokens": 412,
                 "reasoningOutputTokens": 256, "totalTokens": 8622},
        "total": {"inputTokens": 8210, "cachedInputTokens": 6144, "outputTokens": 412,
                  "reasoningOutputTokens": 256, "totalTokens": 8622},
        "modelContextWindow": 272000,
    },
}, note="tokens only: there is no dollar figure anywhere in the Codex contract")

recv_note("turn/completed", {
    "threadId": THREAD,
    "turn": {"id": TURN1, "status": "completed", "items": [],
             "durationMs": 9000, "startedAt": 1786000001, "completedAt": 1786000010},
}, note="carries no cost at all")

# --- turn two, steered mid-flight -----------------------------------------

send_request(4, "turn/start", {
    "threadId": THREAD,
    "input": [{"type": "text", "text": "fix it"}],
    "clientUserMessageId": "relay-turn-2",
})

recv_note("turn/started", {
    "threadId": THREAD,
    "turn": {"id": TURN2, "status": "inProgress", "items": [], "startedAt": 1786000020},
})

recv_response(4, {}, note="unspecified")

recv_note("item/started", {
    "threadId": THREAD, "turnId": TURN2, "startedAtMs": 1786000020100,
    "item": {"id": "item_m2", "type": "agentMessage", "text": ""},
})

recv_note("item/agentMessage/delta", {
    "threadId": THREAD, "turnId": TURN2, "itemId": "item_m2",
    "delta": "I'll relax the assertion so the vault table ",
})

send_request(5, "turn/steer", {
    "threadId": THREAD,
    "expectedTurnId": TURN2,
    "input": [{"type": "text", "text": "no — keep the assertion, fix the migration instead"}],
    "clientUserMessageId": "relay-steer-1",
}, note="mid-turn injection. expectedTurnId is a precondition: the request fails once TURN2 is no longer active")

recv_response(5, {}, note="unspecified")

recv_note("turn/plan/updated", {
    "threadId": THREAD, "turnId": TURN2,
    "explanation": "Steered: keep the assertion, change the migration.",
    "plan": [
        {"step": "keep TestVaultIsNeverIndexed as written", "status": "completed"},
        {"step": "drop the FTS5 trigger from the vault migration", "status": "inProgress"},
    ],
}, note="the plan visibly absorbs the steer — this is what makes steering worth having")

recv_note("item/started", {
    "threadId": THREAD, "turnId": TURN2, "startedAtMs": 1786000024000,
    "item": {
        "id": "item_f1", "type": "fileChange",
        "status": "inProgress",
        "changes": [{
            "path": f"{CWD}/relayd/internal/store/migrations/0002_vault.sql",
            "kind": {"type": "update", "move_path": None},
            "diff": "@@\n-CREATE VIRTUAL TABLE vault_fts USING fts5(secret);\n",
        }],
    },
})

recv_note("item/fileChange/patchUpdated", {
    "threadId": THREAD, "turnId": TURN2, "itemId": "item_f1",
    "changes": [{
        "path": f"{CWD}/relayd/internal/store/migrations/0002_vault.sql",
        "kind": {"type": "update", "move_path": None},
        "diff": "@@\n-CREATE VIRTUAL TABLE vault_fts USING fts5(secret);\n",
    }],
}, note="the live replacement for the deprecated item/fileChange/outputDelta")

recv_note("item/completed", {
    "threadId": THREAD, "turnId": TURN2, "completedAtMs": 1786000025000,
    "item": {
        "id": "item_f1", "type": "fileChange",
        "status": "completed",
        "changes": [{
            "path": f"{CWD}/relayd/internal/store/migrations/0002_vault.sql",
            "kind": {"type": "update", "move_path": None},
            "diff": "@@\n-CREATE VIRTUAL TABLE vault_fts USING fts5(secret);\n",
        }],
    },
})

recv_note("item/agentMessage/delta", {
    "threadId": THREAD, "turnId": TURN2, "itemId": "item_m2",
    "delta": "Dropped the FTS5 table from the vault migration; the assertion stands.",
})

recv_note("item/completed", {
    "threadId": THREAD, "turnId": TURN2, "completedAtMs": 1786000026000,
    "item": {
        "id": "item_m2", "type": "agentMessage",
        "text": "I'll relax the assertion so the vault table Dropped the FTS5 table from the vault migration; the assertion stands.",
    },
}, note="the concatenated deltas do not read as one sentence, which is exactly what a steered turn looks like")

recv_note("thread/tokenUsage/updated", {
    "threadId": THREAD, "turnId": TURN2,
    "tokenUsage": {
        "last": {"inputTokens": 9020, "cachedInputTokens": 8100, "outputTokens": 388,
                 "reasoningOutputTokens": 120, "totalTokens": 9408},
        "total": {"inputTokens": 17230, "cachedInputTokens": 14244, "outputTokens": 800,
                  "reasoningOutputTokens": 376, "totalTokens": 18030},
        "modelContextWindow": 272000,
    },
})

recv_note("turn/completed", {
    "threadId": THREAD,
    "turn": {"id": TURN2, "status": "completed", "items": [],
             "durationMs": 6000, "startedAt": 1786000020, "completedAt": 1786000026},
})

# --- detach ----------------------------------------------------------------

send_request(6, "thread/unsubscribe", {"threadId": THREAD},
             note="the only half of the subscription pair that exists; there is no thread/subscribe")

recv_response(6, {}, note="unspecified")


# --------------------------------------------------------------------------
# Validation
# --------------------------------------------------------------------------

def validate(recs) -> list[str]:
    try:
        import jsonschema
    except ImportError:
        return ["jsonschema is not installed; Go's schema_test.go still checks this file"]

    problems = []
    for r in recs:
        ref = r.get("schema")
        if not ref:
            continue
        file, _, pointer = ref.partition("#")
        doc = load(file)
        name = pointer.rsplit("/", 1)[-1]
        if name == "null":
            if r["msg"].get("params") not in (None, {}):
                problems.append(f"seq {r['seq']}: {r.get('method')} takes null params")
            continue
        sub = {"$ref": f"#/definitions/{name}", "definitions": doc["definitions"]}
        try:
            jsonschema.validate(r["msg"]["params"], sub)
        except jsonschema.ValidationError as e:  # pragma: no cover - failure path
            problems.append(f"seq {r['seq']}: {r.get('method')}: {e.message} at {list(e.absolute_path)}")
    return problems


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true", help="validate the file on disk instead of rewriting it")
    args = ap.parse_args()

    target = records
    if args.check:
        target = json.loads(OUT.read_text())
        if target != records:
            print(f"{OUT} differs from what this generator produces; re-run without --check", file=sys.stderr)
            return 1

    problems = validate(target)
    for p in problems:
        print(p, file=sys.stderr)
    if any(not p.startswith("jsonschema is not installed") for p in problems):
        return 1

    if not args.check:
        OUT.write_text(json.dumps(records, indent=1) + "\n")
        print(f"wrote {OUT} ({len(records)} records, {sum(1 for r in records if r['dir'] == 'recv')} inbound)")
    else:
        print(f"{OUT} is current and validates ({len(target)} records)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
