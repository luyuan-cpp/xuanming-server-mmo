package data

// economy_lock_plan_mysql_test.go —— B5b 锁定读 / 点写的执行计划确定性回归
// (真库;GUILD_TEST_MYSQL_DSN 未设即 Skip,库名白名单照 openGuildIntegrationRepo)。
//
// ⚠ 全体 Skip 不代表通过:验收时 DSN 必须真的设上,交付里写明"看到的是 PASS 而不是 SKIP"。
// 选中本用例:`go test ./internal/data -run TestEconomyLockingStatementsArePrimaryKeyPointLookups`。
//
// # 为什么要有它
//
// friend 服务 2026-09-21 的事故(docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md §3.2 / §6.2):
// SQL 文本看上去是按主键加锁,优化器却把它规划成二级索引扫描,锁集越出本该串行化的那几行,
// 与"按主键删行"的事务在同一行上反序取锁,真死锁 1213。**锁集由执行计划决定,不由 SQL 文本决定**;
// 并发用例只能按概率撞上,EXPLAIN 每次都答得出来。
//
// 帮会这边最该拦的是 guild_member:主键 (guild_id, player_id) 之外另有唯一键 uk_guild_member(player_id)。
// sqlLockMemberRole(B2 的常量,B5b 复用)、sqlLockMemberBalance 与三条帮贡 UPDATE 的 WHERE 同时把这两个
// 唯一索引的全部列都钉死了 —— 正是事故报告 §5.1 所说"规则前提不成立"的情形:若被规划到 uk_guild_member,
// 取锁顺序是"二级索引 → 主键",而离帮 / 被踢 / 解散按主键删成员行是"主键 → 二级索引",同一行上反序成环。
//
// # 做法
//
//   - 对**生产代码里的 SQL 常量本身**做 EXPLAIN FORMAT=TRADITIONAL,测试里不另抄一份 SQL
//     (否则两边漂移时本用例就失去意义)。EXPLAIN 走文本协议,参数由 lockPlanInline 内联成字面量。
//   - 断言 key = PRIMARY,key_len = 主键全部列的字节数之和。key_len 不写死,由 lockPlanPrimaryKeyLen 从
//     INFORMATION_SCHEMA 现算(整数列按类型定长:BIGINT 8、INT 4;可空列再 +1)。按 proto2mysql 的类型映射
//     (uint64 → bigint unsigned NOT NULL,uint32 → int unsigned NOT NULL,枚举 → int NOT NULL),
//     当前应得:guild 8、guild_member 16、guild_daily_counter 20(8+4+4+4)、guild_asset_op 8。
//   - SELECT … FOR UPDATE 另断言 type = const。按主键的 UPDATE:MySQL 单表 UPDATE 走范围优化器,显示 range
//     (rows=1);若某个版本改走 join 优化器则显示 const —— 两者都接受,index / ALL / ref 一律红。
//   - 非锁定读 sqlSelectAssetOpImmutable 只断言 key 与 key_len。
//
// # 夹具为什么只有"被查的那一行"
//
// 小表是最坏情况(优化器在小表上最爱全扫描,测试库与新服刚开时恰恰是小表),所以**不灌数据去帮优化器**:
// guild / guild_daily_counter / guild_asset_op 各只插被点查的那一行,guild_member 是帮主 + 被查成员两行
// (seedManagedGuild 必带帮主)。
// 但被查的行**必须存在**,且满足语句里的其余条件:MySQL 的 const 判定在优化期就去读这一行 ——
// 读不到时 EXPLAIN 只给 "no matching row in const table",读到了但其余条件不成立时给
// "Impossible WHERE noticed after reading const tables",两种情况 table / key 都是 NULL,
// 用例会因为夹具而红、什么都没验到。所以下面每条语句的参数都与夹具行逐列对得上。
//
// # 不在本用例覆盖范围的两处(及原因)
//
//   - assetop.AllocateSeq 的两条锁定读(go/shared/assetop/seq.go,属聚宝斋会话,本批只调用、不改):
//     SQL 在 assetop 里按表名运行期拼接,没有导出常量,要 EXPLAIN 只能在测试里另抄一份 —— 那正是本用例要避免的;
//     而且未决行那条 `… player_id = ? AND stream = ? AND stream_epoch = ? AND status = ? ORDER BY seq LIMIT ? FOR UPDATE`
//     按设计就是 idx_guild_asset_op_2 等值前缀上的范围锁,不是主键点查,"断言 PRIMARY"本身就是错的期望。
//     索引列序由 TestAssetTablesShape 守;计划核对与改动归属见事故报告 §7.2(已登记,交用户决定)。
//   - accelerateDonationDeadlines(离帮 / 被踢 / 解散事务内的提前截止):`WHERE player_id IN (…) AND stream = ? …`
//     的 IN 列表按批次在运行期拼接,同样没有常量可 EXPLAIN;它按设计是按 (player_id, stream) 前缀的多行更新,
//     也不是主键点写。它在调用方已按锁序锁住 guild 行与成员行之后才执行;它实际选中的二级索引要在真实数据量下
//     另行 EXPLAIN 核对,不在"必须走 PRIMARY"这条断言的适用范围内。
//
// INSERT / upsert(sqlInsertAssetOp、sqlUpsertCounterWithLimit)也不在此列:EXPLAIN INSERT 不给出访问路径,
// 它们的锁由插入行的主键 / 唯一键值决定,与执行计划无关。

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	assetpb "proto/common/asset"
	pb "proto/guild"

	"guild/internal/constants"
)

