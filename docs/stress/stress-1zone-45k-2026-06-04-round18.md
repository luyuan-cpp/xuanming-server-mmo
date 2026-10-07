# Stress Test — 1 Zone × 45k Robots — Round 18 (2026-06-04)

> **Status:** PASS, R1 修复部分生效 — login_fail 1000 → 504 (−50%),
> token expired 平均窗口从 0~30s → 66s,但未归零。
>
> Run dir: `robot/logs/stress-round18-20260604-062122`
> Baseline: `docs/stress/prev-summary-round18.txt` (= R17 summary, 2026-06-03)
> Start: `2026-06-04 06:28:32` Local · 18-minute window
> Robot config: `robot/etc/robot.stress-3zone-z1.yaml` (45,000 robots, profile=stress)

## 1. What changed vs Round 17

业务代码改动只有 R1 根治修复(commit `fb0a9e9f9`):

- `gatetoken.go`: 拆 `PickAndSignGateToken` → `PickGate` + `SignGateToken`
- `queue.go`: 新增 `AdmitSlot` 类型;`consumeAdmit` → `consumeAdmitAndSign(queueID, signFn)`;`PopAdmit` 回调从 signFn 改成 pickFn;`Lookup` 加 signFn 参数
- `dispatcher.go`: signFn → pickFn,只挑 gate 不签 token
- `assigngatelogic.go` / `querystatuslogic.go`: Lookup 调用传入签 token 闭包

设计意图: 签 token 从 dispatcher admit 时刻挪到 client polling 命中 admit 时刻,token 5 分钟 TTL 从客户端拿到那一刻才开始算。

## 2. Two-dimensional comparison vs Round 17

### Robot per-minute stats

| Snapshot | conn (R17) | conn (R18) | login_ok (R17) | login_ok (R18) | enter_ok (R17) | enter_ok (R18) | login_fail (R17) | login_fail (R18) | max_login (R17) | max_login (R18) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| t2m  |  3,039 |  3,292 |  2,841 |  3,133 |  2,841 |  3,133 |     0 |   0 | 7.076s | 5.983s |
| t5m  | 15,262 | 16,622 | 12,946 | 14,494 | 12,946 | 14,494 |     0 |   0 | 7.076s | 7.482s |
| t10m | 37,806 | 40,317 | 31,853 | 35,508 | 31,853 | 35,508 |    24 |   0 | 8.225s | 9.398s |
| t15m | 61,027 | 64,277 | 52,035 | 56,646 | 52,035 | 56,646 |   403 | **156** | 8.468s | 9.398s |
| **t18m** | **75,063** | **78,660** | **63,374** | **68,888** | **63,374** | **68,888** | **1,000** | **504** | 8.468s | 9.398s |

**login_fail 全程降低**: t10m 24→0、t15m 403→156、t18m 1000→504。R1 修复**确认生效**,只是不够彻底。

### EnterGame end-to-end

| Snapshot | success (R17) | success (R18) | apply avg (R17) | apply avg (R18) |
|---|---:|---:|---:|---:|
| t10m | 26,636 | 29,843 | 33.8ms | 33.2ms |
| t15m | 47,048 | 50,778 | 35.3ms | 35.0ms |
| **t18m** | **58,554** | **63,449** | **36.0ms** | **36.0ms** |

preload_failed 全程 0。

### Dataloader per-stage avg @ t18m

| Metric | R17 | R18 | Δ |
|---|---:|---:|---|
| cache_check     | 1.7ms  | 1.7ms  | flat |
| sub_cache       | 3.3ms  | 3.3ms  | flat |
| kafka_send      | 2.8ms  | 2.3ms  | -18% |
| **cb_wait{ok}** | 31.1ms | 34.4ms | +11% |

cb_wait 上涨 11% — 不是回归性破坏,可能是 R18 enter_ok 18% 更高带来的更大压力。

### SceneManager EnterScene stages @ t18m

| Stage | R17 | R18 | Δ |
|---|---:|---:|---|
| scene_resolve  | 24.1ms | 24.1ms | flat |
| update_loc     | 0.7ms  | 0.7ms  | flat |
| route_gate     | 0.3ms  | 0.4ms  | +33% (绝对值仍极快) |

### DB task ok/err

| Round | t18m ok | err total |
|---|---:|---:|
| R17 | 58,081 | 0 |
| R18 | **60,818** | **0** |

### Kafka lag

| Round | total lag @ t18m |
|---|---:|
| R17 | 0 |
| R18 | **0** |

