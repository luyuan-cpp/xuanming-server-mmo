package logic

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"match/internal/pkg/ctxkeys"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	pb "proto/match"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
)

// scene 冻结到期时的 SQL 终局只读查询(battle_terminal_decision.go)。不起 TiDB:成局权威换成假读者,
// 盯「验签在读 SQL 之前」「读失败带内 retry」「回包签名绑定请求原件」这几条契约。

const terminalFixtureSceneSecret = "fixture-only-scene-terminal-secret-0000"

type fakeTerminalReader struct {
	decision pb.BattleOutcomeDecision
	expected bool
	err      error
	calls    []uint64
}

func (f *fakeTerminalReader) BattleTerminalState(_ context.Context, battle uint64) (pb.BattleOutcomeDecision, bool, error) {
	f.calls = append(f.calls, battle)
	return f.decision, f.expected, f.err
}

func newTerminalFixture(t *testing.T, reader *fakeTerminalReader) *BattleTerminalDecisionQuery {
	t.Helper()
	q, err := NewBattleTerminalDecisionQuery(reader, " "+terminalFixtureSceneSecret+"\n")
	require.NoError(t, err, "密钥两端的空白与 serviceauth 同一口径被裁掉")
	return q
}

func signedTerminalRequest(t *testing.T, battle uint64, caller, secret string) *pb.GetBattleTerminalDecisionRequest {
	t.Helper()
	in := &pb.GetBattleTerminalDecisionRequest{BattleId: battle}
	require.NoError(t, serviceauth.Sign(pb.MatchInternal_GetBattleTerminalDecision_FullMethodName, in, caller, secret, uint64(time.Now().UnixMilli())))
	return in
}

// 回包必须是 match 以方法 + "/reply" 签的,request_digest 等于请求清空 auth 之后的摘要;scene 两样都核。
func requireSignedTerminalReply(t *testing.T, in *pb.GetBattleTerminalDecisionRequest, out *pb.GetBattleTerminalDecisionResponse) {
	t.Helper()
	verifier, err := serviceauth.New(map[string]string{"match": terminalFixtureSceneSecret})
	require.NoError(t, err)
	caller, err := verifier.Verify(context.Background(), pb.MatchInternal_GetBattleTerminalDecision_FullMethodName+"/reply", out)
	require.NoError(t, err, "回包签名必须用同一把场景密钥、方法 + /reply")
	require.Equal(t, "match", caller)
	digest, err := serviceauth.Digest(in)
	require.NoError(t, err)
	require.Equal(t, digest, out.GetRequestDigest(), "回包绑定的是这一次请求原件")
}

// SQL 终局原样回,连同「终局会不会落定」:有可裁决原件而还没有决定(NONE)回 UNKNOWN + terminal_expected,
// scene 按未知续冻;没有成局行或原件不可裁决回 UNKNOWN 且 terminal_expected=false,scene 照常解冻。COMPLETE / ABORT 照抄。
func TestGetBattleTerminalDecisionAnswersTheSqlTerminalWithASignedReply(t *testing.T) {
	complete := pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE
	abort := pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT
	none := pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN
	for _, tc := range []struct {
		name     string
		decision pb.BattleOutcomeDecision
		expected bool
	}{
		{"有可裁决原件而终局还没落定", none, true},
		{"没有成局行或原件不可裁决", none, false},
		{"已COMPLETE", complete, true},
		{"已ABORT", abort, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeTerminalReader{decision: tc.decision, expected: tc.expected}
			q := newTerminalFixture(t, reader)
			in := signedTerminalRequest(t, 41, "scene", terminalFixtureSceneSecret)

			out, err := q.GetBattleTerminalDecision(context.Background(), in)
			require.NoError(t, err)
			require.Equal(t, tc.decision, out.GetDecision())
			require.Equal(t, tc.expected, out.GetTerminalExpected())
			require.False(t, out.GetRetry())
			require.Equal(t, []uint64{41}, reader.calls, "只读一次 SQL")
			requireSignedTerminalReply(t, in, out)
		})
	}
}

