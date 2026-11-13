"""从当前仓库配置准备独立 Docker 开发栈；只写忽略目录，不启动或构建容器。"""

from __future__ import annotations

import argparse
from contextlib import ExitStack
import copy
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import secrets
import shutil
import subprocess
import tempfile

import yaml


REPO = Path(__file__).resolve().parents[2]
GO_CONFIGS = {
    "login": "login.yaml",
    "db": "db.yaml",
    "data_service": "data_service.yaml",
    "player_locator": "player_locator.yaml",
    "scene_manager": "scene_manager_service.yaml",
    "client_rpc_router": "client_rpc_router.yaml",
    "match": "match_service.yaml",
    # 全球匹配成局的个人结算写进收件箱，scene 再从这里拉取。
    "player_inbox": "player_inbox.yaml",
}
SERVICE_IP_OFFSETS = {
    "login": 10, "db": 11, "data_service": 12, "player_locator": 13,
    "scene_manager": 14, "client_rpc_router": 15, "match": 16, "player_inbox": 17,
    "gate": 30, "scene": 31, "battle": 32,
}
# 观战两把密钥、存盘接口密钥、匹配链路四把密钥、账号目录四把调用方密钥和角色护栏九把密钥晚于已准备好的 .env 引入，也不写进任何数据卷，补生成不算轮换。
ADDITIVE_SECRET_NAMES = ("SPECTATE_TOKEN_SECRET", "MATCH_OPS_CALLER_SECRET", "PLAYER_STORE_SCENE_SECRET",
                         "MATCH_PROBE_SECRET", "RANK_PRODUCER_SECRET_MATCH", "MMORPG_MATCH_OUTCOME_SECRET_BATTLE", "MMORPG_PLAYER_INBOX_SECRET_MATCH",
                         "MMORPG_ACCOUNT_DIRECTORY_SECRET_LOGIN", "MMORPG_ACCOUNT_DIRECTORY_SECRET_TRADE",
                         "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE", "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER",
                         "MMORPG_ROLE_FENCE_SECRET_COORDINATOR", "MMORPG_ROLE_FENCE_SECRET_GUILD", "MMORPG_ROLE_FENCE_SECRET_TRADE",
                         "MMORPG_ROLE_FENCE_SECRET_MAIL", "MMORPG_ROLE_FENCE_SECRET_MATCH", "MMORPG_ROLE_FENCE_SECRET_PLAYER_INBOX",
                         "MMORPG_ROLE_FENCE_SECRET_FRIEND", "MMORPG_ROLE_FENCE_SECRET_CHAT", "MMORPG_ROLE_FENCE_SECRET_DB")
# gate 会话身份签名的开发密钥对（RFC 8032 §7.1 TEST 1 的公开测试向量，docs/design/gate-online-identity.md §10.1）：
# gate 用私钥签名，login 与在线服务只拿公钥。docker 调试栈只跑 dev 模式，这一对不是秘密；Python 标准库没有 Ed25519，
# 不在这里现生成。go/shared/callerauth 的 TestEd25519Rfc8032Vector 钉住它们配对。
DEV_INTERNAL_IDENTITY = {
    "INTERNAL_AUTH_SIGNING_KEY": "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60",
    "INTERNAL_AUTH_PUBLIC_KEYS": "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a",
}
SECRET_NAMES = (
    "MYSQL_ROOT_PASSWORD", "GATE_TOKEN_SECRET",
    "QUEUE_TOKEN_SECRET", "BATTLE_TOKEN_SECRET",
    "LOGIN_DEV_PASSWORD_SHARED_SECRET", "GATEWAY_ADMIN_API_KEY",
    *ADDITIVE_SECRET_NAMES,
)
# 角色护栏参与者（go/shared/rolefence 的 Participants）的 compose 服务名。协调者直连，端口取源配置。
ROLE_FENCE_PARTICIPANT_HOSTS = {"guild": "guild", "trade": "trade", "mail": "mail", "match": "match", "player_inbox": "player-inbox", "friend": "friend"}
# 与 C++ kSpectateBroadcastTopicPartitions（node_command_topic.h）相等；分区数是不可变契约，改数走换代号。
SPECTATE_BROADCAST_PARTITIONS = 16
# 世界频道广播 chat-world-bcast_g<N>：与 C++ kChatWorldBroadcastTopicPartitions、Go kafkacmd.ChatWorldBroadcastTopicPartitions 相等。
CHAT_WORLD_BROADCAST_PARTITIONS = 4
# 写者注册步骤（compose 的 sql-boot、<服务>-sql-boot）在容器里复用的脚本、角色模板和原件。按仓库相对路径复制：dot-source 和模板都按这个布局找。
SQL_BOOT_SOURCES = (
    "tools/scripts/lib/sql_boot_catalog_docker.ps1",
    "tools/scripts/lib/sql_boot_catalog_local.ps1",
    "tools/scripts/lib/release_common.ps1",
    "tools/scripts/maintenance_drain.ps1",
    "deploy/sql/authority_operator_role.sql",
    "deploy/sql/authority_runtime_role.sql",
    "deploy/mysql-init/gateway_tables.sql",
    "java/gateway_node/src/main/resources/application.yaml",
)
# 常驻写者的容器入口（compose 的 entrypoint）。与 K8s 模板同一做法：grant 摘要和 SQL 变量从注册步骤写的私有卷读出来再 exec，凭据不经 compose 环境。
# 部署身份和物理 authority 摘要在 K8s 上来自 Pod UID 和清单，这里同样由注册步骤写进卷。变量名由 writer_authority 按白名单核过。
SQL_WRITER_ENTRY = r"""#!/bin/sh
set -eu
root=/run/sql-writer
MMORPG_SQL_WRITER_GRANT_FILE="$root/grant/boot-grant.json"
MMORPG_SQL_WRITER_GRANT_SHA256="$(cat "$root/grant/boot-grant.json.sha256")"
MMORPG_SQL_WRITER_CONSUME_DIR="$root/consume"
MMORPG_SQL_DEPLOYMENT_ID="$(cat "$root/grant/deployment-id")"
MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256="$(cat "$root/grant/physical-authority-sha256")"
export MMORPG_SQL_WRITER_GRANT_FILE MMORPG_SQL_WRITER_GRANT_SHA256 MMORPG_SQL_WRITER_CONSUME_DIR MMORPG_SQL_DEPLOYMENT_ID MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256
for value_file in "$root"/env/*; do
  [ -f "$value_file" ] || continue
  export "${value_file##*/}=$(cat "$value_file")"
done
exec "$@"
"""


