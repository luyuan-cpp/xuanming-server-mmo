package data

// 回档前检查查询(asset_op_divergence_repo.go)的测试,编号对应 07-rollback-fail-closed.md §7.10.2 的 G1–G6、G5b。
//
// 与 economy_repo_test.go 同一套两层纪律:
//  1. **纯单测**(不连库,任何环境都跑):入参越界在碰库之前失败;语句形状与占位符 / 参数一一对应;
//     G6 源码守卫(状态不写数字字面量、语句不带锁定子句、本文件不开事务)。
//  2. **真库用例**(GUILD_TEST_MYSQL_DSN 未设即 Skip,夹具 openEconomyFixture):G1–G5、G5b。
//     G5 / G5b 必须经真实的 ResolveManually / Finalize 终结行 —— 直接插行钉不住"终态行 next_attempt_ms = 终结时刻"
//     这条主路径(07 §7.4.1 对 B5b 的硬要求),而整道回档闸的正确性建立在它上面。
//
// 时间一律用 testNowMs 派生的常量显式传入,不读真实墙钟(AGENTS §11.4)。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	pb "proto/guild"

	"shared/assetop"

	"guild/internal/constants"
)

const divergenceRepoSource = "asset_op_divergence_repo.go"

// ── 纯单测 ────────────────────────────────────────────────────

// TestListAppliedSinceRejectsMalformedInputBeforeTouchingStorage:GuildRepo 没有连接池,越过校验就会当场 panic,
// 所以"每个坏入参都返回 error"本身就证明了校验先于碰库。
func TestListAppliedSinceRejectsMalformedInputBeforeTouchingStorage(t *testing.T) {
	store, err := NewGuildAssetStore(econNoDBRepo(t))
	require.NoError(t, err)

	tooMany := make([]uint64, MaxAppliedOpsPlayerIDs+1)
	for i := range tooMany {
		tooMany[i] = uint64(i + 1)
	}
	good := func() AppliedOpsQuery {
		return AppliedOpsQuery{PlayerIDs: []uint64{1}, SinceMs: testNowMs, Limit: MaxAppliedOpsPageLimit}
	}
	breakers := []struct {
		name    string
		breakIt func(*AppliedOpsQuery)
	}{
		{"player_ids 为空", func(q *AppliedOpsQuery) { q.PlayerIDs = nil }},
		{"player_ids 超上限", func(q *AppliedOpsQuery) { q.PlayerIDs = tooMany }},
		{"limit 为 0", func(q *AppliedOpsQuery) { q.Limit = 0 }},
		{"limit 超上限", func(q *AppliedOpsQuery) { q.Limit = MaxAppliedOpsPageLimit + 1 }},
		{"since 为 0", func(q *AppliedOpsQuery) { q.SinceMs = 0 }},
	}
	for _, b := range breakers {
		q := good()
		b.breakIt(&q)
		ops, next, err := store.ListAppliedAssetOpsSince(t.Context(), q)
		assert.Error(t, err, b.name)
		assert.Nil(t, ops, b.name)
		assert.Zero(t, next, b.name)
	}
}

