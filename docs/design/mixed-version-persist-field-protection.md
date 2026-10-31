# 新旧版本混跑时的玩家持久字段保护

> **部分过期(2026-10-30)**:严格提交屏障的 Lua(C3)随 Redis 存盘删除,世代校验搬到 go/db 的提交事务里([player-save-direct-commit.md](./player-save-direct-commit.md) §2)。本文关于未知字段携带、覆盖世代的部分仍然有效,`warn` 模式和 `PersistSchemaRegressionMode` 已删除,现在总是拒写。

> **状态**：设计稿 v0.9（2026-10-30，Claude）。经两轮只读对抗评审：v0.1 被推翻三处核心做法，v0.2 又发现一个必现的误报（S-A）以及若干缺口，v0.3 已按两轮结论修订。负责人 2026-10-19 通过 §13 第 1–5 条。v0.4 回写第 1 批实施中的调整，v0.5 回写第 3a 批的调整，v0.6 把 C3 的"只比第一次写入"改为写方令牌并随第 3b / 4 批落码（§5.2、§15），v0.7 回写第 3a′ / 第 5 批落码与两轮对抗复核的调整（欠款版本错位改为 `kVersionSkew`、按数量扣除标"暂无守卫"、越界包类型由 `PlayerBagsComp` 携带、§12 新增未核对值域与等级上限两行；§4.4、§12、§15），v0.8 回写第 6 批落码（退役字段残留不算版本错位、按数量扣除守卫、新币种的唯一取值表与版本错位；§0、R5、§4.4、§9、§12、§13、§15），v0.9 回写 §12.1 持久值域 D1–D6 与等级上限的落码（§0、§8.3、§12、§12.1、§12.2、§15）。各批实施状态见 §9 表前的「实施状态」。
> **起因**：2026-10-18 核对灰度 / 不停服 / 金丝雀时发现，新旧版本混跑会丢玩家持久字段。负责人 2026-10-19 要求先补三件事：①存盘保留不认识的字段；②Redis 父快照记录写方版本并校验；③go/db 只写改动列。先出设计。
> **范围**：玩家数据链，即 C++ scene 内存 → Redis 父快照 `PlayerAllData:{id}` → Kafka `db_task_zone_*` → go/db → MySQL 两张玩家表；外加 login 冷加载、回档快照、离线工具这些旁路。guild、mail、trade 各自的库表，以及客户端契约，不在本文范围内（§12）。
> **证据口径**：[C] 表示对照 2026-10-19 工作区代码读过；[I] 表示推断。相关文件正被其他会话修改，行号会漂移，所以正文以函数名为主。

---

## 0. 结论速览

1. **根因**：链路上有几个环节会**按本版本认识的字段重新拼一份数据**，再整份写回。新版加的字段因此在旧版手里被抹掉（§1.3 的 L1–L7），而且全程不报错（L8）。
2. **一条不变量统一三件事**：每张玩家表消息带一个**覆盖世代** `persist_schema_gen`，意思是"这份数据里，这个世代及以前定义的字段，值都是真实的"。数据每经过一道读写，只有两种合法结局：
   - **完整保留**，输出世代 ≥ 输入；
   - **拒绝或延后**，不产出任何副本。
3. **三个机制**，与负责人提的三件事一一对应：
   - **机制一 · 未知字段跟着对象走**：
     - 重建层改为把未知字段保存在运行时对象里；做不到的消息类型一律冻结，冻结类型不许再加字段。
     - 一个通用的**往返一致性测试**机械地守住这条规则。
     - 有一类语义耦合，"跟着对象走"也挡不住：合堆、等价判定、逻辑重置、跨实体引用。其中合堆、欠款合并、按 config 计数与扣数量三类，旧版代码不把"带着更新版本字段的对象"当普通对象处理：不合堆、不合并、不计不扣（后者按版本错位处理，第 6 批）；纪元重置照常执行，但先整体 `Clear()` 再重设身份字段，未知字段不会跨纪元存活（§4.2 L7、§4.4）。"更新版本的字段"只认台账里没有退役的字段号，退役字段的残留不算（第 6 批）。需要跨纪元保留的流账本字段、跨实体引用字段仍没有守卫，按 R5 走破坏性变更流程。
     - 新枚举值（币种）：取值表的唯一来源是 `proto/common/base/currency_type.proto`，Go 各入口按它拒绝写错的值；scene 遇到本二进制不认识的币种按版本错位处理（收件箱停用本域、资产通道回 RETRY），不记终态（第 6 批，§12）。
   - **机制二 · 覆盖世代校验**：
     - Redis 严格提交屏障**只在上一次屏障写入不是本实体写的时候**（hash 里的写方令牌与本实体不同）比较"加载时世代 ≥ 已记录世代"，同版本重放跳过比较；hash 只升不降（§5.2，v0.6）。
     - login 加载时做同样的门禁。
     - 违反就判定"链路上有人截断过数据"，fail-closed。
   - **机制三 · go/db 不写写方不认识的列**：
     - body 世代超过本实例世代：延后；
     - body 世代低于 SQL 行世代：拒写，进死信；
     - 未盖戳的 body（引导期关闭后）：拒写，进死信；
     - 其余：整行写。

     不做"只写改动列"，也不做"按列掩码写"，理由见 §6.1。**这一条需要负责人确认。**
4. **世代号由生成器发**：台账是 `proto/persist_schema_gen.txt`（仿 `proto/message_id.txt`）。不能安全携带的改动，在生成期就失败（§3.2）。
5. **上线与回滚**：
   - 先上线机制本身，引导期内禁止升世代；
   - 此后每次升世代，按"DDL → go/db 全部 zone → login → scene（可以金丝雀）"的顺序发；
   - go/db 回滚到更低世代，只会卡住，不丢数据；
   - scene 回滚有前提：回滚目标版本能原样携带新版本扩展的持久值域。§12 的删数据点已在第 5 批与 2026-10-23 一批落码（§12.2）；发布代码复跑通过之前、或回滚目标不含这些批次时，这个前提仍须在发布单里逐项写明（§8.3）。
6. **Java 版**：存储是单个 BLOB，机制三不适用，机制一、二同样需要，本批只登记（§11）。

---

## 1. 问题与现状

### 1.1 数据流

```text
C++ scene（ECS 组件，内存是权威）
  │  MarshalPlayerForSave：从组件重拼 PlayerAllData
  ▼
Redis 父快照  PlayerAllData:{id}（PlayerAllData 原始字节）
  │  严格提交屏障 Lua：写父快照 + save_barrier.pending + 完整投递意图
  ▼
Kafka db_task_zone_*  DBTask{body = player_database / player_database_1 的字节}
  ▼
go/db：Unmarshal(body) → 行内围栏事务（FOR UPDATE 读当前行）→ proto2mysql tx.Save
  ▼
MySQL player_database / player_database_1（每个顶层字段一列；子消息存成二进制 PB 的 BLOB）

冷加载（父快照缺失时）：
login → go/db 读任务 → SELECT 本版本认识的列 → 读回执 → login 组装父快照，
两表 SQL ACK 与行围栏都等于 pending 才发布
```

### 1.2 已经能保留未知字段的环节 [C]

| 环节 | 为什么能保留 |
|---|---|
| Go 侧中间跳：login 冷加载组装、`backfillPlayerIdentity`、go/db 写前 Unmarshal | Go protobuf 默认保留未知字段 |
| proto2mysql 的子消息、map、list 列 | `pbconv.SerializeFieldValue` 用 `proto.Marshal` 编码 |
| C++ 中以 PB 直接存放的组件 | 加载时 `emplace<Comp>(player, message.xxx())` 拷贝整份，存盘时 `CopyFrom` 带回。加载期的修正都是就地改字段：`ApplyClassInitialAttributesOrRevive`、`ClampToMaxLevel`、`PlayerAttributeSystem::InitializeOnLoad`、`PetSystem::InitializeOnLoad`、各账本的 `RemoveLast` / `DeleteSubrange`（评审核实）。运行期也没有高频整体替换持久组件；唯一的整块替换是瞬移，会替换 Vector3 / Rotation |
| 物品实例 | `ItemEntry.instance = 15` 承载整份 `ItemComp`（`bag_marshal.cpp`） |

### 1.3 会丢字段的环节 [C]

| 编号 | 位置 | 丢什么 |
|---|---|---|
| L1 | `SavePlayerToRedisImpl` 新建空消息交给 `MarshalPlayerForSave`；退出收敛比较、`discardedProbe` 也一样（`player_lifecycle.cpp`） | 两张表消息的**顶层未知字段**，也就是新增的整列 |
| L2 | `PlayerCurrencyComp::SaveToProto` 先 `clear_debts()`，再逐字段重建（`player_currency_comp.h`） | `CurrencyDebtEntry` 的新字段（资产路径） |
| L3 | `mission_marshal::Marshal` 先 `Clear()` 再重建；`Unmarshal` 只拷两个 map | QuestAllData / QuestScopeData / MissionListComp 的顶层新字段 |
| L4 | `bag_marshal::Marshal` 先 `Clear()`，再重建布局层；`ItemEntry.equip` 由 `ItemComp.equip` 重新算出，是投影 | 布局层的新字段 |
| L5 | `SanitizeSkillList` 新建组件后整体替换（`player_skill.cpp`） | PlayerSkillListComp 的顶层新字段 |
| L6 | `SnapshotSystem::CaptureAndSend` 直接拼一份新消息（`snapshot_system.cpp`） | 同 L1，丢在回档快照里 |
| L7 | go/db 写（`tx.Save` 整行 UPDATE）和读（只 SELECT 已知列）都只覆盖本版本认识的列 | 新版 go/db 把旧写方 body 里缺的新列写成零值；旧版 go/db 冷加载读出截断的副本 |
| L8 | 全链路 | 没有写方版本，也没有覆盖信息，丢了零报错 |

还有一种情况：`stress_test_probe`、`merge_state` 这些已知字段 scene 不 marshal，每次存盘都会被清空（评审核实）。这不属于未知字段问题，处理方式见 §3.2 的冻结清单和 §12。

### 1.4 后果

- **不能金丝雀**：金丝雀本身就是混版本运行。
- **不能回滚**：回滚 scene 后，所有在线玩家的新字段都会被抹掉。
- **丢了不知道**：没有任何报错或指标。

---

## 2. 目标与不变量

- **P1：覆盖世代单调传播。** 每张玩家表消息都带覆盖世代 `c`。每一跳读写只能二选一：
  - 完整保留全部字段，输出 `c' = max(本跳世代, c)`；
  - 拒绝或延后。

  **推论**：只要每一跳都守住 P1，世代大于 `c` 才引入的字段在数据里缺失，就一定是从来没写过，按默认值处理是对的。
- **P2：** 旧版 scene 接手新数据时完整携带新字段；对携带着未知字段的对象，不做可能改变其语义的操作（§4.4）。
- **P3：** go/db 不写写方不认识的列；保存不完整的数据，就延后或拒写。
- **P4：** 违反 P1 的情况，最终一律 fail-closed，配指标和告警，player_id 不进 label。C3 / C4 在引导期可以先用 warn 模式观测（§5.2），残余风险列在 §13。

**非目标**：
- 持久值域扩展（§12）；
- Redis Cluster 跨槽；
- 新旧字段的语义耦合，超出 §4.4 守卫范围的部分。这部分靠 R5 和评审清单约束。

---

## 3. 核心概念：持久 schema 世代

### 3.1 世代号与台账

- **世代号**：`uint32`，从 1 开始单调递增。覆盖从两张玩家表消息（`OptionIsPlayerDatabase`）可达的全部字段。`PlayerAllData` 顶层不经过 SQL，所以**不作为根**，并且冻结。
- **台账** `proto/persist_schema_gen.txt`：由生成器维护，提交进仓库。每个持久字段一行，身份是 `消息全名#字段号`；字段名只为可读，改名不算改动。格式（第 1 批实现）：
  ```text
  state=bootstrap
  1=player_database#21 optional uint32 persist_schema_gen
  1=CurrencyComp#2 repeated CurrencyDebtEntry debts
  2=CurrencyDebtEntry#9 optional uint64 interest
  1=ItemComp#3 map map<uint32,Attr> attrs
  retired=SomeComp#4
  ```
  台账缺失时生成器拒绝运行，不会自动重建基线（重建会把发号历史抹成世代 1）。初始文件只有表头和 `state=bootstrap`，第一次生成时写入世代 1 的基线。
- **生成器的行为**：
  - 只有新增 → 追加世代 `N+1`；
  - 删除且写了 `reserved N;` → 把原行标为 retired；
  - 改号、改类型、复用号 → 生成失败（AGENTS §4.3）。
- **退役字段**：残留字节会作为未知字段一直被携带，因为字段号永不复用，这是无害的。机制三按世代号判断，不看有没有未知字段，所以不会被这些残留卡住。
- **生成产物**（不手改）：
  - C++ 头文件 `cpp/generated/persist_schema/persist_schema_gen.h`：`persist_schema::kPersistSchemaGen`，以及往返一致性测试用的 `kFrozenMessageTypes`、`kUncarriedFields`；只有头文件，不需要登记工程；
  - Go 包 `go/shared/generated/persistschema` 的常量 `Gen`。
- **持久根与可携带集合**：根是两张玩家表，以及显式冻结的消息（让 `PlayerAllData` 这类不经 SQL 的顶层容器也受 R1 约束）。"可携带集合"只从玩家表、以及玩家表到达不了的冻结根出发，不穿过未携带字段；往返一致性测试要覆盖的，就是其中的非冻结类型。
- **只在主干发号**：发布分支和 hotfix 分支不得自己追加世代。需要新持久字段时，先合主干拿号，再把台账行原样 cherry-pick 过去。

### 3.2 可携带性规则

| 规则 | 不可携带的改动 | 怎么做 |
|---|---|---|
| R1 | 给**冻结类型**加字段（生成期强制） | 先给这个类型实现运行时携带，并纳入往返测试，再解冻；或者把数据放进可携带的类型，例如物品数据放进 `ItemComp`（AGENTS §11.6） |
| R2 | 给已有的持久 oneof 加新成员（生成期强制） | 字段放到 oneof 之外，或者走破坏性变更流程（整批停机换版，或 expand / contract） |
| R3 | 改号、改类型、复用号（生成期强制） | 用 `reserved` |
| R4 | 字段迁移：子消息字段提升为列、拆分、换存储位置。生成器只能检测"同一世代内既删又增"并给出提示 | expand / contract：新旧位置双写一个完整版本周期，然后再切换读方 |
| R5 | **语义耦合字段**：会参与等价判定、合堆、合并、逻辑重置的字段，或者按 ID 引用其他实体、而这类实体旧代码能删除后重建的字段。生成器无法检测，列入评审清单 | 依赖 §4.4 的守卫（合堆、欠款合并：旧代码拒绝执行；按 config 计数与扣数量：旧代码不计、不扣带更新版本字段的单位，按版本错位处理，第 6 批；纪元重置：旧代码整体清空未知字段）；守卫覆盖不到的场景，走破坏性变更流程，包括新版要求跨纪元保留的流账本字段（旧版重置时会清掉）。§4.4 仍未守的：`has_equip` 实例按 config 运算（调用方在表上排除装备兜住）；`EquipInstanceData` 内部的新字段（谓词只看 `ItemComp` 本层）；按 guid 托管或改写实例时才生效的新字段（例如不可交易、封印）；`AttributeScheme`、`PetInstance`、`PlayerSkillComp` 本层的新字段（§12.2）。**会被普遍写入的 `ItemComp` 新字段（例如获得时间）分两版发布**：版本 N 只在 proto 里声明（读、忽略、原样存盘），版本 N 全量之后版本 N+1 才开始写，回滚下限是 N；否则跨版本窗口里几乎所有新堆都会变成"不可按数量扣除"（守卫分不出有语义和无语义的字段）。`ItemComp` 新字段不得改变 `size` 的计数含义，也不得让单个实例的可扣量超过 `size`（这是"确定不足可以记终态"的前提）。跨实体引用只能用永不复用的实例 ID |

**两个 option**，都定义在 `proto/db/proto_option.proto`。生成器从描述符的原始字节按字段号动态读取，不需要升级外部模块 `protooption`：

- message 级 `OptionPersistFrozen = 500040`：冻结类型。生成器拒绝给它加字段（R1）；往返测试不在这类节点上注入，但会继续向下遍历它的子字段。
- field 级 `OptionPersistUncarried = 600040`：存盘时不原样携带的字段，包括由别的载体重算的投影，以及 scene 每次存盘重新生成或清空的字段。往返测试不经由它注入。**只经这类字段可达**的消息类型不进入往返测试，也不受 R1 约束，因为这些数据本来就不靠原样携带。v0.3 原名 `OptionPersistProjection`，第 1 批实施时为覆盖压测探针、合服标记这类"每次重新生成"的字段而放宽了含义。
- 编号避开 proto2mysql pbopt 已用的 500000–500024 与 600100–600101。

**初始冻结 / 未携带清单（第 1 批已标注）** [C]：

| 类型或字段 | 标注 | 原因 |
|---|---|---|
| `PlayerAllData` | 冻结 | 不经过 SQL，冷加载由骨架组装 |
| `PlayerAllData.bag_data` / `quest_data` / `mail_data` | 未携带 | 前两个是由数据库记录重算的兼容副本，`mail_data` 没有写入方；`MailAllData` / `MailEntry` 因此只经未携带字段可达，不再单独冻结 |
| `BagAllData`、`BagAllData.DynamicBagData`、`ItemEntry` | 冻结 | 布局层由 `bag_marshal` 重建。`ItemEntry.instance`（ItemComp）是载体，**不冻结** |
| `ItemEntry.equip` | 未携带（投影） | 由 `ItemComp.equip` 重新算出；`EquipInstanceData` 经 `ItemComp.equip` 可达，保持可携带 |
| `QuestAllData`、`QuestScopeData` | 冻结 | 由 `mission_marshal` 重建。`QuestScopeData.mission_list`（MissionListComp）是载体，不冻结 |
| `QuestAllData.active` | 未携带 | 只读旧格式，加载时转换、存盘不写出；`QuestEntry` 因此只经未携带字段可达，不再单独冻结 |
| `player_database.stress_test_probe`、`player_database.merge_state`、`player_database_1.stress_test_probe` | 未携带 | 探针每次写盘重新生成；合服标记 scene 不处理、整行写会清空（是否有意见 §12）。改为标注引用字段，而不是冻结 `PlayerStressTestProbe` / `PlayerMergeStateComp` 类型：这两个类型定义在组件 proto 里 |
| map 的 entry 类型 | 不纳入注入，也不纳入覆盖率统计 | 由生成器自动识别（`map_entry` 选项） |

**不标注的类型**：`Vector3`、`Rotation`，以及上表中 `PlayerStressTestProbe` 等定义在 `proto/common/component/` 下的类型。组件 proto 可能被客户端使用，给它们加 option 就要导入服务端的 option 文件，会影响客户端仓（AGENTS §9、§12）。`Vector3` / `Rotation` 只在瞬移时被整块替换，这属于"重置"语义（§1.2）；它们的字段变更走客户端契约流程。

### 3.3 世代戳

