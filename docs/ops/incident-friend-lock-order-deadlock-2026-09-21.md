# 事故报告:friend 锁定读被规划成索引全扫描,锁集越出守卫,真死锁 1213

- **日期**:2026-09-21(机器 B,`D:\luyuan\wuxingqitan\mmorpg`,时间为本地 -04:00)
- **级别**:P1(产品缺陷,可稳定复现的死锁)。**线上影响:无** —— friend 服务从未部署,本次是它第一次在真 MySQL 上跑并发回归。
- **发现方式**:交接文档 `docs/design/friend-handoff-20260920.md` §2 第 6 步"真 MySQL 并发回归"首跑。
- **状态**:已修复,真库验证通过;**截至本文写入时修复代码尚未提交**(以 `git log -- go/friend/internal/data/friend_repo.go` 为准)。
- **一句话**:好友服务的几条 `SELECT ... FOR UPDATE` 写成了 `OR` / `IN`,优化器把它们规划成索引全扫描,锁到了**别的玩家对**的行上,与那一对玩家"先主键后二级索引"的 `DELETE` 取锁顺序相反,成环。守卫只串行化"这一对玩家",所以锁集一旦越界,守卫就失去了意义。

---

## 1. 现象

`go/friend/internal/data` 在真 MySQL 上首跑(`FRIEND_TEST_MYSQL_DSN` + `FRIEND_REQUIRE_MYSQL_TESTS=1`):**71 PASS / 2 FAIL / 0 SKIP**。

```
friend_guard_lock_order_mysql_test.go:678: 容量行回收与写路径并发 触发 InnoDB 死锁(1213):… 严格组 writer 0 第 2 轮 Unblock:
  unblock 87000001->87000002: Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction
--- FAIL: TestCapacityRowReclaimRacesWithGuardedWrites (13.09s)
```

单独重跑这一个用例,**第一次就复现**,这次撞在另一条路径上:

```
严格组 writer 0 第 20 轮 RemoveFriend: delete friend edge 87000001->87000002: Error 1213 (40001): Deadlock found …
```

另一个失败 `TestAcceptFriend_RejectsBlockedPair` 与死锁无关,是测试夹具自相矛盾(见 §7.4)。

**环境**:`mysql:latest` 实为 **MySQL Community Server 26.7.0**(不是 TiDB);全局 `REPEATABLE-READ`,friend 写事务显式 `READ COMMITTED`;`binlog_format=ROW`;`innodb_deadlock_detect=1`。测试库 `mmorpg_friend_test`,表由测试夹具建,数据量极小(每个用例几十行)。

## 2. 时间线

| 时刻 | 事件 |
|---|---|
| 09-19 | friend 移植 F2 批(`f06090b19`)写下 `blockedEitherWay` / `friendEdgeExistsForUpdate` 的 `OR ... FOR UPDATE`、`lockCapacityRows` 的 `IN (...) FOR UPDATE`、`Block` 取消 pending 的 `OR` 形 `UPDATE`、sweep 的批量 `DELETE ... LIMIT`。整批从未编译、从未运行 |
| 09-20 | 交接文档 §3 第 2 条登记:"三条锁序假设从未在真 MySQL 上 `EXPLAIN` 核对 —— `OR` 形式必须走 index_merge / range 且 key 含 PRIMARY,退化成全索引扫时 `FOR UPDATE` 的锁集当场越出守卫域,ABBA 立刻可能成环"。**风险被准确预言,但无人核对** |
| 09-20 | 收尾批新增并发场景 `TestCapacityRowReclaimRacesWithGuardedWrites`(多个写者各占一对玩家,混跑 AddFriend / AcceptFriend / RemoveFriend / Block / Unblock + 回收) |
| 09-21 约 05:39 | 首次真库回归,该场景 1213 失败(撞在 Unblock) |
| 约 05:42 | 单独重跑第一次即复现(撞在 RemoveFriend 删边);抓取 `SHOW ENGINE INNODB STATUS` 的 `LATEST DETECTED DEADLOCK`(§3.1) |
| 约 05:44 | 对全部锁定语句跑 `EXPLAIN`(§3.2),确认机制,并发现另外三处同类问题 |
| 约 05:45–05:49 | 修复(§5)+ 确定性回归用例 |
| 约 05:50 | `internal/data` 真库重跑 74 PASS / 0 FAIL / 0 SKIP |
| 之后 | 8 个并发锁序场景连跑 5 轮 40/40 PASS;`go/friend` 全部包 314 PASS / 0 FAIL / 0 SKIP |