// TestListAppliedSinceStatementShape(G6 的语句半边):钉住两种形状的完整文本(07 §7.10.3 步 7 的 EXPLAIN 就拿它跑),
// 占位符个数与参数个数逐一对应,两个状态参数绑的是生成常量,且语句不带任何锁定子句(纯非锁定读,92-handoff §12.2)。
func TestListAppliedSinceStatementShape(t *testing.T) {
	assert.Equal(t,
		"SELECT o.`op_id`, o.`player_id`, o.`guild_id`, o.`stream`, o.`kind`, o.`status`,"+
			" o.`funds_delta`, o.`contribution_delta`, o.`updated_ms` FROM guild_asset_op o"+
			" WHERE o.`status` IN (?, ?) AND o.`next_attempt_ms` > ? AND o.`player_id` IN (?)"+
			" AND o.`op_id` > ? ORDER BY o.`op_id` ASC LIMIT ?",
		appliedOpsSQL(1, false))
	assert.Equal(t,
		"SELECT o.`op_id`, o.`player_id`, o.`guild_id`, o.`stream`, o.`kind`, o.`status`,"+
			" o.`funds_delta`, o.`contribution_delta`, o.`updated_ms` FROM guild_asset_op o"+
			" LEFT JOIN guild g ON g.guild_id = o.`guild_id`"+
			" WHERE o.`status` IN (?, ?) AND o.`next_attempt_ms` > ? AND o.`player_id` IN (?,?)"+
			" AND o.`op_id` > ? AND (g.zone_id = ? OR g.guild_id IS NULL) ORDER BY o.`op_id` ASC LIMIT ?",
		appliedOpsSQL(2, true))

	for _, players := range []int{1, 2, MaxAppliedOpsPlayerIDs} {
		for _, zone := range []uint32{0, 7} {
			q := AppliedOpsQuery{ZoneID: zone, PlayerIDs: make([]uint64, players), SinceMs: 1, AfterOpID: 5, Limit: 500}
			query := appliedOpsSQL(players, zone != 0)
			args := appliedOpsArgs(q)
			assert.Equal(t, strings.Count(query, "?"), len(args), "players=%d zone=%d", players, zone)
			assert.Equal(t, uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED), args[0])
			assert.Equal(t, uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL), args[1])
			assert.Equal(t, q.Limit+1, args[len(args)-1], "多取一行只用来判断还有没有下一页")

			upper := strings.ToUpper(query)
			for _, clause := range []string{"FOR UPDATE", "FOR SHARE", "LOCK IN SHARE MODE"} {
				assert.NotContains(t, upper, clause, "回档检查是纯非锁定读:%s", query)
			}
		}
	}
}

// TestListAppliedSinceSourceGuards(G6 的源码半边):
//   - 07 §7.10.2 G6 原文的字面量守卫:`(status|kind|stream)\s*(=|IN)\s*\(?[0-9]` 在本文件零命中;
//   - 本文件不开事务、不取锁:不出现 BeginTx / WithTxRetry / inTx,也不引用任何点锁语句。加了这些,它就进了取锁序列,
//     要回 92-handoff §12.2 / §12.4 重新推演;这里让"顺手包一层事务"当场红。
func TestListAppliedSinceSourceGuards(t *testing.T) {
	raw, err := os.ReadFile(divergenceRepoSource)
	require.NoError(t, err, "go test 的工作目录是包目录,%s 必须能直接读到", divergenceRepoSource)
	literal := regexp.MustCompile(`(status|kind|stream)\s*(=|IN)\s*\(?[0-9]`)
	assert.Empty(t, literal.FindAllString(string(raw), -1), "状态 / 种类 / 流一律绑定生成常量,不写数字字面量")

	file, err := parser.ParseFile(token.NewFileSet(), divergenceRepoSource, raw, parser.SkipObjectResolution)
	require.NoError(t, err)
	forbidden := map[string]bool{
		"BeginTx": true, "Begin": true, "WithTxRetry": true, "inTx": true,
		"sqlLockAssetOp": true, "sqlLockSeqGuard": true, "lockRowExists": true,
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && forbidden[ident.Name] {
			t.Errorf("%s 出现了 %s:回档检查必须是事务外的纯非锁定读(92-handoff §12.2)", divergenceRepoSource, ident.Name)
		}
		return true
	})
}

// ── 真库用例 ──────────────────────────────────────────────────

// divSince 是真库用例共用的检查时刻:造的终态行都终结在它之后。
const divSince = testNowMs - 3_600_000

// divRecord 造一行终态捐献指令(直接插行,G1–G4 用):seq 取 op_id,保证 uk_guild_asset_op 不撞。
func divRecord(opID, playerID, guildID uint64, status pb.GuildAssetOpStatus, finalizedMs uint64, payload []byte) *pb.GuildAssetOpRecord {
	rec := econDonateRecord(opID, playerID, guildID, 1, opID, payload)
	rec.Status = status
	rec.Durable = 1
	rec.NextAttemptMs = finalizedMs
	rec.UpdatedMs = finalizedMs
	return rec
}

