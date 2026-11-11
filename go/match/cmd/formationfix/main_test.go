package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"match/internal/config"
	pb "proto/match"
	"shared/serviceauth"
)

// formationfix 的参数解析、签名与退出码(docs/ops/match-formation-manual-abort-runbook.md §4)。不起 match:
// 客户端换成假实现,记下发出去的请求;签名用服务端同一套 serviceauth 校验。

const fixtureSecret = "fixture-only-formation-fix-secret-000000"

var fixtureDigest = strings.Repeat("ab", 32)

// fixtureTarget 是目标环境名(match 的 GlobalMatch.ManualResolutionEnvironment)。
const fixtureTarget = "staging-global"

type fakeClient struct {
	list     *pb.ListStrandedFormationsResponse
	resolve  *pb.ResolveStrandedFormationResponse
	err      error
	lists    []*pb.ListStrandedFormationsRequest
	resolves []*pb.ResolveStrandedFormationRequest
}

func (f *fakeClient) ListStrandedFormations(_ context.Context, in *pb.ListStrandedFormationsRequest, _ ...grpc.CallOption) (*pb.ListStrandedFormationsResponse, error) {
	f.lists = append(f.lists, in)
	return f.list, f.err
}

func (f *fakeClient) ResolveStrandedFormation(_ context.Context, in *pb.ResolveStrandedFormationRequest, _ ...grpc.CallOption) (*pb.ResolveStrandedFormationResponse, error) {
	f.resolves = append(f.resolves, in)
	return f.resolve, f.err
}

type harness struct {
	client *fakeClient
	dials  int
	env    map[string]string
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newHarness() *harness {
	return &harness{client: &fakeClient{}, env: map[string]string{defaultSecretEnv: fixtureSecret}}
}

func (h *harness) run(args ...string) int {
	return run(args, &h.stdout, &h.stderr, deps{
		dial: func(string) (opsClient, func(), error) {
			h.dials++
			return h.client, func() {}, nil
		},
		getenv: func(name string) string { return h.env[name] },
		now:    time.Now, // 服务端验签窗口是 5 分钟,用例在窗口内跑完
	})
}

func abortArgs(extra ...string) []string {
	args := []string{"-endpoint", "127.0.0.1:1", "abort", "-battle", "42", "-expect-digest", fixtureDigest, "-expect-target", fixtureTarget,
		"-operator", "ops-alice", "-reason", "INC-7 original_mismatch confirmed", "-confirm", "42"}
	return append(args, extra...)
}

// 参数写错一律回 2,而且不签名、不拨号:不该先去碰服务。
func TestFormationfixArgumentsRejectedBeforeAnyCall(t *testing.T) {
	replace := func(flag, value string) []string {
		args := abortArgs()
		for i := range args {
			if args[i] == flag {
				args[i+1] = value
			}
		}
		return args
	}
	for name, args := range map[string][]string{
		"缺 endpoint":        {"show", "-battle", "42"},
		"未知子命令":              {"-endpoint", "127.0.0.1:1", "purge"},
		"show 缺 battle":      {"-endpoint", "127.0.0.1:1", "show"},
		"list 条数越界":          {"-endpoint", "127.0.0.1:1", "list", "-limit", "101"},
		"abort 缺操作人":         replace("-operator", ""),
		"abort 操作人含控制字符":     replace("-operator", "ops\nalice"),
		"abort 缺依据":          replace("-reason", "  "),
		"abort 依据超长":         replace("-reason", strings.Repeat("x", 192)),
		"abort 摘要不是 64 位小写": replace("-expect-digest", strings.ToUpper(fixtureDigest)),
		"abort 缺目标":          replace("-expect-target", ""),
		"abort 二次确认不符":       replace("-confirm", "43"),
		"abort 多余位置参数":       abortArgs("extra"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			require.Equal(t, exitRejected, h.run(args...))
			require.Zero(t, h.dials, "参数不合法不拨号")
		})
	}
}