## 3. 根因(逐环,均有证据)

### 3.1 死锁现场(`LATEST DETECTED DEADLOCK` 原文,去掉十六进制行)

```
*** (1) TRANSACTION:
TRANSACTION 40426, ACTIVE 0 sec updating or deleting
DELETE FROM friend_block WHERE player_id=87000141 AND blocked_player_id=87000142

*** (1) HOLDS THE LOCK(S):
RECORD LOCKS … index PRIMARY of table `mmorpg_friend_test`.`friend_block` … lock_mode X locks rec but not gap
Record lock, heap no 5 …

*** (1) WAITING FOR THIS LOCK TO BE GRANTED:
RECORD LOCKS … index idx_blocked_player of table `mmorpg_friend_test`.`friend_block` … lock_mode X locks rec but not gap waiting
Record lock, heap no 5 …

*** (2) TRANSACTION:
TRANSACTION 40416, ACTIVE 0 sec fetching rows
SELECT 1 FROM friend_block
     WHERE (player_id=87000101 AND blocked_player_id=87000102) OR (player_id=87000102 AND blocked_player_id=87000101)
     LIMIT 1 FOR UPDATE

*** (2) HOLDS THE LOCK(S):
RECORD LOCKS … index idx_blocked_player of table `mmorpg_friend_test`.`friend_block` … lock_mode X locks rec but not gap
Record lock, heap no 5 …

*** (2) WAITING FOR THIS LOCK TO BE GRANTED:
RECORD LOCKS … index PRIMARY of table `mmorpg_friend_test`.`friend_block` … lock_mode X locks rec but not gap waiting
Record lock, heap no 5 …

*** WE ROLL BACK TRANSACTION (1)
```

读法:heap no 5 就是 **141/142 那一行**。事务 (2) 是 **101/102** 的 `blockedEitherWay`,它手里拿着 141/142 那一行在二级索引 `idx_blocked_player` 上的锁 —— 一个与它毫无关系的玩家对。事务 (1) 是 141/142 的 `Unblock`。

### 3.2 执行计划(`EXPLAIN`,同一个库)

| 语句 | 计划 | 后果 |
|---|---|---|
| `blockedEitherWay`:`SELECT 1 FROM friend_block WHERE (player_id=? AND blocked_player_id=?) OR (…) LIMIT 1 FOR UPDATE` | **Covering index scan on friend_block using idx_blocked_player** + Filter | 二级索引全扫描,逐条加锁 |
| `friendEdgeExistsForUpdate`:同形,`friend` 表 | **Covering index scan on friend using idx_friend_player** + Filter | 同上 |
| `lockCapacityRows`:`… WHERE player_id IN (?, ?) ORDER BY player_id FOR UPDATE` | **Index scan on friend_capacity using PRIMARY** + Filter | 主键全扫描(不成环,但见 3.4) |
| `Block` ⑥:`UPDATE friend_request … WHERE status=? AND ((…) OR (…))` | `type=range key=idx_status_updated key_len=4` | 扫**全服** pending 行,先二级索引后主键 |
| 主键点查 `SELECT 1 FROM friend_block WHERE player_id=? AND blocked_player_id=? FOR UPDATE` | `const` | 只锁那一行主键 |

两个二级索引恰好都是**覆盖索引**(`idx_blocked_player (blocked_player_id)` 在 InnoDB 里隐含主键列,等价于 `(blocked_player_id, player_id)`,查询只用到这两列),表又很小,优化器判断"扫整个覆盖索引"最便宜。

### 3.3 成环机制

