package logic

// 帮会经济 logic 层的单测(不连库;05-economy.md §5.24–§5.30,B5b 实现契约 §3)。
//
// 与 guild_manage_logic_test.go 同一套路:走不到仓储的分支一律把 repo / Repo 传 nil,
// 一旦前置顺序被改错、提前碰了库,就当场 nil panic —— 顺序正确是这里唯一能机械守住的东西。
// 需要 MySQL 的完整流程在 economy_flow_integration_test.go(build tag integration)。
//
// 共用替身:clientCtx / fakeHomeZones / newCacheOnlyRepo / seedGuild 在 client_zone_test.go,
// fakeFence 在 merge_fence_test.go,managementCall 在 guild_manage_logic_test.go。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/constants"
	"guild/internal/data"
	assetpb "proto/common/asset"
	pb "proto/guild"
	"shared/assetop"
)

// ── 测试替身 ──────────────────────────────────────────────────

// recordingNotifier 记下每一次推送。加锁:集成用例里 OnAssetFinalized 由重投循环的 worker 并发调用。
type recordingNotifier struct {
	mu   sync.Mutex
	sent []recordedPush
}

type recordedPush struct {
	change     *pb.GuildChangedS2C
	recipients []uint64
}

func (n *recordingNotifier) Notify(change *pb.GuildChangedS2C, recipients []uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, recordedPush{change: change, recipients: append([]uint64(nil), recipients...)})
}

// pushesOf 返回某一类推送的快照。
func (n *recordingNotifier) pushesOf(kind pb.GuildChangeKind) []recordedPush {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []recordedPush
	for _, p := range n.sent {
		if p.change.GetKind() == kind {
			out = append(out, p)
		}
	}
	return out
}

func (n *recordingNotifier) total() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.sent)
}

// countingMinter 是 op_id 发号器替身,记调用次数以证明"拒绝发生在发号之前"。
type countingMinter struct {
	id    uint64
	err   error
	calls int
}

func (m *countingMinter) Mint(context.Context) (uint64, error) {
	m.calls++
	return m.id, m.err
}

// newEconomyLogic 把经济依赖直接挂到字段上,不经 WithEconomy:这些用例走不到 Repo,
// 用零值依赖(Repo 为 nil)就能证明"在碰 Repo 之前就返回了"。
func newEconomyLogic(repo *data.GuildRepo, deps EconomyDeps) *GuildLogic {
	l := NewGuildLogic(repo, nil, nil, nil, nil)
	if deps.Now == nil {
		deps.Now = time.Now
	}
	l.economy = &deps
	return l
}

// economyRPCs 把五个经济 RPC 抹平成同一形状,好让公共前置用一张表覆盖全部入口。
func economyRPCs() []managementCall {
	return []managementCall{
		{"GetGuildDonateOptions", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.GetGuildDonateOptions(ctx, &pb.GetGuildDonateOptionsRequest{})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"DonateToGuild", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.DonateToGuild(ctx, &pb.DonateToGuildRequest{DonateId: 1})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"UpgradeGuild", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.UpgradeGuild(ctx, &pb.UpgradeGuildRequest{ExpectedLevel: 1})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"GetGuildShop", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.GetGuildShop(ctx, &pb.GetGuildShopRequest{})
			return resp.GetErrorMessage().GetId(), err
		}},
		{"BuyGuildShopGoods", func(l *GuildLogic, ctx context.Context) (uint32, error) {
			resp, err := l.BuyGuildShopGoods(ctx, &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 1})
			return resp.GetErrorMessage().GetId(), err
		}},
	}
}

// seedMembership 按 data 包的键契约(guild_repo.go 的 playerGuildKey)写"玩家 → 帮会"映射缓存。
func seedMembership(t *testing.T, mr *miniredis.Miniredis, playerID, guildID uint64) {
	t.Helper()
	require.NoError(t, mr.Set(fmt.Sprintf("player_guild:v2:%d", playerID), fmt.Sprintf("%d", guildID)))
}

