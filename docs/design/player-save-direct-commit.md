# 存盘直接提交 TiDB:落码规格

> **状态**:v3 — 2026-11-05:§1.1 按「大小 × 变化频率」重新分行(钱包、背包、位置拆开,退役 kHot、kAsset),每行声明存盘方式,位置只顺带写;第一批代码已写,**未编译、未测试**(§1.1「落码」)。2026-11-06:第二批删掉 `player_database_1` 与 kLegacy1(§1.2),代码已写,**未编译、未测试**。v2 — 2026-10-30,已按[对抗评审](../handoff/player-save-direct-commit-20261030/spec-review-1.md)修正。B1~B7 代码已写,**未编译、未测试**。当时的过渡开关 `PlayerStorePath: redis | direct` 同日已随 B8 删除:直接提交 SQL 是唯一的存盘路径,玩家存档不经过 Redis(§8 末「2026-10-30」一条)。
> **上游**:[总体方案](./player-data-tidb-scaling-plan.md) §3、§4(本文是它 §13 的 A0,细化 A1~A6);[TiDB 决策 §9](./global-data-layer-tidb-decision.md)(方向与键族,本文回答 §9.4)。
> **现状依据**:[三路盘点](../handoff/player-save-direct-commit-20261030/survey.md),行号以它为准。标注:[C] 读代码确认,[D] 文档记载,[I] 推断。
> **用户已定**:玩家数据只放 TiDB;存盘服务放 go/db;关键变化立即存 + 拆出热行;正常运行不丢、不回档;存盘不再按归属区选 topic 和库。

## 0. 结论

1. **一张新表 `player_store`,主键 `(player_id, kind)`。** 同一玩家的所有行挨在一起;`kind=0` 是写权行,其余每个 kind 一行,`payload` 是一组字段的序列化结果。一条语句就能读出一个玩家的全部行。
2. **go/db 提供两个 RPC:`LoadAndClaim`、`Save`。** 都是一个 TiDB 事务,先 `FOR UPDATE` 锁写权行,再判定、再读写。
3. **写权以 TiDB 写权行为准,代次的铸造不变。** scene_manager 继续在 Redis 里铸造 E(`max(观察+1, 当前毫秒)` [C]),它只是候选值;scene 进场时用它认领,写权行抬到这个值之后,旧持有者的写在 SQL 层被拒。location、handoff、再入屏障这套协调逻辑基本不动。
4. **C++ 内存结构不变。** `PlayerAllData`、loader、Unmarshal 全部不改。存盘时把 `player_database` 按字段投影成若干片段,只发有变化的片段;加载时把片段拼接后 `ParseFromString`,protobuf 的合并语义还原整个消息。字段属于哪一行由 proto 字段选项声明,每一行怎么存由行枚举值的选项声明,漏标启动即失败(§1.1)。
5. **每个玩家同时只有一个在途存盘。** 结果未知就用同样的内容、同一个序号重试,直到拿到明确结果才发下一笔。脏判断以「上一次确认提交」为基准。
6. **「SQL 已追平」的判据全部换成事务结果。** 不再需要 Redis 里的 ACK 镜像、PING 探测、补投。
7. **新旧路径用开关并存**(`PlayerStorePath: redis | direct`,缺省 redis),切换批翻转,验收后删除旧路径。主干全程可运行,不打断 Codex 验收。开关的约束见 §7.1。

## 1. 表结构

proto2mysql 一张表只能对应一个 message,不支持同表多 message 或 oneof [C],所以用通用的 `payload` 列,不按职责分表。这样还有两个好处:一条语句读全部行(读已提交下不需要跨语句快照);同一玩家的行相邻,打散之后的 ID 不会形成热点,可以用聚簇主键,点查不用回表。

**存盘路径不用 `proto2mysql-cpp`(负责人 2026-11-02 定)**:scene 只序列化,go/db 负责写库。这个 C++ 库的事务、`FOR UPDATE`、版本栅栏都有,不用是因为它放不进这条路径(读代码的判断,没有实测对比):

- **换了也还是存字节**:`payload` 本来就是 protobuf 序列化的结果。这个库只有标量字段是独立的数值列,嵌套消息和 repeated 字段都存 `MEDIUMBLOB`,而玩家数据基本都是消息。
- **写库的是 go/db,scene 不连数据库**:写者闸门、按映射拨号、连接总账都在 Go 这一侧(`shared/authoritysql`、`shared/dbplacement`)。scene 直连的话,闸门要在 C++ 再做一份,两边可能不一致;数据库连接数会随 scene 节点数增长;搬库时每个 scene 都要会切换连接。
- **表的形状不合适**:这个库是一个 message 对应一张表。一张表多列时,只改一列也要重写整行,位置一变背包也跟着写;每组一张表时,读一个玩家要多条语句,同一玩家的数据也不再相邻。

它可以用在 C++ 工具这类单进程、直连数据库的场景。Go 版 `proto2mysql` 在这条路径上继续只负责建表。

```sql
CREATE TABLE IF NOT EXISTS `player_store` (
  `player_id`       BIGINT UNSIGNED NOT NULL,
  `kind`            INT UNSIGNED    NOT NULL,            -- 0 = 写权行
  `owner_epoch`     BIGINT UNSIGNED NOT NULL,            -- 写权行:当前代次;数据行:最后写入时的代次
  `save_seq`        BIGINT UNSIGNED NOT NULL,            -- 写权行:最后提交的整笔序号;数据行:最后写入时的序号
  `schema_gen`      INT UNSIGNED    NOT NULL,            -- 写权行:写过的最高世代;数据行:写入时世代
  `holder_instance` MEDIUMBLOB      NOT NULL,            -- 只写权行用:持有者的 scene_instance_id
  `released`        INT UNSIGNED    NOT NULL,            -- 只写权行用:1 = 持有者已最终存盘并释放
  `payload`         MEDIUMBLOB      NOT NULL,
  `claim_seq`       BIGINT UNSIGNED NOT NULL DEFAULT 0,  -- 只写权行用:每次认领加 1,判死比较用,见 horizontal-scale-logical-db.md §6
  PRIMARY KEY (`player_id`, `kind`) /*T![clustered_index] CLUSTERED */
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
```

- 声明在 `proto/common/database/mysql_database_table.proto`,走现有 `mysql_database_table_list.json`,不手写第二份。全新的库由现有基线建出来。基线已跑过的库上 `cmd/migrate` 建不出新表,而且只告警(`PS:406`),所以新路径启动时用 `VerifySchema` 拒绝启动,翻开关时清库(§7.1),不做在线补表。proto2mysql 只负责建表;读写用 `database/sql` 手写语句(它的事务接口没有原生 SQL 出口 [C])。
- 建在每个落点库里(`zone_{N}_db` 或 `player_store_{N}_db`),落点选择规则不变。新号的 `storage_id` 在建角时写定,默认等于建角 zone;是否统一到一个全局库是配置问题,受 TiDB 决策 §D8 约束,不在本规格内。
- TiDB 上用 `SPLIT TABLE … BETWEEN` 预切(发号打散之后 ID 均匀),纳入 `tidb-prepare`。
- `kind` 枚举放在 proto(`proto/player_store/player_store.proto`):`kOwner=0`、`kGrowth=3`、`kProfile=4`、`kCharacter=6`、`kWallet=7`、`kBag=8`、`kLocation=9`;1、2 是 2026-11-05 退役的 kHot、kAsset,5 是随 `player_database_1` 删除的 kLegacy1,都 `reserved`,不复用(§1.1、§1.2)。

### 1.1 分行与存盘方式(v3,2026-11-05)

负责人 2026-11-05:「player_db player_db_1 这种分法我觉得很奇怪……我想在数据存储这块极限提高性能,但是又要强一致性,可读可扩展移动」,随后「开始实施吧」「把代码做完给我」。v2 的四组(热、资产、成长、低频)把「按频率分」和「按领域分」混在一维里:位置一动,等级、职业、基础属性整行跟着写;改几个铜板,整个背包跟着写。本节按下面四条重排。

**原则**

1. **一致性只靠整笔快照。** 一次存盘是内存某一时刻的完整快照,所有和上次确认提交不同的行在同一个事务里、带同一个整笔序号提交(§2、§4)。盘上永远是内存里真实存在过的某个时刻。所以怎么分行、什么时候存,只影响性能和崩溃时丢多少,不影响正确性。I3 这类「必须同一次提交」的不变量要的是同一次提交,不是同一行。
2. **行按「大小 × 变化频率 × 和谁一起变」分。** 小而常变的不和大而少变的放一行。TiDB 一行就是一个 KV,只改一列也要重写整行(TiDB 决策 §8.1)。
3. **存盘时机按「丢了的代价」定**,写在每行的存盘方式和领域入口的事件上(§5.3「关键进度」),和分行是两件事。重要的(资产、账本)要快存;经常变但不重要的(位置)要少存、合并着存,而不是「经常变就马上存」。
4. **策略只决定什么时候拍快照,不能让一次存盘只写一部分脏行。** 位置可以不触发存盘,但只要这次存盘了,位置变了就一起写。

