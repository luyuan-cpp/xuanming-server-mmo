package logic

// 帮会经济 logic 层的单测(不连库;05-economy.md §5.24–§5.30,B5b 实现契约 §3)。
//
// 与 guild_manage_logic_test.go 同一套路:走不到仓储的分支一律把 repo / Repo 传 nil,
// 一旦前置顺序被改错、提前碰了库,就当场 nil panic —— 顺序正确是这里唯一能机械守住的东西。
// 需要 MySQL 的完整流程在 economy_flow_integration_test.go(build tag integration)。
//
// 配表:大多数用例依赖"未加载配表"的状态。"走到配表判定才拒绝"的几个用例(商店份数上限、未知捐献选项)
// 必须有真实配表,它们经 loadEconomyFlowTables 换上新建的管理器实例、用例结束换回原实例(见该函数注释),
// 不污染其它用例。该函数定义在本文件,集成文件与这里共用同一份。
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
	"path/filepath"
	"strings"
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
	"shared/generated/table"
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

// seedEconomyCallerCache 为"前置全过、拒绝只可能来自被测那条判定"的用例布好缓存:帮会 9 为 10 级
// (generated/tables/guildlevel.json 的最高级 id=10,任何捐献 / 商品的等级门槛都挡不住),帮主 42 帮贡充足。
// MySQL 为 nil:前置之后的任何碰库都会当场 panic。
func seedEconomyCallerCache(t *testing.T) *data.GuildRepo {
	t.Helper()
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, data.GuildData{GuildID: 9, ZoneID: 2, Level: 10, Members: []data.MemberData{
		{PlayerID: 42, Role: constants.RoleLeader, ContributionBalance: 100000},
	}}, 0)
	seedMembership(t, mr, 42, 9)
	return repo
}

// countingApplier 是单测用的假 scene(assetop.Applier):一律回 RETRY,只记调用次数。
// 集成文件里能按脚本作答的 scriptedScene 带 build tag integration,单测拿不到。加锁:Loop 的调用方可能并发。
type countingApplier struct {
	mu    sync.Mutex
	calls int
}

var _ assetop.Applier = (*countingApplier)(nil)

func (a *countingApplier) Do(context.Context, assetop.RPC, *assetpb.AssetOpRequest) (assetop.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY}, nil
}

func (a *countingApplier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// countingAssetStore 是 assetop.Store 的计数替身:单测里 Loop 只能经它碰"库",调用 0 次就证明
// 同步投递根本没开始。Reschedule 回 nil,让对照组(假 scene 回 RETRY → 重排)走完整条路径。
type countingAssetStore struct {
	mu    sync.Mutex
	calls int
}

var _ assetop.Store = (*countingAssetStore)(nil)

func (s *countingAssetStore) touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
}

func (s *countingAssetStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *countingAssetStore) ListDue(context.Context, uint64, int) ([]uint64, error) {
	s.touch()
	return nil, nil
}

func (s *countingAssetStore) Claim(context.Context, uint64, uint64, uint64, uint64, uint64) (assetop.Op, bool, error) {
	s.touch()
	return assetop.Op{}, false, nil
}

func (s *countingAssetStore) Finalize(context.Context, assetop.Op, assetop.Status, assetop.Result, uint64) (bool, error) {
	s.touch()
	return false, nil
}

func (s *countingAssetStore) Reschedule(context.Context, assetop.Op, uint64, assetop.Result, uint64) error {
	s.touch()
	return nil
}

// newCountingLoop 用真 assetop.Loop 包住两个计数替身:logic 只认 *assetop.Loop,
// Loop 非 nil 才能越过"资产通道关闭"那道前置,走到本文件要测的判定。
func newCountingLoop(t *testing.T) (*assetop.Loop, *countingApplier, *countingAssetStore) {
	t.Helper()
	applier := &countingApplier{}
	store := &countingAssetStore{}
	loop, err := assetop.NewLoop(assetop.DefaultLoopConfig(), store, applier, nil, time.Now)
	require.NoError(t, err)
	return loop, applier, store
}