// ── 接线与公共前置 ────────────────────────────────────────────

// TestEconomyRPCsUnavailableWithoutDeps:未接线时五个 RPC 回 Unavailable,而不是 nil 解引用崩掉整个进程。
func TestEconomyRPCsUnavailableWithoutDeps(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil)

	for _, tc := range economyRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.run(l, clientCtx(42))

			assert.Equal(t, codes.Unavailable, status.Code(err))
		})
	}
}

// TestWithEconomy:Repo 为 nil 不装配(半装配比不装配更危险);零值字段补默认。
func TestWithEconomy(t *testing.T) {
	t.Run("Repo 为 nil 不装配", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, nil, nil, WithEconomy(EconomyDeps{SyncBudget: time.Second}))

		assert.Nil(t, l.economy)
	})
	t.Run("零值字段补默认", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, nil, nil, WithEconomy(EconomyDeps{Repo: &data.EconomyRepo{}}))

		require.NotNil(t, l.economy)
		assert.NotNil(t, l.economy.Now)
		assert.Equal(t, 2500*time.Millisecond, l.economy.SyncBudget)
		assert.Equal(t, 10*time.Second, l.economy.Lease)
		assert.Nil(t, l.economy.Loop, "Loop 为 nil 表示资产通道关闭,不能被补成别的值")
	})
	t.Run("显式值原样保留", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, nil, nil, WithEconomy(EconomyDeps{
			Repo: &data.EconomyRepo{}, SyncBudget: time.Second, Lease: 30 * time.Second,
		}))

		require.NotNil(t, l.economy)
		assert.Equal(t, time.Second, l.economy.SyncBudget)
		assert.Equal(t, 30*time.Second, l.economy.Lease)
	})
}

// TestEconomyRPCsRequireClientSession:经济 RPC 的请求体里没有 player_id,内部调用拿不出可信身份,
// 放行只会让操作者变成 0 号玩家(R3)。
func TestEconomyRPCsRequireClientSession(t *testing.T) {
	l := newEconomyLogic(nil, EconomyDeps{})

	for _, tc := range economyRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.run(l, context.Background())

			assert.Equal(t, codes.PermissionDenied, status.Code(err))
		})
	}
}

// TestEconomyRPCsRefuseWhenSnapshotLacksCaller:映射说他在帮、快照里却没有他 → 未入帮,
// 且发生在碰经济仓储之前(Repo 为 nil,走到就崩)。不查归属 zone(R4):homeZones 为 nil 也不影响。
// 这一支还必须以 MySQL 复核映射(Y-01):MySQL 替身说他不在任何帮,映射键就得被失效 ——
// 不复核的话,非 0 的陈旧映射在整个 TTL 内都纠正不了。每个子用例重新写映射,保证五个入口都真的走到这一支。
func TestEconomyRPCsRefuseWhenSnapshotLacksCaller(t *testing.T) {
	repo, mr := newNoMembershipRepo(t)
	seedGuild(t, mr, data.GuildData{GuildID: 9, ZoneID: 2, Level: 1, Members: []data.MemberData{
		{PlayerID: 7, Role: constants.RoleLeader},
	}}, 0)
	l := newEconomyLogic(repo, EconomyDeps{})

	for _, tc := range economyRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			seedMembership(t, mr, 42, 9)

			id, err := tc.run(l, clientCtx(42))

			require.NoError(t, err)
			assert.Equal(t, constants.ErrNotInGuild, id)
			assert.False(t, mr.Exists("player_guild:v2:42"), "陈旧映射必须经 MySQL 复核后失效")
		})
	}
}

// noMembershipMySQL 是只会回"查无此行"的 MySQL 替身。一个类型同时充当 Connector / Driver / Conn / Stmt / Rows
// (五个接口的方法互不重名,Close 恰好同签名),只为让"快照里没有本人 → 以 MySQL 复核映射"这一支真的跑到
// VerifyPlayerGuildID,而不必起真库。任何写都回错误:经济前置走到写路径,说明顺序被改错了。
type noMembershipMySQL struct{}

