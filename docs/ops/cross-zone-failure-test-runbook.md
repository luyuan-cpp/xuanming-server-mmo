# 跨 Zone 传送 / 归属交接 失败场景测试 Runbook

> **状态**: v2 — 2026-09-20(整篇重写,v1 的四个场景全部作废,见 §0);v2.2 — 2026-09-21 补 S3L1-1 第二层出口的观测点与场景 B3(见 Changelog);v2.4 — 2026-09-21 补 Z1 修复 / 断线释放标记 A′ / GO-5 的观测点与场景 Z、R,并按 A′ 改写 §3.3 / §3.4 / B2 / C1 / C2 / D2 / F3 的期望(代码已落码,C++ 未重新编译、Go 未编译,均未测试;设计文档 §12.6.10 / §12.7)
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
- 现在「冻结了谁来解」的答案:EnterScene 应答 + 两道 30s 看门狗(存盘看门狗 / 应答看门狗)+ 按 `owner_epoch` 核实去留(`ResolveTravelOutcome`)+「退出优先」。不需要 reaper。
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

**db**(zone1 `:9160`,zone2 `:10160`):

| 指标 | 期望 | 出处 |
|---|---|---|
| `db_stale_owner_write_rejected_total`(无 label)| **所有场景恒 0**(不变量 §6.3)| `go/db/internal/metrics/metrics.go:93-97` |
| `db_owner_epoch_guard_total{outcome}`,`outcome` ∈ `match / legacy_zero / first / advance / stale / read_error` | 每次真实换主后新主第一笔写记一次 `advance`;`stale` 恒 0;全链升级后 `legacy_zero` 归零 | `metrics.go:84-88`、`:121-128` |

**C++ scene 没有 Prometheus 指标**,只有两行 30s 汇总日志(缺口 G1):

- `[TravelHandoff] started=… started_cross_zone=… granted=… granted_without_reply=… resolved_in_place=… aborted=… exit_wins=… save_watchdog_fired=… reply_watchdog_fired=… verify_rearmed=… frozen_ms_total=… frozen_ms_max=… withdraw_deferred=… withdraw_expired=… granted_client_reset=…` —— INFO,**累计值**,**有变化才打**,进程内第一次发生交接后才开始打。字段顺序以 `player_lifecycle.cpp:238-252` 的实际输出为准(2026-09-21 按它重写;此前本行漏了 `withdraw_deferred` / `withdraw_expired`),各字段含义见 `player_lifecycle.h` 的 `travel_handoff_stats` 注释。`granted_client_reset` 是 `granted_without_reply` 的子集:源端销毁实体之前给客户端发了 tip 3027 + 踢线 34 的次数,应接近 0(gate-cmd 消费滞后 >30s 时,迟到但成功的传送也会被计入并踢回选服)。
- `[OwnerEpoch] stale_owner_write_rejected=… home_zone_unknown=… owner_epoch_unknown=…` —— WARN,**任一非 0 才打**(`cpp/libs/services/scene/core/system/redis.cpp:75-81`)。所以"恒 0"的证据是**这行日志从头到尾没出现**。
- **(v2.4)`[ExitPersist] exit_superseded=… exit_intent_missing=… exit_resave=… exit_resave_capped=… exit_exhausted_rekick=… exit_fastpath_deferred=… exit_client_msg_rejected=… exit_deposed_on_reentry=…`** —— INFO,累计值,30s、有变化才打,进程内第一次有玩家退出后开始(`player_lifecycle.cpp:333-340`,定时器由 `EnsureExitStatsTimer` 自挂,**不在** `redis.cpp`)。期望:`exit_superseded` / `exit_intent_missing` / `exit_resave_capped` 恒 0;`exit_resave` 偶发(退出存盘在途时实体又变了);`exit_client_msg_rejected` 在断线瞬间客户端在途包多时会涨,正常;`exit_deposed_on_reentry` 非 0 = 有旧实体被判废黜后丢弃重载(退出被保留到租约之后 / 活僵尸),记录 player_id。
- **(v2.4)`[ExitRelease] attempted=… skip_disabled=… skip_entity_invalid=… skip_intent_missing=… skip_cause=… skip_suppressed_release=… skip_suppressed_identity=… skip_suppressed_unspecified=… skip_relocate=… skip_handoff_inflight=… skip_epoch_unknown=… skip_identity_conflict=… written=… epoch_moved=… failed=… inherit_absent=… inherit_deleted_older=… inherit_deleted_exact=… inherit_kept_newer=… inherit_deleted_malformed=… inherit_epoch_mismatch=… inherit_reply_error=… inherit_reply_lost=… inherit_failed=… inherit_refused=… inherit_rewritten=… inherit_rewrite_skipped=… inherit_rewrite_failed=… inherit_rewrite_dropped_node_holds=… inherit_rewrite_handed_over=…`** —— INFO,同上(`player_lifecycle.cpp:274-303`)。`attempted` = 判定为"写"的次数,写成功看 `written`。期望:干净断线 `attempted`≈`written`,`failed` / `inherit_epoch_mismatch` / `inherit_refused` 稳态恒 0;ReleasePlayer 引起的退出计 `skip_cause`(正常);`skip_epoch_unknown` 非 0 = 还有 owner_epoch 为 0 的存量玩家。**M12 口径**:v2.4 起 `enter_scene_rejected_total{reason="handoff_pending_stale_marker"}` 会混入"上一任的释放标记还在、而新铸落点没载入"的登录重试,排查先对照本行的 `written` / `inherit_*` 与 CPP-2 症状,压测复盘把两类分开统计。

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
| `[ZoneTravel] handoff aborted for player <P>: <reason>; unfreezing and keeping player on this node` | 交接未成,解冻 + 回失败 tip。`<reason>` 取值:`handoff save did not land in time` / `owner_epoch unknown (0); route chain not upgraded` / `zone redis not connected` / `handoff mark write failed` / `redis command dispatch failed` / `no live gate session` / `no SceneManager node reachable` / `scene_manager rejected` / `EnterScene reply timed out` / `reply without redirect` | :2151;reason 出处 :1649 :1764 :1772 :1791 :1828 :1849 :1856 :2102 :1917 :2119 |
| `[ZoneTravel] handoff resolved in place for player <P>: …; ownership unchanged, unfreezing silently` | 同 zone 重发后落回本节点,不算失败 | :2156 |
| `[ZoneTravel] cannot verify travel outcome for player <P> (<reason>): zone redis unavailable; keeping the player frozen and re-arming the watchdog` | Redis 不通,判不清去留 → **保持冻结**并重挂 30s 看门狗 | :1944 |
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
| `[ReentryBarrier] 记录节点死亡时刻: zone=… node=… barrier=…` / `[ReentryBarrier] <site>: 老属主刚判死,再入屏障还差 …` | 判死打点 / 屏障内拒绝 | `reentry_barrier.go:92 / :172` |

