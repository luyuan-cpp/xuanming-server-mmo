# 跨 Zone 就绪审计(Cross-Zone Readiness Audit)

> **⚠️ 状态说明(2026-09-20):本文 §1–§10 已被 [cross-zone-scene-travel.md](./cross-zone-scene-travel.md) 取代,正文仅作历史保留,不得据此推理现状。**
> - 本文描述并试图修补的「Kafka 自治 + 三件套」搬数据链——`player_migrate` / `player_migrate_ack` 两个 topic、`HandleCrossZoneTransfer` / `HandlePlayerMigration` / `HandlePlayerMigrationAck`、`CrossZoneReaper`、Redis `player_migration:{id}`、`kafka/system/kafka.{h,cpp}`——已在跨 zone 传送**阶段 3(2026-09-18)整条删除**,清单见 cross-zone-scene-travel.md §11.3。文中引用的函数与行号均已不存在。
> - 现行跨 zone 路径 = **重定向 + 目标 zone 从盘上直接加载**(CZ-1),数据不经 Kafka 搬运;换手由 `scene_manager.EnterScene` 的两道门裁决(`owner_epoch` CAS + 源 scene 写的 `player:{id}:handoff` 落盘标记,CZ-4)。数据流见该文 §4,源端释放链见 §11.2,已知限制见 §7 / §10.3。「7 组件 / bag、quest 丢失」的判定也已过时:bag / mission 已进普通存盘链。
> - **仍然有效的只有两处**:§3.3(mail 走独立 Go 服务,即 CZ-10)与 §11(`PlayerFrozenComp` 业务接入分类——这道拦写闸被传送链原样复用)。§11 里的「迁移中 / reaper」字样按该节开头的说明换读。
> - 阶段 1/2/3 的代码已进 main 但**尚未编译、未跑测试**,现状同样不得当作"已验证"引用。

> **状态**: v1 — 2026-05-16
> **背景**: 用户明确「玩家肯定要跨 zone 玩」是核心设计。本文件审计当前实现距离这个设计目标的真实差距,并给出可执行修复方案。
> **审计深度**: 直接读 C++ player_lifecycle / scene / proto 持久化代码,所有结论都基于代码事实。
> **核心结论**: **跨 zone 当前不可生产**,玩家跨一次 zone 即丢失 bag/quest/mail 数据。修复需 4-6 周。

---

## TL;DR

| 维度 | 现状 | 严重度 |
|---|---|---|
| 玩家跨 zone 时数据完整跟随 | ❌ 只跟 7 个 ECS 组件,bag/quest/mail 静默丢失 | 🔴 致命 |
| Kafka migration 失败处理 | ❌ 失败 = 丢玩家(zone 1 已 DestroyPlayer,zone 500 没收到) | 🔴 严重 |
| Single Writer 保证 | ✅ 通过「先 flush 再 Kafka 再销毁」隐式成立 | ✅ 已满足 |
| Layer 2 per-player 锁兜底 | ✅ Router.AcquirePlayerLock 已实现 | ✅ 已满足 |
| 架构文档 vs 实际实现 | ⚠️ §7-8 描述的「SceneManager 严格 ACK 编排」未实现,实际是 Kafka 自治 | 🟡 需要修订文档 |

---

## 1. 当前跨 zone 链路(代码事实)

> **历史记录(2026-09-20 标注)**:本节是 2026-05-16 当时的链路。该链已在阶段 3 整条删除(cross-zone-scene-travel.md §11.3),§1.2 引用的 `kafka.cpp` / `player_lifecycle.cpp` 行号均已不存在;现行数据流见 cross-zone-scene-travel.md §4。

### 1.1 完整时序

```
zone 1 scene 节点                                         zone 500 scene 节点
─────────────────                                         ───────────────────

[t0] 玩家请求跨 zone (设置 ChangeSceneInfoComp.is_cross_zone=true)

[t1] HandleExitGameNode (player_lifecycle.cpp:375)
       ├─ emplace UnregisterPlayer 标记
       ├─ 移出 AOI
       ├─ SnapshotSystem::CaptureAndSend(SNAPSHOT_LOGOUT)
       └─ SavePlayerToRedis(player) ← 异步:Marshal 7 组件 + Kafka 发 DBTask
              ↓
              (异步等 Redis ACK + Kafka ACK)
              ↓
[t2] HandlePlayerAsyncSaved (line 163) ← Redis 保存完成回调
       ├─ HandleCrossZoneTransfer (line 170)
       │     ├─ Marshal 7 组件 → PlayerAllData ⚠️ 只 7 组件
       │     ├─ Kafka send "player_migrate" ──→ ──→ ──→ ──→ ──→ ──→ ┐
       │     └─ remove<ChangeSceneInfoComp>                         │
       │                                                            │
       └─ 进入 UnregisterPlayer 路径:                                │
             ├─ RemovePlayerSession                                 │
             └─ DestroyPlayer ⚡ ← 玩家实体此刻销毁                   │
                                                                    │
                                          [t3] KafkaMessageHandler ◄┘
                                                ├─ ParseFromString
                                                ├─ HandlePlayerMigration
                                                │     ├─ InitPlayerFromAllData
                                                │     │   (新建 player 实体,
                                                │     │    Unmarshal 同样 7 组件)
                                                │     └─ SavePlayerToRedis
                                                └─ (玩家在 zone 500 上线)
```