// lockPlanCase 是一条被核对的生产 SQL。
type lockPlanCase struct {
	name      string
	sql       string
	args      []any
	table     string
	wantTypes []string // nil = 不断言 access type(非锁定读)
}

func TestEconomyLockingStatementsArePrimaryKeyPointLookups(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7801
		leader  uint64 = 8801
		member  uint64 = 8802
		goodsID uint32 = 101
		opID    uint64 = 9_780_001
		token   uint64 = 0x5a5a
		funds   uint64 = 25_000
		cost    uint64 = 20_000 // ≤ funds:sqlUpgradeGuild 的 `funds >= ?` 对夹具行成立
		balance uint64 = 100
		debit   uint64 = 10 // ≤ balance:sqlDebitContribution 的 `contribution_balance >= ?` 对夹具行成立
	)

	// 夹具:每张表只有被查的那一行(见文件头)。
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{member: constants.RoleMember})
	mustExec(t, f.ctx, f.db, "UPDATE guild SET funds=? WHERE guild_id=?", funds, guildID)
	econSetContribution(t, f, guildID, member, balance, balance)
	shopKind := int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP)
	mustExec(t, f.ctx, f.db,
		`INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms) VALUES (?, ?, ?, ?, ?, ?)`,
		member, shopKind, goodsID, econDayKey, 3, testNowMs)
	op := econShopRecord(t, opID, member, testNowMs, 1) // PENDING、lease_until_ms = 0
	op.GuildId, op.LeaseToken = guildID, token
	econInsertOp(t, f, op)

	// 枚举一律先转成整数:生成枚举带 String(),直接内联会被印成名字而不是库值。
	pending := PendingStatus()
	applied := int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED)
	outcomeApplied := uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED)
	outcomeRetry := uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY)
	outcomeUnknown := uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN)
	lockRead := []string{"const"}
	pointWrite := []string{"range", "const"}

	cases := []lockPlanCase{
		// guild(锁序位置 1)
		{"sqlLockGuildFunds", sqlLockGuildFunds, []any{guildID}, "guild", lockRead},
		{"sqlLockGuildForUpgrade", sqlLockGuildForUpgrade, []any{guildID}, "guild", lockRead},
		{"sqlUpgradeGuild", sqlUpgradeGuild,
			[]any{uint32(2), cost, uint32(35), guildID, uint32(1), cost}, "guild", pointWrite},
		{"sqlCreditGuildFunds", sqlCreditGuildFunds, []any{uint64(1000), guildID}, "guild", pointWrite},

		// guild_member(锁序位置 3):主键与 uk_guild_member 的列同时被钉死,必须选主键。
		{"sqlLockMemberBalance", sqlLockMemberBalance, []any{guildID, member}, "guild_member", lockRead},
		{"sqlLockMemberRole", sqlLockMemberRole, []any{guildID, member}, "guild_member", lockRead},
		{"sqlCreditContribution", sqlCreditContribution,
			[]any{uint64(10), uint64(10), guildID, member}, "guild_member", pointWrite},
		{"sqlRefundContribution", sqlRefundContribution, []any{uint64(10), guildID, member}, "guild_member", pointWrite},
		{"sqlDebitContribution", sqlDebitContribution, []any{debit, guildID, member, debit}, "guild_member", pointWrite},

		// guild_asset_op(锁序位置 6):领取 / 重排 / 毒行 / 终结 / 人工终结全是主键 CAS。
		{"sqlClaimAssetOp", sqlClaimAssetOp,
			[]any{testNowMs + econLeaseMs, token + 1, testNowMs, opID, pending, testNowMs}, guildAssetOpTable, pointWrite},
		{"sqlRescheduleAssetOp", sqlRescheduleAssetOp,
			[]any{testNowMs + 5000, uint32(0), outcomeRetry, uint32(0), testNowMs, opID, pending, token}, guildAssetOpTable, pointWrite},
		{"sqlPoisonAssetOp", sqlPoisonAssetOp,
			[]any{outcomeUnknown, testNowMs + 3_600_000, testNowMs, opID, token, pending}, guildAssetOpTable, pointWrite},
		{"sqlFinalizeAssetOp", sqlFinalizeAssetOp,
			[]any{applied, outcomeApplied, uint32(0), uint32(0), testNowMs, testNowMs, opID, pending}, guildAssetOpTable, pointWrite},
		{"sqlResolveAssetOp", sqlResolveAssetOp,
			[]any{applied, "ops", "lock-plan", testNowMs, testNowMs, opID, pending}, guildAssetOpTable, pointWrite},
		{"sqlSelectAssetOpImmutable", sqlSelectAssetOpImmutable, []any{opID}, guildAssetOpTable, nil},

		// guild_daily_counter(锁序位置 7):退次数 / 退限购按完整主键点更新。
		{"sqlRefundCounter", sqlRefundCounter,
			[]any{uint32(1), uint32(1), testNowMs, member, shopKind, goodsID, econDayKey}, "guild_daily_counter", pointWrite},
	}

	keyLen := map[string]string{}
	for _, table := range []string{"guild", "guild_member", "guild_daily_counter", guildAssetOpTable} {
		keyLen[table] = lockPlanPrimaryKeyLen(t, f.ctx, f.db, table)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmt := lockPlanInline(t, tc.sql, tc.args...)
			plan := lockPlanExplain(t, f.ctx, f.db, stmt)
			// 先确认计划行确实落在这张表上:夹具行缺失或其余条件不成立时 table / key 都是 NULL,Extra 会写原因。
			require.Equal(t, tc.table, plan["table"],
				"%s 的计划行不是 %s(Extra=%q):夹具行缺失或参数与夹具对不上,见文件头。SQL: %s",
				tc.name, tc.table, plan["Extra"], stmt)
			assert.Equal(t, "PRIMARY", plan["key"],
				"%s 没有走主键(possible_keys=%q type=%q):锁定语句必须是完整主键等值,"+
					"走二级索引就是'先二级后主键',与按主键删行的事务反序成环。SQL: %s",
				tc.name, plan["possible_keys"], plan["type"], stmt)
			assert.Equal(t, keyLen[tc.table], plan["key_len"],
				"%s 没有用满主键全部列(key=%q):锁集会扩到同前缀的其它行。SQL: %s", tc.name, plan["key"], stmt)
			if tc.wantTypes != nil {
				assert.Contains(t, tc.wantTypes, plan["type"],
					"%s 的 access type=%q,期望 %v(index / ALL 会逐行加锁扫过别人的行)。SQL: %s",
					tc.name, plan["type"], tc.wantTypes, stmt)
			}
		})
	}
}

