package data

// asset_store_landing_test.go —— 落地恢复(docs/design/landing-recovery-sync-digest.md §5.3 / §6)在 guild_asset_op 上的落点:
//   - last_send_ms 契约(go/shared/assetop/abandon.go 文件头「LastSendMs 契约」):插行写 created_ms、领取时懒初始化并折叠
//     未收尾的旧租约、重排只在可能发过时推进、毒行推迟与归还租约不动它;
//   - 落地唤醒按玩家列未决行(assetop.PlayerPendingLister)、日对账按类计数(assetop.PendingClassCounter);
//   - 自动放弃(assetop.Abandoner):令牌落空回 ErrLeaseLost、对侧账与 REJECTED / ABORTED 一致、候选按 op_id 翻页;
//   - 与 assetop.Loop 的端到端接线:AbandonOnce / WakePlayer 跑在真库的 GuildAssetStore 上。
//
// 分层同 economy_repo_test.go:纯单测不连库、任何环境都跑;真库用例 GUILD_TEST_MYSQL_DSN 未设即 Skip
// (全体 Skip 不代表通过,验收时 DSN 必须真的设上)。真库用例名带 LastSendMs / ListPendingByPlayer / CountPendingByClass /
// ListAbandonCandidates / Abandon / ReleaseLease / WakePlayer,便于 `-run` 一次选中。
// 时间一律由 testNowMs 派生,不读真实墙钟(AGENTS §11.4);每个真库用例自己重建 schema,互不依赖执行顺序。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"

	"shared/assetop"
	"shared/assetop/assetoptest"

	"guild/internal/constants"
)

// ── 纯单测 ────────────────────────────────────────────────────

// TestMayHaveSentFlag:Reschedule 里"本次是否可能向 scene 发出过请求"的标志(LastSendMs 契约第 3 条)。
// 只有本地合成的 NOT_HERE(Result.Local)记 0;拿不到答复的(传输失败、超时)与 scene 的任何答复都按"发过"记 1 ——
// 漏推进会让放弃的安全间隔从一次真实发送之前算起。
func TestMayHaveSentFlag(t *testing.T) {
	notHere := assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE
	cases := []struct {
		name string
		res  assetop.Result
		want uint32
	}{
		{"等待落点 / 节点未知:本地合成,没发", assetop.Result{Outcome: notHere, Local: true}, 0},
		{"位置键不存在:本地合成,没发", assetop.Result{Outcome: notHere, Local: true, Absent: true}, 0},
		{"传输失败(Caller 回零值)", assetop.Result{}, 1},
		{"传输失败的落库形态 RETRY", assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY}, 1},
		{"scene 自己回 NOT_HERE", assetop.Result{Outcome: notHere, Reason: assetop.ReasonPlayerNotHere}, 1},
		{"未落盘的 APPLIED", assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED}, 1},
		{"scene 回 UNKNOWN", assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN}, 1},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, mayHaveSentFlag(tc.res), tc.name)
	}
}

// TestLastSendMsOnInsertIsAtLeastCreatedMs:插行即持租约,同步投递紧接着就可能发出(契约第 1 条),
// 所以插行值不得小于 created_ms;所有插行路径都经 assetOpInsertArgs,它的最后一列就是 last_send_ms。
func TestLastSendMsOnInsertIsAtLeastCreatedMs(t *testing.T) {
	rec := &pb.GuildAssetOpRecord{CreatedMs: testNowMs}
	assert.Equal(t, testNowMs, lastSendMsOnInsert(rec), "生产调用方不填:取 created_ms")
	rec.LastSendMs = testNowMs - 1
	assert.Equal(t, testNowMs, lastSendMsOnInsert(rec), "比 created_ms 小的值抬到 created_ms")
	rec.LastSendMs = testNowMs + 5
	assert.Equal(t, testNowMs+5, lastSendMsOnInsert(rec), "夹具显式填的更大值原样保留")

	args := assetOpInsertArgs(&pb.GuildAssetOpRecord{CreatedMs: testNowMs})
	require.Len(t, args, assetOpColumnCount)
	assert.True(t, strings.HasSuffix(assetOpColumns, "`last_send_ms`"), "last_send_ms 是第 29 个字段,列清单最后一列")
	assert.Equal(t, testNowMs, args[assetOpColumnCount-1], "插入参数的最后一列取 lastSendMsOnInsert")
}

// TestLastSendMsSQLShape 钉住 LastSendMs 契约与放弃 CAS 在 SQL 文本上的形状(真库用例验语义,这里让"改坏了"在无库环境也红)。
func TestLastSendMsSQLShape(t *testing.T) {
	// 第 2、5 条:领取的 CAS 里 last_send_ms 一项排在改 lease_until_ms / updated_ms 之前 —— MySQL 从左到右求值,
	// 排在最前才能保证读到的是旧租约与旧 updated_ms;懒初始化取 GREATEST(created_ms, updated_ms),再与旧租约取较大值。
	set := sqlClaimAssetOp[strings.Index(sqlClaimAssetOp, " SET "):strings.Index(sqlClaimAssetOp, " WHERE ")]
	lastSend := strings.Index(set, "`last_send_ms` =")
	require.GreaterOrEqual(t, lastSend, 0, sqlClaimAssetOp)
	assert.Less(t, lastSend, strings.Index(set, "`lease_until_ms` = ?"), "last_send_ms 必须先于 lease_until_ms 赋值:%s", sqlClaimAssetOp)
	assert.Less(t, lastSend, strings.Index(set, "`updated_ms` = ?"), "last_send_ms 必须先于 updated_ms 赋值:%s", sqlClaimAssetOp)
	assert.Contains(t, set,
		"GREATEST(IF(`last_send_ms` = 0, GREATEST(`created_ms`, `updated_ms`), `last_send_ms`), `lease_until_ms`)")
	assert.Equal(t, 6, strings.Count(sqlClaimAssetOp, "?"), "last_send_ms 一项不带参数,领取的参数个数不变")

	// 第 3 条:重排只在可能发过时推进,参数 (mayHaveSentFlag, nowMs)。
	assert.Contains(t, sqlRescheduleAssetOp, "`last_send_ms` = IF(?, GREATEST(`last_send_ms`, ?), `last_send_ms`)")
	assert.Equal(t, 10, strings.Count(sqlRescheduleAssetOp, "?"))

	// 第 4 条:毒行推迟与归还租约只清租约;终结类写法(终结 / 人工终结 / 放弃)也不碰它 —— 行已终结,它只是审计证据。
	untouched := []struct{ name, query string }{
		{"sqlPoisonAssetOp", sqlPoisonAssetOp},
		{"sqlReleaseLeaseAssetOp", sqlReleaseLeaseAssetOp},
		{"sqlAbandonAssetOp", sqlAbandonAssetOp},
		{"sqlFinalizeAssetOp", sqlFinalizeAssetOp},
		{"sqlResolveAssetOp", sqlResolveAssetOp},
	}
	for _, u := range untouched {
		assert.NotContains(t, u.query, "last_send_ms", u.name)
	}

	// 放弃的 CAS:以 (op_id, PENDING, lease_token) 为条件;不置 durable、不改 last_outcome / last_reason、不写 reason_tip_id;
	// 同写 next_attempt_ms = 终结时刻(07 §7.4.1:清理判龄与回档检查按它)。
	assert.True(t, strings.HasSuffix(sqlAbandonAssetOp, " WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?"), sqlAbandonAssetOp)
	for _, col := range []string{"durable", "last_outcome", "last_reason", "reason_tip_id"} {
		assert.NotContains(t, sqlAbandonAssetOp, "`"+col+"`", "放弃不写 %s", col)
	}
	assert.Contains(t, sqlAbandonAssetOp, "`next_attempt_ms` = ?")
	// 归还租约只改两列无索引列(lease_until_ms、updated_ms):自动提交在 TiDB 下只有行 key 一个变更,见 asset_store.go 文件头。
	assert.Equal(t, "UPDATE "+guildAssetOpTable+" SET `lease_until_ms` = 0, `updated_ms` = ?"+
		" WHERE `op_id` = ? AND `status` = ? AND `lease_token` = ?", sqlReleaseLeaseAssetOp)
}

