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
// 2026-09-21 起这几条都带 `FORCE INDEX (PRIMARY)`(死锁修复契约 P3);本用例验的是"带了之后计划确实是主键",
// 提示子句被误删、或某个版本不认它时这里会红。
//
// # 做法
//
//   - 对**生产代码里的 SQL 常量本身**做 EXPLAIN FORMAT=TRADITIONAL,测试里不另抄一份 SQL
//     (否则两边漂移时本用例就失去意义)。EXPLAIN 走文本协议,参数由 lockPlanInline 内联成字面量。
//   - 断言 key = PRIMARY,key_len = 主键全部列的字节数之和。key_len 不写死,由 lockPlanPrimaryKeyLen 从
//     INFORMATION_SCHEMA 现算(整数列按类型定长:BIGINT 8、INT 4;可空列再 +1)。按 proto2mysql 的类型映射
//     (uint64 → bigint unsigned NOT NULL,uint32 → int unsigned NOT NULL,枚举 → int NOT NULL),
//     当前应得:guild 8、guild_member 16、guild_player_op_seq 12(8+4)、guild_daily_counter 20(8+4+4+4)、guild_asset_op 8。
//   - SELECT … FOR UPDATE 另断言 type = const。按主键的 UPDATE:MySQL 单表 UPDATE 走范围优化器,显示 range
//     (rows=1);若某个版本改走 join 优化器则显示 const —— 两者都接受,index / ALL / ref 一律红。
//   - 非锁定读 sqlSelectAssetOpImmutable 只断言 key 与 key_len。
//
// # 夹具为什么只有"被查的那一行"
//
// 小表是最坏情况(优化器在小表上最爱全扫描,测试库与新服刚开时恰恰是小表),所以**不灌数据去帮优化器**:
// guild / guild_daily_counter / guild_player_op_seq 各只插被点查的那一行,guild_member 是帮主 + 被查成员两行(seedManagedGuild 必带帮主),
// guild_asset_op 是两行 —— 一行未决(领取 / 重排 / 毒行 / 终结 / 提前截止点改用)、一行保留期外的终态
// (清理点删用;两种语句要求的状态互斥,一行满足不了两边)。
// 但被查的行**必须存在**,且满足语句里的其余条件:MySQL 的 const 判定在优化期就去读这一行 ——
// 读不到时 EXPLAIN 只给 "no matching row in const table",读到了但其余条件不成立时给
// "Impossible WHERE noticed after reading const tables",两种情况 table / key 都是 NULL,
// 用例会因为夹具而红、什么都没验到。所以下面每条语句的参数都与夹具行逐列对得上。
//
// # 不在本用例覆盖范围的(及原因)
//
//   - assetop.AllocateSeq(go/shared/assetop/seq.go,不归本服务改):2026-09-21 起它对 guild_asset_op 只剩**普通读**
//     (未决行查询去掉了 FOR UPDATE,friend 审计 #4 修法 A),不加锁,不在"锁定语句必须走 PRIMARY"的范围里;
//     "不带锁定子句"由 seq_test.go 钉着。它唯一的锁定读是 seq 行 `WHERE player_id = ? AND stream = ? FOR UPDATE`:
//     guild_player_op_seq 只有 PRIMARY(player_id, stream)一个索引,完整主键等值没有第二条计划可选;
//     SQL 在 assetop 里按表名运行期拼接、没有导出常量,在这里另抄一份反而会与生产漂移。
//     guild 自己对同一行的守卫点锁(终结退款分支的 sqlLockSeqGuard,死锁复核 C6)是本包常量,已纳入下面的 EXPLAIN 回归。
//   - 各条**候选普通读**(提前截止的 sqlSelectAccelerateCandidates*、清理的 sqlListCleanupTerminalOps /
//     sqlListCleanupCounters、事务内建 seq 行前的 sqlSeqRowExists):不加锁,走哪个索引只影响性能不影响锁集。"不带锁定子句"由
//     economy_repo_test.go 的 TestEconomyCandidateReadsTakeNoLocks 钉着;真实数据量下的性能计划另行 EXPLAIN。
//   - 重排 / 毒行自 2026-09-21(G-C2)起包进显式 RC 事务:SQL 常量没变,下面的 sqlRescheduleAssetOp / sqlPoisonAssetOp
//     断言照旧有效;"是否在事务里"是 TiDB 上的提交方式问题,EXPLAIN 看不出来,由 economy_repo_test.go 的
//     TestEconomyLockOrder_BackgroundCASQueuesBehindPessimisticWriter 在 TiDB 上验。
//   - op 行的写者(提前截止 / 终结 / 人工终结 / 重排 / 毒行,死锁复核 V1)与清理(C5)在各自的点改 / 点删之前先跑同一条
//     主键点锁 sqlLockAssetOp;它与计数行清理的点锁 sqlLockCleanupCounter、两条点删都在下面。"点锁只写完整主键等值、
//     不带复核条件"(TiDB 快路径的前提)由 TestEconomyCandidateReadsTakeNoLocks 静态钉住,"写之前必先点锁"的调用顺序
//     由 point_lock_order_test.go 静态钉住(EXPLAIN 看不出语句先后)。
//   - INSERT / upsert(sqlInsertAssetOp、sqlUpsertCounterWithLimit、sqlEnsureSeqRow):EXPLAIN INSERT 不给出访问路径,
//     它们的锁由插入行的主键 / 唯一键值决定,与执行计划无关。
//
// 提前截止(accelerateDonationDeadlines)原先是 `WHERE player_id IN (…) AND stream = ? …` 的多行 UPDATE、
// 不在本用例范围;现在它的写只剩 sqlAccelerateDonationDeadline 这条主键点改,已纳入下面的断言。

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
		oldOpID uint64 = 9_780_002 // 保留期外的终态行,只给清理点删用
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
	// seq 行:终结退款分支的守卫点锁(sqlLockSeqGuard)要点到它。走生产的建行语句,纪元 = testNowMs。
	creditStream := uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT)
	mustExec(t, f.ctx, f.db, sqlEnsureSeqRow, member, creditStream, testNowMs, testNowMs)
	op := econShopRecord(t, opID, member, testNowMs, 1) // PENDING、lease_until_ms = 0
	op.GuildId, op.LeaseToken = guildID, token
	// 只为 sqlAccelerateDonationDeadline 的 `deadline_ms > ?` 对夹具行成立;其余语句不看这一列。
	// (点改的 WHERE 不看 kind,商店行做夹具不影响计划。)
	op.DeadlineMs = testNowMs + econDeadlineMs
	econInsertOp(t, f, op)
	old := econShopRecord(t, oldOpID, member, testNowMs, 2)
	old.GuildId = guildID
	old.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
	old.NextAttemptMs = testNowMs - 31*econDayMs // 终结时刻早于下面清理点删的截止参数 testNowMs
	old.UpdatedMs = old.NextAttemptMs
	econInsertOp(t, f, old)

	// 枚举一律先转成整数:生成枚举带 String(),直接内联会被印成名字而不是库值。
	pending := PendingStatus()
	applied := int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED)
	rejected := int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED)
	aborted := int32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED)
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
		// 这五条都带 FORCE INDEX (PRIMARY);sqlLockMemberRole 归 B2(guild_manage_repo.go),这里只复用它的常量。
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
		// 离帮 / 被踢 / 解散的提前截止:候选普通读之后逐行的主键点改(friend 审计 #5 / #10)。
		{"sqlAccelerateDonationDeadline", sqlAccelerateDonationDeadline,
			[]any{testNowMs, testNowMs, testNowMs, opID, pending, testNowMs}, guildAssetOpTable, pointWrite},
		// op 行写前的主键点锁(V1):提前截止 / 终结 / 人工终结 / 重排 / 毒行在各自的点改之前、同一事务里先跑它。
		// 同一条常量也用于清理(下一条,C5);两条夹具行各点一次,证明未决行与终态行上计划相同。
		{"sqlLockAssetOp(未决行)", sqlLockAssetOp, []any{opID}, guildAssetOpTable, lockRead},
		// 清理:终态行的逐行短事务 —— 先点锁(C5)、再点删(单表 DELETE 不接受索引提示,只能靠这里钉住它确实走主键)。
		{"sqlLockAssetOp(终态行)", sqlLockAssetOp, []any{oldOpID}, guildAssetOpTable, lockRead},
		{"sqlCleanupTerminalOp", sqlCleanupTerminalOp,
			[]any{oldOpID, applied, rejected, aborted, testNowMs}, guildAssetOpTable, pointWrite},

		// guild_player_op_seq(锁序位置 5):终结退款分支的计数行守卫(C6)。
		{"sqlLockSeqGuard", sqlLockSeqGuard, []any{member, creditStream}, guildPlayerOpSeqTable, lockRead},

		// guild_daily_counter(锁序位置 7):退次数 / 退限购按完整主键点更新;清理按 4 列完整主键先点锁(C5)再点删。
		{"sqlRefundCounter", sqlRefundCounter,
			[]any{uint32(1), uint32(1), testNowMs, member, shopKind, goodsID, econDayKey}, "guild_daily_counter", pointWrite},
		{"sqlLockCleanupCounter", sqlLockCleanupCounter,
			[]any{member, shopKind, goodsID, econDayKey}, "guild_daily_counter", lockRead},
		{"sqlCleanupCounter", sqlCleanupCounter,
			[]any{member, shopKind, goodsID, econDayKey, uint32(dayKeyFloor), econDayKey}, "guild_daily_counter", pointWrite},
	}

	keyLen := map[string]string{}
	for _, table := range []string{"guild", "guild_member", guildPlayerOpSeqTable, "guild_daily_counter", guildAssetOpTable} {
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