func (noMembershipMySQL) Connect(context.Context) (driver.Conn, error) {
	return noMembershipMySQL{}, nil
}

func (noMembershipMySQL) Driver() driver.Driver {
	return noMembershipMySQL{}
}

func (noMembershipMySQL) Open(string) (driver.Conn, error) {
	return noMembershipMySQL{}, nil
}

func (noMembershipMySQL) Prepare(string) (driver.Stmt, error) {
	return noMembershipMySQL{}, nil
}

func (noMembershipMySQL) Begin() (driver.Tx, error) {
	return nil, errors.New("fake mysql: no writes expected on this path")
}

func (noMembershipMySQL) Close() error {
	return nil
}

func (noMembershipMySQL) NumInput() int {
	return -1
}

func (noMembershipMySQL) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("fake mysql: no writes expected on this path")
}

func (noMembershipMySQL) Query([]driver.Value) (driver.Rows, error) {
	return noMembershipMySQL{}, nil
}

func (noMembershipMySQL) Columns() []string {
	return []string{"guild_id"}
}

func (noMembershipMySQL) Next([]driver.Value) error {
	return io.EOF
}

// newNoMembershipRepo:缓存走 miniredis,MySQL 是 noMembershipMySQL(谁都不在任何帮)。
func newNoMembershipRepo(t *testing.T) (*data.GuildRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	db := sql.OpenDB(noMembershipMySQL{})
	t.Cleanup(func() {
		db.Close()
		rdb.Close()
	})
	return data.NewGuildRepo(rdb, db, time.Minute), mr
}

// TestAssetChannelDisabledRefusesBeforeMinting:资产通道关闭(Loop 为 nil,裁决 D)时捐献 / 兑换
// 在发号与建行之前就回 kGuildAssetPending,且不带订单视图。没有签名器就无法同步投递,
// 建出来的行只会卡在 PENDING、占着玩家的今日次数。
func TestAssetChannelDisabledRefusesBeforeMinting(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, data.GuildData{GuildID: 9, ZoneID: 2, Level: 10, Members: []data.MemberData{
		{PlayerID: 42, Role: constants.RoleLeader, ContributionBalance: 100000},
	}}, 0)
	seedMembership(t, mr, 42, 9)
	minter := &countingMinter{id: 777}
	l := newEconomyLogic(repo, EconomyDeps{OpIDs: minter})

	donate, err := l.DonateToGuild(clientCtx(42), &pb.DonateToGuildRequest{DonateId: 1})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrAssetPending, donate.GetErrorMessage().GetId())
	assert.Nil(t, donate.GetDonation(), "没有写入指令就不能回订单视图")

	buy, err := l.BuyGuildShopGoods(clientCtx(42), &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 1})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrAssetPending, buy.GetErrorMessage().GetId())
	assert.Nil(t, buy.GetOrder())

	assert.Zero(t, minter.calls, "通道关闭时不许发号:号段只进不退")
}

// ── 错误映射 ──────────────────────────────────────────────────

