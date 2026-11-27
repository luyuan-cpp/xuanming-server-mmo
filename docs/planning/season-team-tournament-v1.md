# 《群仙问道赛》全服赛季战队锦标赛设计方案（V1）

> 日期：2026-11-24  
> 状态：**设计定稿并启动全链路实现（C++/Go 服务端、独立 Java 服务端、Unity 客户端非 UI 联网与状态模型；UI 交接详见 [season-team-tournament-v1-ui-handoff.md](ui-handoff/season-team-tournament-v1-ui-handoff.md)）**  
> 上位红线：[项目设定](project-vision.md) · 经济总纲：[equipment-economy-charter.md](equipment-economy-charter.md) · 核心循环与赛季统一口径：[season-pvp-pve-economy-unified-v1.md](season-pvp-pve-economy-unified-v1.md) · L2 全服观战：[battle-spectate-tiered-fanout.md](../design/battle-spectate-tiered-fanout.md)

---

## 一、需求分析

### 1.1 产品定位与填补的空白
对照当前已定稿的三大战斗玩法：
- **首领试炼（PVE）**：解决单人/小队策略解题与装备起步；
- **人宠会武（`PVP_TEAM`）**：解决日常随时可打的个人 Elo 上分；
- **帮会玩法 V2**：解决本区全天无匹配干架、灵脉地盘争夺与江湖恩怨。

当前体系唯独缺少一个**「全服跨区 + 熟人固定战队 + 每赛季一次高光争冠 + 全服围观」**的终极舞台。本系统引入每赛季一届的**《群仙问道赛》全服玩家战队锦标赛**，达成四个核心目标：
1. **给真实养成一个全服最高展示舞台**：沿用正式会武与帮战 V2 统一口径，选手携本人真实装备（词条、法宝、+10/+11）、自由技能与宝宝出战。
2. **以熟人固定小队稳住跨季留存**：全服跨区自由组队报名，首发 **3 人出战 + 最多 1 名替补**（底层规则表支持 1–5 人出战，人口壮大后改表即扩 5V5），大幅降低 5 人缺 1 整队弃权的挫败感。
3. **不肝、不逼打卡、低人口冷启动绝不流产**：
   - 8 周赛季中，前 6 周自由报名与组队磨合；
   - 第 7 周为 **7 天宽时间窗海选积分赛**（任意时间打满至多 6 场即可，不卡点上班）；
   - 第 8 周仅集中 1 个周末打**全服单败淘汰赛与总决赛**，并自动接入既有 **L2 全服赛事观战广播** 与战报回放；
   - 按实际有效报名队数动态收缩淘汰赛规模（16 强 / 8 强 / 4 强 / 2 强直接决赛；不足 2 队直接结算参赛保底不流产）。
4. **保底实用 + 头部排面的健康奖励盘**：严守 [project-vision.md](project-vision.md) 六条红线，绝不发独占战力装备或可交易材料刷子口子；普通队完赛即领绑定重铸砂、悟道残页、灵兽口粮与银两，前 16 / 8 / 4 / 冠亚季获得全服专属称号、徽章与历届冠军殿堂永久留名；未入围全服玩家可免费助威（零付费、零押注）拿观赛纪念小礼。

### 1.2 上位红线准入四问自检（[project-vision.md](project-vision.md)）
1. **普通零氪玩家**：不花钱、每天只玩 10–30 分钟、不卡死每晚打卡，能否凑 2 个熟人报个名打满 2 场就稳拿保底重铸砂/悟道残页/银两？**通过**（海选 7 天宽窗随时开打，完赛 2 场不分胜负即达保底，且单场对局照常计入既有 660/周 150 装备材料池）。
2. **个人开发者收入**：有没有把付费流向送给工作室？**通过**（全赛奖励均为绑定自用耗材与荣耀称号/徽章，不产生可交易货源，反过来刺激玩家为赛季赛打磨装备词条、买永久外观与轻享卡配装槽）。
3. **物价与长期循环**：有没有无限产出可交易材料或通胀口子？**通过**（每账号角色每赛季严格只可归属 1 支战队、每档赛季奖主键去重终身发一次，全部绑定自用，绝不冲击银两集市与聚宝斋）。
4. **防骗与资产安全**：是否存在站外交易诱导或押注博彩风险？**通过**（无付费报名费、无竞猜押注池；免费助威每人每季 1 票零扣除，仅作活跃互动）。

---

## 二、设计分析

### 2.1 赛季四阶段状态机与时间布局
与 8 周赛季（56 天，配表 `TournamentRule` 全部可调）严格对齐，单赛季严格按以下 4 阶段单调推进（支持可恢复断点续跑与维护停服顺延）：

| 阶段枚举 (`TournamentStage`) | 默认天数 | 允许操作 | 阶段流转动作 |
|---|---:|---|---|
| `TOURNAMENT_STAGE_REGISTRATION (1)` | 42 天 | 建队、改阵容、发/受邀、退队/踢人、确认锁定报名、全服查列表 | 到达 `qualifier_start_ms` 时，将所有人数 `>= battle_team_size` 的已锁定/已满编战队置入海选池，未达人数门槛的草稿队标记 `EXPIRED` |
| `TOURNAMENT_STAGE_QUALIFIER (2)` | 7 天 | 已入池战队队长选 `battle_team_size` 名成员随时发起海选对局（每队最多 `qualifier_max_matches = 6` 场）；全服查积分榜与观战 | 到达 `knockout_start_ms` 时冻结海选排名，按实际合格队数确定淘汰赛规模 $K \in \{0, 2, 4, 8, 16\}$ 并生成确定性单败对阵树（非 2 的幂高位种子轮空 `BYE` 自动晋级） |
| `TOURNAMENT_STAGE_KNOCKOUT (3)` | 2 天 | 晋级队按当前轮次备战签到、双方就绪立即建房开战（自动标 `featured` L2 全服观战 + 录回放）；全服玩家免费助威（开赛前/指定窗口内限 1 票） | 每轮结束或到达单轮截止时刻推进下一轮（超时未到齐判弃权，双方未到齐按海选种子高者晋级）；总决赛决出胜负或到达 `settle_start_ms` 自动进入结算 |
| `TOURNAMENT_STAGE_SETTLED (4)` | 5 天展示 | 展示冠亚季/四强/八强/十六强与冠军殿堂、全服查历届记录、查询并领取赛季奖励（邮件自动投递 + 荣耀权益入账） | 到达 `end_ms` 自动开启下一赛季 `season_id + 1`（`REGISTRATION`），往届战队/赛程/荣誉永久可查 |