// 密钥只从环境变量读:缺失回 2、不拨号;报错只报变量名。签出的请求能被服务端同样的校验放行,且绑定方法与正文。
func TestFormationfixSignsFromEnvironmentOnly(t *testing.T) {
	h := newHarness()
	h.env = map[string]string{}
	require.Equal(t, exitRejected, h.run(abortArgs()...))
	require.Zero(t, h.dials)
	require.Contains(t, h.stderr.String(), defaultSecretEnv)

	h = newHarness()
	h.client.resolve = &pb.ResolveStrandedFormationResponse{Verdict: pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE, AuditLine: "[FormationManual] fixture"}
	require.Equal(t, exitOK, h.run(abortArgs()...))
	require.Len(t, h.client.resolves, 1)
	sent := h.client.resolves[0]
	verifier, err := serviceauth.New(map[string]string{config.ManualResolutionCaller: fixtureSecret})
	require.NoError(t, err)
	caller, err := verifier.Verify(context.Background(), pb.MatchInternal_ResolveStrandedFormation_FullMethodName, sent)
	require.NoError(t, err)
	require.Equal(t, config.ManualResolutionCaller, caller)
	_, err = verifier.Verify(context.Background(), pb.MatchInternal_ListStrandedFormations_FullMethodName, sent)
	require.Error(t, err, "签名绑定方法")
	sent.Mode = pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT
	_, err = verifier.Verify(context.Background(), pb.MatchInternal_ResolveStrandedFormation_FullMethodName, sent)
	require.Error(t, err, "签名绑定正文:演练的签名不能改成执行")
	require.NotContains(t, h.stdout.String()+h.stderr.String(), fixtureSecret, "密钥不进输出")

	// -secret-env 改变量名。
	h = newHarness()
	h.env = map[string]string{"OTHER_SECRET": fixtureSecret}
	h.client.resolve = &pb.ResolveStrandedFormationResponse{Verdict: pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE}
	require.Equal(t, exitOK, h.run(append([]string{"-secret-env", "OTHER_SECRET"}, abortArgs()...)...))
}

// abort 不带 -execute 发 DRY_RUN、打印「Plan only」;带 -execute 才发 ABORT。服务端的审计行原样打到 stdout。
func TestFormationfixAbortDryRunByDefault(t *testing.T) {
	h := newHarness()
	h.client.resolve = &pb.ResolveStrandedFormationResponse{Verdict: pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE, AuditLine: "[FormationManual] operator=ops-alice executed=false"}
	require.Equal(t, exitOK, h.run(abortArgs()...))
	require.Equal(t, pb.StrandedFormationMode_STRANDED_FORMATION_MODE_DRY_RUN, h.client.resolves[0].GetMode())
	require.Equal(t, fixtureDigest, h.client.resolves[0].GetExpectDigest())
	require.Equal(t, fixtureTarget, h.client.resolves[0].GetExpectTarget())
	require.Contains(t, h.stdout.String(), "[FormationManual] operator=ops-alice executed=false")
	require.Contains(t, h.stdout.String(), "Plan only")

	h = newHarness()
	h.client.resolve = &pb.ResolveStrandedFormationResponse{Verdict: pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE, AuditLine: "[FormationManual] executed=true"}
	require.Equal(t, exitOK, h.run(abortArgs("-execute")...))
	require.Equal(t, pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT, h.client.resolves[0].GetMode())
	require.NotContains(t, h.stdout.String(), "Plan only")
	require.Contains(t, h.stdout.String(), "aborted")
}