**db**:`STALE-OWNER-WRITE rejected: key=<P> … taskEpoch=… appliedEpoch=…`(**不该出现**,`key_ordered_consumer.go:629`);`owner-epoch advance: key=<P> … taskEpoch=<E'> appliedEpoch=<E>`(每次真实换主一条,正常,`:645`)。

**login**:EnterScene 被拒时返回错误 `scene_manager rejected EnterScene for player <P>: code=<N> msg=…`(`go/login/internal/logic/clientplayerlogin/entergamelogic.go:577`;它具体落在哪一行日志**未核对**)。**(v2.4,GO-5)**重连 / 顶号不指定去向:`[travel] EnterGame player=<P> decision=<d> 不指定去向(ZoneId=0),由 scene_manager 按 location 决定 gate_zone=<z>`(`entergamelogic.go:665`);持票据的第二条腿:`[travel] EnterGame player=<P> 持重定向票据落地 zone=<z>,跳过 home_zone 弹回`(`:660`)。

**robot**(`robot/travel_smoke_scenario.go`):`[travel-smoke] at home`(:246,带 `player_id`)→ `[travel-smoke] travel out`(:279,紧接着发 TravelToZone)→ `[travel-smoke] arrived at visit zone`(:289)→ `[travel-smoke] travel home`(:343)→ `TRAVEL_SMOKE_OK player_id=…`(:376)或 `TRAVEL_SMOKE_FAIL step=<step> reason=<…>`(:203)。受理后失败的 reason 形如 `受理后失败 SendTipToClient tip=<N>`(:762);跟随 124 失败另有一行 `follow gate redirect failed`(`robot/logic/handler/scene_client_player_common_redirect_to_gate.go:64`)。收到踢线 34 时 robot 打一行 Warn `kicked by server`,`reason` 字段是 `TipInfoMessage` 的文本形式(含 `3027` 即命中,`robot/logic/handler/handlers.go:42-47`),robot 自己不断线;传送窗口内若 34 先于 tip 被扫到,失败文案是 `传送途中收到 KickPlayer(目标 zone 的顶号 / 旧会话清理踢到了新会话?)`(:767)。**注意**::762 的文案写着"scene 已解冻",对 §12.5.6 的踢线路径不成立(那里实体已销毁)。

**tip 码**(只按名字引用;数字取自生成物 `generated/code/proto/tip/scene_error_tip.proto`,仅供对照 robot 打印的运行时值):`kEnterSceneSceneNotFound=3007`、`kEnterSceneChangingScene=3014`、`kEnterSceneFailed=3023`、`kZoneTravelTargetZoneNotFound=3024`、`kZoneTravelInBattle=3025`、`kZoneTravelInTeam=3026`、`kZoneTravelTargetBusy=3027`;`kSceneTransferInProgress=13000`(`cross_server_error_tip.proto`)。跨 zone 传送受理后未成**一律**回 `kZoneTravelTargetBusy`(`AbortTravelHandoff`);受理后未成且无法原地恢复时同一个码既作 tip、也作踢线 34 的 `reason.id`(`SendTipAndKickToClient`,`player_lifecycle.cpp:200`)。

