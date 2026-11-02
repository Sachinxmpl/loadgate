#!/usr/bin/env bash

# E5 - What does the health loop buy?
# One backend is "sick": it accepts connections but never responds in time (60s delay,
# past the 300ms response_header timeout set below). Compare the full health loop (passive eviction
# on) against active-only (passive disabled). Round-robin is used so the health loop is
# the ONLY thing that can route around the sick backend.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$ROOT/test/bench/results"
mkdir -p "$OUT"

GOBAL="$ROOT/bin/loadgate"
BACKEND="$ROOT/bin/testbackend"
LOADGEN="$ROOT/bin/loadgen"
REPORT="$ROOT/bin/report"

LISTEN="127.0.0.1:8080"
RATE="${RATE:-1000}"
DURATION="${DURATION:-30s}"
WARMUP="${WARMUP:-5s}"

echo "building binaries..."
go build -o "$GOBAL"   ./cmd/loadgate
go build -o "$BACKEND" ./testbackend
go build -o "$LOADGEN" ./test/loadgen
go build -o "$REPORT"  ./test/bench/report

pids=()
cleanup() { for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

wait_up() { for _ in $(seq 1 100); do curl -s -o /dev/null "$1" && return 0; sleep 0.1; done; echo "timeout $1" >&2; exit 1; }

# two healthy backends and one sick one (60s delay > the 300ms response_header timeout set below).
NAME=b1 PORT=9501 DELAY=5ms "$BACKEND" & pids+=($!)
NAME=b2 PORT=9502 DELAY=5ms "$BACKEND" & pids+=($!)
NAME=b3 PORT=9503 DELAY=60s "$BACKEND" & pids+=($!)
for p in 9501 9502 9503; do wait_up "http://127.0.0.1:$p/health"; done  # /health is fast even for the sick one

BAL_PID=""
run_balancer() { # label fall
  local cfg="$OUT/e5-$1.yaml"
  cat > "$cfg" <<EOF
mode: l7
listen: "$LISTEN"
balancer: round_robin
timeouts:
  response_header: 300ms
health:
  active:
    interval: 2s
    timeout: 500ms
    rise: 2
  passive:
    fall: $2
    cooldown: 10s
routes:
  - match: { path_prefix: "/" }
    pool: default
pools:
  default:
    - addr: "127.0.0.1:9501"
    - addr: "127.0.0.1:9502"
    - addr: "127.0.0.1:9503"
EOF
  "$GOBAL" run -c "$cfg" -metrics-addr 127.0.0.1:9095 -log-level error >/dev/null 2>&1 &
  BAL_PID=$!
  pids+=("$BAL_PID")
  wait_up "http://$LISTEN/"
}

stop_balancer() {
  kill "$BAL_PID" 2>/dev/null || true
  wait "$BAL_PID" 2>/dev/null || true
  sleep 0.5
}

measure() { # label
  "$LOADGEN" -target "http://$LISTEN/" -rate "$RATE" -duration "$WARMUP" -out /dev/null >/dev/null 2>&1 || true
  "$LOADGEN" -target "http://$LISTEN/" -rate "$RATE" -duration "$DURATION" -out "$OUT/e5-$1.csv"
}

# passive+active: fall=3 (normal). active-only: fall very high, so passive never evicts.
echo "E5: passive+active (fall=3) ..."
run_balancer passive_active 3
measure passive_active
stop_balancer

echo "E5: active-only (passive disabled) ..."
run_balancer active_only 1000000
measure active_only
stop_balancer

echo ""
{
  echo "health count ok% thr(req/s) p50 p90 p99 p999 mean max"
  "$REPORT" -label passive+active -in "$OUT/e5-passive_active.csv"
  "$REPORT" -label active-only    -in "$OUT/e5-active_only.csv"
} | { column -t 2>/dev/null || cat; } | tee "$OUT/e5.txt"