1. `SELECT ... FOR UPDATE` 经二级索引扫描时,InnoDB 对**每条扫到的**二级索引记录加锁,再去锁对应的聚簇(主键)记录,之后才在 MySQL 层判断 WHERE。RC 下不匹配的行会在判断后释放,但**加锁这一步必须先等到** —— 而 `SELECT ... FOR UPDATE` 不走 semi-consistent read(那是 `UPDATE` / `DELETE` 才有的),所以会真的去等。取锁顺序:**二级索引 → 主键**。
2. `Unblock` / `deleteFriendEdges` 是按主键的 `DELETE`:先锁聚簇记录,再锁该行的二级索引项。取锁顺序:**主键 → 二级索引**。
3. 两者撞在同一行上,顺序相反 → 环。
4. 容量守卫为什么没挡住:守卫只让"涉及同一个玩家"的事务串行。101/102 的事务和 141/142 的事务**不共享任何玩家**,守卫本来就不管它们 —— 它们之所以相遇,完全是因为锁集越过了自己那一对。代码注释里"这两行都在'本对玩家'范围内……所以锁集不会跨对交叉"的论证,只在执行计划是主键点查或 index_merge 时成立。

### 3.4 同类但未直接成环的两处

- `lockCapacityRows` 的主键全扫描按主键序取锁,所有守卫的获取都是升序,因此不成环;但每个事务都要**依次等到所有更小 `player_id` 上正被别人持有的守卫行**,等于按玩家号把全部好友写路径串行化 —— 吞吐问题,且对延迟不可预测。
- `Block` ⑥ 的 `UPDATE` 和 sweep 的批量 `DELETE`:先二级索引后主键,与 `AddFriendRequest` 的 upsert(先主键,`④` 的 `FOR UPDATE`)在同一行上反序。前者锁全服 pending 行;后者只在"玩家重新申请一个保留期外的旧终态行、恰逢 sweep 正在删它"时撞上,而且 sweep 一次删上千行、undo 大,InnoDB 更可能牺牲**玩家那一侧**。

## 4. 这段代码为什么会存在

1. **为省一次往返写成了 `OR`**。原注释:"单条 OR 查询而不是查两次……一次往返即可;RC 下未命中不加任何锁,命中则锁住那一行"。这个论证默认了执行计划是两次主键点查,而 SQL 文本本身并不保证这一点。
2. **风险被预言了,但核对依赖运行环境**。交接文档 §3 第 2 条准确写出了"退化成全索引扫就会越出守卫域",但它是"运行期核对"项;整批代码此前从未编译、从未连过真库(CI 不设 MySQL DSN,真库用例"SKIP 在报告里等于绿")。
3. **原有的 5 个锁序场景抓不到它**。它们要么共享同一个目标玩家(守卫本来就会串行),要么用互不相干的目标但不混入 `Unblock` / 删边;要让锁集越界的扫描与另一对玩家的主键 `DELETE` 撞在同一行上,需要"多对玩家并发 + 删除操作"。收尾批新增的回收并发场景恰好满足这两点。

## 5. 修复

### 5.1 规则(写进 `go/friend/internal/data/friend_repo.go` 顶部锁序说明 (6))

**守卫之后的锁定读 / 锁定写,一律写成"完整主键的等值点查 / 点更新",不许用 OR / IN / 前缀范围。** 完整主键等值的执行计划是 `const`(`UPDATE` 为 `key=PRIMARY`、用满全部主键列),与统计信息无关;RC 下未命中不加锁,命中只锁那一行的聚簇记录、**不碰任何二级索引项** —— 于是与"先主键后二级索引"的写不可能反序。

⚠ **前提:WHERE 不能同时把另一个唯一索引的全部列也钉死。** 若表上还有唯一二级索引、且它的列也全部出现在等值条件里,优化器在两个 `const` 路径之间任选其一,可能走唯一二级索引(先锁二级索引项、再锁主键),规则就失效。friend 的四张表按 D-14 **零 UNIQUE KEY**,所以本修复成立;而且 §6.2 的回归断言的是 `key = PRIMARY`,真被规划到别的唯一索引上也会变红。这一条是帮会会话核对 `guild_member`(主键 `(guild_id, player_id)` + 唯一键 `uk(player_id)`)时指出的(2026-09-21),推广到别的服务时必须一起检查。

### 5.2 改动清单(6 个文件,均在 `go/friend/internal/`)