def load_yaml(relative: str) -> dict:
    value = yaml.safe_load((REPO / relative).read_text(encoding="utf-8-sig"))
    if not isinstance(value, dict):
        raise ValueError(f"源配置必须是映射: {relative}")
    return value


def write_yaml(path: Path, value: dict) -> None:
    path.write_text(yaml.safe_dump(value, allow_unicode=True, sort_keys=False), encoding="utf-8")


def container_addresses(value):
    """只转换已知本机基础设施地址，保留其余业务配置。"""
    if isinstance(value, dict):
        return {key: container_addresses(item) for key, item in value.items()}
    if isinstance(value, list):
        return [container_addresses(item) for item in value]
    if isinstance(value, str):
        replacements = {
            "127.0.0.1:2379": "etcd:2379", "http://127.0.0.1:2379": "http://etcd:2379",
            "127.0.0.1:6379": "redis:6379", "127.0.0.1:9092": "kafka:29092",
            "localhost:9092": "kafka:29092", "127.0.0.1:3306": "mysql:3306",
        }
        return replacements.get(value, value)
    return value


def read_environment(path: Path, *, generate_additive: bool = True) -> dict[str, str]:
    if not path.exists():
        return {name: secrets.token_hex(32) for name in SECRET_NAMES}
    values = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            key, value = line.split("=", 1)
            values[key] = value
    if generate_additive:
        for name in ADDITIVE_SECRET_NAMES:
            if name not in values:
                values[name] = secrets.token_hex(32)
    # 已有数据卷的口令不能因重新准备配置而悄悄轮换。
    checked = [name for name in SECRET_NAMES
               if generate_additive or name not in ADDITIVE_SECRET_NAMES or name in values]
    if any(not re.fullmatch(r"[0-9a-f]{64}", values.get(name, "")) for name in checked):
        raise ValueError("已有 .env 缺少有效密钥；保留原文件并人工核对，拒绝自动轮换")
    if len({values[name] for name in checked}) != len(checked):
        raise ValueError("已有 .env 不同用途复用了密钥，拒绝继续")
    return values


def check_subnet(subnet: ipaddress.IPv4Network, project: str) -> None:
    names = subprocess.run(["docker", "--context", "desktop-linux", "network", "ls", "-q"], check=True, capture_output=True, text=True).stdout.split()
    if not names:
        return
    networks = json.loads(subprocess.run(["docker", "--context", "desktop-linux", "network", "inspect", *names], check=True, capture_output=True, text=True).stdout)
    for network in networks:
        for config in network.get("IPAM", {}).get("Config") or []:
            current = config.get("Subnet")
            if not current or ":" in current:
                continue
            existing = ipaddress.ip_network(current, strict=False)
            if subnet.overlaps(existing):
                same_project = (network.get("Labels") or {}).get("com.docker.compose.project") == project
                if same_project and subnet == existing:
                    continue
                raise ValueError(f"开发子网与现有 Docker 网络重叠: {network['Name']} ({current})")


