# S1 存储:帮会迁独立库 mmorpg_guild(B1/B1b)
> 本节由 7 个分部合并而成(原分部名保留在小标题里),另附对抗评审处理记录。

<!-- s1_storage_part1.md -->

# S1 存储:帮会迁独立库 `mmorpg_guild`(批次 B1 / B1b)— 第 1 部分

> 状态:设计稿(2026-09-16 按对抗评审修订,见 `s1_storage_review.md`),未落码、未编译,待 Codex 验证。本节遵守 `00_contract.md`,偏离处列在第 7 部分 §21。行号为 2026-09-16 核对值;并行会话在改同一工作树,落码前按函数名重新定位。

## 0. 范围与结论

**用户决策(契约 §0 第 4 条)**:帮会全部表迁入独立库 `mmorpg_guild`,proto 为源,由 `go/schemamigrate` 迁移(照 go/trade),同时修订 D-14 §8。项目未上线,无存量数据,不搬数据、不写版本字段。

**硬前置(不满足不开工)**:go/guild 实际解析到的 proto2mysql 必须含 `TextIndexPrefixLength = 191` 与 TiDB 表选项(§1 第 1–3 行、第 7 部分 §18 第 0 步)。

B1 只做存储地基,不改客户端协议。手改文件 23 个(第 6 部分 §17):
1. 新建 `proto/guild/guild_db.proto`,只含 B1/B2 立即要用的 4 张表:`guild`、`guild_member`、`guild_application`、`guild_player_state`。`guild_player_op_seq`、`guild_asset_op`、`guild_daily_counter`、`guild_activity_progress`、`guild_trial_battle` 由 B5/B6a/B6b 在各自 PR 里加 message 和 `Tables()` 条目,schemamigrate 会补建后加的表(§1 第 12 行)。
2. go/guild 接入 schemamigrate:新增 `-migrate` 入口;启动期按 `Schema.AutoMigrate` 执行 Up 或 Plan;锁忙重试;缺索引拒启;go 指令升到 1.26.5;proto2mysql 解析方式与 schemamigrate 逐字一致。
3. 建库与授权只登记在 `00_init_zone_dbs.sql`;从 `guild_friend_tables.sql` 删除帮会表与遗留迁移过程,好友表留在 `mmorpg`。
4. 删除 `guild_schema_migration` 与 `MigrateLegacyRankScores`,同步拆掉 friend 对这张门表的依赖。
5. repo 改列:`guild.funds`、`guild.name_norm`(帮名唯一键,Go 侧规范化),`guild_member.contribution_total / contribution_balance`(替换 `contribution`);`GuildData/MemberData` 的时间戳改为 `uint64`。
6. zrpc `Timeout` 10000→4000,`DataServiceRpc.Timeout` 3000→2000;新增整请求预算拦截器;`config.Validate` 先调嵌入的 `RpcServerConf.Validate`,再校验超时预算和库名。
7. 测试建表改走 schemamigrate(测试库名白名单);本地 start_game 预检新库;C++ 生成物 `guild_db.pb.cc/.h` 手工登记;K8s migrate Job 只记录,不落地。

**B1b(紧随 B1,手改 9 个)**:tools/merge_zone 与 tools/data_consistency_check 以 `-guild-schema`(默认 `mmorpg_guild`)限定帮会表;merge_zone 的重名探测改比 `name_norm`。拆批原因:B1 加上 C++ 登记三件后共 32 个,超过 30(§21 偏差 2)。**B1 合入到 B1b 合入之间不得执行真实合服**(未上线,本地也没有合服演练排期)。

**不在 B1**:`guild.proto` 的 `GuildMember/GuildInfo` 字段扩展(放 B2,随客户端重生成一起做);新表读写逻辑(B2/B5/B6);`guild_asset_op` 号段 biz_tag 进 BootstrapTags(B5);repo 级死锁重试助手(B2,见 §2.1 规则 5)。

## 1. 现状证据(2026-09-16 核对)

| # | 事实 | 位置 |
|---|---|---|
| 1 | 已提交的 schemamigrate 钉 `github.com/luyuancpp/proto2mysql v0.1.1 h1:s5du1n…`;模块缓存中该版本 `.info` 时间 2026-07-29,内容无 `TextIndexPrefixLength`/`SHARD_ROW_ID_BITS`,自带 go.mod 写 `retract ([v0.1.0, v0.1.1] …)`。在它上面给 MEDIUMTEXT 建唯一键会报 MySQL 1170 | `git show HEAD:go/schemamigrate/go.sum`;`%GOMODCACHE%/github.com/luyuancpp/proto2mysql@v0.1.1/go.mod:5-8` |
| 2 | trade 会话的未提交改动:schemamigrate 与 go/trade 两个 go.mod 都加了 `replace github.com/luyuancpp/proto2mysql v0.1.1 => github.com/luyuan-cpp/proto2mysql v0.1.1`,go.sum 换为 `h1:GsmiAK…`。该替换源 = commit `e90a5f0`(2026-08-26),含 `TextIndexPrefixLength = 191`(`proto2mysql.go:485`)、`uk_<表>`(`:582`)、`idx_<表>_<n>` 从 0 起(`:560-566`) | `git diff go/schemamigrate/go.mod go/trade/go.mod`;`%GOMODCACHE%/github.com/luyuan-cpp/proto2mysql@v0.1.1/` |
| 3 | replace 只在主模块生效:go/guild 作为主模块时继承不到 schemamigrate 的 replace,必须自己写同一行 | Go modules 规则;go/trade 的未提交 go.mod 即如此处理 |
| 4 | 上游分支 `feat/string-key-columns`(7dbda68,未打 tag)把键列里的 string 改成 `VARCHAR(N) … COLLATE utf8mb4_0900_bin`,对旧 MEDIUMTEXT 键列报 `ErrLegacyKeyColumn` | `E:/work/proto2mysql` `git log -1 7dbda68` |
| 5 | guild/guild_member/guild_schema_migration 手写 DDL 在 `mmorpg`;遗留迁移过程在同文件 | `deploy/mysql-init/guild_friend_tables.sql:6-40`、`:69-210` |
| 6 | guild 启动先跑 `MigrateLegacyRankScores`,失败 panic;随后 `RebuildRanks` | `go/guild/guild.go:137-145` |
| 7 | friend 启动与写事务都读 `guild_schema_migration` 的 `friend_capacity_backfill_v1` | `go/friend/friend.go:45`;`friend_repo.go:26,59-93,202,341,412` |
| 8 | guild.yaml:`Timeout: 10000`(:4);DSN 连 `mmorpg`,账号 root(:38);`DataServiceRpc.Timeout: 3000`(:68) | `go/guild/etc/guild.yaml` |
| 9 | 归属区查询 1500ms,从请求 ctx 派生;发号 `idsegment.Next(ctx)` 等请求 ctx,单次领段另有 `FetchTimeout` 默认 3s(后台 ctx) | `logic/home_zone.go:28,50`;`shared/idsegment/client.go:256,279-362,456` |
| 10 | trade 做法:`InBandReplyReserve = 500ms`,`RequestBudget() = Timeout − 500ms`,逻辑入口套预算 ctx | `go/trade/internal/config/config.go:29-35,138-140`;`logic/jubaozhai_logic.go:80-92` |
| 11 | go-zero v1.9.2 `RpcServerConf.Validate()`(Auth=true 时校验 Redis);guild `Config` 嵌入它;`conf.Load` 自动调 `Validator` | `go-zero@v1.9.2/zrpc/config.go:82-88`;`core/conf/config.go:69,97`;`go/guild/internal/config/config.go:11-12` |
| 12 | schemamigrate:漂移阶段对缺失的表生成 `CREATE TABLE IF NOT EXISTS`;多余列 / 缺普通索引只进 `Warnings`;索引只按名字比对;`Report.Clean()` 忽略 Warnings | `schemamigrate.go:153-162,253,279`;`plan.go:306,330,344-356` |
| 13 | schemamigrate API:`Options{Database, Tables, Logf, AdvisoryLockWait…}`、`Plan`/`Up`/`ExitCode`、`ErrLockBusy`(默认等锁 10s)/`ErrDatabaseMismatch`/`ErrDirty` | `schemamigrate.go:96-98,112-192` |
| 14 | trade 模板:`-migrate` flag、`schemaRunner`/`schemaOptions`/`ensureSchema`/`runMigration`/`reportLines`/`printReport`/`logReport` | `go/trade/trade.go:322-427` |
| 15 | go_services.ps1 允许 guild 多开 | `tools/scripts/go_services.ps1:155` |
| 16 | 帮名唯一靠索引名 `uk_name` 判重;帮名只做 TrimSpace + ≤24 rune + 无控制字符 | `guild_repo.go:596-608`;`logic/guild_logic.go:103-115`;`constants.go:74` |
| 17 | 旧索引名 / 旧 SQL 文件出现在注释中:`guild_repo.go:30,32,596,600-601`、`constants.go:44`、`guild_logic.go:60`、`guild_repo_zone_test.go:100`;测试 DDL `guild_repo_test.go:159,169`、`rank_zone_integration_test.go:116,126`;B1b 另有 `merge_zone/guild_step.go:7-8,12,82-83`、`integration_test.go:133,137`、`audit_resources.go:445` | rg 结果 |
| 18 | 时间戳:`GuildData.CreateTimeMs`、`MemberData.JoinTimeMs/LastActiveMs` 为 int64;`guild.proto` 的 `create_time_ms`/`join_time_ms` 为 int64(B1 不改 guild.proto) | `guild_repo.go:50,62-63`;`proto/guild/guild.proto:14,26` |
| 19 | proto-gen 虽注释称 guild 只出 Go,实际为 `proto/guild` 生成 C++;trade 先例把 `trade/trade_table.pb.cc/.h` 手工登记进三处 | `cpp/generated/proto/guild/guild.pb.cc`;`cpp/generated/proto/CMakeLists.txt:116-122`;`proto.vcxproj:122,233`;`.filters:294,611`;`PROGRESS.md:3227` |
| 20 | appuser 对 `zone_1_db`/`zone_2_db`/`testdb`/`mmorpg_trade` 有 ALL 权限 | `deploy/mysql-init/00_init_zone_dbs.sql:6,10,23,37` |
| 21 | merge_zone 帮会 SQL 不带库名;trade 用 `-trade-schema` 限定 | `guild_step.go:64,88,119,125`;`unmerge.go:141`;`audit_checks.go:235,238,283`;`audit_resources.go:506,515`;`trade_step.go:58-82` |
| 22 | data_consistency_check 在 `-mysql-dsn` 默认库上查 guild | `tools/data_consistency_check/main.go:72,230,237,263` |
| 23 | start_game 对 `mmorpg_trade` 做 appuser 预检 | `tools/scripts/start_game.ps1:403-430,472-474` |
| 24 | 未提交改动(`git status --short` 限定到本节涉及的路径):仅 `go/schemamigrate/go.mod`、`go.sum`,`go/trade/` 下若干文件,`cpp/generated/proto/guild/guild.grpc.pb.h`,以及 `cpp/generated/proto/trade/*.grpc.pb.*` 未跟踪 | 2026-09-16 执行结果;落码前必须重跑 |

## 2. 目标形态

- **库**:`mmorpg_guild`,本地与 K8s 同名(D-14 §1)。B1 后库内有 4 张业务表加 schemamigrate 台账 `schema_migrations`;`mmorpg` 里不再有帮会表(旧卷残留按第 7 部分 §19 清理)。
- **事实源**:`proto/guild/guild_db.proto`(package `guildpb`,与 `guild.proto` 同包)。repo 手写参数化 SQL,不写 DDL。
- **建表入口**:`guild -f etc/guild.yaml -migrate`,或启动期 `Schema.AutoMigrate`(dev 为 true)。
- **事务范围**:同事务的写都在同一个库内,不做跨库事务(D-14 §6)。

(§2.1 锁序规则见第 2 部分开头。)

## 3. 建库与授权:`deploy/mysql-init/00_init_zone_dbs.sql`

**只在 `mmorpg_trade` 块之后、`FLUSH PRIVILEGES;` 之前追加**,不改已有行:

```sql
-- mmorpg_guild:帮会 guild 服务独占库(go/guild/etc/guild.yaml MySQL.DataSource 的库名;
-- go/guild/internal/config.Validate 断言 DSN 库名等于它)。按 port-decisions D-14(§8 已修订:
-- 帮会表一并迁入),这里只建库 + 授权;表以 proto/guild/guild_db.proto 为源,由 go/schemamigrate
-- 建(guild 启动期 Schema.AutoMigrate,或 `guild -f etc/guild.yaml -migrate`),不要往 mysql-init 加帮会表。
-- appuser 没有全局 CREATE 权限,库必须在 guild 启动前就存在。
-- 已初始化过的本地数据卷不会重跑 initdb:用 root 手工执行下面两句(再 FLUSH PRIVILEGES),或重建数据卷;
-- tools/scripts/start_game.ps1 在 MySQL 就绪后预检本库,不就绪时跳过 guild 并打印补建命令。
-- K8s:mysql-init-sql ConfigMap 原样带入本文件,新 PVC 首次 initdb 建库;已有 PVC 手工补建(见 deploy/k8s/README.md)。
CREATE DATABASE IF NOT EXISTS mmorpg_guild DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
GRANT ALL PRIVILEGES ON `mmorpg_guild`.* TO 'appuser'@'%';
```

帮名唯一性**不依赖**库或表的排序规则,由 Go 侧 `name_norm` 保证(第 2 部分 §6.3)。

## 4. `deploy/mysql-init/guild_friend_tables.sql` 删改

1. 第 1-2 行文件头改为:
   ```sql
   -- Friend service tables(留在 mmorpg 库)。
   -- 帮会表已迁入独占库 mmorpg_guild,由 proto/guild/guild_db.proto + go/schemamigrate 建(port-decisions D-14 §8 修订)。
   ```
2. 删除第 4-40 行:`── Guild ──` 小节、`guild`、`guild_member` 及其 role 注释、`guild_schema_migration`。
3. 删除第 69-210 行:`── Idempotent expand/backfill migration ──` 整节,含 `migrate_guild_friend_schema` 过程与 `CALL` / `DROP PROCEDURE`。friend 侧替代方案见第 4 部分 §9。
4. `friend`、`friend_request`、`friend_capacity` 三张表原样保留。

