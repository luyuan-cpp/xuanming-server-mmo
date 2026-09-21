package data

// 帮会经济 repo 与资产 Store 的测试(设计 05-economy.md §5.39,经 D2 / X-03 / X-04 / 07 §7.4.1 订正)。
//
// 分两层,与 guild_manage_repo_test.go 同一套纪律:
//
//  1. **纯单测**(不连库,任何环境都跑):手写列清单与 proto 字段序、状态映射、后台重试分类、
//     入参校验必须在碰库之前失败。这些是"编译期看不出来、运行期才炸"的那类错误的唯一看守。
//  2. **真库用例**(GUILD_TEST_MYSQL_DSN 未设即 Skip,库名只许 guild_test / guild_it_<pid>_<n>):
//     upsert 的 RowsAffected 语义、CAS 只赢一次、锁序下的对侧账、提前截止的过滤条件 —— 只有真 InnoDB 才有。
//     用例名带 Reserve / Finalize / TooMany / ListDue / Claim / UpgradeGuild / Accelerate / Cleanup / Recent / Resolve,
//     便于 `-run` 一次选中。
//
// 时间一律用 testNowMs 派生的常量显式传入,不读真实墙钟(AGENTS §11.4)。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"

	"shared/assetop"
	"shared/gameday"

	"guild/internal/constants"
)

// ── 纯单测 ────────────────────────────────────────────────────

// TestAssetOpColumnsCoverEveryProtoField:assetOpColumns 是手写列清单,插入参数与扫描顺序都按它排。
// 往 GuildAssetOpRecord 加字段却忘了同步,表现是"新列永远零值"或运行期 Scan 数量不符,编译期都不报 ——
// 这里用 proto 描述符机械比对列名与顺序,并核对 INSERT 的占位符数与参数个数。
func TestAssetOpColumnsCoverEveryProtoField(t *testing.T) {
	fields := (&pb.GuildAssetOpRecord{}).ProtoReflect().Descriptor().Fields()
	want := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		want = append(want, string(fields.Get(i).Name()))
	}

	got := econSplitColumns(assetOpColumns)
	require.Equal(t, want, got, "assetOpColumns 的列名与顺序必须与 GuildAssetOpRecord 的字段序逐一一致")
	assert.Equal(t, assetOpColumnCount, len(got), "assetOpColumnCount 与列清单不符")
	assert.Len(t, assetOpInsertArgs(&pb.GuildAssetOpRecord{}), fields.Len(), "插入参数个数必须等于字段数")
	assert.Equal(t, fields.Len(), strings.Count(sqlInsertAssetOp, "?"), "INSERT 的占位符数必须等于字段数")
}

// econSplitColumns 把 "`a`, `b`" 拆成 ["a","b"]。
func econSplitColumns(cols string) []string {
	parts := strings.Split(cols, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(strings.TrimSpace(p), "`"))
	}
	return out
}

// TestStatusToRecordMapsEverySemanticStatus:assetop.Status 只表达语义,库值只在 StatusToRecord 一处映射。
// 漏一个分支就会把已终结的行落成 UNSPECIFIED;未知值落成 PENDING 会被循环反复领走,落成 APPLIED 是伪装成功。
func TestStatusToRecordMapsEverySemanticStatus(t *testing.T) {
	cases := []struct {
		in   assetop.Status
		want pb.GuildAssetOpStatus
	}{
		{assetop.StatusPending, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING},
		{assetop.StatusApplied, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED},
		{assetop.StatusRejected, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED},
		{assetop.StatusAborted, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED},
		{assetop.StatusAppliedPartial, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, StatusToRecord(tc.in), "StatusToRecord(%s)", tc.in)
	}
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_UNSPECIFIED, StatusToRecord(assetop.Status(250)))
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_UNSPECIFIED, StatusToRecord(assetop.Status(0)))
}

// TestPendingStatusIsNotZero:零值行(写坏 / 半截插入)绝不能被当成待办领走,所以 PENDING 的库值不能是 0。
func TestPendingStatusIsNotZero(t *testing.T) {
	require.NotZero(t, PendingStatus())
	assert.Equal(t, uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING), PendingStatus())
}

// TestGuildSeqTablesUsesGuildTables:表名与 guild_db.proto 的 OptionTableName 逐字一致,未决库值与 Store 同源。
func TestGuildSeqTablesUsesGuildTables(t *testing.T) {
	tables, err := GuildSeqTables()
	require.NoError(t, err)
	assert.Equal(t, "guild_player_op_seq", tables.SeqTable)
	assert.Equal(t, "guild_asset_op", tables.OpTable)
	assert.Equal(t, PendingStatus(), tables.PendingStatus)
}

// TestIsRetryableBackground:后台写把 1213 / 1205 / 9007 当可重试(包一层也要认得),其余一律不重试 ——
// 把撞唯一键判成可重试,会让一次注定失败的终结反复重放。
func TestIsRetryableBackground(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"死锁 1213", &mysqlDriver.MySQLError{Number: 1213}, true},
		{"锁等待超时 1205", &mysqlDriver.MySQLError{Number: 1205}, true},
		{"TiDB 写冲突 9007", &mysqlDriver.MySQLError{Number: 9007}, true},
		{"唯一键冲突 1062", &mysqlDriver.MySQLError{Number: 1062}, false},
		{"包了一层的锁等待", fmt.Errorf("terminal CAS: %w", &mysqlDriver.MySQLError{Number: 1205}), true},
		{"普通错误", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isRetryableBackground(tc.err), tc.name)
	}
}

// TestNewEconomyRepoAndStoreRejectNilGuildRepo:没有 GuildRepo 就没有 inTx 与缓存失效,构造期就拒绝。
func TestNewEconomyRepoAndStoreRejectNilGuildRepo(t *testing.T) {
	_, err := NewEconomyRepo(nil)
	assert.Error(t, err)
	_, err = NewGuildAssetStore(nil)
	assert.Error(t, err)
}

// econNoDBRepo 造一个没有连接池的 GuildRepo:任何越过入参校验去碰库的路径都会当场 panic,
// 从而证明"畸形输入在碰库之前就失败"。
func econNoDBRepo(t *testing.T) *GuildRepo {
	t.Helper()
	return NewGuildRepo(nil, nil, time.Minute)
}

// TestFinalizeAndResolveRefuseNonFinalStatusBeforeTouchingStorage:终结只能落成四个终态之一。
// 落成 PENDING 等于没终结(循环会立刻再领走),落成 UNSPECIFIED 会让客服查不到结局。
func TestFinalizeAndResolveRefuseNonFinalStatusBeforeTouchingStorage(t *testing.T) {
	store, err := NewGuildAssetStore(econNoDBRepo(t))
	require.NoError(t, err)
	ctx := context.Background()
	for _, status := range []assetop.Status{assetop.StatusPending, assetop.Status(0), assetop.Status(250)} {
		finalized, err := store.Finalize(ctx, assetop.Op{OpID: 1}, status, assetop.Result{}, testNowMs)
		assert.Error(t, err, "Finalize status=%d", status)
		assert.False(t, finalized)

		resolved, err := store.ResolveManually(ctx,
			assetop.ManualResolution{OpID: 1, Final: status, Operator: "ops", Reason: "test"}, testNowMs)
		assert.Error(t, err, "ResolveManually status=%d", status)
		assert.False(t, resolved)
	}
}

