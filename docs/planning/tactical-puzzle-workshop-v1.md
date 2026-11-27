# 《五行奇谈》万象残局坊（UGC 玩家自创回合残局工坊）V1 策划与技术设计方案

> **上位红线**：[项目设定 project-vision.md](project-vision.md)（严格遵守全部七条红线与五问准入自检）  
> **配套 UI 作图交接**：[tactical-puzzle-workshop-v1-ui-handoff.md](ui-handoff/tactical-puzzle-workshop-v1-ui-handoff.md)  
> **归属与对账**：C++/Go 权威版 + Java 副服 + Unity 客户端全端同步实现，零玩家存盘 Schema 扩展（完全归属 Match 权威库与战斗引擎）。

---

## 一、需求分析

### 1.1 玩家痛点与核心价值
- 归元城 NPC 齐问锋目前仅有 3 关策划写死的固定残局（Dungeon `311–313`），玩家 5 分钟即可打完，无法承载《五行奇谈》极其丰富的回合战斗深度（五行相克、破盾法宝、技印共鸣、驱散护势、控场断疗、先手卡速、人宠协同）。
- 毕业后或碎片时间（5–15 分钟）上线时，玩家缺少一种 **0 养成门槛、0 社交压力、高智力正反馈、可发群炫耀** 的轻量化核心玩法。
- 萌新在进入正式论剑排位与高难首领试炼前，缺少直观可玩的“高阶战术示范库”。

### 1.2 核心功能闭环（最小完整版 V1）
1. **公平摆盘出题（无养成差）**：
   - 作者自由设定残局题面：标题（4–20 字）、可选提示（0–40 字）、战术标签（最多 2 项）、逻辑回合上限（`1..6` 回合）、己方固定公平人宠预设（人物 `101 破阵 / 102 控场 / 103 守心`、灵宠 `0 无宠 / 201 破甲 / 202 灵咒 / 203 护主`、五行阵法 `0..5`）、己方开局血/蓝百分比（血 `15..100%`、蓝 `10..100%`）、己方开局护盾（`0..60%`）与开局负面状态（无/减攻/破防/玄毒），以及敌方 `1..3` 名残局怪（取自白名单 `311..316`，各自可配血量 `10..100%`、护盾 `0..80%`、速度微调 `-30..+30%`、开局增益「无/固防/疾行/蓄势增伤/持续回春」）。
   - 全服所有玩家（无论 1 级或满级）攻关同一道残局时，均使用**完全一致的公平预设面板 + 同一份确定性随机种子**，100% 拼战术决策而非数值碾压或赌脸暴击。
2. **「自证可解」发布铁律**：
   - 新建/修改后的残局处于 `DRAFT`（草稿）态，不可上榜、不可被他人挑战或分享。
   - 作者必须点击「亲自验题」进入真实战斗，在 `round_limit` 回合内亲手击败全部敌人（不逃跑），系统于终局 SQL 事务内自动将题目转为 `PUBLISHED`（已发布），并锁定该局回合录像作为**官方标准解录像**（绑定 `match_battle_report.ref_count`）。
   - 若作者后续修改已发布残局的题面配置，版本号 `spec_revision` 递增并强制退回 `DRAFT`，必须重新自证通关方可再次发布（杜绝“简单局骗发布后偷改无解死局”）。
3. **大厅榜单、攻关与一键分享**：
   - 残局坊主界面支持 4 个视角页签：**精选热门**（按赞数/通关热度）、**最新奇局**（按发布时间）、**烧脑难关**（按低通关率筛选，至少 3 次尝试入榜防噪）、**我的残局**（本人草稿/已发布/已通关）。
   - 顶部展示个人纯粹荣誉档案：累计破局数、创作获赞数与对应荣誉称谓（`初窥门径 1题` / `弈棋能手 5题` / `破局国手 15题` / `万象奇师 30题`），**零战力属性、零每日打卡**。
   - 挑战成功自动记录个人首次通关、最快回合数，刷新全服最小回合纪录（展示保持者名），并解锁**点赞该题**（非作者本人且已通关，每人每题限 1 次幂等）。
   - 支持**一键查看作者标准解录像**（复用现有战斗回放播放器），卡关时可看思路复盘学习。
   - 支持**一键发聊天分享卡片 `#PUZ:<puzzle_id>#`**（零聊天库改造，完全沿用试炼志 V3 `#TJC:<card_id>#` 链路），帮会/好友/世界频道内直接展示卡片并一键「挑战残局」或「看标准解」。

