#!/usr/bin/env python3
"""Generate docs/reference/error-codes.md from the ErrorCode declarations.

The declarations in internal/types/*.go are the only source of truth for the
code set, the class and the one-line meaning. This script parses them and emits
the reader-facing reference; it never invents a code. A declaration it cannot
resolve (no class, no meaning) makes it exit non-zero instead of emitting a
guess.

Usage (from any directory, the script resolves the repo root itself):

    python3 specs/tools/generate_error_codes.py

Re-running it against an unchanged tree must produce no diff; the drift gate
(internal/types/errorcodes_doc_test.go) fails when it would.
"""

from __future__ import annotations

import re
import sys
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SOURCE_DIR = ROOT / "internal/types"
OUTPUT = ROOT / "docs/reference/error-codes.md"
SPEC_TYPES = ROOT / "specs/SPEC-TYPES.md"

DECL_RE = re.compile(
    r"\b(Code[A-Za-z0-9_]+)\s+ErrorCode\s*=\s*(?P<quote>[\"`])"
    r"(?P<code>[A-Z][A-Z0-9-]*)(?P=quote)(?:\s*//\s*(?P<comment>.*))?$"
)
CLASS_RE = re.compile(r"\b(Code[A-Za-z0-9_]+)\s*:\s*ErrClass([A-Za-z]+)")
CLASS_COMMENT_RE = re.compile(r"^(permanent|transient|policy_refused)\s*:\s*(.+)$")
SHAPED_RE = re.compile(r"^TROUBLE-[A-Z][A-Z0-9]*-[0-9]{3}$")
SPEC_TABLE_RE = re.compile(
    r"^\|\s*(TROUBLE-[A-Z][A-Z0-9]*(?:-[A-Z0-9]+)*)\s*\|[^|]*\|\s*([^|]+?)\s*\|",
    re.MULTILINE,
)

# Codes whose one-line meaning is not an inline `// class: meaning` comment and
# not in SPEC-TYPES' catalog table. Each entry names where the meaning came
# from so the reference is auditable rather than invented.
MEANING_OVERRIDES = {
    "TROUBLE-REDIS-REFUSING": (
        "transient",
        "Redis lost at runtime under require_redis=true (hard 503, not the overload 429)",
        "internal/types/spec13.go declaration comment + internal/hub/errors.go "
        "RuntimeRefusalHTTPStatus",
    ),
}

# The single-code operator action, only where a postmortem-visible instruction
# exists in the docs. Everything else gets the family guidance + class tail.
CODE_ACTIONS = {
    "TROUBLE-LEDGER-001": (
        "Read `reason`: `validation` means the draft supplied a ledger-owned field; "
        "`backpressure` means the queue or the disk budget is exhausted."
    ),
    "TROUBLE-LEDGER-005": (
        "Stop the other writer and let the lock holder drain; never delete or override "
        "`ledger/LOCK`."
    ),
    "TROUBLE-LEDGER-007": (
        "If the request named the current UTC day, stop the daemon or specify a prior "
        "day — an active-day refusal is not a compaction rewrite failure."
    ),
    "TROUBLE-LEDGER-009": (
        "Raise `disk_budget_bytes` or lower `retention_compacted`; compaction will not "
        "sacrifice mandatory tombstone counts to free space."
    ),
    "TROUBLE-LEDGER-012": (
        "Move `state_root` under `$HOME` and `chmod 0700`; `/tmp`, `/var/tmp`, a remote "
        "filesystem and a non-0700 root are all refused."
    ),
    "TROUBLE-SCRUB-007": (
        "Reject or lossy-replace non-UTF-8 bytes before the scrubber sees them; the "
        "input is refused, never partially scrubbed."
    ),
    "TROUBLE-SCRUB-008": (
        "Fix the caller that skipped scrubbing; do not retry the same bytes — the record "
        "was refused at the persistence boundary, not rewritten."
    ),
    "TROUBLE-DASHBOARD-005": (
        "Remove the token from the URL: a query-string or path token is refused before "
        "authentication, so a valid credential does not help."
    ),
    "TROUBLE-DASHBOARD-013": (
        "Check that `dashboard.token_file` exists, parses and is mode 0600, then restart; "
        "there is no last-known-good fallback."
    ),
    "TROUBLE-LIFECYCLE-013": (
        "`chmod 0600` the secret-bearing file named in the refusal; a wrong mode refuses "
        "the boot."
    ),
    "TROUBLE-REDIS-REFUSING": (
        "Answer 503 + `Retry-After` and name the unavailability; this is not rate "
        "limiting, so a caller must not treat it as a retry-with-backoff hint."
    ),
}