// TestReserveRejectsMalformedInputBeforeTouchingStorage:编程错误(0 值 id / 空 payload / 0 上限 / 0 截止)
// 在碰库之前失败;一次买的份数超过周期限购是纯判断,同样不必碰库。
func TestReserveRejectsMalformedInputBeforeTouchingStorage(t *testing.T) {
	econ, err := NewEconomyRepo(econNoDBRepo(t))
	require.NoError(t, err)
	ctx := context.Background()

	good := DonationReserve{
		OpID: 1, PlayerID: 2, GuildID: 3, DonateID: 1, MinGuildLevel: 1, ContributionGain: 10, FundsGain: 1000,
		DailyLimit: 5, PeriodKey: 20260921, DeadlineMs: testNowMs + 600_000, LeaseUntilMs: testNowMs + 10_000,
		LeaseToken: 7, NowMs: testNowMs, Payload: []byte{0x08},
	}
	breakers := []struct {
		name    string
		breakIt func(*DonationReserve)
	}{
		{"op_id 为 0", func(in *DonationReserve) { in.OpID = 0 }},
		{"lease token 为 0", func(in *DonationReserve) { in.LeaseToken = 0 }},
		{"payload 为空", func(in *DonationReserve) { in.Payload = nil }},
		{"每日上限为 0", func(in *DonationReserve) { in.DailyLimit = 0 }},
		{"截止为 0 即永不中止", func(in *DonationReserve) { in.DeadlineMs = 0 }},
		{"租约不晚于 now", func(in *DonationReserve) { in.LeaseUntilMs = in.NowMs }},
		{"周期键为 0", func(in *DonationReserve) { in.PeriodKey = 0 }},
	}
	for _, b := range breakers {
		in := good
		b.breakIt(&in)
		_, err := econ.ReserveDonation(ctx, in)
		assert.Error(t, err, b.name)
	}

	shop := ShopReserve{
		OpID: 1, PlayerID: 2, GuildID: 3, GoodsID: 101, Count: 6, RequiredGuildLevel: 1, Cost: 180,
		LimitCount: 5, PeriodKey: 20260921, LeaseUntilMs: testNowMs + 10_000, LeaseToken: 7, NowMs: testNowMs,
		Payload: []byte{0x08},
	}
	_, err = econ.ReserveShopOrder(ctx, shop)
	assert.ErrorIs(t, err, ErrShopLimit, "份数超过周期限购,不碰库直接拒")

	shop.Count, shop.Cost = 1, 0
	_, err = econ.ReserveShopOrder(ctx, shop)
	assert.Error(t, err, "cost 为 0 会让扣帮贡的写入自检误判,必须拒")
	shop.Cost, shop.PeriodKey = 30, 0
	_, err = econ.ReserveShopOrder(ctx, shop)
	assert.Error(t, err, "限购份数与周期键必须同为 0 或同非 0")
}

// TestAccelerateRequiresNonZeroNow:deadline_ms = 0 在本表是"永不中止",传 0 的 now 会把所有未决捐献
// 改成永不中止 —— 必须在发语句之前拒绝(tx 为 nil,越过校验就会 panic)。
func TestAccelerateRequiresNonZeroNow(t *testing.T) {
	err := accelerateDonationDeadlines(context.Background(), nil, 1, []uint64{2}, 0)
	assert.Error(t, err)
}

// ── 真库夹具 ──────────────────────────────────────────────────

const (
	econLeaseMs    uint64 = 10_000
	econDeadlineMs uint64 = 600_000
	econDayMs      uint64 = 24 * 3_600_000
)

// econDayKey 是 testNowMs 所在的游戏日键。
var econDayKey = gameday.DayKey(time.UnixMilli(int64(testNowMs)))

// econGuildLevels 逐行抄自 generated/tables/guildlevel.json(2026-09-21 实值):data 包不加载配表,
// 升级用例的期望值(1→2 扣 20000、上限 35)就以这份为准;配表改了这里要同步。
var econGuildLevels = map[uint32]struct {
	cost       uint64
	maxMembers uint32
}{
	1: {20000, 30}, 2: {50000, 35}, 3: {100000, 40}, 4: {180000, 45}, 5: {300000, 50},
	6: {460000, 60}, 7: {680000, 70}, 8: {960000, 80}, 9: {1300000, 90}, 10: {0, 100},
}

func econLevelLookup(level uint32) (uint64, uint32, bool) {
	row, ok := econGuildLevels[level]
	return row.cost, row.maxMembers, ok
}

// econFixture 把真库用例的装配收拢:经济 repo 与资产 store 挂在同一个 GuildRepo 上(与生产接线一致)。
type econFixture struct {
	ctx   context.Context
	db    *sql.DB
	repo  *GuildRepo
	econ  *EconomyRepo
	store *GuildAssetStore
}

func openEconomyFixture(t *testing.T) econFixture {
	t.Helper()
	ctx, db, repo := openGuildIntegrationRepo(t)
	econ, err := NewEconomyRepo(repo)
	require.NoError(t, err)
	store, err := NewGuildAssetStore(repo)
	require.NoError(t, err)
	return econFixture{ctx: ctx, db: db, repo: repo, econ: econ, store: store}
}

func econDonatePayload(t *testing.T, amount uint64) []byte {
	t.Helper()
	payload, err := proto.Marshal(&assetpb.AssetBundle{
		Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 0, Amount: amount}},
	})
	require.NoError(t, err)
	return payload
}

func econShopPayload(t *testing.T, itemID, count uint32) []byte {
	t.Helper()
	payload, err := proto.Marshal(&assetpb.AssetBundle{
		Items: []*assetpb.ItemGrant{{ConfigId: itemID, Count: count}},
	})
	require.NoError(t, err)
	return payload
}

// econDonation 造一笔"银两小捐"(GuildDonate 第 1 行的数值):扣 10000、帮贡 10、资金 1000、每日 5 次。
func econDonation(t *testing.T, opID, playerID, guildID, now uint64) DonationReserve {
	t.Helper()
	return DonationReserve{
		OpID: opID, PlayerID: playerID, GuildID: guildID,
		DonateID: 1, MinGuildLevel: 1, ContributionGain: 10, FundsGain: 1000, DailyLimit: 5,
		PeriodKey: econDayKey, DeadlineMs: now + econDeadlineMs, LeaseUntilMs: now + econLeaseMs,
		LeaseToken: opID | 1<<62, NowMs: now, Payload: econDonatePayload(t, 10000),
	}
}

// econShopOrder 造一笔商品 101 的兑换;limit > 0 时按日限购。
func econShopOrder(t *testing.T, opID, playerID, guildID uint64, count uint32, cost uint64, limit uint32, now uint64) ShopReserve {
	t.Helper()
	var periodKey uint32
	if limit > 0 {
		periodKey = econDayKey
	}
	return ShopReserve{
		OpID: opID, PlayerID: playerID, GuildID: guildID,
		GoodsID: 101, Count: count, RequiredGuildLevel: 1, Cost: cost, LimitCount: limit, PeriodKey: periodKey,
		LeaseUntilMs: now + econLeaseMs, LeaseToken: opID | 1<<62, NowMs: now,
		Payload: econShopPayload(t, 15, 5*count),
	}
}

func econSetContribution(t *testing.T, f econFixture, guildID, playerID, total, balance uint64) {
	t.Helper()
	mustExec(t, f.ctx, f.db,
		"UPDATE guild_member SET contribution_total=?, contribution_balance=? WHERE guild_id=? AND player_id=?",
		total, balance, guildID, playerID)
}

func econFunds(t *testing.T, f econFixture, guildID uint64) uint64 {
	t.Helper()
	var funds uint64
	require.NoError(t, f.db.QueryRowContext(f.ctx, "SELECT funds FROM guild WHERE guild_id=?", guildID).Scan(&funds))
	return funds
}

// econContribution 读帮贡两列;成员行不在时 found=false。
func econContribution(t *testing.T, f econFixture, guildID, playerID uint64) (total, balance uint64, found bool) {
	t.Helper()
	total, balance, found, err := f.econ.MemberContribution(f.ctx, guildID, playerID)
	require.NoError(t, err)
	return total, balance, found
}

// econCounterUsed 读一行计数;行不存在时 found=false。
func econCounterUsed(t *testing.T, f econFixture, playerID uint64, kind pb.GuildDailyCounterKind, refID, periodKey uint32) (uint32, bool) {
	t.Helper()
	var used uint32
	err := f.db.QueryRowContext(f.ctx,
		"SELECT used_count FROM guild_daily_counter WHERE player_id=? AND counter_kind=? AND ref_id=? AND period_key=?",
		playerID, int32(kind), refID, periodKey).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	require.NoError(t, err)
	return used, true
}

// econRecord 读一整行(含 AssetOpRow 不暴露的 durable / lease / resolved_by 等列)。
func econRecord(t *testing.T, f econFixture, opID uint64) *pb.GuildAssetOpRecord {
	t.Helper()
	rec, found, err := f.store.getRecord(f.ctx, opID)
	require.NoError(t, err)
	require.True(t, found, "op %d 应当存在", opID)
	return rec
}

