# 跨 Zone 传送 / 归属交接 失败场景测试 Runbook

> **状态**: v2 — 2026-09-20(整篇重写,v1 的四个场景全部作废,见 §0);v2.2 — 2026-09-21 补 S3L1-1 第二层出口的观测点与场景 B3(见 Changelog);v2.4 — 2026-09-21 补 Z1 修复 / 断线释放标记 A′ / GO-5 的观测点与场景 Z、R,并按 A′ 改写 §3.3 / §3.4 / B2 / C1 / C2 / D2 / F3 的期望(代码已落码,C++ 未重新编译、Go 未编译,均未测试;设计文档 §12.6.10 / §12.7);**v2.5 — 2026-09-29 补冻结硬上限 + 晚发闸 +「标记已发出」统一收口(D-2 的修复)、EnterScene 应答关联号、gRPC 失败回调、CPP-2 gate 侧进场转发补发、GO-6 推迟摘除的观测点与场景 H / SE / E2,并改写 §2.3 / §2.4 / §3.2 K-SM / §3.3 / B1 / F3 / §5 / §6 / §7(代码 2026-09-28 已进 main,**全部未编译、未测试;本版同样是静态编写、未实跑**,见 Changelog)**
> **⚠ 静态编写声明**: 本文依据 2026-09-20 的 `main`(`d9e471b80`)**只读代码**写成。它描述的链路(跨 zone 场景传送阶段 1/2/3 + 同 zone 跨节点换图 + 单节点硬崩接管)**尚未编译、尚未实跑**(`docs/design/cross-zone-scene-travel.md` §11 / §11.4)。文中每条日志原文、键名、指标名、错误码都逐字 grep 核对过,但**注入手法的时序、期望现象的先后**是从代码推出来的,没有一条被执行验证过。**首次执行的人发现与实际不符处,应回改本文**(并在 Changelog 记一笔),不要照着错的文档去"修"代码。凡标了「拿不准」的地方,就是首跑时最该记录实际现象的地方。
> **范围**: 归属交接链(`StartTravelHandoff → 冻结 → 存盘 → BeginTravelHandoff 写 handoff 标记 → scene_manager.EnterScene → 放行销毁 / 未成解冻`)与属主接管(`playerLocationOwnerDead`)的失败场景 SOP。设计参考 `docs/design/cross-zone-scene-travel.md` §4 / §6 / §7 / §10.3 / §11,以及 `docs/design/scene-owner-reentry-barrier.md`。
> **执行环境**: 本地双 zone(`dev_tools.ps1 -Command dev-start-zones -Zones 1,2`,共享同一个 docker `redis` / `kafka`)。K8s 双 zone 的对应手法见 §3.5,**未逐条核对**。
> **执行频率**: 归属交接链 / scene_manager 换手门 / owner_epoch 相关代码改动后 + 每个版本上线前。首次执行是**校准轮**:目的是把本文改对,不是给版本放行。
> **执行者**: 按 `AGENTS.md` §10.1,构建与运行由 Codex / 人执行;Claude 只维护本文。
> **目标**: 场景 A–F 的「通过标准」全部满足;§6 列出的「当前预期失败 / 待修」项**不计入通过**,但每次都要如实记录现象。

---

## 0. 历史:v1 测的东西已经不存在,不要把它补回来

v1(2026-05-19)测的是「跨 zone 三件套」:`PlayerFrozenComp` + Kafka `player_migrate` / `player_migrate_ack` 事件 + `[CrossZoneReaper]` 超时重发,现场靠 `metric=migration_start / migration_done / reaper_failed` 日志与 Redis 键 `player_migration:{P}` 判断。

**这条搬数据链已在阶段 3 整条删除**(设计文档 §11.3,决策 CZ-1):`HandleCrossZoneTransfer / HandlePlayerMigration / HandlePlayerMigrationAck`、`cross_zone_reaper.{h,cpp}`、`kafka/system/kafka.{h,cpp}`、`main.cpp` 的两个 topic 订阅与 reaper 启停全部删掉了。源码里现在只剩注释提到它们(`cpp/nodes/scene/main.cpp:258-265`、`player_frozen_comp.h:25`)。照 v1 跑,四个场景必然全失败——**那不是回归,是被测对象没了**。

- ❌ **不要恢复 reaper / `player_migrate` 订阅 / `player_migration:*` 键**。它被删的原因:摸底证实是运行期死代码(全仓无人触发),且与「数据不搬家、按 home_zone 落库」(CZ-1 / CZ-2)冲突。
- 现在「冻结了谁来解」的答案:EnterScene 应答 + 两道 30s 看门狗(存盘看门狗 / 应答看门狗)+ 按 `owner_epoch` 核实去留(`ResolveTravelOutcome`)+「退出优先」+(v2.5)70s 冻结硬上限(`EnforceTravelFreezeCaps`)。不需要 reaper。
- 唯一留下来的是 `PlayerFrozenComp` 本身与约 20 处业务拦写闸(`IsCrossZoneFrozen()`),传送链复用同一道闸。
- broker 上残留的 consumer group `scene-cross-zone-{nodeId}`、topic `player_migrate(_ack)`、Redis 的 `player_migration:*`(TTL 120s)无害,不用清。

v1 原文在 git 历史里(`git log -- docs/ops/cross-zone-failure-test-runbook.md`),需要对照时去那里看,不要贴回本文。

---

## 1. 机制速记(只列和"怎么看现象"有关的)

```
客户端 TravelToZone{B} ─▶ scene(A) RequestZoneTravel(CZ-6 校验,拒绝 = 同步回 tip,不改状态)
  StartTravelHandoff:挂 PlayerTravelHandoffComp + PlayerFrozenComp ─▶ SavePlayerToRedis
     └ 存盘看门狗 30s:存盘一直不落地 → AbortTravelHandoff("handoff save did not land in time")
  存盘落地 ─▶ BeginTravelHandoff:SET player:{P}:handoff "{epoch}:{ms}" EX 300 ─▶ RequestTravelEnterScene
     └ 应答看门狗 30s:应答没到 → ResolveTravelOutcome(先 DEL 标记,再 GET owner_epoch)
  scene_manager.EnterScene(ZoneId=B):换手门(标记.epoch == 当前 owner_epoch)→ Lua 原子「CAS + INCR epoch + 写 location(等待落点)」
     → Kafka RedirectToGateEvent → gate(A) → 客户端 msg 124
  应答带 Redirect ─▶ scene(A) 不存盘销毁实体("travel_redirect")
  应答是错误 / 超时 ─▶ ResolveTravelOutcome(先 DEL 标记,再 MGET owner_epoch location):epoch 没变 → 解冻 + tip;
       epoch 变了 → 已被放行 → 销毁;其中跨 zone、证据是失败应答 / 无应答、玩家不在退出中、location 恰是本次交接的
       等待落点时,销毁之前先发 tip 3027 + 踢线 34([ZoneTravel][ClientReset],设计文档 §12.5.6)
  (v2.5)冻结硬上限,按单调时钟从冻结那一刻起算:冻结满 70s 仍无结论(1s 扫描,最迟约 71s)
       → SET 没发出:解冻 + tip([ZoneTravel][FreezeCap] action=unfreeze,应恒不出现)
       → SET 已发出:tip + 踢线 34 + 不存盘销毁([ZoneTravel][MarkSentDestroy] site=travel_freeze_cap)
     35s 晚发闸:存盘晚于 35s 才落地 → 不写标记、解冻;SET 的 OK 晚于 35s 才到 → 不发 EnterScene、按「标记已发出」收口
     SET 已 OK 之后发现没有 gate 会话 / 没有 scene_manager → 同样按「标记已发出」收口(不再解冻)
客户端跟随 124 → gate(B) → login(B) → scene_manager(第二条腿,再铸造一次)→ scene(B) 从共享 Redis 载入
```

三个事实决定了本文的观测方式:

1. **去留只由 `owner_epoch` 变没变裁决**。应答丢了、超时了、失败了,都不能证明"没被放行"。
2. **标记只由 scene 写、由 scene 撤**;scene_manager 只比对不删除。放行之后标记原样留到 TTL(此时它的 epoch 已落后于当前值,无害)。*v2.4 起(设计文档 §12.6)还有两个 scene 侧读写方*:干净退出收敛后源 scene 写一份**断线释放标记** `"E:<ms>"`(A1′,E == 当前 owner_epoch,EX 300);任何节点**新载入**玩家之前先核 owner_epoch 再删 ≤ 本次路由 epoch 的标记(A2′)。所以"玩家退出后 handoff 键仍在、且 epoch 等于当前 owner_epoch"是 v2.4 起的**正常现象**,下一次载入会把它清掉。
3. **交接发起(标记已写,`requestedAtMs != 0`)之后,源节点不再存盘**(`[SavePlayerToRedis] skip: …`)。所以交接链上不应该出现 `stale_owner_write_rejected`。

---

## 2. 可观测点总表

行号是 2026-09-20 `main` 的位置,会漂;字符串本身是逐字核对过的,改了日志文案的人请同步改这里。

### 2.1 Redis 键

| 键 | 值 | 谁写 | 出处 |
|---|---|---|---|
| `player:{P}:owner_epoch` | 十进制整数;只由 scene_manager `INCR` | scene_manager | `go/shared/ownerepoch/ownerepoch.go:40`、`player_ownership_comp.h:29` |
| `player:{P}:handoff` | `"{epoch}:{saved_at_ms}"`,`EX 300` | 源 scene(C++):交接 / 疏散改派 / **A1′ 断线释放标记**(v2.4);删:撤回、`ResolveTravelOutcome`、**新载入节点的 A2′**(v2.4)| `ownerepoch.go:45`、`player_ownership_comp.h:18` 起、`exit_release_mark.h:31-35` |
| `player:{P}:location` | `PlayerLocation` **proto 二进制**(`redis-cli` 直接读不出字段)| scene_manager | `changesceneutil.go:19` |
| `node:zone:{z}:{n}:death_at` | Unix 毫秒,节点判死时刻,TTL 10 分钟 | scene_manager(leader)| `reentry_barrier.go:37`、`constants/reentry_barrier.go:80` |
| `scene_nodes:zone:{z}:load` | ZSET,活节点负载集 | scene_manager | `load_reporter.go:23` |
| `world_channels:zone:{z}:{conf}` | SET,某张世界图在该 zone 的频道 | scene_manager | `world_init.go:25` |
| `consumer:applied_epoch:{topic}:{key}:{msgType}` | db 已落库的最大 epoch(只升不降)| db | `go/db/internal/kafka/key_ordered_consumer.go:507` |

查键统一用(本地 redis 容器名 `redis`,`deploy/docker-compose.yml:218`):

```powershell
$P = <player_id>   # robot 日志 "[travel-smoke] at home" 那行的 player_id
docker exec redis redis-cli GET "player:${P}:owner_epoch"
docker exec redis redis-cli GET "player:${P}:handoff"
docker exec redis redis-cli TTL "player:${P}:handoff"
docker exec redis redis-cli --no-raw GET "player:${P}:location"   # 二进制,只能看字节;字段靠 scene_manager 日志反推(缺口 G5)
```

### 2.2 scene_manager 错误码(`go/scene_manager/internal/constants/errors.go`)

| 码 | 常量 | 含义 | 行 |
|---|---|---|---|
| 1 | `ErrNoAvailableNode` | 目标 zone 无可用 gate / 目标图没开 / 无节点 | :5 |
| 7 | `ErrKafkaRoute` | 路由 / 重定向的 Kafka 发送失败(location 与 epoch 已回滚)| :11 |
| 8 | `ErrRedis` | 读位置 / epoch / 标记失败,fail-closed 拒绝 | :12 |
| 17 | `ErrSceneReentryBarrier` | 场景属主刚判死、再入屏障未到(可重试)| :44 |
| **18** | `ErrHandoffPending` | 要换节点 / 换 zone,但源 scene 还没为**当前** epoch 写出标记(可重试,未改任何状态)| :50 |
| **19** | `ErrOwnerEpochConflict` | 并发 EnterScene 抢先推进了 epoch,本次 CAS 失败(可重试)| :54 |
| **20** | `ErrHomeZoneUnavailable` | `data_service.GetPlayerHomeZone` 不可用,归属未知不得落库(可重试)| :59 |
| 14 | `ErrUnsafeCrossNodeHandoff` | **历史码,EnterScene 不再发出**;日志里再见到它 = 跑的是旧二进制 | :32 |

18 在 C++ 的唯一定义是 `PlayerLifecycleSystem::kSmErrHandoffPending`(设计文档 §11.2),Go 侧改编号必须同步它。

### 2.3 指标

**scene_manager**(zone1 `:9150`,zone2 `:10150`——`go_services.ps1:351-368` 的规则是 `基准 + (Zone-1)*1000`;实际端口以 `run\etc\go_services\z<N>_scene_manager.yaml` 为准):

| 指标 | label | 期望 | 出处 |
|---|---|---|---|
| `scene_manager_enter_scene_rejected_total` | `zone_id`(**目标** zone)、`reason` ∈ `handoff_pending_no_marker` / `handoff_pending_stale_marker` / `handoff_pending_withdrawn` / `epoch_conflict` / `home_zone_unavailable` / `travel_map_unavailable` / `pending_map_fallback` / `home_zone_unmapped_travel`(2026-09-20 新增:未映射玩家的跨 zone 传送被拒,`zone_id` 是 **gate** zone)/ `scene_gone`(2026-09-20 恢复:指定 scene_id 进入途中场景被销毁)。以 `metrics.go` 的 Help 为准 | `no_marker` 是跨节点换图的常规第一跳,有基线;`stale_marker` / `withdrawn` 稳态应≈0 | `metrics/metrics.go:107-114`、`enterscenelogic.go:57-64`。旧名 `scene_gone / unsafe_handoff / handoff_pending` **已无调用点** |
| `scene_manager_enter_scene_owner_dead_takeover_total` | `zone_id` | 稳态恒 0;节点崩溃后 ≈ 当时挂在死节点上、随后回来的玩家数(每人只记一次)| `metrics.go:258-262`、`enterscenelogic.go:596-598` |
| `scene_manager_reentry_barrier_blocked_total` | `zone_id`、`site` ∈ … `stale_location` / `dead_owner_takeover` | 只在节点刚死后的一个屏障窗口内非 0 | `metrics.go:248-252`、`reentry_barrier.go:41-53` |
| `scene_manager_home_zone_lookup_total` | `outcome` ∈ `mapped/unmapped/unconfigured/error` | 双 zone 下 `mapped` 在涨、`error` 恒 0、`unconfigured` 恒 0 | `metrics.go:123-127` |
| `scene_manager_kafka_delivery_total` | `outcome` ∈ `acked/failed` | 注入 Kafka 故障时 `failed` 会涨 | `metrics.go:235-239` |
| `scene_manager_enter_scene_rollback_total`(2026-09-21 新增)| `outcome` ∈ `rolled_back` / `already_rolled_back`(go-redis 重发 EVAL 时首发已回滚,只对铸造过的落点识别)/ `superseded`(被并发推进)/ `redis_error`(重试耗尽仍出错,Redis 里可能留着本次落点)。**不带 zone_id** | 分母 = 推路由 / 重定向失败的次数;`redis_error` 恒 0,非 0 触发告警 `SceneManagerEnterSceneRollbackRedisError` | `metrics.go:262`、`:410`;发射点 `owner_epoch.go:258` `rollbackPlayerPlacement` |
| `scene_manager_node_detach_deferred_total`(v2.5,GO-6)| `zone_id`、`outcome` ∈ `deferred`(已判死但 death_at 写不进 / 摘负载集失败,节点**留在负载集**进推迟队列;每个节点每次死亡只记一次)/ `recovered`(补写 death_at 成功后摘除,正常自愈)/ `expired`(自首次尝试起满一个再入屏障仍写不进 death_at,**不带标记**摘除)/ `abandoned`(自首次尝试起满 10 分钟仍摘不出负载集,放弃,死节点留在负载集按存活处理,等下一次 fullSync 重扫)/ `dropped`(推迟期间节点重新注册,或本副本失去领导权)| 健康时**所有 outcome 恒 0**;进入 `deferred` 时同 zone 的 `expired` / `abandoned` 会预建值为 0 的序列(给告警的 `increase()` 用),看到 0 值序列不代表出过事。`expired` / `abandoned` 非 0 触发告警 `SceneManagerNodeDetachWithoutDeathMark`(见下)| `internal/metrics/metrics.go` `nodeDetachDeferredTotal` / `ObserveNodeDetachDeferred`;发射点 `reentry_barrier.go` `deferNodeDetach` / `retryDeferredNodeDetaches` / `cancelDeferredNodeDetachOnReregister`、`load_reporter.go` `removeNodeFromRedis` |

**告警 `SceneManagerNodeDetachWithoutDeathMark`(v2.5,`deploy/k8s/scene-manager-alerts.yaml`,warning,不设 `for`,`runbook_url` 指向本文)**:`sum by (zone_id) (increase(scene_manager_node_detach_deferred_total{outcome=~"expired|abandoned"}[10m])) > 0`。判读:

- 背景(GO-6):节点判死时 death_at 必须**先于**把节点摘出负载集落地;以前 death_at 写失败照样摘,读者会看到「不在负载集 + 没有 death_at(= 屏障已过)」,在老节点 15s emergency drain 还在存盘时就改派 / 接管(双写回档)。现在写不进就不摘:节点留在负载集里(`IsNodeAlive` 为真,所有改派 / 接管点都拒),由领导者每 5s 节拍 + 每次 fullSync 重试。
- `expired` = 推迟满一个再入屏障(默认 20s)仍写不进 death_at,不带标记摘除。**这一步本身安全**(推迟期间屏障已由「留在负载集」兜满),但说明 Redis 在拒 SET(内存写满且 noeviction / 只读 / 熔断)。
- `abandoned` = 推迟满 10 分钟仍摘不出负载集。死节点按存活处理,CreateScene / EnterScene 可能被派到它上面(RPC 失败回滚 / 玩家挂在哑连接上),直到下一次 fullSync(watch 重连 / 领导权变化 / 进程重启)重扫。
- 处置:查 Redis(`used_memory` 与 `maxmemory`、慢日志、连接数、go-zero 熔断日志);scene_manager 日志搜 `[ReentryBarrier][DeferDetach]`(各行含义见 §2.4 scene_manager 表)看是哪个节点、卡在哪一步;遇到 `abandoned` 且 Redis 已恢复,可以重启 scene_manager 领导者触发 fullSync 重扫。
- 本地复现手法见场景 **E2**。注意本地 docker redis 是 `allkeys-lfu`(`deploy/docker-compose.yml`),death_at 写成后仍可能被淘汰,效果等同 GO-6 修之前,这个残余本修复不覆盖(K8s 的 `deploy/k8s/manifests/infra/redis.yaml` 不设 maxmemory,Redis 缺省即 noeviction,不受影响)。

**db**(zone1 `:9160`,zone2 `:10160`):

| 指标 | 期望 | 出处 |
|---|---|---|
| `db_stale_owner_write_rejected_total`(无 label)| **所有场景恒 0**(不变量 §6.3)| `go/db/internal/metrics/metrics.go:93-97` |
| `db_owner_epoch_guard_total{outcome}`,`outcome` ∈ `match / legacy_zero / first / advance / stale / read_error` | 每次真实换主后新主第一笔写记一次 `advance`;`stale` 恒 0;全链升级后 `legacy_zero` 归零 | `metrics.go:84-88`、`:121-128` |

**C++ scene 没有 Prometheus 指标**,只有两行 30s 汇总日志(缺口 G1):

- `[TravelHandoff] started=… started_cross_zone=… granted=… granted_without_reply=… resolved_in_place=… aborted=… exit_wins=… save_watchdog_fired=… reply_watchdog_fired=… verify_rearmed=… frozen_ms_total=… frozen_ms_max=… withdraw_deferred=… withdraw_expired=… granted_client_reset=… reply_uncorrelated=… reply_unmatched=… freeze_cap_reached=… dispatch_window_closed=… mark_sent_destroyed=… mark_sent_client_reset=… destroy_deferred_unsettled_save=… freeze_unstamped=… watchdog_early_fire=… handoff_fastpath_forced=…` —— INFO,**累计值**,**有变化才打**。进程内第一次发起交接后开始打;v2.5 起第一次给 `reply_uncorrelated` / `reply_unmatched` 计数时也会挂上(从没交接过的节点也能看到这两项)。**字段顺序以 `player_lifecycle.cpp` `EnsureTravelHandoffStatsTimer` 的实际输出为准**(新 key 一律只追加在末尾,按位置解析的脚本不受影响),各字段含义见 `player_lifecycle.h` 的 `travel_handoff_stats` 注释。`granted_client_reset` 是 `granted_without_reply` 的子集:源端销毁实体之前给客户端发了 tip 3027 + 踢线 34 的次数,应接近 0(gate-cmd 消费滞后 >30s 时,迟到但成功的传送也会被计入并踢回选服)。v2.5 新增的 10 个 key:

  | key | 含义 | 期望 |
  |---|---|---|
  | `reply_uncorrelated` | EnterScene 应答没带 `correlation_id`(号为 0),退回按 `player_id` 对应答的次数(设计文档「EnterScene 应答关联号」一节)。首次出现时每线程打一次 WARN(§2.4)| **scene_manager 全部升级到本批二进制后恒 0**。非 0 = 有 scene_manager 是旧二进制(不回显号),或有发送点绕过了统一出口 `SendCorrelatedEnterScene`。滚动升级窗口里持续增长属正常,升级完仍非 0 要查 |
  | `reply_unmatched` | 被丢弃的**可疑**外来应答:有等待者、号对不上,且是拒绝 / 重定向 / 发生在交接期间(`enter_scene_reply::IsSuspiciousUnmatched`)——正是过去会被错吃成交接证据的那一类。路由先到之后才到的纯成功迟到应答是常态,只打 DEBUG、不计 | 基线接近 0,与"换图后立刻再换图 / 交接作废后立刻换图"的频率相关,**只看突增**。本文单机器人 / 单客户端的场景里应为 0;非 0 时保留对应的 `[EnterSceneReply] dropped unmatched reply` 行 |
  | `freeze_cap_reached` | 冻结满 70s(`travel_freeze_cap::kFreezeCap`,单调时钟)仍无结论而被处置的次数。SET 没发出的那一支同时计入 `aborted`,已发出的那一支同时计入 `mark_sent_destroyed` | **恒 0**(两道 30s 看门狗与 35s 晚发闸都先于它收敛)。非 0 = 有归属核实不了(zone Redis 不可用 / 半开),场景 H1 故意制造 |
  | `dispatch_window_closed` | 冻结超过 35s(`kDispatchWindow`)才走到发送点、被晚发闸拦下的次数。`set_mark` 阶段(SET 还没发)同时计入 `aborted`,`enter_scene` 阶段(SET 已 OK)同时计入 `mark_sent_destroyed` | 接近 0;场景 H2 故意制造 |
  | `mark_sent_destroyed` | **新终态**:handoff 标记的 SET 已发出、归属在本节点核实不了 → tip(会话活着时)+ 踢线 34 + 不存盘销毁(`ConcludeHandoffAfterMarkSent`)。四个收口点:冻结上限 / `enter_scene` 晚发闸 / SET 之后发现没有 gate 会话 / SET 之后发现没有 scene_manager | 接近 0 |
  | `mark_sent_client_reset` | `mark_sent_destroyed` 的子集:销毁前给客户端发了 tip + 踢线 34(会话活着、实体不在退出中)| — |
  | `destroy_deferred_unsettled_save` | 标记已发出、该销毁了,但该玩家还有未落地的存盘而推迟销毁的次数(每次交接最多计一次)| **恒 0**。非 0 = 冻结越过了 70s 上限(唯一允许的例外),保留证据 |
  | `freeze_unstamped` | 交接组件上的单调冻结起点(`frozenAtSteady`)没打点、被上限扫描就地补记的次数 | **恒 0**。非 0 = 有一条挂交接组件的路径漏写了它(只会晚处置,不会提前销毁)|
  | `watchdog_early_fire` | 存盘 / 应答看门狗比单调截止时刻早 1s 以上被 muduo(按墙钟)唤醒、按剩余时间重挂的次数 | 非 0 = 墙钟向前跳过 ≥1s(对时 / 人工改时间);本文不做改系统时间的注入 |
  | `handoff_fastpath_forced` | 交接快路径判"盘上已是同一份"、但该 key 还有在途 / 排队中的存盘,改走一次真实存盘再写标记的次数(与退出链 `exit_fastpath_deferred` 同一写法)| 趋势值,如实记录 |

  平均冻结时长 v2.5 起 = `frozen_ms_total / (granted + resolved_in_place + aborted + mark_sent_destroyed)`(`mark_sent_destroyed` 也累计冻结时长)。
