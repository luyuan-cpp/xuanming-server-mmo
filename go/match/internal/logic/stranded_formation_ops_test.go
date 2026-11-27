package logic

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"match/internal/authority"
	"match/internal/config"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	base "proto/common/base"
	pb "proto/match"
	"shared/serviceauth"
)

// 人工处置入口(stranded_formation_ops.go)。不起 TiDB:成局权威换成假实现,盯「准入与参数校验在碰库之前」
// 「目标环境不符不碰库」「Store 结论到带内 verdict 的映射」「提交结果未知回 Unavailable」「只有真写了才是 executed=true」。
// 放行判据本身与锁内写入由 authority/manual_abort_test.go(纯函数 + 真 TiDB)覆盖。

const opsFixtureSecret = "fixture-only-formation-fix-secret-000000"

// opsFixtureTarget 是本部署的环境名(GlobalMatch.ManualResolutionEnvironment),不是库地址。
const opsFixtureTarget = "staging-global"

type fakeStrandedStore struct {
	view   *pb.StrandedFormation
	err    error
	shows  []uint64
	aborts []authority.ManualAbortRequest
	lists  int
}

func (f *fakeStrandedStore) ListStrandedFormations(context.Context, authority.ExpiredOutcomeCursor, int) ([]*pb.StrandedFormation, authority.ExpiredOutcomeCursor, uint64, error) {
	f.lists++
	if f.err != nil {
		return nil, authority.ExpiredOutcomeCursor{}, 0, f.err
	}
	return []*pb.StrandedFormation{f.view}, authority.ExpiredOutcomeCursor{AfterRetryMs: 7, AfterBattle: 42}, 9, nil
}

func (f *fakeStrandedStore) ShowStrandedFormation(_ context.Context, battle uint64) (*pb.StrandedFormation, uint64, error) {
	f.shows = append(f.shows, battle)
	return f.view, 9, f.err
}

func (f *fakeStrandedStore) ManualAbortFormation(_ context.Context, req authority.ManualAbortRequest) (*pb.StrandedFormation, uint64, error) {
	f.aborts = append(f.aborts, req)
	return f.view, 9, f.err
}

func (f *fakeStrandedStore) touched() bool { return f.lists+len(f.shows)+len(f.aborts) != 0 }

func newOpsFixture(t *testing.T, store *fakeStrandedStore) *StrandedFormationOps {
	t.Helper()
	ops, err := NewStrandedFormationOps(store, opsFixtureSecret, opsFixtureTarget, "match-0")
	require.NoError(t, err)
	return ops
}

func signedResolve(t *testing.T, in *pb.ResolveStrandedFormationRequest, caller, method string) *pb.ResolveStrandedFormationRequest {
	t.Helper()
	require.NoError(t, serviceauth.Sign(method, in, caller, opsFixtureSecret, uint64(time.Now().UnixMilli())))
	return in
}

// fixtureApproval 是一份合格的双人复核批准记录:发起人就是 abortRequest 的操作人 ops-alice,批准人是另一个人。
func fixtureApproval() *base.AdminApproval {
	return &base.AdminApproval{ApprovalId: "900", Proposer: "ops-alice", Approver: "ops-bob", ApprovedAtMs: 1}
}

func abortRequest(mode pb.StrandedFormationMode) *pb.ResolveStrandedFormationRequest {
	return &pb.ResolveStrandedFormationRequest{BattleId: 42, Mode: mode, ExpectDigest: strings.Repeat("ab", 32), ExpectTarget: opsFixtureTarget, OperatorName: "ops-alice", Reason: "INC-7",
		Approval: fixtureApproval()}
}

