//go:build integration

package data

// asset_op_landing_integration_test.go —— 落地恢复(docs/design/landing-recovery-sync-digest.md §5.3 / §6)在真 MySQL 的
// trade_asset_op 上的回归。用例按 guild 的 asset_store_landing_test.go 逐条移植,名字同形(带 LastSendMs / ReleaseLease /
// Abandon / ListAbandonCandidates / WakePlayer / AbandonOnce / CleanupTerminalOps),便于 `-run` 一次选中。
//
// 为什么录制驱动(asset_op_recovery_test.go)不够:自动放弃的安全性依赖下面三件事,它们只核对得了 SQL 文本与实参顺序,
// 证明不了库真的这么做 —— 只能在真库上证:
//  1. 领取 CAS 的 SET 从左到右求值:last_send_ms 一项读到的是**旧** lease_until_ms 与**旧** updated_ms
//     (TestClaim_LastSendMsLazyInitAndFoldsUnfinishedLease 的多种行形态,校验落库值等于回读值);
//  2. 重排的 IF(?, GREATEST(`last_send_ms`, ?), `last_send_ms`) 绑 bool(!res.Local)与 uint64 的结果
//     (TestReschedule_LastSendMsAdvancesOnlyWhenMaybeSent 的 Local=true / false);
//  3. 令牌对不上的放弃 / 归还 / 重排一个字都不写(TestAbandon_LeaseLostLeavesRowUntouched 等)。
// 另有 AbandonOnce / WakePlayer 接 AssetOpRepo 的端到端,以及终态行清理的保留口径。
//
// 跑法与门控同 listing_repo_integration_test.go:TRADE_TEST_MYSQL_DSN 为空即 Skip(⚠ 只能指向一次性库),
// 验收时再设 TRADE_REQUIRE_MYSQL_TESTS=1 把 Skip 判红。**整组 Skip 不代表通过。**
// 夹具复用 openSeqGuardDB(经 openIntegrationDB 按 proto 建表,再 TRUNCATE outbox 与 seq 两张表),
// 每个用例自己重建、互不依赖执行顺序;时间一律由 itNow 派生,不读真实墙钟(AGENTS §11.4)。

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	rollbackpb "proto/common/rollback"
	tradepb "proto/trade"

	"shared/assetop"
	"shared/scenenode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	// landingLeaseMs 是夹具领取用的租约长度,与 reconcile 的 loopLease(10s)同值。
	landingLeaseMs uint64 = 10_000
	// landingPoisonMs 是毒行推迟时长,与 reconcile 的 loopPoisonDelay(1h)同值。
	landingPoisonMs uint64 = 3_600_000
	landingDayMs    uint64 = 24 * 60 * 60 * 1000
	// landingEpoch 是夹具行的流纪元。非 0 且比空账本新:ClassifySeq 按空账本看,seq ≤ 1024 判"确定未见"。
	landingEpoch uint64 = 1_700_000_000_000
)

var (
	landingDebit  = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT
	landingCredit = assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT
)

// ── 真库夹具 ──────────────────────────────────────────────────

type landingFixture struct {
	ctx  context.Context
	db   *sql.DB
	repo *AssetOpRepo
}

// openLandingFixture 建一份干净的 outbox(门控、建表、清空都在 openSeqGuardDB 里)。
func openLandingFixture(t *testing.T) landingFixture {
	t.Helper()
	db, repo := openSeqGuardDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return landingFixture{ctx: ctx, db: db, repo: repo}
}

// landingPayload 是一份合法的托管资产包(Debit v1:恰一条货币)。领取时 payload 解不开会走毒行分支,所以夹具必须给合法值。
func landingPayload(t *testing.T) []byte {
	t.Helper()
	b, err := proto.Marshal(&assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: 1, Amount: 100}}})
	require.NoError(t, err)
	return b
}

// landingRow 造一行未决指令:DEBIT 流记 ESCROW_DEBIT、CREDIT 流记 DELIVER_CREDIT;创建于 itNow − 100s、已到期、未被领取。
// 再由 mutate 改状态 / 次数 / 时刻。唯一键是 (player_id, stream, stream_epoch, seq),同一玩家同一流的 seq 由调用方给不同值。
func landingRow(t *testing.T, opID, playerID uint64, stream assetpb.AssetOpStream, seq uint64,
	mutate func(*tradepb.TradeAssetOpRecord)) *tradepb.TradeAssetOpRecord {
	t.Helper()
	kind := tradepb.TradeAssetOpKind_TRADE_ASSET_OP_KIND_ESCROW_DEBIT
	if stream == landingCredit {
		kind = tradepb.TradeAssetOpKind_TRADE_ASSET_OP_KIND_DELIVER_CREDIT
	}
	rec := &tradepb.TradeAssetOpRecord{
		OpId:          opID,
		PlayerId:      playerID,
		Stream:        uint32(stream),
		StreamEpoch:   landingEpoch,
		Seq:           seq,
		Kind:          kind,
		Status:        tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING,
		NextAttemptMs: itNow - 1000,
		TxType:        uint32(rollbackpb.TransactionType_TX_AUCTION_SELL),
		RefKind:       tradepb.TradeAssetOpRefKind_TRADE_ASSET_OP_REF_KIND_LISTING,
		RefId:         opID,
		Payload:       landingPayload(t),
		CreatedMs:     itNow - 100_000,
		UpdatedMs:     itNow - 100_000,
	}
	if mutate != nil {
		mutate(rec)
	}
	return rec
}

