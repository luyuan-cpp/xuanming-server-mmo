# 战斗策略 / 活动 / 玩法模板可配置化（含玩家自定义模板）

> **状态**：设计稿 v0.2（2026-10-19，Claude；§8 经两路评审修订，见 §8.13）。**未落码、未编译、未测试**（AGENTS.md §10.1）。负责人评审通过后按 §10 分批实施，每批交给 Codex 验证。
> **来源**：5 路现状调研（配表管线 / 战斗 / 活动 / 玩法 / Java 版）→ 3 套整体方案（表驱动原子组合 / Lua 脚本 / 节点图 + 事件-条件-动作触发器）→ 2 个视角交叉评审（正确性·一致性·双版本；策划易用·运维·性能）。玩家自定义模板（§8）另做了 4 路调研和 2 视角评审。
> **证据口径**：[C] 表示对照 2026-10-19 工作区代码读过；[I] 表示推断。工作区同时有其他会话在改，行号会漂移，正文以函数名为主、行号只作提示。
> **上线状态**：负责人 2026-10-18 确认项目尚未上线（[db-write-fencing-mysql.md](db-write-fencing-mysql.md) 文末「上线状态」）。文中凡涉及"新旧版本混跑 / 滚动升级 / 旧节点忽略新字段"的约束，上线前都不是阻塞项，首次上线整批起步即可；这些设计仍然保留，供上线后的增量发布使用。
> **相关**：[turn-based-battle-server.md](turn-based-battle-server.md)（回合引擎与确定性）、[pve-team-size-matching.md](pve-team-size-matching.md)（队列 key 语义）、[activity_maintenance_auto_shift.md](activity_maintenance_auto_shift.md)（顺延）、[player-features-ui.md](player-features-ui.md)（活动列表接口）、[ecs-component-access-rules.md](ecs-component-access-rules.md)、[mixed-version-persist-field-protection.md](mixed-version-persist-field-protection.md)（新旧版本混跑的持久字段）、[table-api-reference-zh.md](table-api-reference-zh.md)、[tip-code-axis.md](tip-code-axis.md)、`data/AGENTS.md`（配表 schema 规则）。

---

## 0. 结论速览

1. **三个领域统一分三层**：

   | 层 | 谁负责 | 是什么 | 例子 |
   |---|---|---|---|
   | 原子（积木） | 程序，代码里注册的有限集合 | 条件、目标选择、行动、事件源、规则参数 | "自身血量低于 X""血最少的敌人""放技能 S""杀怪事件" |
   | 模板（规则） | 策划，填表 | 原子怎么组合：优先级规则、阶段、计数器、档位 | "三阶段 Boss 打法""累计击杀活动""3v3 竞技规则" |
   | 实例 | 策划 / 运营，填一行；或**玩家在策划给定范围内自建**（§8） | 模板 + 参数 + 时间 | "山魈王用打法 900""国庆这一期""我的好友 3v3 房规则" |

   纪律只有一条：**程序决定"能做什么"，策划决定"怎么组合"，玩家只能在策划画的圈里选值**。发奖、扣钱、计分这类资产操作只能是代码里的原子，配置与玩家模板只能引用，不能实现。
2. **技术路线：表驱动原子组合**。不引入 Lua 或其他通用脚本；不用现有 exprtk 表达式列承载策略；v1 不做行为树 / 节点图。理由见 §2。
3. **战斗策略**：`BattleAtomKind` 原子字典 + `BattleStrategy` / `BattleStrategyPhase` / `BattleStrategyRule`，一条规则一行"条件1 且 条件2 → 行动 + 目标"。接缝只有 `TurnBattleEngine::FillDefaultActions` 一处（§4）。
4. **活动**：模板 / 实例 / 奖励档位三层；阶段由 (实例起点, 偏移, now) 纯函数推导；触发器是扁平行"事件 + 过滤 → 加计数器"；奖励只走"档位 + 领取"（§5）。
5. **玩法**：`PlayMode` + `BattleRuleSet` 纯参数，替掉引擎里 `IsPveMatch()` 一个布尔撑起来的一堆规则；battle 按 `CreateBattleRequest` 里本来就有的 `(match_mode, battle_config_id)` 自查表，不加新 wire 字段（§6）。
6. **玩家自定义模板**（类网易狼人杀自定义板子）：可以做，但只开放"对局规则"（自建房 / 带板切磋）和"自动战斗策略"（角色与宝宝挂机出招）两类，**活动与奖励类绝不开放**。自定义对局一律 `MATCH_MODE_CUSTOM`，无奖、无损、不计分。官方板子是主入口，第一期只做"官方板子 + 带板切磋"，不建模板服务、不开自由文本；之后再上自建房、玩家板子（全局服务 `go/player_template`，非作者只能凭分享码访问）、宝宝优先的自动战斗策略。使用时按值冻结进对局（§8）。
7. **兼容纪律**：所有新列的 0 值 = 现行行为；v1 不新增任何随机数抽取点；存量同种子回放逐字节不变。
8. **先还债再加功能**：战斗配表指纹默认只告警、跨语言战斗金样没建、Java 已落后 C++ 两处战斗修正（PVE 人数档、装备属性）——这三件事不先处理，可配置化只会把两版越拉越远（§10 第 1 步）。

---

## 1. 现状

### 1.1 战斗

- [C] 回合战斗跑在 battle 节点的纯逻辑确定性引擎 `TurnBattleEngine`：输入快照 + 指令 + 种子，输出事件流；随机只走种子 RNG（`mt19937_64`），**消费顺序就是语义**（`turn_battle_constants.h` 的 `RollPercent` 注释段）。Java 版 `game-battle-engine` 逐位照搬。
- [C] 非玩家单位的出手决策全部在 `TurnBattleEngine::FillDefaultActions`：怪物、超时 / 挂机玩家一律"普攻 + target 0"，结算时由引擎 RNG 随机挑存活敌人；只有宝宝（`BATTLE_ACTOR_TYPE_PET`）按快照顺序取第一个能放的技能，目标写死 `enemies.front()`。
- [C] 怪物不带技能（`AppendMonsterActor` 不设 `skill_table_ids`），`MonsterTable` 只有属性、经验 / 金币奖励和掉落列（没有技能或 AI 列）。设计文档自述"怪物 AI 用技能未做（只普攻）"（turn-based-battle-server.md §15.4）。
- [C] 没有目标策略、技能优先级、阶段转换、Boss 机制。
- [C] 技能 = 固定列 + `damage` 表达式 + `effect[]` 挂 buff id；buff 按 `buff_type` 走手写 switch（`TurnBattleEngine` 的 buff 周期效果段、`buff.cpp`），回合侧只有 8 种类型生效。buff 类型编号至少有 4 份手抄副本（`buff_table.proto` 注释、`buff.h`、`turn_battle_constants.h`、Java `BattleConstants.java`）。
- [C] 散落常量：普攻基础伤害 10、PVP 伤害系数 0.3、逃跑概率、暴击 ×2、连击 / 反震 50%、基础命中 100、PVP 道具限 5 次、缺省回合上限 300（均在 `turn_battle_constants.h`）。
- [C] battle 节点所有房间共用一个 muduo loop 线程（`battle_room_manager.h` 注释），每回合结算是同步 CPU 段。单局规模：每队玩家 ≤5（`kMaxBattleTeamSize`）、怪物 ≤10（`kMaxScaledPveMonsterCount`），连宝宝约 20 个 actor。文档里没有每回合 CPU 预算（capacity-model-hok-scale.md 写 battle 池"未建模"）。
- [C] Lua 5.4（third_party 里是 5.4.6）+ sol2 在代码里只剩 `thread_local sol::state tlsLua` 的一处声明（lua_state.h）和一处定义（lua_state.cpp），没有调用方。lua_state.cpp 和 sol2/lua 头文件路径仍挂在 thread_context 等工程的构建里，但 Lua 运行时库本身没有接进构建（vs2022 工程不在 game.sln，也没有任何工程链接它）。

### 1.2 活动

- [C] **两套互不相通的模型**：
  - scene（C++）：活动 = `mission_type == 2` 的任务行 + `ActivitySchedule` 一个绝对时间窗 `[start, end)`（UTC 毫秒，不自动顺延）。进度存在 `player_database.mission_component`，完成位永久有效，没有日 / 周重置。活动 15/16/17 都未排期。
  - go/guild：`GuildActivity.type` 取 1/2/3，每个 type 一个专用 RPC 和事务；个人次数按游戏日（UTC+8 05:00）计，帮会进度按档期计。
- [C] `mission_type == 2` 魔法数散在 `player_activity_schedule.cpp`、`player_mission.cpp`、`player_feature_snapshot.cpp` 多处。
- [C] 条件事件源只有杀怪 / 升级 / 完成任务三类（`PlayerMissionSystem` 的 `HasProgressSource` 白名单）；限时条件（`valid_duration ≠ 0`）直接拒绝；奖励只有物品、没有货币。
- [C] 不可参与原因是写死的中文串，没用 tip 码；`PlayerActivityInfo` 的 name / description / icon_key 没有配表来源，始终为空。
- [C] 没有活动服务（`ActivityNodeService` 只是预留）、没有阶段流程、没有活动计分接线；C++ 侧没有游戏日实现（Go 有 `go/shared/gameday`，Java 已对齐）。

### 1.3 玩法

- [C] 没有统一的玩法模板，三套类型枚举各自 switch：scene_manager 的 `SceneType`（只决定节点池和销毁策略）、match 的 `MatchMode`（凑满人数、分队、评分）、帮会活动 type。
- [C] 引擎只靠 `IsPveMatch()` 一个布尔切换刷怪、能否逃跑、PVP 伤害系数 0.3、道具限 5 次。胜负判定 `UpdateOutcome` 对所有模式相同："全灭，或回合打满判进攻方负"——这条 PVE 规则漏到了 PVP，match 侧专门打了 `RatingDrawRoundCap` 补丁。
- [C] match 的人数口径写了两遍（`joinqueuelogic.go` 与 `matcher.go` 的 `requiredPlayers`）；PVE 人数档放在 yaml 的 `PveTeamSizeByConfigId`，没用 `DungeonTable.max_team_size`；C++ 手抄了 MatchMode 数值 4/5。
- [C] "副本"实际全部走 battle 节点回合制；实时场景副本没接线（`SceneInfoComp.dungeon_config_id` 生产环境恒为 0），`MirrorTable` 没有任何读取方。

### 1.4 配表管线的能力边界

- [C] 导表器是 `tools/data_table_exporter`（不是 `tools/proto_generator`，后者是 RPC 生成器）。权威 schema 是 `data/schema/<sheet>_table.proto` + `cfg_*` option。
- [C] 只支持五种字段形态：标量、定长数组、集合、map、一层子消息数组（`schema_proto.py` 的 `_infer_shape`）。**不支持 oneof、enum、单个或嵌套子消息，没有 JSON 列**——一列放不下"不同类型参数结构不同"的多态参数。`data/AGENTS.md` 说"proto 里可以直接写 enum"与实现不符（解析器会拒绝 enum 块），需另行勘误。
- [C] 导表期 fail-closed 校验：FK / GFK 结构和取值、主键重复、tip 引用、protobuf 类型与溢出。**没有**枚举取值、数值区间、条件外键（"type=3 时某列必填且是外键"）这几类；这些靠各服务运行期手写，口径还不统一（Go guild 坏表拒启，C++ `PlayerEquipSystem::ValidateTables` 只打 ERROR）。
- [C] 已知会静默丢数据的两处：repeated 标量列里的 0 / -1 / 空串会被当空值丢掉（`excel_reader.py`），下标随之错位；float / double 单元格解析失败时静默返回 None。
- [C] 子消息字段上写 `cfg_fk` 不生效（`_element_subfields` 只读 `cfg_col`）。`cfg_composite` 词表和模板都支持，但 0 张表在用，首次使用要单独验证。
- [C] **热更没落地**：C++ / Go 都生成了 `ReloadTables`（Java 没有这个函数，只生成不可变快照 `ConfigTables.load()`，约定热更时整体替换引用）。三端的生产代码都没有重载调用方。C++ 是逐表替换、旧快照当场析构，单表失败直接 `LOG_FATAL`；Go 的 `ReloadTables` 用裸赋值替换包级全局变量。改表 = 重新导表 + 滚动重启。
- [C] 常量生成链路是现成的：`constants_name` 列（`cfg_owner = "constants_name"`，先例 `actoractionstate_table.proto`）→ `constants_gen.py` → C++ `cpp/generated/table/code/constants/*.h` → Java 经 ContractSync 得到 `TableConstants`。注意生成的只是平铺 constexpr，**没有 `kCount`**。
- [C] 每加一张新表，程序都要改 `cpp/generated/table/table.vcxproj`（逐文件显式列出，CMakeLists 由它生成）。所以"新表"是程序工作，只有"新行"才是策划自助。

### 1.5 双版本

- [C] 配置表数据与 schema 是两版共享的客户端契约（AGENTS.md §12）。Java 用 ContractSync 同步同一批 `.pb`、manifest 和 schema，访问代码由自己的 game-table-codegen 在编译期生成（TableSchemaReader 读 schema，TableSourceGenerator 出代码），加载时重做 sha256 / FK / tip / 表达式校验，fail-closed，每局绑定开局时的配表快照。
- [C] 战斗跨语言金样没建（Java `PARITY.md` 记作"待 mmorpg"），两版已经漂移：C++ 的 PVE 人数档（`19a3b8980`）和装备属性（`10cca6b66`：连击 / 反击 / 反震 / 抗异常与 BATTLE_EVENT_RESIST / hit_kind），Java 都待跟（PARITY 登记了这两处）。
- [C] 表达式列：C++ 用 exprtk（`random()` 走全局 `rand()`，每次求值都重新编译，不检查编译失败），Java 手写子集解析器，Go 完全不支持。
- [C] 活动和玩法逻辑在本仓大量落在 Go（go/guild、go/match），所以"两版"实际是 C++、Go、Java 三种运行时。

---

## 2. 选型：为什么是表驱动原子组合

三套整体方案的评审打分（1–10，两个评审视角取均值，保留到 0.5）：

| 方案 | 战斗策略 | 活动 | 玩法 | 决定性问题 |
|---|---|---|---|---|
| A 表驱动原子组合 | **7.5** | 6 | **7** | 加新原子要写代码（且两版各写一份）；这是本方案的边界 |
| B Lua 脚本做模板 | 3.5 | 4 | 5.5 | 两版解释器语义不一致（C++ PUC 5.4 有整数子类型，Java 只有 5.2 语义的 LuaJ）；共享 `lua_State` 的 GC 受同 loop 其他房间影响，破坏逐字节回放；Lua 运行时库未接进构建（只残留一个不被引用的 lua_state.cpp 和 sol2 头文件路径）；LuaJ 大概率达不到 Java 版"≥2 万 star"选型规则 |
| C 节点图 + 事件-条件-动作 | 6 | 6.5 | 6.5 | xlsx 里拼树难读难 diff，可视化编辑器要到很后期；示例里漏算了随机数消耗点 |

采用 A 的主干，嫁接 C 的三样东西：**原子字典带参数规格列**（导表期通用校验）、**DecisionTrace**（线上排障）、**每局绑定编译快照**。活动的数据模型取 C 的"模板 / 实例 / 档位"三层，但触发器做减法（扁平行，不要条件图 / 动作图）。

不选 B 的其他理由：活动的事实源只有三类，脚本能多出来的表达力很有限，却把玩家资产路径交给两套解释器；脚本实际是程序在写，不是策划。

不选行为树的理由：回合制每回合只做一次瞬时决策，用不到行为树的 Running 状态和黑板；优先级规则表最容易读、最容易 diff。

**重新评估的条件**：原子数量真的失控（同类原子 > 30 且仍频繁新增），并且跨语言战斗金样已经建好、两版在同一版本上逐字节一致。

**原子准入纪律**：每个新原子至少要有 2 个真实配置实例在用才允许加，防止字典表慢慢演变成一门写得很别扭的脚本语言。

---

## 3. 共用底座

### 3.1 原子字典表

每个领域一张字典表：`BattleAtomKind`、`ActivityEventKind`、`PlayModeKind`。字典是编号的唯一来源，`constants_name` 列生成三端具名常量，替掉"枚举只写在注释里、四份手抄"的做法。凡是引用原子的列一律 `cfg_fk` 到字典表，"种类不存在"在导表期 fail-closed 拦下（FK 校验把 0 / -1 / 空视为未填）。

字典表直接带**参数规格列**，由导表器的通用校验器按规格校验，不为每个 kind 单写 Python：

```proto
// 形状示意：每个领域的字典表都带这几列
uint32 category  = 2;   // 闭集：条件 / 目标 / 行动 / ……，校验"这一列只能填哪类原子"
int64  p1_min = 3; int64 p1_max = 4;
int64  p2_min = 5; int64 p2_max = 6;   // min = max = 0 表示不用这个参数，填了非 0 即报错
string ref_table = 7;   // 需要引用的表（"Skill" / "Buff" / 空），导表器据此做条件外键
bool   consumes_rng = 8;// 会消费引擎 RNG（只对战斗字典有意义）
int32  constants_name = 9001 [(cfg_owner) = "constants_name"];
int32  design_note    = 9002 [(cfg_owner) = "designer"];
```

### 3.2 参数列口径（硬约束）

多态参数一律写成"**类型列 + 具名标量参数列 + 单独的引用列**"：`xxx_kind` + `xxx_p1` / `xxx_p2`（int64，统一用整数或万分比）+ `skill_id` / `buff_id` 这类具名 FK 列。组合关系用主表 + 子表表达：子表自带 id，`parent_id` 同时标 `cfg_fk` 与 `cfg_index`（先例 `attributedimension_table.proto`），顺序放在显式的 `order` / `priority` 列（§11.6：集合下标不得承载业务语义）。

禁止的写法：

