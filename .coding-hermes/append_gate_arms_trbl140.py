#!/usr/bin/env python3
"""TRBL-140: append ONE dispatchability GATE ARM line to 8 pending rows.

Each arm names a command that exists on the harness path this row targets,
plus a bare expect/exit outcome word. Original criteria are preserved
verbatim above the appended arm (the row's own AC requires it).
QA-TROUBLE-17-R4 is out of scope per the row's own text (now in_progress).
"""
import json
import subprocess

BOARD = "/home/kara/trouble/.coding-hermes/board/tasks.jsonl"
SCRIPT = "/home/kara/.hermes/scripts/bunker-qa.sh"

ARMS = {
    "TRBL-122": f"GATE ARM: Run `grep -n 'WAITING' {SCRIPT}`; expect exit 0 after the fix (0 matches today: the contended path prints neither LAUNCHED nor ERROR - that is the defect).",
    "QA-TROUBLE-REVIEW-1": f"GATE ARM: Run `grep -c 'CODING_HERMES_QA_TARGET' {SCRIPT}`; expect 1 or more after the fix (0 references today - that is the defect).",
    "QA-TROUBLE-DOCS-003": f"GATE ARM: Run `grep -n 'export DOCKER_HOST' {SCRIPT}`; expect at least one match after the fix (0 today: DOCKER_HOST only rides inline ci_cmd values - that is the defect).",
    "QA-TROUBLE-DOCS-004": f"GATE ARM: Run `bash -n {SCRIPT}`; expect exit 0 with the GOTOOLCHAIN=local pin replaced by the installed-toolchain PATH export per the outcomes above.",
    "QA-TROUBLE-DOCS-005": f"GATE ARM: Run `bash -n {SCRIPT}`; expect exit 0 with the chaos-corruption throwaway state root resolving outside /tmp per the outcomes above.",
    "QA-TROUBLE-26": f"GATE ARM: Run `grep 'BUNKER_QA_UI_PORTS' {SCRIPT} | grep 7644`; expect exit 0 after the fix (the default list at bunker-qa.sh:3465 omits 7644 today - that is the defect).",
    "QA-TROUBLE-27": f"GATE ARM: Run `bash -n {SCRIPT}`; expect exit 0 with the collect retry ladder (>=3 attempts over >=120s or a fallback transport) present per the outcomes above.",
    "QA-TROUBLE-28": "GATE ARM: Run `tail -c 1 /home/kara/.hermes/qa/ledger.jsonl | od -An -c`; expect the final byte to be a newline after the repair, and run the guard from the outcomes above against ledger.jsonl.pre-repair-20261115T092218Z; expect exit non-zero naming lines 759 and 766.",
}

rows = {}
with open(BOARD) as f:
    for line in f:
        try:
            r = json.loads(line)
        except json.JSONDecodeError:
            continue
        if r.get("id") in ARMS:
            rows[r["id"]] = r

for tid, arm in ARMS.items():
    row = rows.get(tid)
    if row is None:
        print(f"SKIP {tid}: no row")
        continue
    if row.get("status") == "in_progress":
        print(f"SKIP {tid}: in_progress (out of scope per TRBL-140)")
        continue
    orig = (row.get("acceptance_criteria") or "").rstrip()
    new = (orig + "\n\n" + arm) if orig else arm
    res = subprocess.run(
        ["boardctl", "-C", "/home/kara/trouble", "update", tid,
         "--acceptance-criteria", new],
        capture_output=True, text=True,
    )
    ok = "updated" in (res.stdout + res.stderr)
    print(f"{'OK  ' if ok else 'FAIL'} {tid}: {res.stdout.strip()[:60]} {res.stderr.strip()[:120]}")
