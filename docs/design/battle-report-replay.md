# 回合制战斗战报回放

> **状态(2026-11-05)**:设计稿。**未落码、未编译、未测试**。§13 的几项拍板之后再落码,验证按 §17 交 Codex。Java 版待做(§16)。
> **依据**:本仓 C++ battle、Go match 与 Java 版只读调研;[SLG 战斗笔记 Q5 / Q15](../notes/slg/slg-battle-interview-qa.md) 的事件日志式战报(该笔记是「率土风格」的面试笔记,不代表任何游戏的真实架构)。
> **分工**:玩家的「近期对局列表」不在本文,归 [战斗结果重登恢复](battle-result-recovery.md):玩家存档里最多 10 局,每条带 `battle_id`、`match_mode`、`battle_config_id`。本文只按 `battle_id` 存回放正文,并提供按 `battle_id` 取回放的接口。两份文档 2026-11-05 经会话间沟通对齐过。
> **编号**:本文决策从 BR-1 起编号,不占 turn-based 系列的 D 号。行号取自 2026-11-05 的工作区,其中有其他会话未提交的改动。

## 0. 结论

battle 在房间里记下开战状态和每回合帧。帧是观众视角,和观战下发的是同一份。终局冻结时压成 gzip,随 `CommitBattleOutcome` 一起交给 match。match 在写终局的同一个事务里存一行战报,所以只有 COMPLETE 的局才有战报。客户端从近期战斗结果列表拿到 `battle_id`,调 `MatchService.GetBattleReport` 取回放,解压之后按「开战状态 + 逐回合帧」播放,走和观战一样的播放流程。

- **战报出问题不挡结算**:战报坏了、太大、压缩失败,终局照常提交,只是这一局没有回放。
- **录不录由 match 决定**:建房时按模式决定,冻结进 `CreateBattleRequest`,和观战可见性同一个做法。
- **只许参战者看**:查不到、过期、没录、不是参战者,统一回一个码。
- **保留期按 match 库的时间清理**,默认天数待定(§13)。

## 1. 现状

| 需要的 | 现在有的 | 位置 |
|---|---|---|
| 开战时双方阵容 | 开战帧 `BattleStartS2C.state`:全部单位的名字、等级、职业、外观、站位、HP/MP、buff、技能 | `battle_room_manager.cpp:593-611`、`player_battle.proto:28-44` |
| 逐回合过程 | `TurnResultS2C`:事件序列 + 出手顺序 + 结算后的全量状态。事件带 `target_health_after` / `target_mana_after`,可以从任一回合开始播 | `player_battle.proto:46-52`、`battle_data.proto:156-202` |
| 公开视角 | 观众版帧统一清掉冷却、道具余量和装备概率(`RedactStateForViewer(state, 0)`) | `battle_room_manager.cpp:1512-1518`、`:1522-1543`、`turn_battle_engine.cpp:2744` |
| 落盘 | **没有**。`ResolveRound` 算完直接推直连,不保存;没有直连的人这一帧直接丢 | `battle_room_manager.cpp:1236`、`:1499-1500` |
| 终局原件 | `BattleResultEvent`:胜负、队伍、回合数、个人结算,经 `CommitBattleOutcome` 存进 `match_battle_completion`。**不含任何帧** | `battle_room_manager.cpp:1288-1352`、`completion.go:381-461` |
| 超时平局 | `OnBattleDeadline` 强制判平,没有最后一回合帧,结果里也不记是第几回合被强制结束 | `battle_room_manager.cpp:1239-1263` |
| 查询 | 客户端没有任何取历史战斗帧的协议。近期战斗结果列表只有胜负和本人奖励 | `match_service.proto:19-52`、[battle-result-recovery.md](battle-result-recovery.md) |

设计文档一直把这件事列为预留:turn-based §9 的「回放持久化(事件流落盘)」,观战分级 §2、§11 P3 的「归档消费者把帧落对象存储」。本文定下具体做法,这两处正文已改成指向本文。

## 2. 目标与非目标

**目标**

- 参战者事后能完整回看自己打过的一局:开战 → 每回合 → 结局,画面与当时观战看到的一致。
- 有战果才有战报,两者同一个事务提交。提交不成功的终局不留战报;也不会出现战果在、战报丢的情况(录制被截断或丢弃除外,见 BR-5、BR-7)。
- 战报出任何问题都不能挡住结算。
- C++ / Go 与 Java 同批;两版共享的只有客户端契约(AGENTS §12)。

**非目标(本期不做)**

- **近期对局列表**:归 [战斗结果重登恢复](battle-result-recovery.md)。match 不建按玩家的索引表。
- **按指令流重算校验**(SLG 笔记里 GM 仲裁那条):要另外记每回合被接受的指令、挂机开关、引擎版本和节点强制终局(`configurable-templates.md` §8.6.6)。这和客户端回放无关,P3 再做。
- **给非参战者看**:分享、公开局 / 赛事局人人可看,放 P2(§12)。
- **冷归档到对象存储**:按实测容量再定。
- **帮会 / 活动的战报列表**:有需求再做。
- **旧战报按当时的配表显示**:v1 按客户端当前配表显示。战报里记了配表指纹,客户端可以提示「版本已更新」(§8)。

## 3. 方案比较

