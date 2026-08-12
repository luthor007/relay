#!/usr/bin/env python3
"""Verify docs/fixtures/adapters/codex-methods.md against the vendored Codex schemas.

The three schema files are produced by `codex app-server generate-json-schema`.
When Codex is bumped and those files change, this check goes red until a human
re-reads the diff and re-annotates the inventory — which is the entire point of
vendoring the contract instead of remembering it.

    python3 check_codex_methods.py           # verify, exit 1 on drift
    python3 check_codex_methods.py --print   # emit fresh table rows to paste

Also importable as a test:

    from check_codex_methods import check
    assert check() == []
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
DOC = HERE / "codex-methods.md"

# schema file -> (heading anchor text in the doc, human label)
SCHEMAS = {
    "ClientRequest.json": ("Client → server requests", "client request"),
    "ServerNotification.json": ("Server → client notifications", "notification"),
    "ServerRequest.json": ("Server → client requests", "server request"),
}

DISPOSITIONS = {"use", "later", "ignore", "must-answer"}


def schema_methods(path: Path) -> dict[str, str]:
    """method name -> params definition name ('' when params are null/inline)."""
    doc = json.loads(path.read_text())
    out: dict[str, str] = {}
    for variant in doc["oneOf"]:
        props = variant["properties"]
        name = props["method"]["enum"][0]
        params = props.get("params", {})
        ref = params.get("$ref", "") if isinstance(params, dict) else ""
        out[name] = ref.rsplit("/", 1)[-1] if ref else ""
    return out


def doc_tables(text: str) -> dict[str, list[tuple[str, str, str, int]]]:
    """anchor -> [(method, params type, disposition, line number)]."""
    lines = text.splitlines()
    tables: dict[str, list[tuple[str, str, str, int]]] = {}
    current: str | None = None
    for lineno, line in enumerate(lines, 1):
        if line.startswith("## "):
            current = None
            for anchor, _ in SCHEMAS.values():
                if anchor in line:
                    current = anchor
                    tables.setdefault(anchor, [])
                    break
            continue
        if current is None or not line.startswith("| `"):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) < 4:
            continue
        method = cells[0].strip("`")
        # "`ThreadStartParams` — required..." or "*(params: `null`)*"
        m = re.search(r"`([A-Za-z0-9_]+)`", cells[1])
        params = m.group(1) if m else ""
        if params == "null":
            params = ""
        tables[current].append((method, params, cells[-1], lineno))
    return tables


def check() -> list[str]:
    problems: list[str] = []
    if not DOC.exists():
        return [f"missing inventory: {DOC}"]
    tables = doc_tables(DOC.read_text())

    for filename, (anchor, label) in SCHEMAS.items():
        path = HERE / filename
        if not path.exists():
            problems.append(f"missing schema: {path}")
            continue
        want = schema_methods(path)
        rows = tables.get(anchor)
        if rows is None:
            problems.append(f"{DOC.name}: no table found under a '## ...{anchor}...' heading")
            continue

        seen: set[str] = set()
        for method, params, disposition, lineno in rows:
            if method in seen:
                problems.append(f"{DOC.name}:{lineno}: duplicate {label} row '{method}'")
            seen.add(method)
            if method not in want:
                problems.append(
                    f"{DOC.name}:{lineno}: '{method}' is documented but is not in {filename} "
                    f"— removed upstream?"
                )
                continue
            if params != want[method]:
                problems.append(
                    f"{DOC.name}:{lineno}: '{method}' params documented as "
                    f"'{params or '(none)'}' but {filename} says '{want[method] or '(none)'}'"
                )
            if disposition not in DISPOSITIONS:
                problems.append(
                    f"{DOC.name}:{lineno}: '{method}' has disposition '{disposition}', "
                    f"expected one of {sorted(DISPOSITIONS)}"
                )

        for method in sorted(set(want) - seen):
            problems.append(
                f"{DOC.name}: {filename} defines {label} '{method}' "
                f"(params {want[method] or '(none)'}) with no row in the inventory — new upstream?"
            )

        header_count = re.search(
            rf"\| `{re.escape(filename)}` \| [^|]+ \| (\d+) \|", DOC.read_text()
        )
        if header_count and int(header_count.group(1)) != len(want):
            problems.append(
                f"{DOC.name}: header table says {filename} has {header_count.group(1)} "
                f"variants; it has {len(want)}"
            )

    problems.extend(check_tally(DOC.read_text(), tables))
    return problems


def check_tally(text: str, tables: dict[str, list[tuple[str, str, str, int]]]) -> list[str]:
    """The §9 summary table must equal the dispositions actually written in §4-§6."""
    problems: list[str] = []
    order = ["use", "must-answer", "later", "ignore"]
    for anchor, _ in SCHEMAS.values():
        rows = tables.get(anchor)
        if rows is None:
            continue
        pattern = rf"^\| {re.escape(anchor)} \| (\d+) \| (\d+) \| (\d+) \| (\d+) \| (\d+) \|"
        m = re.search(pattern, text, re.MULTILINE)
        if not m:
            problems.append(
                f"{DOC.name}: §9 tally table has no row starting '| {anchor} |'"
            )
            continue
        claimed = [int(g) for g in m.groups()]
        actual = [len(rows)] + [sum(1 for r in rows if r[2] == d) for d in order]
        if claimed != actual:
            problems.append(
                f"{DOC.name}: §9 tally for '{anchor}' says "
                f"total/{'/'.join(order)} = {'/'.join(map(str, claimed))}, "
                f"but the table has {'/'.join(map(str, actual))}"
            )
    return problems


def print_rows() -> None:
    for filename, (anchor, _) in SCHEMAS.items():
        print(f"\n## {anchor} (`{filename}`, {len(schema_methods(HERE / filename))})\n")
        print("| Method | Params type — required fields | Purpose | Relay |")
        print("|---|---|---|---|")
        doc = json.loads((HERE / filename).read_text())
        defs = doc["definitions"]
        for variant in doc["oneOf"]:
            props = variant["properties"]
            method = props["method"]["enum"][0]
            params = props.get("params", {})
            ref = params.get("$ref", "").rsplit("/", 1)[-1] if isinstance(params, dict) else ""
            if ref:
                required = defs.get(ref, {}).get("required", [])
                req = ", ".join(f"`{r}`" for r in required) if required else "*(none required)*"
                cell = f"`{ref}` — {req}"
            else:
                cell = "*(params: `null`)*"
            desc = (variant.get("description") or "").replace("\n", " ").strip()
            print(f"| `{method}` | {cell} | {desc} | ignore |")


def test_codex_method_inventory_matches_schemas() -> None:
    problems = check()
    assert not problems, "\n".join(problems)


def main() -> int:
    if "--print" in sys.argv:
        print_rows()
        return 0
    problems = check()
    if problems:
        print(f"codex-methods.md is out of date with the vendored schemas ({len(problems)}):\n")
        for p in problems:
            print(f"  {p}")
        return 1
    print("codex-methods.md matches ClientRequest.json, ServerNotification.json, ServerRequest.json")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