// loadEconomyFlowTables 加载真实导表产物(仓库根 generated/tables),并跑一遍启动校验:
// 用例的数值(捐献 1 = 10000 银两 / 帮贡 10 / 资金 1000;商品 101 每份 30 帮贡、每日限 10)以它为准。
// 单测(本文件)与集成用例(economy_flow_integration_test.go)共用这一份;名字沿用集成文件里的旧名,
// economy_config_test.go 的文件头按名字引用它。
//
// 隔离:各 *TableManagerInstance 是包级变量,生产代码每次都经它现查、不缓存指针。所以这里换上新建的
// 管理器再加载,t.Cleanup 换回原实例,还原"未加载配表"的状态。换指针是普通赋值而非原子操作,成立的前提是:
// 本包用例都不调 t.Parallel,且调用它的用例结束时没有仍在读配表的 goroutine(Tick / ProcessOne 同步返回,
// 推送替身同步记录)。日后给本包用例加 t.Parallel 之前,先改掉这里。
func loadEconomyFlowTables(t *testing.T) {
	t.Helper()
	origRule := table.GuildRuleTableManagerInstance
	origLevel := table.GuildLevelTableManagerInstance
	origDonate := table.GuildDonateTableManagerInstance
	origShop := table.GuildShopTableManagerInstance
	origItem := table.ItemTableManagerInstance
	t.Cleanup(func() {
		table.GuildRuleTableManagerInstance = origRule
		table.GuildLevelTableManagerInstance = origLevel
		table.GuildDonateTableManagerInstance = origDonate
		table.GuildShopTableManagerInstance = origShop
		table.ItemTableManagerInstance = origItem
	})
	table.GuildRuleTableManagerInstance = table.NewGuildRuleTableManager()
	table.GuildLevelTableManagerInstance = table.NewGuildLevelTableManager()
	table.GuildDonateTableManagerInstance = table.NewGuildDonateTableManager()
	table.GuildShopTableManagerInstance = table.NewGuildShopTableManager()
	table.ItemTableManagerInstance = table.NewItemTableManager()

	// 下面的方法值在换指针**之后**求值,绑定的是新实例。
	dir := filepath.Join("..", "..", "..", "..", "generated", "tables")
	loaders := []struct {
		name string
		load func(configDir string, useBinary bool) error
	}{
		{"GuildRule", table.GuildRuleTableManagerInstance.Load},
		{"GuildLevel", table.GuildLevelTableManagerInstance.Load},
		{"GuildDonate", table.GuildDonateTableManagerInstance.Load},
		{"GuildShop", table.GuildShopTableManagerInstance.Load},
		{"Item", table.ItemTableManagerInstance.Load},
	}
	for _, l := range loaders {
		require.NoError(t, l.load(dir, false), l.name)
	}
	require.NoError(t, ValidateEconomyTables())
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

// TestEconomyRPCsRefuseWhenSnapshotLacksCaller:映射说他在帮、缓存快照里却没有他、MySQL 说他不在任何帮 → 未入帮,
// 且发生在碰经济仓储之前(Repo 为 nil,走到就崩)。不查归属 zone(R4):homeZones 为 nil 也不影响。
// 这一支还必须以 MySQL 复核映射(Y-01):MySQL 替身说他不在任何帮,映射键就得被失效 ——
// 不复核的话,非 0 的陈旧映射在整个 TTL 内都纠正不了。
//
// 每个子用例用自己的缓存与 MySQL 替身,重新写映射**和快照**:坏路径会把可疑快照失效掉(ResolvePlayerGuild),
// 共用一份缓存、只重写映射的话,从第二个子用例起缓存里已经没有 9 号帮的快照、MySQL 又说帮会行不存在,
// 走的就成了"帮已不存在"那一支(由 TestEconomyRPCsRefuseWhenMappedGuildGone 单独覆盖),用例照样变绿却不再测它名字说的事。
// 帮会行读数为 0 把分支钉死:快照只可能取自缓存,即"快照缺本人"。
func TestEconomyRPCsRefuseWhenSnapshotLacksCaller(t *testing.T) {
	for _, tc := range economyRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			fake := &resolveScriptedMySQL{player: 42, memberships: []uint64{0}}
			repo, mr := newResolveScriptedRepo(t, fake)
			seedEconomyStaleSnapshot(t, mr, 9)
			l := newEconomyLogic(repo, EconomyDeps{})

			id, err := tc.run(l, clientCtx(42))

			require.NoError(t, err)
			assert.Equal(t, constants.ErrNotInGuild, id)
			assert.Zero(t, fake.guildRowReadCount(), "快照必须取自缓存:走的是'快照缺本人',不是'帮已不存在'")
			assert.Equal(t, 1, fake.membershipReadCount(), "以 MySQL 复核映射(Y-01),且只复核一次")
			assert.False(t, mr.Exists("player_guild:v2:42"), "陈旧映射必须经 MySQL 复核后失效")
			assert.False(t, mr.Exists("guild:v2:9"), "可疑快照必须被失效,下一个读者才会回源")
		})
	}
}

