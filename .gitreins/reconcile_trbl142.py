#!/usr/bin/env python3
"""TRBL-142: reconcile stale gitreins in_progress records against board truth.

14 records are in_progress in .gitreins/tasks.yaml while the board marks the
work complete (10 rows) or the work shipped with no surviving board row
(4 orphans: TRBL-100, TRBL-101, TRBL-103, TRBL-REVIEW-CLOSE-ERR).
Marks each record complete WITHOUT firing a re-grade (their diffs landed long
ago; re-judging stale artifacts is the TRBL-135 defect).
"""
import shutil
import sys
from datetime import datetime, timezone

import yaml

PATH = "/home/kara/trouble/.gitreins/tasks.yaml"
BACKUP = PATH + ".bak-trbl142"
ORPHANS = {"TRBL-100", "TRBL-101", "TRBL-103", "TRBL-REVIEW-CLOSE-ERR"}
TARGETS = [
    "TRBL-059", "TRBL-061", "TRBL-082", "TRBL-100", "TRBL-101", "TRBL-103",
    "TRBL-REVIEW-CLOSE-ERR", "TRBL-115", "TRBL-116", "TRBL-118",
    "QA-TROUBLE-17-R2-R2", "TRBL-131", "QA-TROUBLE-029", "TRBL-137",
]

shutil.copy2(PATH, BACKUP)
with open(PATH) as f:
    data = yaml.safe_load(f)

now = datetime.now(timezone.utc).isoformat()
reconciled, skipped = [], []
for task in data.get("tasks", []):
    tid = task.get("id")
    if tid in TARGETS and task.get("status") == "in_progress":
        task["status"] = "complete"
        task["completed_at"] = now
        reconciled.append((tid, "orphan-shipped-work" if tid in ORPHANS else "board-complete"))
    elif tid in TARGETS:
        skipped.append((tid, task.get("status")))

with open(PATH, "w") as f:
    yaml.safe_dump(data, f, default_flow_style=False, width=100000, sort_keys=False)

print(f"reconciled {len(reconciled)}:")
for tid, kind in reconciled:
    print(f"  {tid} -> complete ({kind})")
print(f"skipped (not in_progress): {skipped}")
print(f"backup: {BACKUP}")
sys.exit(0 if len(reconciled) == 14 else 1)