// landingInsert 经产品路径 InsertOp(含 last_send_ms 不小于 created_ms 的兜底)在一个 RC 事务里插入并提交。
func landingInsert(t *testing.T, f landingFixture, recs ...*tradepb.TradeAssetOpRecord) {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	for _, rec := range recs {
		if err := f.repo.InsertOp(f.ctx, tx, rec); err != nil {
			_ = tx.Rollback()
			t.Fatalf("夹具:插入 op %d 失败: %v", rec.GetOpId(), err)
		}
	}
	require.NoError(t, tx.Commit())
}

// landingSetLastSendMs 直接改 last_send_ms:模拟 schemamigrate ADD COLUMN 之后的存量行(0)或某个已维护的值。
// InsertOp 会把它抬到不小于 created_ms,所以只能事后改。
func landingSetLastSendMs(t *testing.T, f landingFixture, opID, v uint64) {
	t.Helper()
	res, err := f.db.ExecContext(f.ctx, "UPDATE "+AssetOpTableName+" SET `last_send_ms` = ? WHERE `op_id` = ?", v, opID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	// 值没变时 MySQL 默认回 0 行(found rows 未开),所以只要求不超过 1 行。
	require.LessOrEqual(t, n, int64(1))
}

// landingRecord 按主键读回整行(产品路径 getOp)。
func landingRecord(t *testing.T, f landingFixture, opID uint64) *tradepb.TradeAssetOpRecord {
	t.Helper()
	rec, err := f.repo.getOp(f.ctx, opID)
	require.NoError(t, err, "读 op %d", opID)
	return rec
}

// landingClaim 以 nowMs 领一行(租约 landingLeaseMs),要求领到。
func landingClaim(t *testing.T, f landingFixture, opID, nowMs, token uint64) assetop.Op {
	t.Helper()
	op, claimed, err := f.repo.Claim(f.ctx, opID, nowMs, nowMs+landingLeaseMs, nowMs+landingPoisonMs, token)
	require.NoError(t, err, "claim op %d", opID)
	require.True(t, claimed, "claim op %d", opID)
	return op
}

// landingOpIDs 列出表里剩下的全部 op_id(升序)。
func landingOpIDs(t *testing.T, f landingFixture) []uint64 {
	t.Helper()
	ids, err := f.repo.queryOpIDs(f.ctx, "SELECT `op_id` FROM "+AssetOpTableName+" ORDER BY `op_id` ASC")
	require.NoError(t, err)
	return ids
}

func landingNotHereLocal() assetop.Result {
	return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true, Absent: true}
}

// landingLocator 是 assetop.Resolver 的替身:登记了的玩家按登记的错误回(nil = 有位置),没登记的一律"位置键不存在"。
type landingLocator map[uint64]error

func (l landingLocator) Resolve(_ context.Context, playerID uint64) (scenenode.Target, error) {
	if err, ok := l[playerID]; ok {
		return scenenode.Target{}, err
	}
	return scenenode.Target{}, scenenode.ErrNotOnline
}

// landingLedger 是 assetop.LedgerReader 的替身:每个玩家都有一份空 blob。对纪元非 0、seq ≤ 1024 的行它给出"确定未见"。
type landingLedger struct{}

func (landingLedger) ReadPersistedLedger(context.Context, uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return &componentpb.PlayerAssetOpLedgerComp{}, nil
}

// ReadVerifiedPersistedLedger 的替身代表同次读取已核验的空账本，保留生产默认核验门禁。
// 缺账本或读取错误不构成已核验事实。
func (l landingLedger) ReadVerifiedPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, bool, error) {
	ledger, err := l.ReadPersistedLedger(ctx, playerID)
	return ledger, ledger != nil && err == nil, err
}

// landingApplier 记下每一次投递并一律回"已落盘的全额应用"。
type landingApplier struct {
	mu    sync.Mutex
	calls []*assetpb.AssetOpRequest
}

func (a *landingApplier) Do(_ context.Context, _ assetop.RPC, req *assetpb.AssetOpRequest) (assetop.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, req)
	return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}, nil
}

func (a *landingApplier) requests() []*assetpb.AssetOpRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*assetpb.AssetOpRequest(nil), a.calls...)
}