- **位置**：两张表消息各加 `uint32 persist_schema_gen`：`player_database` 用 21 号，`player_database_1` 用 5 号。
- **取值约定**：0 表示未盖戳，包括机制上线前写入的数据和 login 的新号种子。比较时 0 一律按 1 处理。所有写入方写入的值都必须 ≥ 1。
- **为什么放在表消息里**：戳和内容在同一份字节里，自动跟着父快照、DBTask body、MySQL 列、子表缓存、跨 zone 迁移、回档快照走；Go 中间跳不认识这个字段也会原样保留；它本身就是一列，go/db 读行时自动带上。
- **加载世代** = 两表戳的**最小值**（0 按 1 处理）。取最小值，是为了不让某一张表被截断的事实被另一张表的高戳洗白。
- **输出世代** = `max(kPersistSchemaGen, 加载世代)`，两张表盖同一个值。
- **save_barrier hash 里的 `schema_gen`**（§5.2）只是索引，不是权威。hash 不存在、或者里面没有这个字段时，**一律不比较**。会出现这种情况的有：机制前的存量玩家、合服回退（unmerge）或 PITR 清理删过 hash、本地 debug_import。前向 copy 合服 2026-10-20 起保留 hash（r2-mz#1），C3 / C4 照常比较；恢复工具重建 hash 时写入 `schema_gen`（§5.4、§13 第 8 条）。

---

## 4. 机制一：未知字段跟着对象走

### 4.1 为什么不从加载原件嫁接

v0.1 的做法是在实体上保存加载原件，存盘时按身份键把原件里的未知字段嫁接回去。评审 S1 证明这会**复活已删除元素的新字段**。

例子：欠款还清后删除，之后 `AttachDebt` 用同一个 `currency_type` 新建欠款，嫁接就把旧欠款的新字段挂到了新欠款上。任务放弃后重新接取，情况相同。

根因是嫁接把未知字段的生命周期，和它所属对象的生命周期拆开了。v0.3 遵循 protobuf 本身的语义：**未知字段只存在于它所属的对象里**，对象被删除，未知字段也随之消失。

### 4.2 改造清单

| 编号 | 改造 |
|---|---|
| L1 | 新增两个持久数据类组件：`PlayerDatabaseCarryComp`、`PlayerDatabase1CarryComp`。各保存对应表消息的**顶层**未知字段，以及该表加载时的戳。<br>**创建点**：分别在手写的 `PlayerDatabaseMessageFieldsUnmarshal` / `PlayerDatabase1MessageFieldsUnmarshal` 里创建，不放在 `InitPlayerFromAllData`。这样经过生成的 `PlayerAllDataMessageFieldsUnmarshal` 加载的测试实体也带有这两个组件（评审 M-E）。<br>存盘入口缺少它们，就整份拒存（持久数据类规则）。<br>顶层未知字段是旧代码完全没有逻辑对应的整列，旧代码不会删除或重置它们，所以这里不会出现 S1 那种复活。按 ID 引用其他实体的新列另当别论，归 R5 / §4.4 管 |
| L2 | 运行时结构 `CurrencyDebt` 增加 `std::string unknownFields`（`UnknownFieldSet` 的序列化）。`LoadFromProto` 时保存，`SaveToProto` 时写回。欠款删除后重建得到的是新对象，未知字段为空 |
| L3 | `MissionListComp` 改为整份保存，`Unmarshal` 不再只拷两个 map；QuestAllData / QuestScopeData 冻结 |
| L4 | 布局层冻结；`ItemEntry.equip` 标为投影 |
| L5 | `SanitizeSkillList` 替换组件时，保留原组件的顶层未知字段（删技能问题另见 §12） |
| L6 | 回档快照改走统一入口 |
| L7（新增） | 逻辑重置统一改为"`Clear()` 后重设身份字段"。现有的 `ResetAssetOpStreamForEpoch`（`asset_op_ledger.cpp`）是逐字段清空，未知字段会跨纪元存活下来，把旧纪元的状态带进新纪元（评审 M-F，资产路径） |

**统一入口**：新增手写模块 `services/scene/player/system/player_persist_marshal.{h,cpp}`。

```cpp
// 存盘、退出收敛比较、跨 zone 迁移、回档快照的唯一拼装入口:比的、写的、快照的是同一个形状。
// 步骤:PlayerAllDataMessageFieldsMarshal → 填两表 player_id → 两表并回顶层未知字段(两个 Carry 组件)
//      → 两表盖戳 max(kPersistSchemaGen, 加载世代)。
// 返回 false = 持久组件缺失(含两个 Carry 组件);调用方必须拒绝这次写入。
[[nodiscard]] bool MarshalPlayerForPersist(entt::entity player, Guid playerId, PlayerAllData& out);
```

- `MarshalPlayerForSave` 改为转调它；退出收敛和两处 `discardedProbe` 因此一并覆盖。
- `SnapshotSystem::CaptureAndSend` 改为调它。
- `player_data_loader.h` 是生成产物，**不改**。
- 盖戳在快路径比较之前完成；`last_save_seq` 照旧在快路径之后写。

### 4.3 往返一致性测试

**宿主**：放在 bag_test 工程里。它已经调用 `PlayerAllDataMessageFieldsUnmarshal`，SetUp 里也已经加载了 Mission、Condition、Class、AttributePool、AttributeDimension、AttributeRule 等配置表。还要补加载 SkillTable 和宠物相关表，让夹具用真实表 ID 走"本版本认识这些 ID"的生效路径（评审核实）。第 5 批前，不加载技能表时 `SanitizeSkillList` 会把技能节点全部删掉；第 5 批起查不到表的技能保留但不生效，属于另一条路径，不在往返测试里混测。

**夹具**：现有夹具只覆盖背包和任务，需要新建一份**覆盖全部持久组件**的夹具，并使用真实的表 ID。

**步骤**：

1. 用反射遍历夹具里所有已设置的消息节点。在每一个**非冻结、非 map entry、不经由投影字段到达**的节点上，注入一个未知 varint 字段：字段号不超过 2^29−1，并避开 19000–19999；值是该节点的唯一标签。冻结节点本身不注入，但继续向下遍历。比如 `ItemEntry` 冻结，但它的 `instance`（ItemComp）照常注入。
2. 依次执行 `PlayerAllDataMessageFieldsUnmarshal` → `MarshalPlayerForPersist`。
3. 断言：
   - 全部标签都出现在输出里，并且所在节点的类型与注入时相同（按标签的多重集比对，不依赖元素顺序）；
   - 两张表的戳等于期望值。
4. **覆盖率断言**：从两张表可达的每一个非冻结类型，至少要有一个节点被注入。缺失时测试失败，并给出缺失的类型名。例如 `EquipInstanceData` 只能经由 `ItemComp.equip` 满足。

**测试改动面**（评审 M-E）：手工搭建实体的测试都要补齐两个 Carry 组件，或者改用统一入口，否则会进入拒存分支。已知涉及：`cross_zone_test`（`MarshalAsSaved` 不盖戳，和盖了戳的 current 比较必然不等）、`save_seq_test`、`db_task_dispatch_test`、`player_feature_persistence_test`。

### 4.4 语义耦合的守卫（评审 M-F）

对象内携带只保证"字节不丢"，不保证"旧代码对这个对象的操作，在新语义下依然正确"。仓库里有四类操作已经注定会出问题，现在四类都在当前版本里加了守卫（按 config 计数和扣数量为第 6 批）。这样等以后它们成为"旧版本"时，守卫才会生效。

判"带着更新版本的字段"只有一个谓词：`unknown_field_carry::CarriesNewerVersionFields`。对象本层的未知字段里，只要有一个字段号不在台账的退役清单里就算（台账 `retired=<消息全名>#<字段号>` 行，生成为 `persist_schema::kRetiredFields` / `IsRetiredField`）。退役字段的残留不算：只看"有没有未知字段"的话，带残留的对象会在所有版本上被永久当成新版本数据，永远不能合堆、合并、按数量扣除（第 6 批）。守卫一律用它，不直接看 `UnknownFieldSet` 是否为空：

| 操作 | 现状 [C] | 守卫 |
|---|---|---|
| 合堆、等价判定 | `ItemStore::CanStack` 只判断 `has_equip`；`ApplyStackFill` / `MergePartialStacks` 按配置合堆。`ItemEntry` 的 TODO 里已规划 bound_state、expire_at | 如果任一方 `ItemComp` 带有更新版本的字段，`CanStack` 返回 false。旧版不会把带新属性（比如绑定、限时）的物品并进普通堆。判定只有一份（`ItemStore::IsIndependentInstance`，公开）：包内既有堆的空余估算与整理、传入物品一侧 `Bag::AddItems` 的规划分流、宝石拆卸的静默预检（直接调 `CanStack`）都用它，预检、规划与提交不分叉（后两处为第 3a′ 批） |
| 按 config 计数和扣数量 | `ItemStore::TotalCountOf` / `CheckQuantities` / `DrainStacks`。调用方：资产通道数量扣出（快照只记 config 与数量，对端按 `ItemCountMap` 新建普通实例）、强化扣材料、镶嵌扣宝石（装备上只记 config，拆下时生成普通宝石）、战斗快照与结算消耗（`RemoveItemsClamped`；收件箱结算经 `ConsumeInventoryAndGrant`）（2026-10-19 对抗复核 B-1） | **第 6 批已守**。谓词 `ItemStore::IsCountableByConfig`（= 不带更新版本的字段）与 `IsIndependentInstance` 分工：前者管能不能按 config 计数、扣除，后者管能不能合堆；`has_equip` 实例照算，闸在调用方（四个调用方都在表上排除了装备）。计数（`TotalCountOf` / `Bag::GetTotalItemCount`，名字未改，语义是"可按 config 扣的数量"）、预检、扣除（`DrainStacks`）同口径跳过不可计的单位。<br>预检是三态 `ItemStore::CheckQuantities`：任一项算上带新字段的单位仍不够 → 确定不足（优先，可以记终态）；否则任一项只差在带新字段的单位上 → 版本错位（非终态）；其余 → 够。`Bag::CheckItemsAvailable` 映射成 kSuccess / `kFeatureUnavailable`(1006) / kBagInsufficientItems；原 bool 的 `HasAll` 会把错位并进"不足"，已删除。<br>各调用方：资产通道数量扣出版本错位回 RETRY + 1006、不记账，确定不足照旧记终态 REJECTED；错位排在按 guid、宝宝这些与版本无关的确定性校验之后，那些坏包仍先记终态。强化、镶嵌回 1006，排在金币判定之前，判完才扣金币，所以不会出现 ENHANCE_PARTIAL。战斗快照只计可计单位；收件箱结算遇到带新字段的消耗物品回 `kVersionSkew`（战斗结算会话落码）；直接结算路径扣不满按夹紧处理，打防刷 WARN（跨版本战斗窗口里可能误报）。<br>代价（fail-closed，写进发布单）：带新属性的可叠加物品在旧版里不能按 config 消耗、交易、带进战斗；聚宝斋托管遇到它会一直 RETRY，卖家可以撤单（托管阶段可撤，2026-10-30） |
| 合并 | `AttachDebt` 会把新欠款并进已有欠款 | 已有欠款带更新版本的字段时，`AttachDebt` 按 fail-closed 返回失败，不做合并。这是版本错位，不是确定性拒绝：不得记成终态拒绝，也不能按本地副本原地重试（这笔欠款可能在会话内被还清删除，而那条处置可能已被作废）。调用方用只读谓词 `CurrencySystem::DebtCarriesUnknownFields` 先判，不拿通用返回码反推；收件箱执行器命中回 `kVersionSkew`，收件箱停用本域，实体重建后重拉（退出完成后再登录，或换节点；退出保留期内重登复用实体，不重拉）（第 3a′ 批，对抗复核 A-1 后由 `kDefer` 改来，`player-inbox-service.md` §7.6 第 3 步）。`AdjustDebt` / `WaiveDebt` / `FreezeDebt` 目前没有生产调用方（GM RPC 仍是桩），实现时要补同样的守卫 |
| 逻辑重置 | `ResetAssetOpStreamForEpoch` 逐字段清空 | 改为 `Clear()` 后重设（§4.2 L7）。这不是"拒绝执行"：重置照常进行，未知字段随旧纪元一起清掉；新版要求跨纪元保留的字段，旧版会在重置时清掉，归 R5 |

跨实体引用（比如新列按 pet_id 引用宠物，而旧代码能按同一个 pet_id 恢复宠物）没有通用守卫。归入 R5：只能引用永不复用的实例 ID，否则按不可携带处理。

技能条目（`PlayerSkillComp`）、加点方案（`AttributeScheme`）、宝宝实例（`PetInstance`）本层的新字段**暂无守卫**，按 R5 走破坏性变更（§12.2 约定由本节登记）。这些对象上的操作都是玩家主动的修改（修习、加点、洗点、出战），新字段会怎样约束它们只有新版本知道；一刀切地拒绝修改带新字段的对象，会在混版本窗口里把这些玩法整片停掉，所以不加。将来某个新字段确需旧版本拒绝修改时，按需在该操作入口用 `CarriesNewerVersionFields` 判，它已能分开新版本字段与退役字段的残留。

### 4.5 成本

平时（全网同一版本）没有任何未知字段，Carry 组件里只是两个空集合，欠款的 `unknownFields` 为空串，不做反射遍历。混版本期间的开销，与未知字段的字节数成正比。

---

## 5. 机制二：覆盖世代校验

### 5.1 校验点

| 点 | 位置 | 条件 | 动作 | 观测 |
|---|---|---|---|---|
| C3 | C++ 严格提交屏障 Lua | 上一次屏障写入不是本实体写的（hash 的 `schema_writer` ≠ 本实体令牌，§5.2）、且不是同版本重放时，加载世代 < hash 里的 `schema_gen`，或者存储值已损坏 | warn：照写，返回 2；enforce：拒写，返回 3 | C++ 日志行 `metric=persist_schema_regression where=redis`，LogQL 固定查询（C++ 节点没有 Prometheus 出口；ruler 未接线，告警未配） |
| C4 | login `beginParentLoad` 快路径 | 父快照的加载世代 < hash 里的 `schema_gen` | warn：照常 EnterGame，计数；enforce：`beginParentLoad` 返回错误。走现有失败路径，经 `notifyEnterGameFailed` 推送固定的 `kEnterSceneFailed`，**不新增 tip**。按现行契约，失败原因不外发给客户端（评审核实） | `login_persist_schema_regression_total` |
| C0 / C1 / C1b / C2 | go/db | 见 §6.2 | 延后 / 死信 / 读失败 | `db_task_deferred_total{reason}`、死信原因 |

### 5.2 Redis 严格提交屏障

> **v0.6 修订（2026-10-19，第 3b 批落码）**：v0.3–v0.5 的"实体只在第一次存盘里传真实加载世代、之后 `ARGV[9] = ARGV[8]`"被交接核对推翻（首存在途时排队的后继必误报、首存被顶替时漏比较、"回包丢失后重发的一定是同一元素"不成立）；改为本节的**写方令牌**。取舍与被否决的方案见 §15 v0.6。

**C3 只在"上一次屏障写入不是本实体写的"时比较**（修复评审 S-A）。

v0.2 要求每次存盘都满足"加载世代 ≥ hash"，而写入成功后 hash 会被抬到输出世代。于是新版 scene 接手旧戳数据后，从第二次存盘起每次都会被判为倒退；enforce 模式下就是全服丢进度。要比较的只有"接手别人写的数据"那一刻，那也正是截断可能被引入的地方。问题是怎么认出"上一次是我写的"：

- **不能用 C++ 侧的"第一次存盘"标记**：屏障的参数在入队时定死，首存 A 在途时排进的 B 带着入队时的值；A 未证实提交就被顶替（排队值顶替、ERROR / 空应答后的 `QueueSaveForRetry`、`OnReconnected`、在途时新值覆盖排队值、owner 拒绝后 guard 不同的排队值直接发）时，被丢弃的元素不执行任何回调，C++ 分不出 A 是"已提交但回包丢了"还是"没提交"。
- **不能用 owner 代次（pending 的 owner 前缀）**：同一个实体会换代次（退出保留期间玩家重登铸出 E+1 又派回本节点复用实体、B5 回滚采纳 E+2），同一个代次也会先后加载出两个实体（同落点重连时上一个实体已退出销毁、废黜后回流、节点崩溃后同号新进程接替）。前者会确定误报（kGen > L 时 enforce 下每次返回 3），后者会漏检。

**规则**：每个加载实体生成一个全局唯一的**写方令牌**（随机 UUID 文本，存在实体的运行时组件 `PlayerPersistWriterComp` 里，存盘写入口懒建，实体销毁即作废；不用 entt 实体号或"节点 + 玩家"这类能被再次加载复现的值）。严格屏障每次写入都把令牌记进 hash 的 `schema_writer`；脚本只在 `schema_writer ≠ ARGV[11]` 时比较。于是：

| 情形 | 令牌 | 结果 |
|---|---|---|
| 新实体（任何来源的加载）的第一次写入 | 与 hash 不同 | 比较真实加载世代 `ARGV[9]`，`ARGV[9]` 永远是真实加载世代，C++ 不维护任何"首存"状态 |
| 本实体的后续写入；首存在途时排队的后继；换了 owner 代次之后的写入 | 相同 | 跳过 |
| 首存已提交、回包丢失，随后被任意代次的排队值顶替 | 相同（A 已把令牌写进去） | 跳过 |
| 首存未提交就被顶替 | 不同（A 没写成） | 后继照样比较真实加载世代，与 A 本该做的比较一致 |
| enforce 拒绝（返回 3） | 不写令牌 | 下一次仍比较、仍拒 |
| 别的写方插在本实体两次写入之间 | 不同 | 照常比较本实体当初的加载世代（升世代窗口里会判倒退）。生产中不可达，见下表"前提"一行 |
| warn 判倒退那次的回包丢失，随后重放或改发同令牌后继 | 相同（跳过比较） | `schema_regressed` 把 2 带回来，计数不丢 |
| 机制前或 3b 前的旧二进制写过屏障 | 旧二进制不写令牌，残留的旧令牌只属于已被取代的实体 | 新实体令牌必然不同，照常比较 |

**同版本重放**（`pending == ARGV[3]`，且父快照与投递意图字节相同）显式跳过 C3、返回 1：现有脚本的同版本分支不提前返回，会落到写入。

**C3 在脚本里的位置**：放在参数校验、owner ≠ `ARGV[2]` 返回 0、pending 比 `ARGV[3]` 新报错、同版本冲突报错**之后**。已被废黜的旧持有者必须拿到 0（走 `DestroyDeposedPlayer`），不能先拿到 3。

