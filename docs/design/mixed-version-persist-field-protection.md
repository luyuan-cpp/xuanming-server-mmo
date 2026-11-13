# 新旧版本混跑时的玩家持久字段保护

> **2026-11-12 按现行路径改写**：玩家存档 2026-10-30 起直接提交 TiDB（[player-save-direct-commit.md](./player-save-direct-commit.md)），Redis 父快照、Kafka 写回、MySQL 按列存储、login 冷加载都已删除。§0～§2、§3.3、§5～§8、§10、§14 已按现行路径重写；§9、§15 是历史记录，里面提到的 C0～C4、写方令牌、`warn` / `enforce` 模式只在那条已删除的链路上存在。

> **状态**：设计稿 v0.15（2026-11-12，Claude）。经两轮只读对抗评审：v0.1 被推翻三处核心做法，v0.2 又发现一个必现的误报（S-A）以及若干缺口，v0.3 已按两轮结论修订。负责人 2026-10-19 通过 §13 第 1–5 条。v0.4 回写第 1 批实施中的调整，v0.5 回写第 3a 批的调整，v0.6 把 C3 的"只比第一次写入"改为写方令牌并随第 3b / 4 批落码（§5.2、§15），v0.7 回写第 3a′ / 第 5 批落码与两轮对抗复核的调整（欠款版本错位改为 `kVersionSkew`、按数量扣除标"暂无守卫"、越界包类型由 `PlayerBagsComp` 携带、§12 新增未核对值域与等级上限两行；§4.4、§12、§15），v0.8 回写第 6 批落码（退役字段残留不算版本错位、按数量扣除守卫、新币种的唯一取值表与版本错位；§0、R5、§4.4、§9、§12、§13、§15），v0.9 回写 §12.1 持久值域 D1–D6 与等级上限的落码（§0、§8.3、§12、§12.1、§12.2、§15），v0.10 回写第 7 批：台账封存门禁、战斗结算的币种版本错位、收件箱停用补打（§3.1、§8.1、§9、§12、§13、§15），v0.11 回写审计 r2-ps#6 / #7：欠款自动扣还守卫、神秘商人换轮 `Clear()`（§0、R5、§4.4、§15），v0.12 回写第 8 批：载荷新字段判版本错位、Item / Pet 表行只增不删、§13 第 10 条定案（§9、§12、§13、§15），v0.13 回写第 8 批收尾：战斗结算对查不到表的物品 / 宝宝种类改整笔 DEFER（§9、§12、§13、§15），v0.14 回写补缴跳过的进程计数与告警、免除 / 调整 / 冻结欠款没有生产入口的复核（§4.4、§15，2026-11-12）。v0.15 按存盘直接提交的路径改写正文：旧版本可以接手更新世代的存档，行里不认识的顶层字段按行带回，更新版本才有的行不读不写，空行释放不判世代倒退（§0、§1、§2、§3.3、§4.2、§5～§8、§10、§13、§14、§15，2026-11-12）。各批实施状态见 §9 表前的「实施状态」。
> **起因**：2026-10-18 核对灰度 / 不停服 / 金丝雀时发现，新旧版本混跑会丢玩家持久字段。负责人 2026-10-19 要求先补三件事：①存盘保留不认识的字段；②Redis 父快照记录写方版本并校验；③go/db 只写改动列。先出设计。
> **范围**：玩家存档，即 C++ scene 内存 → go/db → TiDB `player_store`（§1.1）；外加建角、判死释放、审计快照、展示读、调试导入这些旁路。2026-10-19 起稿时的链路（Redis 父快照、Kafka `db_task_zone_*`、MySQL 按列存的两张玩家表、login 冷加载）已删除。guild、mail、trade 各自的库表，以及客户端契约，不在本文范围内（§12）。
> **证据口径**：[C] 表示对照工作区代码读过（2026-10-19 起；现行路径部分 2026-11-12 核对）；[I] 表示推断。相关文件正被其他会话修改，行号会漂移，所以正文以函数名为主。

---

## 0. 结论速览

1. **根因**：链路上有几个环节会**按本版本认识的字段重新拼一份数据**，再整份写回。新版加的字段因此在旧版手里被抹掉（§1.3 的 L1–L7），而且全程不报错（L8）。
2. **一条规则**：**本二进制不认识的内容，不读、不改、不丢，原样留在它原来的位置。** 做不到原样保留的那一步只能拒绝或延后，不能产出一份缺东西的副本。落到存盘直接提交的路径上（§1.1），分四层：
   - 子消息里的未知字段**跟着对象走**（机制一，§4.2）。做不到的消息类型冻结，冻结类型不许再加字段。通用的往返一致性测试机械地守住这条。
   - 一行里本二进制不认识的**顶层字段**由存盘状态机按行保管，每次重写这一行时原样接回去（§4.2「行存储上的携带」）。
   - **更新版本才有的行**不读也不写，库里那一行原样留着。
   - 对象内携带挡不住的语义耦合（合堆、合并、按数量扣除、自动扣还、逻辑重置）由守卫处理：旧版代码不对「带着更新版本字段的对象」做这些操作，或者先整体 `Clear()` 再重设（§4.4）。守卫覆盖不到的走破坏性变更流程（R5）。
3. **所以旧版本可以接手新版本写过的存档**（2026-11-12 起）：认领不按「存档世代比 scene 新」拒绝。在金丝雀 zone 存过盘的玩家可以跨到还没升级的 zone、可以回自己的 home zone，zone 也可以回退 scene（§5.2、§7、§8.3）。
4. **世代号不负责防丢字段**（机制二，§5）。它只做两件事：
   - **保证 go/db 不比 scene 和存档旧**：认领时存档或 scene 的世代高于 go/db 认识的，回 `SCHEMA_TOO_NEW`、不抬代次；存盘时同理，scene 保留快照重试。
   - **发现没把内容带回来的写入**：带着数据行、世代却低于写权行记下的世代，回 `SCHEMA_REGRESSED`、不写。合法的 scene 存盘世代取 max(自己的, 加载到的)，不会触发。
5. **go/db 不解释载荷**（机制三，§6）：`player_store` 的 payload 是不透明字节，go/db 的版本不影响任何字段。按列存储才有的「写方不认识的列被写成零值」「旧 go/db 读出截断的副本」两类问题随存储改造消失。
6. **世代号由生成器发**：台账是 `proto/persist_schema_gen.txt`（仿 `proto/message_id.txt`）。不能安全携带的改动，在生成期就失败（§3.2）。
7. **发布与回滚**（§8）：每次升世代先发全部 zone 的 go/db，再发 scene（可以按 zone 金丝雀）。scene 可以回滚到任何带本机制的版本；go/db 不能回滚到低于存档世代的版本，回滚了只会卡住认领和存盘，不丢已提交的数据。
8. **Java 版**：存储是单个 BLOB，机制三不适用，机制一、二同样需要，只登记（§11）。

---

## 1. 问题与现状

### 1.1 数据流（2026-10-30 起：存盘直接提交 TiDB）

```text
C++ scene（ECS 组件，内存是权威）
  │  MarshalPlayerForPersist：从组件拼 player_database，盖世代 max(本二进制世代, 加载到的世代)
  │  ProjectPlayer：按 OptionPersistGroup 把字段投影成若干行，每行只含本二进制认识的字段
  ▼
PlayerStoreClient（存盘状态机）
  │  接上认领时带回的行内未知字段 → 与上一次确认提交逐行比字节 → 只发有变化的行
  ▼  gRPC Save
go/db playerstore：一个事务里锁写权行 → 判代次、持有者、序号、世代 → 写写权行和有变化的行
  ▼
TiDB player_store（主键 (player_id, kind)；kind 0 是写权行，其余每个 kind 一行，payload 是不透明字节）

加载：scene → go/db LoadAndClaim（一个事务里认领并读出全部行）
        → PlayerStoreClient 播种（库里原字节；逐行取出本二进制不认识的顶层字段）
        → AssemblePlayer 把认识的内容拼回 player_database → InitPlayerFromAllData
```

玩家存档不经过 Redis、Kafka，也不经过 login。规格见 [player-save-direct-commit.md](./player-save-direct-commit.md)。旁路只有：

- **建角**：data_service 在建角事务里写写权行和资料行（`account_role_initial_store.go`）。
- **判死释放**：data_service 只改写权行的代次、持有者、`released`，不碰数据行和世代。
- **审计快照**：go/db 在提交存盘的同一个事务里，按写完之后的全部行拼出快照（`buildAuditSnapshot`），不经 scene 重拼。
- **展示读、角色交易的冻结证据**：data_service 只读。
- **本地调试导出导入**：`go/data_service/cmd/debug_fetch`、`debug_import`。

本文 2026-10-19 起稿时是另一条链路：scene → Redis 父快照 → Kafka `db_task_zone_*` → go/db → MySQL 两张按列存的玩家表，外加 login 冷加载。§1.3 的问题清单按那条链路写，保留作起因；每一条现在怎样了，见表的最后一列。

### 1.2 本来就能保留未知字段的环节 [C]

| 环节 | 为什么能保留 |
|---|---|
| go/db、TiDB | payload 是不透明字节，原样存取 |
| Go 侧的只读方：data_service 展示读、go/db 拼审计快照 | Go protobuf 默认保留未知字段；它们不回写存档 |
| C++ 中以 PB 直接存放的组件 | 加载时 `emplace<Comp>(player, message.xxx())` 拷贝整份，存盘时 `CopyFrom` 带回。加载期的修正都是就地改字段：`ApplyClassInitialAttributesOrRevive`、`PlayerAttributeSystem::InitializeOnLoad`、`PetSystem::InitializeOnLoad`、各账本的 `RemoveLast` / `DeleteSubrange`（评审核实）。运行期也没有高频整体替换持久组件；唯一的整块替换是瞬移，会替换 Vector3 / Rotation |
| 物品实例 | `ItemEntry.instance = 15` 承载整份 `ItemComp`（`bag_marshal.cpp`） |

### 1.3 起因：2026-10-19 链路上会丢字段的环节 [C]

| 编号 | 当时的位置 | 丢什么 | 现在 |
|---|---|---|---|
| L1 | `SavePlayerToRedisImpl` 新建空消息交给 `MarshalPlayerForSave`；退出收敛比较、`discardedProbe` 也一样（`player_lifecycle.cpp`） | 玩家表消息的**顶层未知字段**，也就是新增的整列 | 行里不认识的顶层字段由存盘状态机按行带回（§4.2「行存储上的携带」） |
| L2 | `PlayerCurrencyComp::SaveToProto` 先 `clear_debts()`，再逐字段重建（`player_currency_comp.h`） | `CurrencyDebtEntry` 的新字段（资产路径） | 欠款对象携带本层未知字段（§4.2） |
| L3 | `mission_marshal::Marshal` 先 `Clear()` 再重建；`Unmarshal` 只拷两个 map | QuestAllData / QuestScopeData / MissionListComp 的顶层新字段 | `MissionListComp` 携带本层未知字段，容器层冻结（§4.2） |
| L4 | `bag_marshal::Marshal` 先 `Clear()`，再重建布局层；`ItemEntry.equip` 由 `ItemComp.equip` 重新算出，是投影 | 布局层的新字段 | 布局层冻结，`ItemEntry.equip` 标为投影（§3.2） |
| L5 | `SanitizeSkillList` 新建组件后整体替换（`player_skill.cpp`） | PlayerSkillListComp 的顶层新字段 | 替换前并回组件本层未知字段（§4.2） |
| L6 | `SnapshotSystem::CaptureAndSend` 直接拼一份新消息（`snapshot_system.cpp`） | 同 L1，丢在回档快照里 | 这个环节不存在了：快照由 go/db 按库里的行拼，不经 scene 重拼 |
| L7 | go/db 写（`tx.Save` 整行 UPDATE）和读（只 SELECT 已知列）都只覆盖本版本认识的列 | 新版 go/db 把旧写方 body 里缺的新列写成零值；旧版 go/db 冷加载读出截断的副本 | 这个环节不存在了：存储改成不透明的行（§6） |
| L8 | 全链路 | 没有写方版本，也没有覆盖信息，丢了零报错 | 写权行记世代，倒退拒写并告警（§5、§8.4） |

`stress_test_probe` 这个已知字段不属于未知字段问题：scene 每次写盘重新打探针，不读回加载值，标 `OptionPersistUncarried`（§3.2）。当时一并提到的 `merge_state` 已从玩家存档删除。

### 1.4 后果（当时）

- **不能金丝雀**：金丝雀本身就是混版本运行。
- **不能回滚**：回滚 scene 后，所有在线玩家的新字段都会被抹掉。
- **丢了不知道**：没有任何报错或指标。

---

## 2. 目标与不变量

- **P1：不认识的内容原样保留。** 数据每经过一跳读写，这一跳对自己不认识的内容只有两种合法结局：
  - 原样保留，位置不变；
  - 拒绝或延后，不产出任何副本。
- **P2：世代单调。** 每份存档的写权行记着它写过的最高世代 `c`。带数据的写入，世代 `c'` 都满足 `c' ≥ c`：scene 取 `max(本二进制世代, 加载到的世代)`。`c' < c` 的一律拒绝。

  **推论**：只要每一跳都守住 P1，某个世代才引入的字段在数据里缺失，就一定是从来没写过，按默认值处理是对的。
- **P3：** 旧版 scene 接手新数据时完整携带新内容；对携带着更新版本字段的对象，不做可能改变其语义的操作（§4.4）。
- **P4：** go/db 不解释载荷，原样存取。它只要求自己认识的世代不低于 scene 和存档的世代（§5、§6）。
- **P5：** 违反 P1、P2 的情况一律 fail-closed，配指标和告警，player_id 不进 label（§8.4）。

