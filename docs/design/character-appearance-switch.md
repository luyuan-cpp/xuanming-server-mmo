# 角色形象永久收藏衣柜、形象卡解锁与 1 小时免费试用设计

> 2026-11-23 批次二升级（在 2026-11-21 易容切换与 1 小时试用基线上演进）。
> 负责人拍板口径：永久解锁衣柜制 + 3402 形象卡永久买断（预留配表 `card_unlockable` 开关给后续玩法专属）+ 新号初始开放 2 个免费形象（阿禾 `00_reference_topright_boy`、冰剑少女 `01_ice_sword_girl`），老号原有永久形象自动承认入柜 + 双服与客户端全链路同步。

## 1. 需求分析

1. **永久多人物收藏衣柜与免费切换**：
   - 每名角色维护独立的永久已拥有形象集合（位图存储），已拥有的形象可随时 **0 消耗免费切换**为当前永久形象，且不再覆盖丢弃原有形象。
   - 明确区分四个事实：可预览（全部启用形象）、已拥有（衣柜位图置位或等于当前永久形象）、当前永久使用（`appearance_id`）与正在试用（`trial_appearance_id`）。
2. **3402 角色形象卡（10 元）升级为永久买断解锁券**：
   - 未拥有且配置表允许用卡解锁（`card_unlockable = 1`）的形象，消耗 `card_item_count`（默认 1）张 `3402` 角色形象卡即可**永久解锁入柜并自动切换**，原永久形象同步保留入柜。
   - 配表预留 `card_unlockable = 0` 开关，未拥有时拒绝卡换（返回 `kAppearanceNotCardUnlockable`），供批次三排位/PVE/活动专属形象接入，并同步提供服务端内部授权发放接口 `GrantAppearance`（不自动换上、幂等置位）。
3. **1 小时免费试用独立保留**：
   - 仅对尚未拥有的形象开放每形象 1 次 1 小时免费试用（`appearance_trial_used_bits` 与 `appearance_owned_bits` 严格分离，试用绝不授永久权）；已拥有的形象直接免费切换，禁止浪费试用资格（返回 `kAppearanceAlreadyOwned`）。
4. **新号初始 2 选 1 与老号无损兼容**：
   - 新建角色初始仅开放 2 位免费形象：`00_reference_topright_boy`（阿禾）与 `01_ice_sword_girl`（冰剑少女）；兼容旧端/机器人空串 `""`。
   - 其余 6 位现役形象（`02`/`03`/`05`/`09`/`10`/`15`）退出新建白名单，但**保留为历史角色登录/进场与丢回包精确重试**；老号当前持有的合法永久形象无需迁移脚本即自动视作已拥有，且首次切换离场时自动将原形象补写进 `appearance_owned_bits`。

## 2. 设计分析

### 2.1 配表与稳定位序

| 表名 | 变更 | 说明 |
|---|---|---|
| `CharacterAppearance`（`data/CharacterAppearance.xlsx` + `data/schema/characterappearance_table.proto`） | 在字段 6 后追加 `uint32 starter_creatable = 7`、`uint32 card_unlockable = 8`（`common` owner） | `id` 继续保留 `cfg_bit_index = true` 与 `cfg_ids_permanent = true`，绝不改 `characterappearance_mapping.json`；现役 8 行中 `id=2,3` 置 `starter_creatable=1`，全部 8 行暂置 `card_unlockable=1`（批次三改表即可切专属） |
| `Item`（`data/Item.xlsx`） | 更新 `id=3402` 描述文案 | 说明形象卡用于永久解锁并切换支持兑换的形象，已拥有可免费切换 |
| `Tip`（`data/tip/Tip.xlsx`） | `//appearance_error` 末尾追加 `33007..33009` | `AppearanceNotCardUnlockable`（33007）、`AppearanceAlreadyOwned`（33008）、`AppearanceNotOwned`（33009），均为业务拒绝（非 fault） |

### 2.2 持久存储与双服隔离

