# 端到端阶梯压测: HTTP /api/login 路径 (2026-05-09)

**Date:** 2026-05-09
**前置改动:** [open-server-rate-limit-design.md](./open-server-rate-limit-design.md), [third-party-login-end-to-end-design.md](./third-party-login-end-to-end-design.md), [gate-login-rpc-boundary.md](./gate-login-rpc-boundary.md)
**目的:** 在新登录链路全部上线后,跑一次阶梯加压拐点测试,作为后续容量规划的基线。

---

## 一、链路 & 配置

```
robot ──HTTP /api/login──→ Java Gateway (limiter OFF) ──gRPC──→ go-zero login
robot ──HTTP /api/assign-gate──→ Java Gateway ──gRPC──→ go-zero login (HMAC token)
robot ──TCP──→ cpp gate ──gRPC──→ go-zero login (auth_type=access_token,轻量 BindSession)
robot ──TCP──→ cpp gate ──Kafka gate-{0}──→ go-zero login + scene_manager
```

- `robot/etc/robot.stress-{N}.yaml` (`use_http_login: true`, `auth_type: password`)
- 每档跑 30 秒
- 每档前清 redis 测试残留(`player:session:*` / `account_data:*` / `player_locator:*`/ `access_token:*` / `refresh_token:*` / `login_session:*` / `login_step:*` / `account_lock:*` / `player_locker:*`)
- 单 cpp gate(zone 1, gate_node_id 0) + 单 cpp scene
- 所有 go 服务 + Java Gateway 单实例,本机 docker infra(redis / etcd / mysql / kafka)

---

## 二、结果(基线)

| robot_count | login_ok | login_fail | login_stuck | enter_ok | avg_login | max_login | duration |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 50  | 100 | 0 | 0 | 100 | **101 ms** | 342 ms | 30 s |
| 100 | 200 | 0 | 0 | 200 | **84 ms**  | 331 ms | 31 s |
| 200 | 316 | 0 | 0 | 316 | **75 ms**  | 339 ms | 30 s |
| 500 | 604 | 0 | 0 | 604 | **69 ms**  | 163 ms | 30 s |

> `login_ok` 数 ≈ 2 × `robot_count` 是因为 robot 在窗口内有一次 reconnect(`recon_ok` 列与 `robot_count` 接近),所以每个机器人贡献 2 次 login。500 档少 4(626/630),原因是窗口结束时机器人还在握手,不算 fail/stuck。

### 关键观察
1. **没有 fail / stuck** —— 4 档总计 1220 次成功登录,0 失败 0 卡住
2. **avg_login 随并发上升下降** —— 100→69 ms,JIT/cache warmup 主导,真实瓶颈远未到
3. **max_login 在 500 档降到 163 ms** —— 50/100/200 的 ~340 ms 是首次启动冷启动 outlier(JVM/etcd channel 首次初始化),之后稳定
4. **msg 吞吐**: 500 档 99 msg/s,大部分是周期性心跳/动作

---

## 三、本机环境的限制

这是一台 **Windows 开发机** 的基线,生产容量更高:
- 单 cpp gate 进程,Windows 无 sysctl tuning(详见 [gate-kernel-tuning-runbook.md](../ops/gate-kernel-tuning-runbook.md)),Linux 生产 ~5x 余量
- 单 docker kafka(单 broker),Linux 生产至少 3 broker
- 单 zone 配置,生产横向多 zone

---

## 四、过程中复现的预存 Bug(已记录,不阻塞登录路径)

### #B-1 robot 退出时不发 Disconnect → player_locator session 永远 ONLINE
- robot 进程退出 / 被 kill 时,**没有主动发 `ClientPlayerLogin.Disconnect`**
- cpp gate 检测到 TCP 关闭也只是清自己的 SessionInfo,没把 Kafka 消息转给 player_locator
- 结果:`player:session:{playerId}` 永远 `state=ONLINE`,下一轮重启 robot 时 `DecideEnterGame` 命中 ONLINE → ReplaceLogin → KickPlayer 给"幽灵 gate" → 新 gate 永远收不到 BindSession → robot 等 scene-ready 超时
- **生产环境**:玩家正常拔网/退出会走 TCP RST,gate 能监听到 fin/rst → 触发完整 cleanup,所以**生产无症状**;只是 dev 反复跑机器人时遇到
- **解法**(下个迭代):
  1. robot main `defer` 里发 Disconnect 再退出(直接的修法)
  2. cpp gate 在 TCP close 时,通过 Kafka 发 `PlayerDisconnected` 给 `player_locator` topic(架构上的修法,见 player_login_flow.md §2)
- 已加到 docs/design/architecture-current-state-vs-gaps-2026-05.md 缺口清单

### #B-2 docker kafka 在测试机上偶发 Exited (255)
- 跑测试中段 kafka 容器自己 die(`docker ps -a` 看 Exited 49 minutes ago)
- 重启后必须**重启 cpp gate** 让 rdkafka client 干净 bootstrap(看到 `Topic gate-0 partition count changed from 1 to 0` 是恢复信号但不充分)
- 生产 kafka 集群多 broker,偶发不会影响登录,但 dev 单 broker 要警惕

