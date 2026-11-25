# 《群仙问道赛》全服赛季战队锦标赛 V2 趣味扩展设计案

> **状态**：设计已落盘，待确认分期实施范围与 Codex 增量 UI 交付（V1 主链已全栈闭环）。  
> **上游事实源**：[season-team-tournament-v1.md](season-team-tournament-v1.md)、[season-team-tournament-v1-ui-handoff.md](season-team-tournament-v1-ui-handoff.md)。  
> **配套 Codex 增量 UI 规格书**：[season-team-tournament-v2-ui-handoff.md](season-team-tournament-v2-ui-handoff.md)。  
> **适用范围**：C++/Go 权威服（`xuanming-server-mmo-cpp`）、Java 独立服（`xuanming-server-mmo`）、Unity 双形态客户端（`mmorpg-client`，微信/抖音小游戏 WebGL2 + iOS/Android App IL2CPP）。

---

## 1. 需求分析

V1 已完整打通「跨区自由建队报名 → 7 天宽窗海选 → 2/4/8/16 单败淘汰 + L2 公开直播 → 封榜领奖 + 冠军殿堂 + 每季 1 票免费助威」的稳定底座。但站在 MMO 玩家长线体验视角，V1 还有四处明显可以「更好玩、更有谈资、更有策略与仪式感」的提升空间：

1. **替补战术深度不足**：V1 名册支持「3 首发 + 1 替补」，但淘汰赛全部是 BO1 一局定胜负，替补更多只是「有人缺席时的兜底」，缺少围绕环境/对手临场变阵的博弈乐趣。
2. **42 天报名期偏冷清**：长赛季报名期长达 42 天，建完队锁定后队员只能干等海选开启，缺少战队专属的赛前磨合玩法。
3. **决赛与冠军的“全服可见度”仍停留在弹窗里**：夺冠后只在赛事面板「冠军殿堂」占一行卡片，普通玩家走过天墉城没有直观的荣耀仪式感，也不易激发下赛季拉人参赛的向往。
4. **围观群众参与感偏单向**：V1 淘汰赛支持 L2 公开观战，但观众进房只能静静看；且错过直播的普通玩家无法从对阵树点看半决赛/决赛精彩回放（V1 战报仅限参战成员可看）。

**红线约束（与 V1 完全一致）**：
- 零付费门票、零真钱/代币竞猜押注、零逼肝日课；
- 所有新增奖励 100% 为绑定自用养成资源（银两/口粮/重铸砂/悟道残页）或纯荣誉外观；
- 不新增自由文本输入（避免无谓的 UGC 审核面扩大与合规风险）；
- 全程保持双服务端对账（C++/Go + Java）与客户端小游戏/App 双形态兼容。

---

## 2. 设计分析（四大独立可扩展模块）

四个扩展模块彼此解耦，可按优先级独立开关（全部挂在 `TournamentRule` 开关列），互不强绑定：

| 模块 | 玩法名称 | 核心乐趣价值 | 是否需要 Codex 新 UI | 推荐分期 |
|---|---|---|---|---|
| **模块 A** | **半决赛/决赛 BO3 + 五行灵脉阵眼** | 局间 60 秒看灵脉换替补，策略博弈与翻盘张力翻倍 | **需要**（灵脉图标 5 枚 + BO3 局分牌 + 局间换阵弹层） | V2.1（核心策略） |
| **模块 B** | **天墉城总冠军雕像群 + 每日免费观道领礼** | 全服主城仪式感 + 休闲玩家每日白嫖绑定礼 | **需要**（主城玉座牌匾切片 + 观道致敬轻弹窗） | V2.1（核心荣誉） |
| **模块 C** | **淘汰赛全服公开回放 + 直播预设喝彩条** | 错过直播随时复盘冠军战；现场 6 条零审核仙家弹幕 | **需要**（观战底栏超薄预设喝彩条；公开回放复用 V1 对阵树按钮） | V2.2（观战氛围） |
| **模块 D** | **报名期战队热身约战（零积分 3v3 切磋）** | 42 天报名期随时约战磨合，替补轮换真刀真枪练手 | **不需要**（直接复用 V1 战队页/榜单页与阵容弹窗） | **立即可做（零美术阻塞）** |

### 2.1 模块 A：半决赛 / 总决赛 BO3 + 「五行灵脉阵眼」轮转

1. **赛制范围**：
   - 海选、16 强、8 强保持 **BO1**（快节奏、单晚收尾不拖堂）；
   - **半决赛与总决赛（`remaining_teams <= 4`）自动升级为 BO3（三局两胜，先胜 2 局晋级）**。
   - 仅在 `TournamentRule.bo3_top4_enabled = true` 时启用；若有效参赛队仅 2 支（直接决赛），决赛直接采用 BO3。
