# 回合制战斗战报回放

> **状态(2026-11-10)**:服务端 C++ / Go 已落码。已有的运行证据:proto 与 tip 码已生成(`kMatchBattleReportNotFound = 16025`,消息号 `ListBattleReports = 379`、`GetBattleReport = 380`);C++ 在 Codex 11-08 的命名审查整解里编译通过,录制器 6 个用例与战斗节点 2 个用例全部通过(`build/verification/naming-audit-20261108/cpp-tests/`);Go match 在 11-06 编译通过,纯逻辑用例通过,4 个真 TiDB 用例当时没设 `MATCH_IT_TIDB` 被跳过,**还没在真库上跑过**。11-10 补了限频配置、体积实测用例和 robot 冒烟核对,**这三项未编译、未运行**。剩余验证见 §17。客户端未做,交接见 [battle-report-replay-client.md](../handoff/battle-report-replay-client.md)。Java 版待做(§16)。
> **负责人决定**:§13 五项 2026-11-06 按建议拍板;同日又改了两项:**只录 PVP**、**每名玩家保留最近 100 条**(取代原来的「活动局也录」和「保留 7 天」)。因此 match 自己维护按玩家的 PVP 战报列表,不再有按天数的定时清理。
> **依据**:本仓 C++ battle、Go match 与 Java 版只读调研;[SLG 战斗笔记 Q5 / Q15](../notes/slg/slg-battle-interview-qa.md) 的事件日志式战报(该笔记是「率土风格」的面试笔记,不代表任何游戏的真实架构)。
> **和近期战斗结果的关系**:[战斗结果重登恢复](battle-result-recovery.md)存在玩家存档里,最多 10 局、各种模式都有、带本人实际到账的奖励,用来在重登后补弹结果。本文的 PVP 战报列表存在 match 库里,最多 100 局、只有 PVP、带回放。两份列表的用途不同,都带 `battle_id`;近期战斗结果里的 PVP 局同样能凭 `battle_id` 取回放。2026-11-05 / 11-06 与该文档作者经会话间沟通对齐过。
> **编号**:本文决策从 BR-1 起编号,不占 turn-based 系列的 D 号。行号取自 2026-11-06 的工作区,其中有其他会话未提交的改动。

## 0. 结论

battle 在房间里记下开战状态和每回合帧。帧是观众视角,和观战下发的是同一份。终局冻结时压成 gzip,随 `CommitBattleOutcome` 一起交给 match。match 在写终局的同一个事务里存战报正文,并给每名参战者的 PVP 战报列表加一条;每人只留最近 100 条,超出的最旧一条在同一个事务里删掉,正文没有任何人再列着时一起删。客户端调 `MatchService.ListBattleReports` 拿列表,调 `GetBattleReport(battle_id)` 取回放,解压之后按「开战状态 + 逐回合帧」播放,走和观战一样的播放流程。

- **只录 PVP**:录不录由 match 建房时按模式决定,冻结进 `CreateBattleRequest.record_replay`,和观战可见性同一个做法。
- **战报出问题不挡结算**:回放坏了、太大、压缩失败,终局照常提交;列表条目照写,只是标「没有回放」。
- **只许列表里有这一局的玩家看**:查不到、已被挤出、没录、不是参战者,统一回一个码。
- **没有定时任务**:写入、挤出、删正文都在终局事务里完成。

## 1. 现状(落码前)

| 需要的 | 落码前有的 | 位置 |
|---|---|---|
| 开战时双方阵容 | 开战帧 `BattleStartS2C.state`:全部单位的名字、等级、职业、外观、站位、HP/MP、buff、技能 | `battle_room_manager.cpp` HandleCreateBattle、`player_battle.proto` `BattleStateS2C` |
| 逐回合过程 | `TurnResultS2C`:事件序列 + 出手顺序 + 结算后的全量状态。事件带 `target_health_after` / `target_mana_after`,可以从任一回合开始播 | `player_battle.proto` `TurnResultS2C`、`battle_data.proto` `BattleEventItem` |
| 公开视角 | 观众版帧统一清掉冷却、道具余量和装备概率(`RedactStateForViewer(state, 0)`) | `battle_room_manager.cpp` BroadcastTurnResult / RedactStateForViewer、`turn_battle_engine.cpp` StripEngineOnlyState |
| 落盘 | **没有**。`ResolveRound` 算完直接推直连,不保存;没有直连的人这一帧直接丢 | `battle_room_manager.cpp` ResolveRound / BroadcastTurnResult |
| 终局原件 | `BattleResultEvent`:胜负、队伍、回合数、个人结算,经 `CommitBattleOutcome` 存进 `match_battle_completion`。**不含任何帧** | `battle_room_manager.cpp` BuildResult、`completion.go` commitBattleOutcomeLocked |
| 超时平局 | `OnBattleDeadline` 强制判平,没有最后一回合帧,结果里也不记是第几回合被强制结束 | `battle_room_manager.cpp` OnBattleDeadline |
| 查询 | 客户端没有任何取历史战斗帧的协议 | `match_service.proto` `MatchService` |

turn-based §9 的「回放持久化(事件流落盘)」和观战分级 §2、§11 P3 的「归档消费者把帧落对象存储」原来都是预留,两处正文已改成指向本文。

## 2. 目标与非目标

**目标**

- 参战者事后能完整回看自己打过的 PVP 对局:开战 → 每回合 → 结局,画面与当时观战看到的一致。
- 每名玩家能翻看最近 100 场 PVP 战报。
- 有战果才有战报,两者同一个事务提交。提交不成功的终局不留战报;也不会出现战果在、战报丢的情况(回放被截断或丢弃时除外,见 BR-5、BR-7)。
- 战报出任何问题都不能挡住结算。
- C++ / Go 与 Java 同批;两版共享的只有客户端契约(AGENTS §12)。本轮 Java 只登记待做(§16)。