// TestEconomyTipMapping 把经济事务的错误 → tip 映射整张钉死(契约 §3.2)。
// 这张表是仓储哨兵变成玩家可见文案的唯一出口,错一格没有任何征兆。
// repo 传 nil 是安全的:会复核映射的几支都经 verifyMapping,它对 nil repo 直接返回。
func TestEconomyTipMapping(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil)
	ctx := context.Background()

	tips := []struct {
		name       string
		err        error
		wantID     uint32
		wantResult string
	}{
		{"合服闸门", data.ErrZoneMerging, constants.ErrZoneMerging, resultFence},
		{"闸门读失败(包了一层)", fmt.Errorf("%w: fence unreadable", data.ErrZoneMerging), constants.ErrZoneMerging, resultFence},
		{"帮会等级不足(捐献与商店共用)", data.ErrGuildLevelTooLow, constants.ErrShopLevelTooLow, resultLevel},
		{"今日次数用完", data.ErrDonateLimit, constants.ErrDonateLimit, resultLimit},
		{"限购已满", data.ErrShopLimit, constants.ErrShopLimit, resultLimit},
		{"帮贡不足", data.ErrContributionInsufficient, constants.ErrContributionInsufficient, resultInsufficient},
		{"已满级", data.ErrGuildMaxLevel, constants.ErrMaxLevel, resultLevel},
		{"资金不足", data.ErrFundsInsufficient, constants.ErrFundsInsufficient, resultInsufficient},
		{"未决过多(包了一层)", fmt.Errorf("allocate seq: %w", assetop.ErrTooManyPending), constants.ErrAssetPending, resultPendingGuard},
		// 以下交给 mapWriteErr(§11.4 的统一映射)。
		{"写冲突", data.ErrWriteConflict, constants.ErrBusyRetry, resultBusyRetry},
		{"帮会已不存在", data.ErrGuildGone, constants.ErrGuildNotFound, resultNotMember},
		{"已不是成员", data.ErrNotGuildMember, constants.ErrNotInGuild, resultNotMember},
		{"职位不足", data.ErrRankTooLow, constants.ErrRankTooLow, resultRank},
	}
	for _, tc := range tips {
		t.Run(tc.name, func(t *testing.T) {
			tip, result, err := l.economyTip(ctx, 42, 9, tc.err)

			require.NoError(t, err, "业务拒绝必须回 tip + nil error")
			require.NotNil(t, tip)
			assert.Equal(t, tc.wantID, tip.GetId())
			assert.Equal(t, tc.wantResult, result)
		})
	}

	t.Run("配表缺行是故障", func(t *testing.T) {
		tip, result, err := l.economyTip(ctx, 42, 9, data.ErrGuildLevelConfigMissing)

		assert.Nil(t, tip)
		assert.Equal(t, codes.Internal, status.Code(err))
		assert.Equal(t, resultError, result)
	})
	t.Run("未知错误原样返回", func(t *testing.T) {
		boom := errors.New("some unexpected failure")
		tip, result, err := l.economyTip(ctx, 42, 9, boom)

		assert.Nil(t, tip, "认不出的错误不能被翻成任何业务码")
		assert.ErrorIs(t, err, boom)
		assert.Equal(t, resultError, result)
	})
	t.Run("入参为 nil 时三者皆空", func(t *testing.T) {
		tip, result, err := l.economyTip(ctx, 42, 9, nil)

		assert.Nil(t, tip)
		assert.Empty(t, result)
		assert.NoError(t, err)
	})
}

// TestEconomyFence:与 mergeFenceTip 同一套三分支,区别只是返回 data.ErrZoneMerging 让事务回滚。
func TestEconomyFence(t *testing.T) {
	ctx := context.Background()

	t.Run("满足 data.FenceFunc", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, &fakeFence{merging: true}, nil)
		var fence data.FenceFunc = l.economyFence

		assert.ErrorIs(t, fence(ctx, 2), data.ErrZoneMerging)
	})
	t.Run("未配置闸门放行", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, nil, nil)

		assert.NoError(t, l.economyFence(ctx, 2))
	})
	t.Run("zone 为 0 不查", func(t *testing.T) {
		fence := &fakeFence{merging: true}
		l := NewGuildLogic(nil, nil, nil, fence, nil)

		assert.NoError(t, l.economyFence(ctx, 0))
		assert.Zero(t, fence.calls)
	})
	t.Run("合服中拒绝,查的是传入的事务内 zone", func(t *testing.T) {
		fence := &fakeFence{merging: true}
		l := NewGuildLogic(nil, nil, nil, fence, nil)

		assert.ErrorIs(t, l.economyFence(ctx, 7), data.ErrZoneMerging)
		assert.Equal(t, uint32(7), fence.lastZone)
	})
	t.Run("读不到按封锁处理", func(t *testing.T) {
		fence := &fakeFence{err: errors.New("dial tcp: connection refused")}
		l := NewGuildLogic(nil, nil, nil, fence, nil)

		assert.ErrorIs(t, l.economyFence(ctx, 7), data.ErrZoneMerging, "闸门读失败必须 fail-closed")
	})
	t.Run("未合服放行", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, &fakeFence{}, nil)

		assert.NoError(t, l.economyFence(ctx, 7))
	})
}

