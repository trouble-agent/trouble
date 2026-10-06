#!/usr/bin/env bash
# Repo-owned corruption/restart probe for the fleet chaos-corruption cell.
set -uo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
bin="$repo_root/bin/troubled"

if [[ ! -x "$bin" ]]; then
  if ! command -v go >/dev/null 2>&1; then
    printf 'corruption restart: skipped (Go toolchain unavailable; cannot build troubled)\n'
    exit 2
  fi
  if ! mkdir -p "$repo_root/bin" || ! (cd "$repo_root" && go build -o "$bin" ./cmd/troubled); then
    printf 'corruption restart: skipped (could not build bin/troubled in this environment)\n'
    exit 2
  fi
fi

if ! command -v mktemp >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
  printf 'corruption restart: skipped (mktemp/timeout utility unavailable)\n'
  exit 2
fi

tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/trouble-corruption.XXXXXX") || {
  printf 'corruption restart: skipped (could not create throwaway state root)\n'
  exit 2
}
state_root="$tmp_root/state"
config="$tmp_root/config.toml"
heartbeat="$state_root/heartbeat.json"

active_pid=""
cleanup() {
  if [[ -n "$active_pid" ]]; then
    if kill -0 "$active_pid" 2>/dev/null; then
      # setsid gives the daemon its own process group, so stop any descendants too.
      kill -TERM -- "-$active_pid" 2>/dev/null || kill -TERM "$active_pid" 2>/dev/null || true
      for _ in {1..20}; do
        kill -0 "$active_pid" 2>/dev/null || break
        sleep 0.1
      done
      kill -KILL -- "-$active_pid" 2>/dev/null || kill -KILL "$active_pid" 2>/dev/null || true
    fi
    wait "$active_pid" 2>/dev/null || true
    active_pid=""
  fi
  rm -rf "$tmp_root"
}
trap cleanup EXIT INT TERM

mkdir -m 0700 -p "$state_root" || {
  printf 'corruption restart: skipped (could not create throwaway state root)\\n'
  exit 2
}
chmod 0700 "$state_root" || {
  printf 'corruption restart: skipped (could not secure throwaway state root)\\n'
  exit 2
}
cat > "$config" <<EOF
# Minimal standalone config for the isolated QA corruption probe.
state_root = "$state_root"

[lifecycle]
heartbeat_interval = "1s"
idle_heartbeat_interval = "1s"

[fs]
forbidden_state_roots = []
EOF

start_daemon() {
  local log_path=$1
  if command -v setsid >/dev/null 2>&1; then
    setsid timeout --signal=TERM --kill-after=2s 75s "$bin" --config "$config" >"$log_path" 2>&1 &
  else
    timeout --signal=TERM --kill-after=2s 75s "$bin" --config "$config" >"$log_path" 2>&1 &
  fi
  active_pid=$!
}

daemon_alive() {
  local pid=$1 stat
  kill -0 "$pid" 2>/dev/null || return 1
  stat=$(ps -o stat= -p "$pid" 2>/dev/null) || return 1
  [[ -n "$stat" && "$stat" != Z* ]]
}

stop_daemon() {
  local pid=$active_pid
  [[ -n "$pid" ]] || return 0
  if daemon_alive "$pid"; then
    kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
    for _ in {1..30}; do
      daemon_alive "$pid" || break
      sleep 0.1
    done
    if daemon_alive "$pid"; then
      kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
    fi
  fi
  wait "$pid" 2>/dev/null
  active_pid=""
  return 0
}

# First prove this config and environment can boot before injecting corruption.
start_daemon "$tmp_root/initial.log"
boot_deadline=$((SECONDS + 25))
while (( SECONDS < boot_deadline )); do
  if [[ -s "$heartbeat" ]]; then break; fi
  if ! daemon_alive "$active_pid"; then
    wait "$active_pid" 2>/dev/null
    boot_rc=$?
    active_pid=""
    boot_detail=$(tail -n 1 "$tmp_root/initial.log" 2>/dev/null | tr '\n' ' ')
    printf 'corruption restart: skipped (throwaway daemon did not boot; rc=%s%s)\n' "$boot_rc" "${boot_detail:+: $boot_detail}"
    exit 2
  fi
  sleep 0.2
done
if [[ ! -s "$heartbeat" ]]; then
  printf 'corruption restart: skipped (throwaway daemon produced no heartbeat within 25s)\n'
  exit 2
fi

stop_daemon
if ! printf '%064d' 0 > "$heartbeat"; then
  printf 'corruption restart: skipped (could not truncate throwaway heartbeat)\n'
  exit 2
fi
truncated_size=$(wc -c < "$heartbeat" | tr -d '[:space:]')
if [[ "$truncated_size" != 64 ]]; then
  printf 'corruption restart: skipped (could not create 64-byte heartbeat fixture)\n'
  exit 2
fi

start_daemon "$tmp_root/restart.log"
restart_started=$SECONDS
restart_deadline=$((restart_started + 30))
while (( SECONDS < restart_deadline )); do
  if [[ -s "$heartbeat" ]] && (( $(wc -c < "$heartbeat") > 64 )); then
    printf 'corruption restart: recovered (rewrote heartbeat within %ss)\n' "$((SECONDS - restart_started))"
    exit 0
  fi
  if ! daemon_alive "$active_pid"; then
    wait "$active_pid" 2>/dev/null
    restart_rc=$?
    active_pid=""
    if (( restart_rc != 0 )); then
      printf 'corruption restart: clean refusal (rc=%s)\n' "$restart_rc"
      exit 0
    fi
    printf 'corruption restart: FAIL (daemon exited successfully without rewriting corrupted heartbeat)\n'
    exit 1
  fi
  sleep 0.2
done

printf 'corruption restart: FAIL (restart hung for 30s without rewriting heartbeat or refusing cleanly)\n'
exit 1