// landingLoop 建一个跑在真库 AssetOpRepo 上的重投循环,时钟固定在 nowMs。
func landingLoop(t *testing.T, f landingFixture, applier assetop.Applier, nowMs uint64) *assetop.Loop {
	t.Helper()
	loop, err := assetop.NewLoop(assetop.DefaultLoopConfig(), f.repo, applier, nil,
		func() time.Time { return time.UnixMilli(int64(nowMs)) })
	require.NoError(t, err)
	return loop
}

// ── last_send_ms 契约 ─────────────────────────────────────────

// TestInsertOp_WritesLastSendMsAtLeastCreatedMs:契约第 1 条 —— 插行即持租约,提交后的同步投递紧接着就可能发出,
// 所以落库的 last_send_ms 不小于 created_ms;生产者给了更晚的值照用(只许偏大)。
func TestInsertOp_WritesLastSendMsAtLeastCreatedMs(t *testing.T) {
	f := openLandingFixture(t)
	const player uint64 = 9701
	created := itNow - 1000
	landingInsert(t, f,
		landingRow(t, 701, player, landingDebit, 1, func(r *tradepb.TradeAssetOpRecord) { r.CreatedMs = created }),
		landingRow(t, 702, player, landingDebit, 2, func(r *tradepb.TradeAssetOpRecord) { r.CreatedMs, r.LastSendMs = created, created-500 }),
		landingRow(t, 703, player, landingDebit, 3, func(r *tradepb.TradeAssetOpRecord) { r.CreatedMs, r.LastSendMs = created, created+5 }),
	)
	assert.Equal(t, created, landingRecord(t, f, 701).GetLastSendMs(), "生产者漏填:取 created_ms")
	assert.Equal(t, created, landingRecord(t, f, 702).GetLastSendMs(), "比 created_ms 小的值抬到 created_ms")
	assert.Equal(t, created+5, landingRecord(t, f, 703).GetLastSendMs(), "生产者给的更晚时刻原样保留")
}

// TestClaim_LastSendMsLazyInitAndFoldsUnfinishedLease:契约第 2、5 条 —— 领取的 CAS 在改写租约**之前**把上一个租约折进来;
// 存量行(0)懒初始化为 GREATEST(created_ms, updated_ms);已维护的值不被 updated_ms 拉动(活性)。回读填 CreatedMs / LastSendMs,
// 且回读值等于落库值。这一组同时证明 UPDATE 从左到右求值时 last_send_ms 一项读到的是**旧**租约与**旧** updated_ms:
// 读到新值的话,802 / 804 会得到 now + 租约,801 会得到 now。
func TestClaim_LastSendMsLazyInitAndFoldsUnfinishedLease(t *testing.T) {
	f := openLandingFixture(t)
	const player uint64 = 9721
	createdMs := itNow - 5000
	// leaseUntilMs 是上一个租约:非 0 且已过期 = 没有正常收尾(持有者发完请求就崩溃)。lastSendMs 为 0 = 存量行。
	type row struct {
		opID         uint64
		updatedMs    uint64
		leaseUntilMs uint64
		lastSendMs   uint64
		want         uint64
		why          string
	}
	rows := []row{
		{801, itNow - 3000, 0, 0, itNow - 3000, "存量行:懒初始化取 GREATEST(created_ms, updated_ms)"},
		{802, itNow - 3000, itNow - 100, 0, itNow - 100, "存量行 + 未收尾租约:初值再与旧租约取较大值"},
		{803, itNow - 10, 0, itNow - 4000, itNow - 4000, "已维护:updated_ms 再新也不拉动它(本地重排不算发送)"},
		{804, itNow - 10, itNow - 50, itNow - 4000, itNow - 50, "已维护 + 未收尾租约:折进旧租约"},
		{805, itNow - 10, itNow - 4500, itNow - 4000, itNow - 4000, "旧租约早于已记的发送:取较大值"},
		{806, createdMs, 0, 0, createdMs, "存量行且 updated_ms 未动过:初值取 created_ms"},
	}
	for i, r := range rows {
		landingInsert(t, f, landingRow(t, r.opID, player, landingDebit, uint64(i+1), func(rec *tradepb.TradeAssetOpRecord) {
			rec.CreatedMs, rec.UpdatedMs, rec.LeaseUntilMs = createdMs, r.updatedMs, r.leaseUntilMs
		}))
		landingSetLastSendMs(t, f, r.opID, r.lastSendMs)
	}
	for _, r := range rows {
		token := 0xc00 + r.opID
		op := landingClaim(t, f, r.opID, itNow, token)
		assert.Equal(t, r.want, op.LastSendMs, "op %d:%s", r.opID, r.why)
		assert.Equal(t, createdMs, op.CreatedMs, "op %d:回读填 CreatedMs", r.opID)
		assert.Equal(t, token, op.LeaseToken, "op %d:回读带回本次令牌", r.opID)
		stored := landingRecord(t, f, r.opID)
		assert.Equal(t, r.want, stored.GetLastSendMs(), "op %d:落库值与回读一致", r.opID)
		assert.Equal(t, itNow+landingLeaseMs, stored.GetLeaseUntilMs(), "op %d:租约照常写新值", r.opID)
		assert.Equal(t, itNow, stored.GetUpdatedMs(), "op %d:updated_ms 写领取时刻", r.opID)
		assert.Equal(t, token, stored.GetLeaseToken(), "op %d", r.opID)
	}

	// 租约未到期:别的副本领不到,last_send_ms 也不能被动。
	_, claimed, err := f.repo.Claim(f.ctx, 803, itNow+1, itNow+1+landingLeaseMs, itNow+1+landingPoisonMs, 0xd02)
	require.NoError(t, err)
	assert.False(t, claimed, "租约未到期不得被领走")
	assert.Equal(t, itNow-4000, landingRecord(t, f, 803).GetLastSendMs(), "领取落空时一个字都不写")

	// 持有者领到之后崩溃、没来得及重排:租约过期后别的副本再领,last_send_ms 必须盖住那次可能的发送(不晚于旧租约到期)。
	op := landingClaim(t, f, 803, itNow+landingLeaseMs+1, 0xd03)
	assert.Equal(t, itNow+landingLeaseMs, op.LastSendMs, "未收尾的租约被折进 last_send_ms")
	assert.Equal(t, itNow+landingLeaseMs, landingRecord(t, f, 803).GetLastSendMs())
}