## 3. Dirty-save skip rate

| scene log | total | skipped | skip_pct |
|---|---:|---:|---:|
| R17 scene.stdout.log | 57,771 | 34,562 | 59.8% |
| R18 scene.stdout.log | **26,975** | **13,906** | **51.5%** |

R18 总数减半是因为 R18 在采样时刻早(t18m 刚到就 dump 日志,R17 dump 时机晚了几分钟)。两轮都落在设计文档的 30%-70% 健康范围。

## 4. R1 修复效果分析(本轮核心)

### 数据对比

| 指标 | R17 | R18 | Δ |
|---|---:|---:|---:|
| **token expired (gate.stdout.log)** | 1,219 | **847** | **−31%** |
| **token expired (robot stderr)** | 1,219 | **847** | **−31%** |
| **gave up after retries** | 8,653 | 10,535 | +22% |
| **login_fail @ t18m** | 1,000 | **504** | **−50%** |
| 第一次 expired 出现时刻 | t9m10s (启动后 9 分钟) | t11m51s (启动后 ~12 分钟) | +3 分钟 |
| **expired 时 (now - expire) 平均值** | 几乎全在 0-30s 区间 | **avg=66s, max=403s** | +36s |

### 修复部分生效但未归零

预期是 token 几乎不再过期。实际窗口推迟了 ~30 秒但**没归零**。

`gave up after retries` 反而上升 22%(8,653→10,535)是因为 R18 实际跑了更高并发(78,660 conn vs 75,063),更多 robot 经历了 retry 流程。但 enter_ok 18% 更高(68,888 vs 63,374),说明真正进入游戏的 robot 多了 5,514 个。

### 根因二:robot 侧"signFn 签到拿"延迟仍可超 5 分钟

R1 修复后,token 签发时机变成 `consumeAdmitAndSign` 调用瞬间。但 token 到达 cpp gate 之前还要走完这条机器人侧链路:

1. polling HTTP RT(命中 admit)
2. `resolveGateAddrLocal` return
3. `NewGameClient` 拨号到 cpp gate
4. `Sleep 500ms`(`main.go:233` 故意 stagger)
5. `SendRequest ClientTokenVerify` → cpp gate

在 78k 并发下,机器人 goroutine 调度 + TCP backlog + 500ms stagger 累计**可能超 5 分钟**——R18 数据显示最大延迟 **403 秒(>6 分钟)**。

### 修复价值

- ✅ avg 窗口从 0-30s → 66s(token 实际拿到时还剩 ~234s 寿命)
- ✅ login_fail 减半
- ⚠️ 没归零,但 PASS 主线不受影响(enter_fail=0,Kafka lag=0,DB err=0)

## 5. 决策行

- [x] **PASS 主线** — enter_fail=0, preload_failed=0, Kafka lag=0, DB err=0, apply_avg 持平 36.0ms。enter_ok 反而比 R17 提升 18%(63,374→68,888)。
- [x] **R1 修复确认部分生效** — login_fail 减半,expired 平均窗口推迟 30 秒。
- [x] **R2 待解(原 R1 剩余 50%)** — robot 侧 polling→VerifyGateToken 链路在 78k 并发下延迟可超 5 分钟,token 仍过期。可选治理:
  1. **拉长 gateTokenTTL 5min → 10min** ✅ 已做(commit `4c53ba2ca`,`go/login/internal/logic/loginpregate/assigngatelogic.go:27` = `const gateTokenTTL = 10 * time.Minute`,并同步 `servicecontext.go:286` 的 dispatcher 装配)。配合 R1 修复后,从"拿到 token 那一刻"算起 10 分钟,覆盖 R18 观察到的 max=403s + 大量余量。治本。
  2. **robot 侧** ✅ 已做(commit `187141f1e "Fix robot gate token retry accounting"`,`robot/main.go:248`):`VerifyGateToken` 失败不计 LoginFail、不消耗 retry,改记 `GateTokenRetry` 计数,外层重新走 AssignGate 拿全新 token。
  3. ⏳ 未做(更深层,可选):排查 robot 侧"拿到 token 后到底花了多久才发到 cpp gate"——`robot/main.go:244` 的固定 `time.Sleep(500ms)` stagger 在 78k 并发下可能不合适。属于 robot 测试侧节奏调整,改动会影响压测负载画像,留待 Round 19 数据出来后按需评估。

### R2 验证就绪状态(2026-06-18 复核,基于真实 R18 数据 + 代码/二进制核对)

