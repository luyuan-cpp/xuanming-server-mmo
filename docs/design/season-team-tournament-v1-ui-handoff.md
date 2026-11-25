# 《群仙问道赛》全服赛季战队锦标赛 — UI 制作与联调交接规格（给 ChatGPT / Codex）

> 日期：2026-11-24  
> 对应总设计：[season-team-tournament-v1.md](season-team-tournament-v1.md)  
> 适用范围：Unity 客户端 `mmorpg-client` UI 界面、Prefab、动效、美术占位与交互联调。  
> **服务端（C++/Go + 独立 Java）与客户端非 UI 网络层（`TournamentClient` + `TournamentModels`）均已由本仓与 `mmorpg-client` 落地**，UI 层只需直接订阅 `TournamentClient.Instance` 即可，严禁在 UI 层直接拼网络包或硬编码数值。

---

## 一、给 ChatGPT 的一句话定位与视觉基调

制作横屏回合制仙侠手游《五行奇谈》的**「群仙问道赛（全服赛季战队锦标赛）」**界面。  
核心体验是：**轻快、有仪式感、低压力**——平时 8 周赛季里随时拉 2～3 个熟人组一支固定战队（3 首发 + 1 替补），海选周随便抽空打满几场攒分拿保底，赛季末周末全服一起围观 8 强/总决赛拿冠军雕像与专属称号。

### 硬性红线（UI 严禁触犯）
1. **双形态平台兼容（强制）**：
   - 必须适配手机横屏 `Screen.safeArea`（刘海/灵动岛）以及**微信/抖音小游戏右上角胶囊按钮避让**（右上角留出至少 `140px` 逻辑宽度或将关闭/帮助按钮避开右上胶囊区）；
   - 严禁依赖鼠标 `Hover` 悬停展示关键信息，所有提示改用点击弹窗/说明卡；
   - 严禁在 UI 代码里使用 `Task.Run`、`Thread`、`HttpClient` 或反射代码生成（WebGL2 / IL2CPP 约束）；
   - 新增 `.cs` / `.prefab` / 美术资源必须同步保证 `.meta` 完整。
2. **文案与商业化红线**：
   - **严禁出现**“付费报名”“押注竞猜”“赔率”“每日必打”“漏打补签”等字样；
   - 助威功能明确标为**「免费助威 · 每季 1 票 · 零消耗」**；
   - 奖励明细清楚区分**「绑定自用道具/银两」**与**「全服永久/赛季荣耀排面（称号、徽章、冠军殿堂留名）」**。

---

## 二、客户端 C# 接口绑定（已实现，UI 直接调用）

UI 层统一通过 `MmorpgClient.Game.Tournament.TournamentClient.Instance` 与后端交互（纯 C# 真源，不需手动处理 Protobuf 解包或切号清理）：

### 2.1 状态属性与事件（单向数据流）
```csharp
var client = TournamentClient.Instance;

// 订阅刷新事件（OnEnable 订阅，OnDisable/OnDestroy 必须对称退订！）
client.Changed += RefreshView;

// 核心只读快照
TournamentOverviewSnapshot overview = client.Overview;         // 主厅全量快照（赛季、我的战队、邀请、精选观战、我的助威、我的奖励、冠军殿堂）
TournamentStandingsSnapshot standings = client.Standings;      // 全服积分榜 + 淘汰赛对阵树
bool isBusy = client.IsLoading || client.IsMutating;           // 加载/提交态，用于置灰防连点
string statusText = client.Status;                             // 成功/进行中提示
string errorText = client.LastError;                           // 服务端 Tip 错误文案（已自动映射中文）
```

### 2.2 可调用动作方法（全部返回 `bool`，内部已带 `RpcGuard` 限流防连点）