| 位置 | 改动 |
|---|---|
| `data/friend_repo.go` `blockedEitherWay` | `OR` → 两次主键点查(常量 `lockBlockRowSQL`),任一命中即返回 |
| `data/friend_repo.go` `friendEdgeExistsForUpdate` | 同上(`lockFriendEdgeRowSQL`) |
| `data/friend_repo.go` `lockCapacityRows` | `IN (...) FOR UPDATE` → 按升序逐个主键点查(`lockCapacityRowSQL`);点查顺序即取锁顺序 |
| `data/block_repo.go` `Block` ③ | 名额 `COUNT(*) ... FOR UPDATE` → 守卫内**普通读**:能让它变大的只有 `Block` 自己,而它必须先持有同一把守卫;RC 每条语句新快照。与 `AddFriendRequest` ⑤⑥ 同一论证。`COUNT(*)` 天然偏好小覆盖索引,留着 `FOR UPDATE` 迟早会在统计信息变化时重蹈覆辙 |
| `data/block_repo.go` `Block` ⑥ | `OR` 形 `UPDATE` → 两条按主键的点更新(`cancelPendingRequestSQL`) |
| `data/sweep_repo.go` `deleteTerminalRequestsBefore` | 批量 `DELETE ... LIMIT` → 候选普通读 + 逐行按主键删(`deleteTerminalRequestSQL`,WHERE 重复终态与截止点作提交点复核),与容量行回收同一写法;出错时带回已删行数 |
| `logic/sweep.go` | 终态清理失败日志带上"看到 / 已删"行数 |
| 测试:`data/friend_guard_lock_order_mysql_test.go` | 新增确定性回归 `TestLockingStatementsArePrimaryKeyPointLookups`(§6.2) |
| 测试:`data/friend_repo_mysql_test.go` | 修 `TestAcceptFriend_RejectsBlockedPair` 的夹具矛盾(§7.4) |

保留不改的:`friendEdgeExists`(`RemoveFriend` 的事务外前置判定)仍是 `OR`,但它是**普通读、不加锁**,不在本规则范围内。

### 5.3 代价

一对玩家的"任一方向"判定从一次往返变成两次;容量守卫从一次往返变成两次;sweep 终态清理一轮至多 `BatchLimit` 次往返(默认 1000,5 分钟一轮)。均可接受。

## 6. 验证

### 6.1 真库回归(MySQL 26.7.0)

| 轮次 | 结果 |
|---|---|
| 修复前,`internal/data` 首跑 | 71 PASS / **2 FAIL** / 0 SKIP |
| 修复前,单独重跑回收并发场景 | 第 1 次即 FAIL(1213) |
| 修复后,`internal/data` | **74 PASS / 0 FAIL / 0 SKIP** |
| 修复后,8 个并发锁序场景 `-count=5` | **40/40 PASS**,场景 (g) 以外零 1213 |
| 修复后,`go/friend` 全部包(真库) | **314 PASS(含子用例)/ 0 FAIL / 0 SKIP**;`gofmt` / `go build` / `go vet` 零输出 |

日志存档(仓外):`D:\luyuan\wuxingqitan\friend-lockorder.log`(修复前首跑,后被修复后的全量结果覆盖)、`friend-lockorder-x5.log`、`friend-alltests.log`、`friend-deadlock.txt`(`SHOW ENGINE INNODB STATUS` 原文)。

### 6.2 确定性回归:`TestLockingStatementsArePrimaryKeyPointLookups`

对**生产代码里同一个 SQL 常量**(不在测试里另抄一份)做 `EXPLAIN FORMAT=TRADITIONAL`,断言 `key = PRIMARY`、`key_len` 用满全部主键列、SELECT 的 access type 为 `const`。覆盖 `lockCapacityRowSQL`、`lockBlockRowSQL`、`lockFriendEdgeRowSQL`、`cancelPendingRequestSQL`、`deleteTerminalRequestSQL`。**旧写法三处都会被它拦下**(§3.2 的计划:`type=index` / `key=idx_blocked_player` / `key=idx_status_updated`)。它不依赖并发时序,每次运行结论相同;并发场景只能按概率撞上这类问题。

### 6.3 附带证实的一条推演

评审时推演、未曾复现的"回收留下 delete-marked 记录后,并发 `INSERT IGNORE` 在 S→X 升级上成环",这次在真库上**复现**了(日志:`[friend] WARN ensure 容量行撞上 1213 … attempt=1/3`),被 `ensureFriendCapacityRows` 的有限重试吸收,场景 (g) PASS。

## 7. 遗留与后续

### 7.1 【待提交】修复代码尚未提交