func econOpExists(t *testing.T, f econFixture, opID uint64) bool {
	t.Helper()
	_, found, err := f.store.getRecord(f.ctx, opID)
	require.NoError(t, err)
	return found
}

// econInsertOp 直接插一行指令(走生产的 INSERT 语句与参数顺序),用来造 ListDue / 清理 / 排序要的各种状态。
func econInsertOp(t *testing.T, f econFixture, rec *pb.GuildAssetOpRecord) {
	t.Helper()
	_, err := f.db.ExecContext(f.ctx, sqlInsertAssetOp, assetOpInsertArgs(rec)...)
	require.NoError(t, err, "insert op %d", rec.GetOpId())
}

// econShopRecord 造一行商店指令的骨架;调用方再改状态 / 次数 / 时刻。
func econShopRecord(t *testing.T, opID, playerID, epoch, seq uint64) *pb.GuildAssetOpRecord {
	t.Helper()
	return &pb.GuildAssetOpRecord{
		OpId: opID, PlayerId: playerID, Stream: uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT),
		Seq: seq, GuildId: 7700, Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP,
		Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		NextAttemptMs: testNowMs - 10, Payload: econShopPayload(t, 15, 5), RefId: 101, RefCount: 1,
		CreatedMs: testNowMs - 1000, UpdatedMs: testNowMs - 1000,
		TxType: uint32(rollbackpb.TransactionType_TX_GUILD_SHOP), StreamEpoch: epoch,
	}
}

// econCaptureOrphans 把 orphan 计数换成记录器,返回读取函数。
func econCaptureOrphans(t *testing.T) func() []string {
	t.Helper()
	var got []string
	original := recordAssetOrphan
	recordAssetOrphan = func(kind, what string) { got = append(got, kind+"/"+what) }
	t.Cleanup(func() { recordAssetOrphan = original })
	return func() []string { return got }
}

// econAppliedResult 是 scene 已落盘的全额应用答复。
func econAppliedResult() assetop.Result {
	return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}
}

// ── T-D 捐献预留 ─────────────────────────────────────────────

// TestReserveDonation_WritesPendingRowAndOccupiesCount:成功路径写满 28 列、占一次今日次数,
// 可空的三列写成空值而不是 NULL,合服闸门拿到的是事务内读到的 zone。
func TestReserveDonation_WritesPendingRowAndOccupiesCount(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7501
		leader  uint64 = 8501
		donor   uint64 = 8502
		zone    uint32 = 3
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, zone, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	var fencedZone uint32
	in := econDonation(t, 9_500_001, donor, guildID, testNowMs)
	in.Fence = func(_ context.Context, zoneID uint32) error { fencedZone = zoneID; return nil }
	res, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), res.Seq, "首笔 seq 从 1 起")
	assert.Equal(t, testNowMs, res.StreamEpoch, "纪元 = 事务外建 seq 行的时刻")
	assert.Equal(t, zone, fencedZone, "闸门必须拿事务内读到的 guild.zone_id")

	rec := econRecord(t, f, in.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus())
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, rec.GetKind())
	assert.Equal(t, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT), rec.GetStream())
	assert.Equal(t, guildID, rec.GetGuildId())
	assert.Equal(t, in.DeadlineMs, rec.GetDeadlineMs())
	assert.Equal(t, in.LeaseUntilMs, rec.GetLeaseUntilMs())
	assert.Equal(t, in.LeaseUntilMs, rec.GetNextAttemptMs(), "同步投递握着租约,租约到期前循环不该碰它")
	assert.Equal(t, in.LeaseToken, rec.GetLeaseToken())
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_DONATE), rec.GetTxType())
	assert.Equal(t, in.Payload, rec.GetPayload())
	assert.Equal(t, econDayKey, rec.GetPeriodKey())
	assert.Equal(t, uint32(1), rec.GetRefId())
	assert.Equal(t, uint32(1), rec.GetRefCount())
	assert.Equal(t, uint64(10), rec.GetContributionDelta())
	assert.Equal(t, uint64(1000), rec.GetFundsDelta())
	assert.Equal(t, res.StreamEpoch, rec.GetStreamEpoch())

	var payloadNull, resolvedByNull, resolveReasonNull bool
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT payload IS NULL, resolved_by IS NULL, resolve_reason IS NULL FROM guild_asset_op WHERE op_id=?", in.OpID).
		Scan(&payloadNull, &resolvedByNull, &resolveReasonNull))
	assert.False(t, payloadNull || resolvedByNull || resolveReasonNull, "三个可空列必须显式写值,不能留 NULL")

	usage, err := f.econ.DonateUsage(f.ctx, donor, econDayKey)
	require.NoError(t, err)
	assert.Equal(t, map[uint32]uint32{1: 1}, usage)

	pending, err := f.econ.PendingOps(f.ctx, donor, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, 16)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, in.OpID, pending[0].OpID)

	state, found, err := f.econ.OpState(f.ctx, in.OpID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, state.Status)
	_, found, err = f.econ.OpState(f.ctx, 1)
	require.NoError(t, err)
	assert.False(t, found)
}

// TestReserveDonation_LimitReachedRollsBack:达上限整体回滚 —— 行不落、seq 不前进、次数不再加。
// seq 前进而行没落,scene 会看到一个永远不来的空洞 seq,后面的指令全部被窗口判定挡住。
func TestReserveDonation_LimitReachedRollsBack(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7511
		leader  uint64 = 8511
		donor   uint64 = 8512
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	first := econDonation(t, 9_510_001, donor, guildID, testNowMs)
	first.DailyLimit = 1
	_, err := f.econ.ReserveDonation(f.ctx, first)
	require.NoError(t, err)

	second := econDonation(t, 9_510_002, donor, guildID, testNowMs+1)
	second.DailyLimit = 1
	_, err = f.econ.ReserveDonation(f.ctx, second)
	assert.ErrorIs(t, err, ErrDonateLimit)

	assert.False(t, econOpExists(t, f, second.OpID), "达上限时指令行必须随事务回滚")
	var nextSeq uint64
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT next_seq FROM guild_player_op_seq WHERE player_id=? AND stream=?",
		donor, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT)).Scan(&nextSeq))
	assert.Equal(t, uint64(2), nextSeq, "seq 不许因为被拒的那一笔前进")
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Equal(t, uint32(1), used)
}