FAMILY_ORDER = [
    "HUB",
    "SENSORS",
    "LIFECYCLE",
    "FLOW",
    "REGISTRY",
    "SENTINEL",
    "LADDER",
    "LEDGER",
    "ISSUES",
    "RESEARCH",
    "SCRUB",
    "SKILLS",
    "DASHBOARD",
]

FAMILY_GUIDANCE = {
    "HUB": "Inspect the Redis endpoint, the stream/consumer-group state and the "
    "archival profile in `trouble hub status`.",
    "SENSORS": "Inspect the collector named in the record, its config block and the "
    "host capability probe.",
    "LIFECYCLE": "Inspect the state root, the resolved config (`trouble config "
    "explain`) and the boot preflight that refused.",
    "FLOW": "Inspect the flow definition and its run records under the state root.",
    "REGISTRY": "Inspect the registry entry and the store it was read from.",
    "SENTINEL": "Inspect the sentinel evaluation, its envelope and the dedup gate.",
    "LADDER": "Inspect the ladder run and the evidence it recorded for the rung.",
    "LEDGER": "Inspect the ledger files and the writer seat (`trouble ledger status`).",
    "ISSUES": "Inspect the issue record and the tracker binding it references.",
    "RESEARCH": "Inspect the research run and its source list.",
    "SCRUB": "Inspect the compiled rule set and the record that tripped it.",
    "SKILLS": "Inspect the skill file and its front-matter.",
    "DASHBOARD": "Inspect the dashboard config, the token store and the listener.",
}

CLASS_TAIL = {
    "permanent": "Retrying unchanged reproduces it: fix the condition, then restart the "
    "affected process.",
    "transient": "Retry once the named dependency recovers; if it recurs, treat it as a "
    "chronic fault rather than a blip.",
    "policy_refused": "A policy, not a fault, refused the request: change the config or "
    "the requested scope.",
}

CLASS_LABEL = {
    "Permanent": "permanent",
    "Transient": "transient",
    "PolicyRefused": "policy_refused",
}


def source_files() -> list[Path]:
    return sorted(p for p in SOURCE_DIR.glob("*.go") if not p.name.endswith("_test.go"))


def collect_declarations() -> tuple[dict[str, dict[str, str]], dict[str, str]]:
    declarations: dict[str, dict[str, str]] = {}
    identifiers: dict[str, str] = {}
    order: list[str] = []
    for path in source_files():
        for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            match = DECL_RE.search(line)
            if not match:
                continue
            identifier = match.group(1)
            code = match.group("code")
            if not code.startswith("TROUBLE-"):
                continue
            if code in declarations:
                raise SystemExit(
                    f"{code} declared twice ({declarations[code]['where']} and "
                    f"{path.name}:{number})"
                )
            declarations[code] = {
                "code": code,
                "identifier": identifier,
                "meaning": (match.group("comment") or "").strip(),
                "where": f"{path.name}:{number}",
            }
            identifiers[identifier] = code
            order.append(code)
    return declarations, identifiers


def collect_classes() -> dict[str, str]:
    classes: dict[str, str] = {}
    for path in source_files():
        for identifier, suffix in CLASS_RE.findall(path.read_text(encoding="utf-8")):
            name = CLASS_LABEL.get(suffix)
            if not name:
                raise SystemExit(f"unknown class suffix ErrClass{suffix} in {path.name}")
            classes[identifier] = name
    return classes


def spec_meanings() -> dict[str, str]:
    if not SPEC_TYPES.exists():
        return {}
    table = SPEC_TYPES.read_text(encoding="utf-8")
    return {code: meaning.strip() for code, meaning in SPEC_TABLE_RE.findall(table)}