### 1.3 上位红线五问准入自检（全部通过）
1. **普通零氪 / 反「上班」**：无任何每日次数、无签到、无日清活跃、无定时在线、无缺勤血亏；1 级零氪玩家与满级玩家 100% 同面板体验全部内容。
2. **个人开发者收入**：不侵蚀官方 7:3 商业化（玩家高粘性留在游戏内并反哺论剑/试炼/外观与轻便利消费），零额外客服与资金风险。
3. **物价与长期循环**：完全复用 `kBossTrialKindTacticalPuzzle = 10` 隔离规则，**0 经验、0 银两、0 材料、0 掉落、0 捕宠、0 可交易货源**，绝不向经济系统注入一分泡沫。
4. **防骗与资产安全**：无任何交易/押注/银两对赌逻辑，纯战前固定预设快照，绝不扣减玩家真实血蓝或消耗品。
5. **商店过审与 UGC 合规**：标题（4–20 字）与提示（0–40 字）提交时强制走既有 `ContentGuard`（`CheckText` fail-closed，不可用/拒绝直接拦回），无自由图片上传、无外链、无诱导文案；账号注销时 `RoleFencePurge` 原子清理本人残局、通关/点赞记录并释放录像引用计数。

---

## 二、设计分析

### 2.1 复用优先与单一事实源架构（Actor 边界清晰）
- **权威状态归属 `Match` 服务（Go `go/match` & Java `game-match`）**：
  - 残局元数据、版本、统计与个人破局/点赞记录全量落在 `Match` 权威 SQL 库（仅新增 **2 张表**），零修改 `player_database` / `PlayerState` Schema（省去跨服务台账迁移负担）。
  - 复用 `Match` 既有 `BattleIDGen`（永久 ID）、`ContentGuard`（UGC 文本审校）、`match_battle_report`（`ref_count` 引用计数锁定作者标准解录像）、统一单人 `PVE_SOLO` gather 管线与 `RoleFencePurge` 删角清理。
- **战斗执行归属 `Battle` 引擎（C++ `TurnBattleEngine` & Java `TurnBattleEngine`）**：
  - 通过 `CreateBattleRequest.tactical_puzzle_context` 下发冻结的 `TacticalPuzzleBattleContext`，在 `InitPlayers` / `InitMonsters` 阶段按确定性规则构造残局开局态，战斗内伤害/破盾/驱散/控制/回春公式 **100% 复用现役引擎，零魔改分支**。
- **场景隔离归属 `Scene`（零新逻辑）**：
  - 配表新增 `Dungeon 314`（`boss_trial_kind = 10`, `scene_id = 0`, `max_team_size = 1`, `allow_pet_capture = 0`, `round_limit = 6`）与 `PlayMode 475`（`match_mode = 4 [PVE_SOLO]`, `battle_config_id = 314`, `team_size_max = 1`）。
  - 因 `boss_trial_kind = 10` 已在双服 `Scene` 与 `Battle` 接入完整隔离（0 经验/0 掉落/0 材料/0 赛季/0 游历点/0 血蓝回写），且 `scene_id = 0` 允许从任意地图或聊天卡片直接开战，**Scene 仅需复用 `RankedPreset` 免费快照路径（并支持 `pet_preset_id == 0` 单人无宠出战）即可零侵入闭环**。

### 2.2 协议契约设计