| 写法 | 原因 |
|---|---|
| repeated 槽位存参数 | 0 / -1 / 空串会被丢弃，下标错位，而且无报错 |
| double / float 参数列 | 解析失败静默丢格；跨语言浮点 1 ulp 风险 |
| exprtk 表达式列 | `random()` 走 `rand()` 绕过种子 RNG；每次求值重编译；Go 不支持 |
| STRUCT_LIST 子字段上挂 FK | 子字段上的 `cfg_fk` 不生效；半空槽会被并进上一槽 |
| `map<string,string>` 参数包 | 展开成键值字符串列，FK 校验不了，两套校验器（Python 导表器 + Java `TableSourceGenerator` 生成的加载期校验）都要新写 |

### 3.3 代码侧分发

每个领域一张**平铺的处理器常量表**，每行 `{生成的 kind 常量, category, Validate, Eval/Apply}`，用 constexpr 做唯一性检查（§11.2：普通枚举 + 平铺常量表，不用 X-macro，不建继承树；参考 `guid_segment_registry`）。

因为生成的常量没有 `kCount`，"字典行 ↔ 处理器行一一对应"**不能**用 `static_assert` 守住，只能在启动期核对：字典删了一行，生成常量随之消失，C++ / Java 编译期报错；字典多了一行而没写处理器，启动期拒绝启动。

### 3.4 三层校验

1. **导表期**（主力）：现有 FK / PK / tip fail-closed，再在 `orchestrator.py` 的 `run()` 校验槽里挂一个**按字典规格的通用校验器**：列角色（这一列只能填哪个 category）、参数区间、条件外键、不用的参数必须为 0。失败则产物不落盘，策划在导表那一步就能看到哪张表哪行哪列错了。Java 侧在 `game-table-codegen` 的 `TableSourceGenerator` 生成的加载期校验段（与 `checkForeignKeys` / `checkTipReferences` 同处，运行时辅助在 `game-table` 的 `com.game.table.load`）同步实现同一套规则；`TableSchemaReader` 只在编译期读 schema。
2. **启动期**（最后一道防线）：C++ 放在节点的 `TableLoadHandler`（scene / battle 的 `main.cpp`），Go 照 `go/guild/internal/activity/config.go` 的写法：`ValidateTables` 读全局表后交给纯函数 `validateRows`，由 `guild.go` 用 `logx.Must` 拒启，Java 放在各领域加载器。注意：battle 现在的 `TableLoadHandler` 只打 `LOG_ERROR`，改成拒启会把爆炸半径放大到整个 battle 池——所以错误必须尽量在导表期就拦下，启动期只兜底。
3. **运行期**：战斗靠 `CheckActionPrerequisites` 与 `ExecuteSkill` 出手前复校验、失败降级；活动照 guild `PickForWrite` 的写法，只接受当前处于开放状态的实例。

### 3.5 事件与计时

- 不新建事件总线。战斗引擎是纯逻辑库（零网络、零 ECS、零 timer；回合结算路径不打日志，只有 Initialize 阶段的开局校验失败时有 4 处 LOG_ERROR），条件在回合开始时轮询求值。
- scene 侧条件事实只走 `ConditionEvent` → `PlayerMissionSystem::OnCondition` 这一个入口；活动在它旁边**直接调用**，不走 dispatcher 订阅（ecs-component-access-rules：必需步骤直接调用，dispatcher 只做可有可无的通知）。
- 战斗里的冷却、持续、阶段一律按回合计，不起 timer。
- 活动的开放状态和阶段由纯函数 `StateOf(表行, now)` 惰性算出（先例 `PlayerActivityScheduleSystem::BuildInfo` 的纯计算重载、guild `rules.go` 的 `StateOf`），不为每个活动起定时器。`now` 每个请求只取一次，作为参数传入。
- 结算、封榜、全服发奖这类必须有人主动推进的阶段，由拥有者服务的巡检幂等推进（先例 guild 巡检器）。

### 3.6 版本、指纹与热更

- 模板在加载期**编译**成不可变的派生索引，运行时只持有 id 或 `shared_ptr<const 编译集>`，**不存表行指针**（data/AGENTS.md：C++ 热更时旧快照当场析构）。
- 每个战斗房间开局时绑定当时的编译快照，与 Java"每局绑定开局配表快照"对齐，为将来整批热更铺路。
- 战斗相关的新表一律**追加到** `BattleTableFingerprint` 的末尾（不能插中间），Java 按同样顺序追加；每张新表都会改变指纹，scene / match / battle 必须同批发布。
- **指纹默认只告警，不拦截**：match `TableFingerprintMode` 默认 `warn`（`config.go` 与 `match_service.yaml`），battle 默认 `FingerprintMode::Warn`（`battle_room_manager.cpp`）。"新表进指纹"在当前配置下保证不了表版本一致——必须先排计划切到 `enforce`（§10 第 1 步）。
- v1 不做热更。热更作为独立决策，前提三条：整批编译成功后才原子替换；每局绑定开局快照；启动日志打出 `manifest.json` 的 version / digest（现在只有 Java 消费 manifest）。

### 3.7 灰度与止血

- 所有新列的 0 值 = 现行行为：`Monster.strategy_id = 0`、`PlayMode` 无匹配行 = 现行规则、`ActivityInstance.enabled = false`。新旧版本并存窗口内逐位兼容，存量回放基线不平移。
- 新实例靠 `enabled` + 排期窗逐行打开。v1 没有按 zone 或按玩家比例的灰度（活动表没有 zone 维度）。
- battle 节点加一个**节点级开关**（环境变量，先例 `BATTLE_TABLE_FINGERPRINT_MODE` 覆盖），强制所有怪物按 `strategy_id = 0` 处理，用来止血。它和改表一样要重启，但不用重新导表。
- Go 的 `killswitch` 只能当 RPC 止血阀；它出故障时 fail-open，**不能**拿来判定活动是否开放或谁有发奖资格。

### 3.8 可观测性

- battle 和 scene 两个 C++ 进程**没有 Prometheus 端点**（`battle_room_manager.cpp` 注释："由日志侧提取计数"）。每次决策打一行日志会随"房间数 × 怪物数 × 回合数"爆炸。
- 做法：进程内按低基数维度聚合计数（`atom_kind`、`outcome`、`activity_event_kind`、`result`），每 60 秒打一行 `metric=battle_ai_decisions ...` 结构化日志，写法照 `battle_settlement_pending_backlog`。label 不带 `player_id`、`guild_id`、`strategy_id`、实例 id（AGENTS.md §9）。
- 战斗求值器返回 **DecisionTrace**（走过的阶段 / 规则 id 序列），引擎的回合结算路径不打日志；battle 节点按 debug 开关输出，用于回答"Boss 为什么放了这招"。
- 导表时额外产出一份**可读的规则渲染报告**（如"策略 900 / 阶段 2 / 规则 10：若存活敌人 ≥ 3 则施放 横扫(5002)"），供评审和 diff。

---

## 4. 战斗策略模板

### 4.1 模型

- **原子**（代码注册）：条件、目标选择、行动三类；v1 不做效果原子，阶段效果复用 buff。
- **模板 = 实例**：`BattleStrategy` 一行，下挂 `BattleStrategyPhase`（阶段，只进不退）和 `BattleStrategyRule`（规则，priority 小的先判）。
- **挂载**：`MonsterTable` 新增 `strategy_id`，0 = 现行写死行为（普攻 + target 0 + 结算期 RNG 选目标）。一份策略可以挂给多种怪。
- **分层**（借 §11.6 的思路）：Monster 表回答"它是什么"（属性、掉落），策略表回答"它怎么出手"。怪物的技能集**不另开** `Monster.skill` 列，由策略引用的 `skill_id` 派生，避免同一件事两份来源。
- **复用同一参数**：规则的阈值参数允许引用策略实例上的具名参数槽（如 `BattleStrategy.param1..4`），"同一套打法只改一个阈值"不必复制整份策略。

### 4.2 表结构（示意，字段号以落码时为准）

```proto
// data/schema/battleatomkind_table.proto —— 战斗原子编号的唯一来源
message BattleAtomKindTable {
  option (cfg_sheet) = "BattleAtomKind"; option (cfg_source_file) = "BattleAtomKind.xlsx"; option (cfg_primary_key) = "id";
  uint32 id = 1;              // 原子编号，上线后不复用
  uint32 category = 2;        // 1 条件 2 目标 3 行动（闭集）
  int64  p1_min = 3; int64 p1_max = 4;
  int64  p2_min = 5; int64 p2_max = 6;   // min = max = 0 表示不用
  string ref_table = 7;       // "Skill" / "Buff" / 空
  bool   consumes_rng = 8;    // v1 只有 RandomEnemy 为 true，且只许配普攻
  bool   player_selectable = 9; // 是否允许玩家自定义策略选用（§8），默认 false；为 true 时 p 区间须落在 [0, 2^21)（§8.3）
  int32  constants_name = 9001 [(cfg_owner) = "constants_name"];
  int32  design_note    = 9002 [(cfg_owner) = "designer"];
}

// data/schema/battlestrategy_table.proto —— 策略主表
message BattleStrategyTable {
  uint32 id = 1;
  uint32 fallback_target_kind = 2 [(cfg_fk) = "BattleAtomKind"]; // 全部规则落空时普攻的目标；0 = 旧行为
  int64  param1 = 3; int64 param2 = 4; int64 param3 = 5; int64 param4 = 6; // 规则可引用的具名阈值
  int32  design_note = 9001 [(cfg_owner) = "designer"];
}

// data/schema/battlestrategyphase_table.proto —— 阶段子表
message BattleStrategyPhaseTable {
  uint32 id = 1;
  uint32 strategy_id = 2 [(cfg_fk) = "BattleStrategy", (cfg_index) = true];
  uint32 phase_order = 3;                                // 同策略内 1..N 连续，显式
  uint32 enter_kind  = 4 [(cfg_fk) = "BattleAtomKind"];  // 进入条件（category = 条件）；第 1 阶段必须 0
  int64  enter_p1 = 5; int64 enter_p2 = 6;
  uint32 enter_buff_id = 7 [(cfg_fk) = "Buff"];         // 进入时挂给自己；0 = 不挂
}

// data/schema/battlestrategyrule_table.proto —— 规则子表（条件 → 行动）
message BattleStrategyRuleTable {
  uint32 id = 1;
  uint32 phase_id = 2 [(cfg_fk) = "BattleStrategyPhase", (cfg_index) = true];
  uint32 priority = 3;                                   // 阶段内唯一，小者先判
  uint32 cond1_kind = 4 [(cfg_fk) = "BattleAtomKind"]; int64 cond1_p1 = 5; int64 cond1_p2 = 6;
  uint32 cond2_kind = 7 [(cfg_fk) = "BattleAtomKind"]; int64 cond2_p1 = 8; int64 cond2_p2 = 9; // AND；0 = 恒真
  uint32 action_kind = 10 [(cfg_fk) = "BattleAtomKind"];  // Attack / CastSkill / Defend
  uint32 skill_id    = 11 [(cfg_fk) = "Skill"];           // 仅 CastSkill 必填，其余必须 0（导表期条件校验）
  uint32 target_kind = 12 [(cfg_fk) = "BattleAtomKind"]; int64 target_p1 = 13;
}

// monster_table.proto 纯追加（现有字段 1–10、9001）
uint32 strategy_id = 11 [(cfg_fk) = "BattleStrategy"];   // 0 = 现行普攻随机

// proto/battle/battle_data.proto —— 引擎内部状态，不进客户端可见的 BattleActorState
message BattleAiState { uint64 actor_id = 1; uint32 phase_order = 2; }
```

v1 原子集合（每个都要有 ≥2 个真实实例才加）：

| 类别 | 原子 | 参数 | 是否消耗 RNG |
|---|---|---|---|
| 条件 | `SelfHpBelowPermyriad` | p1 = 万分比 | 否 |
| 条件 | `RoundEveryN` | p1 = N，p2 = 起始回合 | 否 |
| 条件 | `AliveEnemyCountAtLeast` | p1 = 人数 | 否 |
| 目标 | `RandomEnemy` | — | 是（复用现有 target 0 + 结算期 RNG，**只许配普攻**） |
| 目标 | `LowestHpEnemy` / `HighestHpEnemy` | — | 否，同分按 (hp, actor_id) 升序 |
| 目标 | `FirstEnemy` / `Self` / `LowestHpAlly` | — | 否 |
| 行动 | `Attack` / `CastSkill` / `Defend` | — | 否 |

### 4.3 运行时

**C++（battle 节点，唯一接缝 `TurnBattleEngine::FillDefaultActions`）**：

1. actor 是 MONSTER 且 `strategy_id ≠ 0` 时：先推进阶段（只进不退，越过的每个阶段按 `phase_order` 依次挂 `enter_buff`），再按 priority 逐条求值。
2. 条件为真的规则构造 `BattleAction`，送进现有的 `CheckActionPrerequisites`；通过就采用，不通过看下一条；全部落空走 `fallback_target_kind` 或旧路径。
3. 出手时 `ExecuteSkill` 照常复校验，失败降级。
4. 新模块 `cpp/libs/services/battle/system/battle_strategy.{h,cpp}`：纯逻辑，不 include scene，不打日志。内容是平铺处理器常量表 + `CompiledBattleStrategy`（加载期编译出的只读索引，阶段和规则已排序，附派生技能集）。
5. `BattleDataProvider` 新增 `FindStrategy(id)`；生产实现在 `TableLoadHandler` 里编译一次，单测用 `MemoryBattleDataProvider` 喂内存数据。
6. `AppendMonsterActor` 在 `strategy_id ≠ 0` 时把派生技能集写进 `actor.skill_table_ids`，这样 `CheckActionPrerequisites` 的"技能必须在快照列表里"、冷却、沉默判定都直接复用。
7. 阶段状态存成 `BattleAiState` proto（turn_battle_engine.h：引擎状态由 proto 承载，不写平行 struct）。

**Java（game-battle-engine）**：在 `TurnBattleEngine.fillDefaultActions` 同一位置接入；`BattleData` 增加 `findStrategy`；`TableBattleData` 加载期编译并按同一口径 Validate；`BattleTableFingerprint` 按同序追加；原子编号用 `TableConstants`。

### 4.4 确定性硬约束

- **v1 不新增 RNG 抽取点**：规则按优先级确定性选取；目标同分按 (hp, actor_id) 升序；"随机敌人"只给普攻用，走现有 target 0 路径。`turn_battle_constants.h` 里记录的消费顺序不变。
- **v1 禁止任何作用于敌方的 buff 原子**：给敌方挂异常类 buff（毒 50 / 冰冻 52 / 眩晕 30 / 沉默 31）会走 `AddBuffToActor → RollAilmentResist`，有效抵抗率在 1..99 时调用 `RollPercent` 消耗随机数；`CastSkill` 引用技能的 `effect[]` 打在敌方目标上同样经 `ApplySkillToTarget → AddBuffToActor` 走这条路；而抗异常是 Java 还没追平的 C++ 修正，两版会在同一配置下分叉。v1 阶段 buff 只许挂给自己（同队施加不触发抗异常掷骰）。等 Java 追平、并把消耗点登记进 Java battle-engine-spec §8.4 之后再开放。
- **导表期据此校验两条**：`CastSkill` 规则的 `target_kind` 不得为 0（`ValidateSkillTarget` 对 targeting_mode 非空的技能一律拒绝 target_id = 0，AOE 也不例外）；v1 里 `CastSkill` 引用技能的 `effect[]` 不得含毒 / 冰冻 / 眩晕 / 沉默类 buff。
- **阶段事件必须放在事件组清零之后**：`FillDefaultActions` 在 `ResolveCurrentRound` 里早于 `currentGroupId = 0` 执行。阶段推进若在里面发 buff 事件，group_id 会沿用上一回合末的组计数（第 1 回合为 0，先 BeginEventGroup 则为上一回合末 +1），与本回合从 1 重新编号的行动组撞号或乱序；且 `FillDefaultActions` 目前不接收 `TurnResultS2C`，发事件需先改签名。要么调整清零顺序，要么把阶段推进移到清零之后。
- 同优先级权重随机是**新的 RNG 抽取点**：要在 `turn_battle_constants.h` 与 Java spec §8.4 登记位置并刷新基线，作为单独决策，不进 v1。

### 4.5 热路径上限

battle 所有房间共用一个 loop。上限：每策略阶段 ≤ 8、每阶段规则 ≤ 16、每规则条件 ≤ 2。单局约 20 个单位，每回合求值是万次量级的整数比较，不分配内存。导表期校验上限；Codex 跑满配基准后把结果写回本节。

### 4.6 示例：三阶段 Boss「山魈王」（Monster 9001，策略 900）

| 阶段 | 进入条件 | 进入时 | 规则（priority 小先判） |
|---|---|---|---|
| 1 | — | — | p10：`RoundEveryN(3,1)` → 放横扫（AOE，不耗蓝）；p20：→ 普攻 `LowestHpEnemy` |
| 2 | `SelfHpBelowPermyriad(6000)` | 给自己挂回血 buff（type 40，回合侧已生效） | p10：`AliveEnemyCountAtLeast(3)` → 横扫；p20：→ 裂地（单体高伤，effect[] 不含异常 buff）打 `LowestHpEnemy` |
| 3 | `SelfHpBelowPermyriad(2500)` | — | p10：`RoundEveryN(2,0)` → 重击 `LowestHpEnemy`；p20：→ 普攻 `RandomEnemy` |