**分行**(`PlayerStoreKind` 在 `proto/player_store/player_store.proto`;字段用 `mysql_database_table.proto` 的 `OptionPersistGroup` 标行)

| kind | 值 | 字段 | 特点 | 存盘方式 |
|---|---|---|---|---|
| kOwner(写权行) | 0 | 代次、序号、世代、持有者 | 每笔都写 | — |
| kLocation(位置) | 9 | `transform` | 一直在变,丢了不心疼 | 只顺带 |
| kCharacter(角色) | 6 | `level_component`、`derived_attributes_component`(基础属性与当前血蓝) | 小,战斗和升级时变 | 定时 |
| kWallet(钱包) | 7 | `currency`、`asset_op_ledger`、`inbox_ledger`、`mystery_merchant`、`client_request_ledger`(客户端请求幂等键账本,2026-11-06 别的批次加,[请求幂等](client-request-idempotency.md) §5.3) | 小,任何经济行为都改它 | 定时 |
| kBag(背包) | 8 | `bag_component` | 最大的一行,只在道具操作时变 | 定时 |
| kGrowth(成长) | 3 | `skill_list`、`attribute_component`、`pet_component`、`mission_component` | 中等,随进度变 | 定时 |
| kProfile(资料) | 4 | `uint32_pb_component`(职业)、`uint64_pb_component`(注册时间)、`profile_component`、`battle_results`、`item_gain_block`、`stress_test_probe` | 都很小,大多建角后不变 | 定时 |
| 元数据(不入 payload) | — | `player_id`、`persist_schema_gen` | | |

- 退役号:1(旧 kHot)、2(旧 kAsset),proto 里 `reserved`。新加一行取下一个空闲号,不回填。
- 钱包和背包分开之后,一次交易同时改了两者时两行同一事务提交(原则 1)。
- 职业、注册时间移到资料行:建角只写写权行和资料行(§5.4);展示资料读 kCharacter(等级)和 kProfile(职业、名字、外观、性别)(§5.6)。
- `battle_results`(本人近期战斗结果,[战斗结果重登恢复](battle-result-recovery.md) §4.1)和 `item_gain_block`(物品获取封禁,[玩家收件箱](player-inbox-service.md) §2.2)是 2026-11-05 别的批次加的,原本就放资料行,不动:只在结算、GM 处置时变,资料行仍然小。
- `derived_attributes_component` 字段名沿用旧名(改名要动生成代码和全部调用点,随 §1.2 的改名批一起);装的是基础属性和当前血蓝,真正的派生属性 `DerivedAttributesComp` 不落库、加载时重算。基础属性能不能由职业和加点重算,待核对:能重算的就不该落库(AGENTS §11.6),只存当前血蓝。
- 新加字段按上面的原则选行。小而常变的不放背包行;资产、账本、任务领取标记不放只顺带的行。
- 认领回包的上限按「每行都到上限」算(§5.2,`playerstore.New` 校验):现在 6 个数据行 × 1 MiB + 64 KiB ≤ 8 MiB(第二批删掉 kLegacy1 之前是 7 行)。再加行要先核这条。

**存盘方式**(`PersistRowSavePolicy` 在 `proto/db/proto_option.proto`,用 `OptionPersistRowSavePolicy` 写在 `PlayerStoreKind` 每个数据行的枚举值上)

- **定时(PERIODIC)**:有变化就随下一次定时存盘写入(缺省 60 秒,`SCENE_PLAYER_SAVE_INTERVAL_SECONDS`)。
- **只顺带(PIGGYBACK)**:单独有变化不触发定时存盘。别的行要写、任何非定时的存盘(资产、关键进度、下线、交接、疏散、停机)、或者被推迟超过兜底间隔时一起写。兜底间隔缺省 300 秒,环境变量 `SCENE_PLAYER_PIGGYBACK_FALLBACK_SECONDS`,0 = 不推迟(压测对照用),初值压测后定。崩溃时位置最多回到几分钟前;位置不是资产,可以接受(总体方案 §4.6 第一行,本次从「压测之后再定」提前为缺省)。
- 只顺带的行只许放 `transform`(白名单 `kPiggybackAllowedFields`,`player_store_projection.cpp`)。白名单防的是资产的崩溃丢失窗口从定时间隔拉长到兜底间隔;它不防拆分提交,拆分本来就不会发生(原则 4)。

**落码(2026-11-05,第一批,未编译)**

- 推迟只发生在定时存盘入口 `PlayerLifecycleSystem::SavePlayerPeriodic`(`RedisSystem` 的周期回调调用它)。`SavePlayer` 不变:它还以函数指针给资产通道用,不能推迟。`SubmitPlayerSave` 在不带 release、不必强制写时先问 `PlayerStoreClient::DeferPeriodicSave`,返回 true 就返回 `SaveResult::kDeferred`。这份快照不进状态机,没有回调、不取号,所以「已落盘」类结果(`kAlreadyPersisted`、落盘快照替换)永远不代表一份没写的位置。
- `DeferPeriodicSave` 只在这些条件都成立时返回 true:已播种;没有在途或排队的存盘;和上次确认提交相比只有只顺带的行不同;只顺带的行不在结果未决集合里;上一笔不带 release;代次不落后;从第一次推迟算起不到兜底间隔。`Start` 发出一笔带着只顺带的行的存盘时清掉推迟计时,所以别的入口顺带写出位置之后重新计时。
- 启动校验(`ValidatePersistGroups`):每个数据行都声明了存盘方式;字段只能标到现存的数据行(0 = 元数据;退役号、不存在的号都拒绝);只顺带的行只许白名单字段。
- 加载:库里出现退役号或不认识的 kind,`AssemblePlayer` 和认领回包校验都整份拒绝,不按现在的分行去解读别的布局写的行(那会清掉不属于本行的字段,等于静默丢数据)。项目未上线,遇到就清库。
- 计数:`dirty_save_stats::PiggybackDeferred`;`/metrics` 的 `scene_player_save_events_total{event="piggyback_deferred"}`;`[DirtySave]` 汇总行尾 `piggyback_deferred=N`。推迟判断本身计进 compare 段,不推迟时 `Start` 还会再比一次,两次都计。
- 测试:`cpp/tests/bag_test/player_store_client_test.cpp` 末尾一组(推迟、顺带写出、兜底、重新计时、在途与结果未决不推迟、0 不推迟、退役行拒绝、存盘方式声明、位置行只有 transform);Go 侧建角初始行、展示资料、账本读、导出包、全栈资产测试按新行改。
- 别的会话同批配合改的:`redis.cpp` 改调 `SavePlayerPeriodic`;`stress_summarize.ps1` 的行名表与 `test_stress_summarize.ps1` 夹具换成新行名。

**这次重排为什么没有违反下面「不能直接换组」那一条。** 生成器只拒绝「台账已经记下分组之后再改」。本次重排时 `proto/persist_schema_gen.txt` 里所有字段行都还没有 `group=`,第一次生成会按新分行补记,此后就受换组检查约束。旧布局的行一律拒绝加载(上一段),所以也不会出现 N1 说的「换组后第一次载入把字段清掉」:搬走字段的旧 kHot、kAsset 用的是退役号,kGrowth、kProfile 只多了从退役行搬来的字段,没有字段在两个现存行之间搬。

**变化提示:以后要省序列化 CPU 时怎么做**(负责人 2026-11-05 问:要不要像属性同步 `SetActorBaseAttributesS2CAttrDirtyBit` 那样打脏标记,又怕别人忘了标没存,要兜底)

- **写什么永远由字节比较算出,标记只当提示**:标了的行先序列化,没标的行可以跳过序列化。漏标不能导致「这一行永远不写」。
- **兜底**:最终存盘(下线、交接、疏散、停机)和每 N 次定时存盘一律全量序列化比较,不看提示。漏标最多晚到一个兜底周期,正常下线不丢。
- **抓漏标**:全量比较时发现「没被标却变了」的行,计数 `scene_player_save_unhinted_dirty_total{kind}` 并打日志(不带 player_id 标签);单测里直接断言失败。这样漏标会被发现、被修,不是默默被兜底吃掉。
- **提示是行级,不是字段级**:存盘的单位是行。打标记的地方收口在领域入口(货币、背包、任务、等级各自的写入函数),不散在玩法代码里。
- 和属性同步的区别:属性同步漏标,客户端看不到更新,马上就能发现;存盘漏标平时看不出来,只在崩溃时多丢,所以兜底和抓漏标必须同时有。
- **现在不做**:每次存盘都全量序列化比较,没有提示,不存在漏标丢数据。压测显示序列化占 CPU 明显再加(总体方案 §4.6 表「序列化占 CPU 明显」一行)。

