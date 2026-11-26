# 装备流派方案预设（一键多套换装 + 付费扩展套位）完整设计方案 V1

> **上位依据**：
> - 策划总红线：[project-vision.md](project-vision.md)
> - 经济与 7:3 商业化总纲：[equipment-economy-charter.md](equipment-economy-charter.md)
> - 装备八槽与强化镶嵌技术设计：[equipment-eight-slot-implementation.md](../design/equipment-eight-slot-implementation.md)、[equipment-enhance-inlay.md](../design/equipment-enhance-inlay.md)
> - 既有对标实现（布局与实例分层、乐观锁、幂等键）：
>   - 技能战法配装方案：`proto/common/component/player_skill_comp.proto`（`SkillLoadoutScheme`）、`proto/scene/player_skill.proto`（`SaveSkillLoadout` / `SwitchSkillLoadout`）
>   - 属性加点付费方案：`proto/common/component/player_attribute_comp.proto`（`AttributeScheme`）、`cpp/libs/services/scene/player/system/player_attribute.cpp`（`CreateScheme`）
> - 配套 UI 交接单：[equipment-loadout-presets-ui-handoff.md](ui-handoff/equipment-loadout-presets-ui-handoff.md)
>
> **状态**：已定稿，可供独立开发窗口按本规格全链路落地（配表 + 协议 + C++ `scene` + Unity 客户端 + 单测），并配套异机 Codex 制作纯功能 UI 切片。

---

## 1. 需求分析

### 1.1 为什么要做「装备流派方案预设」
《五行奇谈》已落地 **阶段二每周 150 份副套材料池**、`3410` **流派副套继承符**（同部位无损继承主套 `+10/+11` 强化与保底至副套，目标转绑定）、**3 套战法构筑方案（`SkillLoadout`）** 与 **多套属性加点方案（`AttributeScheme`）**。
目前最后缺失的一环是**装备侧的一键多套流派切换**：
- 玩家用 `3410` 养出「PVE 暴伤输出套」「PVP 高速控场套」「帮战减伤生存套」后，当前只能回背包逐件手动点 8 个装备槽切换，体验割裂；
- 若支持**每套方案记录一套完整的 8 槽装备引用（一套一个），一键切换流派，且第 2～5 套方案位支持付费扩展解锁**，就能把「零氪白嫖首套 → 阶段二养成副套 → 买 `3410` 继承高强 → 买方案位一键切流派 → 联动战法与加点方案」串成一条极强可玩性、且 100% 符合官方 7:3 便利型变现（零战力售卖）的长线循环。

### 1.2 核心需求清单
1. **“一套一个”的真实装备引用**：每套方案（`scheme_id = 1..5`）独立记录 `EquipSlot.xlsx` 全部 8 个正式装备槽位（槽位 `3..10`：武器/帽子/衣服/鞋子/首饰一/首饰二/法宝一/法宝二）的 `slot -> item_guid` 映射；未穿戴的副套装备仍真实存在于玩家背包 `kInventory` 中，不凭空复制装备、不造隐藏仓库。
2. **共享与独立兼容**：多套方案可以完全独立 8 件，也可以共享部分件（例如两套共用同一把 `+11` 武器，只换衣服/鞋子/双首饰/双法宝）。
3. **每套方案位付费解锁（严守零氪底线 + iOS IAP 合规）**：
   - 默认免费开放 **第 1 套**；
   - 第 **2～5 套** 逐套解锁（上限 5 套），支持 **消耗「装备方案扩容符（`3411`）」优先，无符则直接扣除对应档位灵石**；
   - 第 2 套允许零氪用活动/月卡日常积累的**绑定灵石（`kCurrencyBindDiamond`）或非绑灵石（`kCurrencyDiamond`）**二选一解锁（保障零氪尊严），第 3～5 套使用非绑灵石（`kCurrencyDiamond`）或官方商城直售的 `3411 装备方案扩容符`。
4. **一键切换 + 可选跨系统流派联动**：
   - 切换方案时，服务端原子完成 8 槽批量换装并仅重算 1 次属性、推送完整背包/装备快照；
   - 每套方案可选择性关联一套 `linked_skill_scheme_id`（`0` 不联动，`1..3` 对应 `SkillLoadout`）和一套 `linked_attribute_scheme_id`（`0` 不联动，`>0` 对应 `AttributeScheme`），玩家开启「同步切换战法/加点」后，一键切完一整套流派。
