# 《论剑 / 人宠会武》V2 Unity 客户端 UGUI 补全与新交互交接单（Codex 专供）

> 日期：2026-11-25  
> 适用对象：另一台电脑负责 `mmorpg-client` Unity UGUI 实现与视觉打磨的 Codex 代理  
> 权威上位设计：[ranked-play-v2-overhaul.md](../ranked-play-v2-overhaul.md) · [season-pvp-lunjian-v1-repaired.md](../season-pvp-lunjian-v1-repaired.md)  
> 服务端与客户端非 UI 状态：**C++/Go 权威服、Java 独立服、`mmorpg-client` 非 UI 网络/模型/单测已全部落码并通过编译与单测；本交接单只含 UGUI 表现层补全与新控件，不要求改动任何服务端或协议文件。**

---

## 一、交接范围与严禁触碰红线

### 1.1 已就绪底座（直接调用，禁止重复造轮子）
以下非 UI 网络层、Proto、消息号与数据模型已在 `mmorpg-client` 落码并过编（`client_compile_check.ps1`: 718 文件 0 错误，纯 C# NUnit 27/27 PASS）：
- **Proto 与消息号**（`Assets/Scripts/Net/MessageIds.cs`、`Assets/Scripts/Generated/Proto/`）：
  - `MessageIds.SurrenderBattle = 479`（直连 battle 节点，已接入 `DirectRoutingBattleTransport.IsDirectMessage` 与 `RpcGuard` `"battle"` 桶）；
  - `MessageIds.NotifySurrenderVote = 480`（同队单播，敌方与观众永不收到）；
  - `BattleEventItem.IsPrimaryFocus`（字段 `27`）；
  - `BattleSettlementData.Surrendered`（字段 `32`）；
  - `BattleStateS2C.RoundLimit`（`21`）、`EscalationStacks`（`22`）、`EscalationStartRound`（`23`）、`SurrenderUnlockRound`（`24`）、`SurrenderVote`（`25`）；
  - `RankedSeasonInfo.TierQingfengCenti`（`24`）、`TierYunzhangCenti`（`25`）、`TierTianshuCenti`（`26`）、`TierWushuangCenti`（`27`）、`MilestoneWinMatches`（`28`）；
  - `RankedRewardKind.RankedRewardWinMilestone = 7`（6 胜「锋芒」永久称号，可装入 `UpdateRankedCosmetic.EquippedTitleCode`）；
  - 新增 4 个 `match_error` Tip：`16052`（本局不可投降）、`16053`（前 4 回合不可投降）、`16054`（投降冷却中）、`16055`（本轮已表决）。
- **`BattleClient`（`Assets/Scripts/Game/Battle/BattleClient.cs`）**：
  - 属性：`RoundLimit`、`EscalationStacks`、`EscalationStartRound`、`SurrenderUnlockRound`、`CurrentSurrenderVote`（`BattleSurrenderVoteState`）、`CanSurrenderNow`、`LastBattleEnd`；
  - 事件：`event Action<BattleSurrenderVoteState> OnSurrenderVoteChanged`；
  - 方法：`bool SurrenderBattle(bool agree = true, Action<bool> completed = null)`；
  - 行为：`AckBattleEnd()` 已对正式排位 / 物法易势 5V5 / 赛季锦标赛禁止旧 `ContinuousBattle` 自动续排。
- **`RankedModels` & `RankedClient`（`Assets/Scripts/Game/Ranked/`）**：
  - `RankedProfileSnapshot`：`RuleVersion`（缺省 `2u`）、`TierQingfengCenti` / `TierYunzhangCenti` / `TierTianshuCenti` / `TierWushuangCenti`、`MilestoneWinMatches`、`UsesOwnProgression`、`AllowedTeamSizes`、`GetNextTierHint()`、`GetWinMilestoneHint()`；
  - `RankedFormatting`：`RewardKindName(RankedRewardWinMilestone) -> "锋芒称号"`、`NextTierHint(...)`、`WinMilestoneHint(wins, targetWins)`、`ActivePetWarning(activePetId)`、`TierBadgeKey(tier)`、`TierDisplayName(tier, qualified)`；
  - `RankedClient`：已补全 `BeginSettlementFromRecovery` 的 `MaterialTradableUnitsAdded` 与 `MaterialPvpLossIneligible` 透传，并映射 `16052..16055` Tip 文案。