**非目标**：
- 持久值域扩展（§12）；
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
- **封存**（第 7 批，2026-11-05，审计 r2-ps#1 / r2-ps#10 的机器门禁）：只比世代号的判定（§5.1）都靠一条不变量成立——**同一世代号，所有二进制认识的字段集合相同**。（2026-11-12 起携带不看世代号，这条不变量被破坏不会再丢字段；失效的是「go/db 不比 scene、不比存档旧」这道门禁，§5.3。）有两种情况会破坏它：
  - 上线后台账还停在 `bootstrap`：新字段继续并进已经上线的世代 1；
  - 两个分支从同一基线各拿到世代 N+1，其中一个先发布，另一个合入时 git 自动合并了台账（新增行落在不同位置时不冲突）。

  做法：
  - 发布前在主干运行 `dev_tools.ps1 -Command persist-schema-seal`（生成器 `--persist-schema-seal`），台账末尾追加 `sealed=<最大世代>:<行数>:<sha256>`。摘要口径见 `persistschema.SealDigest`：世代 ≤ 封存世代的字段行，按（消息全名，字段号）排序，取世代、身份、标签、类型、oneof 求 SHA-256。字段名不进摘要（改名不改线格式）；退役行不进摘要（退役不改变这个世代认识哪些字段）。
  - 生成器每次运行都核对全部封存行，已封存世代的字段集合一旦增删，生成就失败：
    - 引导期往世代 1 加字段：提示先按 `release-checklist.md` §6.2 切 `active`；
    - 合并带进同世代的行：提示丢弃本分支在台账里新增的行，在合入后的主干上重新生成，新字段拿新世代。
  - 已有封存被违反时，封存命令拒绝执行，不能用新封存把问题盖住。
  - 发布预检 `release_preflight.ps1 -ReleaseVersion` 的 `release.persist_schema.sealed` 要求本次发布的台账已封存到最大世代，并调生成器的只核不写模式 `--persist-schema-verify`（`dev_tools.ps1 -Command persist-schema-verify`）核每条封存的摘要。只比世代号会放过「合并带进同世代的行」：那时封存已被违反，最大世代却仍等于最后一条封存的世代（2026-11-05 评审）。档位与预检其余检查一致：只有 dev 档降为 WARN。
  - 封存前先跑一次生成，确认台账与 proto 一致（`release-checklist.md` §2.1 ①）：封存只封台账，不读描述符。
  - 只对要上生产的版本封存：引导期一旦封存，再加持久字段就必须先切 `active`。
  - 没有做运行期指纹（每次写盘带字段集合指纹、go/db 遇到同世代异指纹时延后）。它是纵深防御。2026-11-12 起携带不依赖世代号，同一世代两种字段集合不会再丢字段；go/db 不解析载荷，它要认识的行种在每笔存盘进事务前就核（§5.1），指纹能多挡住的只剩发布顺序，不值得为它改存盘协议。构建期门禁已经从源头堵住上面两种来源。剩下的漏洞靠发布纪律，记在 §13 第 11 条：部署绕过发布流程（用未封存或未经预检的产物上线）；台账与 proto 脱节时封存（例如解决合并冲突时丢了台账行、却留下了生成代码）。仓库台账的规范格式、封存与存储分组由生成器 `persistschema` 包的单测核对（`TestRepositoryLedgerIsCanonical` 读仓库里的台账），2026-11-05 起 CI 的 `go-modules-ci.yml` 用单独的 `persist-schema-ledger` job 跑这个包的 vet 与单测，结果计入「Go 门禁汇总」。整个生成器模块仍不在模块矩阵里：它的 build / test 没在 Linux 上实测过。
- **存储分组不得改**（2026-11-05，数据覆写审计 R2 的 N1）：玩家存储按 `OptionPersistGroup` 把 `player_database` 拆成多行（`player-save-direct-commit.md` §1），加载时只认字段的当前分组。字段换组后第一次载入会把旧分组那一行里的字段清掉，新分组那一行里还没有，第一次存盘就把它永久写丢。台账给玩家表顶层字段记 `group=<分组>`（不进封存摘要），换组或删掉分组选项时生成失败；分组出现之前写下的旧行在第一次生成时原地补记一次。换存储位置按 R4 走 expand / contract。

### 3.2 可携带性规则

| 规则 | 不可携带的改动 | 怎么做 |
|---|---|---|
| R1 | 给**冻结类型**加字段（生成期强制） | 先给这个类型实现运行时携带，并纳入往返测试，再解冻；或者把数据放进可携带的类型，例如物品数据放进 `ItemComp`（AGENTS §11.6） |
| R2 | 给已有的持久 oneof 加新成员（生成期强制） | 字段放到 oneof 之外，或者走破坏性变更流程（整批停机换版，或 expand / contract） |
| R3 | 改号、改类型、复用号（生成期强制） | 用 `reserved` |
| R4 | 字段迁移：子消息字段提升为列、拆分、换存储位置。生成器只能检测"同一世代内既删又增"并给出提示；玩家表顶层字段换存储分组（`OptionPersistGroup`）由生成器直接拒绝（2026-11-05，§3.1「存储分组不得改」） | expand / contract：新旧位置双写一个完整版本周期，然后再切换读方 |
| R5 | **语义耦合字段**：会参与等价判定、合堆、合并、逻辑重置的字段，或者按 ID 引用其他实体、而这类实体旧代码能删除后重建的字段。生成器无法检测，列入评审清单 | 依赖 §4.4 的守卫（合堆、欠款合并：旧代码拒绝执行；欠款自动扣还：旧代码不扣还、不删除，按冻结处理；按 config 计数与扣数量：旧代码不计、不扣带更新版本字段的单位，按版本错位处理，第 6 批；纪元重置与神秘商人换轮：旧代码整体清空未知字段）；守卫覆盖不到的场景，走破坏性变更流程，包括新版要求跨纪元保留的流账本字段、跨轮保留的神秘商人限购字段（旧版重置时会清掉）。§4.4 仍未守的：`has_equip` 实例按 config 运算（调用方在表上排除装备兜住）；`EquipInstanceData` 内部的新字段（谓词只看 `ItemComp` 本层）；按 guid 托管或改写实例时才生效的新字段（例如不可交易、封印）；`AttributeScheme`、`PetInstance`、`PlayerSkillComp` 本层的新字段（§12.2）。**会被普遍写入的 `ItemComp` 新字段（例如获得时间）分两版发布**：版本 N 只在 proto 里声明（读、忽略、原样存盘），版本 N 全量之后版本 N+1 才开始写，回滚下限是 N；否则跨版本窗口里几乎所有新堆都会变成"不可按数量扣除"（守卫分不出有语义和无语义的字段）。`ItemComp` 新字段不得改变 `size` 的计数含义，也不得让单个实例的可扣量超过 `size`（这是"确定不足可以记终态"的前提）。跨实体引用只能用永不复用的实例 ID |

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

- **位置**：`player_store` 表的 `schema_gen` 列。写权行（kind 0）上的值是这份存档**写过的最高世代**，只升不降，判定只看它；数据行上的值是这一行写过的最高世代，只作排查用。`player_database.persist_schema_gen`（21 号）是元数据，不进任何一行的 payload：加载时由 scene 从认领回包填回消息，存盘时随请求的 `schema_gen` 发给 go/db，审计快照里由 go/db 填。
- **取值约定**：0 表示未盖戳，只会出现在不经存储的测试夹具里，比较时按 1 处理（`player_persist::EffectiveSchemaGen`）。所有写入方写入的值都 ≥ 1。
- **加载世代** = 认领回包里写权行的 `schema_gen`，记在持久数据类组件 `PlayerDatabaseCarryComp::loadedSchemaGen` 里。存盘入口缺这个组件就整份拒存。
- **输出世代** = `max(kPersistSchemaGen, 加载世代)`（`player_persist::MarshalPlayerForPersist`）。写权行和这一笔里有变化的行都写它。
- **建角**：data_service 写初始行时盖它自己二进制的世代（`persistschema.Gen`）。建角重放比对初始行时，库里的世代不超前就算同一批（`sameInitialRows`）。
- **判死释放**不改世代：它只改写权行的代次、持有者和 `released`。

2026-11-05 之前玩家存档有两张表消息，加载世代取两张表戳的最小值；`player_database_1` 删除之后只剩一张（`player-save-direct-commit.md` §1.2）。

---

## 4. 机制一：未知字段跟着对象走

### 4.1 为什么不从加载原件嫁接

v0.1 的做法是在实体上保存加载原件，存盘时按身份键把原件里的未知字段嫁接回去。评审 S1 证明这会**复活已删除元素的新字段**。

例子：欠款还清后删除，之后 `AttachDebt` 用同一个 `currency_type` 新建欠款，嫁接就把旧欠款的新字段挂到了新欠款上。任务放弃后重新接取，情况相同。

根因是嫁接把未知字段的生命周期，和它所属对象的生命周期拆开了。v0.3 遵循 protobuf 本身的语义：**未知字段只存在于它所属的对象里**，对象被删除，未知字段也随之消失。

### 4.2 改造清单

| 编号 | 改造 |
|---|---|
| L1 | 持久数据类组件 `PlayerDatabaseCarryComp`：保存 `player_database` 的加载世代（`loadedSchemaGen`），以及不经行存储、直接加载整份消息时（单测夹具）的顶层未知字段。<br>**创建点**：在手写的 `PlayerDatabaseMessageFieldsUnmarshal` 里创建，不放在 `InitPlayerFromAllData`。这样经过生成的 `PlayerAllDataMessageFieldsUnmarshal` 加载的测试实体也带有这个组件（评审 M-E）。<br>存盘入口缺少它就整份拒存（持久数据类规则）。<br>经行存储加载时，行里的顶层未知字段不进这个组件，由存盘状态机按行保管（下文「行存储上的携带」）。2026-11-06 之前还有对应 `player_database_1` 的 `PlayerDatabase1CarryComp`，随那个消息删除。<br>顶层未知字段是旧代码完全没有逻辑对应的整列，旧代码不会删除或重置它们，所以这里不会出现 S1 那种复活。按 ID 引用其他实体的新列另当别论，归 R5 / §4.4 管 |
| L2 | 运行时结构 `CurrencyDebt` 增加 `std::string unknownFields`（`UnknownFieldSet` 的序列化）。`LoadFromProto` 时保存，`SaveToProto` 时写回。欠款删除后重建得到的是新对象，未知字段为空 |
| L3 | `MissionListComp` 改为整份保存，`Unmarshal` 不再只拷两个 map；QuestAllData / QuestScopeData 冻结 |
| L4 | 布局层冻结；`ItemEntry.equip` 标为投影 |
| L5 | `SanitizeSkillList` 替换组件时，保留原组件的顶层未知字段（删技能问题另见 §12） |
| L6 | 回档快照：当时 `SnapshotSystem::CaptureAndSend` 改走统一入口。2026-10-30 起快照由 go/db 在存盘事务里按库里的行拼（`buildAuditSnapshot`），不经 scene 重拼，行里的未知字段原样在快照里 |
| L7（新增） | 逻辑重置统一改为"`Clear()` 后重设身份字段"。现有的 `ResetAssetOpStreamForEpoch`（`asset_op_ledger.cpp`）是逐字段清空，未知字段会跨纪元存活下来，把旧纪元的状态带进新纪元（评审 M-F，资产路径） |

**统一入口**：手写头文件 `services/scene/player/system/player_persist_marshal.h`。

```cpp
// 存盘与退出收敛比较的唯一拼装入口:比的、写的是同一个形状。
// 步骤:PlayerAllDataMessageFieldsMarshal → 填 player_id → 盖戳 max(kPersistSchemaGen, 加载世代)。
// 返回 false = 持久组件缺失(含 Carry 组件);调用方必须拒绝这次写入。
[[nodiscard]] bool MarshalPlayerForPersist(entt::entity player, Guid playerId, PlayerAllData& out);
```

- `MarshalPlayerForSave` 改为转调它；退出收敛和两处 `discardedProbe` 因此一并覆盖。
- 回档快照不再经过 scene（L6）。
- `player_data_loader.h` 是生成产物，**不改**。
- 盖戳在投影与比脏之前完成；`last_save_seq` 在这一笔提交之后才写回快照（`OnPlayerStoreSaveDone`）。

#### 行存储上的携带（2026-11-12）

存盘直接提交之后，玩家存档在库里是一组行，每一行的 payload 是 `player_database` 的一部分字段（§1.1）。存盘时一行整行重写，所以旧版本只要改了某一行里自己认识的字段，就得把这一行里它不认识的内容一起写回去。分三种：

| 旧版本不认识的内容 | 怎么保留 |
|---|---|
| 子消息里的字段 | 跟着对象走（上表 L2–L5、§1.2），和存储形状无关 |
| 一行里的顶层字段：更新版本加在这一行的新字段，或已退役字段的残留 | **不进内存**，由存盘状态机按行保管。认领时 `PlayerStoreClient` 逐行调 `Options::carriedBytes`（生产接 `player_store_rows::CarriedTopLevelBytes`）取出原样的字节，随播种存进 `State::carried`；之后 `Submit`、`DeferPeriodicSave` 收到投影结果，先把这一行带回的字节接在后面，再比脏、再发送 |
| 更新版本才有的行（`IsNewerKind`：号比本二进制认识的都大） | 认领时跳过，不播种、不比脏、不写。库里那一行没有人动 |

几条决定和原因：

- **顶层未知字段为什么不并进消息、不放进 Carry 组件**：各行拼成一份 `player_database` 之后，分不清哪个未知字段属于哪一行，`ProjectPlayer` 没法把它写回原来的行。按行保管就不需要知道它是什么。所以 `ProjectPlayer` 遇到顶层有未知字段的消息仍然拒绝：那说明这份消息不是从行里加载来的。
- **为什么放在存盘状态机里，不挂在实体上**：带回的字节必须在第一笔存盘之前就位，晚一步，那一行就可能被重写成不含它们的版本。状态机在认领回包到达的同一个调用里播种，早于建实体；状态以玩家号为键，实体销毁、存盘还在途时也在。
- **退役字段的残留也带回，不在加载时丢掉**：删字段的版本和还在用这个字段的版本会同时在线（滚动、回滚）。新版本加载时把残留丢掉，玩家回到旧版本就少了这个字段。残留一直留在行里，要清掉须另配迁移。
- **接回去的字节放在认识的字段之后**。更新版本当初写这一行时字段按号排，两者的字节顺序可能不同；那样这一行会在旧版本第一次存盘时被判脏、重写一次，内容等价。
- **更新版本的行为什么可以不碰**：行种的号只增、不回填、不复用，字段不许换行（生成器按台账的 `group=` 拒绝，§3.1）。所以号更大的行里只有本二进制不认识的字段，不读它不会缺任何认识的内容。号比本二进制认识的最大号小、又不是数据行的，只剩退役号（旧布局的 1、2、5），照旧整份拒绝加载。
- **解析不了的行**：取不出要带回的内容就不能播种，这次认领按没有结论处理，进场失败。
- **生产必须接 `carriedBytes`**（`MakePlayerStoreOptions`）。不接就等于不带回。`cross_zone_test` 的 `PlayerStoreDirectEcs.ClaimCarriesWhatThisBuildDoesNotKnowBackIntoItsRow` 走生产接线守住这一条。
- **成本**：认领时每行多解析一遍（取未知字段一遍，拼装一遍），只发生在进场。全网同一版本时每行带回的字节都是空的，存盘路径不做任何拼接。

