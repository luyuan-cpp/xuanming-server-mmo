# 3 Zone × 15000 压测 — Runbook(2026-05)

> 单机 3 进程,合计 45000 robot,覆盖 4 个场景:开服风暴 / 在线稳态 / 跨 zone 调度 / 重连。
> 配套脚本:`robot/scripts/run-stress-3zone.{sh,ps1}`,yaml:`robot/etc/robot.stress-3zone-z{1,2,3}.yaml`,seed:`java/gateway_node/src/main/resources/seed_stress_3zones.sql`。

---

## 0. 为什么是 3 zone × 15k 而不是 1 zone × 45k

- 单 zone 45k 的瓶颈在 **scene 承载** + **single-host ephemeral port**;先打满一个再扩,问题混在一起难判。
- 3 zone × 15k 让 Gateway / login / Kafka / scene_manager 路由层先承压,scene 自己只承担 15k —— 跟历史 500/1k 压测可线性比对。
- 真要打 1 zone × 45k,把 z2/z3 yaml 的 `zone_id` 都改成 1,robot_count 各 15000,**别动账号前缀**(`z2bot_*` / `z3bot_*` 仍要错开,否则同账号在同 zone 会互相顶号)。

---

## 1. Pre-flight(顺序很重要)

### 1.1 数据库:扩 zone

```bash
mysql -u root -p mmorpg < java/gateway_node/src/main/resources/seed_stress_3zones.sql
# 期望输出:zone_id 1/2/3,capacity=20000 各一条
```

### 1.2 内核(Linux 施压机):**不调跑不到 5k**

```bash
sudo sysctl -w net.ipv4.ip_local_port_range="1024 65535"
sudo sysctl -w net.ipv4.tcp_tw_reuse=1
sudo sysctl -w net.core.somaxconn=65535
sudo sysctl -w net.ipv4.tcp_max_syn_backlog=65535
ulimit -n 200000   # **同一个 shell 里**,启动脚本前必须做
```

不开 `tcp_tw_recycle`(NAT 会丢)。完整背景见 [gate-kernel-tuning-runbook.md](gate-kernel-tuning-runbook.md)。

### 1.3 内核(Windows 施压机,管理员 PowerShell)

```powershell
netsh int ipv4 set dynamicport tcp start=1024 num=64511
reg add HKLM\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters /v MaxUserPort       /t REG_DWORD /d 65534 /f
reg add HKLM\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters /v TcpTimedWaitDelay /t REG_DWORD /d 30    /f
```

### 1.4 Server 端:确认 Bucket4j 限流不会卡 robot

`java/gateway_node/src/main/resources/application.yaml` 里的 `bucket4j.assign-gate.capacity` 至少 ≥ 5000(我们要打的不是限流,是后端真实承载)。打限流另起一次专项,别混。

### 1.5 Robot 端:HTTP 连接池已修

代码侧已经把 `http.DefaultClient` 换成 `sharedHTTPClient`(`MaxIdleConnsPerHost=4096`),首次跑前确认编译通过即可:

```bash
cd robot && go build -o robot.exe .
```

---

## 2. 启动

### Linux

```bash
cd robot
ulimit -n 200000
./scripts/run-stress-3zone.sh
```

### Windows

```powershell
cd robot
.\scripts\run-stress-3zone.ps1
```

启动行为:
- 3 个进程错开 1s 起,每个进程内部还有 `time.Sleep(50ms)` 错峰 → 真实斜坡是 ~750s 进满 15k(单进程)。
- 每个进程独立 cwd:`logs/stress-3zone-<ts>/z{1,2,3}/`,避免 `behavior_test_results.csv` 互相覆盖。
- Ctrl-C 走 `signal.Notify` → `LeaveGame` → 关 Kafka session,**不要 kill -9**(残留 ONLINE session 会污染下一次跑)。

---

## 3. 4 个场景的判读标准

### 场景 A — 开服风暴(0~5min)

**关注指标(每个 zone log):**

| 指标 | 期望 | 异常信号 |
|---|---|---|
| `connected` 增速 | ~20/s/进程(50ms stagger) | <5/s → robot 在 retry,看 `login_fail` |
| `queue_entered` | ≈ 0(capacity=20k,15k 不该排队) | >0 → Bucket4j 限流卡住了,调 1.4 的 capacity |
| `login_ok` 累计 | 15k × 3 = 45k | <40k → 看 `login_fail` 错误分布 |
| `avg_login` | <500ms | >2s → Java Gateway 或 go-zero login 慢,jstack/pprof |
| ephemeral port 用量 (`ss -s` Linux) | <30k | >50k → tcp_tw_reuse 没生效,检查 1.2 |

