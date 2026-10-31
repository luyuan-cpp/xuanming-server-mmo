#!/usr/bin/env bash
# Staging deployment helper for the HTTP /api/login migration.
#
# Why this exists:
#   The full deploy story is documented step-by-step in
#   docs/ops/linux-staging-stress-runbook.md §B, but copy-pasting that into
#   a tmux session is error-prone (wrong start order = login dialing
#   not-yet-listening player_locator, gate registering with stale etcd
#   leases, etc). This script encodes the exact tier-staged sequence so
#   a one-line invocation from staging /opt/mmorpg actually starts in
#   the right order with the right wait-for-ready gates between layers.
#
# Why not docker compose / k8s manifests:
#   The repo's existing manifests under deploy/k8s/ are the production
#   path. This script targets the in-between case: a single Linux box
#   running the dev binaries against docker-hosted infra, which is what
#   ops uses for the 1k/2k/5k stress runs (#221 in docs/notes/todo.md).
#   For real prod use the k8s manifests; for dry-runs / capacity tests
#   use this.
#
# Required PATH binaries:
#   - redis-cli        (for residue wipe + readiness check)
#   - etcdctl          (for snowflake compaction; optional but recommended)
#   - go               (only if --build is passed)
#   - mvn / java       (only if --build is passed AND --gateway is enabled)
#   - msbuild / cmake  (cpp build is out of scope; build cpp manually
#                       and drop binaries under bin/)
#
# Usage examples:
#   ./tools/scripts/deploy-staging.sh start
#   ./tools/scripts/deploy-staging.sh stop
#   ./tools/scripts/deploy-staging.sh restart
#   ./tools/scripts/deploy-staging.sh status
#   ./tools/scripts/deploy-staging.sh smoke           # one /api/login curl
#   ./tools/scripts/deploy-staging.sh stress 1000 2000 5000

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
LOG_DIR="${REPO_ROOT}/run/logs"
PID_DIR="${REPO_ROOT}/run/pids"
mkdir -p "${LOG_DIR}/go_services" "${LOG_DIR}/cpp_nodes" "${LOG_DIR}/java" "${PID_DIR}"

# ── Configuration ──────────────────────────────────────────────────
GATEWAY_HOST="${GATEWAY_HOST:-127.0.0.1}"
GATEWAY_PORT="${GATEWAY_PORT:-8081}"
LOGIN_GRPC="${LOGIN_GRPC:-127.0.0.1:50000}"

# Staged startup tier order — lower tier first, dependent tier waits for
# its dependency to LISTEN before booting. The numbers double as
# reasoning: "tier 1 = pure storage, tier 2 = stateless services that
# only need storage, tier 3 = services that depend on tier 2", etc.
declare -A SVC_TIER=(
  [db]=1
  [data_service]=1
  [player_locator]=2
  [scene_manager]=2
  [login]=3
)
# Listen-port mapping (matches go/<svc>/etc/<svc>.yaml). Used for
# wait-for-ready loops between tiers.
declare -A SVC_PORT=(
  [db]=6000
  [data_service]=9000
  [player_locator]=50200
  [scene_manager]=60300
  [login]=50000
)

declare -a TIER_ORDER=(db data_service player_locator scene_manager login)

# ── Helpers ────────────────────────────────────────────────────────
log() { printf '[deploy-staging %s] %s\n' "$(date +%H:%M:%S)" "$*"; }
die() { log "ERROR: $*"; exit 1; }

wait_for_port() {
  local port=$1 timeout=${2:-30}
  local elapsed=0
  while ! ss -ltn 2>/dev/null | grep -qE "[:.]$port[[:space:]]"; do
    sleep 1
    elapsed=$((elapsed + 1))
    [[ $elapsed -ge $timeout ]] && return 1
  done
  return 0
}

is_port_listen() {
  ss -ltn 2>/dev/null | grep -qE "[:.]$1[[:space:]]"
}

# C++ 节点启动后的存活检查(2026-10-18)。节点在 Linux 上会把 RLIMIT_NOFILE 软限自抬到硬限,
# 硬限低于需求(gate = GateMaxConnections + 4096,scene = 4096)时拒绝启动,stderr 一行
# "FATAL open file limit config: ..."。不查的话脚本照样打印 all services up,原因只落在
# run/logs/cpp_nodes/<node>.log 里。先认拒启行(原因更具体),再看进程是否还在。
# die 时本次已起的 Go 服务(以及 gate 拒启时的 scene)仍在后台;cmd_start 不查已有 pid,直接重跑 start 会让
# 新实例绑端口失败、wait_for_port 却被旧进程满足、pid 文件被改写成已退出的 pid,之后 stop 找不到旧进程。
# 所以 die 文案要求先 stop 再重试。
check_cpp_node_started() {
  local node=$1
  local logfile="${LOG_DIR}/cpp_nodes/${node}.log"
  local pid; pid=$(cat "${PID_DIR}/${node}.pid")
  local refusal
  refusal=$(grep -m1 'FATAL open file limit config:' "$logfile" 2>/dev/null || true)
  if [[ -n "$refusal" ]]; then
    die "$node refused to start: ${refusal} -- raise the hard nofile limit of this shell (docs/ops/linux-staging-stress-runbook.md A.1), or export NODE_OPEN_FILE_LIMIT_MODE=warn for a temporary exemption; services started so far are still running, run $0 stop before retrying"
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    die "$node (pid $pid) exited during startup; check ${logfile} and ${REPO_ROOT}/bin/logs/cpp_nodes/${node}*; services started so far are still running, run $0 stop before retrying"
  fi
  # 打出 fd 限值行(以及豁免生效时的 WARN 行),便于确认实际软限。
  grep '\[open_file_limit\]' "$logfile" 2>/dev/null | while read -r line; do log "  $node: $line"; done || true
}