- **C++/Go 权威服**：`PlayerProfileComp`（`proto/common/component/player_comp.proto`）追加 `bytes appearance_owned_bits = 11`。沿用低位在前紧凑位图与 `CharacterAppearanceBitMap` 映射，置位时保留未知高位 bit，不截断。
- **Java 独立服**：`AppearanceState`（`game-player-store/.../player_state.proto`）追加 `bytes owned_bits = 6`，在 `persistent-schema` 登记第 7 代并更新 `PlayerStateSchemaLedgerTest`；`PlayerAppearance` 与 C++ 同位图格式并保留未知高位与未知字段。
- **读零写盘 + 首次切走自动补位**：读取与鉴权统一判定 `IsOwned = TestBit(owned_bits, cfgId) || (appearance_id == permanent_appearance_id)`；当角色首次执行 `SwitchAppearance` 离开原形象时，在同一原子写路径里先将合法 `beforePermanent`（含已下架但表行仍在者）补置进 `owned_bits`，再写入新目标位与 `appearance_id`。

### 2.3 协议契约（`proto/scene/player_appearance.proto`）

沿用 `SceneAppearanceClientPlayer`（442–447），不新增 RPC 方法号与幂等种类，仅在现有消息末尾追加字段：
- `CharacterAppearanceEntry` 追加 `bool owned = 6`、`bool card_unlockable = 7`。
- `PlayerAppearanceInfo` 追加 `bytes owned_bits = 14`（服务端按持久化 `owned_bits` 并上当前永久形象对应位输出，高位未知 bit 原样保留）。
- `SwitchAppearanceRequest` 追加 `bool expect_free_switch = 3`：
  - `true`（客户端点击「免费切换」）：服务端强制校验已拥有，未拥有时 **fail-closed 返回 `kAppearanceNotOwned`**，绝不误扣形象卡。
  - `false`（客户端二次确认「解锁并切换」）：若服务端发现此时已拥有，直接 **0 消耗免费切换**（不浪费卡）；若未拥有，则校验 `card_unlockable != 0` 并原子扣卡 + 置位 + 切换。

### 2.4 建角白名单与客户端轻量立绘分离

- Go `go/login/.../character_appearance.go` 与 Java `CharacterAppearances.java` 同步对齐：
  - 建角白名单仅允许 `""`、`00_reference_topright_boy`、`01_ice_sword_girl`；
  - `02`/`03`/`05`/`09`/`10`/`15` 并入退役精确重试集合（只用于本账号已提交建角的丢回包恢复，禁止新创角）；
  - `04`/`06`/`08`/`17` 维持彻底删除拒绝。
- Unity 客户端 `QdaoCharacterCatalog.cs` 拆分 `IsReleasedAppearance`（8 个现役外观，供 `AcquireCreationPortrait` 轻量立绘租约继续加载全部 8 人衣柜立绘）与 `IsCreatableAppearance`（仅 2 个初始外观，供 `CreationOriginalAll`、`RoleFlowUi` 与 `GameClient` 建角使用）。

## 3. 思想分析

- **零额外 RPC 的显式意图防误扣**：通过 `expect_free_switch` 在既有 `SwitchAppearance` 上显式声明“我只要免费切”还是“我已确认耗卡解锁”，既省去冗余消息号与限流/换节点白名单维护，又从协议层消灭“UI 缓存陈旧导致点免费切被扣 10 元”的资金事故。
- **读纯写补的无迁移升级**：不造历史数据版本迁移脚本，也不靠试用位图或持卡数猜测历史权益；仅凭当前已落盘的合法永久形象自动入柜，做到旧档零写盘读兼容、切换不丢原外观。
- **复用单一真相与位图映射**：已拥有位图与试用位图复用同一 `CharacterAppearanceBitMap` 但独立存字段，彻底杜绝试用与永久权串位。

## 4. 漏洞分析与防线