### 1.2 关键代码引用

| 文件 / 行 | 作用 | 问题 |
|---|---|---|
| `cpp/libs/services/scene/player/system/player_lifecycle.cpp:434` `HandleCrossZoneTransfer` | 跨 zone 转移入口 | 只 Marshal 7 组件 |
| `cpp/libs/services/scene/player/system/player_data_loader.h:10-14` `PlayerAllDataMessageFieldsMarshal` | PlayerAllData 序列化 | 等同 player_database + player_database_1(空壳) |
| `cpp/libs/services/scene/player/system/player_database_loader.cpp:17-25` `Marshal` | 实际 marshal 实现 | 7 个 emplace,无 bag/quest/mail |
| `cpp/libs/services/scene/player/system/player_database_1_loader.cpp:5-9` | player_database_1 Marshal | **函数体为空** |
| `cpp/libs/services/scene/player/system/player_lifecycle.cpp:217` `DestroyPlayer` | t2 销毁玩家 | 销毁后 Kafka 失败就丢玩家 |
| `cpp/libs/services/scene/kafka/system/kafka.cpp:9-20` `KafkaMessageHandler` | zone 500 接收 | 无 ACK 回送 |

### 1.3 PlayerAllData 实际形态

```proto
// proto/common/database/player_cache.proto
message PlayerAllData {
  player_database player_database_data = 2;       // 7 个组件
  player_database_1 player_database_1_data = 3;   // 空壳,只有 stress_test_probe
}

// proto/common/database/mysql_database_table.proto:90-107
message player_database {
  uint64 player_id = 1;
  Transform transform = 2;                        // ✅
  PlayerUint64Comp uint64_pb_component = 3;       // ✅
  PlayerSkillListComp skill_list = 4;             // ✅
  PlayerUint32Comp uint32_pb_component = 5;       // ✅
  BaseAttributesComp derived_attributes_component = 6;  // ✅
  LevelComp level_component = 7;                  // ✅
  CurrencyComp currency = 8;                      // ✅
  PlayerStressTestProbe stress_test_probe = 9;
  // ❌ 无 BagComp
  // ❌ 无 QuestComp
  // ❌ 无 MailComp
}
```

跨 zone 时携带的就是这个 `PlayerAllData`。bag/quest/mail **在 proto 里就不存在**,自然不会跟。

---

## 2. 现状跟设计意图的差距

`mmo_cross_server_architecture.md §1` 写明:

> Player affiliation: each player has a home zone, **but can freely visit scenes on other zones**.

`§2 Core Principle: Location Transparency`:

> 不管玩家在哪个服务器上,处理逻辑都和在本服一样,不需要判断"是否跨服"

**实际**:玩家跨 zone 时,bag/quest/mail 数据**静默归零**。位置透明这个原则,只对「7 个 ECS 组件」成立,对集合型数据完全不成立。

### 三个层级的问题

#### 🔴 致命:跨 zone 静默丢数据

每次跨 zone 切换,玩家的背包 / 任务 / 邮件被永久丢弃。重启 / 退出登录时也丢(`HandleExitGameNode` 也走同样 7 组件 SavePlayerToRedis)。这是当前架构跨 zone **不可投产**的根本原因。

#### 🔴 严重:Kafka migration 失败 = 丢玩家

zone 1 销毁玩家实体后,如果 zone 500 没收到消息(broker 重启 / 网络分区 / partition 重平衡 lag 几十秒),玩家**两边都不在线**。需要超时检测 + 重发 + dead-letter 处理,**当前完全没有**。

代码证据:`player_lifecycle.cpp:217` `DestroyPlayer(playerId)` 在 Kafka send 之后立即执行,没有 ACK 等待。

#### 🟡 中等:架构文档与实现不一致

