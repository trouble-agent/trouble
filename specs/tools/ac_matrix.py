#!/usr/bin/env python3
"""ac_matrix.py — the v0.1 exit gate: every AC in the SPEC-INDEX matrix against
the evidence actually present in the tree.

SPEC-INDEX §3.2 is the binding AC contract: 27 ACs, each mapped to one or more
owning specs, each marked B (built in v0.1), P (partial) or D (deferred). This
script answers, mechanically and without judgement:

  1. which ACs the index declares, and which specs own each one;
  2. where each AC is named in the tree (headers, comments, test function names);
  3. which owning spec has NO reference to its own ACs — the "silently dropped
     requirement" case the index exists to prevent;
  4. whether the tree contains a test that names each in-scope (B) AC.

It deliberately does not decide pass/fail: a test that names AC-16 is evidence of
attempted coverage, not proof of it. The judgement (run the named tests, read the
thresholds) is the reviewer's, and ac_matrix.py prints the exact test names to
run so that judgement is cheap.

Usage:
    python3 specs/tools/ac_matrix.py            # table
    python3 specs/tools/ac_matrix.py --json     # machine-readable
    python3 specs/tools/ac_matrix.py --ac AC-19 # one AC in detail
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
INDEX = os.path.join(REPO, "specs", "SPEC-INDEX.md")
SPECS = os.path.join(REPO, "specs")

AC_RE = re.compile(r"\bAC-(\d+)\b")
MATRIX_ROW_RE = re.compile(r"^\|\s*(AC-\d+)\s*\|(.*?)\|\s*([^|]*?)\s*\|\s*([BPD])\s*\|\s*$")
SPEC_FILE_RE = re.compile(r"^SPEC-(\d+|TYPES|INDEX)-")

SKIP_DIRS = {".git", "node_modules", ".worktrees", "bin", "vendor", ".zz-tmp"}


def parse_matrix() -> dict[str, dict]:
    """Return {AC-nn: {text, specs, status}} from SPEC-INDEX §3.2."""
    out: dict[str, dict] = {}
    with open(INDEX, encoding="utf-8") as fh:
        for line in fh:
            m = MATRIX_ROW_RE.match(line.rstrip("\n"))
            if not m:
                continue
            ac, text, specs, status = m.group(1), m.group(2).strip(), m.group(3), m.group(4)
            # The index writes owners as "SPEC-10 (visibility)" or "SPEC-05, SPEC-07":
            # keep the stems, drop the annotations.
            owners = sorted(set(re.findall(r"SPEC-(?:\d+|TYPES|INDEX)", specs)))
            out[ac] = {"text": text, "specs": owners, "status": status}
    return out


def parse_spec_ac_headers() -> dict[str, list[str]]:
    """Return {spec-stem: [AC-nn, ...]} from each spec's `ACs:` metadata line."""
    out: dict[str, list[str]] = {}
    for name in sorted(os.listdir(SPECS)):
        if not name.endswith(".md"):
            continue
        path = os.path.join(SPECS, name)
        with open(path, encoding="utf-8") as fh:
            for line in fh:
                if line.startswith("ACs:"):
                    out[name[:-3]] = [f"AC-{n}" for n in AC_RE.findall(line)]
                    break
    return out


def scan_tree() -> dict[str, list[dict]]:
    """Return {AC-nn: [{file, line, text, kind}]} for every reference in the tree."""
    hits: dict[str, list[dict]] = {}
    for root, dirs, files in os.walk(REPO):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        for name in files:
            if not name.endswith((".go", ".py", ".sh", ".md", ".tmpl", ".toml")):
                continue
            path = os.path.join(root, name)
            rel = os.path.relpath(path, REPO)
            # A test "names" an AC when the AC appears in the test function's own
            # comment block or body: attribution follows the most recent func Test
            # declaration seen within the previous 20 lines.
            is_test_file = rel.endswith("_test.go") or "/tests/" in rel or rel.endswith("_test.py")
            recent_test: tuple[int, str] | None = None
            try:
                with open(path, encoding="utf-8", errors="replace") as fh:
                    for i, line in enumerate(fh, 1):
                        stripped = line.strip()
                        if stripped.startswith("func Test"):
                            name = stripped.split("(")[0].replace("func ", "")
                            recent_test = (i, name)
                        elif recent_test is not None and i - recent_test[0] > 20:
                            recent_test = None
                        for ac in set(AC_RE.findall(line)):
                            key = f"AC-{ac}"
                            attrib = "" 
                            kind = "ref"
                            if is_test_file and recent_test is not None:
                                kind = "test"
                                attrib = recent_test[1]
                            hits.setdefault(key, []).append(
                                {"file": rel, "line": i, "text": stripped[:160], "kind": kind, "test": attrib}
                            )
            except OSError:
                continue
    return hits


