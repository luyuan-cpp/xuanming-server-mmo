# 实时技能冷却与 Buff 跨重登保留

> 2026-11-19,Claude 定案并落码。**协议未生成、未导表、未编译、未测试**,验证步骤见 §8,交 Codex。
> Java 版只在 Java 仓 `PARITY.md` 末尾登记待做(§9)。
> 收掉 [断线重连审计](disconnect-reconnect-audit-20261020.md) §9.2 的「规则需确定」与 [断线保留](player-connection-lost-hold.md) §8 第 5 条。
> 2026-11-20 复核补(同样未编译):一起修掉再施加不刷新时长、回合快照按全量时长换算两处(§2.6);离线暂停(§2.3)与客户端下发(§7)改为定案不做,写明理由。
> 同日独立评审后再补(未编译):本节点表里查不到行的 Buff 条目原样带回存档、不丢(§2.5 第 4 条);空冷却列表也保留更新版本的列表级字段(§5);恢复出来的到期时刻与存档一字不差(§3);新建条目生效回调之后按号重查再挂定时器。

## 0. 一句话

技能冷却全部随玩家存档保留,存 UTC 毫秒的绝对时刻;Buff 只保留表里标了「下线保留」的那几种,同样存 UTC 毫秒的到期时刻;其余 Buff 是战斗类短效状态,实体一重建就清掉。离线期间时间照常走。

## 1. 问题

- 冷却在运行时组件 `CooldownTimeListComp`,Buff 在 `BuffListComp`,两者原来都不落库。
- 实体复用时(断线保留内重连、同节点换场景、离线托管)两者都还在。实体重建时(保留到期后重登、主动下线后重登、跨节点交接、跨 zone 传送、节点崩溃后重登)从玩家存档加载,存档里没有它们,全部丢失。
- 后果:
  - 下线重登、换一次节点就能清掉长冷却;
  - 长时效增益(例如经验加成)换节点、重登即白白丢失;
  - 复用与重建两条路径的结果不一样(审计 §9.6 第三行)。

## 2. 规则(定案)

| 状态 | 跨实体重建 | 存什么 | 时钟 | 离线期间 |
|---|---|---|---|---|
| 技能冷却 | 全部保留,不设时长阈值 | 冷却组号 + 开始时刻;就绪时刻 = 开始时刻 + Cooldown 表 `duration` | UTC 墙钟毫秒 | 照常走 |
| Buff 表 `keep_on_logout = 1` 的 Buff | 保留 | 表号 + 层数 + 到期时刻(永久 Buff 记 0) | UTC 墙钟毫秒 | 照常走 |
| 其余 Buff(控制、伤害、护盾、隐身等战斗类短效状态) | 实体重建即清 | 不存 | — | — |

复用实体的路径不受影响,运行时状态照旧。所以冷却在两条路径上完全等价;不保留的 Buff 只在实体复用时还在,而它们都只有几秒(现有实时 Buff 最长 12 秒),断线保留(25 秒)到期之前就已经过期。

### 2.1 冷却为什么不设阈值

- 跨节点换图、跨 zone 传送都是重建。阈值以下的冷却每换一次节点就清一次,玩家可以借换节点、重登刷短冷却。
- 设了阈值,实体复用与重建两条路径就不再等价。
- 体积有上界:冷却按冷却组号覆盖写,条数不超过 Cooldown 表的行数(现有 9 行),每条十几个字节。
- 业界做法相同:TrinityCore 等 WoW 服务端把全部技能冷却连同绝对结束时刻存库,加载时跳过已经过期的。

### 2.2 冷却存开始时刻,不存剩余时长

- 运行时本来就是「开始时刻 + 表里的时长」现算剩余(`CoolDownTimeMillisecondSystem::Remaining`)。存档沿用这个形状,`CooldownTimeListComp` 本身就是存档消息,热更改了冷却时长时复用与重建两条路径照样一致。
- 开始时刻和就绪时刻都是绝对时刻,离线期间照常走。
- 到点的记录不删:它不影响判定(按时间判),同组下次施法时覆盖。存盘时也不按时间筛选,所以这一行的字节只在施法时变,不会因为冷却到点而多写一次。

