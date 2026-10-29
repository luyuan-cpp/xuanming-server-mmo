package barrierrestore

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"shared/savebarrier"
)

// 沿用正式proto2mysql建表及随机隔离库/键夹具；不手写ALTER补列或放宽真实TiDB门控。
// 此文件只是待执行候选，不代表旧库新增列迁移或真库恢复已通过。
func TestIntegrationBarrierRestoreSchemaGeneration(t *testing.T) {
	dsn, addr, strict := os.Getenv("BARRIER_RESTORE_TEST_DSN"), os.Getenv("BARRIER_RESTORE_TEST_REDIS"), os.Getenv("BARRIER_RESTORE_REQUIRE_TIDB")
	if dsn == "" && addr == "" && strict == "" {
		t.Skip("未启用独立schema_gen真TiDB/Redis恢复集成")
	}
	if dsn == "" || addr == "" || strict != "1" {
		t.Fatal("显式集成必须同时指定无库DSN、Redis地址和BARRIER_RESTORE_REQUIRE_TIDB=1")
	}
	f := newRestoreIntegrationFixture(t, dsn, addr)
	for _, tc := range []struct {
		name           string
		first, second  uint32
		existing, want string
	}{
		// 恢复以 SQL 为准:schema_gen = 两行戳最小值(0 按 1),覆盖 hash 里已有的值(混版本持久字段保护 §13 第 8 条)。
		{"legacy_zero", 0, 0, "", "1"},
		{"locked_sql_minimum", 4, 7, "", "4"},
		{"existing_higher_index_overwritten", 4, 7, "9", "4"},
		{"one_table_unstamped", 0, 7, "", "1"},
		{"uint32_maximum_preserved", 4294967295, 4294967295, "", "4294967295"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, first, second := f.player(t, 8, 90)
			playerID := strconv.FormatUint(id, 10)
			for i, table := range []string{"player_database", "player_database_1"} {
				gen := []uint32{tc.first, tc.second}[i]
				result, err := f.r.DB.ExecContext(f.ctx, "UPDATE "+table+" SET persist_schema_gen=? WHERE player_id=?", gen, playerID)
				require.NoError(t, err, "真实DDL须已具有正式世代列，不可临时补列绕过")
				// SQL默认0→0可能RowsAffected为0；独立全列读取与proto回读核对实际值。
				_, err = result.RowsAffected()
				require.NoError(t, err)
			}
			first.PersistSchemaGen, second.PersistSchemaGen = tc.first, tc.second
			before := f.assets(t, id)
			version := savebarrier.Version(8, 90)
			if tc.existing != "" {
				require.NoError(t, f.r.Redis.HSet(f.ctx, savebarrier.Key(id), "pending", version, "player_database", version, "player_database_1", version, "schema_gen", tc.existing).Err())
			}
			plan, err := f.r.Prepare(f.ctx, f.schema, f.zone, f.maintenance, []string{playerID}, false)
			require.NoError(t, err)
			for attempt := 0; attempt < 2; attempt++ {
				require.NoError(t, f.r.ApplyOne(f.ctx, plan, 0))
				fields, err := f.r.Redis.HGetAll(f.ctx, savebarrier.Key(id)).Result()
				require.NoError(t, err)
				require.Equal(t, map[string]string{"pending": version, "player_database": version, "player_database_1": version, "schema_gen": tc.want, "handoff_fence": assertRestoredFence(t, fields["handoff_fence"], "19", "", "0", strings.Repeat("0", 32))}, fields)
				ttl, err := f.r.Redis.PTTL(f.ctx, savebarrier.Key(id)).Result()
				require.NoError(t, err)
				require.Equal(t, -time.Nanosecond, ttl)
				require.Equal(t, before, f.assets(t, id), "恢复floor不能改SQL原戳或其它资产列")
				f.rowsEqual(t, first, second)
			}
		})
	}
}
