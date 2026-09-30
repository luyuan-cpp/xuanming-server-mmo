package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	dbpb "proto/common/database"

	"github.com/luyuancpp/proto2mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 全局库(SnapshotMySQL)的表结构只有一个真源:
// proto/common/database/rollback_database_table.proto。建表 / 补列走
// proto2mysql.CreateOrUpdateTable,与 go/db 的 zone 库同一套机制。
//
// 本包**不再**手写任何 CREATE TABLE。历史上 snapshot_store / transaction_log_store
// 各自手写 DDL,已经与 proto 漂移过三次(rollback_audit_log 多一列、transaction_log
// 列名不同、player_snapshot 在 zone 库与全局库各有一份)。手写 DDL 的例外共三处,
// 各自的理由写在对应的常量注释里:
//   - bootstrapTables 清单里的两张表(id_segment、player_name):整表预建,之后不再交给
//     proto2mysql 同步,只做列 / 唯一键的形状守卫。
//   - nullableUniqueKeys 清单里的可空唯一列(player_snapshot.snapshot_guid_nz +
//     uk_snapshot_guid_nz):表仍全程由 proto 驱动,只在同步之后**追加**一列 + 一个键。

// IdSegmentTableName 号段表名(与 proto OptionTableName 一致)。
const IdSegmentTableName = "id_segment"

// PlayerNameTableName 玩家名字注册表名(与 proto OptionTableName 一致)。
//
// 这张表是"名字全服唯一"的唯一真源(设计 docs/design/guild-phase2/03-names.md §3.0):
// zone 库 player_database.profile_component 与账号里的 AccountSimplePlayer.name 都只是
// 只读副本,可能缺失,读侧一律回源 BatchGetPlayerName。所以它只能放全局库一处 ——
// 每个 zone 一份就退化成"区内唯一",跨区必然重名。
const PlayerNameTableName = "player_name"

// idSegmentBootstrapDDL 是本包两段手写 DDL 之一(另一段是 playerNameBootstrapDDL)。
//
// 原因:proto2mysql v0.1.0 把 proto string 一律渲染成 MEDIUMTEXT,而 TEXT 列不能做
// 无前缀主键(MySQL 错误 1170);id_segment 的主键 biz_tag 恰是 string。于是按设计
// §6.2 用 VARCHAR(64) 预建,在 CreateOrUpdateTable 之前执行 —— 与
// go/db/model/mysql_database_table.sql 预建 user_accounts 是同一个模式。
//
// 列注释 pb:N 与 proto2mysql 生成列的格式一致,让日后的迁移工具仍能按字段号识别列。
// CHECK (max_id < 2^55) 是值域上限的最后一道闸(设计 §6.3:号段与存量 snowflake 号
// 永不相交);应用层在 UPDATE 之前就拒绝,CHECK 只兜底手工 SQL。TiDB ≥ 7.2 才校验
// CHECK,更早版本解析后忽略,不影响建表。
//
// 预建之后**不能**再对本表跑 CreateOrUpdateTable:proto2mysql 会把 VARCHAR(64) 判成
// 与 MEDIUMTEXT 不兼容而 MODIFY COLUMN,同样撞 1170。所以 migrateSchemaOn 对
// id_segment 只做"proto 每个字段都有同名列"的漂移检查,不做同步。
const idSegmentBootstrapDDL = "CREATE TABLE IF NOT EXISTS `id_segment` (\n" +
	"  `biz_tag` VARCHAR(64) NOT NULL COMMENT 'pb:1',\n" +
	"  `max_id` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:2',\n" +
	"  `step` int unsigned NOT NULL DEFAULT 0 COMMENT 'pb:3',\n" +
	"  `version` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:4',\n" +
	"  PRIMARY KEY (`biz_tag`) /*T![clustered_index] NONCLUSTERED */,\n" +
	"  CONSTRAINT `chk_id_segment_max_id_cap` CHECK (`max_id` < 36028797018963968)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci" +
	" /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */ COMMENT='id_segment';"

// PlayerNameOwnerUniqueKey 是 player_name 上建在 player_id 的唯一键名。
//
// 新结构里 name_norm 是**聚簇主键**(根治审计 #12,见 playerNameKeyedByNameDDL),player_id 退成
// 这条二级唯一键。它承担的不变量是"一个 player_id 只登记一个名字":Reserve 的 ODKU 靠它把
// "这个 player_id 已经有别的名字"变成受影响 0 行(进而判成 ReserveConflict),
// ownedNormOf 也靠它做点查。键没了 = 同一个 player_id 能占多个名字,且 Reserve 会当成插入成功。
//
// 名字集中在这里(与 SnapshotGuidUniqueKey 同理):建表 DDL、迁移与真库用例引用同一个常量。
const PlayerNameOwnerUniqueKey = "uk_player_name_owner"

// playerNameBootstrapDDL 是第二段手写 DDL,成因与 id_segment 同源,但后果更硬。
//
// proto2mysql 把 proto string 一律渲染成 MEDIUMTEXT,而 player_name 的主键恰好建在
// string 列 name_norm 上 —— TEXT 列上建无前缀主键 / UNIQUE KEY 会被 MySQL 以 1170 拒掉。
// "名字全服唯一"这条不变量**只能**由库上的唯一约束兜住:应用层"先查后插"在并发下必漏
// (两个建角请求同时查到"没人占",然后各插一行),所以这张表必须预建成 VARCHAR + 唯一约束,
// 不能交给 CreateOrUpdateTable。
//
// # 为什么主键是 name_norm 而不是 player_id(2026-09-29 用户拍板的根治,审计 #12)
//
// 被争抢的键是"名字"。名字如果只是**二级**唯一索引,Reserve 的重复检查就发生在二级索引上,
// 而二级索引上的插入要申请插入意向锁、要检查后继记录 —— 于是"新记录的主键小于删除标记项原主人"
// 时必然成环(号段发的 player_id 都 < 2^55,存量角色是更大的 snowflake id,新号抢老角色释放的名字
// 正是这一路,是生产常态而不是罕见交错)。
//
// 2026-09-29 在本机真 MySQL 26.7.0 上跑的探针给出的是实测数据,不是推演
// (全局 REPEATABLE-READ、binlog ROW、innodb_deadlock_detect=ON,每格重复 5 次):
//
//	表结构                                    写法          竞争者都小于原主人  都大于  一大一小
//	PRIMARY KEY(player_id) + UNIQUE(name_norm)  普通 INSERT   5/5 死锁          2/5    5/5
//	PRIMARY KEY(player_id) + UNIQUE(name_norm)  ODKU          5/5 死锁          0/5    2/5
//	PRIMARY KEY(name_norm) + UNIQUE(player_id)  普通 INSERT   5/5 死锁          5/5    5/5
//	PRIMARY KEY(name_norm) + UNIQUE(player_id)  **ODKU**      **0/5**           0/5    0/5
//
// 也就是说:**"名字当聚簇主键" + "ODKU" 两个条件缺一不可**,合起来才是 0/15 全清;
// 只改写法(旧结构 + ODKU)在生产常态那一路仍然 5/5 成环。
//
// 原理:聚簇索引上撞同键的删除标记记录走的是 row_ins_must_modify_rec → 原地改写
// (row_ins_clust_index_entry_by_modify),取的是 LOCK_X + LOCK_REC_NOT_GAP(**不带间隙**),
// **根本不申请插入意向锁** —— 没有插入意向锁,就没有"插入意向锁被对方排队中的请求挡住"这条边。
//
// # 代价(记在这里,免得下次被当成"这么改不划算")
//
//   - 聚簇主键宽了:VARCHAR(191) utf8mb4_bin 最长 764 字节(DYNAMIC 行格式的索引键上限 3072,够用),
//     每条二级索引项都要带上主键值,所以 uk_player_name_owner 的项是 (player_id, name_norm)。
//     本表一个角色一行、只有一条二级索引,量级可忽略。
//   - 按 player_id 读(BatchGet / ownedNormOf)从聚簇点查变成"二级唯一索引点查 + 回表",多一次页访问;
//     按名字读(ownerOfNorm)反过来从二级索引点查变成聚簇点查,省一次回表。两者都还是点查。
//   - TiDB 上 `/*T![clustered_index] NONCLUSTERED */` 让主键仍是二级索引形态(要配 SHARD_ROW_ID_BITS),
//     所以"名字是聚簇键"这个前提在 TiDB 上**不成立**;但 TiDB 没有间隙锁与插入意向锁,上面那三类形状
//     按官方文档本来就不存在(**未在 TiDB 上实测**)。迁移在 TiDB 上跳过,见 ensurePlayerNameKeyedByName。
//
// COLLATE=utf8mb4_bin 见下;主键建在 name_norm 上之后,它同时决定"库认为哪两个名字是同一个主键"。
//
// COLLATE=utf8mb4_bin 是刻意的:NFKC 归一与大小写折叠全部在 Go 侧完成(go/shared/playername
// 的 Normalize),结果落进 name_norm 列,库只做逐字节比较。若改成 utf8mb4_unicode_ci,库会把
// 一批 Go 认为不同的字符判等,于是出现"库说撞名、Go 说不撞"的死结:玩家换成一个在规则包看来
// 毫不相干的名字仍被 1062 拒绝,而服务端给不出任何能解释给玩家听的理由。
//
// 列宽:name VARCHAR(64)、name_norm VARCHAR(191)。MySQL 的 VARCHAR(N) 按**字符**计而非字节,
// 两者都远超规则包的结构上限 playername.StructuralMaxRunes(32 个码点)。191 是 utf8mb4 时代
// 767 字节索引上限留下的安全长度,唯一键直接覆盖整列,不需要写前缀索引。
//
// 与 id_segment 同理:预建之后**不能**再对本表跑 CreateOrUpdateTable(proto2mysql 会把
// VARCHAR 判成与 MEDIUMTEXT 不兼容而 MODIFY COLUMN,重新撞 1170),所以 migrateSchemaOn
// 对它只做"proto 每个字段都有同名列"的漂移检查。列注释 pb:N 与 proto2mysql 生成列同格式,
// 日后的迁移工具仍能按字段号识别列。
//
// 改了 rollback_database_table.proto 里的 message player_name,就必须同步改这里,
// 否则启动期的 assertColumnsPresent 会直接拒绝迁移(这正是它存在的意义)。
//
// 列的顺序 / 类型 / 注释与改主键之前逐字相同(只动了键),这样存量表跑完迁移之后
// `SHOW CREATE TABLE` 与新建库一致,"二次迁移零变更"的回归才有意义。
const playerNameBootstrapDDL = "CREATE TABLE IF NOT EXISTS `player_name` (\n" +
	"  `player_id` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:1',\n" +
	"  `name` VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'pb:2',\n" +
	"  `name_norm` VARCHAR(191) NOT NULL COMMENT 'pb:3',\n" +
	"  `created_ms` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:4',\n" +
	"  PRIMARY KEY (`name_norm`) /*T![clustered_index] NONCLUSTERED */,\n" +
	"  UNIQUE KEY `" + PlayerNameOwnerUniqueKey + "` (`player_id`)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin" +
	" /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */ COMMENT='player_name';"

// bootstrapTables 是走手写 DDL 预建、**不**交给 proto2mysql 同步的表:表名 → 建表语句。
//
// 迁移时先逐条执行(全部 CREATE TABLE IF NOT EXISTS,幂等),之后的同步循环遇到清单内的表
// 只做列漂移检查。再加一张这类表 = 在这里加一行,不要回到 migrateSchemaOn 里加 if 分支;
// 清单化是为了让"哪些表不由 proto 驱动建"这件事只有一处答案。
var bootstrapTables = map[string]string{
	IdSegmentTableName:  idSegmentBootstrapDDL,
	PlayerNameTableName: playerNameBootstrapDDL,
}

// isBootstrapTable 判断一张表是否由 bootstrapTables 预建。
// 迁移路径与 DDL 契约测试共用这一个判据,免得两处各抄一份表名清单再各自漂移。
func isBootstrapTable(tableName string) bool {
	_, ok := bootstrapTables[tableName]
	return ok
}

// bootstrapUniqueColumns 是 bootstrap 表上"必须存在单列唯一键"的列:表名 → 列名清单。
//
// 为什么需要这张表:bootstrap 路径是 `CREATE TABLE IF NOT EXISTS`,表已存在就整条语句跳过;
// 迁移对 bootstrap 表又只跑 assertColumnsPresent(只比**列名**)。两者叠加的结果是,一张
// 形状不对的既有 player_name(手工建的、从别处导入的、被 `ALTER TABLE ... DROP INDEX` 改过的)
// 能一路静默通过启动检查 —— 而 player_name 的唯一键**就是**"名字全服唯一"这条不变量的全部
// 实现(store 层刻意不做"先查后插",见 player_name_store.go 文件头)。索引没了 = 不变量整条
// 失效,且并发建角重名时零报错,只有玩家互相看到同名才会被发现。
//
// 判据是"**恰好只有这一列、且不带前缀长度**的唯一索引",不是"这一列排在某条唯一索引的第一位":
// UNIQUE KEY (name_norm, player_id) 的 name_norm 也在 SEQ_IN_INDEX=1,但它只保证
// **组合**唯一 —— 每个 player_id 都能再占一次同一个名字,不变量照样是破的。
// PRIMARY KEY 同样满足(NON_UNIQUE=0),单列主键建在 name_norm 上一样能兜住唯一性,故一并接受。
//
// id_segment 刻意没进这张表:它的唯一性由主键 biz_tag 承担,而主键缺失会让**写入**直接出错
// (不是静默),不存在 player_name 这种"坏了也没人知道"的形态;给它加一条只会让一批存量 dev 库
// 在启动期 fail-closed,代价大于收益。真要加时在这里加一行即可。
//
// 两列都要:这条守卫刻意**不区分**新旧主键形态(新结构 PRIMARY(name_norm) + UNIQUE(player_id),
// 旧结构 PRIMARY(player_id) + UNIQUE(name_norm) —— 两种形态在这条判据上都通过)。
// 哪一种形态、要不要迁移由 ensurePlayerNameKeyedByName 负责;本条只回答"两条唯一性还在不在",
// 因为它们各自都是一条业务不变量的全部实现:name_norm 是"名字全服唯一",
// player_id 是"一个角色只登记一个名字"(没有它,Reserve 的 ID 复用检测会静默失效)。
var bootstrapUniqueColumns = map[string][]string{
	PlayerNameTableName: {"name_norm", "player_id"},
}