### 1.2 严禁触碰红线（务必遵守）
1. **禁止回退或覆盖并行会话改动**：工作区存在其他玩法（核心循环 UGUI、瑞兽迎春、灵宠翻牌、群仙问道赛、易容人物志）的未提交改动；只编辑本清单点名的 UI 文件，禁止 `git checkout` / `git reset`。
2. **禁止在客户端本地反推段位或写死旧阈值**：段位名称与徽章**必须**读服务端 `CurrentProgress.CurrentTier` 与 `Qualified`，距下段文案必须调用 `profile.GetNextTierHint()`（权威门槛 `1520 / 1600 / 1720 / 1850`，新号 1500 在「初试」）。
3. **禁止给正式会武 `FormalPvpLobbyPanel` 混入免费 3×3 预设**：免费 6 套预设只允许出现在 `SeasonRotationPanel`（护卫救援 / 物法易势 5V5）。
4. **禁止遮挡战斗舞台点击与确认按钮**：战斗内叠加层（战意渐盛徽章、投降表决浮条）除按钮本身外一律 `raycastTarget = false`，严禁拦截单位点选、右下命令环与中下 Confirm/Cancel 确认条。
5. **禁止改 `BattleUiStyle.Pvp1V1BattleConfigId = 1`**：该常量服务 `MATCH_MODE_1V1` 机器人冒烟测试；正式会武已独立定义为 `BattleUiStyle.PvpTeamBattleConfigId = 5`。
6. **禁止执行 `git commit` / `git push`**，所有新增 `.cs` 脚本若放进 Unity 目录须按规范配对 `.meta`（或尽量复用现有文件修改减少新建文件）。

---

## 二、当前实盘已完成项（勿重复返工）

核查 `mmorpg-client` 当前代码后，以下项目**已经完成**，Codex 无需重写：
- `FormalPvpLobbyPanel.cs` 已具备 0–119 秒实时排队计时 / 分差 / 取消匹配 / 120 秒切单人首领试炼状态条，且第 158 行出战宝宝「查看」按钮已指向 `_root.OpenPets`。
- `SeasonRotationPanel.cs` 第 43–45 行已接入 `CoreGameplayLoopModels.TryResolveAuthoritativeSeasonWeek(_game.Season?.Snapshot, out uint authoritativeWeek)`。
- `BattleClient.AckBattleEnd()` 已屏蔽排位/物法易势/锦标赛的旧连续战斗自动重排。

---

## 三、待实现 UGUI 任务清单（按优先级排序）

### 任务 A（P0）：正式人宠会武大厅增强（`FormalPvpLobbyPanel.cs` + `RankedSharedDrawer.cs`）
**目标文件**：
- `Assets/Scripts/UI/Ugui/CoreLoop/FormalPvpLobbyPanel.cs`
- `Assets/Scripts/UI/Ugui/Ranked/RankedUiRoot.cs`
- `Assets/Scripts/UI/Ugui/Ranked/RankedSharedDrawer.cs`

**具体要求**：
1. **接入段位荣誉条与 `RankedSharedDrawer` 入口**（复用 `Assets/Resources/UI/Ugui/LunjianV1/` 已有 47 张精绘素材）：
   - 在 `FormalPvpLobbyPanel` 顶栏/概览区增加紧凑段位信息带：
     - 服务端段位徽章（`RankedFormatting.TierBadgeKey(progress.CurrentTier)`）+ 段位名（`RankedFormatting.TierDisplayName(...)`）+ 整数积分（`RankedFormatting.FormatRating(progress.RatingCenti)`）；
     - 距下段提示：直接展示 `profile.GetNextTierHint()`（例：「距青锋还差 20 分」/「完成 1 场有效对局即可亮段定级」）；
     - 双里程碑短进度：参与进度 `有效场次 {ValidMatches}/12`（`RankedFormatting.NextMilestoneHint(ValidMatches)`）+ **胜场进度 `profile.GetWinMilestoneHint()`**（例：「再赢 4 场解锁『锋芒』称号（2/6）」）；
     - 三个直达按钮：**「排行榜」**、**「赛季记录」**、**「玉符与称号」**，点击分别调用 `RankedUiRoot.Instance` 打开 `RankedSharedDrawer` 对应页签（`Leaderboard` / `History` / `Cosmetics`），不再让 47 图与三页签抽屉失联。
