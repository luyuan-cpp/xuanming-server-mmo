# Linux Staging 部署 + 阶梯压测留位

**Date:** 2026-05-10
**目的:** 给运维一条**可复制粘贴**的命令序列,把"新登录链路"在 Linux staging 真跑 1k/2k/5k 阶梯。Windows dev 跑不了(`stress-test-2026-05-http-login.md` §四 #B-4),这份是它的对照。
**前置阅读:** [release-checklist.md](./release-checklist.md), [gate-kernel-tuning-runbook.md](./gate-kernel-tuning-runbook.md)

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
- redis-cli 在压测主机的 PATH 里(`stress-linux-tier.sh` wipe 阶段需要)

### A.2 镜像 / 二进制

| 组件 | 来源 |
|---|---|
| `gateway-node-0.0.1-SNAPSHOT.jar` | `mvn -DskipTests package spring-boot:repackage` 后 ship,~90 MB |
| go-zero `login` / `player_locator` / 其他 | `dev.bat go-svc-build`(linux 上换成 `go build -o bin/go_services/<svc> ./cmd/<svc>`),~30-50 MB each |
| cpp `gate` / `scene` | `dev.bat build`(linux 上跑 `cmake --build build --target gate scene -j`),~70 MB each |
| `robot` 压测客户端 | `cd robot && go build -o robot .`,~26 MB |

### A.3 配置(从 dev 同步,改这几项)

| 文件 | 字段 | dev 值 | staging 值 |
|---|---|---|---|
| `go/login/etc/login.yaml` | `ListenOn` | `127.0.0.1:51000`(避 Win 保留段) | **`0.0.0.0:50000`** |
| `go/player_locator/etc/player_locator.yaml` | `ListenOn` | `127.0.0.1:51200` | **`0.0.0.0:50200`** |
| `java/gateway_node/.../application.yaml` | `login.grpc.endpoints` | `127.0.0.1:51000` | login 主机 IP `:50000` |
| `bin/etc/base_deploy_config.yaml` | `Kafka.Brokers` | `localhost:9092` | 3 个 broker host:port |
| `bin/etc/base_deploy_config.yaml` | `etcd_hosts` | `localhost:2379` | etcd 集群地址 |
| `bin/etc/base_deploy_config.yaml` | `gate_token_secret` | dev 值 | 用 `openssl rand -hex 32` 生成,**和 login.yaml 同步** |
| `gate.token-secret` (application.yaml) | 同上 | | 同上 |

---

## B. 部署顺序(每次发版照做)

```bash
# 1. infra(假设已是常驻)
docker compose ps                    # 期望: redis / etcd / mysql / kafka × 3 都 healthy

# 2. sysctl 一次性应用(只第一次)
sudo cp docs/ops/99-gate.conf /etc/sysctl.d/         # 把 runbook §二 复制成文件
sudo sysctl --system

# 3. 起 go 服务(staged: db -> data_service -> player_locator -> login -> scene_manager)
for s in db data_service player_locator login scene_manager; do
  ./bin/go_services/${s} -f go/${s}/etc/${s}.yaml >run/logs/go_services/${s}.log 2>&1 &
  sleep 2
done

# 4. 起 cpp gate / scene
./bin/scene >run/logs/cpp_nodes/scene.log 2>&1 &
sleep 2
./bin/gate  >run/logs/cpp_nodes/gate.log  2>&1 &
sleep 5
# 验证 gate 在 etcd 注册:
etcdctl get --prefix GateNodeService.rpc/zone --keys-only

# 5. 起 Java Gateway
java -jar gateway-node.jar >run/logs/java/gateway.log 2>&1 &

# 6. smoke
curl -s -X POST http://${GATEWAY_HOST}:8081/api/login \
     -H "Content-Type: application/json" \
     -d '{"zone_id":1,"account":"smoke","password":"123456"}'
# 期望: {"code":0,"access_token":"...","refresh_token":"..."}
```

---

## C. 跑阶梯压测

```bash
# 在压测主机(可以是 gate 同机,也可以是单独 client 主机):
cd /opt/mmorpg
./tools/scripts/stress-linux-tier.sh                  # 默认 1000/2000/5000
# 或自定义:
./tools/scripts/stress-linux-tier.sh 100 500 1000 2000 5000
PER_TIER_SEC=120 ./tools/scripts/stress-linux-tier.sh # 每档 2 分钟而非默认 60s
```

输出:`run/logs/stress/results-linux-<TS>.csv`,每行一档:
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
  ps -p $(pgrep -f bin/gate) -o %cpu,rss --no-headers
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

> **TODO(staging-ops):** 跑完后,把 csv 第一行加进 [stress-test-2026-05-http-login.md §二](../stress/stress-test-2026-05-http-login.md) 的表格,新增一列 `env`(dev/Linux-staging),并把第一档拐点信号写到本文 §D 的"实测"列。

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

## F. 已知限制(同样适用于 staging)

1. **本压测不验证战斗内 RPC 性能**:robot 在 [stats] 看到的 `msg_sent/msg_recv` 是 stress profile 的低速心跳,不代表真实战斗压力。这个由另一套 robot mode(`profile: fighter` + 高 `action_interval`)覆盖,不在本文 scope。
2. **本压测假设 Bucket4j limiter OFF**:T+0 灰度期默认行为,生产 staging 在切 T+1 后再补一轮 limiter ON 的对照测。
3. **#B-1 修复后理论上不再需要 wipe**(robot defer Disconnect + cpp gate forward player_id 双层)。但 wipe 仍保留在 `stress-linux-tier.sh` 里,因为(a)兜底,(b)排除 redis 上其它服务残留干扰。

---

## 关联文档
- [stress-test-2026-05-http-login.md](../stress/stress-test-2026-05-http-login.md) — Windows dev 基线 + 命中数据
- [release-checklist.md](./release-checklist.md) — 全套上线前 checklist + 回滚 SOP
- [gate-kernel-tuning-runbook.md](./gate-kernel-tuning-runbook.md) — sysctl 完整清单
- [open-server-rate-limit-design.md](../design/open-server-rate-limit-design.md) — Bucket4j 真实阈值参考
- 脚本:`tools/scripts/stress-linux-tier.sh`