**仍然成立的规则**

- 账本不变量 I3(货币、背包、任务领取标记、两本账本、限购同一次落盘 [C])由「一次存盘的所有脏行同事务」保证,所以货币和背包分行、任务放 kGrowth 都不违反 I3。
- 启动时按描述符校验:每个字段恰好属于一组,否则拒绝启动(fail-closed)。这样以后加字段漏标,不会静默丢数据。
- **不能直接换组(2026-11-05 起由生成器拒绝)。** 加载时**逐行解析,清除不属于本 kind 的字段,再合并**,以当前分组为准。这一步防的是两行各有一份时合并翻倍(拼接会合并消息字段、追加 repeated 字段,背包翻倍),但它挡不住丢失:字段 F 从 kGrowth 改到 kAsset 之后,新二进制第一次载入会把 kGrowth 那一行里的 F 清掉,而 kAsset 那一行里还没有 F,第一次存盘就把两行都写成不含 F 的版本,所有登录过的玩家永久丢 F(数据覆写审计 R2 的 N1)。所以分组一旦记下就不得再改:持久 schema 台账给玩家表顶层字段记 `group=<分组>`,换组或删掉分组选项时 proto 生成直接失败(`persistschema.Apply`,[混版本持久字段保护](mixed-version-persist-field-protection.md) R4)。确需换存储位置,按 R4 走 expand / contract:在新分组加一个新字段,新旧双写一个完整版本周期,再切读方,旧字段写 `reserved` 退役。
- 顶层出现未知字段的消息拒绝存盘:新二进制写的字段被旧二进制丢掉,比拒写更糟。
- `PlayerAllData` 顶层的 `last_save_seq`、未携带字段不变。

### 1.2 第二批:删掉 player_database_1(2026-11-06 落码,未编译)

- **为什么删**:`player_database_1` 只剩 `player_id`、`stress_test_probe`、`persist_schema_gen`,后两个 `player_database` 里都有。它原来的用意(重要数据放 `player_database`、登录立即加载,不重要的放另一张)已经被分行取代;留着只有负担:多一个 kLegacy1 行;它和 `player_database` 是两个消息,字段号有重叠(字段 2 与 `transform` 冲突),加载时要单独解析、不参与拼接;覆盖世代要两边盖同一个值,载体组件、身份核对、快照都要做两份。
- **删完之后的规矩**:玩家持久数据只有 `player_database` 一个消息。不要再按「重要 / 不重要」开第二张子表;存在哪里由 `OptionPersistGroup` 选行,什么时候存由行的存盘方式和领域事件决定(§1.1)。
- **改了什么**:
  - proto:删 `player_database_1` 消息;`PlayerAllData` 字段 3、`PlayerSnapshotEntry` 字段 6(`player_database_1_blob`)、`PlayerStoreKind` 的 5(kLegacy1)都写 `reserved`(连名字)。`persist_schema_gen.txt` 不手改:生成器会给 `PlayerAllData#3` 记退役行,`player_database_1` 自己的字段行按「类型已不可达」保留(`persistschema/apply.go`)。已落库的旧快照里字段 6 解析成未知字段,不影响读取。
  - 生成的 `player_data_loader.h` 不手改(AGENTS §3),重新生成后不再声明 `PlayerDatabase1MessageFields*`,`PlayerAllDataMessageFields*` 也只处理一张表。
  - C++:删 `player_database_1_loader.cpp`(`scene.vcxproj`、`.filters` 已去掉;`CMakeLists.txt` 由 vcxproj 生成,不手改,Linux 构建前要重新生成);删 `PlayerDatabase1CarryComp`;`player_persist::LoadedSchemaGen` 只看主表载体,不再取两表最小值;删 `StampPlayerDatabase1`;入场证据和快照提交核对只核主表的 `player_id`;快照不再带 db1 blob;`player_lifecycle.cpp` 的退出收敛比较、载入身份核对、认领拼装、存盘投影四处;`ProjectPlayer(player_database&, KindBytes&, error)`、`AssemblePlayer(rows, player_database&, error)` 去掉 db1 参数,kLegacy1 行在加载时按退役号拒绝。
  - Go:导出包不再认 kLegacy1;Kafka 快照测试夹具去掉 db1 blob。`go/db/internal/maintenancephase/restore.go` 里 `"player_database_1"` 是拒绝 Redis 旧字段名残留的字符串检查,保留。
  - 测试:12 个 C++ 测试按单表改(往返测试的世代断言从「两表取最小」改成单表,载体缺失只测一个;快照提交核对的串号用例改成主表串号)。
  - 脚本与说明:压测汇总去掉 legacy1 列(`stress_summarize.ps1` 与 `test_stress_summarize.ps1`),`stress_round19.ps1`、`cpp/AGENTS.md`、`go/AGENTS.md`、`proto/AGENTS.md` 的说明。
- **没改的**:`go/db/model`、`go/login/model`、`go/player_locator` 下的 `mysql_database_table.sql` 文件头写明是历史手工导出、不是权威 DDL,里面 `player_database` 也早就不是表了,这次不动;生成器测试 `db_model_test.go` 里的 `player_database_1` 只是自造的夹具名,不依赖真 proto。
- **暂不改名**:`player_database` 改成按职责的名字(比如 `PlayerSaveData`)、`derived_attributes_component` 改成 `base_attributes`,能提升可读性,但 `player_database` 被 100 多个文件引用,2026-11-05 工作区里有二十多个会话在并行改这些文件,大面积改名会互相冲突。等并行改动收敛后单独一批。

## 2. `Save`

```proto
message SavePlayerRequest {
  ServiceCallAuth auth = 1;
  uint64 player_id = 2;
  uint32 storage_id = 3;        // 认领时返回的落点;0 只用于实体已没的空行释放,由 go/db 查落点,带行或不是释放就回 INVALID
  uint64 owner_epoch = 4;
  uint64 save_seq = 5;
  uint32 schema_gen = 6;
  bytes  instance_id = 7;       // scene_instance_id
  repeated PlayerStoreRow rows = 8;  // 有变化的片段,可以为空
  bool   release = 9;           // 最终存盘:提交后释放写权
}
message SavePlayerResponse {
  SavePlayerResult result = 1;
  uint64 row_owner_epoch = 2;   // 拒绝时带回库里的值
  uint64 row_save_seq = 3;
}
```

事务(`storage_id` 选库后,读已提交 + 悲观锁):

```
进事务前校验(确定性拒绝,不碰库):E、q、实例非空,kind 是已知值、非 0、不重复 → 否则 INVALID;payload 超预算 → TOO_LARGE
SELECT owner_epoch, save_seq, schema_gen, holder_instance, released
  FROM player_store WHERE player_id=? AND kind=0 FOR UPDATE
按 §4 判定;拒绝或幂等成功就回滚并返回
INSERT INTO player_store (…) VALUES (写权行), (脏行 1), (脏行 2)…
  ON DUPLICATE KEY UPDATE owner_epoch=VALUES(owner_epoch), save_seq=VALUES(save_seq),
    schema_gen=VALUES(schema_gen), holder_instance=VALUES(holder_instance),
    released=VALUES(released), payload=VALUES(payload)
COMMIT
```

- 业务语句只有 3 条,与脏行数无关。写权行的 `payload` 恒为空。绑定前把 nil payload 归一成空串,否则驱动按 NULL 写,撞 NOT NULL。
- 写权行的 `released` 按请求重写:`release=false` 的存盘会把它清回 0。写权行的 `owner_epoch` 写成请求的 E(可能高于库里,见 §4)。
- `payload` 超预算在进事务前拒绝(`TOO_LARGE`),不写任何行。预算不分 kind:单行 1 MiB、单笔 3 MiB(`playerstore.DefaultLimits`,可配),已经覆盖现有账本的 1 MiB 口径 [C]。压测看到某一行顶到上限再分。
- 判定是纯函数,与 SQL 分开,单测逐格覆盖。
- 无重试队列、无死信。SQL 错误一律回 gRPC 错误,算结果未知,scene 同内容同 q 重试。`authoritysql` 对写路径上除 1062 之外的错误不下结论(`driver.go:76`),这里也不绕开它,所以确定性错误要靠别的办法防住,否则 scene 无限重试、退出卡死:
  - 进事务前校验挡掉 kind 非法、payload 超预算、nil payload(见上);
  - `playerstore.Store.VerifySchema` 在启动时核对表和列,缺了拒绝启动(挡住 1146、1054);
  - 超过传输上限(4 MiB)的请求 go/db 收不到,通道在本地回 `RESOURCE_EXHAUSTED`,那会被当成结果未知永远重试。scene 发送前按序列化大小预检,超限就按 `TOO_LARGE` 处理,排到下一轮回调,调用方可能还在自己的栈上;
  - 退出遇到确定性拒绝就放弃这一笔(§4);停机由 Node 的 drain 看门狗限时,到期把没有结论的玩家号打进 ERROR(`metric=player_store_shutdown_unsettled`)。**运行中的退出遇到结果未知不设单独的预算**:TiDB 故障和确定性 SQL 错误在这里分不清,放弃就是把一次故障升级成回档;实体留着不丢数据,玩家重连还能复用,代价只是这段时间占内存、连续失败时打日志(`metric=player_store_save_retry`)。

