package commerce

import (
	"context"
	"testing"
	"time"

	rfpb "proto/role_fence"
	tradepb "proto/trade"
	"shared/rolefence"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
)

// 删角收尾(docs/design/account-deletion.md §6.2):只删他的收藏;别人收藏同一挂单不动,seq 行与终态资产指令留着。
// 第二次调用没有可删的行,照样成功。
func TestTiDBRolePurgeDeletesFavoritesAndKeepsTradeRecords(t *testing.T) {
	e, install := tiDBEngineWithInstall(t)
	insertFenceFixture(t, install, "trade_favorite", &tradepb.TradeFavoriteRecord{PlayerId: 11, ListingId: 501, CreatedMs: 1})
	insertFenceFixture(t, install, "trade_favorite", &tradepb.TradeFavoriteRecord{PlayerId: 11, ListingId: 502, CreatedMs: 1})
	insertFenceFixture(t, install, "trade_favorite", &tradepb.TradeFavoriteRecord{PlayerId: 12, ListingId: 501, CreatedMs: 1})
	insertFenceFixture(t, install, "trade_player_op_seq", &tradepb.TradePlayerOpSeqRecord{PlayerId: 11, Stream: 3, NextSeq: 2, Epoch: 1, UpdatedMs: 1})
	insertFenceFixture(t, install, "trade_asset_op", &tradepb.TradeAssetOpRecord{OpId: 51, PlayerId: 11, Seq: 1, Status: tradepb.TradeAssetOpStatus(2)})
	db := e.Repo.DB
	server, err := rolefence.NewParticipantServer(db, RolePending, RolePurge, fenceCoordinatorSecret)
	require.NoError(t, err)
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		require.NoError(t, db.QueryRow(query, args...).Scan(&n))
		return n
	}
	for round := 0; round < 2; round++ {
		in := &rfpb.PurgeRemovedRoleRequest{State: fenceOf(11, "", 2, 0, 2)}
		require.NoError(t, serviceauth.Sign(rfpb.RoleFenceParticipant_PurgeRemovedRole_FullMethodName, in, rolefence.CoordinatorCaller, fenceCoordinatorSecret, uint64(time.Now().UnixMilli())))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := server.PurgeRemovedRole(ctx, in)
		cancel()
		require.NoError(t, err)
		require.False(t, out.GetRetry(), "清理应一次成功")
		require.Zero(t, count("SELECT COUNT(*) FROM trade_favorite WHERE player_id=11"))
		require.Equal(t, 1, count("SELECT COUNT(*) FROM trade_favorite WHERE player_id=12 AND listing_id=501"), "别人的收藏不动")
		require.Equal(t, 1, count("SELECT COUNT(*) FROM trade_player_op_seq WHERE player_id=11"), "seq 行永不删除")
		require.Equal(t, 1, count("SELECT COUNT(*) FROM trade_asset_op WHERE player_id=11"), "资产指令是对账记录,保留")
		fence, err := rolefence.Read(context.Background(), db, 11)
		require.NoError(t, err)
		require.Empty(t, fence.Account, "副本已合并成墓碑")
	}
}