// TestRefundsCounterpartyCoversAbandoned:"退回预留"只此一处判定,ABANDONED 与 REJECTED / ABORTED 同列(设计 §6.3)。
func TestRefundsCounterpartyCoversAbandoned(t *testing.T) {
	for _, st := range []assetop.Status{assetop.StatusRejected, assetop.StatusAborted, assetop.StatusAbandoned} {
		assert.True(t, refundsCounterparty(st), "%s 要退回预留", st)
	}
	for _, st := range []assetop.Status{assetop.StatusPending, assetop.StatusApplied, assetop.StatusAppliedPartial, assetop.Status(0)} {
		assert.False(t, refundsCounterparty(st), "%s 不退", st)
	}
}

// TestPendingByPlayerLimitIsTwoStreamsOfMaxPending:落地唤醒一次最多列 32 行 = 本服务独占的两条流 × 每流未决上限 16(I5 / I6),
// 与 assetop 一次唤醒的上限同值。
func TestPendingByPlayerLimitIsTwoStreamsOfMaxPending(t *testing.T) {
	assert.Equal(t, 2*int(assetop.DefaultLimits.MaxPending), pendingByPlayerLimit())
	assert.Equal(t, 32, pendingByPlayerLimit())
}

// TestLandingStoreRefusesBadInputBeforeTouchingStorage:没有连接池的 store 上,越过入参校验去碰库就会当场 panic,
// 从而证明这些输入在碰库之前就被拒绝 / 短路。
//   - 令牌 0 在本表表示"没有租约":拿它做放弃的条件会匹配一条从未被领取过的行,等于不持租约就放弃(fail-closed);
//     这是调用方的 bug,不是租约被接管,所以不回 ErrLeaseLost。
//   - player_id 0、limit <= 0、截止时刻 0 都是"没有候选",直接回空。
func TestLandingStoreRefusesBadInputBeforeTouchingStorage(t *testing.T) {
	store, err := NewGuildAssetStore(econNoDBRepo(t))
	require.NoError(t, err)
	ctx := context.Background()

	err = store.Abandon(ctx, assetop.Op{OpID: 1}, testNowMs)
	assert.Error(t, err)
	assert.NotErrorIs(t, err, assetop.ErrLeaseLost)
	err = store.ReleaseLease(ctx, assetop.Op{OpID: 1}, testNowMs)
	assert.Error(t, err)
	assert.NotErrorIs(t, err, assetop.ErrLeaseLost)

	ids, err := store.ListPendingByPlayer(ctx, 0, 32)
	assert.NoError(t, err)
	assert.Empty(t, ids)
	ids, err = store.ListPendingByPlayer(ctx, 7, 0)
	assert.NoError(t, err)
	assert.Empty(t, ids)
	ids, err = store.ListAbandonCandidates(ctx, 0, 0, 200)
	assert.NoError(t, err)
	assert.Empty(t, ids)
	ids, err = store.ListAbandonCandidates(ctx, testNowMs, 0, 0)
	assert.NoError(t, err)
	assert.Empty(t, ids)
}

// ── 真库夹具 ──────────────────────────────────────────────────

// landingShopRow 造一行商店未决指令(econShopRecord 的骨架:帮会 7700、无周期键),再由 mutate 改状态 / 次数 / 时刻。
func landingShopRow(t *testing.T, opID, playerID, seq uint64, mutate func(*pb.GuildAssetOpRecord)) *pb.GuildAssetOpRecord {
	t.Helper()
	rec := econShopRecord(t, opID, playerID, testNowMs-100_000, seq)
	if mutate != nil {
		mutate(rec)
	}
	return rec
}

// landingSetLastSendMs 直接改 last_send_ms:模拟 schemamigrate ADD COLUMN 之后的存量行(0)或某个已维护的值。
// 插行路径会把它抬到不小于 created_ms(lastSendMsOnInsert),所以只能事后改。
func landingSetLastSendMs(t *testing.T, f econFixture, opID, v uint64) {
	t.Helper()
	mustExec(t, f.ctx, f.db, "UPDATE guild_asset_op SET last_send_ms=? WHERE op_id=?", v, opID)
}

// landingClaim 以 nowMs 领一行(租约 econLeaseMs),要求领到。
func landingClaim(t *testing.T, f econFixture, opID, nowMs, token uint64) assetop.Op {
	t.Helper()
	op, claimed, err := f.store.Claim(f.ctx, opID, nowMs, nowMs+econLeaseMs, nowMs+3_600_000, token)
	require.NoError(t, err, "claim op %d", opID)
	require.True(t, claimed, "claim op %d", opID)
	return op
}

// landingCaptureFinalized 把 OnFinalized 换成记录器(回调在 terminate 里同步调用,不需要加锁)。
func landingCaptureFinalized(f econFixture) func() []FinalizedOp {
	var got []FinalizedOp
	f.store.OnFinalized = func(_ context.Context, op FinalizedOp) { got = append(got, op) }
	return func() []FinalizedOp { return got }
}

func landingNotHereLocal() assetop.Result {
	return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE, Local: true, Absent: true}
}

// landingClassKey 是 (流, 类别):把 CountPendingByClass 的结果转成 map,断言不依赖返回顺序。
type landingClassKey struct {
	stream assetpb.AssetOpStream
	class  assetop.PendingClass
}