# ── Subcommands ────────────────────────────────────────────────────
cmd_start() {
  log "checking docker infra (redis/etcd/mysql/kafka)..."
  for c in redis etcd mysql kafka; do
    if ! docker ps --format '{{.Names}}' | grep -qx "$c"; then
      die "$c container not running (run docker compose up -d first)"
    fi
  done

  log "wiping redis residue (player_locator/login_session/etc) for clean rollout..."
  # player:gate_index:* 是 player_locator 按 gate 建的会话索引与就绪标记 / 代次(与会话同生同灭):
  # 只删会话不删索引会留下脏成员与幽灵 gate,下次启动后由对账补建重建。
  # player:leases:* 是按 slot 分桶的断线租约,player:locator_shard:* 是分片登记与段租约,同样与会话同生同灭。
  # REDIS_PLAYER_NODES:玩家域在 Redis Cluster 上时,写集群全部节点的 host:port(空格分隔)。SCAN 只看本节点,所以逐个主节点扫;
  # 副本跳过(它的 SCAN 列出的键,DEL 回 MOVED)。
  for p in 'player:session:*' 'player:gate_index:*' 'player:leases:*' 'player:locator_shard:*' 'player:*:location' 'player_to_account:*' \
           'access_token:*' 'refresh_token:*' 'refresh_family:*' 'account_refresh:*' \
           'login_session:*' 'login:sessfence:*' 'login_account_sessions:*' \
           'login_step:*' 'account_lock:*' 'player_locker:*'; do
    redis-cli --scan --pattern "$p" 2>/dev/null \
      | while read -r k; do [[ -n "$k" ]] && redis-cli DEL "$k" >/dev/null; done
    for node in ${REDIS_PLAYER_NODES:-}; do
      node_host="${node%:*}"; node_port="${node##*:}"
      [[ "$(redis-cli -h "$node_host" -p "$node_port" --raw role 2>/dev/null | head -n 1)" == "slave" ]] && continue
      redis-cli -h "$node_host" -p "$node_port" --scan --pattern "$p" 2>/dev/null \
        | while read -r k; do [[ -n "$k" ]] && redis-cli -h "$node_host" -p "$node_port" DEL "$k" >/dev/null; done
    done
  done

  for tier in 1 2 3; do
    log "starting tier $tier services..."
    for svc in "${TIER_ORDER[@]}"; do
      [[ "${SVC_TIER[$svc]}" != "$tier" ]] && continue
      local bin="${REPO_ROOT}/bin/go_services/${svc}"
      [[ ! -x "$bin" ]] && die "missing binary: $bin (run --build first)"
      local cfg="${REPO_ROOT}/go/${svc}/etc/${svc}.yaml"
      [[ ! -f "$cfg" ]] && die "missing config: $cfg"

      "$bin" -f "$cfg" \
        > "${LOG_DIR}/go_services/${svc}.log" 2>&1 &
      echo $! > "${PID_DIR}/${svc}.pid"
      log "  $svc (pid $(cat "${PID_DIR}/${svc}.pid"), port ${SVC_PORT[$svc]})"
    done

    log "waiting for tier $tier ports..."
    for svc in "${TIER_ORDER[@]}"; do
      [[ "${SVC_TIER[$svc]}" != "$tier" ]] && continue
      if ! wait_for_port "${SVC_PORT[$svc]}" 30; then
        die "$svc port ${SVC_PORT[$svc]} did not LISTEN within 30s; check ${LOG_DIR}/go_services/${svc}.log"
      fi
    done
  done

  log "starting cpp scene + gate (if binaries present)..."
  if [[ -x "${REPO_ROOT}/bin/scene" ]]; then
    # 节点按 cwd 读 etc/ 并写 logs/,必须从 bin/ 启动(同 linux-staging-stress-runbook.md B 第 4 步);
    # 从仓库根直接起会在 fd 检查之前就因找不到 etc/base_deploy_config.yaml 退出。exec 后 $! 就是节点 pid。
    (cd "${REPO_ROOT}/bin" && exec ./scene) > "${LOG_DIR}/cpp_nodes/scene.log" 2>&1 &
    echo $! > "${PID_DIR}/scene.pid"
    sleep 2
    check_cpp_node_started scene
  else
    log "  WARN: bin/scene missing — skipping (build cpp before re-running)"
  fi
  if [[ -x "${REPO_ROOT}/bin/gate" ]]; then
    (cd "${REPO_ROOT}/bin" && exec ./gate) > "${LOG_DIR}/cpp_nodes/gate.log" 2>&1 &
    echo $! > "${PID_DIR}/gate.pid"
    sleep 5
    check_cpp_node_started gate
  else
    log "  WARN: bin/gate missing — skipping"
  fi

  if [[ -f "${REPO_ROOT}/java/gateway_node/target/gateway-node-0.0.1-SNAPSHOT.jar" ]]; then
    log "starting Java Gateway on :${GATEWAY_PORT}..."
    java -jar "${REPO_ROOT}/java/gateway_node/target/gateway-node-0.0.1-SNAPSHOT.jar" \
      > "${LOG_DIR}/java/gateway.log" 2>&1 &
    echo $! > "${PID_DIR}/gateway.pid"
    if ! wait_for_port "$GATEWAY_PORT" 60; then
      die "gateway did not LISTEN on :$GATEWAY_PORT; check ${LOG_DIR}/java/gateway.log"
    fi
  else
    log "  WARN: gateway jar missing — run mvn package + spring-boot:repackage first"
  fi

  log "all services up. run: $0 status   |   $0 smoke"
}