**非目标(本期不做)**

- **PVE 局的回放**:刷怪局最多、最频繁,不录;以后按副本单独打开(§12)。
- **按指令流重算校验**(SLG 笔记里 GM 仲裁那条):要另外记每回合被接受的指令、挂机开关、引擎版本和节点强制终局(`configurable-templates.md` §8.6.6),与客户端回放无关,P3 再做。
- **给非参战者看**:分享、公开局 / 赛事局人人可看,放 P2(§12)。
- **冷归档到对象存储**:按实测容量再定。
- **帮会 / 活动的战报列表**:有需求再做。
- **旧战报按当时的配表显示**:v1 按客户端当前配表显示。战报里记了配表指纹,客户端可以提示「版本已更新」(§8)。

## 3. 方案比较

| | A 随终局原件同事务提交(采用) | B 终局后单独提交 | C battle 发 Kafka,归档消费者落库 | D 只存输入 + 种子,客户端重算 | E 客户端本地录制 |
|---|---|---|---|---|---|
| 一致性 | 战报和战果同一事务;提交未知时按原字节幂等重放 | 两个提交点,battle 在两者之间崩溃就丢战报;要另做重试和幂等 | battle 没有持久化,发送失败就丢;至少一次投递要去重 | 输入在就能重算 | 只在本机,换设备就没了 |
| 新增部件 | 请求加一个字段、两张表、两个查询 RPC | 比 A 多一个 RPC,房间还要多留一阵 | 新 topic、新消费者、新存储 | 指令流记录;客户端要内嵌完整引擎 | 无服务端改动 |
| 客户端要求 | 播放器(复用观战播放) | 同 A | 同 A | 内嵌引擎;配表或逻辑一改,旧战报就对不上 | 同 A |
| 代价 | 终局请求变大(上限 512 KiB,§9);终局事务多几条语句;match 库多存数据 | 比 A 复杂,收益只是终局请求小一点 | 多一条链路,丢失窗口更大 | 指令流要另记;引擎暴露给客户端 | 别人看不了,也不权威 |

**选 A**:

- **不丢、强一致**:只有终局 COMPLETE 才有战报,而且写在同一个事务里。终局提交结果未知时,battle 按冻结的原字节重放,战报跟着一起幂等。这对应 SLG 笔记 Q15「战报和战果同一事务提交、提交后才推送」。
- **battle 仍然零持久化**(turn-based §7 第 7 条、§8):房间只在内存里攒帧,持久化仍然只有 match 一处。
- **改动集中**:不新建服务和链路,也不加后台任务。
- B、C 都要多造一个提交点和对应的重试、去重。D 解决的是重算校验,不是播放。E 解决不了多设备和权威性。

## 4. 决策

| # | 决策 | 理由 |
|---|---|---|
| BR-1 | **存事件日志,不存输入重算**:战报 = 开战状态 + 每回合 `TurnResultS2C`(观众视角)+ 结局信息 | 客户端只需要播放器;配表或引擎改了,旧战报照样能播;客户端拿不到引擎逻辑。与 SLG 笔记 Q5 的选择一致。我们是手动回合,只有输入和种子重算不出来 |
| BR-2 | **复用现有消息,不做平行结构**:`BattleReplay` 直接装 `BattleStateS2C` 和 `TurnResultS2C` | 客户端回放可以复用观战的播放流程;AGENTS §3 不许手写平行结构 |
| BR-3 | **观众视角**:录进战报的帧一律先过 `RedactStateForViewer(state, 0)`,与观众收到的逐字段相同 | 不会多出新的泄漏面。代价是参战者回看时也看不到自己当时的冷却和道具余量 |
| BR-4 | **随终局原件同事务提交**:`CommitBattleOutcomeRequest.replay`;match 在 `CommitBattleOutcome` 写 `match_battle_completion` 的同一个事务里写正文和列表。ABORT 不写 | 见 §3。只有 COMPLETE 的局有战报,玩家不会看到之后被作废的对局 |
| BR-5 | **录制上限**:原始 1 MiB,录制时按每帧序列化大小累计(预算再留 16 KiB 给头部和 tag),超过就停录后面的回合并标 `truncated`;压缩后超过 512 KiB 就整份不发 | 保证 battle→match 请求、match 存库、match→gate→客户端回包都远低于各段的 4 MiB 上限和 gate 的 2 MiB 高水位(§9);房间内存有界 |
| BR-6 | **gzip(RFC 1952)**:battle 压缩一次,match 原样存取、不解压,客户端解压 | battle 节点已链接 zlib;Go、Java、C# 都有自带的 gzip,四端都不加依赖。gzip 自带 CRC32,客户端能发现数据损坏 |
| BR-7 | **战报坏了不挡终局**:match 在事务里先看原件让不让录、再查外壳(编码、版本、大小);不合格就丢掉回放、记指标,终局照常提交,列表条目照写并标 `has_replay = false`。重放时回放摘要对不上也只记错误,返回已持久的决定 | 战报是展示数据,结算是资产。AGENTS §11.3 允许展示数据走 fail-open,但要配日志、指标和测试(§10、§15、§17) |
| BR-8 | **只录 PVP,由 match 建房时冻结**:`recordReplayFor(enabled, mode)` 决策表的结果写进 `CreateBattleRequest.record_replay`;battle 只认这个字段 | 和观战可见性同一个做法(`spectate_visibility.go`),只有一个决策点;新旧版本混跑时天然安全(§11);全局开关关掉后新局不录,已存的照样能查 |
| BR-9 | **只许列表里有这一局的玩家看**:请求不带 player_id,只认会话里的本人;先查本人列表里有没有这一局,再读正文。查不到、已被挤出、没录、不是参战者,统一回 `kMatchBattleReportNotFound` | 列表行就是「这名玩家还保留着这一局」的权威;统一一个码,不泄露别人的对局是否存在;不在列表里的人读不到大行 |
| BR-10 | **每名玩家保留最近 100 条**:列表行与终局同一事务写入,同一事务把超出 100 条的最旧行删掉;正文记 `ref_count`(还列着它的玩家数),行被删时减一,减到 0 删正文。没有按天数的清理 | 负责人 2026-11-06 定。玩家同一时刻只有一场战斗,一名玩家的列表行只被他自己那一局的终局事务改,挤出没有并发;正文按 `battle_id` 升序减引用,并发的终局事务不会死锁 |
| BR-11 | **PVP 战报列表在 match**,与近期战斗结果(玩家存档,10 局)分开 | 100 局放进玩家存档的 PROFILE 行太大(近期战斗结果按 10 局估约 1 KB、封顶 8 KB);战报列表的权威与正文同在 match 库,同一事务维护 |

