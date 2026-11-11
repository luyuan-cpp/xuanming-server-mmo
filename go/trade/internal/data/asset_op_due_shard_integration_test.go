//go:build integration

package data

// asset_op_due_shard_integration_test.go —— 资产指令重投的到期扫描按玩家分片(缺口清单第 25 条,
// docs/design/assetop-redelivery-sharding-20261110.md)在真 MySQL 的 trade_asset_op 上的端到端回归。
//
// 跑法与门控同 asset_op_landing_integration_test.go:TRADE_TEST_MYSQL_DSN 为空即 Skip(⚠ 只能指向一次性库),
// 验收时再设 TRADE_REQUIRE_MYSQL_TESTS=1 把 Skip 判红。夹具复用 openLandingFixture / landingRow / landingLoop,
// 时间一律由 itNow 派生,不读真实墙钟(AGENTS §11.4)。

import (
	"testing"

	tradepb "proto/trade"

	"shared/workshard"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTick_ReplicasSplitDueRowsByPlayerShard:三个重投循环跑在同一份真库 outbox 上,前两个按玩家片互补分工,
// 第三个一片都不负责(成员表里没有它)。各自只领、只投自己片里的行,合起来每一行恰好投一次、全部终结;
// 一片都不负责的循环一行都不领。B 的行更早到期,排在队头,A 照样只拿到自己的那一行。
// 「条件在 LIMIT 之前」由录制驱动用例钉语句文本(asset_op_due_shard_test.go),guild 的真库用例 TestListDue_OwnedShardsOnly 钉批量效果。
func TestTick_ReplicasSplitDueRowsByPlayerShard(t *testing.T) {
	f := openLandingFixture(t)
	const kind = "trade-assetop"
	const playerA, playerB uint64 = 64*151 + 1, 64*151 + 2 // 片 1 与片 2
	earlier := func(r *tradepb.TradeAssetOpRecord) { r.NextAttemptMs = itNow - 50_000 }
	landingInsert(t, f,
		landingRow(t, 2101, playerA, landingDebit, 1, nil),
		landingRow(t, 2201, playerB, landingDebit, 1, earlier),
		landingRow(t, 2202, playerB, landingDebit, 2, earlier),
	)
	shardA := workshard.ShardKey(kind, workshard.ShardOf(playerA))

	idleApplier := &landingApplier{}
	idle := landingLoop(t, f, idleApplier, itNow)
	require.NoError(t, idle.BindWorkOwner(kind, func(string) bool { return false }))
	assert.Zero(t, idle.Tick(f.ctx), "一片都不负责的副本一行都不领")
	assert.Empty(t, idleApplier.requests())

	applierA, applierB := &landingApplier{}, &landingApplier{}
	loopA := landingLoop(t, f, applierA, itNow)
	loopA.Ledger = applierA
	require.NoError(t, loopA.BindWorkOwner(kind, func(key string) bool { return key == shardA }))
	loopB := landingLoop(t, f, applierB, itNow)
	loopB.Ledger = applierB
	require.NoError(t, loopB.BindWorkOwner(kind, func(key string) bool { return key != shardA }))

	assert.Equal(t, 1, loopA.Tick(f.ctx), "A 只领自己片里的那一行")
	assert.Equal(t, 2, loopB.Tick(f.ctx), "B 领其余片里的两行")
	assert.Zero(t, loopA.Tick(f.ctx), "终结之后没有到期行")
	assert.Zero(t, loopB.Tick(f.ctx), "终结之后没有到期行")

	for _, req := range applierA.requests() {
		assert.Equal(t, playerA, req.GetPlayerId(), "A 只投自己片里的玩家")
	}
	for _, req := range applierB.requests() {
		assert.Equal(t, playerB, req.GetPlayerId(), "B 只投自己片里的玩家")
	}
	assert.Len(t, applierA.requests(), 1, "每一行恰好投一次")
	assert.Len(t, applierB.requests(), 2, "每一行恰好投一次")
	for _, opID := range []uint64{2101, 2201, 2202} {
		rec := landingRecord(t, f, opID)
		assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, rec.GetStatus(), "op %d", opID)
		assert.Equal(t, uint32(0), rec.GetAttempts(), "op %d:第一次投递就终结,没有被别的副本抢走后重排", opID)
	}
}