### #B-3 EnterGame 默认 decision = ReplaceLogin,zset 是空的
- player_locator 的 lease ZSET 在测试中保持空,但 GetSession 直接读 `player:session:{id}` key 仍能命中残留
- 这两个存储不一致——zset(lease 监控)和 session key(权威)各管一摊;残留不影响在线人数统计但会让 Decide 走错分支
- **观察**:这是 `LeaseMonitor` 的 watchdog 设计,session key 是事实源,lease ZSET 只用于 30s 重连超时 trigger

---

### #B-4 1k+ 阶梯压测在 Windows dev 跑不动

- **状态:** 仅 dev,生产 Linux + 多 broker kafka 不会触发
- **应对:** **1k/2k/5k 必须在 Linux staging 跑**,直接用 `tools/scripts/stress-linux-tier.sh`(本仓库已附),把结果追加到本文 §二
- **当前基线:** Windows dev 单实例 50/100/200/500 全 0 失败,延迟 69-101ms

---

## 四点五、Bucket4j 限流首跑命中验证(2026-05-10)

L 阶段在同机 staging 上首次打开 `gate.rate-limit.enabled=true` 跑了三层命中验证,验证 Bucket4j → Redis 通路在生产意图下工作正常。**全部用 curl 直打 `/api/login`**,不需要 cpp gate / 真账号。

### 启动参数(临时小阈值便于触发)

```bash
java \
  -Dgate.rate-limit.enabled=true \
  -Dgate.rate-limit.zone-default-rps=2 \
  -Dgate.rate-limit.zone-default-burst=4 \
  -Dgate.rate-limit.ip-rps=2 \
  -Dgate.rate-limit.ip-burst=4 \
  -Dgate.rate-limit.account-cooldown-ms=5000 \
  -jar gateway-node.jar
```

启动日志确认通路:`Bucket4j Redis client -> localhost:6379` + `Started GatewayNodeApplication in 17.246 seconds`。

### 三层命中

| 测试 | 输入 | 期望 | 实际 |
|---|---|---|---|
| **Zone bucket**(zone=1, burst=4, rps=2) | 10 个不同 IP/account 立即 burst | 头 4 通过,后续被 zone bucket 拒 | **6 × code:0,4 × code:100 (QUEUEING)** ✅ <br> 等待中又 refill 2 个,所以放过 6,余下 4 排队 |
| **IP bucket**(burst=4, rps=2) | 同 IP `10.99.99.99` + 10 个不同 account | 头 4 通过,后续被 IP bucket 拒 | **5 × code:0,5 × code:100** ✅ |
| **Account cooldown**(5000ms) | 同 account 5s 内连续 2 次 | 第二次立即拒 | 第一次 `code:0` + access/refresh token;**第二次 `code:429 ACCOUNT_COOLDOWN`** ✅ |

### 关键证据(curl 实际响应片段)

> 历史压测凭据已经脱敏；该片段只证明当时的限流行为，不代表当前 password 认证可使用示例口令。

```http
# Account cooldown 命中
$ curl -s -X POST http://127.0.0.1:8081/api/login \
       -H "X-Forwarded-For: 10.88.88.88" \
       -d '{"zone_id":1,"account":"acct_dup","password":"x"}'
{"code":0,"players":[],"access_token":"<redacted>","refresh_token":"<redacted>","access_token_expire":1778410845,"refresh_token_expire":1780995645}

$ curl -s -X POST http://127.0.0.1:8081/api/login \
       -H "X-Forwarded-For: 10.88.88.88" \
       -d '{"zone_id":1,"account":"acct_dup","password":"x"}'
{"code":429,"message":"ACCOUNT_COOLDOWN"}
```

### 结论

- 三层互不干扰、各自命中(zone QUEUEING / IP QUEUEING / account COOLDOWN)
- Bucket4j 经 Lettuce 写到 Redis 的 CAS 路径在 Linux + Windows 均能正常工作(本机 docker redis 验证)
- 生产 staging 切 T+1 时可以放心 `enabled: true`,真实阈值参考 [open-server-rate-limit-design.md §五](./open-server-rate-limit-design.md)

---

## 四点六、Gateway-only mock 压测(跳过 cpp gate,2026-05-11)

用 `tools/scripts/gateway-mock-stress.sh` 只打 `/api/login`(绕开 cpp gate TCP 链路),看 **Java Gateway + go-zero login 单实例自身**的天花板。复现命令:

```bash
PER_TIER_SEC=10 ./tools/scripts/gateway-mock-stress.sh 100 500 1000
```

### 结果

| tier(并发)| sent | ok_200 | 429 | curl_fail | avg | p50 | p95 | p99 | max | duration |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 100 | 200 | 200 | 0 | 0 | 8 ms | 6 ms | 22 ms | 31 ms | 33 ms | 10 s |
| 500 | 500 | 500 | 0 | 0 | 6 ms | 5 ms | 19 ms | 23 ms | 47 ms | 19 s |
| 1000 | 1000 | 1000 | 0 | 0 | 6 ms | 4 ms | 18 ms | 25 ms | 31 ms | 41 s |