删除后文件只剩三条 `CREATE TABLE IF NOT EXISTS`。旧数据卷里残留的 `mmorpg.guild*` 三张表不会自动删除,见第 7 部分 §19。

<!-- s1_storage_part2.md -->

# S1 存储(B1)— 第 2 部分:锁序规则、`guild_db.proto` 与表形状

## 2.1 全库加锁规则(B1 定义,B2–B6 必须遵守)

**规则 1:表间顺序。** 同一事务要锁多张表的行时,按下表从上到下加锁。`INSERT` 新行、`UPDATE`/`DELETE` 已有行都算对该行加锁,按所在表的位置计算。表名固定;B1 未建的表由对应批次建表时照此位置执行。

| 位置 | 表 | 建表批次 |
|---|---|---|
| 1 | `guild` | B1 |
| 2 | `guild_player_state`(每玩家串行化锁行) | B1 |
| 3 | `guild_member` | B1 |
| 4 | `guild_application` | B1 |
| 5 | `guild_player_op_seq` | B5 |
| 6 | `guild_asset_op` | B5 |
| 7 | `guild_daily_counter` | B5 |
| 8 | `guild_activity_progress` | B6a |
| 9 | `guild_trial_battle` | B6b |

`guild_player_state` 放在 `guild_member` 之前,因为审批的自然顺序是"先锁申请人状态行,再给申请人插成员行"。放在后面会让审批逆序。

**规则 2:同表多行。** 一律按主键升序加锁,不区分操作者和目标。例如 `guild_member` 主键是 `(guild_id, player_id)`,同帮两行就按 player_id 升序。

**规则 3:不要对可能不存在的行做加锁读,再据此插入。** MySQL RR 下,这种读会拿间隙锁,两边并发 INSERT 时互等,形成死锁。TiDB 没有间隙锁:对唯一键点查不存在的行会锁住这个键,但范围加锁读锁不住后续插入。所以:
- 唯一性(一人一帮、帮名唯一、申请主键)只靠唯一键 / 主键的 1062 判定:`uk_guild_member` → `ErrPlayerAlreadyInGuild`,`uk_guild` → `ErrGuildNameTaken`。
- 需要"每玩家串行化"时(每人待审上限、同一玩家的并发申请),先在**事务外**执行一条自动提交的 `INSERT IGNORE INTO guild_player_state (player_id, updated_ms) VALUES (?, ?)`,确保行存在;事务内再按位置 2 执行 `SELECT player_id FROM guild_player_state WHERE player_id = ? FOR UPDATE`。
- 允许对不存在的行做加锁读,但只能用于"查不到就返回错误"(例如审批时申请已不存在),不能接着 INSERT。

**规则 4:判定用加锁读。** 作为业务判定依据的计数或存在性读(例如"本人未过期申请数 < max_pending_applications_per_player"),必须是加锁读(`… FOR UPDATE`),并放在它所依赖的串行化锁之后。MySQL RR 和 TiDB 的普通 SELECT 读的是事务快照,可能看不到刚提交的并发插入。普通快照读只用于展示。

**规则 5:死锁重试。** repo 层对 MySQL 错误 1213 整个事务重试 1 次(回滚后从 BEGIN 重来,不复用任何读结果)。再失败就包装原错误返回,logic 按内部错误处理(与现有 `fmt.Errorf` 路径相同,不新增 tip)。助手 `withTxRetry` 由 B2 在 `guild_repo.go` 落地(B2 是第一个出现多行写事务的批次);B1 不改现有事务。

**规则 6:outbox / 重投 worker 的写法**(B5/B6 用):
1. 事务外、不加锁,读出候选 op(`status=PENDING AND next_attempt_ms<=now`,有界)。
2. 对每个 op:BEGIN → 若要改帮会或成员,按位置 1、3 锁 `guild` / `guild_member` → `SELECT … FROM guild_asset_op WHERE op_id = ? FOR UPDATE` → **重新检查 status 和租约**,不符就回滚并跳过 → 写结果 → COMMIT。
3. 禁止先锁 `guild_asset_op` 再去锁 `guild` / `guild_member`。

现有事务(`guild_repo.go:323,386,543`)都是先锁已存在的 `guild` 行、再锁 `guild_member`,符合上述规则,B1 不改。

## 5. `proto/guild/guild_db.proto`(新建,全文)

```proto
syntax = "proto3";
package guildpb;

option go_package = "guild/proto/guild";

import "proto/db/proto_option.proto";

// 帮会独占库 mmorpg_guild 的表结构,唯一事实源(port-decisions D-14,§8 已修订)。
// - 建表 / 加列只经 go/schemamigrate(go/guild 的 -migrate 与启动期 Schema.AutoMigrate);业务 SQL 手写。
// - D-14 §2:主键只用整数 / 枚举列;string 列建成 MEDIUMTEXT,进索引只按 191 前缀;每表至多一个唯一键。
// - 改表纪律见 guild-phase2 §6.4:新表 / 新字段只追加;OptionIndex 组只能追加到末尾,不得调序;
//   已有唯一键或索引组的列一旦改动,开发期必须删库重建。
// - 与 guild.proto 同包:message 一律带 Record 后缀,枚举值一律带枚举名前缀。
// - 本文件不进客户端 tools/gen_proto.ps1 清单,不定义 service。
// - 加锁顺序:guild → guild_player_state → guild_member → guild_application → guild_player_op_seq
//   → guild_asset_op → guild_daily_counter → guild_activity_progress → guild_trial_battle(guild-phase2 §2.1)。
// - 后续表由 B5 / B6a / B6b 在本文件追加 message,并在 go/guild/internal/data/tables.go 追加。

message GuildRecord {
  option(OptionTableName) = "guild";
  option(OptionPrimaryKey) = "guild_id";
  option(OptionUniqueKey) = "name_norm";      // 生成 uk_guild;帮名全服唯一,判重只看规范化值
  option(OptionIndex) = "zone_id;leader_id";  // idx_guild_0 = 合服 / 分区榜重建;idx_guild_1 = 按会长查
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 guild_id = 1;        // data_service 号段 biz_tag=guild
  string name = 2;            // 展示名:TrimSpace 后原样保存,不建索引;≤ MaxGuildNameRunes=24
  uint64 leader_id = 3;       // 与 guild_member.role=3 同事务维护
  uint32 level = 4;           // 建帮写 1;B5 升级改写
  string announcement = 5;    // ≤ MaxAnnouncementBytes=600
  uint64 create_time_ms = 6;
  uint32 max_members = 7;     // 建帮写入;B5 起按 GuildLevel.max_members 改写
  uint32 zone_id = 8;         // 帮会归属 zone;merge_zone 步骤 3 改写
  int64 score = 9;            // 排行分权威副本;Redis ZSET 由 RebuildRanks 重建
  uint64 funds = 10;          // 帮会资金;B1 恒为 0,B5 起写入
  string name_norm = 11;      // data.GuildNameNorm(name):NFKC → TrimSpace → 小写;≤ 48 rune(远小于 191 前缀)
  // 追加区:从 12 起。
}

message GuildPlayerStateRecord {
  option(OptionTableName) = "guild_player_state";
  option(OptionPrimaryKey) = "player_id";
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 player_id = 1;       // 每玩家一行,只作串行化锁行(§2.1 规则 3);INSERT IGNORE 于事务外建行
  uint64 updated_ms = 2;      // 最近一次加锁写入的时刻,仅诊断用
  // 追加区:从 3 起。刻意不存"待审申请数":帮会侧批量删过期申请、解散删申请都不锁申请人行,计数会漂;
  // 上限判定一律在持有本行锁后对 guild_application 做加锁计数(§2.1 规则 4)。
}

message GuildMemberRecord {
  option(OptionTableName) = "guild_member";
  option(OptionPrimaryKey) = "guild_id,player_id";
  option(OptionUniqueKey) = "player_id";      // 生成 uk_guild_member;一人至多一个帮
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 guild_id = 1;
  uint64 player_id = 2;
  uint32 role = 3;                   // 0 成员 / 1 长老 / 3 帮主(D-4;权限比较走 constants.Rank)
  uint64 join_time_ms = 4;
  uint64 last_active_ms = 5;         // 目前只在入帮时写入
  uint64 contribution_total = 6;     // 累计帮贡,只增;B5 起写入
  uint64 contribution_balance = 7;   // 可消费帮贡;商店扣减;B5 起写入
  // 追加区:从 8 起。
}

message GuildApplicationRecord {
  option(OptionTableName) = "guild_application";
  option(OptionPrimaryKey) = "guild_id,player_id";
  option(OptionIndex) = "player_id;expire_ms";  // idx_guild_application_0 = 本人申请;idx_guild_application_1 = 过期清理
  option(OptionTiDBNonclusteredPK) = true;
  option(OptionTiDBShardRowIDBits) = 4;
  option(OptionTiDBPreSplitRegions) = 4;

  uint64 guild_id = 1;
  uint64 player_id = 2;        // 申请人
  uint64 apply_ms = 3;         // 最近一次申请(重复申请会刷新)的时刻
  uint64 expire_ms = 4;        // apply_ms + GuildRule.application_expire_hours × 3600000
  // 追加区:从 5 起(B2)。只存待审;通过、拒绝、撤销、过期都删行。
}
```

**选项格式来源**:`OptionIndex` 用 `;` 分组、`,` 分列,复合 `OptionPrimaryKey` 的写法,均照 `proto/trade/trade_table.proto:29-35,60-65`。`OptionUniqueKey` 的选项号为 500012(`proto/db/proto_option.proto:89`)。索引序号取 `OptionIndex` 分组的切片下标,从 0 开始(见第 1 部分 §1 第 2 行)。go/guild 是首个使用 `OptionUniqueKey` 的服务,因此 §18 第 5 步要核对 `UNIQUE KEY uk_guild (name_norm(191))`。

**proto-gen**:`proto_gen.yaml:410-418` 以整个 `proto/guild/` 目录为源,新文件自动编译,Go 产物是 `go/proto/guild/guild_db.pb.go`,C++ 产物是 `cpp/generated/proto/guild/guild_db.pb.cc/.h`(可能还有空的 `guild_db.grpc.pb.*`)。C++ 按 trade 先例只登记 `.pb.cc/.pb.h`(第 6 部分 §17 第 21–23 项)。客户端 `mmorpg-client/tools/gen_proto.ps1:38` 按文件逐个列出 proto,不扫目录,因此客户端无需改动。本文件不含 service,不分配 message id。

## 6. 表形状核对

### 6.1 D-14 §2 约束

| 表 | 主键(整数) | UNIQUE | 进索引的 string |
|---|---|---|---|
| guild | guild_id | 1(name_norm) | name_norm,191 前缀(值 ≤48 rune,前缀覆盖全值) |
| guild_player_state | player_id | 0 | — |
| guild_member | guild_id, player_id | 1(player_id) | — |
| guild_application | guild_id, player_id | 0 | — |

四张表都满足约束,不需要书面例外。

### 6.2 与旧手写 DDL 的差异

| 列 / 键 | 旧 | 新 | 处理 |
|---|---|---|---|
| guild.name | `VARCHAR(64) NOT NULL` + `uk_name` | `MEDIUMTEXT` 可空,无索引 | INSERT 恒带值;SELECT 仍直接读取 |
| guild.name_norm | 无 | `MEDIUMTEXT` 可空 + `uk_guild(name_norm(191))` | repo 在 CreateGuild 内计算写入(第 4 部分 §8) |
| guild.announcement | `TEXT` | `MEDIUMTEXT` 可空 | SELECT 用 `COALESCE(announcement,'')` |
| guild.level / max_members | 默认值 1 / 50 | `int unsigned DEFAULT 0` | CreateGuild 始终显式写值 |
| guild_member.contribution | `BIGINT UNSIGNED` | 删除,拆为 contribution_total / contribution_balance | §8 |
| guild.funds | 无 | `bigint unsigned NOT NULL DEFAULT 0` | 新增 |
| 键名 | `uk_name` / `uk_player` / `idx_leader` / `idx_zone` | `uk_guild` / `uk_guild_member` / `idx_guild_1` / `idx_guild_0` | `guildNameUniqueKey` 改为 `"uk_guild"`;业务 SQL 不引用普通索引名 |

### 6.3 帮名唯一性:Go 侧规范化

- `data.GuildNameNorm(display string) (string, bool)`:`strings.ToLower(strings.TrimSpace(norm.NFKC.String(display)))`,用 `golang.org/x/text/unicode/norm`(guild go.mod 已有 v0.32.0 indirect,tidy 后变为直接依赖)。结果为空或超过 `MaxGuildNameNormRunes = 48` 时返回 false。48 的来由:展示名上限 24 rune,NFKC 在正常文字上不膨胀;少数兼容字符会展开(如 `㍿`),给一倍余量,并保证不超过 191 前缀。
- 与契约 §3.4 player `name_norm` 使用同一公式(NFKC + 小写)。当前不共用代码,原因:B1 不依赖 B3a 的 `go/shared/playername`,而且帮名规则(24 rune、允许符号)与角色名不同。B3a 合入后可以把公式抽成 `playername.Key`,两边共用(§21 偏差 3)。
- 唯一性以 `name_norm` 为准。"青云门"与"青云门 "、"ABC"与"abc"、"ＡＢＣ"与"abc"都判为重名,不依赖表的排序规则。
- 现行 `utf8mb4_unicode_ci` 会带来**额外**的误判重名(`é` 与 `e`、所有 U+10000 以上字符之间),只会误拒,不会漏判。proto2mysql 升级到键列 `VARCHAR … utf8mb4_0900_bin`(7dbda68)后,这些误判消失。但升级后 schemamigrate 会对现有 MEDIUMTEXT 键列报 `ErrLegacyKeyColumn`,guild 拒绝启动;未上线期间按 §6.4 删库重建,并给 `name_norm` 加 `max_length`。

### 6.4 开发期改表纪律(按 schemamigrate 实际行为)

schemamigrate 能做到和做不到的:
- **能自动做**:建库里缺的表;给已有表 `ADD COLUMN`。
- **报需人工(ExitCode=4,guild 拒绝启动)**:列类型漂移、缺主键或主键列不符、**缺唯一键(按名字判断)**、台账记为已执行而库里又缺失的表或列。
- **只报 Warning(Clean() 忽略,Plan 模式也放行)**:proto 删掉的字段(多余列)、多余表、**缺普通索引**。
- **完全检测不到**:唯一键或索引组改了列但名字不变(`uk_<表>`、`idx_<表>_<n>` 与列无关);在已有索引组前面插入新组导致序号后移(只表现为"缺索引");枚举数值重排(列类型都是 int)。

