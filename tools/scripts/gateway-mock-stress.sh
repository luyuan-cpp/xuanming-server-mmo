#!/usr/bin/env bash
# Gateway-only mock stress: 1k/2k/5k tier on the HTTP path *without* cpp gate.
#
# Why this exists:
#   docs/notes/todo.md #221 (S) targets the full cpp gate + multi-broker
#   kafka stack on Linux staging. That requires hardware I don't have.
#   But the Java Gateway + go-zero login self-imposed ceiling is also
#   useful — if /api/login crests at e.g. 2k req/s on a single Java
#   process, that's a real number ops can compare with the cpp-gate-side
#   number when staging is up. Better than "we have no data point".
#
# What it does (and doesn't):
#   ✅ Bursts N concurrent POST /api/login requests for D seconds.
#   ✅ Counts 200 / 401 / 429 / network-fail by parsing curl exit codes.
#   ✅ Records min/max/avg/p50/p95/p99 latency.
#   ✅ Wipes redis between tiers so each tier sees decision=FirstLogin.
#   ✅ Reuses unique accounts per request — account_cooldown won't blanket-kill.
#   ❌ Does NOT exercise the cpp gate TCP path.
#   ❌ Does NOT need a real EnterGame flow — just measures
#      auth-resolve + token-issue throughput.
#
# Limitations vs the real stress runner (stress-linux-tier.sh):
#   - No connection reuse: each curl is a fresh process + fresh TCP +
#     fresh TLS-less handshake. This is the same penalty an unkeep-alive
#     SDK client pays, so the number is a conservative floor.
#   - No HTTP/2: each curl is HTTP/1.1.
#   - The number we get is a per-second snapshot, not a sustained run —
#     for sustained drift use stress-linux-tier.sh against real infra.
#
# Usage:
#   ./tools/scripts/gateway-mock-stress.sh                  # default 1000 2000 5000
#   ./tools/scripts/gateway-mock-stress.sh 100 500 1000
#   GATEWAY=http://127.0.0.1:8081 PER_TIER_SEC=20 ./tools/scripts/gateway-mock-stress.sh

set -euo pipefail

