package main

// 玩家主数据跨 zone 库搬运(合服最关键、此前**完全缺失**的一步)。
//
// 事实基础(2026-09-08 实地核对,别信设计稿里的「零迁移」):
//   玩家主数据今天是**按 zone 分库**的。C++ scene 把存盘任务写进 Kafka topic
//   db_task_zone_{N}(go/db/internal/config.DbTaskTopic),go/db 的消费者把它
//   落进 MySQL 库 zone_{N}_db(同文件 ZoneDBName)。
//   docs/design/global-data-layer-tidb-decision.md 里那句「合服 =
//   RemapHomeZoneForMerge 改逻辑归属,零玩家主数据迁移」的前提是 **Phase 2
//   的全局 TiDB 层**,而 Phase 2 明确尚未落地。
//
//   所以在 Phase 2 之前,只改 player:zone:{id} 映射的合服 = 把玩家指到一个
//   **没有他任何行**的库上:登录时 go/db 在 zone_dst_db 查不到 player_id,
//   玩家看到的是一个空号。这是静默的全量数据丢失,而且映射已经改完,
//   连「他原来在哪个库」都查不回来(只能翻清单或备份)。
//
//   data_service 的 player:{id}:* blob(-migrate-player-blobs 拷的那些)
//   **不在**这条路径上:那是 data_service 自己的热缓存 / 跨区搬运通道,
//   C++ 玩法侧的存档走的是上面的 Kafka→MySQL 链。两者都要搬,缺一不可。
//
// 本步骤做什么:
//   1. 从 go/db 的建表清单 JSON(generated/data/mysql_database_table_list.json)
//      取表名全集,再用 information_schema 过滤出「本 zone 库里真实存在
//      且有 player_id 列」的表。今天恰好是 player_database /
//      player_database_1 / player_centre_database 三张(见
//      proto/common/database/mysql_database_table.proto 的
//      OptionPrimaryKey = "player_id"),但**不硬编码**:以后 proto 加表,
//      导表器更新 JSON,这里自动跟上。
//   2. 逐表:先查目标库有没有同 player_id 的行 —— 有就是 **ID 安全事件**
//      (两个 zone 发出了同一个 player_id,或这批人已经合过一次),立刻中止,
//      不写任何东西。
//   3. 逐表事务:分批
//      `INSERT INTO dst.t (col...) SELECT col... FROM src.t WHERE player_id IN (...)`。
//      列清单来自两库 information_schema 的**交集校验**(alignedPlayerColumns):
//      按列名对位,不按列序 —— 理由见那个函数的注释。
//      **不用** INSERT IGNORE / ON DUPLICATE KEY UPDATE:重复键必须炸,
//      静默跳过等于把两个玩家的数据合成一个。
//   4. 核对行数:dst 里这批 id 的行数必须等于 src 里的行数。
//   5. 删 go/db + login 的共享缓存键(DB 0),否则玩家登录会读到从旧库
//      读出来、缓存下来的那份,或者更糟:缓存里有、库里没有。
//
// 为什么用 information_schema 而不是解析 proto:
//   merge_zone 是独立 go.mod,不引主工程的 codegen。JSON 给「有哪些表」,
//   information_schema 给「哪些表真有 player_id 列」—— 后者是数据库的
//   现场事实,比任何静态推断都硬。两者取交集,少一边都会错:只看 JSON 会
//   把 user / user_accounts 这些账号表也搬走(它们没有 player_id,SQL 直接
//   报错);只看 information_schema 会把别人手工建的临时表也卷进来。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

// playerIDColumn 是 zone 库里「这是一张玩家表」的判据列名,与
// proto/common/database/mysql_database_table.proto 的
// option(OptionPrimaryKey) = "player_id" 一致。
const playerIDColumn = "player_id"