## 3. `LoadAndClaim`

```proto
message LoadAndClaimRequest {
  ServiceCallAuth auth = 1;
  uint64 player_id = 2;
  uint64 owner_epoch = 3;       // 路由带来的候选代次
  bytes  instance_id = 4;
  uint32 binary_schema_gen = 5;
}
message LoadAndClaimResponse {
  LoadAndClaimResult result = 1;
  uint32 storage_id = 2;
  uint64 owner_epoch = 3;       // 写权行当前值
  uint64 save_seq = 4;
  uint32 schema_gen = 5;
  repeated PlayerStoreRow rows = 6;
}
```

事务:

```
选库:读玩家落点(§15 落地前沿用 placement 查询,之后直读 TiDB)
SELECT 写权行 FOR UPDATE
按 §4 判定;拒绝就回滚并返回
UPDATE 写权行 SET owner_epoch=候选代次, holder_instance=实例, released=0
SELECT kind, payload FROM player_store WHERE player_id=? AND kind>0
COMMIT
```

- 写权行不存在 = 角色不存在或未建完,回 `NO_OWNER`,不造空档(今天的规则 [C])。
- 世代比本进程新 = `SCHEMA_TOO_NEW`,拒绝加载(今天的 C2)。
- scene 把片段拼接后 `ParseFromString`,再把 E、q、世代填回元数据字段,`InitPlayerFromAllData` 不改。q 按 `max(本进程发号, 响应 save_seq)` 播种。

## 4. 写权判定表

记请求为 E、q,库里为 Er、qr。判定自上而下,命中即止。

**`Save`:**

| 条件 | 结果 | scene 处置 |
|---|---|---|
| 写权行不存在 | `NO_OWNER` | 告警(和结果未知同一个节流),保留实体,按结果未知的退避重试 |
| E < Er | `STALE_OWNER`(带 Er) | 进 `HandlePlayerSaveRejected`,语义不变 |
| 持有者 ≠ 实例 | `NOT_HOLDER`(E=Er)或 `UNCLAIMED`(E>Er) | 同 `STALE_OWNER` |
| q = qr | `EQUAL`(幂等成功) | 按提交成功处理。release 重试也走这一格 |
| q < qr | `OLDER_SEQ` | 迟到副本,无人等待;在途请求收到说明状态错乱,告警后用 qr 重新播种 |
| 世代 < 库里世代 | `SCHEMA_REGRESSED` | 拒写,告警(今天的 C1b) |
| 世代 > go/db 认识的世代 | `SCHEMA_TOO_NEW` | 重试,属发布顺序问题(今天的 C1) |
| 其余 | 写入,`COMMITTED`。E>Er 时把 Er 抬到 E,`released` 按请求重写 | 提交成功 |

**`Save` 进事务前的确定性拒绝:** `INVALID`(E、q、实例为空,或 kind 非法、重复)、`TOO_LARGE`(payload 超预算)。不碰库。scene 告警,这笔的 q 作废,等下一次变化重试;退出时遇到它就记告警并放弃这一笔,不无限等。E=0 的无 guard 路径删除。

**三条要点:**

- **E>Er 且持有者是自己,是合法状态,必须放行并抬 Er。** v1 把它判成 `UNCLAIMED`,实体再也存不进,下线后进度全丢。三条真实路径:路由回滚后采纳新 E(`PL:7044`、`PL:7048`);墙钟落后时铸 E+1,复用实体不调 RPC(`OE:198`、`PL:2987`);载入在途被新路由覆盖,认领的是旧 E(`PL:2879`、`PL:2876`)。有中间持有者时,持有者必不是自己,不会误放。
- **释放不是终态。** 同实例已释放,照样放行,并按请求重写 `released`。否则:断线 → `Save(release)` 在途 → 30 秒内重连,不铸造、取消退出、复用实体(`ES:747`、`PL:2953`)→ release 提交 → 下一笔被拒 → 实体销毁,重连之后的进度全丢。
- **持有者核对排在 `EQUAL` 之前。** 跨实例同 E 现在不可达,但这个顺序才不依赖这一点。

**`LoadAndClaim`:**

| 条件 | 结果 |
|---|---|
| 写权行不存在 | `NO_OWNER` |
| E < Er | `STALE_OWNER`(路由过期,上层按进场失败处理) |
| E = Er,持有者非空且 ≠ 实例 | `NOT_HOLDER`(今天 `kRegisterFenceLua` 拒同 E 跨实例的语义) |
| 库里世代比本进程新 | `SCHEMA_TOO_NEW`,不抬代次(今天的 C2) |
| E = Er,其余 | 认领成功(同实例重进,或新号初行) |
| E > Er | 认领成功,抬代次(换持有者或接管) |

E=0、实例为空在进事务前回 `INVALID`。

- **闭合「新持有者首存之前老写能通过」**:认领时就把 Er 抬上去,老持有者的下一笔立刻 `STALE_OWNER`。这是 TiDB 决策 §9.4 第 3 条的答案,不再依赖 Redis Lua 的 owner 比对。
- **Redis 回退**:E 是墙钟值,回退后新铸的值仍大于旧值(`OE:157` [C]);万一更小,认领被拒,进场失败,不会覆盖新档。
- **同节点不铸造**:同实例同 E 重进,认领幂等。实体还在内存时不走加载,也不调 RPC。

## 5. C++ 侧

### 5.1 存盘状态机(替换 `SavePlayerImpl` 之后的部分)

每个玩家一个 `PlayerStoreSaveState`,放在场景级的表里,以 Guid 为键,**不挂在实体上**:实体销毁时状态还在,存盘结果才有地方落。内容:`confirmed[kind]`(已确认提交的片段字节)、`uncertain`(结果未决的 kind 集合)、`inflight`、`pending`(最新一份完整快照及回调)。

1. **播种**:`confirmed` 用 `LoadAndClaim` 返回的**库里原字节**,不用加载后重新序列化的结果。否则加载时改过的字段不会被写回,`Uncarried` 的 `merge_state` 也永远清不掉。
2. **算脏**:`MarshalPlayerForSave` 拼整份,按 kind 投影、确定性序列化。任何 kind 与 `confirmed` 不同,或在 `uncertain` 里,就算脏。全部不脏且无欠账 → `kAlreadyPersisted`,与今天一致。
3. **压测探针**:比脏时排除 `stress_test_probe`。有其他脏行时才打戳,打戳后 kProfile 并入脏集。这样探针不会让玩家恒脏,也不会打了戳却不落盘。
4. 有在途 → 新快照覆盖 `pending`,回调并入。**排队的是完整快照,所以覆盖不丢行**;脏行在发送时才算。
5. 无在途 → 发 q(`save_seq.h`,只有确定要写才发),发送。
6. 回包:
   - `COMMITTED` / `EQUAL`:更新 `confirmed` 中本次发送的 kind,从 `uncertain` 去掉;`PlayerLastPersistedSnapshotComp` 替换为这份快照;执行回调;有 `pending` 就发下一笔。
   - gRPC 非 OK(结果未知):**同样的内容、同一个 q、同一个 E** 退避重试(0.5s 起,封顶 5s,乘 0.5~1 的随机系数,否则 go/db 重启时在途的存盘一起重试),可以换 go/db 节点,不发新 q。`ServiceCallAuth` 每次重新签名:它有 ±300s 的时间窗(`auth.go:138`),不重签的话故障超过 5 分钟就永久失败。
   - `STALE_OWNER` 后重存(`PL:4873`)、`OLDER_SEQ` 重新播种:上一笔的行可能已经提交了,把它们并入 `uncertain`,否则改回原值的行不会被写。
   - 其余拒绝:按 §4。有 `pending` 时 `INVALID`、`TOO_LARGE`、`SCHEMA_REGRESSED` 不是结论,排队的快照内容更新,可能提交得了,由最后一笔决定怎么处置;写权拒绝和 `OLDER_SEQ` 照常处理(同代次的写权拒绝已经把排队的丢了,其余要马上重存)。