#### A. 战斗上下文与结算回传（`proto/battle/battle_data.proto` & `battle_node.proto` & `player_battle.proto`）
在 `proto/battle/battle_data.proto` 末尾追加：
- `TacticalPuzzleEnemySpec`：
  - `uint32 monster_table_id = 1`（白名单 `311..316`）
  - `uint32 hp_pct = 2`（`10..100`）
  - `uint32 initial_shield_pct = 3`（`0..80`）
  - `int32 speed_delta_pct = 4`（`-30..+30`）
  - `uint32 initial_buff_kind = 5`（`0=无, 1=固防[100140], 2=疾行[100160], 3=蓄势增伤[100250], 4=持续回春[110070]`）
- `TacticalPuzzleSpec`：
  - `uint32 round_limit = 1`（`1..6`）
  - `uint32 character_preset_id = 2`（白名单 `101..103`）
  - `uint32 pet_preset_id = 3`（白名单 `0` 或 `201..203`）
  - `uint32 formation_id = 4`（`0..5`）
  - `uint32 player_hp_pct = 5`（`15..100`）
  - `uint32 player_mp_pct = 6`（`10..100`）
  - `uint32 pet_hp_pct = 7`（`15..100`）
  - `uint32 pet_mp_pct = 8`（`10..100`）
  - `uint32 player_initial_shield_pct = 9`（`0..60`）
  - `uint32 player_initial_debuff_kind = 10`（`0=无, 1=减攻[100031], 2=破防[130051], 3=玄毒[100190]`）
  - `repeated TacticalPuzzleEnemySpec enemies = 11`（`1..3` 项）
- `TacticalPuzzleBattleContext`：
  - `uint64 puzzle_id = 1`
  - `uint32 spec_revision = 2`
  - `bool is_author_verify = 3`
  - `string title = 4`
  - `TacticalPuzzleSpec spec = 5`
- 关联消息追加字段（严守字段号只追加不复用）：
  - `CreateBattleRequest` 追加 `TacticalPuzzleBattleContext tactical_puzzle_context = 24;`
  - `BattleStateS2C` 追加 `TacticalPuzzleBattleContext tactical_puzzle_context = 43;`
  - `BattleSettlementData` 追加 `TacticalPuzzleBattleContext tactical_puzzle_context = 57;`

#### B. 客户端 RPC（`proto/match/match_service.proto`，消息号 `574..580`）
1. `ListTacticalPuzzles`（`574`）：按分类（`1=精选热门, 2=最新奇局, 3=烧脑难关, 4=我的创作, 5=我已破局`）与可选 `tag_mask` 筛选返回最多 30 条 `TacticalPuzzleSummary` + 个人荣誉统计 `TacticalPuzzleProfileView`（累计破局数、创作数、累计获赞数、荣誉档位）。
2. `GetTacticalPuzzle`（`575`）：按 `puzzle_id` 返回单题完整详情 `TacticalPuzzleDetail`（含 `spec`、作者、统计、个人通关/点赞状态；草稿仅作者本人可见）。
3. `SaveTacticalPuzzleDraft`（`576`）：新建（`puzzle_id=0`）或编辑（`puzzle_id>0`）草稿；执行全量白名单范围校验、`ContentGuard` 标题/提示审校、每玩家上限 10 题校验；编辑已发布题时 `spec_revision++`、状态回落 `DRAFT` 并释放旧标准解录像引用。
4. `DeleteTacticalPuzzle`（`577`）：作者软删/物理删本人残局，同事务清理相关通关记录并递减 `verified_battle_id` 对应 `match_battle_report.ref_count`。
5. `StartTacticalPuzzleBattle`（`578`）：入参 `(puzzle_id, is_author_verify)`；校验身份与状态（验题限作者本人；攻关限 `PUBLISHED`），生成固定 `puzzle_seed` 驱动的单人 `PVE_SOLO` 战斗，走既有 `gather` 管线开战。
6. `LikeTacticalPuzzle`（`579`）：入参 `(puzzle_id, liked)`；强验题目为 `PUBLISHED`、调用者非作者本人、且本人已有通关记录；幂等更新 `liked` 与 `like_count`。
7. `GetTacticalPuzzleSolutionReplay`（`580`）：入参 `(puzzle_id)`；强验题目为 `PUBLISHED` 且 `verified_battle_id != 0`（或作者查本人已验题草稿），返回 `BattleReplayEnvelope`（与 `GetSharedTrialReplay` 同构）。