// ── player_name 主键迁移:player_id → name_norm(审计 #12 的根治)────────────────────
//
// 目标形态见 playerNameBootstrapDDL;为什么必须改成"名字当聚簇主键"见那里的实测表格。
// 本段只解决一件事:**存量库上那张 PRIMARY KEY(player_id) 的表,怎么安全地换成目标形态**。
//
// # 做法:一条 ALTER 同时换主键与唯一键,不走"建新表 → 拷数据 → 改名替换"
//
// 影子表 + RENAME 的那条路要求**停写**:拷贝开始到改名之间落进旧表的行会被整段丢掉
// (login 的建角刚占到的名字会凭空消失,而 login 那边已经按成功继续建角了)。
// 单条 ALTER 没有这个窗口 —— MySQL 8.0 手册 "Online DDL Operations" 的 Table 17.17
// (https://dev.mysql.com/doc/refman/8.0/en/innodb-online-ddl-operations.html)对
// "Dropping a primary key and adding another"给出的是 In Place = Yes、Rebuilds Table = Yes、
// **Permits Concurrent DML = Yes**:执行期间的写入进 online row log,在提交阶段被重放,不丢。
// (同表的 "Dropping a primary key" 单独一条是 In Place = No —— 所以两个动作**必须写在同一条
// ALTER 里**,拆成两条会退化成 COPY 且中间有一段没有主键的窗口。)
//
// 刻意**不写** `ALGORITHM=INPLACE, LOCK=NONE`:同一张表里手册还写着"Adding a primary key using
// ALGORITHM=INPLACE is only permitted when the SQL_MODE setting includes strict_trans_tables or
// strict_all_tables flags"。显式钉死算法时,一个 sql_mode 不含 strict 的实例上迁移会直接失败,
// 而失败的代价是整条 MigrateSchema 失败 → AutoMigrate 路径把 store 全置 nil → 业务瘫掉。
// 不钉死时服务端自行退回 ALGORITHM=COPY:**语义仍然正确**(COPY 默认 LOCK=SHARED,读可以、写被
// 阻塞到 ALTER 结束,阻塞不是丢失),只是这段时间建角会排队。两种算法下都不需要停写,
// 区别只是"写入被排队"还是"写入照常"。**本仓实例实际走的是哪一种,未在真库验证。**
//
// # 幂等 / 可重跑
//
// 每次都先读当前主键形态再决定做什么:已是 PRIMARY(name_norm) → 交给 convergePlayerNameKeyShape
// 把**其余两把键**收敛到目标形态(新建库与已迁移库在那里一条 DDL 都不发);
// 仍是 PRIMARY(player_id) → 执行这条 ALTER;两者都不是 → fail-closed 报错,不猜。
// ALTER 失败时 MySQL 8.0 的 DDL 是原子的(要么全成要么全不成,崩溃也不会留半张表),
// 所以"再跑一次 -migrate"总是安全的,不需要人工清理中间态。
//
// **"主键对了"不等于"形状对了"**:回滚只做了一半、或有人手工只执行了 ADD PRIMARY KEY(name_norm)
// 而没 DROP 旧的 uk_player_name 时,库里会留下两种中间态 ——
//  1. PRIMARY(name_norm) + **残留的** uk_player_name(name_norm):多出来的那条二级唯一索引会让写路径
//     在二级索引上多取一次锁(Reserve 插入新名字时要往它里面插项,那条路上有插入意向锁),
//     把改主键消掉的形状带回来,而且是静默的 —— 功能全对,只是又开始死锁;
//  2. PRIMARY(name_norm) + **缺** uk_player_name_owner:"一个角色只登记一个名字"没有实现了,
//     Reserve 的 ID 复用检测(ReserveConflict)与 ownedNormOf 一起静默失效。
//
// 这两种形态**都通不过**人工审视之外的任何一道既有守卫:assertUniqueColumns 只问"这一列上有没有
// 单列唯一约束"(第 1 种里 PRIMARY 就满足,第 2 种由它拦下但文案指不出缺的是哪条键),
// NewPlayerNameStore 也只看主键形态。所以收敛这两条是迁移的职责,不能留在"快路径 return nil"里。
//
// # 回滚(可执行,不是口号)
//
// 停掉 data_service(本服务 replicas=1 + Recreate,见 deploy/k8s/manifests/go-svc/data-service.yaml),
// 然后:
//
//	ALTER TABLE `player_name`
//	  DROP PRIMARY KEY,
//	  DROP INDEX `uk_player_name_owner`,
//	  ADD PRIMARY KEY (`player_id`),
//	  ADD UNIQUE KEY `uk_player_name` (`name_norm`);
//
// 一行数据都不会丢(两种形态的列完全一样,只是键换了位置),回滚之后本服务的新二进制会探到
// 旧形态并自动切回旧 Release 语句(见 NewPlayerNameStore),仍然可用,只是又回到"靠有界重试吸收
// 死锁"的形态。
//
// # 新旧版本并存窗口
//
// data_service 的部署不变量是 **replicas=1 + strategy: Recreate**,所以正常升级路径上
// **不存在**新旧二进制同时在线的窗口。两个方向各自的行为:
//   - **新二进制 + 旧表结构**(忘了跑 -migrate、或迁移在 TiDB 上被跳过):安全。
//     NewPlayerNameStore 探到旧主键形态,Release 自动改用带 FORCE INDEX (uk_player_name) 的旧语句,
//     取锁顺序与 Reserve 的 ODKU 仍然一致;死锁回到"生产常态那一路 5/5"的老形态,由有界重试兜住。
//     启动日志里会打一条 Error 指明"跑 -migrate 才能根治"。
//   - **旧二进制 + 新表结构**(跑完迁移又把镜像回滚了):**不安全,必须连 DDL 一起回滚**。
//     旧二进制的 Release 语句写死 `FORCE INDEX (uk_player_name)`,而那条索引在新结构上已经不存在,
//     于是每一条 Release 都以 ER_KEY_DOES_NOT_EXIST(1176)失败 —— 建角补偿释放全线失效,
//     孤儿名字只涨不消。**注意 Reserve 不受影响**(ODKU 不点索引名),所以不会重名、不会写坏数据,
//     症状是"名字占着不放"这一种,可以在回滚 DDL(或滚回新镜像)之后用运维口的
//     ReleasePlayerName(minCreatedMs=0)清掉。
//     运维口径:**回滚镜像之前先按上面那条 ALTER 回滚 DDL**。
//
// # 失败时的运维出路
//
//   - 报"主键形态既不是 name_norm 也不是 player_id":表被人工改过。先 `SHOW CREATE TABLE player_name`
//     看清现状,人工改成两种形态之一再重跑 -migrate;本步骤拒绝在看不懂的形状上下发 ALTER。
//   - 报"找不到 name_norm 上的单列唯一索引":这张表上"名字全服唯一"已经没有实现了(比它更严重)。
//     先查重 `SELECT name_norm, COUNT(*) FROM player_name GROUP BY name_norm HAVING COUNT(*) > 1`,
//     清掉重复行之后 `ALTER TABLE player_name ADD UNIQUE KEY uk_player_name (name_norm)` 再重跑。
//   - 主键已是 name_norm、但**残留了 name_norm 上的二级唯一键**或**缺 uk_player_name_owner**
//     (上面"主键对了不等于形状对了"那两种中间态,典型来源是回滚只做了一半):**不需要人工处置**,
//     convergePlayerNameKeyShape 会自动 DROP / ADD 收敛,且幂等。它补 uk_player_name_owner 时若以
//     1062 失败,说明库里已经有"同一个 player_id 占了多个名字"的行 —— 那是数据事故,按错误文案里的
//     查重语句先人工定夺留哪一行,不要靠迁移去猜。
//   - ALTER 本身超时 / 被 MDL 挡住:这条 ALTER 要重建整张表,启动期 AutoMigrate 只给 5 分钟
//     (svc.autoMigrateTimeout)。生产请用部署阶段的 `data_service -f <yaml> -migrate`(不设时限);
//     被长事务挡住 MDL 时先 `SHOW PROCESSLIST` 找到那个事务。ALTER 是原子的,超时重跑即可。
//   - **这条 ALTER 在本实例上跑不动**(表太大 / 窗口不够 / 反复被 MDL 挡住),而其余迁移
//     (另外四张表的列同步、可空唯一列、id_segment 地板校验)是无关且必要的:打开
//     MigrateOptions.AllowLegacyPlayerNamePrimaryKey,本步骤会打一条 Error 说明"本实例仍是旧形态、
//     名字抢注的环仍在、由有界重试兜住"然后 return nil,其余迁移照跑。与 TiDB 那一支同口径。
//     打开之后 NewPlayerNameStore 会探到旧形态、**自动退回**带 FORCE INDEX (uk_player_name) 的旧
//     Release 语句(这条链路本来就有,不必新写),锁序仍然一致,功能完全正确 —— 代价只是
//     "生产常态那一路仍然 5/5 成环、靠 1213 重试吸收"。这是一次有意的推迟,不是可忽略的告警。

// playerNamePKShape 是 player_name 当前的主键形态。
type playerNamePKShape int

const (
	// playerNamePKUnknown 主键不是我们认识的两种形态之一(被人工改过 / 表不存在)。
	playerNamePKUnknown playerNamePKShape = iota
	// playerNamePKByName 目标形态:PRIMARY KEY(name_norm)。
	playerNamePKByName
	// playerNamePKByPlayerID 旧形态:PRIMARY KEY(player_id),等待本迁移换掉。
	playerNamePKByPlayerID
)