// 读失败与非法持久值都带内 retry,不回 gRPC 错误(发布准入会把 Unavailable 记成未知结局)。回包照样签名。
func TestGetBattleTerminalDecisionReadFailureAndInvalidRowAreInBandRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader *fakeTerminalReader
	}{
		{"SQL读失败", &fakeTerminalReader{err: errors.New("tidb: connection refused")}},
		{"非法持久值", &fakeTerminalReader{decision: pb.BattleOutcomeDecision(7), expected: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newTerminalFixture(t, tc.reader)
			in := signedTerminalRequest(t, 42, "scene", terminalFixtureSceneSecret)

			out, err := q.GetBattleTerminalDecision(context.Background(), in)
			require.NoError(t, err, "只读查询没有结局未知,读不到也带内回")
			require.True(t, out.GetRetry())
			require.Equal(t, pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, out.GetDecision(), "retry 时不替 scene 猜结论")
			require.False(t, out.GetTerminalExpected())
			requireSignedTerminalReply(t, in, out)
		})
	}
}

// 所有拒绝都发生在读 SQL 之前,且只用确定拒绝的码(PermissionDenied / InvalidArgument),不拖住排空证明。
func TestGetBattleTerminalDecisionRejectsBeforeReadingSql(t *testing.T) {
	reader := &fakeTerminalReader{decision: pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, expected: true}
	q := newTerminalFixture(t, reader)
	tampered := signedTerminalRequest(t, 43, "scene", terminalFixtureSceneSecret)
	tampered.BattleId = 44 // 签名覆盖 battle_id:换局即验不过
	background := context.Background()
	sessionCtx := ctxkeys.WithSessionDetails(background, &base.SessionDetails{SessionId: 1, PlayerId: 9701})

	for _, tc := range []struct {
		name string
		q    *BattleTerminalDecisionQuery
		ctx  context.Context
		in   *pb.GetBattleTerminalDecisionRequest
		code codes.Code
	}{
		{"未装配", nil, background, signedTerminalRequest(t, 43, "scene", terminalFixtureSceneSecret), codes.PermissionDenied},
		{"没有签名", q, background, &pb.GetBattleTerminalDecisionRequest{BattleId: 43}, codes.PermissionDenied},
		{"别的调用方battle", q, background, signedTerminalRequest(t, 43, "battle", terminalFixtureSceneSecret), codes.PermissionDenied},
		{"密钥不对", q, background, signedTerminalRequest(t, 43, "scene", strings.Repeat("x", 40)), codes.PermissionDenied},
		{"签名后改了局号", q, background, tampered, codes.PermissionDenied},
		{"客户端来源", q, sessionCtx, signedTerminalRequest(t, 43, "scene", terminalFixtureSceneSecret), codes.PermissionDenied},
		{"局号为0", q, background, signedTerminalRequest(t, 0, "scene", terminalFixtureSceneSecret), codes.InvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader.calls = nil
			out, err := tc.q.GetBattleTerminalDecision(tc.ctx, proto.Clone(tc.in).(*pb.GetBattleTerminalDecisionRequest))
			require.Nil(t, out)
			require.Equal(t, tc.code, status.Code(err))
			require.Empty(t, reader.calls, "拒绝发生在读 SQL 之前")
		})
	}
}

// 装配:缺密钥、密钥不足 32 字节、没有成局权威都拒绝装配(入口随之 PermissionDenied)。
func TestNewBattleTerminalDecisionQueryRejectsMissingSecretOrStore(t *testing.T) {
	_, err := NewBattleTerminalDecisionQuery(&fakeTerminalReader{}, "")
	require.Error(t, err)
	_, err = NewBattleTerminalDecisionQuery(&fakeTerminalReader{}, "short-secret")
	require.Error(t, err)
	_, err = NewBattleTerminalDecisionQuery(nil, terminalFixtureSceneSecret)
	require.Error(t, err)
}