// 带会话、入口关闭、调用方不对、签名绑定的方法不对:一律 PermissionDenied,不碰库。
func TestStrandedFormationOpsRejectsCallerBeforeStore(t *testing.T) {
	store := &fakeStrandedStore{}
	ops := newOpsFixture(t, store)
	method := pb.MatchInternal_ResolveStrandedFormation_FullMethodName
	sessionCtx := ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{SessionId: 1, PlayerId: 11})
	for name, c := range map[string]struct {
		ops *StrandedFormationOps
		ctx context.Context
		in  *pb.ResolveStrandedFormationRequest
	}{
		"客户端会话":    {ops, sessionCtx, signedResolve(t, abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW), config.ManualResolutionCaller, method)},
		"入口关闭":     {nil, context.Background(), signedResolve(t, abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW), config.ManualResolutionCaller, method)},
		"观战运营的调用方": {ops, context.Background(), signedResolve(t, abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW), config.SpectateOpsCaller, method)},
		"签给了列表方法":  {ops, context.Background(), signedResolve(t, abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW), config.ManualResolutionCaller, pb.MatchInternal_ListStrandedFormations_FullMethodName)},
		"没有签名":     {ops, context.Background(), abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.ops.ResolveStrandedFormation(c.ctx, c.in)
			require.Equal(t, codes.PermissionDenied, status.Code(err))
		})
	}
	_, err := (*StrandedFormationOps)(nil).ListStrandedFormations(context.Background(), &pb.ListStrandedFormationsRequest{Limit: 1})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.False(t, store.touched(), "拒绝发生在任何读写之前")
}

// DRY_RUN / ABORT 的参数不合规回 InvalidArgument;目标环境不符回 REFUSED/target_mismatch。都不碰库。
func TestStrandedFormationOpsValidatesBeforeStore(t *testing.T) {
	store := &fakeStrandedStore{}
	ops := newOpsFixture(t, store)
	method := pb.MatchInternal_ResolveStrandedFormation_FullMethodName
	for name, edit := range map[string]func(*pb.ResolveStrandedFormationRequest){
		"没有 battle_id": func(in *pb.ResolveStrandedFormationRequest) { in.BattleId = 0 },
		"没有 mode": func(in *pb.ResolveStrandedFormationRequest) {
			in.Mode = pb.StrandedFormationMode_STRANDED_FORMATION_MODE_UNSPECIFIED
		},
		"没有摘要":  func(in *pb.ResolveStrandedFormationRequest) { in.ExpectDigest = "" },
		"没有操作人": func(in *pb.ResolveStrandedFormationRequest) { in.OperatorName = " " },
		"依据含换行": func(in *pb.ResolveStrandedFormationRequest) { in.Reason = "INC-7\n[FormationManual] forged" },
	} {
		t.Run(name, func(t *testing.T) {
			in := abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT)
			edit(in)
			_, err := ops.ResolveStrandedFormation(context.Background(), signedResolve(t, in, config.ManualResolutionCaller, method))
			require.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
	in := abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT)
	in.ExpectTarget = "prod-global"
	out, err := ops.ResolveStrandedFormation(context.Background(), signedResolve(t, in, config.ManualResolutionCaller, method))
	require.NoError(t, err)
	require.Equal(t, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED, out.GetVerdict())
	require.Equal(t, "target_mismatch", out.GetRefusal())
	require.Equal(t, opsFixtureTarget, out.GetTarget(), "回包带上服务端真实目标,便于核对连错了哪里")
	require.Contains(t, out.GetAuditLine(), "executed=false")
	require.False(t, store.touched(), "目标环境不符不碰库")
}

// Store 的结论映射成带内 verdict;DRY_RUN 传 Execute=false、ABORT 传 true;只有 ABORT 且放行才是 executed=true;
// 提交结果未知回 Unavailable(发布准入据此 Hold),绝不报成功。
func TestStrandedFormationOpsMapsStoreOutcome(t *testing.T) {
	method := pb.MatchInternal_ResolveStrandedFormation_FullMethodName
	view := &pb.StrandedFormation{BattleId: 42, State: 3, Digest: strings.Repeat("ab", 32)}
	for name, c := range map[string]struct {
		err     error
		verdict pb.StrandedFormationVerdict
		refusal string
	}{
		"放行":      {nil, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE, ""},
		"前置条件不满足": {&authority.ManualRefusal{Code: "deadline_not_due"}, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED, "deadline_not_due"},
		"成局已变":    {authority.ErrManualChanged, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_CHANGED, ""},
		"已被处理":    {authority.ErrManualAlreadyResolved, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_RESOLVED, ""},
		"没有这一局":   {authority.ErrManualNotFound, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_NOT_FOUND, ""},
		"读库失败":    {errors.New("fixture: TiDB unavailable"), pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_RETRY, ""},
	} {
		t.Run(name, func(t *testing.T) {
			for _, mode := range []pb.StrandedFormationMode{pb.StrandedFormationMode_STRANDED_FORMATION_MODE_DRY_RUN, pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT} {
				store := &fakeStrandedStore{view: view, err: c.err}
				out, err := newOpsFixture(t, store).ResolveStrandedFormation(context.Background(), signedResolve(t, abortRequest(mode), config.ManualResolutionCaller, method))
				require.NoError(t, err)
				require.Equal(t, c.verdict, out.GetVerdict())
				require.Equal(t, c.refusal, out.GetRefusal())
				require.Len(t, store.aborts, 1)
				require.Equal(t, mode == pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT, store.aborts[0].Execute)
				require.Equal(t, "match-0", store.aborts[0].Replica)
				executed := mode == pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT && c.err == nil
				require.Contains(t, out.GetAuditLine(), "executed="+map[bool]string{true: "true", false: "false"}[executed])
				require.Contains(t, out.GetAuditLine(), `operator="ops-alice" battle_id=42`)
			}
		})
	}
	store := &fakeStrandedStore{view: view, err: errors.Join(authority.ErrManualCommitUnknown, errors.New("fixture: commit lost"))}
	_, err := newOpsFixture(t, store).ResolveStrandedFormation(context.Background(), signedResolve(t, abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT), config.ManualResolutionCaller, method))
	require.Equal(t, codes.Unavailable, status.Code(err), "提交结果未知不能回带内结论")

	// SHOW 走锁外只读,不调 ManualAbortFormation,也不要求操作人与摘要。
	store = &fakeStrandedStore{view: view}
	out, err := newOpsFixture(t, store).ResolveStrandedFormation(context.Background(), signedResolve(t, &pb.ResolveStrandedFormationRequest{BattleId: 42, Mode: pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW}, config.ManualResolutionCaller, method))
	require.NoError(t, err)
	require.Equal(t, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE, out.GetVerdict())
	require.Equal(t, []uint64{42}, store.shows)
	require.Empty(t, store.aborts)
	require.Empty(t, out.GetAuditLine())
}