// ensurePlayerNameKeyedByName 把存量 player_name 的主键换成 name_norm,幂等。
// tableName 不是 player_name 时直接返回(调用点在 bootstrap 表的循环里,不为一张表加 if 分支)。
//
// 顺序上它排在 assertColumnsPresent 之后、assertUniqueColumns 之前:
// 先确认列齐(不然读到的形状没有意义),再换键,最后由 assertUniqueColumns 复核
// **迁移之后**两条唯一性都还在 —— 那一条同时守住新旧两种形态,不必在这里重复判。
func ensurePlayerNameKeyedByName(ctx context.Context, db *sql.DB, dbName, tableName string, opts MigrateOptions) error {
	if tableName != PlayerNameTableName {
		return nil
	}

	pkCols, err := primaryKeyColumns(ctx, db, dbName, tableName)
	if err != nil {
		return err
	}
	switch playerNamePKShapeOf(pkCols) {
	case playerNamePKByName:
		// 新建库与已迁移库都走这里,但**不是**什么都不做:主键对了不等于另外两把键也对
		// (见本段头注"主键对了不等于形状对了"的两种中间态)。收敛那两条同样幂等,
		// 没事可做时一条 DDL 都不发、也不打日志 —— 这条路径每次启动都会走到。
		return convergePlayerNameKeyShape(ctx, db, dbName, tableName)
	case playerNamePKByPlayerID:
		// 继续往下迁移。
	default:
		return fmt.Errorf("table %s: 主键是 %v(空 = 表没有主键),既不是目标形态 (name_norm) 也不是待迁移的旧形态 (player_id):"+
			"这张表被人工改过,拒绝在看不懂的形状上下发 ALTER。先 `SHOW CREATE TABLE %s` 看清现状,"+
			"改回两种形态之一再重跑 `data_service -f <yaml> -migrate`",
			tableName, pkCols, tableName)
	}

	if opts.AllowLegacyPlayerNamePrimaryKey {
		// 运维显式选择"这条 ALTER 本实例先不跑",理由与出路见本段「失败时的运维出路」最后一条。
		// 与 TiDB 那一支同口径:打 Error 说明失效范围,然后放行,好让其余四张表的迁移照常完成。
		// 位置要紧:它排在"认不出的主键形状"那一支**之后** —— 这个开关只免掉"迁不迁",
		// 不免"看不懂的形状要 fail-closed",后者与表大小、维护窗口都无关。
		logx.Errorf("[schema] table %s: 跳过「主键改 name_norm」—— MigrateOptions.AllowLegacyPlayerNamePrimaryKey 已显式打开。"+
			"**本实例仍是旧形态** PRIMARY KEY(player_id) + UNIQUE(name_norm):功能完全正确(名字仍全服唯一),"+
			"但名字抢注在生产常态那一路(新号段 id 抢存量角色释放的名字)仍会成环 —— 真库实测 5/5,"+
			"只能靠有界 1213 重试吸收。NewPlayerNameStore 会探到旧形态并自动改用带 FORCE INDEX (%s) 的旧 Release 语句,"+
			"锁序仍然一致。这是一次有意的推迟:窗口合适时关掉这个开关重跑一次 -migrate 即可根治",
			tableName, PlayerNameLegacyNormUniqueKey)
		return nil
	}

	isTiDB, version, err := serverIsTiDB(ctx, db)
	if err != nil {
		return err
	}
	if isTiDB {
		// TiDB 上**不迁**,而且这不是 fail-closed 的场合:旧形态在功能上完全正确(名字仍然全服唯一),
		// 它只是在 InnoDB 上更容易成环 —— 而 TiDB 的悲观事务没有间隙锁与插入意向锁,那几类形状
		// 按官方文档本来就不存在(**未在 TiDB 上实测**)。在这里报错只会让整条 MigrateSchema 失败、
		// 三个 store 全置 nil,拿一次确定的业务中断去换一个在本引擎上并不存在的收益。
		// 另外 TiDB 对"一条 ALTER 里同时增删主键"的支持随版本而异,盲目下发反而可能留下中间态。
		logx.Errorf("[schema] table %s: 跳过「主键改 name_norm」—— 服务端是 TiDB(%s)。"+
			"本表保持旧形态 PRIMARY KEY(player_id) + UNIQUE(name_norm),功能正确(名字仍全服唯一);"+
			"NewPlayerNameStore 会探到旧形态并自动改用带 FORCE INDEX 的旧 Release 语句。"+
			"要在 TiDB 上也拿到根治,需要先定下 TiDB 形态的建表方案(候选:建表时就把主键写成 name_norm,"+
			"并放弃与之冲突的 SHARD_ROW_ID_BITS),这是待决项",
			tableName, version)
		return nil
	}

	uniqueIndexes, err := loadUniqueIndexColumns(ctx, db, dbName, tableName)
	if err != nil {
		return err
	}
	// name_norm 上承担"名字全服唯一"的那条(或那几条)二级唯一索引:主键换到 name_norm 之后它们
	// 全部多余,必须**在同一条 ALTER 里**一起 DROP 掉 —— 留一条下来就是上面说的中间态 1。
	normIndexes := strayNameNormUniqueIndexNames(uniqueIndexes)
	if len(normIndexes) == 0 {
		return fmt.Errorf("table %s: 主键还是旧形态 (player_id),但找不到 name_norm 上的单列全长唯一索引 —— "+
			"「名字全服唯一」这条不变量在这张表上已经没有实现了,先处理它再谈迁移。"+
			"查重:`SELECT name_norm, COUNT(*) FROM %s GROUP BY name_norm HAVING COUNT(*) > 1`;"+
			"清干净后 `ALTER TABLE %s ADD UNIQUE KEY uk_player_name (name_norm)`,再重跑 `data_service -f <yaml> -migrate`",
			tableName, tableName, tableName)
	}

	// player_id 那条唯一键可能已经在了(上一次迁移在非原子的实现上只做了一半,或有人先手工加过)。
	// 已在就不重复 ADD,让本步骤在任何中间态上都能重跑。
	hasOwnerKey, err := hasUniqueIndex(ctx, db, dbName, tableName, PlayerNameOwnerUniqueKey)
	if err != nil {
		return err
	}

	clauses := []string{"DROP PRIMARY KEY"}
	for _, idx := range normIndexes {
		clauses = append(clauses, "DROP INDEX `"+idx+"`")
	}
	clauses = append(clauses, "ADD PRIMARY KEY (`name_norm`)")
	if !hasOwnerKey {
		clauses = append(clauses, "ADD UNIQUE KEY `"+PlayerNameOwnerUniqueKey+"` (`player_id`)")
	}
	alter := "ALTER TABLE `" + tableName + "` " + strings.Join(clauses, ", ")

	logx.Infof("[schema] table %s: 把主键从 player_id 换成 name_norm(根治审计 #12 的名字抢注死锁)。"+
		"这条 ALTER 会**重建整张表**,按手册 Table 17.17 可以在线做(并发 DML 允许),"+
		"退回 ALGORITHM=COPY 时写入会被阻塞但不会丢;表大时请用部署阶段的 `data_service -f <yaml> -migrate`,"+
		"不要指望启动期 AutoMigrate 的 5 分钟。语句: %s", tableName, alter)
	if _, err := db.ExecContext(ctx, alter); err != nil {
		return fmt.Errorf("table %s: 换主键失败(表未被改动,MySQL 8.0 的 DDL 是原子的,修好原因后重跑 -migrate 即可): %w",
			tableName, err)
	}

	// 回读复核:与 assertNullableUniqueKey 同一个理由 —— ALTER 在个别兼容层上会被解析后忽略,
	// 那种"成功"正是最不能出现的失败形态(store 以为名字是聚簇主键、按新锁序发语句)。
	if err := assertPlayerNameTargetShape(ctx, db, dbName, tableName, "换主键的 ALTER"); err != nil {
		return err
	}
	logx.Infof("[schema] table %s: 主键已是 name_norm,%s 已就位,name_norm 上没有多余的二级唯一索引",
		tableName, PlayerNameOwnerUniqueKey)
	return nil
}

// convergePlayerNameKeyShape 在"主键已经是 name_norm"的表上,把**另外两把键**收敛到目标形态,幂等:
//   - DROP 掉 name_norm 上多余的单列唯一索引(主键已经覆盖了这条唯一性,多出来的那条只会让 Reserve
//     插入新名字时多往一个二级唯一索引里插项 —— 那条路上有插入意向锁,正是改主键消掉的形状);
//   - 缺 uk_player_name_owner 就补建(它是"一个角色只登记一个名字"的全部实现)。
//
// 两件事都不需要做时一条 DDL 都不发、也不打日志:新建库与已迁移库每次启动都会走到这里。
// 要做时用**一条** ALTER(DROP 与 ADD 在同一条里,与换主键那条同样只留一个提交点),做完回读复核。
//
// 为什么放在迁移里而不是让 store 拒绝启动:这两种中间态在**功能上都是对的**(名字仍全服唯一;
// 第 2 种缺的那条键会被紧随其后的 assertUniqueColumns 拦下,只是文案指不出缺的是哪条),
// 而修它们只要两条不重建表的 DDL。拿一次确定的业务中断去换一个迁移顺手就能修好的形状不划算 ——
// 与 NewPlayerNameStore"探到旧形态不拒启动"同一条取舍。
//
// 这一步**不按引擎分叉**(与换主键那条不同):它只增删二级索引,不碰主键,没有"一条 ALTER 同时增删主键"
// 那种随版本而异的支持度问题,TiDB 上同样是支持的 DDL。但 TiDB 上新建库的主键本来就是 NONCLUSTERED
// (见 playerNameBootstrapDDL 的「代价」),走到这里时通常无事可做。**未在 TiDB 上实测。**
//
// **未在真库验证**:DROP INDEX / ADD UNIQUE KEY 在 InnoDB 上按手册是 In Place、不重建表,
// 但本仓实例上的实际算法与耗时本轮没有实测。
func convergePlayerNameKeyShape(ctx context.Context, db *sql.DB, dbName, tableName string) error {
	uniqueIndexes, err := loadUniqueIndexColumns(ctx, db, dbName, tableName)
	if err != nil {
		return err
	}
	strays := strayNameNormUniqueIndexNames(uniqueIndexes)
	hasOwnerKey, err := hasUniqueIndex(ctx, db, dbName, tableName, PlayerNameOwnerUniqueKey)
	if err != nil {
		return err
	}
	if len(strays) == 0 && hasOwnerKey {
		return nil
	}

	clauses := make([]string, 0, len(strays)+1)
	for _, idx := range strays {
		clauses = append(clauses, "DROP INDEX `"+idx+"`")
	}
	if !hasOwnerKey {
		clauses = append(clauses, "ADD UNIQUE KEY `"+PlayerNameOwnerUniqueKey+"` (`player_id`)")
	}
	alter := "ALTER TABLE `" + tableName + "` " + strings.Join(clauses, ", ")

	// 打 Error 而不是 Info:主键已经换过、却还留着这种形状,只可能来自"回滚做了一半"或人工改表,
	// 运维应当知道自己的库经历过什么,而不是让它在 Info 里划过去。
	logx.Errorf("[schema] table %s: 主键已是 name_norm,但键的形状没收敛(多余的 name_norm 唯一索引 %v;"+
		"%s 在不在 = %v)—— 典型来源是按注释里的反向 ALTER 只回滚了一半,或有人手工只执行了 ADD PRIMARY KEY。"+
		"多余的那条会把名字抢注的死锁悄悄带回来,缺 %s 则让 Reserve 的 ID 复用检测静默失效。现在收敛: %s",
		tableName, strays, PlayerNameOwnerUniqueKey, hasOwnerKey, PlayerNameOwnerUniqueKey, alter)
	if _, err := db.ExecContext(ctx, alter); err != nil {
		return fmt.Errorf("table %s: 收敛键形状失败(DDL 原子,修好原因后重跑 -migrate 即可)。"+
			"若是以 1062 失败,说明库里已经有「同一个 player_id 占了多个名字」的行,那是数据事故,不能由迁移去猜留哪一行 —— "+
			"先 `SELECT player_id, COUNT(*) FROM %s GROUP BY player_id HAVING COUNT(*) > 1` 查出来人工定夺。语句: %s: %w",
			tableName, tableName, alter, err)
	}
	if err := assertPlayerNameTargetShape(ctx, db, dbName, tableName, "收敛键形状的 ALTER"); err != nil {
		return err
	}
	logx.Infof("[schema] table %s: 键形状已收敛到目标形态(PRIMARY(name_norm) + UNIQUE %s(player_id))",
		tableName, PlayerNameOwnerUniqueKey)
	return nil
}

// assertPlayerNameTargetShape 回读复核这张表确实是目标形态:主键 name_norm、name_norm 上没有多余的
// 单列唯一索引、uk_player_name_owner 就位。ALTER 报成功之后必须走一次 —— 理由与 assertNullableUniqueKey
// 相同:ALTER 在个别兼容层(代理 / 旧版本)上会被解析后忽略,而那种"成功"正是最不能出现的失败形态
// (store 以为名字是聚簇主键、按新锁序发语句,或以为 ID 复用检测还在)。
func assertPlayerNameTargetShape(ctx context.Context, db *sql.DB, dbName, tableName, after string) error {
	pkCols, err := primaryKeyColumns(ctx, db, dbName, tableName)
	if err != nil {
		return err
	}
	if playerNamePKShapeOf(pkCols) != playerNamePKByName {
		return fmt.Errorf("table %s: %s 报成功,回读主键却仍是 %v —— 拒绝继续"+
			"(这一步是名字抢注死锁根治的全部依据,形状没换成功就必须停在这里)", tableName, after, pkCols)
	}
	uniqueIndexes, err := loadUniqueIndexColumns(ctx, db, dbName, tableName)
	if err != nil {
		return err
	}
	if strays := strayNameNormUniqueIndexNames(uniqueIndexes); len(strays) > 0 {
		return fmt.Errorf("table %s: %s 报成功,name_norm 上却仍有多余的单列唯一索引 %v —— 拒绝继续"+
			"(主键已经覆盖这条唯一性,多出来的那条会让 Reserve 插入新名字时多走一次二级唯一索引的插入路径,"+
			"把改主键消掉的死锁形状带回来,而且功能全对、毫无报错)。处置:`ALTER TABLE %s DROP INDEX <名字>`",
			tableName, after, strays, tableName)
	}
	hasOwnerKey, err := hasUniqueIndex(ctx, db, dbName, tableName, PlayerNameOwnerUniqueKey)
	if err != nil {
		return err
	}
	if !hasOwnerKey {
		return fmt.Errorf("table %s: %s 报成功,%s 却不在 —— 拒绝继续"+
			"(它是「一个角色只登记一个名字」的全部实现:没有它,Reserve 的 ID 复用检测与 ownedNormOf 一起静默失效)。"+
			"处置:`ALTER TABLE %s ADD UNIQUE KEY %s (player_id)`",
			tableName, after, PlayerNameOwnerUniqueKey, tableName, PlayerNameOwnerUniqueKey)
	}
	return nil
}

// playerNamePKShapeOf 把主键列清单归到两种已知形态之一。列名大小写不敏感(MySQL 列名如此)。
func playerNamePKShapeOf(pkCols []string) playerNamePKShape {
	if len(pkCols) != 1 {
		return playerNamePKUnknown
	}
	switch {
	case strings.EqualFold(pkCols[0], "name_norm"):
		return playerNamePKByName
	case strings.EqualFold(pkCols[0], "player_id"):
		return playerNamePKByPlayerID
	default:
		return playerNamePKUnknown
	}
}

