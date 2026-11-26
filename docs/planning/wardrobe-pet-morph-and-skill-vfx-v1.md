# 衣柜扩展：灵宠幻化与技能特效 V1 设计

> 日期：2026-11-25  
> UI 素材来源：`D:\work\image\designs\wardrobe-cosmetics-ui-20261125`（Azure `image/main` `e59c4ad7484a`）  
> 关联设计：[角色形象永久衣柜](character-appearance-switch.md) · [经济与官方 7:3 总纲](equipment-economy-charter.md) · [灵石百宝阁 V1](spirit-shop-and-starter-packs-v1.md)

---

## 1. 需求分析

1. **复用单一衣柜窗口承接三类外观**：
   - 在现有 `AppearanceUiRoot` 同一窗口顶部新增「人物 / 灵宠 / 特效」三个类别页签，不另开新窗口。
   - 「人物」页 100% 保持现行易容、1 小时免费试用、系列收集与人物志阅读行为不变。
   - 「灵宠」与「特效」页复用同一窗口壳体与宽屏/窄横屏响应式流程，使用交付的 `WardrobeCosmetics` 无字切片。
2. **纯表现零数值红线**：
   - 灵宠幻化仅改变单只灵宠实例的外观展示（主城面板、灵宠列表、战斗单位模型与头像），**不改变**资质、属性点、条目、灵阶、物种判定或任何战斗数值。
   - 技能特效仅改变对应技能的战斗演出画面（特效帧条与色调），**不改变**技能伤害、目标数、冷却、耗蓝或判定规则。
3. **购买与穿戴解耦，权威校验闭环**：
   - 购买入口统一复用既有「官方商城 → 永久衣柜」子页签；仅当服务端商城实际返回该条目对应的有效可售 SKU 时展示「前往永久衣柜」，未上架或不可售时展示取得条件/不可用状态，绝不伪造直购按钮。
   - 购买完成后刷新外观目录与拥有权益，不自动替玩家穿戴，由玩家返回衣柜自主选择穿戴。
   - 支持从灵宠面板 / 技能面板上下文定位到对应 `petId` 或 `skillId`，保留当前会话内各类别的选中项与滚动位置。

---

## 2. 设计分析

### 2.1 权威配置表（新增 2 张独立表，零触碰既有脏表）

均声明 `(cfg_ids_permanent) = true` 与主键 `(cfg_bit_index) = true`，严格遵守 [位图存储与删表 ID 红线](../rules/bit-storage-and-table-ids.md)：

| 表名 | 源表 / Schema | 核心字段（全 `common` owner） | 校验不变量 |
|---|---|---|---|
| `PetCosmetic` | `data/PetCosmetic.xlsx` + `data/schema/petcosmetic_table.proto` | `id (cfg_bit_index)`、`cosmetic_key`、`name`、`target_pet_table_id`（0=通用）、`visual_pet_table_id`、`visual_art_key`、`enabled`、`sort_order`、`mall_sku_id`、`acquire_hint`、`desc` | `cosmetic_key`/`name` 非空且启用项 `cosmetic_key` 唯一；`target_pet_table_id` 非 0 时必须存在于 `Pet` 表；`visual_pet_table_id` 非 0 时须存在于 `Pet` 表或 `visual_art_key` 非空；每行须存在有效 `bit_index` |
| `SkillVfxCosmetic` | `data/SkillVfxCosmetic.xlsx` + `data/schema/skillvfxcosmetic_table.proto` | `id (cfg_bit_index)`、`vfx_key`、`name`、`skill_table_id`、`fx_id`、`projectile_fx_id`、`telegraph_fx_id`、`tint_hex`、`enabled`、`sort_order`、`mall_sku_id`、`acquire_hint`、`desc` | `vfx_key`/`name`/`fx_id` 非空且启用项 `vfx_key` 唯一；`skill_table_id` 必须存在于 `Skill` 表；`tint_hex` 为空或合法 `#RRGGBB`；每行须存在有效 `bit_index` |

### 2.2 持久化存储（双服严格隔离，高位未知 bit 零截断）

- **C++/Go 权威服**：
  - `PlayerProfileComp`（`proto/common/component/player_comp.proto`）追加：
    - `bytes pet_cosmetic_owned_bits = 12;`（按 `PetCosmeticBitMap` 紧凑低位在前存储）
    - `bytes skill_vfx_owned_bits = 13;`（按 `SkillVfxCosmeticBitMap` 紧凑低位在前存储）
    - `map<uint32, uint32> equipped_skill_vfx = 14;`（`skill_table_id -> SkillVfxCosmetic.id`）
  - `PetInstance`（`proto/common/component/player_pet_comp.proto`）追加：
    - `uint32 cosmetic_id = 26;`（`0` = 原形，非 0 = `PetCosmetic.id`；随灵宠实例同生命周期，放生/交易无残留悬空引用）