| 项 | 规则 |
|---|---|
| 参数 | 由 7 个增加到 11 个：`ARGV[8]` 输出世代（两表戳），`ARGV[9]` 本实体的加载世代，`ARGV[10]` 模式（`warn` / `enforce`），`ARGV[11]` 写方令牌。旧二进制自带旧版脚本源码，只发 7 个参数，不受影响（`ScriptSlot` 按源码计算 SHA）。11 个参数随元素走，重试、NOSCRIPT 重发、`OnReconnected` 都发同一组 |
| 比较 | 新增函数 `num_gt`：先比长度，再按字节比较。现有的 `gt` 只适用于等长字符串 |
| 存储值异常 | hash 里的 `schema_gen` 不是规范十进制 uint32（空串、`0`、前导零、超界、非数字）时，视为倒退证据，按模式返回 2 或 3，**不返回 error_reply**。返回 error_reply 会让 `OnSaved` 无限重试，这名玩家就永远存不了盘（评审 M-B）。warn 下写入值取 `ARGV[8]` |
| 不比较 | hash 不存在，或没有 `schema_gen` 字段（机制前的存量玩家、unmerge 或 PITR 清理删过 hash、本地 debug_import；前向 copy 合服保留 hash，照常比较） |
| 参数异常 | 世代不是 1..4294967295、`ARGV[9] > ARGV[8]`、模式不是两个字面量、令牌为空 / 超过 64 字节 / 含字母数字与 `:` `-` 以外的字符：返回 error_reply。这是 C++ 的编程错误：`SaveWithCommitBarrier` 入队前用同一口径校验并返回 false，单测钉住 |
| 写入 | 只要写入成功（返回 1 或 2），在同一个 `HSET` 里写 `schema_gen = max(存储值, ARGV[8])` 与 `schema_writer = ARGV[11]`。**只升不降**，否则 warn 模式下低世代写方会把 hash 拉低，截断就此被洗白（评审 M-A）。返回 3 时一个键都不动 |
| warn 证据（3b 评审） | warn 判倒退时另写 `schema_regressed = ARGV[11]`。之后 `schema_regressed == ARGV[11]` 的写入（判定那次回包丢失后重放同一元素、或改发同令牌的后继——两者都跳过比较）照写并**继续返回 2**。否则判定那次的回包一丢，计数永远为 0，切 enforce 的门禁拿到假阴性。别的写方写成功也**不清**这个字段：它只在等于本令牌时起作用而令牌全局唯一，保留对 C3 没有副作用；清掉的话"判倒退后回包丢失、随即被废黜或销毁"的那次判定就永远不计数（所以切 enforce 门禁要加一项巡检它的存量，§8.1 第 5 步）。只有 `save_barrier_restore` 以 SQL 为准重建时清掉。C++ 按写方令牌去重计数（`PlayerPersistWriterComp::regressionWarnReported`），同一写方只记一次 |
| 前提：别的写方不会插在同一实体两次写入之间 | 判据是"上一次屏障写入是不是本实体写的"。若别的写方插在本实体两次写入之间，本实体下一次写入会拿**当初的**加载世代去比，升世代窗口里会误判（enforce 下永久拒存）。生产中这条路径不可达（2026-10-22 接手会话的三路攻击与裁决）：唯一的候选是跨节点交接回滚 B5 采纳前"幽灵目标节点"的一次存盘，但 B5 只认与本次标记原文逐字节相同的回执、凭标记回滚要求标记原样还在，而目标节点 A2′ 先核对 epoch 并删标记、之后才建实体，两者互斥；`cross-zone-scene-travel.md` 讲"幽灵目标可在回滚前建实体并存盘"的是不凭标记的铸造，那种回滚清空回执，源端不走 B5；其余情形都被 owner 守卫（返回 0）挡在 C3 之前。**例外**：dev 旁路 `AllowUnsafeCrossNodeHandoff` 让两个节点共用同一个代次，开着它的环境 C3 只能用 warn。没有采纳"本实体写入被证实提交后改用输出世代比较"的加固：它把首存状态与回包依赖带回 C++，而要防的路径不可达（裁决）。真实 Redis 用例 `RedisCommitBarrierForeignWriteBetweenSameEntityWritesIsComparedAgainstRealRedis` 钉住判据本身 |
| 返回值处理 | `OnSaved`（`redis_client.h`）对屏障元素：返回 1 或 2 都算提交证据，触发 `publishDbTasks` 与 `save_callback_`；返回 2 先同步调一次计数回调（`SetSaveSchemaWarnedCallback`）。返回 3 不重试、不触发提交钩子、不发布 `save_callback_`；同 key 排队值的 guard **与写方令牌**都与被拒的相同才一并丢弃，否则照发、不回调（令牌不同 = 同一代次下另一个加载实体的存盘，C3 只看写方自己的加载世代，被拒的那份不能替它下结论；与 0 分支共用 `IssuePendingWithNewerGuard`，0 分支只看 guard）；走**新增的回调** `SetSaveSchemaRejectedCallback`。2 / 3 只对屏障元素有含义，其余路径收到仍是坏回包、按失败重试。新回调不能复用 `HandlePlayerSaveRejected`：它会按 epoch 重存而无限循环，或者按"废黜"处理，语义都不对。`core/system/redis.cpp` 接到 `PlayerLifecycleSystem::HandlePlayerSaveSchemaRejected`：退出中走 `DestroyDeposedPlayer`；交接中且标记未发（`requestedAtMs == 0`）走 `AbortTravelHandoff`；其余情况保留实体、打日志、本次不落盘 |
| 模式配置 | `BaseDeployConfig.persist_schema_regression_mode`（部署键 `PersistSchemaRegressionMode`）。`readBaseDeployConfig` 读入时归一化：缺键或空串 = `warn`，`warn` / `enforce` 原样，其余拒绝启动（LOG_FATAL）；`ARGV[10]` 只会是两个字面量。仓库默认 `warn`。`k8s_deploy.ps1` 从 `bin/etc/base_deploy_config.yaml` 镜像进 node ConfigMap 并在生成期校验，契约测试钉住；`start_game.ps1` 启动前校验。login 用 `login.yaml` 的同名键（§5.1 C4），两边分别切换 |
| 观测 | C++ 节点没有 Prometheus 出口。逐次一条 `metric=persist_schema_regression where=redis mode=warn|enforce` 的 ERROR 日志（含玩家号、key），另由 `redis.cpp` 每 30 秒汇总一行 `[PersistSchema] regression_warned=N regression_rejected=M`（有计数才打）。Loki ruler 尚未接线（`deploy/k8s/owner-epoch-alerts.yaml` 同一现状），LogQL 写成值班固定查询（`deploy/k8s/README.md`「玩家持久 schema 世代」），**告警规则未配** |
| 恢复工具 | `save_barrier_restore` 的字段白名单接受 `schema_writer` 与 `schema_regressed`，写入时把两者 `HDEL` 掉（第二轮复核）：以 SQL 为准之后，任何写方的下一次严格屏障存盘都重新比较。合规流程里恢复时没有在线写方，结果不变；万一还有截断数据的写方在线，它不能凭旧令牌跳过 C3（fail-closed）。`schema_gen` 的取值口径见 §13 第 8 条 |

**enforce 模式的代价**：被判倒退的玩家，本次在线进度不会落盘。按 §5.3 的推导，合法流程不会触发；触发了就说明数据已经被截断。此时宁可不写，也不能让截断的数据去覆盖 SQL。

### 5.3 为什么合法流程不会触发 C3 / C4 [C/I]

- **会被比较的写入（令牌不同）的加载世代来自父快照**：
  - 父快照最近一次是 scene 写的：加载世代 = 那次的输出世代 = hash 值。
  - 最近一次是 login 冷加载写的：它要求两表 SQL ACK 和行围栏都等于 pending（`parent_commit_barrier.go`），所以 SQL 行里已经包含最近一次 scene 写入，行戳 ≥ hash。
  - 最近一次是 login backfill 写的：Go 原样保留，戳不变。
- **本实体之后的写入不比较**：hash 的令牌就是它自己的，基底是它自己的上一次写入（v0.6 起不再要求"后续写入的 `ARGV[9]` 等于输出世代"）。
- **新号种子**：hash 还不存在，不比较。
- **引导期**：0 按 1 处理，存量数据都是 1。

**唯一的合法例外是回档**：回档可能把数据恢复到更低的戳。目前 `RollbackFence` 还没有赋值、一律返回 NotImplemented；将来实现回档时，必须显式重置 `schema_gen`。

**引导期掩盖问题的教训**（评审 S-A）：全网世代都是 1 时，任何"只在升世代后才出现"的缺陷都不会暴露。所以 §8.1 规定：切到 enforce 之前，必须在 staging 上做一次**真实升世代**的演练。

### 5.4 各写入方

| 写入方 | 要求 |
|---|---|
| login 冷加载组装 | 不改逻辑。C4 只在快路径检查 |
| login `backfillPlayerIdentity` | 不改逻辑。加测试，验证未知字段和戳都保留 |
| login 新号种子 | **不盖戳**，保持 0。原因：`isUnownedLoginSeed` 用 `proto.Equal` 把父快照和骨架比较，未知字段也参与比较；login 混版本运行时，各 pod 盖的戳不同，会导致新号首次入场失败（评审 M3） |
| login `beginParentLoad` 脚本 | 在返回的三元组后追加第 4 个元素 `HGET schema_gen`，供 C4 使用；`inspect` 与 publish 共用这个脚本，追加元素无害（评审核实） |
| go/db 读回执与子表缓存 | 经过 C2 后原样包含行戳 |
| go/db 写后发布缓存 / ACK / 资产证据 | 不改。机制三不做部分写入，body 就是整行 |
| go/db 补投 | 无。投递的字节里已经带戳 |
| `go/db/cmd/save_barrier_restore` | 接受 `schema_gen` 字段：把 `HLEN == 3` 改为字段白名单（v0.6 起也接受 `schema_writer` / `schema_regressed`，写入时删掉）。重建 hash 时写 `schema_gen = max(1, 两行戳的**最小值**)`，覆盖 hash 里已有的值（2026-10-22 起，原为"最大值、只升不降"；理由见 §13 第 8 条） |
| `go/db/cmd/retry_requeue` | 重放的任务照常经过 §6.2 的判定 |
| `tools/merge_zone` | 无。戳随列复制；前向 copy 合服保留整把 hash（含 `schema_gen` / `schema_writer` / `schema_regressed`，r2-mz#1），合服后新实体令牌必然不同，首存照常比较；unmerge 删 hash 后视为"不比较"。`verify:save_barrier` 只核 pending 与两表 ACK，不核这三个字段 |
| `go/data_service/cmd/debug_import`（只在本地用） | 增加前置检查：行戳 > 工具世代时拒绝导入 |
| data_service 回档 | 将来实现时：原样保存字节，显式重置 `schema_gen` |
| `go/db/cmd/data_stress` | 盖本二进制的世代 |
| `go/db/cmd/verifier` | 把戳当作普通列比较 |
| C++ owner_epoch 为 0 的兼容路径（`Save`，不走屏障） | 不经过 C3，也不写 hash（评审 L-D）。这是现有的兼容窗口，保护等级维持现状，列为残余风险 |

评审核实过的只读方：`java/`、`go/scene_manager`、friend、match、data_service 内部逻辑。旧的 Kafka 跨 zone 搬数据链已经下线。C++ 只有 `InitPlayerFromAllData` 一个加载入口。

每一批开工前，按 AGENTS §11.4 重新 grep 清点一次。

---

## 6. 机制三：go/db 不写写方不认识的列

### 6.1 为什么不是"只写改动列"，也不是"按列掩码写"

1. **兼容要防的是"写方不认识的列被写成零值"**，这件事由写方世代唯一确定。"只写改动列"解决的是另一个问题：减少写入量。
2. **"只写改动列"会破坏现有的收敛保证。** 现在的设计（`db-task-delivery-and-retention.md` §A）是每次整份重写、从不重放旧载荷。改成只写改动列后，只要有一次写入被围栏拒绝或进了死信，它改过的那几列就永远缺失，而且不报错。另外，它需要 C++ 维护逐列的脏标记，这正是 `proto_dirty_compare.h` 文首记录的那类缺陷。
3. **v0.1 的"按列掩码写"，只在倒退写时才会改变结果。** 而倒退写本身就说明链路上有截断：掩码保住了新列，但旧列 BLOB 里的嵌套新字段仍会被覆盖，行戳还会声称数据完整。所以对倒退写直接拒写，更正确，也更简单。

"按改动列写"如果另有性能上的需要，作为独立议题提出，并附压测数据（AGENTS §11.3）。

### 6.2 规则

记号：body 戳为 `s`，本实例世代为 `G`，加锁读到的行戳为 `r`。比较时 0 都按 1 处理。

| 编号 | 位置 | 条件 | 动作 |
|---|---|---|---|
| C0 | 写任务，进 SQL 之前 | 本 zone 已关闭引导期，且 `s` 的原值是 0 | **确定性拒写，进死信**，原因 `persist_unstamped`。不做无限延后（评审 M-G）：机制前的 body 在引导期关闭后永远不会成功，延后只会让重试队列永远清不空。死信保留，以后仍可人工重放 |
| C1 | 写任务，进 SQL 之前 | `s > G` | 延后，不耗预算，原因 `schema_too_new`。按世代号判断，因为 proto3 不序列化零值，新列被清零时 body 里根本没有这个字段。升级完成后自然消化 |
| C1b | 行内围栏事务内，围栏判定之后、`tx.Save` 之前 | `s < r` | 违反 P1，**不写**，直接进死信，原因 `schema_regression`，并告警 |
| C2 | 读任务，SELECT 之后 | `r > G` | 立即回失败结果 `Success=false, Error="schema_too_new"`，不回填缓存 |
| 其余 | | | 与现在相同：整行 `tx.Save`。行不存在时走事务外 `Insert`，本版本认识的列按消息值写入（proto3 零值），等价于"从没写过" |

**接线方式**（评审核实了现成路径）：

- **C1b**：
  - `writeOutcome.kind` 增加 `schema_regression`，走现有的 `quarantineOrderingConflict` 路径。这条路径不重试、直接进死信，还会调用 `markSaveOutboxTerminal`。
  - legacy 分支和 dual 的 `default` 分支，都在 `tx.Save` 之前各判断一次。
  - `runDBOp` 的 kind 白名单和提前返回要加上它，否则它会被当成 unknown outcome，进入耗预算的重试。
  - `handleTask` 的 switch 增加对应 case，指标增加对应的死信标签。
  - 围栏结局 older_seq / stale_owner / equal 的优先级保持不变。
- **C2**：在 `handleTask` 里对哨兵错误单开分支，调用 `failRead`（`placement_route.go`）。不能落到 `deferFailedTask`，否则会照常耗费读预算重试（评审 M7）。
- **C0 标记**：参照 `db:save_protocol` 标记，用 `db:persist_schema:bootstrap_closed:zone:{z}`，读取位置在 `db.go` 的 `AnnounceStrictGeneration` / `WatchLegacy` 旁边：
  - 启动时读一次，读取失败就拒绝启动；
  - 之后按 `WatchLegacy` 的 5 秒节拍刷新，结果存入 `atomic.Bool`，一旦置真就不再回退；
  - 不在每条任务上都去 GET。
- **新的延后原因**：`deferReason`（`store_failure.go`）和 `retryBudgetExempt`（`placement_route.go`）各加一个哨兵；`deterministicWriteRejectReason` 不收这个哨兵。
- **日志限速**：C1 延后时每条任务都会打一行 ERROR，纳入现有的限速汇总（评审 L-C）。

**延后的代价**：延后是针对单条任务的，不会阻塞整个分区。但在 go/db 回滚、或者违反发布顺序期间，同一个 key 的每次存盘都会被延后，重试列表会持续增长 [I]。告警阈值设为持续 5 分钟不为 0，处置方式是升回去，或者把滚动发布走完。

**单表死信**：C1b 或 C0 只让其中一张表的任务进了死信时，两表的 ACK 永远不会相等，这名玩家的冷加载和交接会一直卡住（评审 L-G）。运维手册要写明处置步骤：先确认 Redis 父快照完好，再按 `retry_requeue` 的流程重放，或者人工修复。

### 6.3 DDL 顺序

沿用现有门禁：新版 go/db 启动时，如果库里缺 proto 声明的列，就拒绝启动（`assertSchemaUpToDate`），所以 DDL 一定先于新版 go/db。

---

## 7. 混版本矩阵

假设世代 6 新增了顶层列 `X` 和子消息字段 `y`。

| 场景 | 结果 |
|---|---|
| 新版 scene(6) → 新版 go/db(6) | 整行写，行戳为 6 |
| 新版 scene(6) **接手戳为 5 的数据，并连续存盘多次** | 第一次：令牌不同，加载世代 5，hash 为 5 或不存在，通过，hash 升到 6、记下令牌。之后每次：令牌相同，不比较，通过（这一行是修复 S-A 的回归用例） |
| 同上，期间实体换了 owner 代次（E→E+1 复用实体、B5 采纳） | 令牌随实体保留，不比较，通过 |
| 第一次存盘已提交，但回包丢失后重发同一元素 | 走同版本重放分支，跳过 C3，返回 1 |
| 第一次存盘已提交、回包丢失，随后被排队值顶替（任意代次） | 令牌已被第一次写入记下，不比较，通过 |
| 同一代次先后加载出第二个实体（同落点重连、废黜后回流、同号新进程接替） | 新实体令牌不同，照常比较它的真实加载世代 |
| 旧版 scene(5) 接手戳为 6 的数据 → 新版 go/db(6) | 对象内携带 `X` 和 `y`；输出世代 6，`s = G`，整行写，数据不丢 |
| 旧版 scene(5) 接手戳为 5 的数据 → 新版 go/db(6) | `s = 5`，`r ≤ 5`，整行写；`X` 写为零值，因为它从来没被写过 |
| 新版 scene(6) → 旧版 go/db(5) | C1 延后并告警，等新版 go/db 处理 |
| 冷加载：行戳 6 → 旧版 go/db(5) 读 | C2 立即失败，玩家稍后重试 |
| 冷加载：行戳 6 → 新版 go/db 读 → 旧版 login 组装 | Go 原样保留，父快照戳为 6 |
| 引导期关闭后，误部署了机制前的 scene | 它写出的父快照戳为 0，内容已被截断；它的 DBTask 被 C0 送进死信，SQL 不受影响。下次加载时 C4 或 C3 判定倒退（加载世代 1 < hash 6），enforce 模式下拒绝。按运维手册处理：清掉父快照，走冷加载，从 SQL 恢复。它在线期间的进度会丢，这是误部署的代价 |
| 全部 scene 从 6 回滚到 5 | 对象内携带，数据不丢。**前提**是世代 6 没有扩展持久值域（§8.3） |

---

## 8. 发布与回滚顺序

### 8.1 引导期：上线机制本身

1. **生成器就位**：台账写入世代 1 的基线，冻结清单和投影清单通过生成器检查，往返一致性测试通过。
2. **引导期内不升世代**：台账为 `state=bootstrap` 时，生成器把新增字段一律记为世代 1。这样不会阻断其他人加持久字段，世代号也始终是 1。这与"引导期禁止升世代"的目的一致：机制前的二进制还在时，任何大于 1 的世代都没有意义。R1–R3 照常检查。状态不能从 `active` 改回 `bootstrap`，生成器会拒绝。
3. **发布顺序**：
   - DDL：两张表各加戳列，默认值 0；
   - go/db：全部 zone，此时不开 C0，`G = 1`；
   - login；
   - scene：开启携带、盖戳，C3 / C4 用 warn 模式。