`mmo_cross_server_architecture.md §7-8` 描述的「SceneManager 编排转移流程,旧 scene release → SceneManager 确认 → 新 scene load」**根本没实现**。实际是「旧 scene 自己 flush 完后直接 Kafka 通知新 scene」,SceneManager 不参与跨 zone 转移过程。

这不是 bug,实际形态(Kafka 自治)其实更好(见 §3),但文档需要修订以反映真相。

---

## 3. 修复方案 — Kafka 自治 + 三件套

### 3.1 为什么不回到 SceneManager 严格编排

理论上 `mmo_cross_server_architecture.md §7-8` 那种「SceneManager 严格 ACK 编排」最干净,但**对你的设计意图不合适**。理由:

1. **延迟**:每次跨 zone 4 次 RPC(老 scene ↔ SceneManager ↔ 新 scene),跨 zone 体验从 50-200ms 飙到 500ms-1s
2. **单点**:SceneManager 挂了所有玩家不能跨 zone
3. **Kafka 已经提供顺序**:按 playerId 一致性哈希到同 partition,串行问题已解决
4. **不符合「位置透明」高频跨 zone 的核心场景**

所以正确方向是:**保留 Kafka 自治形态,补三件套修复问题**。

### 3.2 三件套

#### 件 1:PlayerAllData 数据完整化

在 `PlayerAllData` 里加 `BagAllData / QuestAllData / MailAllData` 子 message(平级于 `player_database_data`,**不挤进 player_database**)。理由:

- player_database 已经 7 个组件,再加进去 schema 越来越乱
- bag/quest/mail 是「集合型」数据,跟「组件型」分开更清晰
- 拆分后回档 partial 天然支持(独立 field path)

新形态:

```proto
message PlayerAllData {
  player_database player_database_data = 2;
  player_database_1 player_database_1_data = 3;
  BagAllData bag_data = 4;        // ⚡新
  QuestAllData quest_data = 5;    // ⚡新
  MailAllData mail_data = 6;      // ⚡新(取决于设计决策,见 §3.3)
}

message BagAllData {
  repeated ItemEntry items = 1;
}

message ItemEntry {
  uint64 item_uuid = 1;        // SnowFlake guid (已有)
  uint32 config_id = 2;
  uint32 stack_size = 3;
  uint32 pos = 4;
  uint32 bag_type = 5;         // EnumBagType
  // ⚠️ 装备的强化等级 / 词条 / 宝石 镶嵌 — 待产品/策划定 schema
}
```

#### 件 2:Frozen 状态 + 延后 DestroyPlayer

当前代码在 Kafka send 后立刻 `DestroyPlayer`(`player_lifecycle.cpp:217`),这是 Single Writer 严格成立的核心 —— 但也是失败时丢玩家的核心。

改成:**保留实体但加 `PlayerFrozenComp` 标记**。所有业务系统看到这个标记就 skip 处理:
- AOI 不发消息(此条已经做了一半 —— `HandleExitGameNode` 已移出 AOI grid)
- 战斗系统不接受输入
- 货币系统不能扣
- gain_block 全开
- 但**玩家实体存在**,client 还连着 gate,失败时能直接解冻继续玩

Single Writer 仍然成立 —— Frozen 状态下不接受任何写入,等同于"不存在"。

收到 ACK 后才真正 `DestroyPlayer`。

#### 件 3:Redis migration 状态 + ACK + reaper

```
Redis key: "player_migration:{playerId}" → JSON {
  from_zone: 1,
  to_zone: 500,
  to_node: "N-23",
  start_ms: t1,
  attempt: 1,
  payload_sha256: "...",      // 检测 Kafka 重复投递
}
TTL = 60s
```

**正常路径**:
- t1: zone 1 写入 Redis
- t3: zone 1 收到 ACK,删除 Redis key

**失败路径(后台 reaper,每 10s 跑)**:
- 扫所有 `player_migration:*` Redis key
- 过滤 `from_zone == self`
- 发现 attempt=1 超过 30s 没 ACK → 重发 Kafka(attempt=2,max 3 次)
- 第 3 次仍超时 → 走兜底:解冻玩家,通知 client「跨 zone 失败,留在原 zone」,删 Redis key

**zone 1 节点崩溃恢复**:
- 启动时扫所有 `player_migration:*` Redis key,过滤 from_zone=自己
- 查 `player_locator:{pid}` 当前 home_node
- 如果指向 to_node → 转移其实成功了,清理 Redis migration 状态
- 还指向 from_node → 重发 Kafka migrate

### 3.3 mail 系统的特殊讨论(决策 6)