// playerRowsBatchSize 是一条 INSERT...SELECT 里 IN (...) 的 id 个数。
// 1000 个 uint64 ≈ 20KB SQL,远低于 max_allowed_packet 默认 64MB,
// 同时让单条语句的锁持有时间保持在毫秒级。
const playerRowsBatchSize = 1000

// mysqlErrDupEntry 是 MySQL 的 ER_DUP_ENTRY。它在本步骤里不是「可忽略的
// 并发冲突」,而是 ID 安全事件的信号 —— 单列出来是为了给运维一条能看懂的
// 错误信息,而不是一串 driver 原文。
const mysqlErrDupEntry = 1062

// playerRowsReport 是逐表拷贝的计数汇总。
type playerRowsReport struct {
	Tables       []string       // 实际处理的表(有序)
	SourceRows   map[string]int // 每表:源库里这批 id 的行数
	CopiedRows   map[string]int // 每表:实际 INSERT 的行数(dry-run 恒 0)
	TargetRows   map[string]int // 每表:拷完后目标库里这批 id 的行数
	MissingRows  map[string]int // 每表:源库里**没有**行的 id 数(正常:玩家没这张表的数据)
	CacheDeleted int            // 删掉的 DB 0 缓存键数
}

func newPlayerRowsReport() playerRowsReport {
	return playerRowsReport{
		SourceRows:  map[string]int{},
		CopiedRows:  map[string]int{},
		TargetRows:  map[string]int{},
		MissingRows: map[string]int{},
	}
}

func (r playerRowsReport) String() string {
	if len(r.Tables) == 0 {
		return "no player tables"
	}
	parts := make([]string, 0, len(r.Tables))
	for _, t := range r.Tables {
		parts = append(parts, fmt.Sprintf("%s(src=%d copied=%d dst=%d)",
			t, r.SourceRows[t], r.CopiedRows[t], r.TargetRows[t]))
	}
	return strings.Join(parts, " ") + fmt.Sprintf(" cache_keys_deleted=%d", r.CacheDeleted)
}

// ── 表发现 ────────────────────────────────────────────────────

// tableListFile 是 go/db 建表清单 JSON 的结构(generated/data/mysql_database_table_list.json,
// 由导表器从 mysql_database_table.proto 的 message 成员生成)。
type tableListFile struct {
	Messages []string `json:"messages"`
}

func loadTableListJSON(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read table list %s: %w", path, err)
	}
	var f tableListFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse table list %s: %w", path, err)
	}
	if len(f.Messages) == 0 {
		return nil, fmt.Errorf("table list %s has no messages", path)
	}
	return f.Messages, nil
}

// discoverPlayerTables 在 schema 里挑出「在 candidates 里 && 有 player_id 列」的表。
// 返回值有序(information_schema 的顺序不保证稳定,而事务顺序必须可复现)。
func discoverPlayerTables(ctx context.Context, db *sql.DB, schema string, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, errors.New("empty candidate table list")
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(candidates)), ",")
	args := make([]any, 0, len(candidates)+2)
	args = append(args, schema, playerIDColumn)
	for _, c := range candidates {
		args = append(args, c)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT TABLE_NAME FROM information_schema.COLUMNS
		  WHERE TABLE_SCHEMA = ? AND COLUMN_NAME = ? AND TABLE_NAME IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("discover player tables in %s: %w", schema, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// schemaNamePattern:MySQL 未加引号标识符里最保守的子集,长度上限 64 同 MySQL。
// 库名不是绑定参数,只能直接拼进语句,所以形状校验是这条拼接唯一的注入防线;
// -trade-schema / -guild-schema 共用它。
var schemaNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// assertSchemaExists 是目标区存在性的硬证据:zone_{N}_db 建过没有。
// 合服前 zone-up 过的 zone 一定有这个库(go/db 的 AutoCreateDatabase 或
// deploy/mysql-init/00_init_zone_dbs.sql 建的)。库不存在 = 目标 zone 根本
// 没部署过,把玩家指过去等于把他们指进虚空。
func assertSchemaExists(ctx context.Context, db *sql.DB, schema string) error {
	var name string
	err := db.QueryRowContext(ctx,
		"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", schema).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("database %s does not exist", schema)
	}
	if err != nil {
		return fmt.Errorf("look up schema %s: %w", schema, err)
	}
	return nil
}

// ── 行拷贝 ────────────────────────────────────────────────────

// countRowsForIDs 数某表里落在 ids 内的行数。分批统计,避免超长 IN。
func countRowsForIDs(ctx context.Context, db *sql.DB, qualified string, ids []uint64) (int, error) {
	total := 0
	for _, batch := range chunkUint64(ids, playerRowsBatchSize) {
		var n int
		q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s IN (%s)",
			qualified, playerIDColumn, inListLiteral(batch))
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return 0, fmt.Errorf("count %s: %w", qualified, err)
		}
		total += n
	}
	return total, nil
}