// strayNameNormUniqueIndexNames 列出 name_norm 上**除 PRIMARY 之外**的单列全长唯一索引名,按名字排序。
//
// 同一份清单在两处有**相反**的含义,所以只能有一个实现:
//   - 旧形态(主键是 player_id):它们承担着"名字全服唯一",换主键那条 ALTER 必须把它们全部 DROP 掉;
//   - 目标形态(主键是 name_norm):主键已经覆盖这条唯一性,它们全是多余的残留,同样要 DROP。
//
// 按形状找名字而不是写死 "uk_player_name":存量库上这条键可能是手工建的、叫别的名字
// (NewPlayerNameStore 的错误文案也写着这种可能)。**全部**返回而不是只挑一条,是因为 DDL 要一次收敛干净 ——
// 只删一条会留下下一次运行才发现的中间态。排序由 singleColumnUniqueIndexNames 保证:
// map 遍历顺序随机,而调用方据此下发 DROP INDEX,顺序不定就会让同一张表在两次运行里生成不同的语句。
//
// 只看**唯一**索引:非唯一的 name_norm 索引不是本迁移任何一条路径会产生的形态(反向 ALTER 建的是
// UNIQUE KEY),而它可能是别人为查询有意加的,迁移不该替他删。新建库上"name_norm 上一条二级索引都没有"
// 由真库用例 TestPlayerName_BootstrapDDLIsVarcharUniqueBin 钉住。
func strayNameNormUniqueIndexNames(uniqueIndexes map[string]uniqueIndexColumns) []string {
	names := singleColumnUniqueIndexNames(uniqueIndexes, "name_norm")
	strays := make([]string, 0, len(names))
	for _, n := range names {
		if strings.EqualFold(n, "PRIMARY") {
			continue
		}
		strays = append(strays, n)
	}
	if len(strays) == 0 {
		return nil
	}
	return strays
}

// playerNameStrayNormUniqueIndexes 是 strayNameNormUniqueIndexNames 的连库版本,给 NewPlayerNameStore
// 在建店时探一次形状用(它只打日志、不下发 DDL,见那里的注释)。
func playerNameStrayNormUniqueIndexes(ctx context.Context, db *sql.DB, dbName string) ([]string, error) {
	uniqueIndexes, err := loadUniqueIndexColumns(ctx, db, dbName, PlayerNameTableName)
	if err != nil {
		return nil, err
	}
	return strayNameNormUniqueIndexNames(uniqueIndexes), nil
}