// TestReserveDonation_Rejections:帮会不在 / 不是成员 / 等级不够 / 合服闸门(含闸门读失败)各自回对应哨兵,
// 且拒绝之后库里什么都没留下。
func TestReserveDonation_Rejections(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID  uint64 = 7521
		missing  uint64 = 7529
		leader   uint64 = 8521
		donor    uint64 = 8522
		outsider uint64 = 8523
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	_, err := f.econ.ReserveDonation(f.ctx, econDonation(t, 9_520_001, donor, missing, testNowMs))
	assert.ErrorIs(t, err, ErrGuildGone)

	_, err = f.econ.ReserveDonation(f.ctx, econDonation(t, 9_520_002, outsider, guildID, testNowMs))
	assert.ErrorIs(t, err, ErrNotGuildMember)

	tooHigh := econDonation(t, 9_520_003, donor, guildID, testNowMs)
	tooHigh.MinGuildLevel = 2
	_, err = f.econ.ReserveDonation(f.ctx, tooHigh)
	assert.ErrorIs(t, err, ErrGuildLevelTooLow)

	merging := econDonation(t, 9_520_004, donor, guildID, testNowMs)
	merging.Fence = func(context.Context, uint32) error { return ErrZoneMerging }
	_, err = f.econ.ReserveDonation(f.ctx, merging)
	assert.ErrorIs(t, err, ErrZoneMerging)

	unreadable := econDonation(t, 9_520_005, donor, guildID, testNowMs)
	unreadable.Fence = func(context.Context, uint32) error { return errors.New("redis down") }
	_, err = f.econ.ReserveDonation(f.ctx, unreadable)
	assert.ErrorIs(t, err, ErrZoneMerging, "闸门读不出来也按拒绝处理(fail-closed)")

	var ops int
	require.NoError(t, f.db.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM guild_asset_op").Scan(&ops))
	assert.Zero(t, ops, "被拒的预留不能留下指令行")
	_, found := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.False(t, found, "被拒的预留不能占次数")
}

// TestReserveDonation_TooManyPending:未决行达到 assetop.DefaultLimits.MaxPending 之后,守卫拒绝下一笔,
// 错误可被 errors.Is(assetop.ErrTooManyPending) 识别(logic 据此回 GuildAssetPending),且被拒那笔不占次数。
func TestReserveDonation_TooManyPending(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7531
		leader  uint64 = 8531
		donor   uint64 = 8532
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	maxPending := assetop.DefaultLimits.MaxPending
	for i := uint32(0); i < maxPending; i++ {
		in := econDonation(t, 9_530_000+uint64(i), donor, guildID, testNowMs+uint64(i))
		in.DailyLimit = 100
		_, err := f.econ.ReserveDonation(f.ctx, in)
		require.NoError(t, err, "第 %d 笔", i+1)
	}
	over := econDonation(t, 9_539_999, donor, guildID, testNowMs+1000)
	over.DailyLimit = 100
	_, err := f.econ.ReserveDonation(f.ctx, over)
	assert.ErrorIs(t, err, assetop.ErrTooManyPending)
	assert.False(t, econOpExists(t, f, over.OpID))
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Equal(t, maxPending, used, "被守卫拒绝的那笔不占次数")
}

// ── T-S 兑换预留 ─────────────────────────────────────────────

// TestReserveShopOrder_DebitsContributionAndOccupiesLimit:扣可用帮贡、按份数占限购、写商店指令(永不中止);
// 达上限那一笔整体回滚,帮贡与限购都不动。
func TestReserveShopOrder_DebitsContributionAndOccupiesLimit(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7541
		leader  uint64 = 8541
		buyer   uint64 = 8542
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{buyer: constants.RoleMember})
	econSetContribution(t, f, guildID, buyer, 1000, 1000)

	first := econShopOrder(t, 9_540_001, buyer, guildID, 2, 300, 5, testNowMs)
	res, err := f.econ.ReserveShopOrder(f.ctx, first)
	require.NoError(t, err)
	assert.Equal(t, uint64(700), res.BalanceAfter)
	assert.Equal(t, uint64(1), res.Seq)

	rec := econRecord(t, f, first.OpID)
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP, rec.GetKind())
	assert.Equal(t, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT), rec.GetStream())
	assert.Zero(t, rec.GetDeadlineMs(), "商店指令永不中止")
	assert.Equal(t, uint32(101), rec.GetRefId())
	assert.Equal(t, uint32(2), rec.GetRefCount())
	assert.Equal(t, uint64(300), rec.GetContributionDelta())
	assert.Zero(t, rec.GetFundsDelta())
	assert.Equal(t, econDayKey, rec.GetPeriodKey())
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_SHOP), rec.GetTxType())

	second := econShopOrder(t, 9_540_002, buyer, guildID, 3, 450, 5, testNowMs+1)
	res, err = f.econ.ReserveShopOrder(f.ctx, second)
	require.NoError(t, err)
	assert.Equal(t, uint64(250), res.BalanceAfter)

	third := econShopOrder(t, 9_540_003, buyer, guildID, 1, 150, 5, testNowMs+2)
	_, err = f.econ.ReserveShopOrder(f.ctx, third)
	assert.ErrorIs(t, err, ErrShopLimit)
	assert.False(t, econOpExists(t, f, third.OpID))

	total, balance, found := econContribution(t, f, guildID, buyer)
	require.True(t, found)
	assert.Equal(t, uint64(1000), total, "兑换只扣可用余额,不动累计")
	assert.Equal(t, uint64(250), balance, "被拒那笔不扣帮贡")

	usage, err := f.econ.ShopUsage(f.ctx, buyer, econDayKey, gameday.WeekKey(time.UnixMilli(int64(testNowMs))))
	require.NoError(t, err)
	assert.Equal(t, map[ShopUsageKey]uint32{{GoodsID: 101, PeriodKey: econDayKey}: 5}, usage)
}

// TestReserveShopOrder_Rejections:帮贡不足 / 等级不够回对应哨兵且不留痕;不限购的商品不占计数行。
func TestReserveShopOrder_Rejections(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7551
		leader  uint64 = 8551
		buyer   uint64 = 8552
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{buyer: constants.RoleMember})
	econSetContribution(t, f, guildID, buyer, 100, 100)

	poor := econShopOrder(t, 9_550_001, buyer, guildID, 1, 300, 5, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, poor)
	assert.ErrorIs(t, err, ErrContributionInsufficient)
	assert.False(t, econOpExists(t, f, poor.OpID))
	_, found := econCounterUsed(t, f, buyer, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.False(t, found, "帮贡不足时不能占限购")

	tooHigh := econShopOrder(t, 9_550_002, buyer, guildID, 1, 30, 5, testNowMs)
	tooHigh.RequiredGuildLevel = 6
	_, err = f.econ.ReserveShopOrder(f.ctx, tooHigh)
	assert.ErrorIs(t, err, ErrGuildLevelTooLow)

	unlimited := econShopOrder(t, 9_550_003, buyer, guildID, 1, 30, 0, testNowMs)
	res, err := f.econ.ReserveShopOrder(f.ctx, unlimited)
	require.NoError(t, err)
	assert.Equal(t, uint64(70), res.BalanceAfter)
	assert.Zero(t, econRecord(t, f, unlimited.OpID).GetPeriodKey(), "不限购 = 不占计数行,period_key 记 0")
	var counters int
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT COUNT(*) FROM guild_daily_counter WHERE player_id=?", buyer).Scan(&counters))
	assert.Zero(t, counters)
}

// ── T-U 升级 ─────────────────────────────────────────────────

// TestUpgradeGuild_ChargesCurrentLevelCostAndRaisesCap:1→2 级扣 GuildLevel[1].upgrade_cost_funds(20000),
// 上限取 GuildLevel[2].max_members(35);职位、expected_level、资金、满级、配表缺行、闸门各自拒绝。
func TestUpgradeGuild_ChargesCurrentLevelCostAndRaisesCap(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7561
		leader  uint64 = 8561
		officer uint64 = 8562
		member  uint64 = 8563
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 30, leader, map[uint64]uint32{
		officer: constants.RoleOfficer, member: constants.RoleMember,
	})
	mustExec(t, f.ctx, f.db, "UPDATE guild SET funds=? WHERE guild_id=?", 25000, guildID)

	// 先把快照读进缓存,才能证明升级之后缓存真的失效了。
	cached, err := f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	require.Equal(t, uint32(1), cached.Level)

	_, err = f.econ.UpgradeGuild(f.ctx, guildID, member, 1, econLevelLookup, nil)
	assert.ErrorIs(t, err, ErrRankTooLow, "普通成员不能升级")

	_, err = f.econ.UpgradeGuild(f.ctx, guildID, officer, 1, econLevelLookup,
		func(context.Context, uint32) error { return ErrZoneMerging })
	assert.ErrorIs(t, err, ErrZoneMerging)

	stale, err := f.econ.UpgradeGuild(f.ctx, guildID, officer, 2, econLevelLookup, nil)
	require.NoError(t, err)
	assert.False(t, stale.Changed, "expected_level 对不上 = 无改动")
	assert.Equal(t, uint32(1), stale.NewLevel)
	assert.Equal(t, uint64(25000), econFunds(t, f, guildID))

	res, err := f.econ.UpgradeGuild(f.ctx, guildID, officer, 1, econLevelLookup, nil)
	require.NoError(t, err)
	assert.True(t, res.Changed)
	assert.Equal(t, uint32(2), res.NewLevel)
	assert.Equal(t, []uint64{leader, officer, member}, res.MemberIDs, "收件人 player_id 升序")
	assert.Equal(t, uint64(5000), econFunds(t, f, guildID), "扣当前等级行的 20000")
	var maxMembers uint32
	require.NoError(t, f.db.QueryRowContext(f.ctx,
		"SELECT max_members FROM guild WHERE guild_id=?", guildID).Scan(&maxMembers))
	assert.Equal(t, uint32(35), maxMembers, "新上限取下一级行")

	fresh, err := f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	assert.Equal(t, uint32(2), fresh.Level, "提交后必须失效帮会快照缓存")

	again, err := f.econ.UpgradeGuild(f.ctx, guildID, officer, 1, econLevelLookup, nil)
	require.NoError(t, err)
	assert.False(t, again.Changed, "重复点击不能连升两级")
	assert.Equal(t, uint64(5000), econFunds(t, f, guildID))

	_, err = f.econ.UpgradeGuild(f.ctx, guildID, leader, 2, econLevelLookup, nil)
	assert.ErrorIs(t, err, ErrFundsInsufficient)

	// 过期视图自愈:模拟"上一次升级已提交、COMMIT 回执丢失、当时没失效缓存"—— 先把快照读进缓存,再绕过 repo 直接改库。
	// 调用方按旧等级重试,命中 expected_level 不符的无改动分支;这一支不扣钱,但必须失效缓存。
	cached, err = f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	require.Equal(t, uint32(2), cached.Level)
	mustExec(t, f.ctx, f.db, "UPDATE guild SET level=? WHERE guild_id=?", 3, guildID)
	healed, err := f.econ.UpgradeGuild(f.ctx, guildID, leader, 2, econLevelLookup, nil)
	require.NoError(t, err)
	assert.False(t, healed.Changed)
	assert.Equal(t, uint32(3), healed.NewLevel)
	assert.Equal(t, uint64(5000), econFunds(t, f, guildID), "无改动分支不扣钱")
	fresh, err = f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), fresh.Level, "expected_level 不符 = 调用方视图过期,必须顺手失效缓存")

	mustExec(t, f.ctx, f.db, "UPDATE guild SET level=?, funds=? WHERE guild_id=?", 10, 9_000_000, guildID)
	_, err = f.econ.UpgradeGuild(f.ctx, guildID, leader, 0, econLevelLookup, nil)
	assert.ErrorIs(t, err, ErrGuildMaxLevel)

	mustExec(t, f.ctx, f.db, "UPDATE guild SET level=? WHERE guild_id=?", 9, guildID)
	missingNext := func(level uint32) (uint64, uint32, bool) {
		if level == 10 {
			return 0, 0, false
		}
		return econLevelLookup(level)
	}
	_, err = f.econ.UpgradeGuild(f.ctx, guildID, leader, 0, missingNext, nil)
	assert.ErrorIs(t, err, ErrGuildLevelConfigMissing, "配表缺下一级行按错误处理,不默认放行")
}