// 退出码:成功 0;拒绝 / 没有这一局 / 入口未开 2;已处理或成局已变 3;读库失败、传输失败、提交结果未知 1。
func TestFormationfixAbortExitCodes(t *testing.T) {
	verdict := func(v pb.StrandedFormationVerdict) *pb.ResolveStrandedFormationResponse {
		return &pb.ResolveStrandedFormationResponse{Verdict: v, Refusal: "deadline_not_due"}
	}
	for name, c := range map[string]struct {
		resp *pb.ResolveStrandedFormationResponse
		err  error
		want int
	}{
		"前置条件不满足": {resp: verdict(pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED), want: exitRejected},
		"没有这一局":   {resp: verdict(pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_NOT_FOUND), want: exitRejected},
		"成局已变":    {resp: verdict(pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_CHANGED), want: exitUnchanged},
		"已被处理":    {resp: verdict(pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_RESOLVED), want: exitUnchanged},
		"读库失败":    {resp: verdict(pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_RETRY), want: exitError},
		"提交结果未知":  {err: status.Error(codes.Unavailable, "ABORT 提交结果未知"), want: exitError},
		"传输失败":    {err: status.Error(codes.DeadlineExceeded, "timeout"), want: exitError},
		"入口未开":    {err: status.Error(codes.PermissionDenied, "人工处置入口未开启"), want: exitRejected},
		"服务端参数拒绝": {err: status.Error(codes.InvalidArgument, "bad"), want: exitRejected},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			h.client.resolve, h.client.err = c.resp, c.err
			require.Equal(t, c.want, h.run(abortArgs("-execute")...))
		})
	}
	h := newHarness()
	h.client.resolve = verdict(pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED)
	h.run(abortArgs()...)
	require.Contains(t, h.stderr.String(), "deadline_not_due", "拒绝要打出原因码")
}

// list 打印目标环境、分类与下一页游标;show 打印摘要与「actionable」一行,NOT_FOUND 回 2、RETRY 回 1。
func TestFormationfixListAndShow(t *testing.T) {
	h := newHarness()
	h.client.list = &pb.ListStrandedFormationsResponse{Target: fixtureTarget, NowMs: 9, NextRetryAtMs: 7, NextBattleId: 42,
		Formations: []*pb.StrandedFormation{{BattleId: 42, State: 3, FormationClass: pb.StrandedFormationClass_STRANDED_FORMATION_CLASS_ORIGINAL_MISMATCH, FailedCheck: "created_before_formation"}}}
	require.Equal(t, exitOK, h.run("-endpoint", "127.0.0.1:1", "list", "-limit", "5"))
	require.EqualValues(t, 5, h.client.lists[0].GetLimit())
	out := h.stdout.String()
	require.Contains(t, out, "target="+fixtureTarget)
	require.Contains(t, out, "original_mismatch")
	require.Contains(t, out, "created_before_formation")
	require.Contains(t, out, "list -after-retry 7 -after-battle 42")

	h = newHarness()
	h.client.list = &pb.ListStrandedFormationsResponse{Retry: true}
	require.Equal(t, exitError, h.run("-endpoint", "127.0.0.1:1", "list"))

	h = newHarness()
	h.client.resolve = &pb.ResolveStrandedFormationResponse{Verdict: pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED, Target: fixtureTarget,
		Formation: &pb.StrandedFormation{BattleId: 42, Digest: fixtureDigest, Refusal: "auto_recoverable"}}
	require.Equal(t, exitOK, h.run("-endpoint", "127.0.0.1:1", "show", "-battle", "42"))
	require.Equal(t, pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW, h.client.resolves[0].GetMode())
	require.Contains(t, h.stdout.String(), "digest="+fixtureDigest)
	require.Contains(t, h.stdout.String(), "actionable: no (auto_recoverable)")

	for v, want := range map[pb.StrandedFormationVerdict]int{
		pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_NOT_FOUND: exitRejected,
		pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_RETRY:     exitError,
	} {
		h = newHarness()
		h.client.resolve = &pb.ResolveStrandedFormationResponse{Verdict: v}
		require.Equal(t, want, h.run("-endpoint", "127.0.0.1:1", "show", "-battle", "42"), v.String())
	}

	// 拒绝却没有核对清单(成局行本身解不开):回 2、报原因码,不打印一份全是 0 的清单,更不能显示「actionable: yes」。
	h = newHarness()
	h.client.resolve = &pb.ResolveStrandedFormationResponse{Verdict: pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED, Target: fixtureTarget, Refusal: "formation_unreadable"}
	require.Equal(t, exitRejected, h.run("-endpoint", "127.0.0.1:1", "show", "-battle", "42"))
	require.Contains(t, h.stderr.String(), "formation_unreadable")
	require.NotContains(t, h.stdout.String(), "actionable:")
}