4. **判定全网已无机制前的写方**：只看 go/db 侧原值为 0 的写 body 计数。这个计数归零，并持续一个完整的存盘周期后，由负责人逐个 zone 写入引导关闭标记（开启 C0），再去掉台账上的"引导中"标记。
5. **切到 enforce 之前**，必须先在 staging 上做一次**真实升世代**的演练：加一个测试用的持久字段，让 gen 升到 2；然后用新旧两套二进制按 §7 的矩阵混跑，重点验证"新版接手旧戳数据并连续存盘"和"回包丢失后重放"两项。演练中 C3 / C4 的倒退计数为 0，并且线上 warn 模式观察期内计数也为 0，才能切到 enforce。"计数为 0"指三项同时为 0（2026-10-22 补）：C++ `[PersistSchema] regression_warned`（LogQL `metric=persist_schema_regression`）、`login_persist_schema_regression_total`、以及巡检 `save_barrier` hash 里 `schema_regressed` 字段的存量（§5.2"warn 证据"一行：判倒退的写方回包丢失后随即被废黜时，前两项可能都漏计，只有这个字段留着；巡检方法见 `deploy/k8s/README.md`）。不能用"下一个实体或 C4 会补计"代替：数据被新版写方以更高世代重新写过之后不会再判倒退。开着 dev 旁路 `AllowUnsafeCrossNodeHandoff` 的环境不切 enforce（§5.2"前提"一行）。
6. **首次上线之后，台账必须在第一次带新持久字段的发布之前切到 `active`**（2026-10-20 补，审计 r2-ps#1）。引导期内新增字段一律并入世代 1，所以「同为世代 1」的两个二进制，认识的持久字段集合可以不同；C1 / C1b / C2 只比世代号，看不出这种差别。上线后台账若仍是 `bootstrap`，第一次带新持久列 F 的滚动发布里：新 scene 的存盘戳仍为 1，落到不认识 F 的旧 go/db 上 C1 不拦，`tx.Save` 只写认识的列，F 停在旧值照常 ACK；冷加载落到旧 go/db 时只 SELECT 认识的列，C2 不拦，父快照缺 F，scene 再存盘就把 F 写成零值或旧值。F 若是限购或幂等账本，就会重复发放。项目尚未上线，首次上线整批起步不触发；上线后第一次带新持久列的发布即可触发。
   - 人工门禁写在 [release-checklist.md](../ops/release-checklist.md) §6.2（关闭引导期后由负责人改台账 `state`）与 §2.1 ⑦（发布前比对台账，有新增字段行而仍是 `bootstrap` 就停止发布）。
   - 机器门禁暂缺，记为待办。可选做法：生成器在世代号之外再产出持久字段集合指纹，go/db 遇到「世代相同、指纹不同」时延后或失败；或由发布预检比对线上版本与待发布版本的台账。

### 8.2 此后每次升世代

`DDL（如果有新列）→ go/db 全部 zone → login → scene（可以按 zone 金丝雀）`

顺序如果被违反，C1 / C2 只会延后或失败，不会丢数据。

可选项（第 4 批）：在 go/db 的能力心跳里带上世代号，`k8s_deploy.ps1` 发布 scene 之前做只读预检。

### 8.3 回滚规则

| 对象 | 能否回滚 | 说明 |
|---|---|---|
| login | 可以回滚到任何一个带本机制的版本 | Go 原样保留 |
| scene | **有条件** | 对象内携带保证未知字段不丢，但**持久值域扩展**不在台账里：本版本解释不了的持久值，如果加载期按表或常量把它删掉、改写，回滚或混版本跨 zone 时就会丢数据。已落码的处理（Codex 2026-10-24 已有构建与全量用例运行的旁证，见 §12.2；没有做过真实回滚演练）：<br>- 第 5 批：越界包类型原样携带，查不到表的技能保留但不生效；<br>- 2026-10-23 一批（§12.2）：属性 / 宝宝加点的维度与超额、职业技能重数与归属、等级上限，都改为原样携带、按本版本生效。<br>**解除条件**：回滚目标版本本身含这些批次（更早的版本照样会删），且发布所用的代码已按交接 §13.2 复跑通过。满足之前，发布单须逐项写明本版本**没有**做以下扩展，写不出就不能回滚 scene：新增包类型；新增技能；新增属性或宝宝加点维度；调大属性池或宝宝池点数、降低解锁等级；调大职业技能 `max_rank`；提高等级上限。<br>**始终适用**（与验证无关）：调大 `kMaxAppliedRecords` 的版本不可回滚 scene（D7）；改技能的职业 / 通用归属、复用表行 id、下调上限等，按 §12.2 的约束处理。<br>**不限制回滚**：标"不适用"的 N1–N15（前提是回滚目标版本不早于 2026-10-22 核对的代码）；W1–W3（装备槽位挪位、坐标拉回出生点、当前 HP / MP 夹到上限）不删资产，已接受。回滚期间，用过新资产通道流号或新收件箱结局的玩家，资产通道 / 收件箱会停用到升回（N8、N9，不丢数据） |
| go/db | 只能回滚到世代 ≥ 数据最高戳的版本 | 回滚得更低会卡住存盘和冷加载，但不丢数据，处置方式是升回去 |
| 回到机制上线前的二进制 | 禁止 | 引导期关闭后，C0 与 C3 / C4 会兜底 |

### 8.4 告警与运维手册

| 告警 | 来源 | 处置 |
|---|---|---|
| `DbTaskSchemaTooNew` | go/db 的 Prometheus 指标 | 检查 go/db 版本，升回去，或者把滚动发布走完 |
| `DbTaskPersistUnstamped` | go/db 死信原因 | 找到机制上线前的写方，下线它 |
| `PersistSchemaRegression` | go/db 死信原因、login 指标、C++ 日志（LogQL） | 按 `where` 定位出问题的那一跳，暂停该跳的发布；清掉父快照，走冷加载恢复。单表死信按 §6.2 处理 |

- 运维手册写进 `deploy/k8s/README.md`；
- Prometheus 告警规则写在 `deploy/k8s/persist-schema-alerts.yaml`；
- C++ 侧的告警按 `docs/ops/grafana-loki-local-logs.md` 的 LogQL 方式配置。

---

## 9. 分批实施（每批 < 30 个文件，AGENTS §10.2）

**实施状态（2026-10-19）：**
- **第 1 批：已验证。** Codex 已跑正式 `proto-gen-build` 和 no-unity `proto-gen-run`，台账写入世代 1 的基线（引导期内其他会话新加的 `player_database#22` 也已并入世代 1）；生成器持久 schema 单测通过；本地库迁移出两表 `persist_schema_gen`（`int unsigned`，默认 0）。证据见 `build/verification/enter-game-smoke-20261019-044832/`。
- **第 2 批：主体已落码，go/db 模块构建通过，单测未跑。** C0 / C1 / C1b / C2、延后与死信原因、引导期标记与复查、指标都在 `go/db/internal/kafka/persist_schema.go` 及其接线处；同一份 Codex 构建记录里 go/db 模块原生退出 0，输入清单含该文件。后续补齐（Claude，未编译、未测试）：
  - `persist_schema_test.go` 单测，以及 C1b 真库集成用例 `TestIntegrationPlayerSQLRejectsPersistSchemaRegression`；
  - `data_stress` 盖戳、`debug_import` 世代前置检查；
  - `deploy/k8s/persist-schema-alerts.yaml` 与 `deploy/k8s/README.md` 运维小节。

  `save_barrier_restore` 接受 `schema_gen` 字段、并按两行 SQL 世代的最大值重建屏障，这部分由 Codex 在 `da0f1822c` 完成。
- **第 3a 批：已落码，未编译、未测试（Claude，2026-10-19）。** 待 Codex 按下面「Codex 验证」一列执行：
  - 载体与统一入口：`unknown_field_carry.h`（engine/core/utils/proto）、两个 Carry 组件（`player_persist_carry_comp.h`，在两张表的手写 Unmarshal 里创建）、`player_persist_marshal.h`（统一入口，只有头文件）；存盘的 `MarshalPlayerForSave` 与回档快照 `SnapshotSystem::CaptureAndSend` 都改为调用它，两表盖戳 max(本二进制世代, 两表加载世代的最小值)；
  - 对象内携带：欠款 `CurrencyDebt.unknownFields`（`SaveToProto` 改为返回 bool，解析失败整份拒存）、`MissionListComp` 本层未知字段（重复 scope 只取首份）、`SanitizeSkillList` 替换前并回组件本层未知字段；
  - 守卫：`ResetAssetOpStreamForEpoch` 改为 `Clear()` 后重设流号与纪元（清掉的字段与旧写法一致，包括回执）；`ItemStore::IsIndependentInstance` 统一 `CanStack`、reserve 侧空余估算、整理合并三处判定；`AttachDebt` 遇到带未知字段的既有欠款返回 `kFeatureUnavailable`，不合并；
  - 测试：新增 `cpp/tests/bag_test/persist_round_trip_test.cpp`（注入、两跳往返、覆盖率断言、复用输出不翻倍、盖戳三种组合、缺载体拒存）；`bag_instance_data_test.cpp` 补带未知字段实例的合堆用例，`asset_op_ledger_test.cpp` 补纪元重置清未知字段用例，`currency_test.cpp` 补欠款携带与合并守卫用例；手搭实体的测试补齐两个 Carry 组件或改走统一入口（`bag_test`、`player_feature_persistence_test`、`player_equip_attribute_test`、`cross_zone_test`、`db_task_dispatch_test`、`save_seq_test`）。
  - **已知缺陷（交接前只读核对发现，均经对抗复核）**，列为第 3a′ 批，详见 [交接 §3.1](../handoff/mixed-version-persist-handoff-20261019.md)：
    - 收件箱执行器把 `AttachDebt` 的版本错位拒绝记成终态 REJECTED（应按版本错位处理：最初定为 `kDefer`，对抗复核 A-1 后改为 `kVersionSkew`，见下一条）；
    - `Bag::AddItems` 规划仍只按 `has_equip()` 分流，与提交侧判定不一致；
    - 宝石拆卸预检 `HasStackWithRoom` 判据未跟上。
- **第 3a′ 批：已落码（Claude，2026-10-19）；Codex 10-24 运行旁证：下列新用例在 currency_test / bag_test 全量运行中通过（§12.2）。** 三条缺陷各自修正并配用例：
  - `CurrencySystem::DebtCarriesUnknownFields`（只读谓词，`AttachDebt` 与执行器共用）；`GmSanctionInboxExecutor::AttachCurrencyDebt` 命中回新结局 `kVersionSkew`（对抗复核 A-1 后由 `kDefer` 改来），`PlayerInboxSystem` 据此停用本域（`PlayerInboxHaltReason::kVersionSkew`）并确认已定前缀；用例 `GmSanctionInboxExecutorTest.AttachCurrencyDebtReportsVersionSkewWhenExistingDebtCarriesUnknownFields`、`PlayerInboxSystemTest.ExecutorVersionSkewHaltsAfterConfirmingThePrefixAndNeverResumesLocally`，`CurrencyTest.AttachDebtRefusesToMergeIntoDebtWithUnknownFields` 补谓词断言；`player-inbox-service.md` §7.5 / §7.6 与 `docs/ops/player-inbox-runbook.md` §5.2 A′ 同步；
  - `ItemStore::IsIndependentInstance` 改为公开，`Bag::AddItems` 规划按它分流；用例 `BagInstanceDataTest.AddItemsPlansIncomingInstancesCarryingUnknownFieldsAsSeparate`；
  - 宝石拆卸预检 `HasStackWithRoom` 改为直接调 `ItemStore::CanStack`；用例 `PlayerEquipTest.RemoveGemPrecheckSkipsAStackCarryingUnknownFields`（用日志出口探针确认没有走到容器层的调用栈分支）。
- **第 5 批：已落码（Claude，2026-10-19）；Codex 10-24 运行旁证：下列新用例在 cross_zone_test / skill_test / currency_test 全量运行中通过（§12.2）。** 见 §12 两行：
  - 越界包类型：`PlayerBagsComp::unsupportedBagTypes` 携带 bag_type ≥ `kBagTypeCount` 的整份 `ItemEntry` 与下标 ≥ `kBagTypeCount` 的容量，`bag_marshal::Unmarshal` 整体重填、`Marshal` 原样追加；用例 `CrossZoneBagMarshal.UnmarshalCarriesOutOfRangeBagTypeVerbatim`（原 `UnmarshalDropsOutOfRangeBagType` 改写）。携带记录参与跨包 guid 唯一性检查：新增 `Bag::ContainsGuid`、`PlayerBagsComp::ContainsGuid`，资产通道入账改用后者（对抗复核 D-1）；用例 `AssetOpSystemTest.CreditRejectsAnInstanceAlreadyCarriedInAnUnsupportedBagType`；
  - 查不到表的技能：`SanitizeSkillList` 只去 0 与重复项，查不到表的保留；新增 `PlayerSkillSystem::IsActiveSkill`，`HasSkill`、`ListSkills` 下发只认生效的项，战斗快照原本就跳过查不到表的项；`ReleaseSkill` 原本先按表拒。用例 `PlayerSkillSystemTest.SanitizeSkillListKeepsUnknownSkillsInactiveAndRemovesDuplicates`（原 `SanitizeSkillListRemovesUnknownAndDuplicateSkills` 改写）、`SanitizeSkillListLeavesACleanListUntouched`。`ListSkills` 的过滤没有单测（处理器不在任何测试工程里）。
- **第 3b 批：已落码，未编译、未测试（Claude，2026-10-19～20）。** §5.2 先改为写方令牌（v0.6，§15）再落码：
  - 严格屏障 Lua 11 参（`num_gt`、C3 放在 owner / 版本判定之后、同版本重放与本实体令牌跳过、warn 返回 2 照写、enforce 返回 3 不写、`schema_gen` 只升不降、`schema_writer`）；`RedisSaveSchemaGen` 与入队前同口径校验；`OnSaved` 的 2 / 3 分支与两个新回调（返回 3 时排队值 guard 与令牌都相同才丢）；
  - scene：`PlayerPersistWriterComp`（令牌，存盘写入口懒建）、调用点传输出 / 加载世代与模式、`HandlePlayerSaveSchemaWarned` / `HandlePlayerSaveSchemaRejected` 分派、`persist_schema_stats` 与 30 秒 `[PersistSchema]` 汇总；
  - 配置：`BaseDeployConfig.persist_schema_regression_mode`（字段 23，**proto 待 Codex 重新生成**）、`readBaseDeployConfig` 归一化、`bin/etc` 默认 `warn`、`k8s_deploy.ps1` 镜像与生成期校验、契约测试、`start_game.ps1` 启动前校验；
  - `save_barrier_restore` 字段白名单接受 `schema_writer`；运维 LogQL 固定查询（`deploy/k8s/README.md`）。
- **第 4 批：已落码，未编译、未测试（Claude，2026-10-19～20）。** login C4：`beginParentLoad` 追加 `schema_gen` 与存在标记（字段存在但为空串按损坏），快路径在身份 / 水位 / 种子校验之后判；warn 计数放行、enforce 返回错误走现有失败路径；`login_persist_schema_regression_total{reason,mode}` 起服预置 4 条序列；`login.yaml` 键与启动期归一化（非法值拒绝启动）；`k8s_deploy.ps1` login ConfigMap 镜像与契约测试；告警 `LoginPersistSchemaRegression`；种子不盖戳与 backfill 往返用例。
- **§12.2 一批（持久值域 D1–D6 与等级上限）：已落码（Claude，2026-10-23～24）；Codex 10-24 构建与三个测试工程全量运行通过（旁证，§12.2）。** 加载与重算不再改写加点、技能重数、等级：纯规则在 `attribute_allocation_rules.h`（`StoredPoolAllocation` / `PoolAllocationEffective` / `RemainingPoints`，角色与宝宝共用）、`player_level_rules.h`（`CappedLevel`）；改动在 `player_attribute.cpp`、`player_pet.cpp`、`player_skill.cpp`、`player_database_loader.cpp`，以及各等级读点。用例在 `skill_test`、`turn_battle_engine_test`、`bag_test`（含 `PersistRoundTripTest.ValueDomainsBeyondThisBuildSurviveLoadAndSave`）。Codex 验证清单见交接 §13.2。
- **恢复工具口径已对齐（2026-10-22，未编译）**：`save_barrier_restore` 的 `schema_gen` 改为两行戳最小值、覆盖旧值，与 C3 / C4 的加载世代同口径（§13 第 8 条）。
- **第 6 批：已落码（Claude，2026-10-23；10-30 按对抗复核补齐）。Codex 10-24 运行旁证：currency_test 318 条、bag_test 708 条全量通过，其中含这批 10-23 的新用例；Go 只有 shared 模块有 10-24 全量单测旁证。10-30 新增或改动的部分未编译、未测试。** 三件事：
  - 退役字段残留不算版本错位：生成器把台账的 `retired=` 行生成为 `persist_schema::kRetiredFields` / `IsRetiredField`；`unknown_field_carry::CarriesNewerVersionFields` 取代 `HasUnknown`，合堆、欠款合并、按数量扣除三处守卫统一用它（§4.4）；
  - 按数量扣除守卫（§4.4）：`IsCountableByConfig`、三态 `CheckQuantities`（`HasAll` 已删）、`DrainStacks` 过滤；资产通道、强化、镶嵌、战斗快照四处按三态分支。10-30 补：资产扣出把版本错位排到按 guid / 宝宝的确定性校验之后；聚宝斋卖家在托管阶段也可撤单（材料订单的托管腿没有截止，否则 RETRY 没有出口）；
  - 新币种（§12）：取值表唯一来源 `proto/common/base/currency_type.proto`，C++ `currency.h` 与 Go `shared/currencytype` 都从它派生，`IsKnownCurrencyType` 收成一份；Go 各入口（收件箱入箱、邮件附件、帮会捐献配置、交易配置与托管包、神秘商人表）按同源取值表校验；scene 收件箱执行器遇到不认识的币种回 `kVersionSkew`（与版本无关的解码、金额校验先判），资产通道新增第 8b 步回 RETRY + 1006、不记账（排在中止与确定性包校验之后）；战斗节点奖励档的两处币种判据改调同一谓词。

  文件、用例与 Codex 命令见[交接 §14](../handoff/mixed-version-persist-handoff-20261019.md)。