// TestReschedule_LastSendMsAdvancesOnlyWhenMaybeSent:契约第 3 条 —— 本地 NOT_HERE(Local=true)不推进(否则停放中的离线行
// 永远"刚发过",自动放弃永远不会发生),其余结果(含传输失败的零值结果)一律按发过推进到本次 ProcessOne 的开始时刻,
// 且取较大值不倒退;令牌落空时一个字都不写。这一组证明 IF(?, …) 绑 bool 在真库上的结果。
func TestReschedule_LastSendMsAdvancesOnlyWhenMaybeSent(t *testing.T) {
	f := openLandingFixture(t)
	const player uint64 = 9731
	landingInsert(t, f, landingRow(t, 811, player, landingDebit, 1, func(r *tradepb.TradeAssetOpRecord) {
		r.CreatedMs, r.UpdatedMs = itNow-5000, itNow-5000
	}))
	retry := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY}

	op := landingClaim(t, f, 811, itNow, 0xe1)
	require.Equal(t, itNow-5000, op.LastSendMs)
	require.NoError(t, f.repo.Reschedule(f.ctx, op, itNow+1_800_000, landingNotHereLocal(), itNow+5))
	stored := landingRecord(t, f, 811)
	assert.Equal(t, itNow-5000, stored.GetLastSendMs(), "Local=true:本地 NOT_HERE 没发请求,不推进")
	assert.Equal(t, itNow+5, stored.GetUpdatedMs(), "updated_ms 照写 —— 这正是放弃不能看 updated_ms 的原因")
	assert.Equal(t, uint32(1), stored.GetAttempts())
	assert.Equal(t, itNow+1_800_000, stored.GetNextAttemptMs())
	assert.Zero(t, stored.GetLeaseUntilMs())
	assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE), stored.GetLastOutcome())

	op = landingClaim(t, f, 811, itNow+10, 0xe2)
	assert.Equal(t, itNow-5000, op.LastSendMs, "重排已清租约,下一次领取没有可折的旧租约")
	require.NoError(t, f.repo.Reschedule(f.ctx, op, itNow+1000, retry, itNow+20))
	assert.Equal(t, itNow+20, landingRecord(t, f, 811).GetLastSendMs(), "Local=false(scene 回 RETRY):发过,推进到 now")

	op = landingClaim(t, f, 811, itNow+30, 0xe3)
	require.NoError(t, f.repo.Reschedule(f.ctx, op, itNow+1000, assetop.Result{}, itNow+15))
	assert.Equal(t, itNow+20, landingRecord(t, f, 811).GetLastSendMs(), "now 比已记的值旧(副本时钟偏差):取较大值,不倒退")

	op = landingClaim(t, f, 811, itNow+40, 0xe4)
	require.NoError(t, f.repo.Reschedule(f.ctx, op, itNow+1000, assetop.Result{}, itNow+45))
	assert.Equal(t, itNow+45, landingRecord(t, f, 811).GetLastSendMs(), "传输失败(零值结果,非 Local)按发过推进")

	op = landingClaim(t, f, 811, itNow+50, 0xe5)
	stolen := op
	stolen.LeaseToken = 0xe6
	assert.ErrorIs(t, f.repo.Reschedule(f.ctx, stolen, itNow+1000, retry, itNow+60), assetop.ErrLeaseLost)
	stored = landingRecord(t, f, 811)
	assert.Equal(t, itNow+45, stored.GetLastSendMs(), "租约不是我的:last_send_ms 也不能写")
	assert.Equal(t, uint32(4), stored.GetAttempts(), "落空的重排不算一次投递")
	assert.Equal(t, itNow+50+landingLeaseMs, stored.GetLeaseUntilMs(), "落空时租约原样留给真正的持有者")
}

