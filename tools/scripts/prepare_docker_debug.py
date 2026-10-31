"""从当前仓库配置准备独立 Docker 开发栈；只写忽略目录，不启动或构建容器。"""

from __future__ import annotations

import argparse
import copy
import hashlib
import ipaddress
import json
from pathlib import Path
import re
import secrets
import shutil
import subprocess

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
    # 调试栈不常驻 guild，只用这份配置建表：match 启动要核对 guild 的历练表（compose 的 guild-migrate）。
    "guild": "guild.yaml",
}
SERVICE_IP_OFFSETS = {
    "login": 10, "db": 11, "data_service": 12, "player_locator": 13,
    "scene_manager": 14, "client_rpc_router": 15, "match": 16, "player_inbox": 17,
    "gate": 30, "scene": 31, "battle": 32,
}
# 观战两把密钥、存盘接口密钥、匹配链路四把密钥和账号目录四把调用方密钥晚于已准备好的 .env 引入，也不写进任何数据卷，补生成不算轮换。
ADDITIVE_SECRET_NAMES = ("SPECTATE_TOKEN_SECRET", "MATCH_OPS_CALLER_SECRET", "PLAYER_STORE_SCENE_SECRET",
                         "MATCH_PROBE_SECRET", "RANK_PRODUCER_SECRET_MATCH", "MMORPG_MATCH_OUTCOME_SECRET_BATTLE", "MMORPG_PLAYER_INBOX_SECRET_MATCH",
                         "MMORPG_ACCOUNT_DIRECTORY_SECRET_LOGIN", "MMORPG_ACCOUNT_DIRECTORY_SECRET_TRADE",
                         "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE", "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER")
SECRET_NAMES = (
    "MYSQL_ROOT_PASSWORD", "GATE_TOKEN_SECRET",
    "QUEUE_TOKEN_SECRET", "INTERNAL_AUTH_SECRET", "BATTLE_TOKEN_SECRET",
    "LOGIN_DEV_PASSWORD_SHARED_SECRET", "GATEWAY_ADMIN_API_KEY",
    *ADDITIVE_SECRET_NAMES,
)
# 与 C++ kSpectateBroadcastTopicPartitions（node_command_topic.h）相等；分区数是不可变契约，改数走换代号。
SPECTATE_BROADCAST_PARTITIONS = 16
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


def read_environment(path: Path) -> dict[str, str]:
    if not path.exists():
        return {name: secrets.token_hex(32) for name in SECRET_NAMES}
    values = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        if line and not line.startswith("#"):
            key, value = line.split("=", 1)
            values[key] = value
    for name in ADDITIVE_SECRET_NAMES:
        values.setdefault(name, secrets.token_hex(32))
    # 已有数据卷的口令不能因重新准备配置而悄悄轮换。
    if any(not re.fullmatch(r"[0-9a-f]{64}", values.get(name, "")) for name in SECRET_NAMES):
        raise ValueError("已有 .env 缺少有效密钥；保留原文件并人工核对，拒绝自动轮换")
    if len({values[name] for name in SECRET_NAMES}) != len(SECRET_NAMES):
        raise ValueError("已有 .env 不同用途复用了密钥，拒绝继续")
    return values


def check_subnet(subnet: ipaddress.IPv4Network, project: str) -> None:
    names = subprocess.run(["docker", "network", "ls", "-q"], check=True, capture_output=True, text=True).stdout.split()
    if not names:
        return
    networks = json.loads(subprocess.run(["docker", "network", "inspect", *names], check=True, capture_output=True, text=True).stdout)
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