`PlayerDatabaseCarryComp::topLevelUnknown` 只在不经行存储、直接加载整份消息时（单测夹具）非空，这时由手写 Marshal 并回消息顶层。

### 4.3 往返一致性测试

**宿主**：放在 bag_test 工程里。它已经调用 `PlayerAllDataMessageFieldsUnmarshal`，SetUp 里也已经加载了 Mission、Condition、Class、AttributePool、AttributeDimension、AttributeRule 等配置表。还要补加载 SkillTable 和宠物相关表，让夹具用真实表 ID 走"本版本认识这些 ID"的生效路径（评审核实）。第 5 批前，不加载技能表时 `SanitizeSkillList` 会把技能节点全部删掉；第 5 批起查不到表的技能保留但不生效，属于另一条路径，不在往返测试里混测。

**夹具**：现有夹具只覆盖背包和任务，需要新建一份**覆盖全部持久组件**的夹具，并使用真实的表 ID。

**步骤**：

1. 用反射遍历夹具里所有已设置的消息节点。在每一个**非冻结、非 map entry、不经由投影字段到达**的节点上，注入一个未知 varint 字段：字段号不超过 2^29−1，并避开 19000–19999；值是该节点的唯一标签。冻结节点本身不注入，但继续向下遍历。比如 `ItemEntry` 冻结，但它的 `instance`（ItemComp）照常注入。
2. 依次执行 `PlayerAllDataMessageFieldsUnmarshal` → `MarshalPlayerForPersist`。
3. 断言：
   - 全部标签都出现在输出里，并且所在节点的类型与注入时相同（按标签的多重集比对，不依赖元素顺序）；
   - 表消息的戳等于期望值。
4. **覆盖率断言**：从玩家表可达的每一个非冻结类型，至少要有一个节点被注入。缺失时测试失败，并给出缺失的类型名。例如 `EquipInstanceData` 只能经由 `ItemComp.equip` 满足。

**测试改动面**（评审 M-E）：手工搭建实体的测试都要补齐 Carry 组件，或者改用统一入口，否则会进入拒存分支。已知涉及：`cross_zone_test`（`MarshalAsSaved`）、`bag_test` 的 `player_feature_persistence_test`。

### 4.4 语义耦合的守卫（评审 M-F）

对象内携带只保证"字节不丢"，不保证"旧代码对这个对象的操作，在新语义下依然正确"。仓库里有五类操作已经注定会出问题，现在五类都在当前版本里加了守卫（按 config 计数和扣数量为第 6 批；欠款自动扣还、神秘商人换轮为 2026-11-05 审计 r2-ps#6 / #7）。这样等以后它们成为"旧版本"时，守卫才会生效。

判"带着更新版本的字段"只有一个谓词：`unknown_field_carry::CarriesNewerVersionFields`。对象本层的未知字段里，只要有一个字段号不在台账的退役清单里就算（台账 `retired=<消息全名>#<字段号>` 行，生成为 `persist_schema::kRetiredFields` / `IsRetiredField`）。退役字段的残留不算：只看"有没有未知字段"的话，带残留的对象会在所有版本上被永久当成新版本数据，永远不能合堆、合并、按数量扣除（第 6 批）。守卫一律用它，不直接看 `UnknownFieldSet` 是否为空：

| 操作 | 现状 [C] | 守卫 |
|---|---|---|
| 合堆、等价判定 | `ItemStore::CanStack` 只判断 `has_equip`；`ApplyStackFill` / `MergePartialStacks` 按配置合堆。`ItemEntry` 的 TODO 里已规划 bound_state、expire_at | 如果任一方 `ItemComp` 带有更新版本的字段，`CanStack` 返回 false。旧版不会把带新属性（比如绑定、限时）的物品并进普通堆。判定只有一份（`ItemStore::IsIndependentInstance`，公开）：包内既有堆的空余估算与整理、传入物品一侧 `Bag::AddItems` 的规划分流、宝石拆卸的静默预检（直接调 `CanStack`）都用它，预检、规划与提交不分叉（后两处为第 3a′ 批） |
| 按 config 计数和扣数量 | `ItemStore::TotalCountOf` / `CheckQuantities` / `DrainStacks`。调用方：资产通道数量扣出（快照只记 config 与数量，对端按 `ItemCountMap` 新建普通实例）、强化扣材料、镶嵌扣宝石（装备上只记 config，拆下时生成普通宝石）、战斗快照与结算消耗（`RemoveItemsClamped`；收件箱结算经 `ConsumeInventoryAndGrant`）（2026-10-19 对抗复核 B-1） | **第 6 批已守**。谓词 `ItemStore::IsCountableByConfig`（= 不带更新版本的字段）与 `IsIndependentInstance` 分工：前者管能不能按 config 计数、扣除，后者管能不能合堆；`has_equip` 实例照算，闸在调用方（四个调用方都在表上排除了装备）。计数（`TotalCountOf` / `Bag::GetTotalItemCount`，名字未改，语义是"可按 config 扣的数量"）、预检、扣除（`DrainStacks`）同口径跳过不可计的单位。<br>预检是三态 `ItemStore::CheckQuantities`：任一项算上带新字段的单位仍不够 → 确定不足（优先，可以记终态）；否则任一项只差在带新字段的单位上 → 版本错位（非终态）；其余 → 够。`Bag::CheckItemsAvailable` 映射成 kSuccess / `kFeatureUnavailable`(1006) / kBagInsufficientItems；原 bool 的 `HasAll` 会把错位并进"不足"，已删除。<br>各调用方：资产通道数量扣出版本错位回 RETRY + 1006、不记账，确定不足照旧记终态 REJECTED；错位排在按 guid、宝宝这些与版本无关的确定性校验之后，那些坏包仍先记终态。强化、镶嵌回 1006，排在金币判定之前，判完才扣金币，所以不会出现 ENHANCE_PARTIAL。战斗快照只计可计单位；收件箱结算遇到带新字段的消耗物品回 `kVersionSkew`（战斗结算会话落码）；直接结算路径扣不满按夹紧处理，打防刷 WARN（跨版本战斗窗口里可能误报）。<br>代价（fail-closed，写进发布单）：带新属性的可叠加物品在旧版里不能按 config 消耗、交易、带进战斗；聚宝斋托管遇到它会一直 RETRY，卖家可以撤单（托管阶段可撤，2026-10-30） |
| 合并 | `AttachDebt` 会把新欠款并进已有欠款 | 已有欠款带更新版本的字段时，`AttachDebt` 按 fail-closed 返回失败，不做合并。这是版本错位，不是确定性拒绝：不得记成终态拒绝，也不能按本地副本原地重试（本地副本看不到服务端之后挂的作废，错位一旦在会话内消失——这笔欠款被删除——续做就会挂上已作废的处置；下一行的自动扣还不会把它还清删除，错位不消失时续做只会空转）。调用方用只读谓词 `CurrencySystem::DebtCarriesUnknownFields` 先判，不拿通用返回码反推；收件箱执行器命中回 `kVersionSkew`，收件箱停用本域，实体重建后重拉（退出完成后再登录，或换节点；退出保留期内重登复用实体，不重拉）（第 3a′ 批，对抗复核 A-1 后由 `kDefer` 改来，`player-inbox-service.md` §7.6 第 3 步）。`AdjustDebt` / `WaiveDebt` / `FreezeDebt` 目前没有生产调用方：GM RPC 已删除（`proto/message_id.txt` 106 / 113 / 109 记为 reserved），收件箱只有挂欠款一个类型（2026-11-12 复核）。接任何入口之前要先补同样的守卫：带更新版本字段时不改、不删，打 `metric=persist_version_skew` 并计数，调用方按版本错位处理（`currency_system.h` 三个声明前写明） |
| 欠款自动扣还 | `AddCurrency` 按 owed−paid 从收入里扣还，扣满即整条删除，新字段随之消失（审计 r2-ps#6） | 欠款带更新版本的字段时，本版本不扣还也不删除，按冻结处理：这笔收入全额入账，欠款与未知字段原样保留，留给认识这些字段的版本去收。只在本版本本来要扣还时（欠款有效、还有余额）打一条 `metric=persist_version_skew where=currency_clawback` 的 WARN，这一行就是计数口径；同一时刻进程计数加一，scene 导出为 `scene_currency_clawback_version_skew_total`，告警 `SceneCurrencyClawbackVersionSkew`（`deploy/k8s/persist-version-skew-alerts.yaml`，2026-11-12）。判据与合并同一个：`DebtCarriesUnknownFields` 与 `AddCurrency` 共用 `currency_system.cpp` 里的 `DebtCarriesNewerVersionFields`。<br>代价：混版本窗口里这名玩家这一币种的补缴暂停，收入先全额到手；欠款不丢，落到新版本节点后按新口径收。战斗收件箱结算的整批预检 `CanCreditWholeBattleCurrency` 遇到这类欠款照旧回 `kVersionSkew`、不入账（2026-11-05） |
| 逻辑重置 | `ResetAssetOpStreamForEpoch` 逐字段清空；神秘商人换轮 `merchantrules::RecordPurchase` 逐字段清空（`clear_purchases` + `set_round_id`，审计 r2-ps#7） | 两处都改为 `Clear()` 后重设（§4.2 L7）：资产通道重设流号与纪元，神秘商人重设轮次（2026-11-05）。这不是"拒绝执行"：重置照常进行，未知字段随旧纪元、旧轮一起清掉；新版要求跨纪元、跨轮保留的字段，旧版会在重置时清掉，归 R5 |

跨实体引用（比如新列按 pet_id 引用宠物，而旧代码能按同一个 pet_id 恢复宠物）没有通用守卫。归入 R5：只能引用永不复用的实例 ID，否则按不可携带处理。

技能条目（`PlayerSkillComp`）、加点方案（`AttributeScheme`）、宝宝实例（`PetInstance`）本层的新字段**暂无守卫**，按 R5 走破坏性变更（§12.2 约定由本节登记）。这些对象上的操作都是玩家主动的修改（修习、加点、洗点、出战），新字段会怎样约束它们只有新版本知道；一刀切地拒绝修改带新字段的对象，会在混版本窗口里把这些玩法整片停掉，所以不加。将来某个新字段确需旧版本拒绝修改时，按需在该操作入口用 `CarriesNewerVersionFields` 判，它已能分开新版本字段与退役字段的残留。

### 4.5 成本

平时（全网同一版本）没有任何未知字段，Carry 组件里只是两个空集合，欠款的 `unknownFields` 为空串，不做反射遍历。混版本期间的开销，与未知字段的字节数成正比。

---

## 5. 机制二：世代判定

世代号不负责防丢字段，那是 §4 的事。它只回答两个问题：go/db 是不是比 scene 和存档旧；这一笔写入是不是没把加载到的内容带回来。

### 5.1 判定点 [C]

记号：存盘请求的世代为 `s`，写权行记下的世代为 `r`，scene 二进制的世代为 `B`，go/db 认识的世代为 `G`。

| 位置 | 条件 | 结果 | scene 的处置 |
|---|---|---|---|
| 认领，go/db `decideClaim` | `r > G` 或 `B > G` | `LOAD_AND_CLAIM_SCHEMA_TOO_NEW`，不抬代次、不读数据 | 这次进场失败，玩家重试；旧持有者不受影响 |
| 存盘，go/db `validateSave`（进事务前） | 带了 go/db 不认识的行种 | `SAVE_PLAYER_SCHEMA_TOO_NEW`，整笔不写 | 保留快照，退避重试 |
| 存盘，go/db `decideSave` | 带着数据行，且 `s < r` | `SAVE_PLAYER_SCHEMA_REGRESSED`，整笔不写 | `HandlePlayerSaveSchemaRejected`，与确定性拒绝同一处置：在线的保留实体，这次不落盘；退出中的不存盘丢弃实体；交接中的中止这次交接。还有排队的快照时由最后一笔决定 |
| 存盘，go/db `decideSave` | `s > G` | `SAVE_PLAYER_SCHEMA_TOO_NEW`，整笔不写 | 保留快照，退避重试 |
| 认领回包，scene `PlayerStoreClient` 与 `AssemblePlayer` | 有退役行种的行（旧布局的 1、2、5），或某一行解析不了 | 这次认领没有结论 | 进场失败 |
| 投影，scene `ProjectPlayer` | 要存的消息顶层有未知字段 | 拒存，打 ERROR | 这一笔不提交 |

判定都是纯函数（`go/db/internal/playerstore/decide.go`），与 SQL 分开，逐格有单测。完整的写权判定表（代次、持有者、序号）在 [player-save-direct-commit.md](./player-save-direct-commit.md) §4，这里只列与世代有关的行。

### 5.2 不拦的情况：存档的世代比 scene 新

`r > B` 不拒绝（2026-11-12）。旧版 scene 认领更新版本写过的存档，照常加载：

- 它不认识的内容原样留着（§4.2）；
- 存盘世代 `s = max(B, r) = r`，不会倒退；
- 带着更新版本字段的对象，守卫拦住会改变其语义的操作（§4.4）；持久值域按本版本的规则生效、不改写（§12.2）。

2026-10-30 到 11-12 之间这里是拒绝的，理由是「scene 解释不了这份存档」。那样没有丢数据的风险，但升了世代的版本一旦放出去就收不回来：

- 在金丝雀 zone 存过盘的玩家进不了还没升级的 zone。别的 zone 的玩家来金丝雀 zone 一趟，回自己的 home zone 时也被挡在外面，直到 home zone 升级。
- zone 回退 scene 之后，所有在新版本上存过盘的玩家都进不了场，只能向前修复。

这等于升世代的版本不能金丝雀、不能回滚，正是本文要解决的问题（§1.4）。机制一从第 3a 批起就是为「旧版本带着新内容继续跑」做的，这里把它在直接提交的路径上接通。被否决的做法见 §13 第 12 条。

### 5.3 为什么合法流程不会触发倒退 [C]

