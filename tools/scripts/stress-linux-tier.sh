#!/usr/bin/env bash
# Linux staging stress runner — run against a fully-deployed cluster
# (Java Gateway + go-zero login + cpp gate + scene + docker infra).
#
# Why this is a separate script (and not a generic shell loop): the dev box
# (Windows) maxes out around 500 bots due to (a) Hyper-V port reservations
# 50000-50059 + 50160-50259, (b) single-broker docker kafka, (c) absent
# sysctl tuning. See docs/stress/stress-test-2026-05-http-login.md §三 and
# docs/ops/release-checklist.md #B-3 for the full constraints.
#
# Run on the staging gate host (where cpp gate listens). Pre-reqs:
#   - sysctl baseline applied (docs/ops/gate-kernel-tuning-runbook.md §二)
#   - ulimit -n >= 1048576 in the shell that runs robot
#   - kafka broker count >= 3 in staging
#   - the helper compiles robot.exe locally first (skip if you ship binary)
#
# Usage:
#   ./tools/scripts/stress-linux-tier.sh           # default tiers 1000 2000 5000
#   ./tools/scripts/stress-linux-tier.sh 100 500 1000 2000

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
ROBOT_DIR="${REPO_ROOT}/robot"
RESULTS_DIR="${REPO_ROOT}/run/logs/stress"
RESULTS_CSV="${RESULTS_DIR}/results-linux-$(date +%Y%m%d-%H%M%S).csv"
TIERS=("$@")
[[ ${#TIERS[@]} -eq 0 ]] && TIERS=(1000 2000 5000)

# Stick to dev's well-known smoke duration so cross-environment compares
# stay apples-to-apples. Bump to 120s for sustained-throughput tiers.
PER_TIER_SEC="${PER_TIER_SEC:-60}"

mkdir -p "${RESULTS_DIR}"
echo "robot_count,login_ok,login_fail,login_stuck,enter_ok,avg_login_ms,max_login_ms,recon_ok,duration_s" > "${RESULTS_CSV}"

# ── Build robot if missing ────────────────────────────────────────────
if [[ ! -x "${ROBOT_DIR}/robot.exe" && ! -x "${ROBOT_DIR}/robot" ]]; then
  echo "[build] robot binary..."
  ( cd "${ROBOT_DIR}" && go build -o robot . )
fi
ROBOT_BIN="${ROBOT_DIR}/robot"
[[ -x "${ROBOT_DIR}/robot.exe" ]] && ROBOT_BIN="${ROBOT_DIR}/robot.exe"

# ── Tier loop ─────────────────────────────────────────────────────────
for n in "${TIERS[@]}"; do
  echo ""
  echo "================================================================"
  echo "  Tier robot_count=${n}"
  echo "================================================================"

  # Wipe redis state between tiers so EnterGame stays decision=FirstLogin.
  # Failing this leaves residual sessions and skews avg_login (#B-1).
  echo "[wipe] redis test residue..."
  for p in 'player:session:*' 'player:*:location' 'player_to_account:*' \
           'access_token:*' 'refresh_token:*' 'login_session:*' \
           'login_step:*' 'account_data:*' 'account_lock:*' \
           'player_locker:*'; do
    redis-cli --scan --pattern "$p" 2>/dev/null \
      | while read -r k; do [[ -n "$k" ]] && redis-cli DEL "$k" >/dev/null; done
  done

  # Materialize a tier-specific config from the template.
  cfg="${ROBOT_DIR}/etc/robot.stress-${n}.yaml"
  sed "s/REPLACE_ME/${n}/" "${ROBOT_DIR}/etc/robot.stress-template.yaml" > "${cfg}"

  log="${RESULTS_DIR}/run-linux-${n}.log"
  start_epoch=$(date +%s)
  ( cd "${ROBOT_DIR}" && timeout "${PER_TIER_SEC}" "${ROBOT_BIN}" \
      -c "etc/robot.stress-${n}.yaml" > "${log}" 2>&1 ) || true
  end_epoch=$(date +%s)
  duration=$(( end_epoch - start_epoch ))

  # Last [stats N] line is the most representative end-of-window snapshot.
  last_stats="$(grep -E '\[stats ' "${log}" | tail -1 || true)"
  ok=$(echo    "${last_stats}" | grep -oE 'login_ok=[0-9]+'    | grep -oE '[0-9]+')
  fail=$(echo  "${last_stats}" | grep -oE 'login_fail=[0-9]+'  | grep -oE '[0-9]+')
  stuck=$(echo "${last_stats}" | grep -oE 'login_stuck=[0-9]+' | grep -oE '[0-9]+')
  eok=$(echo   "${last_stats}" | grep -oE 'enter_ok=[0-9]+'    | grep -oE '[0-9]+')
  avg=$(echo   "${last_stats}" | grep -oE 'avg_login=[0-9]+ms' | grep -oE '[0-9]+')
  mx=$(echo    "${last_stats}" | grep -oE 'max_login=[0-9]+ms' | grep -oE '[0-9]+')
  recon=$(echo "${last_stats}" | grep -oE 'recon_ok=[0-9]+'    | grep -oE '[0-9]+')

  echo "${n},${ok:-0},${fail:-0},${stuck:-0},${eok:-0},${avg:-0},${mx:-0},${recon:-0},${duration}" \
       >> "${RESULTS_CSV}"

  echo "[done] ${n}: ${last_stats}"
done

echo ""
echo "================================================================"
echo "  All tiers done — results: ${RESULTS_CSV}"
echo "================================================================"
column -ts, "${RESULTS_CSV}" 2>/dev/null || cat "${RESULTS_CSV}"

cat <<'EOF'

Next steps:
  - Append a row to docs/stress/stress-test-2026-05-http-login.md §二
    (new column: env=Linux-staging vs the existing Windows-dev row).
  - If avg_login climbs above ~150ms or max above ~1s in any tier,
    that's the first cliff: capture cpp gate %CPU, ListenOverflows,
    kafka lag in the same minute and grep for "Bound=" / "SynSent=".
  - tier 5000+: also watch ephemeral port exhaustion. Compare
    `ss -s` TIME_WAIT to ip_local_port_range; multi-source-IP work
    needed if it goes >50%.
EOF