## 5. 协议

### 5.1 回放正文与外壳(`proto/battle/player_battle.proto` 末尾)

放在 `player_battle.proto`:`BattleStateS2C`、`TurnResultS2C` 定义在这里,而它 import 了 `battle_data.proto`,放到 `battle_data.proto` 会循环引用。只加消息、不加 RPC,不影响 `BattleClientPlayer` 的发号。

```protobuf
enum eBattleReplayCodec {
  BATTLE_REPLAY_CODEC_NONE = 0;   // 未压缩。v1 不产出,留给「match 解压后下发」(§8 第 6 条)
  BATTLE_REPLAY_CODEC_GZIP = 1;   // RFC 1952。v1 只产出这一种
}

enum eBattleReplayEndReason {
  BATTLE_REPLAY_END_UNSPECIFIED = 0;
  BATTLE_REPLAY_END_FINISHED = 1;       // 引擎分出胜负(含回合打满判负、全员逃跑)
  BATTLE_REPLAY_END_DEADLINE_DRAW = 2;  // 整场期限到,battle 节点强制判平;最后没有回合帧,看 final_state
}

message BattleReplay {
  uint64 battle_id = 1;
  uint32 match_mode = 2;
  uint32 battle_config_id = 3;
  uint64 started_at_ms = 4;            // CreateBattleRequest.created_at_ms
  uint64 finished_at_ms = 5;           // 与 BattleResultEvent.finished_at_ms 同值
  eBattleOutcome outcome = 6;
  eBattleReplayEndReason end_reason = 7;
  uint32 total_rounds = 8;             // 引擎结算过的回合数,不受截断影响
  string table_fingerprint = 9;
  BattleStateS2C start_state = 10;     // action_deadline_ms 为 0,pending_actor_ids 为空
  repeated TurnResultS2C rounds = 11;
  BattleStateS2C final_state = 12;     // 只在 DEADLINE_DRAW 时填
  bool truncated = 13;
}

message BattleReplayEnvelope {
  uint32 format_version = 1;  // v1 = 1
  eBattleReplayCodec codec = 2;
  uint32 raw_bytes = 3;       // 解压出来超过它或超过 1 MiB 就判坏(防解压炸弹)
  bytes payload = 4;
}
```

录进 `start_state`、`rounds[*].state`、`final_state` 的状态都清掉两项只对实时有用的字段:`action_deadline_ms` 置 0,`pending_actor_ids` 清空。

### 5.2 建房原件(`proto/battle/battle_node.proto`)

`CreateBattleRequest.record_replay = 16`(bool)。缺省 false = 不录,老发送方和漏填一律不录。

### 5.3 终局请求(`proto/match/match_internal.proto`)

`CommitBattleOutcomeRequest.replay = 6`(`BattleReplayEnvelope`),文件加 import `proto/battle/player_battle.proto`。`raw_result_payload` 不动:`BattleResultEvent` 会经发件箱进 Kafka,上限 960 KiB(`completion.go` `maxCompletionBytes`),回放不能塞进去。

### 5.4 客户端查询(`proto/match/match_service.proto`)

```protobuf
service MatchService {
  ...
  rpc ListBattleReports(ListBattleReportsRequest) returns (ListBattleReportsResponse) {}
  rpc GetBattleReport(GetBattleReportRequest) returns (GetBattleReportResponse) {}
}

message BattleReportMember {        // 开战时的快照,不含宝宝和怪物
  uint64 player_id = 1;
  uint32 team_index = 2;            // 0 = A 方(进攻),1 = B 方
  string name = 3;
  uint32 level = 4;
  uint32 class_id = 5;
}

message BattleReportSummary {       // 终局提交时由 match 生成,之后不变
  uint64 battle_id = 1;
  MatchMode mode = 2;
  uint32 battle_config_id = 3;
  uint64 finished_at_ms = 4;
  eBattleOutcome outcome = 5;       // 本人胜负看本人在 members 里的 team_index
  uint32 total_rounds = 6;
  repeated BattleReportMember members = 7;  // 按 team_index、player_id 升序
  bool has_replay = 8;              // false = 回放没存下来,GetBattleReport 回 kMatchBattleReportNotFound
}

message ListBattleReportsRequest {}
message ListBattleReportsResponse {
  TipInfoMessage error_message = 1;
  repeated BattleReportSummary reports = 2;  // 新的在前,最多 100 条,一次全给
}

message GetBattleReportRequest { uint64 battle_id = 1; }
message GetBattleReportResponse {
  TipInfoMessage error_message = 1;
  BattleReplayEnvelope replay = 2;
}
```