### 2.3 Buff 为什么按绝对到期时刻、离线照常走

- **存档内容必须只由实体状态决定,不随时钟变。** 退出收尾时 `player_exit::IsPersistedPayloadCurrent` 拿刚落地的存档与当前内存按行投影、逐行比字节,相同才销毁实体;周期存盘判脏也是逐行比字节。绝对到期时刻在线期间不变,只在施加、刷新和到期时变,满足这一条。
- 和冷却用同一条时钟规则,好讲,也好查。
- 下线不会给玩家带来好处(增益照常过期),也就无从利用。
- **定案:不做「离线不计时」**,所有保留的 Buff 离线照常计时。离线暂停只有两种做法,代价都大于收益:
  - 每次存盘写当时的剩余时长:存档随时钟变,退出收敛永远比不上,一直重存到轮数上限;有这种 Buff 的玩家每次周期存盘都重写整行。
  - 在退出、交接冻结、交接中止三处加暂停 / 恢复的状态转换:多出一套跨流程的状态机,只为一种表里没有的 Buff。

### 2.4 标记放在 Buff 表

- 位置:`data/schema/buff_table.proto` 新字段 `keep_on_logout`(uint32,字段号 54,`cfg_owner = "server"`),源表 `data/Buff.xlsx` 第 64 列。只有服务端读,客户端的表产物不变。
- 取值:0 = 不保留(缺省),1 = 保留。
- 现有 98 行全部是 0:实时侧的现有 Buff 都是战斗效果或演示行,回合战斗的 Buff 由战斗引擎自己管理,不走这条链。
- 哪些行该配 1:持续时间远大于一次重登(分钟级以上)、不是战斗效果、效果和施法者无关的增益或惩罚,例如经验加成、料理、称号类增益、死亡惩罚。控制、伤害、护盾、隐身、子 Buff 都不配。
- 配 1 的行不要做成周期性重复施加的来源(例如光环每秒重挂一次):每次刷新都会改存档里的到期时刻,玩家下线收尾时会一直判成没收敛、重存到轮数上限。

### 2.5 保留的 Buff 的附带规则

1. **不存施法者。** 运行时的施法者是实体句柄,重建后没有意义。恢复后按系统施加处理(没有施法者)。
   在线时同一行也不按施法者分条:`keep_on_logout = 1` 的行按 `no_caster = 1` 叠层(`buff.cpp` 的 `IgnoresCaster`)。否则恢复出来的条目和之后别人再施加的同一个 Buff 会各占一条,效果翻倍。
2. **子 Buff 不单独存。** 挂在别的 Buff 子项表(`sub_buff_list_id`)里的条目由父项负责:父项恢复后按自己的逻辑重新挂。单独恢复会留下没人撤销的孤儿,脱战加成就是这种情况。
3. **恢复不重跑准入判断。** 免疫、驱散、叠层判断在当初施加时已经判过,恢复是把存档时活着的条目原样放回,结果与恢复顺序无关。冻结和回合战斗两道闸在加载时还没挂上,也挡不到恢复。
4. **恢复按加载时的表。**
   - 表里查不到这一行(更新版本加的行,或本节点还没热更到):不生效,留在暂存组件上原样带回存档(连未知字段),过了它自己的到期时刻才丢。丢掉的话,玩家回到新版本节点时这个 Buff 就永远没了(混版本保护 P1)。表没有标「行号只增不删」,所以删掉的永久行会一直被带着,条数以删过的行数为上界;
   - 表里改成了 0:丢弃并打日志。把已有的行从 0 改成 1 的那次热更窗口里,还在用旧表的节点会按 0 丢弃,只影响窗口里重建的那几个人;
   - 层数夹到 `[1, max_layer]`;
   - 剩余时长不超过表里的时长,写盘节点时钟超前时最多也只多一个整时长;
   - 原来是永久 Buff、表改成了有时限:从加载时起算一个整时长;
   - 已到期:丢弃。