- 同一时刻只有一个持有者能写（写权行的代次与持有者，`player-save-direct-commit.md` §4）。
- 持有者的加载世代就是认领回包里写权行的 `r`，存盘世代取 `max(B, r) ≥ r`。
- 不带数据行的存盘不判倒退。实体已经没了的空行释放（`ReleaseDetached`）手里没有加载世代，带的是 `B`；旧 scene 持有更新世代的存档时它比 `r` 低。这一笔不写任何数据，判成倒退的话释放永远提交不了。写权行的世代由写入语句取 `GREATEST`（`sqlSaveTail`），不会被它拉低。

所以 `SCHEMA_REGRESSED` 出现，就说明有写方带着数据行却没把加载到的世代带回来：导入工具、手工改库，或者代码缺陷。这时宁可不写。

世代相同不代表字段集合相同：引导期里所有二进制都是世代 1（§8.1）。携带不依赖世代号，引导期里照常生效；世代判定在引导期不起作用。

### 5.4 各写入方与只读方 [C]

| 谁 | 对世代做什么 |
|---|---|
| scene 存盘 | 盖 `max(B, 加载世代)`；写权行和这一笔里有变化的行都写它 |
| scene 空行释放（`ReleaseDetached`，实体已经没了） | 带 `B`，不带数据行；不判倒退，不拉低写权行的世代 |
| data_service 建角 | 初始行盖它自己二进制的世代；重放时库里的世代不超前就算同一批（`sameInitialRows`） |
| data_service 判死释放 | 只改写权行的代次、持有者、`released`，不动世代 |
| go/db 审计快照 | 只读：按提交后的全部行拼出快照，`persist_schema_gen` 填这一笔请求的世代 |
| data_service 角色交易的冻结证据 | 只读。存档世代比 data_service 新时不出证据（`decideFrozenEvidence`）。这一条有意保留：过户不可逆，宁可等 data_service 升级 |
| `go/data_service/cmd/debug_import`（只在本地用） | 存档世代比工具新就拒绝导入；写入时取本地与来源的较大值（`playerbundle.PlanImport`） |
| 搬库 `tools/dbmove`、PITR | 整行复制，不解释内容 |

每次改动这条链路之前，按 AGENTS §11.4 重新 grep 清点一次直接写 `player_store` 的语句。

### 5.5 已删除的判定（2026-10-30）

| 原判定 | 原来的位置 | 为什么不再需要 |
|---|---|---|
| C3、写方令牌、`warn` / `enforce` 模式 | Redis 严格提交屏障的 Lua | 玩家存档不进 Redis。令牌防的是多跳链路上有一跳截断；现在只有 scene → go/db 一跳，写权由写权行判 |
| C4 | login 冷加载 | login 不读玩家存档 |
| C0（未盖戳拒写）、引导期关闭标记 | go/db 消费 Kafka 写任务 | 项目未上线，没有机制上线前的写方；建角初始行就带世代 |
| C1、C1b、C2 | go/db 按列写、按列读 | 改成 `decideSave`、`decideClaim` 里的世代判定（§5.1）。按列读写的截断问题随存储改造消失（§6） |

这些判定当时的设计与取舍（写方令牌的五个候选方案、`warn` 证据、恢复工具的口径）见本文件的 git 历史与 §15 的 v0.6。

---

## 6. 机制三：go/db 不解释载荷

2026-10-19 起稿时玩家存档是两张按列存的表，每个顶层字段一列。新版 go/db 会把旧写方 body 里缺的新列写成零值，旧版 go/db 冷加载只读自己认识的列、读出截断的副本（L7）。当时的规则是「go/db 不写写方不认识的列」。

2026-10-30 起存档在 `player_store`：一个玩家一组行，`payload` 是 `player_database` 的一段序列化结果。**go/db 不解析 payload**，原样存、原样取。所以：

- go/db 的版本不影响任何字段。新字段加在哪一行、是顶层还是嵌套，go/db 都看不见。
- 加字段不需要 DDL。只有 `player_store` 表结构本身变化才需要迁移。
- 没有「只写改动列」的问题：scene 只发与上一次确认提交不同的行，每行整行重写；一次存盘是内存某一时刻的完整快照，所有有变化的行在同一个事务里提交（`player-save-direct-commit.md` §1.1）。

go/db 仍然要求自己「不比对方旧」。原因不在字段，在它和 scene 之间的协议：

| go/db 必须认识的 | 不认识时 |
|---|---|
| 行种（`PlayerStoreKind`）：认领回包的大小预算按「每行都到上限」算，行数由它认识的行种决定 | 存盘带了不认识的行种，回 `SCHEMA_TOO_NEW` |
| 审计字段（随存盘提交的流水与快照请求） | 存档照样提交，回包不置 `audit_outbox_written`，scene 留着下一笔再交 |

世代号把这件事收成一个数：新行种一定随新字段出现，新字段一定升世代，所以「go/db 认识的世代 ≥ scene 的世代、≥ 存档的世代」就保证 go/db 认识对方用到的行种。判定见 §5.1，发布顺序见 §8.2。

新加行种的约束（`proto/player_store/player_store.proto`）：取下一个空闲号，不回填、不复用；新行里只能放新字段，已有字段不许换行（生成器按台账的 `group=` 拒绝，§3.1）。这样旧 scene 见到号比自己认识的都大的行，就知道里面没有它认识的东西，可以不读不写（§4.2）。

---

## 7. 混版本矩阵

假设世代 6 新增了顶层字段 `X`（放在钱包那一行）、子消息字段 `y`，以及一种新行 `K`。全部 zone 的 go/db 已经是世代 6。

| 场景 | 结果 |
|---|---|
| 新版 scene(6) 存盘 | 写入，写权行世代 6 |
| 新版 scene(6) 接手世代 5 的存档，连续存盘多次 | 第一笔把写权行抬到 6，之后每笔 6 = 6 |
| **旧版 scene(5) 接手世代 6 的存档** | 认领成功。`X` 不进内存，由存盘状态机按行保管；`y` 跟着对象；`K` 那一行不读不写。存盘世代 max(5, 6) = 6 |
| 同上，玩家改了外观 | 只重写资料那一行。钱包行、`K` 行没有人动 |
| 同上，玩家花了钱 | 重写钱包行：货币是新值，`X` 原样接在后面 |
| 同上，玩家再回到新版 scene | `X`、`y`、`K` 都还是离开新版本时的值 |
| 旧版 scene(5) 对带 `y` 的物品合堆、按数量扣除 | 守卫拒绝或不计入（§4.4），不会把带新属性的物品当普通物品处理 |
| 金丝雀 zone(6) 的玩家跨到 stable zone(5)；别的 zone 的玩家从金丝雀 zone 回 home zone(5) | 同「旧版 scene 接手」 |
| 全部 scene 从 6 回滚到 5 | 同上。更新版本的功能不可用；持久值域按 §12.2 处理 |
| 旧版 scene(5) 认领过世代 6 的存档，实体没建起来，随后发空行释放 | 带的世代是 5、不带数据行：不判倒退，释放照常提交，写权行世代仍是 6 |
| 新版 scene(6) 连着旧版 go/db(5) | 认领回 `SCHEMA_TOO_NEW`，不抬代次，进场失败；已经在线的玩家存盘回 `SCHEMA_TOO_NEW`，保留快照重试。go/db 升上来之后恢复 |
| 存档世代 6，go/db 回滚到 5 | 认领回 `SCHEMA_TOO_NEW`。升回去 |
| 导入工具或有缺陷的写方带着数据行、世代却是 5 | `SCHEMA_REGRESSED`，不写 |
| data_service(6) 建的新号，本 zone 的 scene 还是 5 | 初始行世代 6，旧版 scene 认领成功 |
| data_service(6) 建的新号，本 zone 的 go/db 还是 5 | 认领回 `SCHEMA_TOO_NEW`，新号进不了场，到 go/db 升级为止。所以 data_service 不能先于 go/db 升级（§8.2） |
| 引导期（台账 `bootstrap`）：两个二进制都是世代 1，认识的字段不同 | 携带照常生效，上面「旧版 scene 接手」各行的结果不变；世代判定不起作用 |

---

## 8. 发布与回滚顺序

### 8.1 台账：引导期、封存、切到 `active`

- **引导期**（台账 `state=bootstrap`，现状）：生成器把新增字段一律记为世代 1，所有二进制的世代都是 1。项目未上线，这样上线前加持久字段不必每次升世代。R1–R3 照常检查。状态不能从 `active` 改回 `bootstrap`，生成器会拒绝。
- 引导期里世代判定不起作用（都是 1），**携带不受影响**：它不看世代号（§5.3）。
- **首次上线前封存**（`dev_tools.ps1 -Command persist-schema-seal`，§3.1「封存」）。封存之后引导期内再加持久字段，proto 生成直接失败。
- **上线后第一次发布带新持久字段的版本之前，把台账切到 `active`**（[release-checklist.md](../ops/release-checklist.md) §6.2）。此后每次出现新增字段的生成运行追加一个新世代。不切的话世代号永远是 1：「go/db 先于 scene」只剩行种那一道检查，世代倒退也发现不了。
- 发布预检 `release.persist_schema.sealed` 要求每个版本的台账封存到最大世代（§3.1）。
- 没有「机制上线前的二进制」需要兼容：首次上线的版本就带着这套机制，建角初始行就带世代。原先为存量部署设计的引导关闭标记、未盖戳拒写（C0）、`warn` 观察期都已删除（§5.5）。

### 8.2 每次升世代

`全部 zone 的 go/db → scene（可以按 zone 金丝雀）`

- **go/db 先于 scene。** scene 只连本 zone 的 go/db。`k8s_deploy.ps1` 的 `zone-up` 先发本 zone 的 go/db（写者回收 CronJob 跟着提前），staging / prod 恒等它滚完再发 scene，dev 只在 `-WaitReady` 下等，口径与迁移 Job 门禁相同（2026-11-12，r2 文档 §6「2026-11-10」批 S；之前是先发 scene、后发 go/db）。
- **全部 zone 的 go/db 先于任何一个 zone 的 scene。** 玩家会带着新世代的存档跨到别的 zone，那里的 go/db 也要认识这个世代。只靠逐 zone 跑 `zone-up` 时，已升级 zone 的玩家在别的 zone 的 go/db 升级之前进不了那些 zone（认领回 `SCHEMA_TOO_NEW`，不丢数据）。所以升世代的版本分两步部署（[release-checklist.md](../ops/release-checklist.md) §2.1 ⑦）：
  1. 对每个 zone 跑 `zone-up -ZoneId <z> -NodeImage <线上版本的节点镜像> -GoSvcTag <本版本 tag>`。显式给 `-GoSvcTag` 时 Go 服务的 tag 不跟随 `-NodeImage`，所以这一步只换 zone 内的 Go 服务（含 go/db），scene 不动；
  2. 全部 zone 都滚完之后，逐 zone 跑 `zone-up -ZoneId <z> -NodeImage <本版本节点镜像>`，可以先开金丝雀 zone（[release-track-gray-canary.md](./release-track-gray-canary.md) §2.7）。

  台账封存的世代（`sealed=<世代>:…`）比线上版本高，就是升世代的版本。
- **data_service 不早于 go/db。** 它建角时给初始行盖自己的世代。金丝雀期间它保持 stable、随 stable 一起升级（[release-track-gray-canary.md](./release-track-gray-canary.md) §2.5），本来就排在 go/db 之后。
- 没有 DDL 这一步，也没有 login：加字段不改表，login 不读玩家存档。

顺序被违反时不丢已提交的数据：认领和存盘回 `SCHEMA_TOO_NEW`，scene 进场失败或保留实体重试。代价是窗口期内新 scene 进不了人，已在线玩家的进度只在 scene 内存里；scene 进程在 go/db 升上来之前退出，这部分进度会丢。所以这个顺序仍然要守。

给收件箱载荷、资产通道请求加字段，或者新增币种、物品、宝宝表行时的次序，另见 §12 载荷行与 §13 第 7 条。

**不做部署前的世代预检**（2026-11-12 定）。原来列作可选项：go/db 的能力心跳带上世代号，`k8s_deploy.ps1` 发布 scene 之前核对全部 zone 的 go/db。不做的原因：
- 顺序错了不丢数据，运行期判定会拦住：认领、存盘回 `SCHEMA_TOO_NEW`；告警 `DbPlayerStoreSchemaMismatch` 是 critical，出现就报（§8.4）。
- 部署脚本要核对，就得知道镜像里编进去的世代：要给镜像加标签，再给 go/db 加查询接口。改动要穿过出包、发布清单和部署脚本，换来的只是早几分钟发现顺序错了。
- 本 zone 内的顺序 `zone-up` 已经机械地保证（go/db 先、scene 后）。跨 zone 的顺序由上面的两步部署保证，写进了发布清单。

### 8.3 回滚规则

| 对象 | 能否回滚 | 说明 |
|---|---|---|
| scene | 可以，回到任何带本机制的版本 | 旧版本原样带回它不认识的内容（§4.2），在新版本上存过盘的玩家照常进场，更新版本的功能不可用。两条约束始终适用：<br>- **语义耦合**（R5）：守卫覆盖不到的新字段不能靠滚动发布上线，更不能指望回滚后旧代码正确处理它；<br>- **持久值域**（§12.2）：回滚目标版本要含 D1–D6 与等级上限那一批（2026-10-23），更早的版本会在加载时删改超出自己值域的值。项目未上线，首次上线的版本就含这一批。改技能归属、复用表行 id、下调上限等始终按 §12.2 的约束处理。<br>回滚期间，用过新资产通道流号或新收件箱结局的玩家，资产通道 / 收件箱会停用到升回（§12.1 N8、N9，不丢数据）。W1–W3（装备槽位挪位、坐标拉回出生点、当前 HP / MP 夹到上限）不删资产，已接受 |
| go/db | 只能回滚到世代 ≥ 存档最高世代、也 ≥ 在跑的 scene 世代的版本 | 回滚得更低会卡住认领和存盘（`SCHEMA_TOO_NEW`），不丢已提交的数据。scene 进程在升回去之前退出，会丢在线进度。处置是尽快升回去 |
| data_service | 可以 | 它不解释载荷。回滚到比存档更低的世代时，角色交易的冻结证据出不来（§5.4），其余不受影响 |
| login | 不涉及 | 不读玩家存档 |

### 8.4 告警与运维手册