def topic_script(base: dict, data: dict, db: dict) -> str:
    commands = base["Kafka"]
    audit = data["Kafka"]
    # 审计 topic 的生产者是 go/db 的审计发件箱转发(docs/design/player-audit-outbox.md §7),消费者是 data_service:
    # 世代或基名不一致,流水与快照就写进没人消费的 topic,两边都不报错。C++ 不再直接发这两个 topic。
    outbox = db["AuditOutbox"]
    for key in ("TopicGeneration", "TransactionLogTopic", "SnapshotTopic"):
        if outbox[key] != audit[key]:
            raise ValueError(f"go/db AuditOutbox.{key} ({outbox[key]!r}) differs from data_service Kafka.{key} ({audit[key]!r})")
    groups = [audit["TransactionLogConsumerGroup"], audit["SnapshotConsumerGroup"]]
    if any(not isinstance(group, str) or not re.fullmatch(r"[A-Za-z0-9._-]+", group) for group in groups):
        raise ValueError("源配置包含无效 Kafka 审计消费组")
    groups = list(dict.fromkeys(groups))
    # 两个审计 topic 只许加分区、可以原地加(docs/design/audit-pipeline-horizontal-scale-20261106.md §2.A):
    # 落库按主键 / 唯一键去重、不依赖顺序。只核「不少于配置」,少了原地加;其余 topic 按分区寻址,分区数必须相等。
    tx_topic = f"{audit['TransactionLogTopic']}_g{audit['TopicGeneration']}"
    snapshot_topic = f"{audit['SnapshotTopic']}_g{audit['TopicGeneration']}"
    growable_topics = {tx_topic, snapshot_topic}
    topics = [
        (f"gate-cmd_g{commands['CommandTopicGeneration']}", commands["CommandTopicPartitions"], 3600000),
        (f"scene-cmd_g{commands['CommandTopicGeneration']}", commands["CommandTopicPartitions"], 3600000),
        (f"gate-spectate_g{commands['CommandTopicGeneration']}", commands["CommandTopicPartitions"], 600000),
        (f"spectate-bcast_g{commands['CommandTopicGeneration']}", SPECTATE_BROADCAST_PARTITIONS, 600000),
        (f"chat-world-bcast_g{commands['CommandTopicGeneration']}", CHAT_WORLD_BROADCAST_PARTITIONS, 600000),
        (tx_topic, audit["TransactionLogPartitions"], audit["RetentionMs"]),
        (snapshot_topic, audit["SnapshotPartitions"], audit["RetentionMs"]),
    ]
    script = r"""#!/bin/sh
set -eu
command -v timeout >/dev/null || { echo 'Kafka CLI timeout tool is missing' >&2; exit 1; }
cli_output=$(mktemp)
trap 'rm -f "$cli_output"' EXIT
trap 'exit 1' HUP INT TERM
now_seconds() {
  # /proc/uptime 是容器可见的单调时钟，宿主校时不能延长初始化预算。
  read -r elapsed ignored < /proc/uptime
  printf '%s\n' "${elapsed%%.*}"
}
run_cli() {
  cli_deadline="$1"; shift
  cli_remaining=$((cli_deadline - $(now_seconds)))
  [ "$cli_remaining" -gt 0 ] || return 124
  [ "$cli_remaining" -le 15 ] || cli_remaining=15
  # 单次调用最多 15 秒，且不能越过本阶段截止时间；TERM 后最多再给 1 秒退出。
  if timeout -k 1 "$cli_remaining" "$@" >"$cli_output" 2>&1; then
    return 0
  else
    return "$?"
  fi
}
fail_cli() {
  echo "$1" >&2
  cat "$cli_output" >&2
  exit 1
}
wait_metadata() {
  if [ "$(now_seconds)" -ge "$1" ]; then fail_cli "$2"; fi
  sleep 1
}
topic_missing() {
  # 只把创建确认后这一条明确的缺失视为传播延迟，查询超时、鉴权等错误直接失败。
  grep -Fq "Topic '$1' does not exist as expected" "$cli_output"
}
# Compose 已等待 broker 健康；这里再次确认连通，真实 CLI 错误不静默重试。
broker_deadline=$(($(now_seconds) + 15))
if ! run_cli "$broker_deadline" /opt/kafka/bin/kafka-broker-api-versions.sh --bootstrap-server kafka:29092; then
  fail_cli 'Kafka readiness check failed or timed out'
fi
ensure_topic() {
  topic="$1"; partitions="$2"; retention="$3"; grow="${4:-}"
  topic_deadline=$(($(now_seconds) + 60))
  if ! run_cli "$topic_deadline" /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:29092 --create --if-not-exists --topic "$topic" --partitions "$partitions" --replication-factor 1 --config "retention.ms=$retention"; then
    fail_cli "creating topic failed or timed out: $topic"
  fi
  cat "$cli_output"
  while :; do
    if run_cli "$topic_deadline" /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:29092 --describe --topic "$topic"; then
      actual=$(sed -n 's/.*PartitionCount:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$cli_output")
      if [ "$grow" = grow ]; then
        case "$actual" in ''|*[!0-9]*) fail_cli "cannot parse the partition count of topic $topic";; esac
        [ "$actual" -lt "$partitions" ] || break
        # data_service 的 EnsureTopics 可能同时在加:alter 失败不退出,回到上面重读分区数,由本阶段截止时间兜底。
        run_cli "$topic_deadline" /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:29092 --alter --topic "$topic" --partitions "$partitions" || true
        wait_metadata "$topic_deadline" "audit topic $topic still has fewer partitions than configured at the deadline: expected>=$partitions actual=$actual"
        continue
      fi
      [ "$actual" = "$partitions" ] || fail_cli "partition contract mismatch: $topic expected=$partitions actual=$actual"
      break
    fi
    if ! topic_missing "$topic"; then fail_cli "describing topic failed or timed out: $topic"; fi
    wait_metadata "$topic_deadline" "topic metadata still not visible at the deadline: $topic"
  done
  if ! run_cli "$topic_deadline" /opt/kafka/bin/kafka-configs.sh --bootstrap-server kafka:29092 --entity-type topics --entity-name "$topic" --alter --add-config "retention.ms=$retention"; then
    fail_cli "setting retention failed or timed out: $topic"
  fi
  while :; do
    if run_cli "$topic_deadline" /opt/kafka/bin/kafka-configs.sh --bootstrap-server kafka:29092 --entity-type topics --entity-name "$topic" --describe; then
      actual=$(sed -n 's/^[[:space:]]*retention\.ms=\([0-9][0-9]*\).*/\1/p' "$cli_output")
      case "$actual" in ''|*[!0-9]*) fail_cli "cannot parse the retention contract: $topic";; esac
      [ "$actual" != "$retention" ] || break
    elif ! topic_missing "$topic"; then
      fail_cli "describing retention failed or timed out: $topic"
    fi
    wait_metadata "$topic_deadline" "retention still violates the contract at the deadline: $topic expected=$retention actual=${actual:-unknown}"
  done
  echo "topic ready: $topic partitions=$partitions"
}
"""
    for name, partitions, retention in topics:
        if not re.fullmatch(r"[A-Za-z0-9._-]+", name) or int(partitions) <= 0 or int(retention) <= 0:
            raise ValueError("源配置包含无效 topic 契约")
        grow = " grow" if name in growable_topics else ""
        script += f"ensure_topic '{name}' {int(partitions)} {int(retention)}{grow}\n"
    script += r"""
# 冷 broker 的 API/业务 topic 就绪不代表消费组协调器已装载。
# DescribeGroup 经正常 FindCoordinator 让 broker 自行初始化内部偏移 topic；不加入组、不提交位点。
ensure_group_coordinator() {
  group="$1"
  while :; do
    status=0
    run_cli "$coordinator_deadline" /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server kafka:29092 --describe --group "$group" --state --timeout 10000 || status=$?
    case "$status" in 0|1) ;; *) fail_cli "consumer group coordinator query timed out or failed: $group (exit=$status)";; esac
    # Kafka CLI 某些错误只打印到输出而仍退出 0，必须一起检查实际语义。
    classification=0
    awk -v group="$group" '
      BEGIN { quote=sprintf("%c", 39) }
      {
        line=$0; sub(/^[[:space:]]+/, "", line); sub(/[[:space:]]+$/, "", line)
        if (line == "Error: Consumer group " quote group quote " does not exist." ||
            line == "Error: The consumer group " quote group quote " does not exist.") { missing=1; next }
        if (line ~ /org[.]apache[.]kafka[.]common[.]errors[.]GroupIdNotFoundException([:[:space:]]|$)/) { missing=1; next }
        if (line ~ /org[.]apache[.]kafka[.]common[.]errors[.](CoordinatorNotAvailableException|CoordinatorLoadInProgressException|NotCoordinatorException)([:[:space:]]|$)/) { pending=1; next }
        if ($1 == "GROUP" && $2 == "COORDINATOR" && $3 == "(ID)") { headers++; next }
        if ($1 == group) {
          rows++
          if ($2 !~ /:[0-9]+$/ || $3 !~ /^[(][0-9]+[)]$/ ||
              $(NF-1) !~ /^(Empty|Stable|PreparingRebalance|CompletingRebalance|Assigning|Reconciling)$/ || $NF !~ /^[0-9]+$/) bad=1
          next
        }
        if (line == "Consumer group " quote group quote " has no active members." ||
            line == "Warning: Consumer group " quote group quote " is rebalancing.") next
        if (line ~ /Error|ERROR|Exception/) bad=1
      }
      END {
        if (bad) exit 1
        if (pending) exit 2
        if (missing && (headers || rows)) exit 1
        if (missing || (headers == 1 && rows == 1)) exit 0
        exit 1
      }
    ' "$cli_output" || classification=$?
    if [ "$classification" -eq 0 ] && [ "$status" -eq 0 ]; then
      cat "$cli_output"
      echo "consumer group coordinator ready: $group"
      return
    fi
    if [ "$classification" -ne 2 ]; then fail_cli "consumer group coordinator query failed or returned an unknown format: $group (exit=$status)"; fi
    wait_metadata "$coordinator_deadline" "consumer group coordinator still not ready at the deadline: $group"
  done
}
ensure_offsets_leaders() {
  while :; do
    status=0
    run_cli "$coordinator_deadline" /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:29092 --describe --topic __consumer_offsets || status=$?
    if topic_missing __consumer_offsets; then
      # 已发出正规协调器请求后的明确传播延迟；不能混入其他错误。
      if grep -E 'Error|ERROR|Exception' "$cli_output" | grep -Fv "Topic '__consumer_offsets' does not exist as expected" >/dev/null; then
        fail_cli 'describing the internal offsets topic returned other errors'
      fi
      wait_metadata "$coordinator_deadline" 'internal offsets topic still not visible at the deadline'
      continue
    fi
    [ "$status" -eq 0 ] || fail_cli 'describing the internal offsets topic failed or timed out'
    classification=0
    awk '
      function ids(text, empty_ok, parts, count, i, seen) {
        if (text == "") return empty_ok
        count=split(text, parts, ",")
        for (i=1; i<=count; i++) {
          if (parts[i] !~ /^[0-9]+$/ || seen[parts[i]]++) return 0
        }
        return 1
      }
      function contains(text, wanted, parts, count, i) {
        count=split(text, parts, ",")
        for (i=1; i<=count; i++) if (parts[i]+0 == wanted+0) return 1
        return 0
      }
      NF == 0 { next }
      {
        topic=""; partition=""; leader=""; replicas=""; isr=""; count=""
        nt=np=nl=nr=ni=nc=0
        for (i=1; i<=NF; i++) {
          value=(i<NF && $(i+1) !~ /:$/) ? $(i+1) : ""
          if ($i == "Topic:") { topic=value; nt++ }
          if ($i == "PartitionCount:") { count=value; nc++ }
          if ($i == "Partition:") { partition=value; np++ }
          if ($i == "Leader:") { leader=value; nl++ }
          if ($i == "Replicas:") { replicas=value; nr++ }
          if ($i == "Isr:") { isr=value; ni++ }
        }
        if (nt != 1 || topic != "__consumer_offsets") { bad=1; next }
        if (nc) {
          if (nc != 1 || np || nl || nr || ni || count !~ /^[1-9][0-9]*$/) bad=1
          total=count+0; summaries++; next
        }
        if (np != 1 || nl != 1 || nr != 1 || ni != 1 || partition !~ /^[0-9]+$/ ||
            leader !~ /^(none|-1|[0-9]+)$/ || !ids(replicas, 0) || !ids(isr, 1)) { bad=1; next }
        if (seen[partition]++) bad=1
        rows++
        if (leader == "none" || leader == -1) pending=1
        else if (!contains(replicas, leader) || !contains(isr, leader)) pending=1
      }
      END {
        if (bad || summaries != 1 || rows != total) exit 1
        for (i=0; i<total; i++) if (!(i in seen) || seen[i] != 1) exit 1
        if (pending) exit 2
        exit 0
      }
    ' "$cli_output" || classification=$?
    if [ "$classification" -eq 0 ]; then
      cat "$cli_output"
      echo 'internal offsets topic: Leader/ISR ready on every partition'
      return
    fi
    if [ "$classification" -ne 2 ]; then fail_cli 'internal offsets topic: partition set, Leader/ISR or output format is not as expected'; fi
    wait_metadata "$coordinator_deadline" 'internal offsets topic still has partitions without a ready Leader/ISR at the deadline'
  done
}
# 两个真实组、内部偏移分区和最后复核共享最多 90 秒，不随调用次数增加总预算。
coordinator_deadline=$(($(now_seconds) + 90))
"""
    for group in groups:
        script += f"ensure_group_coordinator '{group}'\n"
    script += "ensure_offsets_leaders\n"
    for group in groups:
        script += f"ensure_group_coordinator '{group}'\n"
    script += '[ "$(now_seconds)" -lt "$coordinator_deadline" ] || fail_cli "consumer group cold-start readiness exceeded the total budget"\n'
    return script