| UI 操作 | 调用方法 | 对应 RPC | 触发时机与 UI 反馈 |
|---|---|---|---|
| 打开主厅 / 下拉刷新 | `client.RequestOverview(includeHistory: true)` | `MatchService.GetTournamentOverview` | 打开窗口时调用；返回后按 `overview.Season.Stage` 切换主视觉态 |
| 创建战队 | `client.CreateSquad(string squadName)` | `MatchService.CreateTournamentSquad` | 仅 `REGISTRATION` 且无队时可见；队名 2–12 字，服务端审核通过后自动刷新 `Overview` |
| 邀请玩家 | `client.InviteMember(ulong targetPlayerId)` | `MatchService.InviteTournamentMember` | 仅队长且未锁定、未满员时可见；支持输入角色 ID 或从好友列表选人 |
| 回应邀请 | `client.RespondInvite(ulong squadId, bool accept)` | `MatchService.RespondTournamentInvite` | 邀请列表中点「同意 / 拒绝」 |
| 退队 / 踢人 / 解散 | `client.LeaveOrKick(ulong targetPlayerId)` | `MatchService.LeaveTournamentSquad` | `targetPlayerId == 0` 或等于自己表示退队（队长单人退队即解散）；队长传队员 ID 表示移出 |
| 调整首发阵容 | `client.UpdateLineup(IReadOnlyList<ulong> starterPlayerIds)` | `MatchService.UpdateTournamentLineup` | 勾选恰好 `BattleTeamSize`（默认 3）名首发成员，其余自动为替补 |
| 确认锁定报名 | `client.RegisterSquad()` | `MatchService.RegisterTournamentSquad` | 二次确认弹窗：「锁定后本赛季不可更换队员，确认代表本战队报名《群仙问道赛》？」 |
| 查看积分榜 & 对阵树 | `client.RequestStandings(uint seasonId = 0, uint page = 1, uint pageSize = 20)` | `MatchService.ListTournamentStandings` | `seasonId = 0` 表示当前赛季；支持切换往届赛季只读回看 |
| 发起海选匹配 / 淘汰赛备战 | `client.StartMatch(ulong matchId, IReadOnlyList<ulong> lineupPlayerIds)` | `MatchService.StartTournamentMatch` | 海选期 `matchId = 0`，淘汰赛传当前轮次 `matchId`；返回 `battle_id != 0` 后由既有战斗链路接管进房，若为 `WAITING_OPPONENT` 显示「战队等待对手就绪」横幅 |
| 免费助威 | `client.CheerSquad(ulong squadId)` | `MatchService.CheerTournamentSquad` | 淘汰赛前/窗口内全服玩家可点，每人每季 1 票，投完不可改 |
| 领取/核对赛季奖励 | `client.ClaimReward(uint seasonId = 0)` | `MatchService.ClaimTournamentReward` | `SETTLED` 阶段亮起可领按钮，领完展示已发往邮箱与已解锁称号/徽章 |
| 一键围观精选赛事 | `SpectateClient.Instance.WatchBattle(battleId)`（复用既有观战客户端） | `MatchService.WatchBattle` | 点击主厅或对阵树中「激战中 · 围观」按钮直接进全服 L2 观战 |

---

## 三、页面清单与详细布局规格（共 5 个主分页 + 3 个轻弹窗）

建议主窗口命名为 **`TournamentWindow`**（全屏横屏安全区容器，顶部通栏标题 + 赛季阶段进度条 + 左侧/顶部 5 页签切换）。

### 入口位置（主城 HUD / 竞技入口）
- 在主城「人宠会武」旁或「赛季活动」入口新增卡片：**「群仙问道赛」**；
- 卡片动态徽标（严格不制造焦虑红点）：
  - `REGISTRATION`：显示「报名中 · 剩 X 天」（有未处理战队邀请时才显 1 个邀请提示点）；
  - `QUALIFIER`：显示「海选积分赛 · 随时开战」（已报名战队显示 `已打 X/6 场`）；
  - `KNOCKOUT`：显示「全服淘汰赛 · 围观中」（有进行中精选局时显示呼吸灯「直播中」）；
  - `SETTLED`：显示「本季总冠军诞生」（有未领奖励时显「可领奖」轻标）。

---

### 分页 1：赛事总览（主厅 · 默认页）
**布局结构（左 60% 主视觉区 + 右 40% 我的战队/行动卡）**：
1. **顶部四阶段时间轴条（横向 4 节点）**：
   - `① 赛季报名` → `② 宽窗海选(7天)` → `③ 全服淘汰赛` → `④ 封榜发奖`
   - 高亮当前阶段，并在节点下方精确展示剩余倒计时（基于 `Overview.ServerTimeMs` 校准本地单调时钟，不信本机系统时间）。
2. **左侧主区（随阶段自适应，避免空屏）**：
   - **报名期**：展示赛事规则摘要卡（`3V3 真实养成 + 1 替补`、`海选 7 天自由打 6 场`、`完赛 2 场保底领重铸砂与悟道残页`）+ 上届冠军战队荣耀横幅（若有）。
   - **海选期**：展示「当前海选前 5 强快览」+「本季进行中的精选对局（一键观战）」。
   - **淘汰赛期**：顶部放大展示**「焦点激战中」大卡**（红蓝双队名、轮次如「总决赛 / 半决赛」、大按钮**「立即围观」**直接调 `WatchBattle`），下方缩略展示 4 强对阵。
   - **结算展示期**：展示**「本季全服总冠军荣耀展台」**（金框队名、3–4 名冠军成员姓名与所属区服、总战绩）+ 本人名次奖章。