| 信号 | 来源 | 处置 |
|---|---|---|
| `DbPlayerStoreSchemaMismatch` | go/db 指标 `db_player_store_requests_total{result=~".*SCHEMA.*"}`，规则在 `deploy/k8s/player-store-alerts.yaml` | `TOO_NEW`：把 go/db 升到不低于 scene 与存档世代的版本，或把滚动发布走完。`REGRESSED`：有写方带着数据行却没带回加载到的世代，按 scene 日志定位玩家与写方 |
| `metric=persist_schema_regression where=player_store` | scene 的 ERROR 日志（LogQL 固定查询，Loki ruler 未接线） | 同上 `REGRESSED` |
| `metric=persist_version_skew`、`SceneCurrencyClawbackVersionSkew` | scene 日志与 `deploy/k8s/persist-version-skew-alerts.yaml`：守卫命中（§4.4、§12 载荷行） | 混版本窗口里预期会有；全量之后还有，按 `where` 定位 |
| `metric=player_store_claim_carried` | scene 的 INFO 日志：认领到本二进制不认识的内容（全进程第一次和此后每 1000 次打一条） | 混版本窗口里预期会有，只作确认用 |

运维手册在 `deploy/k8s/README.md`「玩家持久 schema 世代」。

---

## 9. 分批实施（每批 < 30 个文件，AGENTS §10.2）

**实施状态（2026-10-19）：**

> 2026-10-30 存盘改为直接提交 TiDB 时，第 2 批（go/db 的 C0～C2）、第 3b 批（Redis 屏障上的 C3 与写方令牌）、第 4 批（login C4）的代码随旧链路删除，判定改成 §5.1 的形式。下面各条按当时的状态保留，作历史记录；现行判定以 §5 为准。2026-11-12 的改动见 §15 v0.15。

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
  - **运行证据（10-30 部分）**：Codex 11-02 的全量 C++ 回归（`build/player-store-verify-20261102-r3-prep/06-cpp-tests/`）里，这批 16 条 C++ 用例都实际运行且通过，其中包括 10-30 新增的 `AssetOpSystemTest.QuantityDebitDeterministicRejectionsComeBeforeVersionSkewRetry`；currency_test 299 条、turn_battle_engine_test 300 条全绿。同一轮 bag_test 的 16 条失败与这批无关：13 条装备属性测试的数值超过了配置表上限，1 条宝宝加点用例属另一会话；`PersistRoundTripTest.UnknownFieldsOnEveryCarriedNodeSurviveLoadAndSave` 报的是新持久类型 `AssetRecallLine` 不在往返夹具里，已由别人在 `ccbb4af69` 补上夹具，尚未重跑。Go 侧（`currencytype`、收件箱入箱、交易托管撤单）没有运行记录。
- **第 7 批：已落码，未编译、未测试（Claude，2026-11-05）**。四件事：
  - 台账封存门禁（§3.1「封存」）：生成器 `persistschema` 的封存行解析、写回与核对（`ledger.go`、`apply.go`、`generate.go`），生成器命令行开关 `--persist-schema-seal`（`cmd/root.go`），`dev_tools.ps1 -Command persist-schema-seal`，发布预检 `release.persist_schema.sealed`，以及 `release-checklist.md` §2.1 ① / ⑦、§6.2。用例在 `persistschema_test.go`、`dev_tools_proto_gen_exit.tests.ps1`、`release_common_version.tests.ps1`。
  - 战斗结算的币种版本错位（§12 载荷行）：`player_battle.cpp` 的币种校验拆成三种结论（有效 / 不认识 / 坏载荷），收件箱执行器对不认识的币种回 `kVersionSkew`；用例 `PlayerBattleSettlementTest.InboxSettlementWithCurrencyUnknownToThisVersionReportsVersionSkew`。
  - 存储分组不得改（数据覆写审计 R2 的 N1，§3.1）：生成器读 `OptionPersistGroup`，台账给玩家表顶层字段记 `group=`，换组或删分组选项即失败，旧行首次补记。用例 `TestCollectSchemaReadsStorageGroupOnTableFieldsOnly`、`TestApplyRejectsStorageGroupChange`、`TestApplyRejectsRemovedStorageGroup`、`TestApplyBackfillsStorageGroupOnce`。
  - 评审修正：生成器加只核不写的 `--persist-schema-verify`（`dev_tools.ps1 -Command persist-schema-verify`），发布预检用它核摘要；封存违反按两种成因分别给处置，同一根因只报一次；`TestRepositoryLedgerIsCanonical` 同时核仓库台账的封存。
  - 收件箱域停用的周期补打：停用之后每 5 分钟补打一条 `metric=inbox_halted ... relog=1`（`kHaltedDomainRelogIntervalMs`），告警在实体存活期间一直亮着；`player-inbox-alerts.yaml` 的毒消息告警描述先排除版本错位。用例 `PlayerInboxSystemTest.HaltedDomainRelogsOnTheIntervalWhileTheEntityLives`。

  文件与 Codex 命令见[交接 §17、§19](../handoff/mixed-version-persist-handoff-20261019.md)。
- **第 8 批：已落码，未编译、未测试（Claude，2026-11-11）**。§12 载荷行剩下的两项，以及 §13 第 10 条的三项待拍板：
  - **载荷新字段**（§12）：收件箱载荷与资产通道请求一律整棵判未知字段（`unknown_field_carry::TreeCarriesNewerVersionFields`）。退役判据是该层消息的 reserved 段或台账退役行；bytes 字段（原样携带的实例）不往里解。
    - 收件箱：`player_inbox_registry::PayloadCarriesNewerVersionFields`，带新字段的条目按 `kVersionSkew` 停用本域，不调执行器。
    - 资产通道：第 8b 步 `where=asset_request`，回 RETRY + 1006、不记账。
    - 用例：`PlayerInboxSystemTest.PayloadCarryingFieldsFromNewerVersionHaltsWithoutExecuting`、`AssetOpSystemTest.RequestCarryingFieldsFromNewerVersionRetriesWithoutRecording`、`UnknownFieldCarryTest.TreeCheckWalksSubMessagesAndSkipsReservedNumbers`。
  - **新物品 / 新宝宝表行**（§13 第 10 条第 1 项，按推荐定案）：
    - Item / Pet 表行 id 只增不删：`data/schema` 里写 `(cfg_ids_permanent)`，导表器按台账 `tools/data_table_exporter/state/permanent_ids/*_ids.txt` 守住，表里少了台账里的 id 即整批不产出。
    - 资产通道第 8b 步 `where=asset_config`：「本节点表里查不到」改判为版本错位（RETRY + 1006、不记账）；配置号 0 仍是坏包。
    - 用例：`AssetOpSystemTest` 新增与改写 4 条（按数量扣出、入账、整件实例、重复配置号），回收用例改口径；导表器 `test_permanent_ids.py` 与 `test_schema_proto.py` 一条。
    - 战斗结算对查不到表的掉落 / 消耗当时仍按 C-2 剔除该项、记 ERROR 交人工；已在下面的「第 8 批收尾」改成整笔 DEFER。
  - **§13 第 10 条第 2、3 项**：照推荐定案，不改代码（见 §13）。

  文件与 Codex 命令见[交接 §24](../handoff/mixed-version-persist-handoff-20261019.md)。
- **第 8 批收尾：已落码，未编译、未测试（Claude，代码 2026-11-11、用例与文档 11-12）**。scene 收件箱战斗结算执行器遇到「本节点 Item / Pet 表查不到」改为整笔 DEFER、零副作用（§13 第 10 条）：
  - **物品**：`ApplySettlementToEntity` 在剔除封禁项、算实际可扣量之前判。消耗或掉落里有查不到的配置号，整笔返回 false，日志 `metric=persist_version_skew where=battle_item`（带 `side`、`config_id`）。`DropItemsWithoutConfigRow` 与 `metric=battle_inbox_item_effect_skipped` 已删。
  - **宝宝**：玩家带进这一局的宝宝，种类在本节点 Pet 表查不到时，`CollectApplicableInboxPets` 不再跳过这一只，整笔返回 false，日志 `where=battle_pet`（带 `pet_id`、`pet_table_id`）。宝宝已不在身上、宝宝属性池缺配不属于这条规则，仍只跳过这一只。
  - **不判版本错位**：不停用本域。收件箱照常本地退避重试（1s→60s），这一行热更到本节点之后续做即成功。
  - 用例（bag_test）：
    - `PlayerBattleSettlementItemTest.InboxDropWithConfigUnknownToThisNodeDefersWholeSettlement`；
    - `PlayerBattleSettlementItemTest.InboxConsumptionWithConfigUnknownToThisNodeDefersWholeSettlement`；
    - `PlayerBattleSettlementItemTest.InboxSettlementAppliesOnRetryOnceTheItemRowsAreHotUpdated`；
    - `PlayerBattleSettlementTest.InboxPetConfigUnknownToThisNodeDefersWholeSettlement`；
    - `PlayerBattleSettlementTest.InboxSettlementAppliesOnRetryOnceThePetRowIsHotUpdated`。

  文件与 Codex 命令见[交接 §25](../handoff/mixed-version-persist-handoff-20261019.md)。

| 批 | 内容 | 主要改动 | Codex 验证 |
|---|---|---|---|
| 1 | 台账；生成器（发号、冻结和投影检查、R2 / R3、"引导中"标记）；两个 option 及其标注；两张表的戳字段；生成常量与清单 | `proto/db/proto_option.proto`、各持久 proto 的标注、`proto/persist_schema_gen.txt`、`tools/proto_generator/protogen/internal/...`（新增生成器及测试）、生成产物 | 先 `proto-gen-build`，再 `proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`；protogen 跑 `go test ./...`；最后完整编译 C++ 与全部 Go module。生成的 .pb 文件如果超过 30 个，先按 §10.2 报告 |
| 2 | go/db：C0 / C1 / C1b / C2 及其接线，新的延后原因和死信原因，指标、告警、引导期标记，日志限速；cmd/migrate 的 DDL；save_barrier_restore、debug_import、data_stress 的配套改动 | `go/db/internal/kafka/{player_row_fence,key_ordered_consumer,store_failure,placement_route}.go`、`go/db/db.go`、`go/db/internal/metrics`、`go/db/internal/barrierrestore/scripts.go`、`go/data_service/cmd/debug_import`、`deploy/k8s/persist-schema-alerts.yaml`、测试 | go/db 与 data_service 跑 `go test ./...`、`go vet`；真库集成测试（自起一次性 MySQL / TiDB） |
| 3a | C++ 携带：两个 Carry 组件（在手写 loader 里创建）、统一入口、欠款与任务列表的携带、`SanitizeSkillList` 保留顶层未知字段、`ResetAssetOpStreamForEpoch` 改为 `Clear()`、§4.4 的两个守卫、回档快照改入口、盖戳、往返一致性测试与全组件夹具、受影响测试的修正 | `services/scene/player/{comp,system}/...`、`modules/currency/...`、`mission_marshal.cpp`、`player_skill.cpp`、`asset_op_ledger.cpp`、`modules/bag/item_store.cpp`、`snapshot_system.cpp`、bag_test 及 §4.3 列出的测试、vcxproj / CMakeLists。文件可能接近 30 个，必要时把守卫拆成 3a′ | `game.sln` 串行 `/m:1` 完整编译；新测试和所有存盘相关单测 |
| 3b | C++ 校验：严格屏障 Lua 增加到 11 个参数、`num_gt`、写方令牌（v0.6）、返回值 2 / 3、`OnSaved` 的处理与新回调（`core/system/redis.cpp`）、`BaseDeployConfig` 及部署渲染、`save_barrier_restore` 字段白名单 | `redis_client.h`、`core/system/redis.cpp`、`player_lifecycle.cpp`、`currency_test.cpp`（现有用例断言"返回 2 是坏回包"、字面量白名单、以 7 个参数直接 EVAL 的真 Redis 用例、调用文本契约，这几处都要改写）、`base_deploy_config` 的 proto / `config.cpp` / yaml、`k8s_deploy.ps1` 及其契约测试、`start_game.ps1` | `game.sln` 完整编译；Redis 脚本测试；k8s_deploy 契约测试 |
| 4 | login：C4（`beginParentLoad` 增加第 4 个返回元素并比较）、种子保持不盖戳的测试、backfill 往返测试、`login.yaml`；可选的能力心跳与部署预检；运维手册 | `go/login/internal/logic/pkg/dataloader/...`、`go/login/etc/login.yaml`、`deploy/k8s/README.md` | login 跑 `go test ./...`、`go vet` |
| 5 | §12 中持久值域的两个删数据点（这是 scene 无条件回滚的前提） | `player_skill.{h,cpp}`、`player_skill_handler.cpp`（`ListSkills`）、`bag_marshal.{h,cpp}`、`modules/bag/comp/player_bags_comp.h`、`modules/bag/bag_system.h`（`ContainsGuid`）、`asset_op_system.cpp`（入账查重）；测试 `cross_zone_test.cpp`、`skill_test.cpp`、`asset_op_system_test.cpp`、`persist_round_trip_test.cpp`（注释） | 同 3a，另跑 `skill_test` |
| 6 | 退役字段残留过滤；按数量扣除守卫；新币种取值表与版本错位 | `persistschema/{render,generate}.go`、`unknown_field_carry.h`、`modules/bag/{item_store,bag_system,bag_service}.*`、`asset_op_system.cpp`、`player_equip.*`、`player_battle.cpp`（快照一行）、`proto/common/base/currency_type.proto`、`currency.h`、`currency_system.cpp`、`gm_sanction_inbox_executor.*`、`player_mystery_merchant.cpp`、战斗节点奖励档两处、`go/shared/currencytype`、Go 各入口、`go/trade/internal/commerce/engine.go`（托管阶段撤单）；测试见交接 §14 | 生成产物已在库，无需重新生成；`game.sln` 串行 `/m:1`；bag_test、currency_test、turn_battle_engine_test；Go 受影响模块 `go test ./...`、`go vet ./...`；trade 真库用例需 `COMMERCE_TEST_DSN` |
| 7 | 台账封存门禁；战斗结算的币种版本错位 | `tools/proto_generator/protogen/internal/persistschema/{ledger,apply,generate}.go`、`cmd/root.go`、`proto/persist_schema_gen.txt`（只改表头注释）、`tools/scripts/{dev_tools,release_preflight}.ps1`、`docs/ops/release-checklist.md`、`cpp/libs/services/scene/battle/system/player_battle.cpp`、`player_inbox_{system.cpp,rules.h}`、`player_inbox_runtime_comp.h`、`deploy/k8s/player-inbox-alerts.yaml`；测试 `persistschema_test.go`、`dev_tools_proto_gen_exit.tests.ps1`、`release_common_version.tests.ps1`、`player_battle_settlement_test.cpp`、`player_inbox_system_test.cpp` | 先 `proto-gen-build`（生成器源码变了）；protogen `go test ./internal/persistschema/...`；两份 PowerShell 契约测试与 `k8s_deploy_contract` 的 LogQL 契约；`game.sln` 串行 `/m:1` 后跑 bag_test、currency_test |
| 8 | 载荷新字段判版本错位；Item / Pet 表行 id 只增不删与资产通道「查不到表」改判版本错位 | `cpp/libs/engine/core/utils/proto/unknown_field_carry.h`、`player_inbox_{registry.h,registry.cpp,system.cpp}`、`asset_op_system.cpp`、`data/schema/{cfg_options,item_table,pet_table}.proto`、`tools/data_table_exporter/core/{permanent_ids,schema,schema_proto,orchestrator}.py`、`docs/design/guild-phase2/04-asset-channel.md`、`data/AGENTS.md`；测试 `bag_instance_data_test.cpp`、`player_inbox_system_test.cpp`、`asset_op_system_test.cpp`、`test_permanent_ids.py`、`test_schema_proto.py` | 导表器 pytest；导表一次（生成两份台账，与产物一起提交）；`game.sln` 串行 `/m:1` 后跑 bag_test、currency_test |
| 8 收尾 | 战斗结算对本节点 Item / Pet 表查不到的物品、宝宝种类改整笔 DEFER | `cpp/libs/services/scene/battle/system/player_battle.{h,cpp}`；测试 `player_battle_settlement_test.cpp`；`docs/ops/player-inbox-runbook.md`（§5.2 A′、§6.1 表）、`docs/design/battle-result-recovery.md` | `game.sln` 串行 `/m:1` 后跑 bag_test |