// lockPlanExplain 跑一条 EXPLAIN FORMAT=TRADITIONAL,返回第一行的列名 → 值(NULL 记为空串)。
// 被测语句都是单表,第一行就是它的计划。
func lockPlanExplain(t *testing.T, ctx context.Context, db *sql.DB, stmt string) map[string]string {
	t.Helper()
	rows, err := db.QueryContext(ctx, "EXPLAIN FORMAT=TRADITIONAL "+stmt)
	require.NoError(t, err, stmt)
	defer rows.Close()

	cols, err := rows.Columns()
	require.NoError(t, err)
	require.True(t, rows.Next(), "EXPLAIN 没有返回任何行: %s", stmt)
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	require.NoError(t, rows.Scan(ptrs...))
	plan := make(map[string]string, len(cols))
	for i, col := range cols {
		plan[col] = vals[i].String
	}
	return plan
}

// lockPlanSafeString:允许内联进 SQL 的字符串形状(只有人工终结的操作人与理由会用到)。
var lockPlanSafeString = regexp.MustCompile(`^[A-Za-z0-9_-]*$`)

// lockPlanInline 把 SQL 里的 ? 依次替换成字面量,供 EXPLAIN 使用。
// 只接受整数与字母数字短串:参数全是 id / 状态码 / 毫秒时刻,外加人工终结的两个文本列;
// 出现别的类型(尤其是没转整数的生成枚举)说明用法错了,直接失败。
func lockPlanInline(t *testing.T, query string, args ...any) string {
	t.Helper()
	require.Equal(t, strings.Count(query, "?"), len(args), "占位符与参数个数不符: %s", query)
	for _, arg := range args {
		var literal string
		switch v := arg.(type) {
		case int, int32, int64, uint32, uint64:
			literal = fmt.Sprint(v)
		case string:
			require.Regexp(t, lockPlanSafeString, v, "只内联字母数字短串")
			literal = "'" + v + "'"
		default:
			t.Fatalf("lockPlanInline 不接受 %T(生成枚举先转成 int32 / uint32)", arg)
		}
		query = strings.Replace(query, "?", literal, 1)
	}
	return query
}