mail 跟 bag/quest 不同 —— 邮件有**离线发送**场景:玩家 A 给离线的玩家 B 发邮件,B 不在任何 scene 节点上。

两种处理:

**选项 A**:mail 跟 bag 一样塞进 PlayerAllData。问题:离线收件人没有 scene 节点处理邮件,需要专门的离线邮件服务来缓存。

**选项 B**:mail 走独立 Go 服务(像 guild/friend 一样),不跟 PlayerAllData 跨 zone。MMO 主流做法。

**我的建议**:**选项 B**。独立 mail Go 服务 + Redis/MySQL 持久化。跨 zone 时 mail 自然不感知 zone(就像 guild/friend),完全符合「位置透明」原则。

**这件事可独立做**,不阻塞 bag/quest 修复。

### 3.4 SceneManager 的最小职责

明确划清边界:

**SceneManager 做**:
1. 玩家上线时分配 home scene 节点(已实现)
2. **跨 zone 时的「目的节点选择」**:玩家说「去 zone 500」,SceneManager 回答「zone 500 当前负载最低的 scene 节点是 N-23」

**SceneManager 不做**:
- ❌ 不编排转移过程
- ❌ 不持有 in-flight migration 状态
- ❌ 不发 ACK
- ❌ 不持久化

转移状态在 Redis(决策 3),所有 scene 节点都能读。SceneManager 不变成单点。

---

## 4. 实施顺序与工作量

按依赖关系:

| 步骤 | 任务 | 工作量 | 阻塞 |
|---|---|---|---|
| **1** | PlayerAllData proto 加 BagAllData(子 message)+ Marshal/Unmarshal | 1-2 周 | 产品定 ItemEntry schema |
| **2** | 跨 zone 加 Frozen 状态(PlayerFrozenComp)+ 延后 DestroyPlayer | 0.5 周 | 0 |
| **3** | 跨 zone ACK + Redis migration 状态 + reaper | 1 周 | 步骤 2 |
| **4** | 失败场景测试(Kafka broker 重启 / scene 节点崩溃 / partition 重平衡)| 0.5 周 | 步骤 3 |
| **5** | quest 持久化(同样模式扩展)| 0.5-2 周 | 先做任务 #22 摸清 quest 系统当前形态 |
| **6** | mail 系统设计与实现(选项 B 独立 Go 服务)| 2-3 周 | 设计决策 |
| **7** | 更新 `mmo_cross_server_architecture.md §7-8 §13` | 0.5 个会话 | 步骤 1-4 完成 |

**总工作量 4-6 周**(不含 mail 系统,mail 独立做需另加 2-3 周)。

**步骤 1 是阻塞所有的核心**。它的真实阻塞是「**产品策划定 ItemEntry schema**」—— 装备的强化等级 / 词条 / 宝石镶嵌需要哪些字段?这个 AI 拍不了,**需要先去拉策划聊**。

---

## 5. 任务对应表

把 #11 / #21 / #22 三个 pending 任务跟本方案对应:

| 任务 | 对应方案步骤 | 备注 |
|---|---|---|
| #11 (ExportPlayerData / ImportPlayerData RPC) | 步骤 1-4 完成后 → 加 proto 字段过滤即可 ~2-3 天 | **真正阻塞 #11 的是跨 zone 修复,不是 RPC 本身** |
| #21 (bag 持久化方案 A) | **改为本方案的步骤 1**(BagAllData + PlayerAllData 路径) | 之前的「方案 A 加 BagComp 到 player_database」改成「加 BagAllData 到 PlayerAllData」,更清晰 |
| #22 (quest/mail 持久化审计) | 步骤 5 + 步骤 6 的前置 | quest 走类似 bag 的路径,mail 单独决策 |

---

## 6. 跟 transaction_log / 补缴系统的关系

`docs/design/single_player_rollback.md` 描述的「物品 UUID + transaction_log」体系**已经完整落地**(详见 `AUDIT.md §1.1.2-1.1.5`)。本方案不冲突:

- bag 内每个 item 已经有 `item_uuid`(SnowFlake)
- `BagAllData.items[i].item_uuid` 直接用现有的 guid
- 跨 zone 不需要额外的 transaction_log 记录(物品没易主)
- **补缴系统(`PlayerCurrencyComp::debts`)在 CurrencyComp 里**,已经在 7 组件之内,跨 zone 自然跟随 ✅

---

## 7. 失败场景详细处理

### 失败 A:zone 500 没收到 Kafka(broker 重启 / partition 重平衡)