5. **自定义方案名与流派标签**：每套方案支持 `role_tag`（`0 默认 / 1 输出 / 2 控场 / 3 辅助 / 4 生存`）与自定义短名（1～12 字符，过敏感词过滤）。

---

## 2. 设计分析

### 2.1 方案位付费阶梯（零属性加成、纯便利变现）
| 方案位（`scheme_id`） | 默认名称 | 解锁方式 | 灵石价格（10 灵石 = ¥1） | 可替代道具 | 零氪可达性 |
|---:|---|---|---:|---|---|
| `1` | 方案一 | 创角免费自带 | `0` | — | 免费 |
| `2` | 方案二 | 绑灵石 / 灵石 / 扩容符 | `60`（≈ ¥6） | `3411 装备方案扩容符 ×1` | 可用绑定灵石免费肝出或直买 |
| `3` | 方案三 | 灵石 / 扩容符 | `120`（≈ ¥12） | `3411 装备方案扩容符 ×1` | 官方小额便利直售 |
| `4` | 方案四 | 灵石 / 扩容符 | `180`（≈ ¥18） | `3411 装备方案扩容符 ×1` | 官方小额便利直售 |
| `5` | 方案五 | 灵石 / 扩容符 | `240`（≈ ¥24） | `3411 装备方案扩容符 ×1` | 官方小额便利直售 |

> **为什么这样定价**：
> - 与现有官方 7:3 定价体系（解封剥离符 ¥6、流派副套继承符 ¥12、仙途轻享卡 ¥18）保持同一阶梯节奏；
> - 全程不走站外支付，购买 `kCurrencyDiamond`（灵石）或商城 SKU `3411` 全部走官方合规收银台 / iOS IAP。

### 2.2 持久化数据结构设计（严格遵守 `OptionPersistGroup` 与 `OptionPersistFrozen`）
- **严禁修改 `BagAllData` / `ItemEntry`**（已标 `OptionPersistFrozen`）。
- 新增独立持久组件文件：`proto/common/component/player_equip_loadout_comp.proto`：
  ```protobuf
  syntax = "proto3";
  option go_package = "common/component";

  message EquipLoadoutSlotBinding {
    uint32 slot = 1;       // 对应 EquipSlot.xlsx 正式槽位号（3..10）
    uint64 item_guid = 2;  // 永久物品实例 guid（0 表示该槽位刻意留空/不指定）
  }

  message EquipLoadoutScheme {
    uint32 scheme_id = 1;                         // 1..5 显式方案号
    string name = 2;                              // UTF-8 1..12 字符（空则显示默认「方案一..五」）
    uint32 role_tag = 3;                          // 0默认 / 1输出 / 2控场 / 3辅助 / 4生存
    repeated EquipLoadoutSlotBinding slots = 4;   // 按 slot 升序，严禁靠下标承载槽位语义
    uint32 linked_skill_scheme_id = 5;            // 0 = 不联动；1..3 = 联动 SkillLoadoutScheme
    uint32 linked_attribute_scheme_id = 6;        // 0 = 不联动；>0 = 联动 AttributeScheme
  }

  message PlayerEquipLoadoutComp {
    uint32 unlocked_scheme_count = 1;             // 已解锁套数（0 兼容按 1 处理，上限 5）
    uint32 active_scheme_id = 2;                  // 当前激活方案号（0 兼容按 1 处理，1..unlocked）
    repeated EquipLoadoutScheme schemes = 3;      // 已解锁方案明细（按 scheme_id 升序）
    uint64 state_version = 4;                     // 乐观锁版本号（0 按 1 计）
    string last_request_id = 5;                   // 最近一次保存/切换幂等键
    uint64 last_base_version = 6;                 // 最近一次请求的期望版本
    uint32 last_scheme_id = 7;                    // 最近一次操作的目标方案号
  }
  ```
- 挂载到 `proto/common/database/mysql_database_table.proto` 的 `player_database` 末尾**下一个空闲字段号 `36`**：
  - `PlayerEquipLoadoutComp equip_loadout_component = 36 [(OptionPersistGroup) = 3]; // GROWTH`
  - 与 `skill_list`、`attribute_component`、`craft_component` 同属 `GROWTH` 组，存盘时与 `bag_component` / `currency` / `client_request_ledger` 同事务提交（满足不变量 I3）。
  - 加载时无条件挂组件（旧存档缺字段合法，默认 `unlocked_scheme_count=1, active_scheme_id=1`）；存盘缺组件 fail-closed 拒存。