func divList(t *testing.T, f econFixture, q AppliedOpsQuery) ([]*pb.GuildAssetOpBrief, uint64) {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = MaxAppliedOpsPageLimit
	}
	ops, next, err := f.store.ListAppliedAssetOpsSince(f.ctx, q)
	require.NoError(t, err)
	return ops, next
}

func divOpIDs(ops []*pb.GuildAssetOpBrief) []uint64 {
	ids := make([]uint64, 0, len(ops))
	for _, op := range ops {
		ids = append(ids, op.GetOpId())
	}
	return ids
}

// TestListAppliedSince_OnlyAppliedAndPartial(G1):只回 APPLIED / APPLIED_PARTIAL;PENDING / REJECTED / ABORTED
// 即使终结时刻晚于 since 也不出现;别的玩家的行不出现;摘要各字段与库行一致。
func TestListAppliedSince_OnlyAppliedAndPartial(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		playerA uint64 = 8801
		playerB uint64 = 8802
		other   uint64 = 8803
		guildID uint64 = 7801
	)
	payload := econDonatePayload(t, 10000)
	at := divSince + 10
	econInsertOpsBulk(t, f, []*pb.GuildAssetOpRecord{
		divRecord(9_800_001, playerA, guildID, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, at, payload),
		divRecord(9_800_002, playerA, guildID, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, at, payload),
		divRecord(9_800_003, playerA, guildID, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, at, payload),
		divRecord(9_800_004, playerB, guildID, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED, at, payload),
		divRecord(9_800_005, playerB, guildID, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL, at+1, payload),
		divRecord(9_800_006, other, guildID, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, at, payload),
	})

	ops, next := divList(t, f, AppliedOpsQuery{PlayerIDs: []uint64{playerA, playerB}, SinceMs: divSince})

	assert.Zero(t, next)
	assert.Equal(t, []uint64{9_800_002, 9_800_005}, divOpIDs(ops))
	require.Len(t, ops, 2)
	wantFirst := &pb.GuildAssetOpBrief{
		OpId: 9_800_002, PlayerId: playerA, GuildId: guildID,
		Stream:     uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT),
		Kind:       uint32(pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_DONATE),
		Status:     uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED),
		FundsDelta: 1000, ContributionDelta: 10, UpdatedMs: at,
	}
	assert.True(t, proto.Equal(wantFirst, ops[0]), "摘要字段与库行不符:%v", ops[0])
	assert.Equal(t, uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED_PARTIAL), ops[1].GetStatus())
	assert.Equal(t, playerB, ops[1].GetPlayerId())
}

// TestListAppliedSince_SinceIsStrict(G2):next_attempt_ms == since 不返回,since + 1 返回。
func TestListAppliedSince_SinceIsStrict(t *testing.T) {
	f := openEconomyFixture(t)
	const player uint64 = 8811
	payload := econDonatePayload(t, 10000)
	econInsertOpsBulk(t, f, []*pb.GuildAssetOpRecord{
		divRecord(9_810_001, player, 7811, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, divSince, payload),
		divRecord(9_810_002, player, 7811, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, divSince+1, payload),
	})

	ops, next := divList(t, f, AppliedOpsQuery{PlayerIDs: []uint64{player}, SinceMs: divSince})

	assert.Zero(t, next)
	assert.Equal(t, []uint64{9_810_002}, divOpIDs(ops), "终结时刻等于 since 的行在快照之内,不算分歧")
}