---

## 3. 前置准备

### 3.1 环境

1. **二进制必须是当前 main 编出来的**:gate / scene / login / scene_manager / db / robot。编译与 regen 清单见设计文档 §9 / §11.4,不在本文范围。`robot.exe` 需先在 `robot/` 下编出。
2. **切到生产口径**:`go/scene_manager/etc/scene_manager_service.yaml:42` 本地默认是 `AllowUnsafeCrossNodeHandoff: true`(dev 旁路)。跑本 runbook 前**临时**改成 `false` 再启动服务(派生 yaml 是启动时从它拷出来的);测完还原,**不要提交**。旁路开着时场景 A / E 完全失去意义(无标记也放行)。核对方法:scene_manager 日志里**不出现** `[Handoff] dev 旁路`。
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
| **K-SM** | `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-stop -GoServices scene_manager` | **硬杀所有 zone 的** scene_manager(`Stop-Process -Force`,`go_services.ps1:759`;按名字过滤,不分 zone)。其 etcd 租约 60s(`scene_manager_service.yaml:17`)内,scene 仍会把 EnterScene 发给它;生成的 gRPC 客户端 status 非 OK 时**不回调**应答处理器(`player_lifecycle.cpp:137-139`)→ 撑开一个 **30s 的"标记已写、没有应答"窗口** | 中。若 scene 在 SM 进程死后立刻把它从注册表摘掉,现象会变成 `handoff aborted … no SceneManager node reachable`(立即解冻),窗口打不开 |
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

### 3.4 每个场景前后

- **前**:记下 `$P`;确认 `EXISTS player:{P}:handoff` 为 0、或其 epoch 已落后于当前 `owner_epoch`、或它是上一轮退出写下的断线释放标记(epoch == 当前值,且上一轮日志里有对应的 `[ExitRelease] mark written … mark=<同一原文>`;v2.4 起这是常态,本轮登录的 A2′ 会清掉它)。**判"标记已撤回"的步骤都在玩家仍在线时做**;玩家退出之后键重新出现是 A1′,不是撤回失败;拉一次指标快照(`curl -s http://127.0.0.1:9150/metrics`、`:10150`、`:9160`、`:10160` 存成文件);记下各 scene 进程最后一行 `[TravelHandoff]`(没有就当全 0)。
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
- **期望**(scene z1):`handoff started` → `Saving complete` → `requested EnterScene` → **约 30s 无任何应答,玩家处于冻结态** → `[ZoneTravel] handoff aborted for player <P>: EnterScene reply timed out; unfreezing and keeping player on this node`。`[TravelHandoff]` 增量:`started+1 started_cross_zone+1 reply_watchdog_fired+1 aborted+1`,`frozen_ms_max` ≈ 30000。客户端收到 `kZoneTravelTargetBusy`;robot:`TRAVEL_SMOKE_FAIL step=travel-out reason=…受理后失败 SendTipToClient tip=3027…`。
- **Redis**:看门狗到期前 `GET player:{P}:handoff` = `"E:<ms>"`;到期后**不存在**(`ResolveTravelOutcome` 先 `DEL` 再 `GET`,`:1955`);`owner_epoch` 仍是 E。
- **通过标准**:✅ 冻结时长 ≤ 30s + 一个调度抖动;✅ 解冻后标记已撤回;✅ epoch 未变;✅ 恢复 SM 后基线重跑 OK,金币 = 上一轮出发值(没回档,也没重复加)。
- 若现象是立刻 `handoff aborted … no SceneManager node reachable`:同样是 fail-closed 的正确行为(通过),但**没测到"应答丢失"**,记为 K-SM 对本环境无效,改用 KF-PAUSE 撑窗口再试。

**B2 —— 应答丢失,但其实已经放行**(仓库内**没有可靠的注入手段**,缺口 G4)

需要让 scene_manager 走完铸造 Lua 之后、回应答之前死掉。唯一能手工碰的办法:KF-PAUSE 让它卡在 Kafka 发送上(已铸造、已写「等待落点」),这几秒内 K-SM,再 `docker unpause kafka`。成功率低,**首跑能做就做,做不出来记 SKIP**,不要为它改代码加后门。