func landingCountsByKey(counts []assetop.ClassCount) map[landingClassKey]assetop.ClassCount {
	out := make(map[landingClassKey]assetop.ClassCount, len(counts))
	for _, c := range counts {
		out[landingClassKey{stream: c.Stream, class: c.Class}] = c
	}
	return out
}

// landingLedger 是 assetop.LedgerReader 的替身:每个玩家都有一份空账本,写权行已释放。对纪元非 0 的 seq 它给出"确定未见"
// (assetop.ClassifySeq:请求纪元比账本新 → 按空账本看,seq 在窗内即未见)。
type landingLedger struct{}

func (landingLedger) ReadPersistedLedger(context.Context, uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return &componentpb.PlayerAssetOpLedgerComp{}, nil
}

// ReadStoreLedger 让自动放弃读到「没有持有者、账本为空」。
func (landingLedger) ReadStoreLedger(context.Context, uint64) (assetop.StoreLedger, error) {
	return assetop.StoreLedger{Found: true, Released: true, Ledger: &componentpb.PlayerAssetOpLedgerComp{}}, nil
}

// landingApplier 记下每一次投递并一律回"已落盘的全额应用",结局同时记进自己的已提交账本(Loop 终结前核对它)。
// WakePlayer 逐行串行,锁只为让读取方不与写入竞争。
type landingApplier struct {
	assetoptest.CommittedLedger

	mu    sync.Mutex
	calls []*assetpb.AssetOpRequest
}

func (a *landingApplier) Do(_ context.Context, _ assetop.RPC, req *assetpb.AssetOpRequest) (assetop.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, req)
	res := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}
	if err := a.Commit(req, res); err != nil {
		return assetop.Result{}, err
	}
	return res, nil
}

func (a *landingApplier) requests() []*assetpb.AssetOpRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*assetpb.AssetOpRequest(nil), a.calls...)
}

// ── last_send_ms 契约 ─────────────────────────────────────────

// TestReserve_WritesLastSendMsAsCreatedMs:契约第 1 条 —— 捐献 / 兑换预留与直接插行都把 last_send_ms 写成 created_ms。
func TestReserve_WritesLastSendMsAsCreatedMs(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7711
		leader  uint64 = 8711
		player  uint64 = 8712
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	econSetContribution(t, f, guildID, player, 1000, 1000)

	donate := econDonation(t, 9_711_001, player, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, donate)
	require.NoError(t, err)
	shop := econShopOrder(t, 9_711_002, player, guildID, 1, 100, 5, testNowMs+7)
	_, err = f.econ.ReserveShopOrder(f.ctx, shop)
	require.NoError(t, err)

	assert.Equal(t, testNowMs, econRecord(t, f, donate.OpID).GetLastSendMs(), "捐献预留:插行即持租约,last_send_ms = created_ms")
	assert.Equal(t, testNowMs+7, econRecord(t, f, shop.OpID).GetLastSendMs(), "兑换预留:同上")

	direct := landingShopRow(t, 9_711_003, player, 9, func(r *pb.GuildAssetOpRecord) { r.CreatedMs = testNowMs - 1000 })
	econInsertOp(t, f, direct)
	assert.Equal(t, testNowMs-1000, econRecord(t, f, direct.GetOpId()).GetLastSendMs(), "其余插行路径同走 assetOpInsertArgs")
}

// TestClaim_LastSendMsLazyInitAndFoldsUnfinishedLease:契约第 2、5 条 —— 领取的 CAS 在改写租约之前把上一个租约折进来;
// 存量行(0)懒初始化为 GREATEST(created_ms, updated_ms);已维护的值不被 updated_ms 拉动(活性)。回读填 CreatedMs / LastSendMs。
func TestClaim_LastSendMsLazyInitAndFoldsUnfinishedLease(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		player    uint64 = 8721
		createdMs        = testNowMs - 5000
	)
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
		{801, testNowMs - 3000, 0, 0, testNowMs - 3000, "存量行:懒初始化取 GREATEST(created_ms, updated_ms)"},
		{802, testNowMs - 3000, testNowMs - 100, 0, testNowMs - 100, "存量行 + 未收尾租约:初值再与旧租约取较大值"},
		{803, testNowMs - 10, 0, testNowMs - 4000, testNowMs - 4000, "已维护:updated_ms 再新也不拉动它(本地重排不算发送)"},
		{804, testNowMs - 10, testNowMs - 50, testNowMs - 4000, testNowMs - 50, "已维护 + 未收尾租约:折进旧租约"},
		{805, testNowMs - 10, testNowMs - 4500, testNowMs - 4000, testNowMs - 4000, "旧租约早于已记的发送:取较大值"},
	}
	for i, r := range rows {
		econInsertOp(t, f, landingShopRow(t, r.opID, player, uint64(i+1), func(rec *pb.GuildAssetOpRecord) {
			rec.CreatedMs, rec.UpdatedMs, rec.LeaseUntilMs = createdMs, r.updatedMs, r.leaseUntilMs
		}))
		landingSetLastSendMs(t, f, r.opID, r.lastSendMs)
	}
	for _, r := range rows {
		op := landingClaim(t, f, r.opID, testNowMs, 0xc00+r.opID)
		assert.Equal(t, r.want, op.LastSendMs, "op %d:%s", r.opID, r.why)
		assert.Equal(t, createdMs, op.CreatedMs, "op %d:回读填 CreatedMs", r.opID)
		stored := econRecord(t, f, r.opID)
		assert.Equal(t, r.want, stored.GetLastSendMs(), "op %d:落库值与回读一致", r.opID)
		assert.Equal(t, testNowMs+econLeaseMs, stored.GetLeaseUntilMs(), "op %d:租约照常写新值", r.opID)
	}

	// 持有者领到之后崩溃、没来得及重排:租约过期后别的副本再领,last_send_ms 必须盖住那次可能的发送(不晚于旧租约到期)。
	op := landingClaim(t, f, 803, testNowMs+econLeaseMs+1, 0xd03)
	assert.Equal(t, testNowMs+econLeaseMs, op.LastSendMs, "未收尾的租约被折进 last_send_ms")
}