// ── Store:领取、重排 ─────────────────────────────────────────

// TestListDue_FreshRowsFirst:第一段只取新行并优先占名额,第二段只补缺口;租约未到期、未到期、非未决的行都不出现。
func TestListDue_FreshRowsFirst(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8571
	epoch := testNowMs - 100_000

	for i, opID := range []uint64{101, 102, 103} { // 老行:attempts 已达 FreshAttemptLimit,且更早到期
		rec := econShopRecord(t, opID, player, epoch, uint64(i+1))
		rec.Attempts = assetop.FreshAttemptLimit + 2
		rec.NextAttemptMs = testNowMs - 50_000 + uint64(i)
		econInsertOp(t, f, rec)
	}
	for i, opID := range []uint64{201, 202} { // 新行
		rec := econShopRecord(t, opID, player, epoch, uint64(10+i))
		rec.NextAttemptMs = testNowMs - 10 + uint64(i)
		econInsertOp(t, f, rec)
	}
	notDue := econShopRecord(t, 301, player, epoch, 20)
	notDue.NextAttemptMs = testNowMs + 1000
	econInsertOp(t, f, notDue)
	leased := econShopRecord(t, 302, player, epoch, 21)
	leased.LeaseUntilMs = testNowMs + 5000
	econInsertOp(t, f, leased)
	done := econShopRecord(t, 303, player, epoch, 22)
	done.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
	econInsertOp(t, f, done)

	ids, err := f.store.ListDue(f.ctx, testNowMs, 2)
	require.NoError(t, err)
	assert.Equal(t, []uint64{201, 202}, ids, "新行满额时老行一条都不取")

	ids, err = f.store.ListDue(f.ctx, testNowMs, 4)
	require.NoError(t, err)
	assert.Equal(t, []uint64{201, 202, 101, 102}, ids, "第二段只补缺口,新行排在前面")

	ids, err = f.store.ListDue(f.ctx, testNowMs, 100)
	require.NoError(t, err)
	assert.Equal(t, []uint64{201, 202, 101, 102, 103}, ids)
}

// TestClaim_ReturnsFullOpAndHoldsLease:领到的 Op 带齐纪元 / correlation(= op_id)/ 令牌 / 包;租约期内别人领不到。
func TestClaim_ReturnsFullOpAndHoldsLease(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8581
	rec := econShopRecord(t, 401, player, testNowMs-100_000, 7)
	rec.Attempts = 2
	rec.LastReason = assetop.ReasonBagFull
	econInsertOp(t, f, rec)

	const token uint64 = 0xabc1
	op, claimed, err := f.store.Claim(f.ctx, 401, testNowMs, testNowMs+econLeaseMs, testNowMs+3_600_000, token)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Equal(t, uint64(401), op.OpID)
	assert.Equal(t, player, op.PlayerID)
	assert.Equal(t, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, op.Stream)
	assert.Equal(t, uint64(7), op.Seq)
	assert.Equal(t, rec.GetStreamEpoch(), op.StreamEpoch)
	assert.Equal(t, uint64(401), op.CorrelationID, "correlation_id 取 op_id,不是 ref_id")
	assert.Equal(t, token, op.LeaseToken)
	assert.Equal(t, uint32(rollbackpb.TransactionType_TX_GUILD_SHOP), op.TxType)
	assert.Equal(t, uint32(2), op.Attempts)
	assert.Equal(t, assetop.ReasonBagFull, op.LastReason)
	wantBundle := &assetpb.AssetBundle{}
	require.NoError(t, proto.Unmarshal(rec.GetPayload(), wantBundle))
	assert.True(t, proto.Equal(wantBundle, op.Bundle))

	stored := econRecord(t, f, 401)
	assert.Equal(t, testNowMs+econLeaseMs, stored.GetLeaseUntilMs())
	assert.Equal(t, token, stored.GetLeaseToken())

	_, claimed, err = f.store.Claim(f.ctx, 401, testNowMs+1, testNowMs+1+econLeaseMs, testNowMs+3_600_000, 0xabc2)
	require.NoError(t, err)
	assert.False(t, claimed, "租约期内另一个副本领不到")
}

// TestClaim_PoisonRowIsDeferred:payload 解不开或为 NULL 都是毒行 —— 推迟到循环给的 poisonUntilMs、
// 放掉租约、last_outcome 记未知,并回 assetop.ErrPoisonRow;不许把空包下发给 scene。
func TestClaim_PoisonRowIsDeferred(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8591
	garbage := econShopRecord(t, 501, player, testNowMs-100_000, 1)
	garbage.Payload = []byte{0xff, 0xff}
	econInsertOp(t, f, garbage)
	null := econShopRecord(t, 502, player, testNowMs-100_000, 2)
	null.Payload = nil // 插入时绑定 NULL
	econInsertOp(t, f, null)

	const poisonUntil = testNowMs + 3_600_000
	for _, opID := range []uint64{501, 502} {
		_, claimed, err := f.store.Claim(f.ctx, opID, testNowMs, testNowMs+econLeaseMs, poisonUntil, 0x77)
		assert.ErrorIs(t, err, assetop.ErrPoisonRow, "op %d", opID)
		assert.False(t, claimed)

		stored := econRecord(t, f, opID)
		assert.Equal(t, poisonUntil, stored.GetNextAttemptMs(), "毒行推迟到循环算好的时刻,不自写常量")
		assert.Zero(t, stored.GetLeaseUntilMs(), "毒行放掉租约")
		assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN), stored.GetLastOutcome())
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, stored.GetStatus(), "毒行不终结")
	}

	// 竞态:Claim 的 CAS 写下令牌之后、推迟毒行之前,assetopfix 已把行人工终结(它的 CAS 只看 status、不换令牌)。
	// 这里直接造出"令牌仍是本次、状态已是终态"的行再推迟:终态行必须一个字不动 —— next_attempt_ms 仍是终结时刻、
	// last_outcome 仍是人工终结保留的证据(07 §7.2 / §7.4.1)。
	const (
		token      uint64 = 0x78
		resolvedAt uint64 = testNowMs - 5
	)
	resolved := econShopRecord(t, 503, player, testNowMs-100_000, 3)
	resolved.Payload = []byte{0xff, 0xff}
	resolved.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED
	resolved.LeaseToken = token
	resolved.NextAttemptMs = resolvedAt
	resolved.UpdatedMs = resolvedAt
	resolved.LastOutcome = uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY)
	econInsertOp(t, f, resolved)

	f.store.markPoison(f.ctx, 503, token, testNowMs, poisonUntil)
	stored := econRecord(t, f, 503)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED, stored.GetStatus())
	assert.Equal(t, resolvedAt, stored.GetNextAttemptMs(), "终态行的 next_attempt_ms 必须仍是终结时刻")
	assert.Equal(t, resolvedAt, stored.GetUpdatedMs(), "终态行此后没有任何路径再改")
	assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY), stored.GetLastOutcome(), "证据不能被抹成 0")
}