走一遍：开局 `AppendMonsterActor` 把 {横扫, 裂地, 重击} 写进 `skill_table_ids`。第 1 回合 `RoundEveryN(3,1)` 为真，横扫过校验被采用（前提：规则配了非 0 的 target_kind，如 `FirstEnemy`；`ValidateSkillTarget` 对 targeting_mode 非空的技能一律拒绝 target_id = 0，AOE 也不例外）。第 2 回合落到 p20，普攻血最少的玩家，同血取 actor_id 小者，不耗 RNG。HP 降到 55% 后的下一回合开始时推进到阶段 2，挂回血 buff，`BattleAiState.phase_order = 2`。裂地冷却中就回落 fallback。（v1 不能用"毒刺"这类 effect[] 带毒 / 冰冻 / 眩晕 / 沉默的技能打敌方：它会经 `ApplySkillToTarget → AddBuffToActor → RollAilmentResist` 消耗随机数，见 H3。）若一回合内 HP 从 70% 掉到 20%，下回合依次挂上阶段 2、3 的进入 buff，直接进阶段 3。Boss 被眩晕时照旧跳过行动。

注意：Monster 表没有法力列，`CheckSkillCost` 会拦下有消耗的技能——策划必须给 Boss 配无消耗技能，或另行补怪物法力列。

### 4.7 待拍板

- 派生技能集写进 `skill_table_ids`，而它同时是"客户端可见的本局可用技能列表"——Boss 后期阶段的技能会在开局就暴露给客户端。接受，还是另开一个服务端内部列表？
- 宝宝（`Pet.strategy_id`）是否同批接入，替换"取第一个可用技能"。

---

## 5. 活动模板

### 5.1 模型

按"它是什么"和"它何时开"拆开（§11.6 的思路）：

- **模板** `ActivityTemplate`：规则本身——周期口径、准入、阶段列表、计数器、触发器。
- **实例** `ActivityInstance`：一期排期——模板 id、enabled、UTC 毫秒 `[start, end)`，口径沿用 `ActivitySchedule`。
- **档位** `ActivityRewardTier`：挂在实例上，"计数器达到 N 领 Reward X"。同一套规则换节日、换奖励、换门槛，不必复制模板；"再开一期"只填实例一行加档位。

范围：

- v1 只做**个人维度**，跑在 C++ scene 和 Java game-scene。
- 帮会活动（`GuildActivity.type` 1/2/3）保持现有 Go switch 不动，等出现第 4 种帮会活动再迁（YAGNI）。
- 现有"任务型活动"（`mission_type == 2` + `ActivitySchedule`）保留，与新活动并存，`GetActivityList` 合并输出两者；之后再把活动 15/16/17 迁成新模型并退掉魔法数。

### 5.2 表结构（示意）

```proto
// activityeventkind_table.proto —— 活动事件原子字典（映射到 ConditionEvent 的类别）
message ActivityEventKindTable {
  uint32 id = 1;                 // 杀怪 / 升级 / 完成任务 …… 只收录已有事实源的类别
  uint32 condition_category = 2; // 对应 Condition.condition_category
  int32  constants_name = 9001 [(cfg_owner) = "constants_name"];
}

// activitytemplate_table.proto
message ActivityTemplateTable {
  uint32 id = 1;
  uint32 period_kind = 2;        // 0 整档期 1 游戏日 2 游戏周（UTC+8 05:00 / 周一 05:00）
  uint32 entry_level_min = 3;    // v1 准入只做等级门槛（现行 CheckAccept 没有任何玩家准入门槛（只校验任务状态、条件类型、奖励配置与排期），等级门槛是新增能力）；0 = 无门槛
  int32  design_note = 9001 [(cfg_owner) = "designer"];
}
message ActivityPhaseTable {
  uint32 id = 1;
  uint32 template_id = 2 [(cfg_fk) = "ActivityTemplate", (cfg_index) = true];
  uint32 seq = 3;                // 显式顺序
  uint32 phase_kind = 4;         // 1 进行 2 领奖 3 展示（导表期区间校验）
  uint64 offset_ms = 5;          // 相对实例开始；本阶段在下一阶段起点或实例结束时结束；必须递增
}
message ActivityCounterTable {
  uint32 id = 1;                 // 存档按计数器 id 记，上线后不复用
  uint32 template_id = 2 [(cfg_fk) = "ActivityTemplate", (cfg_index) = true];
  uint64 cap = 3;
}
message ActivityTriggerTable {   // 扁平行：事件 + 过滤 → 加计数器；不要条件图 / 动作图
  uint32 id = 1;
  uint32 template_id = 2 [(cfg_fk) = "ActivityTemplate", (cfg_index) = true];
  uint32 active_phase_id = 3 [(cfg_fk) = "ActivityPhase"];  // 按阶段 id 引用
  uint32 event_kind = 4 [(cfg_fk) = "ActivityEventKind"];
  repeated uint32 event_filter = 5 [(cfg_slots) = 5];       // 各格 ANY；全空 = 不过滤；杀怪 / 完成任务同 Condition.condition1 的等值匹配（升级类别的 condition1 是等级门槛“事件等级 ≥ 任一格”，需单独约定）
  uint32 counter_id = 6 [(cfg_fk) = "ActivityCounter"];
  uint32 amount_kind = 7;        // 1 取事件 amount 2 固定 amount_value
  uint64 amount_value = 8;
}
message ActivityInstanceTable {
  uint32 id = 1;
  uint32 template_id = 2 [(cfg_fk) = "ActivityTemplate"];
  bool   enabled = 3;
  uint64 baseline_start_at_ms = 4;   // UTC 毫秒，左闭右开，与 ActivitySchedule 同口径
  uint64 baseline_end_at_ms = 5;
  string name = 6; string desc = 7; string icon_key = 8;   // 填 PlayerActivityInfo 现在恒空的三字段
}
message ActivityRewardTierTable {
  uint32 id = 1;                 // 存档按档位 id 记已领，上线后不复用
  uint32 instance_id = 2 [(cfg_fk) = "ActivityInstance", (cfg_index) = true];
  uint32 counter_id = 3 [(cfg_fk) = "ActivityCounter"];
  uint64 threshold = 4;          // 导表期校验 ≤ 计数器 cap
  uint32 reward_id = 5 [(cfg_fk) = "Reward"];
  uint32 claim_until_phase_id = 6 [(cfg_fk) = "ActivityPhase"]; // 可领截止阶段
}
```

导表期额外校验：阶段 offset 递增、档位阈值 ≤ cap、同模板启用实例的时间窗不重叠、触发器引用的计数器属于同一模板。

### 5.3 进度存储（硬约束）

- **不要嵌进 `MissionListComp`**。[C] `mission_marshal.cpp` 的 `Unmarshal` 只逐项恢复 missions、mission_begin_time、completed、claimable——往 `MissionListComp` 加字段而不同步改 marshal，**同一版本内**每次加载都会丢它（这与混版本无关）。再配上"缺周期键就当新周期清零"，玩家每次重登都能在同一游戏日里重做、重领——**这是资产复制漏洞**。活动的"计数器 + 档位"与任务的"多目标进度 + 完成位 / 待领位"也不是同一个模型，所以单独成结构。
- 推荐：新持久结构做成 `player_database` 的**顶层新字段** `PlayerActivityComp`：

  ```proto
  message PlayerActivityComp { map<uint32, ActivityProgress> instances = 1; }   // key = instance_id
  message ActivityProgress {
    uint64 period_key = 1;                 // 当前进度所属周期（游戏日 / 周键，或整档期 = instance_id）
    map<uint32, uint64> counters = 2;      // key = counter_id
    map<uint32, uint64> claimed_at_ms = 3; // key = tier_id；值 = 领取成功的 UTC 毫秒
    map<uint32, uint64> carry_claimable = 4; // 换期时保留的"可领未领"档位：tier_id → 达成时的 period_key
  }
  ```

  **勘误（v0.1 初稿写错）**：初稿称"玩家数据按字段存成独立 Redis 键 `player:{id}:<field>`，旧节点不会覆盖新字段"，这不成立。[C] `player:{id}:*` 这族键不在生产读写链上（shared-redis-split-by-purpose.md §2.2、player-storage-placement.md §10.4）；真实链路是 scene → Redis 父快照 `PlayerAllData:{id}` → Kafka DBTask → go/db `savePlayerRow` 用 `tx.Save` **整行写**，写方不认识的列会被写成零值。所以新顶层字段在新旧版本混跑时会被旧 scene / 旧 go/db 抹掉。
  - 上线前（负责人 2026-10-18 确认尚未上线，见 [db-write-fencing-mysql.md](db-write-fencing-mysql.md) 文末「上线状态」）这不是阻塞项：没有旧二进制需要混跑，首次上线整批起步。
  - 上线后再加这类字段，依赖 [mixed-version-persist-field-protection.md](mixed-version-persist-field-protection.md)（第 1 批已验证，后续批次以该文档 §9 为准）；该方案第 1 批已在 `player_database` 落下 21 号 `persist_schema_gen`，22 号也已被 `mystery_merchant` 占用；本字段按加列纪律取落码时的下一个空闲号（当前为 23），声明在 message 末尾。
- 按 ECS 规则：`PlayerActivityComp` 属于持久数据，只在玩家加载时创建；缺了就拒绝服务（返回 `kServiceUnavailable`），不补建（同 `player_feature_snapshot.cpp` 的口径）。读路径（列表、快照）不改组件集合。

备选方案（更快、但改动面更大）：复用 Mission + `ClaimMissionReward(scope = 2)`，客户端协议不变。前提是先改掉 `player_mission.cpp` 里写死 scope 0 的 5 处——`CheckPlayer` 只放行 `kPlayerMission`、`ApplyFollowUps` 的自动领奖和链式接取、`Initialize` 只创建 scope 0 容器、`OnCondition` 只推进 scope 0——否则 scope 2 的自动领奖会去 scope 0 领、失败后只留一行 WARN；周期键仍不能放 `MissionListComp`。本文推荐独立结构，因为活动的"计数器 + 档位"与任务的"多目标进度 + 完成位 / 待领位"不是同一个模型。

### 5.4 阶段与周期

- `PhaseOf(instance, phases, nowMs)` 是纯函数，写法仿照 `PlayerActivityScheduleSystem::BuildInfo` 的纯计算重载；重启不漂移，将来加"按开服日锚定"只是多一种锚点（目前仓里没有开服时间这个事实源，需另补）。
- 周期键只取自游戏日 / 游戏周切点：把 `go/shared/gameday` 的 DayKey / WeekKey 移植成 C++ 纯函数，**三版（C++、Go、Java）共用一份对拍向量**。
- 换期只在写入口惰性执行（`period_key` 与当前不同时）；读路径按"已换期"计算展示，不改存档。
- 时间统一 UTC 毫秒、`[start, end)`；服务端是开放状态的唯一事实源，客户端只负责按 UTC+8 显示。
- 停服顺延按 [activity_maintenance_auto_shift.md](activity_maintenance_auto_shift.md)：基线不变、生效时间全量函数化重算，不在旧 end 上增量加。

### 5.5 领奖（硬约束）

1. 触发器**不直接发物**，只加计数器；奖励一律走"档位 + 领取"。
2. 领取在提交点重新核对：阶段未过 `claim_until_phase_id`、计数器达到阈值、该档未领、实例就是当前期（或在 `carry_claimable` 里）。
3. 核对通过后走 `BagService::AddItems`，新增一个 TX 类型；**发放成功才写 `claimed_at_ms`**，与"发放成功后才清领取权"同序。背包满回 tip 码，状态保持可领。
4. **换期不得吞掉未领奖励**：换期只清计数器；已达成未领的档位转入 `carry_claimable`，在其 `claim_until_phase_id` 截止前仍可领；截止后由巡检转邮件补发（与 guild 活动"奖励绝不跳过"口径一致）。邮件发附件前目标 zone 的 go/db 须已切 `Fencing.Mode=dual`（mail-system.md）。
5. 新增客户端 RPC `ClaimActivityReward`（客户端可见协议，两版同批改）；不可参与 / 领取失败原因在 `Tip.xlsx` 新开 `activity_error` 组发号（建议 base=31000；30000 段已被 `mystery_merchant_error` 占用，以导表器自检为准），替掉写死的中文串。

### 5.6 示例：「国庆·七日讨伐」

- 模板 T100：`period_kind = 0`（整档期），`entry_level_min = 20`；阶段 P1「进行」offset 0，P2「领奖」offset 6 天；计数器 C1「讨伐数」cap 9999；触发器 TR1：`active_phase = P1`，`event_kind = 杀怪`，`event_filter = [9001, 9002]`，加 C1，取事件 amount。
- 实例 I2026：template T100，enabled，2026-10-01 05:00（UTC+8）起 7 天。
- 档位：C1 ≥ 50 → Reward 7001；≥ 150 → 7002；≥ 300 → 7003；`claim_until_phase = P2`。

走一遍：玩家在副本击杀 9001，battle 结算经 Kafka 回到 scene，`ApplySettlement` 构造 `ConditionEvent{杀怪, [9001], 1}`。`PlayerMissionSystem` 照旧处理；`PlayerActivitySystem` 按倒排索引找到 TR1，判断 `PhaseOf = P1`、过滤命中、等级 ≥ 20，C1 加 1、置脏、随下一次存盘落库。第 7 天进入 P2，触发器不再计数，档位仍可领。领 50 档：提交点复核 → `AddItems` 成功 → 写 `claimed_at_ms`。

### 5.7 不在本期范围

- **限时累充**：结构完全一样（换 `event_kind`），但本仓没有充值事实源（支付节点只是预留）。必须先由支付链路按订单号幂等地发 `ConditionEvent`，这属于玩家资产 fail-closed 路径，单独设计。
- **排行型活动**：接 go/rank，新增 `ActivityRankReward` 表替代 Freeze 请求里传入的档位，经 mail 发奖。
- 跨服活动、按 zone 排期、按开服日锚定、对话 / 使用物品 / 交互类事件源（事件源先要有真实写点）。

---

## 6. 玩法模板

### 6.1 模型

- **模板**：`PlayModeKind` 字典一行，对应代码注册的凑局处理器：`InstantSolo`（PVE 单人即开）、`QueueFifo`（按 队伍数 × 每队人数 先到先凑满）、`QueueRatedSnake`（按评分蛇形分队，由 5v5 写法推广到任意每队人数）、`Challenge`（点名切磋）、`GuildActivityBattle`（帮会历练）。
- **实例**：`PlayMode` 一行，把 match_mode、battle_config_id、人数区间、是否计分、规则集串起来。
- **规则集**：`BattleRuleSet` 纯参数，替掉 `IsPveMatch()` 的各个读点与散落常量。

### 6.2 表结构（示意）

> **2026-10-24 更正**：`PlayMode` 已按最小子集先行落档，字段编号以 `data/schema/playmode_table.proto` 为准（`id=1, match_mode=2, battle_config_id=3, enabled=4, team_size_max=5`），见 [global-match-hierarchical-pools.md](global-match-hierarchical-pools.md) §7。下方示意里的 kind / rated / rule_set 等列以后作为新字段号追加，不按示意重排现有编号。

```proto
// playmodekind_table.proto —— 凑局处理器字典
message PlayModeKindTable {
  uint32 id = 1;
  uint32 team_count = 2;   // 处理器要求的阵营数：1 = PVE，2 = 对战（启动期与实例比对）
  int32  constants_name = 9001 [(cfg_owner) = "constants_name"];
}

// playmode_table.proto —— 玩法实例；(match_mode, battle_config_id) 唯一（启动期校验，或首次验证 cfg_composite 后改为复合键）
message PlayModeTable {
  uint32 id = 1;
  uint32 kind = 2 [(cfg_fk) = "PlayModeKind"];
  uint32 match_mode = 3;             // proto MatchMode 数值（客户端契约）
  uint32 battle_config_id = 4 [(cfg_fk) = "Dungeon"];
  uint32 team_size_min = 5; uint32 team_size_max = 6;   // ≤ 5（D14）；替代 yaml PveTeamSizeByConfigId
  bool   rated = 7;                  // 替代 isRatedMode
  uint32 rule_set_id = 8 [(cfg_fk) = "BattleRuleSet"];
  bool   customizable = 9;           // 是否允许玩家自定义规则覆盖（§8.6.2），默认 false
}

// battleruleset_table.proto —— 引擎规则参数（进战斗配表指纹）
message BattleRuleSetTable {
  uint32 id = 1;
  bool   spawn_monsters = 2;               // 现 IsPveMatch 的刷怪分支
  bool   flee_allowed = 3;
  uint32 damage_scale_permyriad = 4;       // 10000 = 1.0；3000 = 现 PVP 0.3
  uint32 item_use_cap = 5;                 // 0 = 不限（现 PVE）；5 = 现 PVP
  uint32 max_rounds = 6;                   // 0 = 沿用 Dungeon.time_limit 换算（现行）
  uint32 round_cap_outcome = 7;            // 1 进攻方负（现行） 2 平局
  bool   scale_monsters_by_players = 8;    // 现 PVE 人数档
  uint32 deadline_seconds = 9;             // 0 = 沿用 match 全局 BattleMaxDurationSeconds
}
```

**伤害系数用整数万分比**：先算 `3000 / 10000.0` 再乘，与字面量 `0.3` 是同一个 double，确定性不变。

### 6.3 查表与下发（硬约束）

- **battle 自己查表**：`CreateBattleRequest` 已经带了 `battle_config_id`（字段 2）和 `match_mode`（字段 5）。battle 按这两个值在自己加载的 `PlayMode` 表里查规则集，查不到就按 match_mode 推导出与现行逐位相同的内置规则。
- **不加 `play_mode_id` 这样的新 wire 字段**：按 proto3 规则，旧 battle 会忽略未知字段，用旧规则开局，新玩法（如"30 回合打满判平"）会**静默失效**。
- **不把规则挂在 Dungeon 行上**（`Dungeon.play_rule_id`）：那会把"这是哪个副本"和"这局怎么打"写进同一行，同一副本想做普通 / 挑战两个变体只能复制 Dungeon 行（违反 §11.6 的分层思路）。
- 滚动升级：新 `PlayMode` 行只在 battle 全量升级后才启用；指纹切到 enforce 后，表批次不一致会被拦截。

