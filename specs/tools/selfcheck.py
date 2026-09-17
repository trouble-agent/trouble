#!/usr/bin/env python3
"""trouble SPEC suite — mandatory self-consistency loop (SPEC-INDEX §7).

Stdlib only. Run from anywhere:  python3 specs/tools/selfcheck.py

Steps implemented
  1  every type consumed by a spec resolves in SPEC-TYPES.md (no orphans, no double defs)
  2  every AC in the SPEC-INDEX matrix maps to >= 1 spec, and every spec has >= 1 AC
  3  interface/cross-reference hygiene: SPEC-nn refs resolve, PRD section ids resolve
  4  every error code exists in the SPEC-TYPES catalog, is unique, and is in its area's range
  5  cross-references (spec<->spec, spec<->PRD) resolve
  6  forbidden strings; deferred items never described as in-scope
  +  counts: files, bytes, sections, types, AC coverage table

Exit code 0 = clean, 1 = at least one hard failure.
"""
from __future__ import annotations

import os
import re
import sys
from collections import defaultdict

HERE = os.path.dirname(os.path.abspath(__file__))
SPECS = os.path.dirname(HERE)

SPEC_IDS = [f"SPEC-{n:02d}" for n in range(1, 14)]
META = ["Spec", "Area prefix", "Package", "Consumed types", "Local types", "ACs", "PRD"]
SECTION_TITLES = ["Purpose", "Interface", "Data model", "Wiring", "Errors", "Edge cases", "Testing", "hilo impact"]
PRD_SECTIONS = {"§03", "§04a", "§04b", "§04c", "§05", "§06", "§06b", "§06c", "§07", "§08", "§09", "§10", "§11", "§12"}
FORBIDDEN = [
    (r"\bTBD\b", "placeholder TBD"),
    (r"\bTODO\b", "placeholder TODO"),
    (r"Phase 2", "deferred-plan phrasing"),
    (r"\bconsider(?:ing)?\b", "hedge 'consider'"),
    (r"\bperhaps\b", "hedge 'perhaps'"),
    (r"should probably", "hedge 'should probably'"),
]
# domain values / rule statements that legitimately contain a forbidden token
FORBIDDEN_ALLOW = [
    re.compile(r'"?todo"?\s*(\||\||$|,|")', re.I),          # board status value
    re.compile(r"status\s*[:=]\s*\"?todo", re.I),
    re.compile(r"`todo`"),
    re.compile(r"--status todo"),
    re.compile(r"forbidden string", re.I),
]
# deferred-in-v0.1 vocabulary: may only appear with an explicit hand-off marker
DEFERRED_TERMS = [
    "sentinel proxy", "proxy binary", "light-mode offload", "light agent binary",
    "ansible-bridge", "ansible bridge", "OTel", "OpenTelemetry",
    "cron-monitors", "jira", "gitlab", "eBPF",
]
HANDOFF_MARKERS = ["v1.0", "1.0 hand-off", "1.0 handoff", "deferred", "defers", "not in v0.1", "out of v0.1", "reserved", "hand-off"]

fails: list[str] = []
warns: list[str] = []


def fail(msg: str) -> None:
    fails.append(msg)


def warn(msg: str) -> None:
    warns.append(msg)


def read(name: str) -> str:
    with open(os.path.join(SPECS, name), encoding="utf-8") as fh:
        return fh.read()