// TestReleaseLease_AndPoisonKeepLastSendMs:契约第 4 条 —— 毒行推迟与放弃判定后的归还租约只清租约,不动 last_send_ms;
// 归还也不动 attempts / next_attempt_ms / 令牌;令牌对不上或行已终结回 ErrLeaseLost 且一个字不写。
func TestReleaseLease_AndPoisonKeepLastSendMs(t *testing.T) {
	f := openLandingFixture(t)
	const player uint64 = 9741
	keep := itNow - 4000

	// 毒行推迟(直接调):对"持租约、已维护 last_send_ms"的行调 markPoison,排除领取那一步对这一列的影响。
	const poisonToken uint64 = 0xf1
	landingInsert(t, f, landingRow(t, 821, player, landingDebit, 1, func(r *tradepb.TradeAssetOpRecord) {
		r.Payload = []byte{0xff, 0xff}
		r.CreatedMs, r.LeaseUntilMs, r.LeaseToken = itNow-5000, itNow+landingLeaseMs, poisonToken
		r.LastOutcome = uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY)
	}))
	landingSetLastSendMs(t, f, 821, keep)
	f.repo.markPoison(f.ctx, 821, poisonToken, itNow, itNow+landingPoisonMs)
	stored := landingRecord(t, f, 821)
	assert.Equal(t, keep, stored.GetLastSendMs(), "毒行推迟不动 last_send_ms")
	assert.Zero(t, stored.GetLeaseUntilMs(), "毒行推迟放掉租约")
	assert.Equal(t, itNow+landingPoisonMs, stored.GetNextAttemptMs())
	assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN), stored.GetLastOutcome(), "毒行把最近结局记成 UNKNOWN(0)")
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, stored.GetStatus())

	// 毒行推迟(经领取):payload 解不开 → Claim 回 ErrPoisonRow;领取 CAS 折完旧租约之后,推迟不再改这一列。
	landingInsert(t, f, landingRow(t, 825, player, landingDebit, 5, func(r *tradepb.TradeAssetOpRecord) {
		r.Payload = []byte{0xff, 0xff}
		r.CreatedMs = itNow - 5000
	}))
	landingSetLastSendMs(t, f, 825, keep)
	_, claimed, err := f.repo.Claim(f.ctx, 825, itNow, itNow+landingLeaseMs, itNow+landingPoisonMs, 0xf6)
	require.ErrorIs(t, err, assetop.ErrPoisonRow)
	assert.False(t, claimed)
	stored = landingRecord(t, f, 825)
	assert.Equal(t, keep, stored.GetLastSendMs(), "领取时没有可折的旧租约,推迟也不动它")
	assert.Zero(t, stored.GetLeaseUntilMs())
	assert.Equal(t, itNow+landingPoisonMs, stored.GetNextAttemptMs(), "毒行按 poisonUntilMs 推迟")

	// 归还租约。
	landingInsert(t, f, landingRow(t, 822, player, landingDebit, 2, func(r *tradepb.TradeAssetOpRecord) {
		r.CreatedMs, r.Attempts, r.NextAttemptMs = itNow-5000, 4, itNow-10
	}))
	landingSetLastSendMs(t, f, 822, keep)
	op := landingClaim(t, f, 822, itNow, 0xf2)
	require.Equal(t, keep, op.LastSendMs)

	stolen := op
	stolen.LeaseToken = 0xf3
	assert.ErrorIs(t, f.repo.ReleaseLease(f.ctx, stolen, itNow+1), assetop.ErrLeaseLost, "令牌不是我的:没有可还的")
	stored = landingRecord(t, f, 822)
	assert.Equal(t, itNow+landingLeaseMs, stored.GetLeaseUntilMs(), "落空时一个字都不写")
	assert.Equal(t, itNow, stored.GetUpdatedMs())

	require.NoError(t, f.repo.ReleaseLease(f.ctx, op, itNow+5))
	stored = landingRecord(t, f, 822)
	assert.Zero(t, stored.GetLeaseUntilMs(), "租约已还")
	assert.Equal(t, itNow+5, stored.GetUpdatedMs())
	assert.Equal(t, keep, stored.GetLastSendMs(), "归还租约不动 last_send_ms")
	assert.Equal(t, uint32(4), stored.GetAttempts(), "归还不算一次投递")
	assert.Equal(t, itNow-10, stored.GetNextAttemptMs(), "归还不改下一次投递时刻")
	assert.Equal(t, uint64(0xf2), stored.GetLeaseToken())
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, stored.GetStatus())

	// 还过的租约不会在下一次领取时被折进来;不还的话,这一行要晚一个安全间隔才能再判。
	op = landingClaim(t, f, 822, itNow+6, 0xf4)
	assert.Equal(t, keep, op.LastSendMs)

	landingInsert(t, f, landingRow(t, 823, player, landingDebit, 3, func(r *tradepb.TradeAssetOpRecord) {
		r.Status, r.LeaseToken = tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, 0xf5
	}))
	assert.ErrorIs(t, f.repo.ReleaseLease(f.ctx, assetop.Op{OpID: 823, LeaseToken: 0xf5}, itNow+7), assetop.ErrLeaseLost,
		"已终结的行:没有可还的")
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, landingRecord(t, f, 823).GetStatus())
}