2. **未出战灵宠战前防呆提醒**：
   - 在 `DrawOwnProgressionSummary()` 中，当 `_game.Pets?.Pets != null && _game.Pets.Pets.ActivePetId == 0` 时：
     - 宠物概览卡文字高亮警告色并显示 `RankedFormatting.ActivePetWarning(0)` 摘要（如「未出战灵宠 · 将少 1 单位」）；
     - 点击「开始匹配」若 `ActivePetId == 0`，先弹轻量二次确认弹窗：主按钮「前往出战灵宠」（调用 `_root.OpenPets()`）、次按钮「仍单人出战」（放行 `_core.EnterFormalPvp(_selectedSize)`）。
3. **`RankedSharedDrawer` V2 适配**：
   - 「玉符与称号」页签支持展示并佩戴/卸下 **`RankedRewardKind.RankedRewardWinMilestone`**（6 胜「锋芒」永久称号，项码 `{seasonKey}_win_milestone_6`，写入 `UpdateRankedCosmeticRequest.EquippedTitleCode`）以及「云章 / 天枢 / 无双」三档封榜段位徽章与赛季称号；
   - 清理抽屉中写死的 `"五档人数共用"` 文案，改为按 `profile.AllowedTeamSizes` 动态展示开放档位；
   - 名片预览优先用当前角色真实形象（`QdaoCharacterCatalog`），历史/赛季 `StartMs / EndMs` 转本地时间（`DateTimeOffset.FromUnixTimeMilliseconds((long)ms).ToLocalTime().ToString("yyyy-MM-dd HH:mm")`）。

---

### 任务 B（P0）：娱乐轮换页 4 技能预设详情与物法易势排队条（`SeasonRotationPanel.cs`）
**目标文件**：`Assets/Scripts/UI/Ugui/CoreLoop/SeasonRotationPanel.cs`

**具体要求**：
1. **V2 6 套 4 技能预设详情卡 / 浮层**：
   - 当前 6 张预设卡仅显示 `preset.Name` 与 `preset.RoleTag`，补一个「查看招式」按钮或卡片下方折叠摘要区，展示服务端 `preset.SkillIds` 全部 **4 个技能**（经本地 `SkillTableManager` 取中文名 + 简短定位；缺失表行时回退显示技能 ID）：
     - `101 破阵`（爆发破疗）：横戈裂岳 / 天崩裂岳 / 战意昂扬 / **断脉咒**
     - `102 控场`（全封映法）：紫霄雷阵 / **封灵咒** / 驱灵散势 / **玄镜映法**
     - `103 守心`（奶盾复苏）：青莲润生 / 清心荡秽 / 独立护盾 / **生生不息**
     - `201 破甲·金猊`（碎甲提速）：横戈裂岳 / 碎甲击 / **青羽催行** / **裂甲追击**（条目 501×20）
     - `202 灵咒·灵狐`（毒法回响）：紫霄雷阵 / 蚀骨毒 / 驱灵散势 / **灵术回响**（条目 502×10 / 531×10）
     - `203 护主·石灵`（双罩保核）：灵鼎护元 / 青莲润生 / **神龙护体（物罩）** / **如意灵环（法罩）**（条目 503×10 / 504×10）
   - 选中不同预设时底部同步刷新预览，无需额外 RPC。