| | A 随终局原件同事务提交(采用) | B 终局后单独提交 | C battle 发 Kafka,归档消费者落库 | D 只存输入 + 种子,客户端重算 | E 客户端本地录制 |
|---|---|---|---|---|---|
| 一致性 | 战报和战果同一事务;提交未知时按原字节幂等重放 | 两个提交点,battle 在两者之间崩溃就丢战报;要另做重试和幂等 | battle 没有持久化,发送失败就丢;至少一次投递要去重 | 输入在就能重算 | 只在本机,换设备就没了 |
| 新增部件 | 请求加一个字段、一张表、一个查询 RPC、一个清理循环 | 比 A 多一个 RPC,房间还要多留一阵 | 新 topic、新消费者、新存储 | 指令流记录;客户端要内嵌完整引擎 | 无服务端改动 |
| 客户端要求 | 播放器(复用观战播放) | 同 A | 同 A | 内嵌引擎;配表或逻辑一改,旧战报就对不上 | 同 A |
| 代价 | 终局请求变大(上限 512 KiB,§9);match 库多存数据 | 比 A 复杂,收益只是终局请求小一点 | 多一条链路,丢失窗口更大 | 指令流要另记;引擎暴露给客户端 | 别人看不了,也不权威 |

**选 A**:

- **不丢、强一致**:只有终局 COMPLETE 才有战报,而且写在同一个事务里。终局提交结果未知时,battle 按冻结的原字节重放(`battle_room_manager.cpp:1342-1351`),战报跟着一起幂等。这对应 SLG 笔记 Q15「战报和战果同一事务提交、提交后才推送」。
- **battle 仍然零持久化**(turn-based §7 第 7 条、§8):房间只在内存里攒帧,持久化仍然只有 match 一处。
- **改动集中**:不新建服务和链路;表、清理循环、查询接口都照 match 现有写法(§7)。
- B、C 都要多造一个提交点和对应的重试、去重。D 解决的是重算校验,不是播放。E 解决不了多设备和权威性。

## 4. 决策

| # | 决策 | 理由 |
|---|---|---|
| BR-1 | **存事件日志,不存输入重算**:战报 = 开战状态 + 每回合 `TurnResultS2C`(观众视角)+ 结局信息 | 客户端只需要播放器;配表或引擎改了,旧战报照样能播;客户端拿不到引擎逻辑。与 SLG 笔记 Q5 的选择一致。我们是手动回合,只有输入和种子重算不出来 |
| BR-2 | **复用现有消息,不做平行结构**:`BattleReplay` 直接装 `BattleStateS2C` 和 `TurnResultS2C` | 客户端回放可以复用观战的播放流程,观战下发的就是这两种消息的观众版;AGENTS §3 不许手写平行结构 |
| BR-3 | **观众视角**:录进战报的帧一律先过 `RedactStateForViewer(state, 0)`,与观众收到的逐字段相同 | 不会多出新的泄漏面:冷却、道具余量、装备概率都不在里面。代价是参战者回看时也看不到自己当时的冷却和道具余量 |
| BR-4 | **随终局原件同事务提交**:`CommitBattleOutcomeRequest` 加 `BattleReplayEnvelope replay`;match 在 `CommitBattleOutcome` 写 `match_battle_completion` 的同一个事务里写 `match_battle_report`。ABORT 不写 | 见 §3。只有 COMPLETE 的局有战报,玩家不会看到之后被作废的对局 |
| BR-5 | **录制上限**:原始字节 1 MiB,录制时按每帧的序列化大小累计,超过就停录后面的回合并标 `truncated`;压缩后超过 512 KiB 就整份不发 | 保证 battle→match 请求、match 存库、match→gate→客户端回包都远低于各段的 4 MiB 上限和 gate 的 2 MiB 高水位(§9);房间内存也有界。按 §9 的估算,正常节奏的局碰不到原始上限 |
| BR-6 | **gzip(RFC 1952)**:battle 压缩一次,match 原样存取、不解压,客户端解压 | battle 节点已链接 zlib;Go `compress/gzip`、Java `java.util.zip`、C# `GZipStream` 都是自带库,四端都不加依赖。gzip 自带 CRC32,客户端能发现数据损坏。match 不解析正文,也就不用懂格式 |
| BR-7 | **战报坏了不挡终局**:match 在事务前先查外壳(编码、版本、大小),在事务里查本局该不该录;不合格就丢掉战报、记指标,终局照常提交。重放时战报摘要对不上也只记错误,返回已持久的决定 | 战报是展示数据,结算是资产。AGENTS §11.3 允许展示数据走 fail-open,但要配日志、指标和测试(§10、§15、§17) |
| BR-8 | **录不录由 match 定,建房时冻结**:决策表 `replayPolicyFor(mode, activity)` 的结果写进 `CreateBattleRequest.record_replay`,随建房原件落库;battle 只认这个字段 | 和观战可见性同一个做法(`spectate_visibility.go`),只有一个决策点;新旧版本混跑时天然安全(§11);全局开关关掉后新局不录,已存的照样能查 |
| BR-9 | **只许参战者看**:请求不带 player_id,只认会话里的本人;按 `match_battle_settlement_source` 的主键 `(battle_id, player_id)` 点查,确认是参战者。查不到、过期、没录、不是参战者,统一回 `kMatchBattleReportNotFound` | 参战者名单在那张表里本来就有(终局同事务写),不另存一份;统一一个码,不泄露「这一局存在」 |
| BR-10 | **保留期按 match 库时间算**:行上记 `created_ms = now(tx)`,用 match 库的时间,不用 battle 节点的钟;清理循环沿 `(created_ms, battle_id)` 索引翻页删 | 照 `PurgeClosedSpectatorIntents` 的现成写法;不受 battle 节点时钟偏差影响 |
| BR-11 | **match 不建按玩家的列表**,也不提供列表接口;列表归战斗结果重登恢复 | 一条业务知识只有一个权威来源(AGENTS §11.2 DRY) |