// ── 自动放弃 ─────────────────────────────────────────────────

// TestListAbandonCandidates_PagesByOpIDDebitOnly:只给 PENDING、TRADE_DEBIT、创建**严格早于**截止的行,按 op_id 升序翻页。
// CREDIT 行再老也不进候选(对侧账未落地,放弃交付会让买家钱货两空)。
func TestListAbandonCandidates_PagesByOpIDDebitOnly(t *testing.T) {
	f := openLandingFixture(t)
	const player uint64 = 9781
	cutoff := itNow - 180*landingDayMs
	add := func(opID, seq, createdMs uint64, stream assetpb.AssetOpStream, status tradepb.TradeAssetOpStatus) {
		landingInsert(t, f, landingRow(t, opID, player, stream, seq, func(r *tradepb.TradeAssetOpRecord) {
			r.CreatedMs, r.UpdatedMs, r.Status = createdMs, createdMs, status
		}))
	}
	pending := tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING
	add(1101, 1, cutoff-20*landingDayMs, landingDebit, pending)
	add(1102, 2, itNow-10*landingDayMs, landingDebit, pending)
	add(1103, 3, cutoff-20*landingDayMs, landingDebit, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED)
	add(1104, 4, cutoff-10*landingDayMs, landingDebit, pending)
	add(1105, 5, cutoff-1, landingDebit, pending)
	add(1106, 6, cutoff, landingDebit, pending)
	add(1107, 1, cutoff-30*landingDayMs, landingCredit, pending)

	ids, err := f.repo.ListAbandonCandidates(f.ctx, cutoff, 0, 200)
	require.NoError(t, err)
	assert.Equal(t, []uint64{1101, 1104, 1105}, ids, "太新的、已终结的、恰好等于截止的、CREDIT 流的都不是候选")

	ids, err = f.repo.ListAbandonCandidates(f.ctx, cutoff, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, []uint64{1101}, ids)
	ids, err = f.repo.ListAbandonCandidates(f.ctx, cutoff, 1101, 1)
	require.NoError(t, err)
	assert.Equal(t, []uint64{1104}, ids)
	ids, err = f.repo.ListAbandonCandidates(f.ctx, cutoff, 1104, 5)
	require.NoError(t, err)
	assert.Equal(t, []uint64{1105}, ids)
	ids, err = f.repo.ListAbandonCandidates(f.ctx, cutoff, 1105, 5)
	require.NoError(t, err)
	assert.Empty(t, ids, "翻到底(1107 是 CREDIT,op_id 更大也不出现)")
}