- **期望**(scene z1,2026-09-21 起二选一,取决于 124 有没有真的发出去):
  - **124 没发出去**(SM 在写 Kafka 之前死掉——这是本手法的常见结果):30s 后 `[ZoneTravel] travel for player <P> was granted although the reply was lost/failed (EnterScene reply timed out, evidence=no_reply): owner_epoch E -> E+1; destroying source-side entity` → `[ZoneTravel][ClientReset] player <P> … location awaits placement in zone 2; sending tip 3027 + KickPlayer …` → `[travel_granted_without_reply] … destroyed without persisting (ownership moved away)`;`[TravelHandoff]`:`granted+1 granted_without_reply+1 reply_watchdog_fired+1 granted_client_reset+1`。robot 日志有 `kicked by server`(reason 含 3027),FAIL 文案是 B3 所列两种之一。
  - **124 其实已送达**:客户端跟随 124 离开,旧连接一断 gate(A) 立即发 ExitGame,实体走"退出优先"销毁(`exit_wins+1`),看门狗随后 no-op,**不应**出现 `[ClientReset]`。
  - 出现 `[ZoneTravel][ClientReset] not resetting client …` 时照录 `<why>`;`location is not this handoff's awaiting placement` 在本场景不应出现。
- **已知现象(不是 bug)**:这 30s 里源实体以冻结态留在源场景的 AOI 内,周围玩家看到一个不动的分身(`scene_node_service.cpp:151-160`,设计文档 §11.2 复审 P2)。
- **通过标准**:✅ 源实体**不存盘**销毁(此后无 `[SavePlayerToRedis] Player <P> saved` 出自源节点);✅ 无 `stale_owner_write_rejected`;✅ 玩家从 zone 1 重登后**落在 zone 1**(等待落点不牵引,设计文档 §12.7;owner_epoch 预期 E+2,`[ExitRelease][InheritClear] … result=inherit_deleted_older`),金币 = 出发值。*v2.4 改期望:此前写的是"按等待落点被送到目标 zone(`Cross-zone redirect only`)",那依赖 R8,已被 GO-5 取代;出现 `Cross-zone redirect only` = scene_manager 或 login 还是旧二进制。*

**B3 —— Kafka 推重定向失败 + 回滚也失败(S3L1-1,第二层出口)**(2026-09-21 新增;对应设计文档 §12.5.6,**代码未编译,本场景未实跑**)

需要让第一条腿已铸造、Kafka 推送失败,而随后的回滚 EVAL 拿到 Redis 错误。手法:Kafka pause 撑住推送,epoch 一变就用 ACL 拒掉 EVAL。go-redis 对 `NOPERM` / `ERR` 不重试,回滚一次即判 `redis_error`;源端的 `DEL` 与 `MGET` 不是 EVAL,不受影响。

- **前置**:生产口径(§3.1 第 2 步);基线 travel-smoke 能 OK;记下 `$P` 与当前 epoch `$E`(`GET player:${P}:owner_epoch`)。本地 Redis 若不是 `default` 用户,下面的 ACL 命令换成实际用户名。
- **注入**:
  1. `'\[travel-smoke\] at home' = { docker pause kafka }`(KF-PAUSE)。
  2. 每 100ms 查一次 `GET player:${P}:owner_epoch`,一旦变成 `$E+1`,立即 `docker exec redis redis-cli ACL SETUSER default -eval -evalsha`。
  3. 保持到 zone 1 的 scene 打出 `[ZoneTravel][ClientReset]`(zrpc 本地超时 5s 与 Kafka 写超时 5s 相当,应答可能以 7 当场到达,也可能丢失、约 30s 后由看门狗裁决),然后 `ACL SETUSER default +eval +evalsha`,再 `docker unpause kafka`。
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
- **注入**:记 T0,K-SCENE 杀 `z1_scene`;随后 `cpp-node-start -CppNodes scene -SceneCount 2 -Zone 1` 补备用节点;停掉 robot 再重启它(= 玩家重登),每 30s 左右试一次。
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

### 场景 F:zone Redis 在交接窗口内断开

**F1 —— 交接存盘落地之前 Redis 就断了**(可稳定注入)

- **注入**:`'at home' = { docker stop redis }`。观察到 abort 之后 `docker start redis`。
- **期望**(scene z1):`handoff started` →(约 30s)→ `[ZoneTravel] handoff aborted for player <P>: handoff save did not land in time; …`;`[TravelHandoff]`:`save_watchdog_fired+1 aborted+1`。若那次存盘恰好走了快路径(盘上已是同一份),则是**立即** `handoff aborted … zone redis not connected`(`:1769-1773`)。期间可能出现 `HandlePlayerAsyncSaveFailed: DATA-DURABILITY RISK` ERROR —— Redis 停着时的预期噪声。robot:`TRAVEL_SMOKE_FAIL step=travel-out … tip=3027`。
- **通过标准**:✅ 冻结 ≤ 30s;✅ Redis 恢复后无 `player:{P}:handoff`、`owner_epoch` 未变;✅ 排队的存盘在 Redis 恢复后落地(`Saving complete for player: <P>`),下一轮基线的登录余额 = 本轮出发值(+11 没丢)。

**F2 —— 标记已写之后 Redis 短暂断开,在应答看门狗到期前恢复**