| 潜在风险 / 边界漏洞 | 防护措施 |
|---|---|
| 客户端缓存过旧，把未拥有角色当成已拥有点了“免费切换”导致误扣 10 元形象卡 | `SwitchAppearanceRequest.expect_free_switch = true` 时服务端未拥有一律 fail-closed 拒回 `kAppearanceNotOwned`，不进入扣卡路径 |
| 玩家刚通过其他渠道获得该角色，客户端尚未刷新就点了“耗卡解锁” | `expect_free_switch = false` 遇到目标已拥有时直接走 0 消耗免费切换，不扣卡 |
| 超时重发 `SwitchAppearance` 重复扣卡或把未拥有重判为已拥有 | 幂等查账（`kSwitchAppearance`）严格排在一切状态校验前；指纹覆盖 `appearance_id + expect_free_switch`，重放直接回 `replayed=true` 且零扣卡 |
| 老号首次耗卡解锁新形象后丢掉自己原本的永久形象 | 切换覆盖 `appearance_id` 前先把表内可查到的 `beforePermanent` 补置入 `appearance_owned_bits`，老形象永久入柜 |
| 试用过某形象被误当成永久拥有，或对已拥有形象误消耗 1 小时试用位 | 永久位图与试用位图独立存储；`StartTrial` 遇到已拥有直接拒回 `kAppearanceAlreadyOwned` |
| 策划把某形象设为玩法专属（`card_unlockable=0`），客户端伪造请求试图用卡解锁 | 服务端强制检查 `row.card_unlockable() != 0`，否则拒回 `kAppearanceNotCardUnlockable` |
| 灰度/混跑新老版本截断高位未知 bit | C++ `SetPackedBit` 与 Java `PlayerAppearance` 均仅按需扩容、完整透传高位未知 bit |
| 收紧建角白名单后导致衣柜页其余 6 人 `AcquireCreationPortrait` 变空图 | 客户端区分 `IsReleasedAppearance`（8 人立绘租约）与 `IsCreatableAppearance`（2 人建角） |

## 5. 步骤分析与执行步骤

1. **配表与协议源**：更新 `data/schema/characterappearance_table.proto`、`tools/scripts/appearance_switch_xlsx_patch.py`（补列、更新 3402 描述、追加 3 个 Tip 码）、`proto/common/component/player_comp.proto`、`proto/scene/player_appearance.proto`；执行补丁脚本、`dev.bat gen`（导表 + proto 生成）及 `gen_schema_index.py`。
2. **C++/Go 服务端**：更新 `appearance_rules.h`、`player_appearance.h/.cpp`（永久拥有位图、免费切换、`expect_free_switch` 保护、`card_unlockable` 校验、`GrantAppearance`、禁止试用已拥有）与 `go/login/.../character_appearance.go`（建角收紧 2 人）；补齐 C++ `player_appearance_test.cpp` 与 Go `character_appearance_test.go` 回归用例；执行 `test_cpp_no_raw_pointer_member.py`、Go 测试与 C++ `msbuild /m:1` + `bag_test`。
3. **Java 独立服对齐**：运行 `ContractSync.java` 同步契约（保留已有排位工作区改动）；更新 `player_state.proto`（`AppearanceState.owned_bits = 6`）并用 `PersistentSchemaTool` 登记第 7 代；更新 `PlayerAppearance`、`AppearanceTables`、`AppearanceService`、`CharacterAppearances` 与单测；同步登记 `PARITY.md`；执行 Maven `clean install` / 定向测试。
4. **Unity 客户端同步与 UI 升级**：运行 `sync_tables.ps1` / `gen_proto.ps1` / `gen_messageids.ps1` 同步最新 8 人表与新协议字段；更新 `QdaoCharacterCatalog.cs`（拆分 8 人现役与 2 人建角）、`AppearanceClient.cs`、`AppearanceBiography.cs`、`AppearanceUiArt.cs`、`AppearanceUiRoot.cs` 与单测；运行 `client_compile_check.ps1` 与离线 `appearance_chronicle` 测试。
5. **进度与文档收口**：更新 `character-background-collection-design.md` 批次二状态与 `docs/PROGRESS.md`、`mmorpg-client/Docs/design/appearance-ui-and-story-reader.md`，汇总真实命令与测试证据。

## 6. 批次二·续增量：人物收藏系列（纯外观/叙事，零属性）