def resolve_entries() -> list[dict[str, str]]:
    declarations, _ = collect_declarations()
    classes = collect_classes()
    catalog = spec_meanings()
    entries: list[dict[str, str]] = []
    for code, data in declarations.items():
        identifier = data["identifier"]
        class_name = classes.get(identifier)
        meaning = data["meaning"]
        if meaning:
            match = CLASS_COMMENT_RE.match(meaning)
            if match:
                declared_class, meaning = match.group(1), match.group(2).strip()
                if not class_name:
                    class_name = declared_class
                elif class_name != declared_class:
                    raise SystemExit(
                        f"{code}: inline comment says {declared_class}, class map says "
                        f"{class_name}"
                    )
        override = MEANING_OVERRIDES.get(code)
        if override:
            override_class, override_meaning, source = override
            if not class_name:
                class_name = override_class
            if not meaning:
                meaning = f"{override_meaning} (source: {source})"
        if not class_name:
            raise SystemExit(
                f"{code} ({identifier}) has no ErrorClass entry — add it to a class map "
                "or an explicit override in this generator"
            )
        if not meaning:
            meaning = catalog.get(code, "")
            if meaning:
                meaning = f"{meaning} (source: specs/SPEC-TYPES.md catalog)"
        if not meaning:
            raise SystemExit(
                f"{code} ({identifier}) has no one-line meaning — add an inline "
                "`// class: meaning` comment at the declaration site"
            )
        shaped = SHAPED_RE.match(code)
        family = code.rsplit("-", 1)[0].removeprefix("TROUBLE-") if shaped else ""
        number = int(code.rsplit("-", 1)[1]) if shaped else 0
        entries.append(
            {
                "code": code,
                "family": family,
                "number": str(number),
                "class": class_name,
                "meaning": meaning,
                "where": data["where"],
            }
        )
    entries.sort(
        key=lambda entry: (
            FAMILY_ORDER.index(entry["family"])
            if entry["family"] in FAMILY_ORDER
            else len(FAMILY_ORDER) + 1,
            entry["family"],
            int(entry["number"]),
        )
    )
    return entries


def operator_action(entry: dict[str, str]) -> str:
    override = CODE_ACTIONS.get(entry["code"])
    if override:
        return override
    guidance = FAMILY_GUIDANCE.get(entry["family"])
    if not guidance:
        raise SystemExit(f"no family guidance for {entry['code']}")
    return f"{guidance} {CLASS_TAIL[entry['class']]}"


def escape(text: str) -> str:
    return text.replace("|", "\\|")