#### C. 权威存储表（`proto/match/match_authority.proto`，2 张表）
- `MatchTacticalPuzzleRecord`（表名 `match_tactical_puzzle`，主键 `puzzle_id`）：存储题面序列化、版本、状态、固定 `puzzle_seed`、作者信息、标准解 `verified_battle_id` 及统计字段。
- `MatchTacticalPuzzleClearRecord`（表名 `match_tactical_puzzle_clear`，主键 `(puzzle_id, player_id)`）：存储单玩家单题尝试次数、是否破局、个人最快回合、是否点赞及时间戳。

#### D. 错误码（`data/tip/Tip.xlsx`，`match_error` `16064..16072`）
- `16064 kMatchPuzzleNotFound`：残局不存在或已下架
- `16065 kMatchPuzzleNotOwner`：仅残局作者本人可执行此操作
- `16066 kMatchPuzzleLimitReached`：每人最多保存 10 题残局，请先删除旧题
- `16067 kMatchPuzzleSpecInvalid`：残局配置超出允许范围，请检查后重试
- `16068 kMatchPuzzleTextRejected`：标题或提示包含不适宜内容，请修改后重试
- `16069 kMatchPuzzleNotPublished`：该残局尚未通过作者验题发布
- `16070 kMatchPuzzleCannotLikeOwn`：不能给自己创作的残局点赞
- `16071 kMatchPuzzleClearRequiredToLike`：通关该残局后方可点赞
- `16072 kMatchPuzzleSolutionReplayNotFound`：该残局的标准解录像暂不可用

### 2.3 设计模式与工程约束应用
- **策略/规范校验器（Specification Pattern）**：将残局题面合法性规则抽为纯函数校验器（Go `puzzle/spec.go`、Java `TacticalPuzzleSpecValidator`、C++ `tactical_puzzle_rules.h`、Unity `TacticalPuzzleModel`），四端使用完全一致的白名单常量与边界检查，消除多处散落 `if` 分支。
- **确定性场景装配（Builder / Snapshot Pattern）**：`TurnBattleEngine` 中把残局开局装配收敛到独立纯辅助函数 `ApplyTacticalPuzzleSetup`，仅在 `request.has_tactical_puzzle_context()` 时生效，与常规 PVE/PVP 初始化零耦合。
- **跨模块 Actor / 单向数据流**：客户端严格拆分为 `TacticalPuzzleClient`（通信与限流）、`TacticalPuzzleModel`（纯 C# 无 `UnityEngine` 依赖，承载校验/筛选/聊天卡片解析 `#PUZ:<id>#`/荣誉称谓推导）、`TacticalPuzzleWindow` + `TacticalPuzzleUiRoot`（uGUI 视图与对称退订）。

---

## 三、思想分析

1. **为什么锁定「固定预设 + 固定种子」而非让玩家带自己装备打残局？**
   - 如果允许带自己养成面板打残局，满级顶配玩家一刀秒杀，题面根本无法设计出“精妙解法”，也会让新手望而却步；固定公平预设 + 固定种子让每一道残局都变成**纯粹的象棋残局**——不管谁来解、什么时候来解，每一步都可复盘、可讨论、可优化回合数。
2. **为什么采用「先验题通关再发布」而不是“直接发布 + 举报下架”？**
   - UGC 关卡最大的灾难是 90% 的垃圾/无解题淹没好题。强制作者亲手打赢一局才能发布，既是天然防垃圾闸门，又顺手免费产出了“官方标准解录像”，一举两得。