```
[t1+30s] zone 1 还没收到 ACK
      ├─ 后台 reaper(每 10s 扫一次 "player_migration:*" Redis key)发现 attempt=1 超时
      ├─ 重发 Kafka(attempt=2),max 3 次
      ├─ 第 3 次仍超时 → 走兜底路径(失败 B 处理)
```

### 失败 B:zone 500 节点崩溃(消息消费了但 InitPlayerFromAllData 异常)

```
[t1+90s] 三次 attempt 都超时
      ├─ zone 1 把玩家实体从 Frozen 解冻
      ├─ 通知 client "跨 zone 失败,回到原 zone"
      ├─ 删除 Redis "player_migration:{playerId}"
      └─ 玩家继续在 zone 1 玩(数据完整,因为 t1 已经 SavePlayerToRedis 过)
```

### 失败 C:zone 1 节点在 [t1-t3] 之间崩溃

```
[zone 1 重启 / 新节点替换]
      ├─ 启动时:扫所有 "player_migration:{*}" Redis key,过滤 from_zone=自己
      ├─ 对每条:查 player_locator:{pid} 当前 home_node
      ├─ 如果 player_locator 已经指向 to_node → 转移其实成功了,清理 Redis migration 状态
      └─ 如果还指向 from_node → 重发 Kafka migrate(攒着旧 attempt 计数继续)
```

### 失败 D:Kafka 重复投递(broker 重平衡导致同消息消费两次)

```
zone 500 收到第 2 次 player_migrate
      ├─ 通过 payload_sha256 比对 Redis "player_migration:{playerId}" 已知载荷
      ├─ 一致 → 已经处理过,只回 ACK,不再 InitPlayerFromAllData(幂等)
      └─ 不一致 → 异常报警,人工介入
```

---

## 8. 监控指标(metrics 接入)

跨 zone observability 接入现有 `data_service/internal/metrics/metrics.go` 框架。新增:

| metric | 类型 | label | 用途 |
|---|---|---|---|
| `cross_zone_migration_total` | counter | `from_zone, to_zone, outcome` | outcome ∈ {ok, ack_timeout, kafka_send_failed, dest_node_crashed, retry_succeeded} |
| `cross_zone_migration_latency_seconds` | histogram | `phase` | phase ∈ {marshal, kafka_send, ack_wait, total} |
| `cross_zone_in_flight` | gauge | `from_zone` | Redis player_migration 当前 key 数量,从 reaper 报告 |
| `cross_zone_reaper_recoveries_total` | counter | `outcome` | outcome ∈ {kafka_resent, declared_failed, found_succeeded} |

这些 metric 跟前面会话定义的 `crossSceneTransitionLatency / crossSceneTransitionTotal` 可以合并 / 改名 / 落地。

---

## 10. 步骤 2 实施过程的新发现(2026-05-16 v2)

> **历史记录(2026-09-20 标注)**:本节所列函数与 topic 订阅已在阶段 3 全部删除(cross-zone-scene-travel.md §11.3;`player_migration_event.proto` 只剩打了 DEPRECATED、待下轮 regen 清掉的空壳)。§10.5 的「✅ 已落 / ❌ 必须」是 2026-05-16 的快照,不代表现状,不要照它去补订阅。唯一留下来的是 `PlayerFrozenComp`:现在只由 `PlayerLifecycleSystem::StartTravelHandoff` 与 `PlayerTravelHandoffComp` 成对挂上,摘除时机见 `player_frozen_comp.h` 头注释。

实施步骤 2(PlayerFrozenComp + 延后 DestroyPlayer)时,深查 Kafka 订阅链路发现 **`player_migrate` topic 当前根本没有 consumer**。

### 10.1 证据

```bash
grep -rn "RegisterKafkaMessageHandler" cpp/
# 只命中 engine 实现 + SceneCommand 命令处理器模板。
# 没有任何代码调用 `RegisterKafkaMessageHandler({"player_migrate"}, ...)`
```

`HandleCrossZoneTransfer` 发布到 `"player_migrate"` topic 的消息,**当前无人消费**。`KafkaSystem::KafkaMessageHandler` 写的代码是对的,但**没接到任何订阅链路**。

### 10.2 这个事实改变了什么

跨 zone **当前是单向的**:
- 源端 zone 1 publish `player_migrate` → broker 收 ✅
- 目的端 zone 500 **没订阅** ❌ → 玩家在 zone 500 不会出现
- 源端 zone 1 `DestroyPlayer` 立即跑(改之前的代码)→ **玩家两边都没了**