// TestAbandon_LeaseLostLeavesRowUntouched:放弃以 (op_id, PENDING, lease_token) 为条件。令牌对不上(租约在判定期间被接管,
// 别人可能已经发出请求)、行已终结、行不存在,一律回 ErrLeaseLost,一个字都不写。放弃成功的行:ABANDONED、不置 durable、
// 不改 last_outcome、同写 next_attempt_ms = 终结时刻、不碰 last_send_ms。CREDIT 流在碰库之前拒绝,且不是 ErrLeaseLost。
func TestAbandon_LeaseLostLeavesRowUntouched(t *testing.T) {
	f := openLandingFixture(t)
	const player uint64 = 9791
	notHere := uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE)
	landingInsert(t, f, landingRow(t, 1201, player, landingDebit, 1, func(r *tradepb.TradeAssetOpRecord) {
		r.LastOutcome = notHere
	}))

	op := landingClaim(t, f, 1201, itNow, 0x1a1)
	stolen := op
	stolen.LeaseToken = 0x1a2
	assert.ErrorIs(t, f.repo.Abandon(f.ctx, stolen, itNow+1), assetop.ErrLeaseLost)
	rec := landingRecord(t, f, 1201)
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, rec.GetStatus())
	assert.Equal(t, uint64(0x1a1), rec.GetLeaseToken())
	assert.Equal(t, itNow+landingLeaseMs, rec.GetLeaseUntilMs(), "落空时一个字都不写")
	assert.Equal(t, itNow, rec.GetUpdatedMs())

	// 原持有者的租约过期、被别的副本重新领走之后,它再来放弃同样落空。重领把旧租约折进 last_send_ms。
	retaken := landingClaim(t, f, 1201, itNow+landingLeaseMs+1, 0x1a3)
	require.Equal(t, itNow+landingLeaseMs, retaken.LastSendMs)
	assert.ErrorIs(t, f.repo.Abandon(f.ctx, op, itNow+landingLeaseMs+2), assetop.ErrLeaseLost)
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, landingRecord(t, f, 1201).GetStatus())

	// 现持有者放弃成功。
	abandonAt := itNow + landingLeaseMs + 3
	require.NoError(t, f.repo.Abandon(f.ctx, retaken, abandonAt))
	rec = landingRecord(t, f, 1201)
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED, rec.GetStatus())
	assert.False(t, rec.GetDurable(), "不是 scene 确认的结局,不置 durable")
	assert.Equal(t, notHere, rec.GetLastOutcome(), "保留最后一次真实答复作证据")
	assert.Equal(t, abandonAt, rec.GetNextAttemptMs(), "终态行的 next_attempt_ms = 终结时刻(清理按它判龄)")
	assert.Equal(t, abandonAt, rec.GetUpdatedMs())
	assert.Zero(t, rec.GetLeaseUntilMs())
	assert.Equal(t, itNow+landingLeaseMs, rec.GetLastSendMs(), "终结不碰 last_send_ms")
	assert.Empty(t, rec.GetResolvedBy(), "自动放弃不是人工终结,不写操作人")

	// 再放弃一次(行已终结)落空;行不存在(点锁读不到)同样落空。
	assert.ErrorIs(t, f.repo.Abandon(f.ctx, retaken, abandonAt+1), assetop.ErrLeaseLost, "已终结")
	assert.Equal(t, abandonAt, landingRecord(t, f, 1201).GetUpdatedMs(), "已终结的行一个字都不写")
	assert.ErrorIs(t, f.repo.Abandon(f.ctx, assetop.Op{OpID: 1299, LeaseToken: 0x1a4, Stream: landingDebit}, itNow),
		assetop.ErrLeaseLost, "行不存在")

	// CREDIT 流:持着租约也不放弃(对侧账未落地),且不是"租约被接管"。
	landingInsert(t, f, landingRow(t, 1202, player, landingCredit, 1, nil))
	creditOp := landingClaim(t, f, 1202, itNow, 0x1a5)
	err := f.repo.Abandon(f.ctx, creditOp, itNow+1)
	require.Error(t, err)
	assert.NotErrorIs(t, err, assetop.ErrLeaseLost)
	rec = landingRecord(t, f, 1202)
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, rec.GetStatus())
	assert.Equal(t, itNow+landingLeaseMs, rec.GetLeaseUntilMs(), "拒绝放弃时一个字都不写")
}

// TestAbandonOnce_EndToEndWithTradeStore:assetop.Loop.AbandonOnce 跑在真库的 AssetOpRepo 上(六条证据由 assetop 判,
// 这里验 Store 一侧的接线):玩家无位置、账本确定未见、距最后一次可能的发送已超过安全间隔的 DEBIT 行被放弃;
// 有位置的那一行判定不放弃、归还租约;同一个离线玩家的 CREDIT 行根本不进候选(一次都不被领取);整个过程不向 scene 发任何请求。
func TestAbandonOnce_EndToEndWithTradeStore(t *testing.T) {
	f := openLandingFixture(t)
	const (
		offline uint64 = 9801
		online  uint64 = 9802
	)
	at := func(r *tradepb.TradeAssetOpRecord) { r.CreatedMs, r.UpdatedMs = itNow, itNow }
	landingInsert(t, f,
		landingRow(t, 1601, offline, landingDebit, 1, at),
		landingRow(t, 1602, online, landingDebit, 1, at),
		landingRow(t, 1603, offline, landingCredit, 1, at),
	)

	nowMs := itNow + 181*landingDayMs
	applier := &landingApplier{}
	loop := landingLoop(t, f, applier, nowMs)
	loop.Locator = landingLocator{online: nil}
	loop.Ledger = landingLedger{}

	abandoned, err := loop.AbandonOnce(f.ctx, nowMs)
	require.NoError(t, err)
	assert.Equal(t, 1, abandoned)
	assert.Empty(t, applier.requests(), "放弃路径不向 scene 发任何请求")

	off := landingRecord(t, f, 1601)
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED, off.GetStatus())
	assert.Equal(t, nowMs, off.GetNextAttemptMs())
	assert.Zero(t, off.GetLeaseUntilMs())
	assert.Equal(t, itNow, off.GetLastSendMs(), "放弃不碰 last_send_ms(插行写 created_ms,领取时没有可折的旧租约)")

	on := landingRecord(t, f, 1602)
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, on.GetStatus(), "玩家有位置:交给正常流程")
	assert.Zero(t, on.GetLeaseUntilMs(), "判定不放弃之后归还了租约")
	assert.Equal(t, itNow, on.GetLastSendMs(), "领取与归还都不推进 last_send_ms")

	credit := landingRecord(t, f, 1603)
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, credit.GetStatus(), "CREDIT 流不放弃")
	assert.Zero(t, credit.GetLeaseToken(), "CREDIT 行不进候选,一次都不被领取")
	assert.Equal(t, itNow, credit.GetUpdatedMs())
}