cmd_stop() {
  log "stopping in reverse tier order..."
  for svc in gateway gate scene login scene_manager player_locator data_service db; do
    local pidfile="${PID_DIR}/${svc}.pid"
    if [[ -f "$pidfile" ]]; then
      local pid; pid=$(cat "$pidfile")
      if kill -0 "$pid" 2>/dev/null; then
        kill "$pid" 2>/dev/null || true
        log "  $svc (pid $pid) stopped"
      fi
      rm -f "$pidfile"
    fi
  done
}

cmd_restart() { cmd_stop; sleep 2; cmd_start; }

cmd_status() {
  printf '%-18s %-8s %-10s %s\n' SERVICE PORT PID STATUS
  for svc in "${TIER_ORDER[@]}" scene gate gateway; do
    local port="${SVC_PORT[$svc]:-}"
    [[ "$svc" == gateway ]] && port="$GATEWAY_PORT"
    local pid='-' status='-'
    if [[ -f "${PID_DIR}/${svc}.pid" ]]; then
      pid=$(cat "${PID_DIR}/${svc}.pid")
      kill -0 "$pid" 2>/dev/null && status='RUNNING' || status='DEAD-PID'
    fi
    if [[ -n "$port" ]] && is_port_listen "$port"; then
      [[ "$status" == '-' ]] && status='LISTENING(no-pidfile)'
    fi
    printf '%-18s %-8s %-10s %s\n' "$svc" "${port:-?}" "$pid" "$status"
  done
}

cmd_smoke() {
  log "smoke test: POST /api/login..."
  local resp
  resp=$(curl -fsS -m 5 -X POST "http://${GATEWAY_HOST}:${GATEWAY_PORT}/api/login" \
    -H 'Content-Type: application/json' \
    -d '{"zone_id":1,"account":"deploy_smoke","password":"123456"}' 2>&1) \
    || die "smoke failed: $resp"
  echo "$resp"
  if echo "$resp" | grep -q '"code":0'; then
    log "smoke OK: code=0, tokens issued"
  else
    die "smoke returned non-zero code: $resp"
  fi
}

cmd_stress() {
  shift  # drop the "stress" arg
  local script="${REPO_ROOT}/tools/scripts/stress-linux-tier.sh"
  [[ -x "$script" ]] || die "stress runner missing: $script"
  log "delegating to stress-linux-tier.sh $*"
  exec "$script" "$@"
}

# ── Dispatch ───────────────────────────────────────────────────────
case "${1:-status}" in
  start)   cmd_start ;;
  stop)    cmd_stop ;;
  restart) cmd_restart ;;
  status)  cmd_status ;;
  smoke)   cmd_smoke ;;
  stress)  cmd_stress "$@" ;;
  *)
    cat <<EOF
usage: $0 {start|stop|restart|status|smoke|stress [tiers...]}

  start          tier-staged startup of go services + cpp gate/scene + Java Gateway
  stop           reverse-order shutdown using PID files in run/pids/
  restart        stop + 2s + start
  status         table of service / port / pid / state (RUNNING|DEAD-PID|LISTENING)
  smoke          one curl to /api/login asserting code=0
  stress N M K   delegate to tools/scripts/stress-linux-tier.sh with the given tiers
EOF
    exit 2
    ;;
esac