- `[OwnerEpoch] stale_owner_write_rejected=… home_zone_unknown=… owner_epoch_unknown=…` —— WARN,**任一非 0 才打**(`cpp/libs/services/scene/core/system/redis.cpp:75-81`)。所以"恒 0"的证据是**这行日志从头到尾没出现**。
- **(v2.4)`[ExitPersist] exit_superseded=… exit_intent_missing=… exit_resave=… exit_resave_capped=… exit_exhausted_rekick=… exit_fastpath_deferred=… exit_client_msg_rejected=… exit_deposed_on_reentry=…`** —— INFO,累计值,30s、有变化才打,进程内第一次有玩家退出后开始(`player_lifecycle.cpp:333-340`,定时器由 `EnsureExitStatsTimer` 自挂,**不在** `redis.cpp`)。期望:`exit_superseded` / `exit_intent_missing` / `exit_resave_capped` 恒 0;`exit_resave` 偶发(退出存盘在途时实体又变了);`exit_client_msg_rejected` 在断线瞬间客户端在途包多时会涨,正常;`exit_deposed_on_reentry` 非 0 = 有旧实体被判废黜后丢弃重载(退出被保留到租约之后 / 活僵尸),记录 player_id。
- **(v2.4)`[ExitRelease] attempted=… skip_disabled=… skip_entity_invalid=… skip_intent_missing=… skip_cause=… skip_suppressed_release=… skip_suppressed_identity=… skip_suppressed_unspecified=… skip_relocate=… skip_handoff_inflight=… skip_epoch_unknown=… skip_identity_conflict=… written=… epoch_moved=… failed=… inherit_absent=… inherit_deleted_older=… inherit_deleted_exact=… inherit_kept_newer=… inherit_deleted_malformed=… inherit_epoch_mismatch=… inherit_reply_error=… inherit_reply_lost=… inherit_failed=… inherit_refused=… inherit_rewritten=… inherit_rewrite_skipped=… inherit_rewrite_failed=… inherit_rewrite_dropped_node_holds=… inherit_rewrite_handed_over=…`** —— INFO,同上(`player_lifecycle.cpp:274-303`)。`attempted` = 判定为"写"的次数,写成功看 `written`。期望:干净断线 `attempted`≈`written`,`failed` / `inherit_epoch_mismatch` / `inherit_refused` 稳态恒 0;ReleasePlayer 引起的退出计 `skip_cause`(正常);`skip_epoch_unknown` 非 0 = 还有 owner_epoch 为 0 的存量玩家。**M12 口径**:v2.4 起 `enter_scene_rejected_total{reason="handoff_pending_stale_marker"}` 会混入"上一任的释放标记还在、而新铸落点没载入"的登录重试,排查先对照本行的 `written` / `inherit_*` 与 CPP-2 症状,压测复盘把两类分开统计。
- **(v2.5,C++ gate,CPP-2)`[SceneEntry] deferred=… recovered=… gave_up=… superseded=… session_closed=… player_changed=… links_ready=… pending=…`** —— INFO,gate 进程(`z<N>_gate*` 的日志),30s 一次、**计数或积压有变化才打**(`cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp` `MaybeLogSummary`;前 7 项是累计值,`pending` 是当前还在补发的会话数)。含义:`deferred` = 进场转发第一次没交出去、转入补发(链路没连上 / 没握手 / 节点找不到);`recovered` = 补发成功;`gave_up` = 超限放弃(推 3023 + 踢线 34 + 关连接);`superseded` = 补发期间来了新路由、整体替换;`session_closed` = 补发期间客户端连接已关、撤销;`player_changed` = 路由属于另一名角色、撤销;`links_ready` = scene 链路握手就绪次数。期望:健康栈基线 `gave_up=0`、`pending` 回到 0;`links_ready` 至少等于 scene 节点数(每次重连握手再 +1)。C++ 没有接 Prometheus,故障期间排障以这一行的 `pending` / `gave_up` 为准(原先逐条的 `RoutePlayer: scene node not found in registry` ERROR 已删)。

### 2.4 日志原文(grep 用)

**C++ scene**(`cpp/libs/services/scene/player/system/player_lifecycle.cpp`;日志文件见 §3.1):

| 片段 | 含义 | 行 |
|---|---|---|
| `[ZoneTravel] rejected: bad target zone` / `… scene_config_id is not a world map` / `… in battle` / `… in team` / `… scene change busy` | 同步拒绝,未改状态 | :1459 / :1475 / :1485 / :1495 / :1506 |
| `[ZoneTravel] handoff started for player <P> target_zone=<Z> (cross-zone)`(同 zone 为 `(same-zone cross-node)`)| 已冻结、已发起存盘 | :1602 |
| `[SavePlayerToRedis] Player <P> saved to Redis, DB write tasks enqueued (topic=db_task_zone_<home>, owner_epoch=<E>)` | 存盘已发;**topic 必须是 home_zone 的**,访客区也一样(CZ-2)| :1339、`constants/player.h:8` |
| `HandlePlayerAsyncSaved: Saving complete for player: <P>` | 存盘落地 | :339 |
| `[ZoneTravel] requested EnterScene for player <P> target_zone=…` | 标记已写、EnterScene 已发 | :1884 |
| `[ZoneTravel] handoff granted for player <P> -> gate <ip>:<port>; destroying source-side entity` + `[travel_redirect] local entity for player <P> destroyed without persisting (ownership handed over)` | 跨 zone 放行,源实体销毁 | :2126、:1405 |
| `[ZoneTravel] same-zone handoff granted for player <P>: owner_epoch <E> -> <E'>` + `[scene_handoff_granted] …` | 同 zone 跨节点放行 | :1994、:1405 |
| `[ZoneTravel] EnterScene rejected for player <P> code=<N> msg=…` | scene_manager 回了错误码 | :2100 |
| `[ZoneTravel] handoff aborted for player <P>: <reason>; unfreezing and keeping player on this node` | 交接未成,解冻 + 回失败 tip。`<reason>` 取值(v2.5):`handoff save did not land in time` / `handoff dispatch window closed before the mark was sent`(v2.5 晚发闸 `set_mark` 阶段,前一行是 `[ZoneTravel][DispatchWindow] … stage=set_mark`)/ `handoff freeze cap reached before the handoff mark was sent`(v2.5 冻结上限的「SET 没发出」一支,前一行是 `[ZoneTravel][FreezeCap] … action=unfreeze`,**应恒不出现**)/ `owner_epoch unknown (0); route chain not upgraded` / `zone redis not connected` / `handoff mark write failed` / `redis command dispatch failed` / `scene_manager rejected` / `EnterScene reply timed out` / `reply without redirect` / `handoff mark write result unknown`(SET 应答为空、核实后 epoch 未变)。**不该出现**的两条防御分支:`could not force a save before writing the handoff mark`、`mark-sent conclusion reached without a sent handoff mark`。**v2.5 删去** `no live gate session` / `no SceneManager node reachable`:SET 已 OK 之后这两种情形不再解冻,改走 `[ZoneTravel][MarkSentDestroy]`(见下方 v2.5 表);SM 不可达改在冻结之前同步拒绝(`handoff not started … no SceneManager node reachable`)。v2.5 之后的二进制里再见到这两条旧 reason = 跑的是旧二进制 | `AbortTravelHandoff`;reason 出处以 `grep -n 'AbortTravelHandoff(' player_lifecycle.cpp` 为准(2026-09-29 工作树::2898 :2940 :3322 :3336 :3344 :3374 :3415 :3644 :3993 :4030;`:3644` 透传 `ResolveTravelOutcome` 的 reason)|
| `[ZoneTravel] handoff resolved in place for player <P>: …; ownership unchanged, unfreezing silently` | 同 zone 重发后落回本节点,不算失败 | :2156 |
| `[ZoneTravel] cannot verify travel outcome for player <P> (<reason>): zone redis unavailable; keeping the player frozen and re-arming the watchdog` | Redis 不通,判不清去留 → **保持冻结**并重挂 30s 看门狗(v2.5:最多到 70s 冻结上限,之后按「标记已发出」收口)| :1944 |
| `[ZoneTravel] MGET owner_epoch/location failed or malformed while verifying travel outcome for player <P> (<reason>, evidence=<e>); keeping the player frozen and re-arming the watchdog` | 同上(读失败 / 应答形状不对)。2026-09-21 起这次读是单条 `MGET owner_epoch location`,旧文案 `GET owner_epoch failed …` 已不存在;重挂时证据原样透传 | `player_lifecycle.cpp:2200` |
| `[ZoneTravel] SET handoff mark result unknown for player <P> (connection dropped before reply); keeping the player frozen until it is verified` | SET 回调拿到空 reply(连接断了,**可能已写入**)| :1807 |
| `[ZoneTravel] travel for player <P> was granted although the reply was lost/failed (<reason>, evidence=<e>): owner_epoch <E> -> <E'>; destroying source-side entity` + `[travel_granted_without_reply] … (ownership moved away)` | 应答丢了 / 失败但其实已放行。`evidence` ∈ `failed` / `no_reply` / `mark_write_unknown` / `anomalous`(`succeeded` 走同 zone 放行那一行)。**排障以 `evidence=` 为准**:看门狗核实时 `<reason>` 仍写 `EnterScene reply timed out`,但若应答其实已到,`evidence` 打印的是应答带来的证据 | `player_lifecycle.cpp:2234` |
| `[ZoneTravel][ClientReset] player <P> (<reason>, evidence=<e>): owner_epoch <E> -> <E'>, location awaits placement in zone <Z>; sending tip 3027 + KickPlayer so the client re-logs in` | 紧跟上一行之后、`[travel_granted_without_reply]` 之前:跨 zone、证据 `failed` / `no_reply`、实体没在退出、location 是本次交接的等待落点 → 给客户端发 tip 3027 + 踢线 34,`granted_client_reset` +1(设计文档 §12.5.6)| `player_lifecycle.cpp:2265` |
| `[ZoneTravel][ClientReset] not resetting client of player <P> (<reason>, evidence=<e>): <why>; target_zone=<Z> owner_epoch=<E'>; destroying only` | 跨 zone 且证据符合、但运行期条件不满足,只销毁不踢。`<why>` ∈ `player is exiting (UnregisterPlayer) …` / `location missing or unparsable` / `location is not this handoff's awaiting placement (epoch advanced by another request)`。**后者出现时要记录**:说明 epoch 是被别的请求推进的(同一会话迟到的同 zone 换图 / 顶号)| `player_lifecycle.cpp:2275` |
| `[ZoneTravel] re-entry with newer owner_epoch … hit a stale in-handoff entity …` + `[handoff_superseded_by_reentry] …` | 应答丢失后玩家又被派回本节点,旧实体先销毁再重载 | :1727 |
| `[ZoneTravel] EnterScene reply for player <P> but entity is gone (exited during travel); ignoring` | 退出优先之后才到的应答 | :2023 |
| `[ZoneTravel] handoff mark landed but travel intent is gone/replaced for player <P>; not requesting EnterScene` | 标记落地时交接已作废 | :1819 |
| `HandlePlayerAsyncSaved: player <P> is exiting; dropping in-flight zone travel intent (exit wins)` | **退出优先**(标记还没写的那一支)| :359 |
| `[SavePlayerToRedis] skip: zone travel handoff already requested for player <P>` → `HandleExitGameNode: player <P> already persisted (dirty-save fast path); finishing exit inline` → `FinishExitAfterPersist: player <P> exited during ownership handoff; dropping handoff intent (exit wins) handoff_mark_written=<0\|1>` | **退出优先**(标记已写的那一支,随后 `DEL` 标记)| :1184、:800、:850 |
| `FinishExitAfterPersist: orphan PlayerFrozenComp without handoff intent` | **不该出现**:有路径漏摘 Frozen | :884 |
| `HandlePlayerSaveRejected: owner_epoch CAS rejected save for player <P> … (metric=stale_owner_write_rejected)` | **不该出现**:曾经双主 | :1369 |
| `[gRPC] ReleasePlayer: player <P> has an ownership handoff in flight; leaving the outcome to the EnterScene reply / watchdog` | 交接中收到 ReleasePlayer,不处理 | `cpp/nodes/scene/handler/grpc/scene_node_service.cpp:164-165` |
| `[EmergencyRelocate] node identity lost; persisting and relocating <N> online player(s) to the main world` / `[EmergencyRelocate] requested main-world re-home for player <P>` / `[EmergencyRelocate] zone redis unavailable; handoff mark not written …` / `[SceneDrain] draining scene entity …` | 整节点疏散 / 单场景排空(也写 handoff 标记)| :959 / :1098 / :1038 / :980 |
| `HandlePlayerAsyncLoaded: Loading player <P>` / `Destroying player: <P>` | 哪个进程载入 / 销毁了该玩家(判"同一时刻只有一个持有者"用)| :1088 / :1621(v2.4 行号)|

**C++ scene —— v2.4 新增(Z1 修复 / 断线释放标记 A′,设计文档 §12.6.10;行号为 2026-09-21 工作树)**:

| 片段 | 含义 | 行 |
|---|---|---|
| `HandleExitGameNode: Player <P> is exiting the scene node (cause=<c>)` | 退出发起。`<c>` ∈ `client_disconnect` / `node_shutdown` / `scene_drain` / `identity_conflict` / `released_by_transfer` / `unspecified` | `player_lifecycle.cpp:1743` |
| `Player marked for unregistration: <P> (cause=<c>, resave_rounds=<n>)` | **v2.4 起这行只在退出收敛、即将销毁时打**(以前在退出发起时打);其后紧跟 `Player session removed` → `Destroying player: <P>`。另两种形态 `(converged on re-save, …)` / `(converged on exhausted re-kick)` | :1312、:1342、:1778 |
| `HandlePlayerAsyncSaved: exiting player <P> changed while its exit save was in flight (…); re-saving before destroying, round <k>/5 (metric=exit_resave)` | 落地内容 ≠ 当前内存,更新快照后重存(M1) | :1323 |
| `HandlePlayerAsyncSaved: exiting player <P> did not converge after <n> re-save round(s) …; keeping the entity … (metric=exit_resave_capped)` | ERROR。**不该出现**:还有未识别的逐帧改动源,实体 fail-closed 保留 | :1355 |
| `HandlePlayerAsyncSaved: exit of player <P> superseded by a newer session <S> (…); keeping the entity (metric=exit_superseded)` | WARN。**不该出现**(正常重连在 EnterScene 第 0 步就摘了退出标记)。修复前二进制对应的旧文案是 `ignoring stale UnregisterPlayer … live session …` —— **v2.4 之后的二进制里再见到它 = 跑的是旧二进制** | :1274 |
| `HandlePlayerAsyncSaved: exiting player <P> has UnregisterPlayer but no PlayerExitIntentComp; … (fail-closed, metric=exit_intent_missing)` | ERROR。**不该出现** | :1289 |
| `HandleExitGameNode: player <P> matches its last persisted snapshot but still has an unsettled save; forcing a real save … (metric=exit_fastpath_deferred)` | 快路径被 M4 拦下,改走真实存盘 | :1830 |
| `HandleReentry: player <P> re-entered with owner_epoch <N> while a stale exiting\|live entity (cached <E>) was still retained; discarding it … (metric=exit_deposed_on_reentry)` | 旧实体被判废黜(N ≥ E+2),丢弃后从 Redis 重载 | :2868 |
| `[ExitRelease] mark written player=<P> mark=<E>:<ms> site=exit_release` | A1′ 写成功。**是 Redis 异步应答,出现在 `Destroying player` 之后** | :683 |
| `[ExitRelease] mark not written player=<P> … : owner_epoch moved on` / `[ExitRelease] mark write failed player=<P> … reason=<r> (not retried)` | epoch 已前移(不写是对的)/ 失败(WARN,不重试)。`site=inherit_rewrite` 的同款行是 A2′ 放弃补写 | :687 / :661、:691、:706 |
| `[ExitRelease] not writing release mark player=<P> reason=<skip_*> cause=<c> owner_epoch=<E>` | INFO:压制 / 改派 / 交接在途 / 身份不确认 / 意图缺失;`skip_cause` / `skip_disabled` / `skip_epoch_unknown` / `skip_entity_invalid` 只打 DEBUG,看汇总行 | :762 / :757 |
| `[ExitRelease] SCENE_EXIT_RELEASE_MARK=<raw> -> on\|off` | 进程内第一次用到开关时打一次(`SCENE_DEV_UNSAFE_CROSS_NODE_HANDOFF` 同款) | :537 |
| `[ExitRelease][InheritClear] player=<P> epoch=<N> result=inherit_absent\|inherit_deleted_older\|inherit_deleted_exact\|inherit_kept_newer\|inherit_deleted_malformed` | A2′ 核对通过(INFO)。同节点不铸造的重登是 `exact`;跨节点 / 首次落点铸了新 epoch 是 `older` | :856 |
| `[ExitRelease][InheritClear] player=<P> epoch=<N> result=inherit_epoch_mismatch\|inherit_reply_error\|inherit_reply_lost …` / `… clear failed … will retry` | 核对不过 / 失败(WARN) | :861 / :839 |
| `[ExitRelease][InheritClear] refusing entry player=<P> owner_epoch=<N> session=<S> reason=<r> … (metric=inherit_refused)` | ERROR,拒建实体、客户端收 3023 | :807 |
| `[ExitRelease][InheritClear] route for player <P> carries no owner_epoch; clearing marks up to the current owner_epoch instead` | 旧版路由(兼容窗口) | :3427 |
| `HandlePlayerAsyncLoaded: player <P> loaded; waiting for the inherited-mark clear of owner_epoch <N> before creating the entity` | 闸门等 A2′ 应答 | :1125 |
| `Shutdown drain progress: redis_player_saves=… kafka_messages=… remaining_players=… exit_release_marks=…` | 停机 drain 进度,多了 A1′ 在途数 | `cpp/nodes/scene/main.cpp:218-221` |

**C++ scene —— v2.5 新增(冻结硬上限 / 晚发闸 /「标记已发出」统一收口 / 快路径补写,设计文档「交接冻结硬上限 + 晚发闸 +『标记已发出』统一收口」一节;行号为 2026-09-29 工作树 `player_lifecycle.cpp`,以函数名为准)**:

| 片段 | 含义 | 出处 |
|---|---|---|
| `[ZoneTravel] handoff not started for player <P>: no SceneManager node reachable` | WARN。冻结**之前**的 SM 预检:同步拒绝,**没有** `handoff started`、不冻结、不计 `started`,客户端立刻收到失败 tip(跨 zone `kZoneTravelTargetBusy`,同 zone `kEnterSceneFailed`)。K-SM 杀得太早(SM 已从注册表摘掉)时就是这一行 | `StartTravelHandoff`(:2832)|
| `[ZoneTravel] handoff of player <P> matches its last persisted snapshot but still has an unsettled save; forcing a real save before writing the handoff mark (metric=handoff_fastpath_forced)` | INFO。快路径补写:同 key 还有更早的存盘在途 / 排队,改走一次真实存盘,落地(`Saving complete`)后才写标记。保证"SET 发出 ⇒ Redis 里已是冻结那一刻的状态"。`handoff_fastpath_forced` +1。同函数里的 ERROR `… could not force a save while an older save is unsettled; not writing the handoff mark` **不该出现** | `StartTravelHandoff`(:2888、:2896)|
| `[ZoneTravel][DispatchWindow] player=<P> stage=set_mark frozen_ms=<ms>: handoff save landed too late to verify ownership before the freeze cap; not writing the mark` | WARN。交接存盘晚于 35s 才落地:不写标记,随后 `handoff aborted … handoff dispatch window closed before the mark was sent`(解冻 + tip)。`dispatch_window_closed`+1、`aborted`+1。30s 存盘看门狗正常先到,出现 = 它没按时触发(墙钟回拨把 muduo 定时器推迟了等)| `BeginTravelHandoff`(:3319)|
| `[ZoneTravel][DispatchWindow] player=<P> stage=enter_scene frozen_ms=<ms>: handoff mark reply arrived too late to verify ownership before the freeze cap; not sending EnterScene` | WARN。SET 的 OK 晚于 35s 才到:标记已写,**不许解冻**,不发 EnterScene,紧跟 `[ZoneTravel][MarkSentDestroy] … site=travel_dispatch_window`。`dispatch_window_closed`+1、`mark_sent_destroyed`+1。场景 H2 | `RequestTravelEnterScene`(:3449)|
| `[ZoneTravel] no live gate session for player <P> after the handoff mark was written; not sending EnterScene` / `[ZoneTravel] no SceneManager node reachable for player <P> after the handoff mark was written; not sending EnterScene` | WARN。SET 已 OK 之后才发现没有 gate 会话 / 没有 SM(后者只剩"SM 恰好在 SET 往返那几毫秒里消失"的窄窗口),紧跟 `[MarkSentDestroy] … site=travel_no_gate_session`(只销毁不踢)/ `site=travel_no_scene_manager`。v2.4 及以前这两种是 `handoff aborted … no live gate session / no SceneManager node reachable`(解冻)| `RequestTravelEnterScene`(:3462、:3472)|
| `[ZoneTravel][FreezeCap] player=<P> action=unfreeze target_zone=<Z> frozen_ms=<ms>: handoff mark was never sent; unfreezing with a failure tip` | WARN。冻结满 70s、SET 从没发出:解冻 + tip,随后 `handoff aborted … handoff freeze cap reached before the handoff mark was sent`。`freeze_cap_reached`+1、`aborted`+1。30s 存盘看门狗正常先到,**应恒不出现** | `ExpireTravelFreeze`(:3988)|
| `[ZoneTravel][MarkSentDestroy] player=<P> site=<site> target_zone=<Z>(cross-zone\|same-zone) mark=<E>:<ms> enter_scene_sent=0\|1 corr=<N> evidence=<e> frozen_ms=<ms> exiting=0\|1 kick=0\|1: handoff mark already sent and ownership cannot be verified here; destroying without persisting (ownership UNKNOWN, not confirmed moved)` | **新终态**:SET 已发出、归属在本节点核实不了 →(`kick=1` 时)tip + 踢线 34 → 不存盘销毁。`site` ∈ `travel_freeze_cap`(**ERROR**:两道看门狗与晚发闸都没能收敛)/ `travel_dispatch_window` / `travel_no_gate_session` / `travel_no_scene_manager`(WARN)。注意 `target_zone=<Z>` 与括号之间**没有空格**。`enter_scene_sent` 按关联号是否非 0 判出;`corr` 是交接那条 EnterScene 的关联号(没发出为 0);`evidence` 是应答 / 核实记到组件上的证据(`succeeded` / `failed` / `mark_write_unknown` / `anomalous`),**从没收到任何应答时是 `none`**(`no_reply` 不会出现在这里);`kick` = 实体不在退出中且 gate 会话仍活着,tip 与 34 的 `reason.id` 相同:跨 zone `kZoneTravelTargetBusy`(3027),同 zone `kEnterSceneFailed`(3023)。计数:`mark_sent_destroyed`+1;`kick=1` 时 `mark_sent_client_reset`+1;`site=travel_freeze_cap` 时 `freeze_cap_reached`+1。**不发任何 Redis 命令、不撤回标记**:留下的标记起断线释放标记(A1′)的作用 | `ConcludeHandoffAfterMarkSent`(:4069-4084)|
| `[travel_freeze_cap] local entity for player <P> destroyed without persisting (ownership moved away)`(方括号里是 site 名,另三种为 `[travel_dispatch_window]` / `[travel_no_gate_session]` / `[travel_no_scene_manager]`)| WARN,在上一行之后(中间隔一行 `Destroying player: <P>`)。**这是沿用下来的原文**(`DestroyDeposedPlayer` 在 routine=false 时的固定文案),在这里的实际含义是「**归属未知**」,不是"已确认交出去";判读以上一行的 `(ownership UNKNOWN, not confirmed moved)` 为准 | `DestroyDeposedPlayer`(:2662)|
| `[ZoneTravel][MarkSentDestroy] player=<P> site=<site>: unsettled save still pending; deferring destroy until it lands (metric=destroy_deferred_unsettled_save)` | ERROR,每次交接最多一条。该玩家还有未落地的存盘 → 推迟销毁(此刻销毁,那笔写随后落地会把盘改回中间态)。这是冻结越过 70s 的**唯一允许的例外**;快路径补写之后按代码推理不可达,**出现即保留证据** | `ConcludeHandoffAfterMarkSent`(:4044)|
| `[ZoneTravel][MarkSentDestroy] player=<P> site=<site>: called although the handoff mark was never sent (requested_at_ms=0); unfreezing instead` | ERROR,**不该出现**(前置条件被破坏,按 SET 未发出一侧解冻)| `ConcludeHandoffAfterMarkSent`(:4028)|
| `[ZoneTravel][FreezeCap] handoff of player <P> has no monotonic freeze stamp; stamping it now (metric=freeze_unstamped)` | ERROR,**不该出现**:某条挂交接组件的路径漏写了单调冻结起点;就地补记,只会晚处置、不会提前销毁 | `ElapsedSinceFreeze`(:637)|
| `[ZoneTravel] handoff watchdog fired <N>ms before its monotonic deadline (wall clock stepped forward?); re-arming (metric=watchdog_early_fire)` | WARN。存盘 / 应答看门狗被 muduo(按墙钟到期)提前 >1s 唤醒,按剩余时间重挂。非 0 = 墙钟前跳过。墙钟**回拨**会把所有定时器(含 1s 上限扫描)推迟,见 §6 FC-1 | `RunAtMonotonic`(:317-321)|

**C++ scene —— v2.5 新增(EnterScene 应答关联号 + gRPC 失败回调;设计文档「EnterScene 应答关联号」一节、`docs/design/grpc-client-deadline-failure-callback.md`)**:

v2.5 起 scene 发出的每条 EnterScene 都带 `correlation_id`(scene_manager 回显),应答按号分发(`player_id` 只用来找实体)。交接相关的行都多了号:`[ZoneTravel] requested EnterScene for player <P> … session=<S> corr=<N>`、`[ZoneTravel] handoff granted for player <P> -> gate …; destroying source-side entity corr=<N> correlated=1`、`[ZoneTravel] EnterScene rejected for player <P> code=<c> msg=… corr=<N> correlated=0|1`、`[ZoneTravel] same-zone handoff reply for player <P> corr=<N> correlated=0|1; verifying outcome`。**判读**:`requested EnterScene` 的 `corr` 必须等于随后那条应答行的 `corr`,且 `correlated=1`;`correlated=0` = 旧版 scene_manager(退回按 `player_id`,同时 `reply_uncorrelated` 在涨)。上表各行的前缀不变,旧的 grep 口径仍可用。

| 片段 | 含义 | 出处 |
|---|---|---|
| `[EnterSceneReply] scene_manager 未回显 correlation_id,退回按 player_id 对应答(首见 player=<P>);此后同类应答只计 reply_uncorrelated,不再逐条告警` | WARN,每线程只打一次。= 有 scene_manager 是旧二进制,或有发送点绕过了统一出口。本批全栈二进制下**不该出现** | `DispatchEnterSceneReply`(:3023)|
| `[EnterSceneReply] dropped unmatched reply player=<P> corr=<N> route=unmatched handoff_tag=<T> scene_change_tag=<S> error_code=<c> has_redirect=0\|1` | INFO,`reply_unmatched`+1。有等待者但号对不上、且是拒绝 / 重定向 / 发生在交接期间的应答被丢弃(上一代交接 / 被顶替的请求 / 超过在途 TTL 才到的)。外来的 redirect 只记录、不销毁 | `DispatchEnterSceneReply`(:3084)|
| `[EnterSceneReply] invariant broken: scene-change reply while a handoff is in flight, player=<P> … ; dropping` / `[ZoneTravel] invariant broken: travel reply routed without an issued handoff EnterScene, player=<P> …` | ERROR,**不该出现**(前者同时 `reply_unmatched`+1)| :3069、`HandleTravelEnterSceneReply`(:3770)|
| `[ZoneTravel] handoff EnterScene transport failure for player <P> corr=<N> (<method> code=<grpc code> msg=<…>); outcome unknown, leaving the verdict to the reply watchdog / freeze cap` | WARN。**2026-09-28 起生成的 gRPC 客户端对非 OK 状态也回调失败处理器**(deadline:`bin/etc/base_deploy_config.yaml` `GrpcClient.CallDeadlineMs.SceneManagerNodeService` = 10000ms = 服务端 `Timeout` 8000 + 2000)。交接路径上传输失败 = **结果未知**:只记这一行,不当失败证据、不提前核实、不计任何数,去留仍由 30s 应答看门狗 / 70s 上限按 `owner_epoch` 裁决。K-SM 下常见 `code=14`(UNAVAILABLE)或 `code=4`(DEADLINE_EXCEEDED)| `DispatchEnterSceneTransportFailure`(:3150)|
| `[ZoneTravel] EnterScene transport failure for player <P> scene_id=… scene_conf_id=… corr=<N> (…); notifying the client` / `[ZoneTravel] team-follow EnterScene transport failure for player <P> …; player stays in the current scene` | 普通换图(不是交接)的传输失败:释放在途槽;玩家发起的回 `kServiceUnavailable`(不断言失败,路由可能随后到达),队伍跟随只记日志。在途槽 TTL v2.5 起 = SceneManager deadline + 1000ms(旧的 5s 小于服务端 8s、同一玩家可能两条 EnterScene 并行的窗口已随之消除)| `DispatchEnterSceneTransportFailure`(:3172、:3164)|

**scene_manager**(`go/scene_manager/internal/logic/`):

| 片段 | 含义 | 行 |
|---|---|---|
| `[Handoff] <site> 暂拒:源 scene 尚未为当前归属代际写出落盘标记(可重试): player=… owner_epoch=… handoff="…"`(`<site>` = `场景交接` / `跨区重定向`)| 回 18。**Infof 不是 Errorf**:它是跨节点换图的常规第一跳 | `enterscenelogic.go:649` |
| `[Handoff] 落点时交接标记已被源 scene 撤回…` / `[Handoff] 跨区放行时交接标记已被源 scene 撤回…` | 预检后、铸造前标记被撤回(回 18,reason=`handoff_pending_withdrawn`)| :557 / :855 |
| `[Handoff] dev 旁路 AllowUnsafeCrossNodeHandoff=true,无落盘标记放行且不铸造 epoch` | **本 runbook 跑的时候不该出现**(出现 = 没切到生产口径)| :634 |
| `Cross-zone detected: player <P> gate_zone=… target_zone=… place=<bool> mint=<bool>, redirecting` | 第一条腿开始 | :839 |
| `Cross-zone redirect failed for player <P>: no gate nodes available for zone <Z>` | 目标 zone 无 gate,回 1,未改状态 | :844、`gate_redirect.go:44` |
| `Cross-zone handoff released: player=<P> target_zone=<Z> owner_epoch=<E'> minted=true (awaiting placement)` | 第一条腿放行,location 变成「等待落点」| :888 |
| `Cross-zone redirect only (ownership unchanged): player=<P> target_zone=<Z>` | 只送连接,不动归属(R7)。v2.4 起(GO-5)访客在重连窗口内从归属区入口重登、location 落在别 zone 的具体节点上时走这里;**等待落点不再走这里**(R8 已被设计文档 §12.7 取代)| :891(v2.4 工作树 `:1030`)|
| `Failed to push redirect to gate (placed=true), rolling back location/epoch: …` | Kafka 发送失败,回滚,回 7 | :878 |
| `[RouteRollback] outcome=redis_error 回滚玩家位置/epoch 时 Redis 出错…` / `[RouteRollback] outcome=already_rolled_back 首发回滚已生效(go-redis 重发)…` / `[RouteRollback] outcome=superseded 玩家位置或 epoch 已被并发请求推进…` | 回滚结果(2026-09-21 起统一前缀,**旧原文 `route 回滚玩家位置/epoch CAS 失败` / `route 回滚检测到…` 已不存在,按旧文案 grep 会漏**)。`rolled_back` 只计数不打日志。`redis_error` = Redis 里可能留着本次落点:跨 zone 时源 scene 会发 `[ZoneTravel][ClientReset]`;同 zone 时玩家挂在哑连接上(已知限制)。grep 口径:`"[RouteRollback] outcome="` | `owner_epoch.go:258-283` |
| `[Travel] 跨 zone 传送被拒:目标 zone 没有这张图的世界频道,未改任何状态` | 第一条腿地图只读预检拒绝,回 1 | `enterscenelogic.go:808` |
| `[Travel] 等待落点记下的目标地图不可用,回落默认大世界` | 第二条腿地图回落(人必须能落地)| :398 |
| `位置记录的属主节点已确认死亡且再入屏障已过,按无持有者处理: player=… dead_zone=… dead_node=…` | **属主接管**(§11.5)| :302 |
| `忽略已下线 zone 的陈旧玩家位置: player=…` | 整 zone 下线的陈旧位置(`playerLocationOwnerGone`)| :284 |
| `Player <P> entered scene <S> on node <N> (zone <Z>, home_zone <H>, owner_epoch <E>)` | 落点成功;看 node / home_zone / epoch | :601 |
| `[ReentryBarrier] 记录节点死亡时刻: zone=… node=… barrier=…` / `[ReentryBarrier] <site>: 老属主刚判死,再入屏障还差 …` | 判死打点(death_at **写成功**才打)/ 屏障内拒绝 | `reentry_barrier.go` `markNodeDeath` / `reentryBarrierBlocks`(v2.5 起按函数名引用,行号会漂)|
| `[ReentryBarrier][DeferDetach] 已判死但 death_at 未落地或摘负载集失败,节点暂留负载集(按存活处理),每拍重试: zone=… node=… scenes=… captured=… pending=… err=…` | (v2.5,GO-6)ERROR。判死时 death_at 写不进(或写成了但 ZREM 失败):**不摘**,节点留在负载集进推迟队列;`node_detach_deferred_total{outcome="deferred"}`+1。同一节点再次失败时是 `… 仍未摘出负载集,沿用首次推迟的时刻与快照(已等 …)`;抄场景快照失败另有 `… 抄死节点场景快照失败,摘除时再现读` | `reentry_barrier.go` `deferNodeDetach`(调用方 `load_reporter.go` `removeNodeFromRedis`、fullSync 的 `sweepStaleLoadSetMembers`)|
| `[ReentryBarrier][DeferDetach] 仍写不进 death_at,已等 <d> / 屏障 <b>,下一拍再试: …` / `[ReentryBarrier][DeferDetach] 摘负载集失败,下一拍再试(已等 <d> / 放弃期限 10m0s): …` | ERROR,每拍(5s ticker + 每次 fullSync)一条,推迟期间持续出现 | `retryDeferredNodeDetaches` |
| `[ReentryBarrier][DeferDetach] 补写 death_at 成功,已摘出负载集(已等 <d>): zone=… node=…` | INFO,`outcome="recovered"`+1。正常自愈;这一拍之前会先打一行 `记录节点死亡时刻` | `retryDeferredNodeDetaches` |
| `[ReentryBarrier][DeferDetach] 自首次尝试已 <d> ≥ 屏障 <b> 仍写不进 death_at,不带标记摘除(…): zone=… node=… err=…` | ERROR,`outcome="expired"`+1,**触发告警** `SceneManagerNodeDetachWithoutDeathMark`。安全(屏障已由推迟态兜满),但说明 Redis 在拒 SET | `retryDeferredNodeDetaches` |
| `[ReentryBarrier][DeferDetach] 自首次尝试已 <d> 仍摘不出负载集,放弃;节点留在负载集按存活处理,由下一次 fullSync 重扫: …` | ERROR,`outcome="abandoned"`+1,**触发告警**。死节点留在负载集,可能被派到 | `retryDeferredNodeDetaches` |
| `[ReentryBarrier][DeferDetach] 节点在推迟摘除期间重新注册,放弃推迟的摘除;…` / `[ReentryBarrier][DeferDetach] 本副本已不是领导者,放弃推迟的摘除(…)` | INFO(快照没抄到的那种是 ERROR),`outcome="dropped"`+1。前者不给活节点打死亡标记;后者由新领导者 fullSync 重扫 | `cancelDeferredNodeDetachOnReregister` / `retryDeferredNodeDetaches` |