| 批 | 内容 | 主要改动 | Codex 验证 |
|---|---|---|---|
| 1 | 台账；生成器（发号、冻结和投影检查、R2 / R3、"引导中"标记）；两个 option 及其标注；两张表的戳字段；生成常量与清单 | `proto/db/proto_option.proto`、各持久 proto 的标注、`proto/persist_schema_gen.txt`、`tools/proto_generator/protogen/internal/...`（新增生成器及测试）、生成产物 | 先 `proto-gen-build`，再 `proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`；protogen 跑 `go test ./...`；最后完整编译 C++ 与全部 Go module。生成的 .pb 文件如果超过 30 个，先按 §10.2 报告 |
| 2 | go/db：C0 / C1 / C1b / C2 及其接线，新的延后原因和死信原因，指标、告警、引导期标记，日志限速；cmd/migrate 的 DDL；save_barrier_restore、debug_import、data_stress 的配套改动 | `go/db/internal/kafka/{player_row_fence,key_ordered_consumer,store_failure,placement_route}.go`、`go/db/db.go`、`go/db/internal/metrics`、`go/db/internal/barrierrestore/scripts.go`、`go/data_service/cmd/debug_import`、`deploy/k8s/persist-schema-alerts.yaml`、测试 | go/db 与 data_service 跑 `go test ./...`、`go vet`；真库集成测试（自起一次性 MySQL / TiDB） |
| 3a | C++ 携带：两个 Carry 组件（在手写 loader 里创建）、统一入口、欠款与任务列表的携带、`SanitizeSkillList` 保留顶层未知字段、`ResetAssetOpStreamForEpoch` 改为 `Clear()`、§4.4 的两个守卫、回档快照改入口、盖戳、往返一致性测试与全组件夹具、受影响测试的修正 | `services/scene/player/{comp,system}/...`、`modules/currency/...`、`mission_marshal.cpp`、`player_skill.cpp`、`asset_op_ledger.cpp`、`modules/bag/item_store.cpp`、`snapshot_system.cpp`、bag_test 及 §4.3 列出的测试、vcxproj / CMakeLists。文件可能接近 30 个，必要时把守卫拆成 3a′ | `game.sln` 串行 `/m:1` 完整编译；新测试和所有存盘相关单测 |
| 3b | C++ 校验：严格屏障 Lua 增加到 11 个参数、`num_gt`、写方令牌（v0.6）、返回值 2 / 3、`OnSaved` 的处理与新回调（`core/system/redis.cpp`）、`BaseDeployConfig` 及部署渲染、`save_barrier_restore` 字段白名单 | `redis_client.h`、`core/system/redis.cpp`、`player_lifecycle.cpp`、`currency_test.cpp`（现有用例断言"返回 2 是坏回包"、字面量白名单、以 7 个参数直接 EVAL 的真 Redis 用例、调用文本契约，这几处都要改写）、`base_deploy_config` 的 proto / `config.cpp` / yaml、`k8s_deploy.ps1` 及其契约测试、`start_game.ps1` | `game.sln` 完整编译；Redis 脚本测试；k8s_deploy 契约测试 |
| 4 | login：C4（`beginParentLoad` 增加第 4 个返回元素并比较）、种子保持不盖戳的测试、backfill 往返测试、`login.yaml`；可选的能力心跳与部署预检；运维手册 | `go/login/internal/logic/pkg/dataloader/...`、`go/login/etc/login.yaml`、`deploy/k8s/README.md` | login 跑 `go test ./...`、`go vet` |
| 5 | §12 中持久值域的两个删数据点（这是 scene 无条件回滚的前提） | `player_skill.{h,cpp}`、`player_skill_handler.cpp`（`ListSkills`）、`bag_marshal.{h,cpp}`、`modules/bag/comp/player_bags_comp.h`、`modules/bag/bag_system.h`（`ContainsGuid`）、`asset_op_system.cpp`（入账查重）；测试 `cross_zone_test.cpp`、`skill_test.cpp`、`asset_op_system_test.cpp`、`persist_round_trip_test.cpp`（注释） | 同 3a，另跑 `skill_test` |
| 6 | 退役字段残留过滤；按数量扣除守卫；新币种取值表与版本错位 | `persistschema/{render,generate}.go`、`unknown_field_carry.h`、`modules/bag/{item_store,bag_system,bag_service}.*`、`asset_op_system.cpp`、`player_equip.*`、`player_battle.cpp`（快照一行）、`proto/common/base/currency_type.proto`、`currency.h`、`currency_system.cpp`、`gm_sanction_inbox_executor.*`、`player_mystery_merchant.cpp`、战斗节点奖励档两处、`go/shared/currencytype`、Go 各入口、`go/trade/internal/commerce/engine.go`（托管阶段撤单）；测试见交接 §14 | 生成产物已在库，无需重新生成；`game.sln` 串行 `/m:1`；bag_test、currency_test、turn_battle_engine_test；Go 受影响模块 `go test ./...`、`go vet ./...`；trade 真库用例需 `COMMERCE_TEST_DSN` |

每批完成后在 `docs/PROGRESS.md` 追加一条，并注明 Java 版的状态（§11）。

---

## 10. 测试计划

**C++（3a / 3b / 5）**：
- 往返一致性测试，包括覆盖率断言。
- 欠款：删除后用同一个 `currency_type` 重建，不应带上旧的未知字段；不删除时，未知字段应保留。
- 任务：放弃后重新接取，不应带上旧的未知字段。
- 合堆守卫：带未知字段的物品不参与合堆。
- `AttachDebt` 守卫：已有欠款带未知字段时，返回失败。
- `ResetAssetOpStreamForEpoch` 之后，未知字段为空。
- 两张表的顶层未知字段往返后保留；加载世代取两表的 min，输出世代取 max；同一份数据连续存两次，第二次命中快路径。
- 严格屏障 Lua（真 Redis，`RedisCommitBarrierSchemaGenAgainstRealRedis`）：
  - `num_gt` 的边界：9 对 10、10 对 9、1 对 19、相等；
  - hash 不存在 / 没有 `schema_gen` / 值损坏（空串、`0`、前导零、超界、非数字）；
  - 新实体（令牌不同）倒退：warn 返回 2 并照写，enforce 返回 3 且父快照、pending、意图、令牌、世代都不动，再发仍返回 3；
  - **同一实体连续存盘**、**同一实体换 owner 代次后存盘**都通过（S-A 回归，覆盖 E→E+1 复用与 B5 采纳）；
  - **回包丢失后重发同一元素**走重放分支，即使令牌被换、记录值更高也返回 1；
  - 已被废黜的持有者先拿 0，不先拿 3；
  - warn 模式下 hash 只升不降；
  - 参数非法（世代越界或前导零、加载 > 输出、模式大小写错、令牌空 / 超长 / 非法字符）报 `SAVE_BARRIER invalid schema argument` 且不改任何键；参数个数不对报错；别名键用例改为 11 参并断言 `keys must differ`。
- `OnSaved`（无连接单测）：返回 2 是提交证据且计数回调先于提交钩子；返回 3 不重试、不准入、丢 guard 与令牌都相同的排队值、走 C3 回调而非废黜回调；返回 3 时 guard 不同或令牌不同的排队值照发不回调；非屏障元素收到 2 / 3 按坏回包重试；`SaveWithCommitBarrier` 入队前拒绝非法覆盖世代参数，合法参数随元素携带；令牌生成器产物合法且每次不同；模式归一化。
- warn 证据不随回包丢失消失（真 Redis，`RedisCommitBarrierSchemaWarnEvidenceSurvivesLostReplyAgainstRealRedis`）：判倒退后重放同一元素、同令牌后继、换代后的同令牌写入都返回 2；别的写方无倒退写成功后清掉证据；enforce 拒写不留证据。
- 严格屏障 11 参在线格式（真连接，`RedisCommitBarrierWireFormatThroughClientAgainstRealRedis`）：经 `MessageAsyncClient` 发出，第一次走 EVAL、第二次走 EVALSHA，参数取可区分的值；第三次另一写方被拒走 C3 回调。
- 令牌的 ECS 契约（`cross_zone_test` 的 `ExitPersistEcs.PersistWriterTokenIsPerLoadedEntity`）：读路径不建、同实体两次相同、换 owner 代次不变、同一玩家重新加载（实体号可能复用）必变、形状合法。warn 计数按写方去重（`SchemaWarnOnlyCountsAndGonePlayerIsNoop`）。
- 判据本身（真 Redis，`RedisCommitBarrierForeignWriteBetweenSameEntityWritesIsComparedAgainstRealRedis`）：外来写夹在同一实体两次写之间时照常比较（生产中不可达，§5.2"前提"一行）。
- 新回调在退出中、交接中（标记未发）、在线三种状态下的分派，以及 warn 只计数、实体已不在时为空操作（`cross_zone_test` 的 `ExitPersistEcs.Schema*`）。
- 尚无单测、靠 §8.1 第 5 步演练覆盖的：首存在途时排队的后继、首存被顶替（未提交 / 已提交回包丢失）、同落点重连在同一代次下重载——这些都由"令牌是否相同"一条规则决定，Lua 用例已逐一覆盖判定本身，端到端的异步时序没有单测宿主。

**Go（2 / 4）**：
- C0：引导期关闭后，`s = 0` 进死信。
- C1：`s > G` 时延后且不耗预算；新列被清零（body 里没有该字段）时同样延后。
- C1b：`s < r` 时进死信，SQL 不变；围栏判定的优先级不变；runDBOp 白名单包含新的 kind。
- C2：立即失败，不回填缓存，不消耗读预算。
- 用 `dynamicpb` 模拟高世代的 body。
- login C4 的 warn 与 enforce 两种模式；种子不盖戳时 `isUnownedLoginSeed` 判断不变；backfill 往返保留戳和未知字段。
- save_barrier_restore 接受 `schema_gen`，写入值 ≥ 1；debug_import 的前置检查生效。

**生成器（1）**：
- 新增字段 → 追加世代；
- 删除并写了 reserved → 标为 retired；
- 以下情况生成失败：改类型、复用号、给 oneof 加成员、给冻结类型加字段、投影字段的类型没有其他载体可达、"引导中"期间追加世代；
- 输出稳定、可重复。

**真实环境（第 2、3 批之后，由 Codex 或负责人执行）**：按 §8.1 第 5 步做真实升世代演练，按 §7 矩阵逐行混跑验证。这一步不能用单测代替；跑完之前，不能宣称"混版本安全"。

---

## 11. Java 版对齐（AGENTS §12）

- **形态** [C]：玩家状态存在 `player_state.data` 这一个 BLOB 里，内容是 `PlayerState` 的 PB，用 `toByteArray` / `parseFrom` 读写；`player` 表的其他列由固定 SQL 写入。
- **适用性**：
  - 机制三不适用。
  - 机制一（对象内携带 + 往返一致性测试 + 语义耦合守卫）和机制二（世代戳 + 加载 / 写入校验）都需要做。
- **状态**：**待做**。Java 要按自己的存储形态来实现（§12 规定服务端内部按 Java 惯用方式）。第 1 批实施时，在 Java 仓的 `PARITY.md` 登记一行。

---

## 12. 不在本次范围、但属于同类的丢数据点

| 问题 | 现状 [C] | 建议 |
|---|---|---|
| 持久值域：旧配置表删掉新技能 | 第 5 批前（以及不含第 5 批的回滚目标版本）：`SanitizeSkillList` 删除表里查不到的技能 | **第 5 批已落码**（Codex 10-24 运行旁证通过，§12.2）：保留，但不生效（`PlayerSkillSystem::IsActiveSkill`；`HasSkill`、`ListSkills`、战斗快照、`ReleaseSkill` 都只认生效的项）。附带约束：技能表行 id 删除后**不得复用**给另一个技能，否则留在老玩家数据里的不生效 id 会在复用后自动生效 |
| 持久值域：越界的包类型 | 第 5 批前（同上）：`bag_marshal::Unmarshal` 丢弃越界条目；`capacities` 只输出 `kBagTypeCount` 个 | **第 5 批已落码**（Codex 10-24 运行旁证通过）：越界条目与多出的容量存进 `PlayerBagsComp::unsupportedBagTypes`，原样存回；本版本的玩法看不到也不碰它们，但它们仍参与跨包 guid 唯一性检查（`PlayerBagsComp::ContainsGuid`，资产通道入账用它；漏查会让同一实例在新旧两个包各落一份，2026-10-19 对抗复核 D-1） |
| 持久值域：其他按配置表或代码常量解释的持久值（物品 config_id、任务 / 条件 config_id、宠物种类、属性加点维度、装备槽位、越界币种等） | **已核对（2026-10-22，只读代码 [C]，未编译，没有回滚场景用例）**，逐项见 §12.1：<br>- 会删 7 项（D1–D7）：属性加点维度、宝宝加点维度、属性加点超额整池返还、宝宝加点超额清空、职业技能重数压回、技能改职业 / 通用归属、战斗结算账本条数上限；<br>- 会改写 3 项（W1–W3）：装备栏槽位挪位、入场坐标吸附或拉回出生点、当前 HP / MP 夹到本版本上限；<br>- 其余 15 项原样保留，标"不适用"（N1–N15）：物品 config_id、堆叠数、包容量、动态包 profile、装备强化 / 词条 / 宝石、币种、任务 / 条件 / 成就 / 活动、资产通道账本、收件箱账本、限购记录、属性方案数、宝宝种类 / 数量 / 等级、职业、复活、不持久化的 buff / 称号 / 坐骑 / 时装。<br>原线索里"装备槽位挪位"属实（W1）；"查不到表的物品不丢""任务查不到表不删"属实（N1、N7）；"加点维度会删"属实（D1、D3） | **D1–D6 已于 2026-10-23 落码**（Codex 10-24 运行旁证通过）：原样携带、按本版本生效，见 §12.2；D7 保留发布约束，W1–W3 接受。标"不适用"的值域随本次核对解除回滚限制；D1–D6 在回滚目标版本含这一批、且发布代码复跑通过之后解除（§8.3） |
| 持久值域：代码常量上限（等级上限 `playerlevel::kMaxLevel`） | 2026-10-23 前（以及不含这一批的回滚目标版本）：加载时 `ClampToMaxLevel` 把超限等级压回（`player_database_loader.cpp`），连带 `ConvergeOverAllocation` 整池返还加点、出战宝宝等级高于主人时取消出战，随下一次存盘落库；新版提高上限后回滚 scene，再升回新版也恢复不了（2026-10-20 对抗复核 O-1） | **2026-10-23 已落码**（Codex 10-24 运行旁证通过）：存储等级原样携带，玩法统一读 `playerlevel::CappedLevel`；超额的加点池整池不生效、不删；出战只在等级真的变化时取消（§12.2）。代价：下调上限不再自动收敛，要配显式迁移。回滚目标版本含这一批、且发布代码复跑通过之后，提高等级上限的版本才可回滚 scene |
| 收件箱、资产通道载荷的新字段与新枚举值 | **新枚举值（币种）第 6 批已落码**：取值表唯一来源是 `proto/common/base/currency_type.proto`；Go 各入口按同源取值表拒绝写错的值；scene 收件箱执行器遇到本二进制不认识的币种回 `kVersionSkew`（停用本域，实体重建后重拉），资产通道回 RETRY + `kFeatureUnavailable`（第 8b 步，不记账）。订正：此前这里写的"资产通道在已有改动之后遇到不认识的币种记 APPLIED_PARTIAL"不对，第 6 批之前实际是在第 8 步、任何改动之前记终态 REJECTED(kAssetInvalidBundle)。<br>**仍未修**：<br>- 载荷新字段：旧版执行器忽略新字段，记 APPLIED（Go 入箱拒未知字段，scene 不判）；<br>- 新物品、新宝宝表行：资产通道入账查不到表时（`ValidateCreditBundle` 的物品 config、整件物品实例、宝宝 `pet_table_id` 三处）在第 8 步记终态 REJECTED。交易交付被拒后合同转人工（托管原件保留，不丢资产），邮件、帮会商店按各自的拒绝补偿。加载侧对查不到表的物品、宝宝原样保留（§12.1 N1 / N12），两边口径不一致（2026-10-30 §12 核对会话转来） | - 载荷新字段：给载荷加版本号，或执行器遇到未知字段时 fail-closed；开启收件箱消费之前必须修。<br>- 新物品、新宝宝表行：与新币种同口径，但要先定表行规则（§13 第 10 条）：物品 / 宝宝表行只增不删（下线改开关），Go 各入口按同源配置表校验 config 存在；满足之后 scene 把"表里查不到"改判为版本错位（RETRY）。在此之前按发布次序约束：scene 与配置表全量先上线，内容开关在全量之后才打开；回滚 scene 不得回滚到不含这些表行的版本。<br>- 新币种的发布次序：Go 侧全量升级完成之后才开始滚 scene（只有首次上线可以同版本整批起步）；scene 全量升级之前，GM 不得对新币种下处置，交易、邮件不得发放新币种 |
| Kafka 控制面命令 | 遇到未知事件直接丢弃 | 新增事件类型时配发件箱或重投机制 |
| guild、mail、trade 的库表 | 未核对 | 按本文的不变量逐个评估 |
| 客户端契约 | 见 `published-contract-compatibility-20261017.md` | 已有独立方案 |
| 待确认 | scene 不 marshal `merge_state(10)` 和 `stress_test_probe(9)`，每次整行写都会把它们清空 | 确认是否是有意设计（例如"首次存盘清除合服标记"）。v0.3 先把它们冻结 |

### 12.1 持久值域逐项核对（2026-10-22）

> **口径**：只读代码 [C]，没有编译，也没有回滚场景用例。行号以 2026-10-22 工作区为准，会漂移，以函数名为主。
> **问题**：新版本扩展了某个按配置表或代码常量解释的持久值（加表行、调大常量、加枚举值），scene 回滚到旧版本后，旧版本加载这份数据会怎样处理？处理结果会不会随下一次存盘落库？

**核对范围**：
- **加载**：`InitPlayerFromAllData`（`player_lifecycle.cpp`）→ 生成的 `PlayerAllDataMessageFieldsUnmarshal` → 手写的 `PlayerDatabaseMessageFieldsUnmarshal`（`player_database_loader.cpp`）。后者依次调用：
  - `bag_marshal::Unmarshal`、`mission_marshal::Unmarshal`；
  - 两本账本的校验；
  - `PlayerCurrencyComp::LoadFromProto`；
  - 技能、属性、宝宝三个 `InitializeOnLoad`；
  - 之后必经 `EnterScene`。
- `PlayerDatabase1MessageFieldsUnmarshal` 只挂 Carry 组件，没有按表解释的值。
- **存盘**：`MarshalPlayerForPersist`（`player_persist_marshal.h`）→ 两张表的手写 Marshal。存盘这一侧不按表过滤：
  - 其余组件 `CopyFrom` 原样写出；
  - 背包、任务由各自的 marshal 从运行时状态重建。
- 所以加载期改掉的东西，都会随下一次存盘落库。

**判定口径**：
- **会删**：持久值被删掉并随存盘落库，升回新版本也恢复不了。
- **会改写**：值被换成另一个值后落库。
- **不适用**：字节原样往返，本版本只是不生效或忽略它。

**会删 / 会改写**（受版本值域影响）：