**之前 §1 §7 描述的"失败场景"其实是"每次都失败"**。生产部署如果跨 zone,玩家**100% 丢**(不是偶发)。

不过现在 PlayerFrozenComp 落地后,**最坏退化为「玩家永远卡在源端 Frozen 状态」**,不再"两边都消失"。reaper 启动后会自动 unfreeze。所以**步骤 2 的代码实际上把单向 broken 变成单向 stuck**(从致命到可恢复)—— 严格意义上仍然是改进。

### 10.3 修复方案选项

两个方案在任务 #25(ACK + reaper)时一起做:

**选项 A:直接订阅,新 topic 不走 SceneCommand 包装**

```cpp
// 在 cpp/nodes/scene/main.cpp 加(SetAfterStart 内或 SetKafkaHandlers 内):
node.RegisterKafkaMessageHandler(
    {"player_migrate", "player_migrate_ack"},
    "scene-cross-zone",
    {},
    &KafkaSystem::KafkaMessageHandler);
```

**优点**:简单直接,跟 `KafkaSystem::KafkaMessageHandler` 已写的代码完美对接。
**缺点**:增加一组独立的 Kafka subscription,跟 `SceneCommand` 的体系分离。groupId 需要好好定(不要跨节点共享 consumer group,否则同 zone 多个 scene 节点抢同一份 ack)。

**选项 B:把 PlayerMigrationEvent / PlayerMigrationAckEvent 包进 SceneCommand**

让两个事件通过现有 `scene-{nodeId}` 命令 topic 走。需要:
1. 把 `PlayerMigrationEvent.SerializeAsString()` 包成 `SceneCommand.payload`
2. 在 `scene_kafka_command_router.cpp` 加 case 分发到 `HandlePlayerMigration / HandlePlayerMigrationAck`
3. 目的端要知道哪个 scene 节点是目的(已经走 partition_key=playerId 一致性哈希,但 SceneCommand 的 topic 是 `scene-<nodeId>`,需要 SceneManager 路由查询拿到 nodeId 才能选 topic)

**优点**:跟现有 `SceneCommand` 体系一致,只一组 Kafka subscription。
**缺点**:跨 zone 转移需要先 RPC 查 SceneManager 拿目的 nodeId,**延迟从纯 Kafka 增加到 RPC + Kafka**,违背了 §3.1 不回到 SceneManager 编排的原则。

**我的判断**:**选 A**。SceneCommand 包装会把跨 zone 的延迟从 50-200ms 拉到 250-400ms(因为多一次 SceneManager.PickSceneNode RPC),失去了 Kafka 自治的核心价值。选 A 的独立 subscription 是合理的复杂度成本。

### 10.4 任务 #25 的细化

任务 #25(ACK + reaper)实际包含 3 个子任务:

1. **#25-a topic 订阅接线**(选项 A 的代码改动)
2. **#25-b Redis migration 状态 + reaper**(原 §3.2 件 3 的核心)
3. **#25-c 失败场景测试**(原任务 #24)

#25-a 是独立可做的最小步骤(< 0.5 个会话),做完之后**跨 zone 才真正能跑**(只是没失败恢复;有了 Frozen 状态,失败也只是 stuck 而非丢玩家)。

### 10.5 步骤 2 的真实完成度

| 件 | 状态 |
|---|---|
| PlayerFrozenComp 纯 C++ struct | ✅ 已落 (`player_frozen_comp.h`) |
| HandleCrossZoneTransfer emplace Frozen | ✅ 已落 |
| HandlePlayerAsyncSaved 跳过销毁 | ✅ 已落 |
| HandlePlayerMigrationAck 收到 ACK 后清 Frozen + DestroyPlayer | ✅ 已落 |
| HandlePlayerMigration 发 ACK | ✅ 已落(用 PlayerMigrationAckEvent protobuf) |
| KafkaSystem::KafkaMessageHandler 路由 ACK topic | ✅ 已落(但 see §10.1 — handler 没被订阅) |
| PlayerMigrationAckEvent proto 定义 | ✅ 已落 |
| 业务系统(AOI / 战斗 / 货币)检查 IsCrossZoneFrozen skip | ❌ **未做** — 这是另一笔工作,跨多个 system 文件 |
| 重新生成 proto(让 `PlayerMigrationAckEvent` 在 .pb.h 出现) | ❌ **必须**,否则编译不过 |
| Kafka topic 订阅(`player_migrate` + `player_migrate_ack`)| ❌ **必须**,否则消息不来 |

「业务系统加 Frozen 检查」单独拆成任务 #27(下次会话做)。整套链路 build 通 + 真正跑起来还需要做 §10.3 选项 A 的订阅接线。