// ── 视图 ──────────────────────────────────────────────────────

// TestOrderViewOf:库状态 → 视图状态显式映射;PENDING 取 last_reason,终态取 reason_tip_id。
func TestOrderViewOf(t *testing.T) {
	const lastReason, terminalReason = assetop.ReasonInBattle, assetop.ReasonBlocked
	cases := []struct {
		name       string
		st         pb.GuildAssetOpStatus
		wantStatus pb.GuildAssetOrderStatus
		wantReason uint32
	}{
		{"结算中取最近一次暂时原因", pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
			pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, lastReason},
		{"已应用", pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED,
			pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, terminalReason},
		{"已拒绝取终态原因", pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED,
			pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED, terminalReason},
		{"已中止", pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED,
			pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_ABORTED, terminalReason},
		{"部分发放", pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL,
			pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED_PARTIAL, terminalReason},
		{"未指定", pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_UNSPECIFIED,
			pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_UNSPECIFIED, 0},
		{"未知值不许猜", pb.GuildAssetOpStatus(99),
			pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_UNSPECIFIED, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStatus, gotReason := orderViewOf(tc.st, lastReason, terminalReason)

			assert.Equal(t, tc.wantStatus, gotStatus)
			assert.Equal(t, tc.wantReason, gotReason)
		})
	}
}

// TestResultOfOrderCoversEveryViewStatus:指标 label 与视图状态一一对应,未知值归 error(label 集合有界)。
func TestResultOfOrderCoversEveryViewStatus(t *testing.T) {
	assert.Equal(t, resultPending, resultOfOrder(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING))
	assert.Equal(t, resultApplied, resultOfOrder(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED))
	assert.Equal(t, resultRejected, resultOfOrder(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED))
	assert.Equal(t, resultAborted, resultOfOrder(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_ABORTED))
	assert.Equal(t, resultAppliedPartial, resultOfOrder(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED_PARTIAL))
	assert.Equal(t, resultError, resultOfOrder(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_UNSPECIFIED))
}

// TestDonationRejectTip:只有 durable 的 REJECTED 才进 error_message;货币不足单独一句。
// PENDING(哪怕原因是余额不足、尚未落盘)不许进 error_message —— 客户端遇到非 0 tip 会中断刷新。
func TestDonationRejectTip(t *testing.T) {
	rejected := pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED

	assert.Equal(t, constants.ErrCurrencyInsufficient,
		donationRejectTip(rejected, assetop.ReasonCurrencyInsufficient).GetId())
	assert.Equal(t, constants.ErrAssetRejected, donationRejectTip(rejected, assetop.ReasonBlocked).GetId())
	assert.Equal(t, constants.ErrAssetRejected, donationRejectTip(rejected, 0).GetId())

	assert.Nil(t, donationRejectTip(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, assetop.ReasonCurrencyInsufficient))
	assert.Nil(t, donationRejectTip(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, 0))
	assert.Nil(t, donationRejectTip(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_ABORTED, 0))
	assert.Nil(t, donationRejectTip(pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED_PARTIAL, 0))
}