### 5.5 存储(`proto/match/match_authority.proto`)

| 表 | 主键 | 列 | 说明 |
|---|---|---|---|
| `match_battle_report` | `battle_id` | `created_ms`、`match_mode`、`battle_config_id`、`envelope`(确定性序列化的外壳)、`envelope_digest`(sha256)、`ref_count` | 回放正文。只有回放合格才写;`ref_count` 初值 = 参战人数 |
| `match_player_battle_report` | `(player_id, created_ms, battle_id)` | `summary`(确定性序列化的 `BattleReportSummary`) | 每名玩家的 PVP 战报列表,按 `created_ms`、`battle_id` 倒序取前 100 |

`created_ms` 都是终局事务里的 match 库时间(`now(tx)`),不用 battle 节点的钟。`bytes` 字段由迁移器建成 MEDIUMBLOB。两张表加进 `authority.Tables()`;主键之外没有二级索引:列表、挤出和鉴权都是一名玩家主键前缀内(最多 100 来行)的范围读。`battle_id` 是发号器发的打散值,正文表写入不集中在一个 Region。

### 5.6 tip 码

`data/tip/Tip.xlsx` 的 `match_error` 段加了 `MatchBattleReportNotFound`,文案「这一局没有回放，或已不在你的战报列表中」,业务拒绝。导表后生成 `kMatchBattleReportNotFound`,match 的 `constants.ErrBattleReportNotFound` 引用它。存储故障与缺会话身份沿用 `kMatchInternal`。

## 6. battle 侧(C++)

### 6.1 录制模块

`cpp/libs/services/battle/system/battle_replay_recorder.{h,cpp}`,在 battle 库里,不依赖 muduo 网络、ECS 和节点。

- 构造:记头部字段(battle_id、模式、配置、开始时间、配表指纹)和开战状态。
- `AddRound`:追加一回合;累计字节超过录制预算(1 MiB − 16 KiB)后不再追加,置 `truncated`,打 `metric=battle_replay_truncated`。
- `MarkDeadlineDraw`:强制判平时记 `final_state` 和结束原因。
- `Freeze(outcome, totalRounds, finishedAtMs)`:填结局字段,序列化,gzip(级别 1);序列化失败、原始超 1 MiB、压缩失败、压缩后超 512 KiB 都返回空并打 `metric=battle_replay_dropped reason=...`;成功打 `metric=battle_replay_frozen`(回合数、原始字节、压缩后字节、压缩耗时)。冻结后释放已录的帧,之后的调用都无效。
- 输入必须是观众版帧,模块自己不做脱敏;只在 loop 线程调用;失败不抛异常、不影响战斗。
- `GzipCompress` / `GzipDecompress` 封 zlib 的 `deflateInit2` / `inflateInit2`(windowBits 15 + 16)。没用 muduo 的 `ZlibOutputStream`:它写 zlib 格式(RFC 1950)、级别固定,客户端不好直接解。

### 6.2 接入点(`cpp/nodes/battle/logic/battle_room_manager.{h,cpp}`)

| 位置 | 改动 |
|---|---|
| `BattleRoom` | 加 `std::optional<turnbattle::BattleReplayRecorder> replay`(`record_replay` 为真才创建)和 `std::optional<BattleReplayEnvelope> outcomeReplay`(冻结结果) |
| `HandleCreateBattle` | 开战帧建好之后,`record_replay` 为真时用它的观众版构造录制器 |
| `ResolveRound` | `action_order` 填完之后、广播或 `BeginResultCommit` 之前调 `RecordReplayRound`(拷一份过 `RedactStateForViewer(state, 0)`)。最后一回合也录 |
| `OnBattleDeadline` | 判平前对当前状态的观众版调 `MarkDeadlineDraw` |
| `BeginResultCommit` | 终局原件校验通过之后调 `Freeze`,结果存进 `outcomeReplay`,释放录制器,再经 `BuildOutcomeRequest` 生成原件并算摘要 |
| `SendSqlOutcome` | 每次发送都经 `BuildOutcomeRequest` 拼请求,再和冻结时的摘要比。首次与重放只有这一处拼法,新字段漏拷就会让摘要永远对不上 |

### 6.3 内存与 CPU

- **房间内存**:录制按序列化字节累计,有界;消息对象本身另有开销,要实测(§9)。节点内存吃紧时改成每回合只存序列化字节、冻结时再拼,协议不用改。
- **CPU**:序列化和压缩只在冻结时做一次,跑在 loop 线程上;按 zlib 最快档的量级,1 MiB 大约几毫秒到十几毫秒(未测)。回合制每回合至少 2 秒(`kAutoRoundIntervalMs`),这点阻塞可以接受;实测超标再改成每回合增量压缩。
- **重放摘要**:`SendSqlOutcome` 每次发送都对整份请求算一次摘要。match 长时间不可达、大量房间同时重放时这部分 CPU 会上升,要在实测里看;超标就冻结时把请求序列化一次存下来。

## 7. match 侧(Go)

### 7.1 录制策略(`go/match/internal/logic/battle_replay_policy.go`)

`recordReplayFor(enabled, mode)` 是唯一决策点,`replayPolicyOfMode` 每个 `MatchMode` 一行、各写理由,表外的值不录。`gather.go` 建 `CreateBattleRequest` 时写 `RecordReplay`。全局开关 `BattleReport.Enabled`(yaml,缺省 true)为假时一律不录。`TestReplayPolicyDecidesEveryMatchMode` 遍历 `MatchMode_name`,新增模式没进表就红。