GATEWAY="${GATEWAY:-http://127.0.0.1:8081}"
PER_TIER_SEC="${PER_TIER_SEC:-15}"
TIERS=("$@")
[[ ${#TIERS[@]} -eq 0 ]] && TIERS=(1000 2000 5000)

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT_DIR="${REPO_ROOT}/run/logs/stress"
mkdir -p "${OUT_DIR}"
RESULTS="${OUT_DIR}/gateway-mock-$(date +%Y%m%d-%H%M%S).csv"
echo "tier_req_count,sent,ok_200,fail_401,queued_100,denied_429,curl_fail,avg_ms,p50_ms,p95_ms,p99_ms,max_ms,duration_s" > "${RESULTS}"

# ── helpers ────────────────────────────────────────────────────────
percentile() {
  # $1 = sorted-asc input via stdin, $2 = 50 / 95 / 99
  awk -v p="$2" 'BEGIN{n=0} {a[n++]=$1} END{
    if(n==0){print 0;exit}
    k=int((p/100)*(n-1)+0.5);
    if(k<0)k=0; if(k>=n)k=n-1;
    print a[k]
  }'
}

wipe_redis() {
  # Wipe the same keys the linux stress runner wipes so EnterGame stays
  # decision=FirstLogin and the limiter's account cooldown doesn't smear
  # results across tiers.
  for p in 'access_token:*' 'refresh_token:*' 'refresh_family:*' 'account_refresh:*' 'account_lock:*' \
           'account_data:*' 'rl:zone:*' 'rl:ip:*'; do
    docker exec redis redis-cli --scan --pattern "$p" 2>/dev/null \
      | while read -r k; do [[ -n "$k" ]] && docker exec redis redis-cli DEL "$k" >/dev/null; done
  done
}

one_request() {
  # Issues one /api/login. Prints "<status_code> <ms>" on stdout.
  # account is per-request unique so account_cooldown doesn't kick in.
  local idx="$1"
  local acct="gw_stress_$$_${idx}"
  local body
  body=$(printf '{"zone_id":1,"account":"%s","password":"x"}' "$acct")

  # -w prints status + total_time; -o discards body.
  # If curl itself fails (network), it returns nonzero exit and we
  # record that as "curl_fail".
  local out rc
  out=$(curl -s -o /dev/null -m 5 \
         -w '%{http_code} %{time_total}' \
         -X POST "${GATEWAY}/api/login" \
         -H 'Content-Type: application/json' \
         -d "$body" 2>/dev/null) && rc=0 || rc=$?

  if (( rc != 0 )); then
    echo "000 0"
    return
  fi
  # Convert seconds -> ms with shell arith via awk
  awk -v line="$out" 'BEGIN{
    split(line,a," ");
    printf "%s %d\n", a[1], a[2]*1000;
  }'
}
export -f one_request
export GATEWAY

# ── tier loop ──────────────────────────────────────────────────────
for n in "${TIERS[@]}"; do
  echo
  echo "================================================================"
  echo "  Tier n=$n  duration=${PER_TIER_SEC}s  gateway=${GATEWAY}"
  echo "================================================================"

  wipe_redis

  log="${OUT_DIR}/gateway-mock-tier-${n}.log"
  : > "$log"

  start=$(date +%s)
  end_target=$((start + PER_TIER_SEC))
  i=0
  # Spawn workers in batches: $n parallel curls, repeat until the window
  # closes. We use xargs -P for the parallel handling so we don't have
  # to manage a process pool by hand.
  while (( $(date +%s) < end_target )); do
    # Generate n indices, pipe through xargs which keeps n concurrent
    # one_request calls. Each call writes one line "code ms" to the log.
    seq "$i" $((i + n - 1)) | xargs -P "$n" -I{} bash -c 'one_request "$@"' _ {} >> "$log"
    i=$((i + n))
  done
  finish=$(date +%s)
  duration=$((finish - start))

  # Parse counts + latencies
  sent=$(wc -l < "$log" | tr -d ' ')
  ok_200=$(awk '$1=="200"' "$log" | wc -l | tr -d ' ')
  fail_401=$(awk '$1=="401"' "$log" | wc -l | tr -d ' ')
  queued_100=$(awk '$1=="100"' "$log" | wc -l | tr -d ' ')
  denied_429=$(awk '$1=="429"' "$log" | wc -l | tr -d ' ')
  curl_fail=$(awk '$1=="000"' "$log" | wc -l | tr -d ' ')

  # Latencies only from successful responses — failed reqs are 0ms
  # placeholder which would skew the average otherwise.
  tmp=$(mktemp)
  awk '$1=="200"{print $2}' "$log" | sort -n > "$tmp"
  count=$(wc -l < "$tmp" | tr -d ' ')
  if (( count > 0 )); then
    avg=$(awk '{s+=$1} END{printf "%.0f", s/NR}' "$tmp")
    p50=$(percentile - 50 < "$tmp")
    p95=$(percentile - 95 < "$tmp")
    p99=$(percentile - 99 < "$tmp")
    max=$(tail -1 "$tmp")
  else
    avg=0 p50=0 p95=0 p99=0 max=0
  fi
  rm -f "$tmp"

  printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
    "$n" "$sent" "$ok_200" "$fail_401" "$queued_100" "$denied_429" "$curl_fail" \
    "$avg" "$p50" "$p95" "$p99" "$max" "$duration" >> "${RESULTS}"

  echo "  tier $n:  sent=$sent ok=$ok_200 401=$fail_401 100=$queued_100 429=$denied_429 fail=$curl_fail"
  echo "           avg=${avg}ms p50=${p50}ms p95=${p95}ms p99=${p99}ms max=${max}ms duration=${duration}s"
done

echo
echo "================================================================"
echo "  Done — results: ${RESULTS}"
echo "================================================================"
column -ts, "${RESULTS}" 2>/dev/null || cat "${RESULTS}"

cat <<'EOF'

What to do with the numbers:
  - "ok_200" / duration_s = Gateway+login sustained req/s for this tier.
    Linux staging will beat this number (no Hyper-V port reservations,
    real keep-alive HTTP clients). Take this as a floor.
  - "429" climbing = Bucket4j limiter is kicking in. That's working
    as designed; tighten zone-rps in application.yaml to test queueing
    threshold separately.
  - "100" (QUEUEING) requires the wave schedule to be active. By default
    it's off, so this column should stay zero.
  - "p99 > 500ms" but "max < 1s" + 100% ok_200 = Gateway saturated but
    not failing. That's the ceiling number you want to record.
  - "curl_fail > 0%" = network/timeout, NOT a Gateway problem — usually
    means xargs spawned more processes than your shell + ulimit allows.
    Drop -P n and re-run.
EOF