- **Java 独立服**：
  - `AppearanceState`（`game-player-store/.../player_state.proto`）追加 `pet_cosmetic_owned_bits = 7`、`skill_vfx_owned_bits = 8`、`equipped_skill_vfx = 9`；
  - `PetEntry` 追加 `cosmetic_id = 26`；与 C++ 保持同位图格式并原样保留未知高位与未知字段。

### 2.3 协议契约（沿用 `SceneAppearanceClientPlayer`）

- **快照扩展**（`PlayerAppearanceInfo` 末尾追加 `16..21`）：
  - `pet_cosmetic_owned_bits`、`pet_cosmetics`（全部启用目录及实时 `owned` 判定）、`equipped_pet_cosmetics`（当前玩家每只灵宠实例的实际穿戴 `pet_id -> cosmetic_id`）；
  - `skill_vfx_owned_bits`、`skill_vfx_cosmetics`（全部启用目录及实时 `owned` 判定）、`equipped_skill_vfx`（`skill_table_id -> cosmetic_id`）。
- **末尾追加 2 个 RPC**（不动既有方法序 0..5）：
  - `EquipPetCosmetic(EquipPetCosmeticRequest) returns (EquipPetCosmeticResponse)`：`cosmetic_id = 0` 表示恢复原形；回包带最新 `PlayerAppearanceInfo`、`pet_id` 与最终 `equipped_cosmetic_id`。
  - `EquipSkillVfx(EquipSkillVfxRequest) returns (EquipSkillVfxResponse)`：`cosmetic_id = 0` 表示恢复默认；回包带最新 `PlayerAppearanceInfo`、`skill_table_id` 与最终 `equipped_cosmetic_id`。
- **战斗与灵宠快照透传**：
  - `PetInfo`（`proto/scene/player_pet.proto`）追加 `cosmetic_id = 22`、`cosmetic_art_key = 23`；
  - `BattlePetSnapshot` 追加 `cosmetic_id = 17`、`cosmetic_art_key = 18`；`BattlePlayerSnapshot` 追加 `equipped_skill_vfx = 31`；`BattleActorState` 追加 `pet_cosmetic_art_key = 46`、`equipped_skill_vfx = 47`。
  - 客户端渲染器（`BattleArtCatalog` / `BattleUnitView` / `BattlePresenter` / `PetPanel`）优先读取已验证的 `pet_cosmetic_art_key` 与 `equipped_skill_vfx`，缺失任何资源时无声回退原形与默认特效。

### 2.4 客户端架构与 UI 接入（`mmorpg-client`）