---

## 11. 业务系统 Frozen 接入分类指南(任务 #29)

> **本节仍然有效,但语境已变(2026-09-20 标注)**:分类判据与拦写闸(`IsCrossZoneFrozen` / `any_of<PlayerFrozenComp>`)被传送链原样复用,多处代码注释仍引用本节编号。阅读时请换读三处——
> - 「跨 zone 迁移中 / 转移中」= **归属交接在途**:跨 zone 传送,以及同 zone 跨节点换图(被 scene_manager 以 18 `ErrHandoffPending` 暂拒之后的「冻结 → 存盘 → 写 handoff 标记 → 重发」,cross-zone-scene-travel.md §11.2)。
> - 「等 ACK 后 DestroyPlayer」= scene_manager **放行**后由 `DestroyDeposedPlayer` 不存盘销毁;已没有 Kafka 迁移包,也没有目的端 ACK,目标节点从盘上加载。
> - 「reaper 兜底 / reaper 解冻」(§11.4 的 GM 强制下线行、§11.6 的失败场景项)= 已没有 reaper。交接未成(EnterScene 错误应答,或存盘 / 应答看门狗到期)由 `AbortTravelHandoff` 解冻——EnterScene 发出之后的「未成」先经 `ResolveTravelOutcome` 按 `owner_epoch` 核实,epoch 已变说明其实已放行,改为销毁而不是解冻;交接途中退出(含 GM 强制下线)走普通退出的「退出优先」分支:`FinishExitAfterPersist` 摘掉 `PlayerTravelHandoffComp` + `PlayerFrozenComp` 并撤回已写的 handoff 标记,漏摘的 Frozen 记 ERROR 后解冻继续退出,绝不悬挂。

> **2026-05-16 v3 新增**: 步骤 2 落地 PlayerFrozenComp 后,需要让所有业务系统在写入 / tick 玩家数据前检查 `PlayerLifecycleSystem::IsCrossZoneFrozen(player)`,否则 Single Writer 在业务层不严格成立。这一节是给下次会话用的**分类决策表**,避免「每文件加一行」式粗暴改动。

### 11.1 写入类(必须 reject)

发起写入的入口必须在第一行检查 Frozen,return error / 静默 skip(选哪种看现有 RPC 错误码体系是否合适)。