// TestClaimThenReschedule_LeaseLost:Reschedule 带错令牌必须回 ErrLeaseLost(不能伪装成功);
// 带对令牌则推进次数、放租约、记下本次答复。
func TestClaimThenReschedule_LeaseLost(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8601
	econInsertOp(t, f, econShopRecord(t, 601, player, testNowMs-100_000, 1))

	op, claimed, err := f.store.Claim(f.ctx, 601, testNowMs, testNowMs+econLeaseMs, testNowMs+3_600_000, 0xa1)
	require.NoError(t, err)
	require.True(t, claimed)

	res := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY, Reason: assetop.ReasonInBattle}
	stolen := op
	stolen.LeaseToken = 0xb2
	err = f.store.Reschedule(f.ctx, stolen, testNowMs+5000, res, testNowMs+1)
	assert.ErrorIs(t, err, assetop.ErrLeaseLost)
	assert.Zero(t, econRecord(t, f, 601).GetAttempts(), "租约不是我的,一个字都不能写进去")

	require.NoError(t, f.store.Reschedule(f.ctx, op, testNowMs+5000, res, testNowMs+1))
	stored := econRecord(t, f, 601)
	assert.Equal(t, uint32(1), stored.GetAttempts())
	assert.Equal(t, testNowMs+5000, stored.GetNextAttemptMs())
	assert.Zero(t, stored.GetLeaseUntilMs())
	assert.Zero(t, stored.GetDurable())
	assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY), stored.GetLastOutcome())
	assert.Equal(t, assetop.ReasonInBattle, stored.GetLastReason())
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, stored.GetStatus())
}

// ── Store:终结与对侧账 ───────────────────────────────────────

// TestFinalize_DonateAppliedCreditsFundsAndContribution:APPLIED 按 op.guild_id 记资金与帮贡、同写
// next_attempt_ms = now(07 §7.4.1)、失效缓存、推送一次;重复终结 CAS 落空,对侧账不做第二遍。
func TestFinalize_DonateAppliedCreditsFundsAndContribution(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7611
		leader  uint64 = 8611
		donor   uint64 = 8612
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, 9_610_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)

	var pushed []FinalizedOp
	f.store.OnFinalized = func(_ context.Context, op FinalizedOp) { pushed = append(pushed, op) }
	cached, err := f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	require.Zero(t, cached.Funds)

	finalNow := testNowMs + 1234
	op := assetop.Op{OpID: in.OpID, PlayerID: donor}
	finalized, err := f.store.Finalize(f.ctx, op, assetop.StatusApplied, econAppliedResult(), finalNow)
	require.NoError(t, err)
	assert.True(t, finalized)

	assert.Equal(t, uint64(1000), econFunds(t, f, guildID))
	total, balance, found := econContribution(t, f, guildID, donor)
	require.True(t, found)
	assert.Equal(t, uint64(10), total)
	assert.Equal(t, uint64(10), balance)

	rec := econRecord(t, f, in.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, rec.GetStatus())
	assert.Equal(t, uint32(1), rec.GetDurable())
	assert.Equal(t, finalNow, rec.GetNextAttemptMs(), "终态行的 next_attempt_ms 必须等于终结时刻(回档检查按它判)")
	assert.Equal(t, finalNow, rec.GetUpdatedMs())
	assert.Zero(t, rec.GetLeaseUntilMs())
	assert.Zero(t, rec.GetReasonTipId())
	assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED), rec.GetLastOutcome())

	fresh, err := f.repo.GetGuild(f.ctx, guildID)
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), fresh.Funds, "改过帮会行必须失效快照缓存")

	require.Len(t, pushed, 1)
	assert.Equal(t, FinalizedOp{
		OpID: in.OpID, PlayerID: donor, GuildID: guildID,
		Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, Status: assetop.StatusApplied,
	}, pushed[0])

	again, err := f.store.Finalize(f.ctx, op, assetop.StatusApplied, econAppliedResult(), finalNow+1)
	require.NoError(t, err)
	assert.False(t, again, "第二次终结 CAS 落空")
	assert.Equal(t, uint64(1000), econFunds(t, f, guildID), "对侧账只记一次")
	assert.Len(t, pushed, 1, "只有本次终结才推送")
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Equal(t, uint32(1), used, "APPLIED 保留次数")
}

// TestFinalize_DonateAppliedGuildGoneIsOrphan:D2 —— 绑定的帮会已解散,资金与帮贡都记不上,
// 计 orphan{donate,guild_gone},不补偿,也不能凭空把帮会行写回来。
func TestFinalize_DonateAppliedGuildGoneIsOrphan(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7621
		leader  uint64 = 8621
		donor   uint64 = 8622
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, 9_620_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)
	_, err = f.repo.DisbandGuild(f.ctx, guildID, leader, testNowMs+10)
	require.NoError(t, err)

	orphans := econCaptureOrphans(t)
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: in.OpID, PlayerID: donor},
		assetop.StatusApplied, econAppliedResult(), testNowMs+20)
	require.NoError(t, err)
	assert.True(t, finalized, "帮会不在也要终结,否则这一行会被永远重投")
	assert.Equal(t, []string{"donate/guild_gone"}, orphans())
	assert.Zero(t, guildRowCount(t, f.ctx, f.db, guildID), "资金无处可记,帮会行不能被写回来")
	assert.Zero(t, memberCount(t, f.ctx, f.db, donor))
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, econRecord(t, f, in.OpID).GetStatus())
}

// TestFinalize_DonateAppliedMemberGoneCreditsFundsOnly:D2 —— 捐献者已离帮而帮会还在,资金照记给
// op.guild_id,帮贡跳过,计 orphan{donate,member_gone}。
func TestFinalize_DonateAppliedMemberGoneCreditsFundsOnly(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7631
		leader  uint64 = 8631
		donor   uint64 = 8632
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, 9_630_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)
	_, err = f.repo.LeaveGuild(f.ctx, guildID, donor, testNowMs+10)
	require.NoError(t, err)

	orphans := econCaptureOrphans(t)
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: in.OpID, PlayerID: donor},
		assetop.StatusApplied, econAppliedResult(), testNowMs+20)
	require.NoError(t, err)
	assert.True(t, finalized)
	assert.Equal(t, uint64(1000), econFunds(t, f, guildID), "资金照记给发起时绑定的帮会")
	_, _, found := econContribution(t, f, guildID, donor)
	assert.False(t, found, "成员行不能被写回来")
	assert.Equal(t, []string{"donate/member_gone"}, orphans())
	leaderTotal, _, _ := econContribution(t, f, guildID, leader)
	assert.Zero(t, leaderTotal, "帮贡不能错记到别人头上")
}

