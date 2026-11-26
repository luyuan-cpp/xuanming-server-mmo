# 《论剑 / 人宠会武》V4 扩展设计：队内一键战术标点、本局战术复盘徽记与连胜破势

> 日期：2026-11-25  
> 状态：**设计提案已落盘，待负责人拍板并在并行会话完成 `wuxing-gameplay-expansions-v1` 的 `proto/battle/*` 改动后立即实施代码；配套纯美术 UI 需求单可直接发给 Codex 先行出图**  
> 适用范围：`xuanming-server-mmo-cpp`（C++ `battle` / `scene` + Go `match`）、`xuanming-server-mmo`（Java 独立服同步）、`mmorpg-client`（Unity 客户端与 UGUI）、`D:/work/image/designs/ranked-play-v4-ui-20261125`（Codex 纯美术交付目录）  
> 关联设计：[ranked-play-v2-overhaul.md](ranked-play-v2-overhaul.md) · [ranked-play-v3-tactical-prep.md](ranked-play-v3-tactical-prep.md) · [wuxing-gameplay-expansions-v1.md](wuxing-gameplay-expansions-v1.md)（严格零重叠、零冲突）

---

## 一、需求分析

当前《论剑 / 人宠会武》V1 + V2 + V3 已完整打通 1V1–5V5 跨规模单分排位、6 套 4 技能娱乐轮换预设、凝锋 `+20%`、战意渐盛、校准五段与 6 胜「锋芒」称号、PVP 第 5 回合体面投降，以及战前 15 秒明牌变阵（换赛季印 / 换首发宠），双服与 Unity 终版帮会原图风格 UI 均已闭环。

继续放大 PVP 对局爽感与长线黏性，还有 **3 处零肝度、零数值破坏、但玩家体感极强的空白**：
1. **3V3 / 5V5 路人局缺乏极速沟通手段**：在手机触屏与微信/抖音小游戏 25 秒回合窗内打字几乎不可行，经常出现 A 打敌方人物、B 打敌方石灵、控场重复封同一目标的尴尬，大幅削弱组队论剑与多人试炼的配合爽感。
2. **战后缺少「我这一局打出什么名场面」的正向复盘反馈**：目前结算页仅展示胜负、Elo 变动、有效场次和材料额度；即便打出一场精彩的控场翻盘或极限保核，战后看不到自己的凝锋输出、护盾/治疗化解与关键控场驱散数据，输局更容易产生挫败感。
3. **高状态连胜缺少局内仪式感与「终结强者」的对抗高潮**：连胜 3 场、5 场以上目前只有静默数字变化，对手也不知道自己正在挑战一位连胜高手，少了一层回合制竞技天然的宿敌张力。
4. **顺手闭环 V2 微小尾项**：`PlayerBattleResult` 断线重登补弹补齐 `bool surrendered`，使投降后强退重登补弹与现场结算 100% 一致展示「体面认输（不计逃跑）」。

---

## 二、设计分析

### 2.1 模块一：队内一键战术标点（`BattleTacticalPing`，零文字沟通）
- **生效范围**：所有多人同队对局（正式 2V2–5V5 论剑、物法易势 5V5、群仙问道赛、多人首领试炼、护卫救援）；1V1 单人队隐藏入口，观战与回放不显示标点（或回放严格脱敏不记录私密队内标点）。
- **4 种战术标点（`enum BattlePingKind`）**：
  1. `BATTLE_PING_FOCUS = 1`：**「集火」**（可标敌方任意存活人物/灵宠/怪物，提示全队主目标触发凝锋集火）；
  2. `BATTLE_PING_CONTROL = 2`：**「封控」**（可标敌方目标，提示控场优先封灵/封法）；
  3. `BATTLE_PING_PROTECT = 3`：**「保核」**（可标己方任意存活单位，提示辅助优先治疗/护盾/复活印）；
  4. `BATTLE_PING_DISPEL = 4`：**「驱散」**（可标敌我目标，提示净化己方受控或驱散敌方护势/护盾）。
- **生效时机**：同时支持 **V3 战前 15 秒明牌变阵期** 与 **每回合 25 秒手动行动窗**。
- **防刷屏与确定性**：
  - 引擎内确定性限频：每名玩家每回合（含变阵期独立计 1 轮）最多提交 **2 次**标点，同一玩家的新标点自动覆盖自己的旧标点（即每名队友在舞台上至多保留 1 个当前意图角标，不会糊满屏幕）；
  - 回合结算推进下一回合时自动清空上一回合标点；
  - 战斗直连新增 2 条轻量 RPC：`SendBattlePing` 与单播推送 `NotifyBattlePing`（仅推同队 `team_index` 成员）；`BattleStateS2C` 追加 `repeated BattleTacticalPing team_pings`，`RedactStateForViewer`（C++）与 `BattleViews`（Java）强制向对手与观众清空，100% 杜绝军情泄露。