### 2.6 一起修掉的两处(都靠 `BuffEntry::expireAtMs`)

1. **再施加同一条 Buff 刷新时长。** 原来 `BuffSystem::OnBuffRefresh` 是空的 TODO:同一施法者再施加只加层,到期时刻不动,第二瓶经验丹的时长白白浪费。现在剩余时长回到表里的整时长(重挂满时,取消旧定时器),与回合引擎刷新持续回合的口径一致;永久与瞬时的不动,不重跑 `OnBuffStart`。
2. **回合战斗快照按剩余时长换算回合。** 原来 `PlayerBattleSystem::BuildBattleSnapshot` 拿不到剩余时长,按表里的全量时长换算(注释写着「另开任务」):增益带进战斗偏宽、减益偏严。现在按这条自己的到期时刻换算,已到点、到期回调还没跑的不带;没有到期时刻的有限行记 0,由引擎入场时夹到表里的全量时长。控制类照旧不带(PVP 应战前挂控制的问题与时长无关)。

## 3. 时钟口径

- 存档里的时刻一律是 UTC 墙钟毫秒:冷却取 `TimeSystem::NowMilliseconds`,Buff 取 `TimeSystem::NowMillisecondsUTC`,两者都读 boost 的 `universal_time`。跨节点比较依赖 NTP 对时,偏差在毫秒级,只会让冷却或 Buff 早几毫秒、晚几毫秒结束。
- 写盘节点时钟超前,存档里出现未来时刻:
  - 冷却开始时刻晚于加载节点的当前时刻时,压到当前时刻。剩余时长最多一个整冷却,不会锁到那个未来时刻之后。
  - Buff 的剩余时长夹到表里的时长。
- 时钟回拨:冷却显得更长(上限一个整冷却),Buff 晚到期(上限一个整时长)。不另做处理。
- 运行时到期仍由 muduo 定时器驱动(`TimerTaskComp`)。`BuffEntry::expireAtMs` 供存档和回合快照算剩余,与定时器在同一处(`buff.cpp` 的 `ArmBuffExpiry`)设置、刷新时一起重设。
- 恢复时剩余时长与到期时刻按同一次读到的时刻算,恢复出来的到期时刻与存档里的一字不差:重建多少次都不漂移,加载后的第一次存盘也不会因为它被判脏。

## 4. 覆盖的路径

| 路径 | 实体 | 冷却与保留的 Buff |
|---|---|---|
| 断线保留内重连、同节点换场景、离线托管 | 复用 | 运行时状态照旧,不经存档 |
| 保留到期后重登、主动下线后重登 | 重建 | 退出存盘时写入,加载时恢复 |
| 跨节点交接(同 zone 换图)、跨 zone 传送 | 重建 | 冻结时那一次存盘写入,目标节点加载时恢复 |
| 节点崩溃后重登 | 重建 | 回到最后一次落盘,与血蓝、背包同一口径 |

只有一个恢复入口:`PlayerLifecycleSystem::InitPlayerFromAllData`,所有重建都经过它。

## 5. 存储

- `player_database` 新增两个字段,都放 CHARACTER 行(`OptionPersistGroup = 6`),与当前血蓝一样是战斗中会变的小数据:
  - `skill_cooldowns = 28`:`CooldownTimeListComp`;
  - `saved_buffs = 29`:`repeated SavedBuffComp`(`proto/common/component/buff_comp.proto`,字段为 `buff_table_id`、`layer`、`expire_at_ms`)。
- 存盘:
  - 冷却组件原样拷进存档,没有组件就写空;
  - 保留的 Buff 按 buff_id 排序后写出,同样的状态产出同样的字节,不会因为 `unordered_map` 的遍历顺序被判脏。
- 混版本:两个字段的未知字段都随对象携带。
  - 冷却组件本身就是存档消息;列表里没有记录、只带着更新版本的列表级字段时也照样挂上,不丢那些字段;
  - Buff 条目的未知字段在恢复时存进 `BuffEntry::persistUnknown`,存盘时并回;本节点表里查不到行的条目整条原样带回(§2.5 第 4 条);
  - 往返一致性测试(`persist_round_trip_test`)的夹具覆盖了这两个字段的全部新类型。