// primaryKeyColumns 读一张表主键的列序(按 SEQ_IN_INDEX)。表没有主键时返回空切片、不报错 ——
// "没有主键"是一种要由调用方定性的形状,不是查询失败。
func primaryKeyColumns(ctx context.Context, db *sql.DB, dbName, tableName string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = 'PRIMARY'
		  ORDER BY SEQ_IN_INDEX`, dbName, tableName)
	if err != nil {
		return nil, fmt.Errorf("list primary key columns of %s: %w", tableName, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, fmt.Errorf("scan primary key columns of %s: %w", tableName, err)
		}
		cols = append(cols, col)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate primary key columns of %s: %w", tableName, err)
	}
	return cols, nil
}

// PlayerSnapshotTableName player_snapshot 表名(与 proto OptionTableName 一致)。
// 唯一列 / 唯一键名要在 store 与真库用例两处引用,集中在这里,不各写一份字面量。
const PlayerSnapshotTableName = "player_snapshot"

const (
	// SnapshotGuidNzColumn 是 player_snapshot 上"去掉 0 之后的 snapshot_guid"存储型生成列。
	// nz = non-zero:guid=0 的 GM 行在这一列上是 NULL。
	SnapshotGuidNzColumn = "snapshot_guid_nz"
	// SnapshotGuidUniqueKey 是建在 SnapshotGuidNzColumn 上的唯一键名。
	SnapshotGuidUniqueKey = "uk_snapshot_guid_nz"
)

// ── 可空唯一列:本包第三条手写 DDL 例外(审计 #11 第二步根治)────────────────────────────
//
// 为什么 proto2mysql 表达不了这一条,所以必须手写:
//
//  1. 它要的是**可空**唯一列。proto2mysql 把 proto3 标量渲染成 `NOT NULL DEFAULT 0`,
//     nullable 选项也只去掉 NOT NULL、仍留 DEFAULT 0,而写入路径对未设置的标量一律绑 0 ——
//     直接把 UNIQUE 建在 snapshot_guid 上,第二条 GM 行(guid 恒为 0)就撞键。
//  2. 它要的是**生成列**。proto2mysql 的列只从 proto 字段来,没有"由表达式算出的列"这个概念;
//     而 snapshot_guid_nz 也刻意**不该**进 proto:它不是快照的一个字段,是同一个事实的
//     一种索引形态,进 proto 就会被 C++ / Go 两侧的 message 当成要填的业务字段。
//
// 于是这一条与 idSegmentBootstrapDDL / playerNameBootstrapDDL 同类(手写 DDL),但形态不同:
// 那两张表是**整表**预建、之后不再交给 proto2mysql 同步;player_snapshot 仍然全程由 proto 驱动,
// 这里只在同步之后**追加**一列 + 一个键(纯加法),所以放在 ensureIndexes 之后作为独立一步,
// 而不是把 player_snapshot 塞进 bootstrapTables —— 塞进去会连带停掉它的列同步,proto 以后加字段
// 就再也补不上列了。
//
// 为什么 proto2mysql 的列 / 索引同步不会碰这一列与这个键(2026-09-28 对 v0.2.0 源码逐条核对,
// 单测 TestSnapshotGuidNzSurvivesProtoSync 钉住判据):
//   - 它"永不 DROP COLUMN":buildColumnClauses 只按 proto 字段名 / 列注释里的 `pb:N` 找列,
//     剩下的列收在局部变量 remaining 里、**之后再没有被读过**,既不删也不改。
//   - 所以 snapshotGuidNzColumnDDL 的列注释**绝不能**写成 `pb:N` 开头:那会让它被当成某个 proto
//     字段的列,进而被 CHANGE COLUMN 改名改类型(甚至与真正那一列抢同一个字段号而 ErrSchemaDrift)。
//   - 表已存在时 proto2mysql 一条索引都不建、也不删(schema.go 的 ensureIndexes 头注释就是为此而写)。
//   - ensureIndexes 只 CREATE 缺失的索引:判据 indexCovered 比的是**列名前缀**,
//     "snapshot_guid_nz" 与 proto 声明的 "snapshot_guid" 不相等,既不会把本键误判成
//     idx(snapshot_guid) 的替身(那会让真正需要的索引不被建出来),也不会被它顶掉。
//   - assertColumnsPresent / assertUniqueColumns 只作用于 bootstrapTables 里的表,
//     player_snapshot 不在其中,不会因为"proto 里没有 snapshot_guid_nz"而报错。
//
// # 反向耦合:这条生成列**钉死了** snapshot_guid(改 proto 的人得不到任何提示)
//
// 上面论证的是"proto2mysql 不会碰这一列";反过来的一条同样成立,而且更容易踩:
// snapshot_guid_nz 依赖 snapshot_guid,MySQL 从此会拒绝对 snapshot_guid 的 DROP / RENAME
// (ER_DEPENDENT_BY_GENERATED_COLUMN),对它的 MODIFY 也不再能走 INPLACE(退化成拷表)。
// 也就是说本步骤给 proto 侧的 snapshot_guid 字段加了一条新的不可改约束,
// 而 proto/common/database/rollback_database_table.proto **不归本服务所有** ——
// 改那个字段的人不会读到这里,症状是某一次 `-migrate` 硬失败。
// 真要改 snapshot_guid 的名字 / 类型时,顺序是:先 DROP INDEX uk_snapshot_guid_nz、
// 再 DROP COLUMN snapshot_guid_nz(即下面的回滚方案两条),改完再跑一次 -migrate 重建。
//
// 另:同一个 proto 文件里 snapshot_guid 字段那几行注释仍写着"刻意用普通 INDEX 而不是 UNIQUE……
// 先查后插(check-then-insert)",已被本次改动作废,需由该目录负责人更新(本组只提出,不改 proto)。
//
// # 回滚方案
//
// 生成列与唯一键都是**加法**,回滚只需两条 DROP,一行数据都不丢:
//
//	ALTER TABLE `player_snapshot` DROP INDEX `uk_snapshot_guid_nz`;
//	ALTER TABLE `player_snapshot` DROP COLUMN `snapshot_guid_nz`;
//
// 新旧版本二进制并存(滚动升级窗口)时的行为:
//   - **旧二进制 + 新表结构**:旧版按 `INSERT ... SELECT ... WHERE NOT EXISTS` 去重,插之前
//     先确认这个 guid 不在表里,所以它**不会**违反新加的唯一键;真撞上(与新版写者赛跑)时收到的是
//     1062,旧版把它当普通错误交给消费者重试,重试时 NOT EXISTS 判假、0 行受影响,走"查已存在 id"
//     的分支收敛。既不丢也不重。
//   - **新二进制 + 旧表结构**(DDL 还没跑、或刚刚回滚):新版在建 store 时探测唯一键是否存在,
//     探不到就继续走 NOT EXISTS 那条语句(见 snapshot_store.go 的 InsertSnapshotIfGuidAbsent),
//     去重照旧有效,只是又回到了靠 RR 间隙锁、并因此需要有界 1213 重试的形态。
//
// 两个方向都安全,所以 DDL 与二进制的上线顺序不需要编排;推荐仍是先 `-migrate` 再滚动发版,
// 好让窗口里尽早只剩一条写路径。
const (
	// snapshotGuidNzColumnDDL 追加可空唯一列。
	//
	// NULLIF(snapshot_guid, 0) 把 GM 行的 0 映射成 NULL;SQL 的唯一键不约束 NULL,
	// 所以 GM 行可以有任意多条,而 source=1 的 C++ 快照按 guid 严格唯一 —— 这正是
	// proto 注释里"UNIQUE 会让第二条 GM 行就撞键"那个障碍的解法。
	//
	// STORED 而不是 VIRTUAL:值实打实落盘,唯一键与点查都直接读列,不必每次求表达式。
	// 代价是 MySQL 手册明确说的 "ADD COLUMN is not an in-place operation for stored columns"
	// —— 这条 ALTER 会**重建整张表**。所以它必须由部署阶段的 `data_service -f <yaml> -migrate`
	// 执行(不设时限),不要指望启动期 AutoMigrate 的 5 分钟(svc.autoMigrateTimeout)够用;
	// 表大时启动路径会超时、三个 store 全置 nil,而 MySQL 侧那条 ALTER 还在继续跑
	// (与 warnIfLegacyTable 描述的是同一种事故形态)。
	//
	// 列注释刻意不是 `pb:N` 开头:见上面那段"为什么同步不会碰它"。
	// snapshotGuidNzExpr 是生成列的表达式,**只写一次**:DDL 由它拼出,建完之后的复核
	// (hasGeneratedColumn)也拿它跟 INFORMATION_SCHEMA.GENERATION_EXPRESSION 比。
	// 各写一份的话,改了 DDL 而忘了改复核,症状是"复核永远通过"。
	snapshotGuidNzExpr = "NULLIF(`snapshot_guid`, 0)"

	snapshotGuidNzColumnDDL = "ALTER TABLE `player_snapshot` ADD COLUMN `snapshot_guid_nz` bigint unsigned" +
		" GENERATED ALWAYS AS (" + snapshotGuidNzExpr + ") STORED" +
		" COMMENT '非 proto 列:NULLIF(snapshot_guid,0),GM 行(guid=0)在此为 NULL,供 uk_snapshot_guid_nz 做可空唯一约束'"

	// snapshotGuidNzUniqueDDL 建唯一键。必须与列分成两条语句下发:MySQL 对 STORED 生成列的
	// ADD COLUMN 是拷表操作,把建索引并进同一条 ALTER 只会让那次重建更长、失败时更难判断进度。
	snapshotGuidNzUniqueDDL = "ALTER TABLE `player_snapshot` ADD UNIQUE KEY `uk_snapshot_guid_nz` (`snapshot_guid_nz`)"
)

// nullableUniqueKey 描述一条"可空唯一列":一个由表达式算出的生成列 + 建在它上面的唯一键,
// 外加建键之前必须先做的历史去重(唯一键建不上去,ALTER 会以 1062 失败)。
//
// 做成清单而不是在 migrateSchemaOn 里写 if 分支,理由与 bootstrapTables 相同:
// "哪些表有非 proto 驱动的列与键"这件事只该有一处答案。再加一条 = 在 nullableUniqueKeys 里加一项。
type nullableUniqueKey struct {
	table      string // 表名
	column     string // 生成列名
	columnExpr string // 生成列的表达式(回读复核时与 GENERATION_EXPRESSION 比,见 hasGeneratedColumn)
	columnDDL  string // 追加生成列的 ALTER(表已有**形状相符**的此列时跳过)
	indexName  string // 唯一键名
	indexDDL   string // 建唯一键的 ALTER(键已存在时整步跳过)

	// sourceColumn 是生成列的源列;去重按它分组。
	// zeroValue 是"不参与唯一约束"的那个值(映射成 NULL 的值),去重时排除它。
	sourceColumn string
	zeroValue    uint64
}

// nullableUniqueKeys 当前只有一条:player_snapshot 按 snapshot_guid 去重(审计 #11)。
var nullableUniqueKeys = []nullableUniqueKey{{
	table:        PlayerSnapshotTableName,
	column:       SnapshotGuidNzColumn,
	columnExpr:   snapshotGuidNzExpr,
	columnDDL:    snapshotGuidNzColumnDDL,
	indexName:    SnapshotGuidUniqueKey,
	indexDDL:     snapshotGuidNzUniqueDDL,
	sourceColumn: "snapshot_guid",
	zeroValue:    0,
}}

const (
	// snapshotGuidDedupeGuidsPerBatch 一批处理多少个重复 guid。
	// 每个 guid 先查出要删的 id,再逐条主键点删(见 dedupePointDeleteSQL);
	// 批是游标推进的粒度,免得重复行很多时把整张表的分组结果一次性拉进内存。
	snapshotGuidDedupeGuidsPerBatch = 200

	// snapshotGuidDedupeMaxRows 是"一次迁移最多删多少行"的**默认值**,
	// 可由 MigrateOptions.SnapshotGuidDedupeMaxRows 覆盖(运维不必重新编译)。
	//
	// 为什么要上限:这是**删数据**的一步,而"重复行"在设计上根本不该存在
	// (部署不变量是 data-service replicas=1 + Recreate)。真删出成千上万行时,更可能是
	// 有人把两套环境指到了同一个库、或 snapshot_guid 发号出了问题 —— 那种情况下应该停下来让人看,
	// 而不是让迁移安静地删到天亮。
	snapshotGuidDedupeMaxRows = 100_000
)

// ensureNullableUniqueKeys 给 tableName 补齐 nullableUniqueKeys 里登记的可空唯一列,幂等。
//
// 顺序(每一条都必须在前一条之后,理由见各自处):
//  1. 唯一键已存在**且确实是唯一索引**、**且它约束的那一列确实是形状相符的生成列** → 整步跳过
//     (幂等的快路径)。两个条件缺一不可,理由见快路径里那段注释。
//     同名的非唯一索引 / 同名的普通列在这里直接判错,不当成"已存在"(见 hasUniqueIndex / hasGeneratedColumn)。
//  2. TiDB → **报错停住**(官方文档明确建不出来);只有 MigrateOptions.AllowMissingGuidUniqueKey
//     显式打开时才放行,并在日志里写明"本实例的快照 guid 去重已失效"。
//  3. 去重历史重复行 —— 必须在建键**之前**:有重复行时 ADD UNIQUE KEY 会以 1062 失败。
//  4. 追加生成列(已存在**且形状相符**则跳过;同名的普通列在这里判错,见 hasGeneratedColumn)。
//  5. 建唯一键。
//  6. 复核列与键都在、且形状正确 —— fail-closed:这个键是"同一 guid 只落一行"的唯一实现,
//     没建上却当成建上了,后果是消费者重放时静默写重复行(§11.3 数据完整性路径默认 fail-closed)。
func ensureNullableUniqueKeys(ctx context.Context, db *sql.DB, dbName, tableName string, opts MigrateOptions) error {
	for _, k := range nullableUniqueKeys {
		if k.table != tableName {
			continue
		}
		if err := ensureNullableUniqueKey(ctx, db, dbName, k, opts); err != nil {
			return err
		}
	}
	return nil
}

func ensureNullableUniqueKey(ctx context.Context, db *sql.DB, dbName string, k nullableUniqueKey, opts MigrateOptions) error {
	hasKey, err := hasUniqueIndex(ctx, db, dbName, k.table, k.indexName)
	if err != nil {
		return err
	}
	if hasKey {
		// 键在**还不够**:下面建键的那一步(indexDDL)排在复核(assertNullableUniqueKey)**之前**,
		// 所以库里可能留下一个"键已建出、列却是坏的"的中间态 —— 一个把 `GENERATED ALWAYS AS (...) STORED`
		// 解析后降级成普通列的兼容层会让 ADD COLUMN 与 ADD UNIQUE KEY 都不报错,只有随后的复核判红。
		// 那一次运行是 fail-closed 的,但**下一次**运行如果只看键名就会从这条快路径直接 return nil,
		// 迁移变成"成功",写路径切到 ODKU,而唯一键挂在一个恒为 NULL 的普通列上 ——
		// 同一个 guid 静默落成多行、全程零报错,正是本步骤要消灭的形态。
		// 所以快路径必须把列的形状也校验一遍,把那次 fail-closed 钉住,不让它在重跑时翻成放行。
		hasCol, err := hasGeneratedColumn(ctx, db, dbName, k.table, k.column, k.columnExpr)
		if err != nil {
			return err
		}
		if !hasCol {
			return fmt.Errorf("table %s: 唯一键 %s 在,但它约束的列 %s 不存在 —— 这是一个「键在、列坏」的中间态"+
				"(上一次迁移建完键之后复核失败留下的),唯一键约束不到任何行,拒绝继续。"+
				"处置:`ALTER TABLE %s DROP INDEX %s`,再按 hasGeneratedColumn 的提示处理那一列,然后重跑 -migrate",
				k.table, k.indexName, k.column, k.table, k.indexName)
		}
		logx.Infof("[schema] table %s: nullable unique key %s(%s) already present (column shape verified)",
			k.table, k.indexName, k.column)
		return nil
	}

	isTiDB, version, err := serverIsTiDB(ctx, db)
	if err != nil {
		return err
	}
	if isTiDB {
		// TiDB 上这条 DDL 建不出来(官方文档两条限制,2026-09-29 复核仍然属实):
		// 「You cannot add a stored generated column through ALTER TABLE」与「NULLIF() 不支持」
		// —— https://docs.pingcap.com/tidb/stable/generated-columns/
		//
		// 建不出来就必须**停**,不能打一条 Errorf 然后 return nil:那样迁移照常成功、服务照常启动,
		// 而 TiDB 上的实际后果是 #11 的整个根治静默失效 —— 唯一键没有,退路那条
		// INSERT...SELECT...NOT EXISTS 又依赖 RR 的间隙加锁读,TiDB 没有间隙锁,
		// 两个并发写者会同时判定"不存在"各插一行,同一 guid 多行落库且全程零报错。
		// 这正是本步骤要消灭的失败形态,所以与紧邻的 assertNullableUniqueKey 同口径:fail-closed
		// (AGENTS.md §11.3 数据完整性路径默认 fail-closed)。AutoMigrate 路径会因此把三个 store 置 nil、
		// 业务立刻拒绝 —— 症状明确,好过安静地写重复行。
		if !opts.AllowMissingGuidUniqueKey {
			return fmt.Errorf("table %s: 服务端是 TiDB(%s),建不出 %s(官方文档:不支持 ALTER 添加 STORED 生成列、"+
				"且不支持 NULLIF();https://docs.pingcap.com/tidb/stable/generated-columns/)。"+
				"没有这个键,同一个 %s 会静默落成多行(退路的 NOT EXISTS 去重依赖 RR 间隙锁,TiDB 上不成立),"+
				"所以迁移在这里停住。两条出路:(1) 先定下 TiDB 形态的可空唯一约束再迁 —— "+
				"候选是建表时就写进 CREATE TABLE、或改 VIRTUAL 生成列 + CASE WHEN 表达式;"+
				"(2) 明确接受「本实例的快照 guid 去重失效」,由配置显式打开 MigrateOptions.AllowMissingGuidUniqueKey 放行",
				k.table, version, k.indexName, k.sourceColumn)
		}
		logx.Errorf("[schema] table %s: 跳过 %s —— 服务端是 TiDB(%s),且 AllowMissingGuidUniqueKey 已显式打开。"+
			"**本实例的快照 guid 去重已失效**:同一个 %s 可以落成多行,且不会有任何报错。"+
			"这是一次有意的放行,不是可忽略的告警 —— TiDB 形态的可空唯一约束仍是待决项",
			k.table, k.indexName, version, k.sourceColumn)
		return nil
	}

	if err := dedupeBySourceColumn(ctx, db, k, opts); err != nil {
		return err
	}

	hasCol, err := hasGeneratedColumn(ctx, db, dbName, k.table, k.column, k.columnExpr)
	if err != nil {
		return err
	}
	if !hasCol {
		logx.Infof("[schema] table %s: adding stored generated column %s (rebuilds the table; prefer `data_service -f <yaml> -migrate`)",
			k.table, k.column)
		if _, err := db.ExecContext(ctx, k.columnDDL); err != nil {
			return fmt.Errorf("add generated column %s.%s: %w", k.table, k.column, err)
		}
	}

	// 注意顺序风险:建键在复核**之前**,所以复核判红时库里会留下"键在、列坏"的中间态。
	// 上面那条快路径必须能认出它(否则下一次运行会把这次 fail-closed 翻成静默放行)。
	if _, err := db.ExecContext(ctx, k.indexDDL); err != nil {
		return fmt.Errorf("add unique key %s on %s(%s): %w", k.indexName, k.table, k.column, err)
	}
	logx.Infof("[schema] table %s: created nullable unique key %s(%s)", k.table, k.indexName, k.column)

	return assertNullableUniqueKey(ctx, db, dbName, k)
}

// assertNullableUniqueKey 复核生成列与唯一键都已就位**且形状正确**。
// 建出来之后立刻回读,而不是相信 ExecContext 没报错:ALTER 在个别兼容层(代理 / 旧版本)上
// 会被解析后忽略,那种"成功"正是这条键最不能出现的失败形态 —— 键不在而写入路径以为它在。
// 形状(STORED 生成列 + 表达式、NON_UNIQUE=0)由 hasGeneratedColumn / hasUniqueIndex 判,
// 它们在形状不符时直接返回错误:把 UNIQUE 降级成普通索引、把生成列建成普通列的兼容层,
// 与"解析后忽略"落在同一个威胁模型里,只按名字回读认不出来。
func assertNullableUniqueKey(ctx context.Context, db *sql.DB, dbName string, k nullableUniqueKey) error {
	hasCol, err := hasGeneratedColumn(ctx, db, dbName, k.table, k.column, k.columnExpr)
	if err != nil {
		return err
	}
	if !hasCol {
		return fmt.Errorf("table %s: generated column %s absent right after the ALTER reported success; "+
			"refusing to continue (the unique key on it is the only enforcement of one-row-per-%s)",
			k.table, k.column, k.sourceColumn)
	}
	hasKey, err := hasUniqueIndex(ctx, db, dbName, k.table, k.indexName)
	if err != nil {
		return err
	}
	if !hasKey {
		return fmt.Errorf("table %s: unique key %s absent right after the ALTER reported success; "+
			"refusing to continue (it is the only enforcement of one-row-per-%s)", k.table, k.indexName, k.sourceColumn)
	}
	return nil
}

// dedupeBySourceColumn 清掉历史重复行:按 sourceColumn 分组(排除 zeroValue),每组只留最小 id。
//
// # 前置条件:写者必须已经停下
//
// 本步骤**要求 player_snapshot 上没有在线写者**(生产的做法是停掉 data_service 再跑
// `data_service -f <yaml> -migrate`)。两条理由,都不是洁癖:
//   - 判据会漂:每一批的"哪些值还重复"都从当前库重算,在线写者插进来的新行会让循环看不到终点;
//   - 会误删:一个 guid 的"要删的 id"是在取批那一刻算出来的,若此时在线写者为同一个 guid 又插了一行
//     (正是退路形态下会发生的重复),它会落进"待删"集合被顺手删掉,而消费者可能已经提交了
//     那条消息的 offset —— 迁移期悄悄丢一条快照。
//
// 为什么留**最小** id:自增 id 单调递增,最小 id 就是先到的那一行;而 rollback_audit_log.snapshot_id_used
// 引用的是这个自增 id,留先到的一行能让既有审计记录继续指得到东西(后到的重复行还没被任何审计引用过 ——
// 它是"同一 guid 落了两次"的产物,消费者用的是第一次的 id)。
//
// 幂等 / 可重跑:每一步都从当前库状态重新算,删到"没有重复值"为止。中途失败(网络断、上限用尽)
// 再跑一次就从断点继续,不会重复删、也不会漏。
//
// 先报数再删:第一条查询把"多少个值重复、合计多余多少行"打进日志。删数据这一步必须留下
// 执行前的数字 —— 出了问题时,这是唯一能回答"本来有多少"的证据。
//
// # 代价量级(运维据此定维护窗口)
//
// 分组查询按 sourceColumn 游标推进(每批带上一批的最大值),整趟去重合计是**一次**有序索引扫描,
// 而不是每批都重扫一遍全表。player_snapshot 在设计上就是张大表(go/db 的配置里把它列为"天然就该大的表"),
// 早先那种每批重跑一次全表分组的写法,在重复组数 G 时要扫 ceil(G/批大小) 遍,上限 10 万行时最坏 500 遍。
func dedupeBySourceColumn(ctx context.Context, db *sql.DB, k nullableUniqueKey, opts MigrateOptions) error {
	maxRows := opts.dedupeMaxRows()
	dupGroups, extraRows, err := countDuplicateGroups(ctx, db, k)
	if err != nil {
		return err
	}
	if dupGroups == 0 {
		logx.Infof("[schema] table %s: no duplicate %s to clean up before creating %s", k.table, k.sourceColumn, k.indexName)
		return nil
	}
	logx.Errorf("[schema] table %s: %d 个 %s 有重复行,合计多余 %d 行,建 %s 之前按「每组留最小 id」清理。"+
		"重复行在设计上不该存在(部署不变量是 data-service 单实例),出现它说明曾有多个写者并发写这张表 —— "+
		"清完之后请顺带核对是不是有两套环境指到了同一个库",
		k.table, dupGroups, k.sourceColumn, extraRows, k.indexName)

	// 每组多余行数 >= 1,所以 extraRows 就是本次要删的总数;超上限直接停手,不删一行。
	//
	// 文案只给**真正可执行**的出路。特别是不能写"分多次跑 -migrate,迁移会从断点继续":
	// 这个分支在删任何一行**之前**返回,而 extraRows 每次都从当前库重算 —— 重跑只会拿到同一个错误、
	// 删 0 行,永远没有"断点"。(循环里那条同类提示在那里成立,因为那时确实已经删掉了一部分。)
	if extraRows > maxRows {
		return fmt.Errorf("table %s: %d 行重复的 %s 超过本次迁移上限 %d,拒绝自动删除(一行都没删)。"+
			"先停掉所有写者并人工确认这些重复行的来源(最可能是多个 data_service 实例或两套环境写了同一个库)。"+
			"确认无误后二选一:(1) 把 MigrateOptions.SnapshotGuidDedupeMaxRows 调到 >= %d 再跑一次 -migrate;"+
			"(2) 停写后人工分批执行:"+
			"DELETE t FROM %s t JOIN (SELECT %s AS v, MIN(id) AS keep FROM %s WHERE %s <> %d GROUP BY %s"+
			" HAVING COUNT(*) > 1) d ON t.%s = d.v AND t.id > d.keep",
			k.table, extraRows, k.sourceColumn, maxRows, extraRows,
			k.table, k.sourceColumn, k.table, k.sourceColumn, k.zeroValue, k.sourceColumn, k.sourceColumn)
	}

	var deleted int64
	var cursor uint64 // 已处理到的 sourceColumn 值;下一批只看比它大的,总代价 = 一次有序扫描
	for {
		batch, err := loadDuplicateBatch(ctx, db, k, cursor)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, d := range batch {
			n, err := deleteDuplicateRowsOf(ctx, db, k, d, maxRows-deleted)
			deleted += n
			if err != nil {
				return fmt.Errorf("table %s: delete duplicate rows of %s=%d (kept id %d, %d rows deleted so far): %w",
					k.table, k.sourceColumn, d.value, d.keepID, deleted, err)
			}
			if deleted >= maxRows {
				return fmt.Errorf("table %s: 已删 %d 行仍未去重干净,达到本次迁移上限 %d(执行期间有人在写?)。"+
					"迁移幂等:停掉写者后再跑一次 `data_service -f <yaml> -migrate` 会从断点继续;"+
					"确属正常存量则调高 MigrateOptions.SnapshotGuidDedupeMaxRows",
					k.table, deleted, maxRows)
			}
			cursor = d.value
		}
	}
	logx.Infof("[schema] table %s: deduped %s, deleted %d duplicate rows (kept the smallest id per value)",
		k.table, k.sourceColumn, deleted)
	return nil
}

// duplicateGroup 一个重复值,以及这一组里要保留的那一行。
type duplicateGroup struct {
	value  uint64
	keepID uint64
}

// dedupePointDeleteSQL 按主键删一行。
//
// 为什么是主键**点删**,而不是一条 `WHERE sourceColumn = ? AND id > ?` 的范围删:
//   - 仓库统一口径是"守卫之后的锁定读 / 写一律完整主键等值点查 / 点更新,配 EXPLAIN 回归"
//     (回归 TestSnapshotMigration_DedupePointDeleteUsesPrimaryKey);
//   - 范围删的取锁顺序与在线写者**相反**:INSERT 先写聚簇记录再写二级索引项,而范围删先在
//     idx(sourceColumn) 上取 next-key X 再回表取聚簇行 X —— 正是残余环的构成条件。
//     点删先锁聚簇记录,与写者同向。
//   - 一次 1213 会让整条 MigrateSchema 失败(AutoMigrate 下三个 store 全置 nil),所以这条点删
//     也配有界 1213 重试,与 InsertSnapshotIfGuidAbsent 同款(次数上限 + 抖动退避 + ctx 可取消)。
func dedupePointDeleteSQL(table string) string {
	return fmt.Sprintf("DELETE FROM `%s` WHERE `id` = ?", table)
}

// dedupeVictimIDsSQL 列出一组里要删的行(留最小 id,删其余)。limit 由调用方按剩余预算给。
func dedupeVictimIDsSQL(table, sourceColumn string, limit int64) string {
	return fmt.Sprintf("SELECT `id` FROM `%s` WHERE `%s` = ? AND `id` > ? ORDER BY `id` LIMIT %d",
		table, sourceColumn, limit)
}

// deleteDuplicateRowsOf 删掉这一组里除 keepID 之外的行,逐条主键点删。budget <= 0 时不删。
// 返回实际删掉的行数(出错时也返回已删数,调用方据此报告进度)。
func deleteDuplicateRowsOf(ctx context.Context, db *sql.DB, k nullableUniqueKey, d duplicateGroup, budget int64) (int64, error) {
	if budget <= 0 {
		return 0, nil
	}
	// 先取 id 列表再逐条删:取的时候是一致性读(不加锁),删的时候每条只锁一行主键记录。
	// 多取一条(budget+1)是为了让上层的上限分支能看出"这一组还没删完"。
	ids, err := loadDuplicateVictimIDs(ctx, db, k, d, budget+1)
	if err != nil {
		return 0, err
	}

	var deleted int64
	for _, id := range ids {
		if deleted >= budget {
			break
		}
		var res sql.Result
		_, err := retryOnDeadlock(ctx, snapshotGuidInsertAttempts, snapshotGuidDeadlockBackoff, func() error {
			var execErr error
			res, execErr = db.ExecContext(ctx, dedupePointDeleteSQL(k.table), id)
			return execErr
		})
		if err != nil {
			return deleted, fmt.Errorf("point-delete id %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("rows affected of point-delete id %d: %w", id, err)
		}
		deleted += n
	}
	return deleted, nil
}

// loadDuplicateVictimIDs 取这一组里 id > keepID 的行(升序,最多 limit 条)。
func loadDuplicateVictimIDs(ctx context.Context, db *sql.DB, k nullableUniqueKey, d duplicateGroup, limit int64) ([]uint64, error) {
	rows, err := db.QueryContext(ctx, dedupeVictimIDsSQL(k.table, k.sourceColumn, limit), d.value, d.keepID)
	if err != nil {
		return nil, fmt.Errorf("list duplicate rows of %s=%d: %w", k.sourceColumn, d.value, err)
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan duplicate row id of %s=%d: %w", k.sourceColumn, d.value, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate duplicate rows of %s=%d: %w", k.sourceColumn, d.value, err)
	}
	return ids, nil
}

// countDuplicateGroups 报数:多少个值有重复行、合计多余多少行。
func countDuplicateGroups(ctx context.Context, db *sql.DB, k nullableUniqueKey) (groups, extraRows int64, err error) {
	q := fmt.Sprintf(
		"SELECT COUNT(*), COALESCE(SUM(`n` - 1), 0) FROM ("+
			"SELECT COUNT(*) AS `n` FROM `%s` WHERE `%s` <> ? GROUP BY `%s` HAVING COUNT(*) > 1) AS d",
		k.table, k.sourceColumn, k.sourceColumn)
	if err := db.QueryRowContext(ctx, q, k.zeroValue).Scan(&groups, &extraRows); err != nil {
		return 0, 0, fmt.Errorf("table %s: count duplicate %s: %w", k.table, k.sourceColumn, err)
	}
	return groups, extraRows, nil
}

// loadDuplicateBatch 取下一批待去重的值(每批最多 snapshotGuidDedupeGuidsPerBatch 个)。
//
// after 是上一批处理到的 sourceColumn 值,按它做游标:分组随之推进,整趟去重合计只扫一次有序索引。
// (早先没有游标,每批都重跑一次覆盖全表的分组 —— 批只是循环粒度,代价是 ceil(重复组数/批大小) 次全扫。)
// 前置条件是写者已停(见 dedupeBySourceColumn 头注),所以游标不会漏掉"删完之后又冒出来"的组。
func loadDuplicateBatch(ctx context.Context, db *sql.DB, k nullableUniqueKey, after uint64) ([]duplicateGroup, error) {
	q := fmt.Sprintf(
		"SELECT `%s`, MIN(`id`) FROM `%s` WHERE `%s` <> ? AND `%s` > ? GROUP BY `%s` HAVING COUNT(*) > 1 ORDER BY `%s` LIMIT %d",
		k.sourceColumn, k.table, k.sourceColumn, k.sourceColumn, k.sourceColumn, k.sourceColumn, snapshotGuidDedupeGuidsPerBatch)
	rows, err := db.QueryContext(ctx, q, k.zeroValue, after)
	if err != nil {
		return nil, fmt.Errorf("table %s: list duplicate %s: %w", k.table, k.sourceColumn, err)
	}
	defer rows.Close()

	var out []duplicateGroup
	for rows.Next() {
		var d duplicateGroup
		if err := rows.Scan(&d.value, &d.keepID); err != nil {
			return nil, fmt.Errorf("table %s: scan duplicate %s: %w", k.table, k.sourceColumn, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("table %s: iterate duplicate %s: %w", k.table, k.sourceColumn, err)
	}
	return out, nil
}

// hasGeneratedColumn / hasUniqueIndex 是可空唯一列这一步的两个**存在性 + 形状**判据。
// 刻意不复用 loadIndexColumns / assertColumnsPresent:那两个函数各自服务于别的判据(前缀覆盖、proto 列齐),
// 把"这一个名字在不在"塞进去会让签名变浑;这几条 INFORMATION_SCHEMA 查询只在迁移 / 建 store 时各跑一次。
//
// 两个函数都**只按名字放行是不够的**,原因是同一条:它们同时是三处判据的唯一依据 ——
// 迁移快路径(整步跳过)、建键后的复核、以及 SnapshotStore 建池时决定写路径形态的那次探测。
// 名字对而形状不对时放行,后果都是最坏的那一种:写路径以为唯一约束在,实际一行都约束不到,
// 同一个 guid 静默落成多行且零报错(§11.3 数据完整性路径默认 fail-closed)。
// 威胁模型不是假想:assertNullableUniqueKey 存在的理由就是"ALTER 在个别兼容层(代理 / 旧版本)上
// 会被解析后忽略",而把 UNIQUE 悄悄降级成普通 INDEX、把生成列悄悄建成普通列,正落在同一个模型里;
// 人工加过同名列 / 同名索引的存量库也一样。

// hasGeneratedColumn 判定这一列在、且确实是 wantExpr 算出的 **STORED 生成列**。
// 只看列名会放过"有人手工加了一个同名普通列"的库:那时迁移跳过 ADD COLUMN,
// 唯一键就建到了一个恒为 NULL(约束不到任何行)或恒为 0(第二条 GM 行就撞键)的普通列上。
// 判不出形状时返回错误而不是 false —— false 会让调用方去 ADD COLUMN,撞上"列已存在"的 1060,
// 错误文案指不到真正的原因。
func hasGeneratedColumn(ctx context.Context, db *sql.DB, dbName, tableName, column, wantExpr string) (bool, error) {
	var extra, genExpr sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT EXTRA, GENERATION_EXPRESSION FROM INFORMATION_SCHEMA.COLUMNS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`, dbName, tableName, column).Scan(&extra, &genExpr)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up column %s.%s: %w", tableName, column, err)
	}
	if !strings.Contains(strings.ToUpper(extra.String), "STORED GENERATED") {
		return false, fmt.Errorf("table %s: 列 %s 已存在但不是 STORED 生成列(EXTRA=%q):"+
			"唯一键建在它上面约束不到任何东西,拒绝继续。先人工确认这一列是谁加的,"+
			"确认可丢弃后 `ALTER TABLE %s DROP COLUMN %s` 再重跑迁移",
			tableName, column, extra.String, tableName, column)
	}
	if got, want := normalizeSQLExpr(genExpr.String), normalizeSQLExpr(wantExpr); got != want {
		return false, fmt.Errorf("table %s: 生成列 %s 的表达式是 %q,与本迁移要求的 %q 不一致:"+
			"表达式不同意味着唯一键约束的根本不是同一个事实(例如把 NULLIF 写成了 IFNULL,GM 行会挤在同一个值上),拒绝继续",
			tableName, column, genExpr.String, wantExpr)
	}
	return true, nil
}

