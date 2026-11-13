package authority

// 原件内容不合格的局在截止之后作废(rejected_original_abort.go,2026-11-11)。
// 纯函数不碰 SQL;真 TiDB 沿用 terminalResultFixture(MATCH_IT_TIDB / MATCH_IT_REQUIRE_TIDB),每个子用例独立的库。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
	inboxpb "proto/player_inbox"
	"shared/inboxcontract"
)

func rejectedOriginalRequest(t *testing.T, e *kafkapb.BattleResultEvent) *pb.CommitBattleOutcomeRequest {
	t.Helper()
	return &pb.CommitBattleOutcomeRequest{BattleId: 901, PlacementOperationId: "op", TargetInstanceId: "uuid", RawResultPayload: resultBytes(t, e)}
}

func unknownVarintField() []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, 199, protowire.VarintType), 1)
}

// 版本错位只认:请求或结算正文带本版本不认识的字段、结算格式版本不是 1。内容不合格(索引对不上、超过 Inbox 上限)
// 与解不开的原字节都不算 —— 前者才会提前作废,后者照旧等截止作废取证与硬上限。
func TestOutcomeVersionSkewOnlyFlagsUnreadableVersions(t *testing.T) {
	valid := completeResult()
	require.False(t, outcomeVersionSkew(rejectedOriginalRequest(t, valid)), "合格原件")

	fled := proto.Clone(valid).(*kafkapb.BattleResultEvent)
	fled.FledPlayerIds = []uint64{999}
	require.False(t, outcomeVersionSkew(rejectedOriginalRequest(t, fled)), "逃跑索引对不上是内容不合格")

	limit, ok := inboxcontract.MaxPayloadBytes(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT)
	require.True(t, ok)
	oversized := proto.Clone(valid).(*kafkapb.BattleResultEvent)
	padSettlementTo(t, oversized.Settlements[0], limit+1)
	require.False(t, outcomeVersionSkew(rejectedOriginalRequest(t, oversized)), "超过 Inbox 上限是内容不合格")

	broken := rejectedOriginalRequest(t, valid)
	broken.RawResultPayload = []byte{0xff}
	require.False(t, outcomeVersionSkew(broken), "解不开的原字节是原件坏了")

	nested := proto.Clone(valid).(*kafkapb.BattleResultEvent)
	nested.Settlements[0].Pets[0].ProtoReflect().SetUnknown(unknownVarintField())
	require.True(t, outcomeVersionSkew(rejectedOriginalRequest(t, nested)), "结算正文带未知字段")

	format := proto.Clone(valid).(*kafkapb.BattleResultEvent)
	format.SettlementFormatVersion = 2
	require.True(t, outcomeVersionSkew(rejectedOriginalRequest(t, format)), "结算格式版本不是 1")

	unknownRequest := rejectedOriginalRequest(t, valid)
	unknownRequest.ProtoReflect().SetUnknown(unknownVarintField())
	require.True(t, outcomeVersionSkew(unknownRequest), "请求本身带未知字段")
}

// rejectedOriginalEdits 是两类内容拒收:锁外校验就拒(逃跑索引对不上,走 settleRejectedOriginal)与锁内对照创建原件才拒
// (对局配置与创建原件不符,走 commitBattleOutcomeLocked)。
var rejectedOriginalEdits = []struct {
	name string
	edit func(*kafkapb.BattleResultEvent)
}{
	{"锁外拒收", func(e *kafkapb.BattleResultEvent) { e.FledPlayerIds = []uint64{999} }},
	{"锁内拒收", func(e *kafkapb.BattleResultEvent) { e.BattleConfigId++ }},
}

func rejectedOriginalFixture(t *testing.T, s *Store, expired bool, edit func(*kafkapb.BattleResultEvent)) (*pb.MatchFormationRecord, *pb.CommitBattleOutcomeRequest) {
	t.Helper()
	f, req := terminalResultFixture(t, s, expired)
	event := &kafkapb.BattleResultEvent{}
	require.NoError(t, proto.Unmarshal(req.RawResultPayload, event))
	edit(event)
	req.RawResultPayload = resultBytes(t, event)
	return f, req
}

// 截止之前:内容不合格的原件照旧回原件冲突,不改任何行(battle 5 秒后重放)。
func TestRejectedOriginalTiDBBeforeDeadlineStaysRejected(t *testing.T) {
	for _, tc := range rejectedOriginalEdits {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			f, req := rejectedOriginalFixture(t, s, false, tc.edit)
			_, err := s.CommitBattleOutcome(ctx, req)
			require.ErrorIs(t, err, ErrBattleOutcomeOriginal)
			require.Zero(t, terminalRows(t, s, f.BattleId), "截止之前不作废")
			after, err := s.Formation(ctx, f.BattleId)
			require.NoError(t, err)
			require.True(t, proto.Equal(f, after), "被拒的原件不改成局")
		})
	}
}