| 模式 | 录不录 |
|---|---|
| `1V1`、`5V5`、`PVP_TEAM`、`PVP_CHALLENGE` | 录 |
| `3V3` | 录(未开放;开放后按 PVP 录) |
| `PVE_SOLO`、`PVE_TEAM` | 不录。帮会历练等活动局也走 `PVE_TEAM`,所以也不录 |
| `UNSPECIFIED` | 不录 |

### 7.2 写入(`completion.go` + `battle_report.go`)

`commitBattleOutcomeLocked` 写完全员个人结算原件之后、更新成局行之前调 `writeBattleReportTx`:

1. 解析成局行上的建房原件;`record_replay` 为假就不写任何战报(请求带了回放时记 `reason=not_requested`)。
2. 查回放外壳:`format_version == 1`、`codec == GZIP`、`raw_bytes` 与 `payload` 都非空且不超上限。不合格只丢回放(记 `invalid_envelope` / `too_large`),**不返回错误**(BR-7)。
3. 回放合格时写 `match_battle_report`,`ref_count` = 参战人数。
4. 按 `player_id` 升序,给每名参战者写一条列表行,再读他主键前缀内第 101 条起的旧行(每次最多 16 条),逐条按主键删,只按真正删掉的行(`RowsAffected = 1`)记下要减的引用数。
5. 按 `battle_id` 升序给被挤出的正文减 `ref_count`(`IF(ref_count > n, ref_count - n, 0)`),减到 0 就删正文;那一局没存正文时两条语句都影响 0 行。
6. 任一步的 SQL 错误都按普通错误返回:整笔回滚,返回「未确认」,battle 按原字节重放。提交成功之后才记写入与挤出指标。

重放(锁外快速路径,库里已是 COMPLETE)时 `checkBattleReportReplay` 比对请求里的回放与库里的 `envelope_digest`:库里有这一行而摘要不同才记 `match_battle_report_replay_mismatch`,回执照常;库里没有就不比(第一次提交时可能被合法丢弃,或者正文已经被挤出删掉)。ABORT 路径不碰战报。

**为什么 SQL 错误不也 fail-open**:SQL 错误和事务里其他行一样是瞬时的;一定会失败的情形(超大、格式不对)在写之前就挡掉了。在事务里吞掉错误要开保存点,或者让整笔事务处在未知状态,反而更复杂。

**事务变大**:每名参战者在满 100 条之后,每局多 3 条语句(插入、读超出行、删一行),每个被挤出的正文多 2 条(减引用、删正文)。1v1 约多 10 条,5v5 约多 50 条;正文常见几 KB 到几十 KB,最大 512 KiB。事务持有成局行和票据行的锁,提交变慢就多持一会儿锁,要在真库用例里对比终局提交耗时(§17)。

### 7.3 查询(`go/match/internal/logic/battlereportlogic.go`)

照 `requestbattleticketlogic.go`:会话身份取自 `ctxkeys.GetSessionDetails`,没有就回 `kMatchInternal`(fail-closed)。

- `ListBattleReports`:`Store.BattleReports` 读本人列表行,按 `(created_ms, battle_id)` 倒序取前 100,反序列化 `summary` 原样返回。
- `GetBattleReport`:`battle_id` 为 0 直接回 `kMatchBattleReportNotFound`;`Store.BattleReport` 先在本人主键前缀内查这一局,再按主键读正文。列表里没有、正文不存在都回 `kMatchBattleReportNotFound`。
- 都不开事务、不加锁,存储错误回 `kMatchInternal`,不在服务端重试。

**限频**:gate 的按消息限频(`CheckMessageLimit` → `MessageLimiter` 表,没有配置行时用默认额度)。`data/MessageLimiter.xlsx` 已加两行(11-10):`380`(`GetBattleReport`)每秒 1 次、`379`(`ListBattleReports`)每 2 秒 1 次,`tip_message` 与同表其他行相同(1000);导表后生效(§17 第 1 步)。单次回包最大约 512 KiB,即使连发几次也到不了 gate 2 MiB 的高水位(`client_session_handler.cpp` `kClientHighWaterMark`)。

### 7.4 依赖与跨库

- 战报两张表和终局、个人结算原件都在 `mmorpg_match` 这一个逻辑库里,同一事务不跨库,`CrossDatabase` 契约测试不受影响(AGENTS §7 第 6 条)。以后要把战报拆到单独的逻辑库,就不能再同事务提交,要改成发件箱,另立方案。
- 玩家角色删除后,他的列表行(最多 100 条)留在表里,数量有界;要清理时随角色删除流程另做,并照 §7.2 第 5 步减引用。

## 8. 客户端契约

细节与验收见 [客户端交接](../handoff/battle-report-replay-client.md)。要点:

1. **入口**:战报列表调 `ListBattleReports`;近期战斗结果(`battle-result-recovery.md` §7)里的 PVP 局也可以直接用 `battle_id` 调 `GetBattleReport`。`has_replay = false` 的条目不显示「看回放」。
2. **解码**:`codec == GZIP` 就 gzip 解压;解压出来超过 `raw_bytes` 或 1 MiB 就判坏;`format_version` 不认识就提示升级;再按 `BattleReplay` 解析。
3. **播放**:`start_state` 当开战帧,`rounds` 逐个当回合帧,走观战的播放流程。每帧都带结算后的全量状态,可以从任一回合开始播。
4. **结局**:看 `outcome` 和 `end_reason`。`DEADLINE_DRAW` 时最后显示 `final_state` 和「超时平局」;`truncated` 时在最后一帧之后提示「回放只录到第 N 回合」,结局照 `outcome` 显示。
5. **版本**:`table_fingerprint` 和客户端当前配表对不上时,可以提示「游戏已更新,技能显示可能与当时不同」。
6. **兜底**:某个平台没有可用的 gzip 时,改成 match 解压后下发 `codec = NONE`,回包变大但仍在上限内。这只是 match 一处改动,协议不变。