// TestEconomyRPCsRefuseWhenMappedGuildGone:映射指着一个缓存里没有、MySQL 里也不存在的帮(帮已解散),
// 他也不在任何别的帮 → 未入帮,映射经 MySQL 复核后失效。帮会行读数为正说明快照确实回源过、确实查无此帮,
// 与上一个用例的"快照缺本人"是两条分支。
func TestEconomyRPCsRefuseWhenMappedGuildGone(t *testing.T) {
	for _, tc := range economyRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			fake := &resolveScriptedMySQL{player: 42, memberships: []uint64{0}}
			repo, mr := newResolveScriptedRepo(t, fake)
			seedMembership(t, mr, 42, 9)
			l := newEconomyLogic(repo, EconomyDeps{})

			id, err := tc.run(l, clientCtx(42))

			require.NoError(t, err)
			assert.Equal(t, constants.ErrNotInGuild, id)
			assert.Positive(t, fake.guildRowReadCount(), "缓存里没有快照:必须回源读帮会行,并读到'不存在'")
			assert.Equal(t, 1, fake.membershipReadCount(), "以 MySQL 复核映射(Y-01),且只复核一次")
			assert.False(t, mr.Exists("player_guild:v2:42"), "指向已解散帮会的映射必须经 MySQL 复核后失效")
		})
	}
}

// TestEconomyCallerHealsFromAuthoritativeSnapshot(回归,B5b 自愈路径):坏路径上他其实**在**帮里,
// economyCaller 必须回 MySQL 直读的权威快照,而不是未入帮。
//
// 第一例是这条自愈路径存在的理由:映射是对的、帮会快照是旧的(审批通过后快照失效失败)。只纠映射的旧实现
// 在这里纠了等于没纠,刚入帮的玩家会在整个快照 TTL 里捐献 / 兑换 / 打开商店全部回"未入帮"。
// 后两例是映射本身陈旧的两种形态,确认同一条路径把它们也纠正过来。
// 缓存旧快照是 1 级、MySQL 是 3 级:回带快照的等级为 3,证明它取自 MySQL 而不是那份旧缓存。
func TestEconomyCallerHealsFromAuthoritativeSnapshot(t *testing.T) {
	cases := []struct {
		name string
		// cachedGuild 是缓存映射指向的帮;staleSnapshot 为 true 时缓存里有它一份不含本人的快照。
		cachedGuild   uint64
		staleSnapshot bool
	}{
		{"映射对、快照旧:刚入帮时快照失效失败", 9, true},
		{"映射陈旧、旧帮快照不含本人:离帮后入了别的帮", 8, true},
		{"映射指向已不存在的帮:旧帮解散后入了别的帮", 8, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &resolveScriptedMySQL{player: 42, memberships: []uint64{9}, guilds: map[uint64]data.GuildData{
				9: economyMySQLGuildNine(true),
			}}
			repo, mr := newResolveScriptedRepo(t, fake)
			if tc.staleSnapshot {
				seedEconomyStaleSnapshot(t, mr, tc.cachedGuild)
			} else {
				seedMembership(t, mr, 42, tc.cachedGuild)
			}
			l := newEconomyLogic(repo, EconomyDeps{})

			playerID, g, tip, err := l.economyCaller(clientCtx(42))

			require.NoError(t, err)
			require.Nil(t, tip, "他在 9 号帮里:不能回未入帮")
			require.NotNil(t, g)
			assert.Equal(t, uint64(42), playerID)
			assert.Equal(t, uint64(9), g.GuildID)
			_, ok := memberOf(g, 42)
			assert.True(t, ok, "回带的快照必须含本人")
			assert.Equal(t, uint32(3), g.Level, "回带的必须是 MySQL 的权威快照,不是缓存里那份旧的")
			assert.False(t, mr.Exists(fmt.Sprintf("guild:v2:%d", tc.cachedGuild)), "可疑快照必须被失效,下一个读者才会回源")
		})
	}
}