// copyPlayerRows 执行第 2~4 步。
//
// dryRun 只做只读部分(源行数 / 目标冲突检查),不开事务、不写。
func copyPlayerRows(
	ctx context.Context,
	db *sql.DB,
	srcSchema, dstSchema string,
	tables []string,
	ids []uint64,
	dryRun bool,
) (playerRowsReport, error) {
	rep := newPlayerRowsReport()
	rep.Tables = append(rep.Tables, tables...)
	if len(ids) == 0 {
		return rep, nil
	}

	for _, t := range tables {
		srcQ := srcSchema + "." + t
		dstQ := dstSchema + "." + t

		srcN, err := countRowsForIDs(ctx, db, srcQ, ids)
		if err != nil {
			return rep, err
		}
		rep.SourceRows[t] = srcN
		rep.MissingRows[t] = len(ids) - srcN

		// 目标库预检:这批 id 在目标库里已经有行 = 目标区已经有同号玩家。
		// 可能是 (a) 这次合服跑过一半,(b) 两个 zone 的发号器撞了号。
		// 两种都不能靠 INSERT 决定 —— 停下来让人看。
		dstPre, err := countRowsForIDs(ctx, db, dstQ, ids)
		if err != nil {
			return rep, err
		}
		if dstPre > 0 {
			return rep, fmt.Errorf(
				"ID SAFETY: %s already holds %d of the %d source player ids (table %s). "+
					"Either this merge already ran (resume with the manifest instead of a fresh run) "+
					"or the two zones issued colliding player_ids. Refusing to write",
				dstQ, dstPre, len(ids), t)
		}

		// 列对齐检查放在 dry-run **之前**:两个 zone 库的 proto2mysql 迁移没跑齐
		// (典型:一个库已加 player_database.asset_op_ledger,另一个还没)必须在
		// 维护窗口开始前就暴露出来,而不是等真写的时候才炸。
		cols, err := alignedPlayerColumns(ctx, db, srcSchema, dstSchema, t)
		if err != nil {
			return rep, err
		}

		if dryRun {
			log.Printf("[DRY-RUN] %s: would copy %d rows for %d ids (%d ids have no row), %d columns aligned by name",
				t, srcN, len(ids), rep.MissingRows[t], len(cols))
			continue
		}
		if srcN == 0 {
			rep.TargetRows[t] = 0
			continue
		}

		copied, err := copyOneTable(ctx, db, srcQ, dstQ, cols, ids)
		if err != nil {
			return rep, err
		}
		rep.CopiedRows[t] = copied

		dstN, err := countRowsForIDs(ctx, db, dstQ, ids)
		if err != nil {
			return rep, fmt.Errorf("verify: %w", err)
		}
		rep.TargetRows[t] = dstN
		if dstN != srcN {
			return rep, fmt.Errorf(
				"row count mismatch after copying %s → %s: source has %d rows for these ids, target has %d",
				srcQ, dstQ, srcN, dstN)
		}
		log.Printf("%s: copied %d rows (%d ids had no row in source)", t, copied, rep.MissingRows[t])
	}
	return rep, nil
}