### 2.3 配表与错误码增量
1. **`data/Item.xlsx`**（遵守永久 ID 追加原则，仅末尾追加 1 行）：
   - `3411`：**装备方案扩容符**（`max_stack_size=99`，不可战斗使用，非装备；官方商城可直售，解锁额外装备流派方案位时优先消耗）。
2. **`data/tip/Tip.xlsx`**（`equip_error` 域末尾追加，单轴发号，严禁手写数字）：
   - `kEquipLoadoutSchemeInvalid`（方案号越界或未解锁）
   - `kEquipLoadoutMaxUnlocked`（已达最大 5 套方案上限）
   - `kEquipLoadoutVersionChanged`（方案状态版本已变化，请刷新重试）
   - `kEquipLoadoutDiamondNotEnough`（灵石不足且背包无装备方案扩容符）
   - `kEquipLoadoutNameInvalid`（方案名称长度超限或包含敏感词）
   - `kEquipLoadoutDuplicateItem`（同一件装备不可重复配置到多个槽位）
   - `kEquipLoadoutSlotUnavailable`（槽位或装备部位不匹配 / 等级职业不满足 / 背包空间不足以卸下多余装备）
   - `kEquipLoadoutUnlockCountChanged`（已解锁套数已变化，防止连点连买两套）

### 2.4 客户端协议设计（追加至 `proto/scene/player_bag.proto` 末尾）
> **严格遵守 `SceneBagClientPlayer` 方法追加纪律**：只可在 `service SceneBagClientPlayer` 末尾追加，不改变任何已有 RPC 的 method index！

新增 4 条 RPC：
1. `GetEquipLoadouts(GetEquipLoadoutsRequest) returns (GetEquipLoadoutsResponse)`
   - 只读快照：返回 `unlocked_scheme_count`、`max_scheme_count=5`、`active_scheme_id`、`state_version`、每套 `EquipLoadoutSchemeInfo`（含每个槽位对应的实时状态枚举 `EQUIPPED` / `IN_BAG` / `MISSING_OR_STORED` 与道具简要预览）、下一套解锁价格（`next_unlock_cost_diamond`、`next_unlock_allow_bind_diamond`、`unlock_item_config_id=3411`、`unlock_item_owned`）以及当前可用的战法/加点方案摘要供联动选择。
2. `SaveEquipLoadout(SaveEquipLoadoutRequest) returns (SaveEquipLoadoutResponse)`
   - 参数：`scheme_id`、`name`、`role_tag`、`repeated EquipLoadoutSlotBinding slots`（或 `capture_current_equipped = true` 一键保存当前身上全套装备！）、`linked_skill_scheme_id`、`linked_attribute_scheme_id`、`state_version`、`request_id`。
   - 校验：方案已解锁、非战斗/非冻结、名称合规、`item_guid` 唯一且归属本人背包/装备栏、部位与 `EquipSlotTable` 匹配。
3. `SwitchEquipLoadout(SwitchEquipLoadoutRequest) returns (SwitchEquipLoadoutResponse)`
   - 参数：`scheme_id`、`apply_linked_schemes`（是否同时切换关联战法/加点方案）、`state_version`、`request_id`。
   - 执行：原子批量换装（详见第 4 节漏洞防线），回传最新全量 `BagInfo bag`、`BagInfo equipment`、`EquipLoadoutSnapshot snapshot`、`repeated uint32 missing_slots`（已缺失被跳过的槽位）与 `repeated string linked_notices`（如加点冷却中未切提示）。
4. `UnlockEquipLoadout(UnlockEquipLoadoutRequest) returns (UnlockEquipLoadoutResponse)`
   - 参数：`client_request_id`（`uint64` 持久化幂等键，走 `ClientRequestLedgerComp` 防双扣钱！）、`expected_unlocked_count`、`use_bind_diamond_first`（仅第 2 套有效）。
   - 执行：校验 `unlocked_scheme_count < 5` 与 `expected_unlocked_count` 一致 → 优先扣 `3411 × 1`（`BagService::RemoveItemsExact`），无符则扣对应灵石（第 2 套可选先扣 `kCurrencyBindDiamond` 再补 `kCurrencyDiamond`，第 3～5 套仅扣 `kCurrencyDiamond`）→ `unlocked_scheme_count++` 并初始化空方案 → 写入 `ClientRequestLedgerComp` 并**等待真实落盘 `durable = true` 后才返回成功**（与 `CommitEquipmentCraft` / `ClaimEquipmentMaterialReward` 同级资金安全标准！）。

---

## 3. 思想分析