// TestReschedule_LastSendMsAdvancesOnlyWhenMaybeSent:契约第 3 条 —— 本地 NOT_HERE 不推进(否则停放中的离线行永远"刚发过",
// 自动放弃永远不会发生),其余结果(含传输失败)一律按发过推进到本次 ProcessOne 的开始时刻,且取较大值不倒退;
// 令牌落空时一个字都不写。
func TestReschedule_LastSendMsAdvancesOnlyWhenMaybeSent(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8731
	econInsertOp(t, f, landingShopRow(t, 811, player, 1, func(r *pb.GuildAssetOpRecord) {
		r.CreatedMs, r.UpdatedMs = testNowMs-5000, testNowMs-5000
	}))
	retry := assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY}

	op := landingClaim(t, f, 811, testNowMs, 0xe1)
	require.Equal(t, testNowMs-5000, op.LastSendMs)
	require.NoError(t, f.store.Reschedule(f.ctx, op, testNowMs+1_800_000, landingNotHereLocal(), testNowMs+5))
	stored := econRecord(t, f, 811)
	assert.Equal(t, testNowMs-5000, stored.GetLastSendMs(), "本地 NOT_HERE 没发请求,不推进")
	assert.Equal(t, testNowMs+5, stored.GetUpdatedMs(), "updated_ms 照写 —— 这正是放弃不能看 updated_ms 的原因")
	assert.Equal(t, uint32(1), stored.GetAttempts())
	assert.Zero(t, stored.GetLeaseUntilMs())

	op = landingClaim(t, f, 811, testNowMs+10, 0xe2)
	assert.Equal(t, testNowMs-5000, op.LastSendMs, "重排已清租约,下一次领取没有可折的旧租约")
	require.NoError(t, f.store.Reschedule(f.ctx, op, testNowMs+1000, retry, testNowMs+20))
	assert.Equal(t, testNowMs+20, econRecord(t, f, 811).GetLastSendMs(), "scene 回了 RETRY:发过,推进到 now")

	op = landingClaim(t, f, 811, testNowMs+30, 0xe3)
	require.NoError(t, f.store.Reschedule(f.ctx, op, testNowMs+1000, assetop.Result{}, testNowMs+15))
	assert.Equal(t, testNowMs+20, econRecord(t, f, 811).GetLastSendMs(), "now 比已记的值旧(副本时钟偏差):取较大值,不倒退")

	op = landingClaim(t, f, 811, testNowMs+40, 0xe4)
	require.NoError(t, f.store.Reschedule(f.ctx, op, testNowMs+1000, assetop.Result{}, testNowMs+45))
	assert.Equal(t, testNowMs+45, econRecord(t, f, 811).GetLastSendMs(), "传输失败(零值结果,非 Local)按发过推进")

	op = landingClaim(t, f, 811, testNowMs+50, 0xe5)
	stolen := op
	stolen.LeaseToken = 0xe6
	assert.ErrorIs(t, f.store.Reschedule(f.ctx, stolen, testNowMs+1000, retry, testNowMs+60), assetop.ErrLeaseLost)
	assert.Equal(t, testNowMs+45, econRecord(t, f, 811).GetLastSendMs(), "租约不是我的:last_send_ms 也不能写")
}

// TestReleaseLease_AndPoisonKeepLastSendMs:契约第 4 条 —— 毒行推迟与放弃判定后的归还租约只清租约,不动 last_send_ms;
// 归还也不动 attempts / next_attempt_ms / 令牌;令牌对不上或行已终结回 ErrLeaseLost 且一个字不写。
func TestReleaseLease_AndPoisonKeepLastSendMs(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8741
	keep := testNowMs - 4000

	// 毒行推迟:解码失败发生在发送之前。直接对"持租约、已维护 last_send_ms"的行调 markPoison,排除领取那一步对这一列的影响。
	const poisonToken uint64 = 0xf1
	econInsertOp(t, f, landingShopRow(t, 821, player, 1, func(r *pb.GuildAssetOpRecord) {
		r.Payload = []byte{0xff, 0xff}
		r.CreatedMs, r.LeaseUntilMs, r.LeaseToken = testNowMs-5000, testNowMs+econLeaseMs, poisonToken
	}))
	landingSetLastSendMs(t, f, 821, keep)
	f.store.markPoison(f.ctx, 821, poisonToken, testNowMs, testNowMs+3_600_000)
	stored := econRecord(t, f, 821)
	assert.Equal(t, keep, stored.GetLastSendMs(), "毒行推迟不动 last_send_ms")
	assert.Zero(t, stored.GetLeaseUntilMs(), "毒行推迟放掉租约")
	assert.Equal(t, testNowMs+3_600_000, stored.GetNextAttemptMs())

	econInsertOp(t, f, landingShopRow(t, 822, player, 2, func(r *pb.GuildAssetOpRecord) {
		r.CreatedMs, r.Attempts, r.NextAttemptMs = testNowMs-5000, 4, testNowMs-10
	}))
	landingSetLastSendMs(t, f, 822, keep)
	op := landingClaim(t, f, 822, testNowMs, 0xf2)
	require.Equal(t, keep, op.LastSendMs)

	stolen := op
	stolen.LeaseToken = 0xf3
	assert.ErrorIs(t, f.store.ReleaseLease(f.ctx, stolen, testNowMs+1), assetop.ErrLeaseLost, "令牌不是我的:没有可还的")
	assert.Equal(t, testNowMs+econLeaseMs, econRecord(t, f, 822).GetLeaseUntilMs(), "落空时一个字都不写")

	require.NoError(t, f.store.ReleaseLease(f.ctx, op, testNowMs+5))
	stored = econRecord(t, f, 822)
	assert.Zero(t, stored.GetLeaseUntilMs(), "租约已还")
	assert.Equal(t, testNowMs+5, stored.GetUpdatedMs())
	assert.Equal(t, keep, stored.GetLastSendMs(), "归还租约不动 last_send_ms")
	assert.Equal(t, uint32(4), stored.GetAttempts(), "归还不算一次投递")
	assert.Equal(t, testNowMs-10, stored.GetNextAttemptMs(), "归还不改下一次投递时刻")
	assert.Equal(t, uint64(0xf2), stored.GetLeaseToken())
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, stored.GetStatus())

	// 还过的租约不会在下一次领取时被折进来;不还的话,这一行要晚一个安全间隔才能再判。
	op = landingClaim(t, f, 822, testNowMs+6, 0xf4)
	assert.Equal(t, keep, op.LastSendMs)

	econInsertOp(t, f, landingShopRow(t, 823, player, 3, func(r *pb.GuildAssetOpRecord) {
		r.Status, r.LeaseToken = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, 0xf5
	}))
	assert.ErrorIs(t, f.store.ReleaseLease(f.ctx, assetop.Op{OpID: 823, LeaseToken: 0xf5}, testNowMs+7), assetop.ErrLeaseLost,
		"已终结的行:没有可还的")
}

// ── 落地唤醒 / 日对账 ─────────────────────────────────────────

