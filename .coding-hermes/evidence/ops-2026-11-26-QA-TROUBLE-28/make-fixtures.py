#!/usr/bin/env python3
"""Build synthetic JSONL fixtures for the line-guard proof."""
import json, pathlib

d = pathlib.Path("/home/kara/.hermes/cache/scratch/ops-2026-11-26")

# 1. healthy: compact one-object-per-line, trailing newline
ok = d / "fixture-healthy.jsonl"
recs = [{"tick": "t1", "n": 1}, {"tick": "t2", "n": 2}]
ok.write_text("".join(json.dumps(r, separators=(",", ":")) + "\n" for r in recs))

# 2. glued: line 2 holds TWO complete objects (append omitted newline),
#    and the file's final line has no trailing newline (precondition for
#    the NEXT glue).
g = d / "fixture-glued.jsonl"
with open(g, "wb") as f:
    f.write(b'{"tick":"a","n":1}\n')
    f.write(b'{"tick":"b","n":2}{"tick":"c","n":3}\n')  # two objects, one line
    f.write(b'{"tick":"d","n":4}')                       # no trailing newline

print(f"healthy={ok} glued={g}")