## 5. 协议

### 5.1 回放正文与外壳(`proto/battle/player_battle.proto` 末尾追加)

放在 `player_battle.proto`,不放 `battle_data.proto`:`BattleStateS2C`、`TurnResultS2C` 定义在 `player_battle.proto`,而它 import 了 `battle_data.proto`,反过来放会循环引用。这里只加消息、不加 RPC,不影响 `BattleClientPlayer` 的发号。

```protobuf
// ---- 战报回放(docs/design/battle-report-replay.md)----
enum eBattleReplayCodec {
  BATTLE_REPLAY_CODEC_NONE = 0;   // 未压缩。v1 不产出,留给「match 解压后下发」(§8 第 6 条)
  BATTLE_REPLAY_CODEC_GZIP = 1;   // RFC 1952。v1 只产出这一种
}

enum eBattleReplayEndReason {
  BATTLE_REPLAY_END_UNSPECIFIED = 0;
  BATTLE_REPLAY_END_FINISHED = 1;       // 引擎分出胜负(含回合打满判负、全员逃跑)
  BATTLE_REPLAY_END_DEADLINE_DRAW = 2;  // 整场期限到,battle 节点强制判平;最后没有回合帧,看 final_state
}

// 解码后的回放正文。所有帧都是观众视角(BR-3),与 NotifySpectate* 下发的逐字段相同。
message BattleReplay {
  uint64 battle_id = 1;
  uint32 match_mode = 2;              // CreateBattleRequest.match_mode
  uint32 battle_config_id = 3;
  uint64 started_at_ms = 4;           // CreateBattleRequest.created_at_ms(Unix 毫秒)
  uint64 finished_at_ms = 5;          // 与 BattleResultEvent.finished_at_ms 同值
  eBattleOutcome outcome = 6;
  eBattleReplayEndReason end_reason = 7;
  uint32 total_rounds = 8;            // 引擎结算过的回合数(BattleResultEvent.total_rounds)
  string table_fingerprint = 9;       // 录制节点的战斗配表指纹(BattleTableFingerprint::Current())
  BattleStateS2C start_state = 10;    // 开战状态
  repeated TurnResultS2C rounds = 11; // 按回合序;每帧带结算后的全量状态,可以从任一回合开始播
  BattleStateS2C final_state = 12;    // 只在 DEADLINE_DRAW 时填:强制结束那一刻的状态
  bool truncated = 13;                // 超过录制上限(BR-5),rounds 只录到截断前
}

// 回放的外壳:battle → match 提交、match 存库、match → 客户端下发都用它。
message BattleReplayEnvelope {
  uint32 format_version = 1;          // BattleReplay 的格式版本,v1 = 1;客户端不认识就提示升级
  eBattleReplayCodec codec = 2;
  uint32 raw_bytes = 3;               // 解码后 BattleReplay 的字节数;解压出来超过它或超过 1 MiB 就判坏(防解压炸弹)
  bytes payload = 4;                  // codec 编码后的 BattleReplay
}
```

录进 `rounds` 的帧还要清掉两项只对实时有用的字段:`state.action_deadline_ms` 置 0,`state.pending_actor_ids` 清空。

### 5.2 建房原件(`proto/battle/battle_node.proto`)

```protobuf
message CreateBattleRequest {
  ...
  // 本局是否录战报回放(battle-report-replay.md BR-8)。match 按 replayPolicyFor 决定、随原件冻结;
  // battle 只认它。缺省 false = 不录,老发送方和漏填一律不录。
  bool record_replay = 16;
}
```

字段号以落码时的最大号加 1 为准,现在最大的是 15 `spectate_visibility`。

### 5.3 终局请求(`proto/match/match_internal.proto`)

```protobuf
message CommitBattleOutcomeRequest {
  ...
  // 战报回放(battle-report-replay.md BR-4)。空 = 本局不录,或录制被丢弃;不影响终局。
  // 与 raw_result_payload 一起冻结:重放时逐字节相同,摘要和签名都覆盖它。
  BattleReplayEnvelope replay = 6;
}
```

`match_internal.proto` 要加 import `proto/battle/player_battle.proto`。`raw_result_payload` 不动:`BattleResultEvent` 会经发件箱进 Kafka,上限是 `maxCompletionBytes = 960 KiB`(`completion.go:25`),战报不能塞进去。

### 5.4 客户端查询(`proto/match/match_service.proto`)

```protobuf
service MatchService {
  ...
  // 战报回放:只许本局参战者取。请求不带 player_id,只认会话本人。
  rpc GetBattleReport(GetBattleReportRequest) returns (GetBattleReportResponse) {}
}

message GetBattleReportRequest {
  uint64 battle_id = 1;
}

message GetBattleReportResponse {
  TipInfoMessage error_message = 1;  // 0 = 成功;查不到 / 过期 / 没录 / 不是参战者统一回 kMatchBattleReportNotFound
  BattleReplayEnvelope replay = 2;   // 成功时非空,就是存库的那一份
}
```