// TestListPendingByPlayer_OnlyPendingRowsOfPlayer:只列该玩家的 PENDING 行,按 op_id 升序;不看租约与 next_attempt_ms
// (唤醒本来就是要提前,租约由随后的 Claim CAS 判);截断时先给老行;至多 32 行。
func TestListPendingByPlayer_OnlyPendingRowsOfPlayer(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		player  uint64 = 8751
		other   uint64 = 8752
		nobody  uint64 = 8753
		crowded uint64 = 8754
	)
	payload := econDonatePayload(t, 10000)
	leased := econDonateRecord(901, player, 7700, testNowMs-100_000, 1, payload)
	leased.LeaseUntilMs = testNowMs + 5000
	econInsertOp(t, f, leased)
	econInsertOp(t, f, landingShopRow(t, 903, player, 1, nil))
	econInsertOp(t, f, landingShopRow(t, 902, player, 2, func(r *pb.GuildAssetOpRecord) {
		r.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
	}))
	econInsertOp(t, f, landingShopRow(t, 904, player, 3, func(r *pb.GuildAssetOpRecord) {
		r.NextAttemptMs = testNowMs + 1_800_000
	}))
	econInsertOp(t, f, landingShopRow(t, 905, other, 1, nil))

	ids, err := f.store.ListPendingByPlayer(f.ctx, player, 32)
	require.NoError(t, err)
	assert.Equal(t, []uint64{901, 903, 904}, ids, "租约在别人手里的、停放中的都照列;终态行与别人的行不列")

	ids, err = f.store.ListPendingByPlayer(f.ctx, player, 2)
	require.NoError(t, err)
	assert.Equal(t, []uint64{901, 903}, ids, "截断时先给老行")

	ids, err = f.store.ListPendingByPlayer(f.ctx, nobody, 32)
	require.NoError(t, err)
	assert.Empty(t, ids, "没有未决行不是错误")

	// 上限 32 = 两条流 × MaxPending(16):守卫失效时多出来的行照常等自己的 next_attempt_ms。
	recs := make([]*pb.GuildAssetOpRecord, 0, 40)
	for i := uint64(1); i <= 40; i++ {
		recs = append(recs, econDonateRecord(950+i, crowded, 7700, testNowMs-100_000, i, payload))
	}
	econInsertOpsBulk(t, f, recs)
	ids, err = f.store.ListPendingByPlayer(f.ctx, crowded, 100)
	require.NoError(t, err)
	require.Len(t, ids, 32, "调用方给的 limit 再大也只列 32 行")
	assert.Equal(t, uint64(951), ids[0])
	assert.Equal(t, uint64(982), ids[31])
}

// TestCountPendingByClass_UsesAssetopClassification:分类口径只在 assetop.ClassifyPending 一处(Store 只喂列),
// 这里逐类各造一行,连同"告警优先于放弃候选""scene 自己回的 NOT_HERE 不算离线"两条优先级一起核对;终态行不计。
func TestCountPendingByClass_UsesAssetopClassification(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8761
	day := econDayMs
	notHere := uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE)
	unknown := uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN)
	shopRow := func(opID, seq, ageMs uint64, attempts, outcome, reason uint32) {
		econInsertOp(t, f, landingShopRow(t, opID, player, seq, func(r *pb.GuildAssetOpRecord) {
			r.CreatedMs, r.Attempts, r.LastOutcome, r.LastReason = testNowMs-ageMs, attempts, outcome, reason
		}))
	}
	// 新行,还没投过(last_outcome 也是 0):online_retry。
	shopRow(1001, 1, 1000, 0, unknown, 0)
	// 投过且 scene 回 UNKNOWN:alert。
	shopRow(1002, 2, 2000, 3, unknown, 0)
	// 本地合成的 NOT_HERE(不带原因码):offline。
	shopRow(1003, 3, 3000, 6, notHere, 0)
	// scene 自己回的 NOT_HERE(一律带 kAssetPlayerNotHere):online_retry。
	shopRow(1004, 4, 4000, 2, notHere, assetop.ReasonPlayerNotHere)
	// 早于放弃截止:abandon_candidate。
	shopRow(1005, 5, 181*day, 9, notHere, 0)
	// 又老又 UNKNOWN:alert 优先(告警类不能因为又老又离线就不响)。
	shopRow(1006, 6, 200*day, 4, unknown, 0)
	// 终态行不计。
	econInsertOp(t, f, landingShopRow(t, 1007, player, 7, func(r *pb.GuildAssetOpRecord) {
		r.Status, r.CreatedMs = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, testNowMs-300*day
	}))
	// 另一条流:捐献,投过一次、scene 回 RETRY:online_retry。
	donate := econDonateRecord(1008, player, 7700, testNowMs-100_000, 1, econDonatePayload(t, 10000))
	donate.CreatedMs, donate.Attempts, donate.LastOutcome = testNowMs-500, 1, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY)
	econInsertOp(t, f, donate)

	credit := assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT
	debit := assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT

	counts, err := f.store.CountPendingByClass(f.ctx, testNowMs, testNowMs-180*day)
	require.NoError(t, err)
	want := []assetop.ClassCount{
		{Stream: credit, Class: assetop.PendingOnlineRetry, Rows: 2, OldestAgeMs: 4000},
		{Stream: credit, Class: assetop.PendingAlert, Rows: 2, OldestAgeMs: 200 * day},
		{Stream: credit, Class: assetop.PendingOffline, Rows: 1, OldestAgeMs: 3000},
		{Stream: credit, Class: assetop.PendingAbandonCandidate, Rows: 1, OldestAgeMs: 181 * day},
		{Stream: debit, Class: assetop.PendingOnlineRetry, Rows: 1, OldestAgeMs: 500},
	}
	assert.Equal(t, landingCountsByKey(want), landingCountsByKey(counts))

	// abandonBeforeMs = 0(自动放弃关闭):不分出放弃候选,那一行按最近结局归 offline。
	counts, err = f.store.CountPendingByClass(f.ctx, testNowMs, 0)
	require.NoError(t, err)
	byKey := landingCountsByKey(counts)
	assert.NotContains(t, byKey, landingClassKey{stream: credit, class: assetop.PendingAbandonCandidate})
	assert.Equal(t, assetop.ClassCount{Stream: credit, Class: assetop.PendingOffline, Rows: 2, OldestAgeMs: 181 * day},
		byKey[landingClassKey{stream: credit, class: assetop.PendingOffline}])
}