// ── 落地唤醒 ─────────────────────────────────────────────────

// TestWakePlayer_EndToEndWithTradeStore:落地唤醒走与退避重投相同的 Claim + ProcessOne(设计 L-4),跑在真库的 AssetOpRepo 上:
// 停放中的行立刻投出并终结(Finalize 先主键点锁再 CAS),不等 next_attempt_ms;终态行与别的玩家的行不受影响。
func TestWakePlayer_EndToEndWithTradeStore(t *testing.T) {
	f := openLandingFixture(t)
	const (
		player uint64 = 9771
		other  uint64 = 9772
	)
	parked := func(r *tradepb.TradeAssetOpRecord) { r.NextAttemptMs, r.Attempts = itNow+1_800_000, 6 }
	landingInsert(t, f,
		landingRow(t, 1401, player, landingDebit, 1, parked),
		landingRow(t, 1402, player, landingDebit, 2, parked),
		landingRow(t, 1403, player, landingDebit, 3, func(r *tradepb.TradeAssetOpRecord) {
			r.Status = tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED
		}),
		landingRow(t, 1404, other, landingDebit, 1, parked),
	)

	applier := &landingApplier{}
	loop := landingLoop(t, f, applier, itNow)
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	processed, err := loop.WakePlayer(ctx, player)
	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	for _, opID := range []uint64{1401, 1402} {
		rec := landingRecord(t, f, opID)
		assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, rec.GetStatus(), "op %d", opID)
		assert.True(t, rec.GetDurable(), "op %d:scene 确认落盘的结局置 durable", opID)
		assert.Equal(t, itNow, rec.GetNextAttemptMs(), "op %d:终态行的 next_attempt_ms = 终结时刻", opID)
	}
	assert.Equal(t, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, landingRecord(t, f, 1404).GetStatus(), "别的玩家的行不受影响")
	reqs := applier.requests()
	require.Len(t, reqs, 2)
	for _, req := range reqs {
		assert.Equal(t, player, req.GetPlayerId())
	}
}

// ── 终态行清理 ───────────────────────────────────────────────

// TestCleanupTerminalOps_KeepsPendingAndPartialDeletesExpiredAbandoned:保留期外的 APPLIED / REJECTED / ABORTED / ABANDONED 删掉;
// PENDING(永不删)与 APPLIED_PARTIAL(部分发放要人工补偿,证据不能自动消失)无论多老都留;判龄按 next_attempt_ms
// (终态行上等于终结时刻)且严格早于截止才删;保留期内的终态行留。seq 表一行都不碰。
func TestCleanupTerminalOps_KeepsPendingAndPartialDeletesExpiredAbandoned(t *testing.T) {
	f := openLandingFixture(t)
	const player uint64 = 9811
	now := time.UnixMilli(int64(itNow))
	retention := 30 * 24 * time.Hour
	cutoff := itNow - uint64(retention.Milliseconds())
	add := func(opID, seq uint64, status tradepb.TradeAssetOpStatus, nextAttemptMs uint64) {
		landingInsert(t, f, landingRow(t, opID, player, landingDebit, seq, func(r *tradepb.TradeAssetOpRecord) {
			r.Status, r.NextAttemptMs, r.CreatedMs, r.UpdatedMs = status, nextAttemptMs, cutoff-100*landingDayMs, nextAttemptMs
		}))
	}
	add(1501, 1, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, cutoff-1)
	add(1502, 2, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED, cutoff-1)
	add(1503, 3, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_REJECTED, cutoff-10*landingDayMs)
	add(1504, 4, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABORTED, cutoff-10*landingDayMs)
	add(1505, 5, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL, cutoff-50*landingDayMs)
	add(1506, 6, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_PENDING, cutoff-50*landingDayMs)
	add(1507, 7, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_ABANDONED, cutoff)
	add(1508, 8, tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED, itNow-landingDayMs)
	deletedRec := captureCleanupDeleted(t)

	deleted, err := f.repo.CleanupTerminalOps(f.ctx, now, retention)
	require.NoError(t, err)
	assert.Equal(t, int64(4), deleted)
	assert.Equal(t, []int64{4}, deletedRec(), "一批删完,计数只报一次")
	assert.Equal(t, []uint64{1505, 1506, 1507, 1508}, landingOpIDs(t, f),
		"APPLIED_PARTIAL 与 PENDING 永不删;恰好等于截止的、保留期内的终态行留下")

	// 再跑一轮:没有可删的行,幂等。
	deleted, err = f.repo.CleanupTerminalOps(f.ctx, now, retention)
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.Equal(t, []uint64{1505, 1506, 1507, 1508}, landingOpIDs(t, f))
}