### 6.4 match 侧

- match 现在**不加载任何配表**（`LoadTables` 只出现在 guild、login、player_locator、scene_manager）。接表照 `go/guild/internal/svc/servicecontext.go` 的写法，k8s 也要同步部署表数据目录。
- 启动期建 `(match_mode, battle_config_id) → PlayMode` 映射并 fail-closed 校验（`ValidateTables` + `logx.Must`），启动日志打出 manifest version / digest。
- 用 `PlayMode.kind` 处理器替掉 `joinqueuelogic.go`、`matcher.go`、`gather.go`、`rating.go` 里的 MatchMode switch；队列 key 维持现行形态（PVP 为 `(mode, config)`，PVE_TEAM 另带人数段 `size<N>`，见 pve-team-size-matching.md §3.2），语义一变就要整批替换实例（pve-team-size-matching.md）。
- 删 yaml 的 `PveTeamSizeByConfigId`（它同时被 team 的 `StartTeamMatch` 整队开战使用，k8s ConfigMap 也是逐字搬运，要一起改）；`round_cap_outcome` 上线后删 `RatingDrawRoundCap` 补丁。
- guild 历练结算写死要求 `match_mode == PVE_TEAM`，改为校验 PlayMode。
- **不加 `PlayMode.activity_id`**：那等于要求 go/match 再实现一遍活动状态机（第三、第四份实现）。限时开放先由 match 的 yaml 配置，或等活动服务落地后再接。

### 6.5 胜负规则

v1 只用 `round_cap_outcome` 枚举。出现"击杀指定单位即胜"这类真实需求时再加 `win_on_monster_id` 列；出现第三种需求时再考虑条件树。胜负判定**不得消耗随机数**，`RollDrops` 之前的随机序列保持不变。

### 6.6 示例：周末 3v3 回合竞技

- `BattleRuleSet 21`：不刷怪、不可逃跑、伤害 ×0.3（3000）、道具 5 次、30 回合、打满判平。
- `PlayMode 301`：kind = `QueueRatedSnake`，match_mode = 3V3，battle_config_id = 竞技场副本行，每队 3..3，rated，rule_set 21。
- 走一遍：客户端 `JoinQueue(3V3, X)`（协议不变）→ match 查到 PlayMode 301 → 凑满 2 × 3 = 6 人、按评分蛇形分队 → `CreateBattleRequest` 照旧只带 match_mode + battle_config_id → battle 自查到规则集 21 → 结算后 match-rating 计 Elo，不再需要 `RatingDrawRoundCap`。
- 前提：v1 框架已把 5v5 的蛇形分队推广成按每队人数参数化（3V3 现在在 `JoinQueue` 走 default 被拒）。

### 6.7 不在本期范围：实时场景玩法

夺旗、占点、限时刷怪这类实时玩法需要先补三样基础：场景玩法组件在 `SceneLifecycle::CreateScene` 唯一创建点 emplace；`GameplaySystem` 按 view 遍历挂进 `World::Update`，单帧预算需另行测定并写入设计（scene-node-threading-model.md 只给出演进信号：网络 / Kafka 回调单帧持续超过 2ms、`World::Update` 接近帧间隔；那里的 2ms 不是玩法 System 的帧预算）；进场 / 离场钩子直接调用，不走 dispatcher。另立里程碑。

---

## 7. 硬约束汇总

| # | 约束 | 违反后果 | 出处 |
|---|---|---|---|
| H1 | 所有新列 0 值 = 现行行为，存量同种子回放逐字节不变 | 回放基线平移，两版对拍失效 | turn_battle_engine.h 确定性约束 |
| H2 | v1 不新增 RNG 抽取点；同分按 (hp, actor_id)；`RandomEnemy` 只配普攻 | 随机序列平移，两版分叉 | turn_battle_constants.h 消费顺序 |
| H3 | v1 禁止对敌施 buff 的原子，且 CastSkill 不得引用 effect[] 含对敌异常类 buff 的技能 | `RollAilmentResist` 掷骰 + Java 未追平 → 两版分叉 | `AddBuffToActor` |
| H4 | 阶段事件在事件组清零之后发 | group_id 撞号 | `ResolveCurrentRound` |
| H5 | AI 运行态只放服务端内部 `BattleAiState`，不进 `BattleActorState` | 改客户端契约，泄漏 Boss 阶段 | player_battle.proto |
| H6 | 战斗新表追加到指纹末尾，指纹切 enforce | 表版本不一致照常开局 | battle_table_fingerprint、match config |
| H7 | 活动周期状态不进 `MissionListComp`，用顶层新字段并配自己的 marshal 往返测试 | 重登丢周期键 → 重复领奖 | mission_marshal `Unmarshal` |
| H8 | 换期只清进度，不吞未领奖励 | 玩家资产损失 | mission.cpp"发奖成功前保留领取权" |
| H9 | 触发器不直接发物；领奖提交点复核，发放成功才置已领 | 双发 / 漏发 | player_mission 领奖顺序 |
| H10 | 玩法规则由 battle 按已有字段自查，不加新 wire 字段；唯一例外是玩家自定义覆盖层（字段 14，battle 在任何副作用之前按模式与参数白名单 fail-closed，§8.6.2） | 旧 battle 静默跑旧规则 | proto3 未知字段语义 |
| H11 | 参数列用具名标量 int64，不用 repeated 槽位 / double / 表达式 | 静默丢数据、确定性破坏 | excel_reader.py、table_expression.h |
| H12 | 不存表行指针，只存 id 或编译快照 | 热更后野指针 | data/AGENTS.md |
| H13 | 指标进程内聚合、周期打日志，label 不带高基数 id | 日志爆炸 / Prometheus 高基数 | battle 无 Prometheus 端点；AGENTS §9 |
| H14 | 处理器表与字典一一对应在启动期核对（生成常量无 kCount，不能 static_assert） | 漏注册的原子运行期才暴露 | constants_gen.py |
| H15 | tip 码只由导表器从 `Tip.xlsx` 发号；不可参与原因不写中文串 | 客户端文案错乱 | AGENTS §4.4、§7.5 |
| H16 | 玩家自定义模板只能在策划声明的基底范围内调整；自定义对局一律 `MATCH_MODE_CUSTOM`，无奖、无损、不计分，四层代码写死 | 刷经验 / 掉落 / 评分 / 历练计数 | §8.7 F1–F11 |
| H17 | 活动、奖励、掉落、评分、历练规则不开放给玩家自定义 | 开放刷奖通道 | §8.1 |
| H18 | 玩家策略运行态不进 `BattleActorState`；策略只能挑合法行动（过 `CheckActionPrerequisites`） | 对手看到打法；产出手动做不到的行动 | §8.6.5 |
| H19 | 两版同批：客户端可见 proto、tip、配表与 Java 同批改并在 Java `PARITY.md` 登记 | 两版契约分叉 | AGENTS §12 |

---

## 8. 玩家自定义模板（类网易狼人杀自定义板子）

> 本节 v0.2：v0.1 初稿经"安全·资产·一致性·确定性"与"产品·运维·性能·双版本"两路评审修订，改动明细见 §8.13。

### 8.0 结论速览

狼人杀的做法是：策划定义可选角色池和规则开关，玩家在范围内勾选，存成"板子"，开自建房时引用，可以分享；大多数人玩的其实是官方板；自定义房不计排位、不发正式奖励。本游戏照这个模式做：

1. **能做，开放两类**：
   - **对局规则板子**：带板切磋、自建房、好友竞技。板子里**只放引擎规则**（回合上限、打满判负 / 判平、伤害档位、禁药 / 药限、禁宝宝）；每边人数、可见性这类"这间房怎么开"的参数属于房间，不属于板子（§11.6）。
   - **自动战斗策略**：宝宝和角色挂机出招规则（梦幻 / 问道式自动战斗设置），复用 §4 的策略求值器。**宝宝优先**：宝宝恒为自动行动，宝宝策略一上线就有用。
2. **绝不开放**：活动、奖励档位、触发器、计数器、掉落、怪物属性、经验倍率、评分 / 段位、`PlayMode` / `BattleRuleSet` 本体、帮会历练规则。帮会 / 玩家自建赛事以后最多做成"一组自定义房的编排"，v1 不做。
3. **一条铁律：自定义对局 = `MATCH_MODE_CUSTOM`（新枚举值 7）= 无奖、无损、不计分**，在 match、battle 引擎、scene、guild 四层用代码写死，不依赖配表开关（§8.7）。
4. **官方板子是主入口**，玩家自建板子是进阶。第一期（P1）只做"官方板子 + 内联规则的带板切磋"，**不建模板服务、不开自由文本**；模板服务、分享码、自建房、策略、内容安全按 §8.10 分期递进。
5. **模板库（P3 起）放新的全局 Go 服务 `go/player_template`**，不放玩家档；**非作者只能凭分享码访问**，template_id 只对作者本人有效（§8.5）。
6. **使用时按值冻结**：规则作为显式覆盖层进 `CreateBattleRequest` 新字段 14，由 battle **在任何副作用之前**按模式与参数白名单 fail-closed（不做回显摘要）；策略由 match 在凑局时写进快照（§8.6）。
7. **硬边界在代码里**：每个可覆盖参数的绝对上下限写成 C++ / Go / Java 常量并共用对拍向量，表只能在这个圈内再画小圈；battle 不读模板相关的表。
8. **前置依赖链**：§10 第 1 步（指纹 enforce；跨语言金样——尚待用户同意）、第 3 步（策略求值器）、第 5 步（`PlayMode` / `BattleRuleSet` 建表、match 接表；它又依赖 Java 先追平两处漂移）。`data/schema` 下这些表目前都还不存在——§8 任何一期前面至少排着 4 个没开工的步骤（§10 第 1、3、5 步，加上 §8.2 导表期校验依赖的第 2 步通用规格校验器），交付说明里要写明。

### 8.1 适用范围

| 候选 | 结论 | 理由 |
|---|---|---|
| 对局规则板子（带板切磋 / 自建房 / 好友竞技） | **开放** | 只调引擎规则参数（§8.2），局类型强制 CUSTOM，对经济和评分零影响 |
| 自动战斗策略（宝宝 + 角色） | **开放，带限制** | 策略只在合法行动里挑：每个候选都要过 `CheckActionPrerequisites`，手动做不到的它也做不到。限制：只能选 `BattleAtomKind.player_selectable = true` 的原子；每份 ≤ 16 条规则、每条 ≤ 2 个条件；不新增 RNG 抽取点；不开放对敌施 buff（H2、H3）；评分 PVP 里默认禁用（§8.12 第 3 条） |
| 帮会 / 玩家自建活动或赛事 | **v1 不做**，以后也只做"赛事编排" | 活动本质是"计数器 + 档位 + 发奖"，开放等于开放刷奖通道；可接受的形态只有"一组 CUSTOM 房 + 对阵表"，不发帮会资金、不计历练、不进排行 |
| 活动模板、档位、触发器、计数器 | **绝不开放** | 资产 fail-closed 路径 |
| 掉落、怪物属性、经验倍率、PVE 副本组合 | **绝不开放** | 引擎 `BuildSettlement` / `RollDrops` 按击杀产出经验、金币、掉落，scene 再按击杀推进任务 |
| 评分 / 段位 / `PlayMode` / `BattleRuleSet` 本体 | **绝不开放** | 玩家只能叠加覆盖层，且只在 CUSTOM 下生效 |
| 帮会历练规则 | **绝不开放** | `activity_context` 只能由内部 `MatchInternal.StartActivityBattle` 写入 |
| 每边人数、可见性 / 观战 | **不进板子**，作为房间 / 邀请参数 | 板子分享出去后不应替使用者决定"开几人房""对谁可见"；同一套板子要能在切磋和房间间复用 |
| 逃跑开关 | **v1 不做** | PVP 下 `CheckActionPrerequisites` 直接拒绝逃跑，`ExecuteFlee` 非 PVE 分支成功恒为 false；宝宝不能逃而 `SideWiped` 只看 `IsActorActive`，主人逃了宝宝还在打。确有需要时做成确定性的"认输" |
| "统一属性"（好友竞技最常要的公平选项） | **v1 不做，记为已知差距** | 要改战斗快照、牵涉战斗类属性，单独立项 |
| 加点方案、装备方案等个人预设 | 不归入本功能 | 已有 `AttributeScheme`，只给自己用、放玩家档 |

### 8.2 模型、硬边界与策划表

**四层映射**：

| 层 | 是什么 | 承载 |
|---|---|---|
| 原子 + 硬边界（程序） | 可被玩家覆盖的参数种类及其绝对上下限；策略原子沿用 `BattleAtomKind` | **代码常量**（C++ constexpr / Go const / Java 常量，三端共用一份对拍向量）；`PlayerTemplateParamKind` 表只做编号镜像 |
| 基底（策划） | 哪些项可改，每项类型、范围、档位、默认值、是否锁定——相当于狼人杀的"板子类型" | `PlayerTemplateBase` / `Option` / `OptionChoice` |
| 官方板子（策划） | 基底 + 一组固定取值，玩家直接选用 | `PlayerTemplatePreset` / `PresetValue`（**必做，主入口**） |
| 玩家板子（P3 起） | 基底 + 选项值（或策略规则） | 全局服务表 `player_template` |
| 使用快照 | 开局时冻结的规范化取值 | `CustomBattleRules`（规则）、`BattleAutoStrategy`（策略） |

**为什么硬边界不放表里**：如果"程序硬边界"本身是策划表的一列，导表期只能校验"基底区间 ⊆ 字典区间"，字典本身没人校验——一处笔误（如 `MAX_ROUNDS` 上限写成 3000、伤害系数下限写成 0）就能穿过所有校验层。所以硬边界写在代码里，启动期校验"表里的区间 ⊆ 代码区间"，不满足拒绝启动；battle 只用代码常量复校验，**不读**任何模板相关的表，这些表也就不进战斗配表指纹。

**v1 可覆盖参数（代码常量）**：

| 参数 | 类型 | 代码硬边界 | 说明 |
|---|---|---|---|
| `MAX_ROUNDS` | INT_RANGE | 5..45 | 受整场 deadline 约束：现行 `BattleMaxDurationSeconds = 300`、`kRoundDurationMs = 6000`，手动局每回合最多等满 6s 行动窗口（全员提交即提前结算），每回合都等满时约 50 回合（deadline 从 gather 起算，实际略少）；MAX_ROUNDS 超过约 50 时，拖时间不提交的一方能把对局拖进墙钟强制平局（§8.6.3）。若要放宽，改为 match 按冻结的 `MAX_ROUNDS` 推算 deadline，另设硬上限 |
| `ROUND_CAP_OUTCOME` | ENUM_CHOICE | {1 进攻方负, 2 平局} | 对应 §6.2 `round_cap_outcome` |
| `DAMAGE_SCALE_TIER` | ENUM_CHOICE | 档位白名单（如 5000 / 10000 / 15000 / 20000 万分比） | 玩家看不懂万分比滑条，做成 3–4 档 |
| `ITEMS_FORBIDDEN` | BOOL | 0..1 | 禁药，独立布尔项；需 `BattleRuleSet` 追加 `items_forbidden`（0 值 = 现行） |
| `ITEM_USE_CAP` | INT_RANGE | **1**..20 | **下限是 1**：§6.2 规则集里 `item_use_cap = 0` 表示"不限"，玩家选 0 会以为是禁药、实际得到不限——所以覆盖层拒绝 0，禁药走 `ITEMS_FORBIDDEN` |
| `PETS_FORBIDDEN` | BOOL | 0..1 | 禁宝宝；需 `BattleRuleSet` 追加 `pets_forbidden` |

P4 引入策略时再加 `PLAYER_STRATEGY_ALLOWED`（BOOL）。每边人数（1..5，D14）是房间参数，不是板子参数。

**策划表（data/schema 风格，字段号以落码为准）**：

