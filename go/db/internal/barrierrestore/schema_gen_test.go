package barrierrestore

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"shared/savebarrier"
)

// 使用既有Prepare/ApplyOne、双行锁录制驱动和真实Lua接缝；没有直接调用内部世代函数。
// 恢复以 SQL 为准:schema_gen = 两行戳最小值(0 按 1),即恢复后冷加载出的数据的加载世代(与 scene C3、login C4 同口径);
// hash 里已有的值只是索引,一律覆盖 —— 保留更高的旧值会让恢复后的第一次加载与存盘被判倒退(混版本持久字段保护 §13 第 8 条)。
func TestSchemaGenRestoreUsesSQLMinimumAndOverwritesIndex(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, existing, want string
		barrier                             bool
	}{
		{"missing_hash_legacy_zero", "0", "0", "", "1", false},
		{"missing_hash_two_sql_generations", "4", "7", "", "4", false},
		{"existing_three_fields", "7", "4", "", "4", true},
		{"lower_fourth_field_overwritten", "4", "7", "3", "4", true},
		{"higher_fourth_field_overwritten", "4", "7", "9", "4", true},
		{"one_table_unstamped", "0", "7", "", "1", false},
		{"sql_uint32_maximum", "4294967295", "4294967295", "", "4294967295", false},
		{"redis_uint32_maximum_overwritten", "4", "7", "4294967295", "4", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, store, cache := fixture(t, "8", "90")
			store.rows[0][4], store.rows[1][4] = []byte(tc.first), []byte(tc.second)
			version := savebarrier.Version(8, 90)
			if tc.barrier {
				cache.HSet(savebarrier.Key(7), "pending", version, "player_database", version, "player_database_1", version)
			}
			if tc.existing != "" {
				cache.HSet(savebarrier.Key(7), "schema_gen", tc.existing)
			}
			beforeRedis := cache.Dump()
			beforeRows := [2][]driver.Value{append([]driver.Value(nil), store.rows[0]...), append([]driver.Value(nil), store.rows[1]...)}
			plan := prepare(t, r, false)
			require.Equal(t, beforeRedis, cache.Dump(), "Prepare必须只读")
			for attempt := 0; attempt < 2; attempt++ {
				require.NoError(t, r.ApplyOne(context.Background(), plan, 0))
				for _, field := range []string{"pending", "player_database", "player_database_1"} {
					require.Equal(t, version, cache.HGet(savebarrier.Key(7), field), field)
				}
				require.Equal(t, tc.want, cache.HGet(savebarrier.Key(7), "schema_gen"))
				fields, err := r.Redis.HGetAll(context.Background(), savebarrier.Key(7)).Result()
				require.NoError(t, err)
				require.Len(t, fields, 5)
				assertRestoredFence(t, fields["handoff_fence"], "9", "", "0", strings.Repeat("0", 32))
			}
			require.Equal(t, beforeRows, store.rows, "世代索引恢复不能改SQL原戳或资产")
			require.Zero(t, store.commits)
		})
	}
}

// C++ 严格屏障会在 hash 里写写方令牌 schema_writer,warn 下判过倒退时还有证据字段 schema_regressed
// (混版本持久字段保护 §5.2)。恢复必须接受它们(否则任何经过新版 scene 存盘的玩家都无法恢复屏障),
// 写入时清掉:以 SQL 为准之后,任何写方的下一次严格屏障存盘都要重新比较,不能凭旧令牌跳过。
func TestSchemaGenRestoreAcceptsAndClearsWriterFields(t *testing.T) {
	r, store, cache := fixture(t, "8", "90")
	store.rows[0][4], store.rows[1][4] = []byte("4"), []byte("7")
	version := savebarrier.Version(8, 90)
	const writer = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	cache.HSet(savebarrier.Key(7), "pending", version, "player_database", version, "player_database_1", version,
		"schema_gen", "9", "schema_writer", writer, "schema_regressed", writer)
	plan := prepare(t, r, false)
	for attempt := 0; attempt < 2; attempt++ {
		require.NoError(t, r.ApplyOne(context.Background(), plan, 0))
		fields, err := r.Redis.HGetAll(context.Background(), savebarrier.Key(7)).Result()
		require.NoError(t, err)
		require.Equal(t, map[string]string{"pending": version, "player_database": version, "player_database_1": version,
			"schema_gen": "4", "handoff_fence": assertRestoredFence(t, fields["handoff_fence"], "9", "", "0", strings.Repeat("0", 32))}, fields)
	}
	require.Zero(t, store.commits)
}