| 系统 | 入口函数 | 处理 |
|---|---|---|
| 货币 | `CurrencySystem::AddCurrency` / `DeductCurrency`(`currency_system.cpp:57/152`)| **return PrintStackAndReturnError(kInvalidParameter)**(玩家 Frozen 时既不该收钱也不该扣钱)。注意:`AddCurrency` 内部的补缴 hook 也会跟着 skip,这正确 —— 玩家 Frozen 时债务也不该扣 |
| 背包 | `BagService::AddItem` / `RemoveItem`(`bag_service.cpp:34/78`)| **return error**。会同时跳过 `LogItemCreate` / `LogItemDestroy` —— 这正确,跨 zone 期间不该出现额外 transaction_log 记录 |
| 任务 | `QuestSystem::*`(待 #22 摸排)| **return error** |
| 邮件附件 | `MailSystem::ClaimAttachment`(待 mail 模块决策)| **return error** |
| GM 命令 | `GmHandler::Apply*`(grep `Gm.*Player`)| **return error**,客服收到「玩家正在跨 zone,请稍候」 |

**关键**:位置写在最前,**早 return**。不要让任何 transaction_log / Kafka 副作用先发出去再 return —— 那会污染审计流。

```cpp
uint32_t CurrencySystem::AddCurrency(entt::entity player, CurrencyType type, int64_t amount)
{
    // ── 0. Frozen check (cross-zone migration) ──
    if (PlayerLifecycleSystem::IsCrossZoneFrozen(player))
    {
        LOG_DEBUG << "CurrencySystem::AddCurrency rejected: player is mid-cross-zone-migration";
        return kInvalidParameter;
    }
    // ── 1. existing param validation ──
    ...
}
```

### 11.2 被动 tick 类(应该 skip)

每帧自动跑的 system 应该跳过 Frozen 玩家。不 skip 的话会有「Frozen 玩家 buff 还在衰减,到达目的端时已经过期」之类的状态漂移。

| 系统 | 文件 | 处理 |
|---|---|---|
| Buff tick | `buff.cpp` | view 遍历时跳 Frozen 玩家;buff 倒计时**完全冻结**(不衰减) |
| 技能冷却 | `skill.cpp` | 同上 |
| Combat state | `combat_state.cpp` | 同上 |
| AOI grid 更新 | `aoi.cpp`、`interest.cpp` | Frozen 玩家已经在 `HandleExitGameNode` 移出 AOI grid,理论不会再被 view 命中。但加 defensive check 防漏 |
| 移动插值 | `movement.cpp` | Frozen 玩家位置不再变化 |
| 属性计算 | `actor_attribute_calculator.cpp` | skip |
| 属性同步广播 | `attributedelta*frames*.cpp`(generated)| skip(generated 文件,改 generator,不改 cpp) |

实现模式 —— 把检查内置到 view filter 或循环开头:

```cpp
// 选项 A:view filter(entt 0.13+)
auto view = tlsEcs.actorRegistry.view<BuffComp>(entt::exclude<PlayerFrozenComp>);

// 选项 B:循环内 skip
for (auto entity : view) {
    if (PlayerLifecycleSystem::IsCrossZoneFrozen(entity)) continue;
    ...
}
```

**优先选项 A**(entt exclude),性能更好。

### 11.3 接收消息类(应该回反馈)

客户端发来的消息(还连着 gate session,因为 Frozen 不断连),应该收到「迁移中,请稍候」提示而不是静默丢弃。

| 系统 | 入口 | 处理 |
|---|---|---|
| 移动指令 | `movement.cpp` 的 c2s handler | reject + `kSceneTransferInProgress` tip(已存在,见 `player_lifecycle.cpp:501`)|
| 技能释放 | `skill.cpp` 的 c2s handler | 同上 |
| 聊天 | `chat_*.cpp`(grep)| 同上 |
| 交易 / 拍卖 | `trade_*.cpp` / `auction_*.cpp` | 同上 |

提示用现有 `PlayerTipSystem::SendToPlayer(player, kSceneTransferInProgress, {})`,不要新增 tip code。

### 11.4 跨玩家影响类(协议讨论,本会话不决)

| 场景 | 问题 | 暂定策略 |
|---|---|---|
| 玩家 A 攻击 Frozen 的玩家 B | B 应该收伤害吗? | **不收** —— B 已经在转移中,源端不再代表 B 的真实状态。A 的攻击效果 drop |
| 玩家 A 治疗 Frozen 的玩家 B | 同上 | **不治** |
| 队友 buff 给到 Frozen 玩家 | 同上 | **不给** |
| 给 Frozen 玩家发邮件 | 邮件是离线写入,跟 Frozen 无关 | **正常发**(走 mail 服务,不经过源端 scene) |
| GM 强制下线 Frozen 玩家 | 跟正常玩家不同处理 | **特殊路径** —— 把 Frozen 转成异常 abort,reaper 兜底 |

**这一类需要 game design 拍板**(伤害 drop 玩家会抗议吗?),不是纯工程问题。任务 #29 实施时挂起此类,等产品答复。

### 11.5 实施顺序(给下次会话)

1. **先 §11.1(写入类)** —— 1 个会话,不需要协议讨论,直接改
2. **再 §11.2(被动 tick)** —— 1 个会话,改 generator 影响 generated 文件
3. **再 §11.3(消息类)** —— 0.5 个会话,套用现有 tip 机制
4. **§11.4 暂挂**,等 game design 答复

完成 1+2+3 后,**Single Writer 在业务层严格成立**,Frozen 状态语义完整。

### 11.6 验证清单

每改完一类后必须验证:
- [ ] 现有单元测试 pass(`bag_test`、`skill_test` 等)
- [ ] 跨 zone 模拟测试:玩家发起跨 zone 后,客户端发 AddCurrency / 移动 / 技能,看是否被正确 reject + 收到 tip
- [ ] 失败场景:Frozen 状态下源端节点重启,reaper 解冻后玩家应该能恢复正常 AddCurrency / tick



- **2026-05-16 v1**: 初版。直接读 C++ player_lifecycle / scene 代码确认现状。提出 Kafka 自治 + 三件套方案。明确 SceneManager 最小职责。落档失败场景处理。把 mail 系统讨论拆出来作为独立决策。
- **2026-05-16 v2**: 步骤 2(PlayerFrozenComp + 延后 DestroyPlayer)代码层面已落,**深查 Kafka 订阅链路时发现新问题** → 加 §10。
- **2026-05-16 v3**: #27(proto 重新生成)+ #28(topic 订阅接线)代码层面落地;打开 #29(业务系统加 Frozen 检查)时发现需要先做分类决策才能改 17 个文件 → 落档 §11 分类指南,#29 留给下次会话基于指南实施。
