# 装备流派方案预设（方案 A · 7 套流派席位合一：装备 + 战法 + 加点共享）完整设计方案 V2

> **上位依据**：
> - 策划总红线：[project-vision.md](project-vision.md)（好玩优先 / 真不肝 / 小小氪金 0 P2W / 官方 7:3 / 物价稳得住 / 防骗安全 / 零拒审防下架）
> - 经济与 7:3 商业化总纲：[equipment-economy-charter.md](equipment-economy-charter.md)
> - 统一 7 套非永久流派席位规则（唯一权限事实源）：[skill-build-loadout-7slots-v1.md](skill-build-loadout-7slots-v1.md)
> - 装备八槽与强化镶嵌技术设计：[equipment-eight-slot-implementation.md](../design/equipment-eight-slot-implementation.md)、[equipment-enhance-inlay.md](../design/equipment-enhance-inlay.md)
> - 既有对标实现（布局与实例分层、乐观锁、幂等键）：
>   - 统一 7 套流派席位与按月租用：`proto/common/component/player_skill_comp.proto`（`SkillLoadoutScheme`、`rented_extra_slots`、`rent_expires_at_sec`）、`proto/scene/player_skill.proto`（`SaveSkillLoadout` / `SwitchSkillLoadout` / `RentSkillLoadoutSlots`）
>   - 属性加点方案：`proto/common/component/player_attribute_comp.proto`（`AttributeScheme`）、`cpp/libs/services/scene/player/system/player_attribute.cpp`
> - 配套 UI 交接单：[equipment-loadout-presets-ui-handoff.md](ui-handoff/equipment-loadout-presets-ui-handoff.md)、[skill-build-loadout-7slots-ui-handoff.md](ui-handoff/skill-build-loadout-7slots-ui-handoff.md)
>
> **状态（2026-11-26 负责人拍板方案 A）**：已由原 V1「5 套独立买断（1 免费 + 2~5 买断）」升级定稿为 **方案 A「7 套三合一共享流派席位」**，与 [skill-build-loadout-7slots-v1.md](skill-build-loadout-7slots-v1.md) 共享同一份免费/满级/轻享卡/按月租用席位权限，**零重复收费、零新图（复用已落盘 `EquipLoadoutV1` 与 `SkillBuildLoadoutV1` 透明切图）、零新 DB 列、零新 RPC**。

---

## 1. 需求分析

### 1.1 为什么升级为方案 A（7 套三合一共享流派席位）
《五行奇谈》已落地 **阶段二每周 150 份副套材料池**、`3410` **流派副套继承符**（同部位无损继承主套 `+10/+11` 强化与保底至副套，目标转绑定）、**7 套非永久「战法·加点」流派方案（`SkillLoadout` + `AttributeScheme`）** 与 **战前准备期一键换流派快捷条**。
原 V1 装备流派方案采用「1 套免费 + 第 2~5 套独立买断」，与刚落地的 7 套非永久「战法·加点」体系并存时存在三处明显产品短板：
1. **1 套免费卡死最基础的 PVE / PVP 双套切换**：零氪玩家哪怕打出了第二套装备或新法宝，每次打完试炼进会武仍需手动点 8 个格子换装，违背 [project-vision.md](project-vision.md)「换流派轻快、零氪不卡脖子」红线。
2. **装备页与战法·加点页拆成两份收费，玩家感知为“同一套流派交两份房租”**：若将装备页单独扩至 7 套并另收一份月租，会与仙途轻享卡（¥30/月）、战法·加点按月租用叠加成三份账单，显著拉低口碑与续费意愿。
3. **上限不对齐（装备 5 套 vs 战法·加点 7 套）**：玩家开满 7 套战法·加点后，装备页只能配 5 套，战前备战快捷条也只能点 1..5，体验割裂。

