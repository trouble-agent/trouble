#!/usr/bin/env python3
"""Per-line JSON parse census of a JSONL store: prints fact lines only."""
import json, sys

path = sys.argv[1]
with open(path, "rb") as f:
    data = f.read()

total = 0
invalid = []
for lineno, raw in enumerate(data.split(b"\n"), start=1):
    if not raw.strip():
        continue
    total += 1
    try:
        json.loads(raw.decode("utf-8", "strict"))
    except Exception as exc:
        invalid.append((lineno, str(exc)[:100]))

print(f"store={path}")
print(f"valid_lines={total - len(invalid)}")
print(f"invalid_lines={len(invalid)}")
for lineno, err in invalid:
    print(f"invalid line={lineno} err={err}")
