# 回合制战斗缺口收口 G1–G9(2026-09-17)

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
4. **局中闸只放入口层**(见 D48)。任何人想把 `InBattleComp` 判定加进 `BagService` / `CurrencySystem` 之前,先读这条。
5. **快照进引擎前必须清洗**:引擎 `InitPlayers` 对 buff 与技能各有一道兜底过滤。快照来自另一个进程(甚至另一个 zone 的 scene),不能单点信任。

## 6. 已知残留

- **G9 的首投延迟**:结算首投仍打开局时的 scene 节点,玩家换节点重登后要等 battle 侧下一轮探测(≤10s)才入账。R07 的 12 轮 ×10s 耗尽后靠登录钩子兜底。本轮判定为可接受,不改码。
- **buff 剩余时长仍按表全量换算**:控制类已剔除,剩下的都是周期效果类,影响有界。要精确剩余时长得给 `BuffComp` 加到期时间字段。
- **脱敏挡不住推算**:`TurnResultS2C.events` 带 `skill_table_id` / `buff_table_id`,对手查表仍可推算冷却与 buff 时长;`pending_actor_ids` 与 `is_auto` 也暴露"谁已出手/谁挂机"。本轮只堵"直接读"。
- **`items_gained` 全部落在主背包或临时格**:没有邮件兜底(全仓无 mail 模块)。临时格是 FIFO 淘汰的溢出缓冲,极端情况下会挤掉更早的掉落。
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
   前置 `PATH` 里要有 `protoc-gen-go` / `protoc-gen-go-grpc`,仓内 protoc 必须是 `libprotoc 35.1`(`third_party\grpc\install_vs2026_dbg\bin\protoc.exe`);先关掉 Excel(避免 `~$` 锁文件)。
   - 零副作用预检:`py tools/data_table_exporter/tools/sandbox_export.py --out <临时目录> --compare`
   - 正式导表:`py tools\data_table_exporter\run.py tools\data_table_exporter\exporter_config.yaml`(不要直接跑 `dev.bat`,它失败路径带 `pause`,非交互会卡住)
   - 刷新索引:`py tools/data_table_exporter/tools/gen_schema_index.py`
   - 通过标准:日志末尾 `===== Data Table Exporter: DONE =====`,无 `FK:` / `主键:` / `DeployError` / `陈旧产物`;`generated/tables/item.json` 出现 `battle_usable`,`monster.json` 出现 `drop`。
   - **副作用提醒**:导表会写同级客户端仓 `..\mmorpg-client\Assets\Scripts\Table\Generated`。
2. **proto 重生成**(`proto/battle/player_battle.proto` 加了 `self_items`):
   `pwsh tools\scripts\dev_tools.ps1 -Command proto-gen-build` 然后 `pwsh tools\scripts\dev_tools.ps1 -Command proto-gen-run -UseBinary -ConfigPath tools\proto_generator\protogen\etc\proto_gen.yaml`;随后 `cd go && build.bat`。
   - 正向断言(regen 吞代码的历史坑):`grep -n AcquireCreatePermitBlocking cpp/nodes/scene/handler/grpc/scene_node_service.cpp` **必须命中**。
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

### 9.3 结算幂等基底持久化(**2026-09-25 已落码,未编译**)

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
| 去重判据 | `ApplySettlementToEntity` 开头先查**活账本**:命中 = 奖已发过,不再发第二次 |
| 销账判据 | `IsSettlementDurable` 只看 `PlayerLastPersistedSnapshotComp` —— 上一次**确实写进 Redis** 的那份 `PlayerAllData`。条目出现在这份字节里,才允许销账 |
| 销账收口 | 新增 `AckSettlementPending(playerId, battleId)` 为**唯一销账入口**,原先 7 个 `ClearPendingSettlementIfMatch` 调用点全部改走它 |
| 条目回收 | 条件删 EVAL **成功回调**里才 `ForgetApplied`。所以账本只登记"已应用但销账未确认"的局,稳态长度 0~1 |
| 排空兜底 | reaper(30s)第二遍扫账本补 Ack —— 不依赖 battle 发件箱(它只重投 12 轮 120s 就放弃) |
| 压缩窗口 | 应用成功后立刻 `SavePlayerToRedis`(经 `SetPersistFnForTest` 注入点,与资产通道同形状),把"已应用未落盘"从一个周期存盘压到一次 Redis 往返 |

#### 为什么两条判据必须分开

"在账本里"和"已经落盘"是**两件事**,合成一条就等于没改:活账本里有、落盘快照里没有,恰恰是
最危险的一刻 —— 此时销账,崩溃即永久丢失。所以:

- `HasApplied(实体上的活账本)` → **别再发一次奖**;
- `IsSettlementDurable(落盘快照里的账本)` → **可以销账**。

#### 顺带修掉的两个真缺陷

