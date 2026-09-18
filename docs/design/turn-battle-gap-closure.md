# 回合制战斗缺口收口 G1–G9(2026-09-17)

> 状态:**已落码,未编译、未导表、未运行,待 Codex 验证**(AGENTS §10.1)。验证清单见 §7。
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

## 7. 未编译,待 Codex 验证(按序)

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