7. **迟到的回包不会改到重新加载之后的状态。** 回调恰好一次回到 scene 线程,不捕获实体句柄(`grpc_client_call.h` 的约定 [C]),用 Guid 查实体。不用加载代次:存盘未落定时不能重新播种(`Seed` 返回 false),认领也要先等落定,所以实体销毁重载之后(`PL:5940`)不存在还在路上的存盘。
8. **全局完成只在彻底落定时触发。** 有 `pending` 时不算完成(`RC:1171`):否则 q9 一落地就 `BeginTravelHandoff`(`PL:2425`),交接标记早于冻结快照,目标读到旧档。`HasUnsettledPlayerSave`、`HasPendingLoad` 全部接到新的在途状态上。
9. **快路径也要释放。** `PL:3455` 这条「没有变化就不存盘」的下线路径是挂机玩家的常态,要发空行 `Save(release=true)`,否则 `released` 永远不会是 1。

Scene 持久任务作用域托管每次 Save 或 LoadAndClaim 尝试，回包在原 EventLoop 恢复，接纳与结果未知分别处理。存盘排队、原 q/E、认领预算、退避、整笔落定通知仍由玩家存盘状态机决定。维护阶段继续处理真实提交和重试。最终拆除时，本地取消保留未确认的存盘和认领，不触发提交、播种、放行或落定通知。延迟回包和定时器只持有实例的弱身份。

### 5.2 通道

新增 gRPC 域 `player_store`,照 `data_service` 配置生成 C++ 客户端;`eNodeType` 新增节点类型、加入 scene 发现白名单、go/db 用 `shared/noderegistry` 注册;scene 选本 zone 内 READY 的 go/db。鉴权沿用 `ServiceCallAuth`,scene 一把独立密钥。

Deadline:go/db 的 zrpc `Timeout` 显式配置并大于 SQL 锁等待 10s(今天默认 2s,取消即结果未知 [C]);C++ `CallDeadlineMs` 过 `Assert-GrpcClientDeadlineBudget` 门禁。单消息 4 MiB 上限大于单笔预算。认领响应是一个玩家的全部行,而存盘只限单笔,多笔累计可以超过 4 MiB,所以连 go/db 的通道把接收上限放大到 8 MiB(`player_store_path::kMaxClaimResponseBytes`,与 `playerstore.MaxClaimResponseBytes` 同值,Go 单测核对);`playerstore.New` 校验「每行都到上限也装得下」,配大了拒绝启动。

### 5.3 B4 落码时定下的细节

- **主流程分流**:`SavePlayerImpl` 的校验、marshal、交接跳过、计数两条路径共用,之后分流到 `SubmitPlayerSave`。下线、交接、疏散的判定逻辑不改,复用 `HandlePlayerAsyncSaved`、`HandlePlayerSaveRejected`、`HandlePlayerSaveSchemaRejected` 三个入口;确定性拒绝走新的 `HandlePlayerSaveRefused`,处置形状与世代拒绝相同(`DisposeRefusedSave`)。
- **什么时候必须写一笔,哪怕没有脏行**:带 `release`;调用方要确定的提交(`allowSkipWhenPersisted=false`);快照上没有当前代次的提交(和旧路径同一个口径:资产、结算、收件箱拿快照当「已提交」的证据,加载之后第一笔不写,它们就等不到快照);上一次提交带了 `release`,要把 `released` 清掉;本地代次比库里新,要把代次抬上去。后两条由状态机自己判断,不依赖快照组件。
- **退出被取消**:同节点重连(`EnterScene` 第 0 步之后)和退出被更新的会话取代(`HandlePlayerAsyncSaved`)都立刻写一笔,不等定时存盘,否则这段时间库里显示玩家已下线。
- **关键进度**:`PlayerLifecycleSystem::RequestKeyProgressSave`。事件:任务完成、任务领奖、等级变化(目前只有 GM 设等级这一个入口)。每名玩家 1 秒节流,窗口内合并;存盘不在调用方的栈上发生,调用方可能正处在一连串状态变化的中间。总体方案 §4 列的另外几项:副本通关、充值到账走已有的资产、结算立即存盘;稀有掉落现在没有判定入口,留到掉落系统有品质规则时再接,定时存盘兜 60 秒。
- **定时存盘**:新路径缺省 60 秒(初值,压测后定),环境变量 `SCENE_PLAYER_SAVE_INTERVAL_SECONDS` 优先。只有位置变了的那一笔推迟,最长 `SCENE_PLAYER_PIGGYBACK_FALLBACK_SECONDS`(缺省 300 秒),见 §1.1「存盘方式」。
- **序号饱和**:发送前发现就同步返回拒绝,不走回调,调用方还在自己的栈上;排队的快照开始时才发现,走回调。

### 5.4 B5 落码时定下的细节

- **认领放在 `PlayerStoreClient` 里**(`RequestClaim`),和存盘共用传输与「存盘未落定」的判断:存盘或无状态释放在途时先等落定。已有在途的不重发,发送时才从待入场条目取 E,结果未知就重试(认领幂等),30 秒预算(单调时钟)用完报告没有结论,走载入失败。
- **播种在状态机里做**:`confirmed` 直接取响应的 `rows`,只可能是库里的原字节。
- **载入结果接旧入口**:`HandlePlayerStoreClaimed` 拼回 `PlayerAllData`、填回元数据(`player_id`、`fence_*`、`persist_schema_gen`、`last_save_seq`),交给 `HandlePlayerAsyncLoaded`。A2′ 闸门、会话栅栏、交易封存都不用改。`LOAD_AND_CLAIM_NO_OWNER` 映射到 `DataNotFound`,其余拒绝和没有结论映射到 `RedisError`,都回 3023。
- **载入被放弃时回收播种的状态**:没有实体、也没有待入场条目时 `Forget`。写权行留着 `released=0`、持有者是本实例,是 §9.8 接受的残余。
- **退役字段**:拼接时只拒绝「更新版本才有的字段」,已退役字段的残留直接丢掉。否则以后退役一个字段,所有存过它的玩家都进不来。
- **login**:`PlayerStorePath=direct` 时 EnterGame 跳过预加载、不补身份,直接走 `applyLoadedPlayerSession`。
- **建角**:`data_service` 在建角事务里写 `player_store` 的写权行(E=1、q=1、当前世代、`released=1`、持有者为空)、kProfile(职业、名字、外观、性别;v3 之前职业在 kHot,§1.1);不再写两张旧表,不返回种子原件。其余 kind 没有行,首次进场由 scene 按 `registration_timestamp=0`(也在 kProfile)判新号。这回答了 §9.2。
- **开关覆盖范围**:`bin/etc/base_deploy_config.yaml`、`go/db/etc/db.yaml`、`go/login/etc/login.yaml`、`go/data_service/etc/data_service.yaml`、`go/scene_manager/etc/scene_manager_service.yaml`、`go/friend/etc/friend.yaml`、`go/match/etc/match_service.yaml` 七份必须相同,`k8s_deploy.ps1` 生成期、`start_game.ps1` 启动前都核对。

### 5.5 B6 落码时定下的细节