3. **为什么不设每日破局奖励或银两赏金？**
   - 一旦挂钩每日收益或银两，就会立刻催生“1 回合秒杀刷子福利局”互刷搬砖，直接踩爆反「上班」与防刷红线。彻底剥离资源产出、只保留纯粹智力荣誉称谓 + 排行榜 + 社交分享，才能守住“好玩”本心。

---

## 四、漏洞分析与防线设计

| 风险 / 漏洞场景 | 攻击或异常手法 | 权威防线（Fail-Closed） |
| --- | --- | --- |
| **1. 偷换题面绕过自证** | 先用 1 血残敌验题发布，再把敌方改成 3 满血高盾死局 | `SaveTacticalPuzzleDraft` 修改已发布题面时原子 `spec_revision++`、`state=DRAFT`、清空并释放旧 `verified_battle_id`；验题终局结算强验 `battle.spec_revision == row.spec_revision` |
| **2. 越权窥探他人私密录像** | 伪造 `GetTacticalPuzzleSolutionReplay` 拉任意 `battle_id` | 接口入参只接受 `puzzle_id` 不接受 `battle_id`；服务端仅从已发布行读取 `verified_battle_id` 再查 `match_battle_report` |
| **3. 暴力刷赞 / 小号自赞** | 作者自赞或未通关连点点赞刷榜 | `LikeTacticalPuzzle` 强验 `player_id != author_player_id` 且 `(puzzle_id, player_id)` 存在 `cleared=true` 记录；同 `(puzzle_id, player_id)` 主键幂等 |
| **4. 违规标题 / 刷库灌水** | 标题夹带广告/违禁词，或脚本建上万题撑爆库 | 标题/提示全部强制走 `ContentGuard`（不可用直接拒收）；每人上限 10 题（锁 `author_player_id` 角色护栏事务计数）；`MessageLimiter.xlsx` 限频 |
| **5. 录像表膨胀** | 10 万次普通挑战全录回放塞满 `match_battle_report` | `battle_replay_policy` 仅对 `is_author_verify == true` 开启 `record_replay = true`；普通挑战者局 `record_replay = false` |
| **6. 删角残留孤儿数据/泄漏引用** | 作者删角后残局与录像引用长期残留 | `RoleFencePurge` 同事务删除该作者全部残局及所有个人通关/点赞行，并按 `battle_id` 升序调用 `releaseBattleReportsTx` 释放 `ref_count` |
| **7. 超回合卡死 / 非法参数** | 伪造 `round_limit=999`、`hp_pct=0`、`monster_table_id=101`(高奖励首领) | 四端共用白名单强校验：`round_limit∈[1,6]`、`hp_pct∈[10,100]`、`monster_table_id∈[311,316]`（0 掉落/0 经验）、`enemies∈[1,3]`；引擎与 Scene 双重隔离零资产写回 |
| **8. 回合耗尽错判平局/胜利** | `round_limit` 打满后敌方仍存活却被当成通关 | 残局模式回合耗尽仍有存活敌人时权威判 `BATTLE_OUTCOME_SIDE_B_WIN`（攻关失败），仅全灭敌方判 `SIDE_A_WIN` |
| **9. 单人无宠开局崩溃** | `pet_preset_id == 0` 时 `Scene` 误按必选宠拒开局 | `Scene`（C++/Java）在 `tactical_puzzle` 模式下允许 `pet_preset_id == 0` 跳过宠物生成，仅填充人物预设快照 |
| **10. 小游戏/App 双形态崩溃** | 客户端用线程/阻塞 IO 或未退订事件 | Unity 客户端纯主线程单向数据流、`OnDisable/OnDestroy` 对称退订、动态文本关 `richText`、预留右上小游戏胶囊避让与 ≥44px 触控热区 |

---

## 五、步骤分析