## 9. 容量与上限

**各段上限**

| 段 | 上限 | 位置 |
|---|---|---|
| battle → match gRPC 请求 | 4 MiB(muduo_grpc 发送上限,对齐 grpc-go 默认接收上限) | `muduo_grpc/channel.h` `maxSendMessageBytes` |
| 终局原件 `raw_result_payload` | 960 KiB | `completion.go` `maxCompletionBytes` |
| 回放外壳 payload | 压缩后 512 KiB;原始 1 MiB | BR-5 |
| match → gate 回包 | 4 MiB(gate 侧 muduo_grpc 接收上限) | `muduo_grpc/client_session.h` `maxReceiveMessageBytes` |
| gate → 客户端 | 单连接输出缓冲 2 MiB 高水位,超过就强关大厅连接 | `client_session_handler.cpp` `kClientHighWaterMark` |
| 存储列 | MEDIUMBLOB 16 MiB;TiDB 单条 KV 默认 6 MiB | proto2mysql 映射 |

终局请求最大约 960 KiB + 512 KiB,在 4 MiB 以内。单次取回放的回包最大 512 KiB;列表回包 100 条,每条约 100~400 字节。

**体积估算(未实测,只是量级)**

- 一个单位的观众版状态约 240 字节;一个事件约 45 字节;每回合每个单位约 2~4 个事件。
- 1v1 带宝宝每回合约 1.6 KB;5v5 不带宝宝约 4 KB;两边各 5 人 + 5 宝宝约 7 KB。
- 回合数受整场期限 300 秒约束。全自动局每回合间隔 = max(2 秒, 出手单位数 × 250 毫秒)(`turn_battle_constants.h` `AutoRoundIntervalMsFor`):1v1 最多约 150 回合,5v5 最多约 120 回合,20 个单位最多约 60 回合。手动局行动窗口 25 秒,双方都秒出手时回合可以更快。
- 所以全自动 PVP 局的原始体积大约在 240~480 KB 以内,都在 1 MiB 以内;只有手动局双方一直秒出手、拖到期限,才可能碰到上限被截断。帧之间高度重复,gzip 压缩比应该很高,具体要实测。

**存储**:正文行数 ≈ 每天 PVP 局数 × 正文平均存活天数;列表行数 ≤ 有过 PVP 的玩家数 × 100。玩家打满 100 局之前正文一直留着,打得少的玩家的战报会存得很久,这是「保留 100 条」的本意。上线前按 PVP 局数与实测平均大小算一次,写回本节。

**实测用例(11-10 已写,待 Codex 运行)**:`turn_battle_engine_test.cpp` 末尾的 `BattleReplaySize.AutoBattlesStayWithinReplayLimits`,用内存数据源跑 1v1 带宝宝 150 回合、5v5 120 回合、5v5 带宝宝 60 回合三种全自动局,逐回合录观众版帧,每种打印一行 `[replay-size] case=… rounds=… truncated=… raw_bytes=… payload_bytes=… freeze_us=…`,并断言不超过两个上限、压缩后更小。Codex 跑出的数字写回本节,再确认 BR-5 的两个上限和压缩级别;数字是 Debug 构建下的量级,不作性能结论。

## 10. 失败与一致性

| 情形 | 结果 |
|---|---|
| battle 在提交终局之前崩溃 | 房间和帧一起丢;match 到期判 ABORT,没有战报。和落码前一致 |
| 终局提交结果未知 | battle 按冻结的整份请求(含回放)重放,摘要不变;match 按原字节幂等,不重复写 |
| 重放时回放摘要对不上 | 只记错误指标,返回已持久的决定,库里那份不改(BR-7)。正常情况下不会发生:battle 冻结之后逐字节重放 |
| 回放超上限 / 格式不对 / 版本不认识 | 只丢回放、记指标;列表条目照写,`has_replay = false`;终局照常提交 |
| 原件没让录,请求却带了回放 | 丢掉回放、记 `not_requested`;不写任何战报 |
| 录制超过预算 | 停录后面的回合,标 `truncated`;终局不受影响 |
| 压缩失败,或压缩后超过 512 KiB | battle 不发回放,记指标;match 仍按原件写列表条目(`has_replay = false`) |
| 写战报的 SQL 失败 | 整笔回滚,返回「未确认」,battle 重放(和其他行相同) |
| 同一名玩家的两个终局事务同时挤出(不应发生) | 删行只按 `RowsAffected = 1` 计数,引用数不会多减;列表可能暂时多一条,下一局再挤 |
| 查询与挤出同时发生 | 读到就返回,读不到就回 `NotFound`;都是单行或单前缀操作,没有中间态 |
| match 不可用 | 战报查不到;终局本来也在等 match。近期战斗结果在 scene,不受影响 |

## 11. 兼容与上线

项目尚未上线(AGENTS「上线状态」),不用迁移存量数据。新旧版本混跑时也安全,上线后同样成立:

- **新 battle + 老 match**:老 match 不填 `record_replay`,新 battle 就不录,不会发带新字段的终局请求。match 会把带未知字段的终局请求当成永久冲突拒掉(`completionHasUnknown`),所以**只有 match 让录,battle 才发回放**这条必须守住,由 battle 节点用例钉住(§17)。
- **新 match + 老 battle**:老 battle 忽略 `record_replay`,不发回放;match 照常写终局和列表条目(`has_replay = false`)。
- **回滚**:先把 `BattleReport.Enabled` 关掉,等在途的局打完(一场的最长期限),再回滚 battle 和 match。已存的战报照样能查,表保留。
- 上线后字段号不复用(AGENTS §4)。