### 5.5 存储(`proto/match/match_authority.proto`)

```protobuf
// 战报回放正文(battle-report-replay.md)。与 match_battle_completion 同一事务写,只写 COMPLETE 的局。
// 参战者名单不在这里存:鉴权读 match_battle_settlement_source 的主键(BR-9)。
message MatchBattleReportRecord {
  option (OptionTableName) = "match_battle_report";
  option (OptionPrimaryKey) = "battle_id";
  option (OptionIndex) = "created_ms,battle_id";
  uint64 battle_id = 1;
  uint64 created_ms = 2;        // match 库时间 now(tx),保留期按它算(BR-10)
  uint32 match_mode = 3;        // 运维统计用,读路径不看
  uint32 battle_config_id = 4;  // 同上
  bytes envelope = 5;           // 确定性序列化的 BattleReplayEnvelope(MEDIUMBLOB)
  bytes envelope_digest = 6;    // sha256(envelope),重放核对用
}
```

迁移器把 `bytes` 建成 MEDIUMBLOB(`proto2mysql` 的映射),上限 16 MiB,远大于 512 KiB。表加进 `authority.Tables()`(`schema.go:25`);`(created_ms, battle_id)` 加进 `CheckCandidateIndexes`(`schema.go:42`),缺索引就拒绝接流。`battle_id` 是发号器发的打散值,主键写入不集中在一个 Region。

### 5.6 tip 码

`data/tip/Tip.xlsx` 的 `match_error` 段加一行 `kMatchBattleReportNotFound`,文案「这一局没有回放,或者回放已过期」,算业务拒绝(fault 列留空)。存储故障沿用 `kMatchInternal`。数字由导表器发,不手写(AGENTS §4 第 4 条)。

## 6. battle 侧(C++)

### 6.1 录制模块

新文件 `cpp/libs/services/battle/system/battle_replay_recorder.{h,cpp}`,放在 battle 库里。它不依赖 muduo、ECS 和节点,可以在 `turn_battle_engine_test` 里单测。

接口与契约:

- `Start(const BattleStateS2C& spectatorState, header)`:记开战状态和整场不变的头部字段(battle_id、模式、配置、开始时间、配表指纹)。
- `AddRound(const TurnResultS2C& spectatorTurn)`:追加一回合,清掉 `action_deadline_ms` 和 `pending_actor_ids`。累计序列化字节数超过 1 MiB 后不再追加,并置 `truncated`。
- `MarkDeadlineDraw(const BattleStateS2C& spectatorState)`:强制判平时记 `final_state` 和结束原因。
- `Freeze(outcome, totalRounds, finishedAtMs) -> std::optional<BattleReplayEnvelope>`:填结局字段,序列化,gzip 压缩。压缩失败或压缩后超过 512 KiB 时返回空,并打 `metric=battle_replay_dropped reason=...`。冻结后释放已录的帧。
- 输入必须是观众版帧。模块自己不做脱敏,脱敏只在节点的 `RedactStateForViewer` 一处(BR-3)。
- 只在 loop 线程调用。任何失败都不抛异常、不影响战斗,结果只是这一局没有回放。

gzip 用 zlib 的 `deflateInit2(..., windowBits = 15 + 16, ...)`,封成一个 RAII 的小函数。不用 muduo 的 `ZlibOutputStream`:它写的是 zlib 格式(RFC 1950),级别固定为默认值(`ZlibStream.h:54`),客户端不好直接解。压缩级别先用 `Z_BEST_SPEED`,按 §9 的实测再定。

### 6.2 接入点(`cpp/nodes/battle/logic/battle_room_manager.{h,cpp}`)

| 位置 | 改动 |
|---|---|
| `BattleRoom`(`.h:334`) | 加 `std::optional<turnbattle::BattleReplayRecorder> replay`(`record_replay` 为真才创建),以及冻结后的 `std::optional<BattleReplayEnvelope> outcomeReplay` |
| `HandleCreateBattle`(`.cpp:593-611` 之后) | `record_replay` 为真时,用开战状态的观众版调 `Start` |
| `ResolveRound`(`.cpp:1206-1236`) | `result` 填完 `action_order` 之后、广播或 `BeginResultCommit` 之前,拷一份,过 `RedactStateForViewer(state, 0)`,再 `AddRound`。最后一回合也录:它要等 COMPLETE 之后才公布,而战报本来也只在 COMPLETE 之后才看得到 |
| `OnBattleDeadline`(`.cpp:1239-1263`) | 判平前,对当前状态的观众版调 `MarkDeadlineDraw` |
| `BeginResultCommit`(`.cpp:1321-1352`) | `BuildResult` 之后调 `Freeze`,结果存进 `outcomeReplay`,然后再生成原件和摘要 |
| `PumpSqlOutcomes`(`.cpp:1396-1398`) | 每次重放都会**重新拼一份请求**,再和冻结时的摘要比(`.cpp:1399`)。新字段只要有一处漏拷,摘要就永远对不上,终局永远发不出去。所以把首次和重放的两处拼请求合成一个 `BuildOutcomeRequest(room)`,两处都调它 |

