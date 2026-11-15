#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == run && "${2:-}" == list ]]; then
  printf 'true\n'
  exit 0
fi
exit 1