// TestListAppliedSince_PagesByOpID(G3):1201 行按 limit 500 翻三页,op_id 严格升序、无重无漏,末页游标 0;
// 恰好 500 行时一页查完、游标 0(limit+1 判定,不多出一个空页)。
func TestListAppliedSince_PagesByOpID(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		player     uint64 = 8821
		exactly500 uint64 = 8822
		baseOpID   uint64 = 9_820_000
		exactBase  uint64 = 9_830_000
		total             = 1201
		pageLimit         = 500
	)
	payload := econDonatePayload(t, 10000)
	var want []uint64
	var recs []*pb.GuildAssetOpRecord
	// 终结时刻与 op_id 刻意反序:证明翻页按 op_id 而不是按 next_attempt_ms。
	for i := uint64(1); i <= total; i++ {
		opID := baseOpID + i
		want = append(want, opID)
		recs = append(recs, divRecord(opID, player, 7821, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED,
			divSince+total+1-i, payload))
	}
	econInsertOpsBulk(t, f, recs)

	var (
		got   []uint64
		pages []int
		after uint64
	)
	for {
		ops, next := divList(t, f, AppliedOpsQuery{PlayerIDs: []uint64{player}, SinceMs: divSince, AfterOpID: after, Limit: pageLimit})
		pages = append(pages, len(ops))
		got = append(got, divOpIDs(ops)...)
		if next == 0 {
			break
		}
		require.NotEmpty(t, ops)
		assert.Equal(t, ops[len(ops)-1].GetOpId(), next, "游标 = 本页最后一行的 op_id")
		after = next
		require.Less(t, len(pages), 10, "翻页没有收敛")
	}
	assert.Equal(t, []int{500, 500, 201}, pages)
	assert.Equal(t, want, got, "op_id 严格升序、无重无漏")

	recs = recs[:0]
	for i := uint64(1); i <= pageLimit; i++ {
		recs = append(recs, divRecord(exactBase+i, exactly500, 7822, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED,
			divSince+i, payload))
	}
	econInsertOpsBulk(t, f, recs)
	ops, next := divList(t, f, AppliedOpsQuery{PlayerIDs: []uint64{exactly500}, SinceMs: divSince, Limit: pageLimit})
	assert.Len(t, ops, pageLimit)
	assert.Zero(t, next, "恰好 limit 行:一页查完,不再给出空的下一页")
}

// TestListAppliedSince_ZoneFilterKeepsDisbandedGuilds(G4):zone_id = 0 全返回;zone_id = Z 时保留本 zone 与
// **已解散帮会**(guild 行已删,LEFT JOIN 取到 NULL)的行,滤掉别的 zone(07 §7.4.3)。
func TestListAppliedSince_ZoneFilterKeepsDisbandedGuilds(t *testing.T) {
	f := openEconomyFixture(t)
	const (
		player    uint64 = 8831
		guildZ1   uint64 = 7831
		guildZ2   uint64 = 7832
		disbanded uint64 = 7833 // 不建 guild 行 = 已解散
		zone1     uint32 = 1
		zone2     uint32 = 2
	)
	seedManagedGuild(t, f.ctx, f.db, guildZ1, zone1, 1, 50, 8834, nil)
	seedManagedGuild(t, f.ctx, f.db, guildZ2, zone2, 1, 50, 8835, nil)
	payload := econDonatePayload(t, 10000)
	at := divSince + 10
	econInsertOpsBulk(t, f, []*pb.GuildAssetOpRecord{
		divRecord(9_840_001, player, guildZ1, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, at, payload),
		divRecord(9_840_002, player, guildZ2, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, at, payload),
		divRecord(9_840_003, player, disbanded, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, at, payload),
	})

	cases := []struct {
		zone uint32
		want []uint64
	}{
		{0, []uint64{9_840_001, 9_840_002, 9_840_003}},
		{zone1, []uint64{9_840_001, 9_840_003}},
		{zone2, []uint64{9_840_002, 9_840_003}},
		{99, []uint64{9_840_003}},
	}
	for _, tc := range cases {
		ops, next := divList(t, f, AppliedOpsQuery{ZoneID: tc.zone, PlayerIDs: []uint64{player}, SinceMs: divSince})
		assert.Zero(t, next, "zone=%d", tc.zone)
		assert.Equal(t, tc.want, divOpIDs(ops), "zone=%d", tc.zone)
	}
}