def refresh_existing_options() -> tuple[argparse.Namespace, bytes]:
    """Read the canonical environment without printing values or writing files."""
    state = (REPO / "build/docker-debug-current").resolve()
    canonical = state / ".env"
    if not state.is_relative_to(REPO.resolve()) or not canonical.resolve().is_relative_to(state):
        raise ValueError("Canonical environment is outside the workspace")
    original = canonical.read_bytes()
    values = {}
    for line in original.decode("utf-8-sig").splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        key, separator, value = line.partition("=")
        if not separator or not re.fullmatch(r"[A-Z][A-Z0-9_]*", key) or key in values:
            raise ValueError("Invalid or duplicate canonical environment assignment")
        values[key] = value
    required = ("DEBUG_RUN_DIR", "COMPOSE_PROJECT_NAME", "DEBUG_SUBNET", "KAFKA_CLUSTER_ID",
                "GATEWAY_CLIENT_PORT", "GATEWAY_PORT", "GATE_PORT", "BATTLE_PORT")
    if any(not values.get(key) for key in required):
        raise ValueError("Canonical environment lacks required stack settings")
    run = Path(values["DEBUG_RUN_DIR"])
    if (not run.is_absolute() or not run.is_dir() or not run.resolve().is_relative_to(state)
            or not state.is_relative_to((REPO / "build").resolve())):
        raise ValueError("Existing runtime directory is outside the current stack")
    # Existing key validation and additive key generation stay in read_environment.
    environment = read_environment(canonical, generate_additive=False)
    identity_keys = [key in environment for key in DEV_INTERNAL_IDENTITY]
    if any(identity_keys) and not all(identity_keys):
        raise ValueError("Existing internal identity key pair is incomplete")
    if canonical.read_bytes() != original:
        raise ValueError("Canonical environment changed while reading")
    try:
        options = argparse.Namespace(output=str(run.resolve()), project=values["COMPOSE_PROJECT_NAME"],
            subnet=values["DEBUG_SUBNET"], gateway_client_port=int(values["GATEWAY_CLIENT_PORT"]),
            gateway_port=int(values["GATEWAY_PORT"]), gate_port=int(values["GATE_PORT"]),
            battle_port=int(values["BATTLE_PORT"]), refresh_existing=True, environment=environment)
    except ValueError:
        raise ValueError("Invalid existing stack port") from None
    return options, original


