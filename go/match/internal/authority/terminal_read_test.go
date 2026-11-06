package authority

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	pb "proto/match"
)

// scene 冻结到期查询读的 SQL 事实(terminal_read.go)。纯判据不起 TiDB;读路径的真 TiDB 用例受 MATCH_IT_TIDB 门控。

// terminalReadFormation 组一份可裁决的 v1 成局(能力证据完整、原件与成局身份一致),只在本文件用。
func terminalReadFormation(t *testing.T) *pb.MatchFormationRecord {
	t.Helper()
	f := &pb.MatchFormationRecord{BattleId: 931, OperationId: "terminal-read-op", TargetInstanceId: "terminal-read-uuid", Mode: 3, ConfigId: 1, State: 2,
		CreatedMs: 1_800_000_000_000, DeadlineMs: 1_800_000_300_000, Members: []*pb.MatchFormationMember{{PlayerId: 31}, {PlayerId: 32}}}
	request := &battlepb.CreateBattleRequest{BattleId: f.BattleId, PlacementOperationId: f.OperationId, TargetInstanceId: f.TargetInstanceId,
		BattleConfigId: f.ConfigId, MatchMode: f.Mode, OutcomeProtocolVersion: 1, CreatedAtMs: f.CreatedMs, DeadlineMs: f.DeadlineMs,
		Players: []*battlepb.BattlePlayerSnapshot{{PlayerId: 31, TeamIndex: 0}, {PlayerId: 32, TeamIndex: 1}}}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	require.NoError(t, err)
	f.CreateRequest = raw
	f.OutcomeProtocolVersion, f.PreflightSupportedVersion, f.PreflightInstanceId = 1, 1, f.TargetInstanceId
	return f
}

// 终局会不会落定只看原件可不可裁决(截止 ABORT 读的同一判据):可裁决的 v1 原件是「会」;没有成局行、终局协议 0、
// battle 明确采用旧协议、原件与成局身份不符都是「不会」—— 两条写终局的路径都要求原件可裁决。
func TestTerminalOutcomeExpectedOnlyForAdjudicableOriginals(t *testing.T) {
	require.True(t, TerminalOutcomeExpected(terminalReadFormation(t)))
	require.False(t, TerminalOutcomeExpected(nil), "没有成局行")
	for _, c := range []struct {
		why  string
		edit func(*pb.MatchFormationRecord)
	}{
		{"终局协议 0(旧局)", func(f *pb.MatchFormationRecord) { f.OutcomeProtocolVersion, f.PreflightSupportedVersion = 0, 0 }},
		{"没有 Create 原件", func(f *pb.MatchFormationRecord) { f.CreateRequest = nil }},
		{"battle 明确采用旧协议", func(f *pb.MatchFormationRecord) { f.AdoptedOutcomeObserved, f.AdoptedOutcomeProtocolVersion = true, 0 }},
		{"原件截止与成局不符", func(f *pb.MatchFormationRecord) { f.DeadlineMs++ }},
	} {
		f := terminalReadFormation(t)
		c.edit(f)
		require.False(t, TerminalOutcomeExpected(f), c.why)
	}
	adopted := terminalReadFormation(t)
	adopted.AdoptedOutcomeObserved, adopted.AdoptedOutcomeProtocolVersion = true, 1
	require.True(t, TerminalOutcomeExpected(adopted), "battle 回执采用协议 1 照样可裁决")
}

// 真 TiDB:没有成局行 → UNKNOWN、不会落定;可裁决原件而还没有终局 → UNKNOWN、会落定(scene 按未知续冻);
// COMPLETE 落库之后回 COMPLETE。全程只读,不改成局与票。
func TestTerminalTiDBBattleTerminalStateTellsPendingFromNoOriginal(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	decision, expected, err := s.BattleTerminalState(ctx, 987_654_321)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	require.False(t, expected, "没有成局行:这一局永远不会有终局")

	f, request := terminalResultFixture(t, s, false)
	decision, expected, err = s.BattleTerminalState(ctx, f.BattleId)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, decision)
	require.True(t, expected, "可裁决原件而终局还没落定:NONE 不是终局证据")
	after, err := s.Formation(ctx, f.BattleId)
	require.NoError(t, err)
	require.True(t, proto.Equal(f, after), "查询只读")

	committed, err := s.CommitBattleOutcome(ctx, request)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, committed)
	decision, expected, err = s.BattleTerminalState(ctx, f.BattleId)
	require.NoError(t, err)
	require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, decision)
	require.True(t, expected)
}