// TestDonationViewOf:货币与数额从 payload 解出;payload 坏了只缺这两个展示字段,其余照填。
func TestDonationViewOf(t *testing.T) {
	payload, err := proto.Marshal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{
		CurrencyType: 1,
		Amount:       100,
	}}})
	require.NoError(t, err)
	row := data.AssetOpRow{
		OpID:              5,
		RefID:             3,
		Kind:              pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE,
		Status:            pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		LastReason:        assetop.ReasonInBattle,
		ContributionDelta: 200,
		FundsDelta:        20000,
		CreatedMs:         77,
		Payload:           payload,
	}

	view := donationViewOf(row)
	assert.Equal(t, uint64(5), view.GetOpId())
	assert.Equal(t, uint32(3), view.GetDonateId())
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, view.GetStatus())
	assert.Equal(t, assetop.ReasonInBattle, view.GetReasonTipId())
	assert.Equal(t, uint32(1), view.GetCurrencyType())
	assert.Equal(t, uint64(100), view.GetCostAmount())
	assert.Equal(t, uint64(200), view.GetContributionGain())
	assert.Equal(t, uint64(20000), view.GetFundsGain())
	assert.Equal(t, uint64(77), view.GetCreatedMs())

	// 字段 1 的长度前缀是一个被截断的 varint(0xff 带续位却没有下一字节),proto.Unmarshal 必定失败。
	row.Payload = []byte{0x0a, 0xff}
	broken := donationViewOf(row)
	assert.Zero(t, broken.GetCostAmount())
	assert.Equal(t, uint64(200), broken.GetContributionGain(), "payload 坏了不影响其余字段")
}

// TestShopOrderViewOf:goods_id = ref_id,份数 = ref_count,总帮贡 = contribution_delta。
func TestShopOrderViewOf(t *testing.T) {
	view := shopOrderViewOf(data.AssetOpRow{
		OpID:              8,
		RefID:             101,
		RefCount:          3,
		Kind:              pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP,
		Status:            pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED,
		ContributionDelta: 90,
		ReasonTipID:       assetop.ReasonBlocked,
		CreatedMs:         66,
	})

	assert.Equal(t, uint64(8), view.GetOpId())
	assert.Equal(t, uint32(101), view.GetGoodsId())
	assert.Equal(t, uint32(3), view.GetCount())
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED, view.GetStatus())
	assert.Equal(t, uint64(90), view.GetCostContribution())
	assert.Equal(t, assetop.ReasonBlocked, view.GetReasonTipId())
	assert.Equal(t, uint64(66), view.GetCreatedMs())
}

// TestRecentResults:只取 10 分钟内进入终态、且属于本页的行,至多 5 条,保持新的在前。
func TestRecentResults(t *testing.T) {
	const atMs uint64 = 10_000_000
	applied := pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
	row := func(opID uint64, st pb.GuildAssetOpStatus, updatedMs, guildID uint64) data.AssetOpRow {
		return data.AssetOpRow{OpID: opID, Status: st, UpdatedMs: updatedMs, GuildID: guildID,
			Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE}
	}
	// 1 在窗口内;2 未终结;3 恰在窗口边界(保留);4 在窗口外;5 属于别的帮;
	// 6 部分发放也算终态;7、8 正常;9 因已满 5 条而不再取。
	rows := []data.AssetOpRow{
		row(1, applied, atMs-1000, 9),
		row(2, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, atMs, 9),
		row(3, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, atMs-600000, 9),
		row(4, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED, atMs-600001, 9),
		row(5, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL, atMs-10, 8),
		row(6, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL, atMs-10, 9),
		row(7, applied, atMs-10, 9),
		row(8, applied, atMs-10, 9),
		row(9, applied, atMs-10, 9),
	}
	keep := func(r data.AssetOpRow) bool { return r.GuildID == 9 }

	got := recentResults(rows, atMs, keep)

	ids := make([]uint64, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.OpID)
	}
	assert.Equal(t, []uint64{1, 3, 6, 7, 8}, ids)
	assert.Empty(t, recentResults(nil, atMs, keep))
	// 时钟比窗口还小(刚开服的测试环境)时不能下溢成一个巨大的截止值把一切都滤掉。
	assert.Len(t, recentResults([]data.AssetOpRow{row(1, applied, 5, 9)}, 100, keep), 1)
}

