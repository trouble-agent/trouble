#!/usr/bin/env python3
"""jsonl-line-guard: per-line JSONL integrity guard for any JSONL store.

Fails (exit 1, naming file + line numbers) when any physical line:
  - holds more than one JSON value (torn-append glue: an append omitted
    the trailing newline and the next writer glued onto the same line), or
  - does not parse as JSON (e.g. a writer appended json.dumps(indent=2)
    output, so one object spans many physical lines), or
  - the file's final byte is not a newline (the precondition for the
    next glue: the next append will concatenate onto the last line).

Exit 0 = every physical line is exactly one JSON value and the file
ends with a newline. Reusable for any JSONL store: pass one or more
paths (or a directory with --ext to check every matching file).

Usage:
  jsonl-line-guard.py <store.jsonl> [more.jsonl ...]
Options:
  --max-report N   cap printed offending lines per file (default 40)
"""
import argparse
import json
import sys


def check_store(path, max_report):
    """Return (problems, n_lines, n_objects) for one store."""
    with open(path, "rb") as fh:
        data = fh.read()

    problems = []
    n_objects = 0
    if data and not data.endswith(b"\n"):
        problems.append(
            "final byte is not a newline (next append will glue onto the last line)"
        )

    lineno = 0
    for lineno, raw in enumerate(data.split(b"\n"), start=1):
        if not raw.strip():
            continue  # tolerate blank lines between records
        n_values = 0
        decoder = json.JSONDecoder()
        text = raw.decode("utf-8", "strict")
        # raw_decode does not skip leading whitespace; a line may be an
        # indented fragment of a pretty-printed object (valid bare string).
        text = text.strip()
        pos = 0
        while True:
            try:
                _obj, end = decoder.raw_decode(text, pos)
            except json.JSONDecodeError as exc:
                if n_values == 0:
                    problems.append(
                        f"line {lineno}: does not parse ({exc.msg} at char {exc.pos})"
                    )
                else:
                    problems.append(
                        f"line {lineno}: holds {n_values} complete JSON values "
                        f"followed by unparseable remainder at char {exc.pos} "
                        "(torn append: prior write omitted the trailing newline)"
                    )
                break
            n_values += 1
            n_objects += 1
            pos = end
            tail = text[pos:].lstrip()
            if not tail:
                break
            if n_values == 1:
                # more data follows a complete value on the SAME physical
                # line: the prior append omitted its trailing newline
                problems.append(
                    f"line {lineno}: holds 2+ JSON values on one line "
                    "(torn append: prior write omitted the trailing newline)"
                )
        if len(problems) >= max_report:
            problems.append(f"... further findings suppressed (--max-report {max_report})")
            break
    return problems, lineno, n_objects


def main():
    ap = argparse.ArgumentParser(description="Per-line JSONL integrity guard")
    ap.add_argument("paths", nargs="+", help="JSONL store(s) to check")
    ap.add_argument("--max-report", type=int, default=40)
    args = ap.parse_args()

    failed = False
    for path in args.paths:
        try:
            problems, n_lines, n_objects = check_store(path, args.max_report)
        except (OSError, UnicodeDecodeError) as exc:
            print(f"GUARD FAIL {path}: unreadable: {exc}")
            failed = True
            continue
        if problems:
            failed = True
            print(f"GUARD FAIL {path}: {len(problems)} finding(s) over {n_lines} lines "
                  f"({n_objects} objects):")
            for problem in problems[: args.max_report]:
                print(f"  {problem}")
        else:
            print(f"GUARD PASS {path}: {n_lines} lines, {n_objects} objects, "
                  f"one JSON value per line, trailing newline present")

    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