1. **需求分析**：在永久多人物收藏衣柜之上，按世界观/题材将现役人物划分为若干收藏系列（首批 8 人分为「留灯院旧友」「水火冰锋」「清音济世」3 册），提供系列筛选、收集进度（如 `1/3`）、集齐点亮印章与集齐解锁合篇故事；**严格不加任何战斗/成长/掉率属性**。
2. **设计分析**：
   - 新增配置表 `AppearanceSeries`（`data/AppearanceSeries.xlsx` + `data/schema/appearanceseries_table.proto`，`cfg_ids_permanent = true`）：`id`、`series_key`、`name`、`sort_order`、`enabled`、`desc`、`completion_badge_name`、`epilogue_title`、`epilogue_body`（全部 `common` owner，双服与客户端 `.pb` 同步）。
   - `CharacterAppearance` 追加 `uint32 series_id = 9 [(cfg_fk) = "AppearanceSeries"];`（`common` owner）。
   - **零新增存档字段**：系列拥有数与集齐态在 C++ `PlayerAppearanceSystem::FillInfo` 与 Java `AppearanceService.buildInfo` 中依据权威拥有判定（`owned_bits || permanent_appearance_id`）实时聚合派生；试用态不计入拥有数；空系列（`total_count == 0`）不输出。
   - `proto/scene/player_appearance.proto` 在 `CharacterAppearanceEntry` 追加 `uint32 series_id = 8`，新增 `CharacterAppearanceSeriesProgress`，并在 `PlayerAppearanceInfo` 追加 `repeated CharacterAppearanceSeriesProgress series_progress = 15`（不传大段 `epilogue_body`，正文由客户端查本地表并在 `completed == true` 时解锁）。
   - Unity 客户端同步表/协议并新增纯 C# `AppearanceSeriesModels` + 扩展 `AppearancePanelState`（筛选/合篇阅读状态机，零 `UnityEngine` 依赖）；uGUI 视觉层交接给 Codex（见 `character-background-collection-design.md §9.2`）。
3. **思想分析**：派生不落库（杜绝“拥有了人却未点亮系列”或“回档后系列进度错乱”的第二真相）；服务端只传轻量进度与权威 `completed` 开关，长文案留本地表，不膨胀心跳/通知包。
4. **漏洞分析与防线**：
   - *试用人物冒充已拥有点亮系列*：聚合时只认 `IsOwned`，显式排除 `trial_appearance_id`。
   - *配置表误删系列或引用未启用系列*：导表 `(cfg_fk) = "AppearanceSeries"` + C++/Java `ValidateTables` / `AppearanceTables.from` 启动强校验（已启用角色若 `series_id != 0` 须命中已启用系列，且启用系列 `series_key` 唯一非空）。
   - *客户端伪造解锁读合篇*：`AppearanceSeriesModels.TryGetUnlockedEpilogue` 与 `AppearancePanelState.OpenSeriesEpilogue` 均以权威 `completed == true` 为前提，进入合篇层时清除待确认扣卡态，防误扣卡。
5. **执行步骤**：补表 schema 与 xlsx 补丁 → 导表 + protogen + 登记 C++ `table.vcxproj`/`CMakeLists.txt` → 实现 C++ 与 Java 系列校验/聚合与单测 → 同步 Unity 客户端表/协议、实现纯 C# `AppearanceSeriesModels` / `AppearancePanelState` 与单测 → 全量静态检查、编译与定向测试验证。

## 7. 批次二·终章：全员人物志配表化（9 位现役角色）与前后端契约闭环（2026-11-24）

### 7.1 需求分析