3. **右侧行动卡（我的状态）**：
   - **未入队**：显示「创建战队」主按钮 + 「收到的邀请（N 条）」列表入口（每条含队名、队长、当前人数、「同意 / 拒绝」）。
   - **已入队**：显示队名、战队状态标签（`筹备中` / `已锁定报名` / `海选第 X 名` / `已晋级 X 强` / `本季第 X 名`）、成员头像槽（3 首发位带「主战」标 + 1 替补位带「替补」标）、战绩（`X胜 Y平 Z负 · W分`），底部唯一主 CTA 按钮：
     - 报名期未锁定：队长显示「确认锁定报名」（人数不足时置灰并写清「还差 N 名队员」），队员显示「等待队长确认报名」；
     - 海选期：队长显示「发起海选匹配（剩 X 场）」，队员显示「当前阵容预览」；
     - 淘汰期本队有活跃场次：高亮显示「进入淘汰赛备战」；
     - 结算期有可领奖：高亮显示「领取赛季奖励」。

---

### 分页 2：我的战队（名册与阵容管理）
1. **战队头栏**：队名、战队编号（仅展示/复制，不做排序依据）、报名状态、海选/淘汰战绩统计。
2. **成员卡片区（横排 4 格，适配 `roster_max_size` 动态渲染）**：
   - 每格展示：角色名、所属区服标签（如 `S1` / `跨服`）、身份标（队长皇冠 / 首发 / 替补）、在线状态指示灯；
   - 空位格（未锁定且队长视角）：点击弹出**「邀请队员弹窗」**（支持输入 `player_id` 精确邀请或从现有好友/帮会列表快捷填充）；
   - 队长在未锁定阶段点击某队员可切换「设为首发 / 设为替补」或「移出战队」；点击底部「保存阵容」调 `UpdateLineup`。
3. **规则提示条**：明示「全服跨区可组队；每赛季每人仅限加入 1 支战队；锁定报名后不可更换队员，替补可在任意场次顶替上场」。

---

### 分页 3：全服榜 & 淘汰赛对阵树
顶部提供双子切换：**「海选积分榜」** 与 **「淘汰赛对阵树」**，右上角提供**「赛季切换下拉框」**查看历史赛季。
1. **海选积分榜（滚动列表 + 底部钉住「我的战队」一行）**：
   - 列项：`名次` | `战队名称` | `队长与成员数` | `胜/平/负` | `积分` | `助威人气` | `操作（免费助威）`；
   - 晋级线视觉分隔：在第 `knockout_cut` 名（动态 16 / 8 / 4 / 2，按服务端返回 `KnockoutTeamCount` 渲染）下方画一条金色虚线标明「晋级淘汰赛线」。
2. **淘汰赛对阵树（自适应 2 / 4 / 8 / 16 强横向树状图）**：
   - **必须根据 `KnockoutTeamCount`（2 / 4 / 8 / 16）动态渲染列数**，严禁写死 16 强导致冷启动 4 强时满屏空白槽！
     - 2 强：1 列（总决赛大卡居中）；
     - 4 强：2 列（半决赛 2 场 → 总决赛 1 场）；
     - 8 强：3 列（8 强 4 场 → 半决赛 2 场 → 决赛 1 场）；
     - 16 强：4 列（支持双指/拖拽横向平滑滚动）。
   - 每个对阵节点显示：A 队名（含种子号 `#1`）、B 队名（轮空显示「轮空晋级」）、节点状态：
     - `PENDING / READY`：显示开战时间或「备战中（A/B 就绪指示灯）」；
     - `IN_BATTLE`：亮红/金脉冲标 + **「围观」**按钮（传 `match.BattleId` 调 `WatchBattle`）；
     - `FINISHED`：胜方高亮加粗，败方 50% 灰显，若有 `match.BattleId` 显示「回放」按钮（可跳战报系统）。
   - **冷启动空态（`< 2` 支队）**：友好展示「本赛季有效报名战队不足 2 支，未开启淘汰赛；已完赛战队将直接按保底档发放赛季奖励」。

---

### 分页 4：赛季奖励 & 免费助威
1. **上方：我的本季奖励账单卡片**（来自 `Overview.MyRewards`）：
   - 展示当前预计/已定档位、奖励物品图标与数量（全部带「绑定」角标）、银两数、称号与徽章预览；
   - 状态区分：`未结算（按当前名次预览）` / `可领取（高亮按钮调 ClaimReward）` / `已发至邮箱（附“前往邮箱”跳转）`。