// TestFinalize_DonateRejectedOrAbortedRefundsDailyCount:REJECTED / ABORTED 退回今日次数;
// reason_tip_id 只在 REJECTED 时写 scene 的原因码(ABORTED 写 0);资金不动。
func TestFinalize_DonateRejectedOrAbortedRefundsDailyCount(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7641
		leader  uint64 = 8641
		donor   uint64 = 8642
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})

	rejected := econDonation(t, 9_640_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, rejected)
	require.NoError(t, err)
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: rejected.OpID, PlayerID: donor}, assetop.StatusRejected,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Reason: assetop.ReasonCurrencyInsufficient, Durable: true},
		testNowMs+10)
	require.NoError(t, err)
	require.True(t, finalized)
	used, _ := econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Zero(t, used, "REJECTED 退回次数")
	rec := econRecord(t, f, rejected.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, rec.GetStatus())
	assert.Equal(t, assetop.ReasonCurrencyInsufficient, rec.GetReasonTipId())

	aborted := econDonation(t, 9_640_002, donor, guildID, testNowMs+20)
	_, err = f.econ.ReserveDonation(f.ctx, aborted)
	require.NoError(t, err)
	finalized, err = f.store.Finalize(f.ctx, assetop.Op{OpID: aborted.OpID, PlayerID: donor}, assetop.StatusAborted,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}, testNowMs+30)
	require.NoError(t, err)
	require.True(t, finalized)
	used, _ = econCounterUsed(t, f, donor, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Zero(t, used, "ABORTED 退回次数")
	assert.Zero(t, econRecord(t, f, aborted.OpID).GetReasonTipId(), "ABORTED 的 reason_tip_id 写 0")
	assert.Zero(t, econFunds(t, f, guildID), "被拒 / 中止的捐献不记资金")
}

// TestFinalize_AppliedPartialOnlyCAS:部分发放只终结、不做对侧账(X-03):商店不退帮贡不退限购,捐献不记资金不退次数。
func TestFinalize_AppliedPartialOnlyCAS(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7651
		leader  uint64 = 8651
		player  uint64 = 8652
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	econSetContribution(t, f, guildID, player, 1000, 1000)

	shop := econShopOrder(t, 9_650_001, player, guildID, 2, 300, 5, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, shop)
	require.NoError(t, err)
	donate := econDonation(t, 9_650_002, player, guildID, testNowMs+1)
	_, err = f.econ.ReserveDonation(f.ctx, donate)
	require.NoError(t, err)

	partial := assetop.Result{
		Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Reason: assetop.ReasonPartialApplied,
		Durable: true, Partial: true,
	}
	for _, opID := range []uint64{shop.OpID, donate.OpID} {
		finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: opID, PlayerID: player},
			assetop.StatusAppliedPartial, partial, testNowMs+50)
		require.NoError(t, err)
		require.True(t, finalized)
		rec := econRecord(t, f, opID)
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL, rec.GetStatus())
		assert.Equal(t, testNowMs+50, rec.GetNextAttemptMs())
		assert.Zero(t, rec.GetReasonTipId())
	}
	_, balance, _ := econContribution(t, f, guildID, player)
	assert.Equal(t, uint64(700), balance, "部分发放不退帮贡")
	shopUsed, _ := econCounterUsed(t, f, player, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.Equal(t, uint32(2), shopUsed, "部分发放不退限购")
	donateUsed, _ := econCounterUsed(t, f, player, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, econDayKey)
	assert.Equal(t, uint32(1), donateUsed, "部分发放不退次数")
	assert.Zero(t, econFunds(t, f, guildID), "部分发放不记资金")
}

// TestFinalize_ShopRejectedRefundsContributionAndLimit:永久拒绝退帮贡与限购;兑换者已离帮时帮贡退不回去,
// 计 orphan{shop,refund_member_gone},限购照退。
func TestFinalize_ShopRejectedRefundsContributionAndLimit(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7661
		leader  uint64 = 8661
		buyer   uint64 = 8662
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{buyer: constants.RoleMember})
	econSetContribution(t, f, guildID, buyer, 1000, 1000)

	first := econShopOrder(t, 9_660_001, buyer, guildID, 2, 300, 5, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, first)
	require.NoError(t, err)
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: first.OpID, PlayerID: buyer}, assetop.StatusRejected,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Reason: assetop.ReasonBagFull, Durable: true},
		testNowMs+10)
	require.NoError(t, err)
	require.True(t, finalized)
	_, balance, _ := econContribution(t, f, guildID, buyer)
	assert.Equal(t, uint64(1000), balance, "永久拒绝退帮贡")
	used, _ := econCounterUsed(t, f, buyer, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.Zero(t, used, "永久拒绝退限购")
	assert.Equal(t, assetop.ReasonBagFull, econRecord(t, f, first.OpID).GetReasonTipId())

	second := econShopOrder(t, 9_660_002, buyer, guildID, 1, 150, 5, testNowMs+20)
	_, err = f.econ.ReserveShopOrder(f.ctx, second)
	require.NoError(t, err)
	_, err = f.repo.LeaveGuild(f.ctx, guildID, buyer, testNowMs+30)
	require.NoError(t, err)

	orphans := econCaptureOrphans(t)
	finalized, err = f.store.Finalize(f.ctx, assetop.Op{OpID: second.OpID, PlayerID: buyer}, assetop.StatusAborted,
		assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}, testNowMs+40)
	require.NoError(t, err)
	require.True(t, finalized)
	assert.Equal(t, []string{"shop/refund_member_gone"}, orphans())
	_, _, found := econContribution(t, f, guildID, buyer)
	assert.False(t, found, "离帮者的成员行不能被退款写回来")
	used, _ = econCounterUsed(t, f, buyer, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.Zero(t, used, "限购属于玩家,照退")
}

// TestResolveManually_WritesAuditAndNextAttempt:人工终结经包级 assetop.ResolveManually 走同一把 CAS 与同一份对侧账,
// 留下操作人与依据、同写 next_attempt_ms = now,不置 durable、不改 last_outcome;之后自动终结落空。
func TestResolveManually_WritesAuditAndNextAttempt(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7671
		leader  uint64 = 8671
		donor   uint64 = 8672
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, 9_670_001, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)

	resolveNow := testNowMs + 7_200_000
	clock := func() time.Time { return time.UnixMilli(int64(resolveNow)) }
	manual := assetop.ManualResolution{OpID: in.OpID, Final: assetop.StatusApplied, Operator: "ops-alice", Reason: "交易流水已核对,已扣款"}
	resolved, err := assetop.ResolveManually(f.ctx, f.store, manual, nil, clock)
	require.NoError(t, err)
	assert.True(t, resolved)

	rec := econRecord(t, f, in.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, rec.GetStatus())
	assert.Equal(t, "ops-alice", rec.GetResolvedBy())
	assert.Equal(t, manual.Reason, rec.GetResolveReason())
	assert.Equal(t, resolveNow, rec.GetNextAttemptMs(), "人工终结同样同写 next_attempt_ms = now")
	assert.Equal(t, resolveNow, rec.GetUpdatedMs())
	assert.Zero(t, rec.GetDurable(), "人工终结不是 scene 确认的落盘结局,不置 durable")
	assert.Zero(t, rec.GetLastOutcome(), "last_outcome 保留最后一次 scene 真实答复")
	assert.Zero(t, rec.GetLeaseUntilMs())
	assert.Equal(t, uint64(1000), econFunds(t, f, guildID), "人工判 APPLIED 同样记资金")

	resolved, err = assetop.ResolveManually(f.ctx, f.store, manual, nil, clock)
	require.NoError(t, err)
	assert.False(t, resolved, "已非 PENDING")
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: in.OpID, PlayerID: donor},
		assetop.StatusApplied, econAppliedResult(), resolveNow+1)
	require.NoError(t, err)
	assert.False(t, finalized, "人工与自动只有一个赢家")
	assert.Equal(t, uint64(1000), econFunds(t, f, guildID))
}

// ── 离帮 / 被踢 / 解散提前截止 ───────────────────────────────