**C++ gate —— v2.5 新增(CPP-2 gate 侧进场转发补发;设计文档「CPP-2 gate 侧进场转发补发」一节;`cpp/nodes/gate/handler/event/scene_entry_dispatch.cpp`,日志在 `run\logs\cpp_nodes\z<N>_gate*.stdout.log` / `bin\logs\cpp_nodes\`)**:

gate 收到 RoutePlayerEvent 后,先判断"这条 scene 链路是否已连上**并且**在当前连接上握过手"(scene 在握手时才为本 gate 挂 RpcSession,TCP 连上约 0.5s 后才握手,之前发出的进场通知会被 scene 丢掉)。交不出去就在会话上记欠账,250ms 扫描、单调时钟截止、退避 250ms→2s 补发;节点找不到给 3s 发现预算,其余 20s,最多 16 次。会话的 scene 指向**只在转发成功后才提交**(补发期间的客户端消息与断线 ExitGame 仍去旧节点)。超限:推 `kEnterSceneFailed`(3023)+ 踢线 34(`reason.id`=3023)+ 关连接。

| 片段 | 含义 | 出处 |
|---|---|---|
| `[SceneEntry] scene link ready node_id=<n> uuid=<u>` | INFO。gate 到某个 scene 节点的链路在**当前连接**上握手成功(每条链路每次握手一行,重连后再打一行)。**联调基线:gate 启动后每个 scene 节点各一行**;缺了 = 就绪章没盖上,所有进场会在 20s 后被踢(`last_failure=handshake_pending`)| `OnSceneLinkHandshaken` |
| `[SceneEntry] handshake reply for unknown scene node, ignored uuid=<u>` / `[SceneEntry] handshake reply not on the node's current link, ignored uuid=<u>` | WARN。握手应答找不到节点 / 不是节点当前连接上的(迟到的旧连接应答不给新连接盖章)。偶发可接受,持续出现且没有对应的 `scene link ready` 要查 | `OnSceneLinkHandshaken` |
| `[SceneEntry] forward deferred session_id=<S> player_id=<P> target_node_id=<n> scene_id=<s> failure=<f> retry_in_ms=<ms> deferred_total=<k> (sampled 1/64)` | WARN,**1/64 采样**(每线程第一条必打)。进场转发第一次没交出去,转入补发。`failure` ∈ `node_not_found` / `no_rpc_client` / `not_connected` / `handshake_pending`(`invalid_route` 不重试、直接放弃)| `AttemptAndSettle` |
| `[SceneEntry] forward recovered session_id=<S> player_id=<P> target_node_id=<n> scene_id=<s> attempts=<k> last_failure=<f> elapsed_ms=<ms> recovered_total=<k> (sampled 1/64)` | INFO,1/64 采样。补发成功,会话此刻才提交到新节点 | `AttemptAndSettle` |
| `[SceneEntry] giving up session_id=<S> player_id=<P> entry_player_id=<P'> target_node_id=<n> scene_id=<s> attempts=<k> first_failure=<f> last_failure=<f> elapsed_ms=<ms>,推送 kEnterSceneFailed 并踢线、关闭会话` | ERROR,**不采样**,每个会话一条(玩家被踢的唯一服务端证据)。随后客户端收 3023 + 34,约 1s 后 gate 强关连接(`forceCloseWithDelay`),断线回调照常发 ExitGame 到**旧**节点、打 `Client disconnected, session_id=<S>`。**健康栈基线必须为 0**,尤其不得出现 `last_failure=handshake_pending` | `GiveUpSceneEntry` |
| `[SceneEntry] route belongs to another player, entry cancelled session_id=<S> session_player_id=<P> entry_player_id=<P'> …` | WARN,1/64 采样。同一连接上换过角色,欠账撤销、不踢线 | `AttemptAndSettle` |
| `[SceneEntry] deferred=… recovered=… gave_up=… superseded=… session_closed=… player_changed=… links_ready=… pending=…` | 30s 汇总,见 §2.3 | `MaybeLogSummary` |

**db**:`STALE-OWNER-WRITE rejected: key=<P> … taskEpoch=… appliedEpoch=…`(**不该出现**,`key_ordered_consumer.go:629`);`owner-epoch advance: key=<P> … taskEpoch=<E'> appliedEpoch=<E>`(每次真实换主一条,正常,`:645`)。

**login**:EnterScene 被拒时返回错误 `scene_manager rejected EnterScene for player <P>: code=<N> msg=…`(`go/login/internal/logic/clientplayerlogin/entergamelogic.go:577`;它具体落在哪一行日志**未核对**)。**(v2.4,GO-5)**重连 / 顶号不指定去向:`[travel] EnterGame player=<P> decision=<d> 不指定去向(ZoneId=0),由 scene_manager 按 location 决定 gate_zone=<z>`(`entergamelogic.go:665`);持票据的第二条腿:`[travel] EnterGame player=<P> 持重定向票据落地 zone=<z>,跳过 home_zone 弹回`(`:660`)。

**robot**(`robot/travel_smoke_scenario.go`):`[travel-smoke] at home`(:246,带 `player_id`)→ `[travel-smoke] travel out`(:279,紧接着发 TravelToZone)→ `[travel-smoke] arrived at visit zone`(:289)→ `[travel-smoke] travel home`(:343)→ `TRAVEL_SMOKE_OK player_id=…`(:376)或 `TRAVEL_SMOKE_FAIL step=<step> reason=<…>`(:203)。受理后失败的 reason 形如 `受理后失败 SendTipToClient tip=<N>`(:762);跟随 124 失败另有一行 `follow gate redirect failed`(`robot/logic/handler/scene_client_player_common_redirect_to_gate.go:64`)。收到踢线 34 时 robot 打一行 Warn `kicked by server`,`reason` 字段是 `TipInfoMessage` 的文本形式(含 `3027` 即命中,`robot/logic/handler/handlers.go:42-47`),robot 自己不断线;传送窗口内若 34 先于 tip 被扫到,失败文案是 `传送途中收到 KickPlayer(目标 zone 的顶号 / 旧会话清理踢到了新会话?)`(:767)。**注意**::762 的文案写着"scene 已解冻",对 §12.5.6 的踢线路径不成立(那里实体已销毁)。

**tip 码**(只按名字引用;数字取自生成物 `generated/code/proto/tip/scene_error_tip.proto`,仅供对照 robot 打印的运行时值):`kEnterSceneSceneNotFound=3007`、`kEnterSceneChangingScene=3014`、`kEnterSceneFailed=3023`、`kZoneTravelTargetZoneNotFound=3024`、`kZoneTravelInBattle=3025`、`kZoneTravelInTeam=3026`、`kZoneTravelTargetBusy=3027`;`kSceneTransferInProgress=13000`(`cross_server_error_tip.proto`)。跨 zone 传送受理后未成**一律**回 `kZoneTravelTargetBusy`(`AbortTravelHandoff`);受理后未成且无法原地恢复时同一个码既作 tip、也作踢线 34 的 `reason.id`(`SendTipAndKickToClient`,2026-09-29 在 `player_lifecycle.cpp:222`)。v2.5 起还有两处发 tip + 34:`[ZoneTravel][MarkSentDestroy] … kick=1`(跨 zone 用 `kZoneTravelTargetBusy`,**同 zone 用 `kEnterSceneFailed`**);gate 的 `[SceneEntry] giving up`(一律 `kEnterSceneFailed`,由 gate 直接推送,并强关连接)。

---

## 3. 前置准备

### 3.1 环境

1. **二进制必须是当前 main 编出来的**:gate / scene / login / scene_manager / db / robot。编译与 regen 清单见设计文档 §9 / §11.4,不在本文范围。`robot.exe` 需先在 `robot/` 下编出。
2. **切到生产口径**:`go/scene_manager/etc/scene_manager_service.yaml` 的 `AllowUnsafeCrossNodeHandoff`(2026-09-29 在 :61,行号会漂,按键名找)本地默认是 `AllowUnsafeCrossNodeHandoff: true`(dev 旁路)。跑本 runbook 前**临时**改成 `false` 再启动服务(派生 yaml 是启动时从它拷出来的);测完还原,**不要提交**。旁路开着时场景 A / E 完全失去意义(无标记也放行)。核对方法:scene_manager 日志里**不出现** `[Handoff] dev 旁路`。
3. 起双 zone(zone 1 先保持**单 scene 节点**,场景 A / E 依赖"玩家一定在 `z1_scene` 这个进程上"):

   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command dev-start-zones -Zones 1,2
   pwsh -File tools/scripts/dev_tools.ps1 -Command dev-status
   ```

4. 两个 zone 的 login / scene / scene_manager / db 必须指向**同一个 Redis**(CZ-2 契约;`robot/etc/travel_smoke.yaml` 文件头前置 1)。其余前置(`GateTokenSecret` 非空、GM 指令放行、`robot_9601` 首次在 home_zone 建角等)以该 yaml 文件头为准,共 8 条,逐条过。
5. **日志位置**(`docs/ops/log-management.md`):
   - C++:`run\logs\cpp_nodes\<instanceKey>.stdout.log`(`cpp_nodes.ps1:351`)。**stdout 重定向有缓冲,末尾常停在半行,进程被硬杀后不补写**;完整内容在 muduo 文件 `bin\logs\cpp_nodes\`(`cpp_nodes.ps1:318`;PROGRESS.md 2026-09 日志采集条目)。被杀节点取证以 muduo 文件为准。
   - Go:`run\logs\go_services\<instanceKey>.stdout.log` / `.stderr.log`(`go_services.ps1:574-575`)。
   - instanceKey:`z1_scene`(单实例)/ `z1_scene_1`、`z1_scene_2`(多实例),`z1_scene_manager`、`z2_db` …;PID 在 `run\pids\cpp_nodes.pid.json`、`run\pids\go_services.pid.json`。
6. **第一条腿打在哪个 scene_manager 上拿不准**:scene 选 scene_manager 是 `player_id % 已发现的 SM 数`(`cpp/libs/engine/core/network/node_utils.cpp:15-35`),没有按 zone 过滤;scene 是否只发现本 zone 的 SM **未核对**。所以找第一条腿的日志 / 指标时,**两个 zone 的 scene_manager 都要看**。

### 3.2 注入工具箱

| 代号 | 手法 | 效果 | 把握度 |
|---|---|---|---|
| **K-SM** | `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-stop -GoServices scene_manager` | **硬杀所有 zone 的** scene_manager(`Stop-Process -Force`,`go_services.ps1:759`;按名字过滤,不分 zone)。其 etcd 租约 60s(`scene_manager_service.yaml` 的 `LeaseTTL: 60`)内,scene 仍会把 EnterScene 发给它 → 撑开一个 **30s 的"标记已写、没有应答"窗口**。*原先的论据是"生成的 gRPC 客户端 status 非 OK 时只打日志、不回调应答处理器";**2026-09-28 起有失败回调**(见 `docs/design/grpc-client-deadline-failure-callback.md`,SceneManager deadline 10000ms),但交接路径上传输失败仍**不作为证据**:只打一行 `[ZoneTravel] handoff EnterScene transport failure … outcome unknown, leaving the verdict to the reply watchdog / freeze cap`,不解冻、不提前核实,所以窗口照样撑开(§2.4 v2.5 表)* | 中。SM 已从 scene 的注册表摘掉时窗口打不开,v2.5 起分两种:**(a) 发起前就摘了** → `StartTravelHandoff` 预检同步拒绝,`[ZoneTravel] handoff not started for player <P>: no SceneManager node reachable`,不冻结、没有 `handoff started`,客户端立刻收到失败 tip;**(b) SET 之后、发 EnterScene 之前才摘**(几毫秒的窄窗口)→ `[ZoneTravel] no SceneManager node reachable for player <P> after the handoff mark was written …` + `[ZoneTravel][MarkSentDestroy] … site=travel_no_scene_manager … kick=1`,客户端被踢回选服。两种都说明 K-SM 没撑开窗口:B1 按其"若现象是 v2.5 的同步拒绝…"那一条处理,**H1 记为 SKIP、调整 K-SM 的时机后重做**。v2.4 的 `handoff aborted … no SceneManager node reachable`(立即解冻)已不存在 |
| **K-SCENE** | `Stop-Process -Id <pid> -Force`,pid 取自 `run\pids\cpp_nodes.pid.json` 的 `z1_scene` | 硬杀**一个**指定 scene 进程。不要用 `cpp-node-stop -CppNodes scene`:它按名字过滤,会把**所有 zone 的所有** scene 一起杀(`cpp_nodes.ps1:449-457`)| 高 |
| **K-ROBOT** | `Get-Process robot \| Stop-Process -Force` | 客户端断线。gate 发现 TCP 断开后**立即**通知 scene `ExitGame`(`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:409-436`),不等 login 的 30s 断线租约 | 高 |
| **R-PAUSE** | `docker exec redis redis-cli CLIENT PAUSE 20000` | Redis 20s 内不处理任何命令,TCP 不断(hiredis `connected()` 仍为真)→ 撑开"存盘已发、未落地"窗口。会同时卡住 login / scene_manager 等所有 Redis 客户端,它们的日志里会有超时噪声 | 中 |
| **R-STOP** | `docker stop redis` / `docker start redis` | 真断连。本地 redis 开了 AOF + 数据卷(`docker-compose.yml:226-234`),重启后键还在 | 高(hiredis 多久重连上**拿不准**)|
| **KF-PAUSE** | `docker pause kafka` / `docker unpause kafka` | scene_manager 的路由 / 重定向发送是**同步、`MaxAttempts=1`、`WriteTimeout=5s`**(`svc/servicecontext.go:110-142`)→ 铸造之后卡在 Kafka 上,随后失败并回滚。**用 pause 不用 stop/restart**:本机 Kafka 数据不持久,重启即清空日志 | 低。实际卡多久拿不准(`ReadTimeout` 走 kafka-go 缺省),首跑记录 |

恢复:`go-svc-start-exe -GoServices scene_manager -Zone 1`(再 `-Zone 2`);`cpp-node-start -CppNodes scene -Zone 1`(`dev_tools.ps1:972 / :981`;已在跑的实例会被跳过,`cpp_nodes.ps1:337-347`)。

**按 robot 输出卡时序的辅助脚本**(示意,**未执行过**;在 `robot/` 目录下跑。travel-smoke 是一次跑到底的冒烟,自己不会停在交接窗口里——缺口 G8):

```powershell
function Invoke-TravelSmokeWithHooks {
    param([string]$Config = 'etc/travel_smoke.yaml',
          [System.Collections.Specialized.OrderedDictionary]$Hooks)   # 键 = 正则,值 = 命中时只执行一次的脚本块
    $fired = @{}
    & .\robot.exe -c $Config 2>&1 | ForEach-Object {
        $line = "$_"; $line
        foreach ($pattern in @($Hooks.Keys)) {
            if (-not $fired[$pattern] -and $line -match $pattern) { $fired[$pattern] = $true; & $Hooks[$pattern] }
        }
    }
    "robot exit code = $LASTEXITCODE"
}
# 两个锚点:
#   '\[travel-smoke\] at home'     T1 结束。此后到发 TravelToZone 之间还有 3 个间隔 1.1s 的请求(T2 / T2b / 读金币),约 3~5s,
#                                   且这几步只走 gate→scene 内存态,不需要 scene_manager / Redis / Kafka —— 适合在这里"预埋"故障。
#   '\[travel-smoke\] travel out'  紧接着发 TravelToZone{visit_zone}。此后 2~3s 内交接已在途。
```

§4 各场景的「注入」里,钩子写成 `'at home' = { K-SM }` 这种形式时,`'at home'` / `'travel out'` 是上面两个锚点正则的简写,`K-SM` / `K-ROBOT` / `R-PAUSE` 等是本节表格里对应命令的简写——执行时换成真实命令。

### 3.3 基线(不通就别往下跑)

```powershell
cd robot
.\robot.exe -c etc/travel_smoke.yaml      # 期望退出码 0,日志有一行 TRAVEL_SMOKE_OK
```

同时核对去程的日志序列(回程镜像):

- scene(z1):`[ZoneTravel] handoff started … (cross-zone)` → `[SavePlayerToRedis] … (topic=db_task_zone_1, owner_epoch=E)` → `HandlePlayerAsyncSaved: Saving complete` → `[ZoneTravel] requested EnterScene` → `[ZoneTravel] handoff granted … -> gate …` → `[travel_redirect] … destroyed without persisting (ownership handed over)`。
- scene_manager(第一条腿):`Cross-zone detected: … place=true mint=true` → `Cross-zone handoff released: … owner_epoch=E+1 minted=true (awaiting placement)`。**不应**出现 `[Handoff] 跨区重定向 暂拒`(跨 zone 传送是先写标记后请求)。
- scene_manager(第二条腿):`Player <P> entered scene … on node … (zone 2, home_zone 1, owner_epoch …)`。第二条腿也铸造(`owner_epoch.go:25-28` 注释),所以 epoch 预期是 E+2——**从注释推的,首跑核对**。
- scene(z2)此后每次存盘:`topic=db_task_zone_1`(**不是** `db_task_zone_2`)。出现 `db_task_zone_2` = 访客数据落错库(不变量 §6.2),立即停。
- db(z1):`owner-epoch advance: key=<P>`;`db_stale_owner_write_rejected_total == 0`。
- 放行后、第二条腿落点之前 `GET player:{P}:handoff` 仍能读到旧标记,但其 epoch **小于** `GET player:{P}:owner_epoch`——正常(scene_manager 只比对不删)。**v2.4 起**第二条腿在 scene(z2) 载入前会跑 A2′:`[ExitRelease][InheritClear] player=<P> epoch=<E+2> result=inherit_deleted_older`,之后该键为 nil(此前靠 TTL 回收)。
- **v2.4 起** robot 结束(LeaveGame / 断线)后,最后持有它的 scene 会写一份断线释放标记:`[ExitRelease] mark written player=<P> mark=<E'>:<ms> site=exit_release`,`GET player:{P}:handoff` = `"E':<ms>"` 且 E' == 当前 owner_epoch —— 正常,下一次载入由 A2′ 清掉。
- **(v2.5,关联号)** scene(z1) `[ZoneTravel] requested EnterScene … corr=<N>` 的 `<N>` 与随后 `[ZoneTravel] handoff granted … corr=<N> correlated=1` 一致。
- **(v2.5)** 基线跑完后各 scene 的 `[TravelHandoff]`:`reply_uncorrelated=0`、`reply_unmatched=0`、`freeze_cap_reached=0`、`dispatch_window_closed=0`、`mark_sent_destroyed=0`、`mark_sent_client_reset=0`、`destroy_deferred_unsettled_save=0`、`freeze_unstamped=0`、`watchdog_early_fire=0`;`handoff_fastpath_forced` 如实记录;全程没有 `[ZoneTravel][MarkSentDestroy]` / `[ZoneTravel][FreezeCap]` / `[ZoneTravel][DispatchWindow]` 行,robot 不出现 `kicked by server`。`reply_uncorrelated` 非 0 = 有 scene_manager 不是本批二进制,先换齐再跑。
- **(v2.5,CPP-2)** gate:启动后每个 scene 节点各有一行 `[SceneEntry] scene link ready node_id=<n> uuid=<u>`(两个 zone 各看自己的 gate);基线期间**零** `[SceneEntry] giving up`,稳态下也不应有 `failure=handshake_pending` 的 `forward deferred`;`[SceneEntry]` 汇总行要么不出现,要么 `gave_up=0`、`pending=0`。再跑一次 robot `login-test` 模式(`robot/etc/robot.yaml` 的 `mode`),通过数与改动前基准一致(本机记录 23/23),期间同样不得出现 `[SceneEntry] giving up`,尤其不得出现 `last_failure=handshake_pending`(= 就绪章没盖上,全员 20s 后被踢)。

### 3.4 每个场景前后

- **前**:记下 `$P`;确认 `EXISTS player:{P}:handoff` 为 0、或其 epoch 已落后于当前 `owner_epoch`、或它是上一轮退出写下的断线释放标记(epoch == 当前值,且上一轮日志里有对应的 `[ExitRelease] mark written … mark=<同一原文>`;v2.4 起这是常态,本轮登录的 A2′ 会清掉它)。**判"标记已撤回"的步骤都在玩家仍在线时做**;玩家退出之后键重新出现是 A1′,不是撤回失败;拉一次指标快照(`curl -s http://127.0.0.1:9150/metrics`、`:10150`、`:9160`、`:10160` 存成文件);记下各 scene 进程最后一行 `[TravelHandoff]`(没有就当全 0),以及(v2.5)各 gate 进程最后一行 `[SceneEntry] deferred=…` 汇总(同样没有就当全 0)。
- **后**:恢复被注入的组件 → 重跑 §3.3 基线必须仍是 `TRAVEL_SMOKE_OK`(证明没留残局)→ 再拉一次指标快照做差。
- **残留等待**:上一轮死在半路时会留下 login 断线租约(30s)与「等待落点」(随票据 300s 过期,`enterscenelogic.go:782-784`)。T1 报 `step=login-redirected` = 位置记录还指着访客区,**等满 300s** 再跑(`travel_smoke.yaml:41-43`)。*v2.4(GO-5)起:等待落点不再牵引,只有"上一轮停在访客区某个节点上、且在 30s 重连窗口内重登"才会被送回访客区(这正是 GO-5 的期望);等满 30s 断线租约(挂机月卡为 300s)即可,不必等 300s。*不建议手工 `DEL player:{P}:location` 来"加速":那会绕过本文要测的东西。

### 3.5 K8s 双 zone(未逐条核对)

`dev_tools.ps1 -Command k8s-zone-up -ZoneName <name> -ZoneId <id>` 仍然存在(`dev_tools.ps1:3 / :957`)。注入手法的对应物是 `kubectl delete pod … --grace-period=0 --force`(K-SCENE / K-SM)、`kubectl scale … --replicas=0`(R-STOP / Kafka)。namespace / label / Deployment 名字**本文未核对**,首次在 K8s 上执行的人补。v1 引用的 `robot/clients/cmd/multi_zone_seeder`、`single_player` **已不存在**(`robot/` 下没有 `clients/` 目录),不要再找。

---

## 4. 失败场景

每个场景的通过标准之外,**§5 的全局不变量都要同时成立**。

### 场景 A:源节点在「存盘后、写标记前」崩溃

**先说清楚能测到什么**:"存盘落地回调 → 写标记"是同一个回调栈里连着执行的(`HandlePlayerAsyncSaved` `:375` → `BeginTravelHandoff` `:1783` 的 `SET`),外部没有任何手段恰好卡进这条缝(缺口 G4)。本场景验证的是它的**可观测等价类**:「源节点死亡时,当前 epoch 没有有效的 handoff 标记」——不管存盘落没落地,scene_manager 看到的状态是一样的。

- **前置**:zone 1 只有一个 scene 进程 `z1_scene`(§3.1 第 3 步),事先读出它的 PID;生产口径(§3.1 第 2 步)。
- **注入**:

  ```powershell
  $scenePid = (Get-Content ..\run\pids\cpp_nodes.pid.json | ConvertFrom-Json).z1_scene
  $hooks = [ordered]@{
      '\[travel-smoke\] at home'    = { docker exec redis redis-cli CLIENT PAUSE 20000 }        # R-PAUSE:存盘发得出去、落不了地
      '\[travel-smoke\] travel out' = { Start-Sleep 3; Stop-Process -Id $scenePid -Force }      # K-SCENE:交接在途时杀源节点
  }
  Invoke-TravelSmokeWithHooks -Hooks $hooks
  # 随后补一个备用 scene 节点(它的 node_id 与死掉的那个不同,接管才有地方落):
  pwsh -File ..\tools\scripts\dev_tools.ps1 -Command cpp-node-start -CppNodes scene -SceneCount 2 -Zone 1
  ```

  `-SceneCount 2` 会新起 `z1_scene_1`、`z1_scene_2` 两个实例(instanceKey 与已死的 `z1_scene` 不同,`cpp_nodes.ps1:331-333`)——**从脚本静态读出来的,首跑核对**。
- **期望**:
  - robot:`TRAVEL_SMOKE_FAIL step=travel-out`(124 等不到,60s 超时;gate 发现 scene 死后若主动断开客户端,失败形态会不同,首跑记录)。
  - Redis(PAUSE 结束后):`player:{P}:handoff` **不存在**;`owner_epoch` 仍是 E。被杀进程那笔在途存盘有没有落地**拿不准**(Redis 对已断开客户端的挂起命令多半直接丢弃),两种都可接受。
  - 之后重跑 travel-smoke 充当"玩家重登"探针,T1 会卡在 `step=login`,直到属主接管放行。时间线同场景 E:C++ 节点 etcd 租约 `NodeTTLSeconds: 180`(`bin/etc/base_deploy_config.yaml:6`)+ 再入屏障 20s(`constants/reentry_barrier.go:65-69`)≈ **200s 以上**。
  - 这段时间内 scene_manager:`[Handoff] 场景交接 暂拒 … handoff=""` + `enter_scene_rejected_total{reason="handoff_pending_no_marker"}` 上涨;**或者**请求被解析回死节点、scene_manager 回 0 并把路由发给一个已死的进程,客户端只是超时(缺口 G7,**拿不准会是哪一种**,首跑记录)。
  - 放行时:`位置记录的属主节点已确认死亡且再入屏障已过,按无持有者处理: player=<P>` → `Player <P> entered scene … on node <新节点> (… owner_epoch E+1)`;`enter_scene_owner_dead_takeover_total{zone_id="1"}` +1。
- **通过标准**:
  - ✅ 全程**没有**任何一次"凭标记放行"(scene_manager 日志无 `Cross-zone handoff released`,`owner_epoch` 在接管前一直是 E);
  - ✅ 玩家在 ≈ 租约 + 屏障 + 一次重试间隔内重新进得去,**不需要**运维手工清 `location`;
  - ✅ 重登后金币 ∈ {本轮登录余额, 本轮登录余额 + 11}(取决于那笔存盘落没落地;崩溃固有的进度丢失),**绝不是 0**,也不小于本轮登录余额;
  - ✅ `takeover_total` 对这名玩家**只 +1**(重试多次不重复计,`enterscenelogic.go:594-598`)。
- **失败时保留**:robot 全量输出;`bin\logs\cpp_nodes\` 下被杀进程的 muduo 日志;两个 zone 的 scene_manager / login 日志;§2.1 四条 redis 查询的输出 + `GET node:zone:1:<死节点>:death_at` + `ZRANGE scene_nodes:zone:1:load 0 -1 WITHSCORES`;注入前后指标快照。

> 变体(可选,不单独计分):用 K-SM 代替 R-PAUSE,在 `travel out` +3s 先确认 `GET player:{P}:handoff` 有值再杀 scene —— 这是「标记已写、EnterScene 没被处理、源节点崩溃」。此时盘上是交接那一刻的最终态、标记对当前 epoch 有效,恢复 SM 后玩家的下一次跨节点落点可以**凭这份标记立即放行**(不必等 200s),这是安全的(标记只在存盘落地后才写)。

### 场景 B:标记已写、EnterScene 应答丢失

**B1 —— 应答丢失,且没有被放行**(可稳定注入)

- **注入**:`'\[travel-smoke\] at home' = { K-SM }`(杀掉所有 scene_manager;T1 登录已经完成,不受影响)。
- **期望**(scene z1):`handoff started` → `Saving complete` → `requested EnterScene … corr=<N>` →(v2.5,数秒内、最迟 gRPC deadline 10s)`[ZoneTravel] handoff EnterScene transport failure for player <P> corr=<N> (…); outcome unknown, leaving the verdict to the reply watchdog / freeze cap`(只记日志,**不解冻**)→ **约 30s 无任何应答,玩家处于冻结态** → `[ZoneTravel] handoff aborted for player <P>: EnterScene reply timed out; unfreezing and keeping player on this node`。`[TravelHandoff]` 增量:`started+1 started_cross_zone+1 reply_watchdog_fired+1 aborted+1`,`frozen_ms_max` ≈ 30000。客户端收到 `kZoneTravelTargetBusy`;robot:`TRAVEL_SMOKE_FAIL step=travel-out reason=…受理后失败 SendTipToClient tip=3027…`。
- **Redis**:看门狗到期前 `GET player:{P}:handoff` = `"E:<ms>"`;到期后**不存在**(`ResolveTravelOutcome` 先 `DEL` 再 `GET`,`:1955`);`owner_epoch` 仍是 E。
- **通过标准**:✅ 冻结时长 ≤ 30s + 一个调度抖动;✅ 解冻后标记已撤回;✅ epoch 未变;✅ 恢复 SM 后基线重跑 OK,金币 = 上一轮出发值(没回档,也没重复加)。
- 若现象是 v2.5 的同步拒绝 `[ZoneTravel] handoff not started for player <P>: no SceneManager node reachable`(没有 `handoff started`),或极少见的 `[ZoneTravel][MarkSentDestroy] … site=travel_no_scene_manager`(踢回选服):同样是 fail-closed 的正确行为(通过),但**没测到"应答丢失"**,记为 K-SM 对本环境无效,改用 KF-PAUSE 撑窗口再试(见 §3.2 K-SM 的把握度一栏)。v2.4 写的"立刻 `handoff aborted … no SceneManager node reachable`"已不存在。
- `[TravelHandoff]` 的 v2.5 计数在本场景应全部不动(尤其 `freeze_cap_reached` / `mark_sent_destroyed`):B1 在 30s 看门狗处就收敛了,远没到 35s / 70s。

**B2 —— 应答丢失,但其实已经放行**(仓库内**没有可靠的注入手段**,缺口 G4)

需要让 scene_manager 走完铸造 Lua 之后、回应答之前死掉。唯一能手工碰的办法:KF-PAUSE 让它卡在 Kafka 发送上(已铸造、已写「等待落点」),这几秒内 K-SM,再 `docker unpause kafka`。成功率低,**首跑能做就做,做不出来记 SKIP**,不要为它改代码加后门。

- **期望**(scene z1,2026-09-21 起二选一,取决于 124 有没有真的发出去):
  - **124 没发出去**(SM 在写 Kafka 之前死掉——这是本手法的常见结果):30s 后 `[ZoneTravel] travel for player <P> was granted although the reply was lost/failed (EnterScene reply timed out, evidence=no_reply): owner_epoch E -> E+1; destroying source-side entity` → `[ZoneTravel][ClientReset] player <P> … location awaits placement in zone 2; sending tip 3027 + KickPlayer …` → `[travel_granted_without_reply] … destroyed without persisting (ownership moved away)`;`[TravelHandoff]`:`granted+1 granted_without_reply+1 reply_watchdog_fired+1 granted_client_reset+1`。robot 日志有 `kicked by server`(reason 含 3027),FAIL 文案是 B3 所列两种之一。
  - **124 其实已送达**:客户端跟随 124 离开,旧连接一断 gate(A) 立即发 ExitGame,实体走"退出优先"销毁(`exit_wins+1`),看门狗随后 no-op,**不应**出现 `[ClientReset]`。
  - 出现 `[ZoneTravel][ClientReset] not resetting client …` 时照录 `<why>`;`location is not this handoff's awaiting placement` 在本场景不应出现。
- **已知现象(不是 bug)**:这 30s 里源实体以冻结态留在源场景的 AOI 内,周围玩家看到一个不动的分身(`scene_node_service.cpp:151-160`,设计文档 §11.2 复审 P2)。
- **通过标准**:✅ 源实体**不存盘**销毁(此后无 `[SavePlayerToRedis] Player <P> saved` 出自源节点);✅ 无 `stale_owner_write_rejected`;✅ 玩家从 zone 1 重登后**落在 zone 1**(等待落点不牵引,设计文档 §12.7;owner_epoch 预期 E+2,`[ExitRelease][InheritClear] … result=inherit_deleted_older`),金币 = 出发值。*v2.4 改期望:此前写的是"按等待落点被送到目标 zone(`Cross-zone redirect only`)",那依赖 R8,已被 GO-5 取代;出现 `Cross-zone redirect only` = scene_manager 或 login 还是旧二进制。*

**B3 —— Kafka 推重定向失败 + 回滚也失败(S3L1-1,第二层出口)**(2026-09-21 新增;对应设计文档 §12.5.6,**代码未编译,本场景未实跑**)

> v2.5 注:GO-2 根治(owner_epoch 严格单调:回滚再 INCR + 回滚回执 + 源端原子取证脚本)**设计已定稿,落码中**。它落地后源端取证会改成带 `#!lua` 的 EVAL,本场景"ACL 拒掉 EVAL、源端的 DEL / MGET 不受影响"的前提作废,期望会整体改写(届时以新版 runbook 为准);在那之前按下文执行。

需要让第一条腿已铸造、Kafka 推送失败,而随后的回滚 EVAL 拿到 Redis 错误。手法:Kafka pause 撑住推送,epoch 一变就用 ACL 拒掉 EVAL。go-redis 对 `NOPERM` / `ERR` 不重试,回滚一次即判 `redis_error`;源端的 `DEL` 与 `MGET` 不是 EVAL,不受影响。

- **前置**:生产口径(§3.1 第 2 步);基线 travel-smoke 能 OK;记下 `$P` 与当前 epoch `$E`(`GET player:${P}:owner_epoch`)。本地 Redis 若不是 `default` 用户,下面的 ACL 命令换成实际用户名。
- **注入**:
  1. `'\[travel-smoke\] at home' = { docker pause kafka }`(KF-PAUSE)。
  2. 每 100ms 查一次 `GET player:${P}:owner_epoch`,一旦变成 `$E+1`,立即 `docker exec redis redis-cli ACL SETUSER default -eval -evalsha`。
  3. 保持到 zone 1 的 scene 打出 `[ZoneTravel][ClientReset]`(zrpc 服务端超时 8s ≥ 归属查询 1.5s + Kafka 写超时 5s + 1.5s 余量,2026-09-28 起;Kafka 写在 5s 超时处返回时应答以 7 当场到达,但 docker pause 下 kafka-go 的元数据查询不受写超时约束,仍可能超过 8s、应答丢失,约 30s 后由看门狗裁决),然后 `ACL SETUSER default +eval +evalsha`,再 `docker unpause kafka`。
  - 已知副作用:`-eval` 期间全服 EVAL 都失败(含 scene 的存盘 Lua、scene_manager 的其它落点),属预期噪声,窗口尽量短;恢复后 scene_manager 的 Redis 熔断器可能还开几秒。
- **通过口径(三者都要有)**:
  - scene(z1):`[ZoneTravel][ClientReset] player <P> (…, evidence=failed|no_reply): owner_epoch <E> -> <E+1>, location awaits placement in zone 2; sending tip 3027 + KickPlayer …`,前一行是 `… was granted although the reply was lost/failed …`,后一行是 `[travel_granted_without_reply]`;`[TravelHandoff]` 增量 `started+1 started_cross_zone+1 granted+1 granted_without_reply+1 granted_client_reset+1`(证据为 `no_reply` 时另有 `reply_watchdog_fired+1`)。
  - scene_manager:`scene_manager_enter_scene_rollback_total{outcome="redis_error"}` +1,日志 `[RouteRollback] outcome=redis_error …`。第一条腿落在哪个 SM 上不确定(G9),**两个 zone 的 SM 都要看**。
  - robot:日志有 `kicked by server` 且 `reason` 含 `3027`。scene 先发 tip 后发 34,但 scene→玩家推送不保证顺序(`player_message_utils.h:7-9`),所以 robot 的 FAIL 文案两种都算命中:`TRAVEL_SMOKE_FAIL … 受理后失败 SendTipToClient tip=3027…`(tip 先到;文案里的"scene 已解冻"对本路径不成立,忽略)或 `TRAVEL_SMOKE_FAIL … 传送途中收到 KickPlayer…`(34 先到)。
- **状态核对**:`owner_epoch` = `$E+1`;`EXISTS player:${P}:handoff` = 0;`stale_owner_write_rejected` 为 0;无 `[OwnerEpoch]` 行。
- **重登**:恢复后重跑基线,能落地(从 zone 1 进:等待落点无持有者、直接铸造;epoch 预期 `$E+2`,**从代码推的,首跑核对**),金币 = 本轮出发值(冻结时那次存盘就是最终态)。
- **判定未命中**:SM 日志是 `outcome=rolled_back`、scene 是 `handoff aborted …` = ACL 翻晚了,回滚在 EVAL 被拒之前已成功——最多重试一次,仍不中如实记录。SM 日志是 `superseded` = 有并发请求,记录现象。scene 出现 `[ClientReset] not resetting client … location is not this handoff's awaiting placement` = 不符合预期,保留全部证据。
- **Unity 客户端**(可选,需服务端就位):同一注入下,客户端日志先 `[tip] id=3027` 再 `[gate] kicked by server reason=3027`,选服界面显示"目标区暂时繁忙…请重新登录。",而不是通用的"连接已断开"。
- **回归**:基线 travel-smoke 通过后 `granted_client_reset` 不变、robot 不出现 `kicked by server`;B1 仍是"解冻 + tip,不踢线"。可选:同 zone 跨节点换图加同样的注入,确认**不踢线**(同 zone 的哑连接是已知限制,设计文档 §12.5.6 残余 1),记录现象。

### 场景 C:玩家在交接窗口内退出("退出优先",标记须被撤回)

**C1 —— 标记还没写就退出**

- **注入**:`'at home' = { R-PAUSE 20000 }`;`'travel out' = { Start-Sleep 3; K-ROBOT }`。
- **期望**(PAUSE 结束后,scene z1)二选一,都算对:
  - 常见:`HandlePlayerAsyncSaved: player <P> is exiting; dropping in-flight zone travel intent (exit wins) handoff_mark_written=0` → `Player marked for unregistration: <P> (cause=client_disconnect, resave_rounds=…)` → `Destroying player: <P>` →(异步)`[ExitRelease] mark written player=<P> mark=<E>:<ms> site=exit_release`;
  - 若交接那次存盘走了 dirty-save 快路径(标记的 `SET` 已经排进被暂停的连接):`FinishExitAfterPersist: … (exit wins) handoff_mark_written=1`,随后 `[ZoneTravel] handoff mark landed but travel intent is gone/replaced …; not requesting EnterScene`,`DEL` 排在 `SET` 后面按序执行;这一形态**不写** A1′:`[ExitRelease] not writing release mark player=<P> reason=skip_handoff_inflight …`。
  - `[TravelHandoff]`:`started+1 exit_wins+1`,`granted / aborted` 不变。
- **通过标准**:✅ 实体正常销毁,**没有**以冻结态悬挂(无 `orphan PlayerFrozenComp` ERROR);✅ `player:{P}:handoff`:常见形态下 = `"E:<ms>"`(A1′ 断线释放标记,E == 当前 owner_epoch,`<ms>` 晚于交接开始,TTL ≤ 300),快路径形态下不存在;✅ `owner_epoch` 未变;✅ 基线重跑 OK(v2.4 起不必等 30s 断线租约),金币 = 本轮出发值(退出那次存盘落地了)。*v2.4 改期望:此前常见形态也要求键不存在;A1′ 落地后退出收敛时会写一份释放标记(设计文档 §12.6.3 第二步)。标记已写的那次交接被退出抢先时不写(M11),所以两种形态的期望不同。*

**C2 —— 标记已写、应答未回时退出(本场景的核心)**

- **注入**:`'at home' = { K-SM }`;`'travel out' = { Start-Sleep 3; docker exec redis redis-cli GET "player:<P>:handoff"; K-ROBOT }`(`<P>` 要从上一轮已知;`robot_9601` 的 player_id 跨轮不变)。
- **期望**(scene z1,按序):`[SavePlayerToRedis] skip: zone travel handoff already requested for player <P>` → `HandleExitGameNode: player <P> already persisted (dirty-save fast path); finishing exit inline` → `FinishExitAfterPersist: player <P> exited during ownership handoff; dropping handoff intent (exit wins) handoff_mark_written=1` → `[ExitRelease] not writing release mark player=<P> reason=skip_handoff_inflight …`(v2.4,M11:交接 EnterScene 可能在途,不写新标记)→ `Destroying player: <P>`。`[TravelHandoff]`:`exit_wins+1`;`[ExitRelease]`:`skip_handoff_inflight+1`,`written` 不变。30s 后应答看门狗到期时实体已不在,**不应**再有 `handoff aborted`。
- **Redis**:K-ROBOT 之前 `GET` 返回 `"E:<ms>"`;之后 1s 内 `EXISTS player:{P}:handoff` = **0**;`owner_epoch` = E。(v2.4 期望不变:本场景 A1′ 走 `skip_handoff_inflight`,键之后**不应**重新出现;若出现 `"E:<新 ms>"` = M11 没生效,保留证据。)
- **通过标准**:✅ 标记被撤回(这是 09-18 复审 P1 修的点:不撤回的话,玩家 30s 内重连回本节点继续玩,之后任意一次跨节点 EnterScene 都能凭这份旧标记免存盘过门 → 回档,`player_lifecycle.cpp:857-867`);✅ 恢复 SM 后基线重跑 OK。
- **与场景 F 的交叉处(缺陷 D-1,修复已于 2026-09-20 落码、未编译未验证)**:旧实现撤回用的 `DEL` 被 `redis && redis->connected()` 守着,Redis 断开时静默跳过、连日志都没有,标记残活到 300s TTL。现在两个撤回点统一走 `WithdrawHandoffMark`(待撤回表 + 按标记原文的条件删除 + 重连 / 1s 定时器重试,见 `handoff_mark_withdraw.h` 与设计文档 §12.1-A)。Redis 不通时的期望见 **F3**;不要把 C2 的通过外推到 Redis 不通的情形。
- 撤回现在有日志与计数(原缺口 G3 已补):成功 `[ZoneTravel][WithdrawMark] withdrawn player=<P> mark=<E>:<ms>`(INFO);当场发不出去 / 应答丢失 `[ZoneTravel][WithdrawMark] deferred … reason=redis_unavailable|dispatch_failed|reply_lost|reply_error`(首发 ERROR,后续重试 DEBUG + 每轮一条 WARN 汇总);`[TravelHandoff]` 行末尾 `withdraw_deferred=` / `withdraw_expired=`。C2(Redis 正常)下应只看到一条 `withdrawn`,两个计数增量为 0。

### 场景 D:目标 zone / 目标地图不可用

**D1 —— 目标 zone 没有可用 gate(或 zone 不存在)**

- **注入**:拷一份配置改目标(`run/` 在 `.gitignore:68`,不会脏工作树):把 `robot/etc/travel_smoke.yaml` 拷到 `run\etc\robot\travel_smoke_d1.yaml`,`visit_zone` 改成一个**不存在**的 zone(如 `9`;robot 只校验非 0 且 ≠ home_zone,`robot/config` `TravelSmokeConfig.validate`)。仍在 `robot/` 目录下跑 `.\robot.exe -c ..\run\etc\robot\travel_smoke_d1.yaml`。
- **期望**:scene_manager:`Cross-zone detected: … target_zone=9 place=true …` → `Cross-zone redirect failed for player <P>: no gate nodes available for zone 9` → 回 1。scene(z1):`[ZoneTravel] EnterScene rejected for player <P> code=1 msg=no gate available in target zone` → `[ZoneTravel] handoff aborted for player <P>: scene_manager rejected; unfreezing and keeping player on this node`。robot:`TRAVEL_SMOKE_FAIL step=travel-out reason=…受理后失败 SendTipToClient tip=3027…`。
- **通过标准**:✅ scene_manager **一个字节没改**(`owner_epoch` 仍是 E;无 `Cross-zone handoff released`);✅ 标记已撤回;✅ 冻结时长是亚秒级(`frozen_ms_max` 不被这一轮刷新到秒级);✅ 玩家留在原场景可继续操作(robot 失败后的 cleanup 能正常 LeaveGame,下一轮基线 OK)。
- 注意:C++ 侧判不了目标 zone 存不存在(没有 zone 表,`player_lifecycle.cpp:1455-1456`),所以这是**受理后失败**,不是同步拒绝;`kZoneTravelTargetZoneNotFound` 只在目标为 0 或本 zone 时同步返回。

**D2 —— 放行之后目标 zone 才不可达(票据过期未落地)**

- **注入**:`'at home'` 钩子里硬杀 zone 2 的全部 gate 进程(PID 取自 `cpp_nodes.pid.json` 的 `z2_gate*`)。gate 的 etcd 租约 180s 内 scene_manager 仍会把票据签给这个死 gate。
- **期望**:第一条腿**照常放行**(`Cross-zone handoff released … (awaiting placement)`,源实体 `[travel_redirect] … destroyed`);robot 在跟随 124 时失败:`follow gate redirect failed` → `TRAVEL_SMOKE_FAIL step=travel-out`。此刻**没有任何节点持有该玩家**,location 是 `{zone=2, node_id="", owner_epoch=E+1}`。
- 重登(从 zone 1 进):**v2.4 起不论 300s 内外**,等待落点都不牵引(设计文档 §12.7,R8 已被取代),scene_manager 按 gate zone(zone 1)落点并铸造新 epoch(E+2),不出现 `Cross-zone redirect only`;第 3b 步仍对这条传送残留查归属(未映射回 20)。*v2.4 改期望:此前写的是"300s 内再次把连接送往 zone 2、过 300s 才回家"。*
- **通过标准**:✅ 下一次重试即能在 home zone 进游戏(不再需要等 300s);✅ 金币 = 出发值(交接前那次存盘就是最终态,没回档);✅ 全程无双持有(两个 zone 的 scene 日志里 `HandlePlayerAsyncLoaded: Loading player <P>` 不重叠)。
- 恢复 zone 2:`cpp-node-start -CppNodes gate -Zone 2`。

**D3 —— 目标地图不可用**(可选)

- 同步拒绝那一半 travel-smoke 的 T2b 已经覆盖(`[ZoneTravel] rejected: scene_config_id is not a world map`,`reject_map_tip` 非 0)。
- scene_manager 第一条腿的只读预检(`[Travel] 跨 zone 传送被拒:目标 zone 没有这张图的世界频道`,`reason="travel_map_unavailable"`,回 1 → 源端解冻)需要一张"World 表里有、但目标 zone 没开频道"的图。本地两个 zone 用同一份表、开同样的频道,**造不出这个条件**;硬造要手工 `DEL world_channels:zone:2:<conf>`,会破坏 zone 2 的频道登记,world_init 是否自动重建**未核对**。首跑可 SKIP。
- 第二条腿回落(`[Travel] 等待落点记下的目标地图不可用,回落默认大世界`,`reason="pending_map_fallback"`)同理无现成注入手段。

### 场景 E:单 scene 节点硬崩后的属主接管(§11.5)

- **前置**:生产口径;zone 1 单 scene 节点 `z1_scene`,有 ≥1 名玩家在线且**不在交接中**。现成的常驻在线驱动是 `.\robot.exe -c etc/robot_smoke.yaml`(3 个 stress 机器人登 zone 1)——它在双 zone + 生产口径下能否登上、密码口径(该 yaml 写的是 `dev-local-secret-2026`,travel_smoke 是 `123456`)**未核对**,以能登上为准。
- **注入**:记 T0,K-SCENE 杀 `z1_scene`;随后 `cpp-node-start -CppNodes scene -SceneCount 2 -Zone 1` 补备用节点;停掉 robot 再重启它(= 玩家重登),每 30s 左右试一次。*(v2.5 注)备用节点必须在死节点租约到期**之前**起:C++ 节点启动时取全局最小空号,租约到期后才起的进程可能复用死节点的 node_id,撞上 GO-3(见 §6),接管永不触发。*
- **期望时间线**:

  | 阶段 | scene_manager 现象 |
  |---|---|
  | T0 → 租约到期(≤180s)| 节点仍在 etcd 注册表 → `playerLocationOwnerDead` 为假。请求被 18 暂拒(`handoff_pending_no_marker`),**或**被解析回死节点、回 0 后客户端超时(缺口 G7,拿不准)|
  | 租约到期 | leader:`[ReentryBarrier] 记录节点死亡时刻: zone=1 node=<n> barrier=20s` |
  | 屏障内(20s)| `[ReentryBarrier] dead_owner_takeover: 老属主刚判死,再入屏障还差 …`;`reentry_barrier_blocked_total{site="dead_owner_takeover"}` 上涨;请求仍回 18 |
  | 屏障过后 | `位置记录的属主节点已确认死亡且再入屏障已过,按无持有者处理: player=<P> … dead_node=<n>` → `Player <P> entered scene … on node <新节点> (… owner_epoch E+1)` |

  db:每名被接管的玩家一条 `owner-epoch advance`。
- **通过标准**:
  - ✅ 总锁定时长 ≈ 租约 TTL + 20s + 重试间隔,**无需运维清 location**;
  - ✅ `enter_scene_owner_dead_takeover_total{zone_id="1"}` 的增量 == 死节点上回来的玩家数(不是它的若干倍);没有节点死亡的其它时段它恒 0;
  - ✅ `db_stale_owner_write_rejected_total == 0`、无 `[OwnerEpoch]` 行(死节点写不了任何东西);
  - ✅ 数据 = 死节点最后一次成功存盘(周期存盘默认 300s,`redis.cpp:84-86`;这段进度丢失是崩溃固有代价,**不算失败**)。
- **判定要的是正面证据**:scene_manager 在屏障窗口内重启过的话,新进程没有"亲眼看到节点消失"的本地记录(`load_reporter.go:125-170` `nodeGoneObservedAt`),会多等;这属于 fail-closed,记现象不算失败。
- **未设计注入手法的反面**:节点没死、只是丢了 etcd 租约(僵尸)。预期是老进程 `[EmergencyRelocate] node identity lost; …` 自己存盘 + 写标记 + 请求改派;若它没来得及、又被接管铸了新 epoch,它之后的存盘会被 `HandlePlayerSaveRejected … (metric=stale_owner_write_rejected)` 拒绝并自毁——**这是全文唯一一处 `stale_owner_write_rejected` 非 0 属于预期的情形**。(限本 runbook 的故障注入场景;一般运行中的另一种预期来源 —— 旧节点退出存盘迟到被守卫拒 —— 见 §9。)

### 场景 E2:判死时 death_at 写不进(v2.5 新增,GO-6 推迟摘除;设计文档「P3 收尾:GO-6 / GO-4 残余 / 告警盲区」一节,代码未编译、本场景未实跑)

验证:scene_manager 判死一个节点时若 `node:zone:{z}:{n}:death_at` 写不进,**不把它摘出负载集**(节点按存活处理、谁都不能接管),补写成功后才摘(`recovered`),补不上则满一个再入屏障后不带标记摘(`expired`,告警)。

- **前置**:生产口径;zone 1 单 scene 节点 `z1_scene`,有 ≥1 名玩家在线且不在交接中(同场景 E 的常驻在线驱动)。先记下 Redis 原配置,结束时要原样恢复:

  ```powershell
  docker exec redis redis-cli CONFIG GET maxmemory          # 本地 compose 默认 8gb(= 8589934592)
  docker exec redis redis-cli CONFIG GET maxmemory-policy   # 本地 compose 默认 allkeys-lfu
  ```

- **注入**:
  1. 记 T0,K-SCENE 杀 `z1_scene`,**随即**补备用节点(`cpp-node-start -CppNodes scene -SceneCount 2 -Zone 1`,同场景 E;理由见下面"恢复")。死节点要等 etcd 租约(`NodeTTLSeconds: 180`)到期才会被 scene_manager 判死。
  2. **租约到期前**(约 T0+150s)压住 Redis 的写。**顺序不能反**:本地策略是 `allkeys-lfu`,先压上限会立刻按 LFU 淘汰键(含玩家数据)。

     ```powershell
     docker exec redis redis-cli CONFIG SET maxmemory-policy noeviction
     docker exec redis redis-cli INFO memory                       # 读 used_memory
     docker exec redis redis-cli CONFIG SET maxmemory <used_memory 的当前值>
     ```

     noeviction 下内存超过上限时,会增内存的写(SET / SETEX 等)一律被拒(OOM),不淘汰任何键;ZREM / DEL 照常。上限取**当前值(或略低)**:取得略高时,死节点的 `SETEX death_at` 可能正好落在余量里写成,注入不中。**全局副作用**:期间所有进程的写(存盘、会话、scene_manager 的落点 Lua)都会失败,属预期噪声;窗口尽量短,期间不做别的场景。
  3. 等租约到期(约 T0+180s),之后二选一:
     - **E2a(recovered)**:看到下面第一条 DeferDetach 日志后 **20s 内**解除限制 —— 先放开上限、再恢复策略:`CONFIG SET maxmemory <原值>`,然后 `CONFIG SET maxmemory-policy <原值>`。
     - **E2b(expired)**:保持限制 ≥25s(屏障 20s + 一拍 5s),看到 `expired` 那行后再按上面的顺序解除。
- **期望**(zone 1 的 scene_manager;本地每个 zone 一个 SM,即领导者):
  - 租约到期:`[ReentryBarrier][DeferDetach] 已判死但 death_at 未落地或摘负载集失败,节点暂留负载集(按存活处理),每拍重试: zone=1 node=<n> … err=写 death_at 失败: …`;**没有** `[ReentryBarrier] 记录节点死亡时刻`;指标 `scene_manager_node_detach_deferred_total{zone_id="1",outcome="deferred"}` +1(同时出现值为 0 的 `expired` / `abandoned` 序列)。
  - 之后每 5s 一条 `[ReentryBarrier][DeferDetach] 仍写不进 death_at,已等 <d> / 屏障 20s,下一拍再试: …`。
  - 推迟期间 `docker exec redis redis-cli ZRANGE scene_nodes:zone:1:load 0 -1 WITHSCORES` **仍含**该节点,`GET node:zone:1:<n>:death_at` 为 nil;`enter_scene_owner_dead_takeover_total{zone_id="1"}` **不涨**,没有 `位置记录的属主节点已确认死亡…按无持有者处理`。这段时间让玩家重登可以作旁证,但 scene_manager 自己的写也被 OOM 拒着,重登失败的形态(回 8、18 或 G7 的超时)不能单独当证据,以"没有接管"为准。
  - **E2a**:解除后的下一拍 `[ReentryBarrier] 记录节点死亡时刻: zone=1 node=<n> barrier=20s` → `[ReentryBarrier][DeferDetach] 补写 death_at 成功,已摘出负载集(已等 <d>): zone=1 node=<n>`;`outcome="recovered"` +1。之后按场景 E 的时间线走屏障(death_at 从补写时刻起算)与接管。
  - **E2b**:`[ReentryBarrier][DeferDetach] 自首次尝试已 <d> ≥ 屏障 20s 仍写不进 death_at,不带标记摘除(…): zone=1 node=<n> err=…`;`outcome="expired"` +1;节点离开负载集、death_at 仍为 nil。若这一拍看到的是 `摘负载集失败,下一拍再试` —— go-zero 的 Redis 熔断可能因连续 OOM 错误对 ZREM 也快速失败(**拿不准**),记录现象,不算失败,解除限制后下一拍应补写成功并记 `recovered`。告警 `SceneManagerNodeDetachWithoutDeathMark` 的表达式在之后 10 分钟内成立(本地没有 Prometheus / Alertmanager,手工拿注入前后两次 `curl -s http://127.0.0.1:9150/metrics` 快照做差即可)。屏障已由推迟态兜满,解除限制后接管不必再额外等 20s。
- **恢复**:`CONFIG GET` 确认两项都回到原值;重登探针同场景 E。备用 scene 节点之所以要在第 1 步杀节点之后**立即**补:让它在死节点租约还在时取号;租约到期后才起的新进程会取全局最小空号、可能复用死节点的 node_id,撞上 GO-3(见 §6),接管永不触发。
- **通过标准**:✅ 推迟期间节点一直在负载集里,**零接管**(`takeover_total` 增量 0);✅ E2a 记 `recovered`、E2b 记 `expired`,各 +1,`abandoned` 为 0;✅ 恢复后玩家能重新进入,数据 = 死节点最后一次成功存盘(同场景 E);✅ 无 `stale_owner_write_rejected`。
- **失败时保留**:scene_manager 日志里所有 `[ReentryBarrier]` 行;注入前后 `:9150/metrics` 快照;`ZRANGE` / `GET death_at` 的输出;`CONFIG GET` 的恢复记录。

### 场景 F:zone Redis 在交接窗口内断开

**F1 —— 交接存盘落地之前 Redis 就断了**(可稳定注入)

- **注入**:`'at home' = { docker stop redis }`。观察到 abort 之后 `docker start redis`。
- **期望**(scene z1):`handoff started` →(约 30s)→ `[ZoneTravel] handoff aborted for player <P>: handoff save did not land in time; …`;`[TravelHandoff]`:`save_watchdog_fired+1 aborted+1`。若那次存盘恰好走了快路径(盘上已是同一份),则是**立即** `handoff aborted … zone redis not connected`(`:1769-1773`)。*v2.5:快路径若发现同 key 还有更早的存盘没落地(例如 Redis 停着时该玩家有一笔存盘在退避重试),会先打 `… forcing a real save before writing the handoff mark (metric=handoff_fastpath_forced)` 改走真实存盘,现象随之变回 30s 存盘看门狗那一支。*期间可能出现 `HandlePlayerAsyncSaveFailed: DATA-DURABILITY RISK` ERROR —— Redis 停着时的预期噪声。robot:`TRAVEL_SMOKE_FAIL step=travel-out … tip=3027`。
- **通过标准**:✅ 冻结 ≤ 30s;✅ Redis 恢复后无 `player:{P}:handoff`、`owner_epoch` 未变;✅ 排队的存盘在 Redis 恢复后落地(`Saving complete for player: <P>`),下一轮基线的登录余额 = 本轮出发值(+11 没丢)。

**F2 —— 标记已写之后 Redis 短暂断开,在应答看门狗到期前恢复**

- **注入**:`'at home' = { K-SM }`;`'travel out' = { Start-Sleep 3; docker stop redis; Start-Sleep 12; docker start redis }`。
- **期望**:T3+30s 看门狗到期时 Redis 已连上 → `handoff aborted … EnterScene reply timed out`,标记被 `DEL`。若到期时 hiredis **还没重连上**(重连节奏拿不准),会先看到 F3 的 `cannot verify travel outcome …`,此时 robot 的 60s 预算会先到、断线退出,场景自动滑进 F3。
- **通过标准**:同 B1,外加 ✅ 无 `SET handoff mark result unknown`(出现它说明 Redis 是在 `SET` 回包前断的——也合法,走的是 `ResolveTravelOutcome`,记下来)。

**F3 —— 标记已写、Redis 持续断开(含玩家退出)—— 🟡 标记撤回(D-1)修复已落码待验证 / 🟡 冻结上限(D-2)修复已落码待验证(2026-09-28,未编译;打到上限的验证在场景 H)**

- **注入**:`'at home' = { K-SM }`;`'travel out' = { Start-Sleep 3; docker exec redis redis-cli GET "player:<P>:handoff"; docker stop redis }`。**不要**手工杀 robot:它 60s 超时后自己会断线,正好构成"退出"。约 90s 后 `docker start redis` 并立刻查键。
- **当前代码下的预期现象**(第 2、3 步按 2026-09-20 落码的 D-1 修复改写;该修复**未编译、未实跑**,首次执行即是对它的验证):
  1. (v2.5)`requested EnterScene` 之后数秒内先有一行 `[ZoneTravel] handoff EnterScene transport failure for player <P> corr=<N> (…); outcome unknown, …`(K-SM 下的 gRPC 失败回调,只记日志)。T3+30s:`[ZoneTravel] cannot verify travel outcome for player <P> (EnterScene reply timed out): zone redis unavailable; keeping the player frozen and re-arming the watchdog`;`[TravelHandoff]`:`reply_watchdog_fired+1 verify_rearmed+1`。*v2.5 改期望:此前写的是"每 30s 重复一次,没有次数上限、没有时长上限"(D-2)。现在冻结有 70s 硬上限:**客户端不断线时**,冻结起 70–71s 出现 ERROR `[ZoneTravel][MarkSentDestroy] player=<P> site=travel_freeze_cap … kick=1` + 踢线 34(场景 H1a 就是这条路径)。但 **robot 的一跳预算是 60s**(`robot/travel_smoke_scenario.go` `travelSmokeRedirectTimeout`),会先于上限断线,所以 robot 下本场景仍走第 2 步的"退出优先"。*
  2. T3+60s:robot 超时断线 → `[SavePlayerToRedis] skip: …` → `FinishExitAfterPersist: … (exit wins) handoff_mark_written=1` → `[ExitRelease] not writing release mark … reason=skip_handoff_inflight`(v2.4,M11)→ 实体销毁。撤回当场发不出去:`[ZoneTravel][WithdrawMark] deferred player=<P> mark=<E>:<ms> site=exit wins reason=redis_unavailable pending=1; scene change stays refused …`(ERROR,仅首发一条),此后每 5s 重试一次、每轮一条 WARN `retried 1 unconfirmed withdrawal(s) site=retry pending=1`;`[TravelHandoff]`:`withdraw_deferred` 持续上涨。
  3. Redis 恢复后(重连回调立即强制重发,`site=reconnect`):出现 `[ZoneTravel][WithdrawMark] withdrawn player=<P> mark=<E>:<ms>`;数秒内 `EXISTS player:{P}:handoff` = **0**;`GET player:{P}:owner_epoch` = E;`withdraw_expired` 增量为 0。**若仍读到 `"E:<ms>"` 且 TTL > 0 = D-1 的修复没生效**,按 §8 保留证据。Redis 停机超过约 305s 才恢复时,改为看到 `gave up on 1 mark(s): deadline (mark TTL) passed` 与 `withdraw_expired+1`——此刻 Redis 侧标记也已过期,同样合法。
- **D-1 为什么是缺陷(背景)**:残留期内玩家重连回本节点(同落点不铸造,epoch 仍是 E)继续产生新状态,之后任意一次跨节点 / 跨 zone EnterScene 都会凭这份旧标记**免存盘过换手门** → 目标节点读到旧档(回档),且 `owner_epoch` CAS 不会响(epoch 没变过),零报错。与 09-18 复审修掉的 P1 是同一个洞,只是入口换成了"Redis 不通"。`AbortTravelHandoff` 里的同款 `DEL`(`:2171-2175`)有同样的守卫;`BeginTravelHandoff` 的空 reply 分支已经为此改走 `ResolveTravelOutcome`;退出路径与 Abort 路径由 2026-09-20 的 `WithdrawHandoffMark` 补上。
- **通过标准(只针对 D-1)**:✅ 第 3 步的标记被撤回(或已随 TTL 过期并计入 `withdraw_expired`);✅ 撤回未确认期间让该玩家重登 zone 1 并发起换图,scene 日志出现 `[ZoneTravel][WithdrawMark] scene change refused for player <P>`、客户端收到"切换中"类 tip,而不是免存盘过门;✅ 撤回确认后换图恢复正常。**冻结上限(D-2)部分**:✅ 冻结 ≤ 71s(robot 下由退出优先在约 60s 收尾;`[TravelHandoff]` 的 `freeze_cap_reached` / `mark_sent_destroyed` 在 robot 下应不动);如实记录冻结持续了多久、`verify_rearmed` 涨到几。
- **残余(设计文档 §12.3「标记残留」)**:这道闸只管本节点替在线玩家发的请求;断线重登被 scene_manager 挑到**别的节点**时不经过它,撤回确认前仍只有 TTL 兜底。源实体已销毁、盘上即最终态,不回档,但值得在记录里注明落到了哪个节点。
- **D-2 已修(2026-09-28 落码,未编译未验证)**:冻结硬上限 70s(单调时钟)+ 35s 晚发闸 +「标记已发出」统一收口,设计见设计文档「交接冻结硬上限 + 晚发闸 +『标记已发出』统一收口」一节。本节的 🔴 已去掉,§6 已同步;打到上限的现象在场景 H 验证。"当前冻结中玩家数"的观测仍然没有(缺口 G2)。

### 场景 Z:Z1 复现与修复验证(v2.4 新增;设计文档 §12.6 / §12.6.10,代码未重新编译、本场景未实跑)

Z1 = 真写盘的正常断线退出不销毁实体(僵尸)。**必须先用修复前的二进制跑 Z-pre**(没有它就无法证明修复改变了现象),再用本批二进制跑 Z-post。单 zone、单 scene 节点即可,生产口径与否不影响本场景。

**Z-pre(修复前二进制:本批之前编出的 `bin/`,或检出 `15212c295` 编一份)**

- **注入**:robot(`login-test` 或 travel-smoke 的登录段)登录 → 做一次会改存盘数据的操作(例如 travel-smoke 的 GM 加金币)→ K-ROBOT。**必须改数据**:不改数据会走 dirty-save 快路径(`already persisted (dirty-save fast path); finishing exit inline`),那条路径修复前也会销毁,测不到 Z1。
- **期望(复现成功)**:`HandlePlayerAsyncSaved: Saving complete for player: <P>` 之后出现 `ignoring stale UnregisterPlayer … live session <S> indicates reconnect superseded the logout intent`,`<S>` 就是退出那条会话;**没有** `Destroying player: <P>`;此后该玩家还会被周期存盘(`Saving complete` 再出现)。保留这段日志前后 20 行。
- 复现不出来(直接出现 `Destroying player`)时,先确认走的不是快路径,再如实记录,不要往下判修复。

**Z-post(本批二进制)**

- **注入**:同 Z-pre。
- **期望**(scene 日志按序):`HandleExitGameNode: Player <P> is exiting the scene node (cause=client_disconnect)` → `HandlePlayerAsyncSaved: Saving complete for player: <P>` → `Player marked for unregistration: <P> (cause=client_disconnect, resave_rounds=0)` → `Player session removed` → `Destroying player: <P>` →(Redis 异步应答,**晚于**销毁)`[ExitRelease] mark written player=<P> mark=<E>:<ms> site=exit_release`。偶尔在 `Saving complete` 与 `marked` 之间多一轮 `… re-saving before destroying, round 1/5 (metric=exit_resave)`,属正常。
- **Redis**:`GET player:{P}:handoff` = `"E:<ms>"`,E == `GET player:{P}:owner_epoch`,`TTL` ≤ 300。
- **汇总行**(30s 内):`[ExitRelease]` 的 `attempted` 与 `written` 各 +1;`[ExitPersist]` 行若出现,`exit_superseded=0 exit_intent_missing=0 exit_resave_capped=0`。
- **重登**:30s 内同一节点重登 → `[ExitRelease][InheritClear] player=<P> epoch=<E> result=inherit_deleted_exact` → `HandlePlayerAsyncLoaded: Loading player <P>`(从 Redis 重载,不再复用旧实体)→ handoff 键为 nil;金币 = 退出前的值。
- **通过标准**:✅ Z-pre 出现 `ignoring stale UnregisterPlayer` 且无 `Destroying`;✅ Z-post 出现 `Destroying player` 与 `[ExitRelease] mark written`;✅ Z-post 全程**不出现** `ignoring stale UnregisterPlayer`(出现 = 跑的是旧二进制)与 `superseded by a newer session`;✅ 重登后数据不回档。
- **附加(可选)**:`SCENE_EXIT_RELEASE_MARK=0` 启动 scene 重做 Z-post → 开关日志 `[ExitRelease] SCENE_EXIT_RELEASE_MARK=0 -> off`,仍有 `Destroying player`,汇总行 `skip_disabled` +1,**没有** handoff 键(Z1 修复与 A1′ 相互独立)。M1 注入:R-PAUSE 撑住退出存盘时让另一个仍在线的会话 / GM 对该玩家发背包操作 → `exit_client_msg_rejected` +1;战斗结算期间退出 → 结算走离线暂存、不销账。

### 场景 R:GO-5 重连落点(v2.4 新增;设计文档 §12.7,代码未编译、本场景未实跑)

决定:**重连窗口(= login 的 30s 断线租约,挂机月卡顺延到 300s)内回到原处;超出窗口或主动登出回家。** login 在 ShortReconnect / ReplaceLogin 时发 `ZoneId=0`,scene_manager 只跟随落在具体节点上的 location。依赖场景 Z 的 A1′:回原处时若被挑到另一个节点,要靠 A1′ 标记过换手门。

- **前置**:生产口径(§3.1 第 2 步);login 与 scene_manager **都是本批二进制**(只上一边,R2 / R5 的现象会错);R1 需要 zone 1 起 2 个 scene 节点(`cpp-node-start -CppNodes scene -SceneCount 2 -Zone 1`),R2–R5 用双 zone。
- **通用观测**:login 日志 `[travel] EnterGame player=<P> decision=<d> 不指定去向(ZoneId=0)…`(`<d>` 为 ShortReconnect / ReplaceLogin);FirstLogin 不打这一行。

| # | 情形 | 注入 | 期望 |
|---|---|---|---|
| R1 | 同 zone 多节点快速重登 | 玩家在节点 X 上改一点数据后 K-ROBOT,**Z-post 的 `Destroying player` 出现之后** 30s 内重登 | login `decision=ShortReconnect … ZoneId=0`;无 18。挑到另一节点 Y:owner_epoch = E+1,Y 上 `inherit_deleted_older`,handoff 键 nil;挑回 X:epoch = E,`inherit_deleted_exact`,X 从 Redis 重载(无 `EnterScene: cancelled pending unregistration`)。退出存盘还在途时重登可能先回一次 18(`[Handoff] 场景交接 暂拒`),重试即过,属正确拒绝 |
| R2 | 访客在 B 区断线,30s 内从 A 区入口重登 → **回 B** | travel-smoke 走到 `arrived at visit zone` 后 K-ROBOT,等 zone 2 的 `Destroying player` 出现,30s 内用 zone 1 的入口重登 | login(z1) `decision=ShortReconnect … ZoneId=0` → scene_manager(z1) `Cross-zone redirect only (ownership unchanged): player=<P> target_zone=2` → 客户端跟随 124 → login(z2) `持重定向票据落地 zone=2` → scene_manager(z2) `Player <P> entered scene … (zone 2, home_zone 1, owner_epoch …)`;之后 scene(z2) 存盘 topic 仍是 `db_task_zone_1`。z2 落到另一节点时凭 A1′ 标记放行(epoch +1),同一节点时 epoch 不变 |
| R3 | 断线超过 30s → **回 A** | 同 R2,K-ROBOT 后等 >30s(确认 player_locator 已 LeaveScene;挂机月卡号要等 300s)再从 zone 1 入口登录 | login 不打 `ZoneId=0` 那行(FirstLogin);scene_manager(z1) 无 `Cross-zone redirect only`,`Player <P> entered scene … (zone 1, home_zone 1, owner_epoch E+1)`;z1 上 `inherit_deleted_older` 清掉 z2 写的 A1′ 标记 |
| R4 | 主动登出 → **回 A** | 在 zone 2 用 LeaveGame 正常登出(不是杀进程),立即从 zone 1 入口登录 | 同 R3,不需要等 30s |
| R5 | 跨 zone 传送途中断线后重登 → **回 A** | `'travel out' = { Start-Sleep 2; K-ROBOT }`(第一条腿已放行、location 为等待落点),30s 内从 zone 1 入口重登 | login `decision=ShortReconnect … ZoneId=0`;scene_manager(z1) **不**出现 `Cross-zone redirect only`(等待落点不牵引),直接 `Player <P> entered scene … (zone 1, …, owner_epoch E+2)`;金币 = 出发值。第一条腿还没放行时 location 仍在 z1 的节点上,同样落 z1 |

- **顶号**:另一台设备在 R2 的窗口内登录同一角色 → login `decision=ReplaceLogin … ZoneId=0`,落点规则同 R2(去角色当前所在 zone)。
- **通过标准**:✅ R1–R5 与上表一致;✅ 全程无 `stale_owner_write_rejected`、无双持有(§5 第 1、2 条);✅ 不出现"来回重定向"(同一次重登里 124 最多 1 跳)。
- **R2 的前提(2026-09-21 已静态核实)**:login(z1) 能看到玩家在 zone 2 留下的 DISCONNECTING 会话。会话键 `player:session:{id}` 不带 zone(`go/player_locator/internal/logic/keys.go:7`),存在全服单一的 SharedRedis(`docs/design/cross-zone-matchmaking.md` D12);`sessionmanager.CanReconnect` 只比状态与账号、不比 zone(`go/login/internal/logic/pkg/sessionmanager/session_manager.go:180-185`)。**部署前提**:各 zone 的 player_locator 必须连同一个 Redis(D12);若某环境按 zone 拆了 Redis,login(z1) 会判 FirstLogin(没有 `ZoneId=0` 那行)、带 `ZoneId=1`,玩家凭 A1′ 标记回 A —— 那是环境违反 D12,记下 login 日志里的 decision 回报,不要改代码凑现象。
- **客户端**:登录期重定向若先于 EnterGame 应答到达(此时 `PlayerId` 仍为 0),第二条腿沿用本次 EnterGame 请求的角色,不回选角(`mmorpg-client` `GameClient.ResolveRedirectPlayerId`,2026-09-21)。用 Unity 跑 R2 时,第二条腿不应弹出选角界面。

### 场景 H:冻结硬上限(v2.5 新增,D-2 的修复验证;设计文档「交接冻结硬上限 + 晚发闸 +『标记已发出』统一收口」一节,代码未编译、本场景未实跑)

**要验证的规则**:冻结按单调时钟从 `handoff started` 那一刻起算,最迟约 71s(70s 上限 + 1s 扫描)必有结论。分界线是 handoff 标记的 SET **发没发出去**:

- **没发出**(Redis 在写标记之前就不可用)→ 解冻 + 失败 tip,由 30s 存盘看门狗(F1)或 35s 晚发闸 `set_mark` 阶段先收敛;上限的 `[ZoneTravel][FreezeCap] … action=unfreeze` 一支只是兜底,**应恒不出现**,不单独设场景(单测 `TravelFreezeCapEcs.MarkNeverSentUnfreezesWithoutWithdrawal` / `BeginTravelHandoffPastDispatchWindowAbortsWithoutMark` 覆盖)。
- **已发出**(本场景)→ 永不解冻:tip + 踢线 34 + 不存盘销毁(`[ZoneTravel][MarkSentDestroy]`)。跨 zone 的 tip 与 34 `reason.id` 都是 `kZoneTravelTargetBusy`(3027),同 zone 都是 `kEnterSceneFailed`(3023)。

**必须用 Unity 客户端**:robot travel-smoke 的一跳预算 60s < 70s,robot 会先断线走"退出优先",看不到上限分支(要 robot 自动验收需另案给 robot 加 ≥75s 的可配置预算,归 robot 属主)。Unity 客户端"已受理的交接"等结论的预算是 75s(`CityTravelRequest.AcceptedHandoffBudgetSeconds`;服务端 `travel_freeze_cap.h` 的 `kClientAcceptedHandoffBudget` 镜像它并用 `static_assert` 守着 71 < 75)。用 DevAutoPilot `-travelZone 2` 或手动从地图窗发起跨 zone 传送。客户端侧的日志 / 文案本文**未核对客户端仓**,以下按规格摘录,首跑以实际为准。

**H1a —— 标记已写、Redis 持续断开超过 70s(U1:核实时 Redis 不可用,看门狗重挂)**

- **前置**:生产口径;双 zone;Unity 客户端在 zone 1 游戏内、不在战斗 / 队伍中。
- **注入**:
  1. K-SM(`dev_tools.ps1 -Command go-svc-stop -GoServices scene_manager`),然后在 SM 的 60s etcd 租约内发起跨 zone 传送,让 EnterScene 拿不到应答。
  2. scene(z1) 日志出现 `[ZoneTravel] requested EnterScene for player <P> … corr=<N>` 之后、**25s 以内**执行 `docker stop redis`(晚于 30s 应答看门狗时,核实会在 Redis 还通时做完,结局变成 B1 的"解冻 + tip")。stdout 日志有缓冲(§3.1 第 5 步),看不到就看 `bin\logs\cpp_nodes\` 的 muduo 文件。
  3. Redis 保持停止到 `[ZoneTravel][MarkSentDestroy]` 出现之后。
- **期望**(scene z1,按序;T0 = `handoff started`):
  - `[ZoneTravel] handoff started for player <P> target_zone=2 (cross-zone) …` → `requested EnterScene … corr=<N>` → `[ZoneTravel] handoff EnterScene transport failure for player <P> corr=<N> (…); outcome unknown, leaving the verdict to the reply watchdog / freeze cap`(只记日志)。
  - 约 T0+30s、T0+60s 各一行 `[ZoneTravel] cannot verify travel outcome for player <P> (EnterScene reply timed out): zone redis unavailable; keeping the player frozen and re-arming the watchdog`。
  - **T0+70–71s**:ERROR `[ZoneTravel][MarkSentDestroy] player=<P> site=travel_freeze_cap target_zone=2(cross-zone) mark=<E>:<ms> enter_scene_sent=1 corr=<N> evidence=none frozen_ms=<70000~71000> exiting=0 kick=1: handoff mark already sent and ownership cannot be verified here; destroying without persisting (ownership UNKNOWN, not confirmed moved)` → `Destroying player: <P>` → WARN `[travel_freeze_cap] local entity for player <P> destroyed without persisting (ownership moved away)`(**沿用的原文,这里表示归属未知**;`DestroyDeposedPlayer` 先销毁、后打这行)。
  - 随后客户端断线,gate 的 ExitGame 到达时实体与会话都已不在,scene 可能打一行 `ProcessClientPlayerMessage: session id not found …`(预期噪声,从代码推的)。
  - `[TravelHandoff]` 增量:`started+1 started_cross_zone+1 reply_watchdog_fired+2 verify_rearmed+2 freeze_cap_reached+1 mark_sent_destroyed+1 mark_sent_client_reset+1`;`granted` / `aborted` / `exit_wins` / `destroy_deferred_unsettled_save` / `freeze_unstamped` 不变;`frozen_ms_max` ≈ 70000–71000。`reply_watchdog_fired` / `verify_rearmed` 的次数取决于 Redis 停的时刻与 30s 节拍,±1 都算对。
  - 客户端:日志 `[gate] kicked by server reason=3027`(tip 3027 可能先到),显示"目标区繁忙"一类的原因后回到选服。
- **恢复**:`docker start redis`;`go-svc-start-exe -GoServices scene_manager -Zone 1`,再 `-Zone 2`。
- **恢复后核对**:重登成功;金币 = 出发时的值;`GET player:{P}:owner_epoch` = E(SM 全程是死的,不可能铸造);全程无 `stale_owner_write_rejected`;`handoff started` 与 `MarkSentDestroy` 两行时间戳差 ≤ 71s。`player:{P}:handoff` 可能仍是 `"E:<ms>"`(上限收口**不撤回标记**,它起断线释放标记的作用,下一次载入由 A2′ 清掉)—— 正常。
- **SKIP 条件**:出现 `[ZoneTravel] handoff not started … no SceneManager node reachable`(没有 `handoff started`)或 `site=travel_no_scene_manager` = SM 已先被摘掉,记 SKIP,调整 K-SM 的时机后重做(§3.2 K-SM)。出现 B1 形态(`handoff aborted … EnterScene reply timed out`)= `docker stop redis` 晚了,重做。

**H1b —— 标记已写、Redis 半开(U3:核实命令发出后没有应答)**

- **注入**:同 H1a 第 1 步;第 2 步改为在 `requested EnterScene` 之后约 25s 执行 `docker exec redis redis-cli CLIENT PAUSE 60000 ALL`(TCP 不断,hiredis `connected()` 仍为真,30s 看门狗的 `DEL` + `MGET` 发出去之后一直没有应答)。
- **期望**:与 H1a 相同,区别是**没有** `cannot verify travel outcome` 行(Redis 看上去是连着的),`verify_rearmed` 不涨、`reply_watchdog_fired+1`;T0+70–71s 同样是 `[ZoneTravel][MarkSentDestroy] … site=travel_freeze_cap … enter_scene_sent=1 … evidence=none … kick=1`。PAUSE 到期后那条迟到的 `MGET` 应答回调发现实体已不在,不打任何裁决行;排在前面的 `DEL` 此时才执行,会把标记删掉 —— 所以恢复后 `player:{P}:handoff` 不存在也是正常的(只影响"重登被挑到别的节点时要等断线租约"的活性)。
- **恢复 / 核对 / SKIP**:同 H1a(恢复时等 PAUSE 自然到期,不用 `docker start`)。

**H2 —— 晚发闸 `enter_scene` 阶段(能造就做,造不出记 SKIP)**

- **前置**:Unity 客户端站着不动,等最近一次存盘落地后再操作,让交接走**快路径**(判据:`handoff started` 之后没有这名玩家的 `[SavePlayerToRedis] Player <P> saved to Redis …`,也没有 `forcing a real save before writing the handoff mark`,标记直接写出)。
- **注入**:发起传送前一刻执行 `docker exec redis redis-cli CLIENT PAUSE 40000 WRITE`,SET 约 40s 后才回应答(不需要 K-SM)。
- **期望**:`handoff started` → 约 40s 后 WARN `[ZoneTravel][DispatchWindow] player=<P> stage=enter_scene frozen_ms=<≈40000>: handoff mark reply arrived too late to verify ownership before the freeze cap; not sending EnterScene` → WARN `[ZoneTravel][MarkSentDestroy] player=<P> site=travel_dispatch_window … enter_scene_sent=0 corr=0 evidence=none … kick=1 …` → `Destroying player: <P>` → `[travel_dispatch_window] local entity for player <P> destroyed without persisting (ownership moved away)`;客户端被踢,原因同 H1a;**没有** `requested EnterScene`、**没有** `[ZoneTravel][WithdrawMark]`。`[TravelHandoff]`:`dispatch_window_closed+1 mark_sent_destroyed+1 mark_sent_client_reset+1`,`freeze_cap_reached` 不变。
- **判 SKIP**:看到 `handoff save did not land in time`(= 没走快路径,存盘被 PAUSE 卡住、30s 存盘看门狗解冻),或 `forcing a real save before writing the handoff mark`(快路径被补写拦下,同样会被 PAUSE 卡住)。实际走到了哪一步如实记录。

**不做**:墙钟跳变(改系统时间属于修改系统设置,AGENTS §10.2 应停手),只靠单测 `TravelFreezeCap.EarlyFireWithinToleranceRunsInsteadOfRearming` / `RemainingUntilNeverNegative` 覆盖。

**回归(每次跑 H 之前 / 之后)**:§3.3 基线 `TRAVEL_SMOKE_OK`,且基线里 v2.5 的冻结类计数全为 0、不出现 `KickPlayer` / `kicked by server`;B1 仍是"解冻 + tip,不踢线"。

**失败时保留**:scene 日志里所有 `[ZoneTravel]`、`[TravelHandoff]`、`[FreezeCap]`、`[DispatchWindow]`、`[MarkSentDestroy]` 行;客户端断线前后各 20 行。

### 场景 SE:gate 侧进场转发补发(v2.5 新增,CPP-2;设计文档「CPP-2 gate 侧进场转发补发」一节,代码未编译、本场景未实跑)

用 SE 而不用 G,避免和 §6 的缺口 G1–G10 混淆。gate 相关日志见 §2.4「C++ gate」表。

**SE —— 路由指向一个连不上的 scene,20s 后放弃并踢回选服**

- **前置**:zone 1 只起一个 scene 节点 `z1_scene`;测试号已在游戏内(Unity 客户端,或 robot 常驻在线驱动)。
- **注入**:
  1. K-SCENE 硬杀 `z1_scene`(`Stop-Process -Id <pid> -Force`)。本地 `NodeTTLSeconds: 180`,节点在这 180s 内不会被摘除,location 仍指向它。
  2. 30s 内让客户端断线重登(ShortReconnect,scene_manager 按 location 把玩家派回原节点)。
- **期望**(gate z1,按序):
  - `[SceneEntry] forward deferred session_id=<S> … failure=not_connected …`(采样,但每线程第一条必打);
  - 约 20s 后 ERROR `[SceneEntry] giving up session_id=<S> … attempts=<12~16> first_failure=not_connected last_failure=not_connected elapsed_ms=<20000~20300>,推送 kEnterSceneFailed 并踢线、关闭会话`;
  - 随后 `Client disconnected, session_id=<S>`(约 1s 内 gate 强关连接)。
  - 客户端:收到 tip 3023 + 踢线 34(reason 3023),提示"进入场景失败"一类原因并回到选服,而**不是**等满 60s 后的通用"等待进入场景超时"(文案以客户端实际为准)。
- **未命中判据**:login 直接推 3023(scene_manager 回了 18 之类的拒绝),gate 没有任何 `[SceneEntry]` 行 —— 记为"未命中",如实记录,**不算失败**。顺带这就是缺口 G7 的答案:命中 SE = scene_manager 在租约窗口内把请求解析回了死节点(G7 的第二种),未命中 = 走了 18。
- **恢复后**:按启动器重新拉起 scene(`cpp-node-start -CppNodes scene -Zone 1`),gate 出现 `[SceneEntry] scene link ready node_id=<n'> uuid=<u>`(基线检查点:新链路握手就绪)。再登录能否立刻进场取决于 location:死节点的租约(180s)到期之前,location 仍指向死节点,重登会重复 SE 的现象或回 18;到期 + 再入屏障 20s 之后按场景 E 被接管进新节点(**从代码推的,首跑记录**)。若新进程是在死节点租约到期**之后**才起、复用了同一个 node_id,会撞上 GO-3(本轮不落,见 §6),记下 `node_id`,不算本场景失败。

**SE2 —— 新节点刚起来时的握手竞态(尽力而为)**

- **注入**:zone 1 第二个 scene 节点刚注册时(`cpp-node-start -CppNodes scene -SceneCount 2 -Zone 1` 之后立即)让一批 robot 登录。
- **期望**:若出现 `[SceneEntry] forward recovered … last_failure=handshake_pending|not_connected …`,同一 `session_id` 必须真的进场(robot 收到 NotifyEnterScene),且新节点日志里**没有** `RpcSession not found for gate`(= 进场通知在握手前被发出、被 scene 丢掉,CPP-2 修的正是这个竞态)。
- 没命中不算失败,如实记录。

**失败时保留**:gate 日志里所有 `[SceneEntry]` 行,以及同一 `session_id` 前后 50 行;同一时段 scene 日志里的 `RpcSession not found` 行。

---

## 5. 全局不变量(每个场景都要核)

1. **无双主**:所有 scene 进程日志里**没有** `HandlePlayerSaveRejected: owner_epoch CAS rejected save`,**没有** `[OwnerEpoch]` 行;两个 zone 的 db `db_stale_owner_write_rejected_total` 增量为 0,无 `STALE-OWNER-WRITE rejected`。唯一例外见场景 E 末尾的僵尸情形。
2. **任一时刻只有一个持有者**:按时间排 `HandlePlayerAsyncLoaded: Loading player <P>` 与 `Destroying player: <P>` / 进程死亡时刻,新持有者的载入必须晚于旧持有者的销毁(或死亡 + 屏障)。
3. **不落错库**:访客区 scene 的 `[SavePlayerToRedis] … (topic=…)` 永远是 `db_task_zone_<home_zone>`;`scene_manager_home_zone_lookup_total{outcome="error"}` 与 `{outcome="unconfigured"}` 增量为 0。
4. **不回档、不串档**:金币永远 ∈ 本轮已知的合法值集合,**绝不为 0**(0 = 目标区读不到档、把人当新号建了空实体,它一存盘就覆盖原档——travel-smoke 专门为抓这个设计了出发值,`travel_smoke_scenario.go:91-99`)。
5. **没有永久冻结**:静置后,存活进程的 `[TravelHandoff]` 满足 `started == granted + resolved_in_place + aborted + exit_wins + mark_sent_destroyed`(出处:`player_lifecycle.h` `travel_handoff_stats` 注释末尾,2026-09-29 在 :197-198;差值 = 仍在途的 + 交接期间被存盘 CAS 拒而销毁的,后者计入 `stale_owner_write_rejected`,按第 1 条应为 0);无 `orphan PlayerFrozenComp`。v2.5 起**冻结有界**:每一行 `[ZoneTravel] handoff started` 在 71s 内都跟着一个终态(`handoff granted` / `same-zone handoff granted` / `handoff resolved in place` / `handoff aborted` / 退出优先 / `[ZoneTravel][MarkSentDestroy]`),唯一允许的例外是 `destroy_deferred_unsettled_save` 非 0(有未落地存盘而推迟销毁,出现即保留证据)。*v2.4 写的"F3(D-2)是已知例外"已删:D-2 的修复已落码(未编译)。GO-2 根治(设计已定稿,落码中)也会改 `[TravelHandoff]`,它落地后若新增终态,本式随之扩展,`mark_sent_destroyed` 保留。*
6. **拒绝不改状态**:凡 scene_manager 回 1 / 8 / 18 / 19 / 20 的请求,`owner_epoch` 与 location 前后一致;回 7 的请求要么已回滚(`rolling back location/epoch` 之后 `rollback_total{outcome="rolled_back"|"already_rolled_back"}` +1),要么有 `[RouteRollback] outcome=superseded|redis_error` 可查;`redis_error` 且跨 zone 时源 scene 必须有对应的 `[ZoneTravel][ClientReset]`(或写明为何不踢的 `not resetting client`)。
7. **不是 dev 旁路**:scene_manager 日志无 `[Handoff] dev 旁路`;无 14 号错误码。
8. **(v2.4)退出不留僵尸**:每一条 `HandleExitGameNode: Player <P> is exiting …`(原因不是交接 / 改派的)最终都跟着 `Destroying player: <P>`;全程无 `ignoring stale UnregisterPlayer`、无 `superseded by a newer session`、无 `exit_resave_capped`;`[ExitRelease]` 行的 `failed` / `inherit_refused` / `inherit_epoch_mismatch` 增量为 0(场景 Z / R 的故障注入步骤除外)。
9. **(v2.5)新计数的恒 0 项**:全栈都是本批二进制时,`[TravelHandoff]` 的 `reply_uncorrelated`、`freeze_unstamped`、`destroy_deferred_unsettled_save`、`watchdog_early_fire` 增量为 0;`freeze_cap_reached` / `dispatch_window_closed` / `mark_sent_destroyed` / `mark_sent_client_reset` 只在场景 H 里非 0(`mark_sent_destroyed` / `mark_sent_client_reset` 另可能因 K-SM 时序不巧的 `site=travel_no_scene_manager` 各 +1,见 FC-3);`reply_unmatched` 在本文的单玩家场景里应为 0。gate 无 `[SceneEntry] giving up`(场景 SE 除外);scene_manager 的 `node_detach_deferred_total` 各 outcome 增量为 0(场景 E2 除外)。

---

## 6. 已知缺口与「当前预期失败」汇总

**待修缺陷(不计入通过,但必须记录现象)**

| # | 缺陷 | 位置 | 对应场景 |
|---|---|---|---|
| D-1(**修复已落码 2026-09-20,未编译未验证**;F3 首次执行即是验证) | 退出路径撤回 handoff 标记的 `DEL` 被 `redis->connected()` 守着,Redis 断开时静默跳过,标记对当前 epoch 有效地残活 ≤300s → 后续跨节点落点可免存盘过门(回档,零报错)。`AbortTravelHandoff` 的 `DEL` 同款 | `player_lifecycle.cpp:868-875`、`:2171-2175` | C2 × F → **F3** |
| D-2(**修复已落码 2026-09-28,未编译未验证**;场景 **H** 即验证)| 冻结没有服务端上限:Redis 不可用时 `ResolveTravelOutcome` 无限重挂 30s 看门狗,玩家只能靠断线("退出优先")脱身。修法:70s 单调时钟硬上限(`EnforceTravelFreezeCaps`,1s 扫描)+ 35s 晚发闸 +「SET 已发出」统一收口为 tip + 踢线 34 + 不存盘销毁(`ConcludeHandoffAfterMarkSent`)+ 两道看门狗按单调时钟复核 + 快路径补查未落地存盘;设计文档「交接冻结硬上限 + 晚发闸 +『标记已发出』统一收口」一节 | `ResolveTravelOutcome` 的三处重挂(Redis 未连接 / MGET 应答形状不对 / 命令发不出去)与 `DEL`+`MGET` 发出后不挂定时器,现由 `player_lifecycle.cpp` `EnforceTravelFreezeCaps` 封顶 | **H**(F3 在 robot 下由退出优先先收尾,不打到上限)|
| GO-3(**本轮不落**,需用户拍板;设计见设计文档 §12.3 GO-3 行)| 死节点的 node_id 被新进程复用后,属主接管永不触发:C++ 节点启动时取全局最小空号(`node_allocator.cpp` `AcquireNode`),死节点租约一到期它的号就能被新进程拿走;scene_manager 的注册表镜像只按 (zone, node_id) 记,同号新进程一注册,`playerLocationOwnerDead` 就恒为假 → 请求回 18(解析到别的节点时),或被派给同号的新进程(解析回原节点时,不铸造)。性质是活性问题(回可重试的 18,不引入数据风险;玩家断线约 31s 不重登时,player_locator 的断线租约到期会 `LeaveScene` 删掉遗留 location,真正卡住的是间隔 <30s 持续重试的客户端,见设计文档 §12.3 GO-3 行)。推迟原因:根治方案的设计残余 R1 —— 滞留在 Kafka / gate 里的路由事件可能在同号新进程注册之后才投递,让一个健康的活进程在没发 handoff SET 的情况下被判"已替换"而丢掉归属,违反交接不变量;根治要改 C++ 与路由协议 | `enterscenelogic.go` `playerLocationOwnerDead`、`load_reporter.go` 注册表镜像 | 场景 A / E / E2 / SE 的"补节点":备用节点一律在死节点租约到期**之前**起 |

**已接受的残余(v2.5,冻结上限带来的;不计入通过,遇到如实记录)**

| # | 残余 | 现象 / 对应场景 |
|---|---|---|
| FC-1 | **墙钟回拨会推迟上限**。所有判定按单调时钟,但唤醒靠 muduo 定时器,而 muduo 按墙钟(Windows 上 `system_clock`)到期:墙钟回拨 X 秒,1s 上限扫描与两道看门狗最多被推迟约 X 秒。墙钟前跳不会让任何判定提前(看门狗提前醒来按剩余时间重挂,计 `watchdog_early_fire`)。引擎级问题,本批不修 | 冻结超过 71s 且没有 `destroy_deferred_unsettled_save` 时,先查主机时间是否被回拨;本文不做改系统时间的注入 |
| FC-2 | **同 zone 上限踢线可能多踢一次已改绑的会话**。到上限时 epoch 状态未知(可能已放行且会话已被 RoutePlayerEvent 改绑到新节点,也可能没放行 / 路由丢了 / 铸造后又回滚);不踢就会留哑连接,所以同 zone 也踢。gate 不核对 34 来自哪个节点,若其实已放行,这次 34 会断掉一条合法会话。跨 zone 同理:踢线可能先于一次迟到但成功的 124 到达。数据都安全,代价是多重登一次;精确修法("gate 只认会话当前绑定节点发来的 34")是设计文档 §12.5.6 残余 1 的另案 | 同 zone 的 `[MarkSentDestroy] … target_zone=<本 zone>(same-zone) … kick=1` 之后,玩家其实已在另一节点上 —— 记录,不算失败 |
| FC-3 | **SET 之后 SM 消失时会踢线**。`StartTravelHandoff` 的 SM 预检把"请求时 SM 就不在"挡在冻结之前(同步回失败 tip);剩下"SM 恰好在 SET 往返那几毫秒里消失"的窄窗口,v2.4 是解冻 + tip,v2.5 改为 `[MarkSentDestroy] … site=travel_no_scene_manager` + 踢线回选服(按骨架,SET 已发出就不许解冻)| K-SM 时序不巧时出现;B1 / H 里遇到按 SKIP 处理 |

**观测 / 注入缺口(想写进通过标准,但代码里没有对应观测点)**

| # | 缺口 |
|---|---|
| G1 | C++ 的交接 / 归属计数没进 Prometheus,只有 `[TravelHandoff]`(有变化才打)与 `[OwnerEpoch]`(非 0 才打)两行 30s 日志;`stress_summarize.ps1` 也不解析(`player_lifecycle.cpp:176-178` 自述"目前还没有解析方",设计文档 §10.3)。进程被硬杀时最后 <30s 的计数直接丢失,场景 A / E 里看不到被杀节点的最终计数 |
| G2 | 没有"当前冻结中玩家数 / 单个玩家已冻结多久"的观测;`frozen_ms_*` 只在走到终态时累计,一直冻着的玩家只体现为 `verify_rearmed` 缓慢上涨。*v2.5:单个玩家的冻结已被 70s 上限封顶(例外只有 `destroy_deferred_unsettled_save`),到上限会留下 `freeze_cap_reached` 与 `[ZoneTravel][MarkSentDestroy] … frozen_ms=…`;"当前冻结中"的实时观测仍然没有* |
| G3 | ~~撤回标记的 `DEL` 是空回调,无日志无计数~~ **已随 D-1 的修复补上**(`[ZoneTravel][WithdrawMark]` 日志 + `withdraw_deferred` / `withdraw_expired`),仍未进 Prometheus(同 G1) |
| G4 | 没有故障注入钩子:"存盘落地 → 写标记"同栈连续执行;"铸造 Lua 之后、应答之前"只有毫秒;`handoff_pending_withdrawn`(预检与铸造之间标记被撤回)、`epoch_conflict`(19)同样无法手工触发。场景 A 只能测等价类,B2 靠碰运气,19 / withdrawn 只有单测覆盖(`owner_epoch_test.go`,**均未运行**)|
| G5 | `player:{P}:location` 是 proto 二进制,scene_manager 没有查询位置的 debug 端点(`/debug` 下只有 `rebalance-plan`,`world_rebalance_debug.go:43`)。location 的 zone / node / epoch / `pending_scene_conf_id` 只能靠日志反推 |
| G6 | scene_manager 只对**拒绝**计数,「凭标记放行」没有指标,只能数日志 `Cross-zone handoff released` 或 db 的 `owner_epoch_guard_total{outcome="advance"}` |
| G7 | 节点已死但 etcd 租约未到期的窗口(≤180s):设计文档 §11.5 写的是"请求照旧被 18 暂拒",但从代码看,若解析出的场景仍映射在死节点上,会命中 `samePlacement`(`enterscenelogic.go:434-491`)→ 回 0 并把路由发给已死进程,客户端只是超时,**没有任何拒绝指标**。两种都可能,拿不准,首跑记录。*v2.5:gate 侧有了 CPP-2 的补发与放弃,第二种的现象变成 gate `[SceneEntry] forward deferred … failure=not_connected` → 约 20s 后 `[SceneEntry] giving up`、客户端收 3023 + 34 回选服,不再是静默超时;场景 SE 命中与否就是 G7 的答案。scene_manager 侧仍没有拒绝指标* |
| G8 | travel-smoke 是一次跑到底的冒烟,没有"停在交接窗口 / 在窗口内退出"的开关;场景 B / C / F 的时序全靠外部撑窗口 + 杀进程 |
| G9 | scene 选 scene_manager 不按 zone 过滤(`node_utils.cpp:15-35`),双 zone 下第一条腿落在哪个 SM 上不确定,日志与指标要两边找;K-SM 因此只能"全杀" |
| G10 | 客户端侧(Unity `DevAutoPilot -travelZone / -travelScene / -quitOnTravelEnd`,设计文档 §11.1)本文**未核对**(服务端任务不读客户端仓)。*v2.5:Unity 的"已受理交接"预算是 75s(服务端 `travel_freeze_cap.h` `kClientAcceptedHandoffBudget` 镜像它),长于 robot 的 60s,场景 H 因此只能用 Unity 做;robot 要覆盖上限分支需另案加 ≥75s 的可配置预算* |

---

## 7. 结果记录

记录到 `docs/ops/cross-zone-test-results-<date>.md`:

```markdown
# Cross-Zone Handoff Failure Test Results — YYYY-MM-DD

Operator: <name>      Build: <commit-sha>      Env: local dual-zone / k8s
AllowUnsafeCrossNodeHandoff: false(已核对日志无 "[Handoff] dev 旁路")
Runbook 版本: v2.5(首跑 = 校准轮;与实际不符处已回改 runbook:是 / 否,改了哪些)

| 场景 | 结果 | 备注(实际现象、耗时、与 runbook 不符处) |
|---|---|---|
| 基线 travel-smoke | OK / FAIL | 第二条腿 epoch 实际 = E+? |
| A 存盘后写标记前崩溃(等价类) | PASS / FAIL | 租约窗口内是 18 还是路由到死节点(G7);锁定总时长 |
| B1 应答丢失-未放行 | PASS / FAIL / K-SM 无效 | 冻结时长 |
| B2 应答丢失-已放行 | PASS / FAIL / SKIP(注入不出来) | 走的是踢线还是退出优先 |
| B3 推重定向失败 + 回滚 Redis 错误 | PASS / FAIL / 未命中(ACL 翻晚) | 证据是 failed 还是 no_reply;robot 两种 FAIL 文案中的哪一种;重登后 epoch |
| C1 退出优先-标记未写 | PASS / FAIL | 走的是哪一种日志形态 |
| C2 退出优先-标记已写 | PASS / FAIL | DEL 前后键状态 |
| D1 目标 zone 无 gate | PASS / FAIL | |
| D2 放行后目标不可达 | PASS / FAIL | 多久后能回家 |
| D3 目标地图不可用 | PASS / FAIL / SKIP | |
| E 单节点硬崩接管 | PASS / FAIL | takeover_total 增量 vs 玩家数;锁定总时长;备用节点的 node_id 是否与死节点不同(GO-3)|
| E2 判死时 death_at 写不进(GO-6)| E2a PASS / FAIL;E2b PASS / FAIL / 熔断未命中 | `node_detach_deferred_total` 各 outcome 增量;推迟期间 takeover_total 增量(应 0);E2b 是 expired 还是"摘负载集失败,下一拍再试";maxmemory / policy 已恢复原值 |
| F1 存盘前 Redis 断 | PASS / FAIL | |
| F2 窗口内 Redis 闪断 | PASS / FAIL / 滑进 F3 | hiredis 重连耗时 |
| F3 Redis 持续断 + 退出 | 标记撤回(D-1):PASS / FAIL;冻结上限:PASS / FAIL(冻结 ≤71s)| 冻结时长、verify_rearmed、withdraw_deferred / withdraw_expired、恢复后标记是否还在及剩余 TTL;robot 下应是退出优先先收尾(`freeze_cap_reached` 不动)|
| Z 修复前复现(Z-pre) | 复现 / 未复现 | 是否确认没走快路径;`ignoring stale UnregisterPlayer` 那行的会话号 |
| Z 修复后验证(Z-post) | PASS / FAIL | `Destroying` 与 `[ExitRelease] mark written` 的先后;handoff 键原文与 TTL;重登的 `inherit_*` 结果 |
| R1 同 zone 多节点快速重登 | PASS / FAIL | 落到哪个节点;epoch;是否先回过一次 18 |
| R2 访客窗口内从 A 重登 → 回 B | PASS / FAIL / 拿不准(decision 不是 ShortReconnect) | login 的 decision;是否出现 `Cross-zone redirect only` |
| R3 断线 >30s → 回 A | PASS / FAIL | 实际等了多久 |
| R4 主动登出 → 回 A | PASS / FAIL | |
| R5 传送途中断线 → 回 A | PASS / FAIL | 断线时第一条腿是否已放行 |
| H1a 上限销毁(Redis 断 >70s)| PASS / FAIL / SKIP(SM 已先被摘掉,或 docker stop 晚了)| `handoff started` → `[MarkSentDestroy] site=travel_freeze_cap` 的实际间隔;客户端 `kicked by server reason=`;`[TravelHandoff]` 增量;重登后金币与 owner_epoch |
| H1b 上限销毁(Redis 半开)| PASS / FAIL / SKIP | 同 H1a;PAUSE 到期后 handoff 键是否已被迟到的 DEL 删掉 |
| H2 晚发闸 enter_scene 阶段 | PASS / FAIL / SKIP(没走快路径)| `frozen_ms`;是否出现 `requested EnterScene`(应无);`dispatch_window_closed` / `mark_sent_destroyed` 增量 |
| SE gate 转发补发放弃 | PASS / FAIL / 未命中(login 直接推 3023)| `attempts` / `elapsed_ms` / `last_failure`;客户端回选服的时刻(约 20s 还是 60s);顺带记 G7 走的是哪一种 |
| SE2 握手竞态补发 | 命中 / 未命中 | `forward recovered` 的 `last_failure`;新节点有无 `RpcSession not found for gate` |
| 全局不变量 1–9 | 全部成立 / 第 N 条不成立 | |
```

F3 的冻结上限一栏:v2.5 起 D-2 的修复已落码(未编译),按"冻结 ≤71s"判 PASS / FAIL;robot 的 60s 预算会先断线、由退出优先收尾,所以 F3 打不到上限本身,**真正打到上限的验证在场景 H**(Unity 客户端)。H 没做或 SKIP 时,不能只凭 F3 宣称 D-2 已验证。标记撤回一栏填 PASS 之前先确认真的测到了点上:日志里必须出现过 `[ZoneTravel][WithdrawMark] deferred`(没出现多半是 Redis 停得太晚,或 K-SM 没撑开窗口,撤回在 Redis 还通的时候就成功了——那只是重复了 C2)。

---

## 8. 失败时的 fallback

1. **不要 ship**,先定位 root cause。尤其是全局不变量 1 / 3 / 4 任一条不成立 = 数据一致性问题,优先级最高。
2. **先怀疑本文,再怀疑代码**:这条链从未实跑,本文的时序与期望是推出来的。现象与本文不符时,对着 §2.4 的行号回到源码确认"代码本来就是这么写的"还是"代码写错了"。是前者就改本文。
3. 抓现场(压测期间不上传任何日志,`AGENTS.md` §6.2):robot 全量输出;`run\logs\` 与 `bin\logs\cpp_nodes\` 整个目录;§2.1 的 redis 查询输出;四个指标端口的注入前后快照;`run\etc\go_services\z*_scene_manager.yaml`(证明口径)。
4. 确认是代码缺陷后:在 `docs/design/cross-zone-scene-travel.md` 的已知限制(§10.3 / §11.4)补一条,`PROGRESS.md` 追加一条,再开修复任务。
5. **不要用"把 reaper 补回来""把 `AllowUnsafeCrossNodeHandoff` 改回 true""手工 DEL location"来让场景变绿**——三者都是绕过被测机制。

---

## 9. 与帮会资产通道相关的归属信号(帮会二期 B4c)

来源:`docs/design/guild-phase2/08-save-owner-fence.md` §8.2.4、§8.3。本节只补充,不改上文各场景的判定。

**`stale_owner_write_rejected` 的第二种预期来源。** 上文(§2.3 / §2.4 表格、§5 第 1 条)把 `HandlePlayerSaveRejected … (metric=stale_owner_write_rejected)` 与
`db_stale_owner_write_rejected_total` 非 0 视为"曾经双主"。在**本 runbook 的故障注入场景里**这个判定不变;但在一般运行中还有一种合法来源:
**旧节点的退出存盘超过重连租约才落地,此时玩家已在别的节点铸出更高的 epoch,旧存盘被守卫拒掉** —— 这是帮会资产通道要防的 K1 被正确拦下,不是事故。
区分方法:看被拒的那个 scene 节点上,该玩家在被拒之前是否已进入退出链 —— 同一 player_id 的 `HandleExitGameNode: Player <P> is exiting the scene node` 早于被拒那一行 `HandlePlayerSaveRejected: owner_epoch CAS rejected save for player <P>`(退出链上没有含 `UnregisterPlayer` 字样的日志,别按它搜)。Z1 僵尸被拒后自清也归入这一类,判定相同。
是 → K1 被拦下,记录即可;否 → 按"曾经双主"处理。

**scene 侧只能靠日志的三条查询**(scene 的计数不进 Prometheus;Loki 的 ruler 目前没有挂载规则目录、也没有 `alertmanager_url`,
所以这三条是值班固定查询,不是自动告警;ruler 接线归上线批 BK8s):

```logql
# 资产操作因 owner_epoch = 0 被挡(资产通道在无围栏的实体上一律回 RETRY)
sum(count_over_time({service="scene"} |= "[AssetOp] blocked: owner_epoch unknown" [10m])) > 0

# 有 owner_epoch = 0 的存盘(兼容窗口仍在;共享 / 预发环境开启帮会资产操作时必须恒为 0)
sum(count_over_time({service="scene"} |= "[OwnerEpoch]" |~ "owner_epoch_unknown=[1-9]" [10m])) > 0

# 退出存盘超过重连租约才落地(epoch 非 0 时不再意味着覆盖,保留作辅助信号)
sum(count_over_time({service="scene"} |= "save outran reconnect lease" [10m])) > 0
```

Go 侧的两条(`DbStaleOwnerWriteRejected`、`DbOwnerEpochLegacyZero`)是可部署的 PrometheusRule,见 `deploy/k8s/owner-epoch-alerts.yaml`。

## Changelog

- **2026-09-29 v2.5**(**静态编写、未实跑**;所依据的代码 2026-09-28 已进 main —— 关联号 9da27f9a4 / 4e409c5c3 / 935ec83b1,冻结上限 / CPP-2 / GO-6 等 P3 在 09f71f9d5,infra 在 9da27f9a4 / 24dd3e6c5 / 6941e7344,gRPC 失败回调第二批 b85f13c07 —— **全部未编译、未测试**,待 Codex 验证。本版新增 / 改写的每条日志原文、计数名、常量值都在 2026-09-29 的工作树上逐条 grep 核对过,时序与期望现象仍是从代码推出来的)。
  - **冻结硬上限(D-2 的修复)**:§0 / §1 补上限与晚发闸;§2.3 `[TravelHandoff]` 字段清单补齐到当前实际输出(新增 `reply_uncorrelated` / `reply_unmatched` 与冻结上限的 8 个 key,逐个写含义与期望),出处改为"以 `player_lifecycle.cpp` 实际输出为准";§2.4 abort reason 列表删去 `no live gate session` / `no SceneManager node reachable`、加上 `handoff dispatch window closed before the mark was sent` / `handoff freeze cap reached before the handoff mark was sent`(另列 `handoff mark write result unknown` 与两条不该出现的防御分支);新增 v2.5 C++ 日志表(`[ZoneTravel][FreezeCap]`、`[ZoneTravel][DispatchWindow] stage=set_mark|enter_scene`、`[ZoneTravel][MarkSentDestroy] site=… kick=0|1`、`handoff watchdog fired … before its monotonic deadline`、`handoff not started … no SceneManager node reachable`、`forcing a real save before writing the handoff mark`),并注明 `[travel_freeze_cap] … (ownership moved away)` 是沿用的原文、这里实际表示"归属未知";§3.2 K-SM 的"立即解冻"改为"发起前被摘 → 同步拒绝不冻结 / SET 之后被摘 → `site=travel_no_scene_manager` 踢回选服",H 遇到记 SKIP;B1 补传输失败行与新的无效判据;F1 补快路径补写;F3 标题去掉 🔴、第 1 步改为"客户端不断线时 70–71s 出现 `site=travel_freeze_cap` + 踢线,robot 60s 预算先到仍是退出优先"、原"D-2 修复后应改成的通过标准"改为已修;新增**场景 H**(H1a Redis 断开超过 70s / H1b Redis 半开 / H2 晚发闸 enter_scene 阶段,用 Unity 客户端);§5 第 5 条终态等式加 `mark_sent_destroyed`、删去"F3(D-2)是已知例外"、出处更正为 `player_lifecycle.h` `travel_handoff_stats` 注释末尾,并加"冻结有界"判据;§6 D-2 标为修复已落码待验证,新增已接受残余 FC-1(墙钟回拨推迟上限)/ FC-2(同 zone 上限踢线可能多踢一次已改绑的会话)/ FC-3(SET 之后 SM 消失时会踢线),G2 / G10 同步;§7 F3 一栏改为 `PASS / FAIL(冻结 ≤71s)`,新增 H1a / H1b / H2 行,表后说明改写。
  - **EnterScene 应答关联号 + gRPC 失败回调**:§2.3 写明 `reply_uncorrelated`(升级完成后恒 0)/ `reply_unmatched`(基线接近 0、只看突增)的含义与期望;§2.4 新增关联号与传输失败日志表,说明交接相关行多出的 `corr=` / `correlated=`;§3.2 K-SM 里"生成的 gRPC 客户端对非 OK 状态只打日志、不回调"这一论据按 2026-09-28 的失败回调改写(见 `docs/design/grpc-client-deadline-failure-callback.md`;交接路径上传输失败仍不作为证据),历史推理保留;注明换图在途 TTL 已改为 SceneManager deadline + 1000ms,旧的 5s < 服务端 8s 的并行窗口已消除;§3.3 基线补 corr 一致性与新计数全 0。
  - **CPP-2(gate 侧进场转发补发)**:§2.3 新增 gate `[SceneEntry]` 30s 汇总行;§2.4 新增 C++ gate 日志表(`scene link ready` / `forward deferred` / `forward recovered` / `giving up` / 汇总);§3.3 基线补"每个 scene 节点一行 `scene link ready`、login-test 期间零 `giving up`(尤其不得有 `last_failure=handshake_pending`)";新增**场景 SE / SE2**(不用 G 命名,避免和缺口 G1–G10 混淆);G7 补"SE 命中与否即 G7 的答案"。
  - **GO-6 推迟摘除**:§2.3 新增 `scene_manager_node_detach_deferred_total{zone_id,outcome}` 与告警 `SceneManagerNodeDetachWithoutDeathMark` 的判读(该告警的 `runbook_url` 指向本文);§2.4 scene_manager 表里 `markNodeDeath` / `reentryBarrierBlocks` 改按函数名引用,新增 `[ReentryBarrier][DeferDetach]` 各行;新增**场景 E2**(`CONFIG SET` 压内存上限让 death_at 写不进,先改策略再压上限)。
  - **infra**:B3 第 3 步的 zrpc 超时说明改为 8s(归属查询 1.5s + Kafka 写 5s + 1.5s 余量),通过口径不变。
  - **本轮未落 / 落码中**:GO-2 根治(owner_epoch 严格单调 + 回滚回执 + 源端原子取证)与 CPP-3(疏散改派待确认表)**设计已定稿,落码中**,本版不含它们的场景,B3 加了"GO-2 落地后会改写"的提示,§5 第 5 条注明合并时保留 `mark_sent_destroyed`;GO-3 本轮不落(需用户拍板),§6 新增一行并在场景 E / E2 / SE 注明"备用节点在死节点租约到期前起";`handoff_pending_no_marker` 比值告警推迟到有压测基线。§3.1 第 2 步 `AllowUnsafeCrossNodeHandoff` 的行号改为按键名找。§9(帮会 B4c)及其引用的日志原文未改。
- **2026-09-21 v2.4**:Z1 修复、断线释放标记 A1′ / A2′、GO-5 重连落点、epoch==0 同节点铸造已落码(设计文档 §12.6.10 / §12.7 / §12.6.9;C++ 未重新编译、Go 未编译,均未测试)。§1 事实 2、§2.1 handoff 键的写入 / 删除方补 A1′ / A2′;§2.3 新增 `[ExitPersist]` / `[ExitRelease]` 两行汇总与 M12 的 `stale_marker` 口径;§2.4 新增 v2.4 的 C++ 日志表(`[ExitRelease]`、`[ExitRelease][InheritClear]`、`Player marked for unregistration` 改到收敛时才打等)与 login `ZoneId=0` 日志;新增**场景 Z**(Z1 复现与修复验证)与**场景 R**(GO-5 重连落点五种情形);§5 新增不变量 8;§7 表同步。因 A′ / GO-5 改期望:§3.3(第二条腿载入后 A2′ 清掉旧标记;robot 退出后出现 A1′ 标记)、§3.4(前置接受 A1′ 标记;残留等待改为 30s)、B2 与 D2(重登落在 home zone,不再按等待落点送往目标 zone —— R8 已被取代)、C1(常见形态退出后键 = A1′ 标记)、C2 / F3(补 `skip_handoff_inflight`,键仍不应出现)。§9 未改。同日追加:场景 R2 的前提改为已静态核实(会话在 SharedRedis,D12;各 zone 的 player_locator 必须连同一个 Redis),并注明客户端第二条腿不应再弹选角(`GameClient.ResolveRedirectPlayerId`);`skip_identity_conflict` 的触发条件随 M3 探针收紧而变严(lease 未授予 / 重注册中 / ACK 超过 TTL/2 都算),验证见设计文档 §12.6.10 清单 11b。
- **2026-09-21 v2.3**:新增 §9(帮会二期 B4c):`stale_owner_write_rejected` 的第二种预期来源(K1 被正确拦下)与区分方法;scene 侧三条值班 LogQL;指向 `deploy/k8s/owner-epoch-alerts.yaml`。上文各场景的判定不变。
- **2026-09-21 v2.2**:S3L1-1 第二层出口 + 铸造重放识别落码(设计文档 §12.5.6,未编译未验证)。§2.3 `[TravelHandoff]` 行按 `player_lifecycle.cpp:238-252` 实际输出重写(补 `withdraw_deferred` / `withdraw_expired` / `granted_client_reset`),新增指标 `scene_manager_enter_scene_rollback_total{outcome}`;§2.4 新增 `[ZoneTravel][ClientReset]` 两条、`GET owner_epoch failed` 改为 `MGET owner_epoch/location failed or malformed`、`was granted although …` 带 `evidence=`;scene_manager 回滚日志改为 `[RouteRollback] outcome=` 前缀,**旧原文 grep 口径作废**;robot 补 `kicked by server` 口径;§1 机制补踢线分支;B2 期望改成"踢线 / 退出优先"二选一;新增 **B3**(Kafka pause + 回滚注入 Redis 错误);§5 不变量 6 与 §7 结果表同步。
- **2026-09-20 v2.1**:缺陷 D-1(Redis 断开时 handoff 标记撤不回)的修复已落码(未编译未验证):C2 / F3 / §6 / §7 按 `WithdrawHandoffMark` 的日志与计数改写,F3 拆成「标记撤回:可判 PASS/FAIL」与「冻结上限:仍 KNOWN-FAIL(D-2)」;原缺口 G3 关闭;`enter_scene_rejected_total` 的 reason 补 `home_zone_unmapped_travel` / `scene_gone`。前置新增:测试号必须有 `player:zone` 映射(新建角色自带;存量号先跑 `merge_zone -backfill-home-zone`),否则 TravelToZone 第一条腿直接回 20。
- **2026-09-20 v2**:整篇重写。v1 的 A–D 四个场景(Kafka 暂不可达 / 目的节点持续不可达 / 源节点重启 / Kafka 重复投递)全部针对已下线的 `player_migrate` + ACK + `CrossZoneReaper` 链,作废。新场景 A–F 面向 handoff 标记 + `owner_epoch` 两道门、两道 30s 看门狗、"退出优先"与属主接管。依据 `main`@`d9e471b80` 静态编写,**链路未编译、未实跑,本文未经执行验证**。F3 标注为当前预期失败(D-1 退出路径 `DEL` 被 `connected()` 守卫;D-2 冻结无服务端上限)。移除对已不存在的 `robot/clients/cmd/multi_zone_seeder`、`single_player` 的引用。
- **2026-05-19 v1**:初版,覆盖跨 zone 三件套的 4 个失败场景。被测对象已于 2026-09-18 阶段 3 删除。