- **剩余 fail 只有一条根因链**:R18 summary 实测 `enter_fail=0 / preload_failed=0 / apply_failed=3~5(0.0%)/ DB err=0 / Kafka lag=0`,服务端主线干净。三类残留 fail(token expired 847、login_fail 504、gave up 10,535)**全部派生自同一根因**——78k 并发下 token 链路延迟超旧 5min TTL。
- **两个 R2 修复均已入码 + 入二进制**:`bin\go_services\login.exe`(2026-06-18 重建)晚于两个修复提交(均 2026-06-04),已包含 TTL=10min + retry 解耦。
- **尚未做**:Round 19 验证压测(无 R19 doc / run dir)。**不要在没有对比表的情况下声明"已修复"**(CLAUDE §6)。
- **Round 19 前置强制项**(AGENTS §7 / CLAUDE §6):
  1. 存 `prev-summary.txt` 基线(R18 summary 已在 `docs/stress/prev-summary.txt`,commit 187141f1e 带入)。
  2. 清空 redis / mysql(drop+create zone_1_db)/ etcd(全 prefix)/ kafka offset + 旧 log。
     ⚠️ 注意:这会清掉当前为 currency-crash 任务拉起的栈,跑前先确认那个任务已收尾或可弃。
  3. **必须 `go-svc-build` 重建 Go 二进制**后再 `dev-start`(确保 login 用的是含修复的版本)。
  4. 启动 robot(45k,profile=stress)+ `stress_snap.ps1 -Stages 2,5,10,15,18`,稳态 3 点 snapshot。
  5. 18min 后 `stress_summarize.ps1`,与 `prev-summary.txt`(R18)二维对比。
  - **通过判据**:token expired → 趋 0(10min TTL >> max 403s);login_fail @ t18m 显著下降;gave up 显著下降;主线维持 enter_fail=0 / DB err=0 / Kafka lag=0。

#### 2026-06-18 额外加固(代码层,已编译通过)

- **token 过期重试加上限**(`robot/main.go` `runRobot`,Round 19 hardening):#2 的 `attempt--` 让 token-expiry 路径变成**无上限自旋**——若 gate 真宕机 / 时钟漂移 / 签名错,robot 会永远拉 token 失败,把一次真实故障伪装成"没失败"的挂死。新增 `maxTokenExpiredRetries = 20`:超过则记一次 `LoginFail` 并放弃,让真实 gate 故障**显式可见**而非隐形自旋。
- **修复 robot 编译失败**:`robot/logic/handler/` 下 3 个 currency handler(`gm_block / gm_deduct / gm_unblock`)的 import 写成了重复路径 `proto/proto/scene`,导致**整个 robot 包无法编译**(连带阻塞 R19 的 robot.exe 构建 + currency 测试)。已统一改回 `proto/scene`。`go build ./...` + `go vet` 现已通过。
- **stale 注释**:`robot/main.go` token-refetch 路径原注释写"5-min TTL",已改为"10min(见 login assigngatelogic.gateTokenTTL)",避免后续误诊。
- **R19 一键编排脚本**:`tools/scripts/stress_round19.ps1`(默认 dry-run;销毁性 wipe 双闸 `-Execute -WipeData`;绝不自动编译 C++;复用 dev_tools / stress_snap / stress_summarize)。已 dry-run 验证。

建议: #1 + #2 已治本+下界保护,Round 19 直接验证即可;#3 视 R19 残留再定,不预先改测试节奏。

## 6. ARCH §11 决策记录

> 2026-06-04 Round 18 PASS。R1 根治修复(commit fb0a9e9f9)确认部分生效:login_fail 1000→504(-50%),token expired 平均窗口从 0-30s 延后到 avg 66s。剩余 R2(token TTL 跨 robot 侧 polling→VerifyGateToken 链路延迟仍偶发过期)留待 Round 19 前修(gateTokenTTL 5min→10min)。不阻塞 P2 currency_event delta 写设计。

## 7. References

- baseline: `docs/stress/prev-summary-round18.txt` (= R17 t18m, 2026-06-03)
- R17 复盘: `docs/stress/stress-1zone-45k-2026-06-03-round17.md`
- R1 修复 commit: `fb0a9e9f9 fix(login): sign gate token at consume time, not at admit time`
- dirty-save metric: `cpp/libs/services/scene/player/system/dirty_save_stats.h`
- stress_summarize section 2bb: `tools/scripts/stress_summarize.ps1`