2. **「物法易势 5V5」排队与取消状态条**：
   - 当 `_game.Battle?.Phase == BattlePhase.Queued` 时，在 `SeasonRotationPanel` 底部渲染与 `FormalPvpLobbyPanel` 同口径的排队进度条（已等时间、取消匹配按钮、取消中禁点、满 120 秒转单人试炼兜底），避免进入 5V5 娱乐排位后在轮换页看不到倒计时与取消按钮。

---

### 任务 C（P0）：战斗内 V2 爽点反馈与 PVP 延迟投降 UI
**目标文件**：
- `Assets/Scripts/UI/Ugui/CoreLoop/BattleCoreHudOverlay.cs`
- `Assets/Scripts/UI/Ugui/Battle/BattleHud.cs`
- `Assets/Scripts/UI/Ugui/Battle/BattleScreen.cs`
- `Assets/Scripts/UI/Ugui/Battle/BattlePresenter.cs`
- `Assets/Scripts/UI/Ugui/Battle/DamageNumber.cs`

**具体要求**：
1. **显式回合上限展示（`BattleHud` / `BattleScreen`）**：
   - 当 `_battle.RoundLimit > 0` 时，顶部回合数展示为 **`第 {RoundIndex} / {RoundLimit} 回合`**（正式会武 `1/20`，物法易势 `1/16`）；当 `RoundLimit == 0` 时维持原 `第 {RoundIndex} 回合`。
   - 剩余 ≤ 3 回合时切警示态，最后一回合强警示。
2. **「战意渐盛」绝杀加速徽章（`BattleCoreHudOverlay`）**：
   - 数据源：`_game.Battle.EscalationStacks` 与 `_game.Battle.EscalationStartRound`。
   - 当 `EscalationStartRound > 0` 且当前回合 `RoundIndex == EscalationStartRound - 1` 时，显示预告小标签：`下回合开启「战意渐盛」`；
   - 当 `EscalationStacks > 0` 时，在顶部信息带（`nextY` 向下排布，严格避开计时环、左列与右上角色卡，`raycastTarget = false`）显示状态徽章：
     - 标题：`战意渐盛 · 第 {stacks} 层`
     - 副文：`伤害 +{stacks * 10}% · 治疗与新盾 -{Math.Min(80u, stacks * 10u)}%`
   - 每次 `EscalationStacks` 数值增加时给徽章加轻量缩放脉冲（`0.25s`）。
3. **主目标「凝锋 +20%」集火飘字（`BattlePresenter` + `DamageNumber`）**：
   - 播放 `TurnResultS2C.Events` 时，若 `ev.EventType == eBattleEventType.BattleEventDamage && ev.IsPrimaryFocus`：
     - 在目标头顶飘字带前缀短标 **`凝锋 -{value}`**（略放大 `1.15x`，与普通伤害区分），让玩家直观感知 1V1/残局点选主目标的 `+20%` 集火收益；
     - 副目标（`IsPrimaryFocus == false`）维持普通飘字样式。