// lockPlanIntegerWidth:整数列在索引里的定长字节数(与 unsigned 无关)。
var lockPlanIntegerWidth = map[string]int{"tinyint": 1, "smallint": 2, "mediumint": 3, "int": 4, "bigint": 8}

// lockPlanPrimaryKeyLen 从 INFORMATION_SCHEMA 现算"用满主键全部列"时的 key_len:
// 逐列按类型取定长,可空列再加 1 字节 NULL 标志。
// 遇到非整数列直接失败:D-14 规定主键只用整数 / 枚举列,出现别的类型说明表结构变了,
// 要先回来确认本用例的期望还成不成立。两步查询(先取主键列名、再逐列取类型),不在 INFORMATION_SCHEMA 视图之间做 JOIN。
func lockPlanPrimaryKeyLen(t *testing.T, ctx context.Context, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = 'PRIMARY'
		  ORDER BY SEQ_IN_INDEX`, table)
	require.NoError(t, err, table)
	defer rows.Close() // 循环里 require 失败时也要释放连接;下面的显式 Close 重复调用无害
	var columns []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NotEmpty(t, columns, "%s 没有主键", table)

	total := 0
	for _, column := range columns {
		var dataType, nullable string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT DATA_TYPE, IS_NULLABLE FROM INFORMATION_SCHEMA.COLUMNS
			  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
			table, column).Scan(&dataType, &nullable), "%s.%s", table, column)
		width, ok := lockPlanIntegerWidth[strings.ToLower(dataType)]
		if !ok {
			t.Fatalf("%s 的主键列 %s 类型为 %s,不在整数定长表里", table, column, dataType)
		}
		total += width
		if strings.EqualFold(nullable, "YES") {
			total++
		}
	}
	return strconv.Itoa(total)
}