- **`ConfirmSnapshotSqlCommit`**:新路径上核对完「这份快照就是这个实体当前代次的提交」就同步回 true,不读证据。结算、收件箱的调用方都能处理同步回调,不用逐个改。探测与退避的代码留到旧路径删除时一起删。
- **`CheckSql`(交易冻结)**:`required` 只在提交回调和认领时抬高,走到这里实体已经销毁,所以「没有未落定的存盘」就等于「required 已提交」。
- **PING 快路径**换成 `PlayerStoreClient::RunWhenSettled`:存盘落定时看一次结果。结算的这条回调只看不压存盘,压了也会被拒,原地重试就是死循环;收件箱的 `RequestPersist` 自身有节流。
- **账本读**:`data_service` 新增 `AccountDirectoryStore.ReadPlayerStoreAsset`,按 `player_route` 选库,一条语句读写权行和 kWallet(v3 之前是 kAsset,§1.1;账本只在钱包行,不读背包)。`GetPlayerAssetOpLedger` 的三种读法(普通、`verify_redis_copy`、`require_sql_commit`)和 `GetPlayerInboxLedger` 都走它:只有一个来源,核验恒真,证据就是账本本身。写权行在、kWallet 行不在是「账本为空」,不是「没有存档」。
- **自动放弃**:`GetPlayerAssetOpLedger` 加 `read_player_store`,回带 `player_released`。`assetop.Loop` 先问一次:对端走新路径就用 `judgeWithPlayerStore`(`released=1` 代替「没有位置」,与账本同一次读),走旧路径回到原判定,不读 Redis。不给 guild、trade、mail 各加一个开关:判断跟着 data_service 走,不会分叉。
- **判死释放**:`LeaveScene` 判出 `owner_dead` 时,先释放写权,再删 location;任何一步失败或拿不准就保留 location 重试。释放分三步,与交易判死(`fenceDeadTradeHolders`)同一口径(2026-11-05 起,`go/scene_manager/internal/logic/dead_holder_release.go`、`go/data_service/internal/store/player_store_dead_holder.go`):① `data_service.ReadPlayerStoreHolder` 普通读写权行,记下 (E, H, claim_seq);没有写权行或 E 已比 location 这一代新就没有东西要释放。② H 非空时 scene_manager 向 etcd 做一次线性一致读(与 GO-3 顶替判定共用 `freshRegistryInstances`),H 在册就不释放、回 FailedPrecondition,读不到 etcd 回 Unavailable。判死看的是本副本镜像,可能滞后;判死之后持有进程也可能失租重注册、以同一 UUID 回来,玩家同节点重连复用实体 —— 只比代次的旧实现会照样释放,把在线玩家踢掉。③ `ReleaseDeadPlayerStoreHolder` 带上 `seen`:存档库事务里写权行还是 ① 读到的三列才释放,变了回 FailedPrecondition。① 排在 ② 前面是 ③ 的依据:② 看到 H 不在册,H 之后再认领一定晚于 ①,claim_seq 必变(同实例同代次重登只有它变)。剩下的窗口是 ② 与 ③ 提交之间(一次 RPC)H 重注册且玩家复用实体(不认领,claim_seq 不变);H 在册但不是判死节点上的进程时也不释放:判死节点还没认领(交接不 release,写权行停在仍然活着的交接源上),拿不准它此刻是否还持有这名玩家(交接回滚之后它就是现任);代价是 `released` 保持 0、location 留着,自动放弃与这次清理都等玩家下次进场或那个进程下线,归入 §9 第 8 条已接受的残余。两处都不丢不盖。事务里 E 抬到 `location 这一代 + 1`、持有者清空、`released=1`。库里已比它新就不动,所以幂等。不带 `seen` 的只按代次释放留给无资格撤销之后的补释放(`location-release-binding.md` §17)。代次用 location 记的这一代,不用之后才读的 `owner_epoch` 键:这期间另一路 `EnterScene` 可能已经接手、铸了新代次,拿新值释放会把刚接手的新持有者围掉(它已认领就丢认领后未提交的进度,没认领就进不了场)。location 没记代次时不释放,下一次认领照样抬代次把旧持有者挡住。写在 data_service 是因为它已有跨库写、scene_manager 已有它的客户端和独立密钥,不用给 scene_manager 再接一条 go/db 的发现与鉴权。`EnterScene` 判死接管不用调:新认领抬 E 就把旧持有者挡住了。
- **交易冻结证据**:`SQLRoleFreezeVerifier.UsePlayerStore` 后读 `player_store`,版本取写权行,摘要覆盖这名玩家所有行的所有列(`claim_seq` 除外:它不是存档内容,冻结期间也没有人能认领);初始提交判定改成「所有行还是 (1, 1)」。
- **维护恢复**(`verifyMaintenanceRecovery`)建立在 Redis 屏障和两张旧表的提升上,新路径先拒绝(`maintenance_recovery_not_on_player_store`)。崩溃持有者在交易冻结上卡住时要人工处理;是否要做新路径的恢复证明放到旧路径删除时一起定。

### 5.6 B7 落码时定下的细节

- **展示资料**:`data_service` 新增 `BatchGetPlayerDisplay`,按路由分库读 kCharacter(等级)和 kProfile(职业、名字、外观、性别)(v3 之前等级、职业在 kHot,§1.1)。好友列表、在线目录、队伍视图走它;会话、战斗锁这些还在 Redis,不动。没有另做 Redis 投影:那要多一个写方和一份会过期的副本,等压测说读量扛不住再加。
- **login 外观**(`fillMissingRoleAppearances`):新路径直接跳过。它只在账号目录缺外观时才读父键,而新路径没有父键,外观在建角事务里和账号目录一起提交。
- **建角时的 id 复用探测和种子**:`probeMintedID` 照旧探 Redis 的父键和 `owner_epoch`,只读,`owner_epoch` 还在 Redis。`NewPlayerSeed` 开着也不会种父键:新路径没有种子原件,`cacheCommittedNewPlayer` 走不到。
- **`HasHistoricalPlayers`**:新路径上也看 `player_store`。旧路径的已有库可能没有这张表,查它会让启动失败,所以按开关加。
- **GM 回档**(`RollbackPlayer`、`RollbackZone`、`RollbackAll`):新路径回 `ErrCodeNotImplemented`。它们写 Redis,写了也不生效,不能回成功;也不能回 `ErrCodeRollbackFailed`,那是故障码,拦截器会 Hold,GM 调一次这个进程的排空证明就废了。
- **工具**:`debug_fetch`、`debug_import` 按配置拒绝;`save_barrier_restore`、`merge_zone`(合服、搬库)看库里有没有 `player_store` 行,有就拒绝。合服在新路径上的做法是别名合服,见总体方案 §7.1,那时再放开。`admission_recover` 不用改:读不到旧表行就拒绝。
- **`verifier`**:新路径读 kProfile 行里的探针,没有子表缓存要比。
- **`go/db/cmd/data_stress`、`robot/data_stress.go` 不改**:它们驱动的是旧的 DBTask 消费链,新路径没有这一层。L3(机器人 + scene 打探针)用 `verifier` 照常核对,L2 随旧路径一起删。
- **旧的消费链、saveoutbox 还在跑**:新路径上它们没有输入,闲着。B8 删。

### 5.7 评审之后定下的细节

做过两轮独立评审。B4、B5 那轮没有找到丢档、回档、串档、一物两得的序列;B6、B7 那轮找到一条回档序列(判死释放围掉新持有者,见 §5.5),已修。除了各节已经写进去的,还修了:

- **初始提交标记**:`account_role.initial_player_commit` 按「本事务写了初始行」记,不按有没有种子原件记。新路径没有原件,按原件记的话从未进场的角色永远冻结不了。
- **普通关闭也等落定**:`RoleAdmissionSystem::Tick` 在 `CloseOrAck` 之前看 `IsPlayerSaveUnsettled`。新路径的下限在提交后才抬高(旧路径发送前就抬),不等的话下限漏掉在途那笔,它会在冻结之后落地改行。
- **每次提交都换快照**(§5.1 第 6 条):排队的存盘还没发时,前一笔的 `OnPlayerStoreSaveDone` 也换快照,只是不走 `HandlePlayerAsyncSaved`。否则前一笔提交、排队那笔被拒时,快照落后于 `confirmed`,结算和收件箱的确认卡到下线。
- **C++ 命名空间**:投影的命名空间叫 `player_store_rows`,不能叫 `player_store`:`mysql_database_table.proto` 没有 package,`message player_store` 是全局类。
- **账本和展示资料读的并发上限**:最多占账号目录池的一半(`acquirePlayerStoreRead`,缺省 4 条)。这些读和登录的悲观事务共用这个池,好友列表、在线目录、队伍视图原来走 Redis,读量一大进场就会等不到连接。等不到名额按调用方的截止时间失败。是否要单开只读池,压测再定。
- **展示资料**:一个玩家的落点读不了只跳过他,不让整批失败。
- **交易排空证据(10-30 评审)**:只有冻结发起之后提交的快照才算。提交时才知道结果,但「算不算」要按提交那一刻定:`FreezeRoleForTrade` 过了战斗检查才打标记,`SubmitPlayerSave` 提交时读标记,带进完成回调。封存时已经在途的那笔不算(否则排队的冻结存盘被拒时,交易带着更旧的存档继续);局中结算自己的存盘也不算(否则重放封存不再冻结,交易一直停在 FREEZING)。冻结存盘被拒时不发 AckDrain,交易停住,这是告警级事件(`DbPlayerStorePayloadRejected`),`admission_recover` 摘掉之后才能走完。

### 5.8 单次存盘与认领的协程接续（2026-11-05）

`RunSaveAttempt`、`RunClaimAttempt` 只表达一次异步提交及其结果处理，由 Scene 的持久任务作用域持有。`confirmed / uncertain / inflight / pending`、相同内容与相同序号重试、重新签名、退避和认领截止时间仍按 §5 执行，没有改成整笔业务的一条无限协程。回复在传输回调返回前复制为自有 protobuf；任务和定时器以弱 `Lifetime` 认领原 `PlayerStoreClient`，实例已销毁就丢弃。

维护和正常关机先停瞬态任务，存盘与必要的认领继续参加原排空。只有最终 `BeforeTeardown` 的 `StopAll` 才结束本地等待：之后新 `Submit` 返回 `kStopped`，不登记新认领；原来未落定的快照保留到实例销毁，不消费晚结果、不触发 `Finish / Seed / Notify`，也不推进 `pending`。这不是远端写入撤销或失败的证明，`HasUnsettled` 此时仍为真是有意的保守语义。真实离线释放的在途记录仍按原完成路径归还。

Java 的提交协议与可观察结果没有因本批变化；不要求复制 C++ 的协程控制流。任务线程及未来标准 `task` 迁移见[线程模型](scene-node-threading-model.md)和[队伍设计](team-system.md)。

## 6. 等待点替换