### 2.2 战队名册、报名锁定与海选/淘汰规则
1. **战队名册约束**：
   - 队名：长度 `2..12` Unicode 字符，去首尾空格、禁止控制字符；服务端强制接入既有 `content_guard` 审核（本地词库 + 平台审核）；归一化键 `name_norm`（小写去空白）在同一赛季内唯一。
   - **一人一季一队硬约束**：`match_tournament_member` 以 `(season_id, player_id)` 为主键，从数据库层彻底封死一人同时占两队或换队双领奖励。
   - 角色角色划分：`CAPTAIN (1)` 队长、`STARTER (2)` 首发、`SUBSTITUTE (3)` 替补。首发人数最多为 `battle_team_size`（默认 3），全队上限 `roster_max_size`（默认 4）。
   - 报名锁定：人数达到 `battle_team_size` 且首发已选齐后，队长调用 `RegisterTournamentSquad` 锁定战队（状态 `DRAFT -> REGISTERED`）。锁定后不可踢人/退队/改名，保证赛程名册稳定；锁定前可自由变更或解散。
2. **海选宽窗积分赛**：
   - 胜 +3 分、平 +1 分、负 +0 分（参数全部来自 `TournamentRule`）。
   - 每队最多打 `qualifier_max_matches`（默认 6）场；为防止同一对战队刷分，同一对无序战队 `(min(squadA, squadB), max(squadA, squadB))` 在海选阶段至多交手 **1 场**（若全服活跃战队数 `<= 4` 允许放宽至最多 2 场以保证低人口可完赛）。
   - 排位确定性比较序：`qualifier_points DESC -> qualifier_wins DESC -> qualifier_draws DESC -> registered_ms ASC -> squad_id ASC`。
   - **成局方式（复用既有 `go/match` 权威管线）**：
     - 队长调用 `StartTournamentMatch` 携带本场出战阵容 `lineup_player_ids`（须全部为本队成员、去重、数量等于 `battle_team_size`、且均在线空闲）；
     - 海选期：提交战队待战单，服务端撮合另一支满足条件且同季未超交手上限的已就绪战队（或由后台撮合循环按等待/积分差配对），原子写入 `match_tournament_match`（`phase = QUALIFIER`）并调既有 `reserveGlobalDirect` + `gather` 管线开房；
     - 淘汰期：队长针对本队当前活跃淘汰赛 `match_id` 提交出战阵容确认就绪，当对阵双方均就绪时立即原子建房开打，并同步将 `battle_id` 写入 `spectate:featured:active`（触发全服 L2 观战推荐）。
3. **淘汰赛对阵树与低人口动态降档**：
   - 按海选有效战队数 $M$ 确定淘汰席位 $K$：
     - $M < 2$：跳过淘汰赛直接进入 `SETTLED`（有 1 支达标队仍发参与保底，绝不报错卡死）；
     - $2 \le M < 4$：$K = 2$（1 轮，直接总决赛）；
     - $4 \le M < 8$：$K = 4$（2 轮，半决赛 + 决赛）；
     - $8 \le M < 16$：$K = 8$（3 轮，八强 + 半决赛 + 决赛）；
     - $M \ge 16$：$K = 16$（4 轮，十六强 → 总决赛）。
   - 标准种子对位（例如 8 强为 `1v8, 4v5, 2v7, 3v6`），若实际入围数不足 $K$，空槽记为 `squad_b_id = 0` 并直接置 `BYE` 自动晋级下一轮。
   - **单局平局与回合上限裁决**：
     - 沿用竞技模板 `Dungeon.id = 5`（720 秒 / 120 逻辑回合）；
     - 海选期平局双方各得 `qualifier_draw_points = 1` 分；
     - 淘汰赛若打满回合判平，按**淘汰赛决胜规则**确定晋级方：优先比较终局存活单位数（人物+宝宝），若仍平比较剩余气血百分比和，若完全相同取海选种子顺位高者晋级，确保对阵树 100% 收敛无死循环。
4. **奖励结算与幂等投递**：
   - 进入 `SETTLED` 时，在事务内为每名符合条件的参赛队员计算其唯一最高名次档（`1 冠军 / 2 亚军 / 3 四强 / 4 八强 / 5 十六强 / 6 参赛保底`）以及免费助威命中档（`7 助威命中`），生成 `match_tournament_reward` 行（主键 `(season_id, player_id, tier_kind)`，永不重复）；
   - 道具与银两只经既有 `MailAdmin.SendPersonalMail`（`sender_kind = 6`, `caller = "match"`, `request_id = reward_id`）投递到玩家邮箱，由玩家在锦标赛界面调 `ClaimTournamentReward` 触发，同时确认发放状态并点亮荣誉称号与徽章。邮件请求只由奖励行决定（结算时冻结标题正文、不带 `expire_ms`），确认邮件已建才落已领取；发件没装配时带附件的奖励保持可领取，纯荣誉奖励照常领取（2026-11-26 改正：原文写「异步可靠投递」，代码里没有后台投递，原实现发件没装配时还会把奖励直接标成已领取、附件不发。定案与原因见第七节）；
   - 单局战斗本身在 `scene`（C++/Java）的 `EvaluateBattle` 中纳入 `MATCH_MODE_SEASON_TOURNAMENT = 11` 分支，遵循与 `PVP_TEAM` 相同的 660 / 周 150 装备材料与防挂机门槛，不让玩家为了打锦标赛亏损日常养成进度。