1. **补齐全部现役 9 位角色的人物志**：原首版仅在客户端硬编码 3 位人物志（陆照禾、祝停云、桑听雨），其余 5 位常规角色（冰剑少女、火符少年、莲花医者、天音少女、水龙书生）与五行试炼志 18 印专属角色（星阵宗师 `20_star_formation_master_girl`）均显示“暂无人物志”。现补齐全部 9 位现役角色的人物档案名、小名、人物小传与一段往事，且保持纯叙事、零属性加成。
2. **清理已删角色残留引用**：祝停云往事《纸也赢过一回》中曾提及已移除的 `17_ghost_script_calligrapher_boy`（闻砚生），统一替换为同在归元城与留灯院往来的现役角色「沈澄」（水龙书生 `15_water_dragon_scholar_boy`），同时与「水火冰锋」合篇《霜火沧澜试》形成角色联动。
3. **人物志迁入权威配置表单一事实源**：将人物志文案从客户端硬编码迁至 `CharacterAppearance` 权威配表（全字段 `common` owner，双服校验 + 客户端本地 `.pb` 查表），后续策划改字或新增角色传记只需改表并热更，无需重编客户端代码，且**不增加网络协议包体积**。

### 7.2 设计分析

1. **配表 Schema 增量**（`data/schema/characterappearance_table.proto`，字段号 `10..14` 顺序追加，不动既有 `1..9` 及永久位序 `cfg_bit_index`）：
   - `string archive_name = 10;` — 人物档案名（空串表示该形象暂无人物志）
   - `string nickname = 11;` — 人物小名/别称（可留空）
   - `string biography_summary = 12;` — 人物小传摘要
   - `string story_title = 13;` — 人物往事篇名
   - `string story_body = 14;` — 人物往事正文（段落间以 `\n\n` 分隔）
2. **零网络协议变更**：沿用 `proto/scene/player_appearance.proto`（RPC 442–447），不在 `CharacterAppearanceEntry` 透传长文案，避免每次拉取快照或状态推送膨胀。
3. **全员 9 位现役人物档案总表**：
   | `id` | `appearance_id` | 原形象名 | 所属系列 | 档案名（小名） | 往事篇名 |
   |---:|---|---|---|---|---|
   | 2 | `00_reference_topright_boy` | 金发带Q道童 | 1 · 留灯院旧友 | 陆照禾（阿禾） | 《第四只碗》 |
   | 3 | `01_ice_sword_girl` | 冰剑少女 | 2 · 水火冰锋 | 霜见雪 | 《霜阶扫雪》 |
   | 4 | `02_fire_talisman_boy` | 火符少年 | 2 · 水火冰锋 | 许炽（小炽） | 《焦边朱砂》 |
   | 5 | `03_lotus_healer_girl` | 莲花医者 | 3 · 清音济世 | 苏青莲 | 《药包双结》 |
   | 7 | `05_celestial_musician_girl` | 天音少女 | 3 · 清音济世 | 林知音 | 《廊下试弦》 |
   | 8 | `09_bamboo_archer_girl` | 竹弓少女 | 1 · 留灯院旧友 | 桑听雨 | 《没有画的那一页》 |
   | 9 | `10_crimson_spear_girl` | 赤枪少女 | 1 · 留灯院旧友 | 祝停云 | 《纸也赢过一回》 |
   | 10 | `15_water_dragon_scholar_boy` | 水龙书生 | 2 · 水火冰锋 | 沈澄 | 《半盏未凉》 |
   | 11 | `20_star_formation_master_girl` | 星阵宗师 | 0 · 试炼志 18 印专属 | 星璇 | 《十八星印》 |
4. **双服务端启动强校验**（C++ `PlayerAppearanceSystem::ValidateTables` 与 Java `AppearanceTables.from` 保持一致）：
   - 任意行允许 `archive_name` / `biography_summary` / `story_title` / `story_body` 四项全空（表示为“暂无人物志”）；
   - 若四项或 `nickname` 任一非空，则四项核心字段（`archive_name`、`biography_summary`、`story_title`、`story_body`）必须**同时非空**，否则启动表校验 fail-closed 拒启，杜绝半残传记进入线上。
5. **客户端热更安全查表**（`mmorpg-client/Assets/Scripts/Game/Appearance/AppearanceBiography.cs`）：
   - `AppearanceBiographyCatalog` 遵守「不长期缓存 `*TableManager` 行引用」规则，每次按原始 `rawAppearanceId`（`StringComparer.Ordinal`，绝不经 `ResolveVisualId`）即时遍历 `CharacterAppearanceTableManager.Instance.FindAll()`；
   - 表已加载时完全以当前快照为唯一事实源（支持热更立即生效，且表内留空人物志字段时精确返回 `false`）；仅在表尚未加载（`Count == 0` 的极简纯 C# 测试上下文）时回退至同源只读内置目录。