## 12. 以后再做

- **P2 分享与公开局**:分享时由 match 签一个短期凭证,非参战者凭证查看;公开局 / 赛事局可以让所有人看。鉴权改成「列表里有,或持有效凭证,或公开局」。
- **P2 按副本录 PVE**:副本配置表加一列,BOSS 局这类单独打开。
- **P3 重算校验**:另记每回合被接受的指令、挂机开关、引擎版本和节点强制终局,GM 可以重跑核对(`configurable-templates.md` §8.6.6)。
- **按需:冷归档**:按实测容量和需求定。

## 13. 负责人决定(2026-11-06)

§13 的五项先按建议拍板,同日负责人又改了第 1、2 项:

1. **哪些模式录**:只录 PVP(§7.1 的表)。原来的「活动局也录」取消。PVE 以后按副本单独打开(§12)。
2. **保留多少**:每名玩家保留最近 100 条 PVP 战报(BR-10),取代原来的「保留 7 天」。
3. **录制上限与截断**:原始 1 MiB、压缩后 512 KiB;原始超限就截断后面的回合,压缩后超限就整份不存(BR-5)。§9 实测之后如果要调,改两版的常量并同步本文。
4. **谁来解压**:客户端解压,省流量。某个平台没有可用的 gzip 时,改成 match 解压后下发(§8 第 6 条),协议不变。
5. **非参战者能不能看**:v1 只给列表里有这一局的玩家;分享和公开局放 P2(§12)。

## 14. 落码清单(2026-11-06 落码;11-10 补限频配置、体积用例与 robot 核对)

**proto 与表(客户端契约;Java 版待同批改,§16)**

- `proto/battle/player_battle.proto`:§5.1 的四个类型。
- `proto/battle/battle_node.proto`:`CreateBattleRequest.record_replay = 16`。
- `proto/match/match_internal.proto`:`CommitBattleOutcomeRequest.replay = 6`,加 import。
- `proto/match/match_service.proto`:`ListBattleReports`、`GetBattleReport` 与五个消息。
- `proto/match/match_authority.proto`:`MatchBattleReportRecord`、`MatchPlayerBattleReportRecord`。
- `data/tip/Tip.xlsx`:`match_error` 段加 `MatchBattleReportNotFound`。
- `data/MessageLimiter.xlsx`:`379`、`380` 两行限频(11-10)。

**C++**

- 新增 `cpp/libs/services/battle/system/battle_replay_recorder.{h,cpp}`,登记进 `battle.vcxproj` 与 `.filters`(Linux 的 CMakeLists 由 `build_linux.sh` 按 vcxproj 生成,不手改)。
- `cpp/nodes/battle/logic/battle_room_manager.{h,cpp}`:§6.2 的接入点、`BuildOutcomeRequest`、`RecordReplayRound`。
- 测试:新增 `cpp/tests/turn_battle_engine_test/battle_replay_recorder_test.cpp`(已登记进该测试工程);`cpp/nodes/battle/tests/battle_observer_contract_test.cpp` 加两个用例和 `BattleRoomManagerTestAccess::ForceDeadline`;`turn_battle_engine_test.cpp` 末尾加体积实测用例(11-10,§9)。

**robot(11-10)**

- 新增 `robot/battle_report_smoke.go`(查列表、取回放、gzip 解压核对,与客户端交接同一口径)与 `battle_report_smoke_test.go`(解码的正常与各类坏外壳);`battle_smoke_cross_zone_scenario.go` 在双方终局之后加第 5 步,通过时打一行 `BATTLE_REPORT_OK`,失败按原约定打 `CROSS_ZONE_MATCH_FAIL step=a-battle-report / b-battle-report`。

**Go**

- 新增 `go/match/internal/logic/battle_replay_policy.go`(+ `_test.go`);`gather.go` 写 `RecordReplay`。
- 新增 `go/match/internal/authority/battle_report.go`(+ `_test.go`);`completion.go` 两处调用;`schema.go` 的 `Tables()` 加两张表。
- 新增 `go/match/internal/logic/battlereportlogic.go`;`server/matchserviceserver.go` 加两个方法。
- `go/match/internal/config/config.go`:`BattleReport.Enabled`。
- `go/match/internal/constants/errors.go`:`ErrBattleReportNotFound`(`errors_test.go` 的 `tipCodes()` 同步)。
- 新增 `go/match/internal/metrics/battle_report.go`;`metrics.go` 注册。

**不动的部分**:`result_commit.h`、`BattleSettlementData`、`PlayerBattleResult`、scene 结算应用点都归战斗结果重登恢复,本批没碰。

## 15. 日志与指标

player_id、battle_id 只进日志字段,不进任何指标 label。

| 指标 / 日志 | 位置 | 含义 |
|---|---|---|
| `metric=battle_replay_frozen`(INFO,回合数、是否截断、原始字节、压缩后字节、压缩耗时) | battle | 一局回放冻结成功 |
| `metric=battle_replay_truncated`(WARN) | battle | 超过录制预算,截断 |
| `metric=battle_replay_dropped reason=serialize_failed / too_large / compress_failed` | battle | 不发回放 |
| `match_battle_report_stored_total` | match | 写入的正文行数 |
| `match_battle_report_dropped_total{reason}` | match | 丢弃回放:`invalid_envelope` / `too_large` / `not_requested` |
| `match_battle_report_evicted_total` | match | 被挤出的列表行数 |
| `match_battle_report_replay_mismatch_total` | match | 重放时回放摘要不一致。应恒为 0,非 0 告警 |
| `match_battle_report_get_total{rpc, result}` | match | 查询:`rpc` = `list` / `get`,`result` = `ok` / `not_found` / `no_session` / `error` |