6 个文件见 §5.2。交接文档与 `PROGRESS.md` 可能先于代码进库,以 `git log -- go/friend/internal/data/friend_repo.go` 为准。

### 7.2 【待核,不属本服务】其它 Go 服务里同类形状的锁定语句

全仓扫描 `FOR UPDATE`(排除测试与生成物)后,多数已是主键点查;以下几处**形状可疑,需要在真 MySQL 上 `EXPLAIN` 核对**,方法同 §6.2:

| 位置 | 语句形状 | 疑点 |
|---|---|---|
| `go/guild/internal/data/guild_manage_repo.go` `lockMemberPair` | `… WHERE guild_id = ? AND player_id IN (?, ?) ORDER BY player_id FOR UPDATE` | **帮会会话已核实表结构**:`guild_member` 主键 `(guild_id, player_id)`,另有唯一键 `uk(player_id)` —— 除了与 `lockCapacityRows` 同形的全扫描风险,还可能被规划到 `uk_guild_member` 上(二级 → 主键),与按主键删成员行反序 |
| 同文件 `sqlLockMemberRole` | `SELECT role FROM guild_member WHERE guild_id = ? AND player_id = ? FOR UPDATE` | 是完整主键等值,但 `player_id = ?` 同时钉死了唯一键 `uk(player_id)` —— 正是 §5.1 那条前提不成立的情形 |
| 同文件 `sqlLockMyApplications` | `SELECT guild_id FROM guild_application WHERE player_id = ? FOR UPDATE` | **已核实**:`guild_application` 主键 `(guild_id, player_id)`,本句走二级索引 `idx_guild_application_0`,先二级后主键 |
| `go/shared/assetop/seq.go` `AllocateSeq` | `… WHERE player_id = ? AND stream = ? AND stream_epoch = ? AND status = ? ORDER BY seq LIMIT ? FOR UPDATE` | 带过滤的范围锁,理想路径是 `idx_guild_asset_op_2` 的等值前缀,锁集取决于索引选择 |

**处置(2026-09-21)**:帮会 B5b 会话对**本批新增**的锁定 / 写语句照 §6.2 加确定性 `EXPLAIN` 回归;上表四处是既有代码(前三处属帮会 B2,第四处属聚宝斋),已由帮会会话写进它的交接文档**交用户决定**,本批不改。本报告只登记,friend 会话不动 `go/guild` / `go/shared`。

### 7.3 【说明】规模上的结论

修复后的锁定语句都是完整主键等值,执行计划与数据量无关。**非锁定**的候选读(sweep 的两条候选 `SELECT`)在小表上同样被规划成全索引扫描,只影响性能不影响正确性;上线后应在真实数据量下再 `EXPLAIN` 一次,确认走 `(status, updated_ms)` / `(friend_count, created_ms)` 的范围。

### 7.4 【已修,非产品缺陷】`TestAcceptFriend_RejectsBlockedPair` 夹具自相矛盾

原移植即有。夹具直写 `friend_block`、绕过 `Block()`,故意造出"拉黑与 pending 并存"去测 `AcceptFriend` ② 的守卫内拉黑复核,末尾却调用含"拉黑后不得有 pending"的 `assertFriendInvariants` —— 断言的是夹具自己造的状态。改为逐条调用其余三条不变量,并显式断言"被拒的 `AcceptFriend` 整体回滚、申请行仍为 pending"。

### 7.5 【未做】交接文档 §2 第 7–9 步

`-migrate` + 常驻启动、缓存手工核对、两区 robot `friend-smoke` 尚未运行。

## 8. 教训

1. **锁集由执行计划决定,不由 SQL 文本决定。** 守卫这类"只串行化一小撮行"的设计,必须让锁集与执行计划无关 —— 完整主键等值是唯一不需要信任优化器的写法。"写成 OR 省一次往返"换来的是把正确性押在统计信息上。
2. **小表是最坏情况,不是最好情况。** 小表上优化器最爱全扫描;测试环境与新服刚开时恰恰是小表。
3. **"需要运行期核对"的风险项要有人认领执行时间。** 这条风险在交接文档里被准确预言,却因为"要真库"一直挂着;第一次真库运行就兑现了。
4. **确定性回归优先于概率性回归。** 并发用例能发现问题,但证明"修好了"要靠 `EXPLAIN` 这类每次结论相同的断言。