硬规则:
1. 只追加表和字段;删字段写 `reserved`,开发期如需删列就删库重建。
2. `OptionIndex` 组只能追加到末尾,不得调序,不得改已有组的列;`OptionUniqueKey` 与主键定下后不得改列。确需修改时,开发期删库重建,上线后另建新表。
3. `ensureSchema` 在 Up 和 Plan 两种模式下,只要 `report.Warnings` 中有以 `缺索引` 开头的行,就拒绝启动(第 3 部分 §7.4),把"缺普通索引"从 Warning 升级为阻断。
4. 枚举一旦有行写入,数值不得重排。
5. 删库重建步骤:停 guild → root 执行 `DROP DATABASE mmorpg_guild;` → 重新执行 §3 的两条语句 → 启动 guild 自动建表。触发了规则 2 或 4 的批次,必须把这个步骤写进交付说明。
6. 集成测试 `TestSchemaMigrateProducesExpectedGuildShape`(第 6 部分 §16)按 `INDEX_NAME` 逐个核对 `COLUMN_NAME / SEQ_IN_INDEX / SUB_PART`,防止同名换列的漂移混过去。

<!-- s1_storage_part3.md -->

# S1 存储(B1)— 第 3 部分:go/guild 接入 schemamigrate 与超时预算

## 7. go/guild 接入

### 7.1 `go/guild/go.mod`

- 第 3 行 `go 1.24.5` 改为 `go 1.26.5`,上方加注释:`// go 1.26.5:schemamigrate(proto2mysql)要求 1.26.5(D-14 理由 3)`。
- 直接依赖块加 `schemamigrate v0.0.0`;文件末尾加 `replace schemamigrate => ../schemamigrate`。
- **proto2mysql 解析方式必须与已提交的 `go/schemamigrate/go.mod` 逐字一致**。replace 只在主模块生效,go/guild 继承不到 schemamigrate 的 replace:
  - schemamigrate 若仍是 `replace github.com/luyuancpp/proto2mysql v0.1.1 => github.com/luyuan-cpp/proto2mysql v0.1.1`,go/guild 末尾加同一行,上方注释:`// 与 go/schemamigrate/go.mod 的 proto2mysql replace 逐字一致:replace 只在主模块生效(guild-phase2 §7.1)。schemamigrate 改 require 正式 tag 后同步删除。`
  - schemamigrate 若已改为 require 含 191 前缀和 TiDB 选项的正式 tag(≥ v0.1.2)并删掉了 replace,go/guild 就不写 replace,也不直接 require proto2mysql。
  - 两种情况都必须满足:`go/guild/go.sum` 里 proto2mysql 的 `h1:` 行与 `go/schemamigrate/go.sum` 相同。§18 第 0、2 步负责核对。
- `github.com/go-sql-driver/mysql` 升到 v1.9.3(schemamigrate 的要求);`golang.org/x/text` 从 indirect 变为直接依赖(§6.3)。go-zero 保持 v1.9.2。`go.sum` 与 indirect 块由 Codex 执行 `go mod tidy` 生成。

### 7.2 `go/guild/internal/data/tables.go`(新建)

```go
// Package data 的库名与表清单:mmorpg_guild 独占库(port-decisions D-14,§8 已修订)。
// 表结构唯一事实源是 proto/guild/guild_db.proto;建表 / 加列只经 go/schemamigrate,本包不写 DDL。
package data

import (
	pb "proto/guild"

	"google.golang.org/protobuf/proto"
)

// DatabaseName 是帮会独占的逻辑库名,本地与 K8s 同名。config.Validate 断言 DSN 库名等于它,
// schemamigrate 连接后再断言 SELECT DATABASE() 等于它;tools/merge_zone 的 defaultGuildSchema 镜像本常量。
const DatabaseName = "mmorpg_guild"

// Tables 返回本库全部表,顺序即 guild-phase2 §2.1 的加锁位置。新增表 = guild_db.proto 加 message 并在
// 这里追加(schemamigrate 会补建后加的表),同时追加 guild_repo_test.go 的 guildTestDropTables。
func Tables() []proto.Message {
	return []proto.Message{
		&pb.GuildRecord{},
		&pb.GuildPlayerStateRecord{},
		&pb.GuildMemberRecord{},
		&pb.GuildApplicationRecord{},
	}
}
```

### 7.3 配置:`internal/config/config.go` 与 `etc/guild.yaml`

config.go 新增 import:`errors`(若尚未引入)、`fmt`、`mysqldriver "github.com/go-sql-driver/mysql"`、`"guild/internal/data"`(data 只 import constants 和 proto,不形成循环)。新增内容:

```go
// MaxRpcTimeoutMs:帮会二期契约 §2,Timeout ≤ 路由服 ForwardTimeoutMs(5000)− 1000。
const MaxRpcTimeoutMs int64 = 4000

// InBandReplyReserve:整请求业务预算相对 Timeout 预留的回包余量(同 go/trade)。
const InBandReplyReserve = 500 * time.Millisecond

// HomeZoneLookupBudgetMs 镜像 logic.DefaultHomeZoneLookupTimeout(config 不能 import logic);
// guild_test.go 的 TestHomeZoneBudgetMirrorsLogic 守住两者相等。
const HomeZoneLookupBudgetMs int64 = 1500

// DataServiceRpc.Timeout 的合法区间:下限防止写 0(go-zero 把 0 当成不设客户端超时),
// 上限为契约 §2 规定的同步跨服务调用 ≤ 3000ms。
const (
	MinDataServiceRpcTimeoutMs int64 = 500
	MaxDataServiceRpcTimeoutMs int64 = 3000
)

// SchemaConf 建表策略(D-14 §4)。AutoMigrate 为 nil(没写)或 true:启动时跑 schemamigrate.Up;
// false:只跑只读的 Plan,不干净就拒绝启动。用 *bool 的理由同 go/trade/internal/config.SchemaConf。
type SchemaConf struct {
	AutoMigrate *bool `json:",optional"`
}
```

`Config` 加字段 `Schema SchemaConf `json:",optional"``,并加三个方法:

```go
// ShouldAutoMigrate 是启动路径跑 Up 还是只跑 Plan 的唯一判据:没写 = Up。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate == nil || *c.Schema.AutoMigrate
}

// RequestBudget 是一次请求内全部 I/O(归属区查询、发号、Redis、MySQL)共用的业务预算,
// 由 guild.go 的 requestBudgetInterceptor 套到每个 handler 的 ctx 上。Validate 保证它为正,
// 且放得下一次 data_service 调用加一次归属区查询。
func (c Config) RequestBudget() time.Duration {
	return time.Duration(c.Timeout)*time.Millisecond - InBandReplyReserve
}

// Validate 由 go-zero conf.Load / MustLoad 自动调用(v1.9.2 core/conf/config.go:69,97)。
// 本方法会遮蔽嵌入的 zrpc.RpcServerConf.Validate,所以第一步必须显式调用它(Auth=true 时校验 Redis)。
// 错误文案不得包含 DataSource 原文(含口令)。
func (c *Config) Validate() error {
	if err := c.RpcServerConf.Validate(); err != nil {
		return err
	}
	if c.Timeout <= 0 || c.Timeout > MaxRpcTimeoutMs {
		return fmt.Errorf("Timeout(%d ms)必须在 (0, %d] 内:上限 = 路由服 ForwardTimeoutMs(5000)− 1000",
			c.Timeout, MaxRpcTimeoutMs)
	}
	ds := c.DataServiceRpc.Timeout
	if ds < MinDataServiceRpcTimeoutMs || ds > MaxDataServiceRpcTimeoutMs {
		return fmt.Errorf("DataServiceRpc.Timeout(%d ms)必须在 [%d, %d] 内:0 = 不设客户端超时,上限见帮会二期契约 §2",
			ds, MinDataServiceRpcTimeoutMs, MaxDataServiceRpcTimeoutMs)
	}
	reserveMs := InBandReplyReserve.Milliseconds()
	if ds+HomeZoneLookupBudgetMs+reserveMs > c.Timeout {
		return fmt.Errorf("超时预算不足:DataServiceRpc.Timeout(%d)+ 归属区查询(%d)+ 回包余量(%d)= %d ms > Timeout(%d ms);"+
			"建帮最坏要串行做归属区查询和同步领号段,超出会变成 DeadlineExceeded 而不是业务 tip",
			ds, HomeZoneLookupBudgetMs, reserveMs, ds+HomeZoneLookupBudgetMs+reserveMs, c.Timeout)
	}
	dsn, err := mysqldriver.ParseDSN(c.MySQL.DataSource)
	if err != nil {
		return errors.New("MySQL.DataSource 无法解析(原文含口令,不打印)")
	}
	if dsn.DBName != data.DatabaseName {
		return fmt.Errorf("MySQL.DataSource 的库名必须是 %q(得到 %q):帮会表只建在独占库(D-14)",
			data.DatabaseName, dsn.DBName)
	}
	return nil
}
```

说明:`ParseDSN` 的错误里可能带 DSN 片段,因此不 `%w` 包装原错误。

**预算核算**(建帮最坏情况):归属区查询 ≤1500 → 号段耗尽时同步领段 ≤2000(zrpc 客户端超时拦截器以 `DataServiceRpc.Timeout` 封顶单次调用;idsegment 自带的 `FetchTimeout` 默认 3s 在后台 ctx 上,更长但会被客户端 2000 先截断)→ Redis 查已入帮 + MySQL 建帮事务,共用剩余预算。整请求预算 3500ms 用完时,`idsegment.Next(ctx)` 或 SQL 在 ctx 到期后返回,logic 按现有映射回 `kGuildIdGenUnavailable` 或内部错误,还剩 500ms 回包,不会出现服务端 DeadlineExceeded。

`guild.yaml` 只改以下几处,其余保留:

```yaml
# zrpc 服务端整体超时(毫秒):≤ 路由服 ForwardTimeoutMs(5000)− 1000 = 4000(帮会二期契约 §2)。
# 另须 ≥ DataServiceRpc.Timeout + 1500(归属区查询)+ 500(回包余量);config.Validate 强制。
Timeout: 4000
```

```yaml
# ---- 独占库 mmorpg_guild(D-14 §1/§5,§8 已修订)----
# 库由 deploy/mysql-init/00_init_zone_dbs.sql 预建(存量卷需 root 手工执行那两行);表由 schemamigrate 按
# proto/guild/guild_db.proto 建。DSN 库名必须是 mmorpg_guild(config.Validate 断言)。
# 账号用 appuser(与 go/trade/etc/trade.yaml 同账号,只对独占库有 ALL 权限)。
MySQL:
  DataSource: "appuser:apppass123@tcp(127.0.0.1:3306)/mmorpg_guild?charset=utf8mb4&parseTime=true&loc=Local"

# 建表策略(D-14 §4):不写或 true = 启动时 schemamigrate.Up(GET_LOCK 串行,锁忙重试 3 次);
# false = 只跑只读 Plan,不干净就拒绝启动,并提示 `guild -f etc/guild.yaml -migrate`。K8s 非 dev 档固定 false。
Schema:
  AutoMigrate: true
```

`DataServiceRpc` 段(第 68 行)`Timeout: 3000` 改为:

```yaml
  # 单次 data_service 调用上限(归属区查询 / 领号段)。与 Timeout 的预算关系见上;Validate 强制 [500, 3000]。
  Timeout: 2000
```

`go_services.ps1` 的 `-Zone` 改写不涉及 `DataSource`,zone 2 多开的 guild 连同一个库。

### 7.4 `go/guild/guild.go`

新增 import:`database/sql`、`errors`、`io`、`strings`、`time`(已有则跳过)、`"schemamigrate"`。新增 flag 与常量:

```go
// migrateOnly:`guild -f <yaml> -migrate` 只对 mmorpg_guild 跑一次 schemamigrate.Up 后退出
// (退出码 0 成功 / 1 错误 / 3 锁忙 / 4 需人工),不连 Redis / etcd / data_service,供将来的 guild-migrate Job 使用。
var migrateOnly = flag.Bool("migrate", false, "run schemamigrate.Up on mmorpg_guild and exit (0 ok / 1 error / 3 lock busy / 4 manual)")

const migrateRemedyCommand = "guild -f etc/guild.yaml -migrate"

// 启动期等迁移锁:本地 go_services 允许 guild 多开,首次建表时实例间会互等 GET_LOCK。
// 没有编排器替我们重拉,所以在进程内重试。测试里把 lockBusyRetryDelay 置 0。
const lockBusyAttempts = 3

var lockBusyRetryDelay = 2 * time.Second
```

**启动顺序(新)**:
1. `flag.Parse()` → `conf.MustLoad(*configFile, &config.AppConfig)`(Validate 自动执行,不合法直接 Fatal)。
2. `if *migrateOnly { os.Exit(runMigration(config.AppConfig)) }`。
3. `svcCtx := svc.NewServiceContext(...)`(不变)。
4. **新增** `if err := ensureSchema(context.Background(), svcCtx.DB, config.AppConfig, schemamigrate.Up, schemamigrate.Plan); err != nil { logx.Must(err) }`,放在 `node.NewNode` 之前:表结构不对的实例不能注册进 etcd。
5. 节点注册、snowflake、killswitch(不变)。
6. `repo := data.NewGuildRepo(...)`。**删除** `MigrateLegacyRankScores` 调用和它上方的注释(第 137-142 行);保留 `RebuildRanks`,注释改为 `// 每次启动从 MySQL 权威快照重建全局 / 分区榜;失败拒绝启动,避免对外提供不完整榜单。`
7. 第 180 行改为 `s.AddUnaryInterceptors(buildUnaryInterceptors(ks, config.AppConfig.RequestBudget())...)`。

**`buildUnaryInterceptors(ks *killswitch.Switch, budget time.Duration)`**:在切片末尾(serverbase 之后,也就是最内层)追加 ⑤ `requestBudgetInterceptor(budget)`,注释为"整请求业务预算(Timeout − 500ms),让 I/O 在服务端超时前失败并回 in-band tip"。

```go
// requestBudgetInterceptor 给 handler 的 ctx 套上整请求业务预算(与 go/trade RequestBudget 同义)。
func requestBudgetInterceptor(budget time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		return handler(ctx, req)
	}
}
```

