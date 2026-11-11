# Linux Staging 部署 + 阶梯压测留位

**Date:** 2026-05-10
**目的:** 给运维一条**可复制粘贴**的命令序列,把"新登录链路"在 Linux staging 真跑 1k/2k/5k 阶梯。Windows dev 跑不了(`stress-test-2026-05-http-login.md` §四 #B-4),这份是它的对照。
**前置阅读:** [release-checklist.md](./release-checklist.md), [gate-kernel-tuning-runbook.md](./gate-kernel-tuning-runbook.md)

**2026-10-15 修订:** 本文的机器规格、阶梯阈值与表格来自 2026-05,不是当前版本容量证据。下面部署示例针对无真实玩家、已隔离的 Linux 联调环境;正式 staging / prod 走 [K8s 开服手册](k8s-open-server-runbook_zh.md)。本次只修订文档,未启动服务或执行压测。

---

## A. 必备 staging 环境

### A.1 机器分布(最小)

| 角色 | 推荐规格 | 数量 | 备注 |
|---|---|---|---|
| Java Gateway | 4C8G | 1 | port 8081 |
| go-zero login | 4C8G | 1 | port 50000(Linux 不撞 Hyper-V 保留) |
| go-zero player_locator | 2C4G | 1 | port 50200 |
| go-zero scene_manager / data_service / db | 2C4G each | 1 | |
| cpp gate | 8C16G(sysctl 调好) | 1 | TCP 长连接接入 |
| cpp scene | 8C16G | 1+ | 战斗节点 |
| docker infra | 8C16G | 1 | redis / etcd / mysql / kafka(**3+ broker**)|

**关键差异 vs Windows dev**:
- kafka **必须 ≥3 broker**(消除 dev 单 broker 偶发 Exited(255))
- gate 主机必须先按 `gate-kernel-tuning-runbook.md` 跑 sysctl
- ulimit -n ≥ 1048576(写到 systemd unit 或 `/etc/security/limits.d/`)
  - **2026-10-18 补注**:C++ 节点(gate / scene / battle)启动时会把 RLIMIT_NOFILE **软限**自抬到 min(硬限, 1048576),所以这里真正要保证的是启动它们的 shell 的**硬限**(`ulimit -Hn`)。硬限低于需求就**拒绝启动**:gate = `GateMaxConnections` + 4096(默认 24096),battle = `BattleMaxConnections` + 4096(默认 8192),scene = 4096;stderr 一行 `FATAL open file limit config: ...`。每次启动 stdout 都有一行 `[open_file_limit] role=... verdict=... mode=... required=... soft=... hard=...`。`tools/scripts/deploy-staging.sh start` 在 sleep 之后会检查 gate / scene 是否还活着并 grep 这条拒启行,命中就报错退出。硬限一时改不了又必须先起服时,在启动前 `export NODE_OPEN_FILE_LIMIT_MODE=warn`(运维豁免:只打一行 WARN 写明缺口、继续启动,超出软限的连接会 EMFILE;与 `GATE_RUN_MODE` 等运行模式无关),压测结论按实际软限解读。细节见 [gate-kernel-tuning-runbook.md](./gate-kernel-tuning-runbook.md) §三。
  - **2026-10-18 更正**:① 上一条把 warn 的后果说轻了。gate 在 accept 拿到 fd 之后才按 `GateMaxConnections` 判准入,上限没调时客户端连接会先占满 fd 表,之后节点间 / Redis / Kafka / etcd 重连与日志滚动也会 EMFILE,节点可能被摘除或崩溃。gate / battle 硬限不够时首选把 `GateMaxConnections` / `BattleMaxConnections` 调到 ≤ 实际软限 − 4096(检查直接通过,不需要豁免);只开 warn 不调上限只适合短时应急,压测结论不可信。详见 gate-kernel-tuning-runbook.md §3.1。② `deploy-staging.sh start` 以前直接以 `${REPO_ROOT}/bin/scene` 起节点、cwd 是调用者目录,节点在 fd 检查之前就因找不到 `etc/base_deploy_config.yaml` 退出,拒启检查只会报 `exited during startup`;现已改为与下文 B 第 4 步相同的 `(cd "$REPO_ROOT/bin" && exec ./scene)`。③ 该脚本因拒启或节点退出而报错时,已起的 Go 服务(以及 gate 拒启时的 scene)仍在后台,先 `./tools/scripts/deploy-staging.sh stop` 再重试 `start`。
- redis-cli 在压测主机的 PATH 里(`stress-linux-tier.sh` wipe 阶段需要)

### A.2 镜像 / 二进制

| 组件 | 来源 |
|---|---|
| `gateway-node-0.0.1-SNAPSHOT.jar` | `mvn -DskipTests package spring-boot:repackage` 后 ship,~90 MB |
| go-zero `login` / `player_locator` / 其他 | 每个服务是独立 Go module,在 `go/<svc>` 中 `go build -o ../../bin/go_services/<svc> .`;入口名不同,不能假定都有 `./cmd/<svc>` |
| cpp `gate` / `scene` | Linux 规范入口 `bash tools/scripts/build_linux.sh --release`;先按构建说明准备依赖,产物放在 `bin/` |
| `robot` 压测客户端 | `cd robot && go build -o robot .`,~26 MB |

### A.3 配置(从 dev 同步,改这几项)

| 文件 | 字段 | dev 值 | staging 值 |
|---|---|---|---|
| `go/login/etc/login.yaml` | `ListenOn` | `127.0.0.1:53000` | 隔离环境自定;下例为 `0.0.0.0:50000` |
| `go/player_locator/etc/player_locator.yaml` | `ListenOn` | `127.0.0.1:53200` | 隔离环境自定;下例为 `0.0.0.0:50200` |
| `java/gateway_node/src/main/resources/application.yaml` | `login.grpc.endpoints` | zone 1 为 `127.0.0.1:53000`,并启用 etcd 发现 | 与实际 login 地址、zone 和发现配置同步 |
| `bin/etc/base_deploy_config.yaml` | `Kafka.Brokers` | `localhost:9092` | 3 个 broker host:port |
| `bin/etc/base_deploy_config.yaml` | `etcd_hosts` | `localhost:2379` | etcd 集群地址 |
| `bin/etc/base_deploy_config.yaml` / login 配置 | gate token HMAC | 开发配置 | 两端一致,通过各自支持的环境变量注入;Java gateway 不选 gate、不签票,无需复制此密钥 |

修改的是该隔离环境自己的配置副本。同步更新所有服务间 RPC 地址、etcd、Redis、MySQL、Kafka 与区服数据,并确认 data_service 的建库 / 迁移已按其手册完成。开发共享口令认证只允许 go-zero `Mode: dev/test`:显式启用 `DevPasswordAuth`,保留 `robot_` / `dev_` 前缀,并从 `LOGIN_DEV_PASSWORD_SHARED_SECRET` 注入共享口令。正式 staging / prod 禁止带入这个开发入口,使用真实认证或已配置的第三方认证。

---

## B. 部署顺序(每次发版照做)

```bash
# 在仓库根目录。以下以本机隔离环境的配置为例。
REPO_ROOT="$PWD"
mkdir -p run/logs/go_services run/logs/cpp_nodes run/logs/java
# 仅限显式启用 DevPasswordAuth 的隔离环境,必须在 login 启动前注入。
read -rsp '隔离环境开发共享口令: ' LOGIN_DEV_PASSWORD_SHARED_SECRET
echo
export LOGIN_DEV_PASSWORD_SHARED_SECRET
# 会话身份密钥对(docs/design/gate-online-identity.md §4、§10.1):gate 用 Ed25519 私钥签名,login 与在线服务只用公钥验签。
# 生成一次:cd go/shared && go run ./cmd/identitykeygen,输出私钥(64 个十六进制字符)与公钥两行。
# 私钥缺失、格式不对或与公钥不配对,gate / 验证方直接拒启,deploy-staging.sh start 在起任何进程前检查;私钥不要写进文件或 shell 历史。
read -rsp 'gate 签名私钥: ' MMORPG_INTERNAL_AUTH_SIGNING_KEY
echo
read -rp '验签公钥列表(逗号分隔): ' MMORPG_INTERNAL_AUTH_PUBLIC_KEYS
export MMORPG_INTERNAL_AUTH_SIGNING_KEY MMORPG_INTERNAL_AUTH_PUBLIC_KEYS

# 1. infra 已由运维按实际拓扑部署并验证;本地单 broker Compose 不等于三 broker staging。
docker compose -f deploy/docker-compose.yml ps

# 2. sysctl 一次性应用(只第一次)
# 先按 gate-kernel-tuning-runbook.md 审查并创建环境自己的 sysctl 文件;
# 仓库没有可直接 cp 的 docs/ops/99-gate.conf。
sudo sysctl --system

# 3. 定义启动映射,然后逐条执行,每条都先确认日志与真实健康 / RPC 探针再继续。
#    配置内容须已按 A.3 对齐,不能用固定 sleep 当就绪证据。
start_service() {
  local service_name="$1" config_name="$2"
  (cd "$REPO_ROOT/go/$service_name" && \
    exec "$REPO_ROOT/bin/go_services/$service_name" -f "etc/$config_name") \
    >"$REPO_ROOT/run/logs/go_services/$service_name.log" 2>&1 &
}
start_service db db.yaml
# 确认 db 就绪后:
start_service data_service data_service.yaml
# 确认 data_service 就绪后:
start_service scene_manager scene_manager_service.yaml
# 确认 scene_manager 就绪后:
start_service player_locator player_locator.yaml
# 确认 player_locator 就绪后:
start_service login login.yaml

# 4. 起 cpp gate / scene
# 节点按 cwd 读取 etc/、脚本与资源,必须从 bin/ 启动。
(cd "$REPO_ROOT/bin" && exec ./scene) >run/logs/cpp_nodes/scene.log 2>&1 &
# 确认 scene 已就绪后再启动 gate;本例使用 C++ 默认的逐服务直连模式。
(cd "$REPO_ROOT/bin" && GATE_CLIENT_RPC_ROUTER=0 exec ./gate) >run/logs/cpp_nodes/gate.log 2>&1 &
# 若启用 GATE_CLIENT_RPC_ROUTER=1,必须先部署并验证 client_rpc_router,不能只改开关。
# 验证 gate 在 etcd 注册:
etcdctl get --prefix GateNodeService.rpc/zone --keys-only
# (2026-10-18)确认 fd 限值:每个节点的 stdout 里有一行 [open_file_limit] ... verdict=ok;
# 出现 "FATAL open file limit config:" 说明这个 shell 的硬限不够,节点已拒启,见 A.1 补注。
grep -h '\[open_file_limit\]\|FATAL open file limit' run/logs/cpp_nodes/scene.log run/logs/cpp_nodes/gate.log

# 5. 起 Java Gateway
java -jar java/gateway_node/target/gateway-node-0.0.1-SNAPSHOT.jar >run/logs/java/gateway.log 2>&1 &

# 6. smoke
# 仅限上文已显式启用开发认证的隔离环境,使用启动 login 时的同一份环境口令。
: "${LOGIN_DEV_PASSWORD_SHARED_SECRET:?login 启动前必须已注入共享口令}"
jq -nc '{zone_id:1,account:"robot_linux_smoke",password:env.LOGIN_DEV_PASSWORD_SHARED_SECRET}' | \
  curl -s -X POST "http://${GATEWAY_HOST:-127.0.0.1}:8081/api/login" \
    -H 'Content-Type: application/json' --data-binary @-
# 期望: {"code":0,"access_token":"...","refresh_token":"..."}
```

---

## C. 跑阶梯压测

`tools/scripts/stress-linux-tier.sh` 是 2026-05 的历史辅助脚本:它从旧模板生成配置,只删部分 Redis 键,不完成当前 AGENTS.md §6 的 MySQL / etcd 清理、上一轮基线、三阶段 snapshot 与总结流程。模板认证口径也不能直接当成上文开发共享口令配置。它暂不能作为当前版本可直接执行的标准压测入口。

开始当前阶梯前,先按根 [AGENTS.md §6](../../AGENTS.md) 确定每档配置、认证方式、清理对象、采样计划与对比基线,再由 Codex / 运维执行。robot 配置使用 `gateway_addr / zone_id / robot_count / account_fmt / auth_type / use_http_login` 等真实小写键;口令若需写入临时配置,只能放受控的 Git 外文件并在结束后清理。不要把历史脚本生成的 CSV 当成已满足当前验收门禁。

历史脚本的输出格式是 `run/logs/stress/results-linux-<TS>.csv`,每行一档:
```
robot_count,login_ok,login_fail,login_stuck,enter_ok,avg_login_ms,max_login_ms,recon_ok,duration_s
```

**同时**在 gate 主机上开监控(每 10s 一次):

```bash
# 单独终端:
while sleep 10; do
  echo "=== $(date +%T) ==="
  ss -s | head -2
  nstat | grep -E 'TcpExtListenOverflows|TcpExtListenDrops' | head
  gate_pids=$(pgrep -d, -x gate || true)
  [ -z "$gate_pids" ] || ps -p "$gate_pids" -o %cpu,rss --no-headers
  redis-cli ZCARD player_locator:lease_zset 2>/dev/null
done | tee run/logs/stress/gate-watch.log
```

---

## D. 拐点判定(参考 release-checklist §F + 本文)

按经验,这台机器在每个阶梯 **预期** 看到的:

| robot_count | 预期 avg_login | 拐点信号(任一即拐点) |
|---|---|---|
| 1000 | < 120 ms | 已通过(本机基线 500 档 69 ms) |
| 2000 | < 200 ms | `ListenOverflows` > 0 / kafka lag > 1000 |
| 5000 | < 500 ms | `Bound` 进入千级 / cpp gate %CPU > 80% |
| 10000+ | 拐点已过 | 多源 IP 必须开;sysctl 必须固化;考虑多 gate 实例 |

**任一档出现下面任意一条**,就停下不要继续往上压:
- `login_fail` > 1%
- `login_stuck` > 0
- p99 / max 跳到秒级
- gate stderr 出现 `Bound=` / `SynSent=` 千级
- kafka lag 持续涨

捕获那一分钟的:
1. `gate-watch.log` 上下 60s
2. `nstat` snapshot
3. cpp gate stderr 最近 200 行
4. `run/logs/stress/run-linux-<N>.log` 末尾 200 行

---

## E. 数据回填位(等真实跑完填这里)

> **历史占位:** 下表来自 2026-05,保留作当时基线。当前新测须在 `docs/stress/` 新复盘中登记环境、版本、配置与 `stress_summarize.ps1` 对比表,不把新版本结果混进旧轮次当作同口径数据。

预填的占位结构:

| robot_count | env | login_ok | login_fail | enter_ok | avg_login | max_login | 拐点信号 |
|---|---|---|---|---|---|---|---|
| 50 | Windows-dev | 100 | 0 | 100 | 101 ms | 342 ms | none |
| 100 | Windows-dev | 200 | 0 | 200 | 84 ms | 331 ms | none |
| 200 | Windows-dev | 316 | 0 | 316 | 75 ms | 339 ms | none |
| 500 | Windows-dev | 604 | 0 | 604 | 69 ms | 163 ms | none |
| **1000** | Linux-staging | _TODO_ | _TODO_ | _TODO_ | _TODO_ | _TODO_ | _TODO_ |
| **2000** | Linux-staging | _TODO_ | _TODO_ | _TODO_ | _TODO_ | _TODO_ | _TODO_ |
| **5000** | Linux-staging | _TODO_ | _TODO_ | _TODO_ | _TODO_ | _TODO_ | _TODO_ |

---

## F. 已知限制与历史边界

1. **本压测不验证战斗内 RPC 性能**:robot 在 [stats] 看到的 `msg_sent/msg_recv` 是 stress profile 的低速心跳,不代表真实战斗压力。这个由另一套 robot mode(`profile: fighter` + 高 `action_interval`)覆盖,不在本文 scope。
2. **限流关闭是 2026-05 基线设置**:当前 K8s staging / prod 默认开启限流和登录排队。开 / 关削峰的结果必须分轮记录,不能沿用旧 T+0 / T+1 时间线推定当前配置。
3. **清理仍是强制门禁**:即使旧 #B-1 缺陷已修,当前每轮登录压测仍须按 AGENTS.md §6 清理隔离测试环境的 Redis / MySQL / etcd 等残留并保存基线;历史脚本的部分 Redis wipe 不满足这一要求。

---

## 关联文档
- [stress-test-2026-05-http-login.md](../stress/stress-test-2026-05-http-login.md) — Windows dev 基线 + 命中数据
- [release-checklist.md](./release-checklist.md) — 全套上线前 checklist + 回滚 SOP
- [gate-kernel-tuning-runbook.md](./gate-kernel-tuning-runbook.md) — sysctl 完整清单
- [open-server-rate-limit-design.md](../design/open-server-rate-limit-design.md) — Bucket4j 真实阈值参考
- 脚本:`tools/scripts/stress-linux-tier.sh`