`replay` 只在 `record_replay` 为真时创建,不录的局没有任何额外开销。

### 6.3 内存与 CPU

- **房间内存**:录制按序列化字节累计,上限 1 MiB;消息对象本身还有额外开销,要实测(§9)。如果实测显示节点内存吃紧,改成每回合只存序列化字节、冻结时再拼,协议不用改。
- **CPU**:序列化和压缩只在冻结时做一次,跑在 loop 线程上。按 zlib 最快档的量级,1 MiB 大约几毫秒到十几毫秒(未测)。回合制每回合至少 2 秒(`kAutoRoundIntervalMs = 2000`),这点阻塞可以接受;实测超标再改成每回合增量压缩。
- **重放摘要**:`PumpSqlOutcomes` 每次重放都对整份请求算一次摘要(`.cpp:1399`)。战报让请求变大,match 长时间不可达、大量房间同时重放时这部分 CPU 会上升,要在实测里看。超标的话,冻结时把请求序列化一次存下来,重放直接用。

## 7. match 侧(Go)

### 7.1 录制策略(新文件 `go/match/internal/logic/battle_replay_policy.go`)

照 `spectate_visibility.go` 写:`replayPolicyFor(mode, activity) bool` 是唯一决策点;`replayPolicyOfMode` 每个 `MatchMode` 一行、各写理由,表外的值不录。`gather.go:184-200` 建 `CreateBattleRequest` 时写 `RecordReplay`。全局开关 `BattleReport.Enabled`(yaml)为假时一律不录。测试遍历 `MatchMode_name`,新增模式没进表就红,同 `TestSpectateVisibilityDecidesEveryMatchMode`。

默认表要拍板(§13 第 1 条),建议:

| 模式 | 建议 | 理由 |
|---|---|---|
| `1V1`、`5V5`、`PVP_TEAM`、`PVP_CHALLENGE` | 录 | 玩家之间的对局,最需要复盘和对证 |
| 活动局(帮会历练等,`activity.kind != NONE`) | 录 | 活动有奖励和次数,出了争议要能看 |
| `PVE_SOLO`、`PVE_TEAM` | 先不录 | 刷怪局最多、最频繁,量大而价值低;以后按副本配置单独打开(§12) |
| `3V3`(未开放)、`UNSPECIFIED` | 不录 | 未开放 / 漏填 |

### 7.2 写入(`go/match/internal/authority/completion.go`)

在 `CommitBattleOutcome`(`:381`)里:

1. **事务前,纯校验**(和 `:385-394` 同一层):`replay` 为空就跳过。不为空时依次检查 `format_version == 1`、`codec == GZIP`、`raw_bytes ≤ 1 MiB`、`len(payload) ≤ 512 KiB`。不合格就丢掉战报,记 `reason=invalid_envelope` 或 `too_large`,**不返回错误**(BR-7)。
2. **事务内**:`terminalFormation` 拿到成局行之后,解析 `f.CreateRequest`;`record_replay` 为假而请求带了战报,也丢掉,记 `reason=not_requested`。`INSERT match_battle_completion` 成功之后、更新成局行之前,`INSERT match_battle_report`,`created_ms` 用同一个 `stamp`(`:423`)。插入失败按普通 SQL 错误处理:整笔回滚,返回「未确认」,battle 按原字节重放。这和事务里其他行一样,不另设分支。
3. **重放**(`replayTerminal`,`:362`):决定是 COMPLETE 时,读 `match_battle_report.envelope_digest` 和请求比。对不上,或者请求带了而库里没有,只记 `match_battle_report_replay_mismatch`,照常返回已持久的决定,不改库里那份(BR-7)。
4. `abortTerminalTx`(`:464`)不动:ABORT 没有战报。

**为什么插入失败不也 fail-open**:SQL 错误和其他行一样是瞬时的;一定会失败的情形(超大、格式不对)已经在事务前挡掉了。在事务里吞掉错误,要么得开保存点,要么会让整笔事务处在未知状态,反而更复杂。

**事务变大**:常见情况下战报只有几 KB 到几十 KB,最大 512 KiB。事务持有成局行和票据行的锁,提交变慢就会多持一会儿锁。真库用例里要对比加战报前后的终局提交耗时(§17)。

### 7.3 查询(新文件 `go/match/internal/logic/getbattlereportlogic.go`)

照 `requestbattleticketlogic.go:39-48`:

1. 会话身份取自 `ctxkeys.GetSessionDetails`,没有就 fail-closed。
2. `SELECT 1 FROM match_battle_settlement_source WHERE battle_id=? AND player_id=?`(主键点查)。没有就回 `kMatchBattleReportNotFound`。
3. `SELECT envelope FROM match_battle_report WHERE battle_id=?`。没有也回 `kMatchBattleReportNotFound`;有就反序列化成 `BattleReplayEnvelope`,原样返回。
4. 存储错误回 `kMatchInternal`,不重试。

两次都是主键点查,不开事务。先鉴权再读正文,非参战者读不到大行。`server/matchserviceserver.go` 加对应方法。

**限频**:用 gate 现有的按消息限频(`CheckMessageLimit`,`client_session_handler.cpp:415`),给这条消息配一个低额度,建议每秒 1 次,配置位置落码时核对。单次回包不超过 512 KiB,即使连发几次,也到不了 gate 2 MiB 的高水位(`client_session_handler.cpp:695`)。