```proto
// playertemplateparamkind_table.proto —— 参数编号镜像（硬边界在代码，启动期核对一一对应）
message PlayerTemplateParamKindTable {
  uint32 id = 1;            // 参数种类，上线后不复用
  uint32 value_type = 2;    // 1 BOOL 2 INT_RANGE 3 ENUM_CHOICE；启动期与代码常量核对
  int32  constants_name = 9001 [(cfg_owner) = "constants_name"];
  int32  design_note    = 9002 [(cfg_owner) = "designer"];
}

// playertemplatebase_table.proto —— 可自定义基底
message PlayerTemplateBaseTable {
  uint32 id = 1;
  uint32 kind = 2;                    // 1 对局规则 2 自动战斗策略（不可变，见 §8.4）
  bool   enabled = 3;                 // false：不能新建、不能使用；已存模板保留
  uint32 base_version = 4;            // 语义变更时 +1
  uint32 min_compatible_version = 5;  // 模板保存时的 base_version 低于它，须作者重存后才能用
  uint32 play_mode_id = 6 [(cfg_fk) = "PlayMode"]; // kind=1 必填：match_mode=CUSTOM 且 customizable=true 的行；kind=2 必须 0（不可变）
  uint32 strategy_actor = 7;          // kind=2：1 角色 2 宝宝；kind=1 必须 0（不可变）
  uint32 max_rules = 8;               // kind=2：≤ 代码上限 16
  string name = 9; string desc = 10; uint32 sort = 11;
}

// playertemplateoption_table.proto —— 基底上的可选项（策划画的圈）
message PlayerTemplateOptionTable {
  uint32 id = 1;                      // 选项 id：存档按它记，上线后不复用、不删行
  uint32 base_id = 2 [(cfg_fk) = "PlayerTemplateBase", (cfg_index) = true];      // 不可变
  uint32 param_kind = 3 [(cfg_fk) = "PlayerTemplateParamKind"];                   // 同一基底内唯一；不可变
  uint32 state = 4;                   // 1 可用 2 已下线·按默认值处理 3 已下线·拒绝使用
  bool   player_editable = 5;         // false：锁定，始终取当前 default，不入玩家存档
  uint32 default_value = 6;
  uint32 min_value = 7; uint32 max_value = 8; uint32 step = 9;  // INT_RANGE 用
  uint32 order = 10;                  // 显式 UI 顺序（§11.6）
  string name = 11; string desc = 12;
}
message PlayerTemplateOptionChoiceTable {   // ENUM_CHOICE 档位白名单
  uint32 id = 1;
  uint32 option_id = 2 [(cfg_fk) = "PlayerTemplateOption", (cfg_index) = true];
  uint32 value = 3;                   // 同一 option 内唯一
  uint32 order = 4; string name = 5;  // name 如"伤害 ×1.5"
}

// playertemplatepreset_table.proto —— 官方板子（主入口）
message PlayerTemplatePresetTable {
  uint32 id = 1;
  uint32 base_id = 2 [(cfg_fk) = "PlayerTemplateBase", (cfg_index) = true];
  bool   enabled = 3;
  string name = 4; string desc = 5; uint32 sort = 6;
}
message PlayerTemplatePresetValueTable {
  uint32 id = 1;
  uint32 preset_id = 2 [(cfg_fk) = "PlayerTemplatePreset", (cfg_index) = true];
  uint32 option_id = 3 [(cfg_fk) = "PlayerTemplateOption"];   // 必须属于同一基底
  uint32 value = 4;
}

// playertemplaterule_table.proto —— 容量口径，单行 id = 1（先例 RoleNameRule）；P3 起使用
message PlayerTemplateRuleTable {
  uint32 id = 1;
  uint32 max_rule_templates = 2;       // 建议 10
  uint32 max_strategy_templates = 3;   // 建议 10
  uint32 max_creates_per_game_day = 4; // 建议 20：按游戏日的创建上限，防"反复建删"撑爆表
}
// 已有表追加：BattleAtomKind.player_selectable（§4.2）、PlayMode.customizable（§6.2）
// BattleRuleSet 追加（0 值 = 现行）：items_forbidden / pets_forbidden / player_strategy_forbidden
```

**v1 不建跨项约束表**：初稿的两个示例一个依赖已被否掉的逃跑开关、一个是凑出来的，不满足 §2 原子准入纪律（≥ 2 个真实实例）。将来真有需要时再加 `PlayerTemplateConstraint`（一行一条"若 A ∈ 区间则 B ∈ 区间"），约束种类用代码枚举、不建字典表。

**导表期校验**（通用规格校验器 + Java `TableSourceGenerator` 生成的加载期校验，失败产物不落盘 / 拒绝加载）：`step ≥ 1`、`min ≤ max`、`(max − min) % step == 0`、default 落在区间内且对齐步长（或在档位白名单内）；同基底 `param_kind` 唯一；官方板子的取值属于同一基底且通过同一套校验；kind = 1 的基底所指 PlayMode 必须 customizable 且 match_mode = CUSTOM；`max_rules ≤ 16`。
**启动期**：表区间 ⊆ 代码硬边界；字典行 ↔ 代码参数一一对应。
**不可变项机械守卫**：基底的 `kind` / `play_mode_id` / `strategy_actor`，选项的 `base_id` / `param_kind` 一经上线不得修改、选项行不得删除。照导表器现有的"状态钉住"做法（`tools/data_table_exporter/state/mapping/` 下的映射文件 + 生成期比对，先例 `bit_index_gen.py`）加一份钉住文件，违反即导表失败。

### 8.3 内容结构（客户端可见，只定义一份）

```proto
// proto/battle/battle_strategy.proto（新）—— 模板、快照、引擎共用；不手写平行 struct
message BattleAutoStrategyRule {
  uint32 priority = 1;             // 严格升序、唯一；不靠下标（§11.6）
  uint32 cond1_kind = 2; uint32 cond1_p1 = 3; uint32 cond1_p2 = 4;
  uint32 cond2_kind = 5; uint32 cond2_p1 = 6; uint32 cond2_p2 = 7;   // 0 = 恒真
  uint32 action_kind = 8;
  uint32 skill_id = 9;             // 只有 CastSkill 必填
  uint32 item_id = 10;             // 只有 UseItem 必填
  uint32 target_kind = 11; uint32 target_p1 = 12;
}
message BattleAutoStrategy {
  uint32 schema_version = 1;       // 只接受 1；引擎不认识的版本确定性忽略整份
  repeated BattleAutoStrategyRule rules = 2;
  uint64 source_template_id = 3;   // 仅由服务端在 Resolve 时填写；客户端上传的值在保存时清零
  uint32 source_revision = 4;      // 同上
}
// proto/player_template/player_template.proto（新）
message PlayerTemplateOptionValue { uint32 option_id = 1; uint32 value = 2; }
message PlayerRuleContent { repeated PlayerTemplateOptionValue values = 1; } // option_id 严格升序唯一；只存玩家可编辑且 state=1 的项
message PlayerTemplateContent { oneof body { PlayerRuleContent rules = 1; BattleAutoStrategy strategy = 2; } } // oneof 必须与 base.kind 一致
```

- **参数一律 uint32**，原子参数区间限制在 [0, 2^21)。原因：gate 的 `CheckMessageSize` 是对**整个外层 `ClientRequest`**（id / service / method / body / message_id）取 `ByteSizeLong()` 与 1024 比较；int64 取负值时 varint 固定 10 字节，16 条规则 × 7 个参数 × 10B 就已超过 1KB；而超限不只是一次普通拒绝——同一个函数会把它计入非法包，默认累计 50 次断开连接。玩家可选原子（player_selectable）的 `p_min..p_max` 必须落在这个区间内（导表期校验）。
- 策略内容不用 map：Java 与 C++ 的确定性序列化对 map 键的排序不同（Java battle-engine-spec §9.8）。
- 服务端可信化：保存时清零 `source_*`、要求 `schema_version == 1`、校验 oneof 与基底 kind 一致；match 对**每份**快照都覆盖或清空策略字段（Resolve 失败时也清空），客户端无法把任何值带进对局。

### 8.4 校验

**分层（客户端不可信）**：

| 层 | 谁 | 查什么 | 失败口径 |
|---|---|---|---|
| 导表期 / 启动期 | §8.2 所列 | 区间、步长、档位、不可变项、代码硬边界 | 产物不落盘 / 拒绝启动 |
| 保存期（权威，P3 起） | `go/shared/playertemplate` 纯函数包（template 服务与 match 共用，Go 侧唯一实现） | 下述逐项校验 | 回 tip，不入库 |
| 使用提交点 | match（开切磋、建房、改规则、开战时用同一个包）；template 服务 Resolve 时也复核 | **先规范化再校验**（见下） | **规则 fail-closed**：拒绝开局。**策略 fail-open**：退回"无策略"（即现行行为，也是资产安全方向） |
| 引擎复校验 | C++ battle / Java engine | 只查程序不变量：match_mode 白名单、param_kind 已知、值在代码硬边界内、schema_version、条数上限、priority 升序、**策略原子 player_selectable、"行动 × 目标"组合矩阵（H2 / H3 是确定性不变量，不只是策划规则）、规则集的策略禁用开关** | 规则违规在任何副作用前拒绝建房；策略违规确定性地忽略该单位整份策略 |

**规范化（使用前）**：剔除 `state = 2` 的项改取 default；补上保存后新增项的 default；锁定项（`player_editable = false`）不入玩家存档、始终取**当前** default。这样策划下线选项、新增选项、调整锁定默认值，都不会让存量板子整体报废或含义被静默改变。

**规则板子逐项校验**：① 基底存在、enabled、kind 匹配；② values 按 option_id 严格升序无重复；③ 每个 option 属于该基底、state = 1、可编辑；④ BOOL 只取 0/1，INT_RANGE 要求 `min ≤ v ≤ max` 且 `(v − min) % step == 0`，ENUM_CHOICE 必须在档位白名单里；⑤ 不在任何环节静默夹取越界值（夹取会改变规则含义）。

**策略逐项校验**：① 条数 ≤ `base.max_rules`（且 ≤ 16），每条 ≤ 2 个条件；② 每个 kind 都在 `BattleAtomKind` 且 player_selectable，category 符合所在列角色；③ 参数在原子 `p_min..p_max` 内，不用的参数为 0；④ `CastSkill` 的 skill_id 存在且不是 passive / toggle / channel（与 `IsTurnBattleCastableSkill` 黑名单同口径），`target_kind` 不为 0，v1 里技能的 `effect[]` 不含毒 / 冰冻 / 眩晕 / 沉默类 buff（H3）；`UseItem` 的 item_id 必须 battle_usable；⑤ 组合矩阵：`RandomEnemy` 只配 Attack（H2），`UseItem` 只配 Self / LowestHpAlly，宝宝基底禁用 `UseItem`（宝宝没有道具副本），`CastSkill` v1 只配敌方目标（`ValidateSkillTarget` 不区分敌我，§8.12 第 10 条）；⑥ **不**校验"玩家是否拥有该技能或道具"——出手时由 `CheckActionPrerequisites` 拦下并跳过该规则，天然 fail-safe。

**策划改表后的存量板子**：

| 改动 | 策划怎么做 | 存量板子的表现 |
|---|---|---|
| 选项下线 | 行不删，`state` 改 2 或 3 | 2：规范化时剔除、取 default，板子仍可用；3：使用时回 `OptionRetired`，作者编辑后重存 |
| 范围收紧 / 步长变化 | 直接改 | 使用时越界回 `OptionOutOfRange`，不夹取 |
| 档位白名单删行 | 删 Choice 行 | 使用时回 `OptionNotInChoices` |
| 锁定项改默认值 | 改 default | 锁定项不入存档，自动取新值（这是策划有意的全局调整） |
| 语义变更 | **禁止**原地改，必须新开 option 行、旧行下线 | 同上 |
| 强制全量重确认 | 抬高 `min_compatible_version` | 低于它的回 `Outdated` |
| 基底停用 | `enabled = false` | 不能建、不能用，可查看和删除 |
| 原子取消 player_selectable | 改字典行 | 规则板子不受影响；策略在 Resolve 时被剔除（fail-open），视图提示"不可用" |
| 容量上限调低 | 改 `PlayerTemplateRule` | 已有板子保留，超出部分不能再新建 |

模板列表视图对每个板子带 `unusable_reason`（tip），按当前表惰性算出，不写库。

**tip 码**：
- P1 / P2（无模板服务）：所有新码放进现有 `match_error`（16000 段，现用 21 个码，放得下）。
- P3 起：新组头 `//player_template_error`。**30000 段已被 `mystery_merchant_error` 占用**（`tip_enum_ids.json` 已发 30000 起的码）；31000 以上当前空闲。建议 §5.5 的 `activity_error` 取 31000（§10 第 6 步在前）、本功能取 32000，最终以导表器自检为准。
- 码清单（业务拒绝不填 fault）：查找与权限（NotFound、NoPermission、KindMismatch）、基底（BaseUnknown、BaseDisabled、Outdated）、规则项（OptionUnknown、OptionLocked、OptionOutOfRange、OptionNotInChoices、OptionNotSorted、OptionRetired）、策略项（StrategyTooManyRules、StrategyAtomNotAllowed、StrategyParamOutOfRange、StrategyRefInvalid、StrategyComboInvalid）、容量与并发（LimitReached、DailyCreateLimitReached、RevisionConflict）、分享码（ShareCodeMalformed、ShareCodeUnavailable）、其他（LoadoutNotOwn、LoadoutSlotMismatch）。P5 开自由文本时再加 Name / Desc 类拒绝码与 PublishBanned。
- 存储故障复用 common 段 1003，限频复用 1008。各服务 `constants.go` 只写 `uint32(table.Xxx_kYyy)`，加 `TestNoHandWrittenTipCodes` 守卫。

### 8.5 存储与服务归属（P3 起）

#### 8.5.1 为什么是独立全局服务

- 别人凭分享码读板子时作者往往不在线；全局服务按契约**不得**读 zone 库、db_task 和 `player:{id}:*`（microservice-zone-contract-20260914.md §0 第 3 点），离线玩家的父快照也不保证在 Redis。
- `player_database` 每次存盘都整行重写（go/db `savePlayerRow` 的 `tx.Save`），放模板会放大写量，还受 go/db BlobGuard（每列 256KiB、每行 1MiB）与 Kafka 生产端约 1MB/条的上限约束。
- zone 回档会回滚玩家档，但全局号段不回退，两边会分叉。
- 先例：mail 因"离线可达"选了独立服务（cross-zone-readiness-audit 决策 6）；最像的范式是聚宝斋 `trade_listing`（玩家创建的记录、号段发 id、全服索引、version CAS）。只给自己用、不可分享的少量预设才适合放玩家档（`AttributeScheme` 先例）。

#### 8.5.2 服务形态与接入清单

- `go/player_template`：进程骨架用 `go/shared/domainhost`（mail / rank 先例）。`domainhost.Config.Validate` 要求 `Producers` 非空、`Secrets()` 要求每个调用方密钥 ≥ 32 字节——match 作为调用方需要新密钥、K8s Secret 注入与轮换流程（先例 `go/rank/etc/rank.yaml` 的 `RANK_PRODUCER_SECRET_MATCH / ADMIN`）。dev / test 模式可设 `RequireTiDB = false`。
- 客户端服务 `ClientPlayerTemplate` 标 `OptionIsClientProtocolService`；内部服务 `PlayerTemplateInternal`、运维服务 `PlayerTemplateAdmin` 不标。运维鉴权统一走 domainhost 的 `Producers.admin` + serviceauth 签名，不再造一套 `x-admin-token`。身份只从 `x-session-detail-bin` 取；zrpc Timeout ≤ 4000ms。
- protogen：新 proto 域要在 `proto_gen.yaml` 与 `proto_gen.no-unity.yaml` 的 `domain_meta` 下各加一块（生成器没解析到的域会丢消息号，team-system.md DV-5）。节点类型由 service 名或 package 名推导（`NodeServiceForCpp`）：package 名要与期望的节点类型一致（例如 package `room` 会推导成 `RoomNodeService`）。新节点类型要加进 `proto/common/base/node.proto` 的 eNodeType，C++ 生成的 `node_util` / `service_register_info` 随之变化，gate 要重新编译。
- 部署登记五处（`go_services.ps1`、`go_svc_image.ps1`、`k8s_deploy.ps1`、manifest、`start_game.ps1`）；建库只登记在 `deploy/mysql-init/00_init_zone_dbs.sql`；迁移走 `-migrate` Job。schemamigrate 不会给已存在的表补索引，**索引在建表时一次写全**。
- 私有 Redis 只放限频配额（noeviction）。

#### 8.5.3 持久化表（mmorpg_player_template，D-14 范式）

```proto
message PlayerTemplateRecord {
  option(OptionTableName) = "player_template";
  option(OptionPrimaryKey) = "template_id";
  option(OptionUniqueKey) = "share_code";               // 本表唯一的 UNIQUE KEY（D-14：每表至多 1 个）
  option(OptionIndex) = "owner_player_id,state,template_id";
  uint64 template_id = 1;          // data_service 号段 biz_tag=player_template（单调递增，只对作者本人暴露为可用句柄）
  uint64 owner_player_id = 2;
  uint32 kind = 3;                 // 1 规则 2 策略
  uint32 base_id = 4;
  uint32 base_version = 5;         // 保存时的基底版本
  bytes  content = 6;              // PlayerTemplateContent 确定性序列化，≤ 2048 B
  uint32 revision = 7;             // 每次提交 +1，CAS
  uint32 visibility = 8;           // 1 私有 2 凭分享码（好友 / 公开留给 P6）
  uint64 share_code = 9;           // crypto/rand 50 bit，永不为 0；Reset 后旧码立即失效
  uint32 share_generation = 10;    // 每次 Reset +1（P5 收藏按它校验）
  uint32 state = 11;               // 1 正常 2 下架 3 已删除（软删，只对分享过的板子）
  bool   ever_shared = 12;         // 从未分享过的板子删除时直接硬删
  uint64 forked_from_template_id = 13;
  uint64 created_ms = 14; uint64 updated_ms = 15; uint64 deleted_ms = 16;
}
message PlayerTemplateOwnerRecord {          // 每人一行：只做行锁 + 按日创建计数，不存"存活数量"
  option(OptionTableName) = "player_template_owner";
  option(OptionPrimaryKey) = "player_id";
  uint64 player_id = 1;
  uint32 create_day_key = 2;       // 游戏日键（go/shared/gameday）
  uint32 creates_today = 3;
}
message PlayerTemplateCreateRequestRecord {  // 创建幂等；保留期 7 天，由服务巡检清理
  option(OptionTableName) = "player_template_create_request";
  option(OptionPrimaryKey) = "owner_player_id,request_id";
  uint64 owner_player_id = 1; uint64 request_id = 2; uint64 template_id = 3; uint64 created_ms = 4;
}
message PlayerStrategyLoadoutRecord {        // 布局层："当前用哪套"，不写在模板行上（§11.6）（P4）
  option(OptionTableName) = "player_strategy_loadout";
  option(OptionPrimaryKey) = "player_id,slot";
  uint64 player_id = 1;
  uint32 slot = 2;                 // 1 本人 2 出战宝宝；必须与模板基底的 strategy_actor 一致
  uint64 template_id = 3;          // 只能是自己的模板
  uint64 updated_ms = 4;
}
message PlayerTemplateAuditRecord {          // 运维操作审计，与状态变更同库同事务（先例 player_inbox_audit）（P5）
  option(OptionTableName) = "player_template_audit";
  option(OptionPrimaryKey) = "audit_id";
  option(OptionIndex) = "template_id,audit_id";
  uint64 audit_id = 1; uint64 template_id = 2; uint64 target_player_id = 3;
  uint32 op = 4; string operator = 5; string reason = 6; string peer_addr = 7;
  bytes payload = 8; uint64 created_ms = 9;
}
```