### 1.2 核心需求清单（方案 A）
1. **“一套一个”的真实装备引用（允许跨套共享件）**：每套方案（`scheme_id = 1..7`）独立记录 `EquipSlot.xlsx` 全部 8 个正式装备槽位（槽位 `3..10`：武器/帽子/衣服/鞋子/首饰一/首饰二/法宝一/法宝二）的 `slot -> item_guid` 映射；未穿戴副套装备仍真实存在于玩家背包 `kInventory` 中，不复制装备、不造隐藏仓库；多套方案可共享同一把主武器/核心防具，仅换局部词条装与法宝，**绝不逼玩家肝 7×8 = 56 件独立毕业装**。
2. **7 套三合一共享席位（唯一权限真相源，严守不逼氪与不扒两层皮）**：
   - 装备方案位（1..7）与战法·加点方案位（1..7）**共用同一份有效席位上限**（单一事实源：`PlayerSkillListComp` + `AfkPassComp` + `LevelComp`）：
     - **方案 1～2**：创角永久免费（覆盖日常 PVE + PVP 刚需，零氪 0 卡脖子）；
     - **方案 3**：角色有效等级 ≥ 85 满级自动永久解锁（0 氪长期纯玩最终拥有 **3 套永久免费**，0 打卡、0 额外任务）；
     - **轻享卡附赠 +2 套**：仙途轻享卡（300 灵石/30 天）生效期内自动附赠 +2 套（85 级前 4 套、满级后自带 5 套，不重复收费）；
     - **按套按月租用 +1～+5 套（封顶 7 套）**：统一通过既有 `RentSkillLoadoutSlots` RPC（**30 真实灵石 / 套 / 30 天**，共享统一到期日，剩余上限 180 天，仅扣真实灵石，无自动扣款）一次开通/扩容/续期，**同时生效于「装备 + 战法 + 加点」三合一整套流派**。
   - 废弃原 V1 单独扣费/扣 `3413` 买断装备套位的双轨入口（协议位 `UnlockEquipLoadout` 保留占位不删、服务端 fail-closed 拒回 `kFeatureUnavailable` 杜绝误双扣，且 `3413` 不上架百宝阁）。
3. **到期「只锁不删」与防误卖保护**：
   - 租期或轻享卡到期后，`scheme_id > unlocked` 的已存装备方案（8 槽绑定、名称、标签、联动 ID）**完整保留在存档与快照中供只读预览**，仅禁止保存与切入；
   - 到期时若玩家当前正激活着已锁定的高编号方案，不强行扒装或改回方案一，允许随时切回 `1..unlocked`（切走后须续期/达 85 级/开轻享卡才可再切回）；
   - 背包与交易/解封防误卖保护继续扫描所有已保存的方案（含暂时到期锁定的方案），防止玩家误把已存副套装备卖掉或分解。
4. **一键切换 + 跨系统联动 + 战前 1..7 快捷条**：
   - 切换方案时，服务端原子完成 8 槽批量换装（0 空格 8v8 原位互换）并仅重算 1 次属性、推送完整背包/装备快照；
   - 可选联动 `linked_skill_scheme_id`（`1..unlocked`）与 `linked_attribute_scheme_id`；
   - 装备主窗与全部 6 处战前备战快捷条同步升级至 `1..7` 套，且在装备方案弹窗内即可直接打开统一的 7 套席位租用与轻享卡引导弹窗。

---

## 2. 设计分析

### 2.1 统一 7 套流派席位权限表（装备 + 战法 + 加点三合一）
| 槽位组成 | 套数 | 获取方式 | 周期与归属 |
|---|---:|---|---|
| **永久免费基础位** | **2 套**（方案 1～2） | 创角永久免费 | 永久免费（PVE + PVP 零痛点） |
| **满级长期纯玩奖励位** | **+1 套**（达 85 级满级） | 有效等级 ≥ 85 自动永久解锁 | 永久免费（0 氪满级共 3 套永久免费） |
| **仙途轻享卡附赠位** | **+2 套**（生效期） | 复用既有 `afk_pass_expires_at`（300 灵石/30 天） | 轻享卡有效期内自动生效（满级月卡党直接拥有 5 套） |
| **按套按月租用位** | **+1～+5 套**（总封顶 7 套） | 统一走 `RentSkillLoadoutSlots`：**30 真实灵石 / 套 / 30 天** | 共享同一到期日，最长叠 180 天，到期只锁不删，零自动扣款 |

- **有效解锁套数权威公式（C++ / Java / Python / 客户端同口径，直接复用 `skillloadout::UnlockedSchemes`）**：
  - `freeSchemes = 2 + (effectiveLevel >= 85 ? 1 : 0)`
  - `unlocked = min(7, freeSchemes + (lightCardActive ? 2 : 0) + activeRentedSlots)`