// hash 里已有的 schema_gen 只是索引:格式损坏时恢复照样以 SQL 为准覆盖成两行最小值,不拒绝(拒绝的话唯一的修复手段
// 也用不了)。混版本持久字段保护 §13 第 8 条。
func TestSchemaGenRestoreOverwritesMalformedIndex(t *testing.T) {
	for _, value := range []string{"", "0", "01", "-1", "+1", "1.0", "1e1", " 1", "4294967296", "18446744073709551615"} {
		t.Run("schema_gen="+value, func(t *testing.T) {
			r, store, cache := fixture(t, "8", "90")
			store.rows[0][4], store.rows[1][4] = []byte("4"), []byte("7")
			plan := prepare(t, r, false)
			version := savebarrier.Version(8, 90)
			cache.HSet(savebarrier.Key(7), "pending", version, "player_database", version, "player_database_1", version, "schema_gen", value)
			require.NoError(t, r.ApplyOne(context.Background(), plan, 0))
			require.Equal(t, "4", cache.HGet(savebarrier.Key(7), "schema_gen"))
			require.Zero(t, store.commits)
		})
	}
}

func TestSchemaGenRejectsInvalidRedisFieldsWithoutWrites(t *testing.T) {
	for _, tc := range []struct{ field, value string }{
		{"future_field", "1"},
		{"outbox_version", savebarrier.Version(8, 90)}, {"outbox_generation", "1"},
	} {
		t.Run(tc.field+"="+tc.value, func(t *testing.T) {
			r, store, cache := fixture(t, "8", "90")
			store.rows[0][4], store.rows[1][4] = []byte("4"), []byte("7")
			plan := prepare(t, r, false)
			version := savebarrier.Version(8, 90)
			cache.HSet(savebarrier.Key(7), "pending", version, "player_database", version, "player_database_1", version, tc.field, tc.value)
			before := cache.Dump()
			require.Error(t, r.ApplyOne(context.Background(), plan, 0))
			require.Equal(t, before, cache.Dump(), "拒绝前不得补floor、删意图或改任一ACK")
			require.Zero(t, store.commits)
		})
	}
}

func TestSchemaGenStillRequiresBothExactCommittedAcknowledgments(t *testing.T) {
	for _, missing := range []string{"pending", "player_database", "player_database_1"} {
		t.Run(missing, func(t *testing.T) {
			r, store, cache := fixture(t, "8", "90")
			plan := prepare(t, r, false)
			version := savebarrier.Version(8, 90)
			for _, field := range []string{"pending", "player_database", "player_database_1"} {
				if field != missing {
					cache.HSet(savebarrier.Key(7), field, version)
				}
			}
			cache.HSet(savebarrier.Key(7), "schema_gen", "2")
			before := cache.Dump()
			require.Error(t, r.ApplyOne(context.Background(), plan, 0))
			require.Equal(t, before, cache.Dump())
			require.Zero(t, store.commits)
		})
	}
}