// 操作人与依据是调用方填的自由文本(允许空格、引号):审计行里一律引起来,伪造不出 executed=true 之类的字段。
func TestStrandedFormationOpsAuditLineQuotesFreeText(t *testing.T) {
	method := pb.MatchInternal_ResolveStrandedFormation_FullMethodName
	store := &fakeStrandedStore{view: &pb.StrandedFormation{BattleId: 42}, err: &authority.ManualRefusal{Code: "deadline_not_due"}}
	in := abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_DRY_RUN)
	in.OperatorName = "x executed=true battle_id=1"
	in.Reason = `INC-7 " executed=true refusal=`
	in.Approval = nil // 演练不要求批准记录;这样的操作人也不是合法主体,带着记录会在碰库之前被拒
	require.NoError(t, authority.ValidateManualAudit(in.OperatorName, in.Reason), "前提:两项都是合规输入")
	out, err := newOpsFixture(t, store).ResolveStrandedFormation(context.Background(), signedResolve(t, in, config.ManualResolutionCaller, method))
	require.NoError(t, err)
	line := out.GetAuditLine()
	require.Contains(t, line, `operator="x executed=true battle_id=1" battle_id=42 `)
	require.Contains(t, line, `reason="INC-7 \" executed=true refusal=" executed=false`)
	require.True(t, strings.HasSuffix(line, " executed=false"), "服务端算出的结论在行尾,不被自由文本顶替: %s", line)
	require.Len(t, store.aborts, 1, "演练走到了锁内复核")
}