**容量与并发**：
- 写事务的串行化：先在**独立短事务**里 `INSERT IGNORE` 建出 owner 行，再在主事务里 `SELECT … FOR UPDATE` 它。原因：RC 隔离下对不存在的行 `FOR UPDATE` 不加锁，并发 INSERT IGNORE 建同一缺失行时，只要首个插入者在提交前回滚，仍会触发 1213 间隙锁环（trade `asset_op_repo.go` 记录过同一个坑；做法是在短事务里先 FOR UPDATE 一行预置、永不删除的全局哨兵守卫行，再 INSERT IGNORE 缺失行并提交）。本表照做需要预置 owner 哨兵行（例如 player_id=0），并在建 owner 行的短事务里先锁住它。
- 存活数量**不存计数器**，在锁内对 `(owner_player_id, state)` 索引做 `COUNT`（每人 ≤ 10 行，很便宜）。存下来的计数会在人工修库或部分恢复后漂移，成为第二份真相。
- 按游戏日的创建上限 `max_creates_per_game_day`，故障时拒绝；从未分享过的板子删除时硬删；幂等行 7 天保留期。这三条一起给表行数一个上界（SQL 存活计数管不住"反复建再删"）。
- C2S 预算：最大请求按最费字节的编码、用外层 `ClientRequest` 包裹构造，单测断言 `ByteSizeLong() < 1024`。

#### 8.5.4 访问控制与分享码

- **非作者只能凭分享码访问**。template_id 来自号段、单调递增，若非作者能用 template_id 访问，50 bit 分享码的熵就没有意义了（可顺序扫号），作者 Reset / 改私有 / 被下架之后别人也照样能用。所以：template_id 只在 `caller == owner` 时有效，否则统一回 `ShareCodeUnavailable`；凭分享码查询时视图里**不回** template_id 与 owner_player_id 之外的可用句柄（复制走"凭码复制"）。
- **判定顺序固定**：先判可见性与码是否有效，再判 KindMismatch / BaseDisabled / Outdated——否则这些码会泄露"这个码存在"。不存在、私有、下架、删除统一回 `ShareCodeUnavailable`。
- **统一的按码解析入口与配额**：模板服务内部只有一个"按码解析"函数，所有能接受分享码的入口（`GetPlayerTemplateByShareCode`、`CopyPlayerTemplate`、建房、改房间规则、带板切磋、举报）都经它；未命中配额按 `caller_player_id` 统一计数（match 调 `ResolveRulesForUse` 时必须带 caller），**故障拒绝**。限频：每人每分钟最多 20 次解析、其中未命中最多 10 次。
- **share_code 生成**：`crypto/rand` 50 bit，与 template_id 无关；`BIGINT UNSIGNED` 唯一键（避开 D-14 string 唯一键区分大小写和尾部空格、并且必须在应用层先限长的问题（v0.2.0 起已是整列索引，不再有 191 前缀问题））；撞号重试 ≤ 3 次。
- **文本形式**：Crockford Base32 编 10 位 + 1 位校验，显示为 `XXXX-XXXX-XXX`；解析时忽略大小写和分隔符，O 当 0、I / L 当 1。**校验位只用 32 字符表内的字符**——Crockford 标准的 mod 37 校验符含 `* ~ $ = U`，其中 `*` 与聊天 `Mask` 逐字替换出的 `*` 混淆，手机上也难输入。
- **防枚举估算** [I]：1e7 个存活码在 2^50 ≈ 1.1e15 空间里单次命中率约 9e-9；按上述配额，即使 1e5 个账号的僵尸网络，每天约 1.4e9 次未命中也只能命中十余个。板子不含敏感信息，风险可接受；另配未命中率告警。

#### 8.5.5 合服、回档、跨 zone、删除、运维

- **合服**：表里没有 zone 列，不需要合服步骤（friend 先例）；作者名用 `BatchGetPlayerName` 回源、不冗余存储。将来做"本服广场"时照 trade 加市场分区列（trade_listing 用的是 `market_zone` = 上架时作者 home_zone，可另存 `*_zone_at_*` 审计原值），并照 merge_zone 的 trade_step.go 加改写步和 drained 审计。
- **回档**：板子不随 zone 回退，与 guild / friend 同口径；不含资产，不需要资产闸。zone 回档产生的 `orphan_player_ids`（zone_data_rollback.md）zone 回档的 `orphan_player_ids`（zone_data_rollback.md）只是待人工核对的无快照候选角色，回档不恢复、不删除它们，作者照常可以访问名下的板子；只有运维确认并走独立审批删除角色之后，才需要用运维工具清理这些角色名下的板子。
- **跨 zone**：所有客户端都经路由服到达全局服务。
- **删除与被引用中**：

  | 引用方 | 删除后的表现 |
  |---|---|
  | 板子本身 | 分享过的软删（`state = 3`），从未分享的硬删；对他人统一回 `ShareCodeUnavailable` |
  | loadout | 下次开局 Resolve 不返回该策略（fail-open），视图提示 |
  | 已建房间、待应答切磋 | 已按值冻结，不受影响；房主改规则时才重新解析 |
  | 进行中的对局 | `CreateBattleRequest` 已冻结，不受影响 |

- **运维手册要补的两条**：全局模板库从备份恢复后，已 Reset 的旧分享码会"复活"；`raiseIdSegmentFloor` 不覆盖本功能的消费表，恢复后人工核对 `max_id ≥ max(template_id)`。

### 8.6 使用链路

#### 8.6.1 带板切磋（P1，第一个可交付入口）

1. `ChallengePlayerRequest` 追加字段 4 `CustomRulesRef custom_rules`（现有字段 1–3）。`CustomRulesRef` 是 oneof：P1 只有 `preset_id`（官方板子）与 `inline`（`base_id` + `PlayerRuleContent`）；P3 再加 `share_code` 与 `own_template_id`。
2. match 收到后：P1 直接用 `go/shared/playertemplate` 规范化并校验（P3 起经 `PlayerTemplateInternal.ResolveRulesForUse(caller, ref)`，预算 1000ms，失败即拒绝）→ 把冻结后的 `CustomBattleRules` 字节写进 `challenge:{id}` 哈希的新字段 `rules` → 推 `NotifyChallengeInvite`（带规则摘要，如"30 回合·打满判平·药限 3"）。
3. `RespondChallenge` 接受时只从记录读冻结值、不再回读模板，按当前表复核后走 `RunCustomGather`。
4. **全球权威模式的恢复路径必须带上规则与座次**：现状 `global_match.go` 的 `reserveGlobalDirect` 只把 `opts.activityContext` 落进 `MatchFormationRecord`，`runGlobalFormation` 也只重建这一项；`direct_intent.go` 的 `decideGlobalChallenge` 传的是空 `gatherOptions{}`、模式写死 `PVP_CHALLENGE`；`gather.go` 的 `teamIndexFor` 对未知模式一律返回 0 队（而引擎 `Initialize` 要求两边都有单位）。进程在 ReserveDirect 之后、SaveIntent 之前崩溃时，恢复出来的会是"CUSTOM、没有规则、全员 0 队"。所以：`MatchFormationRecord` 增加 `custom_rules`（字节）与座次两个字段并由 `runGlobalFormation` 重建；`decideGlobalChallenge` 把模式作为参数传入；`teamIndexFor` 给 CUSTOM 加显式分支、只认 opts 传入的座次；`runGatherWithOptions` 断言 `mode == CUSTOM` 时规则与座次都非空。
5. 不带 `custom_rules` 的切磋保持 `PVP_CHALLENGE` 与现行结算，不变。注意：普通切磋在 `ChallengeLogic.ChallengePlayer` 里仍原样写入客户端传的 `battle_config_id`——本功能只修 CUSTOM 路径（取 `PlayMode(base.play_mode_id).battle_config_id`），普通切磋的这个缺口另行处理。

#### 8.6.2 `RunCustomGather` 与覆盖层（fail-closed 在副作用之前）

- 新入口与 `RunChallengeGather` 同级，签名**不带** activityContext。`runGatherWithOptions` 断言：`mode == CUSTOM && activityContext != nil` 拒绝；`mode != CUSTOM && customRules != nil` 拒绝；`mode == CUSTOM` 时规则与座次必须非空。
- 内部契约追加：

  ```proto
  message CustomBattleParam { uint32 param_kind = 1; uint32 value = 2; }
  message CustomBattleRules {
    repeated CustomBattleParam params = 1;  // 规范化后的完整取值，param_kind 严格升序
    uint32 base_id = 2;
    uint32 preset_id = 3;                   // 来自官方板子时填写（审计用）
    uint64 source_template_id = 4;          // 来自玩家板子时由服务端填写（审计用）
    uint32 source_revision = 5;
    bytes  rules_manifest_digest = 6;       // 冻结时 match 所用配表批次的 manifest digest；battle 与自身批次比对
  }
  // CreateBattleRequest:  CustomBattleRules custom_rules = 14;   （现有 1–13）
  ```

- **battle 侧的 fail-closed**（替代初稿的"回显摘要"）：
  1. 在 `HandleCreateBattle` 里、`EmplaceRoom` **之前**做 match_mode 白名单检查：只接受已知模式；CUSTOM 必须带 `custom_rules`、非 CUSTOM 带了一律拒绝；每个 `param_kind` 必须是本节点已知的参数、取值在代码硬边界内；任一不满足即拒绝建房，此时尚未产生任何副作用。
  2. CUSTOM 先按 `(CUSTOM, battle_config_id)` 在 PlayMode 表查基础规则集，**查不到就拒绝**（不像 §6.3 那样回落内置规则）；该行必须 customizable。
  3. 覆盖层按平铺处理器常量表（§3.3 模式）逐个参数覆盖规则集字段。
  4. 为什么不用回显摘要：`HandleCreateBattle` 在返回响应**之前**就已推送 `BattleStartS2C` 并发出 `SendBattleConfirmedEvent`，scene 一旦进入 FIGHTING 就会拒绝 `CancelBattlePrepare`——摘要比对失败时的"补偿"只会把玩家冻结到 deadline；幂等命中的分支回的是空响应，重试会把正常对局误判为不一致；恢复路径查到 `BATTLE_CREATION_PRESENT` 就直接收尾，根本不看摘要。
  5. 旧 battle 不认识字段 14 的问题：项目尚未上线，没有旧 battle 要兼容；现在加上模式白名单，以后再新增的模式在旧节点上自然 fail-closed。上线后若需混跑，再在节点注册信息里加能力位。match 侧另外新增 yaml 开关 `CustomBattle.Enabled`（现在还没有），只当止血阀用。
- 这是 H10 "不加新 wire 字段"的唯一例外：玩家取值没法由 battle 自查表得到。

#### 8.6.3 回合上限与整场 deadline

现行 match 设 300 秒整场 deadline：旧路径读全局配置 `BattleMaxDurationSeconds = 300`（gather.go），全球权威路径在 `authority.Store.claimTx` 里写死 `DeadlineMs = n + 300000`，battle 的 `OnBattleDeadline` 到期一律强制判平，`kRoundDurationMs = 6000`。所以只要有一方每回合都拖满 6 秒行动窗口，手动局在 deadline 前只能打约 50 回合（全员提前提交时回合会提前结算，回合数可以超过 50），若板子允许更大的 `MAX_ROUNDS`，"打满判负"会被拖时间的一方改写成墙钟平局，而这种节点侧强制终局并不是引擎输入能推导出来的。v1 把 `MAX_ROUNDS` 的代码硬边界定为 45；以后若要放宽，由 match 按冻结的 `MAX_ROUNDS` 推算 deadline 并另设硬上限。无论哪种，**回放材料都要补上"节点强制终局及其发生的回合"**。

#### 8.6.4 自建房（P2）

- 落在 `go/match/internal/room`，与 team 同进程，套用 BattleStarter 端口分层（room 不 import logic）。客户端服务 `ClientPlayerCustomRoom` 配新节点类型（名字与 package 名对齐，见 §8.5.2；理由同 team 的 DV-4：新服务名只有新二进制会注册）。
- **短房号**：房间内部 id 由 match 的 `BattleIDGen` 发（64 位雪花，不用 INCR，避免历练 lobby 的回卷复用问题），但它没法口头报给别人。所以另发一个 6 位数字短房号，映射 `room:code:<code> → room_id`，随房间同寿命；撞号重试；按短号加入同样走限频。
- **房间参数**（不在板子里）：每边人数（1..5）、是否允许好友直接按房号加入。CUSTOM 局 v1 **不开观战**：battle 本来就拒绝参战者当观众，"只对房间成员可见"在现有模型下等于谁都看不了；先固定为私密，观战席另行设计。
- **存储**：MatchRedis，全部 key 带 `{mr}` hash tag（`match:{mr}:room:<id>`、`…:room:of:<pid>`、`…:room:invite:<pid>`、`…:room:code:<code>`），多 key Lua 不会 CROSSSLOT。两点代价写明：MatchRedis 是 `maxmemory-policy volatile-lru`，带 TTL 的 key 可能被**单独**淘汰——`room:of` 被淘汰而 `room` 还在时"一人只在一房"会破，所以 Lua 里每次都双向互验；`{mr}` 把全部房间压在单个 slot，只用到集群三分之一的主节点。房间丢失 = 解散，零资产影响。
- **状态机**：WAITING → STARTING → IN_BATTLE → WAITING（可再来一局）；空闲 30 分钟过期。STARTING / IN_BATTLE 下拒绝离开、改规则、换座、踢人；`room:invite:<pid>` 是单键，新邀请覆盖旧邀请（旧邀请的应答回"邀请已失效"）。房主改规则：重置全员准备、version + 1。开战提交点复核：全员准备、每边人数等于房间设定、每个成员都没有 `battle:lock`、规则按当前表复核，然后调 `RunCustomGather`，用房间座次预设分边。
- **邀请**要查目标的黑名单。现在 match 不查 friend 黑名单、也没有内部 RPC，需新增 `FriendInternal.IsBlocked`（Java 同批）。
- **容量**：`ArchiveCompletion` 对所有模式都归档，"再来一局"会让 CUSTOM 局频繁写进 TiDB——上线前估算容量，或给 CUSTOM 局归档设保留期 [I]。

#### 8.6.5 自动战斗策略（P4，宝宝优先）

1. 玩家用 `SetPlayerStrategyLoadout(slot, template_id)` 选用：只能选自己的模板（别人的先凭码复制），`slot` 必须与模板基底的 `strategy_actor` 一致。**复制来的策略里的 `UseItem` 规则默认关闭**，需要本人确认后才启用，防止被诱导自动消耗贵重道具。
2. **解析时机**：在 `teamAssignment` 之后、`PrepareBattle` **之前**调 `PlayerTemplateInternal.ResolveBattleStrategies(player_ids)`，并把它的耗时（预算 300ms）写进 `queue.go` `matchedTicketTTLFor` 的封闭公式与 `ticket_cas_test.go` 的复算守卫——`prepare_deadline_ms` 在 PrepareBattle 时就已下发给 scene，事后改不了。
3. **不让模板服务成为所有开局的同步依赖**：只对设置了 loadout 的玩家调用（match 侧缓存"谁有 loadout"，由模板服务在 loadout 变更时失效）；加熔断（连续失败后一段时间内跳过这一步）；失败或超时不带策略继续开局并计 `outcome=error`（fail-open）。
4. 结果写 `BattlePlayerSnapshot.auto_strategy = 21` 与 `BattlePetSnapshot.auto_strategy = 13`（两条消息现有最大字段号 20 和 12）。唯一写方是 match（`gather.go` 的快照写点），对每份快照都覆盖或清空。规则集或覆盖层禁用策略时整份清空。
5. battle 在 `Initialize` 时编译成 `CompiledBattleStrategy`（与怪物策略共用编译器和求值器），引擎**自己**再执行一遍原子 player_selectable、组合矩阵、禁用开关检查。编译结果放私有 `std::map<actor_id, …>`，**不进 `BattleActorState`**（H5）。
6. `FillDefaultActions`（唯一接缝）：PET 有策略时替换现行"取第一个能放的技能"（宝宝恒为自动行动，`InitPets` 里固定 `set_is_auto(true)`，所以宝宝策略一上线就生效）；PLAYER 没有 pendingAction 且有策略时按 priority 求值，候选过 `CheckActionPrerequisites`，全部落空回到现行"普攻 + target 0"。超时玩家与挂机玩家都按策略出手（推荐），这样 `is_auto` 不进事件语义，回放不必逐回合记录它。
7. **可解释性**：玩家不知道本回合按哪条规则出手，"策略没生效"会成为常见客诉。参照 `RedactStateForViewer`，只给本人下发"命中第 N 条 / 落空回退普攻"；C++ 指标加 `outcome=fell_back_default`。
8. v1 战斗中不能切换策略；挂机开关保持现状（不持久化）。

#### 8.6.6 确定性、回放与指纹

- 覆盖层和策略都是 `CreateBattleRequest` 里的输入。注意现有幂等：只有带 `placement_operation_id` 的路径做 canonicalRequest 比对，普通路径只按 `FindRoom` 判幂等——两条路径对新字段的处理要一起写测试。回放材料 = `CreateBattleRequest` + 被接受的指令流 + seed + 表指纹 + 引擎版本 + 节点强制终局记录（§8.6.3）。
- 不新增 RNG 抽取点：目标同分按 (hp, actor_id) **无符号**升序决胜（Java 用 `Long.compareUnsigned`，怪物和宝宝的 actor_id 带 bit63）；阈值用整数万分比，复用 `PercentCeil` 写法并防住 max_health 为 0 的除零。
- 指纹：battle 不读模板相关的表，它们不进 `BattleTableFingerprint`；`BattleAtomKind`、`BattleRuleSet` 由 §10 前序步骤追加；玩家取值是输入数据，不进指纹。Go 侧（match、模板服务）用到的表没有战斗指纹，所以冻结规则时带上 match 所用配表批次的 manifest digest（`rules_manifest_digest`），battle 比对；不一致按指纹模式处理（enforce 时拒绝）。
- 兼容硬约束：没有策略、没有覆盖层时事件流必须与现行逐字节相同，现成回归是 `AutoModeMatchesManualDefaultAttackEventStream`。