// TestCountPendingByClass_PagesPastPageSize:按主键翻页读完全部未决行(多于两页,终态行夹在中间),不漏不重。
func TestCountPendingByClass_PagesPastPageSize(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		player uint64 = 8762
		total         = 2*pendingAuditPageSize + 210
	)
	payload := econDonatePayload(t, 10000)
	recs := make([]*pb.GuildAssetOpRecord, 0, total)
	var pending, oldestPendingAge uint64
	for i := uint64(1); i <= total; i++ {
		rec := econDonateRecord(20_000+i, player, 7700, testNowMs-100_000, i, payload)
		rec.CreatedMs = testNowMs - i
		if i%10 == 0 {
			rec.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
		} else {
			pending++
			oldestPendingAge = i
		}
		recs = append(recs, rec)
	}
	econInsertOpsBulk(t, f, recs)

	counts, err := f.store.CountPendingByClass(f.ctx, testNowMs, 0)
	require.NoError(t, err)
	require.Len(t, counts, 1)
	assert.Equal(t, assetop.ClassCount{
		Stream: assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, Class: assetop.PendingOnlineRetry,
		Rows: pending, OldestAgeMs: oldestPendingAge,
	}, counts[0])
}

// TestWakePlayer_EndToEndWithGuildStore:落地唤醒走与退避重投相同的 Claim + ProcessOne(设计 L-4),跑在真库的 Store 上:
// 停放中的行立刻投出并终结,不等 next_attempt_ms;别的玩家的行不受影响。
func TestWakePlayer_EndToEndWithGuildStore(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		player uint64 = 8771
		other  uint64 = 8772
	)
	parked := func(r *pb.GuildAssetOpRecord) { r.NextAttemptMs, r.Attempts = testNowMs+1_800_000, 6 }
	econInsertOp(t, f, landingShopRow(t, 1401, player, 1, parked))
	econInsertOp(t, f, landingShopRow(t, 1402, player, 2, parked))
	econInsertOp(t, f, landingShopRow(t, 1403, player, 3, func(r *pb.GuildAssetOpRecord) {
		r.Status = pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED
	}))
	econInsertOp(t, f, landingShopRow(t, 1404, other, 1, parked))

	applier := &landingApplier{}
	loop, err := assetop.NewLoop(assetop.DefaultLoopConfig(), f.store, applier, nil,
		func() time.Time { return time.UnixMilli(int64(testNowMs)) })
	require.NoError(t, err)
	loop.Ledger = applier

	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	processed, err := loop.WakePlayer(ctx, player)
	require.NoError(t, err)
	assert.Equal(t, 2, processed)
	for _, opID := range []uint64{1401, 1402} {
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, econRecord(t, f, opID).GetStatus(), "op %d", opID)
	}
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, econRecord(t, f, 1404).GetStatus(), "别的玩家的行不受影响")
	reqs := applier.requests()
	require.Len(t, reqs, 2)
	for _, req := range reqs {
		assert.Equal(t, player, req.GetPlayerId())
	}
}

// ── 自动放弃 ─────────────────────────────────────────────────

// TestListAbandonCandidates_PagesByOpID:只给 PENDING 且创建**严格早于**截止的行,按 op_id 升序翻页。
func TestListAbandonCandidates_PagesByOpID(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8781
	cutoff := testNowMs - 180*econDayMs
	add := func(opID, seq, createdMs uint64, status pb.GuildAssetOpStatus) {
		econInsertOp(t, f, landingShopRow(t, opID, player, seq, func(r *pb.GuildAssetOpRecord) {
			r.CreatedMs, r.Status = createdMs, status
		}))
	}
	pending := pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING
	add(1101, 1, cutoff-20*econDayMs, pending)
	add(1102, 2, testNowMs-10*econDayMs, pending)
	add(1103, 3, cutoff-20*econDayMs, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED)
	add(1104, 4, cutoff-10*econDayMs, pending)
	add(1105, 5, cutoff-1, pending)
	add(1106, 6, cutoff, pending)

	ids, err := f.store.ListAbandonCandidates(f.ctx, cutoff, 0, 200)
	require.NoError(t, err)
	assert.Equal(t, []uint64{1101, 1104, 1105}, ids, "太新的、已终结的、恰好等于截止的都不是候选")

	ids, err = f.store.ListAbandonCandidates(f.ctx, cutoff, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, []uint64{1101}, ids)
	ids, err = f.store.ListAbandonCandidates(f.ctx, cutoff, 1101, 1)
	require.NoError(t, err)
	assert.Equal(t, []uint64{1104}, ids)
	ids, err = f.store.ListAbandonCandidates(f.ctx, cutoff, 1104, 5)
	require.NoError(t, err)
	assert.Equal(t, []uint64{1105}, ids)
	ids, err = f.store.ListAbandonCandidates(f.ctx, cutoff, 1105, 5)
	require.NoError(t, err)
	assert.Empty(t, ids, "翻到底")
}

// TestAbandon_LeaseLostLeavesRowUntouched:放弃以 (op_id, PENDING, lease_token) 为条件。令牌对不上(租约在判定期间被接管,
// 别人可能已经发出请求)、行已终结、行不存在,一律回 ErrLeaseLost,不终结、不做对侧账、不回调。
func TestAbandon_LeaseLostLeavesRowUntouched(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8791
	finalized := landingCaptureFinalized(f)
	orphans := econCaptureOrphans(t)
	econInsertOp(t, f, landingShopRow(t, 1201, player, 1, nil))

	op := landingClaim(t, f, 1201, testNowMs, 0x1a1)
	stolen := op
	stolen.LeaseToken = 0x1a2
	assert.ErrorIs(t, f.store.Abandon(f.ctx, stolen, testNowMs+1), assetop.ErrLeaseLost)
	rec := econRecord(t, f, 1201)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, rec.GetStatus())
	assert.Equal(t, uint64(0x1a1), rec.GetLeaseToken())
	assert.Equal(t, testNowMs+econLeaseMs, rec.GetLeaseUntilMs(), "落空时一个字都不写")

	// 原持有者的租约过期、被别的副本重新领走之后,它再来放弃同样落空。
	retaken := landingClaim(t, f, 1201, testNowMs+econLeaseMs+1, 0x1a3)
	assert.ErrorIs(t, f.store.Abandon(f.ctx, op, testNowMs+econLeaseMs+2), assetop.ErrLeaseLost)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, econRecord(t, f, 1201).GetStatus())
	assert.Empty(t, finalized(), "落空不回调")
	assert.Empty(t, orphans(), "落空不做对侧账")

	// 现持有者放弃成功;再放弃一次(行已终结)落空。行不存在同样落空。
	require.NoError(t, f.store.Abandon(f.ctx, retaken, testNowMs+econLeaseMs+3))
	assert.ErrorIs(t, f.store.Abandon(f.ctx, retaken, testNowMs+econLeaseMs+4), assetop.ErrLeaseLost, "已终结")
	assert.ErrorIs(t, f.store.Abandon(f.ctx, assetop.Op{OpID: 1299, LeaseToken: 0x1a4}, testNowMs), assetop.ErrLeaseLost, "行不存在")
	got := finalized()
	require.Len(t, got, 1, "只有真正放弃的那一次回调")
	assert.Equal(t, assetop.StatusAbandoned, got[0].Status)
	// 夹具行绑定的帮会 7700 不存在:兑换退帮贡无处可退,与 REJECTED 一样计 orphan,而且只计一次。
	assert.Equal(t, []string{"shop/refund_member_gone"}, orphans())
}

