#!/usr/bin/env bash
# cli_smoke.sh — live smoke of the two shipped binaries (SPEC-12 §2.1).
#
# Runs the real `trouble` and `troubled` against a throwaway state root and
# asserts the operator-visible contract: a stamped version, a redacted explain
# dump, the topology rows, a minted dashboard token whose plaintext is shown
# once, a checker that exits 0 against a live daemon and 8 once it is gone, an
# install audit that refuses an unstamped build without --force, and a refused
# escalate with no channels configured.
#
# Usage: tests/e2e/cli_smoke.sh [path-to-bin-dir]   (default: ./bin)
set -uo pipefail

BIN="${1:-./bin}"
TROUBLE="$BIN/trouble"
TROUBLED="$BIN/troubled"
FAIL=0

say()  { printf '\n== %s\n' "$*"; }
ok()   { printf '   ok   %s\n' "$*"; }
bad()  { printf '   FAIL %s\n' "$*"; FAIL=1; }

need() { [[ -x "$1" ]] || { echo "missing binary $1 (run make bin)"; exit 2; }; }
need "$TROUBLE"; need "$TROUBLED"

BASE="${HOME}/.local/state/trouble-test/cli-$$"
mkdir -p "$BASE"
chmod 700 "$BASE"
trap 'rm -rf "$BASE"' EXIT

PORT=$(python3 - <<'PY'
import socket
s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()
PY
)
INGEST=$((PORT+1))
CFG="$BASE/config.toml"
ENVF="$BASE/trouble.env"
: > "$ENVF"; chmod 600 "$ENVF"

cat > "$CFG" <<TOML
state_root = "$BASE"
[secrets]
environment_file = "$ENVF"
[lifecycle]
heartbeat_path = "$BASE/heartbeat.json"
heartbeat_interval = "2s"
heartbeat_stale_after = "10s"
drain_timeout = "10s"
[stall]
max_seq_age = "300s"
[checker]
interval = "2s"
confirm_runs = 2
state_file = "$BASE/checker.state.json"
alarm_file = "$BASE/checker.alarm"
[ingest]
bind = "127.0.0.1:$INGEST"
[dashboard]
bind = "127.0.0.1:$PORT"
token_file = "$BASE/dashboard-tokens.json"
[escalate]
channels = [["/bin/true"]]
TOML
chmod 600 "$CFG"

say "version triple is stamped"
V=$("$TROUBLE" --version)
echo "   $V"
[[ "$V" == *"stamped"* && "$V" != *"UNSTAMPED"* ]] && ok "stamped build" || bad "unstamped build: $V"

say "config explain --json carries provenance and redacts secrets"
"$TROUBLE" config explain --config "$CFG" --json > "$BASE/explain.json" 2>"$BASE/explain.err"
if [[ -s "$BASE/explain.json" ]] && grep -q '"source_ref"' "$BASE/explain.json"; then ok "explain dump has source_ref rows"; else bad "explain dump empty or missing provenance"; cat "$BASE/explain.err"; fi
grep -q '"key": "dashboard.bind"' "$BASE/explain.json" && ok "dashboard.bind is a resolved key" || bad "dashboard.bind missing from the dump"
grep -q '"key": "dashboard.mem_pressure_pct"' "$BASE/explain.json" && ok "the SPEC-10 §3.4 key set is resolved" || bad "dashboard.* key set incomplete"

say "topology prints the T1..T5 decisions"
"$TROUBLE" topology --config "$CFG" > "$BASE/topo.txt" 2>&1
grep -q 'T1→T2' "$BASE/topo.txt" && grep -q 'T4→T5' "$BASE/topo.txt" && ok "all topology rows present" || { bad "topology rows missing"; cat "$BASE/topo.txt"; }

say "install --check audits, writes nothing, and the stamp decides enablement"
"$TROUBLE" install --check --config "$CFG" --root "$BASE/units" >"$BASE/install.txt" 2>&1
RC=$?
[[ $RC -eq 0 ]] && ok "install --check passes on a stamped build" || { bad "install --check exit=$RC"; cat "$BASE/install.txt"; }
[[ -z "$(ls -A "$BASE/units" 2>/dev/null)" ]] && ok "check mode wrote no unit files" || bad "check mode wrote into the unit root"
grep -q "$CFG" "$BASE/install.txt" || true

