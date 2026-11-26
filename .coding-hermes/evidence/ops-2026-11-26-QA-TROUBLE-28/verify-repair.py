#!/usr/bin/env python3
"""Verify the ledger repair: stream-parse the backup, line-parse the live
store, compare canonical object multisets and record fact lines."""
import hashlib
import json
import sys
from pathlib import Path


def stream_objects(raw_bytes):
    dec = json.JSONDecoder()
    text = raw_bytes.decode("utf-8")
    idx, out = 0, []
    n = len(text)
    while True:
        while idx < n and text[idx] in " \t\r\n":
            idx += 1
        if idx >= n:
            break
        obj, idx = dec.raw_decode(text, idx)
        out.append(obj)
    return out


def canon(objs):
    return sorted(hashlib.sha256(
        json.dumps(o, separators=(",", ":"), sort_keys=True).encode()
    ).hexdigest() for o in objs)


backup = Path(sys.argv[1])
live = Path("/home/kara/.hermes/qa/ledger.jsonl")

before = stream_objects(backup.read_bytes())
after_raw = live.read_bytes()
after = []
bad = []
for n, line in enumerate(after_raw.split(b"\n"), 1):
    if not line.strip():
        continue
    try:
        after.append(json.loads(line.decode()))
    except Exception as exc:
        bad.append((n, str(exc)[:80]))

fb, fa = canon(before), canon(after)
print(f"objects_before={len(fb)} objects_after={len(fa)}")
print(f"multiset_equal={fb == fa}")
print(f"post_repair_invalid_lines={len(bad)}")
print(f"final_byte_newline={after_raw[-1:] == b'\n'}")
print(f"verify_ok={not bad and len(fb) == len(fa) and fb == fa and after_raw.endswith(b'\n')}")
sys.exit(0 if not bad and len(fb) == len(fa) and fb == fa and after_raw.endswith(b"\n") else 1)