### 为什么实测 req/s 不随并发线性增长

**瓶颈在压测客户端,不在 Gateway**:
- 每次 curl 是一个全新进程(fork + DNS + TCP + HTTP/1.1 握手)
- xargs `-P 1000` 同时拉起 1000 个 curl,Windows 进程调度本身就慢,fork 时间主导
- 真实 SDK 客户端走 HTTP/1.1 keep-alive 或 HTTP/2 多路复用,吞吐应 10x+

### Gateway 实际什么状态

**avg 6 ms、p99 25 ms、0 fail**:完全不吃力。参考:
- Bucket4j 默认 `zone-rps=500 + burst=1000`,这档实际触发 QUEUEING=0,远未到限流
- p99 25 ms 基本都是 login.rpc 过来一圈(Gateway HTTP → gRPC → go-zero login → RedisLocker → token mint → return)
- 本机 docker + Windows 的这个数字 → Linux staging 生产单实例 **5–10x 这个容量**,单实例 ≥ 200 req/s 真实可维持

### 这次跑法的限制

1. **没测战斗后 in-game RPC**:因为 cpp gate 不参与,`EnterGame/LeaveGame/CreatePlayer/Disconnect` 这些 gate→login RPC 没被压到。真完整 mix 由 `stress-linux-tier.sh` 在 Linux staging 覆盖。
2. **避开 account_cooldown**:账号每次唯一(`gw_stress_$$_<idx>`)—— 真玩家重试时 5s cooldown 会压实际吞吐。
3. **无 wave 调度**:wave 默认关,`code:100 QUEUEING` 列全 0。测波次单独把 `gate.rate-limit.wave.enabled=true` 配起来重跑。

### 用途定位

这个数字是 **Linux staging 真跑前的 sanity check** —— 证明 Gateway/login chain 本身不是瓶颈,届时 cpp gate 端的 ephemeral port / kafka / CPU 才是真正的拐点。两个数据联合读:

| 维度 | §二(cpp gate 端到端) | §四点六(Gateway-only) |
|---|---|---|
| 测的东西 | 全栈 HTTP → gate TCP → login | HTTP → login(跳过 gate) |
| 主要瓶颈 | cpp gate ephemeral port / kafka | 客户端 curl 速度 |
| 用途 | 找真实生产拐点 | 证明 Gateway 不是瓶颈 |

Linux staging 真跑见 [docs/ops/linux-staging-stress-runbook.md](../ops/linux-staging-stress-runbook.md) §C。
数据归档: `run/logs/stress/gateway-mock-20260511-*.csv`。

---

## 五、容量推论(粗算)

按 500 档结果反推单机容量上限(本机 Windows dev):

| 资源 | 当前用量 (500 bots) | 估算上限 |
|---|---|---|
| login QPS | ~50 (1000 bots/30s × 1.5×reconnect) | go-zero 单实例 ≥1.5 万,**离顶 300x** |
| Java Gateway QPS | ~50 (与 login 同) | Spring Boot 单实例 1 万,**离顶 200x** |
| cpp gate 长连接 | 600 | muduo 单进程 5 万,**离顶 80x** |
| cpp gate ephemeral port | <100 (`ss -s` Bound 估) | 64K 上限,**离顶 600x** |
| kafka gate-0 partition | 1 | 多分区可线性扩 |

→ **本机 dev 单实例的真实极限大概在 5000-10000 bots**。Linux 生产配 sysctl + 多 gate + 多 zone,**单 zone 5 万玩家是合理目标**。

下一步阶梯应跑到 1000 / 2000 / 5000,看哪个组件先翻车(预期是 cpp gate CPU 或 kafka 单 broker)。

---

## 六、复现命令

```bash
# 一次性环境(首次)
docker compose up -d
dev.bat build
dev.bat start            # cpp gate + scene + go-svc
cd java/gateway_node && mvn -DskipTests package spring-boot:repackage
java -jar target/gateway-node-0.0.1-SNAPSHOT.jar &

# 每轮压测前 wipe(避免 #B-1 残留)
docker exec redis redis-cli FLUSHDB

# 跑某一档
sed "s/REPLACE_ME/500/" robot/etc/robot.stress-template.yaml > robot/etc/robot.stress-500.yaml
cd robot && timeout 30 ./robot.exe -c etc/robot.stress-500.yaml
```

完整自动化脚本:本文档头注释里附的 bash 阶梯循环。

---

## 关联文档
- [docs/design/ARCH.md](./ARCH.md) §11 决策表(本压测确认决策 #9-#13 的正确性)
- [docs/design/stress-test-2026-05-ephemeral-port.md](./stress-test-2026-05-ephemeral-port.md) — 老 gate 路径压测复盘
- [docs/design/architecture-current-state-vs-gaps-2026-05.md](./architecture-current-state-vs-gaps-2026-05.md) — 缺口清单
- 数据: `run/logs/stress/results.csv`, `run/logs/stress/run-{N}.log`