say "an unstamped build is refused (TROUBLE-LIFECYCLE-006) unless --force"
go build -o "$BASE/trouble-unstamped" ./cmd/trouble 2>/dev/null
if [[ -x "$BASE/trouble-unstamped" ]]; then
  "$BASE/trouble-unstamped" install --check --config "$CFG" --root "$BASE/units2" >"$BASE/install-unstamped.txt" 2>&1
  RC=$?
  [[ $RC -eq 13 ]] && ok "unstamped install --check exits 13" || { bad "unstamped install exit=$RC, want 13"; cat "$BASE/install-unstamped.txt"; }
  grep -q 'TROUBLE-LIFECYCLE-006' "$BASE/install-unstamped.txt" && ok "refusal carries TROUBLE-LIFECYCLE-006" || { bad "006 missing"; cat "$BASE/install-unstamped.txt"; }
  "$BASE/trouble-unstamped" install --check --force --config "$CFG" --root "$BASE/units2" >"$BASE/install-forced.txt" 2>&1
  [[ $? -eq 0 ]] && ok "unstamped install --check --force passes" || { bad "--force did not override"; cat "$BASE/install-forced.txt"; }
  "$BASE/trouble-unstamped" --version | grep -q UNSTAMPED && ok "an unstamped binary says so" || bad "unstamped binary does not advertise it"
else
  bad "could not build an unstamped binary for the refusal test"
fi

say "escalate refuses with no channels configured"
cat >> "$CFG" <<'TOML'
TOML
sed -i 's|^channels = .*|channels = []|' "$CFG"
"$TROUBLE" escalate --unit trouble.service --config "$CFG" >"$BASE/esc.txt" 2>&1
RC=$?
[[ $RC -eq 13 ]] && ok "escalate with no channels exits 13" || bad "escalate exit=$RC, want 13"
grep -q 'TROUBLE-LIFECYCLE-016' "$BASE/esc.txt" && ok "refusal names the wiring hole (016)" || { bad "016 missing"; cat "$BASE/esc.txt"; }
sed -i 's|^channels = .*|channels = [["/bin/true"]]|' "$CFG"