1. **销账绕过点**:应用成功后紧接着 `ClearBattleFreeze`(删锁 + 摘 `InBattleComp`),于是发件箱
   10s 后的第一次重投必然落进"无 `InBattleComp` 且锁不在"那一支 —— 那里**不经过**
   `ApplySettlementToEntity`,改造前会当场销账,把延后的努力全部作废。收归单一入口后这些分支
   一并受持久判据保护:账本里有 = 已应用 → 等落盘;账本里没有 = 真作废 → 立即销账(行为不变)。
2. **冻结闸只看 gold/items**:一笔"只有 HP + 宝宝 + 击杀事实、没有金币也没有掉落"的 PVE 结算
   (打空怪,完全正常)会穿过闸门 —— 任务进度被推、宝宝被改,而此刻存盘被硬性跳过,改动随实体
   销毁一起没,账本却记成已应用。现在改为整笔判据 `IsSettlementApplicable`,并补上
   `PlayerTravelHandoffComp` / `UnregisterPlayer`(这两个窗口里 `SavePlayerToRedis` 开头直接跳过
   写盘,应用了也落不了盘)。刻意**不**含资产通道的 `HasFencedOwnership`,理由写在函数注释里。

#### 残留与代价(已知、刻意接受)

- **每场战斗多一次存盘 + 一轮重投**:durable 判定要等异步存盘落地,应用当下探测必为假,所以销账
  通常发生在发件箱 10s 后的那一次重投(或 reaper 的 30s 扫描)。这是照搬通用资产通道"触发存盘、
  如实回报、由上游重试追平"的口径,不新造等待机制。
- **pending 每玩家单槽**(`battle:settlement:pending:{player_id}`,无条件 SET):延后销账把槽位
  占得更久,连打两场时后一场会覆盖前一场的 payload。靠"应用后立刻存盘"把窗口压到一次 Redis
  往返来规避;**彻底修法是 pending 改成按 (player, battle) 一条**,那要动两端 key 契约,未做。
- **"落盘"指写进 Redis,不是 MySQL**:与全仓既有口径一致(Redis 是在线权威存储,MySQL 在其下游)。
- **回档会把账本退回旧值**,而 pending 不参与回档、活 7 天 → 已发的奖可能被重发。但回档同时把
  资产也退回了,重发与回档语义自洽,不额外加机制。
- **账本容量 `kMaxAppliedRecords = 64` 是异常兜底不是常规容量**:条目销账一确认就摘,要堆到 64
  意味着连续 64 局条件删全失败(Redis 已不可用)。溢出淘汰最旧项并打
  `metric=battle_settlement_ledger_evicted`。

#### 设计过程(留痕)

第一版设计挂 `HandlePlayerAsyncSaved` 回调 + 一个独立"待确认集合",被对抗评审(3 个评审面、
`wvo7qkttt`)判 **broken**,4 个 blocker:销账点只搬了 1 个、账本成员被当成已落盘、pending 单槽、
整集合清空会 ACK 掉没落盘的局。现方案逐条规避,且**不再需要新挂点** ——
`PlayerLastPersistedSnapshotComp` 本来就是"刚写进 Redis 的那份字节",跨区传送线预留的
`HandlePlayerAsyncSaved` 挂点**不再需要**,那个文件一行未动。

#### 验证清单(全部未执行)

1. **MySQL 加列**(导表/生成之后、任何新 go/db 或 scene 启动之前,冒烟之前必须):
   `cd go/db && go run ./cmd/migrate -f etc/db.yaml -command plan`,计划应只含 `player_database`
   加列 `settlement_ledger`;确认后 `-command up`(不加 `-allow-modify`)。本地多 zone 时对启动器
   实际用的每份 db 配置各跑一次。随后每个 zone 库 `SHOW COLUMNS FROM player_database LIKE
   'settlement_ledger'` 返回 1 行。**漏这一步会让全服玩家回写全部失败**
   (`AutoMigrateSchema: false`,`key_ordered_consumer.go` 按 descriptor 写全部列)。
2. 编译 scene 相关工程与 `bag_test`。
3. `bag_test` 全绿,重点看本节新增的 7 个用例(账本登记 / 重启后不重复发 / 冻结与交接整笔延后 /
   三条纯规则),以及**契约变更后更新过的**
   `DuplicateSuccessfulCallbacksAndReentryPayAndProgressOnce`。
4. `battle-smoke` 端到端:打一场有掉落的战斗 → 看日志出现一次"结算已应用"、随后出现
   "销账延后"、再在下一轮重投或 reaper 后出现销账落地;`battle:settlement:pending:{id}` 最终消失。
5. 杀进程验丢失窗口:应用之后、存盘之前 kill scene → 重启 → 登录应重新发一次掉落(不丢);
   人为让条件删失败 + 重启 → 登录**不得**重复发(不复制)。

### 9.2 仍然没做

编译、18 个新单测、`battle_smoke` —— 用户 09-18 明确跳过,至今未做。**这批代码从落码到现在
从未被编译器看过**,而期间引擎、节点、背包、scene 四个文件都被别的会话改过(含一次 IDE 批量
删头文件)。真要验证,§7 第 3–6 步照跑;风险排序:能不能编过 > 单测是否真绿 > 端到端是否跑通。