### 2.2 模块二：本局硬核复盘高光 & 战术徽记（`BattleCombatHighlight`）
- **引擎确定性统计（零时钟、零额外随机）**：
  - `TurnBattleEngine` 在现有伤害/治疗/护盾/控制/驱散结算点顺手累加每名玩家（含其出战灵宠归属到主人）5 项整型统计：
    - `total_damage_dealt`（总有效伤害）
    - `primary_focus_damage_dealt`（凝锋主目标累计伤害）
    - `total_support_value`（总有效治疗 + 新增有效护盾量）
    - `control_and_dispel_count`（成功施加控制 + 成功驱散 Buff/Debuff 次数）
    - `damage_taken`（承受伤害与护盾吸收总量）
  - 终局 `BuildSettlement` 按固定优先级规则为该玩家评定 1 枚本局专属 **战术徽记（`enum BattleHighlightKind`）**：
    1. `BATTLE_HIGHLIGHT_CLUTCH_COMEBACK = 5`：**「绝境翻盘」**（本方获胜且曾进入战意渐盛层数 `>= 2` 或本人曾低于 25% 血量仍存活至终局）；
    2. `BATTLE_HIGHLIGHT_PRIMARY_FOCUS = 1`：**「凝锋破核」**（`primary_focus_damage_dealt > 0` 且为本队凝锋输出最高者）；
    3. `BATTLE_HIGHLIGHT_TACTICAL_CONTROL = 2`：**「妙手控场」**（`control_and_dispel_count >= 2` 且为本队最高者）；
    4. `BATTLE_HIGHLIGHT_SAVIOR_SUPPORT = 3`：**「仁心济世」**（`total_support_value > 0` 且为本队最高者）；
    5. `BATTLE_HIGHLIGHT_IRON_GUARD = 4`：**「铁壁护主」**（`damage_taken > 0` 且为本队承伤最高者）。
  - 写入 `BattleSettlementData.highlight_summary` 随结算包下发，客户端结算页展示 4 项精炼数字与 1 枚本局战术徽记（不改 Elo、不发额外资源，零刷分风险）。

### 2.3 模块三：连胜锋芒势 & 破势终结（纯荣誉仪式感，零分数膨胀）
- **权威存储**：在 `RankedPlayerSeasonSummary`（及 Go/Java 排位进度行，复用已有表新增列或既有扩展字段）记录 `current_win_streak` 与 `best_win_streak`（胜 `+1`、负归 `0`、平/作废不变；不引入任何额外 Elo 奖励或保护分）。
- **入局展示**：进入正式论剑时，`RankedMatchContext.RankedMemberSlot` 透传该玩家开局前的 `win_streak`（仅当 `>= 3` 时在战前明牌变阵与开局头顶/顶部提示亮出**「连胜势 ×N」**旗标）。
- **破势终结**：若本局对手阵营存在 `win_streak >= 3` 且本方获胜，终局结算标记 `ended_opponent_streak = maxEnemyStreak`，结算页弹出**「破势终结 · 终结 N 连胜」**喝彩徽记，纯荣誉留念，零经济通胀。

---

## 三、思想分析

1. **为什么选「队内标点 + 复盘徽记 + 连胜破势」，而不加新数值系统？**  
   游戏当前已具备 62 技能、八部位强化、双法宝、单槽赛季印、20 点宝宝条目，以及并行线正在落地的五行阵法与 10 层爬塔；再叠新数值只会制造理解门槛与平衡灾难。这三项扩展 100% 落在**局内战术协同、局后情绪成就感、局间对抗张力**三条纯体验轴上，零肝度、零数值通胀。
2. **为什么与并行 `wuxing-gameplay-expansions-v1` 零冲突？**  
   - 协议只追加独立消息与末尾字段号（待并行线提交完 `proto/battle/*` 后按最新末尾编号顺延追加）；
   - 引擎只挂在现有伤害/治疗/控制钩子与独立 `SubmitBattlePing` 方法上，不改动任何五行阵法、爬塔符印或自定义切磋逻辑。

---

## 四、漏洞分析与防线

| 潜在风险 | 防线设计 |
|---|---|
| 队内标点泄露给敌方或观战者 | `RedactStateForViewer`（C++）与 `BattleViews`（Java）统一对敌方与观众清空 `team_pings`；`NotifyBattlePing` 严格按 `team_index` 单播同队成员 |
| 恶意队友高频连点标点骚扰全队 | 引擎强限每名玩家每回合最多 2 次标点（超限静默拒回 `kOperationTooFrequent`），且每人仅保留最新 1 枚标点不叠层；客户端同时提供「屏蔽本局队内标点」本地静音开关 |
| 客户端伪造非本场单位或已逃离/无效 `target_actor_id` | 服务端强校 `target_actor_id` 为本局存活/在场单位且符合标点阵营约束（集火/封控仅敌方、保核仅己方、驱散敌我皆可） |
| 连胜机制诱发高分段互刷 Elo | 连胜与破势终结 **0 额外 Elo、0 额外材料、0 额外资产**，严格限定为纯视觉旗标与战绩统计 |
| 投降后强退重登补弹漏掉「体面认输」标记 | 同步在 `PlayerBattleResult` 末尾追加 `bool surrendered`，双端 `scene` 结算落盘透传，补弹与现场结算完全同口径 |