// ABORT 不带批准记录、发起人不是操作人、发起人自批:带内 REFUSED,不碰库,不算 executed。
// SHOW / DRY_RUN 带了不合格的记录同样拒绝。
func TestStrandedFormationOpsAbortRequiresDualControlApproval(t *testing.T) {
	method := pb.MatchInternal_ResolveStrandedFormation_FullMethodName
	otherProposer := fixtureApproval()
	otherProposer.Proposer = "ops-carol"
	selfApproved := fixtureApproval()
	selfApproved.Approver = selfApproved.Proposer
	for name, c := range map[string]struct {
		mode     pb.StrandedFormationMode
		approval *base.AdminApproval
		refusal  string
	}{
		"ABORT 不带记录":     {pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT, nil, "dual_control_approval_required"},
		"ABORT 发起人不是操作人": {pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT, otherProposer, "dual_control_approval_invalid: "},
		"ABORT 发起人自批":    {pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT, selfApproved, "dual_control_approval_invalid: "},
		"演练带了不合格的记录":     {pb.StrandedFormationMode_STRANDED_FORMATION_MODE_DRY_RUN, otherProposer, "dual_control_approval_invalid: "},
		"SHOW 带了不合格的记录":  {pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW, selfApproved, "dual_control_approval_invalid: "},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeStrandedStore{view: &pb.StrandedFormation{BattleId: 42}}
			in := abortRequest(c.mode)
			in.Approval = c.approval
			before := metrics.AdminUnapprovedCommandValue()
			out, err := newOpsFixture(t, store).ResolveStrandedFormation(context.Background(), signedResolve(t, in, config.ManualResolutionCaller, method))
			require.NoError(t, err)
			require.Equal(t, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED, out.GetVerdict())
			require.True(t, strings.HasPrefix(out.GetRefusal(), c.refusal), out.GetRefusal())
			require.False(t, store.touched(), "批准记录不合格不碰库")
			require.NotContains(t, out.GetAuditLine(), "executed=true")
			require.Equal(t, before, metrics.AdminUnapprovedCommandValue(), "拒绝不计入不带记录被接受")
		})
	}
}

// 合格的批准记录放行;批准号与批准人交给 Store 写进 match_manual_resolution,也进审计行。
func TestStrandedFormationOpsAbortWithApprovalPassesItToTheStore(t *testing.T) {
	method := pb.MatchInternal_ResolveStrandedFormation_FullMethodName
	store := &fakeStrandedStore{view: &pb.StrandedFormation{BattleId: 42}}
	out, err := newOpsFixture(t, store).ResolveStrandedFormation(context.Background(), signedResolve(t, abortRequest(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT), config.ManualResolutionCaller, method))
	require.NoError(t, err)
	require.Equal(t, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE, out.GetVerdict())
	require.Len(t, store.aborts, 1)
	require.True(t, store.aborts[0].Execute)
	require.Equal(t, "900", store.aborts[0].ApprovalID)
	require.Equal(t, "ops-bob", store.aborts[0].Approver)
	require.Contains(t, out.GetAuditLine(), `approval_id="900" approver="ops-bob" reason="INC-7" executed=true`)
}

// SHOW / DRY_RUN 不要求批准记录;只有开关打开才接受不带记录的 ABORT,每次计数,批准号留空。
func TestStrandedFormationOpsUnapprovedAbortOnlyWhenExplicitlyAllowed(t *testing.T) {
	method := pb.MatchInternal_ResolveStrandedFormation_FullMethodName
	withoutApproval := func(mode pb.StrandedFormationMode) *pb.ResolveStrandedFormationRequest {
		in := abortRequest(mode)
		in.Approval = nil
		return signedResolve(t, in, config.ManualResolutionCaller, method)
	}
	before := metrics.AdminUnapprovedCommandValue()
	store := &fakeStrandedStore{view: &pb.StrandedFormation{BattleId: 42}}
	ops := newOpsFixture(t, store)
	out, err := ops.ResolveStrandedFormation(context.Background(), withoutApproval(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW))
	require.NoError(t, err)
	require.Equal(t, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE, out.GetVerdict())
	out, err = ops.ResolveStrandedFormation(context.Background(), withoutApproval(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_DRY_RUN))
	require.NoError(t, err)
	require.Equal(t, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE, out.GetVerdict())
	require.Equal(t, []uint64{42}, store.shows)
	require.Len(t, store.aborts, 1)
	require.False(t, store.aborts[0].Execute)
	require.Equal(t, before, metrics.AdminUnapprovedCommandValue(), "SHOW / DRY_RUN 不计数")

	ops.allowUnapproved = true
	out, err = ops.ResolveStrandedFormation(context.Background(), withoutApproval(pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT))
	require.NoError(t, err)
	require.Equal(t, pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE, out.GetVerdict())
	require.Len(t, store.aborts, 2)
	require.True(t, store.aborts[1].Execute)
	require.Empty(t, store.aborts[1].ApprovalID)
	require.Contains(t, out.GetAuditLine(), `approval_id="" approver=""`)
	require.Equal(t, before+1, metrics.AdminUnapprovedCommandValue(), "不带记录被接受的 ABORT 计一次")
}

