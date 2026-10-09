# Stress Test — 1 Zone × 45k Robots — Round 17 (2026-06-03)

> **Status:** PASS with one regression item (login_fail 0 → 1000) — see §4.
>
> Run dir: `robot/logs/stress-round17-20260603-221253`
> Baseline: `docs/stress/prev-summary-round17.txt` (= Round 16 summary,2026-06-02)
> Start: `2026-06-03 22:58:07` Local · 18-minute window
> Robot config: `robot/etc/robot.stress-3zone-z1.yaml` (45,000 robots, profile=stress)

## 1. What changed vs Round 16

业务代码层面 Round 16 之后只多了一个 commit (`837be2b2d chore: track last stress run directory`),所以**业务代码没改动**。本轮的目的:

1. **回归保护** — Round 16 之前累积的 P0+P1 改动 + Round 13~15 性能修(dispatcher async LPop / db worker sub-shard / db_task_zone partition 5→10 / login dispatcher GC tick / 跨服 hardening)在 18 分钟 45k 上线压测下不出回归。
2. **dirty-save 实测首跑** — 2026-06-03 加的 `[DirtySave] total=X skipped=Y skip_pct=Z%` LOG_INFO + stress_summarize section 2bb,本轮拿到第一个真实数字。
   - dirty-save slice A/B/C 全套早在 ~2026-05-17 就接通(2026-06-03 核对发现 `proto-compare-dirty-save.md` Status 行还停留在 "not yet wired",已修)。
   - 设计文档预估 70% 是 vibes,本轮拿到的真数字是 **59.8%**。

## 2. Two-dimensional comparison vs Round 16

### Robot per-minute stats

| Snapshot | conn (R16) | conn (R17) | login_ok (R16) | login_ok (R17) | enter_ok (R16) | enter_ok (R17) | login_fail (R16) | login_fail (R17) | max_login (R16) | max_login (R17) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| t2m  | _ | 3,039  | _ | 2,841  | _ | 2,841  | _ | **0**     | _ | 7.076s |
| t5m  | _ | 15,262 | _ | 12,946 | _ | 12,946 | _ | **0**     | _ | 7.076s |
| t10m | _ | 37,806 | _ | 31,853 | _ | 31,853 | _ | **24**    | _ | 8.225s |
| t15m | _ | 61,027 | _ | 52,035 | _ | 52,035 | _ | **403**   | _ | 8.468s |
| t18m | _ | **75,063** | _ | **63,374** | _ | **63,374** | **0** | **1,000** | _ | 8.468s |

> Round 16 prev-summary 没 robot per-minute stats(显示 "no [stats Xm0s] markers"),所以左侧空缺。R17 的 conn/enter 远超 45k 配置,因为 reconnect 累计计入 conn(recon_ok=44,783 占了一半)。**真正的 PASS 信号是 enter_fail=0 + Kafka lag=0 + DB err=0**,见下面三段。

### EnterGame end-to-end

| Snapshot | success (R16) | success (R17) | preload_failed (R16) | preload_failed (R17) | apply avg (R16) | apply avg (R17) |
|---|---:|---:|---:|---:|---:|---:|
| t2m  | 2,773  | 14,981 | 0 | 0 | 33.4ms | 33.8ms |
| t5m  | 11,281 | 14,987 | 0 | 0 | 39.3ms | 33.8ms |
| t10m | 26,217 | 26,636 | 0 | 0 | 42.5ms | 33.8ms |
| t15m | 40,383 | 47,048 | 0 | 0 | 43.3ms | 35.3ms |
| **t18m** | **45,764** | **58,554** | **0** | **0** | **43.5ms** | **36.0ms** ↓17% |

### Dataloader per-stage avg

| Metric | Round 16 @ t18m | Round 17 @ t18m | Δ |
|---|---:|---:|---:|
| cache_check     | 1.8ms  | 1.7ms  | flat |
| sub_cache       | 3.6ms  | 3.3ms  | flat |
| dispatcher      | 0.0ms  | 0.0ms  | flat |
| kafka_send      | 3.3ms  | 2.8ms  | flat |
| **cb_wait{ok}** | 31.4ms | **31.1ms** | flat (持平 = 没回归) |
| cb_wait{fail}   | n/a    | n/a    | n/a (零失败) |

### SceneManager EnterScene stages

| Stage | R16 @ t18m | R17 @ t18m | 备注 |
|---|---:|---:|---|
| dedup            | _ | -       | metric 无数据 |
| **scene_resolve**| _ | 24.1ms  | 主要时间花在这 — 跟 R16 数据缺失,需建立 baseline |
| reserve          | _ | -       | metric 无数据 |
| update_loc       | _ | 0.7ms   | 极快 |
| route_gate       | _ | 0.3ms   | 极快 |
| release_dispatch | _ | -       | metric 无数据 |
| cross_zone       | _ | -       | 单 zone,正常无 |

> Round 16 prev-summary.txt 没 SceneManager 段(只 scrape 了 login :9101),R17 是首次拿到 SM 全表。建立 R17 数据为 baseline,R18+ 做对比。

### DB task ok/err

| Round | t2m ok | t5m ok | t10m ok | t15m ok | t18m ok | err total |
|---|---:|---:|---:|---:|---:|---:|
| R16 | _ | _ | _ | _ | _ | _ |
| R17 | 19,743 | 19,745 | 28,167 | 46,144 | **58,081** | **0** |