2. **五行灵脉阵眼（确定性环境 Buff）**：
   - 新增配表 `TournamentLeyline.xlsx`（或 5 行静态规则，ID `1..5`）：
     - `1 = 金锐灵脉`：物理/法术暴击率 +5%，破防 +4%
     - `2 = 木生灵脉`：治疗与护盾效果 +10%
     - `3 = 水润灵脉`：每回合额外恢复 5% 法力，控制抗性 +8%
     - `4 = 火炽灵脉`：全体五行属性伤害 +6%
     - `5 = 土厚灵脉`：受到伤害降低 5%，首回合护盾 +8%
   - **确定性选脉**：同一场 BO3 的第 1、2、3 局从 5 种灵脉中按 `splitmix64(season_id ^ match_id)` 无放回抽取 3 种不同灵脉（3 局必不重复），建表生成对阵时即固定落库 `match_tournament_match.leyline_ids`，**开战前与局间全服公开展示**，杜绝临场暗箱。
   - **战斗服生效**：在 `CreateBattleRequest` 新增可选字段 `TournamentBattleContext { uint32 leyline_id = 1; uint32 game_index = 2; uint32 best_of = 3; }`，由 C++/Java `battle` 节点在开局第 0 回合确定性给双方全员挂载对应 `leyline_id` 光环 Buff（数值走定点数，零浮点漂移）。
3. **局间 60 秒换阵窗口**：
   - BO3 第 1 局（或第 2 局战成 1:1）结束、Kafka `BattleResultEvent` 入账后：
     - `match_tournament_match` 更新 `wins_a / wins_b` 与 `game_index++`，进入 `TOURNAMENT_MATCH_STATE_INTERMISSION(7)`，写定 `intermission_deadline_ms = now + 60_000`。
     - 局间 60 秒内，双方队长均可调 `StartTournamentMatch` 提交下一局 3 人首发（换上第 4 人替补应对下一局灵脉）并确认就绪；
     - 双方提前确认 → 立刻开下一局；若 60 秒到期仍有任意一方未重新确认 → 后台 5 秒驱动器按**上一局已校验合法阵容**自动触发下一局建房（防恶意卡局间拖时间）。

### 2.2 模块 B：天墉城「群仙问道·总冠军雕像群」+ 每日免费观道领礼

1. **冠军雕像数据快照（单向解耦）**：
   - 赛季 `SETTLED` 落账时，`match` 在 `match_tournament_season` 补充持久化 `champion_statues_blob`（序列化最近一届冠军战队 3–4 名成员的 `player_id / player_name / appearance_id / class_id / gender / zone_id`），以及 `worship_count` 累计瞻仰数。
   - **严守现役获批造型**：客户端渲染雕像时只认 `QdaoCharacterCatalog.ResolveRole(classId, gender, appearanceId)` 对应的现役立绘/模型，缺省时回落道家标准青玉剪影，绝不借用演示占位或废弃角色。
2. **主城展示与每日观道**：
   - 在天墉城「问道坛」坐标落放 3–4 尊半透明青玉鎏金质感角色雕像 + 悬浮战队牌匾（`第 N 届群仙问道赛总冠军 · <战队名>`）。
   - 新增两条轻协议：
     - `GetTournamentChampionStatues`（`MatchService` 只读，缓存 30 秒，返回最新冠军成员外观快照、全服瞻仰数、本人今日是否已观道）；
     - `WorshipTournamentChampion`（`Scene` 玩家域或 `Match -> Scene` 资产通道，**每日 UTC+8 零点重置 1 次**，直接发少量绑定自用礼如绑定银两 ×5,000 + 灵兽口粮 ×3，并原子递增 `worship_count`）。

### 2.3 模块 C：淘汰赛全服公开精彩战报 + 观战室「仙家预设喝彩弹幕」

1. **淘汰赛全服公开精彩战报**：
   - 现有 `BattleReportStore`（Go `go/match` & Java `game-match`）在保存 `MATCH_MODE_SEASON_TOURNAMENT` 的淘汰赛（`phase == KNOCKOUT`）战报时，将战报元数据 `public_visible = true`（海选与常规模式仍 `false`）。
   - `GetBattleReport` 鉴权逻辑放宽为：`参战成员 || report.public_visible == true`。
   - 客户端 `TournamentUiModel.CanWatchReplay(match, mySquad)` 在 `match.Phase == Knockout && match.State == Finished && match.BattleId != 0` 时直接对全服开放「回放」按钮，点击复用到现有 `BattleReportClient.Instance.FetchReplay(battleId)`——**后端改动极小、零新战报格式、零新美术资源！**
