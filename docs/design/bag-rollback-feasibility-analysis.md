# Bag 回档可行性分析(per-service rollback)

> **⚠️ 状态说明(2026-09-20):本文与它指向的 `cross-zone-readiness-audit.md §3 三件套` 都已是历史方案,正文保留为历史,不得据此排期。**
> - 「三件套」要修补的 Kafka 搬数据链(`player_migrate` / `player_migrate_ack` / `CrossZoneReaper`)已在跨 zone 传送阶段 3(2026-09-18)整条删除;跨 zone 不再经 `PlayerAllData` 传输数据,而是重定向 + 目标 zone 从盘上直接加载。现状以 [`cross-zone-scene-travel.md`](cross-zone-scene-travel.md) CZ-1 / §11.3 为准。
> - 「bag 持久化是跨 zone 硬阻塞」的前提已不成立:bag 已接进普通存盘链(`player_database_loader.cpp` 的 `bag_marshal::Marshal / Unmarshal`)。
> - 下文「步骤 1 → 步骤 2(Frozen)→ 步骤 3(ACK + reaper)→ 步骤 4 → 才能做 #11」的阻塞链随之作废:步骤 3 已无对应物,#11 不再被它阻塞。v1 的 §1–§7 调研只作为当时代码事实的记录。

> **状态**: v2 — 2026-05-16(被 cross-zone-readiness-audit.md 取代为权威方案)
> **历史定位**: v1 把 bag 持久化定义为「回档前置」。v2 摸跨 zone 链路后发现这是「跨 zone 能否玩」的硬阻塞,工作量和优先级都被低估。
> **当前定位**: **本文档保留为对 bag 持久化方案的子专题分析**,但**实施方案以 [`cross-zone-readiness-audit.md §3 三件套`](cross-zone-readiness-audit.md) 为准**。

## ⚠️ v2 修正