// alignedPlayerColumns 返回逐列拷贝要用的列清单(源库的 ORDINAL_POSITION 顺序),
// 并在两库列集合不一致时 fail-closed。
//
// 为什么不再用 `INSERT ... SELECT *`(2026-09-18 改,
// docs/design/guild-phase2/04-asset-channel.md §4.42):
//
//	`SELECT *` 是按**列序**对位的。两个 zone 库虽然由同一套 proto2mysql 迁移建表,
//	但迁移是**逐库分别跑**的 —— 只要两边跑迁移的时刻或顺序不同(常态:先升级一个
//	zone 再升级另一个;或 B3a 的 player_profile 与 B4a 的 asset_op_ledger 在两个库
//	里先后不同),同一批新列就会落在不同的 ORDINAL_POSITION 上。
//	列数相同、列序不同时 MySQL **不报错**:它把源库 A 列的字节写进目标库 B 列。
//	两列都是 MEDIUMBLOB 时连类型检查都拦不住,结果是玩家的资产账本 blob 被写进
//	背包列 —— 静默的数据损坏,而且合服后才发现已经无从回滚。
//
//	按列名拷贝没有这个面。列集合不一致时下面显式报错,比驱动原文
//	"column count doesn't match value count" 更能说明该做什么。
func alignedPlayerColumns(ctx context.Context, db *sql.DB, srcSchema, dstSchema, table string) ([]string, error) {
	srcCols, err := tableColumns(ctx, db, srcSchema, table)
	if err != nil {
		return nil, err
	}
	dstCols, err := tableColumns(ctx, db, dstSchema, table)
	if err != nil {
		return nil, err
	}
	onlyInSrc := columnsNotIn(srcCols, dstCols)
	onlyInDst := columnsNotIn(dstCols, srcCols)
	if len(onlyInSrc) > 0 || len(onlyInDst) > 0 {
		return nil, fmt.Errorf(
			"SCHEMA MISMATCH: %s.%s and %s.%s do not have the same columns "+
				"(only in source: %v; only in target: %v). "+
				"Run the proto2mysql migration (go/db: `go run ./cmd/migrate -f <db.yaml> -command up`) "+
				"on BOTH zone databases up to the same revision, then re-run the merge",
			srcSchema, table, dstSchema, table, onlyInSrc, onlyInDst)
	}
	return srcCols, nil
}

// columnsNotIn 返回 a 里有、b 里没有的列名(保持 a 的顺序)。
func columnsNotIn(a, b []string) []string {
	have := make(map[string]struct{}, len(b))
	for _, c := range b {
		have[c] = struct{}{}
	}
	var out []string
	for _, c := range a {
		if _, ok := have[c]; !ok {
			out = append(out, c)
		}
	}
	return out
}

// copyOneTable 在**一个事务**里分批搬完一张表。整表要么全进要么全不进 ——
// 半张表的玩家数据比没有更难排查(有的组件在新库、有的在旧库)。
//
// cols 由 alignedPlayerColumns 产出:已确认两库列集合相同,可以按列名对位。
func copyOneTable(ctx context.Context, db *sql.DB, srcQ, dstQ string, cols []string, ids []uint64) (int, error) {
	if len(cols) == 0 {
		return 0, fmt.Errorf("refusing to copy %s → %s with an empty column list", srcQ, dstQ)
	}
	quoted := make([]string, 0, len(cols))
	for _, c := range cols {
		quoted = append(quoted, "`"+c+"`")
	}
	colList := strings.Join(quoted, ",")

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx for %s: %w", dstQ, err)
	}
	defer func() { _ = tx.Rollback() }() // 已 Commit 后是 no-op

	copied := 0
	for _, batch := range chunkUint64(ids, playerRowsBatchSize) {
		// 列名两侧都写出来:目标列清单固定了写入位置,源列清单固定了读取顺序,
		// 两库列序如何都不影响结果(见 alignedPlayerColumns)。
		q := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s IN (%s)",
			dstQ, colList, colList, srcQ, playerIDColumn, inListLiteral(batch))
		res, err := tx.ExecContext(ctx, q)
		if err != nil {
			var me *mysql.MySQLError
			if errors.As(err, &me) && me.Number == mysqlErrDupEntry {
				return copied, fmt.Errorf(
					"ID SAFETY: duplicate key inserting into %s (%v). "+
						"A source player_id already exists in the target zone — merge aborted, transaction rolled back", dstQ, me)
			}
			return copied, fmt.Errorf("insert into %s: %w", dstQ, err)
		}
		n, _ := res.RowsAffected()
		copied += int(n)
	}
	if err := tx.Commit(); err != nil {
		return copied, fmt.Errorf("commit %s: %w", dstQ, err)
	}
	return copied, nil
}