- **五问准入自检（[project-vision.md §二](project-vision.md#二后续所有子文档准入自检写设计前必答-5-问)）**：
  1. **普通零氪玩家**：创角即享 2 套永久免费（PVE + PVP），满级自动 3 套永久免费，0 战力加成、0 打卡、不卡脖子。
  2. **个人开发者收入**：彻底消除“装备和战法扒两层皮”的抵触，将仙途轻享卡（¥30/月）与单套按月租用（¥3/套/月）价值拉满，100% 官方真实灵石直收，符合官方 7:3。
  3. **物价与长期循环**：槽位权限纯绑定不可交易；鼓励玩家基于同一主武器搭配多件副装/法宝形成 5～7 种流派，温和拉动阶段二副套、重铸砂与 `3410` 继承符长线消耗。
  4. **防骗与资产安全**：统一复走 `ClientRequestLedgerComp` 持久化幂等键（`kRentSkillLoadoutSlots`）+ 报价防呆；退役单独 `UnlockEquipLoadout` 扣费路径杜绝误双扣；到期只锁不删且保留防误卖角标。
  5. **商店过审/防下架安全**：全程走既有灵石充值档位 + 游戏内手动灵石开通/续期，无自动扣款订阅、无外链/切支付、无诱导文案、自定义方案名过敏感词检测。

### 2.2 持久化数据结构设计（零新表、零新列、严守 `OptionPersistFrozen`）
- **严禁修改 `BagAllData` / `ItemEntry`**（已标 `OptionPersistFrozen`）。
- **零新增持久化字段**：
  - 方案 1..7 装备引用继续持久化在既有 `player_database.equip_loadout_component = 36 [(OptionPersistGroup) = 3]`（`PlayerEquipLoadoutComp`，`EquipLoadoutScheme.scheme_id` 放宽至 `1..7`，原 `unlocked_scheme_count` 字段保留不再作为独立付费上限）；
  - 7 套席位租用状态继续以 `PlayerSkillListComp.{rented_extra_slots, rent_expires_at_sec}` 与 `AfkPassComp.afk_pass_expires_at` 为唯一持久真相源，双系统零双写分叉。

### 2.3 协议增量（零新 RPC、向后兼容末尾追加字段）
- `proto/scene/player_bag.proto` 的 `EquipLoadoutSnapshot` 末尾追加字段 `14..21`（只读投影统一席位状态，供装备弹窗与战前快捷条直接展示并试算租用报价）：
  - `uint32 free_schemes = 14;`（2 或 3）
  - `uint32 light_card_bonus_schemes = 15;`（0 或 2）
  - `uint32 rented_extra_slots = 16;`（有效租期内的额外租用套数 0..5，到期为 0）
  - `uint64 rent_expires_at_sec = 17;`（统一到期 Unix 秒）
  - `uint64 server_now_sec = 18;`（服务端当前 Unix 秒）
  - `uint64 rent_unit_price_diamond = 19;`（30）
  - `uint32 rent_period_days = 20;`（30）
  - `uint32 rent_max_remaining_days = 21;`（180）
- 既有的 `UnlockEquipLoadout`（`SceneBagClientPlayer` method index 17）保留占位不删除、不改 RPC 序号，服务端统一返回 `REJECTED + kFeatureUnavailable` 并附最新快照（防旧入口重复收费）。

---

## 3. 思想分析

1. **单一真相源消除“双份房租”与状态分裂**：把装备、战法、加点三者的方案槽位权限收口到同一套 `skillloadout::UnlockedSchemes` 规则上，玩家交一份钱同时扩充整套流派席位，服务端也无需维护两套独立的租期、两套补差价账本和两套到期状态。
2. **“实例与布局分离”+ 跨套共享件守住“不肝”底线**：方案只存 `slot -> item_guid` 引用，不复制 `ItemComp`、不造隐藏仓库；允许 7 套方案共享主武器和核心防具，仅替换词条装与法宝，配合 `3410` 继承符与 0 空格原位原子互换，让 7 套页签成为纯粹的策略乐趣而不是 56 件装备的苦力坑。
3. **到期“只锁不删 + 不断当前穿着 + 保留防误卖”守住口碑底线**：不靠惩罚和催费逼续约，玩家随时可停、随时可续，彻底避免“像上班”。

---

## 4. 漏洞分析与强制防线

| # | 潜在漏洞 / 风险 | 后果 | 强制防护措施（单测必覆盖） |
|---|---|---|---|
| 1 | **旧入口 `UnlockEquipLoadout` 与新入口 `RentSkillLoadoutSlots` 并存导致误扣两份钱** | 玩家花冤枉钱或状态打架 | `PlayerEquipLoadoutSystem::Unlock` 统一 fail-closed 拒回 `REJECTED + kFeatureUnavailable`（零扣币、零扣符）；客户端装备弹窗直连统一 `RentSkillLoadoutSlots` 租用弹窗与轻享卡入口。 |
| 2 | **租期或轻享卡到期后已存的第 4～7 套装备方案丢失，或半夜强行扒装切回方案一** | 玩家心血方案丢失引发客诉 | `BuildSnapshotAt` 始终完整输出 `1..7` 方案明细（含 `unlocked = false` 的只读预览）；`EffectiveActiveSchemeId` 保留 `1..7` 当前激活态不强切；`Save`/`Switch` 拦截对锁定方案的写入与切入，允许从锁定态切回 `1..unlocked`，续期后原样复活。 |
| 3 | **到期锁定的方案内装备在背包/集市失去“方案使用中”提醒被玩家手滑卖掉** | 续期后方案整套缺件 | `EquipLoadoutModel.BuildItemReferenceMap` 扫描全部有槽位绑定的已存方案（含到期锁定方案，标签标为 `方案五(锁)`），持续保护副套资产防误卖/误解封。 |
| 4 | **在装备界面直接点租用时，玩家尚未打开过技能界面导致 `SkillBookClient.State == null` 报错** | 装备弹窗内无法直接开通/续期 | `SkillBookClient.RentLoadoutSlots` 解除对 `State.Snapshot.Loadout` 已加载的前置硬阻断（`RentSkillLoadoutSlotsRequest` 仅依赖报价参数与幂等键），成功后触发 `LoadoutSlotsRented` 事件自动刷新 `EquipLoadouts` 与 `Attributes`。 |
| 5 | **同一件装备 `guid` 填进两个槽位 / 批量换装中途失败 / 战斗或冻结中切装** | 刷双份属性或脏态 | 完整保留既有两阶段原子原位互换（0 空格 8v8）、`std::set<Guid>` 去重、缺失槽容错跳过并回 `missing_slots`、`CheckWritable` 战斗/冻结拦截与富文本/敏感词过滤。 |
| 6 | **跨系统联动切战法/加点与升级/月卡状态不同步** | 开完月卡或续租后装备页仍显示锁住 | `GameClient` 监听 `Skills.LoadoutSlotsRented` 与轻享卡变更并自动刷新 `Features.RequestEquipLoadouts()`；服务端 `BuildSnapshotAt` 每次即时按 `AfkPassSystem::IsActive` + `PlayerSkillListComp` + `LevelComp` 实时计算。 |

---

## 5. 步骤分析与执行步骤

1. **文档落盘**：更新本方案文档、[skill-build-loadout-7slots-v1.md](skill-build-loadout-7slots-v1.md) 与客户端 `Docs/design/pre-battle-loadout-quickbar.md`。
2. **协议增量与跨端生成**：更新 `proto/common/component/player_equip_loadout_comp.proto`（注释对齐 1..7）与 `proto/scene/player_bag.proto`（`EquipLoadoutSnapshot` 追加 `14..21` 统一席位投影字段）；执行 C++ `proto-gen-run`、客户端 `gen_proto.ps1` / `gen_messageids.ps1` 与 Java `ContractSync`。
3. **C++ 权威服实现与 GTest 验收**：升级 `equip_loadout_rules.h`（`kFreeSchemes = 2, kMaxSchemes = 7`、默认名一..七、`EffectiveActiveSchemeId` 保留 `1..7`）与 `player_equip_loadout.{h,cpp}`（`BuildSnapshotAt`/`SaveAt`/`SwitchAt` 统一读取 `skillloadout::UnlockedSchemes` + `AfkPassSystem::IsActive` + `PlayerSkillListComp` + `LevelComp`，`Unlock` 统一 fail-closed 拒回 `kFeatureUnavailable`）；更新 `player_equip_loadout_test.cpp`，通过静态门禁、串行 `msbuild /m:1` 与 `bag_test.exe` / `skill_test.exe` 实跑全绿。
4. **Python 服务端与 Java 契约对账**：同步更新 `xuanming-server-mmo-python` 的 `scene_equip_loadout.py` 与定向 pytest，并更新 `xuanming-server-mmo` 的同步契约与 `PARITY.md` 登记。
5. **Unity 客户端模型、uGUI 与 NUnit 验收**：
   - 升级 `EquipLoadoutModel.cs`（7 套上限、4 态页签、统一席位快照转 `SkillLoadoutView` 复用 `SkillLoadoutRentPlanner`、锁定已存方案防误卖保护与 1..7 战前快捷条状态）；
   - 升级 `SkillBookClient.cs`（`RentLoadoutSlots` 脱离技能窗首拉前提并暴露 `LoadoutSlotsRented` 事件）与 `GameClient.cs`（续租/轻享卡同步刷新 `EquipLoadouts`）；
   - 升级 `EquipmentWindow.{cs,Loadout.cs}`（顶栏 7 页签复用 `SkillBuildLoadoutV1/tab_build_*`、锁定方案只读预览、原解锁弹窗改为复用 `panel_build_rent_modal` 的统一 7 套席位租用与轻享卡引导弹窗）、`SkillLoadoutSubPanel.cs`、`AfkPassUiRoot.cs` 文案对齐「装备 + 战法 + 加点三合一共享 7 套」与 `PreBattleLoadoutQuickBar.cs` 7 槽紧凑排布；
   - 更新 `EquipLoadoutUiTests.cs`，跑通 `tools/client_compile_check.ps1` 与 EditMode NUnit 单测。
