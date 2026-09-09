package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

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
// 列名不同、player_snapshot 在 zone 库与全局库各有一份)。唯一例外见 idSegmentBootstrapDDL。

// IdSegmentTableName 号段表名(与 proto OptionTableName 一致)。
const IdSegmentTableName = "id_segment"

// idSegmentBootstrapDDL 是本包**唯一**一段手写 DDL。
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

// TableMessages 返回全局库四张表的 proto 原型,顺序即迁移顺序。
func TableMessages() []proto.Message {
	return []proto.Message{
		&dbpb.TransactionLog{},
		&dbpb.PlayerSnapshot{},
		&dbpb.RollbackAuditLog{},
		&dbpb.IdSegment{},
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

// migrateSchemaOn 注册四张表 → 表名守卫 → 预建 id_segment → 其余三张 CreateOrUpdateTable
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

	// 手写 DDL 的唯一例外,必须在 CreateOrUpdateTable 之前(见常量注释)。
	if _, err := db.ExecContext(ctx, idSegmentBootstrapDDL); err != nil {
		return fmt.Errorf("bootstrap table %s: %w", IdSegmentTableName, err)
	}

	for _, t := range tables {
		name, _ := proto2mysql.TableNameFromDescriptor(t.ProtoReflect().Descriptor())
		if name == IdSegmentTableName {
			if err := assertColumnsPresent(ctx, db, dbName, t, name); err != nil {
				return err
			}
			logx.Infof("[schema] table %s pre-created by bootstrap DDL; columns verified against proto", name)
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
		return fmt.Errorf("table %s lacks proto-declared columns %v; it is pre-created by idSegmentBootstrapDDL, update that DDL", tableName, missing)
	}
	return nil
}
