package data

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAssetTablesShape 钉住资产三表经 schemamigrate 建出来的**键与索引**(真库;GUILD_TEST_MYSQL_DSN 未设即 Skip)。
//
// 为什么要对着真库断言,而不是只看 proto:
//   - 索引名是 proto2mysql 按声明顺序发的 idx_<表>_<n>,**从 0 起**。设计正文多处把第三组写成
//     idx_guild_asset_op_3(90 清单 X-02),手写 SQL 若照抄 FORCE INDEX 或排障时照着找,名字就是错的。
//   - uk_guild_asset_op 是四列复合唯一键,它是"同一序号至多一条指令"的最后一道防线;
//     建表库若悄悄退化成单列或漏建,重复指令要到对账才会被发现(90 清单 part2 §1 末条要求核对)。
//   - go/shared/assetop 的 AllocateSeq 用 (player_id, stream, stream_epoch, status) 等值 + seq 排序去锁未决行,
//     idx_guild_asset_op_2 的列序与之逐列一致,那条加锁读才是"只扫未决行";列序错了它会退回去扫该玩家的全部历史指令。
//   - 已存在的表 schemamigrate **不会**补建普通索引(只告警),所以这三张表第一次建就必须是对的。
func TestAssetTablesShape(t *testing.T) {
	ctx, db, _ := openGuildIntegrationRepo(t)

	type indexShape struct {
		columns []string
		unique  bool
	}
	want := map[string]map[string]indexShape{
		"guild_player_op_seq": {
			"PRIMARY": {[]string{"player_id", "stream"}, true},
		},
		"guild_asset_op": {
			"PRIMARY":              {[]string{"op_id"}, true},
			"uk_guild_asset_op":    {[]string{"player_id", "stream", "stream_epoch", "seq"}, true},
			"idx_guild_asset_op_0": {[]string{"status", "next_attempt_ms"}, false},
			"idx_guild_asset_op_1": {[]string{"guild_id", "op_id"}, false},
			"idx_guild_asset_op_2": {[]string{"player_id", "stream", "stream_epoch", "status", "seq"}, false},
		},
		"guild_daily_counter": {
			"PRIMARY":                   {[]string{"player_id", "counter_kind", "ref_id", "period_key"}, true},
			"idx_guild_daily_counter_0": {[]string{"period_key"}, false},
		},
	}

	for table, wantIndexes := range want {
		rows, err := db.QueryContext(ctx,
			`SELECT INDEX_NAME, COLUMN_NAME, NON_UNIQUE FROM INFORMATION_SCHEMA.STATISTICS
			  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
			  ORDER BY INDEX_NAME, SEQ_IN_INDEX`, table)
		require.NoError(t, err, table)
		got := map[string]indexShape{}
		for rows.Next() {
			var indexName, column string
			var nonUnique int
			require.NoError(t, rows.Scan(&indexName, &column, &nonUnique))
			shape := got[indexName]
			shape.columns = append(shape.columns, column)
			shape.unique = nonUnique == 0
			got[indexName] = shape
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())

		gotNames := make([]string, 0, len(got))
		for name := range got {
			gotNames = append(gotNames, name)
		}
		sort.Strings(gotNames)
		wantNames := make([]string, 0, len(wantIndexes))
		for name := range wantIndexes {
			wantNames = append(wantNames, name)
		}
		sort.Strings(wantNames)
		require.Equal(t, wantNames, gotNames, "%s 的索引集合", table)
		for name, shape := range wantIndexes {
			assert.Equal(t, shape.columns, got[name].columns, "%s.%s 的列序", table, name)
			assert.Equal(t, shape.unique, got[name].unique, "%s.%s 的唯一性", table, name)
		}
	}

	// 枚举列必须是整数列:业务 SQL 把生成常量当整数绑定;状态机的 CAS(WHERE status = ?)与索引 _0 / _2 都依赖它。
	for _, col := range []struct{ table, column string }{
		{"guild_asset_op", "kind"},
		{"guild_asset_op", "status"},
		{"guild_daily_counter", "counter_kind"},
	} {
		var dataType string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS
			  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
			col.table, col.column).Scan(&dataType), "%s.%s", col.table, col.column)
		assert.Equal(t, "int", dataType, "%s.%s 应为整数列", col.table, col.column)
	}
}