4. **PVP 第 5 回合延迟投降按钮与队友表决条（`BattleHud` + `BattleScreen`）**：
   - **入口按钮（「认输 / 投降」小按钮，置于左上辅助区或自动战斗按钮旁，避开主技能键与小游戏胶囊）**：
     - `_battle.SurrenderUnlockRound == 0`（PVE / 首领试炼 / 护卫救援）：**完全隐藏**；
     - `RoundIndex < SurrenderUnlockRound`：置灰可见，点击 Toast 提示 `第 {SurrenderUnlockRound} 回合起可发起投降`；
     - `CurrentSurrenderVote != null && !CurrentSurrenderVote.Active && CurrentSurrenderVote.NextAllowedRound > RoundIndex`：置灰并显示冷却 `冷却中（R{NextAllowedRound}）`；
     - `_battle.CanSurrenderNow == true`：可点击。
   - **1V1 确认流（己方 `BattleActorType.Player` 仅 1 人）**：
     - 点击弹轻量二次确认弹窗：「确认体面认输？本局按正常战败结算，不计逃跑惩罚。」
     - 确认后调用 `_battle.SurrenderBattle(true)`，按钮置防连点直到回包或战斗结束。
   - **多人表决流（3V3 / 5V5，己方 ≥ 2 名玩家）**：
     - 订阅 `_battle.OnSurrenderVoteChanged`（退屏/销毁时务必对称退订）；
     - 无活跃表决时，点击「投降」→ 二次确认「向队友发起投降表决？」→ 调用 `_battle.SurrenderBattle(true)`；
     - 当 `CurrentSurrenderVote != null && CurrentSurrenderVote.Active` 时，在屏幕中上方或操作区上沿弹出**非模态紧凑投票浮条**（不遮挡舞台单位，不影响玩家继续下本回合指令）：
       - 文案：`队友发起投降表决 · 已同意 {AgreedPlayerIds.Count} / {RequiredAgrees}`（附小字：`连续 2 回合未手动队友不计入票数`）；
       - 若 `AgreedPlayerIds` 未包含 `MyPlayerId`：显示 **`[同意投降]`**（`_battle.SurrenderBattle(true)`）与 **`[继续战斗]`**（`_battle.SurrenderBattle(false)`）；
       - 若 `AgreedPlayerIds` 已包含 `MyPlayerId`：按钮替换为只读状态 `已同意，等待队友表决…`；
     - 收到 `Active == false && Passed == false && NextAllowedRound > RoundIndex` 时，浮条自动收起并 Toast `投降表决未通过（第 {NextAllowedRound} 回合后可再次发起）`。

---

### 任务 D（P0）：结算面板补全（`RankedSettlementPanel.cs`）
**目标文件**：`Assets/Scripts/UI/Ugui/Ranked/RankedSettlementPanel.cs`

**具体要求**：
1. **修复 `SetCoreSettlementVisible(bool visible)` 误藏 Card 1 与 Card 2**：
   - 当前第 109–113 行循环隐藏了全部 3 张 `_legacyRewardCards`；
   - 改为**仅隐藏 `_legacyRewardCards[2]`**（旧材料卡），保留 Card 1（本季有效场次 + 新增一行 `6 胜锋芒进度`）与 Card 2（本局新解锁展示外观，含 `RankedRewardWinMilestone`「锋芒称号」）。
   - 同时核对 `CoreSettlementRewardSection` 锚点位置，确保落在原 Card 3 区域（`x=1260, y=570, w=510, h=259` 或与之不重叠），不压住 Card 1/2。
2. **重登补弹路径旧文案替换**：
   - 将 Card 3 兜底文案中的旧 `今日共享材料自选额度已达 20 份上限` 替换为最新两阶段口径（`第一阶段永久 660 份 / 第二阶段每周 150 份全绑定`），当 `state.MaterialTradableUnitsAdded > 0` 时显式标出 `含可交易 +{n}`，当 `state.MaterialPvpLossIneligible == true` 时提示 `未达有效对抗门槛（需 ≥3 回合、≥2 手动且有有效贡献），本局不计材料`。
3. **体面投降标记**：
   - 若 `_game.Battle?.LastBattleEnd?.Settlement?.Surrendered == true`，在 `_metaLine` 追加 `· 体面认输（不计逃跑）`，避免玩家误以为按逃跑扣罚。

---

### 任务 E（P1）：安全区适配与战斗技能面板细节
**目标文件**：
- `Assets/Scripts/UI/Ugui/Ranked/RankedUiRoot.cs`
- `Assets/Scripts/UI/Ugui/Battle/BattleScreen.cs`
- `Assets/Scripts/UI/Ugui/Battle/BattleUnitView.cs`

**具体要求**：
1. **`RankedUiRoot` 安全区与微信/抖音小游戏右上角胶囊避让**：参照 `CoreGameplayUiRoot.ApplySafeArea` 补齐 `Screen.safeArea` 缩放与右上角胶囊避让。
2. **战斗技能面板细节（`BattleScreen` & `BattleUnitView`）**：
   - 宝宝技能面板加「主动攻击 / 障碍 / 辅助」显式分组头，条件追击/被动技能（如 `13005 裂甲追击`、`13006 灵术回响`）只读展示说明、不可点施放；
   - 主动技能按钮补齐**法力不足 / 被封灵 / 被封物 / 被封法 / 受控**的禁用态与原因提示（当前仅判冷却）；
   - `BattleUnitView` 对多条预告目标加清晰短字角标（如「重劈」「回春」「护卫」）而不止靠红/金圆点。