2. **直播预设仙家喝彩条（零 UGC 风险）**：
   - 仅在 `MATCH_MODE_SEASON_TOURNAMENT` 的 L2 直播观战房间显示底部超薄喝彩条。
   - 6 条固定枚举短语（客户端写死映射表，网络层只传 `uint32 phrase_id ∈ [1..6]` + `uint32 side ∈ {0,1}`，**彻底杜绝任何违禁词/外链注入可能**）：
     - `1`：「此剑惊鸿！」
     - `2`：「五行化境！」
     - `3`：「稳住反打！」
     - `4`：「阵眼妙绝！」
     - `5`：「道友承让！」
     - `6`：「青云直上！」
   - 服务端在 `battle` 节点观战广播回路按每观众 **5 秒冷却** 限流，并在 **500ms 窗口聚合最多 6 条弹幕 + 最新双方人气占比** 下发给该房观战者（绝不每点一次广播全服，保住 Gate 流量预算）。

### 2.4 模块 D：报名期「战队热身约战（零积分 3v3 切磋）」（零新 UI 阻塞）

1. **触发条件**：当前处于 `REGISTRATION` 阶段，己方战队与目标战队均满 `battle_team_size`（3 人），且双方当前均无进行中热身局。
2. **流程**：
   - 己方队长在「全服榜单 / 战队列表」点目标战队的「约战热身」→ 服务端写 `match_tournament_scrimmage`（60 秒过期，复用 `match_tournament_match` 的 `phase = SCRIMMAGE(3)`）；
   - 目标战队队长在「我的战队」收到 60 秒约战提示卡片，点「应战」并确认 3 人首发 → 直接走现有 `reserveGlobalDirect(kind=4)` 开 3v3 真实养成对局；
   - 战果回流 `ApplyTournamentBattleResult` 时发现 `phase == SCRIMMAGE`，只标记该热身单 `FINISHED` 并留存双方战报回放，**不加海选积分、不计入 6 场上限、不影响任何奖励与排名**。
3. **UI 零新建资源**：全部复用 V1 已有「我的战队」卡片、阵容确认弹窗与「全服榜单」行内次级按钮。

---

## 3. 思想分析

1. **为什么选这四块而不是堆「战令/竞猜/积分商城」**：
   - 竞猜押注在国内外双端提审（尤其是微信/抖音小游戏与 App Store）属于高危敏感红线，且极易滋生小号对倒；
   - 付费战令会破坏《群仙问道赛》“零付费、纯靠真实养成与团队协作”的公平口碑；
   - 相反，**BO3+五行灵脉**拉高高手博弈上限、**主城雕像+观道**拉满荣耀与全服日常获得感、**公开回放+预设喝彩**拉活社区观战氛围、**热身约战**填平报名期空白——四个方向全部正向放大“社交荣誉 + 策略乐趣”，零商业化毒素。
2. **架构解耦思想**：
   - **配置驱动 + 开关隔离**：所有 V2 特性挂 `TournamentRule` 布尔开关，默认灰度可控，老赛季/单测不受破坏；
   - **最小契约增量**：复用现有 `MATCH_MODE_SEASON_TOURNAMENT = 11`、`reserveGlobalDirect(kind=4)`、`BattleReport`、`QdaoCharacterCatalog`、`MailAdmin(sender_kind=6)`，不造第二套基础设施；
   - **安全与性能前置**：弹幕只传枚举 ID（0 UGC 风险）+ 500ms 服务端合帧广播（防扇出风暴）；BO3 局间超时自动续开（防恶意拖时）。

---

## 4. 漏洞分析与防刷对策

| 潜在漏洞 / 风险 | 攻击或故障场景 | 闭环对策 |
|---|---|---|
| **BO3 局间恶意拖延 / 假掉线** | 领先一局的队伍在局间 60 秒故意不点确认，试图卡死赛程 | 服务端记 `intermission_deadline_ms = now + 60s`，到点由 5 秒驱动器按上局合法阵容自动拉战；若某方全员离线导致备战失败超过 2 次，该局直接判负 |
| **BO3 战果重复投递** | Kafka 重投同一场 `battle_id` 导致胜场被加两次（1:0 变 2:0） | 新增 `match_tournament_match_game(match_id, game_index, battle_id)` 唯一键按 `battle_id` 幂等去重；`ApplyBattleResult` 先查该 `battle_id` 是否已落账再递增 `wins_a/wins_b` |
| **主城观道每日刷礼** | 跨零点并发连点或换线重领每日观道绑定礼 | 每日领奖状态归属玩家表 `last_tournament_worship_day`（UTC+8 `YYYYMMDD`）+ 玩家单线程 ECS/事务原子 CAS，同一天绝不双发；`worship_count` 异步累加只做荣耀计数 |
| **观战喝彩刷屏打爆 Gate** | 数千观众连点预设短语造成广播风暴 | 观众个人 5 秒令牌桶限流 + `battle` 房间内每 500ms 聚合一次快照（单帧上限 6 条弹幕 + 累计计数），带宽恒定 O(1) 封顶 |
| **公开战报泄露私密切磋** | 开放公开战报误放开海选或私人 1v1 切磋 | 严格仅在 `match_mode == SEASON_TOURNAMENT && phase == KNOCKOUT` 落库时写 `public_visible = 1`，其余模式恒为 `0` |
| **热身约战占用正赛状态** | 报名期临近结束前开热身赛，切入海选后仍占着活跃单 | 进入 `QUALIFIER` 时驱动器自动清理全部未开战 `SCRIMMAGE` 邀请单；进行中的热身局结账不碰海选统计 |