### 8.7 防刷与资产（硬约束）

**`MATCH_MODE_CUSTOM` 局一律不发经验、金币、掉落，不推任务和活动，不计评分和排行，不计帮会历练，不消耗道具，不回写 HP / MP 和宝宝状态（无损）。** 写在代码里，配表没有开关可以绕开。

| # | 卡点 | 位置 | 要做的事 |
|---|---|---|---|
| F1 | 评分 / 排行 | `go/match/internal/logic/rating.go` `isRatedMode`（只认 1V1 / 5V5，default 为 false） | CUSTOM 永不加入；单测 `TestCustomModeNeverRated` |
| F2 | 入队 | `joinqueuelogic.go` `JoinQueue` 的 `switch in.Mode` | default 分支已拒绝未知模式，只需补测试；CUSTOM 只能经房间或切磋开局 |
| F3 | 活动上下文 | `gather.go` `runGatherWithOptions` / `gatherOptions.activityContext` | §8.6.2 的断言；`RunCustomGather` 签名无该参数 |
| F4 | 产出源 | `turn_battle_engine.cpp` `BuildSettlement` / `RollDrops` | CUSTOM 产出无副作用结算：exp / gold / items_gained / defeated_monsters / items_consumed 全空，health / mana / pets 回填开局快照值，is_dead = false |
| F5 | 建房 | `HandleCreateBattle`（`EmplaceRoom` 之前）/ `TurnBattleEngine::Initialize` | §8.6.2 白名单；CUSTOM 且规则集 spawn_monsters = true 时拒绝（v1） |
| F6 | 应用点（纵深防御） | scene `PlayerBattleSystem::ApplySettlementToEntity`（唯一结算入口，含离线 pending 补应用；现在不看任何局类型） | `BattleSettlementData` 追加 `uint32 match_mode = 16`（battle 填；该消息下发客户端，属客户端契约）。CUSTOM 时只记账本、走解锁销账，不做 AddCurrency、HP / MP、`PetSystem::ApplyBattleSettlement`、`ApplySettlementItems`、`OnCondition`；收益字段非空就 ERROR 并计 `metric=battle_custom_settlement_nonempty`（应恒为 0，接 critical 告警） |
| F6b | 独立交叉核对 | `PrepareBattleRequest`（`proto/scene/scene.proto`，现不带 match_mode） | match 在冻结时下发 match_mode，scene 记在备战状态里，结算时与 `settlement.match_mode` 交叉核对，不一致按最严口径（CUSTOM）处理并告警——否则 F6 只是在用"被防的那一方填的字段"防它自己 |
| F7 | 帮会历练 | `go/guild/internal/kafka/trial_result_consumer.go` `isGuildTrialResult`、`activity_trial.go` `SettleTrialResult` | 现有逻辑已双重 fail-closed（mode ≠ PVE_TEAM 判 BadContext）；补 CUSTOM 被跳过的单测 |
| F8 | match-results 新消费者 | `BattleResultEvent.match_mode` | 规则：任何基于战斗结果的新进度（胜场成就等）必须按 mode 白名单接入，CUSTOM 永不进；归档不算发奖 |
| F9 | 观战 | `go/match/internal/logic/watchbattlelogic.go`、`spectate.go` | CUSTOM v1 不开观战（§8.6.4）。另：`WatchBattle` 按 battle_id 指定时目前没有任何鉴权，而 battle_id 是秒级雪花、可推算——私密局上线前必须补成员 / 可见性校验，并覆盖"预写记录、尚未进活跃集合"的窗口 |
| F10 | 枚举重复 | `turn_battle_constants.h` `kMatchModePveSolo / PveTeam` | 新增 `kMatchModeCustom = 7`，与 proto 同批改 |
| F11 | 事后兜底 | AnomalyDetector / GainBlock | CUSTOM 局的任何收益本身就是异常，接告警 |

**顺带记录一个现存问题**：现在任何对局（切磋、PVE 都算）结算时，只要阵亡或 HP 归零就满血满蓝复活（`ApplySettlementToEntity` 里通用的阵亡基础复活，不分局类型；宝宝见 `player_pet.cpp` 的 `PetSystem::ApplyBattleSettlement`）。CUSTOM 局如果走现行结算，同样会「送死回满」。CUSTOM 局能把伤害开到 2 倍，若沿用切磋结算，"送死回满"会更快——这也是 CUSTOM 定为"无损"的理由之一。普通切磋的这一行为是否属于缺陷，另行判断。

**策略对普通对局的影响**：不改变任何发奖规则；`UseItem` 照常消耗玩家道具（玩家自己的选择，复制来的默认关闭），PVP 次数上限照常由 `CheckItemUse` 管住；策略产生不了手动做不到的行动；挂机刷 PVE 的强度会提高，属于反外挂范畴 [I]。其他滥用：用 CUSTOM 局长时间挂 `battle:lock` 躲避别的玩法，由 match deadline 约束；小号互刷胜场零收益。

### 8.8 社交、内容安全与可观测

1. **v1 不开自由文本**：板子的名称用"官方板子名"或自动生成的摘要（如"30 回合·打满判平·药限 3·伤害 ×1.5"），描述不开放。板子内容全是数值选项，唯一的内容安全风险就是名称和描述，推迟它就能去掉 textguard 前置、6 个文本拒绝码、`SetPlayerTemplateMeta`、`ForceResetName` 与举报。
2. **P5 开自由文本时**：名称和描述都用"拒绝"语义（不屏蔽：屏蔽后的文本没法展示，也会让查重失效）；把 `go/chat/internal/textfilter` 下沉为 `go/shared/textguard`（现在 internal 包跨模块引用不到），规范化沿用 `go/shared/playername` 的 NFKC → Trim → Lower；模板名单独一套字符集（CJK + 字母数字 + 少量标点）；敏感词三套实现语义不一致，要先定 textguard 为权威并统一词库、Java 对齐。
3. **可见性**：P3 私有 + 凭分享码；好友可见需要 friend 新增内部查询；公开广场、搜索、点赞（P6）前提是先有审核队列；广场用新表、热门排序走 go/rank，不要在模板行上维护热点计数（schemamigrate 不补索引）。收藏推迟到有广场之后；复制保留（P3）。
4. **举报与下架**（P5）：`ReportPlayerTemplate` 落表；同一板子被 N 个不同举报人举报后自动置"待审"并隐藏查询；`PlayerTemplateAdmin`：TakeDown / Restore / ForceResetName / SanctionPublisher / ListReports / Get，鉴权走 domainhost `Producers.admin` + serviceauth，operator / reason 必填且限长，request_id 幂等，审计行与状态变更同事务；不照抄 `java/gateway_node` 的 AdminApiKeyFilter（`String.equals` 比较、没有操作人）。
5. **限流**：每个新客户端消息号都在 `data/MessageLimiter.xlsx` 加一行（否则落到缺省每秒 3 条）；创建 / 保存每人每分钟 10 次 + 按游戏日上限（§8.5.3），故障拒绝；分享码解析见 §8.5.4（故障拒绝）；邀请每分钟 10 次；举报每游戏日 5 次。killswitch 只当止血阀。
6. **审计日志**：创建、修改、删除、可见性变化、重置分享码各打一条结构化日志，带 player_id、template_id、revision；未通过校验的原文只记字节数。
7. **指标**（label 只用有限枚举，禁止 player_id、template_id、分享码、模板名，照 player_inbox `metrics_test` 做机械断言）：
   - Go：serverbase 已经按 method 产出 `rpc_duration_seconds{method,status}` 与 `rpc_inband_reject_total` / `rpc_inband_fault_total`，不再新增按 method 的请求计数；只加业务维度的 `player_template_validate_reject_total{reason}`、`player_template_share_lookup_total{outcome}`、`player_template_strategy_resolve_total{outcome}`、`match_custom_room_total{event}`、`match_custom_battle_start_total{outcome}`。
   - C++ battle / scene 按 §3.8 进程内聚合、每 60 秒一行：`metric=battle_custom_rules`、`metric=battle_player_strategy outcome=compiled|ignored_invalid|ignored_forbidden|fell_back_default`、`metric=battle_custom_settlement_nonempty`。
8. **告警落到文件**：C++ 日志型指标按 `deploy/k8s/asset-channel-alerts.yaml` 的 LogQL 先例先写成值班固定查询（该文件注明 Loki ruler 尚未挂规则目录、没有 alertmanager_url，ruler 接线后才转为告警规则），ruler 接线后 `battle_custom_settlement_nonempty` 设为 critical，在此之前列入值班必查项；新 Go 服务照 `player-inbox-alerts.yaml`、`rank-alerts.yaml` 建自己的告警文件；模板服务故障（规则 fail-closed 拒绝开局、策略 fail-open）都要有告警。

### 8.9 协议

**客户端可见（两版同批改，Java 用 ContractSync 同步）**：

| 阶段 | 项 | 内容 |
|---|---|---|
| P1 | `MatchMode` | 追加 `MATCH_MODE_CUSTOM = 7`（现有 0–6） |
| P1 | `ChallengePlayerRequest` | 追加 `CustomRulesRef custom_rules = 4`（oneof：`preset_id` / `inline`）；`ChallengeInviteS2C`（`NotifyChallengeInvite` 的推送体，现有字段 1–5）追加规则摘要 |
| P1 | `BattleSettlementData` | 追加 `uint32 match_mode = 16` |
| P1 | 配表 | ParamKind、Base、Option、OptionChoice、Preset、PresetValue 六张；`PlayMode.customizable`；`BattleRuleSet.items_forbidden / pets_forbidden`；PlayMode 的 CUSTOM 行；MessageLimiter 新行 |
| P2 | 新文件 `proto/room/custom_room.proto`，服务 `ClientPlayerCustomRoom` | `CreateCustomRoom{request_id, side_size, oneof source{preset_id, inline, share_code, own_template_id}}`、`GetMyCustomRoom`、`JoinCustomRoomByCode{room_code}`、`UpdateCustomRoomRules{room_id, expected_version, source}`、`InviteToCustomRoom{room_id, target_player_id, side}`、`RespondCustomRoomInvite{room_id, accept}`、`ChangeCustomRoomSeat`、`SetCustomRoomReady`、`KickFromCustomRoom`、`LeaveCustomRoom`、`StartCustomRoomBattle{room_id, expected_version, request_id}`；推送 `NotifyCustomRoomSnapshot` / `NotifyCustomRoomInvite` / `NotifyCustomRoomEvent` |
| P2 | `CustomRoomView` | room_id、room_code、owner_id、version、frozen_rules（base_id、取值、来源、摘要）、seats{side, player_id, ready}、side_size、state、battle_id、expires_at_ms |
| P3 | 新文件 `proto/player_template/player_template.proto`，服务 `ClientPlayerTemplate` | `ListMyPlayerTemplates{kind}`、`GetMyPlayerTemplate{template_id}`（仅作者）、`GetPlayerTemplateByShareCode{share_code}`、`CreatePlayerTemplate{request_id, base_id, content}`、`SavePlayerTemplateContent{template_id, expected_revision, content}`、`SetPlayerTemplateVisibility{template_id, expected_revision, visibility}`、`DeletePlayerTemplate{template_id, expected_revision}`、`ResetPlayerTemplateShareCode{template_id, expected_revision}`、`CopyPlayerTemplateByShareCode{request_id, share_code}` |
| P3 | `PlayerTemplateView` | 作者视图：template_id、kind、base_id、base_version、content、revision、visibility、share_code、state、`TipInfoMessage unusable_reason`、updated_ms、forked_from_template_id；凭码视图：只回 kind、base_id、content、owner_name（回源）、摘要，不回 template_id |
| P4 | 新文件 `proto/battle/battle_strategy.proto`；`SetPlayerStrategyLoadout{slot, template_id}`；本人可见的"命中第 N 条"下发 | — |
| P5 | `SetPlayerTemplateMeta{…name, description}`、`ReportPlayerTemplate{share_code, reason_code}` | — |
| 各期 | tip | P1 / P2 码进 `match_error`；P3 起 `player_template_error` 新组（§8.4） |

消息号由 protogen 发号，同批新方法的号序是随机的（team-system.md DV-5），客户端必须重新生成。

**服务端内部**（不对客户端，Java 按自己的惯用方式实现等价语义）：`CreateBattleRequest.custom_rules = 14`；`MatchFormationRecord` 的 custom_rules 与座次字段；`PrepareBattleRequest` 的 match_mode；`BattlePlayerSnapshot.auto_strategy = 21`、`BattlePetSnapshot.auto_strategy = 13`；`PlayerTemplateInternal`（`ResolveRulesForUse`、`ResolveBattleStrategies`）；`PlayerTemplateAdmin`；`FriendInternal.IsBlocked`；新节点类型。

**双版本工作量（定量）**：

| 项 | 现状 | 本功能新增 |
|---|---|---|
| 客户端消息号 | 317 个（`proto/message_id.txt` 全仓总数，含内部 RPC 与 8 个 reserved；其中客户端协议服务（OptionIsClientProtocolService）约 182 个） | 约 27 个（模板服务约 13、房间约 11、推送 3），两版都要加 MessageLimiter 行 |
| 配表 | 45 张（含 Test / TestMultiKey 两张测试表） | §8 自己 7 张（含官方板子两张）+ 前置步骤 6 张。"只给 Go 用"的表照样会生成 C++ 代码、要改 `table.vcxproj`（配表 option 不能按语言裁剪），Java 的 game-table-codegen 也照样要生成并加载 |
| Java 模块 | — | 新建 1 个（模板服务，参照 game-team），改动 7 个：game-match、game-battle-engine、game-battle、game-scene、game-friend、game-gate、game-guild |

Java 侧要做的事：① 模板服务模块（独占存储）+ 共享校验模块（与 Go 共用对拍向量，含代码硬边界）；② game-match：CUSTOM 模式、带板切磋、全球权威恢复路径带规则与座次、房间与短房号、评分白名单、策略 Resolve 注入；③ game-battle-engine：建房前白名单、覆盖层应用、CUSTOM 无副作用结算、`fillDefaultActions` 接入宝宝与玩家策略（替换 `defaultPetAction`）；④ game-battle：建房白名单在副作用之前；⑤ game-scene：结算按 match_mode 跳过所有变更、与备战时记下的模式交叉核对；⑥ game-guild：CUSTOM 跳过的测试；⑦ game-friend：黑名单内部查询；⑧ game-gate：新服务的路由和限频。Java 匹配已有 M1–M33 有意差异（Java `PARITY.md` 的匹配行），切磋和房间的扩展要逐条对齐。前置追平：`armRoundTimer` 没有按单位数放宽、快照没有 combat 和参战 buff、跨语言金样未建。

### 8.10 分期（每期由 Codex 编译测试；Claude 不跑）

**前提**：§10 第 1 步、第 3 步、第 5 步已落地（§8.0 第 8 条）。

| 阶段 | 最小可交付 | 验收标准 |
|---|---|---|
| **P1 官方板子 + 带板切磋（1v1 CUSTOM）** | 六张策划表与示例行（含 3–5 个官方板子）、代码硬边界常量 + 三端对拍向量、`go/shared/playertemplate` 规范化与校验包；MatchMode 7、`ChallengePlayerRequest.custom_rules`（preset / inline）、`RunCustomGather`、全球权威恢复路径带规则与座次、字段 14、battle 建房前白名单、覆盖层、无副作用结算、scene CUSTOM 跳过与交叉核对、F1–F11；不建模板服务、不开自由文本 | 导表：坏表（step = 0、越出代码硬边界、default 不对齐、改了不可变项）在导表期 / 启动期失败。C++：无覆盖层的全部同种子用例逐字节不变；覆盖层生效（回合上限、伤害档位、禁药 / 药限、禁宝宝）；非 CUSTOM 带覆盖层、CUSTOM 缺覆盖层、未知 param_kind 都在 `EmplaceRoom` 之前被拒；CUSTOM 结算收益与消耗为空、HP 等于开局值。scene：CUSTOM 结算不改任何组件、只记账本；模式交叉核对不一致时按 CUSTOM 处理并告警。Go：`isRatedMode(CUSTOM) = false`、activityContext 断言、JoinQueue 拒绝 CUSTOM、全球权威"ReserveDirect 后崩溃"恢复出的局带规则与正确座次、guild 跳过 CUSTOM、最大请求 `ClientRequest.ByteSizeLong() < 1024`。Java 同批并补金样用例。MSBuild 串行 `/m:1` |
| **P2 自建房** | `go/match/internal/room` + 新节点类型、短房号、邀请（含黑名单）、选边、准备、改规则、开战、过期 | 并发不变量测试（一人只在一房、开战恰好一次，照 team `TestConcurrentInvariantsFuzz`）；模拟 `room:of` 单独淘汰后不变量仍成立；STARTING / IN_BATTLE 下拒绝离开、改规则、换座；改规则重置准备；成员在战斗中时拒绝开战；短房号撞号重试 |
| **P3 模板服务（私有保存 + 分享码，名称用自动摘要）** | `go/player_template`：表、`-migrate`、domainhost 接入与密钥、Create / Save / Visibility / Delete / Reset / GetMy / GetByShareCode / CopyByShareCode、按码解析统一入口与配额、owner 行哨兵、按日创建上限、硬删 / 软删、幂等行保留期、最小运维下架、指标；房间与切磋接 `share_code` / `own_template_id` | 越界回对应 tip；并发创建不超限（含 owner 行首次创建的并发）；同一 request_id 只建一个；revision CAS 冲突；非作者用 template_id 一律 `ShareCodeUnavailable`；私有 / 下架 / 删除 / 不存在对他人回同一个码且判定顺序不泄露存在性；所有入口共享未命中配额；分享码编解码与 32 字符表内校验位；指标 label 不含玩家标识的机械断言；本地一次性 MySQL 迁移成功（不借 codex-* 容器） |
| **P4 自动战斗策略（宝宝优先）** | 策略基底、loadout、`ResolveBattleStrategies`（PrepareBattle 之前、计入 TTL 公式、只对有 loadout 的玩家、熔断）、match 注入字段 21 / 13、引擎编译与求值（宝宝先、玩家后）、本人可见的命中提示 | `AutoModeMatchesManualDefaultAttackEventStream` 不变；每个原子有用例；非法策略被引擎确定性忽略（含组合矩阵、禁用开关）；宝宝策略替换默认路径；超时玩家按策略出手；复制来的 UseItem 规则默认关闭；Resolve 故障 fail-open 且计数、熔断生效；`ticket_cas_test` 复算通过；满配基准（20 单位 × 16 规则）写回 §4.5；C++ 导出事件 hex，Java 回放一致 |
| **P5 自由文本 + 内容安全 + 举报 + GM** | textguard 下沉与统一词库、名称 / 描述、举报、Admin 全套 | 敏感文本被拒；举报阈值隐藏；admin 鉴权、幂等、同事务审计 |
| **P6（可选）** | 好友可见、公开广场与搜索（需审核队列）、收藏、聊天结构化卡片、观战席 | 单独立项 |