---

## 四、联调与静态校验清单（Codex 完成后自检）

1. **零错误编译**：在 `mmorpg-client` 根目录执行 `powershell -NoProfile -ExecutionPolicy Bypass -File tools/client_compile_check.ps1`，确认 `exit=0, errors=0`；
2. **现有纯 C# 单测全绿**：确保 `RankedClientTests`、`BattleClientAutoBattleTests`、`BattleHudLogicTests`、`BattleUiLayoutTests` 无编译或断言回归；
3. **事件对称订阅**：每个新订阅 `_battle.OnSurrenderVoteChanged` / `_game.Ranked.ProfileChanged` 的面板都在 `OnDestroy` 或 `Dispose` 中退订，并在断线/切号后清空残留浮条；
4. **分辨率与安全区**：核查 `1920×1080`、`2400×1080`（异形屏）与微信小游戏右上角 140px 胶囊区均无遮挡、无文字截断。

---

## 五、可直接复制给 Codex 的启动 Prompt

```text
请阅读仓库权威设计文档：
1. d:/luyuan/xuanming-server-mmo-cpp/docs/planning/ranked-play-v2-overhaul.md
2. d:/luyuan/xuanming-server-mmo-cpp/docs/planning/ui-handoff/ranked-play-v2-ui-handoff.md

服务端（C++/Go 与 Java）以及 mmorpg-client 的非 UI 层（Proto、MessageIds 479/480、DirectRoutingBattleTransport、RpcGuard、BattleClient 投降与战意渐盛属性/事件、RankedModels 五段门槛/6胜锋芒/出战宠提醒、RankedClient）已经全部实现并通过测试。

请严格按 ranked-play-v2-ui-handoff.md 中的【红线约束】和【任务 A ~ 任务 E】，在 mmorpg-client 完成所有 Unity UGUI 补全与 V2 新交互：
- 任务 A：FormalPvpLobbyPanel 接入段位/距下段/4-8-12/6胜锋芒进度与 RankedSharedDrawer 三页签入口（复用 LunjianV1 47 图）+ 无出战宠二次确认 + RankedSharedDrawer 支持 6 胜锋芒称号佩戴与云章以上荣誉展示与本地时间格式化；
- 任务 B：SeasonRotationPanel 补全 6 套 V2 4 技能预设招式详情查看 + 物法易势 5V5 排队计时/取消/120s 状态条；
- 任务 C：BattleHud/BattleScreen/BattleCoreHudOverlay/BattlePresenter/DamageNumber 接入「第 X/20 回合」上限展示、「战意渐盛」层数徽章与下回合预告、主目标「凝锋 +20%」飘字区分、以及 PVP 第 5 回合延迟投降按钮（PVE 隐藏、1V1 二次确认、多人非模态队友投票条 + 冷却与挂机不阻挡说明）；
- 任务 D：RankedSettlementPanel 修复 SetCoreSettlementVisible 仅隐藏 Card 3（保留 Card 1 有效场次/6胜进度与 Card 2 外观解锁）、更新重登补弹两阶段 660/周 150 文案、新增体面认输标签；
- 任务 E：RankedUiRoot 补齐 Screen.safeArea 与小游戏右上胶囊避让，以及 BattleScreen/BattleUnitView 宝宝技能分组/被封禁用态/多预告短字角标。

约束：
- 不要改动服务端代码、Proto 或其他并行玩法未提交文件；
- 完成后运行 tools/client_compile_check.ps1 确认 0 错误，并核对事件对称订阅、断线/切号状态清理及 .meta 配对；
- 不要执行 git commit 或 git push。
```