- **注入**:`'at home' = { K-SM }`;`'travel out' = { Start-Sleep 3; docker stop redis; Start-Sleep 12; docker start redis }`。
- **期望**:T3+30s 看门狗到期时 Redis 已连上 → `handoff aborted … EnterScene reply timed out`,标记被 `DEL`。若到期时 hiredis **还没重连上**(重连节奏拿不准),会先看到 F3 的 `cannot verify travel outcome …`,此时 robot 的 60s 预算会先到、断线退出,场景自动滑进 F3。
- **通过标准**:同 B1,外加 ✅ 无 `SET handoff mark result unknown`(出现它说明 Redis 是在 `SET` 回包前断的——也合法,走的是 `ResolveTravelOutcome`,记下来)。

**F3 —— 标记已写、Redis 持续断开(含玩家退出)—— 🟡 标记撤回(D-1)修复已落码待验证 / 🔴 冻结无上限(D-2)仍待修**

- **注入**:`'at home' = { K-SM }`;`'travel out' = { Start-Sleep 3; docker exec redis redis-cli GET "player:<P>:handoff"; docker stop redis }`。**不要**手工杀 robot:它 60s 超时后自己会断线,正好构成"退出"。约 90s 后 `docker start redis` 并立刻查键。
- **当前代码下的预期现象**(第 2、3 步按 2026-09-20 落码的 D-1 修复改写;该修复**未编译、未实跑**,首次执行即是对它的验证):
  1. T3+30s:`[ZoneTravel] cannot verify travel outcome for player <P> (EnterScene reply timed out): zone redis unavailable; keeping the player frozen and re-arming the watchdog`;`[TravelHandoff]`:`reply_watchdog_fired+1 verify_rearmed+1`。此后**每 30s 重复一次,没有次数上限、没有时长上限**(`:1941-1948`)——只要 Redis 不回来、玩家不断线,他就一直冻着。
  2. T3+60s:robot 超时断线 → `[SavePlayerToRedis] skip: …` → `FinishExitAfterPersist: … (exit wins) handoff_mark_written=1` → `[ExitRelease] not writing release mark … reason=skip_handoff_inflight`(v2.4,M11)→ 实体销毁。撤回当场发不出去:`[ZoneTravel][WithdrawMark] deferred player=<P> mark=<E>:<ms> site=exit wins reason=redis_unavailable pending=1; scene change stays refused …`(ERROR,仅首发一条),此后每 5s 重试一次、每轮一条 WARN `retried 1 unconfirmed withdrawal(s) site=retry pending=1`;`[TravelHandoff]`:`withdraw_deferred` 持续上涨。
  3. Redis 恢复后(重连回调立即强制重发,`site=reconnect`):出现 `[ZoneTravel][WithdrawMark] withdrawn player=<P> mark=<E>:<ms>`;数秒内 `EXISTS player:{P}:handoff` = **0**;`GET player:{P}:owner_epoch` = E;`withdraw_expired` 增量为 0。**若仍读到 `"E:<ms>"` 且 TTL > 0 = D-1 的修复没生效**,按 §8 保留证据。Redis 停机超过约 305s 才恢复时,改为看到 `gave up on 1 mark(s): deadline (mark TTL) passed` 与 `withdraw_expired+1`——此刻 Redis 侧标记也已过期,同样合法。
- **D-1 为什么是缺陷(背景)**:残留期内玩家重连回本节点(同落点不铸造,epoch 仍是 E)继续产生新状态,之后任意一次跨节点 / 跨 zone EnterScene 都会凭这份旧标记**免存盘过换手门** → 目标节点读到旧档(回档),且 `owner_epoch` CAS 不会响(epoch 没变过),零报错。与 09-18 复审修掉的 P1 是同一个洞,只是入口换成了"Redis 不通"。`AbortTravelHandoff` 里的同款 `DEL`(`:2171-2175`)有同样的守卫;`BeginTravelHandoff` 的空 reply 分支已经为此改走 `ResolveTravelOutcome`;退出路径与 Abort 路径由 2026-09-20 的 `WithdrawHandoffMark` 补上。
- **通过标准(只针对 D-1)**:✅ 第 3 步的标记被撤回(或已随 TTL 过期并计入 `withdraw_expired`);✅ 撤回未确认期间让该玩家重登 zone 1 并发起换图,scene 日志出现 `[ZoneTravel][WithdrawMark] scene change refused for player <P>`、客户端收到"切换中"类 tip,而不是免存盘过门;✅ 撤回确认后换图恢复正常。**D-2 部分不设通过标准**,如实记录冻结持续了多久、`verify_rearmed` 涨到几。
- **残余(设计文档 §12.3「标记残留」)**:这道闸只管本节点替在线玩家发的请求;断线重登被 scene_manager 挑到**别的节点**时不经过它,撤回确认前仍只有 TTL 兜底。源实体已销毁、盘上即最终态,不回档,但值得在记录里注明落到了哪个节点。
- **D-2 修复后应改成的通过标准**(给修的人;设计稿要点见设计文档 §12.3「冻结上限」):冻结必须有服务端上限(或至少有可告警的"当前冻结中玩家数 / 最长冻结时长"观测)。修完把本节的 🔴 去掉,并同步 §6。

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