// TestExceptPlayer:升级推送不发给操作者本人(B2 §14)。
func TestExceptPlayer(t *testing.T) {
	assert.Equal(t, []uint64{7, 108}, exceptPlayer([]uint64{7, 42, 108}, 42))
	assert.Equal(t, []uint64{7, 42}, exceptPlayer([]uint64{7, 42}, 999))
	assert.Empty(t, exceptPlayer(nil, 42))
}

// ── 发号与令牌 ────────────────────────────────────────────────

// TestMintAssetOpID:号段取不到就整体失败,绝不用 0 或自造 id。
func TestMintAssetOpID(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil, nil)
	ctx := context.Background()

	t.Run("未接线", func(t *testing.T) {
		_, tip := l.mintAssetOpID(ctx, &EconomyDeps{}, 42)
		assert.Equal(t, constants.ErrIDGenUnavailable, tip.GetId())
	})
	t.Run("号段报错", func(t *testing.T) {
		minter := &countingMinter{err: errors.New("segment fenced")}
		_, tip := l.mintAssetOpID(ctx, &EconomyDeps{OpIDs: minter}, 42)
		assert.Equal(t, constants.ErrIDGenUnavailable, tip.GetId())
		assert.Equal(t, 1, minter.calls)
	})
	t.Run("号段发出 0", func(t *testing.T) {
		_, tip := l.mintAssetOpID(ctx, &EconomyDeps{OpIDs: &countingMinter{}}, 42)
		assert.Equal(t, constants.ErrIDGenUnavailable, tip.GetId(), "op_id=0 既不是合法主键也不是可追查的 correlation_id")
	})
	t.Run("正常", func(t *testing.T) {
		id, tip := l.mintAssetOpID(ctx, &EconomyDeps{OpIDs: &countingMinter{id: 123}}, 42)
		assert.Nil(t, tip)
		assert.Equal(t, uint64(123), id)
	})
}

// TestNewAssetOpTokenIsNonZero:0 在表里表示"没有租约",令牌必须恒非 0。
func TestNewAssetOpTokenIsNonZero(t *testing.T) {
	seen := make(map[uint64]struct{}, 64)
	for i := 0; i < 64; i++ {
		token, err := newAssetOpToken()
		require.NoError(t, err)
		require.NotZero(t, token)
		seen[token] = struct{}{}
	}
	assert.Greater(t, len(seen), 1, "令牌必须随机,可预测就可能与重投循环的令牌撞上")
}

// ── 同步投递预算(裁决 K)────────────────────────────────────────

// TestClampSyncBudget:budget = min(SyncBudget, 剩余 − 1000ms),不足 300ms 就跳过同步投递。
func TestClampSyncBudget(t *testing.T) {
	const full = 2500 * time.Millisecond
	cases := []struct {
		name       string
		remaining  time.Duration
		wantBudget time.Duration
		wantOK     bool
	}{
		{"预算充足时封顶在 SyncBudget", 3500 * time.Millisecond, full, true},
		{"预留事务吃掉一部分", 3000 * time.Millisecond, 2000 * time.Millisecond, true},
		{"恰好 300ms", 1300 * time.Millisecond, 300 * time.Millisecond, true},
		{"差 1ms 就跳过", 1299 * time.Millisecond, 299 * time.Millisecond, false},
		{"已经不够尾巴", 500 * time.Millisecond, -500 * time.Millisecond, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budget, ok := clampSyncBudget(full, tc.remaining)

			assert.Equal(t, tc.wantBudget, budget)
			assert.Equal(t, tc.wantOK, ok)
		})
	}
}

