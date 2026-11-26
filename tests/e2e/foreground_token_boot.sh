#!/usr/bin/env bash
# foreground_token_boot.sh — TRBL-149: the canonical foreground quickstart's
# credential sequence, run fresh end to end (deploy/README.md "First foreground
# run").
#
# The measured defect this closes: a first foreground boot without a minted
# token store refuses the dashboard plane at boot (QA-TROUBLE-19) —
# /health.json reports status="degraded" with a subsystems[] row
# {"name":"dashboard","built":false,"refused":true,"code":"TROUBLE-LIFECYCLE-001"}
# and every dashboard route other than health answers 503 +
# TROUBLE-DASHBOARD-013. This script runs the documented sequence in order —
# config prep from examples/config.toml, mint BEFORE boot into the store the
# foreground config actually resolves (the state-root anchoring of TRBL-085:
# declared state_root, token_file unset → $STATE_ROOT/dashboard-tokens.json),
# boot, then asserts health carries NO refused subsystem row and the
# authenticated dashboard answers 200.
#
# Conventions (tests/e2e/):
#   * the daemon is booted ONLY through scratch_boot.sh (TRBL-042: it refuses
#     to start when a documented bind is already held and exec-replaces itself,
#     so killing the backgrounded pid kills the daemon);
#   * ports come from cli_smoke.sh's verified-free pair allocator (TRBL-083) —
#     never the documented 7643/7644, never 8642/8643/9090/3000;
#   * a fresh scratch HOME + state root is created and everything under it is
#     removed on exit; the daemon is killed on every path.
#
# Usage: tests/e2e/foreground_token_boot.sh [path-to-bin-dir]   (default: ./bin)
set -uo pipefail

BIN="${1:-./bin}"
TROUBLE="$BIN/trouble"
TROUBLED="$BIN/troubled"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
FAIL=0

say()  { printf '\n== %s\n' "$*"; }
ok()   { printf '   ok   %s\n' "$*"; }
bad()  { printf '   FAIL %s\n' "$*"; FAIL=1; }

need() { [[ -x "$1" ]] || { echo "missing binary $1 (run make bin)"; exit 2; }; }
need "$TROUBLE"; need "$TROUBLED"

# --- TRBL-083: verified-free PORT PAIR allocation (from cli_smoke.sh) --------
allocate_port_pair() {
  python3 - <<'PY'
import os, socket, sys, time
for attempt in range(1, 21):
    a = socket.socket()
    a.bind(("127.0.0.1", 0))
    base = a.getsockname()[1]
    b = socket.socket()
    try:
        b.bind(("127.0.0.1", base + 1))
    except OSError:
        b.close()
        a.close()
        continue  # port+1 taken: retry the WHOLE pair
    pid = os.fork()
    if pid == 0:
        devnull = os.open(os.devnull, os.O_RDWR)
        os.dup2(devnull, 0); os.dup2(devnull, 1); os.dup2(devnull, 2)
        try:
            while True:
                time.sleep(3600)
        finally:
            os._exit(0)
    print(base, base + 1, pid)
    sys.exit(0)
sys.exit(1)  # 20 attempts exhausted
PY
}
HOLD="" DAEMON_PID=""
release_pair() { [[ -n "${1:-}" ]] && { kill "$1" 2>/dev/null; wait "$1" 2>/dev/null; }; return 0; }

trap 'kill "$DAEMON_PID" "$HOLD" 2>/dev/null; wait "$DAEMON_PID" 2>/dev/null; rm -rf "$BASE"' EXIT

PAIR=$(allocate_port_pair) || { echo "could not allocate a free port pair after 20 attempts"; exit 2; }
read -r DASH_PORT INGEST_PORT HOLD <<< "$PAIR"
for p in "$DASH_PORT" "$INGEST_PORT"; do
  case "$p" in 7643|7644|8642|8643|9090|3000) echo "allocator handed a forbidden port ($p)"; exit 2;; esac
done

BASE="${HOME}/.local/state/trouble-test/fg-token-$$"
mkdir -p "$BASE"
chmod 700 "$BASE"

say "config prep: copy the shipped example and edit it into the scratch HOME"
CONFIG_DIR="$BASE/.config/trouble"
STATE_ROOT="$BASE/state"
CFG="$CONFIG_DIR/config.toml"
ENVF="$CONFIG_DIR/trouble.env"
mkdir -p "$CONFIG_DIR" "$STATE_ROOT"
chmod 700 "$STATE_ROOT"
cp "$REPO/examples/config.toml" "$CFG"
cp "$REPO/examples/trouble.env" "$ENVF"
chmod 600 "$CFG" "$ENVF"
# The documented edit: literal absolute paths (never ~ or shell variables in
# TOML) and the scratch port pair. Each shipped bind/port string is unique in
# the file, so the sed hits exactly the [ingest] and [dashboard] binds.
sed -i \
  -e "s|^state_root = .*|state_root = \"$STATE_ROOT\"|" \
  -e "s|^config_path = .*|config_path = \"$CFG\"|" \
  -e "s|^environment_file = .*|environment_file = \"$ENVF\"|" \
  -e "s|bind = \"127.0.0.1:7643\"|bind = \"127.0.0.1:$INGEST_PORT\"|" \
  -e "s|bind = \"127.0.0.1:7644\"|bind = \"127.0.0.1:$DASH_PORT\"|" \
  "$CFG"
