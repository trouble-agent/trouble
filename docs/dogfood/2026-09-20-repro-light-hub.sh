#!/usr/bin/env bash
# trouble — light-hub dogfood reproduction (2026-09-20).
#
# Boots redis + a light-hub instance on the 766x scratch ports, posts one event through the
# documented on-ramp, stops and starts redis while watching the SPEC-13 §4.3 health fields, then
# tears everything down. Writes ONLY under $HOME/.local/state/trouble-dogfood-repro and
# /tmp/dogfood-trouble; never touches the repo, ports 7643/7644, or the container stack.
#
# Usage: bash docs/dogfood/2026-09-20-repro-light-hub.sh [repo-root]
set -u
REPO="${1:-$(cd "$(dirname "$0")/../.." && pwd)}"
STATE="$HOME/.local/state/trouble-dogfood-repro"
WORK=/tmp/dogfood-trouble
REDIS=trbl-df-redis-repro
PORT=7699
H="http://127.0.0.1:7662/health.json"
ING="http://127.0.0.1:7661"
PK=0123456789abcdef0123456789abcdef

cleanup() {
  pkill -x troubled 2>/dev/null
  docker rm -f "$REDIS" >/dev/null 2>&1
  echo "[cleanup] daemon=$([ -z "$(pgrep -x troubled)" ] && echo stopped || echo RUNNING) redis=$([ -z "$(docker ps -q -f name=$REDIS)" ] && echo removed || echo RUNNING)"
}
trap cleanup EXIT

state() { curl -fsS "$H" -o "$WORK/h.json" 2>/dev/null && python3 -c "
import json;d=json.load(open('$WORK/h.json'));h=d.get('hub') or {};r=h.get('redis') or {}
print('  status=%s hub_degraded=%s reason=%s window=%s stream_len=%s pending=%s' % (d.get('status'),h.get('degraded'),r.get('degraded_reason'),r.get('dedup_window'),r.get('stream_len'),r.get('pending')))"; }

mkdir -p "$WORK" "$STATE" && chmod 0700 "$STATE"
cd "$REPO" || exit 1

echo "== build (stamped) =="
make bin 2>&1 | tail -1

echo "== redis (NO --rm: we stop and start it) =="
docker rm -f "$REDIS" >/dev/null 2>&1
docker run -d --name "$REDIS" -p "127.0.0.1:$PORT:6379" redis:7.4-alpine \
  redis-server --appendonly yes --maxmemory-policy noeviction >/dev/null
sleep 2
docker exec "$REDIS" redis-cli ping

cp deploy/container/config.light-hub.toml "$WORK/config.light-hub.toml"
sed -i "s|redis://redis:6379/0|redis://127.0.0.1:$PORT/0|" "$WORK/config.light-hub.toml"
cp examples/trouble.env "$WORK/trouble.env" && chmod 0600 "$WORK/trouble.env"
TROUBLE_DASHBOARD_TOKEN_FILE="$STATE/dashboard.token" TROUBLE_STATE_ROOT="$STATE" \
  ./bin/trouble dashboard token create --label repro --scopes read --output-env "$WORK/dash.env" >/dev/null 2>&1

echo "== boot light-hub on 766x =="
./bin/troubled --config "$WORK/config.light-hub.toml" --config_path "$WORK/config.light-hub.toml" \
  --state_root "$STATE" --secrets-environment_file "$WORK/trouble.env" \
  --dashboard-token_file "$STATE/dashboard.token" \
  --ingest-bind 127.0.0.1:7661 --dashboard-bind 127.0.0.1:7662 \
  --ingest-advertised_host trouble.example.net -v > "$WORK/repro.log" 2>&1 &
# wait for the HUB STANZA, not just the socket
for i in $(seq 1 45); do
  grep -q '"hub"' "$WORK/h.json" 2>/dev/null && break
  curl -fsS "$H" -o "$WORK/h.json" 2>/dev/null
  sleep 1
done
echo "  baseline:"; state

echo "== one real event through the on-ramp =="
curl -sS -w "  HTTP=%{http_code}\n" -X POST "$ING/api/1/event/?sentry_key=$PK" \
  -H 'Content-Type: application/json' --data '{"message":"repro event","level":"error","release":"0.1.1"}'
sleep 2
echo "  ledger pairs on one sig:"
grep -o '"kind":"\(event\|group\)","schema_version":1,"sig":"[^"]*"' "$STATE"/ledger/*.jsonl 2>/dev/null | tail -4

echo "== SPEC-13 §4.3: stop redis (verify it is OURS first) =="
docker ps -a --format '{{.Names}} | {{.Status}} | {{.Ports}}' | grep "$REDIS" | sed 's/^/  /'
docker stop "$REDIS" >/dev/null
sleep 5
state
curl -sS -D "$WORK/hdr" -o "$WORK/body" -w "  POST during outage: HTTP=%{http_code}\n" \
  -X POST "$ING/api/1/event/?sentry_key=$PK" -H 'Content-Type: application/json' \
  --data '{"message":"repro during outage","level":"error","release":"0.1.1"}'
grep -iE "retry-after|^HTTP/" "$WORK/hdr" | sed 's/^/    /'
echo "  (429 + Retry-After + zero new ledger records is the CONTRACT for require_redis=false)"

echo "== start redis again, no daemon restart =="
docker start "$REDIS" >/dev/null
for i in $(seq 1 30); do
  curl -fsS "$H" -o "$WORK/h.json" 2>/dev/null
  w=$(python3 -c "import json;d=json.load(open('$WORK/h.json'));print(d.get('status'),((d.get('hub') or {}).get('redis') or {}).get('dedup_window'))" 2>/dev/null)
  case "$w" in "ok redis") echo "  RECOVERED at t=$((i*2))s"; break;; esac
  sleep 2
done
state
curl -sS -w "  POST after recovery: HTTP=%{http_code}\n" -X POST "$ING/api/1/event/?sentry_key=$PK" \
  -H 'Content-Type: application/json' --data '{"message":"repro after recovery","level":"error","release":"0.1.1"}'
echo "  redis_restored records: $(grep -c redis_restored "$STATE"/ledger/*.jsonl 2>/dev/null)"
echo "  stream XLEN=$(docker exec "$REDIS" redis-cli XLEN trouble:ingest) pending=$(docker exec "$REDIS" redis-cli XPENDING trouble:ingest ledger-writers | head -1)"
echo "== done — cleanup runs on exit =="