### 2.3 配置表、协议与存储架构（零新进程、逻辑库自治）
全部归属 `go/match`（逻辑库 `mmorpg_match`）与 Java `game-match`，严格遵守 `AGENTS.md §4 / §7 / §12`：

1. **新增 2 张权威配置表 + 扩充 1 行 `PlayMode`**：
   - `data/schema/tournamentrule_table.proto` + `data/TournamentRule.xlsx`（id = 1）；
   - `data/schema/tournamentreward_table.proto` + `data/TournamentReward.xlsx`（id = 1..7，主键 `id`、含 `tier_kind`、`min_rank`、`max_rank`、`gold_amount`、`item_ids`、`item_counts`、`title_code`、`badge_code`、`mail_title`、`mail_body`）；
   - `data/PlayMode.xlsx`：追加 `id = 401`, `match_mode = 11 (MATCH_MODE_SEASON_TOURNAMENT)`, `battle_config_id = 5`, `enabled = 1`, `team_size_max = 5`, `allowed_team_sizes = [3]`。
2. **协议契约（只追加，不破兼容）**：
   - `proto/match/match_service.proto`：
     - `MatchMode` 新增 `MATCH_MODE_SEASON_TOURNAMENT = 11`（预留 9/10 给帮战 V2，避免未来撞号）；
     - `MatchService` 末尾新增 11 个客户端 RPC：`GetTournamentOverview`、`CreateTournamentSquad`、`InviteTournamentMember`、`RespondTournamentInvite`、`LeaveTournamentSquad`、`UpdateTournamentLineup`、`RegisterTournamentSquad`、`ListTournamentStandings`、`StartTournamentMatch`、`CheerTournamentSquad`、`ClaimTournamentReward`。
   - `proto/match/match_authority.proto`：新增 7 张权威 SQL 表（`match_tournament_season`、`match_tournament_squad`、`match_tournament_member`、`match_tournament_invite`、`match_tournament_match`、`match_tournament_cheer`、`match_tournament_reward`）并登记到 `authority.Tables()` 及 Java `MatchFeatureTables`。
   - `data/tip/Tip.xlsx`：在 `//match_error` 段末尾追加 15 个语义化错误码（全仓单轴发号，严禁手写数字）。
3. **跨服务最小增量接线**：
   - `go/match`：新增 `internal/tournament`（纯规则/目录/对阵树/排序/校验，策略模式隔离海选与淘汰决胜）、`internal/authority/tournament*.go`（SQL 事务权威层）、`internal/logic/tournamentlogic.go`（11 个 RPC 处理器 + 后台赛程推进/海选撮合；领奖邮件在玩家领取时发，没有后台发奖 worker，2026-11-26 按代码改正）；更新 `spectate_visibility.go`（`PUBLIC`）、`battle_replay_policy.go`（`true`）、`catalog.go`、`gather.go`（按成局冻结双边分队）、`rating.go`（终局结果分流到锦标赛结算）；
   - `go/mail`：`SenderKind("match") = 6`，允许 `match` 服务带签名调用 `MailAdmin.SendPersonalMail` 投递赛季奖励邮件（2026-11-26 才真正接上：`go/mail/etc/mail.yaml` 的 `Producers` 加 `match`，K8s 与本机给 mail、match 注入同一把 `MAIL_PRODUCER_SECRET_MATCH`，K8s 的 match 配置补 `MailRpc`；此前全仓没有这把密钥的注入，见第七节）；
   - `cpp/libs/services/scene/player/system/player_equipment_material_reward.cpp`：`EvaluateBattle` 增加 `case match::MATCH_MODE_SEASON_TOURNAMENT:`（1 行，零新线程、零裸指针）；
   - `xuanming-server-mmo`（Java 独立服）：在 `game-match` 新增 `com.game.match.tournament.*`（`TournamentTables`、`TournamentRules`、`TournamentStore`、`TournamentService`、`TournamentHandlers`）并接入 `MatchMethods`、`MatchModes`、`SpectateVisibilityPolicy`、`MatchFeatureTables`、`MatchFeatureConfiguration`；在 `game-scene` 接入 `MATCH_MODE_SEASON_TOURNAMENT` 材料平权；同步更新 `PARITY.md`；
   - `mmorpg-client`（Unity 客户端非 UI）：新增 `Assets/Scripts/Game/Tournament/TournamentModels.cs` 与 `TournamentClient.cs`（纯 C#、零 `UnityEngine` 依赖、WebGL2/IL2CPP 兼容、连接与会话代次守护、`RpcGuard` 限流防连点）及 EditMode 单元测试 `TournamentClientTests.cs`。

---

## 三、思想分析

1. **为什么寄宿在 `go/match` 而不是另起一个 `go/tournament` 新服务？**  
   遵循 KISS 与逻辑库自治原则：锦标赛强依赖全服部署、成局原件、直接建房 `gather`、L2 观战推荐、战报回放与对局终局消费流。放在 `go/match` 作为高内聚子模块，既完全复用已有发号器、角色护栏、熔断与 TiDB 事务域，又避免跨服务分布式事务和额外的运维成本。
2. **为什么采用「7 天宽窗海选 + 1 周末淘汰决赛 + 动态降档」？**  
   老 MMO 的四周固定点联赛在低人口期必死（凑不齐 16 支 5 人队即流产，上班族被迫打卡弃坑）。宽窗海选彻底消除时间焦虑，动态降档（16→8→4→2）保证哪怕开服首季只有 4 支队也能打出一场全服观战的精彩总决赛。
