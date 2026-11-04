#!/usr/bin/env bash
#
# docs-check — every `trouble <verb>` named in the operator runbook must be a verb
# the shipped CLI actually dispatches (TRBL-128).
#
# Why: docs/operations.md used to tell an operator responding to an incident to
# run `trouble research status`, `trouble research outcomes <sig>` and
# `trouble research prompt <res_id>`, plus `trouble incidents`, `trouble registry
# policy`, the four `trouble flow …` subcommands, `trouble issues health` and two
# `trouble skills …` verbs. None of those exist in the dispatch table: following
# the runbook under pressure produced `unknown command "research"` and a usage
# dump. A document is part of the operator surface, so it is gated like one.
#
# What this asserts, per documented verb:
#   1. it has a usage line in `trouble --help` (the operator-visible surface), and
#   2. the binary does not answer it with `unknown command` — i.e. the verb is
#      dispatched, not merely advertised.
#
# The probe is `trouble <verb> --help`: every verb parses its own flags (or its
# own sub-verb) before doing anything, so the probe is side-effect free by
# construction — it never touches a state root, a unit, or a daemon.
#
# The verb list is extracted only from CODE in the document — fenced blocks and
# inline backtick spans — never from prose. Prose legitimately says "trouble
# sends the repo" or "trouble never runs git"; a command is what an operator
# pastes, and that is what has to work.
#
# Usage:
#   scripts/docs_check.sh                    # check docs/operations.md
#   DOCS_CHECK_DOC=other.md scripts/...      # check another page (used by the
#                                            # red proof, which must not edit
#                                            # the tracked document)
#   BIN=bin scripts/docs_check.sh            # reuse an already-built binary
#
# Exit: 0 every documented verb resolves · 1 a documented verb does not resolve
#       (every offender is named) · 2 the gate could not run (no document, no
#       binary, or the extractor matched nothing, which would make the gate
#       vacuous).
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DOC=${DOCS_CHECK_DOC:-$ROOT/docs/operations.md}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

if [ ! -f "$DOC" ]; then
	echo "docs-check: FAIL — no such document: $DOC" >&2
	exit 2
fi

# Build once, into the scratch dir: the gate must not clobber bin/ and does not
# care about the link-time stamps (`--help` is identical either way).
BIN=${BIN:-}
if [ -z "$BIN" ]; then
	BIN=$TMP/trouble
	if ! (cd "$ROOT" && go build -o "$BIN" ./cmd/trouble); then
		echo "docs-check: FAIL — could not build ./cmd/trouble" >&2
		exit 2
	fi
elif [ ! -x "$BIN" ]; then
	echo "docs-check: FAIL — BIN=$BIN is not executable" >&2
	exit 2
fi

# 1. The code surface of the document: fenced blocks verbatim, plus each inline
#    backtick span on a prose line.
awk '
	/^[[:space:]]*```/ { fence = !fence; if (!fence) next; next }
	fence { print; next }
	{
		line = $0
		while (match(line, /`[^`]+`/)) {
			print substr(line, RSTART + 1, RLENGTH - 2)
			line = substr(line, RSTART + RLENGTH)
		}
	}
' "$DOC" > "$TMP/code.txt"

# 2. `trouble <verb>` occurrences inside that code surface, one verb per line,
#    and only in COMMAND POSITION: start of a line, after a `$` prompt, or after
#    a shell operator (`|`, `&&`, `;`, `(`). Fenced shell blocks also carry
#    prose — `docker compose ps   # trouble turns healthy on the next run` is a
#    comment, not an instruction — and a runbook gate that flags English would
#    be turned off rather than fixed, which is the failure mode this gate exists
#    to prevent.
grep -oE '(^|[$|;&(][[:space:]]*)(bin/)?trouble[[:space:]]+[a-z][a-z0-9-]*' "$TMP/code.txt" \
	| sed -E 's/.*(bin\/)?trouble[[:space:]]+//' \
	| sort -u > "$TMP/verbs.txt"

VERBS=$(grep -c . "$TMP/verbs.txt" || true)
if [ "$VERBS" -eq 0 ]; then
	echo "docs-check: FAIL — extracted no documented verbs from $DOC;" >&2
	echo "            the extractor has drifted from the document's shape, which would" >&2
	echo "            make this gate vacuous rather than green." >&2
	exit 2
fi

# 3. The advertised surface, once.
if ! "$BIN" --help > "$TMP/help.txt" 2>&1; then
	echo "docs-check: FAIL — '$BIN --help' exited non-zero" >&2
	exit 2
fi

STATUS=0
while IFS= read -r verb; do
	[ -n "$verb" ] || continue

	# (a) operator-visible: a usage line for this verb in `trouble --help`.
	if ! grep -qE "^[[:space:]]*trouble[[:space:]]+${verb}([[:space:]]|\$)" "$TMP/help.txt"; then
		echo "docs-check: FAIL — $DOC names 'trouble $verb' but 'trouble --help' has no such verb" >&2
		STATUS=1
		continue
	fi

	# (b) dispatched: the binary must not answer it with `unknown command`.
	out=$("$BIN" "$verb" --help 2>&1)
	case "$out" in
	*unknown\ command*)
		echo "docs-check: FAIL — $DOC names 'trouble $verb' but the CLI rejects it:" >&2
		printf '            %s\n' "$(printf '%s\n' "$out" | head -1)" >&2
		STATUS=1
		continue
		;;
	esac

	echo "docs-check: ok    trouble $verb"
done < "$TMP/verbs.txt"

if [ "$STATUS" -ne 0 ]; then
	echo "docs-check: FAIL — $DOC documents at least one verb the CLI does not dispatch" >&2
	exit 1
fi

echo "docs-check: PASS — $VERBS documented verb(s) in $DOC all resolve"