### 7.4 保留期清理

照 `PurgeClosedSpectatorIntents`(`spectator.go:755`)和它的循环(`global_spectator.go:662-696`):

- `Store.PurgeExpiredBattleReports(ctx, after, limit)`:沿 `(created_ms, battle_id)` 索引读一页 `created_ms < now - retention` 的行,按主键删,返回下一页游标和删除行数。幂等,多副本同时跑无害。
- 循环 `match.battle_report.purge` 每分钟一页,每页 500 行(行比观战意图大,所以页小一点)。一天最多清 72 万行;每天产生的战报多于这个数时,调大页或缩短间隔,按实测定。
- 保留天数 `BattleReport.RetentionDays`,默认值待拍板(§13 第 2 条,建议 7 天)。

### 7.5 依赖与跨库

- 战报表和终局、个人结算原件都在 `mmorpg_match` 这一个逻辑库里,同一事务不跨库,`CrossDatabase` 契约测试不受影响(AGENTS §7 第 6 条)。以后如果要把战报拆到单独的逻辑库,就不能再同事务提交,要改成发件箱,另立方案。
- 鉴权要求 `match_battle_settlement_source` 的行至少和战报活得一样久。这张表现在「保留到显式迁移 / 恢复流程接管」(`match_authority.proto` 注释)。以后谁要清理它,要先清战报,或者把参战者名单搬进战报行。落码时把这条写进那张表的注释。

## 8. 客户端契约

1. **入口**:近期战斗结果列表(`battle-result-recovery.md` §7,`PlayerBattleResult`)每条带 `battle_id`、`match_mode`、`battle_config_id`。「看回放」直接用 `battle_id` 调 `GetBattleReport`。列表条目不标记有没有回放,点了查不到就显示 tip 文案。
2. **解码**:`codec == GZIP` 就 gzip 解压;解压出来的字节数超过 `raw_bytes` 或超过 1 MiB 就判坏。`format_version` 不认识就提示升级。然后按 `BattleReplay` 解析。
3. **播放**:`start_state` 当开战帧,`rounds` 逐个当回合帧,走观战的播放流程(这些帧本来就是观众版)。可以从任一回合开始播,因为每帧都带结算后的全量状态。
4. **结局**:看 `outcome` 和 `end_reason`。`DEADLINE_DRAW` 时最后显示 `final_state` 和「超时平局」;`truncated` 时在最后一帧之后提示「回放只录到第 N 回合」,结局照 `outcome` 显示。
5. **版本**:`table_fingerprint` 和客户端当前配表对不上时,可以提示「游戏已更新,技能显示可能与当时不同」。
6. **兜底**:如果某个平台没有可用的 gzip,改成 match 解压后下发 `codec = NONE`,回包变大但仍在上限内。这只是 match 一处改动,协议不变。

本文没有看客户端仓(未获客户端任务授权)。第 3 条能不能直接复用观战播放,落码前要在客户端仓核对。

## 9. 容量与上限

**各段上限**

| 段 | 上限 | 位置 |
|---|---|---|
| battle → match gRPC 请求 | 4 MiB(muduo_grpc 发送上限,对齐 grpc-go 默认接收上限) | `muduo_grpc/channel.h:101` |
| 终局原件 `raw_result_payload` | 960 KiB | `completion.go:25` |
| 战报外壳 payload(本文) | 压缩后 512 KiB;原始 1 MiB | BR-5 |
| match → gate 回包 | 4 MiB(gate 侧 muduo_grpc 接收上限) | `muduo_grpc/client_session.h:35` |
| gate → 客户端 | 单连接输出缓冲 2 MiB 高水位,超过就强关大厅连接 | `client_session_handler.cpp:695` |
| 存储列 | MEDIUMBLOB 16 MiB;TiDB 单条 KV 默认 6 MiB | proto2mysql 映射 |

终局请求最大约 960 KiB + 512 KiB,在 4 MiB 以内。单次查询回包最大 512 KiB,四个同时在途也到不了 2 MiB。

**体积估算(未实测,只是量级)**

- 一个单位的观众版状态约 240 字节(名字、外观、8 项属性、1~2 个 buff、技能和技能等级);一个事件约 45 字节;每回合每个单位约 2~4 个事件。
- 30 个单位(两边各 5 人 + 5 宝宝,再加 10 只怪)每回合约 11 KB;5v5 不带宝宝约 4 KB;1v1 带宝宝约 1.6 KB。
- 回合数受整场期限 300 秒约束(`configurable-templates.md` §8.6.3)。全自动局每回合间隔 = max(2 秒, 出手单位数 × 250 毫秒)(`turn_battle_constants.h:39-53`):30 个单位每回合 7.5 秒,最多约 40 回合;10 个单位最多约 120 回合;1v1 最多约 150 回合。手动局行动窗口 25 秒(`kActionTimeoutMs`),双方都秒出手时回合可以更快。
- 所以全自动局的原始体积大约是:30 个单位约 440 KB,5v5 约 480 KB,1v1 约 240 KB,都在 1 MiB 以内。只有手动局双方一直秒出手、拖到期限,才可能碰到上限被截断。帧之间高度重复,gzip 压缩比应该很高,具体要实测。