3. **为什么采用策略化规则核（`internal/tournament` / `TournamentRules`）与数据库主键防重？**  
   把对阵树生成、种子对位、海选排名比较、淘汰平局决胜、名次奖励映射全部收敛为无状态纯函数，既便于 C++/Go 与 Java 双服逐条对齐单测，又保证重启重算完全确定；同时用 `(season_id, player_id)` 复合主键在存储层锁死“一人一队、一人一票、一档一奖”，把并发刷漏洞扼杀在数据库约束层。

---

## 四、漏洞分析与权威防线

| 潜在漏洞 / 攻击面 | 风险后果 | 权威硬防线 |
|---|---|---|
| **一人多队 / 借号换队重复领奖** | 玩家在甲队拿完场次再跳槽到冠军乙队，同时领两份赛季奖 | `match_tournament_member` 主键 `(season_id, player_id)` 物理强制一人一季一队；战队锁定报名后禁止退队/踢人；`match_tournament_reward` 主键 `(season_id, player_id, tier_kind)` 杜绝重领 |
| **海选刷分 / 小号互喂保送冠军** | 拿小号建 3 支假队连续匹配自家主队 6 场刷满分 | 海选同对无序战队最多交手 1 场（全服 `<=4` 队放宽至 2 场）；淘汰赛单败真刀真枪且全服公开 L2 观战 + 强制录回放可供复盘举报；单局材料沿用防挂机 3 回合/2 手操门槛 |
| **淘汰赛对手故意不到场卡死整棵对阵树** | 一方弃权或掉线导致半决赛永远打不完，全服无法决出冠军 | 每轮淘汰赛冻结绝对截止时刻 `deadline_ms`；到期仅一方就绪判其弃权胜，双方皆未就绪按海选种子高者自动晋级；对局 `VOID` 允许在窗口内重开，窗口结束自动裁决推进 |
| **淘汰赛打满 120 回合判平导致无法晋级** | 双方龟缩奶盾打平，不知道谁进下一轮 | 淘汰赛平局采用确定性四级决胜（存活单位数 → 剩余血量比例和 → 海选种子顺位 → 队 ID 兜底），100% 产生唯一晋级方 |
| **队长在海选/淘汰赛中途非法夹带非本队成员或重复 ID 开战** | 雇外部满装大佬顶号上场或传重复 `player_id` 绕过人数校验 | `StartTournamentMatch` 在事务内查 `match_tournament_member` 核对全员归属本队、去重、数量精确等于 `battle_team_size`，并逐人校验在线、场景位置与无战斗占用 |
| **战队名涉黄涉政或冒充官方名** | 违反国内版号与小程序内容安全红线 | `CreateTournamentSquad` 统一强制校验字符集与长度，并在事务外先过 `content_guard` 审核；归一化名 `name_norm` 在 `(season_id, name_norm)` 建唯一约束防同名冒充 |
| **并发点击领奖 / 邮件重试导致双发重铸砂与银两** | 弱网连点 `ClaimTournamentReward` 或发件箱崩溃重启双发道具 | 奖励行写入 `delivery_state` 状态机 + 唯一 `reward_id`；`MailAdmin.SendPersonalMail` 按 `(sender_kind=6, request_id=reward_id)` 幂等去重，重放返回同一 `mail_id` 绝不重发。前提是重放的请求逐字节相同：标题正文在结算时冻结进奖励行、请求不带 `expire_ms`（2026-11-26 补，原实现每次取当前时刻与当前配表，超时后重领必撞请求冲突，见第七节） |
| **发件没装配 / 邮件结果不明时吞掉附件** | 发件密钥没注入时领取把奖励标成已领取、附件没发；邮件超时被当成失败或成功都会错 | 带附件的奖励只有拿到 `mail_id` 才落已领取，其余一律保持可领取、玩家重领即补；非 dev/test 档开着锦标赛而发件没装配，match 启动即拒（第七节） |
| **双服（Go vs Java）契约或裁决漂移** | 同一对阵树种子对位、海选排序或平局决胜两版不一致 | 共享同一套 proto、Tip 错误码、配置表（经 `ContractSync.java` 校验），纯规则核在 Go 与 Java 均配备同口径边界单测（2/3/4/5/8/16 队对阵树、平局决胜、防重名/防多队/幂等领奖） |
| **客户端切号/断线异步回调串数据与连点风暴** | 旧账号回调覆盖新号战队视图、狂点报名打爆网关 | `TournamentClient` 全回调受 `(connectionIdentity, playerId, generation)` 保护，全部 RPC 接入 `RpcGuard` 限流与在途去重，纯 C# 零线程池满足 WebGL2 与 IL2CPP |

---

## 五、步骤分析

1. **阶段 1 · 文档与 UI 交接单落盘**：完成本文与 [season-team-tournament-v1-ui-handoff.md](ui-handoff/season-team-tournament-v1-ui-handoff.md)，刷新 `docs/README.md`。
2. **阶段 2 · 配表、Tip 与 Proto 源契约**：
   - 新增 `TournamentRule`、`TournamentReward` 表 schema 与 `.xlsx`，更新 `PlayMode.xlsx` 与 `Tip.xlsx`；
   - 扩展 `proto/match/match_service.proto` 与 `proto/match/match_authority.proto`；
   - 运行导表、`gen_schema_index.py`、`proto-gen-run` 与客户端同步脚本。
3. **阶段 3 · C++/Go 权威服务端实现**：
   - `go/match` 实现 `internal/tournament`、`internal/authority/tournament*.go`、`internal/logic/tournamentlogic.go`、`matchserviceserver.go` 接线、`spectate_visibility` / `battle_replay_policy` / `catalog` / `gather` / `rating` 联动与完整单测；
   - `go/mail` 增加 `match` 发件身份（`sender_kind = 6`）；
   - C++ `player_equipment_material_reward.cpp` 增加 `MATCH_MODE_SEASON_TOURNAMENT` 分支与单测。