**新增函数**(形状与 `go/trade/trade.go:322-427` 相同,名字与文案换成 guild):
- `type schemaRunner func(ctx context.Context, db *sql.DB, opts schemamigrate.Options) (schemamigrate.Report, error)`
- `schemaOptions()`:返回 `{Database: data.DatabaseName, Tables: data.Tables(), Logf: logx.Infof}`。
- `runWithLockRetry(ctx, runner, db, opts) (schemamigrate.Report, error)`:最多 `lockBusyAttempts` 次。`errors.Is(err, schemamigrate.ErrLockBusy)` 且未到最后一次时,打 `logx.Infof("[guild] 迁移锁忙,%v 后重试(%d/%d)", …)`,再 `select { case <-time.After(lockBusyRetryDelay): case <-ctx.Done(): return report, ctx.Err() }`;其余情况直接返回。
- `ensureSchema(ctx, db, c config.Config, up, plan schemaRunner) error`:
  - `ShouldAutoMigrate` 为真:`report, err := runWithLockRetry(ctx, up, …)`,然后 `logReport("schemamigrate.Up", report)`。`err != nil` 时返回 `[guild] 启动期建表失败(库 mmorpg_guild),拒绝启动: %w`;`len(Manual)>0` 时返回"发现 N 项需人工处理"。
  - 否则:`runWithLockRetry(ctx, plan, …)`。`err != nil` 或 `!report.Clean()` 时返回错误,附上 `migrateRemedyCommand`。
  - **两种模式都要做**:`missing := missingIndexWarnings(report)`,非空时返回 `[guild] 库 mmorpg_guild 缺 %d 个 proto 声明的普通索引,拒绝启动(schemamigrate 不自动补建;开发期按 guild-phase2 §6.4 删库重建): %s`。
- `missingIndexWarnings(r schemamigrate.Report) []string`:返回 `strings.HasPrefix(w, "缺索引")` 的行。前缀取自 `go/schemamigrate/plan.go:346` 的文案;`TestMissingIndexWarningPrefixMatchesSchemamigrate` 负责守住(第 6 部分 §16)。
- `runMigration(c config.Config) int`:`sql.Open("mysql", c.MySQL.DataSource)` → 10s 超时 `PingContext`。失败返回 `schemamigrate.ExitCode(schemamigrate.Report{}, err)`(即 1)。成功则执行 `schemamigrate.Up(context.Background(), db, schemaOptions())` → `printReport` → 返回 `ExitCode`。`-migrate` 模式**不做**锁忙重试(退出码 3 交给 Job 的 backoff),但缺索引时同样返回 4 并打印 MANUAL 行。只打印库名,不打印 DSN。
  - stdout / stderr 固定文案:开头 `guild schema migration: database=mmorpg_guild`;连接失败 `schema migration FAILED (exit 1): <err>`;Up 出错 `schema migration FAILED (exit <code>): <err>`;需人工 `schema migration needs manual action (exit 4): <n> item(s) listed above`;成功 `schema migration OK: mmorpg_guild synced from proto/guild/guild_db.proto (<n> statement(s) executed)`。
- `reportLines` / `printReport(w io.Writer, r)` / `logReport(stage, r)`:逐字照搬 trade,前缀改为 `[guild]`。

不抽公共包:D-14 规定每个建表服务自带入口,trade 的这几个函数在 `package main`,无法跨 module 复用。等第三个建表服务出现,再考虑放进 schemamigrate。

**连接池**:`svc.NewServiceContext` 没有设置 `SetMaxOpenConns`(默认 0 = 不限),满足 schemamigrate 的"≠1"要求。以后如果加上限,必须 ≥2。

### 7.5 删除遗留迁移门

`guild_repo.go` 删除:`ErrLegacyRankSnapshotMismatch`(第 38-40 行)、`guildScoreMigrationKey`(第 110 行)、`MigrateLegacyRankScores`(第 677-792 行)、`diffGuildIDSets` 和 `summarizeGuildIDs`(第 794-823 行),以及只被它们使用的 import `math`、`slices`。`guild_repo_test.go` 删除三个 `TestDiffGuildIDSets_*`(第 20-42 行)和 `slices` import。

`RebuildRanks` 不改:它只执行 `SELECT guild_id, zone_id, score FROM guild`。切到新的空库时,它会清空 Redis 里的旧榜。ensureSchema 在它之前执行,表不存在时进程已经拒绝启动。

<!-- s1_storage_part4.md -->

# S1 存储(B1)— 第 4 部分:repo 改写与 friend 门表

## 8. repo 与 logic 改写

### 8.1 `internal/data/guild_repo.go`

**结构体**(时间戳按 AGENTS / 契约 §2 改为 `uint64`,JSON tag 不变):

```go
type GuildData struct {
	GuildID      uint64 `json:"guild_id"`
	Name         string `json:"name"`
	LeaderID     uint64 `json:"leader_id"`
	Level        uint32 `json:"level"`
	Announcement string `json:"announcement"`
	CreateTimeMs uint64 `json:"create_time_ms"`
	MaxMembers   uint32 `json:"max_members"`
	ZoneID       uint32 `json:"zone_id"`
	// Score 注释不变
	Score   int64        `json:"score"`
	Funds   uint64       `json:"funds"` // 帮会资金;B1 恒 0,B5 起写
	Members []MemberData `json:"members"`
}

type MemberData struct {
	PlayerID            uint64 `json:"player_id"`
	Role                uint32 `json:"role"`
	JoinTimeMs          uint64 `json:"join_time_ms"`
	LastActiveMs        uint64 `json:"last_active_ms"`
	ContributionTotal   uint64 `json:"contribution_total"`
	ContributionBalance uint64 `json:"contribution_balance"`
	Online              bool   `json:"-"` // 仅内存,不入库
}
```

**帮名规范化**(放在 `guildNameUniqueKey` 旁边):

```go
// MaxGuildNameNormRunes 是 name_norm 的上限:展示名 ≤ constants.MaxGuildNameRunes(24),
// NFKC 对少数兼容字符会展开,留一倍余量;同时远小于 proto2mysql 的 191 索引前缀,保证唯一键覆盖全值。
const MaxGuildNameNormRunes = 48

// GuildNameNorm 返回帮名唯一键 name_norm:NFKC → TrimSpace → 小写(与契约 §3.4 player name_norm 同一公式)。
// 唯一性只看它,不依赖表排序规则(guild-phase2 §6.3)。空或超过 MaxGuildNameNormRunes 返回 false。
func GuildNameNorm(display string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(norm.NFKC.String(display)))
	if n == "" || utf8.RuneCountInString(n) > MaxGuildNameNormRunes {
		return "", false
	}
	return n, true
}
```

新增 import:`unicode/utf8`、`golang.org/x/text/unicode/norm`。

**逐条 SQL**(占位符顺序与现有调用一致):

| 位置 | 新语句 / 改动 |
|---|---|
| CreateGuild 开头(第 263 行 `leader :=` 之后) | `nameNorm, ok := GuildNameNorm(guild.Name)`;`!ok` 时返回 `fmt.Errorf("create guild %d: name fails normalization", guild.GuildID)`(logic 已先校验过,这里是防御) |
| CreateGuild 插帮会(第 272 行) | `INSERT INTO guild (guild_id, name, name_norm, leader_id, level, announcement, create_time_ms, max_members, zone_id, score, funds) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,参数在 `guild.Name` 后插入 `nameNorm` |
| CreateGuild 插会长(第 282 行) | `INSERT INTO guild_member (guild_id, player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance) VALUES (?, ?, ?, ?, ?, 0, 0)` |
| AddMemberInZone(第 344 行) | 同上一条 |
| loadGuildFromMySQL 帮会(第 477 行) | `SELECT guild_id, name, leader_id, level, COALESCE(announcement, ''), create_time_ms, max_members, zone_id, score, funds FROM guild WHERE guild_id = ?`,Scan 末尾加 `&guild.Funds` |
| loadGuildFromMySQL 成员(第 492 行) | `SELECT player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance FROM guild_member WHERE guild_id = ? ORDER BY player_id`,Scan 改为 `&m.ContributionTotal, &m.ContributionBalance` |

加 `ORDER BY player_id` 的原因:成员顺序原本依赖 InnoDB 返回顺序,AGENTS §11.6 要求集合顺序显式指定。

**注释与常量**(清除旧索引名,§18 静态检查依赖这一步):
- 第 30 行:`// ErrPlayerAlreadyInGuild:唯一索引 uk_guild_member(player_id)拒绝了跨公会重复 membership。`
- 第 32 行:`// ErrGuildNameTaken:唯一索引 uk_guild(name_norm)拒绝了重名(帮名全局唯一,不分 zone)。`
- 第 596-597 行:
  ```go
  // guildNameUniqueKey 是 guild 表帮名唯一索引名。proto2mysql 按 "uk_"+表名 命名,
  // 由 proto/guild/guild_db.proto 的 OptionUniqueKey="name_norm" 生成;表名改了这里必须同改。
  const guildNameUniqueKey = "uk_guild"
  ```
- 第 600 行:`// MySQL 8 / TiDB 形如 "Duplicate entry 'x' for key 'guild.uk_guild'",5.7 不带表名前缀。`
- 第 601 行:`// 按**结尾**匹配:帮名本身出现在消息中段,名字里含 "uk_guild" 不能让主键冲突被误判成重名。`

`isDuplicateKeyOn` 按结尾匹配:`uk_guild_member` 的消息以 `.uk_guild_member'` 结尾,不会命中 `.uk_guild'` 或 `'uk_guild'`,不会被误判为重名。

**不变的**:事务边界、`FOR UPDATE` 锁序(guild → guild_member)、缓存失效与 generation 机制、错误哨兵、tip 映射。B1 不新增写路径,也不新增 tip。

**缓存兼容**:旧的 `guild:v2:{id}` JSON 没有新字段,解码后为零值;int64 → uint64 解码数值相同。新库本来就是空库,且切换步骤会清空 guild Redis DB 2(§19),所以缓存 key 不升 v3。

### 8.2 `internal/logic/guild_logic.go`

- 第 60 行注释:`// 帮名仍全局唯一(uk_guild,按 name_norm):合服时两区的帮会直接合并,不需要改名。`
- `normalizeGuildName`(第 105-115 行):在控制字符循环之后、`return name, true` 之前加:
  ```go
  	// name_norm 必须可生成且不超长,否则唯一键覆盖不到全值(guild-phase2 §6.3)。
  	if _, ok := data.GuildNameNorm(name); !ok {
  		return "", false
  	}
  ```
  失败沿用现有 `kGuildNameInvalid`,不新增 tip。
- 第 150 行 `now := time.Now().UnixMilli()` 改为 `now := uint64(time.Now().UnixMilli())`。第 162、169、170 行不用改。
- `toGuildInfo`(第 586-606 行):`CreateTimeMs: int64(g.CreateTimeMs)`、`JoinTimeMs: int64(m.JoinTimeMs)`、`LastActiveMs: int64(m.LastActiveMs)`,并加注释 `// guild.proto 仍是 int64(B2 随客户端重生成统一);毫秒时间戳远小于 2^63,转换无损。`第 602 行改为 `Contribution: m.ContributionTotal, // B2 把 GuildMember.contribution 拆成 total/balance 前,展示累计帮贡`。
- 编译器若报出其他 int64/uint64 不匹配(例如 `last_active_ms` 的比较),一律在 proto 边界转换,repo 内部保持 uint64。

### 8.3 `internal/constants/constants.go`

第 44 行注释改为 `// ErrGuildNameTaken:uk_guild(name_norm)全局唯一索引拒绝(帮名跨 zone 唯一,合服不必改名)。`

## 9. friend 去掉 `guild_schema_migration` 依赖

**为什么可以直接删**:这道门只防一种情况:遗留 SQL 过程执行到一半(已建 `friend_capacity`、还没回填),friend 把存量容量当成 0。这个过程随 §4 一起删除,之后没有任何代码会写入 `pending`。容量不变量由写路径保证:缺容量行时按 friend 的权威边重算(`friend_repo.go:406-`,测试 `friend_repo_mysql_test.go:213-214`)。门表删掉后,如果门还留着,friend 在新数据卷上直接起不来。

**改动**(行号为 2026-09-16 核对值):
- `go/friend/friend.go`:删除第 42-43 行注释、`repo.RequireFriendCapacityReady(...)` 调用及其失败分支;保留 `repo := data.NewFriendRepo(...)`。
- `go/friend/internal/data/friend_repo.go`:
  - 删除 `ErrFriendCapacityMigrationNotReady`(21-23)、`friendCapacityMigrationKey`(26)、`RequireFriendCapacityReady`(57-61)、`migrationStateQuerier`(63-65)、`requireFriendCapacityReady`(67-83)、`friendCapacityMigrationStateError`(85-93)。
  - 删除三处调用:202-204(事务内)、341-343(事务内)、410-414(`ensureFriendCapacityRows` 开头,连同注释)。
  - 按编译器提示清理不再使用的 import。
- `go/friend/internal/data/friend_repo_mysql_test.go`:
  - 删除 `TestFriendCapacityMigrationStateRequiresExplicitReady`(143-165)。
  - `TestFriendCapacityHalfMigrationFailsClosedAndMissingRowUsesAuthoritativeCount`(167-215)改名为 `TestFriendCapacityMissingRowUsesAuthoritativeCount`:删除"置 pending、断言 `ErrFriendCapacityMigrationNotReady`"和"不得创建伪 0 行"两段(194-203),删除置 ready 的 UPDATE(205-208),保留"造 3 条好友边后操作,断言 `friend_count == 3`"。
  - `resetFriendIntegrationSchema`:删除 `guild_schema_migration` 的 DROP / CREATE / INSERT(246-251、272-273)。

好友表仍在 `mmorpg`,friend 的 DSN 和 yaml 不变。旧数据卷里的 `mmorpg.guild_schema_migration` 在新版 friend 下已无人读取,按 §19 删除即可。

<!-- s1_storage_part5.md -->

# S1 存储(B1 / B1b)— 第 5 部分:合服工具(B1b)、周边与故障

## 10. tools/merge_zone:`-guild-schema`(B1b)

做法与 trade 一致(`trade_step.go:34-36,58-82`):继续用同一个 `-mysql-dsn`,帮会表写成 `库名.表名` 访问。库名只接受 `[A-Za-z0-9_]{1,64}`,这是字符串拼接唯一的注入防线。