---

## 5. 全局不变量(每个场景都要核)

1. **无双主**:所有 scene 进程日志里**没有** `HandlePlayerSaveRejected: owner_epoch CAS rejected save`,**没有** `[OwnerEpoch]` 行;两个 zone 的 db `db_stale_owner_write_rejected_total` 增量为 0,无 `STALE-OWNER-WRITE rejected`。唯一例外见场景 E 末尾的僵尸情形。
2. **任一时刻只有一个持有者**:按时间排 `HandlePlayerAsyncLoaded: Loading player <P>` 与 `Destroying player: <P>` / 进程死亡时刻,新持有者的载入必须晚于旧持有者的销毁(或死亡 + 屏障)。
3. **不落错库**:访客区 scene 的 `[SavePlayerToRedis] … (topic=…)` 永远是 `db_task_zone_<home_zone>`;`scene_manager_home_zone_lookup_total{outcome="error"}` 与 `{outcome="unconfigured"}` 增量为 0。
4. **不回档、不串档**:金币永远 ∈ 本轮已知的合法值集合,**绝不为 0**(0 = 目标区读不到档、把人当新号建了空实体,它一存盘就覆盖原档——travel-smoke 专门为抓这个设计了出发值,`travel_smoke_scenario.go:91-99`)。
5. **没有永久冻结**:静置后,存活进程的 `[TravelHandoff]` 满足 `started == granted + resolved_in_place + aborted + exit_wins`(`player_lifecycle.h:108-109`);无 `orphan PlayerFrozenComp`。F3(D-2)是已知例外。
6. **拒绝不改状态**:凡 scene_manager 回 1 / 8 / 18 / 19 / 20 的请求,`owner_epoch` 与 location 前后一致;回 7 的请求要么已回滚(`rolling back location/epoch` 之后 `rollback_total{outcome="rolled_back"|"already_rolled_back"}` +1),要么有 `[RouteRollback] outcome=superseded|redis_error` 可查;`redis_error` 且跨 zone 时源 scene 必须有对应的 `[ZoneTravel][ClientReset]`(或写明为何不踢的 `not resetting client`)。
7. **不是 dev 旁路**:scene_manager 日志无 `[Handoff] dev 旁路`;无 14 号错误码。
8. **(v2.4)退出不留僵尸**:每一条 `HandleExitGameNode: Player <P> is exiting …`(原因不是交接 / 改派的)最终都跟着 `Destroying player: <P>`;全程无 `ignoring stale UnregisterPlayer`、无 `superseded by a newer session`、无 `exit_resave_capped`;`[ExitRelease]` 行的 `failed` / `inherit_refused` / `inherit_epoch_mismatch` 增量为 0(场景 Z / R 的故障注入步骤除外)。

---

## 6. 已知缺口与「当前预期失败」汇总

**待修缺陷(不计入通过,但必须记录现象)**

| # | 缺陷 | 位置 | 对应场景 |
|---|---|---|---|
| D-1(**修复已落码 2026-09-20,未编译未验证**;F3 首次执行即是验证) | 退出路径撤回 handoff 标记的 `DEL` 被 `redis->connected()` 守着,Redis 断开时静默跳过,标记对当前 epoch 有效地残活 ≤300s → 后续跨节点落点可免存盘过门(回档,零报错)。`AbortTravelHandoff` 的 `DEL` 同款 | `player_lifecycle.cpp:868-875`、`:2171-2175` | C2 × F → **F3** |
| D-2 | 冻结没有服务端上限:Redis 不可用时 `ResolveTravelOutcome` 无限重挂 30s 看门狗,玩家只能靠断线("退出优先")脱身 | `player_lifecycle.cpp:1941-1948`、`:1969-1976`、`:2009-2015` | **F3** |

**观测 / 注入缺口(想写进通过标准,但代码里没有对应观测点)**