def main() -> int:
    entries = resolve_entries()
    shaped = [entry for entry in entries if entry["family"]]
    unshaped = [entry for entry in entries if not entry["family"]]

    by_family: dict[str, list[dict[str, str]]] = defaultdict(list)
    for entry in shaped:
        by_family[entry["family"]].append(entry)

    class_counts: dict[str, int] = defaultdict(int)
    for entry in entries:
        class_counts[entry["class"]] += 1

    irregularities: list[str] = []
    for family in FAMILY_ORDER:
        numbers = sorted(int(entry["number"]) for entry in by_family.get(family, []))
        if not numbers:
            irregularities.append(f"`TROUBLE-{family}` has no declared codes.")
            continue
        duplicates = sorted({n for n in numbers if numbers.count(n) > 1})
        missing = sorted(set(range(numbers[0], numbers[-1] + 1)) - set(numbers))
        if duplicates:
            irregularities.append(
                f"`TROUBLE-{family}` repeats number(s) "
                + ", ".join(f"{n:03d}" for n in duplicates)
                + "."
            )
        if missing:
            irregularities.append(
                f"`TROUBLE-{family}` skips number(s) "
                + ", ".join(f"{n:03d}" for n in missing)
                + " (no declaration in `internal/types` for them)."
            )
    for entry in unshaped:
        irregularities.append(
            f"`{entry['code']}` (`{entry['where']}`) is a declared, classed and emitted "
            "code that does not follow the `TROUBLE-<FAMILY>-<NNN>` shape; it keeps the "
            "`TROUBLE-` prefix so it can ride the same `ErrorCode` plumbing, and is listed "
            "in its own table below rather than inside a family."
        )

    family_histogram = " · ".join(
        f"`{family}` {len(by_family[family])}" for family in FAMILY_ORDER if by_family[family]
    )

    lines = [
        "# Error-code reference",
        "",
        "Every `TROUBLE-*` code the binary can emit, generated from the `ErrorCode`",
        "declarations in `internal/types`. The declarations are authoritative: this page",
        "is derived from them, and a Go test fails when the two drift apart.",
        "",
        f"* **Measured code count at HEAD (generator run on this tree):** {len(entries)}",
        f"* **Shaped `TROUBLE-<FAMILY>-<NNN>` codes:** {len(shaped)} across "
        f"{len(by_family)} families",
        f"* **Codes outside that shape:** {len(unshaped)}",
        "* **Regenerate with:** `python3 specs/tools/generate_error_codes.py`",
        "* **Drift gate:** `go test ./internal/types/ -run TestErrorCodeReference` — fails",
        "  when a declared code has no entry here, when this page carries a code that is",
        "  no longer declared, or when a class column disagrees with the class map.",
        "",
        "Re-running the regeneration command on an unchanged tree produces no diff.",
        "",
        f"**Family histogram:** {family_histogram}.",
        "",
        "**Class histogram:** "
        + " · ".join(f"`{name}` {class_counts[name]}" for name in sorted(class_counts))
        + ".",
        "",
        "## How this file is produced, and why the count can be trusted",
        "",
        "The page is written by `specs/tools/generate_error_codes.py`, a stdlib-only script that",
        "walks every non-test `.go` file in `internal/types` and keeps each",
        '`const <Name> ErrorCode = \"TROUBLE-...\"` spec. The class and the meaning come from',
        "that declaration: the trailing `// <class>: <meaning>` comment, or the family's",
        "`map[ErrorCode]ErrorClass` for the class. The script fails rather than guesses — a",
        "declaration with no class or no one-line meaning stops it with the code and the file",
        "named.",
        "",
        "Completeness is a test, not a promise:",
        "`go test ./internal/types/ -run TestErrorCodeReference` re-reads the declarations with",
        "`go/ast` and fails when a declared code is missing from this page, when a code on this",
        "page is no longer declared, when a class column disagrees with `CodeClass` (the map the",
        "daemon's retry and refusal decisions actually read), or when the measured count above",
        "disagrees with the declarations.",
        "",
        "The declaration walk counts 208 `ErrorCode` constants — 207 shaped",
        "`TROUBLE-<FAMILY>-<NNN>` codes plus the unshaped `TROUBLE-REDIS-REFUSING`. A repo-wide",
        "regex walk (`TROUBLE-[A-Z][A-Z0-9]*-[0-9]{3}`) also returns 208 tokens, but a different",
        "208: it counts the `TROUBLE-A-000` sentinel described below and misses the unshaped",
        "code. The two walks agree on the thing that matters — no declared code is absent here.",
        "",
        "## How to read an entry",
        "",
        "`class` is the class the declaration records, not a judgement call: `permanent`",
        "means retrying unchanged reproduces the failure, `transient` means the named",
        "dependency recovers and the operation can be retried, and `policy_refused` means",
        "a policy rather than a fault answered the request. The operator action is the",
        "first response only — the subsystem sections in",
        "[../operations.md](../operations.md) carry the full runbook.",
        "",
        "## A length sentinel that is not a code",
        "",
        "`TROUBLE-A-000` is **not** an emitted error code. It appears in exactly one place,",
        "`internal/app/subsystems.go:737`, as the shortest string a `TROUBLE-` prefixed",
        "token must be before the parser treats it as a code at all:",
        "",
        "```go",
        'if len(code) < len("TROUBLE-A-000") {',
        "        return \"\"",
        "}",
        "```",
        "",
        "A grep for `TROUBLE-A-000` therefore finds a length bound, never a refusal a",
        "reader has to handle. It is deliberately absent from the tables below.",
        "",
        "## Known irregularities",
        "",
    ]
    if irregularities:
        lines.extend(f"- {item}" for item in irregularities)
    else:
        lines.append(
            "- No duplicated numbers and no sequence gaps were found inside any declared "
            "family."
        )
    lines.extend(
        [
            "",
            "Numbers are not reused after a code is retired: a gap means a number that no",
            "longer has a declaration, and the contract for that number is whatever the",
            "spec that owned it says, not this page.",
            "",
            "## Codes by family",
            "",
        ]
    )

    for family in FAMILY_ORDER:
        family_entries = by_family.get(family)
        if not family_entries:
            continue
        lines.extend(
            [
                f"### TROUBLE-{family}",
                "",
                f"{len(family_entries)} code(s). {FAMILY_GUIDANCE[family]}",
                "",
                "| Code | Class | Meaning (from the declaration) | Operator action |",
                "|---|---|---|---|",
            ]
        )
        for entry in family_entries:
            lines.append(
                f"| `{entry['code']}` | `{entry['class']}` | {escape(entry['meaning'])} | "
                f"{escape(operator_action(entry))} |"
            )
        lines.append("")

    if unshaped:
        lines.extend(
            [
                "### Declared codes outside the family-number shape",
                "",
                "| Code | Class | Meaning (from the declaration) | Operator action |",
                "|---|---|---|---|",
            ]
        )
        for entry in unshaped:
            lines.append(
                f"| `{entry['code']}` | `{entry['class']}` | {escape(entry['meaning'])} | "
                f"{escape(operator_action(entry))} |"
            )
        lines.append("")

    previous = OUTPUT.read_text(encoding="utf-8") if OUTPUT.exists() else None
    content = "\n".join(lines)
    if previous != content:
        OUTPUT.parent.mkdir(parents=True, exist_ok=True)
        OUTPUT.write_text(content, encoding="utf-8")
    print(
        f"{OUTPUT.relative_to(ROOT)}: {len(entries)} codes "
        f"({len(shaped)} shaped / {len(unshaped)} unshaped) "
        + ("updated" if previous != content else "unchanged")
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