**`guild_step.go`**:
```go
const (
	// defaultGuildSchema 镜像 go/guild/internal/data.DatabaseName(go/guild 的 config.Validate 强制 DSN 库名等于它)。
	// merge_zone 是独立 module,不能 import go/guild;字面量由 merge_unit_test.go 守住。
	defaultGuildSchema = "mmorpg_guild"
	guildTable         = "guild"
	guildMemberTable   = "guild_member"
)

// validateGuildSchemaName 复用 trade_step.go 的 tradeSchemaNamePattern(同包),不改 trade_step.go。
// 不合法时返回 fmt.Errorf("-guild-schema %q is not a plain identifier ([A-Za-z0-9_], 1-64 chars)", schema)。
func validateGuildSchemaName(schema string) error

func guildQualified(schema, table string) string { return schema + "." + table }

// assertGuildTablesReady:校验库名 → assertSchemaExists → tableColumns(schema,"guild") 必须同时含
// zone_id、name、name_norm → tableColumns(schema,"guild_member") 必须含 guild_id。任一不满足都返回错误,并点名 schema.table。
func assertGuildTablesReady(ctx context.Context, db *sql.DB, schema string) error
```

- `collectGuildIDsInZone`、`assertNoGuildNameCollision`、`migrateGuildZone` 的签名都在 `db` 之后加 `schema string`。4 条 SQL 里的 `guild` 一律换成 `guildQualified(schema, guildTable)`,JOIN 两侧都换。
- `assertNoGuildNameCollision` 的探测改为比较规范化名:`SELECT s.guild_id, d.guild_id, s.name FROM <q> s JOIN <q> d ON s.name_norm = d.name_norm AND d.zone_id = ? WHERE s.zone_id = ?`。错误文案中的 `guild.name is globally UNIQUE today` 改为 `guild.name_norm is globally UNIQUE today (uk_guild)`。
- 注释改写:第 7-8 行改为 `proto/guild/guild_db.proto 的 OptionUniqueKey="name_norm"(uk_guild)是**全局**唯一(不带 zone_id),所以`;第 12 行 `uk_name` → `uk_guild`;第 82-83 行 `uk_name 全局唯一` → `uk_guild 按 name_norm 全局唯一`,`uk_name 改成 (zone_id, name)` → `uk_guild 改成 (zone_id, name_norm)`。

**其余文件**:
- `main.go`:options 加 `guildSchema string`;在第 181 行之后加 `flag.StringVar(&o.guildSchema, "guild-schema", defaultGuildSchema, "guild database reached through -mysql-dsn (go/guild forces DSN database mmorpg_guild; override only for isolated tests)")`;第 146 行帮助文字改为 `"MySQL DSN. Must reach zone_<N>_db, the friend tables (DSN default database), the guild database (-guild-schema) and the trade database (-trade-schema)"`;第 268 行附近照 trade 写法调用 `validateGuildSchemaName`;第 283-307 行审计参数传入 `guildSchema: o.guildSchema`。
- `merge_run.go`:第 43 行日志加 `guild: schema=%s`。trade preflight(87-95)之后、生成清单之前加:`if !o.skipGuild || !o.skipRank { if err := assertGuildTablesReady(ctx, db, o.guildSchema); err != nil { log.Fatalf("preflight: guild tables not ready: %v (pass -skip-guild-mysql and -skip-guild-rank only if the guild service was never deployed here)", err) } }`。第 163、244、247 行传入 `o.guildSchema`。
- `unmerge.go`:第 57-60 行日志带上 guild schema;trade preflight(73-77)之后,`len(m.GuildIDs) > 0` 时调用 `assertGuildTablesReady`,失败则 `log.Fatalf("preflight: manifest lists %d guilds to restore, but %v", …)`;第 141 行 UPDATE 用 `guildQualified(o.guildSchema, guildTable)`。
- `audit_resources.go`:`auditConfig`(108-111 附近)与参数结构(150 附近)各加 `guildSchema string`,第 240 行赋值;`auditGuildMembers`(501-525)的两条 SQL 改用限定表名,库名不合法时返回 infra;第 81 行字段注释改为 `game MySQL — friend / zone_{N}_db;帮会表经 guildSchema 限定`;第 445 行 `see deploy/mysql-init/guild_friend_tables.sql` 改为 `see proto/guild/guild_db.proto`。
- `audit_checks.go`:`verifyGuildZoneDrained`(230-248)与 `verifyGuildRankZSets`(256-295)的 3 条 SQL 改用限定表名。**先判 `cfg.db == nil`,再校验库名**,这样 `trade_step_test.go` 里不带 guildSchema 的 `auditConfig` 仍走原来的早退分支。
- `integration_test.go`:
  - 新增一次性库常量 `itGuildSchemaDB = "merge_zone_it_guild"`,加入 TestMain 的 DROP / CREATE 列表。第 134、144 行的 `guild` / `guild_member` 改建到该库;friend 两表仍建在 `merge_zone_it_db`;第 214 行按库拆分清理循环。
  - 第 133 行注释改为 `// 镜像 proto/guild/guild_db.proto 的 guild 表中 merge_zone 用到的列(name_norm 全局 UNIQUE)。`第 137 行 DDL 改为 `name_norm VARCHAR(191) AS (LOWER(name)) STORED, PRIMARY KEY (guild_id), UNIQUE KEY uk_guild (name_norm), KEY idx_guild_0 (zone_id)`。用生成列的好处:第 644、651、660、1042 行的种子 INSERT 不用改,第 651 行"同名插入必须失败"的断言照样成立。
  - itRun 基础参数(第 1012 行附近,与 `-trade-schema` 并列)加 `"-guild-schema", itGuildSchemaDB`;第 716 行 auditConfig 加 `guildSchema: itGuildSchemaDB`。
  - 帮会表必须从 DSN 默认库移走,测试才能证明 flag 真的生效;否则不带库名的 SQL 也能通过。
  - 新增 `TestMergeRefusesMissingGuildSchema`,照第 1231-1242 行的 trade 用例:传 `-guild-schema merge_zone_it_no_such_guild`,断言退出码非 0、输出含 `preflight: guild tables not ready`、源区 `player:zone` 映射与玩家行未被改写。
- `merge_unit_test.go`:新增 `TestDefaultGuildSchemaMatchesGuildService`(`defaultGuildSchema == "mmorpg_guild"`),以及 `TestValidateGuildSchemaName`(接受 `mmorpg_guild`、`merge_zone_it_guild`;拒绝 `""`、`a.b`、``a`b``、`x;DROP`、65 个 `a`)。

**清单兼容性**:manifest 不记录库名。项目未上线,不存在跨版本续跑的合服。

**落码注记(2026-09-17,与上文的差异以本块为准)**:
- 库名形状正则没有"复用 trade 的变量",而是从 `trade_step.go` 提到 `player_rows.go` 的 `schemaNamePattern`(紧挨 `assertSchemaExists`),trade / guild 各自的 `validate*SchemaName` 调它;`trade_step.go` 因此少了 `regexp` import。帮会代码引用一个 trade 命名的变量会误导下一个读码的人。
- `auditFriend`(`audit_resources.go` 445 附近)那处 `see deploy/mysql-init/guild_friend_tables.sql` **不改**:它讲的是 friend 表,friend 三表仍由该 SQL 建。要改的是 `auditConfig.db` 的字段注释(`guild / friend / zone_{N}_db` → friend 与 zone 库,帮会/聚宝斋各在自己的库)。
- 合服前置的跳过条件是 `o.skipGuild && o.skipRank`(两个都给才算跳过),照 trade 的写法给出 `guild SKIPPED` 日志;只给 `-skip-guild-mysql` 仍然拦,因为榜单步也读 guild 表。
- 集成测试没有新增 `itGuildSchemaDB`,而是把原来的 `itGuildDB`("装帮会+friend 的默认库")拆成 `itDefaultDB = merge_zone_it_db`(friend 两表)与 `itGuildDB = merge_zone_it_guild`(帮会两表)。
- 用例名按仓库既有命名落成 `TestGuildNamesMatchGuildService` / `TestValidateGuildSchemaName`(单测)与 `TestIT_AssertGuildTablesReady` / `TestIT_Unmerge_RefusesMissingGuildSchemaBeforeAnyWrite`(集成);合服拒绝的三种情形并入既有的 `TestIT_EndToEnd_RefusesEmptySourceAndSkipPlayerRowsWithoutAttestation`,与 trade 的同类断言并排。

## 11. tools/data_consistency_check(B1b)

- `main.go` 第 72 行后加 `guildSchema := flag.String("guild-schema", "mmorpg_guild", "guild database reached through -mysql-dsn")`。用本地正则 `^[A-Za-z0-9_]{1,64}$` 校验,不合法则 `log.Fatal`(独立 module,不复用 merge_zone 的函数);校验通过后写入 `runConfig.guildSchema`。
- 第 230、237、263 行的 `FROM guild` 改为 `"FROM " + cfg.guildSchema + ".guild"`。"表不存在就报 info"的尽力语义不变,提示文字带上库名。

## 12. data_service 注记(B1 不改 data_service 代码)

1. **回滚**(`rollback_logic.go:378-382`):RollbackPlayer / Zone / All 本来就不回滚帮会数据,B1 只是换了存储位置。B5 引入捐献和商店后,会出现"玩家货币回到快照,帮会资金和帮贡保留"的经济不一致。这归 B4a/B5 的资产账本处理;那一批顺带把这段注释改为"帮会数据在独占库 mmorpg_guild,不参与回滚,资产 op 账本对账见 guild-phase2 §B5"。
2. **号段水位地板**(`schema.go:112-115`、`id_segment_store.go:292-303`):只在 data_service 自己的全局库里找 `guild.guild_id`,迁移前后都找不到,只打 Info,行为不变。人工核对步骤(`deploy/k8s/README.md:351`)改为 `SELECT MAX(guild_id) FROM mmorpg_guild.guild`,放在文档批。
3. `guild_asset_op` 的 biz_tag 进 `BootstrapTags` 归 B5。

## 13. 本地脚本(B1)

- `tools/scripts/start_game.ps1`:在 trade 预检块(403-430)之后照同样形状加帮会块,条件为 `if ('guild' -notin $skippedServices)`;查询 `SCHEMA_NAME = ''mmorpg_guild''`;不就绪时 `$skippedServices += 'guild'` 并输出 `Write-Warning "帮会库 mmorpg_guild 未就绪($guildDbProblem),本次跳过 guild(帮会请求会回「服务不可用」),其余服务照常启动。"`;补建命令与 trade 块相同,库名换成 `mmorpg_guild`。第 472 行已过滤 `$skippedServices`,不用再改。脚本顶部第 34 行附近的说明补一条"MySQL 独占库 mmorpg_guild 未就绪"。trade 块是他人未提交的改动,不抽公共函数,避免冲突。
- `tools/scripts/go_services.ps1`:**不改**。dev 档建表走 yaml 的 `Schema.AutoMigrate: true`;多开时首次建表的锁竞争由 ensureSchema 在进程内重试(§7.4)。

## 14. K8s(记录,不在 B1 落地)

guild 目前没有 ConfigMap 和 manifest。部署 guild 时一次补齐:
- `k8s_deploy.ps1` 服务表的 guild 项写 `Global = $true; MigrateJob = "guild-migrate.yaml"`。ConfigMap 的 `Schema.AutoMigrate`:dev 用 yaml 的值,staging/prod 固定 `false`(照 trade:1485-1518、2076-2078)。`MySQL.DataSource` 库名固定为 `mmorpg_guild`,`DataServiceRpc.Timeout` 与 `Timeout` 满足 §7.3 的预算约束。
- 新增 `deploy/k8s/manifests/go-svc/guild-migrate.yaml`,从 `trade-migrate.yaml` 复制,args 为 `-f /app/etc/guild.yaml -migrate`,镜像 `mmorpg-guild`。staging/prod 等 Job Complete 后再 apply Deployment;退出码 3 靠 backoff 重试,1 或 4 中断发布。
- 镜像:`Dockerfile.go-svc:68-71` 已 COPY `schemamigrate/`;`go_svc_image.ps1` 照 trade(第 88 行)把 schemamigrate 带进构建上下文。若届时 proto2mysql 仍用 replace,构建环境要能拉到 `github.com/luyuan-cpp/proto2mysql`(goproxy.cn)。
- README:补已有 PVC 的手工建库步骤;TiDB BR 按库恢复清单加入 `mmorpg_guild`;按 D-14 §9 补 audit auditor。

## 15. 故障、崩溃窗口与回滚

| 场景 | 表现 | 恢复 |
|---|---|---|
| guild 实际解析到旧版 proto2mysql(h1:s5du1n) | Up 报 1170(BLOB/TEXT 列建键未给长度),guild 拒绝启动 | §7.1:补上与 schemamigrate 相同的 replace;§18 第 0 步事先拦住 |
| 库不存在或 appuser 无权限 | `NewServiceContext` 的 `db.Ping` 报 Unknown database 并 panic;`-migrate` 退出码 1;start_game 预检跳过 guild | root 执行 §3 两句 |
| DDL 执行中进程被杀 | 台账留下 dirty,下次 Up/Plan 返回 `ErrDirty`,拒绝启动 | 未上线:删库重建(§6.4);上线后人工核对 `mmorpg_guild.schema_migrations` |
| 本地多开首次同时建表 | 等锁超过 10s 的一方拿到 `ErrLockBusy`,进程内间隔 2s 重试,最多 3 次;仍忙则拒绝启动 | 再启动一次(这时表已建好,Up 为 0 条语句) |
| 类型漂移、缺唯一键 | Report.Manual 非空,拒绝启动 | 开发期删库重建;上线后单独设计 |
| 后续批次加了 OptionIndex 组,库是旧的 | Warnings 出现"缺索引",ensureSchema 拒绝启动 | 开发期删库重建(§6.4 规则 5) |
| 请求内 I/O 超预算 | 预算 ctx 到期,logic 回 `kGuildIdGenUnavailable` 或内部错误,整个请求 ≤ 4000ms | 查 data_service / MySQL 延迟 |
| 切库后 Redis 旧缓存 | `guild:v2:{id}` 里是旧库的帮会,最长保留 30 分钟 | §19 第 3 步清空 DB 2;`RebuildRanks` 清掉旧榜 |
| 新 yaml 配旧 guild 二进制 | 旧二进制执行 `MigrateLegacyRankScores`,在新库找不到门表,panic;旧 Validate 不存在,不拦 | yaml 与二进制同一提交部署 |
| 新数据卷配旧 friend 二进制 | 门表不存在,friend 拒绝启动 | friend 改动与 mysql-init 改动同在 B1 |
| B1 已合入、B1b 未合入时跑合服 | merge_zone 仍按 DSN 默认库读旧 `mmorpg.guild`,帮会漏迁 | 禁止;B1b 紧随 B1 |
| 回滚 B1(revert) | 旧二进制重新读 `mmorpg`;B1 期间写进 `mmorpg_guild` 的帮会数据不可见 | 未上线可接受;§19 要求 B2 验收前不删 `mmorpg` 里的旧帮会表 |

B1 没有新增玩家可见的错误,也没有新 tip。存储相关的失败都发生在启动期,表现为进程拒绝启动。

<!-- s1_storage_part6.md -->

# S1 存储(B1 / B1b)— 第 6 部分:测试与文件清单

## 16. 测试

B1 不涉及 C++ gtest 和 Unity EditMode:C++ 只登记生成物,没有 C++ 逻辑;客户端也没有改动。

### 16.1 go/guild 纯单测(无外部依赖)

**`internal/config/config_test.go`**
- `minimalGuildYaml`(第 76 行起):加 `Timeout: 4000`;DSN 改为 `u:p@tcp(127.0.0.1:3306)/mmorpg_guild`;加 `DataServiceRpc:\n  Timeout: 2000`。Validate 会自动执行,不改的话现有用例会失败。
- `TestValidateTimeoutBudget`(表驱动,都基于 minimalGuildYaml 反序列化后改字段,再直接调 `c.Validate()`):
  - 通过:(Timeout 4000, DS 2000)、(3500, 1500)、(4000, 500)。
  - 报错:(4001, 2000)、(10000, 2000)、(0, 2000)、(3999, 2000)(预算不足)、(4000, 3000)(预算不足)、(4000, 0)、(4000, 499)、(4000, 3001)。
  - 断言预算不足时的错误文案含 `超时预算不足`。
- `TestValidateCallsEmbeddedRpcServerConfValidate`:`Auth: true` 且 `Redis` 为零值 → 报错;`Auth: false` → 不因 Redis 报错。
- `TestValidateRequiresGuildDatabase`:库名 `/mmorpg` 报错;`/mmorpg_guild` 通过;`"u:p4ss@tcp(::bad"` 报错,且错误文案不含 `p4ss`。
- `TestEtcYamlPassesValidate`:`conf.Load("../../etc/guild.yaml")` 成功,`Timeout==4000`,`DataServiceRpc.Timeout==2000`,`ShouldAutoMigrate()==true`,`RequestBudget()==3500*time.Millisecond`。
- `TestShouldAutoMigrateDefaults`:nil 返回 true;指向 false 的指针返回 false。
- `internal/config/idsegment_conf_test.go` **不改**:第 16 行加载的 `etc/guild.yaml` 已是合法值;第 42 行的内联 yaml 本就刻意忽略加载错误(go-zero 先反序列化再调 Validate,不影响零值断言)。

**`guild_test.go`(package main)**
- 第 65、121、147 行的 `buildUnaryInterceptors(ks)` 改为 `buildUnaryInterceptors(ks, time.Second)`。
- `TestRequestBudgetInterceptorSetsDeadline`:经 `chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{}), 300*time.Millisecond), …)` 调一个不被关停的方法(`GuildService_GetGuild_FullMethodName`,`context.Background()`,不带会话 metadata;`internal/session/session.go:14` 规定无会话即内部调用,放行)。handler 里取 `ctx.Deadline()`,断言存在且距 now 在 (200ms, 300ms] 内。再单独对 `requestBudgetInterceptor(50*time.Millisecond)` 断言:handler 内 `<-ctx.Done()` 在 200ms 内返回 `context.DeadlineExceeded`。
- `TestHomeZoneBudgetMirrorsLogic`:`time.Duration(config.HomeZoneLookupBudgetMs)*time.Millisecond == logic.DefaultHomeZoneLookupTimeout`。
- `TestEnsureSchemaStrategy`:照 `trade_test.go:308-345` 的 fakeSchemaRunner 和 7 个用例改名复制,再加 5 个用例(`lockBusyRetryDelay` 在用例里置 0,`t.Cleanup` 还原):
  1. Up 前两次返回 `ErrLockBusy`、第三次成功 → nil,调用 3 次。
  2. Up 三次都 `ErrLockBusy` → 返回错误,`errors.Is(err, schemamigrate.ErrLockBusy)`,调用 3 次。
  3. Up 成功,但 `Warnings` 含 `缺索引(不会自动建):guild 上没有 proto 声明的索引 idx_guild_1` → 返回错误,文案含 `缺 1 个`。
  4. AutoMigrate=false,Plan 返回 Clean,但 Warnings 含同样的缺索引行 → 返回错误。
  5. Warnings 只有 `多余列(不会删除):…` → nil。
- `TestSchemaOptionsTargetsGuildDatabase`:`Database=="mmorpg_guild"`;`len(Tables)==len(data.Tables())==4`。
- `TestReportLinesFormatsAllSections`。
- `TestMissingIndexWarningDetectedOnRealDB`(需 `GUILD_TEST_MYSQL_DSN`,没设就 Skip):库名先过 §16.2 的白名单 → DROP 全部表后 Up → `ALTER TABLE guild DROP INDEX idx_guild_0` → `schemamigrate.Plan` → 断言 `missingIndexWarnings(report)` 恰为 1 行。它守住 `缺索引` 前缀与 schemamigrate 真实文案一致。

**`internal/data/guild_repo_zone_test.go`**
- `isDuplicateKeyOn` 表驱动(第 27-33 行):应命中 `Duplicate entry '青云门' for key 'guild.uk_guild'`、`… for key 'uk_guild'`、以及 wrapped 的形式;不应命中 `guild_member.uk_guild_member`、`guild.PRIMARY`、`Duplicate entry 'a.uk_guild'' for key 'guild_member.uk_guild_member'`、1452 错误、非 MySQL 错误。
- 第 74 行种子 INSERT 加列 `name_norm`,值 `'zone-two-guild'`。
- 第 100 行注释 `撞 uk_name` 改为 `撞 uk_guild(name_norm)`。
- `TestCreateGuildMapsNameCollisionAcrossZones`(第 90-104 行)保持原断言,`founding` 改为接收 `name string` 参数。在它后面新增 `TestCreateGuildNameNormCollision`:先用 `"Qing云"` 建 7301/8301(zone 1);再依次用 `"qing云"`(7302/8302)、`"ＱＩＮＧ云"`(7303/8303)、`"Qing云 "`(7304/8304,repo 层不 Trim 展示名,但 name_norm 会 Trim)在 zone 2 建帮。每次都断言 `errors.Is(err, ErrGuildNameTaken)` 且 `memberCount(…)==0`。