// TestEconomyRPCsKickedWhileResolvingIsNotAFault:ResolvePlayerGuild 用 uk_guild_member 复核映射之后、
// 绕过缓存直读快照之前,玩家恰好被踢(或退帮)。直读到的快照里没有他,ResolvePlayerGuild 回的是与
// "两份 MySQL 数据互相矛盾"同一个不带哨兵的错误。这是业务态变化,必须回未入帮 tip(文件头纪律 4);
// 抛成 gRPC 错误会把刚被踢的人推进重连隔离。
//
// 替身让映射复核第一次答"在 9 号帮"(被 ResolvePlayerGuild 用掉)、之后答"不在任何帮",9 号帮的成员行里已经没有他 ——
// 正是那个窗口,不靠真库与真并发就能稳定复现。
func TestEconomyRPCsKickedWhileResolvingIsNotAFault(t *testing.T) {
	for _, tc := range economyRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			fake := &resolveScriptedMySQL{player: 42, memberships: []uint64{9, 0}, guilds: map[uint64]data.GuildData{
				9: economyMySQLGuildNine(false),
			}}
			repo, mr := newResolveScriptedRepo(t, fake)
			seedEconomyStaleSnapshot(t, mr, 9)
			l := newEconomyLogic(repo, EconomyDeps{})

			id, err := tc.run(l, clientCtx(42))

			require.NoError(t, err, "复核窗口里被踢是业务态变化,不是故障")
			assert.Equal(t, constants.ErrNotInGuild, id)
			assert.Equal(t, 2, fake.membershipReadCount(),
				"前提:ResolvePlayerGuild 复核时他还在帮(第一次),报错之后再复核一次才看到他已离帮(第二次)")
			assert.False(t, mr.Exists("player_guild:v2:42"), "离帮后的映射必须失效")
		})
	}
}