1. **阶段 1（策划与 UI 交接落盘）**：更新 `project-vision.md` 三类明确禁区；落盘本策划案与 `tactical-puzzle-workshop-v1-ui-handoff.md`；更新 `docs/README.md`。
2. **阶段 2（契约与配表生成）**：更新 `battle_data.proto`、`battle_node.proto`、`player_battle.proto`、`match_service.proto`、`match_authority.proto`；补齐 `Dungeon 314`、`PlayMode 475`、`Tip 16064..16072`、`MessageLimiter 574..580`；跑 `dev.bat gen` 生成 C++/Go/客户端协议与配表。
3. **阶段 3（C++ 战斗/场景 + Go Match 权威服）**：
   - C++ `battle`：`tactical_puzzle_rules.h` + `TurnBattleEngine` 残局开局装配、回合耗尽判负、结算/快照回显 + 引擎单测；
   - C++ `scene`：`player_battle.cpp` 支持 `pet_preset_id == 0` 免费残局预设快照；
   - Go `match`：`authority/tactical_puzzle.go`（2 表 CRUD + 自证发布 + 破局统计 + 点赞 + 录像引用 + `RoleFencePurge`）、`logic/tactical_puzzle_logic.go`（7 RPC + `ContentGuard` + 固定种子单人 PVE 开战）、`battle_replay_policy.go`（验题录播）、`server/matchserviceserver.go` + 全套单测。
4. **阶段 4（Java 副服对账）**：`ContractSync` 同步契约与配表；`game-battle-engine`、`game-battle`、`game-scene`、`game-match`、`game-gate` 同构落地并通过 Maven 测试，登记 `PARITY.md`。
5. **阶段 5（Unity 客户端与 UI 接线）**：`TacticalPuzzleModels.cs`、`TacticalPuzzleClient.cs`、`TacticalPuzzleUiArt.cs`（优雅回退支持 Codex 10 张切图零改代码生效）、`TacticalPuzzleWindow.cs`、`TacticalPuzzleUiRoot.cs`、集成归元城 NPC 齐问锋入口 + `SocialWindow` `#PUZ:<id>#` 互动卡片 + `BattleScreen` 残局回合倒计时/结算重挑与分享条 + `TacticalPuzzleClientTests.cs`。
6. **阶段 6（全端验证闭环）**：低档模型运行 C++/Go/Java/Unity 编译与自动化单测；Codex 交付 10 张透明切片后一键导入并验绿。

---

## 六、执行步骤清单

- [x] 更新 [project-vision.md](project-vision.md) 明确写入三类禁止扩展方向（家园种菜、固定钟点世界 BOSS/大乱斗、新纵向数值养成线）
- [x] 落盘 [tactical-puzzle-workshop-v1.md](tactical-puzzle-workshop-v1.md) 与 [tactical-puzzle-workshop-v1-ui-handoff.md](ui-handoff/tactical-puzzle-workshop-v1-ui-handoff.md)
- [ ] 更新协议（`battle_data` / `battle_node` / `player_battle` / `match_service` / `match_authority`）与配表（`Dungeon 314` / `PlayMode 475` / `Tip 16064..16072` / `MessageLimiter 574..580`），执行 `dev.bat gen`
- [ ] 实现 C++ `battle` / `scene` 残局装配与无宠预设支持，跑通 C++ 单测
- [ ] 实现 Go `match` 残局坊权威表、7 条 RPC、自证发布、录像引用与 `RoleFencePurge`，跑通 Go 单测
- [ ] 同步 Java 副服 `ContractSync` + `game-battle-engine` / `game-battle` / `game-scene` / `game-match` / `game-gate`，跑通 Maven 单测并登记 `PARITY.md`
- [ ] 实现 Unity 客户端 `TacticalPuzzle*` + 齐问锋入口 + `#PUZ:` 聊天卡片 + 战斗/结算联动 + EditMode 单测，跑通编译与测试
- [ ] 验收并导入 Codex 交付的 10 张 `tactical-puzzle-workshop-v1` 切片