def build_report() -> dict:
    matrix = parse_matrix()
    headers = parse_spec_ac_headers()
    hits = scan_tree()

    rows = []
    for ac in sorted(matrix, key=lambda a: int(a.split("-")[1])):
        owners = matrix[ac]["specs"]
        refs = hits.get(ac, [])
        tests = sorted({r.get("test", "") for r in refs if r["kind"] == "test" and r.get("test")})
        files = sorted({r["file"] for r in refs})
        owners_without_ref = [s for s in owners if s.startswith("SPEC-") and not any(s in r["file"] for r in refs)]
        rows.append(
            {
                "ac": ac,
                "status": matrix[ac]["status"],
                "specs": owners,
                "refs": len(refs),
                "files": files,
                "tests": tests,
                "owners_without_ref": owners_without_ref,
                "in_scope_evidence": bool(tests),
            }
        )

    # Specs that declare ACs but whose ACs the matrix assigns elsewhere.
    mismatches = []
    for stem, acs in headers.items():
        if stem.startswith("SPEC-INDEX") or stem.startswith("SPEC-TYPES"):
            continue  # the index owns the matrix and the types file the substrate
        stem_id = stem.split("-")[0] + "-" + (stem.split("-")[1] if len(stem.split("-")) > 1 else "")
        for ac in acs:
            if ac in matrix and stem_id not in matrix[ac]["specs"]:
                mismatches.append(f"{stem} declares {ac}, matrix assigns it to {', '.join(matrix[ac]['specs'])}")

    return {"rows": rows, "spec_headers": headers, "mismatches": mismatches}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--ac", default="")
    args = ap.parse_args()

    rep = build_report()
    if args.json:
        print(json.dumps(rep, indent=2))
        return 0

    rows = rep["rows"]
    if args.ac:
        sel = [r for r in rows if r["ac"] == args.ac]
        if not sel:
            print(f"{args.ac} is not in the SPEC-INDEX matrix", file=sys.stderr)
            return 1
        r = sel[0]
        print(f"{r['ac']} [{r['status']}] owners: {', '.join(r['specs'])}")
        print(f"references: {r['refs']} in {len(r['files'])} files")
        for f in r["files"]:
            print(f"  {f}")
        print(f"tests naming it ({len(r['tests'])}): {', '.join(r['tests']) if r['tests'] else '(none)'}")
        if r["owners_without_ref"]:
            print(f"owners with no reference anywhere: {', '.join(r['owners_without_ref'])}")
        return 0

    print(f"{'AC':6} {'st':2} {'refs':>4} {'tests':>5}  owners")
    print("-" * 78)
    for r in rows:
        print(f"{r['ac']:6} {r['status']:2} {r['refs']:>4} {len(r['tests']):>5}  {', '.join(r['specs'])}")

    built = [r for r in rows if r["status"] == "B"]
    no_test = [r["ac"] for r in built if not r["in_scope_evidence"]]
    orphan = sorted({r["ac"] for r in rows if r["refs"] == 0})
    print()
    print(f"index ACs: {len(rows)}  (B {len(built)}, P {sum(1 for r in rows if r['status']=='P')}, D {sum(1 for r in rows if r['status']=='D')})")
    print(f"B ACs with no test naming them: {', '.join(no_test) if no_test else '(none)'}")
    print(f"ACs referenced nowhere in the tree: {', '.join(orphan) if orphan else '(none)'}")
    if rep["mismatches"]:
        print("spec-declares-AC mismatches:")
        for m in rep["mismatches"]:
            print(f"  {m}")
    else:
        print("spec-declares-AC mismatches: none")

    # Gate semantics: an AC the tree never references is a hard failure (the
    # requirement was dropped on the floor); an AC without a test *named* after it
    # is a warning, because this suite names tests by behaviour and states the AC
    # in the test's doc comment — the reader's job is to confirm the mapping, and
    # the list above is printed for exactly that.
    return 1 if orphan else 0


if __name__ == "__main__":
    raise SystemExit(main())