| 编号 | 持久值 | 依据 | 加载 / 存盘时的处理 [C] | 判定 | 回滚后果与建议 |
|---|---|---|---|---|---|
| D1 | 属性加点维度：`attribute_component.schemes[].allocated` 的键 | `AttributeDimension` 表 | `SanitizeSchemes`（`player_attribute.cpp:631-643`，由 `PlayerAttributeSystem::InitializeOnLoad` 调用）删掉表里查不到的维度 | **会删**（2026-10-23 已改，§12.2） | 新版本加了维度、玩家在上面加过点，回滚后：<br>- 所有方案里这些维度的加点都被删掉，点数退回可分配（不扣资源，丢的是加点布局）；<br>- 升回新版本也恢复不了。<br>建议：查不到表的维度保留但不生效，**仍计入已用点数**（见待拍板第 2 条） |
| D2 | 属性加点超额：同上 | `AttributePool` 表（基础点、每级点数、解锁等级）与等级 | `ConvergeOverAllocation`（`player_attribute.cpp:674-692`，`Recalculate(kLoad)` 调用）：某方案某池的已分配超过本版本算出的总量，就整池删掉 | **会删**（2026-10-23 已改，§12.2） | 新版本调大池点数、降低解锁等级，或提高等级上限（见上表等级上限一行）后回滚：<br>- 超额的方案整池清空，点数返还；<br>- 升回新版本也恢复不了。<br>上限下调时整池返还是有意设计（注释在该函数头），见待拍板第 3 条 |
| D3 | 宝宝加点维度：`pet_component.pets[].allocated` 的键 | 宝宝池维度（`AttributeDimension` 按宝宝池取） | `SanitizeAllocations`（`player_pet.cpp:198-215`，由 `PetSystem::InitializeOnLoad` 调用）删掉不属于本版本宝宝池的维度。种类查不到表的宝宝也照删 | **会删**（2026-10-23 已改，§12.2） | 同 D1 |
| D4 | 宝宝加点超额 | 宝宝池点数与宝宝等级 | `RecalculateOne`（`player_pet.cpp:347-351`）：已用点数超过总量时 `allocated()->clear()` | **会删**（2026-10-23 已改，§12.2） | 同 D2 |
| D5 | 职业技能重数：`skill_list[].rank` | `Skill.max_rank` | `SanitizeSkillList`（`player_skill.cpp:386-392`）把超过表上限的重数压回，然后整体替换组件 | **会改写**（2026-10-23 已改，§12.2） | 新版本调大 `max_rank`、玩家修到更高重数，回滚后：<br>- 重数被压回旧上限，修习花费不退；<br>- 升回新版本也恢复不了。<br>运行期本来就按 `min(存储重数, RankLimit)` 生效（`EffectiveRankOf` → `class_skill_rules.h:82`），加载期这次压回是多余的。建议先做（待拍板第 1 条） |
| D6 | 技能的职业 / 通用归属 | `Skill.class_id` | `SanitizeSkillList`：<br>- 职业技能 rank 为 0 的整条删掉（`player_skill.cpp:377-383`）；<br>- 通用技能 rank 改写为 0（`:399-403`） | **会删 / 会改写**（2026-10-23 已改，§12.2） | - 新版本把职业技能改成通用技能（rank 存 0）后回滚：旧版本把这一条整条删掉。<br>- 新版本把通用技能改成职业技能、玩家修过重数后回滚：旧版本把 rank 清成 0；升回新版本后，又因 rank 为 0 被整条删掉。<br>这是"改表语义"，不是扩展值域。建议加约束，不改代码（待拍板第 4 条） |
| D7 | 战斗结算幂等账本：`settlement_ledger.applied` 的条数 | `kMaxAppliedRecords = 64`（`battle_settlement_ledger.h:32`） | 加载时不校验。运行期每次 `RecordApplied` 都把账本淘汰到 64 条以内（`:58`） | **会删**（运行期） | 新版本调大这个上限、某玩家累积超过 64 条后回滚：<br>- 下一局结算时会淘汰旧条目；<br>- 被淘汰的 battle_id 如果被重投，会重复发奖。<br>稳态下只有 0–1 条。建议作为发布约束（待拍板第 5 条） |
| W1 | 装备栏槽位：装备包里的 `ItemEntry.pos` | `EquipSlot` 表与容量 | `Bag::InsertItemForRestore`（`bag_system.cpp`）按三级退路放置：<br>1. 快照槽位不被本版本槽位表接受，就挪到同部位第一个空槽（`:1437-1448`）；<br>2. 挪不了，退回快照里的 pos（`:1465-1467`）；<br>3. 还不行就自动选位（`:1474-1483`），可能落进别的部位的槽。<br>Marshal 按实际 pos 重建（`bag_marshal.cpp:107`），所以必然落库 | **会改写**（挪位，不删物品） | 新版本加了装备槽位行后回滚：<br>- 装备被挪到同部位的空槽，或者连锁挪进别的槽；<br>- 升回新版本后会回到同部位的某个槽，但原来戴在哪个槽不一定能复原；<br>- 物品不丢，加成也不丢。<br>建议接受（待拍板第 6 条） |
| W2 | 坐标：`transform.location` | 导航网格，以及 `BaseScene` 表的出生点 | 每次入场都走 `EnsureValidEnterLocation`（`player_scene.cpp:263` → `scene_spawn.cpp:119-139`）：<br>- 坐标在网格上，就吸附到网格；<br>- 不在网格上，就改成出生点；<br>- 结果写回 Transform | **会改写** | 新版本扩了地图或重烘了网格后回滚：站在新区域的玩家会被拉回出生点，并随存盘落库。位置不是资产，这里防卡死优先。建议接受 |
| W3 | 当前 HP / MP：`derived_attributes_component.health` / `mana`，以及宝宝的 `health` / `mana` | 本版本算出的上限 | 加载重算时向下夹到上限：<br>- 角色：`player_attribute.cpp:813-817`；<br>- 宝宝：`player_pet.cpp:371-372` | **会改写** | 新版本的上限更高时回滚：当前值被夹低。这是消耗型状态，建议接受 |

等级上限压回原按上表单列的一行处理：它除了自身，还会经 D2、D4 删数据，并在主人等级低于出战宝宝时取消出战（`player_pet.cpp:486-488`）。2026-10-23 起改为存储等级原样携带、按 `CappedLevel` 生效，出战只在等级真的变化时取消（§12.2）。

**不适用**（原样保留）：

| 编号 | 持久值 | 依据 | 加载 / 存盘时的处理 [C] | 回滚后果 |
|---|---|---|---|---|
| N1 | 物品 `config_id`（背包、仓库、临时包、装备栏、动态包） | `ItemTable` | 还原时整份交给 `ItemStore::Insert`，不查表（`bag_system.cpp:1403-1408`）。装备栏查不到部位时（`Bag::EquipKindFor` 返回哨兵），按快照里的 pos 放回 | 原样保留，但不生效：<br>- 装备不计加成（`player_equip.cpp:476-479`）；<br>- 取不下来 |
| N2 | 堆叠数超过表上限 | `ItemTable` 的最大堆叠数 | 还原时不检查。运行期遇到超满的堆一律跳过（`item_store.cpp`） | 原样保留 |
| N3 | 包容量 `capacities` | 容量常量只是新号的初始值 | 还原时不和常量比较（`container_layout.h:159`） | 原样保留 |
| N4 | 动态包 `profile_id` | 布局工厂注册表 | 查不到时退化为自由格，id 保留（`bag_profile_registry.cpp:52-62`）。生产代码目前没有注册任何工厂 | 原样保留 |
| N5 | 装备强化等级、词条、宝石 | `EquipEnhance`、`EquipAttribute` / `EquipAttributeCap`、`EquipGem` / `EquipGemSocket` 表，以及 `kMaxGemSockets` | 只在计算加成时取 min 或跳过（`player_equip.cpp:172`、`:252` 及词条、宝石的查表处），不写回 | 原样保留，超出部分不生效 |
| N6 | 币种：`currency.values` 的下标、`blocked_types`、`debts` | `kCurrencyMax`（= 3） | - `values` 只补长度、不截断（`currency_system.cpp:60`）；<br>- 欠款不按币种过滤（`player_currency_comp.h:52`）；<br>- `SaveToProto` 整表重写，内容不变；<br>- 玩法入口按 `kCurrencyMax` 拒绝越界币种（`currency_system.cpp:604`、`:641` 等） | 原样保留，但不生效。旧版本解不了新币种的封锁，也扣不了新币种的欠款。收件箱 / 资产通道**载荷**里的新币种另见上表"收件箱、资产通道载荷"一行 |
| N7 | 任务、条件、成就、活动（后两者都是任务 scope / 类型） | `MissionTable`、任务位图 | - 新格式不查表，原样插入（`mission_marshal.cpp` 的 `Unmarshal`）；<br>- 不在位图表里的已完成 / 待领取 id 进 unmapped 集合（`mission_comp.cpp:33`、`:38`），存盘时合并写回（`mission_marshal.cpp:29-36`）；<br>- 旧格式查不到表只是不补进度（`:91`）；<br>- 条件数对不上就不推进（`mission.cpp:258`） | 原样保留。例外：GM 的 `CompleteAllMissions`（`mission.cpp:149-153`）会把位图外的进行中任务清掉。它只走 GM 路径，不在加载 / 存盘路径上 |
| N8 | 资产通道账本 | `AssetOpStream` 枚举、`kAssetOpWindowWords` 等常量 | `ValidateAssetOpLedger` 遇到新流号或常量不符，判账本损坏（`asset_op_ledger.cpp:24-26`、`:176-182`）。加载时只挂 `PlayerAssetOpLedgerInvalidComp`，存盘原样写回。裁剪只在校验通过后发生 | 原样保留。回滚期间这名玩家的资产通道 fail-closed（回 RETRY），升回后自动恢复 |
| N9 | 收件箱账本 | `kRecentCapacity`（32），以及三种可记录结局 | `player_inbox_rules::ValidateLedger` 遇到条数超限或新的结局值，判账本损坏（`player_inbox_rules.cpp:91-93`、`:133`、`:155`）。加载时只挂 `PlayerInboxLedgerInvalidComp`。裁剪（`:110`）只在校验通过后发生 | 原样保留。回滚期间这名玩家的收件箱停用，升回后恢复 |
| N10 | 神秘商人限购记录 | 商人轮次（Redis 状态）、商品表 | - 换轮清空是设计内的，与版本无关（`mystery_merchant_rules.h:145-147`）；<br>- 查不到商品只是拒绝购买（`player_mystery_merchant.cpp:560`） | 不适用 |
| N11 | 属性方案数、激活方案 | `AttributeRule.max_schemes` | 不截断，只拒绝新建（`MaxSchemes`，`player_attribute.cpp:121-124`）。激活方案指向不存在的方案也不纠正 | 原样保留 |
| N12 | 宝宝种类、数量、等级、技能 | `PetTable`、`PetRule.max_pets`、`level_cap` | - 种类查不到时跳过重算（`player_pet.cpp:332-337`），召唤、战斗快照、加点一律拒绝；<br>- 数量不截断（`MaxPets` 只拒绝新增）；<br>- 等级只升不降（`:341`）；<br>- 技能 id 不校验 | 原样保留。加点维度仍受 D3 影响。2026-10-30 起，存储等级高于本版本种类上限时按上限生效（§12.2「宝宝等级」） |
| N13 | 职业：`uint32_pb_component.class` | `ClassTable` | - 查不到时只是不补初始技能（`player_skill.cpp:186-190`）；<br>- `BaseAttributesComp` 的 `speed` / `armor` 是派生缓存，每次加载都按本版本重算 | 不适用 |
| N14 | 复活 | `ClassTable` 首行 | 只在 health 为 0 时按首行初值复活（`ApplyClassInitialAttributesOrRevive`），没有"按 id 查表失败"的分支 | 不适用 |
| N15 | buff、称号、坐骑、时装、挂机、活动日程 | — | 都不持久化：<br>- `BuffListComp` 在运行期创建（`buff.cpp:205`）；<br>- `PlayerUint32Comp` 只有 `class`，`PlayerUint64Comp` 只有 `registration_timestamp`（`player_comp.proto:39-47`）；<br>- `AfkComp` 是纯运行期组件；<br>- 活动日程只读表 | 不适用 |

技能 id 查不到表、越界包类型这两项，见上表第 5 批的两行。

**与版本无关的删除**（坏档自愈），不纳入回滚门禁：
- 背包：`size` 为 0 的条目、无效 guid、同包内重复 guid、物品数超过快照容量（`InsertItemForRestore` 末尾的 DROPPING）、动态包 `bag_id` 重复；
- 技能：技能 id 为 0、重复的技能 id；
- 宝宝：出战指向不存在的宝宝；
- 冻结层的未知字段：`BagAllData` / `DynamicBagData` / `ItemEntry` / `QuestAllData` / `QuestScopeData` 这几层由 marshal 重建，未知字段不保留。这是 R1 的设计内行为，生成器禁止给它们加字段。

**附注**：第 5 批说"消费侧按 `IsActiveSkill` 只认生效的项"，口径与实际一致，但实际写法不同：
- `HasSkill`、技能目录 `BuildSkillBook`、战斗快照、被动加成各自用 `FindSkillRow == nullptr` 跳过；
- `IsActiveSkill` 目前只有 `skill_test` 调用；
- `player_skill.cpp` 那段注释的措辞可以顺手改准。

**待负责人拍板**（会删、会改写的项要不要改代码）：2026-10-23 用户要求按最标准的方式做完，各条处理结果见每条末尾的「结果」，改法见 §12.2。

1. **D5，建议先做，改动小**：
   - 删掉 `SanitizeSkillList` 的加载期压回，存储重数原样携带，按表上限生效；
   - 同时确认修习入口 `PlanUpgrade` 在存储重数 ≥ 本版本上限时拒绝，不会回绕；
   - 配一条回滚用例。
   - 结果：已做（未编译）。`PlanUpgrade` 对存储重数 ≥ 上限本来就一步不走、不扣钱，没有另加守卫。
2. **D1 / D3**：建议把查不到表的加点维度改为"保留、计入已用、不生效"。
   - 代价：表里真删了维度之后，点数不会再自动返还，要靠洗点（需确认洗点会清掉这些维度）或一次性迁移。
   - 不推荐"保留但不计入已用"：旧版本里玩家会把这些点挪到别的维度，升回后正好触发 D2 / D4 整池返还。
   - 结果：按"保留、计入已用、不生效"做了（未编译）。与原建议的差别：洗点**不清**未知键（它们不归本池，清了就是旧版本删掉新版本的数据）；维度真被下线时，玩家留在那里的点一直占着，所以下线维度要配迁移，或保留表行（§12.2 约束）。
3. **D2 / D4 与等级上限一行同批拍板**，二选一：
   - (a) 携带超额、按本版本总量生效。要先定义"超额时哪些点生效"，比较复杂；
   - (b) 不改代码，作为发布约束：调大属性池或宝宝池点数、降低解锁等级、提高等级上限的版本，不可回滚 scene。
   - 推荐 (b)。项目尚未上线，不阻塞。
   - 结果：选了 (a) 的最简形态——超额的池**整池**不生效（不定义"哪些点生效"），玩家可以免费洗点；等级上限同批改为原样携带。理由：(b) 靠发布单人工守卫，而混版本跨 zone 迁移同样会触发，不只发生在回滚时。
4. **D6**：建议不改代码，在 §13 第 7 条评审清单加一条约束：已发布的技能 id 不改职业 / 通用归属，要改就换新 id。与"删除的技能 id 不得复用"并列。
   - 结果：代码也改了（rank 0 的职业技能保留、等同未学，通用技能的 rank 原样保留），数据不再被删；约束照样保留，因为归属一改，同一条存档在两个版本里的语义仍然不同。
5. **D7**：建议不改代码，作为发布约束：调大 `kMaxAppliedRecords` 的版本不可回滚 scene。
   - 结果：照建议，不改代码，保留发布约束。
6. **W1–W3**：建议接受，不纳入回滚门禁。理由：物品与资产不丢，位置和当前 HP / MP 不是资产。W1 若要求严格复原槽位，另立一批，改为"快照槽在容量内且空闲时原位占住、不生效"。
   - 结果：照建议接受，不改代码。

### 12.2 持久值域落码（2026-10-23～24，Claude；Codex 10-24 有构建与运行旁证）

> **来由**：2026-10-23 用户（负责人）确认项目未上线、存量玩家数据可以清空，要求按最标准的方式做完：数据强一致、代码可读、可水平扩展。据此把 §12.1 的 D1–D6 与等级上限一行改掉，不写存量迁移；D7、W1–W3 按 §12.1 的建议不改代码。代码已随整仓快照提交入库（`5de132e2`、`a3c9b9b7` 等）。
> **运行证据（旁证）**：Codex 2026-10-24 在含这批改动的共享工作区实际构建 `game.sln`（0 错误），并全量运行 bag_test（708 条）、skill_test（76 条）、turn_battle_engine_test（287 条），均 0 失败，这批新增用例全部在列且通过（PROGRESS「2026-10-24 07:38 UTC」「07:43 UTC 后继实际结果」两条；原始 gtest XML 在 Codex 证据目录 `cpp-current1884-post-full-gtests-evidence-r1/run-20261024T071358902Z-25be95de/`，本会话 10-30 按用例名逐条核对）。这不是本主题专门的验证：构建前没有封存源快照，也没有做真实回滚演练；10-29 之后别的会话又改过 `player_battle.cpp`、`class_skill_test.cpp`，并新增了一条任务等级用例。最新状态的复跑清单见 [交接 §13.2](../handoff/mixed-version-persist-handoff-20261019.md)。

**统一规则**：
1. 加载、重算、存盘都不改写这些持久值。会改它们的只有玩家主动的操作（加点、洗点、修习）和 GM。
2. 本版本解释不了的内容一律不生效，也不能让玩家比合法玩家更强。修改类操作不会先把它们删掉再执行。
3. 生效值是"存储值 + 本版本配置"的纯函数，每个 scene 节点各自算出同一个结果：不需要跨节点协调，也不引入全局可变状态。

规则的权威出处：
- 加点：`attribute_allocation_rules.h`，纯规则 `StoredPoolAllocation` / `PoolAllocationEffective` / `RemainingPoints`，角色与宝宝共用；
- 等级：`player_level_rules.h` 的 `CappedLevel` 及其头注释。

**逐项**：

| 项 | 改法 [C] | 回滚 / 混版本后果 |
|---|---|---|
| 等级上限 | - `playerlevel::CappedLevel` 取代 `ClampToMaxLevel`：0 按 1，超过 `kMaxLevel` 按上限。<br>- 加载不再压回，超限只打 WARN（`metric=level_above_cap`）。<br>- 玩法读点全部改走 `CappedLevel`：属性的 `PlayerLevel`、宝宝的 `OwnerLevel`、技能、装备穿戴、战斗快照、接任务的等级事件、技能伤害公式、回血 buff。<br>- 直读 `LevelComp.level()` 只留在四处：加载器、存盘、`GmSetLevel`、管理端 dump。 | - 存储等级原样往返，旧版本按自己的上限生效，升回新版本后恢复。<br>- `GmSetLevel` 覆写超上限的存储等级时打 WARN（GM 只在 dev / test 开放）。 |
| D1 / D3 | - 删除 `SanitizeSchemes` 与 `SanitizeAllocations`。<br>- 本版本表里属于该池、且属于该归属方的维度是"已知维度"；其余键是"未知键"：原样携带、永不生效、计入已用。<br>- 角色有多个池时，未知键对每个池都扣（偏保守）。<br>- 洗点只清本池已知维度，未知键原样保留；本池已知维度为 0 时回"无可洗"，不扣费。 | 新版本加的维度在旧版本里不生效，占着的点也挪不走；升回新版本后原样生效。 |
| D2 / D4 | - 删除 `ConvergeOverAllocation` 与宝宝超额时的 `clear()`。<br>- 池生效 ⇔ Σ已知 ≤ 本版本总量（按 `CappedLevel` 算）且单项不超上限。<br>- 不生效时：<br>&nbsp;&nbsp;· 已知维度按 0 参与二级属性、面板 value 与战斗快照；<br>&nbsp;&nbsp;· 剩余点为 0，加点回"点数不足"；<br>&nbsp;&nbsp;· 洗点免费（`metric=allocation_reset_free_ineffective`）。<br>- 加载时对不生效的池和未知键各打一条 WARN（`metric=allocation_not_effective`）。<br>- HP / MP 的旧上限、新上限都按生效值算：从不生效状态洗点，HP / MP 不变。 | - 新版本扩点或提高等级上限后回滚：超额的池在旧版本里整池不生效（数值偏低，不会偏高），数据不丢；升回后原样生效。<br>- GM 降级、改表缩点同理：不再自动返还，改为免费洗点。 |
| D5 | - `SanitizeSkillList` 不再按 `max_rank` 压回；生效重数 = min(存储重数, `RankLimit`)。<br>- 存储重数 ≥ 上限时修习由 `PlanUpgrade` 停住，不扣钱。<br>- 技能目录的 `rank` 仍是存储重数（它也是 `expected_rank` 的比较口径），超上限时 `next_rank_cost` 为 0、阻断码为满重。 | 存储重数原样往返，升回后恢复。 |
| D6 | - 不再删除 rank 0 的职业技能，也不再把通用技能的 rank 清成 0。<br>- rank 0 等同未学：修习时复用这一条，不另加条目。<br>- 初始技能补发把 rank 0 的种子原地补到 1 重，条目位置和未知字段不动。<br>- 技能书"已拥有"列表改为显式投影：只填 `skill_table_id`，职业技能再填 `rank`；不再把条目原样（含未知字段与内部 id）发给客户端。 | 数据不再被删或改写。 |
| 宝宝出战 | - `RecalculateAll` 只在 `kLevelChanged` 时因主人等级低于宝宝等级而取消出战；`kLoad` 不写 `active_pet_id`。<br>- 运行期召唤与战斗快照照旧按主人的生效等级拦截。 | 出战状态不会因为加载而被改写。 |
| 宝宝等级 | - 新增 `PetEffectiveLevel`：宝宝存储等级按本版本种类上限 `PetTable.level_cap` 封顶（2026-10-30 评审后补，**未编译、未运行**）。<br>- 总点数、二级属性、列表与战斗快照的等级、召唤 / 参战时"主人生效等级 ≥ 宝宝生效等级"的比较都读它；成长的只升不降仍按存储等级。 | 宝宝存储等级只升不降，可能高于本版本上限（新版本调高上限后回滚、交易带入）：超出部分原样携带、不生效，宝宝不会比本版本的合法宝宝更强。 |