---

## 五、步骤分析与分工

1. **分工约定**：
   - **设计、服务端/客户端业务代码**：由最高档编码模型执行；
   - **协议生成（`dev.bat proto` / `gen_proto.ps1` / `gen_messageids.ps1` / `ContractSync`）、C++/Go/Java/Unity 编译与测试运行**：统一交由 **Gemini 低档模型** 执行；
   - **新 UI 制作与预览**：交由 **Codex** 在 `D:/work/image/designs/ranked-play-v4-ui-20261125` 产出（本策划文档只说明要做什么 UI 与状态，不定任何画风/配色/字体，视觉风格完全由图仓既有规范决定）。
2. **执行步骤（待负责人拍板 & 并行 `wuxing-gameplay-expansions-v1` 稳定后按序推进）**：
   - **步骤 1（协议与生成）**：在 `battle_data.proto` / `player_battle.proto` / `player_battle_result_comp.proto` / `match_service.proto` 末尾追加 `BattleTacticalPing`、`BattleCombatHighlight`、`SendBattlePing` / `NotifyBattlePing`、`win_streak` 与 `PlayerBattleResult.surrendered`，由 Gemini 执行 `dev.bat proto` + `ContractSync` + 客户端同步。
   - **步骤 2（C++/Go/Java 双端服务端实现）**：在 C++ `TurnBattleEngine` / `BattleRoomManager` / `BattleClientEdge`、`scene` 补弹记录、Go `match` 连胜计数与 Java `game-battle-engine` / `game-battle` / `game-match` / `game-scene` 完整实现并补齐单测，由 Gemini 编译跑测。
   - **步骤 3（Unity 客户端接入与新美术对接）**：在 `BattleClient` / `DirectRoutingBattleTransport` / `RpcGuard` / `BattleScreen` / `BattleUnitView` / `RankedSettlementPanel` 接入战术标点浮轮、单位头顶玉符角标、连胜势旗标与结算战术复盘徽记，导入 Codex 交付的无字透明控件并验收。

---

## 六、可交给 Codex 的新 UI 功能清单（只定做什么 UI，不定视觉风格）

> 交付目录：`D:/work/image/designs/ranked-play-v4-ui-20261125`（推送至 Azure `image` 仓库）  
> 说明：以下只列出需要制作的界面元素、用途、状态与尺寸规格；**不指定画风、配色、材质、边框或字体**，视觉风格完全按图仓自身既有统一标准制作。

| 序号 | 元素名称 | 透明 PNG 尺寸 | 功能用途（仅定义是什么，无内置文字） |
|---|---|---|---|
| 1 | `ping_focus.png` | `96×96` | 队内战术标点图标：「集火」（挂在敌方单位头顶与标点按钮上） |
| 2 | `ping_control.png` | `96×96` | 队内战术标点图标：「封控」（挂在敌方单位头顶与标点按钮上） |
| 3 | `ping_protect.png` | `96×96` | 队内战术标点图标：「保核」（挂在己方单位头顶与标点按钮上） |
| 4 | `ping_dispel.png` | `96×96` | 队内战术标点图标：「驱散」（挂在敌我单位头顶与标点按钮上） |
| 5 | `ping_bar_base.png` | `420×84` | 战斗内队内快捷标点浮条容器底图（横向排布 4 枚标点按钮 + 1 个静音开关） |
| 6 | `highlight_focus.png` | `128×128` | 结算战术徽记图标：「凝锋破核」（本队凝锋主目标伤害最高） |
| 7 | `highlight_control.png` | `128×128` | 结算战术徽记图标：「妙手控场」（本队控场与驱散次数最高） |
| 8 | `highlight_support.png` | `128×128` | 结算战术徽记图标：「仁心济世」（本队治疗与护盾贡献最高） |
| 9 | `highlight_guard.png` | `128×128` | 结算战术徽记图标：「铁壁护主」（本队承伤最高） |
| 10 | `highlight_comeback.png` | `128×128` | 结算战术徽记图标：「绝境翻盘」（战意渐盛 2 层以上或残血逆转获胜） |
| 11 | `streak_banner_base.png` | `320×64` | 战前变阵与开局展示「连胜势 ×N」文字的标签底图（仅 3 连胜以上显示） |
| 12 | `streak_break_stamp.png` | `280×68` | 结算页展示「破势终结 · 终结 N 连胜」文字的标签底图 |

需要覆盖的 3 个界面状态预览（1920×1080 透明底）：
1. `01-battle-ping-3v3.png`：3V3 战斗中弹出快捷标点浮条 + 单位头顶挂「集火 / 封控 / 保核」标点（战斗舞台中央留空透明）；
2. `02-prep-win-streak.png`：战前变阵阶段展示「连胜势 ×5」标签的位置与信息排布；
3. `03-settlement-highlight-and-streak-break.png`：论剑结算页展示「本局 4 项复盘数据 + 1 枚战术徽记 + 破势终结标签」。