**典型踩坑:**
- 第 5k 个 robot 起开始 `dial: cannot assign requested address` → 1.2 内核没生效或 ulimit 没改,没救,停掉重来。
- `queue_entered` 突然飙到上千 → 检查 `seed_stress_3zones.sql` 是否真的执行了(capacity 没改的话默认是 5000)。

### 场景 B — 在线稳态(5~30min,所有 robot 已进入)

启动脚本不会自动停,**让它跑 30min**。

**关注:**
- gate 进程 RSS / CPU 是否平稳 → 不平稳意味着有泄漏,重点看 `SessionInfo` map。
- scene tick 周期(scene log)是否稳定在 50ms → 漂到 80ms+ 说明 ECS 满载,15k/zone 是上限。
- Kafka `gate-{id}` topic 的 lag → robot 端动作(profile=stress)会持续打消息,lag 应 ~0。
- Redis QPS(主要是 access_token 校验):稳态期应 <500/s,大幅高于说明有 robot 在重连循环。

### 场景 C — 跨 zone 调度

**单独跑,不和 A/B 混。** 改一份 yaml:

```yaml
# robot.stress-crosszone.yaml
gateway_addr: "http://127.0.0.1:8081"
zone_id: 0           # 让 server-list 自动分配
robot_count: 30000
account_fmt: "xzbot_%05d"
# ... 其他同 z1
```

zone_id=0 走 `resolveZoneIDLocal` → 拿 recommended zone(seed 里是 zone-1)。等 zone-1 被打满 capacity=20000 后,**手动**改 zone-1 `recommended=0` + zone-2 `recommended=1`,看 robot 端 `login_fail` 是否会自动 retry 到 zone-2。

**判读:** 
- robot retry backoff 内能切到 zone-2 → 调度正常。
- 卡死在 zone-1 → `resolveZoneIDLocal` 没在 retry path 里被调,需要改 robot 代码(目前实现:zone_id 在 cfg 里 cache,只在启动时拿一次)。这是已知缺口,跑出来记下来。

### 场景 D — 重连路径

**前置:** A/B 跑 5min 进入稳态后,在另一个 shell 里:

```bash
# 杀 z1 robot 进程(留 z2/z3 不动),观察 30s 重连窗口
kill -9 <z1_pid>
```

**期望:**
- z1 的 15000 个 player 在 login 那一侧被标 `Disconnecting`,30s 内不会被清。
- 此时**单独**起一个小 robot(同账号前缀 `z1bot_*`,robot_count=100),应走 `enter_gs_type=3 RECONNECT` 路径。
- 30s 后再起,应走 `enter_gs_type=2 REPLACE`(顶号)。

**怎么验证 enter_gs_type:** scene log 里 `GateLoginNotify` 那行有 `enter_gs_type=` 字段。grep 即可。

---

## 4. 复盘产出

每次跑完,`logs/stress-3zone-<ts>/` 里至少应该有:

```
SUMMARY.txt                 # 启动脚本自动生成,3 zone 末尾 5 行 stats
z1/z1.log + z1.stdout       # zap dev-logger 全量输出
z1/behavior_test_results.csv # 每 robot 一行:动作分布、消息数
z1/behavior_test_results.jsonl
z2/...
z3/...
```

**最少要看的 3 件事:**

1. SUMMARY.txt 的 `connected=` / `login_ok=` —— 三个 zone 都到 15000 了吗
2. `grep -E "ERROR|FATAL" z*/z*.log` —— 错误集中在哪一步(login / enter / scene_ready)
3. CSV 的 `msg_recv` 列 —— 应该 >>> `msg_sent`(scene 推送的广播)。如果接近 1:1 说明大量 robot 卡在 scene_ready 之前,profile=stress 没真正跑起来。

发现问题 → 写到 `docs/stress/stress-test-3zone-2026-05-<topic>.md`,**不要更新本 runbook**(这是 SOP,不是日志)。

---

## 5. 已知不打这次的范围

- **真实多机施压**:本 runbook 是单机 3 进程。多机要解决 Gateway HMAC 时钟一致性 + zone seed 共享问题,另立 runbook。
- **限流场景**:打 Bucket4j 限流要把 capacity 调小,跟在线稳态目标冲突,单独跑。
- **DB 持久化压测**:`mode: data-stress` 才走 verifier 路径,本次是 `mode: stress`,不验数据落地。