def source_hashes(paths: list[str]) -> dict[str, str]:
    return {path: hashlib.sha256((REPO / path).read_bytes()).hexdigest() for path in paths}


def publish_refresh(staged: Path, output: Path, original: bytes, source_paths: list[str], before: dict) -> None:
    """Publish only a fully rendered batch whose source and control inputs still match."""
    canonical = REPO / "build/docker-debug-current/.env"
    if source_hashes(source_paths) != before:
        raise ValueError("Source inputs changed during configuration refresh")
    if canonical.read_bytes() != original:
        raise ValueError("Canonical environment changed during configuration refresh")
    files = list(staged.rglob("*"))
    for source in files:
        if source.is_file():
            target = output / source.relative_to(staged)
            if not target.resolve().is_relative_to(output.resolve()):
                raise ValueError("Runtime output path escapes the current stack")
    publications = [(source, output / source.relative_to(staged)) for source in files
                    if source.is_file() and source.name not in (".env", "source-manifest.json")]
    runtime_count = len(publications)
    for directory in dict.fromkeys((output, canonical.parent)):
        for name in ("source-manifest.json", ".env"):
            publications.append((staged / name, directory / name))
    # Backups live beside the ignored runtime batch; keep them if recovery fails.
    recovery = Path(tempfile.mkdtemp(prefix=".refresh-recovery-", dir=output.parent))
    touched = []
    completed = set()
    backups = {}
    try:
        for index, (_, target) in enumerate(publications):
            if not target.resolve().is_relative_to(canonical.parent.resolve()):
                raise ValueError("Runtime output path escapes the current stack")
            backup = recovery / str(index)
            existed = target.exists()
            if existed:
                shutil.copyfile(target, backup)
            backups[target] = backup if existed else None
        for index, (source, target) in enumerate(publications):
            if index >= runtime_count:
                if canonical.read_bytes() != original:
                    raise ValueError("Canonical environment changed before refresh commit")
                if source_hashes(source_paths) != before:
                    raise ValueError("Source inputs changed before refresh commit")
            backup = backups[target]
            unchanged = (target.read_bytes() == backup.read_bytes()) if backup else not target.exists()
            if not unchanged:
                raise ValueError("Runtime file changed during configuration refresh")
            target.parent.mkdir(parents=True, exist_ok=True)
            touched.append((target, source))
            # Retain existing inodes: individual Docker file binds must see updates.
            shutil.copyfile(source, target)
            completed.add(target)
        if source_hashes(source_paths) != before:
            raise ValueError("Source inputs changed during refresh commit")
        if canonical.read_bytes() != (staged / ".env").read_bytes():
            raise ValueError("Canonical environment changed during refresh commit")
    except BaseException as error:
        failures = []
        for target, source in reversed(touched):
            try:
                backup = backups[target]
                # Do not undo somebody else's edit after our successful copy.
                if target in completed and target.read_bytes() != source.read_bytes():
                    if backup and target.read_bytes() == backup.read_bytes():
                        continue
                    raise ValueError("Concurrent runtime edit prevents automatic recovery")
                if backup:
                    shutil.copyfile(backup, target)
                else:
                    target.unlink(missing_ok=True)
            except BaseException:
                failures.append(str(target))
        if failures:
            receipt = recovery / "recovery.json"
            try:
                receipt.write_text(json.dumps({"status": "rollback-incomplete", "failedPaths": failures,
                    "backups": {str(target): str(backup) if backup else None
                                for target, backup in backups.items()}}, indent=2), encoding="utf-8")
            except OSError:
                raise RuntimeError("Configuration refresh rollback incomplete; receipt unavailable; "
                                   "private backups retained at: " + str(recovery)) from None
            raise RuntimeError("Configuration refresh rollback incomplete; private recovery receipt: "
                               + str(receipt)) from None
        shutil.rmtree(recovery)
        raise error
    else:
        shutil.rmtree(recovery)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default="build/docker-debug-current")
    parser.add_argument("--project", default="xuanming-debug-current")
    parser.add_argument("--subnet", default="172.29.220.0/24")
    parser.add_argument("--gateway-port", type=int, default=18081)
    parser.add_argument("--gateway-client-port", type=int, default=8081)
    parser.add_argument("--gate-port", type=int, default=28000)
    parser.add_argument("--battle-port", type=int, default=28010)
    parser.add_argument("--refresh-existing", action="store_true",
                        help="Refresh current runtime files while preserving canonical stack settings")
    args = parser.parse_args()
    original = None
    if args.refresh_existing:
        args, original = refresh_existing_options()
    with ExitStack() as stack:
        prepare(args, stack, original)