1. **“实例与布局分离”是一切安全的根基**：方案只存「哪一槽指向哪一件真实装备的 `guid`」，永远不复制 `ItemComp`、永远不另起第二套隐藏背包容器。装备无论在身上还是在背包，永远只有 1 份真实实体，回档、交易、强化、解封、查账全部天然一致。
2. **用 `3410` + 装备流派方案形成“双赢变现闭环”**：
   - 不卖属性、不卖宝石、不卖战力；
   - 玩家先在玩法里自然养出第二套不同词条的副套，再用 `3410` 流派副套继承符省去重复冲 `+10/+11` 的枯燥，再花 ¥6/¥12 买方案位把「装备 + 战法 + 加点」一键合体切换——买得心甘情愿、用一次爽一次、零氪也完全能通过绑定灵石拿到双套核心体验。
3. **原子原位互换，彻底消灭“背包满导致换装失败”的挫败感**：同槽位把旧装备换下来、把新装备穿上去时，两者在 `kEquipment[slot]` 与 `kInventory[pos]` 直接原位互换，**即使玩家背包 0 空格也能把 8 件对 8 件整套丝滑切过去**！

---

## 4. 漏洞分析与强制防线

| # | 潜在漏洞 / 风险 | 后果 | 强制防护措施（开发窗口必做 + 单测必覆盖） |
|---|---|---|---|
| 1 | **连点/弱网重发重复扣灵石买两套方案** | 玩家花 2 份钱或状态错乱 | `UnlockEquipLoadout` 必带 `client_request_id`（持久化至 `ClientRequestLedgerComp` + `ClientRequestKind::kEquipLoadoutUnlock`）与 `expected_unlocked_count`，同键重放原样回 `replayed=true`，客户端持久化恢复（`DurableRequestFileStore`）。 |
| 2 | **同一件装备 `guid` 填进两个槽位（如双首饰/双法宝）** | 刷双份属性或换装崩溃 | `SaveEquipLoadout` 与 `SwitchEquipLoadout` 均做 `std::unordered_set<Guid>` 槽位内去重校验，重复直接拒 `kEquipLoadoutDuplicateItem`。 |
| 3 | **批量换装中途失败导致“穿一半脱一半”或装备丢失** | 装备蒸发或半套脏态 | 切换分两阶段：**Stage 1 只读校验与计划构建**（检查战斗/冻结、每件 `guid` 所在位置、等级/职业/槽位匹配、未被 `ItemHoldComp` 占住、净需空背包格 `max(0, pureUnequip - pureEquip) <= FreeSlotCount()`）；**Stage 2 全或无原子提交**（先原位互换同槽对，再执行纯卸下与纯穿上），最后只调用 1 次 `PlayerAttributeSystem::Recalculate(kEquipmentChanged)`。 |
| 4 | **方案内某件装备已被交易/分解/存仓库导致整套无法切换** | 整套方案报废卡死 | 允许部分缺件容错切换：找不到的 `guid` 记入回包 `missing_slots` 并保持该槽原状不动，其余有效槽位正常切换，客户端弹窗/状态条明确提示缺失槽位引导重新保存。 |
| 5 | **战斗中、跨节点交接冻结中切装或偷换属性** | 破坏回合制公平与跨节点一致性 | 复用 `PlayerEquipSystem::CheckWritable(player)`（`!IsPlayerMutable` → `kEquipFrozen`，`IsInBattle` → `kEquipInBattle`），战斗中与冻结中一律拒绝写入。 |
| 6 | **同方案联动加点处于切换冷却导致装备也切不成** | 体验割裂 | 先原子完成装备切换；联动切换 `SkillLoadout` / `AttributeScheme` 失败（如加点冷却中）不回滚已成功的装备切换，在 `linked_notices` 中明文提示未联动原因。 |
| 7 | **往返存档丢失方案或破坏冻结消息 `BagAllData`** | 下线/跨区丢方案或触发混版本故障 | 独立 `PlayerEquipLoadoutComp` 落 `player_database = 36 [(OptionPersistGroup) = 3]`，绝不改 `BagAllData`；`player_database_loader.cpp` 无条件加载组件且存盘缺组件 fail-closed，必补 `persist_round_trip_test` 往返单测。 |
| 8 | **玩家手滑把已编入副套方案的主力装备拿去上架或解封** | 误卖副套核心件 | 客户端在背包/装备列表展示「方案 N 使用中」角标，上架寄售/当面交易/解封剥离前检查是否被方案引用并弹出二次确认提示。 |
| 9 | **方案自定义名称注入富文本或敏感词** | UI 错乱或内容合规风险 | 服务端按 UTF-8 长度 1..12 校验并过现有敏感词检测；客户端 `TMP_InputField` 与所有展示文本强制 `richText = false`。 |