**仍须遵守的约束**（违反时，携带下来的值会在 id 复用后自动生效，或者两个版本对同一条存档的解读不同）：
- **技能**：`Skill` 行 id 删除后不得复用；已发布技能 id 不改职业 / 通用归属，要改就换新 id；`max_rank` 只增不减，要下调须配显式迁移（返还口径另定）；每个 `skill_table_id` 在列表里至多一条（去重保留首条仍是加载期的坏档自愈）。
- **加点维度与池**：`AttributeDimension` 的 id 发布后不复用、不改 `pool_id`；`AttributePool` 的 id 与 `owner_type` 不改。要下线一个维度时，或者保留表行，或者接受"占点、不生效"——由于池里有不生效的点，玩家可以免费洗点。
- **按等级的规则**：门槛与总量必须随等级单调不减；新增"低于 X 级"这类非单调规则时，要单独评估超上限的存储等级。
- **等级上限与经验系统**：下调 `kMaxLevel` 不再自动收敛，需要显式迁移。将来的经验系统判断能否升级时用存储等级（≥ `kMaxLevel` 不再升），不得写回 `CappedLevel + 1`。
- **对象本层新字段**：`AttributeScheme`、`PetInstance`、`PlayerSkillComp` 本层若新增与加点或重数语义耦合的字段，按 R5 走破坏性变更。这一批不加 `HasUnknown` 守卫，因为它分不出新版本字段与退役字段的残留，会把对象永久冻结（§4.4）。

**已接受的代价**：
- **池不生效时洗点免费**：混版本窗口里，玩家在新版本把扩出来的点加满，再跨到旧版本节点，那里池不生效，可以免费洗一次、重新分配；回到新版本再补剩余点。每个混版本窗口都能重复一次，相当于省掉洗点金币，但数值不会变强（2026-10-30 评审第 3 条）。要堵住就改回照常收费，代价是改表缩点、GM 降级后，玩家要为系统造成的不生效付费。

**客户端可见的语义变化**（AGENTS §12：两版服务器与客户端同批对齐）：
- **属性面板与宝宝列表**：`allocated` 仍是存储值，`value` 只含生效的已分配点；池不生效时 `remaining = 0`、`reset_cost_gold = 0`。
- **技能书**：`skill_list` 改为显式投影；目录里的 `rank` 可能大于 `max_rank`；`player_level` 下发的是生效等级。
- **等级展示**：战斗快照、属性面板与宝宝列表下发生效等级（宝宝按种类上限封顶）；Go 侧好友 / 队伍展示读存储等级，回滚窗口里两边可能不一致。不在 Go 里复制上限常量，免得多出第二份真相。
- **proto**：只改了注释（`player_skill_comp.proto`、`player_attribute.proto`、`player_pet.proto`），需要 Codex 用 no-unity 配置重新生成。

**Java 版**：待做。Java 的 `PlayerLevels.clampStored` 同样会在加载时压回等级；加点、技能也要按同一口径实现，见交接 §13.2。

---

## 13. 风险与待负责人拍板

1. **"只写改动列"改为"超前延后、倒退拒写、未盖戳拒写、其余整行写"**（§6.1），请确认。
2. **option 的读取方式**：推荐生成器动态读取，不升级外部模块。
3. **C3 / C4 先 warn 后 enforce**。切换的前提是 §8.1 第 5 步的真实升世代演练通过。enforce 的代价是被判截断的玩家本次在线进度不落盘。
4. **引导期禁止升世代**，时长取决于全网换版的速度。
5. **冻结清单、R2 / R4 / R5，以及 §4.4 的守卫**会改变今后的开发习惯：
   - 背包布局层、任务容器层不能再直接加字段；
   - 带新属性的物品，在旧版里不会被合堆；
   - 带新字段的欠款，在旧版里不允许再挂新欠款；
   - 带新属性的可叠加物品，在旧版里不能按 config 消耗、交易、带进战斗（第 6 批，fail-closed，发布单写明可用性降级）。
   这些需要负责人认可。
6. **残余风险**：
   - 超出守卫范围的语义耦合；
   - 回档功能实现时必须重置 `schema_gen`；
   - 误部署机制前的 scene 期间，在线进度会丢；
   - warn 期间截断只告警、不拦截；
   - owner_epoch 为 0 的兼容路径没有 C3 保护；
   - 第 6 批的版本错位都不记终态，收口靠升级走完：资产通道的 RETRY 没有次数上限，截止为 0 的入账流（邮件、交易交付）会一直 PENDING 到 scene 升级；这类行不要人工终结（入账没有 Abort 栅栏，人工终结还会在流上留下永久的洞，累计超窗要抬纪元，04 §4.38）；收件箱版本错位停用本域，同域排在后面的处置一起延后，停用只在发生时打一次日志（周期补打见交接 §14 待办）。
7. **评审清单**（凡是修改持久 proto 的 PR 都要过）：
   - 生成器是否追加了世代；
   - 是否在主干上发号；
   - 冻结类型有没有被加字段；
   - 有没有字段迁移（R4）；
   - 有没有语义耦合字段（R5）：等价判定、合堆、合并、逻辑重置、跨实体引用。按数量扣除第 6 批已守（带新字段的单位在旧版里不计、不扣），但会被普遍写入的 `ItemComp` 新字段要分两版发布（先声明、后写入），且不得改变 `size` 的计数含义；需要跨纪元保留的流账本字段、按 guid 托管或改写实例时才生效的新字段（不可交易、封印类）按破坏性变更处理（§4.4、R5）；
   - 有没有改变参与按数量扣除、按 guid 扣出的包集合（例如新包类型参与资产扣出）：旧版看不到新包里的单位（越界包类型只原样携带，`PlayerBagsComp::unsupportedBagTypes`），会把新版能满足的扣除判成确定不足、记终态；按破坏性变更处理；
   - 删除持久消息的字段时，写 `reserved` 并在台账登记 `retired=<消息全名>#<字段号>`；漏登记的话，存档里的残留字节会被当成更新版本的字段，带它的物品、欠款永久不能合堆、合并、按数量扣除（§4.4）；
   - 有没有扩展持久值域，含配置表新增行、代码常量上限（例如提高等级上限）、新枚举值（例如新币种）：发布单要写明"scene 不可回滚到此前版本"。不改持久 proto 的 PR 只要改变了持久值的合法范围，同样要过这一条。配表的 id 复用与单调约束见 §12.2；
   - 新币种另守三条：`currency_type.proto` 只增不删、不复用、从 0 连续（C++ `CurrencyTest.CurrencyTypeEnumIsDerivedFromTheProtoTable` 与 Go `currencytype` 的连续性测试守住）；Go 侧全量升级之后才滚 scene；scene 全量之前 GM 不对新币种下处置，交易、邮件不发放新币种（§12 载荷行）；
   - 新字段被旧写方原样携带时，语义是否仍然成立。
8. **已定（2026-10-22 按推荐落码，未编译；第 4 批落码时发现，2026-10-20 登记）：恢复工具的 `schema_gen` 口径**。原问题：`save_barrier_restore` 重建屏障时写 `schema_gen = max(1, 两行戳的最大值)`，且不降低 hash 里已有的值（§5.4）；而 C3 / C4 的加载世代取两表戳的**最小值**（§3.3）。两行戳不同（单表进了死信，§6.2 末段），或 hash 已被一次没落进 SQL 的写抬高时，恢复之后冷加载出的数据加载世代 < hash：login 快路径与 scene 第一次存盘都会判倒退，enforce 下这名玩家既进不了游戏、也存不进盘 —— 而"停写后用 `save_barrier_restore` 从 SQL 重建屏障"恰恰是 §8.4 / README 给倒退的处置。
   - 推荐：把"从 SQL 恢复"按回档对待（§5.3 末段"回档必须显式重置 `schema_gen`"）：恢复时写 `schema_gen = max(1, 两行戳的最小值)`，并忽略 hash 里已有的值（运维此时已明确以 SQL 为准，恢复本身要求停写、父缓存不在）。代价：两行戳不同的那名玩家，较高那张表里新世代字段的数据会在下一次整行写时按旧世代口径重写 —— 这正是"以 SQL 为准"接受的损失，需在运维手册写明。
   - 现状（2026-10-22）：按推荐落码，未编译。`restore.go` 的 `restoredSchemaGen` 取两行最小值（0 按 1），Lua 不再保留 hash 旧值、旧值格式损坏也不再拒绝（只是索引），用例 `TestSchemaGenRestoreUsesSQLMinimumAndOverwritesIndex`、`TestSchemaGenRestoreOverwritesMalformedIndex` 与真库集成用例期望同步改；运维手册 `docs/ops/mysql-backup-pitr-runbook.md` 追加口径变更小节。原实现（`max`、只升不降，`da0f1822c`）作废。
9. **切 enforce 之前须落地（接手会话复核，2026-10-22 登记）：前任实体的存盘元素活得比实体长**。触发链：实体 X1 被不存盘销毁（`DestroyDeposedPlayer`，例如 `kRejected` 收尾）时 Redis 客户端里还有它没落地的存盘 A → 同落点重连在同一代次下加载出 X2，读到的是 A 之前的父快照 → A 随后落地（写下 X1 的令牌与输出世代）→ X2 第一次存盘令牌不同、拿较旧的加载世代去比，升世代窗口里判倒退，enforce 下 X2 此后永久拒存。这本身也是既有的"读到旧父快照后覆盖新写入"隐患，C3 只是把它在升世代窗口里变成了拒存。
   - 修法二选一：(1) `MessageAsyncClient::AbandonSaves(key)`，由 `DestroyDeposedPlayer` 调用：删排队值；在途元素失败不再重试，回包 1 / 2 只执行 `committed_callback`（SQL 投递仍要发），不发布 `save_callback_` 与 warned / rejected 回调。(2) `HandleEnterGameNode` 走 `kLoadRequired` 时，若该 key 还有未落定的存盘，推迟 `AsyncLoad` 到它落定之后。
   - 现状（2026-10-22）：**按修法 (2) 落码，未编译**。做在 `MessageAsyncClient::AsyncLoad` 内部而不是节点层入口（生产调用方在 `role_admission_system.cpp`，所有加载入口一并受益）：同 key 还有本客户端未落定的存盘（在途或排队）时，加载挂进 `loads_awaiting_saves_`，`RetryDuePending` 每拍检查，存盘落定（成功、被拒、或被更新值取代后落定）再发 GET；挂起期间 `HasPendingLoad` 仍为真，重复调用并进同一挂起项，别的 key 不受影响，`[RedisSystem]` 快照行多一个 `loads_awaiting_saves`。存盘语义不变，前任那份照常落地或被拒，新实体读到的就是落定之后的状态。只覆盖同一节点（同一客户端）的前任；跨节点的前任由 owner 守卫挡住。用例 `CurrencyTest.RedisLoadWaitsForUnsettledSaveOfTheSameKey`。代价：前任那份存盘若因 Redis 持续报错一直重试，新实体会一直进不来——此前它能进来但同样存不进盘，现在改为进不来（fail-closed）。

10. **待拍板（第 6 批，2026-10-30 登记）**：
    - **资产通道遇到本版本查不到表的物品 / 宝宝种类**（§12 载荷行）：推荐与新币种同口径——物品、宝宝表行只增不删（下线改开关），Go 各入口按同源配置表校验 config 存在，之后 scene 把"表里查不到"改判为版本错位（RETRY，不记终态）。前提是表行规则先定：如果允许热更删行，"新行"和"已删行"在旧 scene 上分不开，改判 RETRY 会让已删行的入账永远 PENDING（战斗结算对已删行的处理是剔除该项、记 ERROR 交人工，C-2）。在拍板之前维持现状（终态拒绝，交易转人工、不丢资产）并守发布次序。
    - **GM 对新币种的处置是否改为"按 ID 透明执行"**：封锁、解封、挂欠款对币种只做按 ID 的集合或映射操作，旧 scene 照写也与新版本语义一致，可以不停用处置域（需要给收件箱执行器开只认 ID 的窄口子，内部调用方仍按非法参数拒绝）。现状是回 `kVersionSkew` 停用本域，同一玩家后面的处置一起延后到实体重建。推荐维持现状：发布纪律已要求 scene 全量之前不对新币种下处置；改为透明执行要放宽三个原语的契约，将来原语一旦按币种做事（例如查余额），这条前提会悄悄失效。
    - **新币种的启用开关**：把"二进制认识"与"运营已启用"分开（Go 入口判 `IsKnown(v) && enabled(v)`，`enabled` 来自部署配置白名单，上线顺序与收件箱 `EnqueueTypes` 一致：scene 全量 → 改配置开启）。推荐在第一次真正新增币种时再做（YAGNI）；在那之前靠第 7 条的发布次序。

---

## 14. 证据索引

- **C++**：
  - `cpp/libs/services/scene/player/system/`：`player_lifecycle.cpp`（`MarshalPlayerForSave`、`SavePlayerToRedisImpl`、`OnDbTaskDispatchSettled`、`InitPlayerFromAllData`、`HandlePlayerSaveRejected`）、`player_data_loader.h`（生成产物，模板为 `player_data_loader.h.tmpl`）、`player_database_loader.cpp`、`player_database_1_loader.cpp`、`mission_marshal.cpp`、`bag_marshal.{h,cpp}`、`player_skill.cpp`、`player_attribute.cpp`、`player_pet.cpp`；
  - `cpp/libs/modules/currency/{comp/player_currency_comp.h,system/currency_system.cpp}`、`cpp/libs/modules/mission/system/mission.cpp`、`cpp/libs/modules/bag/item_store.cpp`；
  - 资产通道账本 `asset_op_ledger.cpp`；
  - `cpp/libs/modules/snapshot/snapshot_system.cpp`（其中的 `kSnapshotSchemaVersion` 是快照条目信封的格式版本，与本文的世代无关）；
  - `cpp/libs/engine/infra/storage/redis_client/redis_client.h`（`kSaveWithCommitBarrierLuaScript`、`ScriptSlot`、`OnSaved`、`OnReconnected`）、`cpp/libs/services/scene/core/system/redis.cpp`；
  - 测试：`cpp/tests/` 下的 `bag_test`（`player_feature_persistence_test.cpp`）、`currency_test`、`cross_zone_test`、`save_seq_test`、`db_task_dispatch_test`。
- **go/db**：
  - `go/db/internal/kafka/`：`key_ordered_consumer.go`（`handleDBReadOp`、`publishDBReadResult`、`handleDBWriteOp`、`runDBOp`、`handleTask`、`quarantineOrderingConflict`）、`player_row_fence.go`、`player_cache_publish.go`、`store_failure.go`、`placement_route.go`（`failRead`、`retryBudgetExempt`）、`save_outbox_note.go`；
  - `go/db/db.go`、`go/db/internal/logic/pkg/proto_sql/db.go`（`assertSchemaUpToDate`）、`go/db/internal/saveoutbox/{generation,scripts}.go`、`go/db/internal/barrierrestore/scripts.go`。
- **proto2mysql**：v0.2.0 的 `proto2mysql.go`、`pbconv/convert.go`。
- **login**：
  - `go/login/internal/logic/pkg/dataloader/`：`ensure_player_all_data_async.go`、`parent_commit_barrier.go`（`beginParentLoad`、`isUnownedLoginSeed`）、`new_player_seed.go`；
  - `go/login/internal/logic/clientplayerlogin/`：`player_class_backfill.go`、`entergamelogic.go`（`notifyEnterGameFailed`）。
- **其他**：`tools/merge_zone/player_rows.go`、`go/data_service/cmd/debug_import/bundle_import.go`。
- **proto**：`proto/common/database/{player_cache,mysql_database_table,bag_quest_mail_data}.proto`、`proto/db/proto_option.proto`、`proto/message_id.txt`。
- **Java**：`D:\luyuan\xuanming-server-mmo\game-player-store\src\main\java\com\game\player\store\{PlayerStore,PlayerMapper,PlayerStateRow}.java`。
- **关联文档**：`db-write-fencing-mysql.md`、`db-task-delivery-and-retention.md`、`db-task-store-unavailable-deferral.md`、`ecs-component-access-rules.md`、`bag-instance-layout-split.md`、`data-overwrite-rollback-audit-20261018.md`、`published-contract-compatibility-20261017.md`。

---

## 15. 修订记录

### v0.1

首稿。核心做法：从加载原件嫁接未知字段；Lua 比较输出世代；go/db 按列掩码写。

### v0.2（第一轮评审）

| 评审项 | 修正 |
|---|---|
| S1 嫁接会复活已删除元素 | 改为"未知字段跟着对象走"，加冻结规则和往返一致性测试 |
| S2 截断被洗白 | C3 比较加载世代；新增 login 门禁 C4 |
| S3 零值不序列化 | C1 改为按世代号判断 |
| S4 退役字段 | C1 不再看未知字段 |
| S5 回滚承诺 | 改为有条件 |
| M1–M9 | 返回值、比较函数、种子不盖戳、恢复工具、`s < r` 拒写、R4、C2 立即失败、C0 与主干发号、LogQL |

### v0.4（第 1 批落码，2026-10-19）

负责人通过 §13 第 1–5 条后实施第 1 批，回写以下调整：

| 项 | 调整 | 原因 |
|---|---|---|
| 字段级 option | `OptionPersistProjection` 改为 `OptionPersistUncarried`（600040），含义放宽为"存盘时不原样携带"；只经此类字段可达的类型不受 R1 约束 | 压测探针、合服标记是每次重新生成或清空的字段，不是投影；冻结它们的类型没有意义，还会挡住正当的扩展 |
| 冻结清单 | `QuestEntry`、`MailAllData` / `MailEntry`、`PlayerStressTestProbe`、`PlayerMergeStateComp`、`Vector3` / `Rotation` 不再显式冻结 | 前四个改为标注引用字段；组件 proto 可能被客户端使用，不加服务端 option |
| 引导期 | 新增字段并入世代 1，不阻断开发 | 世代号依旧不超过 1，目的不变；生成器拒绝阻断会卡住其他会话正在做的持久字段 |
| 台账格式 | 逐字段记录，不再用基线哈希 | 逐字段才能判断改类型、删字段、复用字段号 |
| 可携带集合 | 只从玩家表、以及玩家表到达不了的冻结根出发 | 深处冻结类型作为根只为受 R1 约束，不应成为往返测试的覆盖对象 |

### v0.5（第 3a 批落码，2026-10-19）