| 等待点 | 今天 | 新路径 |
|---|---|---|
| 下线 `HandleExitGameNode` → `FinishExitOrAwaitDbTaskAck` | Redis 落地 + Kafka acks=all | 最终 `Save(release=true)` 明确成功 |
| 交接 `BeginTravelHandoff` | `kPublishMarkLua` 要求 pending 等于两表 ACK | 最终存盘明确成功后写标记,脚本去掉 ACK 判断。超时未成功就不写标记,由看门狗(`PL:5362`)解冻;`ResolveTravelOutcome` 在没有标记时直接返回(`PL:6309`),不能靠它。不 release,目标认领时代次抬高即可 |
| 疏散、A1′ | 不等 SQL | 改为最终存盘明确成功后写标记 |
| 会话结束凭证(`PL:5859`) | 不等 SQL | 这时实体已经没了,发空行 `Save(release=true, storage_id=0)`,有结论后写标记。`COMMITTED`、`EQUAL` 算释放成功;`NO_OWNER`、`UNCLAIMED` 说明这个实例在这个代次上没有认领过(登记了资格,载入没走到认领),没有东西要释放;`STALE_OWNER`、`NOT_HOLDER` 说明写权行已经不归这个实例(判死释放过,或者新铸的代次恰好等于库里的值);`OLDER_SEQ` 说明发号器落后于库里,`released` 留在 0,下次登录解。这些都写:这时本节点没有实体、没有未落定的存盘,这个实例不会再写了,不写的话同节点重登不铸造、认领一直被拒,玩家要锁死到节点重启。`INVALID`、世代不符和结果未知不写,不重试,下一次 LeaveScene 再来。在途期间 `HasUnsettled` 为真:它提交时会把 `released` 置 1,不能和本节点的下一次认领交错 |
| 存盘未落定时挂起加载(`RC:516`) | 等 ACK | 保留,接到新的在途状态上。同 E 销毁且 q9 未决时,新实体读到 S8,q9 晚到写入,库和内存就分叉了 |
| 崩溃接管 | saveoutbox 补投 + 屏障等待 | 没有补投;接管后读到的就是最后提交的状态 |
| 交易冻结 `CheckSql` | 读 Redis ACK | 最终存盘成功且实体已销毁;`VerifyFrozen` 改读 `player_store`。普通关闭(`CloseRoleAdmissions`)同样等存盘落定,见 §5.7 |
| 资产 `IsAssetOpDurable` | 只看 Redis 快照 | 快照现在就是 SQL 已提交,函数不改,语义变强 |
| 结算 `RequestSqlConfirmedSettlementAck` | PING + 探测 SQL ACK | `RunWhenSettled` 落定后看快照,`ConfirmSnapshotSqlCommit` 核对快照后同步回 true,再销账放锁(§5.5)。探测代码随旧路径删除 |
| 收件箱 `RequestSqlConfirmedAck` | 同上 | 同上 |
| 停机 | 15s 预算,Kafka 回执 | 全员最终存盘;预算按压测调,到期未完成的记录告警 |
| 账本读 `persisted_player_database.go`、`asset_sql_commit.go`、`asset_op_ledger_verified.go:83` | Redis 父快照、Redis 投影 | 强读 `player_store` 的 kWallet 行(v3 之前是 kAsset)。漏了 `asset_op_ledger_verified.go`,放弃就停摆 |
| 自动放弃判在线 | Redis 位置 | 一条语句同读写权行和 kWallet:`released=1` 才算不在线,不再判死活。判死分支删除(见下) |

**判死放弃要改(阻塞 B6,不阻塞 B1)。** 今天的序列是:scene 应用了 seq,然后和 go/db 断开或进程暂停一小时以上 → etcd 判死 → 放弃并退款 → 恢复之后 E=Er、持有者相同,账本照常提交,一物两得。现有 `playerAbsent`(`abandon.go:557`)只认位置键不存在,判死实现只在 scene_manager(`ES:1080`),写权行只存 uuid,其他服务判不了。改法:放弃不再判死活;scene_manager 判死后调 data_service(§5.5),在同一个事务里抬 E、置 `released=1`,之后旧持有者的写被 `STALE_OWNER` 挡住。`released=1` 分支靠租约与 `LastSendMs` 保证安全。`PF:95` 的比较也改成比写权行。

FIFO 依赖要逐处拆掉:PING 快路径、A2′ 先于 GET、重连补发顺序都假定同一 hiredis 连接 [C],新路径靠回调先后,不靠连接顺序。

## 7. 分批

每批不超过 30 个手写文件,落码后交 Codex 编译测试。**B1、B2 与 §15 的文件相交很少,可以先做;B5 选库部分与 §15 对齐。**

| 批 | 内容 | 对应总体方案 |
|---|---|---|
| B1 | `player_store` 表定义与建表入口;go/db `internal/playerstore`:判定纯函数、`Save`、`LoadAndClaim` 事务、确定性错误分类;单测与真库集成测试 | A1、A2 |
| B2 | `player_store` gRPC 域、go/db 服务端、鉴权、注册发现、配置与部署门禁;go/db 补存盘指标并接进 `stress_summarize`(否则 AGENTS §6 没有对比表) | A1 |
| B3 | C++ 分组投影与启动校验、`OptionPersistGroup`、`PlayerStoreClient`(单在途、重试、重签、回调);不接主流程 | A3 |
| B4 | C++ 存盘主流程接入开关:存盘、被拒、下线、交接、疏散、停机、快照语义、关键变化立即存、定时只存脏行 | A3 |
| B5 | 加载接入:scene `LoadAndClaim`;login 去掉预加载;建角初始行写新表;身份写入 kProfile、职业写入 kHot(`player_class_backfill.go:67`) | A4 |
| B6 | 判据替换:资产、结算、收件箱、交易冻结、账本读、自动放弃与判死 | A5 |
| B7 | 读方与工具:friend、match、login 外观改读投影或 TiDB;`route_bootstrap.go:37` 改查新表;`rollback_logic.go:776` GM 回档写 Redis 却回成功,改掉或 fail-closed;merge_zone 按清单会自动收 `player_store`,这一批之前 fail-closed;debug、barrierrestore、verifier、`robot/data_stress` 依赖探针与 DBTask,适配或 fail-closed | A5 |
| B8 | 全链路验收后翻默认值;按 TiDB 决策 §9.3 删除旧路径 | A6 |

**2026-10-30**:B1~B8 都已落码,未编译,待 Codex 验证。开关 `PlayerStorePath` 已删除,新路径是唯一路径;下面 §7.1 只作历史保留。

### 7.1 开关的约束

开关**单向、全局单源**:所有节点读同一份配置,部署门禁校验,翻转时清库(项目未上线,不做存量迁移)。混跑不安全:跨 zone 到 redis 节点会读旧父键,静默回档;data_service 与 scene 不一致时拿到 `NO_OWNER` 或空数据。缺省 redis 只是为了主干在切换批之前可运行,不是灰度手段。

## 8. 测试(Codex 执行)

- **判定表逐格单测**(§4 两张表)。
- **真 TiDB 集成**:并发 `Save` 与 `LoadAndClaim` 交错;老持有者在认领后写被拒;请求超时后重试、旧请求晚到;同序号重放不写两次;部分脏行之后读回的整体一致;`release` 后同实例再写成功并清除 `released`,他实例认领后原实例再写被拒;E>Er 且持有者是自己时放行并抬 Er,持有者是别人时拒绝;nil payload、kind 为 0、kind 重复;表不存在时 `VerifySchema` 拒绝启动;世代倒退;并发两个实例认领。
- **C++**:分组覆盖全部字段;片段拼接还原与整份序列化等价(含 map 确定性);**字段换组后库里两行各有一份,拼接不翻倍**;退役行(1、2、5)拒绝加载;顶层未知字段拒存;`confirmed` 用库里原字节播种;探针不造成恒脏;在途覆盖不丢行;结果未知重试不换序号、重新签名;`STALE_OWNER` 后的未决行并入脏集;未落定时不能重新播种;有 `pending` 时不触发全局完成;快路径下线发空行 release;回调恰好一次。
- **故障注入**:存盘中途 `kill -9` scene;go/db 重启;TiDB 切主;交接回包丢失;停机排空被打断。
- **回归**:L1–L4 一致性与混沌测试在新路径上重跑;文本契约测试(`currency_test.cpp`)同批改。
- **容量**(AGENTS §6):写池门禁每笔约 20 次非业务往返 [C],是本路径最大的已知容量问题,拆法见 §9 第 1 条;先加「每笔往返次数、分段耗时」指标拿基线,压测对比后再收。

## 9. 未决与风险