// TestAccelerate_LeaveKickDisbandPullDeadlineToNow:三个事务把本帮未决捐献的截止提前到 now、
// next_attempt_ms 拉回 now,不抢租约;商店指令与绑定别的帮会的捐献一律不动。
func TestAccelerate_LeaveKickDisbandPullDeadlineToNow(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7681
		other   uint64 = 7682
		leader  uint64 = 8681
		leaver  uint64 = 8682
		kicked  uint64 = 8683
		stayer  uint64 = 8684
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{
		leaver: constants.RoleMember, kicked: constants.RoleMember, stayer: constants.RoleMember,
	})
	seedManagedGuild(t, f.ctx, f.db, other, 2, 1, 50, 8685, nil)
	econSetContribution(t, f, guildID, leaver, 500, 500)

	reserve := func(opID, playerID uint64) DonationReserve {
		in := econDonation(t, opID, playerID, guildID, testNowMs)
		_, err := f.econ.ReserveDonation(f.ctx, in)
		require.NoError(t, err)
		return in
	}
	leaverDonation := reserve(9_680_001, leaver)
	kickedDonation := reserve(9_680_002, kicked)
	stayerDonation := reserve(9_680_003, stayer)
	leaverShop := econShopOrder(t, 9_680_004, leaver, guildID, 1, 30, 0, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, leaverShop)
	require.NoError(t, err)
	// 绑定别的帮会的一笔(比如早先在那边发起、至今未决):过滤条件 guild_id 必须把它排除。
	foreign := &pb.GuildAssetOpRecord{
		OpId: 9_680_005, PlayerId: leaver, Stream: uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT),
		Seq: 100, GuildId: other, Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE,
		Status:        pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING,
		NextAttemptMs: testNowMs + 30_000, DeadlineMs: testNowMs + econDeadlineMs, Payload: econDonatePayload(t, 100),
		RefId: 1, RefCount: 1, PeriodKey: econDayKey, CreatedMs: testNowMs, UpdatedMs: testNowMs,
		TxType: uint32(rollbackpb.TransactionType_TX_GUILD_DONATE), StreamEpoch: testNowMs,
	}
	econInsertOp(t, f, foreign)

	leaveAt, kickAt, disbandAt := testNowMs+100, testNowMs+200, testNowMs+300
	_, err = f.repo.LeaveGuild(f.ctx, guildID, leaver, leaveAt)
	require.NoError(t, err)
	rec := econRecord(t, f, leaverDonation.OpID)
	assert.Equal(t, leaveAt, rec.GetDeadlineMs(), "退帮:截止提前到 now")
	assert.Equal(t, leaveAt, rec.GetNextAttemptMs(), "退帮:next_attempt_ms 拉回 now")
	assert.Equal(t, leaverDonation.LeaseUntilMs, rec.GetLeaseUntilMs(), "不抢租约")
	assert.Equal(t, kickedDonation.DeadlineMs, econRecord(t, f, kickedDonation.OpID).GetDeadlineMs(), "别人的捐献不动")
	shopRec := econRecord(t, f, leaverShop.OpID)
	assert.Zero(t, shopRec.GetDeadlineMs(), "商店指令永不中止,不被提前截止")
	assert.Equal(t, leaverShop.LeaseUntilMs, shopRec.GetNextAttemptMs())
	foreignRec := econRecord(t, f, foreign.GetOpId())
	assert.Equal(t, foreign.GetDeadlineMs(), foreignRec.GetDeadlineMs(), "绑定别的帮会的捐献不动")
	assert.Equal(t, foreign.GetNextAttemptMs(), foreignRec.GetNextAttemptMs())

	_, err = f.repo.KickMember(f.ctx, guildID, leader, kicked, kickAt)
	require.NoError(t, err)
	assert.Equal(t, kickAt, econRecord(t, f, kickedDonation.OpID).GetDeadlineMs(), "被踢:截止提前到 now")

	_, err = f.repo.DisbandGuild(f.ctx, guildID, leader, disbandAt)
	require.NoError(t, err)
	assert.Equal(t, disbandAt, econRecord(t, f, stayerDonation.OpID).GetDeadlineMs(), "解散:全体成员的捐献截止提前到 now")
	assert.Equal(t, leaveAt, econRecord(t, f, leaverDonation.OpID).GetDeadlineMs(), "已到期的行不会被再改")
	assert.Equal(t, kickAt, econRecord(t, f, kickedDonation.OpID).GetDeadlineMs())
}

// ── 清理 ─────────────────────────────────────────────────────

// TestCleanupOnce_KeepsPendingAndPartial:只删保留期外的 APPLIED / REJECTED / ABORTED;APPLIED_PARTIAL(待人工补偿)
// 与 PENDING 永不自动删;过期的日键 / 周键计数行删掉,当期的保留。
func TestCleanupOnce_KeepsPendingAndPartial(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8691
	now := time.UnixMilli(int64(testNowMs))
	conf := CleanupConf{Interval: time.Minute, TerminalRetention: 30 * 24 * time.Hour, CounterRetention: 30 * 24 * time.Hour}
	oldMs := testNowMs - 31*econDayMs
	recentMs := testNowMs - econDayMs

	type opCase struct {
		opID     uint64
		status   pb.GuildAssetOpStatus
		finalMs  uint64
		survives bool
	}
	cases := []opCase{
		{701, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, oldMs, false},
		{702, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, oldMs, false},
		{703, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED, oldMs, false},
		{704, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL, oldMs, true},
		{705, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, oldMs, true},
		{706, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, recentMs, true},
	}
	for i, c := range cases {
		rec := econShopRecord(t, c.opID, player, oldMs, uint64(i+1))
		rec.Status = c.status
		rec.NextAttemptMs = c.finalMs
		rec.CreatedMs, rec.UpdatedMs = oldMs, c.finalMs
		econInsertOp(t, f, rec)
	}

	insertCounter := func(periodKey uint32) {
		mustExec(t, f.ctx, f.db,
			`INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms) VALUES (?, ?, ?, ?, ?, ?)`,
			player, int32(pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP), 101, periodKey, 1, testNowMs)
	}
	oldDay := gameday.DayKey(now.Add(-31 * 24 * time.Hour))
	recentDay := gameday.DayKey(now.Add(-24 * time.Hour))
	oldWeek := gameday.WeekKey(now.Add(-60 * 24 * time.Hour))
	currentWeek := gameday.WeekKey(now)
	for _, key := range []uint32{oldDay, recentDay, oldWeek, currentWeek} {
		insertCounter(key)
	}

	deleted := map[string]int64{}
	original := recordCleanupDeleted
	recordCleanupDeleted = func(table string, n int64) { deleted[table] += n }
	t.Cleanup(func() { recordCleanupDeleted = original })

	require.NoError(t, f.store.CleanupOnce(f.ctx, now, conf))

	for _, c := range cases {
		assert.Equal(t, c.survives, econOpExists(t, f, c.opID), "op %d status=%s", c.opID, c.status)
	}
	for key, survives := range map[uint32]bool{oldDay: false, recentDay: true, oldWeek: false, currentWeek: true} {
		_, found := econCounterUsed(t, f, player, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, key)
		assert.Equal(t, survives, found, "period_key=%d", key)
	}
	assert.Equal(t, map[string]int64{"guild_asset_op": 3, "guild_daily_counter": 2}, deleted)
}

// ── 读查询 ───────────────────────────────────────────────────

// TestRecentOps_OrderedByEpochThenSeqDesc:最近结果按 (stream_epoch, seq) 倒序,新纪元整体排在旧纪元之前
// (只按 seq 排会把库恢复前后的两个纪元交错);待结算按同一对键升序;别的流不混进来。
func TestRecentOps_OrderedByEpochThenSeqDesc(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8701
	oldEpoch, newEpoch := testNowMs-200_000, testNowMs-100_000

	insert := func(opID, epoch, seq uint64, status pb.GuildAssetOpStatus) {
		rec := econShopRecord(t, opID, player, epoch, seq)
		rec.Status = status
		econInsertOp(t, f, rec)
	}
	insert(801, oldEpoch, 1, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED)
	insert(802, oldEpoch, 2, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING)
	insert(803, oldEpoch, 3, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED)
	insert(804, newEpoch, 1, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING)
	debit := econShopRecord(t, 805, player, newEpoch, 9)
	debit.Stream = uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT)
	econInsertOp(t, f, debit)

	recent, err := f.econ.RecentOps(f.ctx, player, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, 3)
	require.NoError(t, err)
	assert.Equal(t, []uint64{804, 803, 802}, econOpIDs(recent))

	pending, err := f.econ.PendingOps(f.ctx, player, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, 16)
	require.NoError(t, err)
	assert.Equal(t, []uint64{802, 804}, econOpIDs(pending))
	for _, row := range pending {
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, row.Status)
		assert.Equal(t, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, row.Stream)
	}

	stuck, err := f.store.ListStuck(f.ctx, testNowMs, 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uint64{802, 804, 805}, econOpIDs(stuck))
	row, found, err := f.store.GetOp(f.ctx, 803)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, row.Status)
	_, found, err = f.store.GetOp(f.ctx, 999)
	require.NoError(t, err)
	assert.False(t, found)
}

func econOpIDs(rows []AssetOpRow) []uint64 {
	out := make([]uint64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.OpID)
	}
	return out
}
