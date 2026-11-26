#!/usr/bin/env python3
"""qa_ledger_append: canonical safe appender for the QA ledger JSONL.

Appends ONE record as exactly one newline-terminated compact JSON line
with an fsync, so per-line readers (qa_discover.py, ledger reports) never
lose it and the next appender never glues onto a torn last line.

Usage:
  qa_ledger_append.py <store.jsonl> <record.json>     # record file: one JSON value
  qa_ledger_append.py <store.jsonl> -                 # record JSON on stdin

Exits 0 after printing: appended_line=<n> bytes=<b> record=<one-line summary>.
"""
import json
import os
import sys


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: qa_ledger_append.py <store.jsonl> <record.json|->")
    store, src = sys.argv[1], sys.argv[2]
    raw = sys.stdin.read() if src == "-" else open(src, "rb").read().decode("utf-8")
    try:
        obj = json.loads(raw)
    except json.JSONDecodeError as exc:
        sys.exit(f"REFUSE: record is not valid JSON: {exc}")

    line = json.dumps(obj, separators=(",", ":")) + "\n"
    existed = os.path.exists(store)
    payload = line.encode("utf-8")
    if existed:
        with open(store, "rb") as f:
            size = os.fstat(f.fileno()).st_size
            last = b""
            if size:
                f.seek(size - 1)
                last = f.read(1)
        if last and last != b"\n":
            # store's last append omitted its newline: terminate it first so
            # this record lands on its OWN physical line (never glued)
            payload = b"\n" + payload
    # 'a' never truncates; O_APPEND writes land atomically for small lines
    fd = os.open(store, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
    try:
        os.write(fd, payload)
        os.fsync(fd)
    finally:
        os.close(fd)

    with open(store, "rb") as f:
        n_lines = sum(1 for l in f if l.strip())
    print(f"appended_to={store} created={not existed} "
          f"nonblank_lines={n_lines} record_tick={obj.get('tick') if isinstance(obj, dict) else '?'}")


if __name__ == "__main__":
    main()