1. **写资格门禁的开销**:每笔存盘约 22 条语句 [I],2000 万在线下不可接受,但这是 Codex 全球同服线的框架,先不动,压测拿数据后再改。2026-11-05 按代码逐条数过 [C](`go/shared/authoritysql`):
   - **每次从池里取连接**:`database/sql` 复用连接前调 `guardedConn.ResetSession`(`driver.go`),它每次都调 `connector.pin`(`pool.go`):逐条发 3~4 条基础 `SET`(autocommit、隔离级别、sql_mode,可选锁等待)和 7 条 `tidbSettings`,共 10~11 次往返;再读 `CURRENT_USER(),DATABASE()`;写者池还要 `readRawMeta` 和 `verifyBootRegistration`(读 `writer_registry`、`writer_lifecycle_receipt` 两条)。mysql 档另加 `verifyMySQLConnection`。
   - **每笔事务进门**:`runtimeGate.beforeBegin` 读 `route_meta`;`runtimeAttempt.enter` 锁一行 `writer_slot`(`SELECT_ONLY` 档 `FOR UPDATE`,`LOCK_TOUCH` 档多一条 `UPDATE`)、读 `writer_slot`、再读 `route_meta`、再读 `writer_registry`,共 4~5 次(`writer.go`)。
   - 加上 BEGIN、业务 3 条、COMMIT,一笔存盘约 24 次往返,业务只占 3 次。读已提交下每条读语句还要各取一次时间戳(总体方案 §8.1 第 3 条)。按总体方案 §4.4 的推算口径(2000 万在线约每秒 17 万笔),每笔少十几次往返就是每秒少两三百万次 [I]。
   - **收敛方向**(闸门语义不动:不停服搬库要靠它冻结;改动在 `authoritysql`,所有服务的写者池一起受益):①会话设置合成一条 `SET SESSION a=…, b=…`,只在新建物理连接时做,复用时不重设(业务代码不改会话变量,加契约测试钉住;mysql 档的只读核对合成一条 `SELECT` 保留);②`verifyBootRegistration` 只在新建连接时做,复用时靠每笔事务的 slot 状态和 `writer_registry` 判定;③每笔事务的几次读合成一两条,但锁 slot 必须先于读 meta(冻结要拿全部 slot 锁才能推进 epoch,这是闸门和冻结串行的依据)。目标每笔约 6 次往返:BEGIN、闸门 1~2、锁写权行、多行写入、COMMIT。
   - **顺序**:先加指标(每笔存盘的往返次数、pin / gate / 业务 / 提交各段耗时,不带 player_id),按 AGENTS §6 拿基线;①是同语义的合并,风险最低,先做;②③改了判定时机,要过一轮正确性评审(冻结、退休、搬库并发下的交错)再落码,有对比表再宣称收益。
2. **身份写入**:已定,见 §5.4。字段来源是 `AccountSimplePlayer`(`class_id`、`name`、`appearance_id`、`gender`),建角事务里已有。
3. **C3 写方令牌**(`redis_client.h` 严格脚本 [C]):B4 核实后决定新路径不用。令牌防的是「加载到的数据比屏障记录的世代旧」,那是多跳链路上有一跳截断造成的;新路径只有一个来源,世代比本进程新在认领时就拒绝(`SCHEMA_TOO_NEW`),低世代的旧持有者先被代次挡住。旧路径删除时一并删除令牌。
4. **go/db 一进程一 zone**:scene 连本 zone 的 go/db,访客的落点库走按需打开的小池(8/2 [C])。访客多时要调大,压测定。
5. **副本数门禁**:`replicas ≤ PartitionCnt`(K8S:2359)是 Kafka 消费的约束,切换后改掉;写者 roster 上限 1024 [C] 暂时够用。
6. **relocate、合服**:新路径下 relocate 暂时 fail-closed,多集群阶段再做;别名合服见总体方案 §7.1。
7. **快照**:`snapshot_system.cpp` 发两表整份进 Kafka,不受影响;恢复路径本来就没有 [C]。
8. **可接受的残余(评审确认)**:`released=0`、持有者活着却没有实体(`PL:6554`、载入放弃)时,放弃要等到下次登录,不丢不复制。进程退出不一定能解:判死释放要求 location 还在,接管后凭证回 `UNCLAIMED` 或被撤销时 location 已删,库里停在旧持有者、`released=0`。Redis 回退且时钟落后时 E<Er,进场失败到时钟追上,是否要运维工具待定。旧 scene 读新世代行被拒,是滚动期的可用性问题,未上线不处理。1205/1213 后隔离连接的重连开销,压测定。
9. **评审时接受不修的残余**:
   - **无状态释放结果未知后晚到**:分区时释放回结果未知,同节点重登并认领,旧请求之后才落地,q 更大,判 `COMMITTED`,库里 `released=1` 而实体还在。持有者不变,后面的存盘不受影响;加载后第一笔是强制写,会把它清掉,窗口不超过一次定时存盘(约 60 秒)。这段时间自动放弃可能把玩家判成不在线,但它还要过 `AbandonSafetyGap`,而操作投递时 `LastSendMs` 是新的,实际挡得住。
   - **旧 E 的认领被拒**:认领在途时条目被新路由覆盖,而旧 E 已经低于库里,回 `STALE_OWNER` 就走载入失败,不用新 E 再试。要在几毫秒内跨三跳才碰得到,失败关闭,客户端重登即可。
   - **自动放弃的多重故障窗口**:退出存盘已提交 `released=1`,玩家重连取消退出;scene 到 go/db 持续不通,清 `released` 的那一笔一直失败;操作在此期间被应用;guild 到 scene 也断了超过 `AbandonSafetyGap`(缺省 1 小时),`LastSendMs` 不再刷新。恢复后放弃退款,scene 又提交成功,就是一物两得。要几个长故障同时发生,不属于「正常运行不丢」的范围,接受;上线前再复核一次。
   - **login 的 apply 阶段占住 `PreloadPool` 的工作线程**:新路径没有预加载,`onPreloadComplete` 在工作线程上同步跑完,里面有对 scene_manager 的同步调用。旧路径的缓存命中分支已经是这样;压测时看 `entergame_total{result="pool_saturated"}`,不够就调大 `PreloadPool.Size`。
10. **按表授权的部署**:翻开关之前要把 `player_store` 加进 `maintenance_drain.ps1` 的 `$Plan.tables`,go/db 和 data_service 的运行账号都要 `SELECT,INSERT,UPDATE`。旧路径上 `save_barrier_restore`、`merge_zone` 的账号也要能读这张表:它们靠读它判断库里是不是新路径,没有读权限(1142)就拒绝运行。

11. **崩溃的持有者卡住角色交易(2026-10-30 定,B7 留下的待决项)**。崩溃的 scene 实例永远不发 `AckDrain`,它的 `account_role_admission` 也永远不关,交易会卡在 FREEZING,取消卡在 CANCEL_PENDING,这个角色之后每一笔交易都会再次卡住。旧的人工恢复工具建立在 Redis 屏障和两张旧表上,已删除。
    - **判据**:持有者 I 能提交存档的前提是写权行的 `holder_instance` 是它。释放不是终态(§4),`released=1` 的现任也算现任,断线后 30 秒内重连会复用实体接着存。`fenceDeadTradeHolders`(`go/data_service/internal/store/account_role_dead_holder.go`)在 Begin(含重放)、Confirm、Cancel 的事务里检查:只要有一个未排空的持有者还是写权行的现任,就什么都不做,它可能活着、内存里还有没落盘的进度。否则这些持有者都写不了存档,只剩「认领在途」一种可能。认领带的是登记时的代次(`RoleAdmissionIdentity.owner_epoch`),那是铸造时的墙钟,通常远大于库里的代次,所以要把代次抬到**所有待判持有者登记的代次之上**,否则在途的认领照样成功,冻结证据作废(评审第 1 条,10-30 修正)。判为排空,关掉登记。`trade_listing_id` 非 0 时 `Register` 拒绝,持有者拿不到更高的代次;下一个合法持有者的代次由 scene_manager 铸造,取「观察值 + 1」和墙钟的较大者,不小于这个值。
    - **迟到的真实 ACK 和 Close**:按成功重放,不算冲突(判死时 `ack_digest`、`close_digest` 记标记)。过户或删角之后迟到的 Close 带旧修订:旧修订的持有者当时已经排空或判死,没有东西要关,回成功,不改登记行,scene 就此停止重发(2026-10-31,原来回冲突,scene 会每秒空转)。
    - **现任崩溃**:scene_manager 判死时释放(`ReleaseDeadPlayerStoreHolder`)。节点在写 location 之前就死了,或者玩家释放之后、关登记之前死了,判死够不着,运维确认实例已死之后用 `go/data_service/cmd/admission_recover` 摘掉,交易的下一次重试就能走完。
    - **代价**:现任活着却被运维误摘,它内存里没落盘的进度会丢。

## 10. Java 版(AGENTS §12)

Java 版的存档与写权已在 SQL 单事务里(`PlayerStore`)[C],形状一致,内部实现不要求相同。本规格不改客户端契约。对应 Java 版本 0.1.0-SNAPSHOT,落码时核对是否需要补差异。