每期 proto 生成用 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`；新表同步改 `cpp/generated/table/table.vcxproj` 与 CMakeLists；新 proto 域先补 `domain_meta`。

### 8.11 与狼人杀体验的差距（如实写明）

- 狼人杀有公开房间列表、观战席、短房号口头报号即进；本方案 P2 有短房号，但没有公开房间列表，CUSTOM 局不开观战（§8.6.4）。
- 狼人杀板子的核心是"角色组合"，本游戏对应的是规则参数，没有"统一属性"这一好友竞技最常用的公平选项（§8.1）。
- 玩家板子在 P3 才能保存和分享；P1 / P2 只能用官方板子或现场调参。
- 策略在 P4 才有，宝宝先于角色。

### 8.12 风险与待拍板

1. **H10 例外**（需拍板）：`CreateBattleRequest` 加字段 14。fail-closed 由 battle 在副作用之前的模式与参数白名单保证；上线后混跑再加节点能力位。
2. **CUSTOM 局"无损"**（推荐，评审一致），还是沿用现行通用结算（真实扣药、回写 HP、阵亡即满血满蓝复活）。
3. **评分 PVP 里是否允许玩家策略**：建议默认禁用，由 `BattleRuleSet.player_strategy_forbidden` 控制。
4. **策略对超时玩家也生效**（推荐）。若只对挂机玩家生效，`is_auto` 就成了事件语义的输入，回放必须逐回合记录。
5. loadout 只能选自己的模板（推荐），别人的先复制。
6. 宝宝策略按"出战宝宝槽位"绑定（v1），还是按 pet_id 绑定（以后另建表）。
7. 房间放 MatchRedis 的 `{mr}`（推荐，与 challenge 同库，代价见 §8.6.4），还是照 team 的 DV-2 放 SharedRedis。
8. `MAX_ROUNDS` 硬边界 45 是否够用；不够就改为按冻结值推算 deadline（§8.6.3）。
9. PVE 练习房（CUSTOM + 刷怪）v1 不做；要做的话 F4 / F6 必须先有用例证明零收益。
10. 技能没有敌我目标语义（`ValidateSkillTarget` 不区分）：v1 的 `CastSkill` 只配敌方目标，"低血量自疗"只能用道具或自身 buff 技能；要开放治疗技能，得先给 Skill 表补目标侧语义。
11. 敏感词三套实现语义不一致（P5 前置）。
12. tip 段：`activity_error` 31000 / `player_template_error` 32000 的分配需与 §5.5 协调。
13. 公开广场、搜索、点赞必须先有审核队列和统一词库；两仓都没有举报和审核先例。
14. 作者删号如何处置他名下的板子（无先例）。
15. battle 单 loop：20 单位全部带满配策略的开销只是推断（每回合每房间约 2×10^4 次整数运算），**没有基准**，P4 必须补上。
16. CUSTOM 局"再来一局"带来的归档写量（`ArchiveCompletion`）需要容量估算或保留期。
17. `WatchBattle` 按 battle_id 无鉴权是现存问题，私密局上线前必须补（F9）。

### 8.13 v0.1 → v0.2 评审修订记录

| # | v0.1 写法 | 问题（评审出处） | v0.2 改为 |
|---|---|---|---|
| 1 | 回显摘要 `applied_custom_rules_digest` 实现 fail-closed | battle 回包前已推 `BattleStartS2C` / `SendBattleConfirmedEvent`；幂等命中回空响应；恢复路径不看摘要（安全 M4） | 删掉回显；battle 在 `EmplaceRoom` 前按模式与参数白名单拒绝 |
| 2 | 全球权威模式"冻结规则已进 gatherOptions" | `reserveGlobalDirect` / `runGlobalFormation` 只持久化 activityContext；`decideGlobalChallenge` 传空 opts；`teamIndexFor` 未知模式返回 0 队（安全 M3） | `MatchFormationRecord` 加规则与座次，CUSTOM 显式分支与断言 |
| 3 | 非作者可用 template_id 访问 / 收藏 | template_id 单调递增可扫号，Reset 失效被绕开（安全 M1） | 非作者只能凭分享码；判定顺序先可见性；收藏推迟 |
| 4 | 限频只挂在 GetByShareCode | 其他四个入口可绕开枚举（安全 M2、产品 M6） | 模板服务内唯一的按码解析入口，按 caller 统一计数，故障拒绝 |
| 5 | 硬边界 abs_min / abs_max 放在策划表 | 字典本身无人校验，一处笔误穿透所有层（安全 M5） | 硬边界改代码常量 + 对拍向量；表只镜像编号；battle 不读、不进指纹 |
| 6 | `MAX_ROUNDS` 5..300 | 整场 deadline 300 秒 ≈ 50 回合，超出部分被墙钟平局改写（安全 M6） | 硬边界 45；回放补节点强制终局 |
| 7 | 存完整集合 + state=1 校验 + 锁定项等于 default | 三条互相矛盾，下线或改默认值会让存量板子报废；step=0 除零（安全 M7） | 先规范化再校验；锁定项不入存档；补导表校验；不可变项钉住 |
| 8 | owner 行 FOR UPDATE + 存计数 | 首次创建锁不住、1213 间隙锁环；计数是第二份真相（安全 M8） | 先 INSERT IGNORE 哨兵行；锁内 COUNT |
| 9 | 客户端上传 source_* / schema_version | 可伪造，污染审计（安全 M9） | 保存时清零，只由服务端填；match 覆盖或清空快照字段 |
| 10 | PrepareBattle 之后再 Resolve 300ms | 越出 gather 的封闭时间预算；模板服务成为所有开局的同步依赖（安全 M10、产品 M5） | 挪到 PrepareBattle 之前并计入 TTL 公式；只对有 loadout 的玩家；熔断 |
| 11 | tip 段 30000 空闲 | 30000 已被 `mystery_merchant_error` 占用（两路评审） | P1/P2 用 match_error；activity 31000、本功能 32000 |
| 12 | C2S 每条规则约 40B | 校验的是外层 ClientRequest；int64 负数 10 字节，16 条即超 1KB，超限还计非法包（两路评审） | 参数改 uint32、区间 < 2^21；单测按最坏编码 |
| 13 | Crockford mod 37 校验位 | 校验符含 `*`，与聊天打码混淆（两路评审） | 校验位只用 32 字符表内字符 |
| 14 | `ITEM_USE_CAP` 0..20 | 规则集里 0 = 不限，玩家选 0 以为禁药（产品 M2） | 下限 1，禁药独立布尔项 |
| 15 | `FLEE_ALLOWED` 可配 | PVP 下逃跑无语义，宝宝不能逃（产品 M3） | v1 删除 |
| 16 | `SIDE_SIZE` / `SPECTATE_POLICY` 在板子里 | 分层错误，板子替使用者决定房型与隐私（产品 M4、安全 K6） | 改为房间参数；CUSTOM v1 不开观战 |
| 17 | 跨项约束表 + 约束字典 | 示例不成立，不满足原子准入纪律（产品 C3、安全 K4） | v1 不建 |
| 18 | 官方预设"可选" | 大多数人玩官方板（产品 A1） | 官方板子必做、为主入口；伤害改档位 |
| 19 | P1 = 模板服务增删改查 | 交付后玩家拿不到能用的东西（产品 C1） | P1 = 官方板子 + 带板切磋，不建服务、不开自由文本 |
| 20 | 自由文本名称描述 v1 开放 | 唯一的内容安全风险，牵出大量前置（产品 C2） | 推迟到 P5，v1 用自动摘要 |
| 21 | 收藏 P5 | 无广场时价值低（产品 C5） | 推迟到 P6 |
| 22 | 新增 `player_template_requests_total` | serverbase 已有（产品 C4） | 删除 |
| 23 | 存储行数 | 反复建删无上界（产品 M7） | 按日创建上限、未分享硬删、幂等行保留期 |
| 24 | domainhost / protogen 接入未写 | Producers 密钥、`domain_meta`、节点类型命名（产品 M9、M10） | §8.5.2 接入清单 |
| 25 | F6 只看 battle 填的 match_mode | 用被防的一方填的字段防它自己（安全补 1） | scene 备战时记下模式并交叉核对 |
| 26 | 引擎只查程序不变量 | 组合矩阵与禁用开关是确定性不变量（安全补 2） | 引擎也执行 |
| 27 | 房间号用雪花 id、观战按 id | 无法口头报号；WatchBattle 无鉴权（产品 A2、安全补 5） | 短房号；F9 补鉴权 |
| 28 | Redis 淘汰未考虑 | volatile-lru 下 key 可单独淘汰（产品 A4） | Lua 双向互验；写明单 slot 代价 |
| 29 | 策略黑箱 | "策略没生效"客诉（产品 A3） | 本人可见的命中提示 + fell_back_default 计数 |
| 30 | P3 先角色后宝宝 | 宝宝恒为自动行动（产品 A8） | 宝宝优先 |

**全文源码核对（2026-10-19）**：v0.2 定稿后由 6 个只读核对员逐章对照源码核对 407 条现状断言，修正 36 处，影响设计的有：`CastSkill` 引用技能的 effect[] 也会经 `RollAilmentResist` 消耗随机数（H3 扩大、§4.6 示例换技能）；AOE 技能同样要求非 0 目标；`player_database` 的 21 / 22 号已被占用（下一个空闲号 23）；阵亡回满是所有对局的通用结算、不是切磋专有；Java 待追平的是两处而不是三处；全球权威路径的 deadline 写死在 `authority.Store.claimTx`；`CheckAccept` 现在没有任何准入门槛。

---

## 9. 双版本分工

| 领域 | C++ / Go 版 | Java 版 |
|---|---|---|
| 配表与导表器 | Python 导表器加通用规格校验器、时间字符串列 | `TableSourceGenerator` 生成的加载期校验同步实现同一套规格校验 |
| 战斗策略 | battle 节点 `battle_strategy` 模块、`FillDefaultActions` 接入、指纹追加 | game-battle-engine `fillDefaultActions` 同位置接入、`BattleTableFingerprint` 同序追加 |
| 玩法 | go/match 接表与处理器；C++ battle 规则集读点 | game-match 队列 / gather / 评分；game-battle-engine 规则读点；game-guild 历练校验 |
| 活动 | C++ scene `PlayerActivitySystem`、gameday 移植、`ClaimActivityReward` | game-scene 对应解释器、存储同名字段、gameday 对拍向量 |
| 玩家自定义模板 | `go/shared/playertemplate` 校验包与代码硬边界；match 的 CUSTOM 模式、带板切磋、全球权威恢复、自建房与短房号、策略注入；battle 建房前白名单、覆盖层与无副作用结算；scene CUSTOM 跳过与交叉核对；P3 起 `go/player_template` 全局服务 | 新建 1 个模板服务模块 + 共享校验模块（与 Go 共用对拍向量）；改动 game-match / game-battle-engine / game-battle / game-scene / game-guild / game-friend / game-gate 七个模块（§8.9 定量清单） |

每完成一步，在 Java 仓 `PARITY.md` 登记一行，本仓 `docs/PROGRESS.md` 条目注明对应 Java 版本号。

---

## 10. 落地顺序

每一步都交给 Codex 编译、测试；Claude 不执行构建与测试（AGENTS.md §10.1）。

**第 0 步：文档**（本文）。另行勘误 `data/AGENTS.md` 里"proto 里可以直接写 enum"的说法。

**第 1 步：还债，不写新功能**

1. 指纹从 `warn` 切 `enforce` 的灰度方案：match `TableFingerprintMode`、battle `battle_table_fingerprint_mode` / `BATTLE_TABLE_FINGERPRINT_MODE`。先在 dev 环境 enforce，再按 zone 逐步切。
2. 建跨语言战斗金样：在本仓导出同种子用例的事件 hex 作为夹具，Java 回放比对（Java battle-engine-spec §13.9 的 G1–G14 场景）。**需用户同意后再做**。
3. Java 追平 C++ 已漂移的两处：PVE 人数档（`19a3b8980`）、装备属性（`10cca6b66`，含连击 / 反击 / 反震 / 抗异常与 `BATTLE_EVENT_RESIST` / hit_kind；Java `PARITY.md` 已登记待跟）。

**第 2 步：导表器能力**

- `orchestrator.py` 挂按字典规格的通用校验器（列角色、区间、条件外键、未用参数为 0）；Java 同步。
- 新增时间字符串列：cfg option 声明"UTC+8 墙钟串 → UTC 毫秒"，替代手填毫秒戳（填错一位就差好几天，而导表期连 start < end 都不查，只在 scene 运行期由 `PlayerActivityScheduleSystem::BuildInfo` 按查询拒绝）。
- 导表时输出规则渲染报告。

**第 3 步：战斗策略 MVP（单阶段）**

- 表：`BattleAtomKind`、`BattleStrategy`、`BattleStrategyRule`（Phase 表先只允许一行阶段，或第 4 步再建）。
- 原子：条件 3 个、目标 4 个、行动 2 个（§4.2）。
- 改动点：`Monster.strategy_id`、`FillDefaultActions` 接入、`AppendMonsterActor` 派生技能集、`BattleDataProvider.FindStrategy`、DecisionTrace、聚合指标、启动期校验、指纹追加；Java 同批。
- 验收：① `strategy_id = 0` 时现有全部同种子用例逐字节不变；② 每个原子有用例，坏表在导表期失败；③ C++ 导出新用例的事件 hex，Java 回放一致。

**第 4 步：阶段状态机**：`BattleStrategyPhase` + `BattleAiState`；阶段只在回合开始推进，事件放在组清零之后（H4）。可选：宝宝接入。

**第 5 步：玩法 v1**：`PlayModeKind`、`PlayMode`、`BattleRuleSet`；match 接表；`IsPveMatch` 读点改读规则集；删 `PveTeamSizeByConfigId` 与 `RatingDrawRoundCap`；3V3 处理器。前提：第 1 步第 3 项完成，否则规则集在两版起点不同。

**第 6 步：活动 v1（个人维度）**：五张活动表 + `ActivityEventKind`；`PlayerActivityComp` 顶层字段与 DB 列；gameday 移植与三版对拍；`ClaimActivityReward` 与 `activity_error` tip 组（建议 31000）；`GetActivityList` 合并输出。同批把**活动实例表做成第一张可热更的叶子表**：在 scene loop 线程上整表替换、只按 id 读、解析失败回退旧快照而不是 `LOG_FATAL`，这样改排期不用重启 scene。

**第 7 步：玩家自定义模板**：按 §8.10 的 P1–P6 分期：P1 官方板子 + 带板切磋（不建模板服务、不开自由文本）→ P2 自建房与短房号 → P3 模板服务（私有保存 + 分享码）→ P4 自动战斗策略（宝宝优先）→ P5 自由文本与内容安全 → P6 广场等可选项。

**之后各自独立决策**：配表整批原子热更；`buff_type` switch 换成效果原子字典并合并四份 buff 枚举副本；帮会活动迁入模板；排行型活动；实时场景玩法框架。

---

## 11. 待拍板

1. 是否接受 Boss 全部技能在开局即对客户端可见（§4.7）。
2. 活动进度存储：独立 `PlayerActivityComp` + 新 RPC（推荐），还是复用 Mission scope 2 以免改客户端协议（§5.3）。
3. 是否同意第 1 步建跨语言战斗金样（需用户同意）。
4. 指纹切 enforce 的时间表。
5. 玩家自定义模板的开放范围与第一期（§8.1、§8.10），以及 §8.12 列出的 17 项（尤其第 1 条 H10 例外、第 2 条 CUSTOM 局是否无损、第 3 条评分局是否允许玩家策略）。

## 12. 未核实项

- LuaJ 的 GitHub star 数与指令计数 hook 的实际行为（不选 B，未深究）。
- ~~go/db 写盘时是否会清掉调用方没带的列~~ → 已核实 [C]：会清。`savePlayerRow` 用 `tx.Save` 整行写（§5.3 勘误）。
- ~~`domainhost` 默认 `RequireTiDB = true`，本地 MySQL 开发环境能否直接用~~ → 评审核实：dev / test 模式可设 `RequireTiDB = false`（§8.5.2）。
- 回档的帮会闸只检查 guild；活动奖励若走 mail 或 rank 跨存储发放，回档时是否会重复，未核实。
- 每回合 CPU 开销与 battle 节点容量没有基准，需 Codex 跑满配基准后补 §4.5。
