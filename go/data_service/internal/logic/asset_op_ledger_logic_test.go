package logic

import (
	"context"
	"fmt"
	"testing"

	"data_service/internal/config"
	"data_service/internal/routing"
	"data_service/internal/svc"
	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	dbpb "proto/common/database"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// GetPlayerAssetOpLedger 的契约测试(docs/design/guild-phase2/07-rollback-fail-closed.md §7.10.2 L1–L5)。
// 全部走 miniredis + 真 Router(dev 单 Redis 模式),与 data_logic_test.go 同一套装配;不涉及时间。

const ledgerTestPlayerID uint64 = 4242

// ledgerTestKey 用与生产相同的推导(message 全名)拼 key,而不是写字面量 "PlayerAllData:…":
// 测试要钉的是"读的是 PlayerAllData 那把 key",不是某个会随 proto package 变化的字符串。
func ledgerTestKey(playerID uint64) string {
	return fmt.Sprintf("%s:%d", (&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName(), playerID)
}

// sampleLedger 造一份每个字段都非零的账本,逐字段比较才有意义。
func sampleLedger() *componentpb.PlayerAssetOpLedgerComp {
	seen := make([]uint64, 16)
	applied := make([]uint64, 16)
	seen[0] = 0b1011
	applied[0] = 0b0011
	return &componentpb.PlayerAssetOpLedgerComp{
		Streams: []*componentpb.AssetOpStreamLedger{{
			Stream:      assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
			Watermark:   100,
			SeenBits:    seen,
			AppliedBits: applied,
			Rejections:  []*componentpb.AssetOpRejection{{Seq: 104, ReasonTipId: 14001}},
			MaxSeq:      104,
			StreamEpoch: 7,
			PartialSeqs: []uint64{102},
		}},
	}
}

func putPlayerBlob(t *testing.T, mr *miniredis.Miniredis, key string, data *dbpb.PlayerAllData) {
	t.Helper()
	raw, err := proto.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, mr.Set(key, string(raw)))
}

// L1:key 不存在(redis.Nil)→ found=false,无 error,账本为 nil。
func TestGetPlayerAssetOpLedger_L1_AbsentBlob(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)

	ledger, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, ledgerTestPlayerID)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, ledger)
}

// L2:正常 → found=true,账本逐字段相等;blob 里没有账本时回空消息而不是 nil。
func TestGetPlayerAssetOpLedger_L2_ReturnsPersistedLedger(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	want := sampleLedger()
	putPlayerBlob(t, mr, ledgerTestKey(ledgerTestPlayerID), &dbpb.PlayerAllData{
		PlayerDatabaseData: &dbpb.PlayerDatabase{PlayerId: ledgerTestPlayerID, AssetOpLedger: want},
	})

	got, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, ledgerTestPlayerID)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, proto.Equal(want, got), "账本应逐字段相等: want=%v got=%v", want, got)

	t.Run("从未有过资产操作", func(t *testing.T) {
		const other uint64 = ledgerTestPlayerID + 1
		putPlayerBlob(t, mr, ledgerTestKey(other), &dbpb.PlayerAllData{
			PlayerDatabaseData: &dbpb.PlayerDatabase{PlayerId: other},
		})
		got, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, other)
		require.NoError(t, err)
		require.True(t, found)
		require.NotNil(t, got, "found=true 时账本必须非 nil(空消息),否则调用方分不清有没有 blob")
		assert.Empty(t, got.GetStreams())
	})
}

// L3:blob 的 player_id 与请求不符 → Internal(数据已矛盾,不能当"没有");
// player_database_data 缺席同样落进这一支。
func TestGetPlayerAssetOpLedger_L3_PlayerIDMismatchIsInternal(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	putPlayerBlob(t, mr, ledgerTestKey(ledgerTestPlayerID), &dbpb.PlayerAllData{
		PlayerDatabaseData: &dbpb.PlayerDatabase{PlayerId: ledgerTestPlayerID + 9, AssetOpLedger: sampleLedger()},
	})

	ledger, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, ledgerTestPlayerID)
	assert.Equal(t, codes.Internal, status.Code(err), "err=%v", err)
	assert.False(t, found)
	assert.Nil(t, ledger)

	t.Run("player_database_data 缺席", func(t *testing.T) {
		const other uint64 = ledgerTestPlayerID + 2
		putPlayerBlob(t, mr, ledgerTestKey(other), &dbpb.PlayerAllData{})
		_, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, other)
		assert.Equal(t, codes.Internal, status.Code(err), "err=%v", err)
		assert.False(t, found)
	})
}