def prepare(args: argparse.Namespace, stack: ExitStack, original: bytes | None = None) -> None:
    output = (REPO / args.output).resolve()
    if not output.is_relative_to((REPO / "build").resolve()):
        raise ValueError("配置与密钥只能输出到仓库忽略的 build/ 子目录")
    ignored = subprocess.run(["git", "check-ignore", "--quiet", str(output / ".env")], cwd=REPO)
    if ignored.returncode != 0:
        raise ValueError("输出目录没有被 Git 忽略，拒绝写入密钥")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", args.project):
        raise ValueError("项目名必须由小写字母、数字、下划线或连字符组成")
    ports = (args.gateway_client_port, args.gateway_port, args.gate_port, args.battle_port)
    if len(set(ports)) != 4 or any(not 1024 <= port <= 65535 for port in ports):
        raise ValueError("四个宿主入口端口必须不同且在 1024..65535 内")
    subnet = ipaddress.ip_network(args.subnet, strict=True)
    if subnet.version != 4 or subnet.prefixlen != 24 or not subnet.is_private:
        raise ValueError("开发栈需要独立私有 IPv4 /24 子网")
    check_subnet(subnet, args.project)
    output.mkdir(parents=True, exist_ok=True)
    runtime_output = output
    if args.refresh_existing:
        environment = dict(args.environment)
        for name in ADDITIVE_SECRET_NAMES:
            if name not in environment:
                environment[name] = secrets.token_hex(32)
        output = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix=".refresh-", dir=output.parent)))
    else:
        environment = read_environment(output / ".env")
    environment.setdefault("KAFKA_CLUSTER_ID", secrets.token_urlsafe(16))
    # 旧版 .env 里共用的 HMAC 内部身份密钥已退役，换成开发密钥对。
    environment.pop("INTERNAL_AUTH_SECRET", None)
    if args.refresh_existing:
        for key, value in DEV_INTERNAL_IDENTITY.items():
            environment.setdefault(key, value)
    else:
        environment.update(DEV_INTERNAL_IDENTITY)
    config_dir = output / "config"
    sql_dir = output / "mysql-init"
    config_dir.mkdir(exist_ok=True)
    sql_dir.mkdir(exist_ok=True)
    source_paths = [f"go/{service}/etc/{filename}" for service, filename in GO_CONFIGS.items()]
    source_paths += ["bin/etc/base_deploy_config.yaml", "bin/etc/game_config.yaml",
                     "bin/etc/battle_rewards.json", "tools/scripts/prepare_docker_debug.py", *SQL_BOOT_SOURCES]
    # mysql init sources are copied too, so guard them against concurrent edits.
    guarded_paths = list(dict.fromkeys(source_paths + ["deploy/mysql-init/00_init_zone_dbs.sql"]))
    before = source_hashes(guarded_paths)
    configs = {service: container_addresses(load_yaml(f"go/{service}/etc/{filename}")) for service, filename in GO_CONFIGS.items()}
    for service, config in configs.items():
        # 这两个入口把 ListenOn 原样注册为 NodeInfo，并不读取 POD_IP；显式绑定本容器可达地址。
        listen_host = str(subnet.network_address + SERVICE_IP_OFFSETS[service]) if service in ("scene_manager", "player_locator") else "0.0.0.0"
        config["ListenOn"] = listen_host + ":" + config["ListenOn"].rsplit(":", 1)[1]
        config["Mode"] = "dev"
        if service in ("login", "player_locator", "scene_manager"):
            config["TableDir"] = "/generated/tables"
        if "SnowflakeCacheDir" in config:
            config["SnowflakeCacheDir"] = "/tmp/snowflake"
        if "Prometheus" in config:
            config["Prometheus"]["Host"] = "0.0.0.0"
    login = configs["login"]
    login["Secrets"] = {kind: {"Env": name} for kind, name in (
        ("GateToken", "GATE_TOKEN_SECRET"), ("QueueToken", "QUEUE_TOKEN_SECRET"))}
    login.pop("GateTokenSecret", None)
    login["RequireClientEndpoint"] = True
    # 常驻配置不留账号：写者每 boot 一个账号，由注册步骤写进私有卷里的运行配置（sql_boot_catalog_local.ps1 的 Get-LocalSqlBootConfigPatch）。
    db = configs["db"]
    db["ServerConfig"]["JsonPath"] = "/app/data/mysql_database_table_list.json"
    # player_store_1000000_db 是 dev 地域(0)唯一的地域存档库,新号都落这里(login Placement Region 0 / StorageShards 1);
    # zone 库已退役(docs/design/player-data-tidb-scaling-plan.md §7.2 A17c)。与 compose 的 DB_ALLOWED_DATABASES 同一份。
    db["ServerConfig"]["Database"].update(Backend="mysql", AllowedDatabases=["player_store_1000000_db"], AllowlistEnforcement="strict", AutoCreateDatabase=False, AutoMigrateSchema=False)
    for key in ("User", "Passwd"):
        db["ServerConfig"]["Database"].pop(key, None)
    data = configs["data_service"]
    data["SnapshotMySQL"].update(Backend="mysql", Host="mysql:3306", DBName="testdb")
    for key in ("User", "Password"):
        data["SnapshotMySQL"].pop(key, None)
    data["Schema"]["AutoMigrate"] = False
    # 这个栈没有给 data_service 配资产签名密钥：回收只能预览，执行回 ErrCodeRecallDisabled。
    data["Recall"]["Enabled"] = False
    # guild、trade、mail、friend 不在这个栈里，名字解析不到，调用时算不可达：过户和删角拒绝，其余不受影响。
    for participant in data["RoleFence"]["Participants"]:
        port = participant["Endpoints"][0].rsplit(":", 1)[1]
        participant["Endpoints"] = [f"{ROLE_FENCE_PARTICIPANT_HOSTS[participant['Name']]}:{port}"]
    scene_manager = configs["scene_manager"]
    scene_manager["GateTokenSecret"] = environment["GATE_TOKEN_SECRET"]
    scene_manager["RequireClientEndpoint"] = True
    # 本机只需要一个主世界分线；显式避免沿用压测配置为出生图建立 16 份场景。
    scene_manager["WorldChannelCountByConfId"] = {}
    match = configs["match"]
    # 匹配只有 SQL 权威一条路径：连 compose 的 MySQL，库由 mysql-init 预建，表由 match-migrate 建。
    match["GlobalMatch"].update(Backend="mysql", Host="mysql:3306", User="root")
    match.pop("MatchRedis", None)
    player_inbox = configs["player_inbox"]
    # 独占库由 mysql-init 预建，表由 player-inbox-migrate 建；常驻账号同样来自注册步骤写的运行配置。
    player_inbox["MySQL"].update(Backend="mysql", Host="mysql:3306")
    for key in ("User", "Password"):
        player_inbox["MySQL"].pop(key, None)
    player_inbox["Schema"]["AutoMigrate"] = False
    # TicketSecret 与下面 C++ 的 SpectateTokenSecret 同值：match 签发、gate 验签。
    match["Spectate"].update(TicketSecret=environment["SPECTATE_TOKEN_SECRET"], OpsCallerSecret=environment["MATCH_OPS_CALLER_SECRET"])
    for service, config in configs.items():
        write_yaml(config_dir / f"{service}.yaml", config)
    # 建表用 root（compose 的 db-migrate、data-migrate、player-inbox-migrate）：常驻账号没有 DDL。root 口令只落这三份忽略目录里的配置。
    schema_db = copy.deepcopy(db)
    schema_db["ServerConfig"]["Database"].update(User="root", Passwd=environment["MYSQL_ROOT_PASSWORD"])
    write_yaml(config_dir / "db.schema.yaml", schema_db)
    schema_data = copy.deepcopy(data)
    schema_data["SnapshotMySQL"].update(User="root", Password=environment["MYSQL_ROOT_PASSWORD"])
    write_yaml(config_dir / "data_service.schema.yaml", schema_data)
    schema_inbox = copy.deepcopy(player_inbox)
    schema_inbox["MySQL"].update(User="root", Password=environment["MYSQL_ROOT_PASSWORD"])
    write_yaml(config_dir / "player_inbox.schema.yaml", schema_inbox)
    base = container_addresses(load_yaml("bin/etc/base_deploy_config.yaml"))
    base.update(TableDataDirectory="/app/generated/generated_tables/", DataRootDirectory="/app/", GateTokenSecret=environment["GATE_TOKEN_SECRET"], BattleTokenSecret=environment["BATTLE_TOKEN_SECRET"], SpectateTokenSecret=environment["SPECTATE_TOKEN_SECRET"])
    write_yaml(config_dir / "base_deploy_config.yaml", base)
    game = load_yaml("bin/etc/game_config.yaml")
    # scene 的控制集群句柄连 compose 里的单实例 redis;没写 player_redis 段,玩家域沿用这一个(config.cpp readGameConfig)。
    game["control_redis"]["seeds"] = ["redis:6379"]
    # 这个栈常驻 player_inbox，scene 要真的拉取：入箱的结算只有消费之后才落到玩家身上。这里显式写，不跟着源配置变（线上分步回退时源配置可能是 false）。
    game["player_inbox"]["consumer_enabled"] = True
    write_yaml(config_dir / "game_config.yaml", game)
    # 奖励不派生第二份默认值，原样复制权威配置并纳入同批来源摘要。
    shutil.copyfile(REPO / "bin/etc/battle_rewards.json", config_dir / "battle_rewards.json")
    gateway = container_addresses(load_yaml("java/gateway_node/src/main/resources/application.yaml"))
    # 数据源账号同样每 boot 一个，来自注册步骤写的第二份 additional-location。
    gateway["spring"]["datasource"].update(url=gateway["spring"]["datasource"]["url"].replace("localhost:3306", "mysql:3306"))
    for key in ("username", "password"):
        gateway["spring"]["datasource"].pop(key, None)
    gateway["spring"]["data"]["redis"]["host"] = "redis"
    gateway["login"]["grpc"]["endpoints"] = "1=login:53000"
    gateway["gate"]["token-secret"] = environment["GATE_TOKEN_SECRET"]
    gateway["admin"]["api-key"] = environment["GATEWAY_ADMIN_API_KEY"]
    write_yaml(config_dir / "gateway.yaml", gateway)
    for filename in ("00_init_zone_dbs.sql", "gateway_tables.sql"):
        shutil.copyfile(REPO / "deploy/mysql-init" / filename, sql_dir / filename)
    (output / "init-topics.sh").write_text(topic_script(base, data, db), encoding="utf-8", newline="\n")
    for source in SQL_BOOT_SOURCES:
        target = output / "sql-boot" / source
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(REPO / source, target)
    (output / "sql-writer-entry.sh").write_text(SQL_WRITER_ENTRY, encoding="utf-8", newline="\n")
    environment.update(DEBUG_RUN_DIR=runtime_output.as_posix(), COMPOSE_PROJECT_NAME=args.project, DEBUG_SUBNET=str(subnet), GATEWAY_CLIENT_PORT=str(args.gateway_client_port), GATEWAY_PORT=str(args.gateway_port), GATE_PORT=str(args.gate_port), BATTLE_PORT=str(args.battle_port), KAFKA_COMMAND_TOPIC_GENERATION=str(base["Kafka"]["CommandTopicGeneration"]), KAFKA_COMMAND_TOPIC_PARTITIONS=str(base["Kafka"]["CommandTopicPartitions"]))
    if args.refresh_existing:
        for key in ("DEBUG_RUN_DIR", "COMPOSE_PROJECT_NAME", "DEBUG_SUBNET", "GATEWAY_CLIENT_PORT",
                    "GATEWAY_PORT", "GATE_PORT", "BATTLE_PORT"):
            environment[key] = args.environment[key]
    for service, host_number in SERVICE_IP_OFFSETS.items():
        name = f"{service.upper()}_IP"
        if not args.refresh_existing or name not in environment:
            environment[name] = str(subnet.network_address + host_number)
    (output / ".env").write_text("# 仅用于本机独立开发栈；不要输出、上传或加入版本控制。\n" + "".join(f"{key}={value}\n" for key, value in sorted(environment.items())), encoding="utf-8")
    manifest = source_hashes(source_paths)
    (output / "source-manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    if args.refresh_existing:
        publish_refresh(output, runtime_output, original, guarded_paths, before)
        print("Existing stack configuration refreshed; canonical settings preserved.")
    print(f"Prepared the standalone dev stack from the current {len(source_paths)} source configs: {runtime_output}")
    print("Secrets are kept in the ignored directory and existing values were reused; nothing was built, no container was started, no data was deleted.")


if __name__ == "__main__":
    main()