**`internal/data/guild_repo_test.go`**
- 删除三个 `TestDiffGuildIDSets_*` 和 `resetGuildAnnouncementIntegrationSchema`(第 140-180 行,含 `uk_name`/`uk_player` DDL)。
- `TestGuildNameNorm`:`"青云门"`→`"青云门"`;`"  ABC "`→`"abc"`;`"ＡＢＣ"`→`"abc"`;Go 字面量 `"Ab　"`(尾部全角空格)→`"ab"`;`""`、`"   "` → false;`strings.Repeat("帮", 24)` → ok;`strings.Repeat("㍿", 13)`(NFKC 后 52 rune)→ false。
- 第 89-96 行种子:guild 加 `name_norm`(`'auth-test'`)和 `funds`(`0`);guild_member 列改为 `contribution_total, contribution_balance`,值 `0, 0`。
- 新增助手(本文件不带 build tag,integration 构建同样可见):

```go
// guildTestDropTables:本服务全部表 + 台账。新增表时同步追加(TestDropListCoversTables 守数量)。
var guildTestDropTables = []string{"guild_application", "guild_member", "guild_player_state", "guild", "schema_migrations"}

// guildTestDBNamePattern:测试只许碰这两类一次性库。appuser 对 testdb / zone_N_db / mmorpg_trade 也有 ALL 权限,
// 黑名单挡不住 DSN 指错,所以用白名单。
var guildTestDBNamePattern = regexp.MustCompile(`^(guild_test|guild_it_\d+_\d+)$`)

// resetGuildSchemaViaMigrate:DROP 全部表后经 schemamigrate.Up 重建(与生产同一条路径)。
func resetGuildSchemaViaMigrate(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var dbName string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbName))
	if !guildTestDBNamePattern.MatchString(dbName) {
		t.Fatalf("测试 DSN 指向库 %q:只允许 guild_test 或 guild_it_<pid>_<n>(防止误删 data_service / trade / go/db 的表与台账)", dbName)
	}
	for _, table := range guildTestDropTables {
		_, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+table+"`")
		require.NoError(t, err, table)
	}
	report, err := schemamigrate.Up(ctx, db, schemamigrate.Options{Database: dbName, Tables: Tables(), Logf: t.Logf})
	require.NoError(t, err)
	require.Empty(t, report.Manual, "新建库不应出现需人工项")
}
```

- `TestDropListCoversTables`:`len(Tables())+1 == len(guildTestDropTables)`。
- `TestGuildTestDBNamePattern`:接受 `guild_test`、`guild_it_123_4`;拒绝 `mmorpg_guild`、`mmorpg`、`testdb`、`zone_1_db`、`mmorpg_trade`、`guild_test2`、`""`。

### 16.2 go/guild 真库测试

- `TestUpdateAnnouncementAuthorizedRejectsStalePrivilegedCache` 和 `openGuildIntegrationRepo`(`guild_repo_zone_test.go:55`)改为调用 `resetGuildSchemaViaMigrate`。
- 新增 `TestSchemaMigrateProducesExpectedGuildShape`(需 `GUILD_TEST_MYSQL_DSN`)。重建后:
  - 表集合:`SHOW TABLES` 的结果集合等于 `guildTestDropTables`。
  - `information_schema.STATISTICS`(`TABLE_SCHEMA=DATABASE()`)按 `(TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX)` 取 `(COLUMN_NAME, NON_UNIQUE, SUB_PART)`,与期望表**逐行全等**:
    - `guild`:`PRIMARY`(1,guild_id);`uk_guild`(1,name_norm,NON_UNIQUE=0,SUB_PART=191);`idx_guild_0`(1,zone_id);`idx_guild_1`(1,leader_id)。
    - `guild_player_state`:`PRIMARY`(1,player_id)。
    - `guild_member`:`PRIMARY`(1,guild_id)(2,player_id);`uk_guild_member`(1,player_id,NON_UNIQUE=0)。
    - `guild_application`:`PRIMARY`(1,guild_id)(2,player_id);`idx_guild_application_0`(1,player_id);`idx_guild_application_1`(1,expire_ms)。
  - `COLUMNS`:`guild.funds` 为 `bigint unsigned`;`guild.name_norm` 存在;`guild_member` 有 `contribution_total`/`contribution_balance`、没有 `contribution`;`guild_member.join_time_ms` 为 `bigint unsigned`。
  - 再跑一次 Up:`len(report.Statements)==0` 且 `len(report.Warnings)==0`。
- `rank_zone_integration_test.go`(`//go:build integration`):删除 `createGuildTables`(第 102-136 行,含 `uk_name`/`uk_player` DDL);一次性库 `guild_it_<pid>_<n>` 建好后,改为对该库调 `resetGuildSchemaViaMigrate`。`seedGuild`(第 138-150 行)的 guild INSERT 加 `name_norm`(传 `strings.ToLower(name)`),guild_member 列改为 `contribution_total, contribution_balance`。yaml 切到 appuser 后,appuser 建不了库,这组测试会 Skip;验证时必须把 `GUILD_IT_MYSQL_DSN` 设为 root。

### 16.3 friend / B1b 工具

见 §9、§10、§11。

### 16.4 robot 冒烟回归

本地起全栈后执行 `cd robot; go run . -c etc/guild_smoke.yaml`(Mode=guild-smoke)。期望与 B1 之前一致:建帮、入帮、分区榜在 zone 隔离场景下全部通过。

## 17. 文件清单

### 17.1 B1 手改文件(23)

| # | 文件 | 说明 |
|---|---|---|
| 1 | proto/guild/guild_db.proto | 新 |
| 2 | go/guild/go.mod | 改 |
| 3 | go/guild/guild.go | 改 |
| 4 | go/guild/guild_test.go | 改 |
| 5 | go/guild/etc/guild.yaml | 改 |
| 6 | go/guild/internal/config/config.go | 改 |
| 7 | go/guild/internal/config/config_test.go | 改 |
| 8 | go/guild/internal/constants/constants.go | 改(第 44 行注释) |
| 9 | go/guild/internal/data/tables.go | 新 |
| 10 | go/guild/internal/data/guild_repo.go | 改 |
| 11 | go/guild/internal/data/guild_repo_test.go | 改 |
| 12 | go/guild/internal/data/guild_repo_zone_test.go | 改 |
| 13 | go/guild/internal/data/rank_zone_integration_test.go | 改 |
| 14 | go/guild/internal/logic/guild_logic.go | 改 |
| 15 | deploy/mysql-init/00_init_zone_dbs.sql | 改(只追加) |
| 16 | deploy/mysql-init/guild_friend_tables.sql | 改 |
| 17 | go/friend/friend.go | 改 |
| 18 | go/friend/internal/data/friend_repo.go | 改 |
| 19 | go/friend/internal/data/friend_repo_mysql_test.go | 改 |
| 20 | tools/scripts/start_game.ps1 | 改 |
| 21 | cpp/generated/proto/CMakeLists.txt | 改:在 `guild/guild.pb.cc`(第 117 行)之后加 `guild/guild_db.pb.cc` |
| 22 | cpp/generated/proto/proto.vcxproj | 改:照第 122、233 行 trade_table 的写法,加 `guild\guild_db.pb.cc` 的 ClCompile 和 `guild\guild_db.pb.h` 的 ClInclude |
| 23 | cpp/generated/proto/proto.vcxproj.filters | 改:照第 294、611 行,放进与 `guild\guild.pb.cc/.h` 相同的 Filter |

`guild_db.grpc.pb.*`(如果生成)不登记,与 trade_table.grpc 的现状一致。

### 17.2 B1b 手改文件(9)

`tools/merge_zone/{guild_step,main,merge_run,unmerge,audit_resources,audit_checks,integration_test,merge_unit_test}.go`(8 个),以及 `tools/data_consistency_check/main.go`。