4. **阶段 4 · 独立 Java 服务端对齐**：
   - 运行 `ContractSync.java` 同步最新客户端契约与配表；
   - 在 `game-match`、`game-scene`、`game-mail` 实现完整等价逻辑与单元测试；
   - 更新 `PARITY.md`。
5. **阶段 5 · Unity 客户端非 UI 联网与模型实现**：
   - 新增 `TournamentModels.cs`、`TournamentClient.cs`（含 `.meta`）及 EditMode 单元测试 `TournamentClientTests.cs`；
   - 在 `GameClient` / `GameplaySession` 接入挂载点供 UI 层（Gemini 制作）零改造消费。
6. **阶段 6 · 全仓门禁、编译与自动化测试验收**：
   - C++ 裸指针预检 + 线程源检查 + 串行 MSBuild `/m:1` + 单测；
   - Go `go test` 覆盖 `match`（含新增锦标赛全状态机与并发/幂等单测）与 `mail`；
   - Java `mvnw clean install` 与定向全套单测；
   - Unity `tools/client_compile_check.ps1` 与纯逻辑单测自检；
   - 追加 `docs/PROGRESS.md` 记录实跑证据。

---

## 六、执行步骤与验收标准

| 验收项 | 通过标准 |
|---|---|
| **配表与协议同步** | `TournamentRule` / `TournamentReward` / `PlayMode` / `Tip` 导表 0 错误；`gen_schema_index.py --check` 退出 0；C++/Go/Java/Unity 契约生成与 `ContractSync --check` 全通过 |
| **战队全生命周期** | 建队（含重名拦截与字符校验）→ 跨区邀请 → 接受入队 → 调整首发/替补 → 锁定报名；一人加两队被拒；锁定后退队/踢人被拒 |
| **海选宽窗与防互刷** | 两支已报名队各自选 3 人阵容开战 → 终局胜者 +3 分、负者 +0 分、平局 +1 分；满 6 场拒绝再排；大池中同两队重复交手被拦截 |
| **动态降档淘汰赛对阵树** | 覆盖 1 队（直接保底结算）、2 队（直接决赛）、3 队（1 轮空 + 决赛）、5 队（8 强含 3 轮空）、8 队、16 队完整生成、胜者晋级、平局确定性决胜、超时弃权推进直至决出总冠军 |
| **L2 全服观战与回放** | `MATCH_MODE_SEASON_TOURNAMENT` 在两版服务端均冻结 `SPECTATE_VISIBILITY_PUBLIC` 且 `record_replay = true`，淘汰赛自动写入 `featured` 索引 |
| **奖励与助威幂等** | 封榜生成唯一最高名次档 + 免费助威命中档；重复调用 `ClaimTournamentReward` 或重投发件箱绝不双发；660/周 150 装备材料池按 PVP 平权口径入账 |
| **客户端非 UI 与编译体检** | `TournamentClient` 通过切号隔离、状态刷新、建队/邀人/报名/排行榜/开战/助威/领奖全套单测；`tools/client_compile_check.ps1` 0 错误 |

---

## 七、领奖与邮件投递的数据一致性定案（2026-11-26，数据覆写审计 r2 第 4 轮）

> 状态：已落码，**未编译、未运行**（验证交 Gemini，命令见 7.6）。审计编号 r2-tn#1、r2-tn#2、r2-jh#2，原文见 [r2 审计文档](../design/data-overwrite-rollback-audit-r2-20261019.md) §3.12 与 [第 4 轮交接](../handoff/data-overwrite-audit-r2-round4-handoff-20261126.md) §3「批 W」。生死状退费的 scene 侧兜底（待退银两走 SQL 终局复核、入账成功再清标记、不覆盖旧标记）由 scene 那一批实现并写在 [生死状一期实施计划](battle-jianghu-grudge-duel-phase1-implementation-plan.md) 末节「普通战书银两预扣与退还的数据一致性定案」；本节只写 match 与 mail 这一侧。

### 7.1 定案后的领奖流程

1. **结算**（`AdvanceTournamentSeason` → `settleTournamentSeason`，进 `SETTLED` 的那一笔事务）：每条奖励行写入附件原件（确定性序列化的 `MailAttachments`）和邮件标题、正文。文案由 `tournament.RewardMailText` 取当时的 `TournamentReward` 配表，空则用缺省文案，写进 `match_tournament_reward` 新增的 `mail_title` / `mail_body` 两列（`MatchTournamentRewardRecord` 字段 16、17，迁移器按 proto 自动 `ADD COLUMN`）。配表文案超出 mail 的校验口径（标题 1~128 字、正文至多 4096 字、合法 UTF-8）时，锦标赛目录构建（`tournament.Build`）直接拒绝。
2. **玩家点领取**（`ClaimTournamentReward` → `claimTournamentRewards`），逐条处理可领取的行：
   - 没有附件的纯荣誉奖励：直接落已领取，`mail_id = 0`，不依赖邮件。
   - 有附件、发件没装配（没配 `MailRpc` 或没有发件密钥）：回 `kMatchInternal`，行不动。
   - 有附件、发件已装配：只由奖励行构造请求（`tournamentRewardMailRequest`：`sender_kind = 6`、`request_id = reward_id`、收件人取行里的 `player_id`、标题正文取行里冻结的值、附件取行里的原件、`expire_ms` 不填），签名后调 `MailAdmin.SendPersonalMail`。**拿到 `mail_id` 才落已领取**（`MarkTournamentRewardClaimed`，行锁加状态比较，重复落是空操作）；超时、断连、被拒、请求冲突一律不落。
   - 一条失败不挡其余：成功的照常落已领取，响应同时带回已领取的部分和第一条失败的提示。