// normalizeSQLExpr 把 MySQL 回读的表达式与我们写下的 DDL 片段拉到同一形态再比:
// 服务端会重写大小写、反引号与空白(`NULLIF(`snapshot_guid`, 0)` 存成 `nullif(`snapshot_guid`,0)`),
// 逐字符比会在不同版本上乱红。只做这三项归一,不做 SQL 解析 —— 语义等价但写法不同的表达式
// (例如改成 CASE WHEN)**应该**判红:那是一次有意的 DDL 变更,必须走迁移而不是被字符串归一悄悄接受。
func normalizeSQLExpr(expr string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(expr) {
		if r == '`' || unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hasUniqueIndex 判定这个名字的索引在、且 **NON_UNIQUE = 0**(唯一键或主键)。
// 少了 NON_UNIQUE 这一条,一个**同名的非唯一索引**会一路放行:迁移在快路径直接 return nil
// (既不去重也不建唯一键),SnapshotStore 探测置 uniqueGuidKey=true、写路径切到 ODKU ——
// 而 ODKU 在没有唯一约束的表上永远撞不到重复键,同一 guid 静默落成多行。
// 同名但非唯一时返回错误而不是 false:false 会让调用方去 ADD UNIQUE KEY,撞上"索引名已存在"的 1061,
// 错误文案同样指不到真正的原因。
func hasUniqueIndex(ctx context.Context, db *sql.DB, dbName, tableName, indexName string) (bool, error) {
	var total, nonUnique int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(NON_UNIQUE), 0) FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ?`,
		dbName, tableName, indexName).Scan(&total, &nonUnique); err != nil {
		return false, fmt.Errorf("look up index %s on %s: %w", indexName, tableName, err)
	}
	if total == 0 {
		return false, nil
	}
	if nonUnique > 0 {
		return false, fmt.Errorf("table %s: 索引 %s 已存在但**不是唯一索引**(NON_UNIQUE=1):"+
			"它约束不到任何重复,而写路径会因为探到这个名字就以为唯一约束在,把同一个 guid 静默落成多行。"+
			"拒绝继续 —— 先人工确认这条索引是谁建的,确认可丢弃后 `ALTER TABLE %s DROP INDEX %s` 再重跑迁移",
			tableName, indexName, tableName, indexName)
	}
	return true, nil
}

// serverIsTiDB 判断对端是不是 TiDB。TiDB 的 VERSION() 形如 "8.0.11-TiDB-v7.5.0",
// 即它同时声称自己是 MySQL 8 —— 只看主版本号区分不出来,必须看这个后缀。
// 读不到版本号说明连接已经不可用,原样报错:这一步之后要下发 ALTER,不是猜的时候。
func serverIsTiDB(ctx context.Context, db *sql.DB) (bool, string, error) {
	var version string
	if err := db.QueryRowContext(ctx, `SELECT VERSION()`).Scan(&version); err != nil {
		return false, "", fmt.Errorf("read server version: %w", err)
	}
	return strings.Contains(strings.ToUpper(version), "TIDB"), version, nil
}

// TableMessages 返回全局库五张表的 proto 原型,顺序即迁移顺序。
// 末尾两张(id_segment、player_name)在 bootstrapTables 里,同步循环只对它们做列漂移检查。
func TableMessages() []proto.Message {
	return []proto.Message{
		&dbpb.TransactionLog{},
		&dbpb.PlayerSnapshot{},
		&dbpb.RollbackAuditLog{},
		&dbpb.IdSegment{},
		&dbpb.PlayerName{},
	}
}

// ddlTableNameRegex 从生成的建表语句里解出表名,用于启动期表名守卫。
var ddlTableNameRegex = regexp.MustCompile("CREATE TABLE IF NOT EXISTS `([^`]+)`")

// assertTableNameLocked 表名守卫,复制自
// go/db/internal/logic/pkg/proto_sql/db.go:371-391(go/db 的 build 在本机是红的,
// 且 data_service 不该依赖 go/db 的 internal 包)。
//
// proto2mysql 新版默认表名 = proto full name(含点号),一旦 OptionTableName 因任何
// 原因未被读到,schema sync 会静默建出新表,存量数据对业务"消失"。这里强校验每张表
// 生成 DDL 的表名与 proto 里声明的 OptionTableName 完全一致,不一致直接拒绝迁移。
func assertTableNameLocked(model *proto2mysql.DB, table proto.Message) error {
	md := table.ProtoReflect().Descriptor()
	declared, ok := proto2mysql.TableNameFromDescriptor(md)
	if !ok || declared == "" {
		return fmt.Errorf("表名守卫: 消息 %s 未声明 OptionTableName,拒绝按默认表名建表", md.FullName())
	}
	ddl := model.GetCreateTableSQL(table)
	m := ddlTableNameRegex.FindStringSubmatch(ddl)
	if m == nil {
		return fmt.Errorf("表名守卫: 无法从 %s 的建表语句中解析表名: %s", md.FullName(), ddl)
	}
	if m[1] != declared {
		return fmt.Errorf("表名守卫: 消息 %s 生成的表名 %q 与 proto 声明 %q 不一致,拒绝迁移", md.FullName(), m[1], declared)
	}
	return nil
}

// MigrateOptions 迁移路径除 DDL 之外还要做的事。
type MigrateOptions struct {
	// BootstrapTags 表就位之后用 INSERT IGNORE 预建的 id_segment 行(config.IdSegmentConfig.
	// EffectiveBootstrapTags)。nil = 不建行(只给测试用:生产与启动路径都必须传完整清单,
	// 因为运行期补种在生产是关着的,漏掉的 tag 会让 AllocateIdSegment 直接拒绝)。
	BootstrapTags []string

	// AllowMissingGuidUniqueKey 允许在**建不出** player_snapshot 的可空唯一键时继续迁移(目前只有 TiDB)。
	//
	// 默认 false = fail-closed:建不出来就让 MigrateSchema 失败。打开它等于明确接受
	// 「本实例的快照 guid 去重失效,同一个 guid 可以落成多行且零报错」,所以只能由**配置显式**打开,
	// 不允许任何代码路径"顺手"置 true;打开之后启动日志会逐条写明失效范围(见 ensureNullableUniqueKey)。
	//
	// 注:把它接到 yaml 配置项上要改 internal/config 与 internal/svc,不在本次改动范围内
	// (那两处归别的改动),所以现在它只有零值一条路 —— 即 TiDB 上迁移必然失败,这正是期望行为。
	AllowMissingGuidUniqueKey bool

	// AllowLegacyPlayerNamePrimaryKey 允许**跳过** player_name 的换主键迁移,让其余迁移照常跑完。
	//
	// 默认 false = 照常执行那条 ALTER。打开它等于明确接受「本实例的 player_name 仍是旧形态
	// PRIMARY KEY(player_id):功能完全正确,但名字抢注在生产常态那一路仍然成环(真库实测 5/5),
	// 靠有界 1213 重试吸收」。存在的理由是那条 ALTER 要**重建整张表**:表太大 / 窗口不够 /
	// 反复被长事务挡住 MDL 时,没有这个开关运维就只能整条 -migrate 都不跑,连带停掉另外四张表的
	// 列同步与 id_segment 地板校验 —— 那些是无关且必要的。与 AllowMissingGuidUniqueKey 同一形态的逃生口。
	//
	// 打开之后 NewPlayerNameStore 会探到旧形态、自动退回带 FORCE INDEX (uk_player_name) 的旧 Release 语句
	// (这条链路本来就有),锁序仍然一致;启动与迁移各打一条 Error 写明失效范围。
	//
	// 注:与 AllowMissingGuidUniqueKey 一样,把它接到 yaml 配置项上要改 internal/config 与 internal/svc,
	// 不在本次改动范围内,所以现在它只有零值一条路 —— 即默认必迁,这正是期望行为。
	AllowLegacyPlayerNamePrimaryKey bool

	// SnapshotGuidDedupeMaxRows 一次迁移最多删多少行重复快照;<=0 取 snapshotGuidDedupeMaxRows(10 万)。
	//
	// 做成选项而不是只留编译期常量:超上限的分支是**运维要处理的局面**,而常量只能靠重新编译发版来调,
	// 等于没有出路(见 dedupeBySourceColumn 里那条错误的文案)。
	SnapshotGuidDedupeMaxRows int64
}

// dedupeMaxRows 解出本次迁移的去重上限。
func (o MigrateOptions) dedupeMaxRows() int64 {
	if o.SnapshotGuidDedupeMaxRows > 0 {
		return o.SnapshotGuidDedupeMaxRows
	}
	return snapshotGuidDedupeMaxRows
}

// idSegmentFloorSources 迁移时能用消费表最大号给 id_segment 水位兜底的身份(raiseIdSegmentFloor)。
//
// 只有这两种:它们的号是消费表的**主键列**,一条 MAX() 就能查到"已发出的最大号"。
// 另外三种(item / txlog / snapshot)**做不了**这种地板校验:
//   - item guid 躺在 player_database 的玩家 blob(bag 组件)里,不是列,MySQL 查不到;
//   - tx_id / snapshot_id 虽然最终落 transaction_log / player_snapshot,但那两张表与 id_segment
//     同在全局库 —— 从备份恢复时一起回到旧水位,同库 MAX() 给不出独立证据;而真正的
//     "已发出"集合在 Kafka 积压(消费者会把恢复点之后的旧消息重放进来,与重发的新号撞
//     tx_id 主键,INSERT IGNORE 会静默丢掉**新**的流水)和 C++ 进程手里的段里。
//
// 所以恢复全局库是一次 ID 安全事件,不是普通的库恢复:动手之前必须按运维手册核对
// id_segment.max_id ≥ 每种身份的消费侧最大号(item 看各 zone 库 blob 的 bag 最大 guid,
// txlog / snapshot 看 Kafka 两个 topic 里的最大 tx_id / snapshot_id),不够就手工 UPDATE 抬上去。
// 本表的自动校验只是能查到时的一道兜底,查不到时只打 Info,不代表安全。
var idSegmentFloorSources = []idSegmentFloorSource{
	{bizTag: "player", table: "player_database", column: "player_id"},
	{bizTag: "guild", table: "guild", column: "guild_id"},
}

// MigrateSchema 打开一条短连接跑迁移,跑完即关。
// 启动路径(Schema.AutoMigrate=true)与显式入口(`data_service -migrate`)共用这一个函数,
// 生产用后者:多副本同时启动会对同一张表并发 ALTER,MDL 阻塞会让 GM 回滚/流水查询停摆。
func MigrateSchema(ctx context.Context, cfg MySQLConfig, opts MigrateOptions) error {
	db, err := openMySQL(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	return migrateSchemaOn(ctx, db, cfg.DBName, opts)
}

// migrateSchemaOn 注册五张表 → 表名守卫 → 预建 bootstrapTables(id_segment、player_name)
// → bootstrap 表只做形状守卫(列齐 + 单列唯一键还在)、其余三张 CreateOrUpdateTable
// → 补缺失索引 → 补 nullableUniqueKeys 的可空唯一列(player_snapshot.snapshot_guid_nz)
// → 预建 id_segment 行(BootstrapTags)→ player / guild 水位地板校验。
func migrateSchemaOn(ctx context.Context, db *sql.DB, dbName string, opts MigrateOptions) error {
	model := proto2mysql.NewDB().WithContext(ctx)
	if err := model.OpenDB(db, dbName); err != nil {
		return fmt.Errorf("proto2mysql open %s: %w", dbName, err)
	}

	tables := TableMessages()
	for _, t := range tables {
		model.RegisterTable(t)
		if err := assertTableNameLocked(model, t); err != nil {
			return err
		}
	}

	// 手写 DDL 的两个例外,必须在 CreateOrUpdateTable 之前(见各自常量注释)。
	// 按表名排序只为让日志与失败顺序稳定;两条建表语句互不依赖。
	bootstrapNames := make([]string, 0, len(bootstrapTables))
	for name := range bootstrapTables {
		bootstrapNames = append(bootstrapNames, name)
	}
	sort.Strings(bootstrapNames)
	for _, name := range bootstrapNames {
		if _, err := db.ExecContext(ctx, bootstrapTables[name]); err != nil {
			return fmt.Errorf("bootstrap table %s: %w", name, err)
		}
	}

	for _, t := range tables {
		name, _ := proto2mysql.TableNameFromDescriptor(t.ProtoReflect().Descriptor())
		if isBootstrapTable(name) {
			if err := assertColumnsPresent(ctx, db, dbName, t, name); err != nil {
				return err
			}
			// 存量 player_name 换主键(name_norm 当聚簇主键,根治审计 #12)。排在列检查之后、
			// 唯一键检查之前:列不齐时读到的形状没有意义,而换完键正好由下面那条复核。
			// 主键已经对了的库在这里收敛另外两把键(见 convergePlayerNameKeyShape)。
			if err := ensurePlayerNameKeyedByName(ctx, db, dbName, name, opts); err != nil {
				return err
			}
			// 与 assertColumnsPresent 同级的第二道形状守卫:列名对不代表键还在(见
			// bootstrapUniqueColumns 的注释)。两条都 fail-closed,拒绝带着坏形状启动。
			if err := assertUniqueColumns(ctx, db, dbName, name); err != nil {
				return err
			}
			logx.Infof("[schema] table %s pre-created by bootstrap DDL; columns and unique keys verified", name)
			continue
		}
		if err := warnIfLegacyTable(ctx, db, dbName, t, name); err != nil {
			return err
		}
		if err := model.CreateOrUpdateTable(t); err != nil {
			return fmt.Errorf("create or update table %s: %w", name, err)
		}
		// 索引必须在列同步之后补:snapshot_guid 这类新列正是上一步 ADD 出来的。
		if err := ensureIndexes(ctx, db, dbName, name, model.GetCreateTableSQL(t)); err != nil {
			return err
		}
		// 可空唯一列再排在索引之后:去重那一步要按源列分组删行,得先有 idx(snapshot_guid) 才不是全表扫。
		if err := ensureNullableUniqueKeys(ctx, db, dbName, name, opts); err != nil {
			return err
		}
		logx.Infof("[schema] table %s synced from proto", name)
	}

	// 行管理放在所有 DDL 之后:表齐了才有地方写行。先 bootstrap 再抬地板,顺序不能反 ——
	// 库被 drop 重建时 player 行是这一步刚种成 1 的,紧接着的地板校验把它抬回消费表最大号 +1。
	if err := bootstrapIdSegmentRows(ctx, db, opts.BootstrapTags); err != nil {
		return err
	}
	for _, src := range idSegmentFloorSources {
		if err := raiseIdSegmentFloor(ctx, db, dbName, src); err != nil {
			return err
		}
	}
	return nil
}

// ddlIndexRegex 从生成的建表语句里解出 `INDEX `名字` (`列`,`列`)` 子句。
var ddlIndexRegex = regexp.MustCompile("(?m)^\\s*INDEX `([^`]+)` \\(([^)]*)\\)")

// ddlIndexColRegex 解一条索引里的列名。
var ddlIndexColRegex = regexp.MustCompile("`([^`]+)`")

// wantedIndex 是一条"这张表上必须存在"的索引。
type wantedIndex struct {
	name string
	cols []string
}

// extraIndexes 是 proto 的 OptionIndex **还没有**、但读路径确实需要的索引。
//
// 唯一一条:player_snapshot(zone_id, created_at)。手写 DDL 时代它叫 idx_zone_created,
// 收口到 proto 时漏了,于是新建的库上 GetSnapshotPlayerIDsByZone / RollbackZone 的
// `WHERE zone_id = ? AND created_at <= ?` 变成全表扫 —— 回滚一个 zone 要扫完全服快照。
//
// 正确的长期修法是改 proto(本服务不拥有 proto/,由该目录的负责人来做):
//
//	proto/common/database/rollback_database_table.proto:64
//	  option(OptionIndex) = "player_id;snapshot_guid";
//	→  option(OptionIndex) = "player_id;snapshot_guid;zone_id,created_at";
//
// 改完这里这条会被"已被现有索引覆盖"的判断自动跳过(proto2mysql 会把它建成
// idx_player_snapshot_2),不需要再回来删,留着也不会建重复索引。
var extraIndexes = map[string][]wantedIndex{
	"player_snapshot": {{name: "idx_player_snapshot_zone_created", cols: []string{"zone_id", "created_at"}}},
}

// ensureIndexes 给**存量表**补索引,幂等。
//
// 为什么必须有这一步:proto2mysql 的 syncTableSchema 只在表**不存在**时执行完整的
// CREATE TABLE(索引在那条语句里);表已存在时它只 ADD/MODIFY/CHANGE COLUMN,
// 一条索引都不会建。于是任何一个"表已经由老版手写 DDL 建好"的库(以及所有从旧版本
// 升上来的环境),迁移之后列是齐的、索引是缺的 —— player_snapshot 少了 snapshot_guid
// 索引,消费者每落一条快照就全表扫一次,越跑越慢且没有任何报错。
//
// 覆盖判据是"前缀"而不是"同名":存量表的索引名是手写 DDL 起的(idx_zone_created),
// 与 proto2mysql 的 idx_<表名>_<序号> 对不上,但只要某条现有索引的**前若干列**正好是
// 想要的列序,优化器就能用,再建一条只是白占写入代价和空间。
func ensureIndexes(ctx context.Context, db *sql.DB, dbName, tableName, createDDL string) error {
	wanted := parseWantedIndexes(createDDL)
	wanted = append(wanted, extraIndexes[tableName]...)
	if len(wanted) == 0 {
		return nil
	}

	existing, err := loadIndexColumns(ctx, db, dbName, tableName)
	if err != nil {
		return err
	}

	for _, w := range wanted {
		if indexCovered(existing, w.cols) {
			continue
		}
		quoted := make([]string, len(w.cols))
		for i, c := range w.cols {
			quoted[i] = "`" + c + "`"
		}
		stmt := fmt.Sprintf("CREATE INDEX `%s` ON `%s` (%s)", w.name, tableName, strings.Join(quoted, ","))
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create index %s on %s: %w", w.name, tableName, err)
		}
		logx.Infof("[schema] table %s: created missing index %s (%s)", tableName, w.name, strings.Join(w.cols, ","))
		// 现建的这条也要进"已有"集合:同一次迁移里两条 wanted 可能互相覆盖。
		existing[w.name] = w.cols
	}
	return nil
}

// parseWantedIndexes 从 proto2mysql 生成的建表语句里取索引清单,这样索引的真源仍然只有
// proto 一处,名字也与"新库直接建出来"的完全一致。
func parseWantedIndexes(createDDL string) []wantedIndex {
	var out []wantedIndex
	for _, m := range ddlIndexRegex.FindAllStringSubmatch(createDDL, -1) {
		var cols []string
		for _, c := range ddlIndexColRegex.FindAllStringSubmatch(m[2], -1) {
			cols = append(cols, c[1])
		}
		if len(cols) > 0 {
			out = append(out, wantedIndex{name: m[1], cols: cols})
		}
	}
	return out
}

// loadIndexColumns 读线上表的全部索引(含 PRIMARY),值是按 SEQ_IN_INDEX 排好的列序。
func loadIndexColumns(ctx context.Context, db *sql.DB, dbName, tableName string) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT INDEX_NAME, COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		  ORDER BY INDEX_NAME, SEQ_IN_INDEX`, dbName, tableName)
	if err != nil {
		return nil, fmt.Errorf("list indexes of %s: %w", tableName, err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var idx, col string
		if err := rows.Scan(&idx, &col); err != nil {
			return nil, fmt.Errorf("scan indexes of %s: %w", tableName, err)
		}
		out[idx] = append(out[idx], col)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexes of %s: %w", tableName, err)
	}
	return out, nil
}

// indexCovered 判断 want 是否是某条现有索引的前缀列序(大小写不敏感:MySQL 列名如此)。
func indexCovered(existing map[string][]string, want []string) bool {
	for _, cols := range existing {
		if len(cols) < len(want) {
			continue
		}
		match := true
		for i, w := range want {
			if !strings.EqualFold(cols[i], w) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// warnIfLegacyTable 在同步一张**老手写 DDL 建出来的**表之前打一条显眼的日志。
//
// proto2mysql 按列注释里的 `pb:N` 认字段号;老表的列一个注释都没有,于是
// buildAlterClauses 会对**每一列**发 MODIFY COLUMN 来回填注释(顺带 longblob→mediumblob、
// varchar→mediumtext),等于一次整表重写。启动路径(Schema.AutoMigrate=true)只给这件事
// 5 分钟(svc.autoMigrateTimeout),表一大就会超时:进程侧三个 store 全置 nil、服务降级,
// MySQL 侧那条 ALTER 还在继续跑。
//
// 刻意**不**在这里改成"启动路径拒绝重写、只允许 -migrate":那会把所有存量 dev 库上的
// data_service 一次性锁死在 fail-closed 状态(store 全 nil = 回滚/流水/发号全不可用),
// 代价比它防的问题大。给的是一条能提前看见的信号 + 明确处方。
func warnIfLegacyTable(ctx context.Context, db *sql.DB, dbName string, table proto.Message, tableName string) error {
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME, COLUMN_COMMENT FROM INFORMATION_SCHEMA.COLUMNS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, dbName, tableName)
	if err != nil {
		return fmt.Errorf("list column comments of %s: %w", tableName, err)
	}
	defer rows.Close()

	unlabelled := 0
	total := 0
	for rows.Next() {
		var col, comment string
		if err := rows.Scan(&col, &comment); err != nil {
			return fmt.Errorf("scan column comments of %s: %w", tableName, err)
		}
		total++
		if !strings.HasPrefix(comment, "pb:") {
			unlabelled++
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate column comments of %s: %w", tableName, err)
	}
	if total > 0 && unlabelled > 0 {
		logx.Errorf("[schema] table %s has %d/%d columns without a pb:N comment (legacy hand-written DDL): "+
			"the next sync will MODIFY every one of them, i.e. rewrite the whole table. "+
			"Prefer running it once as `data_service -f <yaml> -migrate` (no time limit) instead of on the startup path (5 min cap).",
			tableName, unlabelled, total)
	}
	return nil
}

// assertColumnsPresent 漂移检查:proto 的每个字段在线上表都得有同名列。
// 只用于不走 CreateOrUpdateTable 的预建表,免得 proto 加了字段而 bootstrap DDL 忘了跟。
func assertColumnsPresent(ctx context.Context, db *sql.DB, dbName string, table proto.Message, tableName string) error {
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`,
		dbName, tableName)
	if err != nil {
		return fmt.Errorf("list columns of %s: %w", tableName, err)
	}
	defer rows.Close()

	have := make(map[string]bool)
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return fmt.Errorf("scan columns of %s: %w", tableName, err)
		}
		have[col] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate columns of %s: %w", tableName, err)
	}
	if len(have) == 0 {
		return fmt.Errorf("table %s is absent after bootstrap DDL", tableName)
	}

	fields := table.ProtoReflect().Descriptor().Fields()
	var missing []string
	for i := 0; i < fields.Len(); i++ {
		if name := string(fields.Get(i).Name()); !have[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("table %s lacks proto-declared columns %v; it is pre-created by bootstrap DDL (schema.go bootstrapTables), update that DDL", tableName, missing)
	}
	return nil
}

// assertUniqueColumns 形状检查:bootstrapUniqueColumns 里登记的每一列,在线上表都得有一条
// **恰好只含这一列**的唯一索引(UNIQUE KEY 或单列 PRIMARY KEY)。
// 与 assertColumnsPresent 一样只用于不走 CreateOrUpdateTable 的预建表,且同样 fail-closed:
// 这条键是业务不变量的唯一实现,缺了必须拒绝启动,而不是打一条警告接着跑(见 AGENTS.md §11.3
// "玩家资产和数据完整性路径默认 fail-closed")。
//
// 这里刻意不复用 loadIndexColumns:那个函数服务于 ensureIndexes 的"前缀覆盖"判据,不关心
// 唯一性;把 NON_UNIQUE 塞进它的返回值会让两个用途不同的判据挤在一个签名里。多一条
// INFORMATION_SCHEMA 查询只在迁移期跑一次,不值得为它把接口搅浑。
func assertUniqueColumns(ctx context.Context, db *sql.DB, dbName, tableName string) error {
	cols := bootstrapUniqueColumns[tableName]
	if len(cols) == 0 {
		return nil
	}

	uniqueIndexes, err := loadUniqueIndexColumns(ctx, db, dbName, tableName)
	if err != nil {
		return err
	}

	for _, want := range cols {
		if singleColumnUniqueIndex(uniqueIndexes, want) {
			continue
		}
		return fmt.Errorf("table %s has no single-column full-length UNIQUE index on %q "+
			"(a composite unique key does not count: it only makes the tuple unique; "+
			"a prefix index `%s(N)` does not count either: it would reject names that merely share a prefix); "+
			"the table is pre-created by bootstrap DDL (schema.go bootstrapTables) and this key is the only "+
			"enforcement of the invariant it guards, refusing to start on a table that cannot enforce it",
			tableName, want, want)
	}
	return nil
}

// uniqueIndexColumns 一条唯一索引的列清单。
// prefixed 记录其中**任何**一列带了前缀长度(SUB_PART);带前缀的索引不能当"整列唯一"用。
type uniqueIndexColumns struct {
	cols     []string
	prefixed bool
}

// loadUniqueIndexColumns 读一张表上全部 NON_UNIQUE=0 的索引(**含 PRIMARY**),
// 值是按 SEQ_IN_INDEX 排好的列序 + 是否带前缀长度。
//
// 两个调用方共用:assertUniqueColumns(判"这一列上有没有单列唯一约束")与
// ensurePlayerNameKeyedByName(要知道旧形态里承担名字唯一的**那条索引叫什么**)。
// "怎么读一张表的唯一索引"是同一条知识,抄第二份迟早在 SUB_PART 这类细节上漂移。
func loadUniqueIndexColumns(ctx context.Context, db *sql.DB, dbName, tableName string) (map[string]uniqueIndexColumns, error) {
	// SUB_PART 非 NULL = 这一列用了前缀长度,必须连列数一起读回来判(见 uniqueIndexColumns)。
	rows, err := db.QueryContext(ctx,
		`SELECT INDEX_NAME, COLUMN_NAME, SUB_PART FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND NON_UNIQUE = 0
		  ORDER BY INDEX_NAME, SEQ_IN_INDEX`, dbName, tableName)
	if err != nil {
		return nil, fmt.Errorf("list unique indexes of %s: %w", tableName, err)
	}
	defer rows.Close()

	uniqueIndexes := map[string]uniqueIndexColumns{}
	for rows.Next() {
		var idx, col string
		var subPart sql.NullInt64
		if err := rows.Scan(&idx, &col, &subPart); err != nil {
			return nil, fmt.Errorf("scan unique indexes of %s: %w", tableName, err)
		}
		e := uniqueIndexes[idx]
		e.cols = append(e.cols, col)
		e.prefixed = e.prefixed || subPart.Valid
		uniqueIndexes[idx] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unique indexes of %s: %w", tableName, err)
	}
	return uniqueIndexes, nil
}

// singleColumnUniqueIndexNames 返回 uniqueIndexes 里**只含 col 这一列、且不带前缀长度**的索引名
// (含 PRIMARY;列名大小写不敏感:MySQL 列名如此)。结果按名字排序,好让调用方的选择是确定的 ——
// map 遍历顺序随机,而调用方之一要据此下发 DROP INDEX。
//
// prefixed 必须连着整条索引一起判,不能在查询里 `AND SUB_PART IS NULL` 过滤掉带前缀的行 ——
// 那会把 UNIQUE KEY (name_norm, other(10)) 削成只剩一列,反而把一条**组合**唯一键误判成
// 单列唯一键,正好放过这个守卫要拦的形状。
func singleColumnUniqueIndexNames(uniqueIndexes map[string]uniqueIndexColumns, col string) []string {
	var names []string
	for name, idx := range uniqueIndexes {
		if !idx.prefixed && len(idx.cols) == 1 && strings.EqualFold(idx.cols[0], col) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// singleColumnUniqueIndex 判断 col 上有没有这样一条索引。
func singleColumnUniqueIndex(uniqueIndexes map[string]uniqueIndexColumns, col string) bool {
	return len(singleColumnUniqueIndexNames(uniqueIndexes, col)) > 0
}