### 17.3 生成物与文档

- **生成物(不手改)**:`go/proto/guild/guild_db.pb.go`、`cpp/generated/proto/guild/guild_db.pb.cc/.h`(及可能的 `.grpc.pb.*`)、`go/guild/go.sum`(tidy)。
- **文档(不计入 30,随批次追加)**:`docs/design/xuanming-port-decisions-20260910.md`(D-14 §5/§7/§8/遗留,正文见 §20,随 B1 提交)、`docs/design/guild-phase2.md`、`PROGRESS.md`;B1b 附带 `docs/ops/merge-zone-runbook.md`(第 400 行及 `-guild-schema` 说明);文档批再处理 `deploy/k8s/README.md:351`、`docs/design/friend-persistence-architecture.md:58-79`、`docs/design/guild_friend_service_notes{,_en,_zh}.md:34-38`、`docs/design/guild-zone-client-access.md` 的存储段。

### 17.4 并行会话状态

2026-09-16 执行 `git status --short -- go/guild go/friend tools/merge_zone tools/data_consistency_check deploy/mysql-init tools/scripts/start_game.ps1 docs/design/xuanming-port-decisions-20260910.md go/schemamigrate go/trade proto/guild cpp/generated/proto`,只看到:`go/schemamigrate/go.mod`、`go.sum`;`go/trade/` 下的 go.mod、trade.go、trade_test.go 及若干测试文件,另有未跟踪的 go.sum;`cpp/generated/proto/guild/guild.grpc.pb.h`;未跟踪的 `cpp/generated/proto/trade/*.grpc.pb.*`。本设计涉及的手改文件目前**都没有**他人未提交的改动。这只是快照:落码前必须重跑,并把结果如实写进交付说明。

<!-- s1_storage_part7.md -->

# S1 存储(B1 / B1b)— 第 7 部分:验证命令、切换步骤、D-14 修订、契约偏差

## 18. Codex 验证(串行执行;Claude 一步也没有跑过)

**前置条件**:buildenv 提供 Go 1.26.5;Docker 中 MySQL 与 Redis 已启动;先执行 §17.4 的 `git status`,如实记录,确认本批文件只做合并、不覆盖他人改动。

0. **proto2mysql 解析核对(硬前置,不通过就停止)**,在 `go/schemamigrate` 下执行:
   ```
   git diff --quiet HEAD -- go.mod go.sum; $LASTEXITCODE        # 必须为 0:schemamigrate 的解析方式已提交
   go list -m -json github.com/luyuancpp/proto2mysql              # 记录 Version 与 Replace
   go mod download -json <Replace.Path 或 Path>@<Replace.Version 或 Version>   # 取返回的 Dir
   rg -n "TextIndexPrefixLength" <Dir>/proto2mysql.go              # 必须命中(= 191)
   rg -n "SHARD_ROW_ID_BITS" <Dir>                                  # 必须命中
   rg -n "proto2mysql" go.sum                                       # 记录 h1 行;不得是 h1:s5du1n…
   go test ./... -count=1                                           # plan_test 的 TiDB 方言断言必须通过
   ```
1. **proto-gen**(与其他会话串行):照本仓库现行入口重生成 proto。
   - 期望生成 `go/proto/guild/guild_db.pb.go`,含 `GuildRecord`、`GuildPlayerStateRecord`、`GuildMemberRecord`、`GuildApplicationRecord`;生成 `cpp/generated/proto/guild/guild_db.pb.cc/.h`。
   - `git diff --stat proto/message_id.txt` 为空。
   - `git diff cpp/generated/proto/guild/guild.grpc.pb.h`:该文件本来就有他人改动;重生成后,如果 diff 超出预期,要在报告里写明。
   - robot 若带 proto 副本,检查 `git status robot`,需要时重新同步。
2. **guild 编译与单测**,在 `go/guild` 下执行:
   ```
   go mod tidy
   go list -m -json github.com/luyuancpp/proto2mysql   # Replace / Version 必须与第 0 步一致
   rg -n "proto2mysql" go.sum ../schemamigrate/go.sum  # 两边 h1 行逐字相同
   go build ./...
   go vet ./...
   go test ./... -count=1
   ```
   通过标准:全绿;依赖 MySQL 的用例 Skip。
3. **C++ 登记**:`MSBuild cpp/generated/proto/proto.vcxproj /p:Configuration=Debug /p:Platform=x64 /m:1 /t:ClCompile`,期望 0 error,且编译清单里有 `guild_db.pb.cc`。MSBuild 必须串行,同一时刻只能有一个构建方。
4. **建库与授权**(root):
   ```
   docker --context desktop-linux exec mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -e "CREATE DATABASE IF NOT EXISTS mmorpg_guild DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci; GRANT ALL PRIVILEGES ON mmorpg_guild.* TO appuser@''%''; CREATE DATABASE IF NOT EXISTS guild_test; GRANT ALL PRIVILEGES ON guild_test.* TO appuser@''%''; CREATE DATABASE IF NOT EXISTS friend_test; GRANT ALL PRIVILEGES ON friend_test.* TO appuser@''%''; FLUSH PRIVILEGES;"'
   ```
5. **guild 真库测试**:
   ```
   $env:GUILD_TEST_MYSQL_DSN='appuser:apppass123@tcp(127.0.0.1:3306)/guild_test?parseTime=true&charset=utf8mb4'; go test ./... -count=1 -v
   $env:GUILD_IT_MYSQL_DSN='root:Mmorpg#2026db@tcp(127.0.0.1:3306)/mysql?charset=utf8mb4&parseTime=true'; go test -tags integration ./internal/data/... -count=1 -v
   ```
   通过标准:两次都没有 Skip,全部 PASS。
6. **-migrate 入口**:
   - 执行 `go run . -f etc/guild.yaml -migrate; $LASTEXITCODE`:退出码 0,输出 4 条 `CREATE TABLE IF NOT EXISTS`,没有 `warning:` 行。
   - 再执行一次:退出码 0,输出 `0 statement(s)`。
   - 执行 `docker --context desktop-linux exec mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -e "SHOW TABLES FROM mmorpg_guild; SHOW CREATE TABLE mmorpg_guild.guild\G"'`:应有 5 张表(4 张业务表 + schema_migrations);建表语句含 ``UNIQUE KEY `uk_guild` (`name_norm`(191))``、`` `funds` bigint unsigned``。完整输出保留为证据。
7. **friend**,在 `go/friend` 下执行:`go build ./...; go vet ./...; go test ./... -count=1`;然后设置 `$env:FRIEND_TEST_MYSQL_DSN='appuser:apppass123@tcp(127.0.0.1:3306)/friend_test?parseTime=true&charset=utf8mb4'`,执行 `go test ./internal/data/ -count=1 -v -run 'TestAcceptFriend_ConcurrentHardLimit|TestFriendCapacityMissingRowUsesAuthoritativeCount'`。
8. **静态检查(B1)**:
   - `rg -n "guild_schema_migration|MigrateLegacyRankScores|friend_capacity_backfill" go deploy` → 0 条。
   - `rg -n "uk_name|uk_player|guild_friend_tables" go/guild` → 0 条。
   - `rg -n "Timeout: 10000|Timeout: 3000" go/guild/etc` → 0 条。
   - `rg -n "(CreateTimeMs|JoinTimeMs|LastActiveMs) +int64" go/guild/internal/data` → 0 条。
9. **本地端到端**:按 §19 切换后执行 `pwsh tools/scripts/start_game.ps1`。
   - guild 日志含 `schemamigrate up 完成: database=mmorpg_guild`。重启 guild 一次,日志为 `statements=0`。
   - 用 `go_services.ps1` 同时起 zone 1 / zone 2 的 guild:都能启动;如果出现 `迁移锁忙`,随后要能成功。
   - 跑 §16.4 的 robot guild-smoke,期望通过。

**B1b 验证**(B1 合入后):
- `tools/merge_zone`:`go build ./...; go vet ./...; go test ./... -count=1; go test -tags merge_integration ./... -count=1`(集成测试要求 Redis DB 9-12 为空,否则 Skip;Skip 须如实报告)。
- `tools/data_consistency_check`:`go build ./...; go vet ./...`。
- `rg -n "uk_name|uk_player|guild_friend_tables" tools/merge_zone tools/data_consistency_check` → 0 条。

**需保留的证据**:每一步的命令输出摘要;第 0 步的 Dir、h1 与 rg 结果;第 6 步 `SHOW CREATE TABLE` 全文;guild 启动日志中 schemamigrate 相关段落;失败时的完整错误。

## 19. 本地切换步骤(未上线,不搬数据)

1. 停全部服务(`pwsh tools/scripts/dev_tools.ps1 -Command go-svc-stop`,再停 C++ 节点)。
2. 执行 §18 第 4 步的建库授权。
3. 清空 guild Redis:`docker --context desktop-linux exec redis redis-cli -n 2 FLUSHDB`。
4. 执行 `start_game.ps1`,guild 启动时自动建表。
5. **B2 验收之后**,用 root 清理旧表:`DROP TABLE IF EXISTS mmorpg.guild_member, mmorpg.guild, mmorpg.guild_schema_migration;`。执行前确认 friend 已是 B1 之后的二进制。
6. 以后需要删库重建(§6.4 规则 5)时,重复第 1–4 步,并在第 2 步之前执行 `DROP DATABASE mmorpg_guild;`。

## 20. D-14 修订正文(替换 `xuanming-port-decisions-20260910.md` 对应位置,随 B1 提交)

- **§5 末句**改为:"`deploy/mysql-init` 从本条起**禁止新增业务表**。存量 `gateway_tables.sql` 保留不动;`guild_friend_tables.sql` 只保留 friend 三表(见 §8 修订)。"
- **§7 追加一条**:"replace 只在主模块生效。`go/schemamigrate` 若以 module 路径 replace 解析 proto2mysql(例如 `=> github.com/luyuan-cpp/proto2mysql v0.1.1`),依赖它的建表服务 module(trade、guild)必须写逐字相同的 replace,并让 go.sum 的 h1 与 schemamigrate 一致;schemamigrate 改为 require 正式 tag 后同步删除。这不属于'replace 到仓库外目录'。"
- **§8** 整条替换为:

> 8. **存量不动,guild 除外**(2026-09-16 修订,帮会二期 B1,用户决策)
>    - friend 表与 gateway 的 `zone_config` 表留在 `mmorpg`,不变量按 D-10 保留。
>    - **guild 例外**:项目未上线、无存量数据,`guild` / `guild_member` 与帮会二期新表迁入独占库 `mmorpg_guild`,以 `proto/guild/guild_db.proto` 为源,由 `go/schemamigrate` 迁移(`-migrate` + dev 档 AutoMigrate,照 trade);后续表由各批次追加。
>    - `guild_friend_tables.sql` 删除帮会表与遗留迁移过程;`guild_schema_migration` 门表、`MigrateLegacyRankScores`、friend 的 `friend_capacity_backfill_v1` 门一并删除。friend 缺容量行时按权威边重算。
>    - 帮名唯一键改为 `uk_guild(name_norm)`,规范化在 Go 侧完成,不依赖排序规则;tools/merge_zone 与 data_consistency_check 经 `-guild-schema`(默认 `mmorpg_guild`)限定库名。
>    - 开发期改表纪律(只追加;索引组只追加到末尾;缺普通索引即拒启)见 `guild-phase2.md` §6.4。
>    - §9 清单对 `mmorpg_guild` 的结论:带 zone 的行只有 `guild.zone_id`(merge_zone 步骤 3 已覆盖);biz_tag `guild` 已在 BootstrapTags,`guild_asset_op` 随 B5 加入;TiDB BR 按库恢复清单加入 `mmorpg_guild`。
>    - TiDB Phase 1 是逻辑库对逻辑库迁移,库名不改。data_service 与 go/db 本轮不改迁移路径。

- **"被推翻 / 修订的旧条目"** 追加:"本条 §8 原文'friend、guild……留在 `mmorpg`,`guild_schema_migration` 门表照旧':**修订**(2026-09-16,guild 迁 `mmorpg_guild`)。"
- **"遗留"** 中"guild 无 K8s ConfigMap / manifest"一条追加:"上线时一并补 `guild-migrate` Job(形状照 trade-migrate),staging/prod 固定 `Schema.AutoMigrate=false`。"

## 21. 契约偏差

1. **§3.2 表清单**:B1 只建 `guild`、`guild_player_state`、`guild_member`、`guild_application`。`guild_player_op_seq`、`guild_asset_op`、`guild_daily_counter`(B5)、`guild_activity_progress`(B6a)、`guild_trial_battle`(B6b)的列、枚举数值、索引由各批次在自己的 PR 中定义,schemamigrate 自动补建。原稿"B1 固化全部列、之后只能追加"的条款作废。`stream` 列用 uint32 还是枚举,由 B5 决定。
2. **§1 批次**:B1 加上 C++ 登记共 32 个手改文件,超过 30,拆为 B1(23)+ B1b(9,合服与一致性工具),B1b 紧随 B1。
3. **§3.2 guild UNIQUE**:从 `name` 改为 `name_norm`(NFKC → TrimSpace → 小写,≤48 rune)。公式与 §3.4 相同,但暂时是 guild 自带的实现(`data.GuildNameNorm`);B3a 的 `go/shared/playername` 合入后,可抽出公共 `Key` 函数合并。
4. **新表 `guild_player_state`**(契约无):每玩家一行,只作串行化锁行,不存待审计数(理由见 §5 message 注释)。B2 的 ApplyJoinGuild 必须按 §2.1 规则 3/4 使用它。
5. **§2 zrpc 预算**:除 `Timeout ≤ 4000` 外,另加 `DataServiceRpc.Timeout ∈ [500,3000]`(yaml 取 2000)、`DataServiceRpc.Timeout + 1500 + 500 ≤ Timeout`,以及整请求预算拦截器(`Timeout − 500ms`)。原稿的下限 2000 被这条组合约束取代。
6. **时间戳**:repo 结构体改为 uint64(契约 §2);`guild.proto` 的 `create_time_ms`/`join_time_ms`/`last_active_ms` 仍是 int64。建议 B2 随客户端重生成一并改为 uint64(契约 §3.1 没有列出这项)。
7. **`guild.proto` 字段扩展**(contribution 拆分、funds 等)放 B2,与客户端重生成同批;B1 期间 `GuildMember.contribution=5` 展示 `contribution_total`。
8. **枚举值命名**:`GuildChangeKind` 与本文件同在包 `guildpb`,B2 应使用 `GUILD_CHANGE_KIND_MEMBER_JOINED` 这类带前缀的名字。
9. **merge_zone 与 DSN**:沿用 trade 的"同一 DSN + `-guild-schema`"写法,不另开 DSN flag;只有帮会库部署到另一个 MySQL 实例时才需要。
10. **账号**:guild.yaml 从 root 改为 appuser,用来实际检验 §3 的授权;代价是 `rank_zone_integration_test` 必须显式传 root DSN。

