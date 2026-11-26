#!/usr/bin/env python3
"""One-shot repair of the pretty-printed JSONL block in the QA ledger.

Reads /home/kara/.hermes/qa/ledger.jsonl, takes a timestamped pre-repair
backup, re-serializes the physical-line block [START,END] (one pretty
object) as ONE compact newline-terminated line, atomically replaces the
store after re-verifying no concurrent append landed, and verifies the
object multiset is unchanged (sha256 of canonical dumps, sorted).

Prints fact lines only; exits non-zero on any verification failure.
"""
import hashlib
import json
import os
import sys
import time

STORE = "/home/kara/.hermes/qa/ledger.jsonl"
START = 995   # 1-indexed first physical line of the pretty block
END = 1020    # 1-indexed last physical line of the pretty block


def canon_fingerprints(objects):
    return sorted(hashlib.sha256(
        json.dumps(o, separators=(",", ":"), sort_keys=True).encode()
    ).hexdigest() for o in objects)


def main():
    st = os.stat(STORE)
    size0, mtime0 = st.st_size, st.st_mtime_ns

    with open(STORE, "rb") as f:
        data = f.read()

    backup = f"{STORE}.pre-repair-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}"
    with open(backup, "wb") as f:
        f.write(data)
    print(f"backup={backup} bytes={len(data)}")

    lines = data.splitlines(keepends=True)
    if len(lines) < END:
        sys.exit(f"ABORT: only {len(lines)} physical lines, block is [{START},{END}]")

    block = b"".join(lines[START - 1:END])
    try:
        obj = json.loads(block.decode("utf-8"))
    except Exception as exc:
        sys.exit(f"ABORT: block [{START},{END}] is not one JSON object: {exc}")
    ts = obj.get("ts") if isinstance(obj, dict) else None
    print(f"block_object ts={ts}")

    compact = json.dumps(obj, separators=(",", ":")) + "\n"
    rebuilt = b"".join(lines[:START - 1]) + compact.encode() + b"".join(lines[END:])

    # no concurrent append between snapshot read and replace
    st2 = os.stat(STORE)
    if st2.st_size != size0 or st2.st_mtime_ns != mtime0:
        sys.exit(f"ABORT: store changed during repair read "
                 f"(size {size0} -> {st2.st_size}); rerun from the new snapshot")
    if os.path.exists(rebuilt_path := STORE + ".repair.tmp"):
        os.unlink(rebuilt_path)
    with open(rebuilt_path, "wb") as f:
        f.write(rebuilt)
        f.flush()
        os.fsync(f.fileno())
    os.replace(rebuilt_path, STORE)
    print(f"replaced lines_before={len(lines)} lines_after={len(rebuilt.splitlines())}")

    # verify: per-line parse clean, multiset unchanged, trailing newline
    after = open(STORE, "rb").read()
    bad = []
    for n, raw in enumerate(after.split(b"\n"), start=1):
        if raw.strip():
            try:
                json.loads(raw.decode("utf-8"))
            except Exception as exc:
                bad.append((n, str(exc)[:80]))
    print(f"post_repair invalid_lines={len(bad)}")
    for n, err in bad[:10]:
        print(f"  line {n}: {err}")

    before_objs = []
    for raw in data.split(b"\n"):
        if raw.strip():
            before_objs.append(json.loads(raw.decode("utf-8")))
    after_objs = [json.loads(r.decode()) for r in after.split(b"\n") if r.strip()]
    fb, fa = canon_fingerprints(before_objs), canon_fingerprints(after_objs)
    print(f"objects_before={len(fb)} objects_after={len(fa)} multiset_equal={fb == fa}")
    print(f"final_byte_newline={after.endswith(b'newline_marker') if False else after[-1:] == b'\\n'}")

    ok = not bad and len(fb) == len(fa) and fb == fa and after.endswith(b"\n")
    print(f"repair_ok={ok}")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