say "dashboard token create prints the plaintext exactly once, list never does"
TOK=$("$TROUBLE" dashboard token create --config "$CFG" --label dash-read@cli --scopes read 2>"$BASE/tok.err")
if [[ "$TOK" == tdt_* && ${#TOK} -eq 47 ]]; then ok "minted a tdt_ token (47 chars)"; else bad "token shape wrong: ${TOK:0:12}…"; fi
"$TROUBLE" dashboard token list --config "$CFG" --json > "$BASE/tokens.json" 2>&1
grep -q 'dash-read@cli' "$BASE/tokens.json" && ok "list shows the label" || bad "list missing the label"
grep -q "$TOK" "$BASE/tokens.json" && bad "list leaked the plaintext" || ok "list carries no plaintext"
python3 - "$BASE/tokens.json" <<'PY' && ok "token file stores 32 bytes of sha256 hex, no plaintext" || bad "token hash shape wrong"
import json,sys
t=json.load(open(sys.argv[1]))[0]
assert len(t["hash"])==64, t["hash"]
assert "tdt_" not in json.dumps(t)
PY

printf 'TROUBLE_DASHBOARD_TOKEN=%s\n' "$TOK" > "$ENVF"
chmod 600 "$ENVF"

say "--output-env writes the 0600 env file itself (TRBL-028, shell-less image seeding)"
printf 'TROUBLE_HUB_TOKEN=sk_live_smoke\n' >> "$ENVF"
TOK2=$("$TROUBLE" dashboard token create --config "$CFG" --label dash-read@env --scopes read --output-env "$ENVF" 2>"$BASE/tok2.err")
[[ "$TOK2" == tdt_* ]] && ok "create --output-env minted and wrote the env line" || bad "create --output-env failed: $(cat "$BASE/tok2.err")"
grep -q "TROUBLE_DASHBOARD_TOKEN=$TOK2" "$ENVF" && ok "env file carries the fresh token" || bad "env file missing the fresh token"
grep -q "TROUBLE_DASHBOARD_TOKEN=$TOK$" "$ENVF" && bad "old token line survived" || ok "previous token line replaced"
grep -q 'TROUBLE_HUB_TOKEN=sk_live' "$ENVF" && ok "unrelated env lines preserved" || true
grep -q "^TROUBLE_DASHBOARD_TOKEN=" "$ENVF" && [[ $(grep -c "^TROUBLE_DASHBOARD_TOKEN=" "$ENVF") -eq 1 ]] && ok "exactly one token line" || bad "token line count wrong"
[[ "$(stat -c %a "$ENVF")" == 600 ]] && ok "env file mode 0600 after --output-env" || bad "env file mode drifted"
TOK3=$("$TROUBLE" dashboard token rotate --config "$CFG" --label dash-read@env --output-env "$ENVF" 2>"$BASE/tok3.err")
[[ "$TOK3" == tdt_* ]] && ok "rotate --output-env minted and wrote the env line" || bad "rotate --output-env failed: $(cat "$BASE/tok3.err")"
grep -q "TROUBLE_DASHBOARD_TOKEN=$TOK2\$" "$ENVF" && bad "rotate left the previous token in the env file" || ok "rotate replaced the previous token line"
grep -q "TROUBLE_DASHBOARD_TOKEN=$TOK3\$" "$ENVF" && ok "env file carries the rotated token" || bad "env file missing the rotated token"
[[ "$(stat -c %a "$ENVF")" == 600 ]] && ok "env file mode 0600 after rotate" || bad "env file mode drifted after rotate"
TOK2="" TOK3="" # only the LIVE token ($TOK) is used by the daemon checks below

say "daemon boots on the configured port and serves the health surface"
"$TROUBLED" --config "$CFG" > "$BASE/troubled.log" 2>&1 &
DPID=$!
for _ in $(seq 1 60); do
  code=$(curl -s -o "$BASE/health.json" -w '%{http_code}' "http://127.0.0.1:$PORT/health.json" || true)
  [[ "$code" == "200" ]] && break
  sleep 0.5
done
[[ "${code:-}" == "200" ]] && ok "GET /health.json = 200 on a loopback bind" || { bad "health.json never answered (code=$code)"; tail -20 "$BASE/troubled.log"; }
python3 - "$BASE/health.json" <<'PY' && ok "health parses into the frozen HealthResponse shape" || bad "health body is not a HealthResponse"
import json,sys
d=json.load(open(sys.argv[1]))
for k in ("status","version","git_sha","build_time","uptime_s","ledger_last_seq","ledger_last_ts","ledger_stall_s","sensors","sources","autonomy","breakers","runtime_watermarks"):
    assert k in d, k
assert d["ledger_last_seq"] > 0, d["ledger_last_seq"]
PY

say "the dashboard demands a token on a page route"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/")
[[ "$code" == "401" ]] && ok "anonymous GET / = 401" || bad "anonymous GET / = $code, want 401"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK" "http://127.0.0.1:$PORT/incidents")
[[ "$code" == "200" ]] && ok "Bearer read token renders /incidents" || bad "/incidents with a read token = $code"
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/?token=$TOK")
[[ "$code" == "400" ]] && ok "a URL-borne token is refused with 400" || bad "URL-borne token = $code, want 400"

say "check-stall exits 0 against the live daemon"
"$TROUBLE" check-stall --config "$CFG" > "$BASE/stall1.txt" 2>&1
RC=$?
[[ $RC -eq 0 ]] && ok "check-stall exit 0 (sequence advancing)" || { bad "check-stall exit=$RC"; cat "$BASE/stall1.txt"; }

say "check-stall exits 8 once the daemon is gone but its heartbeat is fresh"
kill -TERM "$DPID" 2>/dev/null
wait "$DPID" 2>/dev/null
"$TROUBLE" check-stall --config "$CFG" > "$BASE/stall2.txt" 2>&1
RC=$?
[[ $RC -eq 8 ]] && ok "check-stall exit 8 (liveness surface unreadable / heartbeat stale)" || { bad "check-stall exit=$RC, want 8"; cat "$BASE/stall2.txt"; }
grep -q 'TROUBLE-LIFECYCLE-008' "$BASE/stall2.txt" && ok "exit 8 carries TROUBLE-LIFECYCLE-008" || { bad "008 missing"; cat "$BASE/stall2.txt"; }

say "the daemon drained cleanly and recorded the shutdown heartbeat"
grep -q 'stage' "$BASE/heartbeat.json" 2>/dev/null || true
python3 - "$BASE/heartbeat.json" <<'PY' && ok "shutdown heartbeat written" || bad "no shutdown heartbeat"
import json,sys
d=json.load(open(sys.argv[1]))
assert d.get("stage")=="shutdown", d
PY
grep -qi 'boot refused' "$BASE/troubled.log" && bad "the daemon refused to boot" || ok "no boot refusal in the log"

say "the shipped DEFAULT token_file path works end to end (no [dashboard] token_file)"
# A second configuration that sets NO dashboard.token_file: the CLI and the
# daemon must both resolve the SPEC-10 §3.4 default
# ~/.config/trouble/dashboard-tokens.json — the exact shape whose broken
# resolution 401'd a freshly minted token (TROUBLE-DASHBOARD-002).
#
# That default lives under $HOME, so this case runs with HOME (and
# XDG_CONFIG_HOME, which os.UserConfigDir honours) pointed at a directory
# INSIDE $BASE: the operator's real store is never read or written, and the
# existing EXIT trap removes everything. Both variables are set as per-command
# prefixes only, so the outer shell's HOME is never modified.
DEFPORT=$(python3 - <<'PY'
import socket
s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()
PY
)
DEFINGEST=$((DEFPORT+1))
DEFCFG="$BASE/config-default.toml"
DEFHOME="$BASE/home"
DEFENVF="$BASE/default.env"
DEFSTORE="$DEFHOME/.config/trouble/dashboard-tokens.json"
mkdir -p "$DEFHOME" "$BASE/state-default"
chmod 700 "$BASE/state-default"
: > "$DEFENVF"; chmod 600 "$DEFENVF"

cat > "$DEFCFG" <<TOML
state_root = "$BASE/state-default"
[secrets]
environment_file = "$DEFENVF"
[lifecycle]
heartbeat_path = "$BASE/state-default/heartbeat.json"
heartbeat_interval = "2s"
heartbeat_stale_after = "10s"
drain_timeout = "10s"
[stall]
max_seq_age = "300s"
[checker]
interval = "2s"
confirm_runs = 2
state_file = "$BASE/state-default/checker.state.json"
alarm_file = "$BASE/state-default/checker.alarm"
[ingest]
bind = "127.0.0.1:$DEFINGEST"
[dashboard]
bind = "127.0.0.1:$DEFPORT"
[escalate]
channels = [["/bin/true"]]
TOML
chmod 600 "$DEFCFG"
grep -q 'token_file' "$DEFCFG" && bad "the default-config fixture must not set dashboard.token_file" || ok "no dashboard.token_file in the default config"

DEFTOK=$(HOME="$DEFHOME" XDG_CONFIG_HOME="$DEFHOME/.config" "$TROUBLE" dashboard token create --config "$DEFCFG" --label dash-read@default --scopes read 2>"$BASE/tok-default.err")
if [[ "$DEFTOK" == tdt_* && ${#DEFTOK} -eq 47 ]]; then ok "minted a tdt_ token against the default path"; else bad "default-path mint failed (${DEFTOK:0:12}…): $(cat "$BASE/tok-default.err")"; fi
if [[ -f "$DEFSTORE" ]]; then ok "the mint landed at \$HOME/.config/trouble/dashboard-tokens.json"; else bad "no store at the default path $DEFSTORE: $(cat "$BASE/tok-default.err")"; fi
[[ "$(stat -c '%a' "$DEFSTORE" 2>/dev/null)" == "600" ]] && ok "the default-path store is 0600" || bad "default-path store mode = $(stat -c '%a' "$DEFSTORE" 2>/dev/null), want 600"
if [[ -n "$DEFTOK" ]] && grep -q "$DEFTOK" "$DEFSTORE" 2>/dev/null; then bad "the default-path store leaked the plaintext"; else ok "the default-path store holds no plaintext"; fi

HOME="$DEFHOME" XDG_CONFIG_HOME="$DEFHOME/.config" "$TROUBLED" --config "$DEFCFG" > "$BASE/troubled-default.log" 2>&1 &
DPID2=$!
code=""
for _ in $(seq 1 60); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$DEFPORT/health.json" || true)
  [[ "$code" == "200" ]] && break
  sleep 0.5
done
[[ "$code" == "200" ]] && ok "the daemon boots on the default token_file" || { bad "default-config daemon never answered (code=$code)"; tail -20 "$BASE/troubled-default.log"; }
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$DEFPORT/incidents")
[[ "$code" == "401" ]] && ok "anonymous GET /incidents = 401 on the default config" || bad "anonymous /incidents = $code, want 401"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $DEFTOK" "http://127.0.0.1:$DEFPORT/incidents")
[[ "$code" == "200" ]] && ok "the default-path token authenticates: GET /incidents = 200" || { bad "GET /incidents with the default-path token = $code, want 200"; tail -20 "$BASE/troubled-default.log"; }
kill -TERM "$DPID2" 2>/dev/null
wait "$DPID2" 2>/dev/null

printf '\n'
if [[ $FAIL -eq 0 ]]; then echo "CLI SMOKE: all checks passed"; else echo "CLI SMOKE: FAILURES above"; fi
exit $FAIL