3. **mail 判重**（`go/mail/internal/data/repo.go` 的 `Repo.Send`）：先按 `(sender_kind, request_id)` 查旧邮件、比请求摘要（`serviceauth.Digest`，清掉签名段后的确定性序列化）。摘要相同回原 `mail_id`（`Duplicated`），不同回请求冲突。这一步先于一切依赖当前时间的校验（有效期、满箱），`expire_ms` 为 0 时有效期在首次落库时按 `MaxPersonalMailTtlDays`（30 天）算。所以「邮件已建、已领取没落定」之后重领，拿回的是第一次建的那封。
4. **部署接线**：mail 的 `Producers` 认 `match`；match 带 `MailRpc` 与发件密钥 `MAIL_PRODUCER_SECRET_MATCH`；dev / test 以外的档位开着锦标赛而发件没装配，match 启动即拒（`svc.requireTournamentMail`）。

### 7.2 r2-tn#1：发件没装配时把奖励标成已领取、附件不发（high）

- **问题与触发时序**：
  1. 赛季结算写下奖励行，状态可领取，附件如冠军档的 12 万绑定银两加四种道具。
  2. 缺省部署下发件根本没接：全仓只有 go/match 自己读 `MAIL_PRODUCER_SECRET_MATCH`，`tools/scripts` 与 `deploy` 没有任何注入；`go/mail/etc/mail.yaml` 的 `Producers` 只有 admin / rank / guild / battle；K8s 渲染的 match 配置也没有 `MailRpc`。match 里 `MailAdmin` 为空或 `MailSecret` 为空。
  3. 玩家点领取，旧代码跳过发件，`mail_id = 0`，照常调 `MarkTournamentRewardClaimed` 置已领取。
  4. 行已是终态，`ClaimableTournamentRewards` 只读可领取的行，之后接好线也查不到它，附件永久丢失。每个领奖玩家都中招。
- **方案**：
  1. 领取 fail-closed：有附件而发件没装配，回可重试的内部错误，行保持可领取；纯荣誉奖励照常直接落已领取。
  2. 接线照 battle 的 `MAIL_PRODUCER_SECRET_BATTLE` 做：`go/mail/etc/mail.yaml` 加 `match: MAIL_PRODUCER_SECRET_MATCH`；`deploy/k8s/manifests/go-svc/mail.yaml` 注入同名变量；`k8s_deploy.ps1` 的 `Add-MatchAuthoritySecretEnv` 给 match 注入同名变量，match 的 ConfigMap 补 `MailRpc`（直连 `mail.<infra>:50900`）与 `MailSecretEnv`（只写变量名）；mail 的 gRPC 准入名单（`-NetworkPolicies on`）加 match；本机 `go_services.ps1` 经 `lib/producer_dev_secret.ps1` 给 mail 与 match 同一把开发密钥。mail 的发件计数多一个 `sender="match"` 取值。
  3. 非 dev / test 档开着锦标赛而发件没装配（含密钥不足 32 字节），match 启动即拒。
- **为什么这样选**：附件只有邮件这一条交付路径，「已领取」必须意味着「附件已经交给 mail」。行保持可领取，接好线之后玩家再点一次就补上，不需要对账或补发工具。启动即拒把配置错误暴露在发布时，而不是等玩家领奖才发现；dev / test 放行，因为本机与 docker 调试栈常常不起 mail，而领奖本身照样 fail-closed。纯荣誉奖励不依赖邮件，直接落已领取，称号徽章不被发件故障挡住。
- **被否决的做法**：
  - 发件没装配时照常落已领取（原实现，注释写「未配置时仍幂等落账已领取状态供本地/测试与荣誉展示」，已改掉）：附件被吞，行已终态，事后无从补发。为本机测试方便，生产上吞掉玩家资产。
  - 部署自动生成 `mail-producer-match`，像 `mail-producer-battle` 那样进托管键表 `$script:ManagedDomainServiceSecrets`：托管键表的定义是「zone 工作负载以 `optional: false` 引用的键」，契约测试按清单反查。只在 infra 用的键（`mail-producer-rank`、`mail-producer-guild`、`rank-producer-match`、`match-internal-guild` 等）一律由运维预置，mail 与 match 都在 infra，照 rank → mail 的先例预置。进托管表还会在部署 zone 的 Go 服务时把这把 infra 专用键复制进每个 zone namespace，违背最小权限。
  - 只靠启动校验、领取时不 fail-closed：dev / test 档、运行期热更打开锦标赛这两种情形仍会吞。
  - 发件没装配时改走别的交付通道（例如让 scene 在线入账）：多一条交付路径，幂等与对账要再做一遍，没有必要。
- **代价与边界**：
  - 运维要在 infra namespace 的 `domain-service-secrets` 预置 `mail-producer-match`（开服手册 §1「运维仍要预置的键」）。缺了 mail 与 match 都停在 `CreateContainerConfigError`，比静默吞奖励好。
  - 已经按旧模板抄过 match 的 `runtimeConfigSecret` 的环境，要按新的 `-DryRun` 模板重抄（多了 `MailRpc` 与 `MailSecretEnv`），否则 pro 档拒启。项目未上线，首次上线整批起步。
  - 没装配期间领奖一直回 `kMatchInternal`（fault 码，计入故障告警），这是有意的。
  - 启动校验只看启动那一刻的锦标赛开关；运行期热更打开锦标赛而发件没装配，只靠领取时 fail-closed 兜底。

### 7.3 r2-tn#2：领奖邮件请求不确定，超时一次就永久卡死（medium）