**存储**:每天录制的局数 × 平均压缩后大小 × 保留天数。前两个数要等录制策略定了、实测出来再算,算完写回本节。

**实测要求(落码时做,交 Codex)**:在 `turn_battle_engine_test` 加一个体积用例,用内存数据源跑 1v1 带宝宝、5v5、10v10 带宝宝三种全自动局,打满到期限对应的回合数,打印原始字节、压缩后字节和压缩耗时。数字写回本节,再确认 BR-5 的两个上限和压缩级别。

## 10. 失败与一致性

| 情形 | 结果 |
|---|---|
| battle 在提交终局之前崩溃 | 房间和帧一起丢;match 到期判 ABORT,没有战报。和现在一致 |
| 终局提交结果未知 | battle 按冻结的整份请求(含战报)重放,摘要不变;match 按原字节幂等 |
| 重放时战报摘要对不上 | 只记错误指标,返回已持久的决定,库里那份不改(BR-7)。正常情况下不会发生:battle 冻结之后逐字节重放 |
| 战报超上限、格式不对、版本不认识、本局不该录 | match 丢掉战报、记指标,终局照常提交 |
| 录制超过 1 MiB | 停录后面的回合,标 `truncated`;终局不受影响 |
| 压缩失败,或压缩后超过 512 KiB | battle 不发战报,记指标;终局不受影响 |
| 写战报的 SQL 失败 | 整笔回滚,返回「未确认」,battle 重放(和其他行相同) |
| 清理和查询同时发生 | 读到就返回,读不到就是过期;都是单行操作,没有中间态 |
| match 不可用 | 战报查不到;终局本来也在等 match。近期战斗结果在 scene,不受影响 |
| 终局 ABORT | 没有战报 |

## 11. 兼容与上线

项目尚未上线(AGENTS「上线状态」),不用迁移存量数据。新旧版本混跑时也安全,上线后同样成立:

- **新 battle + 老 match**:老 match 不填 `record_replay`,新 battle 就不录,不会发带新字段的终局请求。注意 match 会把带未知字段的终局请求当成永久冲突拒掉(`completionHasUnknown`,`completion.go:170`),所以**只有 match 让录,battle 才发战报**这条必须守住,由 battle 节点的用例钉住(§17)。
- **新 match + 老 battle**:老 battle 忽略 `record_replay`,不发战报;match 照常写终局。
- **回滚**:先把 `BattleReport.Enabled` 关掉,等在途的局打完(一场的最长期限),再回滚 battle 和 match。已存的战报照样能查,表保留。
- 上线后字段号不复用(AGENTS §4)。

## 12. 以后再做

- **P2 分享与公开局**:分享时由 match 签一个短期凭证,非参战者凭证查看;公开局 / 赛事局(`SPECTATE_VISIBILITY_PUBLIC`、featured)可以让所有人看。鉴权改成「参战者,或持有效凭证,或公开局」。
- **P2 按副本录 PVE**:副本配置表加一列,BOSS 局这类单独打开。
- **P3 重算校验**:另记每回合被接受的指令、挂机开关、引擎版本和节点强制终局,GM 可以重跑核对(`configurable-templates.md` §8.6.6)。
- **按需:冷归档**:保留期到了先转对象存储再删,按实测容量和需求定。

## 13. 待负责人拍板

1. **哪些模式录**:建议 PVP 全录、活动局录、PVE 先不录(§7.1 的表)。
2. **保留几天**:建议 7 天。
3. **录制上限与截断**:建议原始 1 MiB、压缩后 512 KiB,超过就截断或丢弃(BR-5)。实测后可能调整。
4. **客户端解压还是 match 解压**:建议客户端解压,省流量;某个平台不支持时改 match 解压(§8 第 6 条)。
5. **非参战者能不能看**:建议 v1 只给参战者,分享和公开局放 P2。

## 14. 落码清单

顺序:proto 与 tip 码 → 生成 → match → battle → 测试。match 能存战报之后,才打开策略表里的录制(§11)。

**proto 与表(客户端契约,两版同批改)**

- `proto/battle/player_battle.proto`:§5.1 的消息。
- `proto/battle/battle_node.proto`:`CreateBattleRequest.record_replay`。
- `proto/match/match_internal.proto`:`CommitBattleOutcomeRequest.replay`,加 import。
- `proto/match/match_service.proto`:`GetBattleReport` 和两个消息。
- `proto/match/match_authority.proto`:`MatchBattleReportRecord`。
- `data/tip/Tip.xlsx`:`kMatchBattleReportNotFound`;match 的 `constants` 里引用生成的枚举。

**C++**

- 新增 `cpp/libs/services/battle/system/battle_replay_recorder.{h,cpp}`,登记进 `cpp/libs/services/battle/battle.vcxproj`、`.filters` 和 Linux 构建。
- `cpp/nodes/battle/logic/battle_room_manager.{h,cpp}`:§6.2 的接入点和 `BuildOutcomeRequest`。
- 测试:`cpp/tests/turn_battle_engine_test/battle_replay_recorder_test.cpp`,登记进该测试工程;节点层用例放 `cpp/nodes/battle/tests/`,沿用 `rpc_controller_test` 工程现有的接线(参照 `battle_observer_contract_test.cpp` 的 `BattleRoomManagerTestAccess`)。

**Go**