---

## 5. 步骤分析

1. **阶段 0（前置确认）**：产品/负责人确认 V2 四模块启用顺序，并将 [season-team-tournament-v2-ui-handoff.md](season-team-tournament-v2-ui-handoff.md) 交给 Codex 出增量美术切片与可点击原型。
2. **阶段 1（零新 UI 阻塞项优先落地）**：
   - 模块 D（报名期热身约战）+ 模块 C 的「淘汰赛全服公开回放」——后端双版 + 客户端接入，不等待 Codex 出图即可交付可玩。
3. **阶段 2（策略深度核心：模块 A）**：
   - 新增 `TournamentLeyline` 配表与 `TournamentBattleContext` proto → C++/Go + Java `battle`/`match` 确定性挂载五行灵脉与 BO3 局间驱动 → 接入 Codex 产出的灵脉图标与 BO3 局间弹层。
4. **阶段 3（荣誉与观战氛围：模块 B + C 弹幕）**：
   - `match` 结算快照保存冠军真实外观 + 天墉城主城雕像群与每日观道协议 → 观战房 500ms 聚合预设喝彩 → 接入 Codex 产出的主城雕像底座/观道弹窗与观战喝彩条。
5. **阶段 4（联调与回归门禁）**：
   - 全量跑通 `dev.bat gen` → `ContractSync --check` → `gen.bat client` → C++/Go/Java/Unity 全套单测与真机/编辑器视觉还原验收。

---

## 6. 执行步骤（供后续开工逐条打勾）

1. **契约与配表层（`xuanming-server-mmo-cpp`）**
   - [ ] `data/schema/tournamentleyline_table.proto` + `data/TournamentLeyline.xlsx`（5 条五行灵脉）
   - [ ] `TournamentRule.xlsx` 追加 V2 开关列（`bo3_top4_enabled`、`scrimmage_enabled`、`public_knockout_replay`、`statue_worship_enabled`）
   - [ ] `proto/match/match_service.proto` & `match_authority.proto` & `battle_node.proto` 追加 V2 字段与 RPC（`ChallengeTournamentScrimmage`、`RespondTournamentScrimmage`、`GetTournamentChampionStatues`、`WorshipTournamentChampion`、`SendSpectatePresetCheer`）
   - [ ] `Tip.xlsx` 追加对应错误码；跑 `dev.bat gen`、`gen_schema_index.py --check`
2. **双服务端实现（C++/Go + Java 并行对账）**
   - [ ] `go/match` & `game-match`：BO3 子局表 `match_tournament_match_game` + 局间 60 秒自动续开 + 五行灵脉确定性抽签 + 淘汰赛公开战报 + 热身约战 + 冠军雕像快照
   - [ ] `cpp/nodes/battle` & `game-battle`：开局挂载 `TournamentBattleContext.leyline_id` 光环 Buff + 观战房 500ms 聚合预设喝彩广播
   - [ ] `cpp/libs/services/scene` & `game-scene`：每日观道领礼防重与绑定自用礼发放
   - [ ] 同步 `ContractSync.java --check` 与 `PARITY.md`
3. **Codex 美术与 Unity 接入（`D:\work\image\designs\season-team-tournament-v2` → `mmorpg-client`）**
   - [ ] Codex 按 [season-team-tournament-v2-ui-handoff.md](season-team-tournament-v2-ui-handoff.md) 完成增量切图、原型并 push Azure `main`
   - [ ] Unity 导入增量切图并接入 BO3 局分/灵脉弹层、主城冠军雕像群、观战预设喝彩条、热身约战入口
   - [ ] 跑通 `client_compile_check.ps1`、EditMode NUnit、WebGL2/IL2CPP 静态检查与多进程联机实跑