每批完成后在 `docs/PROGRESS.md` 追加一条，并注明 Java 版的状态（§11）。

---

## 10. 测试计划

**C++**：
- 往返一致性测试（`bag_test` 的 `PersistRoundTripTest.*`），包括覆盖率断言（§4.3）；盖戳的三种组合（加载世代高于、等于、低于本二进制世代）；缺 Carry 组件整份拒存。
- 对象内携带与守卫（`currency_test`、`bag_test`，用例名见 §9 各批）：
  - 欠款删除后用同一个 `currency_type` 重建，不带旧的未知字段；不删除时保留。任务放弃后重新接取同理。
  - 带更新版本字段的物品不合堆、不按数量扣除；已有欠款带更新版本字段时 `AttachDebt` 返回失败、自动扣还跳过。
  - `ResetAssetOpStreamForEpoch`、神秘商人换轮之后未知字段为空。
- 行存储上的携带（`bag_test` 的 `player_store_client_test.cpp`，2026-11-12）：
  - 投影：`PlayerStoreProjectionTest.UnknownTopLevelFieldsOfARowAreCarriedOutsideTheMessage`、`CarriedTopLevelBytesCoverOnlyTheTopLevelOfARow`、`RowOfANewerKindIsLeftAlone`、`ProjectRefusesAMessageWithUnknownTopLevelFields`、`RetiredKindIsRejected`；
  - 状态机：`PlayerStoreClientTest.CarriedBytesOfARowAreWrittenBackWithEveryWriteOfThatRow`、`QueuedSnapshotKeepsItsCarriedBytes`、`PeriodicDeferComparesRowsWithTheirCarriedBytes`、`ClaimLeavesRowsOfNewerKindsUntouched`、`ClaimWithAnUnreadableRowHasNoConclusion`、`ReclaimReplacesCarriedBytes`、`ClaimWithRetiredKindHasNoConclusion`；
  - 生产接线：`cross_zone_test` 的 `PlayerStoreDirectEcs.ClaimCarriesWhatThisBuildDoesNotKnowBackIntoItsRow`。
- 世代拒存的处置：`cross_zone_test` 的 `ExitPersistEcs.Schema*`（退出中、交接中、在线）。

**Go**：
- `go/db/internal/playerstore`：`TestDecideSave`、`TestDecideSave_RowlessSaveIsNotARegression`、`TestDecideClaim` 逐格覆盖 §5.1。
- 真库（`-tags=integration`，TiDB）：
  - `TestIT_NoOwnerAndSchemaChecks`：旧 scene 认领更新世代的存档；带回世代的存盘提交，没带回的拒写；空行释放不拉低世代；存档比 go/db 新时认领被拒、不抬代次。
  - `TestIT_SceneNewerThanStore`：scene 比 go/db 新时认领被拒、不抬代次；带 go/db 不认识的行种存盘整笔不写。
- data_service：建角初始行的世代与重放比对，冻结证据的世代判定。

**生成器**：
- 新增字段追加世代；删除并写了 reserved 标为 retired；
- 以下情况生成失败：改类型、复用号、给 oneof 加成员、给冻结类型加字段、换存储分组、已封存的世代被增删字段；
- 输出稳定、可重复。

**真实环境**（由 Codex 或负责人执行，**还没有做过**）：加一个测试用的持久字段，把台账切到 `active`、升到世代 2，用新旧两套二进制按 §7 的矩阵逐行混跑。重点是：旧 scene 接手新存档后改同一行里的字段再存、跨 zone 来回、scene 回滚、go/db 与 scene 先后顺序颠倒。这一步不能用单测代替；跑完之前，不能宣称「混版本安全」。

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
| 持久值域：旧配置表删掉新技能 | 第 5 批前（以及不含第 5 批的回滚目标版本）：`SanitizeSkillList` 删除表里查不到的技能 | **第 5 批已落码**（Codex 10-24 运行旁证通过，§12.2）：保留，但不生效（`PlayerSkillSystem::IsConfiguredSkill`；`HasSkill`、`ListSkills`、战斗快照、`ReleaseSkill` 都只认生效的项）。附带约束：技能表行 id 删除后**不得复用**给另一个技能，否则留在老玩家数据里的不生效 id 会在复用后自动生效 |
| 持久值域：越界的包类型 | 第 5 批前（同上）：`bag_marshal::Unmarshal` 丢弃越界条目；`capacities` 只输出 `kBagTypeCount` 个 | **第 5 批已落码**（Codex 10-24 运行旁证通过）：越界条目与多出的容量存进 `PlayerBagsComp::unsupportedBagTypes`，原样存回；本版本的玩法看不到也不碰它们，但它们仍参与跨包 guid 唯一性检查（`PlayerBagsComp::ContainsGuid`，资产通道入账用它；漏查会让同一实例在新旧两个包各落一份，2026-10-19 对抗复核 D-1） |
| 持久值域：其他按配置表或代码常量解释的持久值（物品 config_id、任务 / 条件 config_id、宠物种类、属性加点维度、装备槽位、越界币种等） | **已核对（2026-10-22，只读代码 [C]，未编译，没有回滚场景用例）**，逐项见 §12.1：<br>- 会删 7 项（D1–D7）：属性加点维度、宝宝加点维度、属性加点超额整池返还、宝宝加点超额清空、职业技能重数压回、技能改职业 / 通用归属、战斗结算账本条数上限；<br>- 会改写 3 项（W1–W3）：装备栏槽位挪位、入场坐标吸附或拉回出生点、当前 HP / MP 夹到本版本上限；<br>- 其余 15 项原样保留，标"不适用"（N1–N15）：物品 config_id、堆叠数、包容量、动态包 profile、装备强化 / 词条 / 宝石、币种、任务 / 条件 / 成就 / 活动、资产通道账本、收件箱账本、限购记录、属性方案数、宝宝种类 / 数量 / 等级、职业、复活、不持久化的 buff / 称号 / 坐骑 / 时装。<br>原线索里"装备槽位挪位"属实（W1）；"查不到表的物品不丢""任务查不到表不删"属实（N1、N7）；"加点维度会删"属实（D1、D3） | **D1–D6 已于 2026-10-23 落码**（Codex 10-24 运行旁证通过）：原样携带、按本版本生效，见 §12.2；D7 已作废（2026-10-31 战斗结算账本移出玩家存档），W1–W3 接受。标"不适用"的值域随本次核对解除回滚限制；D1–D6 在回滚目标版本含这一批、且发布代码复跑通过之后解除（§8.3） |
| 持久值域：代码常量上限（等级上限 `playerlevel::kMaxLevel`） | 2026-10-23 前（以及不含这一批的回滚目标版本）：加载时 `ClampToMaxLevel` 把超限等级压回（`player_database_loader.cpp`），连带 `ConvergeOverAllocation` 整池返还加点、出战宝宝等级高于主人时取消出战，随下一次存盘落库；新版提高上限后回滚 scene，再升回新版也恢复不了（2026-10-20 对抗复核 O-1） | **2026-10-23 已落码**（Codex 10-24 运行旁证通过）：存储等级原样携带，玩法统一读 `playerlevel::CappedLevel`；超额的加点池整池不生效、不删；出战只在等级真的变化时取消（§12.2）。代价：下调上限不再自动收敛，要配显式迁移。回滚目标版本含这一批、且发布代码复跑通过之后，提高等级上限的版本才可回滚 scene |
| 收件箱、资产通道载荷的新字段与新枚举值 | **新枚举值（币种）第 6 批已落码**：取值表唯一来源是 `proto/common/base/currency_type.proto`；Go 各入口按同源取值表拒绝写错的值；scene 收件箱执行器遇到本二进制不认识的币种回 `kVersionSkew`（停用本域，实体重建后重拉），资产通道回 RETRY + `kFeatureUnavailable`（第 8b 步，不记账）。战斗结算的奖励币种同口径（第 7 批，2026-11-05）：battle 节点奖励档按同一张取值表校验，scene 收件箱战斗结算执行器遇到不认识的币种回 `kVersionSkew`（原来是 `kDefer`，本地续做只会空转），金额越界、重复币种这类坏载荷先判、照旧 `kDefer`。订正：此前这里写的"资产通道在已有改动之后遇到不认识的币种记 APPLIED_PARTIAL"不对，第 6 批之前实际是在第 8 步、任何改动之前记终态 REJECTED(kAssetInvalidBundle)。<br>**第 8 批（2026-11-11 落码，未编译）**：<br>- 载荷新字段：原先旧版执行器忽略新字段、记 APPLIED（Go 入箱拒未知字段，scene 不判）。现在收件箱载荷与资产通道请求整棵判未知字段，带本版本不认识、也不是已退役的字段即按版本错位处理：收件箱回 `kVersionSkew` 停用本域，资产通道第 8b 步回 RETRY + 1006。<br>- 新物品、新宝宝表行：原先资产通道入账查不到表时（物品 config、整件物品实例、宝宝 `pet_table_id` 三处，加上按数量扣出与 GM 回收）在第 8 步记终态 REJECTED，交易交付被拒后合同转人工，与加载侧原样保留（§12.1 N1 / N12）口径不一致。现在 Item / Pet 表行 id 只增不删（导表器守），资产通道把"本节点表里查不到"改判为版本错位（第 8b 步 RETRY、不记账），配置号 0 仍是坏包。<br>**战斗结算同口径（第 8 批收尾，2026-11-11 落码，未编译）**：scene 收件箱战斗结算执行器遇到本节点 Item 表查不到的掉落 / 消耗，或玩家带进这一局的宝宝种类在本节点 Pet 表查不到，整笔 DEFER、零副作用（`where=battle_item` / `where=battle_pet`），这一行热更到本节点之后本地续做即成功。原先的 C-2（只剔除查不到表的物品项、记 ERROR 交人工）与宝宝的 `pet_table_missing` 跳过作废：表行只增不删之后，剔除就是丢掉玩家这一局的掉落（交接 §25） | - 载荷新字段：已按"执行器遇到未知字段时 fail-closed"做（第 8 批）。发布次序：给收件箱载荷、资产通道请求加字段时，消费方 scene 全量之后生产方才写（§13 第 7 条）。<br>- 新物品、新宝宝表行：已按 §13 第 10 条定案做（第 8 批）。Go 各入口的配置存在校验：邮件附件、帮会商店已按同源物品表校验；交易交付的物品、宝宝来自卖方 scene 的托管原件，本来就是真实存在过的行。<br>- 新币种的发布次序：Go 侧全量升级完成之后才开始滚 scene（只有首次上线可以同版本整批起步）；scene 全量升级之前，GM 不得对新币种下处置，交易、邮件不得发放新币种 |
| Kafka 控制面命令 | 遇到未知事件直接丢弃 | 新增事件类型时配发件箱或重投机制 |
| guild、mail、trade 的库表 | 未核对 | 按本文的不变量逐个评估 |
| 客户端契约 | 见 `published-contract-compatibility-20261017.md` | 已有独立方案 |
| 已结（11-05 核对） | 原问题：scene 不 marshal `merge_state(10)` 和 `stress_test_probe(9)`，每次整行写都会把它们清空。现状：`merge_state` 已从玩家存档删除（`player_database` 的 10 号已 `reserved`）；`stress_test_probe` 仍是每次写盘重新打的探针，按设计标 `OptionPersistUncarried` | 不用再确认 |

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
| D7 | 战斗结算幂等账本：`settlement_ledger.applied` 的条数 | `kMaxAppliedRecords = 64`（`battle_settlement_ledger.h:32`） | 加载时不校验。运行期每次 `RecordApplied` 都把账本淘汰到 64 条以内（`:58`） | **会删**（运行期）。**2026-10-31 起不适用** | 新版本调大这个上限、某玩家累积超过 64 条后回滚：<br>- 下一局结算时会淘汰旧条目；<br>- 被淘汰的 battle_id 如果被重投，会重复发奖。<br>稳态下只有 0–1 条。建议作为发布约束（待拍板第 5 条）。<br>**作废（11-05 核对）**：结算改走 match SQL 加 player_inbox，`player_database.settlement_ledger`（17 号）已 `reserved`，`BattleSettlementLedgerComp` 与 `kMaxAppliedRecords` 已从代码里删除（`player-save-direct-commit.md`、`turn-battle-gap-closure.md` 头注），这个删数据点随之消失 |
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
- `IsConfiguredSkill` 目前只有 `skill_test` 调用；
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
   - 结果：照建议，不改代码，保留发布约束。2026-10-31 起作废：结算账本已移出玩家存档，这条约束随之取消（11-05 核对）。