- `go/match/internal/logic/battle_replay_policy.go`(含测试);`gather.go` 写字段。
- `go/match/internal/authority/completion.go`:§7.2;读、写、清理放新文件 `battle_report.go`。
- `go/match/internal/authority/schema.go`:`Tables()`、`CheckCandidateIndexes`。
- `go/match/internal/logic/getbattlereportlogic.go`、`server/matchserviceserver.go`。
- 清理循环挂在现有维护循环旁边;`internal/config` 加 `BattleReport.Enabled`、`RetentionDays`;`metrics` 加 §15 的计数。

**Java**:见 §16。

**不动的部分**:`result_commit.h`、`BattleSettlementData`、`PlayerBattleResult`、scene 结算应用点都归战斗结果重登恢复,本批不碰。

## 15. 日志与指标

player_id 只进日志字段,不进任何指标 label。

| 指标 / 日志 | 位置 | 含义 |
|---|---|---|
| `metric=battle_replay_frozen`(INFO,带回合数、原始字节、压缩后字节、耗时) | battle | 一局战报冻结 |
| `metric=battle_replay_truncated`(WARN) | battle | 超过 1 MiB,截断 |
| `metric=battle_replay_dropped reason=compress_failed / too_large`(ERROR / WARN) | battle | 不发战报 |
| `match_battle_report_stored_total` | match | 写入一行 |
| `match_battle_report_dropped_total{reason}` | match | 丢弃:`invalid_envelope` / `too_large` / `not_requested` |
| `match_battle_report_replay_mismatch_total` | match | 重放摘要不一致。应恒为 0,非 0 告警 |
| `match_battle_report_get_total{result}` | match | 查询:`ok` / `not_found` / `error` |
| `match_battle_report_purged_total` | match | 清理行数 |

## 16. Java 版

服务端内部按 Java 的写法做;客户端契约(§5.1、§5.4、tip 码)与本仓逐字段相同(AGENTS §12)。

- **battle**:`game-battle/src/main/java/com/game/battle/room/BattleRoom.java`、`BattleRoomServiceImpl.java` 在同样的四个点录制;观众视角复用 `ViewerState.java` 的脱敏;压缩用 `java.util.zip.GZIPOutputStream`;终局请求在 `BattleConfiguration.java:321` 一带拼装,首次和重放共用一处。
- **match**:`game-match/.../global/GlobalMatchStore.java` 的终局事务里写 Java 自己的战报表;查询加一个 handler,在 `dispatch/MatchMethods.java` 登记;清理照 Java 现有的保留期清理写。
- **proto**:`game-proto/src/main/proto/proto/` 下的同名文件同批改。
- **状态**:待做。本仓落码时,在 Java 仓 `PARITY.md` 登记「Java 待做」并写明验证命令。

## 17. 验证(交 Codex,AGENTS §10.1)

任一步失败即停,保留完整输出,不改断言、不跳过。下面每一条 Claude 都没有运行过。

1. 改 `Tip.xlsx` 后跑导表器;然后 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。生成器源码没变就不用跑 `proto-gen-build`。
2. C++(串行,`/m:1`):先编 proto、table、battle 库(`cpp/libs/services/battle/battle.vcxproj`),再编 battle 节点。测试工程链接的是预编好的库,改了库要先编库再跑测试。然后 `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter turn_battle`,再编并运行 `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`。
3. Go:在 `go/match` 下跑 `go build ./...`、`go vet ./...`、`go test ./... -count=1`;真库用例按现有 TiDB 用例的方式跑 `go test ./internal/authority/... -run 'BattleReport|CommitBattleOutcome' -count=1 -v`,并记录加战报前后的终局提交耗时。
4. Java:`./mvnw -B -pl game-battle,game-match -am test`。
5. 冒烟:起本地栈,robot 打一局 1v1,结束后调 `GetBattleReport`:没有截断时,解压后 `rounds` 的数量等于 `total_rounds`,每一帧与当时观众收到的帧逐字段相同。

**必须覆盖的用例**

| 层 | 用例 |
|---|---|
| 录制模块 | 开战 + 若干回合 + 冻结,解压解析后与输入逐字段相同;实时字段已清;超过 1 MiB 截断并标记;强制判平记 `final_state`;压缩后超过 512 KiB 返回空;没调 `Start` 就冻结返回空 |
| battle 节点 | `record_replay` 为假时不创建录制、请求里没有 `replay`;为真时首次提交和每次重放的请求摘要相同(钉住 `BuildOutcomeRequest`);录进去的帧等于观众版帧;ABORT 和本地删房两条路径不发战报 |
| match 写入 | 带战报 → 同事务写入;同字节重放 → 返回 COMPLETE、不重复写;重放摘要不同 → 返回原决定、记指标、库里不变;外壳不合格 / 超大 / `record_replay` 为假 → 终局照常、没有战报行;ABORT → 没有战报行 |
| match 查询 | 参战者取得到;非参战者、过期、没录、不存在都回同一个码;没有会话身份 fail-closed;存储错误回 `kMatchInternal` |
| 清理 | 只删过期行;翻页游标正确;多副本同时跑无害;缺索引时拒绝接流 |
| 策略 | 每个 `MatchMode` 都在表里;活动局按表;全局开关关闭时一律不录 |
| 体积 | §9 的实测用例,数字写回本文 |