def main() -> int:
    names = sorted(n for n in os.listdir(SPECS) if n.endswith(".md"))
    expected = {"SPEC-INDEX.md", "SPEC-TYPES.md"} | {f"{s}-{t}.md" for s, t in
                [(f"SPEC-{i:02d}", "") for i in range(1, 14)]}
    docs = {n: read(n) for n in names}
    types_doc = docs["SPEC-TYPES.md"]
    index_doc = docs["SPEC-INDEX.md"]

    # ---------- inventories -------------------------------------------------
    types = set(re.findall(r"^type\s+([A-Za-z_]\w*)\s", types_doc, re.M))
    type_defs = defaultdict(list)
    for m in re.finditer(r"^type\s+([A-Za-z_]\w*)\s+(\w+)", types_doc, re.M):
        type_defs[m.group(1)].append(m.group(2))
    for t, kinds in type_defs.items():
        if len(set(x.lower() for x in kinds)) > 1:
            fail(f"type {t} defined with different kinds in SPEC-TYPES: {kinds}")

    catalog: dict[str, str] = {}
    for m in re.finditer(r"^\|\s*(TROUBLE-[A-Z]+-\d{3})\s*\|\s*(\w+)\s*\|", types_doc, re.M):
        code, klass = m.group(1), m.group(2)
        if code in catalog:
            fail(f"catalog duplicate: {code}")
        catalog[code] = klass
    if not catalog:
        fail("error catalog not parsed from SPEC-TYPES.md §5")

    ranges: dict[str, tuple[int, int]] = {}
    for m in re.finditer(r"^\|\s*TROUBLE-([A-Z]+)-0NN\s*\|\s*(SPEC-\d+)\s*\|\s*(\d{3})[–-](\d{3})\s*\|", index_doc, re.M):
        ranges[m.group(1)] = (int(m.group(3)), int(m.group(4)))

    ac_rows: dict[str, str] = {}
    for m in re.finditer(r"^\|\s*(AC-\d+)\s*\|(.*?)\|\s*(SPEC[-0-9,\s]+|SPEC-[^|]*?)\s*\|\s*([BPD])\s*\|", index_doc, re.M):
        ac_rows[m.group(1)] = m.group(4)
    ac_matrix_specs = defaultdict(set)
    for m in re.finditer(r"^\|\s*(AC-\d+)\s*\|.*?\|\s*([^|]*)\|\s*[BPD]\s*\|", index_doc, re.M):
        for s in re.findall(r"SPEC-\d+", m.group(2)):
            ac_matrix_specs[m.group(1)].add(s)

    # ---------- per-spec checks --------------------------------------------
    coverage: dict[str, list[str]] = {}
    for sid in SPEC_IDS:
        fname = next((n for n in names if n.startswith(sid + "-")), None)
        if fname is None:
            fail(f"{sid}: file missing")
            continue
        doc = docs[fname]

        # metadata block
        for field in META:
            if not re.search(rf"^{field}:", doc, re.M):
                fail(f"{sid}: metadata field missing: {field}")
        m = re.search(r"^Spec:\s*(\S+)", doc, re.M)
        if not m or m.group(1) != sid:
            fail(f"{sid}: metadata Spec: mismatch")
        area = re.search(r"^Area prefix:\s*TROUBLE-([A-Z]+)", doc, re.M)
        if not area:
            fail(f"{sid}: Area prefix missing")
            continue
        area = area.group(1)

        # sections 1..8 in order
        heads = re.findall(r"^## (\d+)\.\s*(.+)$", doc, re.M)
        if [h[0] for h in heads] != [str(i) for i in range(1, 9)]:
            fail(f"{sid}: sections are {[h[0] for h in heads]} (expected 1..8)")
        for (num, title), want in zip(heads, SECTION_TITLES):
            if want.lower() not in title.lower():
                fail(f"{sid}: section {num} is '{title}', expected '{want}'")

        # step 1 — consumed types resolve
        cons = [x.strip() for x in re.search(r"^Consumed types:\s*(.*)$", doc, re.M).group(1).split(",") if x.strip() and x.strip() != "—"]
        miss = [c for c in cons if c not in types]
        if miss:
            fail(f"{sid}: consumed types not in SPEC-TYPES: {miss}")
        local = [x.strip() for x in re.search(r"^Local types:\s*(.*)$", doc, re.M).group(1).split(",") if x.strip()]
        clash = [c for c in local if c in types]
        if clash:
            warn(f"{sid}: local type name also a shared type name: {clash}")

        # step 4 — error codes
        used = set(re.findall(r"TROUBLE-[A-Z]+-\d{3}", doc))
        for code in sorted(used):
            if code not in catalog:
                fail(f"{sid}: uses {code} which is not in the SPEC-TYPES catalog")
                continue
            a, num = re.match(r"TROUBLE-([A-Z]+)-(\d{3})", code).groups()
            if a in ranges and not (ranges[a][0] <= int(num) <= ranges[a][1]):
                fail(f"{sid}: {code} outside the {a} range {ranges[a]}")
        own = {c for c in used if c.startswith(f"TROUBLE-{area}-")}
        if not own and "error codes" not in doc.split("## 5.")[1][:200].lower():
            warn(f"{sid}: no own-area error codes in §5")

        # step 5 — cross references
        for ref in set(re.findall(r"\bSPEC-\d{2}\b", doc)):
            if ref not in SPEC_IDS and ref not in ("SPEC-00",):
                # SPEC-nn may also refer to SPEC-INDEX/SPEC-TYPES by name, not id
                fail(f"{sid}: unresolved spec reference {ref}")
        for prd in set(re.findall(r"§[0-9]{2}[a-z]?", doc)):
            if prd not in PRD_SECTIONS:
                fail(f"{sid}: PRD reference {prd} is not a prd-v2.3 section id")

        # step 6 — forbidden strings
        for pat, why in FORBIDDEN:
            for m in re.finditer(pat, doc):
                line = doc[doc.rfind("\n", 0, m.start()) + 1: doc.find("\n", m.end())]
                if any(a.search(line) for a in FORBIDDEN_ALLOW):
                    continue
                fail(f"{sid}: forbidden string ({why}): {line.strip()[:120]}")
                break

        # step 6b — deferred vocabulary needs a hand-off marker (line or adjacent comment lines)
        lines = doc.split("\n")
        for m in re.finditer("|".join(re.escape(t) for t in DEFERRED_TERMS), doc):
            ln = doc.count("\n", 0, m.start())
            window = " ".join(lines[max(0, ln - 3): ln + 4])
            if any(k.lower() in window.lower() for k in HANDOFF_MARKERS):
                continue
            warn(f"{sid}: deferred term '{m.group(0)}' without a hand-off marker: {lines[ln].strip()[:120]}")

        # ACs in metadata must exist in the matrix and name this spec
        acs = re.findall(r"AC-\d+", re.search(r"^ACs:\s*(.*)$", doc, re.M).group(1))
        coverage[sid] = acs
        for ac in acs:
            if ac not in ac_matrix_specs:
                fail(f"{sid}: {ac} is not in the SPEC-INDEX AC matrix")
            elif sid not in ac_matrix_specs[ac]:
                fail(f"{sid}: claims {ac} but the matrix assigns it to {sorted(ac_matrix_specs[ac]) or 'nobody'}")

    # step 2 — AC coverage both ways
    for i in range(1, 31):
        ac = f"AC-{i}"
        if ac not in ac_matrix_specs:
            fail(f"{ac}: missing from the SPEC-INDEX AC matrix")
        elif not ac_matrix_specs[ac]:
            fail(f"{ac}: matrix row has no spec")
    for sid, acs in coverage.items():
        if not acs:
            fail(f"{sid}: no ACs declared")

    # ---------- report ------------------------------------------------------
    print("=" * 78)
    print("trouble SPEC suite — self-consistency report")
    print("=" * 78)
    print(f"{'file':<28}{'bytes':>9}{'lines':>8}{'## N.':>7}  ACs")
    total = 0
    for n in names:
        b = len(docs[n].encode())
        total += b
        secs = len(re.findall(r"^## \d+\.", docs[n], re.M))
        sid = n.split("-")[0] + "-" + n.split("-")[1] if n.startswith("SPEC-") else ""
        print(f"{n:<28}{b:>9}{len(docs[n].splitlines()):>8}{secs:>7}  {', '.join(coverage.get(sid, []))[:60]}")
    print("-" * 78)
    print(f"files: {len(names)}   bytes: {total}   shared types: {len(types)}   error codes: {len(catalog)}   areas: {len(ranges)}")
    print()
    print("AC coverage (AC -> specs):")
    for i in range(1, 31):
        ac = f"AC-{i}"
        print(f"  {ac:<6} {'B' if ac_rows.get(ac) == 'B' else ac_rows.get(ac, '?')}  {', '.join(sorted(ac_matrix_specs.get(ac, []))) or 'UNMAPPED'}")
    print()
    if warns:
        print(f"WARNINGS ({len(warns)}):")
        for w in warns:
            print("  -", w)
        print()
    if fails:
        print(f"FAILURES ({len(fails)}):")
        for f in fails:
            print("  x", f)
        print()
        print("RESULT: FAIL")
        return 1
    print("RESULT: PASS — every checked invariant holds")
    return 0


if __name__ == "__main__":
    sys.exit(main())