6. **W1–W3**：建议接受，不纳入回滚门禁。理由：物品与资产不丢，位置和当前 HP / MP 不是资产。W1 若要求严格复原槽位，另立一批，改为"快照槽在容量内且空闲时原位占住、不生效"。
   - 结果：照建议接受，不改代码。

### 12.2 持久值域落码（2026-10-23～24，Claude；Codex 10-24 有构建与运行旁证）

> **来由**：2026-10-23 用户（负责人）确认项目未上线、存量玩家数据可以清空，要求按最标准的方式做完：数据强一致、代码可读、可水平扩展。据此把 §12.1 的 D1–D6 与等级上限一行改掉，不写存量迁移；W1–W3 按 §12.1 的建议不改代码；D7 起初保留为发布约束，2026-10-31 随战斗结算账本移出玩家存档作废。代码已随整仓快照提交入库（`5de132e2`、`a3c9b9b7` 等）。
> **运行证据（旁证）**：Codex 2026-10-24 在含这批改动的共享工作区实际构建 `game.sln`（0 错误），并全量运行 bag_test（708 条）、skill_test（76 条）、turn_battle_engine_test（287 条），均 0 失败，这批新增用例全部在列且通过（PROGRESS「2026-10-24 07:38 UTC」「07:43 UTC 后继实际结果」两条；原始 gtest XML 在 Codex 证据目录 `cpp-current1884-post-full-gtests-evidence-r1/run-20261024T071358902Z-25be95de/`，本会话 10-30 按用例名逐条核对）。这不是本主题专门的验证：构建前没有封存源快照，也没有做真实回滚演练；10-29 之后别的会话又改过 `player_battle.cpp`、`class_skill_test.cpp`，并新增了一条任务等级用例。最新状态的复跑清单见 [交接 §13.2](../handoff/mixed-version-persist-handoff-20261019.md)。
> **11-02 r3 回归**（玩家存盘重构那条线的 Codex 运行，`build/player-store-verify-20261102-r3-prep/06-cpp-tests/bag_test.stdout.log`）：本主题在 bag_test 里的用例，含 10-30 新增的宝宝生效等级两条与"只剩未知键时洗点"两条，都通过；只有角色与宝宝的 `AutoAllocateSuggestsOnlyThePointsLeftAfterUnknownKeys` 两条失败，原因是夹具没加载自动加点方案表，`AutoAllocate` 回了"无自动加点方案"。11-02 的 `ccbb4af69` 已给两个夹具补 `AttributeAutoPlanTableManager::Instance().Load()`，补完后还没有复跑。

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
| 等级上限 | - `playerlevel::CappedLevel` 取代 `ClampToMaxLevel`：0 按 1，超过 `kMaxLevel` 按上限。<br>- 加载不再压回，超限只打 WARN（`metric=level_above_cap`）。<br>- 玩法读点全部改走 `CappedLevel`：属性的 `PlayerLevel`、宝宝的 `OwnerLevel`、技能、装备穿戴、战斗快照、接任务的等级事件、技能伤害公式、回血 buff。<br>- 直读 `LevelComp.level()` 只留在四处：加载器、存盘、`GmSetLevel`、管理端 dump。这四处的直读在同一行带写明场合的标记注释，由契约测试 `tools/scripts/tests/test_cpp_level_comp_reads.py` 扫描 `cpp/libs`、`cpp/nodes` 守住（2026-11-05 补，**未运行**）。<br>- 玩家的读点缺 `LevelComp`（加载不变量被破坏）一律 fail-closed，不按 1 级补默认值（2026-11-05 补，**未编译、未运行**）：技能读接口按没有技能 / 被动答，施法入口回 `kServiceUnavailable`（不再答成"没有这个技能"），初始技能不补发；穿戴回 `kEquipInternalError`；回血这一跳不回；带等级条件的任务在写任务之前拒绝（`kServiceUnavailable`）。技能与穿戴缺职业组件时同样处理。非玩家没有等级组件，仍按 1 级。 | - 存储等级原样往返，旧版本按自己的上限生效，升回新版本后恢复。<br>- `GmSetLevel` 覆写超上限的存储等级时打 WARN（GM 只在 dev / test 开放）。 |
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

1. **已结（2026-10-30）**：「只写改动列」改为「超前延后、倒退拒写、未盖戳拒写、其余整行写」，负责人 2026-10-19 确认过。存储改成不透明的行之后 go/db 不再按列写，这一条随之不需要了（§6）。
2. **option 的读取方式**：推荐生成器动态读取，不升级外部模块。
3. **已结（2026-10-30）**：C3 / C4 的 `warn` / `enforce` 两段式随 Redis 屏障、login 冷加载删除。现在带数据行的世代倒退一律拒写（§5.1）。
4. **引导期不升世代**（§8.1）：项目上线之前台账停在 `bootstrap`，所有二进制都是世代 1；首次上线前封存，上线后第一次加持久字段之前切到 `active`。
5. **冻结清单、R2 / R4 / R5，以及 §4.4 的守卫**会改变今后的开发习惯：
   - 背包布局层、任务容器层不能再直接加字段；
   - 带新属性的物品，在旧版里不会被合堆；
   - 带新字段的欠款，在旧版里不允许再挂新欠款；
   - 带新属性的可叠加物品，在旧版里不能按 config 消耗、交易、带进战斗（第 6 批，fail-closed，发布单写明可用性降级）。
   这些需要负责人认可。
6. **残余风险**：
   - 超出守卫范围的语义耦合（R5）；
   - 旧版本接手更新版本的存档之后，更新版本的功能不可用，带新字段的对象被守卫冻住（§4.4）。这是可用性降级，不丢数据；
   - 引导期里所有二进制都是世代 1，「go/db 不比 scene 旧」只靠每笔存盘的行种检查兜住（§5.3）；上线后台账切到 `active` 才完整；
   - 从没做过真实的新旧版本混跑（§10）；
   - 第 6 批的版本错位都不记终态，收口靠升级走完：资产通道的 RETRY 没有次数上限，截止为 0 的入账流（邮件、交易交付）会一直 PENDING 到 scene 升级；这类行不要人工终结（入账没有 Abort 栅栏，人工终结还会在流上留下永久的洞，累计超窗要抬纪元，04 §4.38）；收件箱版本错位停用本域，同域排在后面的处置一起延后；停用期间每 5 分钟补打一条日志，告警一直亮着（第 7 批）。
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
   - 新字段被旧写方原样携带时，语义是否仍然成立；
   - 给收件箱载荷、资产通道请求加字段（2026-11-11）：消费方 scene 全量之后，生产方（Go 各服务、battle）才开始写新字段。scene 遇到不认识的字段一律按版本错位停下（§12 载荷行），不会照常执行，所以次序错了只是延后，不丢语义；
   - 删 Item / Pet 表行：导表器拒绝（`cfg_ids_permanent`，§13 第 10 条）。不再产出的物品、宝宝去掉掉落、商店、奖励里的引用，表行保留。
8. **已结（2026-10-30）：恢复工具的 `schema_gen` 口径**。`save_barrier_restore` 随 Redis 存盘删除。PITR 按整库恢复，写权行的世代与数据行一起恢复，不需要单独的口径；单 zone 回档已退役。原问题与 2026-10-22 的定案见本文件的 git 历史。
9. **已结（2026-10-30）：前任实体的存盘活得比实体长**。直接提交的路径上，存盘未落定时不能重新播种（`PlayerStoreClient::Seed` 返回 false），认领要等这名玩家的存盘全部落定（`RequestClaim` 的 `waitingForSettle`），所以同一节点上新实体读到的一定是前任落定之后的状态；跨节点的前任由写权行的代次与持有者挡住。原问题与当时的修法见本文件的 git 历史（2026-10-22）。

10. **已定（2026-11-11，负责人要求按最标准的方式做完，三项都按推荐定案；第 1 项落码，未编译）**（第 6 批，2026-10-30 登记）：
    - **资产通道遇到本版本查不到表的物品 / 宝宝种类**（§12 载荷行）：采用推荐，与新币种同口径。
      - 表行规则：Item / Pet 表行 id 一经出现就只增不删、不复用。由导表器机械守住：`data/schema/{item,pet}_table.proto` 写 `(cfg_ids_permanent)`，台账 `tools/data_table_exporter/state/permanent_ids/*_ids.txt` 记下出现过的全部 id，表里少了台账里的 id 即整批不产出。原推荐里的「下线改开关」不需要：不再产出就去掉掉落、商店、奖励里的引用，表行保留，已持有的物品照常生效。从未发布过的行要删，在同一次提交里删掉台账里的 id 并写明理由。
      - 有了这条规则，"表里查不到"只可能是更新版本或尚未热更到本节点的行。资产通道第 8b 步回 RETRY + 1006、不记账（`where=asset_config`）；配置号 0 在任何版本上都不合法，仍在第 8 步记终态拒绝。
      - Go 各入口的配置存在校验：邮件附件、帮会商店已按同源物品表校验；交易交付的物品、宝宝来自卖方 scene 的托管原件。
      - 原先担心的「已删行的入账永远 PENDING」由表行规则排除。
      - 战斗结算同口径（第 8 批收尾，2026-11-11 落码，未编译；交接 §25）。原先的 C-2（2026-10-30：只剔除查不到表的掉落 / 消耗、记 ERROR 交人工）是按「配置行可能被热更删掉」定的；表行只增不删之后，剔除就是丢掉玩家这一局的掉落。现在 scene 收件箱战斗结算执行器见到本节点 Item 表查不到的配置号，整笔 DEFER、零副作用（`where=battle_item`），这一行热更到本节点之后本地续做即成功，与号段暂缺、跨区冻结同一条路。
        - 判 DEFER，不判 `kVersionSkew`：版本错位会停用整个域，要等实体重建才重拉；配置表在进程内热更，行到了本地重试就能成功。币种不一样，取值表编在二进制里，会话内永远不会认识，所以币种回 `kVersionSkew`。
        - 宝宝同理（2026-11-12，核对同类入口时发现）：玩家带进这一局的宝宝，种类在本节点 Pet 表查不到时，原先只跳过这只宝宝的战后终值回写（`pet_table_missing`），现在同样整笔 DEFER（`where=battle_pet`）。宝宝已不在身上、宝宝属性池缺配不属于这条规则，仍只跳过这一只。
        - 代价：这一行热更到本节点之前，这名玩家这一局的金币、掉落、任务进度都不到账，同域排在后面的局也等着，Match 的 ready 票不放。发布次序照第 7 条（消费方 scene 的表先到，生产方后产出），次序错了只是延后，不丢。
    - **GM 对新币种的处置是否改为"按 ID 透明执行"**：维持现状，不改代码。封锁、解封、挂欠款对币种只做按 ID 的集合或映射操作，旧 scene 照写本可以与新版本语义一致；但改为透明执行要放宽三个原语的契约，将来原语一旦按币种做事（例如查余额），这条前提会悄悄失效。发布纪律已要求 scene 全量之前不对新币种下处置，现状（回 `kVersionSkew` 停用本域，实体重建后重拉）只在纪律被打破时生效，且不丢数据。
    - **新币种的启用开关**：在第一次真正新增币种时再做（YAGNI，AGENTS §11.2），在那之前靠第 7 条的发布次序。做法届时照原推荐：把"二进制认识"与"运营已启用"分开，Go 入口判 `IsKnown(v) && enabled(v)`，`enabled` 来自部署配置白名单，上线顺序与收件箱 `EnqueueTypes` 一致（scene 全量 → 改配置开启）。

11. **已定（2026-11-05 落码，未编译）：台账的机器门禁用构建期封存，不做运行期指纹**（§3.1「封存」，审计 r2-ps#1 / r2-ps#10）。封存从源头堵住「引导期新字段并进已上线世代」与「合并带进已发布世代」两种来源，发布预检用生成器核每条封存的摘要。剩下的漏洞靠 §3.1「只在主干发号」与 `release-checklist.md` 的发布纪律：部署绕过发布流程（用未封存或未经预检的产物上线、或把发布分支自己追加的世代推上线）；台账与 proto 脱节时封存（§2.1 ① 要求封存前先跑一次生成）。台账包的单测 2026-11-05 已进 CI（`go-modules-ci.yml` 的 `persist-schema-ledger` job，未在 CI 上跑过）。运行期指纹不做：2026-11-12 起携带不看世代号，封存保住的只是「go/db 不比 scene、不比存档旧」这道门禁（§3.1「封存」）。

12. **已定（2026-11-12，负责人要求查出的问题当场修、按最标准的方式做）：旧版本接手更新世代的存档**（§4.2、§5.2）。
    - **采用**：认领只在存档或 scene 的世代高于 go/db 认识的时候拒绝；scene 原样带回自己不认识的内容（子消息跟着对象，行里的顶层字段由存盘状态机按行保管，更新版本的行不读不写），存盘世代取 max(自己的, 加载到的)。这就是 protobuf 的标准做法：读改写时保留未知字段、不按自己认识的字段重拼，加上「读方先于写方升级」的发布顺序。
    - **否决：继续在认领时拒绝**。不丢数据，但升了世代的版本不能金丝雀、不能回滚（§5.2）。
    - **否决：每个持久字段都分两版发布**（版本 N 只认识、不写，N 全量之后 N+1 才写）。这能让所有在线版本都认识全部字段，但每加一个字段都要多一个完整的发布周期。携带已经保证不丢，两版发布只留给语义耦合的字段（R5，例如会被普遍写入的 `ItemComp` 新字段）。
    - **否决：按字段号位图判「写方缺不缺字段」**（xuanming-server 仓 data_service 缓存的做法）。那是给按列读取、会产出缺字段副本的缓存用的；这里 go/db 不解析载荷，scene 也不按认识的字段重拼，不存在缺字段的副本。
    - **否决：把行里的未知顶层字段并进 Carry 组件**。各行拼成一份消息之后分不清字段属于哪一行，写不回原来的行。
    - **否决：删掉世代号**。它还负责「go/db 不比 scene、不比存档旧」和发现没把内容带回来的写入（§5）；代价只是写权行上一个整数列。

---

## 14. 证据索引