### 7.3 思想分析

- **单一事实源优于双份维护**：不另起 `CharacterBiography.xlsx` 并行表，直接复用 `CharacterAppearance` 的永久主键、导出流水线、外键与客户端 `.pb` 裁剪同步，减少表索引维护和跨表对齐风险。
- **Fail-closed 配表校验**：文案虽由客户端渲染，但在双服务端表校验阶段拦住半残字段，确保任何导表合并事故在启动体检即暴露。
- **不破坏既有状态机边界**：阅读层仍纯只读、零写请求、精确匹配原始 `appearance_id`、无档案条目仍可自动收起人物背景区。

### 7.4 漏洞分析与防线

| 潜在风险 | 防护措施 |
|---|---|
| 策划填了档案名却漏填小传或往事正文，导致客户端点开空弹层 | C++ `ValidateTables()` 与 Java `AppearanceTables.from()` 均做四项同空/同全校验；客户端 `TryGetByRawAppearanceId` 也只认四项齐全的行 |
| 客户端缓存表行引用导致表热更后仍显示旧文案 | `AppearanceBiographyCatalog` 每次现查 `CharacterAppearanceTableManager.Instance.FindAll()` 快照，不跨调用缓存 `CharacterAppearanceTable` 实例 |
| 历史视觉替代 ID（如 `QdaoHeadbandBoy`、`28_moon_rabbit_artificer`）误命中新传记 | 查找仅按原始 `row.AppearanceId` 精确 `Ordinal` 相等，绝不调用 `QdaoCharacterCatalog.ResolveVisualId` |
| 既有测试断言“未写传记角色隐藏背景区”因全员补齐而失效 | 表驱动下，任意未在配表写传记的扩展 ID 或将某行传记留空的测试快照均继续精确返回 `false`，验证用例改用无传记条目覆盖该边界 |
| `CharacterAppearance.xlsx` 升级字段时破坏 `cfg_bit_index` 位序或 `permanent_ids` 台账 | 只追加字段 `10..14`，不改动既有 `id`（`2,3,4,5,7,8,9,10,11`）及 `characterappearance_mapping.json` |

### 7.5 步骤分析与执行步骤

1. 更新 `data/schema/characterappearance_table.proto`（追加字段 `10..14`）、更新 `tools/scripts/appearance_switch_xlsx_patch.py` 与 `data/CharacterAppearance.xlsx` 全 9 行人物志数据；执行 `dev.bat export` 与 `gen_schema_index.py`。
2. 在 C++ `player_appearance.cpp` 增加人物志完整性校验，在 `player_appearance_test.cpp` 补充全 9 人配表、半残拦截与全空放行用例；跑 `test_cpp_no_raw_pointer_member.py`、串行 `msbuild /m:1` 编译 `table`/`scene`/`bag_test` 并运行 `bag_test` 与 `scene.exe --validate-tables`。
3. 在 Java 独立服运行 `ContractSync.java` 同步契约，更新 `AppearanceTables.java`、`AppearanceFeatureTest.java` 与 `PARITY.md`，执行 `mvnw.cmd -B -pl game-table,game-scene -am clean test`。
4. 在 Unity 客户端运行 `tools/sync_tables.ps1` 同步最新 `characterappearance.pb` 与生成表类；改造 `AppearanceBiography.cs` 接入 `CharacterAppearanceTableManager`；更新 `AppearanceClientTests.cs`、`AppearanceChronicle.proj`/`Program.cs` 与 `AppearanceUiVerification.cs`；执行 `client_compile_check.ps1`、Unity EditMode 单测、`AppearanceUiVerification.CaptureAll` 截图验收与 `AppearanceChronicle.proj` 检查。
5. 输出 `AppearanceSeriesV1` 完整 UI 美术切图与 ChatGPT 生成细节清单。