// L4:坏字节 → Internal。
func TestGetPlayerAssetOpLedger_L4_CorruptBlobIsInternal(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	// 三个都带续位的字节 = 被截断的 varint tag,proto.Unmarshal 必然失败。
	require.NoError(t, mr.Set(ledgerTestKey(ledgerTestPlayerID), "\xff\xff\xff"))

	ledger, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, ledgerTestPlayerID)
	assert.Equal(t, codes.Internal, status.Code(err), "err=%v", err)
	assert.False(t, found)
	assert.Nil(t, ledger)
}

// L5:读的是 PlayerAllData 全名 key。只往 data_service 自己那套 player:{id}:<field> 写一份
// 合法的整份 blob,必须读不到(found=false);否则说明读错了 key 族。
func TestGetPlayerAssetOpLedger_L5_ReadsPlayerAllDataKeyOnly(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	blob := &dbpb.PlayerAllData{
		PlayerDatabaseData: &dbpb.PlayerDatabase{PlayerId: ledgerTestPlayerID, AssetOpLedger: sampleLedger()},
	}
	for _, field := range []string{"asset_op_ledger", "player_all_data", "PlayerAllData", "player_database"} {
		putPlayerBlob(t, mr, playerField(ledgerTestPlayerID, field), blob)
	}

	ledger, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, ledgerTestPlayerID)
	require.NoError(t, err)
	assert.False(t, found, "player:{id}:* 里的数据不得被当成已落盘账本")
	assert.Nil(t, ledger)

	// 反向对照:同一份 blob 放到 PlayerAllData 全名 key 上就能读到,证明上面的 false 不是别的原因。
	putPlayerBlob(t, mr, ledgerTestKey(ledgerTestPlayerID), blob)
	_, found, err = GetPlayerAssetOpLedger(context.Background(), svcCtx, ledgerTestPlayerID)
	require.NoError(t, err)
	assert.True(t, found)
}

// 语义表其余几行(§7.8.2):入参 0、home_zone 查不到、zone Redis 读失败都必须是 gRPC 错误,
// 绝不能退化成 found=false。
func TestGetPlayerAssetOpLedger_FailureRowsNeverReportAbsent(t *testing.T) {
	t.Run("player_id=0 → InvalidArgument", func(t *testing.T) {
		svcCtx, _ := newTestSvcCtx(t)
		_, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, 0)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "err=%v", err)
		assert.False(t, found)
	})

	t.Run("home_zone 查不到 → Unavailable", func(t *testing.T) {
		// 非 dev 模式:ClientForPlayer 必须先查映射。不配任何 Region,映射也不写。
		mr := miniredis.RunT(t)
		r := routing.NewRouter(config.Config{
			MappingRedis:     redis.RedisConf{Host: mr.Addr(), Type: "node"},
			PlayerLockTTLSec: 3,
		})
		t.Cleanup(func() { r.Close() })
		svcCtx := &svc.ServiceContext{Router: r}

		_, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, ledgerTestPlayerID)
		assert.Equal(t, codes.Unavailable, status.Code(err), "无映射: err=%v", err)
		assert.False(t, found)

		// 有映射但该 zone 没配 Redis 集群,同样是"路由不到"。
		// storageID = 0:不钉落点(与同包 setupPlayer 一致);本用例只要"有映射、zone 未配 Redis"。
		require.NoError(t, r.RegisterPlayerZone(context.Background(), ledgerTestPlayerID, 7, 0))
		_, found, err = GetPlayerAssetOpLedger(context.Background(), svcCtx, ledgerTestPlayerID)
		assert.Equal(t, codes.Unavailable, status.Code(err), "zone 未配置: err=%v", err)
		assert.False(t, found)
	})

	t.Run("zone Redis 读失败 → Internal", func(t *testing.T) {
		svcCtx, mr := newTestSvcCtx(t)
		mr.SetError("injected failure")
		t.Cleanup(func() { mr.SetError("") })

		_, found, err := GetPlayerAssetOpLedger(context.Background(), svcCtx, ledgerTestPlayerID)
		assert.Equal(t, codes.Internal, status.Code(err), "err=%v", err)
		assert.False(t, found)
	})

	t.Run("调用方已取消 → Canceled", func(t *testing.T) {
		svcCtx, _ := newTestSvcCtx(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, found, err := GetPlayerAssetOpLedger(ctx, svcCtx, ledgerTestPlayerID)
		assert.Equal(t, codes.Canceled, status.Code(err), "err=%v", err)
		assert.False(t, found)
	})
}