- **C++**：
  - `cpp/libs/services/scene/player/system/`：`player_persist_marshal.h`（统一拼装入口、盖戳）、`player_store_projection.{h,cpp}`（按行投影与拼装、`CarriedTopLevelBytes`、`IsNewerKind`）、`player_store_client.{h,cpp}`（存盘状态机，按行保管未知字段）、`player_lifecycle.cpp`（`MakePlayerStoreOptions`、`HandlePlayerStoreClaimed`、`SubmitPlayerSave`、`HandlePlayerSaveSchemaRejected`）、`player_database_loader.cpp`、`player_data_loader.h`（生成产物）、`mission_marshal.cpp`、`bag_marshal.{h,cpp}`、`player_skill.cpp`、`player_attribute.cpp`、`player_pet.cpp`；
  - `cpp/libs/services/scene/player/comp/player_persist_carry_comp.h`、`cpp/libs/engine/core/utils/proto/unknown_field_carry.h`；
  - `cpp/libs/modules/currency/{comp/player_currency_comp.h,system/currency_system.cpp}`、`cpp/libs/modules/mission/system/mission.cpp`、`cpp/libs/modules/bag/item_store.cpp`；资产通道账本 `asset_op_ledger.cpp`；
  - 生成产物 `cpp/generated/persist_schema/persist_schema_gen.h`；
  - 测试：`cpp/tests/bag_test`（`persist_round_trip_test.cpp`、`player_store_client_test.cpp`、`player_feature_persistence_test.cpp`）、`currency_test`、`cross_zone_test`。
- **go/db**：`go/db/internal/playerstore/`：`decide.go`（写权与世代判定）、`store.go`（`Save`、`LoadAndClaim`）、`audit_outbox.go`（审计快照按行拼装）；`go/shared/generated/persistschema`。
- **data_service**：`go/data_service/internal/store/`：`account_role_initial_store.go`（建角初始行）、`player_store_dead_holder.go`（判死释放）、`account_role_store_proof.go`（角色交易的冻结证据）、`player_store_access.go`（展示读）；`go/data_service/internal/playerbundle`、`cmd/debug_import`、`cmd/debug_fetch`。
- **生成器与台账**：`tools/proto_generator/protogen/internal/persistschema/`、`proto/persist_schema_gen.txt`、`proto/db/proto_option.proto`。
- **proto**：`proto/player_store/player_store.proto`、`proto/common/database/{player_cache,mysql_database_table,bag_quest_mail_data}.proto`。
- **部署与运维**：`deploy/k8s/player-store-alerts.yaml`、`deploy/k8s/persist-version-skew-alerts.yaml`、`deploy/k8s/README.md`「玩家持久 schema 世代」、`docs/ops/release-checklist.md` §2.1、§6.2。
- **Java**：`D:\luyuan\xuanming-server-mmo\game-player-store\src\main\java\com\game\player\store\{PlayerStore,PlayerMapper,PlayerStateRow}.java`。
- **关联文档**：`player-save-direct-commit.md`、`release-track-gray-canary.md`、`ecs-component-access-rules.md`、`bag-instance-layout-split.md`、`data-overwrite-rollback-audit-r2-20261019.md`、`published-contract-compatibility-20261017.md`。旧存盘链的 `db-write-fencing-mysql.md`、`db-task-delivery-and-retention.md` 只作历史参考。

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
| 11-05 收尾核对 | D7 作废；§12「待确认」一行结案；补记 r3 结果 | 10-31 战斗结算账本移出玩家存档（17 号 `reserved`），`kMaxAppliedRecords` 已删；`merge_state` 同时删除。r3 里两条自动加点用例因夹具缺表失败，夹具已补 |
| 不加 `HasUnknown` 守卫 | 技能条目、加点方案、宝宝实例本层的新字段按 R5 处理（§4.4 由第 6 批会话登记） | `HasUnknown` 分不出新版本字段与退役字段的残留，会把对象永久冻结。第 6 批会话计划用台账的 retired 行修正这个判定，届时可以复用 |

### v0.10（第 7 批：台账封存门禁、战斗结算的币种版本错位、收件箱停用补打，2026-11-05）

用户第三次要求按最标准的方式做完。按服务器缺口清单第 13 条（`docs/handoff/server-gap-audit-20261101.md`）补两件事；10-30 那次独立复核因 API 鉴权失败没跑成，第 6 批的 C++ 部分改以 Codex 11-02 的实际回归为证据（§9）。

| 项 | 调整 | 原因 |
|---|---|---|
| 台账机器门禁（§3.1、§8.1、§13 第 11 条） | 台账加封存行，生成器每次核对已封存世代不变；`dev_tools.ps1 -Command persist-schema-seal`；发布预检 `release.persist_schema.sealed` | 审计 r2-ps#1 / r2-ps#10：同一世代号对应两种字段集合时，只比世代号的五处校验全部失效，旧写方静默丢新列。原来只有发布清单的人工核对 |
| 不做运行期指纹 | 只做构建期封存 | 运行期指纹要改戳列、C1～C4、屏障 Lua、恢复工具，约 30 个文件；构建期门禁已经从源头堵住已知的两种来源 |
| 战斗结算的币种（§12） | 不认识的币种回 `kVersionSkew`，坏载荷照旧 `kDefer` 并且先判 | 与 GM 处置、资产通道同口径；会话内永远不会认识这个值，`kDefer` 的本地续做只会空转 |
| 收件箱停用补打 | 停用之后每 5 分钟补打 `metric=inbox_halted relog=1` | 第 6 批对抗复核：停用对实体一直有效，只打一条的话 [10m] 告警会熄灭；10-30 时相关文件被别的会话占用没做 |
| 存储分组不得改（§3.1、R4） | 台账给玩家表顶层字段记 `group=`，换组或删分组选项即生成失败，旧行首次补记 | 数据覆写审计 R2 的 N1：加载只认当前分组，换组后首次载入清掉旧行里的字段、新行里还没有，首次存盘永久写丢；`player-save-direct-commit.md` 原写「换组必须安全」只防住了翻倍 |
| 评审修正（同日只读复核） | 预检调生成器 `--persist-schema-verify` 核摘要；封存违反分成因报、同一根因只报一次；预检函数挪回正确位置；文档指针改为 §2.1 ① | 只比世代号会放过「合并带进同世代的行」（中）；其余为低危 |

### v0.11（审计 r2-ps#6 / r2-ps#7：欠款自动扣还守卫、神秘商人换轮 `Clear()`，2026-11-05）

| 项 | 调整 | 原因 |
|---|---|---|
| 欠款自动扣还（§4.4 新增一行） | `AddCurrency` 遇到带更新版本字段的欠款不扣还、不删除，按冻结处理、收入全额入账，打 `metric=persist_version_skew where=currency_clawback` | r2-ps#6：旧版按 owed−paid 扣、扣满整条删除，新字段随之消失；原先 §4.4 只守了合并 |
| 神秘商人换轮（§4.4「逻辑重置」） | `RecordPurchase` 换轮改为 `Clear()` 后重设轮次 | r2-ps#7：逐字段清空让更新版本加的按轮字段跨轮存活，与 §4.2 L7 不一致 |
| 合并一行的理由改写 | 收件箱执行器回 `kVersionSkew` 的理由不再写"欠款会被 AddCurrency 还清删除"，改为本地续做看不到作废、错位不消失时续做只会空转 | 结论不变；自动扣还不再删带新字段的欠款，原理由随本批失效 |

### v0.12（第 8 批：载荷新字段判版本错位、Item / Pet 表行只增不删、§13 第 10 条定案，2026-11-11）

| 项 | 调整 | 原因 |
|---|---|---|
| 载荷新字段（§12 载荷行） | 收件箱载荷与资产通道请求整棵判未知字段（`unknown_field_carry::TreeCarriesNewerVersionFields`，退役判据为 reserved 段或台账退役行）；收件箱回 `kVersionSkew` 停用本域，资产通道第 8b 步 `where=asset_request` 回 RETRY | 原先旧版执行器忽略新字段、照常执行并记 APPLIED，新字段的语义静默丢失；§12 写明「开启收件箱消费之前必须修」 |
| 表行规则（§13 第 10 条第 1 项） | Item / Pet 表写 `(cfg_ids_permanent)`，导表器按台账拒绝删行；资产通道第 8b 步 `where=asset_config` 把"本节点表里查不到"改判为版本错位，配置号 0 仍是坏包 | 负责人 2026-11-11 要求待拍板项按标准做法定案；表行只增不删之后，"查不到"与"已删"不再混淆，RETRY 不会永远卡住 |
| §13 第 10 条第 2、3 项 | GM 处置维持 `kVersionSkew`；新币种启用开关等第一次新增币种时再做 | 前者放宽原语契约的风险大于收益；后者 YAGNI，发布次序已覆盖 |
| 评审清单（§13 第 7 条） | 加两条：给载荷 / 请求加字段的发布次序；删 Item / Pet 表行由导表器拒绝 | 与本批规则同步 |
| 当时未改（v0.13 已改） | 战斗结算的 C-2（剔除查不到表的掉落 / 消耗）应改为整笔 DEFER | 前提随表行规则改变；`player_battle.cpp` 当时被别的会话编辑（交接 §24） |

### v0.13（第 8 批收尾：战斗结算对查不到表的物品 / 宝宝种类改整笔 DEFER，2026-11-11 ~ 12）

| 项 | 调整 | 原因 |
|---|---|---|
| 战斗结算的物品（§12 载荷行、§13 第 10 条） | 掉落 / 消耗里有本节点 Item 表查不到的配置号时整笔 DEFER、零副作用，日志 `where=battle_item`；删掉 C-2 的剔除与 `metric=battle_inbox_item_effect_skipped` | C-2 的前提「行可能被热更删掉」已被表行规则排除，剔除等于丢掉玩家这一局的掉落；资产通道已回 RETRY，战斗结算是剩下的不一致点 |
| 战斗结算的宝宝 | 玩家带着的宝宝种类在本节点 Pet 表查不到时整笔 DEFER，日志 `where=battle_pet`；不再按 `pet_table_missing` 跳过这一只 | 同一条表行规则，跳过会永久丢掉这只宝宝的战后终值；核对同类入口时发现，一并改 |
| 判 DEFER，不判版本错位 | 收件箱本地退避重试，不停用本域 | 配置表在进程内热更，行到了本地续做即成功；`kVersionSkew` 要等实体重建才重拉 |

### v0.14（数据审计遗留：补缴跳过的进程计数与告警、免除 / 调整 / 冻结欠款的入口复核，2026-11-12）

| 项 | 调整 | 原因 |
|---|---|---|
| 欠款自动扣还（§4.4） | 跳过补缴时除了那一行 WARN，同一时刻计一次进程计数（`currency_clawback_stats.h`），scene 导出 `scene_currency_clawback_version_skew_total`（`currency_metrics.cpp`），告警 `SceneCurrencyClawbackVersionSkew`（`deploy/k8s/persist-version-skew-alerts.yaml`，warning、出现就报），契约测试 `tools/scripts/tests/persist_version_skew_alerts_contract.tests.ps1` | v0.11 之后它只有日志，Loki ruler 没接线，没人会被叫到；健康系统里恒为 0，非 0 = 有 scene 停在旧版本、被回滚，或发布次序被打破 |
| 免除 / 调整 / 冻结欠款（§4.4 合并一行末句） | 复核结论：`WaiveDebt` / `AdjustDebt` / `FreezeDebt` 没有任何生产入口，GM RPC 已删除（`proto/message_id.txt` 106 / 113 / 109 记为 reserved），收件箱只有挂欠款一个类型；只有单测调用。守卫不预先加，接入口时必须先补，`currency_system.h` 三个声明前写明了这条 | 原文写「GM RPC 仍是桩」已过时；没有调用方的守卫按 YAGNI 不写，但契约要写在函数旁边，接入口的人看得到 |

### v0.15（按存盘直接提交的路径改写；旧版本可以接手更新世代的存档，2026-11-12）

负责人问「旧服读了新服的数据，改别的字段再保存，会不会把新字段丢掉」。核对发现：2026-10-30 存盘改为直接提交 TiDB 之后，本文 §5～§8 讲的还是已经删除的 Redis / Kafka / 按列存储那条链；而代码在认领时按 scene 的世代整份拒绝，旧版本根本拿不到新存档。负责人要求查出的问题当场修、按最标准的方式做。

| 项 | 调整 | 原因 |
|---|---|---|
| 认领（§5.1、§5.2） | `decideClaim` 不再因「存档世代比 scene 新」拒绝，改为「存档世代比 go/db 认识的新」才拒绝 | 原先升了世代的版本不能金丝雀、不能回滚：在新版本上存过盘的玩家进不了旧 scene，包括回自己的 home zone |
| 行里的未知顶层字段（§4.2） | 不进内存，由存盘状态机按行保管、写这一行时接回去（`Options::carriedBytes`、`State::carried`）；`AssemblePlayer` 不再因它拒绝加载，也不再丢掉退役字段的残留 | 一行整行重写，并进消息之后分不清字段属于哪一行；退役字段的残留在回滚目标版本里可能还在用 |
| 更新版本才有的行（§4.2） | 认领时跳过、不播种，存盘不写 | 行种的号只增不回填，新行里只有新字段；原先整份拒绝加载 |
| 空行释放（§5.3） | 不带数据行的存盘不判世代倒退；写权行的世代在写入语句里取 `GREATEST` | 实体已经没了的释放手里没有加载世代，旧 scene 持有更新世代的存档时会被判倒退，永远提交不了 |
| 文档 | §0～§2、§3.3、§5～§8、§10、§13、§14 按现行路径改写；§9 标明已删除的第 2、3b、4 批 | 这几节讲的判定点、发布顺序、告警在代码里已经不存在 |
| 发布顺序（§8.2） | 升世代的版本分两步部署：先对每个 zone 用 `zone-up -NodeImage <线上版本> -GoSvcTag <新>` 只换 Go 服务，全部滚完再换 scene；写进 [release-checklist.md](../ops/release-checklist.md) §2.1 ⑦ | 玩家带着新世代的存档跨 zone，只逐 zone 一次换完会让已升级 zone 的玩家进不了别的 zone |
| 没有做 | 不删世代号；不做「先升二进制、后启用新世代」；不改角色交易冻结证据的世代判定；不做部署前的世代预检 | 见 §13 第 12 条、§5.4、§8.2 |

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