// TestEconomyRPCsFailClosedOnMembershipContradiction:uk_guild_member 始终说他在 9 号帮,9 号帮的成员行里却始终没有他 ——
// 两份 MySQL 数据互相矛盾,是真故障。必须回 gRPC 错误(fail-closed),不能被上一个用例的"复核之后被踢"答复吞成未入帮:
// 那会让玩家在损坏的数据上去建第二个帮。复核仍然做了一次(第二次读),只是它仍说他在帮。
func TestEconomyRPCsFailClosedOnMembershipContradiction(t *testing.T) {
	for _, tc := range economyRPCs() {
		t.Run(tc.name, func(t *testing.T) {
			fake := &resolveScriptedMySQL{player: 42, memberships: []uint64{9}, guilds: map[uint64]data.GuildData{
				9: economyMySQLGuildNine(false),
			}}
			repo, mr := newResolveScriptedRepo(t, fake)
			seedEconomyStaleSnapshot(t, mr, 9)
			l := newEconomyLogic(repo, EconomyDeps{})

			id, err := tc.run(l, clientCtx(42))

			require.Error(t, err, "两份 MySQL 数据互相矛盾是故障,不能降级成 tip")
			assert.Zero(t, id, "故障不带 tip")
			assert.Equal(t, 2, fake.membershipReadCount(), "报错之后复核过一次,且复核仍说他在帮")
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

// seedEconomyStaleSnapshot 布置坏路径的缓存:映射说 42 在 guildID,缓存里 guildID 的快照(1 级)却只有 7 号帮主。
func seedEconomyStaleSnapshot(t *testing.T, mr *miniredis.Miniredis, guildID uint64) {
	t.Helper()
	seedGuild(t, mr, data.GuildData{GuildID: guildID, ZoneID: 2, Level: 1, Members: []data.MemberData{
		{PlayerID: 7, Role: constants.RoleLeader},
	}}, 0)
	seedMembership(t, mr, 42, guildID)
}

// economyMySQLGuildNine 是 MySQL 替身里的 9 号帮:3 级(缓存旧快照是 1 级,用例靠这个差别认出回带的是哪一份),
// 帮主 7;withCaller 决定成员行里有没有 42。成员按 player_id 升序,与 loadGuild 的 ORDER BY 一致。
func economyMySQLGuildNine(withCaller bool) data.GuildData {
	g := data.GuildData{GuildID: 9, Name: "青云门", LeaderID: 7, Level: 3, MaxMembers: 50, ZoneID: 2, Members: []data.MemberData{
		{PlayerID: 7, Role: constants.RoleLeader},
	}}
	if withCaller {
		g.Members = append(g.Members, data.MemberData{PlayerID: 42, Role: constants.RoleMember})
	}
	return g
}

// resolveScriptedMySQL 是按 SQL 作答的只读 MySQL 替身,为 economyCaller 的坏路径(ResolvePlayerGuild 及其后的复核)
// 布置"MySQL 此刻的真相"。与 noMembershipMySQL 的区别:那个只会回"查无此行";这个能回"他在 9 号帮""9 号帮的成员有谁",
// 还能让同一条映射复核前后答得不一样 —— 这是复现"复核之后、直读之前被踢"唯一不靠真库与真并发的办法。
//
// 只认 data 包 guild_repo.go 的三类读(按 SQL 片段匹配:loadPlayerGuildFromMySQL 与 loadGuild 的两条),
// 其余查询一律报错并带出 SQL:路径被改得去碰别的表时当场失败,而不是悄悄回空、让用例变味。
// data 包改了这几条 SQL 的写法时,这里的片段要跟着改。任何写(Begin / Exec)都回错误:经济前置只读。
// 加锁:database/sql 的连接池允许并发取连接,计数与脚本游标不能靠"通常不会并发"。
type resolveScriptedMySQL struct {
	// player 是唯一允许被复核映射的玩家;问到别人说明身份取错了,当场报错。
	player uint64
	// memberships 是映射复核(SELECT guild_id FROM guild_member WHERE player_id = ?)的逐次答案,0 = 不在任何帮;
	// 用完后停在最后一个值。只布一个值 = 答案始终不变。至少布一个。
	memberships []uint64
	// guilds 是帮会行与成员行的权威数据;不在表里的帮按"帮会行不存在"作答。
	guilds map[uint64]data.GuildData

	mu              sync.Mutex
	membershipReads int
	guildRowReads   int
}

// 按 SQL 片段分派的三类读。片段取自 guild_repo.go,注意 "FROM guild WHERE" 与 "FROM guild_member WHERE" 互不包含。
const (
	resolveScriptedSQLMembership = "FROM guild_member WHERE player_id = ?"
	resolveScriptedSQLGuildRow   = "FROM guild WHERE guild_id = ?"
	resolveScriptedSQLMembers    = "FROM guild_member WHERE guild_id = ?"
)

func (s *resolveScriptedMySQL) Connect(context.Context) (driver.Conn, error) {
	return &resolveScriptedConn{db: s}, nil
}

func (s *resolveScriptedMySQL) Driver() driver.Driver {
	return s
}

func (s *resolveScriptedMySQL) Open(string) (driver.Conn, error) {
	return &resolveScriptedConn{db: s}, nil
}

// membershipReadCount:映射复核被问了几次。用例靠它证明真的走到了预期的那一步(复核了、复核了几次)。
func (s *resolveScriptedMySQL) membershipReadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.membershipReads
}

// guildRowReadCount:帮会行被回源读了几次。为 0 说明快照全部取自缓存。
func (s *resolveScriptedMySQL) guildRowReadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guildRowReads
}

// answer 按 SQL 片段作答。参数经 database/sql 的默认转换后是 int64(uint64 高位为 0 时)。
func (s *resolveScriptedMySQL) answer(query string, args []driver.Value) (driver.Rows, error) {
	id, err := resolveScriptedIDArg(args)
	if err != nil {
		return nil, fmt.Errorf("fake mysql: %q: %w", query, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.Contains(query, resolveScriptedSQLMembership):
		if id != s.player {
			return nil, fmt.Errorf("fake mysql: membership of unexpected player %d (scripted for %d)", id, s.player)
		}
		if len(s.memberships) == 0 {
			return nil, errors.New("fake mysql: memberships not scripted")
		}
		s.membershipReads++
		guildID := s.memberships[min(s.membershipReads, len(s.memberships))-1]
		rows := &resolveScriptedRows{cols: []string{"guild_id"}}
		if guildID != 0 {
			rows.data = [][]driver.Value{{int64(guildID)}}
		}
		return rows, nil
	case strings.Contains(query, resolveScriptedSQLGuildRow):
		s.guildRowReads++
		rows := &resolveScriptedRows{cols: []string{
			"guild_id", "name", "leader_id", "level", "announcement", "create_time_ms", "max_members", "zone_id", "score", "funds",
		}}
		if g, ok := s.guilds[id]; ok {
			rows.data = [][]driver.Value{{
				int64(id), g.Name, int64(g.LeaderID), int64(g.Level), g.Announcement,
				int64(g.CreateTimeMs), int64(g.MaxMembers), int64(g.ZoneID), g.Score, int64(g.Funds),
			}}
		}
		return rows, nil
	case strings.Contains(query, resolveScriptedSQLMembers):
		rows := &resolveScriptedRows{cols: []string{
			"player_id", "role", "join_time_ms", "last_active_ms", "contribution_total", "contribution_balance",
		}}
		for _, m := range s.guilds[id].Members {
			rows.data = append(rows.data, []driver.Value{
				int64(m.PlayerID), int64(m.Role), int64(m.JoinTimeMs), int64(m.LastActiveMs),
				int64(m.ContributionTotal), int64(m.ContributionBalance),
			})
		}
		return rows, nil
	}
	return nil, fmt.Errorf("fake mysql: unexpected query %q", query)
}

// resolveScriptedIDArg 取三类读共有的唯一参数(player_id 或 guild_id)。
func resolveScriptedIDArg(args []driver.Value) (uint64, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("want exactly 1 arg, got %d", len(args))
	}
	switch v := args[0].(type) {
	case int64:
		return uint64(v), nil
	case uint64:
		return v, nil
	}
	return 0, fmt.Errorf("unexpected arg type %T", args[0])
}

type resolveScriptedConn struct {
	db *resolveScriptedMySQL
}

func (c *resolveScriptedConn) Prepare(query string) (driver.Stmt, error) {
	return &resolveScriptedStmt{db: c.db, query: query}, nil
}

func (c *resolveScriptedConn) Close() error {
	return nil
}

func (c *resolveScriptedConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fake mysql: no writes expected on this path")
}

type resolveScriptedStmt struct {
	db    *resolveScriptedMySQL
	query string
}

func (s *resolveScriptedStmt) Close() error {
	return nil
}

func (s *resolveScriptedStmt) NumInput() int {
	return -1
}

func (s *resolveScriptedStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("fake mysql: no writes expected on this path")
}

func (s *resolveScriptedStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.db.answer(s.query, args)
}

type resolveScriptedRows struct {
	cols []string
	data [][]driver.Value
	next int
}

func (r *resolveScriptedRows) Columns() []string {
	return r.cols
}

func (r *resolveScriptedRows) Close() error {
	return nil
}

func (r *resolveScriptedRows) Next(dest []driver.Value) error {
	if r.next >= len(r.data) {
		return io.EOF
	}
	copy(dest, r.data[r.next])
	r.next++
	return nil
}

// newResolveScriptedRepo:缓存走 miniredis,MySQL 是 fake。每个用例一份,计数与脚本游标互不串。
func newResolveScriptedRepo(t *testing.T, fake *resolveScriptedMySQL) (*data.GuildRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	db := sql.OpenDB(fake)
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

// TestBuyGuildShopGoodsOverMaxBuyCountRefusesBeforeMinting(完整性审查 finding 4):份数超过单次上限回
// kGuildShopLimit,且发生在发号、建行、投递之前 —— 号段只进不退,超限请求不该烧号,更不能占帮贡与限购。
// 两条样例取真实配表(generated/tables/guildshop.json + item.json):
//   - 101 培元丹:item 15 堆叠 999、每份 5 个 → 单次上限 min(999/5, MaxShopBuyCount=20) = 20,请求 21;
//   - 202 玄铁护符:item 12 堆叠 1 → 单次上限 1,请求 2。
//
// Repo 为 nil(走到预留事务就 panic);假 scene 与假 Store 调用 0 次 = Loop 一下都没碰。
func TestBuyGuildShopGoodsOverMaxBuyCountRefusesBeforeMinting(t *testing.T) {
	loadEconomyFlowTables(t)
	cases := []struct {
		name    string
		goodsID uint32
		count   uint32
	}{
		{"堆叠 999 的商品按份数封顶", 101, 21},
		{"堆叠 1 的商品每次只能 1 份", 202, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 前提:请求份数恰好比单次上限多 1。配表改了使前提不成立时在这里明确失败,而不是让用例悄悄变味。
			row, ok := table.GuildShopTableManagerInstance.FindById(tc.goodsID)
			require.True(t, ok, "guildshop.json 缺商品 %d", tc.goodsID)
			stack, ok := itemMaxStack(row.GetItemId())
			require.True(t, ok, "item.json 缺物品 %d", row.GetItemId())
			require.Equal(t, tc.count-1, MaxBuyCount(row, stack), "配表变了:先按 generated/tables 重算样例")

			loop, applier, store := newCountingLoop(t)
			minter := &countingMinter{id: 777}
			l := newEconomyLogic(seedEconomyCallerCache(t), EconomyDeps{Loop: loop, OpIDs: minter})

			resp, err := l.BuyGuildShopGoods(clientCtx(42), &pb.BuyGuildShopGoodsRequest{GoodsId: tc.goodsID, Count: tc.count})

			require.NoError(t, err, "超过单次上限是业务拒绝,不是故障")
			assert.Equal(t, constants.ErrShopLimit, resp.GetErrorMessage().GetId())
			assert.Nil(t, resp.GetOrder(), "没有写入指令就不能回订单视图")
			assert.Zero(t, minter.calls, "超限请求不许发号:号段只进不退")
			assert.Zero(t, applier.count(), "拒绝发生在投递之前")
			assert.Zero(t, store.count())
		})
	}
}

// TestDonateUnknownOptionRefusesBeforeMinting(finding 4):配表里没有的 donate_id(热更删了行或请求被篡改)
// 回 kGuildAssetRejected,不发号、不建行、不投递。配表是加载了的(generated/tables/guilddonate.json 只有 1–3),
// 证明拒绝来自"查无此行",而不是来自"配表根本没加载"。
func TestDonateUnknownOptionRefusesBeforeMinting(t *testing.T) {
	const unknownDonateID uint32 = 999
	loadEconomyFlowTables(t)
	_, exists := table.GuildDonateTableManagerInstance.FindById(unknownDonateID)
	require.False(t, exists, "前提:guilddonate.json 里没有 id=%d", unknownDonateID)
	loop, applier, store := newCountingLoop(t)
	minter := &countingMinter{id: 777}
	l := newEconomyLogic(seedEconomyCallerCache(t), EconomyDeps{Loop: loop, OpIDs: minter})

	resp, err := l.DonateToGuild(clientCtx(42), &pb.DonateToGuildRequest{DonateId: unknownDonateID})

	require.NoError(t, err, "查不到选项是业务拒绝,不是故障")
	assert.Equal(t, constants.ErrAssetRejected, resp.GetErrorMessage().GetId())
	assert.Nil(t, resp.GetDonation(), "没有写入指令就不能回订单视图")
	assert.Zero(t, minter.calls, "查不到选项不许发号")
	assert.Zero(t, applier.count())
	assert.Zero(t, store.count())
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

// TestDeliverNowSkipsWhenBudgetTooSmall(finding 4,裁决 K):请求只剩 900ms 时扣掉 1000ms 尾巴,预算为负,
// 同步投递必须整个跳过 —— 假 scene 一次都不被调用,Loop 也不碰 Store(行上带着插行时的租约,到期后交给重投循环)。
// 对照组:没有截止的 ctx 按满额投递,假 scene 恰好被调一次,证明替身确实接在投递路径上、前面的 0 次不是空转。
// 端到端的"回包视图为 PENDING"需要真库,见集成文件 TestEconomyFlowDonateBudgetShortSkipsSyncDelivery。
func TestDeliverNowSkipsWhenBudgetTooSmall(t *testing.T) {
	loop, applier, store := newCountingLoop(t)
	l := NewGuildLogic(nil, nil, nil, nil, nil)
	eco := &EconomyDeps{Loop: loop, SyncBudget: defaultSyncBudget}
	op := assetop.Op{
		OpID:          1,
		PlayerID:      42,
		Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		Seq:           1,
		StreamEpoch:   1,
		CorrelationID: 1,
		Bundle:        &assetpb.AssetBundle{},
		LeaseToken:    1,
	}

	short, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	l.deliverNow(short, eco, op, assetKindDonate)

	assert.Zero(t, applier.count(), "预算不足必须跳过同步投递")
	assert.Zero(t, store.count(), "跳过时 Loop 不写行(不重排、不终结),行保持插行时的租约")

	l.deliverNow(context.Background(), eco, op, assetKindDonate)

	assert.Equal(t, 1, applier.count(), "对照:满额预算时同步投递恰好一次")
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