| 项 | 调整 | 原因 |
|---|---|---|
| 统一入口 | 只有头文件 `player_persist_marshal.h`，没有 `.cpp` | 调用方分属 scene 与 modules 两个库；放在头文件里不用在两边工程登记源文件，也不新增库间依赖（它转调的 `PlayerAllDataMessageFieldsMarshal` 本来就是头文件内联） |
| `MissionListComp`（L3） | 运行时本来就是 PB；`Unmarshal` 照旧逐 map 合并已知字段，只额外并入首份 scope 的本层未知字段 | 整份替换会破坏重复 scope「首份获胜」的合并语义；本层其余字段（`type`、旧位图）由 Marshal 重写，不需要携带 |
| 合堆守卫（§4.4） | 判定收进 `ItemStore::IsIndependentInstance`（`has_equip()` 或带未知字段），`CanStack`、`MeasureFreeRoomPerConfig`、`IsMergeCandidate` 共用 | 预检与提交各写一份判定会分叉：reserve 比 commit 乐观，就是「预检通过、写到一半放不下」 |
| 欠款携带（L2） | `PlayerCurrencyComp::SaveToProto` 改为 `[[nodiscard]] bool` | 携带的字节解析失败（只可能是内存被破坏）时必须整份拒存，不能写出缺字段的欠款 |
| 往返测试注入 | map 字段走重复字段的反射视图；注入后先序列化再解析，模拟新版本写下的字节 | protobuf 35 的 map 迭代器反射接口不对外；走字节才是「新字段落进各层 UnknownFieldSet」的真实场景 |

### v0.6（C3 改为写方令牌；第 3b / 4 批落码，2026-10-19）

交接核对（[交接 §3.2](../handoff/mixed-version-persist-handoff-20261019.md)）与接手会话的只读取证（owner_epoch 的铸造与复用、OnSaved 的全部顶替路径，带行号）共同推翻了 v0.3 的"只比第一次写入"。逐个方案的结论：

| 方案 | 做法 | 结论 |
|---|---|---|
| v0.3 原文 | C++ 只在第一次存盘传真实加载世代，之后 `ARGV[9] = ARGV[8]` | 否决。以"首存已落地"为标记：首存在途时排进的后继带着真实加载世代，首存落地后被判倒退，enforce 下永久存不进盘（S-A 重现）。以"已发起"为标记：首存未提交就被顶替时一次都没比较。"回包丢失后重发的一定是同一元素"不成立：有排队值时发出的是新版本 |
| (a) owner 前缀 | C++ 一律传真实加载世代；脚本只在 pending 的 owner 前缀 ≠ `ARGV[2]` 时比较 | 否决。前提"同一 owner_epoch 只对应一个加载实体"两头都不成立：同一实体会换代次（退出保留期间重登铸出 E+1 又派回本节点复用实体、B5 采纳 E+2），换代后第一次存盘拿真实加载世代去比自己抬高过的 hash，确定误报；同一代次也会先后加载出两个实体，漏检 |
| (b) C++ 状态机 | "仍需比较"挂在元素上、顶替时继承、发送时求值 | 否决。被丢弃的元素不执行任何回调，C++ 分不出"首存已提交但回包丢失、随后被顶替"，后继继承"仍需比较"就误报 |
| (a′) 实体基线世代 | 保留 owner 前缀规则；`ARGV[9]` 改为实体基线（初值加载世代，本实体有元素被证实提交后抬到输出世代） | 否决。首存已提交但回包丢失、同时实体换到 E+1 并排队了新值：基线没被证实抬高，新值拿加载世代比较被抬高的 hash，误报；enforce 下此后每次都没有证实提交，永久存不进盘。另需按发送时求值的共享基线单元，复杂度高 |
| **(a″) 写方令牌（采纳）** | 每个加载实体一个全局唯一令牌，随屏障写进 hash 的 `schema_writer`；脚本只在令牌不同时比较，`ARGV[9]` 永远是真实加载世代 | 上面所有情形都由"上一次屏障写入是不是本实体写的"一条规则判对（§5.2 表）。C++ 不维护首存状态，不依赖回包。代价：hash 多一个字段，`save_barrier_restore` 的字段白名单同步接受它；ARGV 增加到 11 个；字面量白名单单测同步 |

其余落码时定下的细节：

| 项 | 调整 | 原因 |
|---|---|---|
| 令牌取值 | 随机 UUID 文本，实体第一次走严格屏障存盘时在写入口懒建（`PlayerPersistWriterComp`，运行时派生数据） | 令牌不能被"同节点同玩家再加载一次"复现：entt 实体号会回收，"节点 + 玩家"在同落点重连时相同 |
| 同版本重放 | 脚本里显式 `pending ~= ARGV[3]` 才比较 | 现有同版本分支不提前返回，会落到写入 |
| 模式归一化 | 读配置时一次性归一化，非法值 LOG_FATAL 拒绝启动；存盘点再归一化一次只为单测宿主（空串），仍非法按 warn 并打 ERROR | 非法字面量进 `ARGV[10]` 会让脚本返回 error_reply、`OnSaved` 无限重试，全服存不进盘；运行期改成拒存同样是全节点存不进盘 |
| 入队前校验 | `SaveWithCommitBarrier` 用与 Lua 同口径的校验拒绝非法覆盖世代参数（返回 false = 本次存盘拒绝） | 同上：不能让参数错误变成无限重试 |
| 返回 3 的排队值处置 | 与 owner 拒绝（0）共用 `IssuePendingWithNewerGuard`；0 只看 guard，3 要 guard **与写方令牌**都相同才丢（`sameWriterOnly`） | 两处处置相同的部分不能分开写；3 多比令牌是因为 C3 只看写方自己的加载世代：同一代次下另一个加载实体的排队存盘不能被连带丢弃，否则它若是退出存盘，会被当成"退出中被拒"不存盘销毁。别把令牌比较当成冗余删掉 |
| warn 证据 `schema_regressed`（3b 评审 + 接手会话裁决） | warn 判倒退时记下写方令牌，同令牌后续写入继续返回 2；别的写方写成功不清；恢复工具清；C++ 按令牌去重计数；巡检存量作为切 enforce 门禁第三项 | 判定那次的回包丢失（重放与同令牌后继都跳过比较）会让计数永远为 0，门禁假阴性 |
| 不采纳"证实提交后改用输出世代"（接手会话裁决） | `ARGV[9]` 永远是真实加载世代 | 它要防的"别的写方插在同一实体两次写入之间"在生产中不可达（§5.2"前提"一行），却会把首存状态与回包依赖带回 C++ |
| 恢复工具清令牌（接手会话复核，可选加固） | `save_barrier_restore` 写入时 `HDEL schema_writer schema_regressed` | 以 SQL 为准之后任何写方都要重新比较；合规流程里没有在线写方，结果不变 |
| 恢复工具 `schema_gen` 改为两行最小值、覆盖旧值（2026-10-22，§13 第 8 条） | `restoredSchemaGen` = max(1, 两行戳最小值)；Lua 不保留更高的旧值，旧值损坏也覆盖 | 与 C3 / C4 的"加载世代 = 两表戳最小值"同口径：恢复后冷加载出的数据正好等于记录值，第一次加载与存盘不会被误判倒退；取最大值会让恢复（倒退的处置手段）本身在 enforce 下把玩家锁死 |
| 观测 | 逐次 ERROR 日志带 `metric=persist_schema_regression`，30 秒汇总 `[PersistSchema]`；LogQL 写成值班固定查询，**不宣称告警已配** | Loki ruler 未接线（归上线批 BK8s），与 `[OwnerEpoch]` 同一先例 |

### v0.7（第 3a′ / 第 5 批落码与对抗复核，2026-10-19）

落码后做了一次七视角只读对抗复核（编译、收件箱、合堆、越界包类型、技能、测试、文档），每条发现两名复核者尝试驳倒；确认的都已在本批修掉或写进文档。

| 项 | 调整 | 原因 |
|---|---|---|
| 欠款版本错位的结局（§4.4） | 收件箱执行器从 `kDefer` 改为新结局 `kVersionSkew`：收件箱停用本域（`reason=version_skew`），已定前缀照常确认，这一条不记账、不确认，实体重建后重拉（退出保留期内重登复用实体，不重拉） | 复核 A-1：DEFER 按本地旧副本续做、看不到服务端之后挂的 VOID；而欠款可能在会话内被还清删除，错位随之消失，续做就会把一条已作废的 GM 欠款挂上并报 APPLIED。不选"每次重拉"：每次 Fetch 让 `delivery_count` +1，几分钟内就触发毒消息告警 |
| 越界包类型的载体（§12） | 不新增独立的持久数据组件，作为 `PlayerBagsComp` 的成员 `unsupportedBagTypes`，存整份 `ItemEntry` | 与固定包同生共死，不需要单独的创建点和缺失语义，也就不触发整套同步清单；整份拷贝不会失败，`bag_marshal::Marshal` 不必改成返回 bool |
| 携带记录与 guid 唯一性（§12） | 新增 `PlayerBagsComp::ContainsGuid`（固定包、动态包、携带记录都查），资产通道入账的跨包查重改用它 | 复核 D-1：漏查会让同一实例在新包与背包里各落一份，升回新版本后两份都还原 |
| 按数量扣除（§4.4、R5、§13） | 新增"按 config 计数和扣数量"一行，标"暂无守卫"；§0 收窄"等价判定"的说法 | 复核 B-1：资产通道数量扣出、强化材料、镶嵌宝石、战斗消耗都不看 `IsIndependentInstance`；文档原先读起来像已经守住 |
| 其他持久值域（§8.3、§12） | §12 新增"其他按配置表解释的持久值：未核对"一行，§8.3 的 scene 回滚门禁对这些值域不随第 5 批解除 | 复核 F-1：§8.3 原文引用了不存在的"待核对行" |
| 技能表行 id（§12） | 删除的技能 id 不得复用 | 查不到表的技能会一直留在老玩家数据里，复用后会自动生效 |
| 第二轮复核（2026-10-20） | 重拉时机写准为"实体重建后"；§0、R5、§4.4 改正"纪元重置是拒绝执行"的说法；§12 两行现状标为"第 5 批前"，未核对行扩到代码常量并补线索，等级上限单列一行，新币种的终态问题记入载荷行；§13 评审清单扩到不改 proto 的值域变更 | 续跑的测试、文档、修正复核三个视角确认 11 条文档与注释不一致（无代码缺陷），另确认等级上限压回（O-1）一处删数据点；新币种被旧版记终态（O-3）由本会话读码核实 |

### v0.8（第 6 批落码：退役字段残留、按数量扣除守卫、新币种，2026-10-23～30）

2026-10-23 用户确认项目未上线、要求按最标准的方式做完。先四路只读取证（数量接口、调用方、币种值域、重试语义），出综合规格，再三路对抗攻击（一致性、行为、运维与扩展）；裁决一步当时因额度失败，2026-10-30 由本会话逐条裁决并补齐。Codex 10-24 的全量运行旁证覆盖 10-23 落的部分（§9）。

| 项 | 调整 | 原因 |
|---|---|---|
| 退役字段残留（§4.4） | 新增谓词 `CarriesNewerVersionFields`：只认台账没有退役的字段号；生成器输出 `kRetiredFields` / `IsRetiredField`；三处守卫统一用它，`HasUnknown` 删除 | 只看"有没有未知字段"会把带退役残留的物品、欠款在所有版本上永久当成新版本数据，永不能合堆、合并、扣除 |
| 按数量扣除（§4.4、R5、§13） | 由"暂无守卫"改为已守：`IsCountableByConfig`、三态 `CheckQuantities`（不足优先）、`DrainStacks` 过滤，四个调用方按三态分支；版本错位复用 `kFeatureUnavailable`(1006)，不改 Tip.xlsx | 复核 B-1；1006 不在 fault 表里，资产通道现有 RETRY 路径产生不了它，"RETRY + 1006"在 Go 行上唯一对应版本错位 |
| 资产扣出的判定顺序（10-30） | 版本错位排到按 guid / 宝宝的确定性校验之后 | 攻击：错位先回 RETRY 会让一个同时带坏 guid 的托管包重投到截止，占着流的未决窗口 |
| 聚宝斋托管阶段撤单（10-30） | 卖家对寄售、材料订单、拍卖在托管阶段也可撤单，复用 `ExpireCommerceEscrowDebitsTx` → Abort | 攻击：材料订单托管腿没有截止，6a 把终态拒绝改成 RETRY 之后没有任何出口 |
| `HasAll` 删除（10-30） | 没有调用方，bool 会把版本错位并进"不足" | 留着是陷阱：新调用方用它就回到记终态的老路 |
| 新币种（§12） | 取值表唯一来源 `currency_type.proto`；Go 各入口按同源表校验；scene 收件箱回 `kVersionSkew`、资产通道第 8b 步回 RETRY | 旧 scene 以前分不出"新币种"和"写错的币种"，只能记终态；Go 入口拦住写错的值之后，不认识的值只可能来自更新版本 |
| 日志关键字 | 统一为 `metric=persist_version_skew where=asset_debit / asset_currency / equip_enhance / equip_inlay / inbox_currency` | 规格原写三个不同关键字；一条 LogQL 抓不全同一类事件 |
| 未采纳 / 改为待拍板 | 越界包类型的携带记录不参与数量判定（只在将来新包类型参与资产扣出时触发，列入 R5 评审）；`GetTotalItemCount` 改名（约 100 处测试调用，注释已写明新语义）；GM 新币种透明执行、新币种启用开关、新物品 / 宝宝表行改判（§13 第 10 条）；收件箱版本错位停用的周期补打与 PoisonSuspect 指引（相关文件当时正被别的会话改，记为交接 §14 待办） | 都不影响数据一致性；要么需要负责人先定规则，要么改动面与在途工作冲突 |

### v0.9（§12.1 持久值域落码：D1–D6 与等级上限，2026-10-23～30）

2026-10-23 用户要求按最标准的方式做完，设计先经四路只读精读、四路对抗复核，再分三路落码（技能、加点、等级），最后在 10-30 补做两路只读评审。Codex 10-24 已有构建与运行旁证（§12.2）。结果见 §12.2。

| 项 | 调整 | 原因 |
|---|---|---|
| 统一口径 | 加载、重算、存盘不改写持久值，原样携带；生效值在运行期按本版本配置算出；本版本解释不了的内容不生效、不让玩家变强 | 第 5 批"原样携带、不生效"的做法推广到其余值域。加载期破坏性修正在回滚和混版本跨 zone 时都会触发，靠发布单人工守卫拦不住 |
| 加点：未知键 | 计入已用、永不生效；洗点不清 | 复核否掉了两种做法：<br>- "方案里有未知键就冻结整套方案"：值为 0 的未知键在客户端全量提交里很常见，会大面积误冻；<br>- "洗点清掉全部键且免费"：等于旧版本在鼓励删除新版本的数据。 |
| 加点：生效判定 | 生效只看已知维度：Σ已知 ≤ 总量，且单项不超上限；未知键只扣剩余点 | 复核指出，未知键若也参与生效判定，宝宝二期加第二个池后，旧版本会让整只宝宝的加点失效，惩罚远超"不比本版本强"所需 |
| 加点：超额 | 整池不生效、免费洗点，取代"整池删除返还" | 不定义"超额时哪些点生效"，规则保持一句话；结果只会偏弱。代价：混版本窗口里，玩家可能多拿到几次免费洗点（已接受） |
| 等级 | `CappedLevel` 单一规则，玩法读点全部改走它；宝宝出战只在 `kLevelChanged` 时取消 | 五份重复的"等级取法"收成一条；运行期召唤与战斗快照本来就按等级拦截，加载期取消出战是多余的落库改写 |
| 技能 | 加载期只去 0 与重复项；rank 0 的种子原地补；已拥有列表改为显式投影 | 运行期本来就按 `EffectiveRankOf` 与 `IsUsable` 生效，加载期压回、删除、清 0 都是多余的。原地补种子是为了不靠"先删 rank 0 条目"来掩盖补发缺陷；显式投影避免把未知字段与内部 id 转发给客户端 |
| 宝宝生效等级（10-30 评审后补） | `PetEffectiveLevel` = min(存储等级, `PetTable.level_cap`)，除成长比较外的宝宝等级读点都改读它 | 评审指出：宝宝存储等级只升不降，回滚或交易带入后可能高于本版本种类上限，原先照常按存储等级生效、能把上限之上才有的点加满，比本版本任何合法宝宝都强，与主人的 `CappedLevel` 口径不一致 |
| 不加 `HasUnknown` 守卫 | 技能条目、加点方案、宝宝实例本层的新字段按 R5 处理（§4.4 由第 6 批会话登记） | `HasUnknown` 分不出新版本字段与退役字段的残留，会把对象永久冻结。第 6 批会话计划用台账的 retired 行修正这个判定，届时可以复用 |

### v0.3（第二轮评审）

| 评审项 | 修正 |
|---|---|
| S-A C3 必现误报 | 只在首次写入时比较加载世代，重放跳过；新增 §7 回归行与 §8.1 第 5 步演练 |
| M-A | hash 只升不降 |
| M-B | 增加模式参数；存储值异常视为倒退证据，不回 error；所有写入值 ≥ 1 |
| M-C | 返回值 3 的新回调与分派；补全 3b 文件清单（`core/system/redis.cpp`、`currency_test.cpp`） |
| M-D | 补全冻结清单；增加字段级投影 option；排除 map entry |
| M-E | Carry 组件改在手写 loader 里创建；补充测试改动面 |
| M-F | 新增 R5 与 §4.4 守卫；`ResetAssetOpStreamForEpoch` 改为 `Clear()` |
| M-G | C0 改为确定性进死信 |
| L-A | 加载世代取两表最小值 |
| L-B | C1b / C2 接线 |
| L-C | 日志限速 |
| L-D | epoch=0 兼容路径列为残余风险 |
| L-E | C4 不新增 tip |
| L-F | 冻结 Vector3 / Rotation |
| L-G | 单表死信的处置 |

### 2026-10-19 历史屏障恢复工具的格式接缝实证

`save_barrier_restore` 已在手写核心落地严格3/4字段白名单与双行点锁SQL世代max恢复：历史0有效为1、已有Redis floor不降、SQL原始戳仍参与计划摘要，缺列/NULL/异常戳拒绝，保持原ACK/outbox/维护/归属上界与路由门禁。旧实现的7个失败子例已实际复现；候选22顶层/103含子项单元与CLI5零失败零跳过，vet/CLI build均0。应用7路径后751完整来源等于已验证私有GREEN，应用原件SHA `13F3F06EAB10927D3195DB365EB9DAB16390D721088F31D8E874FE0DF64BF06D`；运行原件SHA `60585D50533B5E5E7E0D0BBFD4C2E25886D90270E26ECA6964343B8F16EDF6AB`。初次RED收据解释器错误和真实native1未覆盖，另有逐子例补充解释。

这只是恢复工具兼容，不代替C3/C4实现或真实升世代/双区故障。新真TiDB部署矩阵仍待执行，既往64通过不绑定本批；Java直接SQL/BLOB路径不使用这个Redis父屏障协议，本项不代表Java整体持久补偿天然等价。运维前提与当前证据边界见 `docs/ops/mysql-backup-pitr-runbook.md` 的同日补充。

> **2026-10-22 口径变更**：上文"SQL 世代 max 恢复、已有 Redis floor 不降"已作废，改为两行戳最小值、覆盖 hash 旧值（§13 第 8 条、§15 v0.6 最后一行）；上文记录的验证证据只对旧口径成立，新口径未编译、待 Codex 重新验证（交接 §11.9）。