## 16. Java 版

状态:**待做**,已在 Java 仓 `PARITY.md` 登记。服务端内部按 Java 的写法做;客户端契约(§5.1、§5.4、tip 码)与本仓逐字段相同(AGENTS §12),每人保留条数、录制上限、gzip 与只录 PVP 的规则也相同。

- **battle**:`game-battle/src/main/java/com/game/battle/room/BattleRoom.java`、`BattleRoomServiceImpl.java` 在同样的四个点录制;观众视角复用 `ViewerState.java` 的脱敏;压缩用 `java.util.zip.GZIPOutputStream`;终局请求在 `BattleConfiguration.java` 的提交处拼装,首次和重放共用一处。
- **match**:`game-match/.../global/GlobalMatchStore.java` 的终局事务里写 Java 自己的两张战报表,挤出与减引用照 §7.2;建房原件照 §7.1 写 `record_replay`;两条查询各加一个 handler,在 `dispatch/MatchMethods.java` 登记。
- **proto**:`game-proto/src/main/proto/proto/` 下的同名文件同批改。

## 17. 验证(交 Codex,AGENTS §10.1)

任一步失败即停,保留完整输出,不改断言、不跳过。下面每一条 Claude 都没有运行过。已有的运行证据见文首状态,这里只列还没跑的部分。

1. **导表**:在仓库根目录跑导表器,确认 `MessageLimiter` 的生成结果里有 `379`、`380` 两行。proto 与 tip 码已经生成过(消息号 379 / 380、`kMatchBattleReportNotFound = 16025`),之后没有再改战报相关的 proto,不用重跑 `proto-gen-run`;如果有人又改了 proto,按 AGENTS §4 用 `-ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml` 重新生成。
2. **C++(串行 `/m:1`)**:先编 battle 库(`cpp/libs/services/battle/battle.vcxproj`),再编并运行 `turn_battle_engine_test`(`pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter turn_battle`;测试工程链接预编好的库,改了库要先编库)。重点看 `BattleReplaySize.AutoBattlesStayWithinReplayLimits`:通过,并保留它打印的三行 `[replay-size]`,数字写回 §9。录制器与战斗节点用例 11-08 已通过,源码没变,`rpc_controller_test` 不必为本项重跑。
3. **Go 真 TiDB**:在 `go/match` 下用现有的隔离 TiDB 夹具设好 `MATCH_IT_TIDB`(做法与内存门槛参照 `build/verification/battle-handoff-20261106-r4/` 的真库续验),跑 `go test ./internal/authority/... -run 'BattleReport' -count=1 -v`,四个 `TestBattleReportTiDB*` 不许 SKIP;再跑 `-run 'TerminalTiDB|CompletionTiDB'`,确认终局事务加了战报之后原有真库用例不受影响,并记录 `CommitBattleOutcome` 的耗时。match 的 build / vet / 纯逻辑用例 11-06 已通过;本次 `battle_report.go` 只把日志与错误文案改成英文,顺手跑一次 `go build ./... && go vet ./...` 即可。
4. **robot**:在 `robot` 下跑 `go build ./...`、`go vet ./...`、`go test -run 'DecodeBattleReplay' -count=1 .`。
5. **冒烟**:起本地栈(match 接流前要先迁移出两张新表;玩家数据可以清空,常规启动流程里的迁移会建表),跑 battle-smoke 的 cross_zone 子模式(1V1):日志里先出现 `BATTLE_REPORT_OK battle_id=…`,再出现 `CROSS_ZONE_MATCH_OK`。失败时按 `CROSS_ZONE_MATCH_FAIL step=a-battle-report / b-battle-report` 的原因排查,保留 match 日志里的 `metric=match_battle_report_*` 与 battle 日志里的 `metric=battle_replay_*`。PVE 局不录由 `TestRecordReplayOnlyPVP` 覆盖,冒烟不另查。

**用例覆盖**

| 层 | 用例 |
|---|---|
| 录制模块 | 往返逐字段相同、实时字段已清;强制判平记 `final_state`;超预算截断并标记、`total_rounds` 不受影响;压缩后超 512 KiB 返回空;只能冻结一次;gzip 往返、超上限 / 截断 / 尾部校验坏都返回空 |
| battle 节点 | 原件不让录时终局请求不带回放;让录时带回放、最后一回合录进去、帧是观众版;强制判平记 DEADLINE_DRAW 与 `final_state`。请求能发出即说明摘要与冻结时一致 |
| match 写入 | 带回放 → 同事务写正文与双方列表、`ref_count = 2`;同字节重放不重复写;换回放重放回执照常、库里不变;原件不让录 → 不写;外壳不合格 → 列表照写、没有正文、取回放 NotFound |
| 挤出 | 第 101 局挤掉最旧一条;另一名参战者还列着时正文保留、引用减一;最后一个人挤掉时正文删除;没有正文的局被挤出不报错 |
| match 查询 | 参战者取得到;非参战者 NotFound(在 `TestBattleReportTiDBCommitStoresReportWithOutcome` 里) |
| 策略 | 每个 `MatchMode` 都在表里;只录 PVP;开关关闭或表外模式不录 |
| 体积 | `BattleReplaySize.AutoBattlesStayWithinReplayLimits`(11-10 已写,待运行),数字写回 §9 |
| robot | 解码:gzip 往返、未压缩、未知格式 / 编码、长度不符、截断、不是 gzip、超上限;冒烟:双方列表有这一局、`has_replay`、回放 battle_id 与回合数、结局与列表一致 |