// divReserveDonation 走真实的预留事务造一行未决捐献(G5 / G5b 的起点),返回 op_id。
func divReserveDonation(t *testing.T, f econFixture, guildID, leader, donor, opID uint64) uint64 {
	t.Helper()
	seedManagedGuild(t, f.ctx, f.db, guildID, 2, 1, 50, leader, map[uint64]uint32{donor: constants.RoleMember})
	in := econDonation(t, opID, donor, guildID, testNowMs)
	_, err := f.econ.ReserveDonation(f.ctx, in)
	require.NoError(t, err)

	ops, _ := divList(t, f, AppliedOpsQuery{PlayerIDs: []uint64{donor}, SinceMs: testNowMs - 1})
	require.Empty(t, ops, "未决行不是分歧(07 §7.1.1 注 1)")
	return in.OpID
}

// TestListAppliedSince_SeesManuallyResolvedRow(G5):assetopfix 人工判 APPLIED 的行必须查得到,
// 且按人工终结时刻判(ResolveManually 同写 next_attempt_ms = now,07 §7.4.1 / U5)。
func TestListAppliedSince_SeesManuallyResolvedRow(t *testing.T) {
	f := openEconomyFixture(t)
	const donor uint64 = 8842
	opID := divReserveDonation(t, f, 7841, 8841, donor, 9_850_001)

	resolveNow := testNowMs + 7_200_000
	clock := func() time.Time { return time.UnixMilli(int64(resolveNow)) }
	resolved, err := assetop.ResolveManually(f.ctx, f.store,
		assetop.ManualResolution{OpID: opID, Final: assetop.StatusApplied, Operator: "ops-alice", Reason: "回档检查用例"}, nil, clock)
	require.NoError(t, err)
	require.True(t, resolved)

	ops, _ := divList(t, f, AppliedOpsQuery{PlayerIDs: []uint64{donor}, SinceMs: resolveNow - 1})
	require.Len(t, ops, 1)
	assert.Equal(t, opID, ops[0].GetOpId())
	assert.Equal(t, uint32(pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED), ops[0].GetStatus())

	ops, _ = divList(t, f, AppliedOpsQuery{PlayerIDs: []uint64{donor}, SinceMs: resolveNow})
	assert.Empty(t, ops, "人工终结时刻 = since 时不算分歧(严格大于)")
}

// TestListAppliedSince_SeesRowFinalizedByStore(G5b):经**真实** GuildAssetStore.Finalize 终结一行,
// 断言 next_attempt_ms == 传入的 nowMs,且 since = nowMs − 1 时能被查到。钉住 07 §7.4.1 对 B5b 的硬要求:
// Finalize 漏写 next_attempt_ms,终态行就停在最后一次重排时刻,`next_attempt_ms > since` 漏行 = 回档复制资产。
func TestListAppliedSince_SeesRowFinalizedByStore(t *testing.T) {
	f := openEconomyFixture(t)
	const donor uint64 = 8852
	opID := divReserveDonation(t, f, 7851, 8851, donor, 9_860_001)

	finalNow := testNowMs + 1234
	finalized, err := f.store.Finalize(f.ctx, assetop.Op{OpID: opID, PlayerID: donor},
		assetop.StatusApplied, econAppliedResult(), finalNow)
	require.NoError(t, err)
	require.True(t, finalized)
	assert.Equal(t, finalNow, econRecord(t, f, opID).GetNextAttemptMs(), "终态行的 next_attempt_ms 必须等于终结时刻")

	ops, next := divList(t, f, AppliedOpsQuery{PlayerIDs: []uint64{donor}, SinceMs: finalNow - 1})
	assert.Zero(t, next)
	require.Len(t, ops, 1)
	assert.Equal(t, opID, ops[0].GetOpId())
	assert.Equal(t, finalNow, ops[0].GetUpdatedMs())

	ops, _ = divList(t, f, AppliedOpsQuery{PlayerIDs: []uint64{donor}, SinceMs: finalNow})
	assert.Empty(t, ops)
}