- **素材导入**：将 12 张无字切片从 `D:\work\image\designs\wardrobe-cosmetics-ui-20261125\sprites\` 原样拷贝至 `Assets/Resources/UI/Ugui/WardrobeCosmetics/`，按 `layout.json` 参数配置 `Sprite (2D and UI) / Single / Full Rect / PPU=100 / AlphaIsTransparency=true / Mipmaps=false / MaxSize=2048`。
- **纯 C# 状态机分层**（`Assets/Scripts/Game/Appearance/WardrobeCosmeticModels.cs`，零 `UnityEngine` 依赖）：
  - 管理三页签切换、各类别独立选中项与滚动位置、灵宠目标切换、`对比原形` 开关、技能筛选、动态按钮态（已拥有可穿戴 / 当前生效可恢复 / 未拥有有在售 SKU 前往永久衣柜 / 未上架不可用）、断线与换角色清态。
- **网络协调**（`AppearanceClient.cs`）：新增 `EquipPetCosmetic` / `EquipSkillVfx`，严格守住连接代次与角色 ID 校验、提交期锁按钮防连点、超时自动重拉权威快照再解锁。
- **商城联动**（`JubaozhaiUiRoot.cs` + `CommerceWindow.cs`）：新增 `OpenOfficialMallSubTab(OfficialShopTab.PermanentWardrobe, skuId)` 精准定位「永久衣柜」子页签，并以 `HasSellableOfficialSku(skuId)` 校验服务端真实在售目录；付款结算刷新资产时同步刷新衣柜目录（不自动穿戴）。
- **视图层**（`AppearanceUiRoot.Cosmetics.cs` + `AppearanceUiArt.cs`）：在 `AppearanceMainPanel` 顶部挂载三页签，宽屏与窄横屏（1180×650 含 `< 返回列表`）按 `layout.json` 比例自适应排布；特效预览区使用 `RectMask2D` 裁切播放 8 帧真实序列帧条，支持「重播特效」，切项/切页/关闭/断线立即停播。

---

## 3. 思想分析

- **单一衣柜、单一权威源**：人物形象、灵宠幻化、技能特效共用 `SceneAppearanceClientPlayer` 与 `AppearanceUiRoot`，免去多窗口状态漂移与重复网络协调器。
- **纯幂等穿戴收敛**：穿戴/恢复动作不扣资产，重复提交同一目标状态直接收敛返回 `kSuccess` 与最新权威快照，天然消灭并发重复点击导致的误报错与状态分叉。
- **配置与表现解耦、缺图安全回退**：配表用稳定 `visual_art_key` / `fx_id` + 可选 `tint_hex` 表达表现，先用现有已验收高精灵宠动作库与 384px 技能帧条即可跑通全链路实机演示；未来美术产出专属换皮或独立特效帧条时，只需替换资源键并热更配表，零代码改动。

---

## 4. 漏洞分析与防线

| 潜在漏洞 / 边界 | 防护措施 |
|---|---|
| 客户端伪造未拥有的 `cosmetic_id` 请求穿戴 | 服务端 `EquipPetCosmetic` / `EquipSkillVfx` 强制校验对应永久拥有位图（`pet_cosmetic_owned_bits` / `skill_vfx_owned_bits`），未拥有一律 fail-closed 拒回 `kAppearanceNotOwned` |
| 跨灵宠物种误穿或对他人/已放生灵宠穿戴 | 服务端按 `player` 自己的 `PlayerPetComp` 精确查找 `pet_id`，并校验 `target_pet_table_id == 0 \|\| target_pet_table_id == pet.pet_table_id()` |
| 为未学会或非对应技能装备特效 | 服务端校验 `row.skill_table_id() == request.skill_table_id()` 且该技能存在于角色已学/可用技能目录，否则拒回 `kAppearanceInvalid` |
| 下架外观或被回收权益仍残留于战斗展示 | 下发快照与进战构建 `BattlePetSnapshot` / `BattlePlayerSnapshot` 时按当前启用表 + 拥有位图二次过滤，失效项自动回落原形/默认特效 |
| 商城未上架该 SKU 却显示可点购买按钮导致死跳转 | 客户端 `WardrobeCosmeticModels` 仅在服务端官方商城返回 `Available > 0` 的匹配 `OfficialSkuView` 时展示「前往永久衣柜」，否则展示取得条件/暂未上架并禁用购买按钮 |
| 迟到回包、换角色或断线后旧状态覆写新界面 | `AppearanceClient` 按 `(generation, connectionIdentity, playerId)` 三重守卫丢弃过期回包；关闭/断线/换角立即停播特效预览并重置临时态 |
| 灰度混跑截断高位未知拥有 bit | C++ `SetPackedBit` 与 Java `PlayerAppearance.setBit` 仅按需扩容，保留高位未知 bit 原样回写 |
| 复用既有 `Tip.xlsx` / `Item.xlsx` 与并行会话冲突 | 全部复用既有 `appearance_error` / `common_error` / `pet_error` 提示码，新建独立表，零触碰并行脏表 |

---

## 5. 步骤分析与执行步骤

1. **配表与设计落盘**：编写本设计文档、`petcosmetic_table.proto`、`skillvfxcosmetic_table.proto` 与幂等补丁脚本 `wardrobe_cosmetics_xlsx_patch.py`，生成 `PetCosmetic.xlsx` 与 `SkillVfxCosmetic.xlsx`，运行导表与 `gen_schema_index.py`，在 `table.vcxproj` 登记新表。
2. **协议与多端生成**：扩展 `player_comp.proto`、`player_pet_comp.proto`、`player_pet.proto`、`player_appearance.proto`、`battle_data.proto`，执行 `proto-gen-run`、更新 `MessageLimiter.xlsx` 并同步 `mmorpg-client` 的表、proto 与消息号。
3. **C++ 服务端闭环与单测**：扩展 `appearance_rules.h`、`player_appearance.h/.cpp`、`player_appearance_handler.cpp`、`player_pet.cpp`、`player_battle.cpp`、`turn_battle_engine.cpp`，在 `player_appearance_test.cpp` 补齐单测，跑静态检查、`msbuild /m:1` 与 `bag_test`。
4. **Unity 素材与客户端全链路接入**：拷贝 12 张切片与 `.meta` 至 `Assets/Resources/UI/Ugui/WardrobeCosmetics/`；实现 `WardrobeCosmeticModels.cs`、`AppearanceClient.cs`、`JubaozhaiUiRoot.cs`/`CommerceWindow.cs`、`AppearanceUiArt.cs`、`AppearanceUiRoot.Cosmetics.cs` 及战斗/灵宠表现接入；新增 `WardrobeCosmeticsTests.cs` 并执行编译检查与 NUnit 测试。
5. **Java 独立服同步与回归**：同步契约并在 `xuanming-server-mmo` 完成持久化、服务、战斗/灵宠快照与单测对齐，登记 `PARITY.md` 与 `docs/PROGRESS.md`。