| # | 缺口 |
|---|---|
| G1 | C++ 的交接 / 归属计数没进 Prometheus,只有 `[TravelHandoff]`(有变化才打)与 `[OwnerEpoch]`(非 0 才打)两行 30s 日志;`stress_summarize.ps1` 也不解析(`player_lifecycle.cpp:176-178` 自述"目前还没有解析方",设计文档 §10.3)。进程被硬杀时最后 <30s 的计数直接丢失,场景 A / E 里看不到被杀节点的最终计数 |
| G2 | 没有"当前冻结中玩家数 / 单个玩家已冻结多久"的观测;`frozen_ms_*` 只在走到终态时累计,一直冻着的玩家只体现为 `verify_rearmed` 缓慢上涨 |
| G3 | ~~撤回标记的 `DEL` 是空回调,无日志无计数~~ **已随 D-1 的修复补上**(`[ZoneTravel][WithdrawMark]` 日志 + `withdraw_deferred` / `withdraw_expired`),仍未进 Prometheus(同 G1) |
| G4 | 没有故障注入钩子:"存盘落地 → 写标记"同栈连续执行;"铸造 Lua 之后、应答之前"只有毫秒;`handoff_pending_withdrawn`(预检与铸造之间标记被撤回)、`epoch_conflict`(19)同样无法手工触发。场景 A 只能测等价类,B2 靠碰运气,19 / withdrawn 只有单测覆盖(`owner_epoch_test.go`,**均未运行**)|
| G5 | `player:{P}:location` 是 proto 二进制,scene_manager 没有查询位置的 debug 端点(`/debug` 下只有 `rebalance-plan`,`world_rebalance_debug.go:43`)。location 的 zone / node / epoch / `pending_scene_conf_id` 只能靠日志反推 |
| G6 | scene_manager 只对**拒绝**计数,「凭标记放行」没有指标,只能数日志 `Cross-zone handoff released` 或 db 的 `owner_epoch_guard_total{outcome="advance"}` |
| G7 | 节点已死但 etcd 租约未到期的窗口(≤180s):设计文档 §11.5 写的是"请求照旧被 18 暂拒",但从代码看,若解析出的场景仍映射在死节点上,会命中 `samePlacement`(`enterscenelogic.go:434-491`)→ 回 0 并把路由发给已死进程,客户端只是超时,**没有任何拒绝指标**。两种都可能,拿不准,首跑记录 |
| G8 | travel-smoke 是一次跑到底的冒烟,没有"停在交接窗口 / 在窗口内退出"的开关;场景 B / C / F 的时序全靠外部撑窗口 + 杀进程 |
| G9 | scene 选 scene_manager 不按 zone 过滤(`node_utils.cpp:15-35`),双 zone 下第一条腿落在哪个 SM 上不确定,日志与指标要两边找;K-SM 因此只能"全杀" |
| G10 | 客户端侧(Unity `DevAutoPilot -travelZone / -travelScene / -quitOnTravelEnd`,设计文档 §11.1)本文**未核对**(服务端任务不读客户端仓)。它的传送等待上限若长于 robot 的 60s,可以用来观测 F3 第 1 步里更长的冻结 |

---

## 7. 结果记录

记录到 `docs/ops/cross-zone-test-results-<date>.md`:

```markdown
# Cross-Zone Handoff Failure Test Results — YYYY-MM-DD

Operator: <name>      Build: <commit-sha>      Env: local dual-zone / k8s
AllowUnsafeCrossNodeHandoff: false(已核对日志无 "[Handoff] dev 旁路")
Runbook 版本: v2(首跑 = 校准轮;与实际不符处已回改 runbook:是 / 否,改了哪些)

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
| E 单节点硬崩接管 | PASS / FAIL | takeover_total 增量 vs 玩家数;锁定总时长 |
| F1 存盘前 Redis 断 | PASS / FAIL | |
| F2 窗口内 Redis 闪断 | PASS / FAIL / 滑进 F3 | hiredis 重连耗时 |
| F3 Redis 持续断 + 退出 | 标记撤回(D-1):PASS / FAIL;冻结上限:**KNOWN-FAIL(D-2)** | 冻结时长、verify_rearmed、withdraw_deferred / withdraw_expired、恢复后标记是否还在及剩余 TTL |
| Z 修复前复现(Z-pre) | 复现 / 未复现 | 是否确认没走快路径;`ignoring stale UnregisterPlayer` 那行的会话号 |
| Z 修复后验证(Z-post) | PASS / FAIL | `Destroying` 与 `[ExitRelease] mark written` 的先后;handoff 键原文与 TTL;重登的 `inherit_*` 结果 |
| R1 同 zone 多节点快速重登 | PASS / FAIL | 落到哪个节点;epoch;是否先回过一次 18 |
| R2 访客窗口内从 A 重登 → 回 B | PASS / FAIL / 拿不准(decision 不是 ShortReconnect) | login 的 decision;是否出现 `Cross-zone redirect only` |
| R3 断线 >30s → 回 A | PASS / FAIL | 实际等了多久 |
| R4 主动登出 → 回 A | PASS / FAIL | |
| R5 传送途中断线 → 回 A | PASS / FAIL | 断线时第一条腿是否已放行 |
| 全局不变量 1–8 | 全部成立 / 第 N 条不成立 | |
```

F3 的冻结上限一栏在 D-2 修掉之前**只能**填 `KNOWN-FAIL`。标记撤回一栏填 PASS 之前先确认真的测到了点上:日志里必须出现过 `[ZoneTravel][WithdrawMark] deferred`(没出现多半是 Redis 停得太晚,或 K-SM 没撑开窗口,撤回在 Redis 还通的时候就成功了——那只是重复了 C2)。

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

