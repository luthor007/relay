#!/usr/bin/env python3
"""Build docs/fixtures/adapters/acp.trace.json from the vendored ACP schema.

No ACP runtime exists in the build container — no openclaw, no hermes, no
opencode — so nothing here could be recorded. This constructs a complete
session from `@zed-industries/agent-client-protocol@0.4.5`'s own schema and
validates every message against the definition it names, which makes it a
contract fixture rather than evidence of runtime behaviour. The file says so
about itself, and `TestTraceSaysItIsNotARecording` fails if that label is ever
removed.

    python3 relayd/testdata/acp/gen_trace.py            # rewrite it
    python3 relayd/testdata/acp/gen_trace.py --check    # verify the file on disk

--check fails if the file has drifted from what this produces, so a hand-edit
is a red test rather than a silent fork.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve()
REPO = HERE.parents[3]
FIXTURES = REPO / "docs" / "fixtures" / "adapters"
SCHEMA_PATH = FIXTURES / "acp-schema.json"
OUT_PATH = FIXTURES / "acp.trace.json"

SCHEMA_FILE = "acp-schema.json"
PROTOCOL_VERSION = 1
CWD = "/Users/USER/src/relay"
SESSION_ID = "sess_01JQ8Z0RELAYACP0001"

PROVENANCE = "SCHEMA-DERIVED, NOT RECORDED"

WARNING = (
    "This is NOT a recorded ACP session. No openclaw, hermes or opencode binary existed on "
    "the machine that produced it, so no live probe was possible. Every message below was "
    "constructed from the vendored @zed-industries/agent-client-protocol@0.4.5 schema and "
    "validated against the definition named in its `schema` field. It is a contract fixture, "
    "not evidence of runtime behaviour. A real recording is owed from the author's Mac, one "
    "per runtime — ADAPTERS.md §8 tracks it."
)

UNVERIFIED = [
    "the agentCapabilities each of the three runtimes actually advertises: loadSession and "
    "promptCapabilities are per-runtime and per-version, and the values here are the schema's "
    "own defaults with loadSession flipped on so the reattach path is exercised at all "
    "(ADAPTERS.md §8 item 4)",
    "the shape of a real session id. OpenClaw's ACP is a bridge in front of the Gateway and its "
    "session keys look like `agent:main:main`; Hermes and OpenCode mint their own. The opaque id "
    "used here is deliberately neither",
    "the true interleaving of session/update notifications with the responses around them: the "
    "ordering here is one the adapter is written to tolerate, not one anybody observed",
    "whether any of the three emits agent_thought_chunk at all. It is protocol-native and it is "
    "in this trace, which is why the adapter treats CapReasoning as unknown until it sees one",
    "the fs/read_text_file request at seq 30 is deliberately out of contract — Relay advertises "
    "fs and terminal false, so a conforming agent would never send it. It is here because the "
    "-32601 refusal is a path the adapter must get right, not because a runtime was seen doing it",
    "per-turn cost: there is none to record. The word `token` occurs twice in the whole schema, "
    "both times in the max_tokens stop reason, so every TurnCompleted derived from this trace "
    "carries a nil Usage",
]


def text_block(text: str) -> dict:
    return {"type": "text", "text": text}


class Trace:
    def __init__(self) -> None:
        self.records: list[dict] = []
        self.seq = 0

    def add(self, **rec) -> None:
        self.seq += 1
        rec["seq"] = self.seq
        ordered = {
            "dir": rec.pop("dir"),
            "seq": rec.pop("seq"),
            "kind": rec.pop("kind"),
        }
        if "method" in rec:
            ordered["method"] = rec.pop("method")
        ordered["schema"] = rec.pop("schema", None)
        if "note" in rec:
            ordered["note"] = rec.pop("note")
        ordered["msg"] = rec.pop("msg")
        assert not rec, f"unused keys {sorted(rec)}"
        self.records.append(ordered)

    def send_request(self, rid: int, method: str, params: dict, schema: str, note: str) -> None:
        self.add(dir="send", kind="request", method=method, schema=schema, note=note,
                 msg={"jsonrpc": "2.0", "id": rid, "method": method, "params": params})

    def send_notification(self, method: str, params: dict, schema: str, note: str) -> None:
        self.add(dir="send", kind="notification", method=method, schema=schema, note=note,
                 msg={"jsonrpc": "2.0", "method": method, "params": params})

    def send_response(self, rid: int, result: dict, schema: str | None, note: str) -> None:
        self.add(dir="send", kind="response", schema=schema, note=note,
                 msg={"jsonrpc": "2.0", "id": rid, "result": result})

    def send_error(self, rid: int, code: int, message: str, note: str) -> None:
        self.add(dir="send", kind="error", schema=None, note=note,
                 msg={"jsonrpc": "2.0", "id": rid, "error": {"code": code, "message": message}})

    def recv_response(self, rid: int, result: dict, schema: str, note: str) -> None:
        self.add(dir="recv", kind="response", schema=schema, note=note,
                 msg={"jsonrpc": "2.0", "id": rid, "result": result})

    def recv_request(self, rid: int, method: str, params: dict, schema: str, note: str) -> None:
        self.add(dir="recv", kind="request", method=method, schema=schema, note=note,
                 msg={"jsonrpc": "2.0", "id": rid, "method": method, "params": params})

    def update(self, update: dict, note: str) -> None:
        self.add(dir="recv", kind="notification", method="session/update",
                 schema=f"{SCHEMA_FILE}#/$defs/SessionNotification", note=note,
                 msg={"jsonrpc": "2.0", "method": "session/update",
                      "params": {"sessionId": SESSION_ID, "update": update}})


def build() -> list[dict]:
    t = Trace()

    t.add(
        dir="meta",
        kind="meta",
        schema=None,
        msg={
            "provenance": PROVENANCE,
            "warning": WARNING,
            "generator": "relayd/testdata/acp/gen_trace.py",
            "contract": {
                "runtimes": ["openclaw", "hermes", "opencode"],
                "package": "@zed-industries/agent-client-protocol@0.4.5",
                "schema": SCHEMA_FILE,
                "protocolVersion": PROTOCOL_VERSION,
                "transport": "newline-delimited JSON-RPC 2.0 over the agent's stdin/stdout, "
                             "one message per line, no Content-Length framing",
                "symmetric": "the agent issues requests to the client as freely as the client "
                             "issues them to the agent",
                "launch": {
                    "openclaw": "openclaw acp --session agent:main:main --require-existing",
                    "hermes": "hermes acp",
                    "opencode": "opencode acp",
                },
            },
            "covers": [
                "initialize and one-round-trip version negotiation",
                "session/new with an absolute cwd and the shared MCP registry",
                "all eight session/update variants, including plan and available_commands_update",
                "session/request_permission answered with `selected`",
                "an out-of-contract fs/read_text_file answered -32601",
                "session/cancel: the notification, the mandatory `cancelled` outcome for the "
                "outstanding permission request, the flushed update, and stopReason cancelled",
                "the redirect re-prompt that follows a cancel",
                "the queued addition delivered after the redirect turn ends",
            ],
            "unverified": UNVERIFIED,
            "shape": {
                "dir": "meta | send (client to agent) | recv (agent to client)",
                "kind": "request | notification | response | error | meta",
                "schema": "the $defs entry this message's params or result was validated "
                          "against, or null where the contract has none (JSON-RPC error objects)",
                "msg": "the exact object that appears on the wire, one per line",
            },
        },
    )

    # ---- handshake -------------------------------------------------------
    t.send_request(
        1, "initialize",
        {"protocolVersion": PROTOCOL_VERSION,
         "clientCapabilities": {"fs": {"readTextFile": False, "writeTextFile": False},
                                "terminal": False}},
        f"{SCHEMA_FILE}#/$defs/InitializeRequest",
        "all three client capabilities are declared false on purpose (ADAPTERS.md §4): advertise "
        "them and the agent does its file and shell work by calling back over RPC, where nothing "
        "obliges it to also appear in session/update",
    )
    t.recv_response(
        1,
        {"protocolVersion": PROTOCOL_VERSION,
         "agentCapabilities": {"loadSession": True,
                               "promptCapabilities": {"image": False, "audio": False,
                                                      "embeddedContext": False},
                               "mcpCapabilities": {"http": False, "sse": False}},
         "authMethods": [{"id": "oauth-personal", "name": "Log in with your account",
                          "description": "Device code flow; belongs in the installer, not a voice turn"}]},
        f"{SCHEMA_FILE}#/$defs/InitializeResponse",
        "negotiation is one round trip: the agent answers with our version or with the latest it "
        "speaks, and a client that cannot speak the answer must disconnect",
    )

    # ---- session ---------------------------------------------------------
    t.send_request(
        2, "session/new",
        {"cwd": CWD,
         "mcpServers": [{"name": "relay-registry", "command": "/usr/local/bin/relay",
                         "args": ["mcp", "serve"],
                         "env": [{"name": "RELAY_SCOPE", "value": "session"}]}]},
        f"{SCHEMA_FILE}#/$defs/NewSessionRequest",
        "cwd must be absolute; mcpServers is where MEMORY.md §7's shared registry is injected, "
        "over stdio, which is the one transport every agent must support",
    )
    t.recv_response(
        2,
        {"sessionId": SESSION_ID,
         "modes": {"currentModeId": "ask",
                   "availableModes": [{"id": "ask", "name": "Ask"},
                                      {"id": "code", "name": "Code"}]}},
        f"{SCHEMA_FILE}#/$defs/NewSessionResponse",
        "`models` is absent: it and session/set_model are the only UNSTABLE members of the surface",
    )
    t.update(
        {"sessionUpdate": "available_commands_update",
         "availableCommands": [
             {"name": "create_plan", "description": "Draft a plan before touching anything"},
             {"name": "research_codebase", "description": "Read before writing",
              "input": {"hint": "what to look for"}}]},
        "ACP's answer to SYSTEM.md §9 problem 6: the command set is pushed when it changes rather "
        "than polled. It has no home among the nine normalized events, so the adapter stores it "
        "and hands it to a hook",
    )

    # ---- turn 1 ----------------------------------------------------------
    t.send_request(
        3, "session/prompt",
        {"sessionId": SESSION_ID,
         "prompt": [text_block("Get the flaky auth test passing, then run the suite.")]},
        f"{SCHEMA_FILE}#/$defs/PromptRequest",
        "this one request does not resolve until the whole turn is over — every model call, every "
        "tool, every permission round trip happens inside it",
    )
    t.update({"sessionUpdate": "user_message_chunk",
              "content": text_block("Get the flaky auth test passing, then run the suite.")},
             "echo of what we just sent; never spoken, and there is no normalized event for user text")
    t.update({"sessionUpdate": "agent_thought_chunk",
              "content": text_block("The failure looks like a clock skew in the token fixture.")},
             "Reasoning — never spoken, on any runtime. Its arrival is also the only evidence "
             "that this runtime emits thoughts at all, so the adapter narrows CapReasoning here")
    t.update({"sessionUpdate": "plan",
              "entries": [
                  {"content": "Reproduce the flaky auth test", "priority": "high", "status": "in_progress"},
                  {"content": "Freeze the clock in the token fixture", "priority": "high", "status": "pending"},
                  {"content": "Run the full suite", "priority": "medium", "status": "pending"}]},
             "PlanUpdated, native. Structured plans exist on four of five runtimes, so plan-based "
             "narration is the normal path rather than the exception")
    t.update({"sessionUpdate": "agent_message_chunk",
              "content": text_block("Reproducing the failure first. ")},
             "TextDelta — the input to streaming TTS")
    t.update({"sessionUpdate": "tool_call",
              "toolCallId": "call_1",
              "title": "Run npm test -- auth",
              "kind": "execute",
              "status": "pending",
              "rawInput": {"command": "npm test -- auth"}},
             "ToolStarted. `title` is the human-readable part and `kind` the category; there is no "
             "tool *name* anywhere in ACP")
    t.recv_request(
        101, "session/request_permission",
        {"sessionId": SESSION_ID,
         "toolCall": {"toolCallId": "call_1", "title": "Run npm test -- auth",
                      "kind": "execute", "status": "pending",
                      "rawInput": {"command": "npm test -- auth"}},
         "options": [{"optionId": "o1", "name": "Allow", "kind": "allow_once"},
                     {"optionId": "o2", "name": "Always allow", "kind": "allow_always"},
                     {"optionId": "o3", "name": "Reject", "kind": "reject_once"}]},
        f"{SCHEMA_FILE}#/$defs/RequestPermissionRequest",
        "a request, not a notification: the agent blocks on our answer for as long as we take, "
        "which is what makes voice-answerable approval real rather than aspirational",
    )
    t.send_response(
        101, {"outcome": {"outcome": "selected", "optionId": "o1"}},
        f"{SCHEMA_FILE}#/$defs/RequestPermissionResponse",
        "o2 is a standing grant and the orchestrator never selects one on the user's behalf "
        "(ORCHESTRATOR.md §4b); it is still offered, a human just has to pick it",
    )
    t.update({"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "in_progress"},
             "an update may carry only a toolCallId with everything else absent, so the adapter "
             "merges onto the tool_call it already has")
    t.update({"sessionUpdate": "tool_call_update", "toolCallId": "call_1", "status": "completed",
              "content": [{"type": "content",
                           "content": text_block("1 failing: auth token expiry")}]},
             "`content` replaces the collection rather than appending to it")
    t.update({"sessionUpdate": "tool_call", "toolCallId": "call_2",
              "title": "Edit src/auth/token.test.ts", "kind": "edit", "status": "in_progress",
              "locations": [{"path": "src/auth/token.test.ts", "line": 42}]},
             "a second tool call, this one an edit — no permission request, so this runtime's "
             "policy allows edits in the workspace without asking")
    t.update({"sessionUpdate": "tool_call_update", "toolCallId": "call_2", "status": "completed",
              "content": [{"type": "diff", "path": "src/auth/token.test.ts",
                           "oldText": "const now = Date.now()",
                           "newText": "const now = FROZEN_NOW"}]},
             "a diff is named rather than expanded: the useful part is which file, not the bytes")
    t.update({"sessionUpdate": "plan",
              "entries": [
                  {"content": "Reproduce the flaky auth test", "priority": "high", "status": "completed"},
                  {"content": "Freeze the clock in the token fixture", "priority": "high", "status": "completed"},
                  {"content": "Run the full suite", "priority": "medium", "status": "in_progress"}]},
             "the agent must send the complete list on every plan update; the client replaces "
             "the whole plan rather than merging")
    t.update({"sessionUpdate": "agent_message_chunk",
              "content": text_block("Fixed the clock skew. Running the full suite now.")},
             "")
    t.recv_response(
        3, {"stopReason": "end_turn"},
        f"{SCHEMA_FILE}#/$defs/PromptResponse",
        "the single stopReason *is* the turn boundary; end_turn is the only one of the five that "
        "means the work finished",
    )

    # ---- turn 2: cancelled, then redirected ------------------------------
    t.send_request(
        4, "session/prompt",
        {"sessionId": SESSION_ID,
         "prompt": [text_block("Now migrate the whole auth module to the new session store.")]},
        f"{SCHEMA_FILE}#/$defs/PromptRequest",
        "",
    )
    t.update({"sessionUpdate": "agent_message_chunk",
              "content": text_block("Starting the migration. ")}, "")
    t.recv_request(
        102, "session/request_permission",
        {"sessionId": SESSION_ID,
         "toolCall": {"toolCallId": "call_3"},
         "options": [{"optionId": "p1", "name": "Allow", "kind": "allow_once"},
                     {"optionId": "p2", "name": "Reject", "kind": "reject_once"}]},
        f"{SCHEMA_FILE}#/$defs/RequestPermissionRequest",
        "toolCall is a ToolCallUpdate, so only toolCallId is guaranteed: no title, no kind, no "
        "rawInput. There is nothing human-readable to read aloud and the adapter must say so "
        "rather than infer one",
    )
    t.recv_request(
        103, "fs/read_text_file",
        {"sessionId": SESSION_ID, "path": "/Users/USER/src/relay/src/auth/session.ts"},
        f"{SCHEMA_FILE}#/$defs/ReadTextFileRequest",
        "OUT OF CONTRACT and deliberately so: we advertised fs.readTextFile false, so a "
        "conforming agent would never send this. It is here because the refusal is a path the "
        "adapter has to get right",
    )
    t.send_error(
        103, -32601,
        "fs/read_text_file is not available: client capability not advertised: relay declares "
        "fs.readTextFile, fs.writeTextFile and terminal all false",
        "visibly refused, never faked. A faked read is a lie the agent then reasons from",
    )
    t.send_notification(
        "session/cancel", {"sessionId": SESSION_ID},
        f"{SCHEMA_FILE}#/$defs/CancelNotification",
        "the user redirected: \"no, stop, do the smaller thing instead\". A notification, so the "
        "acknowledgement is the original session/prompt resolving with cancelled",
    )
    t.send_response(
        102, {"outcome": {"outcome": "cancelled"}},
        f"{SCHEMA_FILE}#/$defs/RequestPermissionResponse",
        "mandatory, not optional: an outstanding permission request left unanswered means the "
        "agent's turn cannot unwind",
    )
    t.update({"sessionUpdate": "agent_message_chunk",
              "content": text_block("Stopping; the store swap was half applied.")},
             "the flush: on cancel the agent stops model requests, aborts tools, and flushes "
             "pending session/update notifications *before* resolving. Nothing observed is lost, "
             "which is why cancel-and-re-prompt is an acceptable substitute for steering")
    t.recv_response(
        4, {"stopReason": "cancelled"},
        f"{SCHEMA_FILE}#/$defs/PromptResponse",
        "MUST be returned when the client sends session/cancel, even if the cancellation caused "
        "exceptions underneath",
    )
    t.send_request(
        5, "session/prompt",
        {"sessionId": SESSION_ID,
         "prompt": [text_block("Forget the migration. Just fix the session store import in "
                               "src/auth/session.ts.")]},
        f"{SCHEMA_FILE}#/$defs/PromptRequest",
        "the redirect: a fresh prompt carrying the merged instruction. Merging is the small "
        "model's job — the adapter's job is to make the cancel-then-re-prompt cheap and to say "
        "which of the two halves of §4's table it took",
    )
    t.update({"sessionUpdate": "agent_message_chunk",
              "content": text_block("Just the import, then.")}, "")
    t.update({"sessionUpdate": "current_mode_update", "currentModeId": "code"},
             "the agent changed its own mode. Surface it: a mode change can change permission "
             "behaviour underneath a session the registry believes it understands")
    t.recv_response(
        5, {"stopReason": "end_turn"},
        f"{SCHEMA_FILE}#/$defs/PromptResponse", "",
    )

    # ---- the queued addition ---------------------------------------------
    t.send_request(
        6, "session/prompt",
        {"sessionId": SESSION_ID, "prompt": [text_block("Also update the changelog.")]},
        f"{SCHEMA_FILE}#/$defs/PromptRequest",
        "the addition that was queued while the cancelled turn was running. It survived both the "
        "cancel and the redirect and goes out as its own turn — queued additions are not lost",
    )
    t.update({"sessionUpdate": "agent_message_chunk",
              "content": text_block("Changelog updated.")}, "")
    t.recv_response(
        6, {"stopReason": "end_turn"},
        f"{SCHEMA_FILE}#/$defs/PromptResponse", "",
    )

    return t.records


# ---------------------------------------------------------------------------


def load_schema() -> dict:
    with SCHEMA_PATH.open() as f:
        return json.load(f)


def validate(records: list[dict], schema: dict) -> list[str]:
    try:
        from jsonschema import Draft202012Validator
    except ImportError:  # pragma: no cover - the Go test is the real gate
        return ["jsonschema is not installed; skipped (schema_test.go checks the same thing)"]

    defs = schema["$defs"]
    problems: list[str] = []
    checked = 0
    for r in records:
        if r["dir"] == "meta":
            continue
        msg = r["msg"]
        if msg.get("jsonrpc") != "2.0":
            problems.append(f'seq {r["seq"]}: every message must carry jsonrpc "2.0"')
        kind = r["kind"]
        if kind == "request" and ("id" not in msg or "method" not in msg):
            problems.append(f'seq {r["seq"]}: a request needs both an id and a method')
        if kind == "notification" and "id" in msg:
            problems.append(f'seq {r["seq"]}: a notification must not carry an id')
        if kind == "response" and ("id" not in msg or "result" not in msg):
            problems.append(f'seq {r["seq"]}: a response needs an id and a result')
        if kind == "error" and ("id" not in msg or "error" not in msg):
            problems.append(f'seq {r["seq"]}: an error needs an id and an error object')

        ref = r["schema"]
        if ref is None:
            if kind != "error":
                problems.append(f'seq {r["seq"]}: only JSON-RPC errors may carry schema:null')
            continue
        name = ref.split("#/$defs/")[-1]
        if name not in defs:
            problems.append(f'seq {r["seq"]}: {ref} is not in the vendored schema')
            continue
        payload = msg["params"] if kind in ("request", "notification") else msg["result"]
        v = Draft202012Validator({"$ref": f"#/$defs/{name}", "$defs": defs})
        for err in sorted(v.iter_errors(payload), key=lambda e: list(e.path)):
            problems.append(f'seq {r["seq"]} ({ref}): {err.message} at {list(err.path)}')
        checked += 1
    print(f"validated {checked} messages against {SCHEMA_FILE}", file=sys.stderr)
    return problems


def render(records: list[dict]) -> str:
    return json.dumps(records, indent=1, ensure_ascii=False) + "\n"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true",
                    help="verify the file on disk instead of rewriting it")
    args = ap.parse_args()

    schema = load_schema()
    records = build()
    problems = validate(records, schema)
    if problems:
        for p in problems:
            print("FAIL", p, file=sys.stderr)
        return 1

    body = render(records)
    if args.check:
        if not OUT_PATH.exists():
            print(f"FAIL {OUT_PATH} does not exist", file=sys.stderr)
            return 1
        if OUT_PATH.read_text() != body:
            print(f"FAIL {OUT_PATH} has drifted from the generator", file=sys.stderr)
            return 1
        print(f"ok {OUT_PATH} matches the generator", file=sys.stderr)
        return 0

    OUT_PATH.write_text(body)
    print(f"wrote {OUT_PATH} ({len(records)} records)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