v1 把 bag 持久化定义为「**回档前置条件**」(任务 #21 阻塞 #11)。这低估了它的影响范围。

**v2 真实定位**:bag 持久化是「**跨 zone 能否玩**」的硬阻塞,不只是回档前置。理由:
- 跨 zone 链路通过 `PlayerAllData` 传输,proto 里没 bag,跨一次丢一次
- 正常退出登录(`HandleExitGameNode`)走相同 7 组件路径,**也丢 bag**
- 重启 / 节点关停同样路径,**也丢 bag**

详细审计见 `cross-zone-readiness-audit.md`。

### 方案变更

v1 的方案 A「把 BagComp 加到 player_database」被 **取代** 为:

> **加 `BagAllData` 作为 `PlayerAllData` 的独立子 message**,平级于 `player_database_data`。
>
> 不挤进 player_database 里 —— player_database 已经 7 个组件,bag 是「集合型数据」,跟「ECS 组件型」拆开 schema 更清晰。

```proto
// 旧方案 A(已废弃)
message player_database {
  ...
  CurrencyComp currency = 8;
  BagComp bag = 10;          // ❌ 不这么做
}

// 新方案(以此为准)
message PlayerAllData {
  player_database player_database_data = 2;
  player_database_1 player_database_1_data = 3;
  BagAllData bag_data = 4;        // ✅ 平级独立子 message
  QuestAllData quest_data = 5;
  MailAllData mail_data = 6;      // 见 cross-zone-readiness-audit.md §3.3,可能改走独立 Go 服务
}
```

### 工作量更新

v1 估计:1 周(只算 proto + Marshal)
**v2 实际**:1-2 周(proto + 双向 Marshal + bag itemRegistry 序列化 schema + 跨 zone 链路完整性测试 + 重启 / 退出场景回归)

**前置阻塞**:产品 / 策划定 ItemEntry schema(强化等级 / 词条 / 宝石镶嵌 等字段)。

### 任务对应

- 任务 #21 重新表述:「**给 bag 加持久化(走 PlayerAllData.bag_data 路径)**」
- 任务 #21 不再单独阻塞 #11 —— 完整阻塞链是:**步骤 1(bag/quest 加 PlayerAllData) → 步骤 2(Frozen 状态) → 步骤 3(ACK + reaper) → 步骤 4(测试)→ 然后才能做 #11 的字段过滤**

---

## 以下保留 v1 调研内容作为历史参考

> v1 的下面的 §1-§7 内容,在跨 zone 视角下仍然全部有效(代码事实没变),只是结论被 v2 上面那部分**修正了优先级和实施方案**。


---

## TL;DR(给客服总监 / 架构师 5 分钟看完)

| 问题 | 答案 |
|---|---|
| 客服需求「只回档背包」目前能做吗? | ❌ **做不了**。但更糟的是 ↓ |
| 那「整人回档」能恢复背包吗? | ❌ **也不能**。bag 数据**完全没有持久化路径**,快照里就没有这部分内容 |
| 现在玩家背包数据怎么存的? | **只在 C++ scene 节点的内存里**(`entt::registry`)。重启 / 玩家下线就丢 |
| 这是 bug 吗? | 不是,是当前阶段的开发态。但说明 bag 模块**没到生产可用状态** |
| 修复路径? | 见 §4 三个方案,工作量从 1 周到 4-6 周不等 |

**核心结论**:`#11 ExportPlayerData / ImportPlayerData RPC` 这个任务**前置条件不满足**。在 bag 持久化路径未建立前,讨论 per-service rollback 是空中楼阁。**先把 bag 存起来,再谈回档**。

---

## 1. 数据架构现状(直接证据)

### 1.1 player_database proto(`proto/common/database/mysql_database_table.proto:90-107`)

整个 player 持久化 proto 只有 7 个组件字段:

```proto
message player_database {
  uint64 player_id = 1;
  Transform transform = 2;
  PlayerUint64Comp uint64_pb_component = 3;
  PlayerSkillListComp skill_list = 4;
  PlayerUint32Comp uint32_pb_component = 5;
  BaseAttributesComp derived_attributes_component = 6;
  LevelComp level_component = 7;
  CurrencyComp currency = 8;
  PlayerStressTestProbe stress_test_probe = 9;
}
```

**没有 bag 字段**。也没有 `BagComp` / `InventoryComp` / `Items` 之类的引用。

### 1.2 Marshal/Unmarshal(`cpp/libs/services/scene/player/system/player_database_loader.cpp`)

```cpp
void PlayerDatabaseMessageFieldsUnmarshal(entt::entity player, const player_database& message){
    tlsEcs.actorRegistry.emplace<Transform>(player, message.transform());
    tlsEcs.actorRegistry.emplace<PlayerUint64Comp>(player, message.uint64_pb_component());
    tlsEcs.actorRegistry.emplace<PlayerSkillListComp>(player, message.skill_list());
    PlayerSkillSystem::SanitizeSkillList(player);
    tlsEcs.actorRegistry.emplace<PlayerUint32Comp>(player, message.uint32_pb_component());
    tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player, message.derived_attributes_component());
    tlsEcs.actorRegistry.emplace<LevelComp>(player, message.level_component());
    tlsEcs.actorRegistry.emplace<CurrencyComp>(player, message.currency());
}
```

**只装载 7 个组件**。bag 的内存数据(`Bag` 类、`ItemsMap items_`、`itemRegistry_`)**不在 Marshal/Unmarshal 路径内**。

### 1.3 player_database_1 是空壳(`cpp/libs/services/scene/player/system/player_database_1_loader.cpp`)

```cpp
void PlayerDatabase1MessageFieldsUnmarshal(entt::entity player, const player_database_1& message){
}
void PlayerDatabase1MessageFieldsMarshal(entt::entity player, player_database_1& message){
}
```

**两个函数体都是空的**。`player_database_1` proto 只声明了一个 `PlayerStressTestProbe` 字段,根本不给业务用。

### 1.4 Bag 类的内存形态(`cpp/libs/modules/bag/bag_system.h:39-109`)

```cpp
class Bag {
private:
    entt::entity entity_{};
    ItemsMap items_{};                  // unordered_map<Guid, entt::entity>
    PosMap pos_{};
    uint32_t type_{};
    std::size_t capacity_{kDefaultCapacity};
    entt::registry itemRegistry_{};     // 独立 ECS registry,每个 item 是一个 entity
    Guid player_guid_{kInvalidGuid};
};
```

**`itemRegistry_` 是独立的 entt registry,完全在内存里**。没有任何序列化方法(`SaveTo` / `LoadFrom` / `ToProto`)。

### 1.5 transaction_log 的副作用

bag 操作**有写 transaction_log**(`bag_service.cpp:64/97` 调 `LogItemCreate / LogItemDestroy`)。这意味着:
- **道具事件流是有的**(`item_uuid` + `transaction_log`)
- 但**道具状态是没有的**(没有「玩家 P 当前持有 item_uuid=X」的快照)

理论上可以**用事件流重建状态**(像 event sourcing),但这需要从首日开始的全部事件,且要写 reducer 逻辑。**不是当前回档机制的工作范围**。

---

## 2. 当前回档行为的真实结果

按 §1 的事实推断,当前 `RollbackPlayer` RPC 实际能恢复什么:

| 数据 | 在快照里? | 能回档吗? |
|---|---|---|
| `Transform`(位置)| ✅ 是 | ✅ 能 |
| `CurrencyComp`(货币)| ✅ 是 | ✅ 能 |
| `LevelComp`(等级)| ✅ 是 | ✅ 能 |
| `SkillList`(技能)| ✅ 是 | ✅ 能 |
| `PlayerUint64Comp` / `PlayerUint32Comp`(键值杂物袋)| ✅ 是 | ✅ 能 |
| `BaseAttributesComp`(派生属性)| ✅ 是 | ✅ 能 |
| **背包道具** | ❌ **不在** | ❌ **不能** |
| **任务进度** | ❌ 不在(`player_database` 也无 quest 字段)| ❌ 不能 |
| **邮件** | ❌ 不在 | ❌ 不能 |
| 公会数据 | 独立 Go 服务 + 独立 Redis,不走 player blob | 不归这条路径管 |
| 好友数据 | 独立 Go 服务 + 独立 Redis,不走 player blob | 不归这条路径管 |

**严肃结论**:当前的「整人回档」实际上是「**整属性回档**」,只能回货币 / 等级 / 技能 / 位置这类**ECS 组件型数据**。背包 / 任务 / 邮件这类**集合型数据**(数量不定的子实体集合)既没存进去,也回不来。

这跟 `docs/design/single_player_rollback.md` 描述的「玩家所有数据回档」愿景**有显著 gap**。

---

## 3. 为什么会这样

不是 bug,是开发态选择。推断的原因:

1. **bag 是 C++ ECS 的 `entt::registry`**,每个 item 是独立 entity。要持久化得遍历整个 itemRegistry_、把每个 item 的 ItemComp / 堆叠状态 / 位置 / 装备槽 都序列化 —— 需要专门写 schema
2. **proto 模型偏向「定型组件」**(每种组件一个固定结构),不擅长表达「N 个不定数量的子对象」
3. **当前 player_database 设计可能源自早期单机 / 测试架构**,没考虑大型物品集合
4. **transaction_log 提供了事件流,可能架构上想走 event sourcing**(从事件流重放重建状态),但 reducer 没写

不管哪种原因,**结果是 bag 没存**。

---

## 4. 三个修复方案

### 方案 A:加 BagComp 到 player_database(中工作量,推荐)

**做法**:
1. 新增 proto component `BagComp`,字段如:
   ```proto
   message ItemEntry {
     uint64 item_uuid = 1;        // SnowFlake guid
     uint32 config_id = 2;
     uint32 stack_size = 3;
     uint32 pos = 4;              // 位置索引
     uint32 bag_type = 5;         // EnumBagType
   }
   message BagComp {
     repeated ItemEntry items = 1;
   }
   ```
2. 在 `player_database` proto 加 `BagComp bag = 10;`
3. 在 `player_database_loader.cpp` 的 Marshal/Unmarshal 加 bag 序列化(遍历 `itemRegistry_` → 写 ItemEntry / 反向 emplace 回 entt registry)
4. 跑一次完整链路测试:登录 → 加道具 → 退出 → 重登 → 验证道具还在

**工作量**: ~1 周(proto 改动 + Marshal 实现 + 测试)

**之后 #11 怎么做**:bag 进了 player_database 后,**整人回档自动就能回背包**(因为快照存的是整个 player_database blob)。partial restore(只回背包不回任务)可以走方案 A':在 `data_service/internal/logic/rollback_logic.go` 加 proto 字段级过滤,~2-3 天

**风险**:
- ItemEntry 需要包含的字段可能不止上面 5 个(buff、强化等级、附魔等),要先定 schema
- 如果道具数量极多(几千件),整 blob 序列化有性能问题,但 MMO 单玩家通常 < 1000,可接受
- 改 Marshal 链路要重测登录 / 退出 / 跨服切场景全流程

### 方案 B:bag 独立 Go 服务(大工作量,过度设计)

参照 `go/guild/` / `go/friend/` 的形态,新建 `go/bag/` 独立服务,自己存 MySQL/Redis。

**工作量**: 4-6 周(新服务 + ECS 改 RPC 调用 + 跨服访问 + 测试)

**为什么不推荐**:
- bag 是**战斗中高频访问**的(消耗药水、技能代价、装备切换),走 RPC 会引入 2-20ms 延迟,跟 `single_player_rollback.md §Q6 的货币论述`(「放 C++ 玩家实体上,Go 只管持久化」)同理 —— **bag 也应该留在 C++,只是把持久化补上**
- 不必要的服务拆分,违背「先简单做对,再考虑拆分」原则

### 方案 C:event sourcing 从 transaction_log 重建(高难度,创新方案)

每次玩家登录时,从 `transaction_log` 拉取该玩家的全部 `TX_TRADE` / `TX_SYSTEM_GRANT` / `TX_ITEM_DESTROY` 等事件,reduce 出当前 bag 状态。

**工作量**: 不可估(需要写 reducer + 索引优化 + 边界处理)

**为什么不推荐**:
- transaction_log 设计为审计 + 异常追溯,不是状态源
- 玩家在线 1 年后事件几万条,登录重放慢
- 边界 case 多(stack 合并、位置交换、装备穿脱、丢弃地上)

---

## 5. 推荐路径

```
本会话:本文档落档 ✅
       ↓
下次会话:产品 / 架构师决策(关键问题见 §6)
       ↓
[若批准方案 A]
P1 会话:加 BagComp proto + 改 Marshal/Unmarshal + 测试登录持久化
P2 会话:加 quest 持久化(同样的模式,quest 也没存)
P3 会话:加 mail 持久化(独立 Go 服务还是 C++ Component,需另议)
P4 会话:在 data_service/rollback_logic.go 加 proto 字段级 partial restore
       → 此时 #11 真正可用,客服可以「只回档背包不回档任务」
```

**总工作量从 bag 起算约 3-4 周**,但每一步都是可独立合并的、有立竿见影价值的(就算只做 bag 持久化不做 partial,**也立刻让背包数据在重启 / 跨服切换后不丢**,这是当前的硬伤)。

---

## 6. 给决策者的关键问题

在动方案 A 之前,需要回答:

1. **bag 数据丢失目前是不是 bug?**
   - 玩家登录 → 拿道具 → 退出 → 重登,道具是否还在?
   - 如果是,那「bag 没持久化」是更严重的生产 bug,优先级应该比 #11 还高
   - 如果道具靠某种我没找到的路径(可能是 `PlayerUint64Comp` 当 Map 用?)其实存了,那分析得修正
2. **客服需求时间线**
   - 「只回档背包不回档任务」是已发生的工单 / 即将上线的需求 / 长期愿景?
   - 如果是 6 个月内的需求,方案 A 来得及
   - 如果是下周需要,只能临时走客服 GM 工具(查 `transaction_log` 手动补单)
3. **bag schema 谁定?**
   - ItemEntry 至少要包含哪些字段?
   - 装备的强化等级 / 词条 / 宝石镶嵌 等数据放哪?
   - 这是产品 + 系统策划的事,AI 拍不了

---

## 7. 给本任务(#11)的当前裁定

**任务 #11 状态**:**前置条件不满足,无法在当前数据架构下完成**。

降级 / 拆分:
- 不能继续标 in_progress,改回 pending 加注「**前置条件 = bag 持久化方案 A**」
- 同时新建任务:**#21 给 bag 加持久化(方案 A)** —— 这是真正阻塞 #11 的事
- 等 #21 完成后,#11 的工作量会从「写 6 个 Export/Import RPC」缩成「在 data_service 加 proto 字段过滤 ~2-3 天」

---

## 8. Changelog

- **2026-05-15 v1**: 初版。直接读 C++ 代码确认 bag 完全无持久化路径,提出方案 A/B/C,推荐方案 A。把 #11 的真实形态落档。