func TestSchemaGenRejectsInvalidOrMissingSQLStampBeforeRedisPublication(t *testing.T) {
	for _, value := range []driver.Value{nil, []byte(""), []byte("00"), []byte("01"), []byte("-1"), []byte("1.0"), []byte("4294967296")} {
		for table := 0; table < 2; table++ {
			t.Run(string(rune('0'+table))+"/"+schemaGenTestValueName(value), func(t *testing.T) {
				r, store, cache := fixture(t, "8", "90")
				store.rows[table][4] = value
				before := cache.Dump()
				_, err := r.Prepare(context.Background(), "zone_17_db", 17, "维护工单", []string{"7"}, false)
				require.ErrorContains(t, err, "SQL 持久化世代不是规范 uint32")
				require.Equal(t, before, cache.Dump())
				require.Zero(t, store.commits)
			})
		}
	}
	t.Run("missing_column", func(t *testing.T) {
		r, store, cache := fixture(t, "8", "90")
		store.columns = []string{"player_id", "fence_owner_epoch", "fence_save_seq", "payload"}
		before := cache.Dump()
		_, err := r.Prepare(context.Background(), "zone_17_db", 17, "维护工单", []string{"7"}, false)
		require.ErrorContains(t, err, "须先完成正常迁移")
		require.Equal(t, before, cache.Dump())
		require.Zero(t, store.commits)
	})
}

func schemaGenTestValueName(value driver.Value) string {
	if value == nil {
		return "SQL_NULL"
	}
	return string(value.([]byte))
}

func TestSchemaGenOriginalStampRemainsInPlanDigest(t *testing.T) {
	r, store, cache := fixture(t, "8", "90")
	plan := prepare(t, r, false)
	// 0和1的有效世代相同，但原字节变化仍须命中原全列摘要漂移拒绝。
	store.rows[0][4] = []byte("1")
	before := cache.Dump()
	require.ErrorContains(t, r.ApplyOne(context.Background(), plan, 0), "SQL 资产行偏离审定计划")
	require.Equal(t, before, cache.Dump())
	require.Zero(t, store.commits)
}

func TestSchemaGenLegacyMigrationRechecksStampUnderSecondRowLocks(t *testing.T) {
	r, store, cache := fixture(t, "0", "0")
	store.rows[0][4], store.rows[1][4] = []byte("4"), []byte("7")
	plan := prepare(t, r, true)
	store.onCommit = func() { store.rows[1][4] = []byte("8") }
	require.ErrorContains(t, r.ApplyOne(context.Background(), plan, 0), "SQL 资产行偏离审定计划")
	require.False(t, cache.Exists(savebarrier.Key(7)), "迁移后戳漂移不能发布任一ACK/floor")
	require.Equal(t, 1, store.commits, "保留已真实提交的围栏迁移，不能假装未发生")
	marker, err := cache.Get(MaintenanceKey(17))
	require.NoError(t, err)
	require.Equal(t, "维护工单", marker)
}

func TestSchemaGenRecoveryAcrossRedisSplitAndDBFirst(t *testing.T) {
	for _, dbFirst := range []bool{false, true} {
		name := "redis_split"
		if dbFirst {
			name = "db_first"
		}
		t.Run(name, func(t *testing.T) {
			r, store, _, _, route := splitFixture(t, dbFirst)
			store.rows[0][4], store.rows[1][4] = []byte("7"), []byte("4")
			plan, err := r.Prepare(context.Background(), "player_store_1000000_db", 17, "维护工单", []string{"7"}, true)
			require.NoError(t, err)
			for attempt := 0; attempt < 2; attempt++ {
				require.NoError(t, r.ApplyOne(context.Background(), plan, 0))
				got, err := r.Redis.HGetAll(context.Background(), savebarrier.Key(7)).Result()
				require.NoError(t, err)
				require.Equal(t, map[string]string{"pending": plan.Players[0].Target, "player_database": plan.Players[0].Target, "player_database_1": plan.Players[0].Target, "schema_gen": "4", "handoff_fence": assertRestoredFence(t, got["handoff_fence"], "9", "", "0", strings.Repeat("0", 32))}, got) // 两行 7 与 4 取最小值
			}
			require.Equal(t, 1, store.commits)
			if dbFirst {
				for i, query := range route.queries {
					require.Contains(t, query, []string{"route_reconcile", "route_meta", "route_shard=? AND player_id=?"}[i%3])
				}
			}
		})
	}
}