// TestAbandon_CounterpartyMatchesRejected:同一帮会两名玩家各下一笔数值完全相同的捐献与兑换,一人的被 scene 永久拒绝(REJECTED,
// 作对照),另一人的被自动放弃。两人的对侧账必须逐项相同:捐献退今日次数、资金与帮贡不入账;兑换退帮贡、退限购。
// 放弃行本身:ABANDONED、不置 durable、不改 last_outcome、reason_tip_id 保持 0、同写 next_attempt_ms = 终结时刻、
// 不碰 last_send_ms;提交后回调一次(摘要 bump 的唯一出口是 logic.OnAssetFinalized);再放弃落空,对侧账不做第二遍。
func TestAbandon_CounterpartyMatchesRejected(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID   uint64 = 7801
		leader    uint64 = 8801
		abandoner uint64 = 8802
		rejectee  uint64 = 8803
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader,
		map[uint64]uint32{abandoner: constants.RoleMember, rejectee: constants.RoleMember})
	for _, p := range []uint64{abandoner, rejectee} {
		econSetContribution(t, f, guildID, p, 1000, 1000)
	}
	finalized := landingCaptureFinalized(f)

	type orders struct{ donate, shop uint64 }
	place := func(playerID, base uint64) orders {
		d := econDonation(t, base+1, playerID, guildID, testNowMs)
		_, err := f.econ.ReserveDonation(f.ctx, d)
		require.NoError(t, err)
		s := econShopOrder(t, base+2, playerID, guildID, 2, 300, 5, testNowMs)
		_, err = f.econ.ReserveShopOrder(f.ctx, s)
		require.NoError(t, err)
		return orders{donate: d.OpID, shop: s.OpID}
	}
	ab := place(abandoner, 9_801_000)
	rj := place(rejectee, 9_801_100)

	for _, opID := range []uint64{rj.donate, rj.shop} {
		ok, err := f.store.Finalize(f.ctx, assetop.Op{OpID: opID, PlayerID: rejectee}, assetop.StatusRejected,
			econRejectedResult(), testNowMs+100)
		require.NoError(t, err)
		require.True(t, ok)
	}

	// 捐献先经历一次本地 NOT_HERE 重排(留下 last_outcome 作证据),再领取、放弃。预留时的同步投递租约在首次领取时折进 last_send_ms。
	claimAt := testNowMs + econLeaseMs + 1
	op := landingClaim(t, f, ab.donate, claimAt, 0x2b1)
	require.NoError(t, f.store.Reschedule(f.ctx, op, claimAt+1_800_000, landingNotHereLocal(), claimAt))
	op = landingClaim(t, f, ab.donate, claimAt+10, 0x2b2)
	abandonAt := claimAt + 20
	require.NoError(t, f.store.Abandon(f.ctx, op, abandonAt))
	shopOp := landingClaim(t, f, ab.shop, claimAt, 0x2b3)
	require.NoError(t, f.store.Abandon(f.ctx, shopOp, abandonAt))

	counters := []struct {
		kind pb.GuildDailyCounterKind
		ref  uint32
	}{
		{pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1},
		{pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101},
	}
	for _, c := range counters {
		abUsed, _ := econCounterUsed(t, f, abandoner, c.kind, c.ref, econDayKey)
		rjUsed, _ := econCounterUsed(t, f, rejectee, c.kind, c.ref, econDayKey)
		assert.Zero(t, abUsed, "放弃退回计数(kind=%s)", c.kind)
		assert.Equal(t, rjUsed, abUsed, "与 REJECTED 退得一样多(kind=%s)", c.kind)
	}
	abTotal, abBalance, found := econContribution(t, f, guildID, abandoner)
	require.True(t, found)
	rjTotal, rjBalance, _ := econContribution(t, f, guildID, rejectee)
	assert.Equal(t, uint64(1000), abBalance, "兑换退帮贡")
	assert.Equal(t, rjBalance, abBalance)
	assert.Equal(t, uint64(1000), abTotal, "捐献的帮贡不入账")
	assert.Equal(t, rjTotal, abTotal)
	assert.Zero(t, econFunds(t, f, guildID), "被放弃 / 被拒的捐献都不记资金")

	rec := econRecord(t, f, ab.donate)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABANDONED, rec.GetStatus())
	assert.Zero(t, rec.GetDurable(), "不是 scene 确认的结局,不置 durable")
	assert.Equal(t, uint32(assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_NOT_HERE), rec.GetLastOutcome(), "保留最后一次真实答复作证据")
	assert.Zero(t, rec.GetReasonTipId())
	assert.Equal(t, abandonAt, rec.GetNextAttemptMs(), "终态行的 next_attempt_ms = 终结时刻(07 §7.4.1)")
	assert.Equal(t, abandonAt, rec.GetUpdatedMs())
	assert.Zero(t, rec.GetLeaseUntilMs())
	assert.Equal(t, testNowMs+econLeaseMs, rec.GetLastSendMs(), "终结不碰 last_send_ms;本地重排也没推进它")
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABANDONED, econRecord(t, f, ab.shop).GetStatus())

	var abandoned []FinalizedOp
	for _, fo := range finalized() {
		if fo.Status == assetop.StatusAbandoned {
			abandoned = append(abandoned, fo)
		}
	}
	assert.ElementsMatch(t, []FinalizedOp{
		{OpID: ab.donate, PlayerID: abandoner, GuildID: guildID, Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE, Status: assetop.StatusAbandoned},
		{OpID: ab.shop, PlayerID: abandoner, GuildID: guildID, Kind: pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_SHOP, Status: assetop.StatusAbandoned},
	}, abandoned)

	assert.ErrorIs(t, f.store.Abandon(f.ctx, shopOp, abandonAt+1), assetop.ErrLeaseLost, "已终结:再放弃落空")
	_, abBalance, _ = econContribution(t, f, guildID, abandoner)
	assert.Equal(t, uint64(1000), abBalance, "帮贡只退一次")
}