// deletePlayerRows 是 unmerge 用的反向操作:只删目标库里**与源库逐字节相同**
// 的那些行。任何一行在合服后被改过(玩家已经登录过、打过一场)都拒绝删除 ——
// 那时删掉的是合服后产生的新数据,不是「撤销」而是「毁尸」。
//
// 逐字节比较用 CHECKSUM 不行(表级),用 SELECT * 逐列比又要泛型 Scan;
// 这里用 MySQL 自己算:把两库的整行拼成一个可比较的串。
func deletePlayerRows(
	ctx context.Context,
	db *sql.DB,
	srcSchema, dstSchema, table string,
	ids []uint64,
	dryRun bool,
) (deleted int, refused []uint64, err error) {
	srcQ := srcSchema + "." + table
	dstQ := dstSchema + "." + table

	// 「逐字节相同」怎么判:不能用 CHECKSUM(表级)、也不能整行 JSON 化
	// (player_database 的列是 MEDIUMBLOB,JSON 化会炸内存)。按**列**比:
	// 取列名,拼一串 NULL-safe 的 `d.col <=> s.col AND ...` 交给 MySQL 算。
	cols, cerr := tableColumns(ctx, db, dstSchema, table)
	if cerr != nil {
		return 0, nil, cerr
	}
	conds := make([]string, 0, len(cols))
	for _, c := range cols {
		conds = append(conds, fmt.Sprintf("d.`%s` <=> s.`%s`", c, c))
	}

	for _, batch := range chunkUint64(ids, playerRowsBatchSize) {
		in := inListLiteral(batch)
		// JOIN 找出「目标库有、且与源库整行相同」的 id;不同的进 refused。
		q := fmt.Sprintf(
			`SELECT d.%s, (%s) AS identical
			   FROM %s d JOIN %s s ON d.%s = s.%s
			  WHERE d.%s IN (%s)`,
			playerIDColumn, strings.Join(conds, " AND "),
			dstQ, srcQ, playerIDColumn, playerIDColumn, playerIDColumn, in)
		rows, qerr := db.QueryContext(ctx, q)
		if qerr != nil {
			return deleted, refused, fmt.Errorf("compare %s vs %s: %w", dstQ, srcQ, qerr)
		}
		var identical []uint64
		for rows.Next() {
			var id uint64
			var same sql.NullBool
			if serr := rows.Scan(&id, &same); serr != nil {
				rows.Close()
				return deleted, refused, serr
			}
			if same.Valid && same.Bool {
				identical = append(identical, id)
			} else {
				refused = append(refused, id)
			}
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil {
			return deleted, refused, rerr
		}
		if len(identical) == 0 || dryRun {
			continue
		}
		res, derr := db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)",
			dstQ, playerIDColumn, inListLiteral(identical)))
		if derr != nil {
			return deleted, refused, fmt.Errorf("delete from %s: %w", dstQ, derr)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
	}
	return deleted, refused, nil
}