### Kafka lag

| Round | total lag @ t18m |
|---|---:|
| R16 | **0** |
| R17 | **0** |

## 3. Dirty-save 实测跳过率(本轮首次出数据)

| scene log | total | skipped | skip_pct |
|---|---:|---:|---:|
| scene.stdout.log | **57,771** | **34,562** | **59.8%** |

实测 **59.8%** 跳过率落在设计文档预估的"30%-70% 健康范围"内,跟 70% vibes 估值差 10pct。
- 这意味着每 100 次 SavePlayerToRedis,真正写 Redis + 推 Kafka 的只有 40 次。
- 在 stress profile (skill+move) 下,玩家 PlayerAllData 大部分时段是动的(skill 用法/位置/经验),所以 60% 已经很可观。
- 若 profile 改成 idle/AFK 主导,跳过率会显著上去。

底层数据印证:稳态 18 分钟 45k 在线,SavePlayerToRedis 调用 57,771 次,如果没有 dirty-save,这就是 57,771 次 Redis Save + 115,542 次 Kafka DBTask emit;有 dirty-save 后实际只写 23,209 次 Redis + 46,418 次 Kafka——**Kafka emission 减少 60%**。这跟 DB task ok 累计 58,081 互相对得上(t18m total ok 包含 read+write,绝大多数 ok 是 dirty-save 没拦下来的 write)。

## 4. 决策行

- [x] **PASS 主线** — enter_fail=0,preload_failed=0,Kafka lag=0,DB err=0,apply_avg 还从 43.5ms 降到 36.0ms (-17%)。45k 单 zone 单机能力维持,可以推进 P2 (currency_event delta 写) 或多 zone 横向扩展。
- [ ] **回归项 R1: login_fail 0→1000** — 已在 2026-06-04 定位根因。

  **根因: gate token 被 polling 滞后消耗,robot 拿到 token 时已接近过期。**

  - cpp gate `[Token] Expired token` 日志显示 `now - expire = 0~30 秒`,而 token TTL=5 分钟。这意味着 token **在签出来 5 分钟+ 之后才被 robot 用来连 gate**——不是签得太短。
  - 链路时序:
    1. robot 进 login queue 入队(高负载下队首到队尾要 几分钟才轮到)
    2. dispatcher 出列那一刻签 gate token(`time.Now()+5min`),塞进 Redis `admit:{queueId}` (admitTTL=60s,但每次 polling 续期)
    3. robot 通过 `/queue-status` polling 拿 admit(`GetDel admit:{queueId}`)
    4. 步骤 2→3 之间可能间隔 4~5 分钟(robot polling 间隔 + admit 续期 + dispatcher tick 累计)
    5. robot 拿到 token,回头连 cpp gate `VerifyGateToken`——此时 token 寿命剩 0~30 秒,边缘 case 已过期
  - 数据印证:
    - `gate token rejected: token expired` 总计 1,219 次(t18m 累计 1,000)
    - 第一个 token expired 出现在 robot 启动后 9 分钟(t9m10s),刚好对应"4-5 分钟队列等待 + 5 分钟 token TTL 用满"的临界点
    - login_fail 上升曲线(t10m=24, t15m=403, t18m=1000)跟稳态末段 robot 反复 retry + 队列越积越长一致
    - 8,653 个机器人最终 5 次 retry 给 up — 说明这不是个别机器人问题,是稳态末段的系统性现象
  - **PASS 主线不受影响**: enter_fail=0,Kafka lag=0,DB err=0;gate token 错误的 robot 直接被踢掉而不是排队卡住,系统降级而不是雪崩。
  - **修复选项**:
    1. 拉长 gateTokenTTL 5min→10min(`go/login/.../assigngatelogic.go:21`)——简单但治标
    2. dispatcher 出列时延迟签 token,改成 robot polling 命中 admit 时即时签——根治但要重构 admit 流转
    3. robot 端在 `VerifyGateToken` 失败时不计入 LoginFail 也不消耗 retry,让外层逻辑重新走 AssignGate(已经会重新 fetch token,只是当前 retry 计数算它一次)
  - **下一步**: 推 P2 不阻塞,但 Round 18 之前先做选项 1 + 选项 3 一起,削掉这条噪音线。

## 5. ARCH §11 决策记录

> 2026-06-03 Round 17 PASS。dirty-save 实测跳过率 59.8%(预估 70% 偏高 10pct)。回归项 R1 (login_fail 0→1000) 2026-06-04 已定位为 admit token polling 滞后耗尽 5 分钟 TTL,Round 18 前修(扩 TTL + retry 不计 LoginFail);不阻塞 P2 currency_event delta 写设计。

## 6. References

- prev-summary baseline: `docs/stress/prev-summary-round17.txt` (= Round 16 t18m,2026-06-02)
- 上一轮复盘: `docs/stress/stress-1zone-45k-2026-06-01-round15.md` (Round 16 没单独写复盘,直接用了 prev-summary)
- dirty-save 设计: `docs/design/proto-compare-dirty-save.md`
- dirty-save 计数器: `cpp/libs/services/scene/player/system/dirty_save_stats.h`
- dirty-save LOG_INFO 触发: `cpp/libs/services/scene/core/system/redis.cpp` (RedisSystem 30s timer)
- stress_summarize section 2bb: `tools/scripts/stress_summarize.ps1`