// 过了截止加宽限:原执行实例的同一份重放写 ABORT 并回 ABORT。来源 MATCH_REJECTED_ORIGINAL_ABORT、没有 Battle 原字节,
// formation 5、原票 4、没有个人来源;再重放回放同一份 ABORT,不写第二份。修复前这里回原件冲突、什么都不写,
// 要等截止作废取证到硬上限(截止后约 14 分钟)。
func TestRejectedOriginalTiDBAbortsOnReplayAfterDeadline(t *testing.T) {
	for _, tc := range rejectedOriginalEdits {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			f, req := rejectedOriginalFixture(t, s, true, tc.edit)
			counted := metrics.GlobalFlowValue("terminal", terminalAbortRejectedOriginal)
			for round := range 2 {
				decision, err := s.CommitBattleOutcome(ctx, req)
				require.NoError(t, err, "第 %d 次重放", round+1)
				require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, decision)
			}
			require.Equal(t, counted+1, metrics.GlobalFlowValue("terminal", terminalAbortRejectedOriginal), "只在真正写下 ABORT 时计一次,回放不计")
			require.Equal(t, 1, terminalRows(t, s, f.BattleId), "重放只回放,不写第二份")
			var formatVersion, sourceKind uint32
			require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT format_version,source_kind FROM match_battle_completion WHERE battle_id=?", f.BattleId).Scan(&formatVersion, &sourceKind))
			require.Zero(t, formatVersion, "没有 Battle 原字节")
			require.EqualValues(t, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_REJECTED_ORIGINAL_ABORT, sourceKind)
			after, err := s.Formation(ctx, f.BattleId)
			require.NoError(t, err)
			require.EqualValues(t, 5, after.State)
			for _, m := range f.Members {
				require.EqualValues(t, 4, ticketRow(t, s, m.PlayerId).State, "原票随 ABORT 释放")
			}
			requireOnlyVoidSources(t, s, f.BattleId, len(f.Members))
		})
	}
}

// 过了截止也不提前作废的两类:版本错位(结算正文带本版本不认识的字段,新版 match 可能接受)与身份不是原执行实例的原件。
// 都照旧回原件冲突、不写终局,交给截止作废取证与硬上限。
func TestRejectedOriginalTiDBVersionSkewAndForeignIdentityWait(t *testing.T) {
	cases := []struct {
		name string
		edit func(*pb.CommitBattleOutcomeRequest, *kafkapb.BattleResultEvent)
	}{
		{"版本错位", func(_ *pb.CommitBattleOutcomeRequest, e *kafkapb.BattleResultEvent) {
			e.Settlements[0].Pets[0].ProtoReflect().SetUnknown(unknownVarintField())
		}},
		{"别的实例", func(r *pb.CommitBattleOutcomeRequest, e *kafkapb.BattleResultEvent) {
			r.TargetInstanceId = "20000000-0000-4000-8000-000000000009"
			e.FledPlayerIds = []uint64{999}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			f, req := terminalResultFixture(t, s, true)
			event := &kafkapb.BattleResultEvent{}
			require.NoError(t, proto.Unmarshal(req.RawResultPayload, event))
			tc.edit(req, event)
			req.RawResultPayload = resultBytes(t, event)
			_, err := s.CommitBattleOutcome(ctx, req)
			require.ErrorIs(t, err, ErrBattleOutcomeOriginal)
			require.Zero(t, terminalRows(t, s, f.BattleId), "不提前作废")
			after, err := s.Formation(ctx, f.BattleId)
			require.NoError(t, err)
			require.True(t, proto.Equal(f, after))
		})
	}
}

// 锁外读的成局与锁内的成局只差调度字段 retry_at_ms 时算同一成局(2026-11-11):截止作废等证据期间别的副本每 30 秒改一次
// 取证排期,恰好撞上的 COMPLETE 不能因此白回「未确认」。成员、身份、状态任一变了仍算并发变化。
func TestSameFormationIgnoringScheduleOnlyIgnoresRetryAt(t *testing.T) {
	base := &pb.MatchFormationRecord{BattleId: 901, OperationId: "op", TargetInstanceId: "uuid", State: 3, RetryAtMs: 100,
		Members: []*pb.MatchFormationMember{{PlayerId: 11, Ticket: "a"}, {PlayerId: 22, Ticket: "b"}}}
	rescheduled := proto.Clone(base).(*pb.MatchFormationRecord)
	rescheduled.RetryAtMs = 30100
	require.True(t, sameFormationIgnoringSchedule(base, rescheduled), "只改了取证排期")
	require.EqualValues(t, 100, base.RetryAtMs, "比对不改入参")
	for why, edit := range map[string]func(*pb.MatchFormationRecord){
		"状态":   func(f *pb.MatchFormationRecord) { f.State = 4 },
		"实例":   func(f *pb.MatchFormationRecord) { f.TargetInstanceId = "other" },
		"成员原票": func(f *pb.MatchFormationRecord) { f.Members[1].Ticket = "c" },
	} {
		changed := proto.Clone(base).(*pb.MatchFormationRecord)
		edit(changed)
		require.False(t, sameFormationIgnoringSchedule(base, changed), why)
	}
}