**交接给其他分节**:
- **S2**:`guild_application` 的索引名是 `idx_guild_application_0`(player_id)和 `_1`(expire_ms),不是 `_1/_2`;s2_management_part4 第 43 行的 `… WHERE player_id = ? FOR UPDATE` 在 TiDB 上串行化不了并发插入,需要改为先 `INSERT IGNORE guild_player_state`,事务内锁该行后再做加锁计数;审批事务按 §2.1 位置先锁申请人的 `guild_player_state`,再插 `guild_member`;1213 重试由 B2 落地 `withTxRetry`。
- **S5**:s5_economy_part2 §5.6 中"S1 定下的 GuildAssetOpRecord"不再存在,改由 B5 写出完整 message(包括 `guild_player_op_seq`、`guild_daily_counter`),状态数值从 0 开始可以直接定;worker 按 §2.1 规则 6 编写。
- **S6**:`guild_activity_progress` 与 `guild_trial_battle` 由 B6a / B6b 自行定义。

## 22. 风险与未验证项

- proto2mysql 当前靠 trade 会话**未提交**的 replace 才能拿到 191 前缀和 TiDB 选项;B1 不能先于它提交(§18 第 0 步)。正式 tag 需要人来打(AGENTS §9)。
- 上游 7dbda68 一旦进入 tag 并被 schemamigrate 采用,现有 MEDIUMTEXT 唯一键会被判 `ErrLegacyKeyColumn`,guild 拒绝启动;未上线期间按 §6.4 删库重建,上线后必须先完成迁移设计。
- `utf8mb4_unicode_ci` 会把 U+10000 以上字符全部视为相等,含 emoji 等字符的帮名会误判重名(只误拒不漏判),等 proto2mysql 升级后消失。
- GET_LOCK 与 KILL QUERY 在 TiDB 上的语义尚未验证(`schemamigrate.go:75-80`);§2.1 规则 3 关于 TiDB 锁不存在键的描述,来自 TiDB 悲观事务的文档语义,未在本项目的 TiDB v8.5.2 集群实测。
- Go 1.26.5 工具链是否可用取决于 Codex 环境;CI(`go-modules-ci.yml`)检出时必须包含 `go/schemamigrate`,并能访问替换源。
- 本文行号在并行会话持续改动下会漂移,落码时以函数名为准。

---

## 附录:对抗评审处理记录

# S1 存储 — 对抗评审处理记录(2026-09-16)

每条先对照代码复核,再决定采纳或驳回。修订后的设计分 7 个部分:`s1_storage_part1.md`–`s1_storage_part7.md`。原来的第 5 部分拆成了第 6(测试、文件清单)、第 7(验证、D-14、偏差)两部分。复核只读了代码,没有运行任何构建、测试或数据库命令。

| # | 级别 | 问题 | 结论 | 复核与落点 |
|---|---|---|---|---|
| 1 | blocker | proto2mysql v0.1.1 实际解析到已撤回的旧版 | **已采纳**(修复方式有调整) | 复核属实:HEAD 的 `go/schemamigrate/go.sum` 是 `h1:s5du1n…`,缓存里那份 `.info` 时间 2026-07-29,go.mod 带 retract,没有 191 前缀,也没有 TiDB 选项;trade 会话未提交的 go.mod 里,schemamigrate 与 go/trade 都加了 `=> github.com/luyuan-cpp/proto2mysql v0.1.1`(= e90a5f0,含 `TextIndexPrefixLength`/`SHARD_ROW_ID_BITS`)。落点:§0 列为硬前置;§1 第 1–3 行证据改为 go.sum 哈希加缓存内容;§7.1 规定 go/guild 的解析方式与 schemamigrate 已提交的 go.mod **逐字一致**(有 replace 就照抄,改成正式 tag 就不写),go.sum 的 h1 必须相同;§18 第 0 步用 `go list -m -json` / `go mod download -json` / rg 核对,第 2 步比对两边 go.sum;§20 在 D-14 §7 写明"消费方复制 replace 不算 replace 到仓库外目录"。**没有采纳**"在此之前任何 string 列都不得进索引":硬前置已保证解析到含 191 前缀的版本,前置不满足就整批不开工,不需要再单独限制 string 列 |
| 2 | major | 帮名唯一性依赖上游正在改的排序规则 | **已采纳**(两处细节不同) | 复核属实(7dbda68 提交说明)。落点:`name_norm` 列承担 `OptionUniqueKey`,`name` 不建索引(§5、§6.3);`data.GuildNameNorm` = NFKC → TrimSpace → 小写;logic 先做校验(§8.2);集成测试覆盖大小写、全角、尾空格三种撞名(§16.1);merge_zone 的重名探测改为比较 `name_norm`(§10)。不同之处:①限长定为 **48 rune**,不是评审写的"48 字节":24 个汉字已是 72 字节,按字节限会把合法帮名拒掉;②函数暂时放在 go/guild 的 data 包,没有放 go/shared:B1 不依赖 B3a,帮名规则(24 rune、允许符号)也和角色名规则不同;公式与契约 §3.4 一致,等 B3a 合入后再抽公共函数(§21 偏差 3)。另外在 §6.3、§22 补记:unicode_ci 会把 U+10000 以上字符判为相等,只会误拒、不会漏判 |
| 3 | major | B1 提前固化了 B5/B6 的表 | **已采纳**(保留 guild_application) | 复核属实:S5 §5.6 要重排状态值并加租约列,S4 偏差 7 也提了不同形状;`schemamigrate.go:279` 与 `plan.go:306` 确实会补建后加的表。落点:B1 只建 `guild`、`guild_player_state`、`guild_member`、`guild_application`(§0、§5、§7.2);删除原偏差 4 的冻结条款(§21 偏差 1);测试数量按当前清单断言(4 张表)。`guild_application` 留在 B1,原因是 S2 part4 第 7 行已按"B1 已建"设计,形状也一致;只是 S2 写的索引名 `_1/_2` 有误,已写进 §21 的交接 |
| 4 | major | §6.4 对 schemamigrate 检测能力的描述不对 | **已采纳** | 复核属实:`plan.go:330` 多余列只进 warning;`:344-356` 索引只按名字比;`:346` 的缺索引也是 warning;`schemamigrate.go:160-161` 的 `Clean()` 忽略 warning。落点:§6.4 按实际行为重写成"能自动做 / 报需人工 / 只报 warning / 检测不到"四档,并加 6 条硬规则(索引组只能追加到末尾、枚举值不重排等);§7.4 `ensureSchema` 在 Up 和 Plan 两种模式下遇到 `缺索引` 都拒绝启动;§16 按 `INDEX_NAME, SEQ_IN_INDEX` 逐行比对 STATISTICS,另有真库用例守住 `缺索引` 这个前缀 |
| 5 | major | 锁序规则自相矛盾,也不覆盖 TiDB 和 worker | **已采纳**(有一处修正) | 复核属实:原文"先操作者再目标"与"按 player_id 升序"冲突。落点:§2.1 重写为 6 条规则:表间顺序(INSERT 也算加锁);同表多行按主键升序;禁止"加锁读查不到再插入",唯一性只靠 1062;判定必须用加锁读;1213 整个事务重试 1 次(`withTxRetry` 由 B2 落地);worker 先无锁读 → BEGIN → guild → guild_member → `guild_asset_op FOR UPDATE` → 复查。修正:评审说 TiDB 上这类检查"根本起不到串行化作用",不够准确。TiDB 悲观事务对唯一键点查不存在的行会锁住这个键,真正锁不住的是范围读。§2.1 规则 3 按这个区分来写,TiDB 实测列为未验证项(§22) |
| 6 | major | 每人待审申请上限没有可加锁的行 | **已采纳**(实现方式不同) | 复核属实:S2 part4 第 43 行用 `WHERE player_id=? FOR UPDATE` 做范围加锁读,TiDB 上会超额,MySQL RR 上会死锁。落点:B1 新建 `guild_player_state`(player_id 主键),事务外先 `INSERT IGNORE` 建行,事务内按位置 2 锁住,再对 `guild_application` 做加锁计数(§2.1 规则 3/4、§5)。与评审建议不同:①**不存** `pending_application_count`。S2 的帮会侧批量删过期申请(part4 第 42 行)和解散删申请(第 119 行)都不锁申请人的行,计数会漂;只做锁行加计数,就不需要让这些路径去锁每个申请人。②表放在锁序位置 2(guild_member 之前),不是评审说的 guild_member 之后。审批的顺序是"锁申请人状态行 → 插入申请人成员行",放在后面会让审批逆序。③表由 B1 建,不交 B2,和 guild_application 一起;B2 必须照做,已写进 §21 交接 |
| 7 | major | 把 Timeout 降到 4000 却没核算单次请求内的耗时 | **已采纳**(实现方式不同) | 复核属实:`home_zone.go:28` 1500ms;`guild.yaml:68` DataServiceRpc 3000;`idsegment.Next(ctx)` 会等请求 ctx,`FetchTimeout` 默认 3s。落点:§7.3 的 `Validate` 要求 `DataServiceRpc.Timeout ∈ [500,3000]`,并且 `DataServiceRpc.Timeout + 1500 + 500 ≤ Timeout`;yaml 改为 2000;`RequestBudget() = Timeout − 500ms`;§16 补边界用例(含 3000/4000 必须报错)。不同之处:预算用 guild.go 里的拦截器 `requestBudgetInterceptor` 统一加在最内层(§7.4),没有照 trade 在每个 logic 方法入口各套一次。guild 的 logic 不持有 config,拦截器只改一个文件,还能用链测试守住。`HomeZoneLookupBudgetMs` 在 config 里镜像 logic 常量,由 `TestHomeZoneBudgetMirrorsLogic` 保证一致。原来的下限 `MinRpcTimeoutMs=2000` 被组合约束取代 |
| 8 | minor | 新增的 C++ 生成物没登记,文件清单超 30 | **已采纳**(采用方案 A,但算数另行修正) | 复核属实:`cpp/generated/proto/guild/guild.pb.cc` 存在;trade_table 在 CMakeLists:122、vcxproj:122,233、filters:294,611 都有登记。落点:§17.1 第 21–23 项登记 `guild_db.pb.cc/.h`;§18 第 3 步串行跑 MSBuild。评审说"把 D-14 移到文档批就能 ≤30",这个算数不对:原 29 − 1(D-14)+ 3(C++)= 31,再加上为清理 `uk_name` 注释必须改的 `constants.go`,共 32。所以拆成 B1(23)+ B1b(9,merge_zone 与 data_consistency_check),并规定 B1 与 B1b 之间不得跑真实合服(§0、§15、§21 偏差 2)。D-14 修订是文档,不占名额,随 B1 提交 |
| 9 | minor | `Validate` 遮蔽了嵌入的 `RpcServerConf.Validate` | **已采纳** | 复核属实(go-zero v1.9.2 `zrpc/config.go:82-88`)。落点:§7.3 第一行先调 `c.RpcServerConf.Validate()`;§16 加 Auth=true 且缺 Redis 必须报错的用例 |
| 10 | minor | 测试助手只挡正式库名,DSN 指错会误删别的库 | **已采纳** | 复核属实:appuser 对 testdb、zone_N_db、mmorpg_trade 都有 ALL 权限。落点:§16.1 改为白名单 `^(guild_test|guild_it_\d+_\d+)$`,不匹配就 `t.Fatalf`,并加 `TestGuildTestDBNamePattern` |
| 11 | minor | 本地多开时首次建表互相等锁 | **已采纳** | 复核属实:`go_services.ps1:155` 允许多开,`schemamigrate.go:98` 等锁 10s。落点:§7.4 `runWithLockRetry` 最多试 3 次、间隔 2s,且感知 ctx;`-migrate` 模式不在进程内重试(退出码 3 交给 Job 的 backoff);§16 加"锁忙两次后成功"和"三次都忙"的用例;§15 更新恢复方式 |
| 12 | minor | §18 第 9 步的静态检查会失败 | **已采纳** | 复核属实,而且比评审列的还多:`guild_repo.go:30,32,596,600-601`、`constants.go:44`、`guild_logic.go:60`、`guild_repo_zone_test.go:27-33,100`、`guild_repo_test.go:159,169`、`rank_zone_integration_test.go:116,126`;B1b 另有 `guild_step.go:7-8,12,82-83`、`integration_test.go:133,137`、`audit_resources.go:445`。落点:§1 第 17 行列全;§8、§10、§16 逐条给出改写;§18 的 rg 规则按 B1 / B1b 拆开,都包含 `uk_player` 和 `guild_friend_tables` |
| 13 | minor | 并行会话状态的描述已过时 | **已采纳** | 复核属实(2026-09-16 的 git status)。落点:§1 第 24 行、§17.4 如实记录,并要求落码前重跑;§18 第 0 步要求 schemamigrate 的 go.mod/go.sum 已提交且 h1 一致;另外发现 `cpp/generated/proto/guild/guild.grpc.pb.h` 有他人改动,§18 第 1 步要求核对 |
| 14 | minor | 时间戳类型与 AGENTS 规定不一致 | **已采纳**(只改 repo 层) | 复核属实(`guild_repo.go:50,62-63`)。落点:§8.1 `GuildData/MemberData` 改为 uint64;§8.2 logic 用 `uint64(time.Now().UnixMilli())`,在 proto 边界转回 int64。`guild.proto` 的三个字段 B1 不动(客户端协议归 B2),建议 B2 一并改成 uint64(§21 偏差 6);§18 加 rg 检查 |

**驳回**:无。14 条全部属实;其中 1、2、5、6、7、8 条的修复细节与评审建议不同,理由见上表。

**给其他分节的连带影响**(已写进 §21 交接):S2 要改申请事务(使用 `guild_player_state`、索引名、审批锁序);S5 需要自己写出 `guild_asset_op`、`guild_player_op_seq`、`guild_daily_counter` 的完整 message;S6 需要自己定义活动两表。
