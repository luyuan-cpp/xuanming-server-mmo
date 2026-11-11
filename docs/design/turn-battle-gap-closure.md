# 回合制战斗缺口收口 G1–G9(2026-09-17)

> **2026-11-01 更正**:结算已改为只走 match SQL + player_inbox([总体方案 §5.4](./player-data-tidb-scaling-plan.md))。
> §3、§5–§9 里的 `battle:settlement:pending`、发件箱、结算账本、「锁留到落盘」都是历史记录,代码里已经没有了。
> 战斗锁本身的可靠性与恢复分支的去向见 [§10](#10-2026-11-01-战斗锁写入可靠性恢复分支的去向已落码未编译待-codex-验证)。
>
> 状态:**已落码;导表与 proto 重生成已执行(2026-09-18);未编译、未跑测试、未跑冒烟**。
> 用户 2026-09-18 明确指示"不用等 Codex,你帮他做完,不用编译,直接合并到 main",
> 因此 §7 的第 1、2 步已由本会话执行完毕并入库(结果见 §8),第 3–6 步(编译 / 单测 / 冒烟 / 客户端)
> 按用户指示跳过 —— **代码与生成产物从未被编译器看过,正确性未经机器验证**。
>
> 背景:用户连问三轮"战斗掉落要不要 snowflake iid / 战斗里的背包会不会不同步 / 冷却和 buff 是不是快照副本",
> 三轮各跑了一次对抗核验(3 个反驳者 + 1 个完整性审稿,逐文件读码)。核验没有推翻任何一条主线结论
> (iid 归 scene、battle 只有可丢弃副本、引擎在 battle 节点),但翻出 9 个真实缺口。本轮把它们一次收口。
>
> 主文档 [turn-based-battle-server.md](./turn-based-battle-server.md) 只加一节索引(§20)与决策 D41–D50,
> 细节全部在本文件,避免与 team / cross-zone-travel / K8s battle 三条并行线在同一文件上打架。

## 1. 缺口核验表

行号以 2026-09-17 摸底时的 HEAD(`2a2b793f8`)为准,已随本轮改动漂移。

| # | 缺口 | 核验到的现状(改前) | 本轮处置 |
|---|------|------|------|
| G1 | 战斗掉落不落地 | 引擎 `BuildSettlement` 注释"掉落 items_gained 依赖掉落表,留待后续接入";`MonsterTable` 无掉落列;scene 侧只 `LOG_INFO` | Monster 表加 `drop` 槽;引擎终局摇点写 `items_gained`;scene 真入包(D41/D42/D43) |
| G2 | 战斗内用药形同虚设 | 快照 `items` 一期传空;`ItemTable` 无战斗列;固定回血 100;`target_id` 被忽略只能自疗;非法 ITEM 静默换普攻且客户端收 OK;PVP 无限次用药 | Item 表加三列;效果读表;可给同队用;提交回错误码;PVP 限次(D44–D47) |
| G3 | 消耗不真扣 | `Bag::RemoveItems` 全或无,`DrainStacks` 无回执,`BagService` 无按数量扣除入口,流水无战斗来源 | 实例层出回执、桥层加夹紧扣除、编排层逐条落流水(D43) |
| G4 | 战斗中 scene 侧不设防 | 只有加点/宠物/切场景/镜像/跨 zone 有闸;实时技能、实时 buff、移动、背包整理、GM 回滚全无 | 六处补闸,口径写进冻结清单(D48) |
| G5 | 快照 buff 脏 | 全量拷(含眩晕/冰冻/沉默)、瞬时 buff 当无限、`caster_id` 是 scene 的 entt 整数 | scene 过滤 + 域映射,引擎侧再兜一层(D49) |
| G6 | 快照技能不过滤 | 被动/开关/持续施法技能都能当普通技能提交并打出伤害 | scene 过滤 + 引擎黑名单(D50) |
| G7 | 状态广播泄露 + 客户端看不到自己道具 | `BattleStateS2C` 把全员冷却发给对手与观众;本人剩余道具无字段承载,重连后归零 | 节点按收信人裁剪 + `self_items` 新字段(D45/D46) |
| G8 | 指纹不含 Item 表 | 六张表指纹;用药效果改读表后,两端 Item 表版本不同会让同一 id 效果不同 | 指纹加第七张表 |
| G9 | 结算路由固定在开局 | 首投打开局时的 scene 节点 | **已被 2026-09-08 的 R07 结算出箱修掉**(每 10s×12 轮重解析 `player:{id}:location` 重投),本轮不改码,只更正文档 |

## 2. 决策(并入主文档 §19.1 同一数轴,从 D41 起)

| # | 决策 | 理由 |
|---|------|------|
| D41 | **掉落配在 Monster 表的 `drop` 槽**(`drop_item` / `drop_count` / `drop_rate` 万分比整数),不新建 Drop 表、不复用 Reward 表 | 新建表要改 `table.vcxproj` / `CMakeLists` 并且只生成 `FindAllById`;Reward 表没有概率列。槽用 STRUCT_LIST 而不是三条平行 `repeated` 标量:后者任一槽填 0 会被导表器按哨兵丢掉,三条数组当场错位 |
| D42 | **掉落在引擎终局一次性摇**,放在 `UpdateOutcome()` 之后;遍历序 settlements(player_id 升序)× defeatedMonsters(击杀序)× 表内槽序 | 不变量 5 要求随机只走种子 RNG。放在逐怪死亡时摇会把随机消费插进战斗中段,平移后续暴击/随机选目标的序列,同种子回放基线与既有用例期望值一起失效 |
| D43 | **组队 PVE 每个合格成员独立掷点、各拿各的**,不做分赃;发奖条件与经验/金币完全一致(玩家方、未逃、未阵亡) | 与现有"人人全额"的经验金币口径一致;分赃需要产品定规则(轮抽/掷骰/拾取权),现在定等于替策划拍板 |
| D44 | **战斗道具效果读 `ItemTable.battle_usable / battle_heal_hp / battle_heal_mp`**,删掉写死的 `kDefaultItemHealHp=100` | 数值归策划。回血量还要跟着属性单位走(血量千级、法力 ×12/×4 的历次调整),写死在代码里每次调数值都要改码重编 |
| D45 | **ITEM 可以给同队存活单位用**(`target_id` 为 0 或自己 = 自疗);给敌方用药拒绝 | 协议注释本来就写着"ATTACK/SKILL/ITEM 的目标",客户端也一直在传目标——是引擎没读。组队 PVE 给队友喂药是回合制的基本盘 |
| D46 | **`SubmitBattleAction` 回校验码**:引擎加零副作用的 `ValidateAction`,节点先查后提交,失败填 `error_message` | 原先"对客户端一律回 OK,不泄漏校验细节"的代价是:玩家点了吃药、客户端显示成功、回合结算却变成默认普攻,且无从得知原因。玩家资产路径不许静默降级(AGENTS §11.3)。不改 proto,不触发 regen |
| D47 | **PVP 每人每场道具上限 `kMaxItemUsesPerBattlePvp = 5`,PVE 不限** | 打满 `max_rounds` 判进攻方负,而 PVP 一期不可逃:不限次用药的防守方可以靠海量药水拖满回合白赢,进攻方没有止损手段。5 是可调常量,调了要同步改本节 |
| D48 | **局中闸补六处**:实时技能(施法者 + 目标 + 落点期)、**实时 buff 挂载**(`BuffSystem::AddOrUpdateBuff`,含服务端派生的子 buff)、移动上报、位移积分、背包整理、GM 回滚类写操作。**闸只放客户端入口层与实时战斗落点,绝不下沉到 `BagService` / `CurrencySystem`** | 下沉会把结算自己挡住:`ApplySettlementToEntity` 入账时 `InBattleComp` 还挂着(`ClearBattleFreeze` 在应用之后)。判定统一用 `any_of<InBattleComp>` 而不 include `player_battle.h`,避免 skill/buff/movement 这些底层 TU 反向依赖 scene 的战斗服务层 |
| D49 | **快照 buff 剔除控制类(眩晕/冰冻/沉默)与瞬时类**;`caster_id` 只映射"施法者是自己"这一种,其余置 0 | 控制类按表全量时长换算,场景里只剩半秒的眩晕进战斗会变成整整几回合,PVP 里还能被对手在应战前挂上。`caster_id` 是 scene 的 entt 实体整数,与引擎 actor_id 不是一个域,恰好撞上某个 player_id 就会被算成那名玩家(叠层隔离、BUFF_TICK 来源都按它判) |
| D50 | **技能过滤用黑名单**(剔除被动 / 开关 / 持续施法),不是白名单 | 存量 Skill 表未必每行都填了 `skill_type`,白名单会把没填的一律判死。黑名单只拦三类确定不该主动施放的 |

**明确不做**(留给后续,不在本轮):精确 buff 剩余时长(要给 `BuffComp` 加到期时间字段);经验入账(没有经验/等级系统);装备属性进战斗(`bonus_values` 无写入方);Excel 回合列;`SubmitBattleAction` 回合号幂等(D40);客户端道具面板消费 `self_items`(客户端仓,需单独授权)。

## 3. 契约变更

| 类型 | 变更 | 影响面 |
|---|---|---|
| 配表 | `ItemTable` 加 `battle_usable=4` / `battle_heal_hp=5` / `battle_heal_mp=6`;`Item.xlsx` 同步加三列(物品 10 = 回血 300,物品 11 = 回蓝 120,其余 0) | 导表 → C++/Go/Java/C# 表代码全量重生成 |
| 配表 | `MonsterTable` 加 `repeated Monsterdrop drop = 10 [(cfg_slots) = 2]`;`Monster.xlsx` 在 `exp_reward` 前插 6 列(怪 1 必掉 1 个物品 10,怪 2 掉物品 10/11);其余怪留空待策划 | 同上;**改表即改战斗指纹**,scene 与 battle 必须同版本同批替换 |
| proto | `BattleStateS2C` 加 `repeated BattleItemEntry self_items = 7` | 需 regen(C++ / Go / 客户端 `gen_proto.ps1`);无新增 RPC,`message_id` 不变 |
| 指纹 | `BattleTableFingerprint::ComputeFrom` 加第七参 `ItemTableData`,追加在末尾 | 指纹值必变;match 与 battle 两侧默认 `warn`,灰度期不一致只告警 |
| tip | **零新增**。复用 `kInvalidParameter` / `kInvalidTableId` / `kBagInsufficientItems` / `kSkillInvalidTarget` / `kSkillInvalidTargetId` / `kSkillCannotBeCastInCurrentState` | 不动 `Tip.xlsx`(二进制文件,team / trade 会话也在排队改它) |
| 流水 | `LogItemCreate` / `LogItemDestroy` 尾部加 `correlationId` / `extra` 默认参数;`BagService::AddItems`(ItemCountMap 重载)同样加这两个尾参并透传 —— 否则掉落那一半的来源永远进不了流水。掉落用 `TX_ITEM_AWARD`,消耗用 `TX_ITEM_DESTROY`,两者都带 `battle_id` 与 `{"source":"battle","battle_id":N}` | 不动 `transaction_log.proto`(不新增枚举 = 不 regen);尾参带默认值,既有调用点零改动。**逐件重载 `AddItems(vector<InitItemParam>)` 本轮未加**(战斗掉落不走它),日后邮件/托管接入时需同步 |

## 4. 改动集(逐文件)

**配表**
- `data/schema/item_table.proto`、`data/Item.xlsx`:三列战斗属性。
- `data/schema/monster_table.proto`、`data/Monster.xlsx`:`Monsterdrop` 子消息 + 2 槽掉落。

**引擎 `cpp/libs/services/battle/`**
- `data/battle_data_provider.h` / `table_battle_data_provider.{h,cpp}`:新增纯虚 `FindItem`。
- `data/battle_table_fingerprint.{h,cpp}`:第七张表(G8)。
- `constants/turn_battle_constants.h`:删 `kDefaultItemHealHp`,加 `kDropRateDenominator` / `kMaxItemUsesPerBattlePvp`。
- `system/turn_battle_engine.{h,cpp}`:
  - `ValidateAction`(公开、零副作用,给节点回码);`CheckItemUse`(提交期与出手期共用的唯一判据);
  - `ExecuteItem` 重写:出手期重校验 → 扣副本 → 记账本 → 按表回血/回蓝 → 事件 target 改为真实目标;
  - `RollDrops`(终局一次性摇);`SelfItems`(本人剩余道具);`FindItemEntry` 两个重载;
  - `SanitizeSnapshotBuffs`(在全部单位就位后统一跑)、`IsTurnBattleCastableSkill`(技能黑名单);
  - `InitPlayers` 技能拷贝改为过滤后拷贝;`ResolveCurrentRound` 第 7 步调 `RollDrops`。

**battle 节点 `cpp/nodes/battle/`**
- `logic/battle_room_manager.{h,cpp}`:`RedactStateForViewer` / `FillSelfItems` 两个新私有方法;四个下发点(开战首帧、回合广播、重连补拉、观战首帧)按视角出包;`HandleSubmitBattleAction` 先 `ValidateAction` 再提交。
- `main.cpp`:表就绪校验与启动日志加 Item 表(空表只 WARN,不算致命)。

**背包域 `cpp/libs/modules/`**
- `bag/item_store.{h,cpp}`:`DrainStacks` 返回实扣量 + `StackDrain` 回执,跳过僵尸堆。
- `bag/bag_system.{h,cpp}`:`DrainedInstance` 结构;`RemoveItems` 加可选回执(语义不变,仍全或无);新增 `RemoveItemsClamped`;私有 `DrainOneConfig` 收口两条路径。
- `bag/bag_service.{h,cpp}`:新增 `RemoveItemsClamped` 编排(冻结检查 → 扣除 → **逐条**落 `TX_ITEM_DESTROY`)。
- `transaction_log/transaction_log_system.{h,cpp}`:两个 Log 函数加 `correlationId` / `extra` 尾参。

**scene `cpp/libs/services/scene/` 与 `cpp/nodes/scene/`**
- `battle/system/player_battle.cpp`:快照三段(技能过滤 / buff 过滤 + caster 映射 / 道具副本)、结算 `ApplySettlementItems`(先夹紧扣消耗、再发掉落,主包满退临时格)。
- `combat/skill/system/skill.cpp`:施法者在战拒绝、目标在战拒绝、落点期整体早退。
- `combat/buff/system/buff.cpp`:战斗中玩家不挂实时 buff。
- `spatial/system/movement.cpp`:tick 侧 `exclude<InBattleComp>`。
- `player/system/player_feature_snapshot.cpp`:整理背包在战拒绝。
- `nodes/scene/handler/rpc/player/player_movement_handler.cpp`:三个移动上报入口静默丢弃(响应是 `Empty`,回不了 tip;高频包不打日志)。
- `nodes/scene/handler/rpc/player/player_rollback_handler.cpp`:`RejectIfFrozen` 并入战斗在途判定。

**评审后追加的修正**(对抗评审 34 条发现、29 条经独立核实成立,已逐条处置)
- `player_rollback_handler.cpp` 漏 `battle_comp.pb.h`(**编译不过**,实现者无法编译才会漏的那一类)。
- `CheckBuff` 把 SkillPermission 表里"没填"的 0 格原样当错误码返回 → `ValidateAction` 可能回 0,
  节点写进 `error_message.id` 会被客户端读成"成功"。改为坏表按 `kInvalidTableData` 报,节点再兜一层。
- 结算消耗加**反向校验**:只扣 `battle_usable` 的物品。否则一个陈旧/伪造的 battle 节点可以
  点名销毁玩家的任意物品(装备、任务道具)。快照出包与结算扣除共用 `IsBattleUsableItem` 一处判据。
- 掉落入主包失败后**只补投没进去的那部分**:`BagService::AddItems` 返回失败不代表包没变
  (临时格可能已腾位),整批重投等于凭空复制道具。
- 跨 zone 冻结期**整笔结算延后**:原先 `gold_gain == 0` 而只有道具的结算无人拦,会被标记
  Applied 而道具一件没动。现在在任何不可重复副作用之前 `return false` 保留 pending。
- 消耗扣完顺手 `MergeAndCompact(kMergeOnly)` 回收 `size==0` 的僵尸堆(只回收、不挪位置),
  否则主背包会被僵尸堆占满、后续掉落直接满包。
- 出手期目标已死不再让整个回合静默蒸发,**回落成自疗**;`battle_usable=1` 但两列效果都为 0 的
  半填配表在提交期就拒(否则药被吃掉、回合被浪费)。
- `RollDrops` 去掉按 `defeat.count()` 放大(每条恒为一次击杀),与经验/金币聚合口径对齐。
- `SanitizeSnapshotBuffs` 去掉 `const`(它改引擎自己的 actors);`RedactStateForViewer` 统一
  `clear_self_items()`,不再依赖"引擎恰好不填"。

**测试**
- `cpp/tests/turn_battle_engine_test/`:`memory_battle_data_provider.h` 加 `AddItem`/`FindItem`;指纹用例加第七张表;既有 ITEM 用例改为表驱动(provider 里配回血 100,断言不变);新增 9 个用例(掉落必掉/概率 0 不掉、表驱动效果与回蓝、非战斗道具与未知 id 被拒、给队友用药与给敌方被拒、PVP 限次、`SelfItems` 余量、buff 清洗、被动技能不可提交)。
- `cpp/tests/bag_test/bag_test.cpp`:3 个用例(夹紧扣除与回执、全或无语义不变、缺物品与 0 数量是空操作)。
- `cpp/tests/bag_test/player_battle_settlement_test.cpp`:`PlayerBattleSettlementItemTest` 5 个用例(真扣真发、不足夹紧、完全没有也不失败、非战斗道具被拒不销毁、重投不双扣双发)。
- `cpp/tests/bag_test/player_feature_snapshot_test.cpp`:`InBattleSortKeepsSourceLayoutUnchanged`(局中整理被拒且布局未动)。
- `cpp/tests/turn_battle_engine_test/battle_table_fingerprint_test.cpp`:`MakeTables` 补 Item 行与 Monster 掉落槽,
  并新增两个"改了就必须变指纹"的子断言 —— 否则 `ComputeFrom` 漏掉 Item 段也照样全绿。

## 5. 新增/收紧的不变量

1. **道具余量的唯一真相是引擎私有的开局快照副本**(`createRequest.players[].items`)。`BattleActorState` 不承载道具;要下发给客户端只能经 `BattleStateS2C.self_items`,且只填给本人。
2. **掉落只在引擎判定 `SIDE_A_WIN` 的那一刻摇一次**。`BuildSettlement` 仍是 `const` 且可重复读(节点 outbox 会重投并重复读),绝不在里面摇点或改状态。
3. **结算里的道具失败不得让整笔结算失败**:金币已在前面入账,返回 false 会让应用缓存抹掉记录、重投时重复加钱。道具失败一律记日志 + 继续。
   **2026-10-19 补充**(审计 cpp-economy#0 / #1,[data-overwrite-rollback-audit-20261018.md](data-overwrite-rollback-audit-20261018.md) §2.4):
   - 「会自己消失」的失败必须在金币之前判掉,让整笔延后,不能留到这里吞掉。现在 item 号段余量不够时整笔延后:
     需求量取 `BagService::MaxNewInstancesFor`,预检在 `HasItemGuidsForDrops`。留到这里的只剩包满、禁发、配置错误这类终局失败。
   - 金币反过来:`AddCurrency` 回终局的 `kAssetBlocked` 时只跳过金币、继续结算,其余失败仍整笔延后。
4. **局中闸只放入口层**(见 D48)。任何人想把 `InBattleComp` 判定加进 `BagService` / `CurrencySystem` 之前,先读这条。
5. **快照进引擎前必须清洗**:引擎 `InitPlayers` 对 buff 与技能各有一道兜底过滤。快照来自另一个进程(甚至另一个 zone 的 scene),不能单点信任。

## 6. 已知残留

- **G9 的首投延迟**:结算首投仍打开局时的 scene 节点,玩家换节点重登后要等 battle 侧下一轮探测(≤10s)才入账。R07 的 12 轮 ×10s 耗尽后靠登录钩子兜底。本轮判定为可接受,不改码。
- **buff 剩余时长仍按表全量换算**:控制类已剔除,剩下的都是周期效果类,影响有界。要精确剩余时长得给 `BuffComp` 加到期时间字段。
- **脱敏挡不住推算**:`TurnResultS2C.events` 带 `skill_table_id` / `buff_table_id`,对手查表仍可推算冷却与 buff 时长;`pending_actor_ids` 与 `is_auto` 也暴露"谁已出手/谁挂机"。本轮只堵"直接读"。
- **`items_gained` 全部落在主背包或临时格**:没有邮件兜底。临时格 2026-11-05 起满了就拒、不再 FIFO 挤掉更早的掉落:两处都放不下时整笔结算延后,
  玩家用 `RetrieveTemporaryItems` 把临时格的东西取回人物背包腾位后自动补发(`bag-rule-policy-layering.md` §6.6)。
- **掉落数据只填了 1、2 号怪**:其余 14 只怪的 `drop` 留空,等策划。
- **战斗期间 buff 只出不进**:新 buff(含存量 buff 的子 buff 派生)被丢弃,而 `BuffSystem::Update`
  仍在给存量 buff 计时到期。要完全对称还得给 tick 也加 `exclude<InBattleComp>`,本轮判定为可接受
  ——战斗期间场景 buff 的存续本就不进战斗判定。
- **进战不清 `Velocity`**:tick 侧 `exclude<InBattleComp>` 只是把位移推迟到战斗结束那一刻。
  要彻底应在挂 `InBattleComp` 时清零速度并打脏位,本轮未做。
- **逐件 `AddItems(vector<InitItemParam>)` 重载没有 correlationId/extra**:战斗掉落不走它,
  邮件/托管接入时要补,否则那条路的流水同样没有来源。

## 7. 验证清单(第 1、2 步已执行,见 §8;第 3–6 步按用户指示跳过)

工作目录除特别说明外均为仓库根 `E:\work\xuanming-server-mmo`。**动手前先 `git status --porcelain --ignore-submodules=all`**:本轮工作区里还有并行会话的改动(帮会二期、发布打包、merge_zone、go/guild、go/friend),不要把它们混进同一次验证结论。

1. **导表**(改了 `data/schema/*.proto` 与两张 xlsx,必须先于一切 C++ 编译):
   前置 `PATH` 里要有 `protoc-gen-go` / `protoc-gen-go-grpc`,仓内 protoc 必须是 `libprotoc 35.1`(`third_party\protobuf\install_vs2026_dbg\bin\protoc.exe`，2026-10-26 起为独立安装);先关掉 Excel(避免 `~$` 锁文件)。
   - 零副作用预检:`py tools/data_table_exporter/tools/sandbox_export.py --out <临时目录> --compare`
   - 正式导表:`py tools\data_table_exporter\run.py tools\data_table_exporter\exporter_config.yaml`(不要直接跑 `dev.bat`,它失败路径带 `pause`,非交互会卡住)
   - 刷新索引:`py tools/data_table_exporter/tools/gen_schema_index.py`
   - 通过标准:日志末尾 `===== Data Table Exporter: DONE =====`,无 `FK:` / `主键:` / `DeployError` / `陈旧产物`;`generated/tables/item.json` 出现 `battle_usable`,`monster.json` 出现 `drop`。
   - **副作用提醒**:导表会写同级客户端仓 `..\mmorpg-client\Assets\Scripts\Table\Generated`。
2. **proto 重生成**(`proto/battle/player_battle.proto` 加了 `self_items`):
   `pwsh tools\scripts\dev_tools.ps1 -Command proto-gen-build` 然后 `pwsh tools\scripts\dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools\proto_generator\protogen\etc\proto_gen.yaml`;随后 `cd go && build.bat`。
   - 正向断言(regen 吞代码的历史坑):`grep -n AcquireCreatePermitBlocking cpp/nodes/scene/handler/grpc/scene_node_service.cpp` **必须命中**。
     (2026-10-16 更正:今后改为 `grep -n AcquireAllocationPermitAsync` 同一文件必须命中,Agones 块在 `grpc.CreateScene.guard` 守护段里,见 grpc-muduo-transport.md §15.14 第 3 步。)
   - 核对 `proto/message_id.txt` 既有号一个不少(本轮不新增 RPC,该文件应无实质变化)。
3. **C++ 串行编译**(一律 `/m:1`,MSBuild 在 `D:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\MSBuild.exe`):
   顺序 `proto → table → modules → battle → scene → 节点(battle/scene)`。
   `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64 /t:modules;battle;scene`,再 `/t:battle_node;scene_node`(工程名以 `game.sln` 为准)。
   - 期望 0 error;`bin/battle.exe`、`bin/scene.exe` 的 mtime 晚于所有 `lib/*.lib`。
   - scene.exe 在跑会占 PDB 导致 `LNK1201`,先停节点。
4. **C++ 单测**:`pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter 'turn_battle_engine_test|bag_test|cross_zone_test'`。
   - 期望退出码 0;基线 turn 115/115、bag 220/220。本轮新增:引擎 9、bag 容器 3、结算 5、整理闸 1 = 18 个用例
     (指纹用例是在既有用例内加断言,不增计数)。总数应为 turn 124、bag 229 左右 —— **以实际为准,
     关键是无 `[FAILED]` 且退出码 0**。
   - 注意该脚本内置两个坑:测试 exe 需要 `bin/` 下的 `zlibd.dll`/`rdkafka*.dll`;退出码非 0 但没有 `[FAILED]` 行 = 用例中途 `LOG_FATAL`,判红。
5. **robot 冒烟**(`cd robot`,先 `go mod vendor` 因为 regen 动过 `go/proto`):
   `.\robot.exe -c etc\battle_smoke.yaml`,期望 `BATTLE_SMOKE_OK`、退出码 0。
   - **本轮新增的观察点**:PVE 打赢 1 号怪后,玩家主背包应出现 1 个物品 10(`Monster.drop` 必掉);scene 日志无 `道具结算暂缓`(该日志已删),battle 日志无 `fingerprint mismatch`。
   - 指纹变了:**scene 与 battle 必须同批替换**,只重编一端会触发 `warn` 告警(默认不拒开局,但结论不可信)。
6. **客户端**(独立仓,需用户授权;不做也不影响服务端结论):
   `pwsh -File E:\work\mmorpg-client\tools\gen_proto.ps1 -ProtoRoot E:\work\xuanming-server-mmo`、`gen_messageids.ps1`、`client_compile_check.ps1`。
   本轮客户端无代码改动,`self_items` 是新增可选字段,老客户端忽略即可。
7. 任一步失败:保留该步完整 stdout/stderr(导表器是 fail-closed,报错即"产出未落盘")、MSBuild 首个 error 段、scene/battle 日志与失败 step 名;**不重试、不改判据**,回报后等决策。

## 8. 已执行:导表 + proto 重生成(2026-09-18)

2026-10-26 工具路径注记：本节命令和结果保留 09-18 的实际记录。今后重跑使用 §7 的独立 Protobuf 路径，不再从下文历史 gRPC SDK 目录取工具。

本会话按用户指示替 Codex 跑完了生成链的两步。**这两步不是编译**,产物已随仓库的每小时自动提交进入 `main`。

### 8.1 实际执行的命令

```
# PATH 前置(E:\work\tools 已被清空,protoc 用仓内的,protoc-gen-go 在用户 go/bin)
PATH=C:\Users\luyua\go\bin;<repo>\third_party\grpc\install_vs2026_dbg\bin;%PATH%

py -3 tools/data_table_exporter/tools/sandbox_export.py --out <tmp> --compare   # 零副作用预检
py -3 tools/data_table_exporter/run.py tools/data_table_exporter/exporter_config.yaml
py -3 tools/data_table_exporter/tools/gen_schema_index.py
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary \
     -ConfigPath tools/proto_generator/protogen/etc/proto_gen.yaml
```

`dev.bat gen` 没有直接用:它的失败路径带 `pause`,非交互会卡住;`proto-gen-build`(内部是 `go build`)
也没跑 —— 用的是现成的 `proto-gen.exe`(2026-09-16 22:14 构建)。`cd go && build.bat`(goctl)同样未跑。

### 8.2 结果与核对

| 检查项 | 结果 |
|---|---|
| 导表 | `===== Data Table Exporter: DONE =====`,`Deploy: 12 OK, 0 failed`,manifest version=17 / 31 张表 |
| Item 表产物 | `ItemTable` 生成出 `battle_usable=4` / `battle_heal_hp=5` / `battle_heal_mp=6`;`item.json` 里物品 10 = 回血 300、物品 11 = 回蓝 120,其余为 0 |
| Monster 表产物 | 生成出子消息 `Monsterdrop{drop_item,drop_count,drop_rate}` 与 `repeated Monsterdrop drop = 10`;1 号怪 `[{10,1,10000}]`、2 号怪 `[{10,1,5000},{11,1,2000}]`、3 号怪空 |
| C++ / Go 访问器 | `item_table.pb.h` 有 `battle_usable()`;`monster_table.pb.h` 有 `class Monsterdrop` 与 `drop(int)`;Go 侧 `BattleUsable` 已生成 |
| proto 重生成 | 完成(47s);`player_battle.pb.h` 含 `self_items`(36 处),Go 侧同样生成 |
| **Agones 正向断言** | `grep -c AcquireCreatePermitBlocking cpp/nodes/scene/handler/grpc/scene_node_service.cpp` = **1,命中**(regen 没有吞掉这个块) |
| 工程登记 | `proto.vcxproj` / `table.vcxproj` 未登记的 `.pb.cc` = **0**(本轮只给既有表加列、给既有 message 加字段,不产生新文件) |
| 客户端仓 | 生成器写入后 `../mmorpg-client` 无非预期改动(仅 2 个与本任务无关的 Qdao 测试文件处于修改态) |

### 8.3 ⚠ 这次 regen 顺带材料化了并行会话的改动

`proto/message_id.txt` 除本任务外还变了两处,**都来自并行的帮会二期会话**(它们的 proto 已在 `main` 上):

- 新增 8 条:`216=GuildServiceTransferGuildLeader` … `223=GuildServiceReviewGuildApplication`;
- **号位重分配**:`19` 从 `GuildServiceJoinGuild` 改判给 `GuildServiceSetGuildMemberRole` ——
  因为它们把 `rpc JoinGuild` 删掉换成了 `ApplyJoinGuild`,19 号空出来被发号器回收再分配。

这不是本任务引入的,但**是本次 regen 把它落盘的**。`message_id` 是客户端可见契约,
帮会会话需要知道 19 号已经易主(项目未上线、无老客户端,开发期可接受)。
本任务自己没有新增任何 RPC,`self_items` 只是给既有 message 加字段,**不影响任何消息号**。

### 8.4 仍然没做的(用户明确跳过)

C++ 串行编译、C++ 单测(`run_cpp_tests.ps1`)、robot `battle_smoke`、客户端 `gen_proto` / 编译体检。
因此:**本轮代码从未被编译器看过**。§7 的第 3–6 步原样保留,谁要跑照着跑即可。
风险集中在三处 —— 新写的 C++ 是否编得过、18 个新单测是否真的绿、掉落与用药的端到端是否跑通。

## 9. 2026-09-25 整合复核(仍未编译)

本轮改动落 `main` 一周后复核一次:期间组队、跨区传送三阶段、帮会二期、聚宝斋资产通道都合并了,
而它们几条线都点名要动 `player_battle.cpp`。结论:**本任务的产出全部完好,与这周的并行改动无冲突**。

| 复核项 | 结果 |
|---|---|
| 16 项成果(配表 / 生成物 / 引擎 / 节点 / 背包 / scene / 闸 / 测试 / 文档) | 全部在 `HEAD` 内,无被合并冲掉的 |
| 六处局中闸 | `skill.cpp` / `buff.cpp` / `movement.cpp` / `player_movement_handler.cpp` / `player_feature_snapshot.cpp` / `player_rollback_handler.cpp` 逐个确认仍在 |
| `ApplySettlementItems` 与 `IsBattleUsableItem` | 仍在,调用点仍挂在结算 lambda 内 |
| `PlayerLifecycleSystem::IsCrossZoneFrozen` | 签名未变,冻结期整笔延后的闸仍成立(跨区传送三阶段没有改它的语义) |
| `ApplySettlementToEntity` / 应用缓存 | 形状未变,三个调用点仍是 `if (!Apply...) return;` |
| `e8b2bfd04`"用 rider 手动删除不需要的头文件" | 删的都是标准库头(`<cmath>` / `<cstdint>` / `<cstddef>` / `<cstdlib>`)。逐个核对:`turn_battle_engine.cpp` 仍留着 `<algorithm>`(我的 `SelfItems` 用 `std::sort`、`ExecuteItem` 用 `std::min`,都还有头);我新增的项目头(`item_table.pb.h` / `battle_comp.pb.h`)一个没被删。**本任务不受影响** |

### 9.1 新出现的背包扣减入口:`BagService::RemoveItemsByGuid`

D48 立的不变量是"闸只放入口层,日后新增的扣减入口必须各自加 `InBattle` 闸"。这一周资产通道
(聚宝斋 / 帮会 B4)新增了 `BagService::RemoveItemsByGuid`(按 guid 批量扣,`TX_AUCTION_SELL`)。

**现在不用动**:它只有单测调用方(`cpp/tests/bag_test/bag_remove_by_guid_test.cpp`),生产零调用点。

**但寄售上架真正接线的那一刻必须补闸** —— 否则玩家可以在战斗中把身上的战斗消耗品挂上架,
结算按账本扣除时就变成"不足按 0",那几瓶药等于白喝(`player_battle.cpp` 的夹紧 WARN 会开始
常态化刷屏,那就是信号)。闸加在寄售的**客户端入口 handler**,不要加进 `BagService`
(理由见 D48:结算入账时 `InBattleComp` 还挂着,下沉会把结算自己挡住)。

### 9.3 结算幂等基底持久化(**2026-09-25 落码,09-28 按实现评审修订,未编译**)

原缺陷(既有,G1-G9 把影响面从"金币"扩大到"金币 + 掉落"):幂等基底是
`thread_local battle_settlement::SettlementApplicationCache`,**纯进程内**,重启即空;
`ClearPendingSettlementIfMatch` 是 fire-and-forget;登录补应用没有第二道去重。于是二选一:

| 窗口 | 触发 | 后果 |
|---|---|---|
| **丢失(改造前的现状)** | 应用 → 当场销账 → 周期存盘(默认 300s)前崩溃 | 掉落只在内存、pending 已销账、battle 不再重投,而 `transaction_log` 记着发放成功 |
| **复制(只把销账挪到存盘之后就会出现)** | 存盘成功 → 销账那一跳失败 → 重启 → 登录补应用 | 再发一次掉落、再加一次金币 |

两个窗口不能靠调顺序同时堵住。**唯一干净的修法是让"这一局已应用"的标记与资产写进同一份
blob、同一次落盘**,于是两者同生共死:崩溃 → 一起没 → pending 还在 → 重投重发(不丢);
落盘 → 一起在 → 重投/重登被标记挡掉(不复制)。

#### 落地形态

| 件 | 内容 |
|---|---|
| 持久载体 | 新 `proto/common/component/battle_settlement_ledger_comp.proto`:`BattleSettlementLedgerComp.applied[]`,每项 `{battle_id, applied_at_ms}`。**刻意不放进 `battle_comp.proto`** —— 那个文件开头自述"不落库",两条相反契约不共用一段文件头 |
| 存档字段 | `player_database.settlement_ledger = 17`(下一个空闲号;已按 G-03 登记,帮会剩余批次从 18 起) |
| 存盘接线 | `player_database_loader.cpp` 的 Marshal / Unmarshal 各一行,紧挨 `asset_op_ledger` —— 同一条**不变量 I3**:必须与 currency / bag_component 同记录同一次落盘 |
| 纯规则 | `battle_settlement_ledger.h`(header-only,无 ECS/Redis/时钟):`HasApplied` / `RecordApplied` / `ForgetApplied` |
| 去重判据 | `ApplySettlementToEntity` 开头先查**活账本**:命中 = 奖已发过,不再发第二次(返回 true + `alreadyApplied`,调用方不再推结算面板) |
| 销账判据 | `IsSettlementDurable` 只看 `PlayerLastPersistedSnapshotComp` —— 上一次**确实写进 Redis** 的那份 `PlayerAllData`。条目出现在这份字节里,才允许销账 |
| 销账收口 | `AckSettlementPending` 是**唯一销账入口**,原先 7 个 `ClearPendingSettlementIfMatch` 调用点全部改走它。三条规则:活实体 + 账本有 → 落盘才销账;活实体 + 账本无 → 判废,立即销账;**本节点没有活实体 → 不猜**,只有调用方正面证明"锁已被另一局持有"才销账,否则留给登录钩子 |
| **锁留到落盘** | 三条应用路径(在线、按锁匹配、登录补应用)只摘 `InBattleComp`,**不删 `battle:lock`**,并把锁续到至少 `kSettlementLockHoldSec = 180s`(发件箱重投窗口 120s + 余量)。落盘确认后,销账与删锁在**同一条 Lua**(`kAckSettlementScript`)里条件执行,删锁后补一次组队跟随检查 |
| 快路径 | 真正压出一笔存盘后,在同一条连接上紧跟一条 `PING`;玩家存盘客户端与 `tlsRedis.GetZoneRedis()` 是同一个 hiredis 连接,回复按序处理,落地回调同步更新快照 —— PING 回包时当场判落盘、销账放锁,下一场排队只多等一次往返 |
| 双存盘闸 | 销账延后时,若该玩家已有未落地的存盘(`HasUnsettledSave`)就不再压:排队值永远是最新的一份,必然带着账本 |
| 按锁重建前查账本 | `RestoreBattleFreezeOnLogin` / `RebuildBattleFreezeFromLock`:锁指向的局已在活账本里 → 不重建冻结,改推进销账。登录补应用成功后再补一次按锁重建(下一场的冻结不再被旧 pending 挡掉);pending 解析失败的分支**不**重建(那把锁多半就是这条坏记录的那一局) |
| 离线暂存原子化 | 离线分支(玩家不在本节点或正在退出)「锁值 == 本局才写 pending」合成一条 Lua(`kStorePendingIfLockMatchScript`),返回 锁不在 / 已写 / 锁属于别的局。锁留到落盘之后,「看到锁 == 本局」不再证明本局没应用;先 GET 再 SET 的两步之间若插进一次销账,会把已销账的 pending 写回来、下次登录再发一次奖 |
| 缓存让位于账本 | `ApplySettlementToEntity` 在账本未命中时,先清掉进程内缓存里这一局的陈旧已完成项再应用 —— 当场应用,不再「本轮不应用、等下一次投递」(登录钩子只投这一次) |
| 条目回收 | 销账 EVAL **成功回调**里才 `ForgetApplied`。所以账本只登记"已应用但销账未确认"的局,稳态长度 0~1 |
| 排空兜底 | reaper(30s)第二遍扫账本补 Ack —— 不依赖 battle 发件箱(它只重投 12 轮 120s 就放弃) |
| 加列强制 | go/db 在 `AutoMigrateSchema=false` 时启动期只读核对表结构(`proto_sql.assertSchemaUpToDate`):缺 proto 声明的列、或缺表且迁移台账没有干净基线(预建的空库)就拒启并打印补列命令;类型漂移只打 `SCHEMA-DRIFT:` 日志不阻断。操作细节见 [merge-zone-runbook.md](../ops/merge-zone-runbook.md) §4 |

#### 为什么两条判据必须分开

"在账本里"和"已经落盘"是**两件事**,合成一条就等于没改:活账本里有、落盘快照里没有,恰恰是
最危险的一刻 —— 此时销账,崩溃即永久丢失。所以:

- `HasApplied(实体上的活账本)` → **别再发一次奖**;
- `IsSettlementDurable(落盘快照里的账本)` → **可以销账**。

#### 为什么锁要留到落盘(实现评审 wne803bj5 的主结论)

原先代码把"锁不在"当作"这一局已作废"的证据,而应用当下就删锁、早于落盘。于是"锁不在 + 账本里没有"
既可能是真作废,也可能是"应用了、没落盘就崩了/被废黜" —— 后者会被误判作废,唯一的 pending 被删,
奖励永久丢失。同一个根还带出两个问题:

- 玩家能在上一局 pending 还没销账时开下一场,battle 对下一局的**无条件 SET 覆盖掉上一局唯一的 pending**;
- 重登时登录钩子读到旧 pending,走了"补应用"分支而**漏掉下一场的冻结重建**,玩家在战斗中被解冻。

让锁一直留到落盘,这三件事一起消失:锁在,match 就不放下一场;重投窗口内"锁不在"只剩"已作废/已销账"两种含义。

#### 顺带修掉的真缺陷

1. **销账绕过点**:应用成功后紧接着删锁 + 摘 `InBattleComp`,发件箱 10s 后的第一次重投必然落进
   "无 `InBattleComp` 且锁不在"那一支 —— 那里**不经过** `ApplySettlementToEntity`,改造前会当场销账。
   收归单一入口后这些分支一并受持久判据保护。
2. **冻结闸只看 gold/items**:一笔"只有 HP + 宝宝 + 击杀事实"的 PVE 结算会在冻结期穿过闸门。
   改为整笔判据 `IsSettlementApplicable`,并补上 `PlayerTravelHandoffComp` / `UnregisterPlayer`
   (这两个窗口里 `SavePlayerToRedis` 开头直接跳过写盘)。刻意**不**含资产通道的 `HasFencedOwnership`,理由写在函数注释里。
3. **离线分支在锁 TTL 过期后删掉合法 pending**(既有):锁 TTL 是 deadline+60s,离线玩家的结算常常晚于它到达。
   这与在线分支注释里"按锁 gating 会把合法离线结算判掉"的判断自相矛盾。现在离线分支"锁不在"一律不销账,留给登录钩子。
4. **一场结算连存两次盘**:应用时压一次,紧接着的销账延后又压一次(脏比较对的是上次**落地**的快照,拦不住)。

#### 残留与代价(已知、刻意接受)

- **落盘确认前不能排下一场**:正常是一次 Redis 往返(快路径);存盘排在同玩家另一笔在途存盘之后、或写失败进重试时,
  退到发件箱 10s / reaper 30s。battle-smoke 若在战斗结束后**立刻**重新排队,可能偶发被锁挡一次。
- **离线玩家被判废的局会在登录时补发**:离线分支不再以"锁不在"判废,而登录钩子本来就不看锁。能判废 FIGHTING 的只有
  reaper 且它要实体在场,所以只有"退出存盘在途的实体恰好被 reaper 判废、随后迟到的结算"会走到这里,属于既有语义的一致化。
- **reaper 判废 FIGHTING 没有宽限**(既有,未改):`deadline_ms < now` 即判废,恰在 deadline 结算的超时平局可能输给一次
  reaper tick,随后被当作作废丢弃。概率约为"结算传输耗时 / 30s"。
- **崩溃 + 停机超过锁的续期(180s)**:此后若有重投恰好在登录钩子读 pending 之前打到新实体,"锁不在 + 账本无"仍会被判废。
  发件箱 120s 窗口已过,只剩登录钩子这一条路,它不看锁,所以实际只在"重登恰好撞上最后一次重投"时发生。
- **pending 每玩家单槽**(`battle:settlement:pending:{player_id}`,无条件 SET):锁留到落盘后,下一局在上一局销账前开不了,
  单槽覆盖只剩"锁自然过期"一种来路。彻底修法是 pending 改成按 (player, battle) 一条,要动两端 key 契约,未做。
- **"落盘"指写进 Redis,不是 MySQL**:与全仓既有口径一致(Redis 是在线权威存储,MySQL 在其下游)。
- **回档会把账本退回旧值**,而 pending 不参与回档、活 7 天 → 已发的奖可能被重发。但回档同时把资产也退回了,
  重发与回档语义自洽,不额外加机制。
- **账本容量 `kMaxAppliedRecords = 64` 是异常兜底不是常规容量**:条目销账一确认就摘,要堆到 64 意味着连续 64 局
  条件删全失败(Redis 已不可用)。溢出淘汰最旧项并打 `metric=battle_settlement_ledger_evicted`。

#### 设计过程(留痕)

1. 第一版设计挂 `HandlePlayerAsyncSaved` 回调 + 一个独立"待确认集合",被设计评审(`wvo7qkttt`)判 **broken**,
   4 个 blocker。第二版改为只认落盘快照字节,不需要新挂点,`player_lifecycle.cpp` 一行未动。
2. 第二版落码后做了**实现评审**(`wne803bj5`,5 个维度 × 每条 2 名反驳者 + 完整性批评者):9 条全部被确认、0 条被驳回,
   编译维度 0 条。主结论就是上面的"锁留到落盘";其余为双存盘、测试一处必挂(物品版重复应用仍断言 false)、
   重登用例没带账本(测的不是它声称的东西)、销账规则零覆盖、加列无强制、robot vendor 未同步、帮会交接手册会把新列当异常。
   全部已修。
3. 对"锁留到落盘"这轮修改再做**第二轮实现评审**(`w0zw729x1`,编译 / 锁生命周期 / 销账时序 / 单测 4 个维度):
   编译与单测维度 0 条;确认 3 个问题 —— 离线分支读锁与写 pending 不原子(锁留到落盘后会把已销账的 pending 写回、
   造成重复发奖,major)、pending 解析失败分支按锁重建会冻进已结束的战斗、登录时"缓存判重复但账本无"会丢掉唯一一次投递。
   全部已修(见上表"离线暂存原子化""缓存让位于账本"两行与"按锁重建前查账本"一行的补充)。

#### 验证清单(全部未执行)

1. **MySQL 加列必须先跑**:`cd go/db && go run ./cmd/migrate -f etc/db.yaml -command plan`。计划里允许出现的
   `player_database` 加列只有 `profile_component` / `asset_op_ledger` / `settlement_ledger` / `fence_owner_epoch` /
   `fence_save_seq` / `inbox_ledger` 六条,另允许 `player_database_1` 加 `fence_owner_epoch` / `fence_save_seq` 两条
   (2026-10-15 起:`fence_*` 属 MySQL 行内围栏 [db-write-fencing-mysql.md](./db-write-fencing-mysql.md) §3.2,
   `inbox_ledger` 属玩家收件箱 [player-inbox-service.md](./player-inbox-service.md) §4.4;与
   [guild-phase2/90-consistency.md](./guild-phase2/90-consistency.md) G-03 同一口径;已迁移过的不会再出现,**也可能为空**),
   其余语句先停下核对。确认后 `-command up`(不加 `-allow-modify`)。**退出码 4 = 迁移已成功、只是有 NEEDS-REVIEW**
   (本地库上已知的 `user_oauth.provider_id` / `user_phone.phone` 类型漂移会一直触发),不是失败;1 / 3 才是失败。
   本地多 zone 时对启动器实际用的每份 db 配置各跑一次;随后每个 zone 库
   `SHOW COLUMNS FROM player_database LIKE 'settlement_ledger'` 返回 1 行;其余新列照 G-03 逐列各查一次,各 1 行。
   **漏这一步,新编的 go/db 会拒启并打印这条命令**(改造前是静默地让全服玩家回写全部失败)。
   本地启动器(`go_services.ps1` / `start_game.ps1`)和 K8s 都**不会**替你跑迁移,清过 MySQL 的环境同样要先跑一次。
2. 编译 go/db(`go build ./...`、`go vet ./...`),编译 scene 相关工程与 `bag_test`(MSBuild 串行 `/m:1`)。
3. `bag_test` 全绿,重点看本节的用例:账本登记 / 重启后不重复发 / 冻结与交接整笔延后 / 三条纯规则 /
   缓存命中但账本缺失只补发一次 / 销账在未落盘时延后、落盘后才销账;以及契约变更后更新过的
   `DuplicateSuccessfulCallbacksAndReentryPayAndProgressOnce`、`RepeatedApplyDoesNotDoubleConsumeOrDoubleDrop`、
   `ReloginOldPendingDoesNotPayAgainOrRemoveNewBattle`。
4. `battle-smoke` 端到端:打一场有掉落的战斗 → 日志依次出现"结算已应用"、"销账延后"、随后很快(快路径)销账;
   `battle:settlement:pending:{id}` 与 `battle:lock:{id}` 在同一刻消失;每场结算只出现一次结算存盘(不应连存两次);
   战斗结束后立刻重新排队应在一次往返后放行;组队跟随在删锁后恢复。
5. 杀进程验两个窗口:应用之后、存盘之前 kill scene → 重启 → 登录应重发一次(不丢);人为让销账 EVAL 失败 + 重启 →
   登录**不得**重复发(不复制)。再加一条:应用后 kill,**不登录**,等发件箱把剩余重投打完 → pending 仍在,之后登录补发。
6. 重登冻结:玩家 A 打完 X、销账前开了 Y(可人为让 X 的锁过期)再断线重登 → 应重建 Y 的冻结并重绑 gate。

### 9.2 仍然没做 → 09-28 已有一次真实编译运行(09-29 补记)

编译、18 个新单测、`battle_smoke` —— 用户 09-18 明确跳过。**09-28 03:41 属性数值线代 Codex 做过一次
真实编译 + 单测 + 冒烟**(证据在本机 `run/verify-attribute-20260928/`,见 PROGRESS「防御 ×12 / 法力 ×4
联机验收通过」一节):`game.sln`、`turn_battle_engine_test`、`bag_test` 均编译通过,`BATTLE_SMOKE_OK`。
那次跑的是 §9.3 第一版(账本 v1,锁留到落盘之前)的代码,与本节 G1-G9 相关的结果:

| 用例 | 结果 | 处理 |
|---|---|---|
| G1-G9 引擎新用例(含掉落、PVP 道具上限、快照 buff 剔除) | 通过 | — |
| §9.3 第一版结算/账本用例 16 条 | 通过 | — |
| `PlayerBattleSettlementItemTest.RepeatedApplyDoesNotDoubleConsumeOrDoubleDrop` | 失败 | 实现评审 wne803bj5 已预言并修正(账本命中改为返回 true + alreadyApplied) |
| `TurnBattleEngineTest.SilenceBlocksGeneralSkillButAllowsBasicAttack` | 失败 | **是 G 系列 D49 弄挂的**(验收记录里被归为"他人在途",不对):用例从战前快照带入沉默,而 D49 规定快照里的控制类 buff 一律剔除。09-29 改为开战后经 `TurnBattleEngineDeathTestAccess::AddBuff` 在局内挂沉默,用例意图(沉默挡技能、不挡普攻)不变 |
| `BagRemoveByGuidTest.*` 6 条 | 失败 | 不是本线:聚宝斋 P2 / 帮会 B4 的按 guid 扣物用例,入包那一步 `AddItem` 就返回 `kBagAddItemInvalidParam`(6004)。该测试 09-18 才加,写的时候已含本线 09-17 的背包改动,之后 `bag_system` 未再改,本线新增的 3 个背包用例同批通过。留给资产通道那条线 |

**仍未验证的**:§9.3 的"锁留到落盘"及之后两轮修订(09-28 04:40 以后的代码)还没被编译过;
`battle_smoke` 通过的那次也是旧版结算逻辑。按 §9.3 验证清单重跑。

## 10. 2026-11-01 战斗锁写入可靠性;恢复分支的去向(已落码,未编译,待 Codex 验证)

### 10.1 来龙去脉

2026-10-22 到 10-30,会话 `local_f39089db` 在 worktree 里做了一批结算改动(锁不当销账证据、待结算记录每局一条 + 墓碑 +
`{player_id}` 键),当时写在本文件 §10、§11,没有提交,归档时随 worktree 删除。10-30 按会话记录恢复到
`claude/recover-battle-settlement-f39089db`(`b5131475`,基点 `940ef974`),Codex 接到 `codex/battle-settlement-integration`
(基点 `72017963`,工作区未提交),编译和结算、锁契约用例都跑过。

同期 main 走了另一条路:结算改成只走 match SQL + player_inbox,`settlement_outbox.h`、结算账本、`battle:settlement:pending`、
两个发件箱全部删除(`65c69f4a9`)。

**结论:这两条分支不再合回 main。** 合回去会把已删除的整套设计带回来;其中「按 battle_id 升序逐局应用」也已经是错的,
发号器的 battle_id 不单调(AGENTS §7 第 1 条)。锁可靠性那部分按当前 main 重写,落在本节。

### 10.2 原决策的去向

| 恢复分支的决策 | main 现状 |
|---|---|
| 锁不是销账证据;判作废的局迟到照发;在线无组件补读补发(cross-service-assets#1) | **已消除**:Redis 里没有结算原件。`ApplyInboxSettlement` 不读锁、不看冻结,`CompleteInboxSettlement` 组件不在也能收尾 |
| 每局一条 HASH、墓碑、发件箱补落库、积压索引、离线不写、登录 HGETALL(K2–K8) | **已取代**:收件箱每局一条,幂等靠永久域水位,提交点是存档 SQL |
| 键带 `{player_id}`,Go 同批(K1、K11) | **已有等价**:`battle:lock:{pid}`、`battle:ctx:{pid}`,`go/shared/redisslot` 的金样测试解析 C++ 头文件逐字比对 |
| reaper 判 FIGHTING 的宽限 | **已有**:`FreezeExpiresAtMs`;match 侧 `terminalOutcomeGraceMs` |
| 回档 / 合区对待结算记录与墓碑的处置(当时待拍板) | **问题不存在了**:这一族状态已删除 |
| 写锁与 ctx 原子、续期只延长、写锁 / 续锁有界重试、重复确认续期(负责人 10-22 拍板 (b)(c))、续期去重(K9)、重建复核首发才撤销(K10) | **main 没有,本节落码** |

### 10.3 结算走收件箱之后为什么还要修

锁不再影响发奖,但还有两个用途:match 读它拦排队;实体没了时 scene 靠它重建冻结。改动前的 main:

- `PrepareBattle` 发两条 SET,没问到只打日志,注释还写着「不影响正确性」;
- 确认续期只有一次机会,已 FIGHTING 的重复确认直接忽略。没续上时锁停在「备战期限 + 60 秒」,而战斗时限默认 300 秒。

叠加「玩家战斗中完整下线再登录」:读不到锁,新实体不冻结,资产通道的战斗闸放开 —— 正是 `player_battle.h`
`FreezeExpiresAtMs` 说明的「可逐局刷的有界资产复制」窗口;match 也可能放进第二场。

### 10.4 修法

| 文件 | 改动 |
|---|---|
| `cpp/libs/services/scene/battle/system/battle_lock_rules.h` | `kWriteBattleLockScript`(一条 Lua 写两键)、`kRenewBattleLockIfMatchScript`(锁值匹配才动、只延长、不补写);`IsLockWriteAnswered`、`NextLockWriteRetryDelayMs`(最多 6 次含首发,单调时钟 30 秒预算,退避 500 ms 起封顶 8 秒,按 player_id ^ battle_id 确定抖动);`ReplyShapeName` |
| `battle_lock_reply_shape.h` | `ReplyText`(只进日志) |
| `player_battle.cpp` | `WriteBattleLock`(重发前确认 InBattleComp 仍是这一局);`ConfirmBattleLockIfMatch` 与 `ConfirmRebuiltFreeze` 带有界重试;`ConfirmBattle` 已 FIGHTING 时顺带续期;续期链按 (player_id, battle_id) 去重;`ResolveRebuiltFreezeRecheck`(只有首发回 0 才撤销重建) |
| `player_battle.h` | 契约注释更正;测试注入点 `SetLockScriptObserverForTest`、`SetLockRetrySchedulerForTest` |
| `cpp/tests/bag_test/player_battle_settlement_test.cpp` | 接线用例 8 条,`BattleLockContract.*` 4 条(2 条脚本文本 + 2 条 `MMORPG_TEST_REDIS_ADDR` 门控的真实 Redis) |
| `cpp/tests/routing_identity_test/routing_identity_test.cpp` | `BattleLockWriteRetry.*` 4 条、`BattleLockReplyShape.ReplyTextComesOnlyFromTextReplies` |

### 10.5 不变量

1. 锁与 ctx 同生共死:同一条 Lua 写入、续期、删除,同 TTL。
2. 续期只延长,锁值匹配才动,锁不在不补写:一条迟到的确认不能复活被取消 / 结算 / 判废删掉的锁。
3. 写锁重发前必须确认实体上的 InBattleComp 仍是这一局;摘组件与条件删锁在同一个 loop 回合里,所以门禁过了就不会复活已删的锁。
4. 只有脚本的整数应答是结论(0 = 锁不在或已易主,不重试);空应答、ERROR、命令没发出去都是「没问到」。
5. 重试有界,闭包只捕获 id 与字节,不捕获实体句柄或 this(AGENTS §11.7)。

### 10.6 残留与代价

- **重试放弃**(Redis 中断超过 30 秒,`metric=battle_lock_write_gave_up`):续期放弃后,180 秒确认补发窗口内的下一条确认会重新起链。
  写锁放弃后锁一直缺到战斗结束,因为续期不补写;本实体的 InBattleComp 仍挡第二场,完整重登重建不出冻结。结算不受影响。
- **与 Java 不同**:Java 写锁失败即备战失败。C++ 的 `PrepareBattle` 同步应答、不等 Redis,所以照常放行、靠重试兜底。
- **重复确认的 Redis 开销**:在线的 FIGHTING 玩家每条周期补发都发一条续期 EVAL,每局每人最多 18 条(180 秒窗口、每 10 秒一次),
  键带 `{player_id}`,随玩家域分片水平扩展。Java 用 `lockExtended` 标记,续期成功后就不再发。C++ 没有预先加这个标记:
  续期幂等、状态更少,而且还没有压测数据说明它值得多一份状态。压测若显示它占比高,再加同样的标记。
- **离线确认读 ctx 不重试**:靠 battle 的确认周期补发;Redis 中断超过补发窗口时,离线玩家的锁停在备战期 TTL。
- **删锁仍是 fire-and-forget**(既有):结算确认后锁没删掉时,排队闸多挡到 TTL,重登会按锁短暂重建冻结,由收件箱重放或 reaper 收尾。
  只影响活性,不影响数据。

### 10.7 验证(全部未执行,待 Codex)

本批没有动 proto,不需要协议生成。命令见 `docs/PROGRESS.md` 2026-11-01 同名条目。

Java 版:`PARITY.md`「战斗锁不再是销账证据」一行已更正(Java 待做);Java 备战写锁失败即失败、重复确认续期(D9)已有等价。


### 2026-11-05 战斗锁写入与续期的协程接续

`RunLockWrites` 将写入、条件续期及重建复核的一条重试链顺序化为门禁、等待 Lua 结果、等待原定时退避。脚本、TTL 计算、最多次数、单调时钟预算、抖动和整数结论规则沿用原实现。未接纳、ERROR、空回复均按原“没问到”处理，Lua 整数 0 不重试。

每次重发前重新取得玩家和战斗状态，等待期间只留 ID、自有上下文与回复。离线续期沿用首发上下文，在线续期重算期限。续期去重表仍存在，由任务局部 RAII 在成功、放弃、停止时统一摘除。同场景后续跟随、冻结与收件箱结算规则不变。

任务由 Scene 持久作用域持有（`TryStartDurable`），维护 P1、普通停机和身份冲突时不随瞬态任务停止。这些窗口里备战（gRPC 保留到 P3）和 battle 的确认补发（Kafka）仍会到达，改造前的回调链也照常写锁和续期；这时停写，锁会停在“备战期限 + 60 秒”、ctx 停在 PREPARING，玩家在别的节点重登就重建不出冻结。只有拆除前的 `StopAll` 结束本地等待，之后迟到回复和定时器不再恢复业务。取消不撤回已发送 Lua，也不影响锁的现有 TTL。2026-11-10 订正：11-05 的实现把三条链放进了瞬态作用域，一进入停机就停写，现已改回持久作用域，回归用例为 `BattleLockWritesKeepRunningAfterGameplayStopUntilStopAll`。Java 写锁失败即拒绝备战、C++ 同步备战后异步补写的既有差异仍在，本次不改变可观察业务行为。

## 11. 2026-11-06 号段预检续段;FIGHTING 冻结到期先查 SQL 终局(已落码,未编译,待 Codex 验证)

来源:数据覆写审计 R2 的 r2-battle#3 与 r2-v1#1 残余窗口(`data-overwrite-rollback-audit-r2-20261019.md`)。两条都按当前代码复核成立。

### 11.1 号段预检只读余量,不足时没人续段(r2-battle#3)

**根因。** `GuidSegmentClient` 只在发号之后判预取阈值(`TryNext` → `MaybePrefetch`),两段都空时才因发号失败触发续段。
战斗结算(`player_battle.cpp` 的 `HasItemGuidsForDrops`)和资产通道入账(`asset_op_system.cpp` 的 `ApplyCredit` 第 (b) 步)
先用 `Available()` 整批预检,不够就整笔 DEFER / RETRY、一个号都不发。「需要量 > 剩余 > 预取阈值、又没有下一段」时谁都不发号,
预取永远不触发:v1 结算一直 DEFER,玩家的票停在 state 3,进不了匹配也不能过户。资产通道还按 Σcount 算需要量,
一批可叠加物被当成逐件铸号,上限 4096 件时可以超过双 buffer 的容量,永远 RETRY。

**修法。**

| 位置 | 改动 |
|---|---|
| `cpp/libs/modules/id_segment/guid_segment_client.{h,cpp}` | 新增 `RequestTopUp(needed)`,按序判:未启用 / 停机回 `kUnavailable`;够了回 `kEnough`;`needed` 大于 `maxStep` 回 `kExceedsMaxStep`(终局:一段最多 `maxStep` 个号,只靠续段永远凑不齐;不记需求、不发请求、不作废号);下一段已在手里而两段合计仍不够时腾出一个 buffer —— 当前段剩下的号作废(从未发出,发号器不会再发,不会撞号;计入 `topUpAbandonedIds`),下一段换成当前段;然后记下需求,续段那一次的 step 抬到 `max(当前 step, needed)`(不超过 `maxStep`,动态 step 本身不变),没有在途、不在退避就立刻领一段(与预取同一条单飞路径),回 `kFetching`。需求(连同抬高的 step)在下一次校验通过的段落下时清掉。退避到期的重试见到需求照发、也按抬高的 step 领,不再因为「剩余高于阈值」跳过。`Stats` 加 `topUpFetches` / `topUpPending` / `topUpStep` / `topUpAbandonedIds` |
| `player_battle.cpp` `HasItemGuidsForDrops` | 判出不足先 `RequestTopUp` 再整笔 DEFER;`kExceedsMaxStep` / `kUnavailable` 打 ERROR(要人看 IdSegments 的 `MaxStep` / `Enabled`),**照样只 DEFER**:结算不能拒收,奖励与消耗都在这一笔里,原件留在收件箱,改配置后自愈 |
| `asset_op_system.cpp` `ApplyCredit` (b) | 需要量改用 `BagService::MaxNewInstancesFor`(与战斗结算同口径),判出不足先 `RequestTopUp` 再回 RETRY;`kExceedsMaxStep` 按第 8 步包校验同一口径记终局拒绝(REJECTED + `kAssetInvalidBundle`,`metric=asset_credit_exceeds_guid_segment_max_step`),结局随账本固定、同一 seq 只读回放;Go 侧(`shared/assetop` 的 Decide → FinalStatus)按 REJECTED 终结做对侧账:帮会退次数 / 帮贡 / 限购,邮件把附件退回未领取,交易记 REJECTED 终态;上游要发就拆成更小的批次 |

**评审修正(2026-11-06)。** 原先「下一段已在手里还不够」回 `kBuffersFull`、调用方一直等:空闲节点的动态 step 会缩到 `MinStep`(1000),
预取过一次之后的常态正是「当前段剩不多、下一段一整段」,资产通道单笔最多 4096 件不可叠加物,这种批次永远 RETRY。改成上面的腾 buffer
加抬 step 之后,`needed ≤ maxStep` 的批次一次续段就够;只有比一段最多能给的号还多(生产 `MaxStep` 1,000,000,只会是配置写错)才是终局。
选「资产通道记终局拒绝、战斗结算只延后」的理由:资产通道对确定性失败一律记 REJECTED 免得 Go 无限重投同一个包(第 8 步),上游已有
REJECTED 的处置路径;战斗结算没有可退的对侧,拒收就是丢奖励或白喝药,只能留着原件等配置修好。

**代价。** 有需求时会比阈值预取早领一段、可能比当前 step 大(号段本来允许浪费,一段最多 `maxStep` 个号);腾 buffer 时作废当前段剩下的号
(批次大于两段合计时才发生,生产 step 下极少)。

### 11.2 FIGHTING 冻结到期先查 SQL 终局(r2-v1#1 残余窗口)

**根因。** 冻结到期 = 截止 + 60s 终局宽限 + 60s 投递余量(`FreezeExpiresAtMs`,2026-10-30)。但 COMPLETE 不受截止约束,
match 的宽限只推迟 ABORT(`completion.go` 的 `terminalOutcomeGraceMs`);发件箱每 2 秒一轮、每副本每轮首投 4 行,失败等租约到期才重投,
收件箱拉取还有退避。积压时结算可能晚于截止 + 120s 才到。那时已解冻,资产通道的战斗闸只看 `InBattleComp`,
消耗按当时持有量夹紧 —— 交易或帮会资产通道开启时可逐局刷「药白喝」。

**修法。** reaper 判出 FIGHTING 到期(`nowMs > FreezeExpiresAtMs`)后不再直接解冻,交给 `ResolveExpiredFightingFreeze`:

1. 这一局的结算已在本实体应用过(收件箱战斗域最近结局环里有它的 APPLIED,`HasRecentAppliedBattleSettlement`)→ 照常解冻,不查。
   典型场景:结算提交之后删锁没落地,玩家重登按锁重建了冻结。
   **环里的 REJECTED / SKIPPED 不算收尾(2026-11-06 评审核实)**:战斗结算域里这两种都不是终局。收件箱 Ack 对这个域只收 APPLIED,
   报 REJECTED / SKIPPED 回 CONFLICT、行不变;`VoidItem` 对这个域一律拒绝;Match 只凭 APPLIED 收据放票(runbook §6.1)。scene 的执行器
   从不回 `kRejected`(坏载荷也只 DEFER 或按版本错位停用该域),条目带指令或回放到非 APPLIED 的原结局时状态机直接停用该域。
   环里真出现这两种只能是旧版本或缺陷留下的异常,唯一出口是修好之后让 scene 把原件执行成功,结算之后仍可能到达,
   所以照旧查终局、续冻,到上限告警交人工(用例 `RejectedOrSkippedInTheBattleRingIsNotTerminalAndStillQueries`)。
2. 已越过续冻上限(`nowMs > FreezeHoldDeadlineMs`)→ 告警 `metric=battle_freeze_hold_cap_reached`(ERROR,`terminal=not_queried_past_cap`)后
   照常解冻,不再查询、也不等在途的查询:越过上限之后的处置只有解冻一种(评审修正 2026-11-06,原先越过上限那一拍照样发查询、等回答)。
3. 同一局已有查询在途 → 这一拍不动。按**单调钟**在途超过一拍加一次调用期限(36 秒)仍没回 → 当作丢失(`metric=battle_terminal_query_lost`),
   按未知处置。在途时长是进程内耗时(AGENTS §11.3),墙钟只用于跨进程的截止时刻(deadline、续冻上限);原先按墙钟差判,
   墙钟前跳会把正常在途的查询误判丢失,回拨会让丢失的查询一直不判。
4. 否则向 match 发一次只读查询 `MatchInternal.GetBattleTerminalDecision`,按 `battle_freeze_hold_rules.h` 的判定表处置:

| SQL 终局 | 处置 |
|---|---|
| ABORT | 照常解冻:记作废局 → 条件删锁 → 摘冻结(`metric=battle_freeze_expired terminal=ABORT`) |
| NONE,且这一局没有可裁决的 SQL 原件(match 回 `terminal_expected=false`) | 照常解冻(`terminal=NO_ORIGINAL`):COMPLETE 永远落不了库,不能被 NONE 拖满上限 |
| NONE,但有可裁决的 v1 原件(`terminal_expected=true`) | 按「结果未知」续冻一拍,锁跟着续(`metric=battle_freeze_held terminal=PENDING`) |
| COMPLETE(本地还没应用) | 续冻一拍,锁跟着续(`metric=battle_freeze_held terminal=COMPLETE`) |
| 未知:没发出去 / 传输失败 / 回包验签不过 / match 带内 retry / 取值不认识 | 同样续冻一拍(`metric=battle_freeze_held terminal=UNKNOWN`) |
| 续冻到 `FreezeHoldDeadlineMs` = FreezeExpiresAtMs + 15 分钟之后仍是 COMPLETE、PENDING 或未知 | 越过上限的那一拍(第 2 步)告警 `metric=battle_freeze_hold_cap_reached`(ERROR)后照常解冻,不再查询 |

**NONE 为什么分两种(2026-11-06 负责人定)。** 冻结到期 = 截止 + 60s 宽限 + 60s 余量,晚于 match 补写 ABORT 的时刻(截止 + 60s,
`ready_recovery` 的到期恢复)。有原件的局到期还是 NONE,只说明 ABORT 还没写成(match 不可用或恢复任务滞后),而 COMPLETE 不判截止、
仍可能先落库,所以 NONE 不是终局证据,按未知续冻;「解冻之后 COMPLETE 才落库」因此只剩续冻上限之后的一段,归入有告警的那条残余。
没有可裁决原件的局另当别论:COMPLETE 与截止 ABORT 两条写终局的路径都要求原件可裁决(`authority.TerminalOutcomeExpected`,
与 `supportedOutcomeOriginal` 同一判据),这种局永远不会有结算。核实结果:match 现在总是装配 SQL 成局权威(没有本地模式),
所有备战都来自 match 的 gather,新成局只经 `SaveSupportedIntent` 落 v1 原件;没有可裁决原件的只剩上线前的协议 0 旧成局、
battle 明确采用旧协议的成局、原件坏形状的成局,以及锁值对不上任何成局行的情形。它们也会走到这次查询,由 match 回
`terminal_expected=false`,scene 照常解冻。

**续冻上限 15 分钟**(`PlayerBattleSystem::kFreezeHoldCapMs`):盖住 match 个人结算两到三轮失败重投(退避封顶 5 分钟,`pendingSettlementDelayMs`)
加收件箱拉取退避(封顶 60 秒)。结算真的到不了时(收件箱永久拒收、match 长期不可达)不能无限冻人。

**锁 TTL 从开局起就覆盖续冻上限(2026-11-06 负责人定)。** `FightingLockTtlSecFor` 一律按 FreezeHoldDeadlineMs + 60s 算,
不分到期之前还是续冻之中:开局确认、续冻一拍、重登或迟到确认按锁重建时的复核都走它。玩家在冻结到期之前下线、超过到期点才重登,
锁还在,登录按锁重建冻结,到期处理照常查终局;match 也不会提前放下一场。代价:结算一直不来的局,锁最多多留约 15 分钟,
这段时间 match 按 `battle:lock` 拒绝排队,有界;正常结算、取消、到期解冻都照旧按条件删锁,不受影响。
解冻一定早于锁过期(评审修正 2026-11-06,含查询丢失的路径):越过上限的那一拍直接解冻、不等在途查询,所以任何路径的解冻都不晚于
越过上限后的第一拍,即上限之后至多一拍(30s);锁到期 = 上限 + 60s,写锁时 EX 秒级下取整最多早到 1 秒。30s + 1s 小于 60s(`static_assert`)。
原先越过上限那一拍照样发查询,丢失路径要再等两拍才判丢失,解冻最晚可到上限 + 90s,晚于锁过期。

**ctx 缺失时按锁 TTL 反推期限(评审修正 2026-11-06,`PlayerBattleSystem::InferFightingDeadlineFromLockTtlMs`)。** 锁与 ctx 同一条 Lua 写入,
ctx 缺失或不可信只在数据损坏时出现。锁到期 = deadline + 终局宽限 + 投递余量 + 续冻上限 + 锁余量;原先按「现在 + 剩余 TTL」反推,
TTL 里含 15 分钟续冻上限,首次查询晚约 18 分钟。现在剩余 TTL 大于「续冻上限 + 锁余量」(960 秒)时,这把锁一定是 FIGHTING 的、原到期时刻还在后面
(备战期的锁 TTL 只有备战期限 + 60 秒,备战期限受 match 工作租约约束,远短于 15 分钟),扣掉四项后首次查询落在原来的到期时刻,
另加 1.5 秒盖住 EX 下取整与 TTL 读数四舍五入,只会偏晚、不超过两秒。剩余 TTL 不超过 960 秒时分不清是备战期的锁还是续冻后段的 FIGHTING 锁,
照旧取「现在 + TTL」:按 FIGHTING 精确反推会把备战期的锁当场判到期,备战期 match 还没落 v1 原件,查询回 NO_ORIGINAL 就会解冻、删锁,
这一局随后照常开打时冻结与锁都已不在。代价:续冻后段才重登且 ctx 损坏的局,首次查询仍会推迟(最多约 18 分钟),只影响活性。

**新 RPC `MatchInternal.GetBattleTerminalDecision`**(`proto/match/match_internal.proto` 只追加;`go/match/internal/logic/battle_terminal_decision.go`):

- 只读 `match_battle_completion`(终局唯一权威,与 `readTerminal` 同一条 SQL),还没有决定时再读成局行判原件可不可裁决
  (`authority.Store.BattleTerminalState`,回包 `terminal_expected`);不读发件箱投影,不加锁、不写任何行;
- 鉴权与回包签名沿用 `CommitBattleOutcome`:请求由固定调用方 `scene` 签,用场景密钥 `MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE`
  (与 `TeamAuthority.ReadMembership` 同一把,match 已装配,部署不加新密钥;签名带方法名,两条方法之间不能互相重放);
  回包由 `match` 以方法 + `/reply` 签,`request_digest` 绑定请求原件;
- 读失败、非法持久值带内 `retry = true`,不回 gRPC 错误:只读查询没有结局未知,回 `Unavailable` 会被 `PublicationAdmission`
  记成未知结局、拖住排空证明。拒绝只用 `PermissionDenied` / `InvalidArgument`(确定拒绝);
- 列入 `PublicationAdmission` 的 RELAY 方法(收尾已开始的对局,排空期间照常应答),请求体不进 zrpc 日志;
- 不复用 `GetBattleCompletion`:那条只放行 guild、回包不签名,还带最多 960KiB 的原字节。

scene 侧客户端在 `battle_terminal_query.{h,cpp}`:随机挑一个就绪的 match 节点(scene 的 `CanConnectNodeTypeList` 加了全局池的
`MatchNodeService`),单次期限 `kTerminalQueryDeadlineMs` = 6000;回包归一化 `ClassifyReply` 与单测走同一个函数。

**不变量。**

1. 只有「本地已应用(收件箱环里的 APPLIED)」「ABORT」「没有可裁决原件」「越过上限」四种情形解冻 FIGHTING 冻结
   (另有组件缺身份的兜底);COMPLETE 未应用、有原件的 NONE 与未知都续冻,环里的 REJECTED / SKIPPED 也按「结算还会来」续冻;
   资产通道在续冻期间回 RETRY。FIGHTING 锁活到续冻上限 + 60s,解冻不晚于越过上限后的第一拍,冻结活着的时候锁一定在。
2. 续冻有界:判上限用这一拍的墙钟时刻(上限锚定在本局 deadline 上,与重登、换节点无关);判查询丢失用单调钟;
   丢一次回调不会把玩家永远冻住,越过上限的那一拍不等任何查询。
3. 回答到达时按 player_id 现取实体,这一局的冻结还在才动手;「是不是 FIGHTING」与 reaper、`LockTtlSecOf` 同一口径(不是 PREPARING 就按 FIGHTING,
   state 缺失的组件也算;评审修正:只认 FIGHTING 会让这种冻结每拍都查、回答被丢、永远解不了冻);回调只捕获 id 与时刻,不捕获实体句柄(AGENTS §11.7)。
4. 查询只读,多副本并发安全:任一 match 副本读同一份 SQL;每个玩家的冻结只由持有它的 scene 节点处置(单写者)。

**指标。** scene 进程内计数 `PlayerBattleSystem::FreezeHoldEventCount`(held_complete / held_pending / held_unknown / cap_reached / query_lost),
告警走同名结构化日志。值班固定查询(Loki,写法同 `deploy/k8s/asset-channel-alerts.yaml` 的说明):

- warning:`count_over_time({job="cpp_nodes", service="scene"} |= "metric=battle_freeze_hold_cap_reached" [10m]) > 0`
  —— 续冻到上限结算仍没到,按 battle_id 查 match 的 `match_battle_settlement_source` 与收件箱条目;
- warning:`count_over_time({job="cpp_nodes", service="scene"} |= "metric=battle_terminal_query_unknown" [10m]) > 20`
  —— scene 查不到 match 终局(密钥、发现或 match 不可用),续冻会一直拖到上限。

上面两条在 scene 一侧,都是结算已经晚了才报。积压源头在 match 的终局发件箱,另有两条 Prometheus 规则
`deploy/k8s/match-outcome-outbox-alerts.yaml`(2026-11-06),在玩家到续冻上限之前报出。两条都看
`max(match_outcome_outbox_oldest_pending_age_seconds{kind="first"})`,即最老一条还没进收件箱的个人结算从 COMPLETE 提交起等了多久。
这个读数每个 match 副本每分钟读一次 SQL。

| 规则 | 条件 | 阈值依据 |
|---|---|---|
| `MatchSettlementInboxLagging`(warning) | > 60s,持续 2m | 等于收件箱投递余量 `kInboxSettlementDeliveryMarginMs`。打满时限的局截止后才 COMPLETE,冻结在截止后 120s 到期,收件箱拉取退避封顶 60s,所以首投要在提交后 60s 内完成;再晚,到期时就开始续冻 |
| `MatchSettlementInboxStalled`(critical) | > 300s,持续 1m | 打满时限的局约在 COMPLETE 之后 16 分钟到续冻上限(`kFreezeHoldCapMs` 15 分钟,从冻结到期算)。本条约在落后 6~8 分钟时触发,给改参数滚动或扩副本留出约 8~10 分钟 |

契约测试 `k8s_deploy_contract.tests.ps1` 的 match-outcome-outbox-alerts 用例从 `player_battle.h` 读这两个常量,要求两个阈值都满足:

- warning 阈值不超过投递余量;
- critical 阈值离续冻上限至少 10 分钟。

所以改 scene 的续冻上限或投递余量时,告警阈值必须同步改,否则契约测试会红。

为什么只看 `kind="first"`:已入箱、等应用的 `waiting` 里有长期离线的玩家,年龄不代表积压。

已知局限:

- 某个副本读 SQL 失败时,它的 gauge 保留上一次读数,max 会偏高(宁可多报)。
- match 整体不可抓取时这两条不报,由抓取存活告警兜底。
- COMPLETE 本身晚到的局(截止 + 60s 之后才落库),玩家续冻的时间主要花在 COMPLETE 之前。发件箱年龄从 COMPLETE 才开始算,
  这两条规则反映不出这段时间,只能看 scene 一侧的 `battle_freeze_held` 日志。

处置步骤见 [player-inbox-runbook.md](../ops/player-inbox-runbook.md) §6.1「Match 个人结算首投积压」。积压源头的默认吞吐是每副本每轮(2 秒)首投 4 行、逐行串行。
这组默认值是 2026-10-30 之前写死的行为,还没有压测数据支撑调整,这批没改。

**残余与代价。**

- **越过上限之后**照现行规则解冻:COMPLETE 未应用、有原件的 NONE、未知三种都在这里收口,有告警 `battle_freeze_hold_cap_reached`。
  2026-11-10 起这一局带进战斗的道具仍被预留押着([battle-item-reservation.md](battle-item-reservation.md)),结算再晚到也从预留里扣,
  不再按持有量夹紧;剩下的只是 HP / MP、宠物状态按终值覆盖。match 侧 COMPLETE 不判截止这一点不动,不在本批。
- **锁多留。** 结算一直不来的局,FIGHTING 锁比改动前多留约 15 分钟,match 这段时间拒绝这名玩家排队;删锁没落地时同理。有界。
- **离线超过续冻上限才重登**:锁已过期,不再重建冻结;道具预留随存档在盘上,重登后照样押着,由 reaper 的预留复核按 SQL 终局收尾
  (2026-11-10),结算晚到仍从预留里扣。
- **开销。** 每个续冻中的玩家每 30 秒一次主键只读查询,正常运行为 0。
- 告警只到「值班固定查询」这一层:上面两条 LogQL(`battle_freeze_hold_cap_reached`、`battle_terminal_query_unknown`)已写进
  `deploy/k8s/asset-channel-alerts.yaml` 头注释的固定查询清单,但 scene 的这两个信号只在结构化日志里,Loki ruler 还没接线(上线批 BK8s),
  接线之前不会自动告警。match 一侧的发件箱积压已有 PromQL 规则(`deploy/k8s/match-outcome-outbox-alerts.yaml`,见上面「指标」段)。

**2026-11-10 复核:上限之后这一段怎么堵(负责人当日拍板方案 A,已落码,未编译,待 Codex;
做法与验证见 [battle-item-reservation.md](battle-item-reservation.md))。**

越过上限之后的残余有两种来源,一致性上都不成立:

- Match 发件箱积压超过约 17 分钟,结算更晚才到;
- 玩家可控的延后:两个背包都满,结算整笔延后,熬过上限、解冻之后把本局用掉的药经资产通道转走,结算到了只能按持有量夹紧
  (`bag-rule-policy-layering.md` §(7) 记过,`battle_inbox_shortfall_after_freeze_released` 能发现)。

两种情况的影响有边界:解冻之后 match 仍按 APPLIED 收据才放 ready 票,玩家进不了下一场全球匹配。
解冻放开的是资产通道、换场景与跨 zone,可被拿走的只有这一局带进战斗的消耗品。

负责人要求按强一致收口。逐条核对之后,「到上限也不解冻,只认终局证据」不能单独做,原因有四条:

1. **会死锁。** 冻结期间玩家腾不出位置。整理、穿脱装备、神秘商人、资产通道都按「在战」拒绝,背包协议也没有使用、丢弃、
   出售入口,冻结中只能从临时格取回人物背包。两个背包都满、这一局整笔延后时,结算永远放不进去。冻结若不随上限解开,
   玩家就永远冻住。现在的 15 分钟上限同时是这条死锁的唯一出口。
2. **离线那一半要求锁活过离线期。** FIGHTING 锁的 TTL 现在到续冻上限 + 60 秒,玩家在这之后才重登,锁已过期,冻结重建不出来。
   要堵就得让锁在终局之前不过期。删锁失败现在靠 TTL 兜底(`DeleteBattleLockIfMatch` 是发了就不管),也要改成有界重试,
   重试用尽后等玩家重登,再按证据收尾。
3. **证据要细到这名玩家。** 不设上限,解冻只能凭终局证据。现在的查询只回整局的 COMPLETE / ABORT,还要加「这名玩家的个人结算是否已
   APPLIED」(Match 个人结算源行的派发状态)。否则删锁没落地、玩家又重登时,COMPLETE 会把他永远冻住。
4. **Match 截止 ABORT 的硬上限**(`terminal_abort.go` 的 `deadlineAbortHardCapMs`)是按「早于 scene 上限」定的,
   `TestDeadlineAbortBoundsFitSceneFreezeHold` 钉着。scene 不再按上限解冻,这条不等式的含义要一起改。

可选的两条路:

| 方案 | 做法 | 一致性 | 代价 |
|---|---|---|---|
| A. 战斗道具预留(推荐) | 见下文 | 强一致:预留随存档落库,离线、换节点、重登都不丢 | 新增一个持久字段和一道出包闸;客户端要显示「战斗结算中」占用的数量,这是客户端可见协议,两版同批;Java 同做 |
| B. 冻结只凭终局证据解开 | 去掉上限;锁在终局之前不过期;查询带上这名玩家的结算状态;给两包全满开一个出口(冻结中允许丢弃,或溢出改投邮件) | 强一致 | 出口是新的玩法入口,或跨服务投递;事故期间受影响的玩家一直冻住,资产、换场景、跨 zone 都不能动 |

方案 A 的做法:

- 备战快照时,把带进战斗的道具按配置号与数量预留在玩家存档里,与背包同一次提交;
- 结算只从预留里扣,扣完释放余量;ABORT、或没有可裁决原件时整笔释放;
- 所有出包路径(资产通道、商人、强化与镶嵌扣材料等)只能动未预留的部分,`BagService` 是唯一闸口,结算走显式的预留扣减;
- 这样冻结按现行规则到期解冻,也不会出现「药白喝」,不需要锁活过离线期。

负责人选 A。它是「远端按快照执行、事后结算」的标准两阶段做法:先预留,再扣减或作废。

- 只锁住真正会被结算扣掉的东西,其余照常可玩;
- 一致性由存档本身保证,不依赖 Redis 锁的寿命,事故期间也不必冻住玩家。

上面的续冻与上限保持不动,它们还管着 HP / MP、宠物状态的覆盖时序。续冻到上限解冻时**不放**预留,
冻结已不在的预留由 reaper 按同一张终局表收尾、没有上限(`ReviewBattleItemHolds`);上面「越过上限之后照现行规则解冻……按持有量夹紧」
那条残余因此只剩 HP / MP 与宠物状态的覆盖,道具不再夹紧。

### 11.3 用例与验证(全部未执行,待 Codex)

- `cpp/tests/bag_test/bag_test.cpp`:`GuidSegmentClientTest.TopUpFetchesWhenABatchNeedsMoreThanIsLeftAboveThePrefetchThreshold`、
  `TopUpReportsEnoughExceedsMaxStepOrUnavailableWithoutSending`(评审改名,原 `TopUpReportsEnoughFullOrUnavailableWithoutSending`)、
  `TopUpThatFailedIsRetriedAfterBackoffEvenAboveThePrefetchThreshold`(评审补:重试也按抬高的 step 领),评审补
  `TopUpPromotesTheWaitingSegmentWhenTheCurrentOneIsSpent`、`TopUpRaisesTheStepToCoverTheWholeBatchWithoutChangingTheDynamicStep`、
  `TopUpAbandonsTheCurrentRemainderWhenBothBuffersCannotCoverTheBatch`;
- `cpp/tests/bag_test/player_battle_settlement_test.cpp`:`PlayerBattleSettlementItemTest.ItemGuidShortageAboveThePrefetchThresholdRequestsATopUp`,
  评审补 `ItemGuidNeedAboveTheMaxStepDefersTheWholeSettlementWithoutFetching`;
  夹具注入终局查询一律回 NO_ORIGINAL(这些局没有 SQL 原件),原有的到期用例照旧成立;`FirstConfirmUpgradesToFightingAndRenewsFromFreezeExpiry`
  与 `RepeatedConfirmRenewsLockOnceFromComponentStateWithoutChangingIt` 的 TTL 断言改按 FreezeHoldDeadlineMs;
- `cpp/tests/bag_test/battle_freeze_hold_test.cpp`(新):`BattleFreezeHoldRules.DecisionTableAndHoldDeadline`、
  `BattleFreezeHoldRules.LockTtlDeadlineInferenceLandsOnTheOriginalExpiry`(评审补)、
  `BattleFreezeHoldTest.*` 11 条(COMPLETE 未应用续冻、到上限解冻计数(评审改:越过上限不再查询)、查询失败续冻一拍、ABORT 照常解冻、有原件的 NONE 续冻、
  没有 SQL 原件的局到期照常解冻、开局确认的锁活过续冻上限(到期后重登可重建)、续冻期间资产通道 RETRY、本地已应用不查、迟到回答不动、
  丢失按未知(评审改:按单调钟拨)),评审补 `StatelessFreezeIsResolvedLikeFightingAndStaysBounded`、
  `LostQueryIsMeasuredOnTheMonotonicClockNotTheWallClock`、`PastTheCapReleasesOnTheFirstTickWithoutWaitingForAQueryInFlight`、
  `RejectedOrSkippedInTheBattleRingIsNotTerminalAndStillQueries`、`InboxDecisionsOfOtherBattlesOrTypesStillQuery` 共 16 条;
  `BattleTerminalQueryReply.ClassifiesOnlySignedRepliesBoundToTheRequest`、`AssetOpCreditGuidTopUpTest.*` 3 条
  (评审补 `BatchNeedingMoreThanTheMaxStepIsRejectedAsAnInvalidBundle`;夹具先清残留的全局封禁);
- `go/match/internal/logic/battle_terminal_decision_test.go`(新)4 个用例;`go/match/internal/authority/terminal_read_test.go`(新):
  `TestTerminalOutcomeExpectedOnlyForAdjudicableOriginals`(纯判据)、`TestTerminalTiDBBattleTerminalStateTellsPendingFromNoOriginal`(真 TiDB)。

验证顺序(仓库根,串行,失败不重试):

1. 协议生成(`match_internal.proto` 新增一条 RPC 两条消息):`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`(生成器有变化或尚无工具时),
   再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
   通过标准:`cpp/generated/grpc_client/match/match_internal_grpc_client.h` 出现 `SendMatchInternalGetBattleTerminalDecision`,
   `go/proto/match` 出现 `MatchInternal_GetBattleTerminalDecision_FullMethodName`。
2. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`:0 个 `error C` / `LNK`;改了 scene 库与 id_segment 库,先编库再跑测试。
3. `build\cpp\tests\bag_test.exe --gtest_filter=GuidSegmentClientTest.*:GuidSegmentRegistryTest.*:BattleFreezeHold*:BattleTerminalQueryReply.*:AssetOpCreditGuidTopUpTest.*:PlayerBattleSettlement*:BagServiceNewInstanceBound.*` 全绿,再跑一次完整 `bag_test.exe`;
   `currency_test.exe --gtest_filter=AssetOpSystemTest.*:AssetOpRecallTest.*` 不回归。
4. `cd go/match && go build ./... && go vet ./internal/logic/ ./internal/server/ ./internal/authority/ && go test -count=1 ./internal/logic/ -run 'TestGetBattleTerminalDecision|TestNewBattleTerminalDecisionQuery'`,
   `go test -count=1 ./internal/authority/ -run 'TestTerminalOutcomeExpectedOnlyForAdjudicableOriginals'`;
   真 TiDB:`MATCH_IT_TIDB=<端口> MATCH_IT_REQUIRE_TIDB=1 go test -count=1 ./internal/authority/ -run 'TestTerminalTiDBBattleTerminalState'`(不得跳过);
   最后 `go test -count=1 ./...`。

Java 版要做的点(AGENTS §12,本批未改 Java):Java 没有号段客户端(物品 guid 用雪花租约),r2-battle#3 不适用;
冻结到期查终局与只读终局 RPC 两边都要做 —— Java 的 reaper 到期读 Redis 里的个人结算记录、读不到就判废,match 也没有只读终局 RPC。