- **问题与触发时序**：
  1. 玩家领取，旧代码的请求里 `expire_ms` = 当前毫秒 + 30 天，标题正文取领取时刻的配表。
  2. mail 建信成功并提交，但 match 这边应答超时或断连；或者应答拿到了，`MarkTournamentRewardClaimed` 却失败（TiDB 抖动、进程在两步之间重启）。行仍是可领取。
  3. 玩家重领：时间已经前进，`expire_ms` 不同，请求摘要必然不同。mail 按 `(6, reward_id)` 找到旧邮件，摘要不符，回请求冲突；match 当失败返回，而且旧代码在循环里直接返回，排在后面的奖励也领不到。
  4. 之后每次重领都撞同一条冲突。邮件其实已经在玩家邮箱里，锦标赛这边却永远显示可领取，还挡住同批其它奖励。策划改了标题正文，同样触发。
- **方案**：
  1. 请求只由奖励行构造：`expire_ms` 传 0；标题正文在结算时冻结进奖励行（新增两列），领取时不读配表。
  2. 配表文案超出 mail 的校验口径时，目录构建拒绝，不让一份 mail 永远拒收的原件被冻结下来。
  3. 单条失败不中断循环，其余照常领取。
- **为什么这样选**：同一幂等键必须对应同一份请求原件，这是 mail 判重的契约；battle 溢出邮件也是「不带 `expire_ms`、按存档条目逐字段重建」。mail 判重先于时间校验，`expire_ms = 0` 让有效期由 mail 首次落库时决定，于是「超时之后重放」天然收敛到原 `mail_id`，不需要任何新协议。文案冻结进行而不是改成固定文案，保住了策划按档位写标题正文的能力；加列只牵动 proto 追加两列、三处结算 INSERT 与一处扫描，代价小，所以没走固定文案的退路。
- **被否决的做法**：
  - 遇到 mail 冲突就当作已发送：分不清是同一请求的重放（邮件已建，可以落已领取），还是别的请求占了这个号（这一份根本没发出去）。后一种会把没发出的奖励标成已领取，等于吞附件。
  - 每次领取取当前时刻与当前配表（原实现）：违反「同一幂等键 = 同一请求原件」，把一次超时变成永久卡死。
  - 改用固定文案、不读配表：能解决卡死，但丢掉按档位定制的文案；加列的代价不大，不值得退。
  - 给 mail 加「按 request_id 查结果」的接口，match 先查再发：多一个跨服务协议，判重已经能给出同样的答案。
- **代价与边界**：
  - 结算之后改配表文案，不影响已结算的行（有意）。要改只能改库或清档。
  - 判重记录随邮件一起被过期清理删除。`Sweep.Mode` 开了删除模式（缺省 `report_only`，不删）之后，若邮件已建而已领取一直没落定，玩家又 37 天（有效期 30 天加保留 7 天）以上没重领，再领会新建一封；玩家若已领过第一封的附件，就多得一份。缺省配置下不会发生；开删除模式之前，要先给「已建信、已领取没落定」的行加对账（与 [battle 溢出邮件](../design/battle-overflow-mail.md) 的残余同一类）。
  - 响应同时带错误提示与已领取的部分。客户端若只看错误提示，已领取的那几条要等下一次刷新才显示，服务端状态是对的。
  - 邮箱满时 mail 回满箱，领取照样回 `kMatchInternal`，没有把 mail 的满箱提示透传给玩家（本批没改）。

### 7.4 r2-jh#2（match 侧）：生死状普通战书退费两条通道叠加成双退（medium，发件接线后可达）

- **问题与触发时序**：
  1. 发起方队长最后一个备战，scene 扣 500 银两，在 `PlayerJianghuComp` 记待退标记（随存档原子落盘），回 `jianghu_silver_fee_charged`。
  2. 之后 gather 失败（battle 节点不可分配、配表指纹不一致、原件落库失败等），`fail()` 先调 `refundJianghuSilverOnce`：插 `match_jianghu_refund` 标记，再以 `request_id = duel_invite_id` 发退费邮件。
  3. 邮件已建，应答却超时或断连：代码删掉标记、返回 false；`fail()` 随后以 `skip=false` 取消备战，scene 按待退标记在线再退一次。邮件 500 加在线 500，双退。单测还把「超时当失败、允许 scene 在线退」写成了断言。
  4. 有 Create 原件的分支（`globalAbortPrepared`）固定 `skip=false`，`fail()` 已经发过邮件时同样双退。
  5. 当时 match 的发件密钥没接线，`refundJianghuSilverOnce` 一进来就返回 false，这条路走不到；本批给 match 接上发件之后就可达，所以必须同批处理。
- **方案**：退费只留 scene 一条通道。match 侧删掉 `refundJianghuSilverOnce`、没有调用方的 `maybeRefundJianghuSilverForFormation`、退费标记表 `match_jianghu_refund`（建表随 proto 消息与迁移清单一起删，三个读写方法从 `JianghuStore` 接口删掉；全仓 grep 过，没有别的读者）；`fail()` 不再发信；`globalAbortPrepared` 去掉 `skipSilverRefund` 参数；解冻（`cancelPrepared`）一律不置 `skip_jianghu_silver_refund`。这个 proto 字段本批不动，scene 侧怎么处理由 scene 那一批定。match 发信只剩群仙问道赛领奖一处，有 AST 守卫钉住。
- **为什么这样选**：扣费和待退标记都在 scene 的玩家存档里、随存档原子提交，scene 是唯一知道「扣没扣、退没退」的一方。只留一条通道，「退没退」就只有一个事实源，不需要跨服务协调。
- **被否决的做法**：
  - 保留邮件通道，结果不明时保留标记、按同一原件重投：mail 这边能做到不重发，但 scene 的在线退走的是另一条路径（取消备战 RPC），要让 scene 知道「邮件已经接管」，就得在两个服务之间协调。scene 已经有随存档原子提交的待退标记，单通道更简单。
  - 保留邮件通道，取消时一律 `skip=true`：scene 在线退被关掉，邮件结果不明或邮件被拒时就成了漏退。
  - 只把「出错就删标记」改成「结果不明保留标记」：`globalAbortPrepared` 固定 `skip=false` 的分支照样双退。