// tableColumns 返回表的列名(按 ORDINAL_POSITION)。
func tableColumns(ctx context.Context, db *sql.DB, schema, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM information_schema.COLUMNS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("columns of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s.%s has no columns (does it exist?)", schema, table)
	}
	return out, nil
}

// ── 缓存失效 ──────────────────────────────────────────────────

// defaultCacheMsgTypes 是 login 的 PlayerAllData 组装链会缓存的子消息名。
// 键形状 = `<proto message FullName>:<player_id>`:
//   - go/login/internal/logic/pkg/dataloader/data_loader.go::buildParentKey
//   - go/login/.../ensure_player_all_data_async.go 的 subKey
//   - go/db/internal/kafka/key_ordered_consumer.go::buildCacheKey
//
// proto/common/database/*.proto **没有** package 声明,所以 FullName 就是
// message 名本身(player_database 而不是 common.database.player_database)。
// 这几个名字与 zone 库表名同名不是巧合:表名就是 message 名。
var defaultCacheMsgTypes = []string{
	"PlayerAllData", // login 组装出来的父键
	"BagAllData",
	"QuestAllData",
	"MailAllData",
}

// playerCacheKeys 列出一名玩家在**共享 DB 0**上的全部缓存键。
// tables 传入实际发现的玩家表名(它们同时也是 MsgType),这样以后 proto
// 加表时缓存失效自动跟上,不需要再改这里。
func playerCacheKeys(pid uint64, tables []string) []string {
	pidStr := strconv.FormatUint(pid, 10)
	names := make([]string, 0, len(defaultCacheMsgTypes)+len(tables))
	names = append(names, defaultCacheMsgTypes...)
	names = append(names, tables...)
	out := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n+":"+pidStr)
	}
	return out
}

// invalidatePlayerCaches 删掉这批玩家在 DB 0 上的 go/db + login 缓存。
//
// 为什么必须删:这些键**不带 zone**,是全服共享的一份。合服前它们装的是从
// zone_src_db 读出来的内容;合服后玩家归属 dst,但只要键还在,login 的
// EnsurePlayerAllDataInRedisAsync 命中缓存就直接返回,永远不会去 dst 库读。
// 更糟的情况是 TTL 内玩家的存盘先落到 dst 库,再被这份旧缓存覆盖回去。
func invalidatePlayerCaches(
	ctx context.Context,
	rdb *redis.Client,
	ids []uint64,
	tables []string,
	dryRun bool,
) (int, error) {
	if rdb == nil || len(ids) == 0 {
		return 0, nil
	}
	deleted := 0
	for _, batch := range chunkUint64(ids, 200) {
		keys := make([]string, 0, len(batch)*(len(defaultCacheMsgTypes)+len(tables)))
		for _, pid := range batch {
			keys = append(keys, playerCacheKeys(pid, tables)...)
		}
		if dryRun {
			deleted += len(keys)
			continue
		}
		pipe := rdb.Pipeline()
		cmds := make([]*redis.IntCmd, 0, len(keys))
		for _, k := range keys {
			cmds = append(cmds, pipe.Del(ctx, k))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return deleted, fmt.Errorf("delete player cache keys: %w", err)
		}
		for _, c := range cmds {
			deleted += int(c.Val())
		}
	}
	return deleted, nil
}

// ── 小工具 ────────────────────────────────────────────────────

// chunkUint64 分批。批次边界只由长度决定,所以同一份有序 id 列表每次分法一致。
func chunkUint64(in []uint64, size int) [][]uint64 {
	if size <= 0 {
		size = 1
	}
	var out [][]uint64
	for start := 0; start < len(in); start += size {
		end := start + size
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[start:end])
	}
	return out
}

// inListLiteral 把 id 拼成 SQL IN 列表。**只接受 uint64**,所以不存在注入面:
// 数字直接格式化比占位符省一次协议往返,也让 EXPLAIN 出来的 SQL 可读。
func inListLiteral(ids []uint64) string {
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(id, 10))
	}
	return b.String()
}