// TestAbandon_ShopBuyerLeftIsOrphanLikeRejected:兑换者已离帮时帮贡退不回去,计 orphan{shop,refund_member_gone},
// 限购照退 —— 与 TestFinalize_ShopRejectedRefundsContributionAndLimit 的 REJECTED 分支同一结论。
func TestAbandon_ShopBuyerLeftIsOrphanLikeRejected(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7811
		leader  uint64 = 8811
		buyer   uint64 = 8812
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{buyer: constants.RoleMember})
	econSetContribution(t, f, guildID, buyer, 1000, 1000)
	order := econShopOrder(t, 9_811_001, buyer, guildID, 1, 150, 5, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, order)
	require.NoError(t, err)
	_, err = f.repo.LeaveGuild(f.ctx, guildID, buyer, testNowMs+30)
	require.NoError(t, err)

	orphans := econCaptureOrphans(t)
	op := landingClaim(t, f, order.OpID, testNowMs+econLeaseMs+1, 0x2c1)
	require.NoError(t, f.store.Abandon(f.ctx, op, testNowMs+econLeaseMs+2))
	assert.Equal(t, []string{"shop/refund_member_gone"}, orphans())
	_, _, found := econContribution(t, f, guildID, buyer)
	assert.False(t, found, "离帮者的成员行不能被退款写回来")
	used, _ := econCounterUsed(t, f, buyer, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.Zero(t, used, "限购属于玩家,照退")
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABANDONED, econRecord(t, f, order.OpID).GetStatus())
}

// TestAbandon_ActivityRewardOnlyAudit:活动发奖的帮贡与资金在入队事务里已记完,放弃时没有可退之物 —— 只终结、留审计行,
// 帮会资金、成员帮贡、任何计数行都不动,不计 orphan;照常回调一次(OnAssetFinalized 据此 bump 活动域)。
// 行上故意带着非 0 的 funds_delta / contribution_delta 与计数键:哪条对侧账分支误认了这个 kind,这里就会看到变化。
func TestAbandon_ActivityRewardOnlyAudit(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7821
		leader  uint64 = 8821
		player  uint64 = 8822
		opID    uint64 = 9_821_001
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{player: constants.RoleMember})
	mustExec(t, f.ctx, f.db, "UPDATE guild SET funds=? WHERE guild_id=?", 777, guildID)
	econSetContribution(t, f, guildID, player, 100, 60)
	counterKinds := []pb.GuildDailyCounterKind{
		pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE,
		pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP,
		pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_ACTIVITY,
	}
	for _, kind := range counterKinds {
		mustExec(t, f.ctx, f.db,
			`INSERT INTO guild_daily_counter (player_id, counter_kind, ref_id, period_key, used_count, updated_ms) VALUES (?, ?, ?, ?, ?, ?)`,
			player, int32(kind), 1, econDayKey, 2, testNowMs)
	}
	econInsertOp(t, f, landingShopRow(t, opID, player, 1, func(r *pb.GuildAssetOpRecord) {
		r.GuildId = guildID
		r.Kind = pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD
		r.TxType = uint32(rollbackpb.TransactionType_TX_GUILD_ACTIVITY_REWARD)
		r.RefId, r.RefCount, r.PeriodKey = 1, 1, econDayKey
		r.ContributionDelta, r.FundsDelta = 50, 500
	}))

	orphans := econCaptureOrphans(t)
	finalized := landingCaptureFinalized(f)
	op := landingClaim(t, f, opID, testNowMs, 0x2d1)
	require.NoError(t, f.store.Abandon(f.ctx, op, testNowMs+1))

	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABANDONED, econRecord(t, f, opID).GetStatus())
	assert.Equal(t, uint64(777), econFunds(t, f, guildID), "放弃发奖不动帮会资金")
	total, balance, found := econContribution(t, f, guildID, player)
	require.True(t, found)
	assert.Equal(t, uint64(100), total, "放弃发奖不动帮贡")
	assert.Equal(t, uint64(60), balance)
	for _, kind := range counterKinds {
		used, found := econCounterUsed(t, f, player, kind, 1, econDayKey)
		require.True(t, found, "kind=%s", kind)
		assert.Equal(t, uint32(2), used, "放弃发奖不退任何计数(kind=%s)", kind)
	}
	assert.Empty(t, orphans(), "只 CAS 的终结不计 orphan")
	got := finalized()
	require.Len(t, got, 1)
	assert.Equal(t, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_ACTIVITY_REWARD, got[0].Kind)
	assert.Equal(t, assetop.StatusAbandoned, got[0].Status)
}

// TestAbandonOnce_EndToEndWithGuildStore:assetop.Loop.AbandonOnce 跑在真库的 GuildAssetStore 上(设计 §6.3 的六条证据由
// assetop 判,这里验 Store 一侧的接线):玩家无位置、账本确定未见、距最后一次可能的发送已超过安全间隔的那一行被放弃并退帮贡 / 限购;
// 有位置的那一行判定不放弃、归还租约、对侧账不动;整个过程不向 scene 发任何请求。
func TestAbandonOnce_EndToEndWithGuildStore(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		guildID uint64 = 7831
		leader  uint64 = 8831
		offline uint64 = 8832
		online  uint64 = 8833
	)
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader,
		map[uint64]uint32{offline: constants.RoleMember, online: constants.RoleMember})
	for _, p := range []uint64{offline, online} {
		econSetContribution(t, f, guildID, p, 1000, 1000)
	}
	offOrder := econShopOrder(t, 9_831_001, offline, guildID, 1, 100, 5, testNowMs)
	_, err := f.econ.ReserveShopOrder(f.ctx, offOrder)
	require.NoError(t, err)
	onOrder := econShopOrder(t, 9_831_002, online, guildID, 1, 100, 5, testNowMs)
	_, err = f.econ.ReserveShopOrder(f.ctx, onOrder)
	require.NoError(t, err)

	nowMs := testNowMs + 181*econDayMs
	applier := &landingApplier{}
	loop, err := assetop.NewLoop(assetop.DefaultLoopConfig(), f.store, applier, nil,
		func() time.Time { return time.UnixMilli(int64(nowMs)) })
	require.NoError(t, err)
	loop.Ledger = landingLedger{}

	abandoned, err := loop.AbandonOnce(f.ctx, nowMs)
	require.NoError(t, err)
	assert.Equal(t, 1, abandoned)
	assert.Empty(t, applier.requests(), "放弃路径不向 scene 发任何请求")

	off := econRecord(t, f, offOrder.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABANDONED, off.GetStatus())
	assert.Equal(t, nowMs, off.GetNextAttemptMs())
	_, balance, _ := econContribution(t, f, guildID, offline)
	assert.Equal(t, uint64(1000), balance, "放弃退帮贡")
	used, _ := econCounterUsed(t, f, offline, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, econDayKey)
	assert.Zero(t, used, "放弃退限购")

	on := econRecord(t, f, onOrder.OpID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, on.GetStatus(), "玩家有位置:交给正常流程")
	assert.Zero(t, on.GetLeaseUntilMs(), "判定不放弃之后归还了租约")
	_, balance, _ = econContribution(t, f, guildID, online)
	assert.Equal(t, uint64(900), balance, "不放弃就不退")
}