## 6. 组件访问

已同步进 [组件访问规则](ecs-component-access-rules.md)。

| 组件 | 类别 | 创建 | 读缺失 |
|---|---|---|---|
| `CooldownTimeListComp` | 按需状态,随存档保留 | `SkillSystem::StartCooldown`(第一次启动冷却);加载时存档里有冷却记录(`SkillSystem::RestoreCooldowns`) | 没有冷却;存盘写空,不补建 |
| `BuffListComp` | 按需状态 | 唯一创建处仍是新建 Buff 条目处,施加与恢复共用 | 身上没有 Buff |
| `SavedBuffsPendingComp`(新增) | 按需状态 | 加载时存档里有保留的 Buff 才挂 | 没有待恢复的 Buff |

- `SavedBuffsPendingComp` 由 `InitPlayerFromAllData` 消费:派生投影初始化之后、进场景之前,`BuffSystem::RestoreSavedBuffs` 恢复。生效回调(战斗状态、隐身标签)要用到派生投影组件,所以不在反序列化里直接恢复。恢复后只剩本节点表里查不到行的条目时留着,否则摘掉。
- 存盘时 `SavedBuffsPendingComp` 还挂着(恢复没跑到,或有查不到行的条目),就把其中的条目原样写回,保证不丢。

## 7. 客户端

- 客户端目前不显示实时冷却和实时 Buff:技能面板只显示回合战斗的「冷却回合」(`ListSkillsResponse.cooldown_rounds`,表里的静态值),`GameClient.ReleaseSkill` 没有界面调用方。服务端也没有下发实时冷却剩余的协议。
- **定案:不改客户端,不加下发协议。** 没有界面消费的状态不往客户端推(与组件访问规则「没有消费者不发事件」同一判据)。回合战斗里的 Buff 剩余回合照旧随战斗快照与回合广播下发,按剩余时长换算之后客户端拿到的就是准确值(§2.6 第 2 条)。

## 8. 验证(交 Codex)

在仓库根目录按顺序执行,前一步失败就停下、保留输出。

1. **导表**(Buff 表加了一列):
   - 先看 `git status --short data/`,除 `data/Buff.xlsx`、`data/schema/buff_table.proto` 外有别人未提交的表改动时,先停下报告,不要整批导出;
   - 执行 `dev.bat gen`;
   - 执行 `py tools/data_table_exporter/tools/gen_schema_index.py`,刷新 `data/AGENTS.md` 的索引块(Buff 应变成 64 列、54 个字段);
   - 通过标准:导表零报错。`cpp/generated/table` 下 Buff 表的 proto 与代码出现 `keep_on_logout`,客户端表产物不变(`cfg_owner = "server"`)。
2. **协议生成**:
   - 执行 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`;
   - 通过标准:`proto/persist_schema_gen.txt` 只新增世代 1 的行(台账仍在 bootstrap),包括 `player_database#28 … group=6`、`player_database#29 repeated SavedBuffComp saved_buffs group=6`、`SavedBuffComp#1..3`,以及 `CooldownTimeListComp`、`CooldownTimeComp` 的字段行;没有改号或换组报错。
3. **C++ 编译**:MSBuild 一律 `/m:1` 串行。
   - 先编库:生成的 table、proto 工程,以及 engine core 与 `cpp/libs/services/scene/scene.vcxproj`;
   - 再编 `cpp/nodes/scene`;
   - 测试工程链接预编好的 lib,改了库要先编库,再编测试。
