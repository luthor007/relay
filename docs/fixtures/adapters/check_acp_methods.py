#!/usr/bin/env python3
"""Verify docs/fixtures/adapters/acp-methods.md against the vendored ACP schema.

`acp-schema.json` is `schema/schema.json` from
`npm pack @zed-industries/agent-client-protocol@0.4.5`, byte for byte. When ACP is
bumped and that file changes, this check goes red until a human re-reads the diff
and re-annotates the inventory — which is the entire point of vendoring the
contract instead of remembering it.

Three of Relay's five runtimes (OpenClaw, Hermes, OpenCode) ride on this one
contract, so a silent drift here is a silent drift in 60% of the product.

    python3 check_acp_methods.py           # verify, exit 1 on drift
    python3 check_acp_methods.py --print   # emit fresh table rows to paste

Also importable as a test:

    from check_acp_methods import check
    assert check() == []
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
DOC = HERE / "acp-methods.md"
SCHEMA = HERE / "acp-schema.json"

# The schema's `x-side` is the side that *handles* the method. Map it to the
# heading the inventory files it under.
SIDES = {
    "agent": "Client → agent",
    "client": "Agent → client",
}

DISPOSITIONS = {"use", "later", "ignore", "must-answer"}

# Structural invariants. Each one is load-bearing somewhere in ADAPTERS.md §4-§5;
# see the numbered list at the end of acp-methods.md for what each protects.
EXPECTED_METHOD_COUNT = {"agent": 8, "client": 9}
EXPECTED_UPDATE_VARIANTS = {
    "user_message_chunk",
    "agent_message_chunk",
    "agent_thought_chunk",
    "tool_call",
    "tool_call_update",
    "plan",
    "available_commands_update",
    "current_mode_update",
}
EXPECTED_STOP_REASONS = [
    "end_turn",
    "max_tokens",
    "max_turn_requests",
    "refusal",
    "cancelled",
]
EXPECTED_PERMISSION_KINDS = [
    "allow_once",
    "allow_always",
    "reject_once",
    "reject_always",
]
EXPECTED_UNSTABLE = [
    "ModelId",
    "ModelInfo",
    "SessionModelState",
    "SetSessionModelRequest",
    "SetSessionModelResponse",
]
# ClientRequest holds what the CLIENT SENDS TO THE AGENT despite the name; the
# last branch of each union is the untitled extension variant.
EXPECTED_UNION_BRANCHES = {
    "ClientRequest": 8,
    "ClientNotification": 2,
    "AgentRequest": 9,
    "AgentNotification": 2,
}
# Words that would mean mid-turn steering had arrived. Their absence is what
# ADAPTERS.md §4 stakes three runtimes on.
FORBIDDEN_SUBSTRINGS = ("steer", "inject", "interrupt")


def load() -> dict:
    return json.loads(SCHEMA.read_text())


def schema_methods(schema: dict) -> dict[str, dict[str, object]]:
    """wire method -> {side, notification, request_required, response_required}."""
    out: dict[str, dict[str, object]] = {}
    for name, node in schema["$defs"].items():
        if not isinstance(node, dict) or "x-method" not in node:
            continue
        method = node["x-method"]
        entry = out.setdefault(
            method,
            {
                "side": node["x-side"],
                "notification": True,
                "request_required": [],
                "response_required": [],
            },
        )
        if name.endswith("Response"):
            entry["response_required"] = list(node.get("required", []))
        else:
            entry["request_required"] = list(node.get("required", []))
            # A method with no *Response def is a notification. CancelNotification
            # and SessionNotification are the only two.
            entry["notification"] = name.endswith("Notification")
    # A method is only a notification if no Response def was ever seen for it.
    for method, entry in out.items():
        has_response = any(
            isinstance(n, dict)
            and n.get("x-method") == method
            and k.endswith("Response")
            for k, n in schema["$defs"].items()
        )
        entry["notification"] = not has_response
    return out


def _cells(line: str) -> list[str]:
    return [c.strip() for c in line.strip().strip("|").split("|")]


def _names(cell: str) -> list[str]:
    if "*(none)*" in cell:
        return []
    return re.findall(r"`([A-Za-z0-9_]+)`", cell)


def doc_tables(text: str) -> dict[str, list[tuple[str, str, list[str], list[str], str, int]]]:
    """side -> [(method, kind, request required, response required, disposition, line)]."""
    tables: dict[str, list[tuple[str, str, list[str], list[str], str, int]]] = {}
    current: str | None = None
    for lineno, line in enumerate(text.splitlines(), 1):
        if line.startswith("## "):
            current = None
            for side, heading in SIDES.items():
                if heading in line:
                    current = side
                    tables.setdefault(side, [])
                    break
            continue
        if current is None or not line.startswith("| `"):
            continue
        cells = _cells(line)
        if len(cells) != 5:
            continue
        tables[current].append(
            (
                cells[0].strip("`"),
                cells[1],
                _names(cells[2]),
                _names(cells[3]),
                cells[4],
                lineno,
            )
        )
    return tables


def check() -> list[str]:
    problems: list[str] = []
    if not SCHEMA.exists():
        return [f"missing schema: {SCHEMA}"]
    if not DOC.exists():
        return [f"missing inventory: {DOC}"]

    raw = SCHEMA.read_text()
    schema = json.loads(raw)
    want = schema_methods(schema)
    tables = doc_tables(DOC.read_text())

    # --- the inventory matches the schema, method by method -----------------
    for side, heading in SIDES.items():
        rows = tables.get(side)
        if rows is None:
            problems.append(f"{DOC.name}: no table found under a '## ...{heading}...' heading")
            continue
        want_side = {m: e for m, e in want.items() if e["side"] == side}
        if len(want_side) != EXPECTED_METHOD_COUNT[side]:
            problems.append(
                f"{SCHEMA.name}: {heading} has {len(want_side)} methods, "
                f"expected {EXPECTED_METHOD_COUNT[side]} — the method surface moved"
            )
        seen: set[str] = set()
        for method, kind, req, resp, disposition, lineno in rows:
            if method in seen:
                problems.append(f"{DOC.name}:{lineno}: duplicate row '{method}'")
            seen.add(method)
            entry = want_side.get(method)
            if entry is None:
                problems.append(
                    f"{DOC.name}:{lineno}: '{method}' is documented under {heading} but the "
                    f"schema does not define it there — removed or moved upstream?"
                )
                continue
            want_kind = "notification" if entry["notification"] else "request"
            if kind != want_kind:
                problems.append(
                    f"{DOC.name}:{lineno}: '{method}' documented as a {kind}, schema says {want_kind}"
                )
            if req != entry["request_required"]:
                problems.append(
                    f"{DOC.name}:{lineno}: '{method}' required params documented as "
                    f"{req or '(none)'} but schema says {entry['request_required'] or '(none)'}"
                )
            if resp != entry["response_required"]:
                problems.append(
                    f"{DOC.name}:{lineno}: '{method}' required response fields documented as "
                    f"{resp or '(none)'} but schema says {entry['response_required'] or '(none)'}"
                )
            if disposition not in DISPOSITIONS:
                problems.append(
                    f"{DOC.name}:{lineno}: '{method}' has disposition '{disposition}', "
                    f"expected one of {sorted(DISPOSITIONS)}"
                )
        for method in sorted(set(want_side) - seen):
            problems.append(
                f"{DOC.name}: schema defines {heading} method '{method}' with no row in the "
                f"inventory — new upstream?"
            )

    # --- the negative claim three runtimes depend on ------------------------
    lowered = raw.lower()
    for word in FORBIDDEN_SUBSTRINGS:
        if word in lowered:
            problems.append(
                f"{SCHEMA.name}: found '{word}' — ADAPTERS.md §4 states ACP has no mid-turn "
                f"steering and routes redirects through cancel + re-prompt. Re-read §4."
            )
    for union, count in EXPECTED_UNION_BRANCHES.items():
        got = len(schema["$defs"][union]["anyOf"])
        if got != count:
            problems.append(
                f"{SCHEMA.name}: {union} has {got} branches, expected {count} — a method was "
                f"added or removed in that direction"
            )

    # --- the payload shapes the adapter switches on -------------------------
    variants = {b["properties"]["sessionUpdate"]["const"] for b in schema["$defs"]["SessionUpdate"]["oneOf"]}
    if variants != EXPECTED_UPDATE_VARIANTS:
        problems.append(
            f"{SCHEMA.name}: session/update variants changed: "
            f"{sorted(variants ^ EXPECTED_UPDATE_VARIANTS)}"
        )
    stop = [b["const"] for b in schema["$defs"]["StopReason"]["oneOf"]]
    if stop != EXPECTED_STOP_REASONS:
        problems.append(f"{SCHEMA.name}: StopReason is {stop}, expected {EXPECTED_STOP_REASONS}")
    outcomes = sorted(
        b["properties"]["outcome"]["const"]
        for b in schema["$defs"]["RequestPermissionOutcome"]["oneOf"]
    )
    if outcomes != ["cancelled", "selected"]:
        problems.append(f"{SCHEMA.name}: RequestPermissionOutcome is {outcomes}")
    kinds = [b["const"] for b in schema["$defs"]["PermissionOptionKind"]["oneOf"]]
    if kinds != EXPECTED_PERMISSION_KINDS:
        problems.append(f"{SCHEMA.name}: PermissionOptionKind is {kinds}")
    if schema["$defs"]["ToolCallUpdate"]["required"] != ["toolCallId"]:
        problems.append(
            f"{SCHEMA.name}: ToolCallUpdate.required changed — session/request_permission's "
            f"toolCall is a ToolCallUpdate, so this decides what the ping may say"
        )

    # --- capability surface: a new flag is a new decision -------------------
    caps = {
        "ClientCapabilities": {"_meta", "fs", "terminal"},
        "FileSystemCapability": {"_meta", "readTextFile", "writeTextFile"},
        "AgentCapabilities": {"_meta", "loadSession", "promptCapabilities", "mcpCapabilities"},
        "PromptCapabilities": {"_meta", "image", "audio", "embeddedContext"},
    }
    for name, expected in caps.items():
        got = set(schema["$defs"][name]["properties"])
        if got != expected:
            problems.append(
                f"{SCHEMA.name}: {name} fields changed: {sorted(got ^ expected)} — "
                f"decide what Relay advertises before shipping"
            )

    unstable = sorted(
        k
        for k, v in schema["$defs"].items()
        if isinstance(v, dict) and "UNSTABLE" in (v.get("description") or "")
    )
    if unstable != EXPECTED_UNSTABLE:
        problems.append(
            f"{SCHEMA.name}: the UNSTABLE set is {unstable}, expected {EXPECTED_UNSTABLE} — "
            f"something was promoted or demoted upstream"
        )

    if "cost" in lowered or "usage" in lowered:
        problems.append(
            f"{SCHEMA.name}: found a cost/usage field — ADAPTERS.md §5 and §8 say ACP carries "
            f"no per-turn metering at all. Good news, but update both."
        )

    return problems


def print_rows() -> None:
    schema = load()
    want = schema_methods(schema)
    for side, heading in SIDES.items():
        rows = sorted((m, e) for m, e in want.items() if e["side"] == side)
        print(f"\n## {heading} ({len(rows)} methods)\n")
        print("| Method | Kind | Required params | Response — required | Relay |")
        print("|---|---|---|---|---|")
        for method, entry in rows:
            kind = "notification" if entry["notification"] else "request"
            req = ", ".join(f"`{r}`" for r in entry["request_required"]) or "*(none)*"
            resp = ", ".join(f"`{r}`" for r in entry["response_required"]) or "*(none)*"
            print(f"| `{method}` | {kind} | {req} | {resp} | ignore |")


def test_acp_method_inventory_matches_schema() -> None:
    problems = check()
    assert not problems, "\n".join(problems)


def main() -> int:
    if "--print" in sys.argv:
        print_rows()
        return 0
    problems = check()
    if problems:
        print(f"acp-methods.md is out of date with acp-schema.json ({len(problems)}):\n")
        for p in problems:
            print(f"  {p}")
        return 1
    print("acp-methods.md matches acp-schema.json (@zed-industries/agent-client-protocol 0.4.5)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