// 配置项缺省就是要求批准记录。
func TestStrandedFormationOpsFromConfigRequiresApprovalByDefault(t *testing.T) {
	store := &authority.Store{}
	getenv := func(string) string { return opsFixtureSecret }
	c := config.GlobalMatchConf{ManualResolutionSecretEnv: "FIXTURE_FORMATION_FIX", ManualResolutionEnvironment: opsFixtureTarget}
	ops, err := NewStrandedFormationOpsFromConfig(store, c, getenv)
	require.NoError(t, err)
	require.False(t, ops.allowUnapproved)
	c.ManualResolutionAllowUnapproved = true
	ops, err = NewStrandedFormationOpsFromConfig(store, c, getenv)
	require.NoError(t, err)
	require.True(t, ops.allowUnapproved)
}

// 列表:条数越界 InvalidArgument;读失败带内 retry,不回 gRPC 错误(只读查询没有结局未知)。
func TestStrandedFormationOpsList(t *testing.T) {
	method := pb.MatchInternal_ListStrandedFormations_FullMethodName
	signed := func(limit uint32) *pb.ListStrandedFormationsRequest {
		in := &pb.ListStrandedFormationsRequest{Limit: limit}
		require.NoError(t, serviceauth.Sign(method, in, config.ManualResolutionCaller, opsFixtureSecret, uint64(time.Now().UnixMilli())))
		return in
	}
	store := &fakeStrandedStore{view: &pb.StrandedFormation{BattleId: 42}}
	ops := newOpsFixture(t, store)
	_, err := ops.ListStrandedFormations(context.Background(), signed(0))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = ops.ListStrandedFormations(context.Background(), signed(authority.StrandedFormationPageMax+1))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	out, err := ops.ListStrandedFormations(context.Background(), signed(10))
	require.NoError(t, err)
	require.Len(t, out.GetFormations(), 1)
	require.EqualValues(t, 7, out.GetNextRetryAtMs())
	require.EqualValues(t, 42, out.GetNextBattleId())
	require.Equal(t, opsFixtureTarget, out.GetTarget())

	store.err = errors.New("fixture: TiDB unavailable")
	out, err = ops.ListStrandedFormations(context.Background(), signed(10))
	require.NoError(t, err)
	require.True(t, out.GetRetry())
}

// 入口缺省关闭:没配环境变量名回 nil, nil;配了却读不到有效密钥、或没写有效的环境名,回错误(同样关闭)。
// 密钥只取指名的那个变量;target 是环境名,不是库地址(各环境的 Host/Database 常常一样,拦不住连错环境)。
func TestStrandedFormationOpsFromConfigDisabledByDefault(t *testing.T) {
	store := &authority.Store{}
	ops, err := NewStrandedFormationOpsFromConfig(store, config.GlobalMatchConf{Host: "tidb:4000", Database: "mmorpg_match", ManualResolutionEnvironment: opsFixtureTarget}, func(string) string { return opsFixtureSecret })
	require.NoError(t, err)
	require.Nil(t, ops, "没配 ManualResolutionSecretEnv 就关闭")

	secret := func(name string) string {
		if name == "FIXTURE_FORMATION_FIX" {
			return opsFixtureSecret
		}
		return ""
	}
	c := config.GlobalMatchConf{Host: "tidb:4000", Database: "mmorpg_match", ManualResolutionSecretEnv: "FIXTURE_FORMATION_FIX", ManualResolutionEnvironment: opsFixtureTarget}
	_, err = NewStrandedFormationOpsFromConfig(store, c, func(string) string { return "too-short" })
	require.Error(t, err)
	for _, environment := range []string{"", "staging global", "tidb:4000/mmorpg_match", strings.Repeat("e", 65)} {
		bad := c
		bad.ManualResolutionEnvironment = environment
		_, err = NewStrandedFormationOpsFromConfig(store, bad, secret)
		require.Error(t, err, "环境名缺失或不合规就关闭: %q", environment)
	}
	ops, err = NewStrandedFormationOpsFromConfig(store, c, secret)
	require.NoError(t, err)
	require.NotNil(t, ops)
	require.Equal(t, opsFixtureTarget, ops.target)
}
