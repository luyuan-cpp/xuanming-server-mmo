//go:build integration

package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 「键在、列坏」这个**中间态**的两条 fail-closed 回归(2026-09-29 复审补的阻断)。
//
// 它为什么会出现,而不是假想的:ensureNullableUniqueKey 的执行顺序是
// ADD COLUMN → ADD UNIQUE KEY → 回读复核(assertNullableUniqueKey)。
// 一个把 `GENERATED ALWAYS AS (...) STORED` 解析后**降级成普通列**的兼容层 / 代理
// (正是 assertNullableUniqueKey 头注声明的威胁模型)会让前两条 ALTER 都不报错:
// 列建成了一个恒为 NULL 的普通列,唯一键建在它上面也不冲突(NULL 不参与唯一约束)。
// 第三步复核这才判红 —— 那一次运行是 fail-closed 的,**但索引已经留在库里了**。
//
// 于是下一次运行(AutoMigrate 每次重启都跑)就成了真正的坑:只按索引名判断的话,
// 迁移会从快路径直接 return nil(迁移"成功"),SnapshotStore 探测置 uniqueGuidKey=true、
// 写路径切到 ODKU —— 而唯一键挂在一个恒为 NULL 的列上,ODKU 永远撞不到重复键,
// 同一个 guid 静默落成多行、全程零报错。一次 fail-closed 在重跑时翻成了 fail-open。
//
// 所以迁移的快路径与 store 的探测都必须把**列的形状**也校验一遍,这两条用例就是它们的回归。
// (同名非唯一索引 / 同名普通列那两条形态由 snapshot_store_integration_test.go 的
// TestSnapshotMigration_RefusesSameNamedNonUniqueIndex / ...SameNamedPlainColumn 覆盖,
// 它们走的是"列在、键不在"那条路,与这里不是同一条分支。)

// brokenGuidKeyOnPlainColumn 把表造成"键在、列坏"的中间态:先回到没有键没有列的起点,
// 再手工加一个同名的**普通列**,然后把唯一键建到它上面 —— 两条 DDL 都不会报错。
func brokenGuidKeyOnPlainColumn(t *testing.T, ctx context.Context, db *storetest.DB) {
	t.Helper()
	dropGuidUniqueKey(t, ctx, db)

	_, err := db.Raw.ExecContext(ctx, fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` bigint unsigned NULL",
		store.PlayerSnapshotTableName, store.SnapshotGuidNzColumn))
	require.NoError(t, err, "夹具:造一个同名的普通列(模拟兼容层把生成列降级)")

	_, err = db.Raw.ExecContext(ctx, fmt.Sprintf("ALTER TABLE `%s` ADD UNIQUE KEY `%s` (`%s`)",
		store.PlayerSnapshotTableName, store.SnapshotGuidUniqueKey, store.SnapshotGuidNzColumn))
	require.NoError(t, err, "夹具:把唯一键建到那个普通列上 —— 全 NULL 的列上建唯一键不会冲突,"+
		"这正是「键在、列坏」能被造出来的原因")
}

// TestSnapshotMigration_RefusesKeyPresentColumnBroken:键在、列是普通列时,
// 迁移必须**报错停住**,不许从快路径 return nil。
//
// 这一条拦的是"上一次 fail-closed 在重跑时翻成放行":放行之后写路径会切到 ODKU,
// 而唯一键挂在一个恒为 NULL 的列上,约束不到任何行。
func TestSnapshotMigration_RefusesKeyPresentColumnBroken(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if !newSnapshotStore(t, db).UniqueGuidKeyForTest() {
		t.Skipf("迁移没有建出 %s(TiDB?):本用例要先有键再把它换成坏形状,跳过(不代表通过)", store.SnapshotGuidUniqueKey)
	}
	brokenGuidKeyOnPlainColumn(t, ctx, db)

	err := store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{})
	require.Error(t, err, "「键在、列坏」必须让迁移失败 —— 只按索引名判断会直接跳过整步,"+
		"而那个唯一键挂在一个恒为 NULL 的普通列上,同一个 guid 会静默落成多行")
	assert.Contains(t, err.Error(), store.SnapshotGuidNzColumn, "错误里要指名是哪一列,运维才知道去处理什么")
}

// TestSnapshotStore_ProbeRejectsKeyPresentColumnBroken:同一个中间态下,
// 建 store 的探测必须按"唯一键不存在"处理(退回 NOT EXISTS 那条去重语句),不许切到 ODKU。
//
// 这里刻意**不**让 NewSnapshotStore 失败:退路那条语句在有键无键的表上都去重正确,
// fail-safe 到功能正确的一侧比拒启动更合适(与探测失败分支同口径)。
// 真正把人拦下来的是上面那条迁移用例 —— 它会让服务在 AutoMigrate 路径上起不来。
func TestSnapshotStore_ProbeRejectsKeyPresentColumnBroken(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if !newSnapshotStore(t, db).UniqueGuidKeyForTest() {
		t.Skipf("迁移没有建出 %s(TiDB?):本用例要先有键再把它换成坏形状,跳过(不代表通过)", store.SnapshotGuidUniqueKey)
	}
	brokenGuidKeyOnPlainColumn(t, ctx, db)

	ss, err := store.NewSnapshotStore(db.Cfg)
	require.NoError(t, err, "形状不符不该让建 store 失败:退路的 NOT EXISTS 去重在有键无键的表上都正确")
	defer ss.Close()
	assert.False(t, ss.UniqueGuidKeyForTest(),
		"探测把「键在、列坏」当成了唯一键可用:写路径会切到 ODKU,而那个键约束不到任何行 —— "+
			"同一个 guid 会静默落成多行,零报错")
}