2. **中部：全档位奖励总览表**（**严禁写死数字**，直接遍历 `Overview.RewardTiers` 或客户端同步的配置表 `TournamentRewardTableManager` 渲染）：
   - 按冠军 → 亚军 → 四强 → 八强 → 十六强 → 参赛保底（完赛 ≥ 2 场）→ 免费助威命中，清晰分行展示物品、银两、称号与徽章。
3. **下方：免费助威专区**：
   - 展示本季已助威对象（未助威时提示「免费为看好的晋级战队打 Call，零消耗，所选战队进四强或夺冠即得纪念礼」并附「去积分榜/对阵树选队助威」按钮）。

---

### 分页 5：冠军殿堂（历届荣誉墙）
- 按 `season_id` 倒序展示每届已结案赛季的冠军卡片：
  - 赛季标题（如「第一赛季 · 群仙问道赛」）、冠军战队名、冠军成员列表、封榜日期；
  - 无往届记录时展示优雅空态：「首届全服总冠军席位虚位以待，快召集同伴报名留名青史！」。

---

### 3 个轻量弹窗
1. **创建战队弹窗**：单行输入框（2–12 字，实时字数统计 + 敏感词由服务端拦截并展示 Tip 错误文案）+ 确认按钮。
2. **邀请队员弹窗**：角色 ID 输入框（预留好友/帮友快捷选择列表挂载点）+ 发起邀请按钮。
3. **赛前出战阵容确认弹窗**（点「发起海选匹配 / 淘汰赛备战」时弹出）：
   - 展示战队 3–4 名成员，默认勾选当前 `STARTER` 成员；允许队长临时把在线替补换上场（凑满 `BattleTeamSize` 人）；
   - 明确提示「确认后将全员拉入跨服战斗房间，请确保出战成员已整备好本人真实装备、技能与出战宝宝」。

---

## 四、Tip 错误码与异常态处理规范

所有业务拒绝均由服务端 `TipInfoMessage` 返回，`TournamentClient.LastError` 已自动查表翻译中文文案，UI 只需在顶部 Toast 展示即可，无需手写错误号映射：
- `kMatchTournamentNotOpen` → 展示敬请期待空态；
- `kMatchTournamentStageClosed` → Toast 提示并自动刷新一次 `RequestOverview` 对齐最新阶段；
- `kMatchTournamentSquadNameInvalid` → 保持创建弹窗不关，输入框下方标红显示文案；
- `kMatchTournamentMemberNotEnough` / `kMatchTournamentLineupInvalid` → 引导队长补齐成员或勾选足额首发；
- `kMatchTournamentQualifierLimit` → 置灰海选开战按钮并显示「本季海选 6/6 已打满，静待淘汰赛」；
- 断线/切号：订阅 `TournamentClient.Changed`，当 `Overview == null` 时安全回到加载态，绝不残留上一账号数据。

---

## 五、给 ChatGPT / Codex 的 6 条必做可点击验收场景

1. **报名建队闭环**：创建战队（测重名与超长名拦截）→ 发出邀请 → 另一账号同意入队 → 调整首发/替补 → 锁定报名（测锁定后退队/踢人按钮禁用）。
2. **海选宽窗开打**：海选期弹窗确认 3 人出战阵容 → 匹配开战 → 战后积分榜名次与 `胜/平/负/积分` 实时更新 → 打满 6 场按钮切换完成态。
3. **冷启动动态对阵树**：分别用 Mock/真实数据验证 **2 强（1 场决赛）、4 强（半决赛+决赛）、8 强（含轮空 BYE）、16 强** 与 **<2 队空态**，确认无空白死格与遮挡。
4. **全服观战直达**：淘汰赛对阵节点处于 `IN_BATTLE` 时点击「围观」，确认调用 `WatchBattle(battleId)` 进入观战界面。
5. **免费助威与奖励领取**：非参赛号在积分榜免费助威 1 支队（测第 2 次助威按钮变已投）→ 结算期点击「领取赛季奖励」→ 奖励卡切换「已发至邮箱」且不重复弹窗。
6. **双形态安全区检查**：在 `1920×1080`、刘海屏窄宽比（如 `2400×1080`）与微信/抖音小游戏右上胶囊遮挡模拟框下检查 5 个分页，确保无文字截断、按钮重叠与胶囊遮挡。