- **2026-09-21 v2.4**:Z1 修复、断线释放标记 A1′ / A2′、GO-5 重连落点、epoch==0 同节点铸造已落码(设计文档 §12.6.10 / §12.7 / §12.6.9;C++ 未重新编译、Go 未编译,均未测试)。§1 事实 2、§2.1 handoff 键的写入 / 删除方补 A1′ / A2′;§2.3 新增 `[ExitPersist]` / `[ExitRelease]` 两行汇总与 M12 的 `stale_marker` 口径;§2.4 新增 v2.4 的 C++ 日志表(`[ExitRelease]`、`[ExitRelease][InheritClear]`、`Player marked for unregistration` 改到收敛时才打等)与 login `ZoneId=0` 日志;新增**场景 Z**(Z1 复现与修复验证)与**场景 R**(GO-5 重连落点五种情形);§5 新增不变量 8;§7 表同步。因 A′ / GO-5 改期望:§3.3(第二条腿载入后 A2′ 清掉旧标记;robot 退出后出现 A1′ 标记)、§3.4(前置接受 A1′ 标记;残留等待改为 30s)、B2 与 D2(重登落在 home zone,不再按等待落点送往目标 zone —— R8 已被取代)、C1(常见形态退出后键 = A1′ 标记)、C2 / F3(补 `skip_handoff_inflight`,键仍不应出现)。§9 未改。同日追加:场景 R2 的前提改为已静态核实(会话在 SharedRedis,D12;各 zone 的 player_locator 必须连同一个 Redis),并注明客户端第二条腿不应再弹选角(`GameClient.ResolveRedirectPlayerId`);`skip_identity_conflict` 的触发条件随 M3 探针收紧而变严(lease 未授予 / 重注册中 / ACK 超过 TTL/2 都算),验证见设计文档 §12.6.10 清单 11b。
- **2026-09-21 v2.3**:新增 §9(帮会二期 B4c):`stale_owner_write_rejected` 的第二种预期来源(K1 被正确拦下)与区分方法;scene 侧三条值班 LogQL;指向 `deploy/k8s/owner-epoch-alerts.yaml`。上文各场景的判定不变。
- **2026-09-21 v2.2**:S3L1-1 第二层出口 + 铸造重放识别落码(设计文档 §12.5.6,未编译未验证)。§2.3 `[TravelHandoff]` 行按 `player_lifecycle.cpp:238-252` 实际输出重写(补 `withdraw_deferred` / `withdraw_expired` / `granted_client_reset`),新增指标 `scene_manager_enter_scene_rollback_total{outcome}`;§2.4 新增 `[ZoneTravel][ClientReset]` 两条、`GET owner_epoch failed` 改为 `MGET owner_epoch/location failed or malformed`、`was granted although …` 带 `evidence=`;scene_manager 回滚日志改为 `[RouteRollback] outcome=` 前缀,**旧原文 grep 口径作废**;robot 补 `kicked by server` 口径;§1 机制补踢线分支;B2 期望改成"踢线 / 退出优先"二选一;新增 **B3**(Kafka pause + 回滚注入 Redis 错误);§5 不变量 6 与 §7 结果表同步。
- **2026-09-20 v2.1**:缺陷 D-1(Redis 断开时 handoff 标记撤不回)的修复已落码(未编译未验证):C2 / F3 / §6 / §7 按 `WithdrawHandoffMark` 的日志与计数改写,F3 拆成「标记撤回:可判 PASS/FAIL」与「冻结上限:仍 KNOWN-FAIL(D-2)」;原缺口 G3 关闭;`enter_scene_rejected_total` 的 reason 补 `home_zone_unmapped_travel` / `scene_gone`。前置新增:测试号必须有 `player:zone` 映射(新建角色自带;存量号先跑 `merge_zone -backfill-home-zone`),否则 TravelToZone 第一条腿直接回 20。
- **2026-09-20 v2**:整篇重写。v1 的 A–D 四个场景(Kafka 暂不可达 / 目的节点持续不可达 / 源节点重启 / Kafka 重复投递)全部针对已下线的 `player_migrate` + ACK + `CrossZoneReaper` 链,作废。新场景 A–F 面向 handoff 标记 + `owner_epoch` 两道门、两道 30s 看门狗、"退出优先"与属主接管。依据 `main`@`d9e471b80` 静态编写,**链路未编译、未实跑,本文未经执行验证**。F3 标注为当前预期失败(D-1 退出路径 `DEL` 被 `connected()` 守卫;D-2 冻结无服务端上限)。移除对已不存在的 `robot/clients/cmd/multi_zone_seeder`、`single_player` 的引用。
- **2026-05-19 v1**:初版,覆盖跨 zone 三件套的 4 个失败场景。被测对象已于 2026-09-18 阶段 3 删除。