def topic_script(base: dict, data: dict) -> str:
    commands = base["Kafka"]
    audit = data["Kafka"]
    if base["AuditTopicGeneration"] != audit["TopicGeneration"]:
        raise ValueError("C++/data_service 的审计 topic 世代不一致")
    topics = [
        (f"gate-cmd_g{commands['CommandTopicGeneration']}", commands["CommandTopicPartitions"], 3600000),
        (f"scene-cmd_g{commands['CommandTopicGeneration']}", commands["CommandTopicPartitions"], 3600000),
        (f"gate-spectate_g{commands['CommandTopicGeneration']}", commands["CommandTopicPartitions"], 600000),
        (f"spectate-bcast_g{commands['CommandTopicGeneration']}", SPECTATE_BROADCAST_PARTITIONS, 600000),
        (f"{audit['TransactionLogTopic']}_g{audit['TopicGeneration']}", audit["TransactionLogPartitions"], audit["RetentionMs"]),
        (f"{audit['SnapshotTopic']}_g{audit['TopicGeneration']}", audit["SnapshotPartitions"], audit["RetentionMs"]),
    ]
    script = r"""#!/bin/sh
set -eu
command -v timeout >/dev/null || { echo '缺少 Kafka CLI 超时工具' >&2; exit 1; }
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
  fail_cli 'Kafka 就绪检查失败或超时'
fi
ensure_topic() {
  topic="$1"; partitions="$2"; retention="$3"
  topic_deadline=$(($(now_seconds) + 60))
  if ! run_cli "$topic_deadline" /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:29092 --create --if-not-exists --topic "$topic" --partitions "$partitions" --replication-factor 1 --config "retention.ms=$retention"; then
    fail_cli "创建 topic 失败或超时: $topic"
  fi
  cat "$cli_output"
  while :; do
    if run_cli "$topic_deadline" /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:29092 --describe --topic "$topic"; then
      actual=$(sed -n 's/.*PartitionCount:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$cli_output")
      [ "$actual" = "$partitions" ] || fail_cli "分区契约不符: $topic expected=$partitions actual=$actual"
      break
    fi
    if ! topic_missing "$topic"; then fail_cli "查询 topic 失败或超时: $topic"; fi
    wait_metadata "$topic_deadline" "topic 元数据在截止时间前仍不可见: $topic"
  done
  if ! run_cli "$topic_deadline" /opt/kafka/bin/kafka-configs.sh --bootstrap-server kafka:29092 --entity-type topics --entity-name "$topic" --alter --add-config "retention.ms=$retention"; then
    fail_cli "设置保留期失败或超时: $topic"
  fi
  while :; do
    if run_cli "$topic_deadline" /opt/kafka/bin/kafka-configs.sh --bootstrap-server kafka:29092 --entity-type topics --entity-name "$topic" --describe; then
      actual=$(sed -n 's/^[[:space:]]*retention\.ms=\([0-9][0-9]*\).*/\1/p' "$cli_output")
      case "$actual" in ''|*[!0-9]*) fail_cli "无法解析保留期契约: $topic";; esac
      [ "$actual" != "$retention" ] || break
    elif ! topic_missing "$topic"; then
      fail_cli "查询保留期失败或超时: $topic"
    fi
    wait_metadata "$topic_deadline" "保留期在截止时间前仍不符合契约: $topic expected=$retention actual=${actual:-unknown}"
  done
  echo "topic 就绪: $topic partitions=$partitions"
}
"""
    for name, partitions, retention in topics:
        if not re.fullmatch(r"[A-Za-z0-9._-]+", name) or int(partitions) <= 0 or int(retention) <= 0:
            raise ValueError("源配置包含无效 topic 契约")
        script += f"ensure_topic '{name}' {int(partitions)} {int(retention)}\n"
    return script


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default="build/docker-debug-current")
    parser.add_argument("--project", default="xuanming-debug-current")
    parser.add_argument("--subnet", default="172.29.220.0/24")
    parser.add_argument("--gateway-port", type=int, default=18081)
    parser.add_argument("--gate-port", type=int, default=28000)
    parser.add_argument("--battle-port", type=int, default=28010)
    args = parser.parse_args()
    output = (REPO / args.output).resolve()
    if not output.is_relative_to((REPO / "build").resolve()):
        raise ValueError("配置与密钥只能输出到仓库忽略的 build/ 子目录")
    ignored = subprocess.run(["git", "check-ignore", "--quiet", str(output / ".env")], cwd=REPO)
    if ignored.returncode != 0:
        raise ValueError("输出目录没有被 Git 忽略，拒绝写入密钥")
    if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", args.project):
        raise ValueError("项目名必须由小写字母、数字、下划线或连字符组成")
    ports = (args.gateway_port, args.gate_port, args.battle_port)
    if len(set(ports)) != 3 or any(not 1024 <= port <= 65535 for port in ports):
        raise ValueError("三个宿主入口端口必须不同且在 1024..65535 内")
    if 8081 in (args.gate_port, args.battle_port):
        raise ValueError("8081 已保留给客户端默认的 HTTP 网关入口")
    subnet = ipaddress.ip_network(args.subnet, strict=True)
    if subnet.version != 4 or subnet.prefixlen != 24 or not subnet.is_private:
        raise ValueError("开发栈需要独立私有 IPv4 /24 子网")
    check_subnet(subnet, args.project)
    output.mkdir(parents=True, exist_ok=True)
    environment = read_environment(output / ".env")
    environment.setdefault("KAFKA_CLUSTER_ID", secrets.token_urlsafe(16))
    config_dir = output / "config"
    sql_dir = output / "mysql-init"
    config_dir.mkdir(exist_ok=True)
    sql_dir.mkdir(exist_ok=True)
    source_paths = [f"go/{service}/etc/{filename}" for service, filename in GO_CONFIGS.items()]
    source_paths += ["bin/etc/base_deploy_config.yaml", "bin/etc/game_config.yaml",
                     "bin/etc/battle_rewards.json", *SQL_BOOT_SOURCES]
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
        ("GateToken", "GATE_TOKEN_SECRET"), ("QueueToken", "QUEUE_TOKEN_SECRET"), ("InternalAuth", "INTERNAL_AUTH_SECRET"))}
    login.pop("GateTokenSecret", None)
    login["RequireClientEndpoint"] = True
    # 常驻配置不留账号：写者每 boot 一个账号，由注册步骤写进私有卷里的运行配置（sql_boot_catalog_local.ps1 的 Get-LocalSqlBootConfigPatch）。
    db = configs["db"]
    db["ServerConfig"]["JsonPath"] = "/app/data/mysql_database_table_list.json"
    db["ServerConfig"]["Database"].update(Backend="mysql", AllowedDatabases=["zone_1_db"], AllowlistEnforcement="strict", AutoCreateDatabase=False, AutoMigrateSchema=False)
    for key in ("User", "Passwd"):
        db["ServerConfig"]["Database"].pop(key, None)
    data = configs["data_service"]
    data["SnapshotMySQL"].update(Backend="mysql", Host="mysql:3306", DBName="testdb")
    for key in ("User", "Password"):
        data["SnapshotMySQL"].pop(key, None)
    data["Schema"]["AutoMigrate"] = False
    scene_manager = configs["scene_manager"]
    scene_manager["GateTokenSecret"] = environment["GATE_TOKEN_SECRET"]
    scene_manager["RequireClientEndpoint"] = True
    # 本机只需要一个主世界分线；显式避免沿用压测配置为出生图建立 16 份场景。
    scene_manager["WorldChannelCountByConfId"] = {}
    match = configs["match"]
    # 匹配只有 SQL 权威一条路径：连 compose 的 MySQL，库由 mysql-init 预建，表由 match-migrate 建；全局角色库与 data_service 同库。
    match["GlobalMatch"].update(Backend="mysql", Host="mysql:3306", User="root", AccountRoleSchema=data["SnapshotMySQL"]["DBName"])
    match.pop("MatchRedis", None)
    guild = configs["guild"]
    # 连接串只走环境变量（compose 的 guild-migrate），root 口令不落这份配置。
    guild["MySQL"].pop("DataSource", None)
    guild["MySQL"].update(Backend="mysql", DataSourceEnv="MMORPG_GUILD_SQL_DSN")
    player_inbox = configs["player_inbox"]
    # 独占库由 mysql-init 预建，表由 player-inbox-migrate 建；常驻账号同样来自注册步骤写的运行配置。角色目录与 data_service 同库。
    player_inbox["MySQL"].update(Backend="mysql", Host="mysql:3306")
    for key in ("User", "Password"):
        player_inbox["MySQL"].pop(key, None)
    player_inbox["AccountRoleSchema"] = data["SnapshotMySQL"]["DBName"]
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
    game["zoneredis"]["host"] = "redis"
    # 这个栈常驻 player_inbox，scene 要真的拉取：入箱的结算只有消费之后才落到玩家身上。源配置的 false 是线上分步打开用的。
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
    (output / "init-topics.sh").write_text(topic_script(base, data), encoding="utf-8", newline="\n")
    for source in SQL_BOOT_SOURCES:
        target = output / "sql-boot" / source
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(REPO / source, target)
    (output / "sql-writer-entry.sh").write_text(SQL_WRITER_ENTRY, encoding="utf-8", newline="\n")
    environment.update(DEBUG_RUN_DIR=output.as_posix(), COMPOSE_PROJECT_NAME=args.project, DEBUG_SUBNET=str(subnet), GATEWAY_PORT=str(args.gateway_port), GATE_PORT=str(args.gate_port), BATTLE_PORT=str(args.battle_port), KAFKA_COMMAND_TOPIC_GENERATION=str(base["Kafka"]["CommandTopicGeneration"]), KAFKA_COMMAND_TOPIC_PARTITIONS=str(base["Kafka"]["CommandTopicPartitions"]))
    for service, host_number in SERVICE_IP_OFFSETS.items():
        environment[f"{service.upper()}_IP"] = str(subnet.network_address + host_number)
    (output / ".env").write_text("# 仅用于本机独立开发栈；不要输出、上传或加入版本控制。\n" + "".join(f"{key}={value}\n" for key, value in sorted(environment.items())), encoding="utf-8")
    manifest = {path: hashlib.sha256((REPO / path).read_bytes()).hexdigest() for path in source_paths}
    (output / "source-manifest.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"已从当前 {len(source_paths)} 份源配置准备独立开发栈: {output}")
    print("密钥已保存在忽略目录并沿用已有值；未构建、启动容器或删除数据。")


if __name__ == "__main__":
    main()