// TestSyncBudgetFor:预算从 ctx 截止时间倒推;没有截止(单测、内部调用)时按满额。
func TestSyncBudgetFor(t *testing.T) {
	budget, ok := syncBudgetFor(context.Background(), 2500*time.Millisecond)
	assert.True(t, ok)
	assert.Equal(t, 2500*time.Millisecond, budget)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, ok = syncBudgetFor(expired, 2500*time.Millisecond)
	assert.False(t, ok, "截止已过就不该再发同步投递")

	short, cancelShort := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancelShort()
	_, ok = syncBudgetFor(short, 2500*time.Millisecond)
	assert.False(t, ok, "剩 1.2s 时扣掉 1s 尾巴只剩 ≤200ms,不够一次正常往返")
}

// ── 推送 ──────────────────────────────────────────────────────

// TestOnAssetFinalizedPushKinds:异步终结只推本人;DONATE → FUNDS_CHANGED,
// SHOP / ACTIVITY_REWARD → DELIVERY_DONE(X-08:生成常量带前缀)。
func TestOnAssetFinalizedPushKinds(t *testing.T) {
	cases := []struct {
		name string
		kind pb.GuildAssetOpKind
		want pb.GuildChangeKind
	}{
		{"捐献", pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED},
		{"兑换", pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP, pb.GuildChangeKind_GUILD_CHANGE_KIND_DELIVERY_DONE},
		{"活动奖励", pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD, pb.GuildChangeKind_GUILD_CHANGE_KIND_DELIVERY_DONE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &recordingNotifier{}
			l := NewGuildLogic(nil, nil, nil, nil, nil, WithNotifier(n))

			l.OnAssetFinalized(context.Background(), data.FinalizedOp{
				OpID: 1, PlayerID: 42, GuildID: 9, Kind: tc.kind, Status: assetop.StatusApplied,
			})

			pushes := n.pushesOf(tc.want)
			require.Len(t, pushes, 1)
			assert.Equal(t, 1, n.total(), "只推一条")
			assert.Equal(t, uint64(9), pushes[0].change.GetGuildId())
			assert.Zero(t, pushes[0].change.GetActorPlayerId(), "异步终结没有操作者")
			assert.Equal(t, uint64(42), pushes[0].change.GetTargetPlayerId())
			assert.Equal(t, []uint64{42}, pushes[0].recipients, "只推本人,不广播全帮")
		})
	}
}

// TestOnAssetFinalizedSkipsSyncDelivery:同步投递里终结的不推 —— 调用方手上就是回包。
// 标记必须穿过 assetop 的 settleContext(context.WithoutCancel 只丢取消、保留值)。
func TestOnAssetFinalizedSkipsSyncDelivery(t *testing.T) {
	n := &recordingNotifier{}
	l := NewGuildLogic(nil, nil, nil, nil, nil, WithNotifier(n))
	op := data.FinalizedOp{OpID: 1, PlayerID: 42, GuildID: 9,
		Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, Status: assetop.StatusApplied}

	l.OnAssetFinalized(withSyncDelivery(context.Background()), op)
	settle, cancel := context.WithTimeout(context.WithoutCancel(withSyncDelivery(context.Background())), time.Second)
	defer cancel()
	l.OnAssetFinalized(settle, op)

	assert.Zero(t, n.total())
	assert.False(t, isSyncDelivery(context.Background()))
}

// TestOnAssetFinalizedUnknownKindDoesNotPush:推一个猜出来的类型,客户端会去拉错的页面。
func TestOnAssetFinalizedUnknownKindDoesNotPush(t *testing.T) {
	n := &recordingNotifier{}
	l := NewGuildLogic(nil, nil, nil, nil, nil, WithNotifier(n))

	for _, kind := range []pb.GuildAssetOpKind{pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_UNSPECIFIED, pb.GuildAssetOpKind(99)} {
		l.OnAssetFinalized(context.Background(), data.FinalizedOp{OpID: 1, PlayerID: 42, GuildID: 9, Kind: kind})
	}

	assert.Zero(t, n.total())
}