grep -q "\"$STATE_ROOT\"" "$CFG" || { bad "state_root edit did not land"; }
grep -q "bind = \"127.0.0.1:$INGEST_PORT\"" "$CFG" || bad "ingest bind edit did not land"
grep -q "bind = \"127.0.0.1:$DASH_PORT\"" "$CFG" || bad "dashboard bind edit did not land"
if grep -q 'token_file' "$CFG"; then
  bad "the shipped example must leave dashboard.token_file unset (the mint follows the state-root anchoring)"
else
  ok "dashboard.token_file stays unset — the store anchors to the declared state_root (TRBL-085)"
fi

say "mint BEFORE boot, into the store the foreground config resolves"
MINT="$("$TROUBLE" dashboard token create \
  --config "$CFG" --label quickstart --scopes read \
  --output-env "$ENVF" 2>"$BASE/mint.err")" || { bad "mint failed: $(cat "$BASE/mint.err")"; exit 1; }
STORE="$STATE_ROOT/dashboard-tokens.json"
if [[ "$MINT" == tdt_* && ${#MINT} -eq 47 ]]; then
  ok "minted a tdt_ token (47 chars), printed exactly once on stdout"
else
  bad "token shape wrong: ${MINT:0:12}…"
fi
grep -q "dashboard token store: $STORE" "$BASE/mint.err" \
  && ok "the mint's notice named the state-root-anchored store the daemon will read" \
  || { bad "mint notice did not name $STORE"; cat "$BASE/mint.err"; }
[[ -f "$STORE" ]] || bad "no token store at the anchored path $STORE"
[[ "$(stat -c '%a' "$STORE" 2>/dev/null)" == "600" ]] \
  && ok "the token store is 0600" || bad "store mode = $(stat -c '%a' "$STORE" 2>/dev/null), want 600"
if grep -q "$MINT" "$STORE" 2>/dev/null; then
  bad "the token store leaked the plaintext"
else
  ok "the store holds digests only, no plaintext"
fi
[[ "$(stat -c '%a' "$ENVF")" == "600" ]] \
  && ok "--output-env wrote the env file at 0600" || bad "env file mode = $(stat -c '%a' "$ENVF"), want 600"
[[ $(grep -c "^TROUBLE_DASHBOARD_TOKEN=" "$ENVF") -eq 1 ]] \
  && ok "exactly one TROUBLE_DASHBOARD_TOKEN line in the env file" \
  || bad "token line count = $(grep -c '^TROUBLE_DASHBOARD_TOKEN=' "$ENVF"), want 1"
grep -q "^TROUBLE_DASHBOARD_TOKEN=$MINT\$" "$ENVF" \
  && ok "the env file carries the minted token" || bad "env file token line wrong"

say "boot through the scratch_boot wrapper (bind-guarded, exec-replaced)"
release_pair "$HOLD"   # TRBL-083: drop the reservation before the daemon listens
HOLD=""
SCRATCH_HOME="$BASE" "$REPO/tests/e2e/scratch_boot.sh" "$TROUBLED" "$CFG" \
  > "$BASE/daemon.log" 2>&1 &
DAEMON_PID=$!
code=""
for _ in $(seq 1 60); do
  code=$(curl -s -o "$BASE/health.json" -w '%{http_code}' "http://127.0.0.1:$DASH_PORT/health.json" || true)
  [[ "$code" == "200" ]] && break
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then break; fi
  sleep 0.5
done
[[ "$code" == "200" ]] && ok "GET /health.json = 200 on the scratch dashboard bind" \
  || { bad "health.json never answered (code=${code:-none})"; tail -30 "$BASE/daemon.log"; }

say "health is NOT dashboard-refused (the row's gate arm)"
python3 - "$BASE/health.json" <<'PY' && ok "no refused subsystem row; no dashboard row" || bad "health carries a refused subsystem (dashboard refused at boot)"
import json, sys
d = json.load(open(sys.argv[1]))
rows = d.get("subsystems") or []
refused = [r for r in rows if r.get("refused")]
assert not refused, {"refused_rows": refused, "status": d.get("status")}
dash = [r for r in rows if r.get("name") == "dashboard"]
assert not dash, {"dashboard_rows": dash}
PY

say "the authenticated dashboard returns 200; anonymous answers 401 (not the 503 refusal)"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$DASH_PORT/incidents")
[[ "$code" == "401" ]] && ok "anonymous GET /incidents = 401 (data plane up, auth demanded)" \
  || bad "anonymous GET /incidents = $code, want 401 (503 would be the QA-TROUBLE-19 refusal)"
code=$(curl -s -o "$BASE/incidents.html" -w '%{http_code}' \
  -H "Authorization: Bearer $MINT" "http://127.0.0.1:$DASH_PORT/incidents")
[[ "$code" == "200" ]] && ok "Bearer minted token renders /incidents = 200" \
  || { bad "GET /incidents with the minted token = $code, want 200"; tail -30 "$BASE/daemon.log"; }
[[ -s "$BASE/incidents.html" ]] && ok "the authenticated response has a body" || bad "empty authenticated response"

say "clean shutdown"
kill -TERM "$DAEMON_PID" 2>/dev/null
wait "$DAEMON_PID" 2>/dev/null
DAEMON_PID=""
grep -qi 'boot refused' "$BASE/daemon.log" && bad "the daemon refused to boot" || ok "no boot refusal in the daemon log"

printf '\n'
if [[ $FAIL -eq 0 ]]; then echo "FOREGROUND TOKEN BOOT: all checks passed"; else echo "FOREGROUND TOKEN BOOT: FAILURES above"; fi
exit $FAIL