---

## 5. 步骤分析与执行步骤（给新开发窗口的落地顺序）

1. **配表与协议层**
   - `data/Item.xlsx` 追加 `3411`（装备方案扩容符）；`data/tip/Tip.xlsx` 追加 8 条 `kEquipLoadout*` 提示码。
   - 新增 `proto/common/component/player_equip_loadout_comp.proto`；在 `proto/common/database/mysql_database_table.proto` 末尾追加 `equip_loadout_component = 36 [(OptionPersistGroup) = 3]`；在 `proto/common/component/client_request_ledger_comp.proto` 新增 `kEquipLoadoutUnlock` 枚举/回执字段；在 `proto/scene/player_bag.proto` 末尾追加 4 条 RPC 与消息。
   - 服务端执行 `dev.bat gen` + `py tools/data_table_exporter/tools/gen_schema_index.py`；客户端执行 `tools/gen_proto.ps1` + `tools/gen_messageids.ps1` + `tools/sync_tables.ps1`。
2. **C++ `scene` 持久化与核心域逻辑**
   - `player_database_loader.cpp` 与 `player_lifecycle.cpp` 接入 `PlayerEquipLoadoutComp` 的加载、存盘、fail-closed 校验；同步扩展 `client_request_ledger.cpp` 支持 `kEquipLoadoutUnlock`。
   - 新增纯规则头 `cpp/libs/services/scene/player/system/equip_loadout_rules.h`（价格表、槽位校验、原位互换计划纯函数，便于单测）与系统实现 `player_equip_loadout.h/.cpp`（`Get` / `Save` / `Switch` / `Unlock`）。
   - `cpp/nodes/scene/handler/rpc/player/player_bag_handler.cpp` 接入 4 个 RPC Handler。
3. **C++ GTest 验收**
   - 新增 `cpp/tests/bag_test/player_equip_loadout_test.cpp` 覆盖：0 空格 8v8 原位原子互换、背包不足纯卸下拒绝、重复 `guid` 拒绝、缺件跳过并回 `missing_slots`、第 2 套绑定灵石/扩容符解锁与第 3～5 套非绑灵石阶梯解锁、同 `client_request_id` 幂等重放不双扣钱、战斗/冻结态拦截、联动战法与加点方案（含加点冷却容错）、以及 `persist_round_trip_test` 存档往返。
   - 编译 `table / proto / rpc / modules / libs/services/scene / bag_test / nodes/scene`（注意 `/m:1` 防 PDB 争用）并全量跑绿。
4. **Unity 客户端协议、纯 C# 模型与 uGUI 接入**
   - 扩展 `PlayerFeaturesClient`（新增 `PlayerFeaturesClient.EquipLoadout.cs`）：封装 4 条 RPC、`DurableRequestFileStore` 持久恢复解锁请求、会话代次防串号与事件通知。
   - 新增纯 C# `EquipLoadoutModel.cs`（脱离 `UnityEngine`）：方案槽位状态汇总、解锁价格展示、被方案引用的装备角标映射（供背包/交易/解封提示复用）。
   - 扩展 `EquipmentWindow`（新增 `EquipmentWindow.Loadout.cs`）：在装备主窗顶栏/侧边增加「流派方案」快捷条与方案管理弹窗（1～5 套切换按钮、一键保存当前穿着、槽位缺件红标、自定义改名/标签、联动战法/加点下拉选择、未解锁套位付费确认弹窗，预留接入 Codex 第二批 `EconomyLoadoutV1` 切片入口）。
   - 在 `CommerceWindow` 上架选中资产与 `EquipmentWindow.Polish` 解封剥离前，增加「该装备正被装备方案 N 引用」温和二次提示。
5. **客户端编译与 NUnit 测试**
   - 新增 `Assets/Tests/EditMode/Battle/EquipLoadoutUiTests.cs` 覆盖模型映射、幂等恢复、引用保护与 RPC 编解码；跑绿 `tools/client_compile_check.ps1` 与 EditMode NUnit 套件。
   - 待 Codex 产出 `designs/equip-loadout-ui-v1/` 切片后，导入 `Assets/Resources/UI/Ugui/EquipLoadoutV1/`（核对 SHA-256、透明通道与唯一 `.meta` GUID）并完成原图还原验收。