- **代价与边界**：
  - 离线玩家拿回银两要等 scene 那批的终局复核（标记在存档里不丢，复核或下次上线时退），不再有邮件即时到账。
  - `match_jianghu_refund` 从迁移清单删除，迁移器不删表；旧库里若还在，是没人读写的孤表（项目未上线，不做迁移）。
  - `skip_jianghu_silver_refund` 字段仍在协议里，match 不再置 true。

### 7.5 改动清单

- go/match：`internal/logic/tournamentlogic.go`（领取重写，抽出 `claimTournamentRewards` / `claimOneTournamentReward` / `tournamentRewardMailRequest`）、`internal/authority/tournament.go`（结算写冻结文案、读写两列）、`internal/tournament/catalog.go`（`RewardMailText`、文案长度校验）、`internal/svc/servicecontext.go`（`requireTournamentMail` 启动校验）、`internal/logic/jianghu_duel_logic.go` / `gather.go` / `global_match.go`（退费单通道）、`internal/authority/jianghu_duel.go` / `schema.go`（删退费标记表与读写）；`proto/match/match_authority.proto`（奖励行加字段 16、17，删 `MatchJianghuRefundRecord`）。
- go/mail：`etc/mail.yaml`（`Producers` 加 `match`）、`internal/metrics/metrics.go` 与 `internal/server/server.go`（`sender="match"` 计数取值）。
- 部署与本机：`deploy/k8s/manifests/go-svc/mail.yaml`、`tools/scripts/k8s_deploy.ps1`（match 注入发件密钥、ConfigMap 补 `MailRpc` / `MailSecretEnv`、mail 准入名单加 match）、`tools/scripts/lib/producer_dev_secret.ps1`、`tools/scripts/go_services.ps1`；开服手册 §1 加 `mail-producer-match`。
- 测试（都没运行）：`go/match/internal/logic/tournament_reward_claim_test.go`（3 条）、`jianghu_refund_single_channel_test.go`（AST 守卫）、`jianghu_duel_logic_test.go`（删邮件退费断言，改为断言不发信）、`internal/tournament/rules_test.go`（1 条）、`internal/svc/servicecontext_test.go`（1 条）；`go/mail/internal/data/match_sender_identity_test.go`（1 条）、`internal/metrics/metrics_test.go`（序列数）；部署契约 `k8s_deploy_contract.tests.ps1`（mail 生产者、准入名单、新增 match 发件接线 1 条）、`k8s_migrate_gate.tests.ps1`、`lobby_authority_render.tests.ps1`、`logical_db_deploy_contract.tests.ps1`、`go_services_local_mysql.tests.ps1`。
- Java 版（独立仓）：只登记待做，本仓不写 Java 代码。

### 7.6 验证（交 Gemini）

1. 生成：`proto/match/match_authority.proto` 改了，先跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`。这份 proto 不进客户端（Unity 只生成 `MatchService` / `MatchNetwork` / `MatchErrorTip`），不用同步客户端；`robot/vendor/proto/match` 的副本没有自动同步钩子，robot 不读这两个消息，旧副本不影响编译。
2. Go（各模块目录分别执行）：
   - `go/match`：`go build ./...`；`go vet ./internal/logic/ ./internal/authority/ ./internal/tournament/ ./internal/svc/`；`go test ./internal/logic/ -count=1 -run 'TestClaimTournamentRewards|TestTournamentRewardMailRequest|TestJianghuSilverRefundHasOnlyTheSceneChannel|TestJianghuDuel_' -v`；`go test ./internal/tournament/ ./internal/svc/ ./internal/authority/ -count=1`。新增与改名的用例全部 PASS，整包不新增失败。
   - `go/mail`：`go test ./internal/data/ -count=1 -run 'TestMatchSenderIdentityMatchesTournamentClaim|TestBattleSenderIdentityMatchesSceneOutbox' -v`；`go test ./internal/metrics/ ./internal/server/ -count=1`。
3. 部署契约（仓库根，PowerShell 7）：`k8s_deploy_contract.tests.ps1`、`k8s_migrate_gate.tests.ps1`、`lobby_authority_render.tests.ps1`、`logical_db_deploy_contract.tests.ps1`、`go_services_local_mysql.tests.ps1` 全部 `[PASS]`，重点是 mail 生产者五把、mail 准入名单含 `match` 与 `match-canary`、「match 发件接线」一条。

### 7.7 收件箱满时回满箱提示（2026-11-27 补，未编译）

- **问题**：mail 收件箱满时拒收并回 `kMailRecipientBoxFull`（不建邮件），领取把它和超时、断连一起报成 `kMatchInternal`。玩家只看到「内部错误」，不知道清出空位就能领，会一直重试或找客服。
- **方案**：`claimOneTournamentReward` 拿到 mail 的满箱提示时，原样回这个 tip（tip 码全仓单轴，客户端按 id 查文案），行保持可领取；其余失败照旧回内部错误。用例 `TestClaimTournamentRewardsPassesThroughRecipientBoxFull`：满箱那次不落已领取、不建邮件，清出空位后重领照常领到，只有一封。
- **为什么这样选**：满箱是唯一一个玩家自己能处理的拒收原因，透传只改一个分支，不碰判重、不碰「拿到 `mail_id` 才落已领取」。
- **被否决的做法**：把 mail 回的 tip 一律透传。请求冲突、参数非法这类是服务端问题，原样给玩家看只会误导，所以只透传满箱。另一种做法是 match 自己新增「领奖时收件箱满」的 tip，但那是同一件事的第二个码，违反单轴。
- **代价与边界**：响应里同时带满箱提示和其余已领取的条目时，客户端怎么显示没核（与 7.1 的「一条失败不挡其余」同一处）；mail 若以后新增别的玩家可处理的拒收原因，要在这里逐个加，不会自动透传。
