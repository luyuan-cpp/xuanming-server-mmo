# 战斗结果重登恢复:近期战斗结果

> **状态**:v1 — 2026-11-05 方案定稿,服务端已落码,**未编译、未测试**,验证按 §10 交 Codex。客户端 §7 于 2026-11-12 落码(`../mmorpg-client`,未生成、未编译),落码时定下的取舍写在 §7 末尾。Java 版待做,已在 Java 仓 `PARITY.md` 登记(§11)。
> **来源**:[断线重连审计 §9.3](disconnect-reconnect-audit-20261020.md#93-战斗结果展示缺少重登恢复)、[缺口清单「待确认」](../handoff/server-gap-audit-20261101.md)。
> **关系**:结算链路以 [总体方案 §5.4](player-data-tidb-scaling-plan.md#54-战斗结算与本地模式2026-10-30-定) 为准(结算只走 match SQL + player_inbox,scene 的 `ApplyInboxSettlement` 是唯一应用点);组件创建与缺失语义以 [ECS 组件访问规则](ecs-component-access-rules.md) 为准;存档分组见 [存盘直接提交 §1.1](player-save-direct-commit.md#11-字段分组);客户端的落地判定沿用 [落地恢复 §4.3](landing-recovery-sync-digest.md#43-客户端实现mmorpg-client)。

## 0. 结论

1. **数据来源选方案 (a)**:结算应用的同一帧,把这一局**本人实际入账**的结果写进玩家存档里的有界列表 `player_database.battle_results`(字段 23,PROFILE 组)。最多 10 局,按 battle_id 去重,随同一次存档、同一个整笔序号提交。奖励和这条记录要么一起在盘上,要么一起没有。
2. **公开时机与 `BattleEndS2C` 相同**:存档经同 E/q 双 SQL 确认之后,`CompleteInboxSettlement` 才让这条记录对客户端可见。没确认提交的结果不给客户端看。
3. **新客户端协议** `proto/scene/player_battle_result.proto`,服务 `SceneBattleResultClientPlayer`:`GetRecentBattleResults` 查询,`AckBattleResults` 确认已展示。请求不带 player_id,只认会话里的本人。
4. **客户端主动查,服务端不在落地时推**:每次落地查一次;收到没有战斗上下文的 `BattleEndS2C` 时不再丢弃,改为再查一次。展示 `seen = false` 的结果,展示后确认。
5. **结束时刻**:battle 冻结终局原件时,给每一份个人结算盖上 `BattleSettlementData.finished_at_ms`(新字段 17)。
6. **不新增 tip 码**:只用 common 段现成的 1003、1006、1015。

## 1. 问题(已核实)

- **情形 A**:断线期间 scene 已经应用结算。`CompleteInboxSettlement` 在 SQL 确认之后经大厅推一次 `BattleEndS2C`,客户端此时不在线,这份通知丢了。收件箱条目随后确认,不会再投递,也就不会再推。房间已删,140 / 179 只认在场房间,查不到历史终局。
- **情形 B**:断线期间战斗结束,重登之后收件箱才补应用结算。scene 照常推 `BattleEndS2C`,但客户端断线时已经清掉 `_battleId` 和 `_endPendingBattleId`,没有战斗上下文的终局通知被丢弃。
- 两种情形下奖励都已正确入账,只是玩家看不到胜负与奖励明细。代码里没有按玩家查近期终局的接口。

## 2. 约束

- 负责人 11-01 定的标准:不丢数据、强一致、可水平扩展、代码易懂,性能靠实测。
- 结算的唯一应用点是 `PlayerBattleSystem::ApplyInboxSettlement`(经 `ApplySettlementToEntity`),效果与收件箱账本同一帧写、随同一次存档提交;`CompleteInboxSettlement` 是 SQL 确认之后唯一的公开点(解冻、放锁、推 `BattleEndS2C`)。不能靠反复应用结算来补弹窗。
- 持久组件只在加载时创建,读路径只读,缺了 fail-closed,不补默认值(ECS 组件访问规则)。
- 列表顺序必须显式,不能靠下标(AGENTS §11.6)。
- 项目未上线,玩家数据可清空,不做存量兼容迁移。

## 3. 方案比较

| | (a) 存档里的有界列表(采用) | (b) match 终局事务写历史表 | (c) 复用 `match_battle_settlement_source` 加索引 | (d) 落地时把最近一局的 `BattleEndS2C` 再推一次 |
|---|---|---|---|---|
| 记的是什么 | 本人实际入账:消耗按实际持有夹紧,被获得封禁的掉落与币种不记 | 终局原件 | 终局原件 | 终局原件 |
| 与奖励的一致性 | 同一帧写、同一个事务提交;崩溃回到上次存盘时一起回退,收件箱重投后一起重做 | COMPLETE 时就有,早于入账;收件箱 DEFER 或按版本错位停用时,面板上的奖励背包里没有 | 同 (b);要等 `dispatch_state = 2` 才能当成已入账 | 一致,但推送至多一次 |
| 扩展 | 跟着玩家存档按玩家分片;查询在 scene 内存里完成,不给任何服务加读 QPS | 全局表每局每人一行;每次落地多一次跨服务查询 | 同 (b) | 不加存储 |
| 新增部件 | 一个持久组件、一个只在本文件内的按需组件 | 新表、保留期清理任务、新 match RPC、容量规划 | 新索引、新 match RPC;这张表是恢复原件,「保留到显式迁移 / 恢复流程接管」 | 无 |
| 可用性 | 只依赖玩家所在 scene | match 不可用时恢复不了 | 同 (b) | 客户端没上下文照样丢 |
| 代价 | PROFILE 行常见约 1 KB、封顶约 8 KB;要守 ECS 规则与 §11.6 | 记录与入账可能不一致;多一张全局表 | 把内部恢复原件变成客户端契约 | 服务端不知道客户端看没看过,只能每次落地都推或靠时间猜;列不出近几局 |

**选 (a)**:

- **强一致**:效果、收件箱账本、结果记录在 `ApplySettlementToEntity` 的同一帧写入,一次存盘的所有脏行在同一个事务里、带同一个整笔序号提交(总体方案 §4.3),不存在「奖励到了记录没到」或反过来的中间态。(b) 和 (c) 的记录早于入账产生,两个系统之间要另造对账。
- **不丢**:记录落在玩家存档里,跨 zone、合服、回档、迁库都跟着存档走,不需要额外处理。
- **可水平扩展**:按玩家分片,没有新的全局热点和保留期任务;查询不出 scene 进程。
- **代码易懂**:写入点只有一处,就在奖励应用旁边;公开点只有一处,就在 `BattleEndS2C` 旁边。

**(b) 的唯一优势是能查任意长的历史**(战绩、战报)。本需求只要「重登后看到最近几局」,不需要。长历史另立 (b) 类方案,与本列表互不影响:PVP 战报已按这种方式另立(2026-11-06,[战报回放](battle-report-replay.md)),match 库按玩家存最近 100 局 PVP 战报,和这里的近期结果分开存、分开取。本列表里的 PVP 局可以拿 `battle_id` 去取回放(还在本人那 100 条里才取得到),PVE 局没有回放。

## 4. 数据模型

### 4.1 持久记录

`proto/common/component/player_battle_result_comp.proto`(新文件):

```protobuf
message PlayerBattleResultCurrency {
  uint32 currency_type = 1;  // CurrencyType:0 金币、1 钻石、2 绑定钻石
  uint64 amount = 2;         // 本局入账额;先抵欠款的部分也算在内
}

message PlayerBattleResultItem {
  uint32 item_table_id = 1;
  uint64 count = 2;
}

message PlayerBattleResult {
  uint64 result_seq = 1;      // 本玩家的结果序号,从 1 起只增;新旧看它
  uint64 battle_id = 2;       // 去重键:同一局至多一条
  uint32 outcome = 3;         // eBattleOutcome:A 胜 / B 胜 / 平局,不会是 ONGOING
  uint32 team_index = 4;      // 本人所在方(0 = A 方);胜负 = outcome 对照它
  bool is_dead = 5;
  bool fled = 6;
  uint32 total_rounds = 7;
  uint64 finished_at_ms = 8;  // 战斗结束时刻(battle 冻结终局时的 UTC 毫秒);0 = 结算原件没带
  uint64 settled_at_ms = 9;   // 奖励入账时刻(scene 应用结算时的 UTC 毫秒)
  repeated PlayerBattleResultCurrency currencies = 10;  // 按 currency_type 升序,只含非 0
  repeated PlayerBattleResultItem items_gained = 11;    // 本人实得的掉落(入了人物背包或临时背包;items_mailed 时改由邮件发放),按 item_table_id 升序
  repeated PlayerBattleResultItem items_consumed = 12;  // 实际扣除的战斗消耗,按 item_table_id 升序
  bool lines_truncated = 13;  // 物品行超过上限被截掉了一部分
  bool seen = 14;             // 客户端已确认展示过
  uint32 match_mode = 15;     // MatchMode 数值;0 = 结算原件没带
  uint32 battle_config_id = 16; // 战斗配置号;0 = 结算原件没带
  bool items_mailed = 17;     // 掉落两个包都放不下,整批改由邮件发放(2026-11-12)
}

message PlayerBattleResultComp {
  uint64 last_result_seq = 1;
  repeated PlayerBattleResult entries = 2;
}
```

`player_database` 加 `PlayerBattleResultComp battle_results = 23 [(OptionPersistGroup) = 4];`。

- **只记实际入账**:货币取整笔预检通过、实际调用 `AddCurrency` 的金额;掉落取剔除了被获得封禁项之后的清单(被封的项另记 `metric=battle_settlement_item_blocked`,不补发);消耗取 `min(要求, 实际持有)`。掉落或消耗里有本节点 Item 表查不到的配置号时整笔延后、这一局什么都不记(2026-11-11 起,`mixed-version-persist-field-protection.md` §13 第 10 条;此前是剔除这一项后照记)。经验系统还没接入(`exp_gain` 只记日志),所以不记经验,接入时在同一写入点加字段。
- **改投邮件的掉落照常记**(2026-11-12,[战斗掉落改投邮件](battle-overflow-mail.md)):人物背包与临时格都放不下时,掉落整批进玩家存档的待寄队列、
  之后寄成个人邮件。`items_gained` 照常列出这一批(玩家确实得到了,只是去处是邮箱),`items_mailed = true` 说明去处;
  `BattleEndS2C.items_mailed` 取自同一条记录,客户端在结算界面道具行之后加一句提示。两个字段同一帧写、随同一次存档提交。
- **枚举存 uint32**:common 不反向依赖 battle 包,与收件箱账本同口径。
- **顺序显式**:新旧只看 `result_seq`,存放顺序只为确定性序列化。
- **为什么放 PROFILE 组**:列表只在结算和客户端确认时变。放 ASSET 会让每一次资产写入(交易、商店、捐献)都带上它。原子性不受分组影响:一次存盘的所有脏行同事务提交(存盘直接提交 §1.1,「账本不变量 I3 由一次存盘的所有脏行同事务保证」)。
- **上限**:每人至多 `kMaxEntries = 10` 局;每张物品清单至多 32 行,超出只留配置号最小的那些并标 `lines_truncated`;货币至多 3 种。单条封顶约 0.8 KB,整个列表封顶约 8 KB,常见约 1 KB。`kMaxEntries` 同时是查询应答与确认请求的条数上限,必须不超过场景入口的 repeated 字段上限 20(`kProtoFieldCheckerThreshold`),否则合法的确认请求会在派发前被静默丢弃;代码里有 `static_assert`。

### 4.2 列表规则

纯函数,在 `cpp/libs/services/scene/battle/system/battle_result_history.h`,不碰 ECS、时钟和日志:

- `Record`:`battle_id` 为 0 不记;已有同一 `battle_id` 不记(保留第一条);否则 `result_seq = max(last_result_seq, 现存最大 result_seq) + 1`,`seen = false`,追加;超过 10 条按 `result_seq` 从小到大淘汰。加载来的列表即使 `last_result_seq` 落后也不会撞号。
- `VisibleNewestFirst`:按 `result_seq` 降序,跳过还没公开的局。
- `MarkSeen`:把请求里的、已公开的局标成已读;不认识的局忽略;幂等。
- `FillItemLines`:按配置号升序写清单,跳过数量为 0 的项,超过上限截断。

### 4.3 组件类别

| 组件 | 类别 | 创建 | 缺失时 |
|---|---|---|---|
| `PlayerBattleResultComp` | 持久数据 | 只在 `PlayerDatabaseMessageFieldsUnmarshal` 创建 | 存盘整份拒存;结算在任何副作用之前 DEFER;查询回 1003;不补默认值 |
| `BattleResultAwaitingCommitComp`(`player_battle.cpp` 内部) | 按需状态 | `RecordBattleResult` 写进结果的同一帧延迟创建 | 没有 = 没有待公开的结果 |

`BattleResultAwaitingCommitComp` 记「结果已写进列表、存档还没确认提交」的 battle_id。`CompleteInboxSettlement` 摘掉对应 battle_id,空了就摘组件;结果被淘汰出列表时一并摘。查询和确认只读它。它不落库:实体重建后从盘上加载回来的结果都已提交。

## 5. 写入与公开

1. 收件箱把结算交给 `ApplyInboxSettlement` → `ApplySettlementToEntity`。在任何副作用之前,与其他持久组件一起确认 `PlayerBattleResultComp` 在场(缺了整笔 DEFER)。最后一道复核(货币入账之前)也包含它。
2. 全部效果写完之后,同一帧调用 `RecordBattleResult`:组记录、`Record`、把 battle_id 挂进 `BattleResultAwaitingCommitComp`。这之后没有失败分支。
3. 收件箱同一帧记账、请求存盘。效果、账本、结果作为脏行在同一个事务里提交。
4. 同 E/q 双 SQL 确认之后,`CompleteInboxSettlement` 先公开结果(摘 awaiting),再按原规则解冻、条件删锁、推 `BattleEndS2C`。公开放在同局判断之外:玩家此刻若已在下一局里,终局通知照旧不推,结果照样公开。

| 情形 | 行为 |
|---|---|
| 提交之前崩溃 | 效果、账本、结果一起回到上次存盘;收件箱重投,重新应用、重新写结果 |
| 提交之后、公开之前崩溃 | 重登加载出的结果已经可见(盘上的都已提交);收件箱回放原结局,不调执行器、不再写结果;`CompleteInboxSettlement` 照常推 `BattleEndS2C` |
| 同一局换新 seq 重投(C-1) | 按最近结局环认出同局,不调执行器,不写第二条 |
| 列表里已有同一 battle_id(不应发生) | 不写第二条,记 `metric=battle_result_duplicate` |
| reaper 已作废冻结之后结算才到 | 照常应用,照常写结果 |
| 冻结 / 交接 / 退出中 | 结算本就 DEFER(`IsSettlementApplicable`),不写结果 |

## 6. 客户端协议

`proto/scene/player_battle_result.proto`(新文件,`OptionFileDefaultNode = NODE_SCENE`,`OptionIsPlayerService`,`OptionIsClientProtocolService`):

```protobuf
message GetRecentBattleResultsRequest {
}

message GetRecentBattleResultsResponse {
  TipInfoMessage error_message = 1;
  repeated PlayerBattleResult results = 2;  // 已提交的结果,result_seq 降序,至多 10 条,battle_id 不重复
}

message AckBattleResultsRequest {
  repeated uint64 battle_ids = 1;  // 至多 10 个
}

message AckBattleResultsResponse {
  TipInfoMessage error_message = 1;
}

service SceneBattleResultClientPlayer {
  rpc GetRecentBattleResults (GetRecentBattleResultsRequest) returns (GetRecentBattleResultsResponse);
  rpc AckBattleResults (AckBattleResultsRequest) returns (AckBattleResultsResponse);
}
```

- **只认本人**:请求里没有 player_id,scene 按会话解析出的实体回答。
- **查询**:只读,不改变实体的组件集合;冻结、交接中也能查。
- **确认**:把请求里已公开的局标成已读;不认识的局(已淘汰、从未有过、还没提交)忽略;重复确认无副作用。不单独触发存盘,已读标记随下一次存盘落地;丢了最多让客户端再弹一次。
- **消息号**由生成器发号,手写代码只引用生成常量。限流用 `MessageLimiter` 默认档(同一消息号每秒约 3 次),客户端单飞即可远低于它。

| tip | 何时 |
|---|---|
| 1003 `kServiceUnavailable` | `PlayerBattleResultComp` 缺失(加载不变量被破坏,同时记 ERROR) |
| 1006 `kFeatureUnavailable` | 确认时实体冻结、交接中或没有归属围栏;退出中的实体由派发入口统一回 1006 |
| 1015 `kArraySizeTooLargeInMessage` | 确认请求超过 10 个 battle_id |
| 1004 `kEntityIsNull` | 实体无效(派发入口已保证有效,只是防御) |

**结束时刻**:`BattleSettlementData` 加 `uint64 finished_at_ms = 17`。battle 在 `FreezeBattleSettlementSource`(`cpp/libs/services/battle/system/result_commit.h`)里把 `BattleResultEvent.finished_at_ms` 盖进每一份个人结算,与 battle_id、outcome 的统一盖章同一处。match 原样归档、原样投收件箱,不需要改。它不参与任何结算判定。`BattleEndS2C.settlement` 随之也带这个字段。2026-11-05 同一处再盖 `match_mode = 18`、`battle_config_id = 19`(取自 `BattleResultEvent` 的同名字段),scene 照抄进 `PlayerBattleResult` 的 15、16:列表据此展示模式,点「看回放」仍只用 battle_id(回放正文与查询见 [battle-report-replay.md](battle-report-replay.md))。

**发布顺序**:scene 必须先于 battle 升级。旧 scene 的执行器把结算原件里不认识的嵌套字段按版本错位处理,会停用战斗结算域。项目未上线,整批起步;上线后这类加字段都按这个顺序发。

## 7. 客户端怎么用(`../mmorpg-client`,2026-11-12 已落码,未编译)

1. **生成**:客户端的 proto 生成白名单加 `proto/scene/player_battle_result.proto` 与 `proto/common/component/player_battle_result_comp.proto`,重新生成 C#。`BattleSettlementData` 多了 `finished_at_ms`。
2. **落地就查一次**:落地判定沿用落地恢复 §4.3 第 1 条(`(PlayerId, GateConnectionIdentity)` 变化且 `InGame && IsGateReady`),在收到 `NotifyEnterScene` 之后发 `GetRecentBattleResults`。单飞;超时或失败按 15 / 30 / 60 秒封顶退避。实体还在异步载入时 scene 丢弃请求不回包,所以必须等进场之后再发。
3. **没有战斗上下文的 `BattleEndS2C` 不再丢弃**:改为触发一次 `GetRecentBattleResults`(与第 2 条共用单飞,短时间内多次触发只查一次)。有上下文的照旧走现有终局流程。
4. **去重与展示**:进程内维护「本次已展示的 battle_id」集合。查询结果里 `seen = false`、且不在这个集合里的,按 `result_seq` 从小到大逐个弹结果面板(或只弹最新一局,其余进列表,由产品定)。胜负 = `outcome` 对照 `team_index`(`SIDE_A_WIN` 且 `team_index = 0` 为胜,`DRAW` 为平,其余为负),`fled` / `is_dead` 照现有面板显示。时间用 `finished_at_ms`,为 0 时用 `settled_at_ms`。货币与物品用客户端配表显示名称和图标,`lines_truncated` 为真时提示「等」。
5. **确认**:面板展示之后(含有上下文的现有终局面板),发 `AckBattleResults`,带本次展示过的 battle_id。失败(1006、超时)不重弹面板,保留在待确认列表里,下次落地先补发。
6. **测试**:EditMode 用假 transport 覆盖:落地查到一条未读只弹一次;同一局先收到有上下文的 `BattleEndS2C` 再查到不重复弹;无上下文的 `BattleEndS2C` 触发查询;确认失败下次落地补发;查询失败退避。

**2026-11-12 落码(Claude,未生成、未编译)**。11-05 定稿时这一节交给 Codex,一直没做:玩家断线或离线期间结算入账的那一局,重登后看不到结果。按上面六条落码,定下的取舍:

- **在途期间的触发**:「短时间内多次触发只查一次」的做法是在途时只记一笔,回包之后再查一次。只合并不补查的话,结果在第一次查询发出之后才公开,这一局就漏了。
- **弹法**:逐个弹,确认一局再弹下一局。客户端没有结果列表界面,「只弹最新一局、其余进列表」不采用。只在空闲时弹(没有战斗屏、观战屏,也没有结算面板在显示);相位回到空闲、观战收屏或玩家确认面板之后再弹下一局。确认键写「知道了」,不收战斗屏,不调 `AckBattleEnd`。
- **确认时机**:面板真正弹出之后才确认(补弹的与现场的都是);交给界面而没来得及弹(断线)的不确认,下次落地再交。确认一次至多 10 个,一次只有一个在途。
- **作废局**:没有上下文的作废终局已经用提示告诉过玩家(`OnBattleVoidNotice`),视同展示过,直接确认,补查时不再弹面板;补查查到别的作废局按作废显示(标题「本局作废」、作废说明行)。
- **回包里 `seen = true` 的局**:顺手从待确认列表里删掉(上一次确认其实到了,不必再补发)。
- **展示内容**:记录里没有经验与战后血蓝,补弹面板不列这两项;货币、获得与消耗的道具、`items_mailed` 提示行、`lines_truncated` 提示行、阵亡 / 逃离照列。说明行是「上次未查看的战斗 · MM-dd HH:mm 结束 · 共 N 回合」。
- **文件**:新 `Assets/Scripts/Game/Battle/BattleResultRecoveryClient.cs`(纯逻辑,不引用 UnityEngine);`BattleClient.cs`(新事件 `OnBattleEndWithoutContext(end, alreadyNotified)`);`GameClient.cs`(`BattleResults` 装配,`Tick` 驱动);`BattleResultPanel.cs`(`ShowRecovered`、`BuildRecoveredLines`、`BuildRecoveredSubtitle`,胜负标题与展示抽成 `ResolveTitle` / `Present`,现场与补弹共用);`BattleUiRoot.cs`(补弹排队与确认分支)。
- **用例**:新 `Assets/Tests/EditMode/Battle/BattleResultRecoveryClientTests.cs` 13 例,`DamageNumberLayoutTests.cs` 加 4 例。命令见 `docs/PROGRESS.md` 2026-11-12「近期战斗结果客户端补查与补弹」。

## 8. 日志与可观测性

scene 进程没有 Prometheus 端点,计数从结构化日志提取。player_id 只是日志字段,不进任何指标 label。

| 日志 | 级别 | 何时 |
|---|---|---|
| `metric=battle_result_recorded` | INFO | 写进一条结果(带 result_seq、货币与物品行数、是否截断、淘汰数) |
| `metric=battle_result_lines_truncated` | WARN | 物品行超过上限被截断 |
| `metric=battle_result_duplicate` | WARN | 列表里已有同一 battle_id,不写第二条 |
| `metric=battle_result_record_missing` | ERROR | 效果已写完时组件不在(缺陷) |
| `metric=battle_result_published` | INFO | 存档确认提交,结果公开 |
| `metric=battle_result_history_missing` | ERROR | 查询或确认时组件不在(加载不变量被破坏) |
| `Recent battle results queried` | INFO | 每次查询(返回条数、未读条数、待公开条数) |
| `Battle results acknowledged` / `rejected` | INFO | 每次确认 |

## 9. 不做与后续

- 经验系统接入时,在 `RecordBattleResult` 同一处把实际入账的经验写进记录(新字段)。
- 长历史不在本列表里做,见 §3 对 (b) 的说明。PVP 战报回放由[战报回放](battle-report-replay.md)另做。
- robot 冒烟场景本轮不写,冒烟步骤见 §10,由 Codex 用改好的客户端或补 robot 场景执行。
- Java 版待做(§11)。

## 10. 验证(交 Codex,AGENTS §10.1)

1. **协议生成**:仓库根 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`(生成器源码有变化时先 `-Command proto-gen-build`)。期望:
   - 新增 `cpp/generated/proto/common/component/player_battle_result_comp.pb.{h,cc}`、`cpp/generated/proto/scene/player_battle_result.pb.{h,cc}`,并由生成器登记进 `cpp/generated/proto` 的 vcxproj、filters、CMakeLists;
   - 生成 `cpp/nodes/scene/handler/rpc/player/player_battle_result_handler.h`;同名 `.cpp` 已手写好守护段,生成后守护段内容不变;
   - `player_service_interface.cpp` 登记 `SceneBattleResultClientPlayer`;`proto/message_id.txt` 末尾追加两个号;`proto/persist_schema_gen.txt` 追加新字段;
   - Go 绑定 `go/proto/common/component/player_battle_result_comp.pb.go`、`go/proto/scene/player_battle_result.pb.go`,`go/proto/battle/battle_data.pb.go` 多出 `FinishedAtMs`。
   - 不需要导表:没有新 tip 码。
2. **C++ 构建**(Windows Debug x64,一律串行 `/m:1`):`mysql_database_table.proto` 与 `battle_data.proto` 改了,先整编 `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`,0 个 `error C` / `LNK`。测试工程链接预编好的库,不在 `game.sln` 里,库编完之后再逐个跑 `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build -Filter <工程> -LogDir build/verification/battle-result-recovery-<日期>`:`bag_test`、`currency_test`、`cross_zone_test`、`turn_battle_engine_test`。
3. **通过标准**:
   - `bag_test`:`BattleResultHistory.*`、`PlayerBattleSettlementTest.*BattleResult*`、`PlayerBattleSettlementItemTest.*BattleResult*` 全过;`PersistRoundTripTest.UnknownFieldsOnEveryCarriedNodeSurviveLoadAndSave` 的覆盖率断言不点名本次新增的四个类型;`PlayerFeaturePersistenceTest.MarshalRefusesWhenAnyPersistedComponentIsMissing` 过。其余失败集合不得超出 11-02 r3 基线(`build/player-store-verify-20261102-r3-prep/06-cpp-tests/bag_test.stdout.log` 的 16 条)。
   - `currency_test`:`PlayerInboxSystemTest.*` 全过(三条战斗结算域用例要求夹具已挂结果组件)。
   - `cross_zone_test`、`turn_battle_engine_test`:全量不回归;`BattleResultCommit.FullSourcePreservesAllAssetsAndFixedBytes` 过(终局时刻已盖进个人结算)。
4. **冒烟**(本机调试栈,两版 Redis 按惯例):用改好的客户端(或补一个 robot 场景)开一局 PVE、开自动战斗,战斗中断开客户端,等战斗结束、结算入账(scene 日志出现 `metric=battle_result_recorded` 与 `metric=battle_result_published` 各一次),再重新登录。观察:
   - 落地后 `GetRecentBattleResults` 返回的结果里,这一局 **恰好一条**,`seen = false`,货币与掉落和背包实际变化一致,`finished_at_ms` 非 0;
   - 发 `AckBattleResults` 之后再查,同一条 `seen = true`,条数不变;
   - 情形 B(完全下线、重登才应用)另跑一次:重登后客户端收到无上下文的 `BattleEndS2C` 触发查询,同样只一条;
   - 日志里同一 battle_id 的 `battle_result_recorded` 只出现一次,金币只到账一次。

## 11. 双版本(AGENTS §12)

新协议、`PlayerBattleResult*` 消息、`BattleSettlementData.finished_at_ms` 都是客户端可见契约,两版必须一致。Java 版 `0.1.0-SNAPSHOT` 待做,Java 仓 `PARITY.md` 已登记一行:在 Java 的结算应用点同帧写记录并随同一次存盘提交,SQL 确认之后公开;battle 冻结终局时盖结束时刻;scene 实现两个 RPC,错误码同 §6。Java 按自己的存盘形态实现,不照抄本仓的分组。

## 12. 风险与未验证

- 全部未编译、未测试。
- `player_database` 字段号 23 是落码时的下一个空闲号;别的会话同时加字段可能撞号,合并时以先合入者为准,后者改号并重新生成。
- 发布顺序必须 scene 先于 battle(§6)。
- 旧客户端不发确认:结果永远是未读,列表有界,只影响「未读」提示。
- 确认被拒(冻结、交接)或丢失:客户端可能在下一次落地再弹一次同一局;按 battle_id 去重只在一个客户端进程内有效。