4. **C++ 测试**:
   - 执行 `pwsh tools/scripts/run_cpp_tests.ps1 -Build -Filter buff`、`-Filter skill`、`-Filter bag`、`-Filter turn_battle`,日志放 `build/verification/<本次运行>/cpp-tests/`;
   - 新增用例必须全部通过:
     - `buff_test` 的 `BuffKeepOnLogoutTest.*` 与 `BuffDurationTest.ReapplyingRefreshesTheExpiryToTheFullDuration`;
     - `skill_test` 的 `SkillUtilTest.RestoreCooldowns_*`;
     - `bag_test` 的 `PersistRoundTripTest.*`(夹具新增冷却与保留的 Buff 两类节点)与 `PlayerBattleSettlementTest.SnapshotBuffRoundsFollowTheRemainingTime`;
   - 原有用例不得变红。重点看 `BuffStackingTest.*`、`BuffDurationTest.*`、`player_feature_persistence_test` 的 `ExpectLazyActorStateAbsent` 类断言,以及 `turn_battle_engine_test` 里参战 Buff 的回合用例(引擎没改逻辑,只改注释)。
5. **整解与 Linux**:
   - 整解按现有口径构建一次;
   - `cross_zone_test` 全量跑一次,它经 `MarshalAsSaved` 走存盘入口;
   - Linux 按 `tools/scripts/build_linux.sh` 的现有口径构建一次。
6. **冒烟**(本地起服、robot 登录):
   - 放一个有冷却的技能,立刻下线;等断线保留结束(25 秒以上)后重登,立刻再放,应回 `kSkillCooldownNotReady`(冷却未到),等冷却过了才能放;
   - 跨节点换图后同上。
   - 现有表里没有 `keep_on_logout = 1` 的行,Buff 那一半由单测覆盖,不做冒烟。

失败时保留:导表或生成的完整报错、MSBuild 首个错误及其上下文、失败用例的 gtest 输出。

## 9. Java 版

只在 Java 仓 `PARITY.md` 末尾登记待做,本轮不写 Java 代码。要对齐的是行为:

- 冷却全部按绝对时刻跨重登、换节点保留;
- Buff 表 `keep_on_logout = 1` 的 Buff 按到期时刻保留,其余重建即清;
- 恢复按加载时的表裁剪(见 §2.5 第 4 条);
- 再施加同一条 Buff 重挂满时;带进回合战斗的 Buff 按剩余时长换算回合(§2.6)。

客户端不可见,不涉及协议。配置表的这一列两版共用。

## 10. 改动清单

- 配置表:`data/schema/buff_table.proto`、`data/Buff.xlsx`(新列 `keep_on_logout`,数据全空 = 0)。
- 协议:
  - `proto/common/component/buff_comp.proto`:`SavedBuffComp`;
  - `proto/common/database/mysql_database_table.proto`:字段 28、29;
  - `proto/player_store/player_store.proto`:只改 CHARACTER 行的注释。
- 服务端:
  - `combat/buff/comp/buff_comp.h`:`BuffEntry::expireAtMs` / `persistUnknown`、`SavedBuffsPendingComp`;
  - `combat/buff/system/buff.{h,cpp}`:新建条目抽成 `StartBuffEntry`,施加与恢复共用;到期计时收成 `ArmBuffExpiry`;`OnBuffRefresh` 重挂满时;`IgnoresCaster`;`SaveKeptBuffs` / `RestoreSavedBuffs`;
  - `combat/skill/system/skill.{h,cpp}`:`RestoreCooldowns`;
  - `player/system/player_database_loader.cpp`:加载与存盘;
  - `player/system/player_lifecycle.cpp`:`InitPlayerFromAllData` 调恢复;
  - `battle/system/player_battle.cpp`:战斗快照按剩余时长换算回合;
  - `cpp/libs/services/battle/system/turn_battle_engine.cpp`:只改刷新口径那一行注释。
- 测试:`cpp/tests/buff_test/buff_test.cpp`、`cpp/tests/skill_test/skill_test.cpp`、`cpp/tests/bag_test/persist_round_trip_test.cpp`、`cpp/tests/bag_test/player_battle_settlement_test.cpp`。
- 文档:本文;[组件访问规则](ecs-component-access-rules.md);[断线重连审计](disconnect-reconnect-audit-20261020.md) §9.2;[断线保留](player-connection-lost-hold.md) §8 第 5 条;`docs/README.md`;`docs/PROGRESS.md`。
