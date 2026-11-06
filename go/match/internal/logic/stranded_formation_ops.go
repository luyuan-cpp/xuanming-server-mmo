package logic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"match/internal/authority"
	"match/internal/config"
	"match/internal/pkg/ctxkeys"
	pb "proto/match"
	"shared/serviceauth"
)

// 永久 UNKNOWN 全球成局的人工处置入口(MatchInternal.ListStrandedFormations / ResolveStrandedFormation,2026-11-05,
// 手册 docs/ops/match-formation-manual-abort-runbook.md)。放行判据、锁序与写入都在 authority/manual_abort.go,这里只做:
//   - 调用方准入:带会话一律 PermissionDenied;入口没开(GlobalMatch.ManualResolutionSecretEnv 没配或密钥无效)一律 PermissionDenied;
//     只认 serviceauth 调用方 config.ManualResolutionCaller,签名绑定方法与整个请求;
//   - DRY_RUN / ABORT 的参数校验与目标环境核对(expect_target 必须与本部署的环境名 GlobalMatch.ManualResolutionEnvironment 逐字相等);
//   - 把 Store 的结论映射成带内 verdict,写审计日志行 [FormationManual]。
//
// 为什么写操作不让工具直连 match 库:匹配库只归 match 服务(AGENTS.md §7.6),写资格(authoritysql 写者 boot grant)一个进程只能
// 消费一次,提交点还要角色护栏与目录装配。交给运行中的副本执行,自然沿用本库写者闸门、冻结与护栏,与自动恢复器同一进程模型。
//
// 错误语义(发布准入 serverbase.PublicationAdmission 据此判结局是否已知):确定的拒绝与没写成的失败都走带内 verdict 或
// PermissionDenied / InvalidArgument;只有 ABORT 提交结果未知回 Unavailable,让本副本的排空证明 Hold,运维先 SHOW 重查。

// strandedFormationStore 是人工处置入口对成局权威的全部依赖,生产实现是 *authority.Store。收窄成接口只为测试隔离:
// 准入、参数校验、目标核对与结论映射不起 TiDB 也能确定性复现。
type strandedFormationStore interface {
	ListStrandedFormations(ctx context.Context, after authority.ExpiredOutcomeCursor, limit int) ([]*pb.StrandedFormation, authority.ExpiredOutcomeCursor, uint64, error)
	ShowStrandedFormation(ctx context.Context, battle uint64) (*pb.StrandedFormation, uint64, error)
	ManualAbortFormation(ctx context.Context, req authority.ManualAbortRequest) (*pb.StrandedFormation, uint64, error)
}

// StrandedFormationOps 启动时装配一次,之后只读,可并发调用。nil 表示入口关闭。
type StrandedFormationOps struct {
	store    strandedFormationStore
	verifier *serviceauth.Verifier
	target   string // 本部署的环境名:GlobalMatch.ManualResolutionEnvironment
	replica  string // 本副本主机名,只进审计
}

// manualResolutionEnvironmentRe 是环境名的字符集:进审计行与工具输出不用转义,也不会被读成别的字段。
var manualResolutionEnvironmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ManualResolutionTarget 是 SHOW / list 回给运维、DRY_RUN / ABORT 要求逐字相等的目标环境标识:本部署的环境名
// GlobalMatch.ManualResolutionEnvironment(2026-11-05 评审补)。原先取 Host/Database,可库名固定是 mmorpg_match,各环境的 Host
// 又常是同一个 Service DNS(按映射拨号时 Host 还不是实际集群),拼出来的串在不同环境里一模一样,拦不住连错环境。
// 运维按工单上写明的环境名填 -expect-target,不从 show 的输出照抄。缺了或不合规回错误,入口随之关闭(fail-closed)。
func ManualResolutionTarget(c config.GlobalMatchConf) (string, error) {
	if !manualResolutionEnvironmentRe.MatchString(c.ManualResolutionEnvironment) {
		return "", errors.New("match: 人工处置入口缺少有效的 GlobalMatch.ManualResolutionEnvironment(本部署的环境名,1~64 个 ASCII 字母、数字或 . _ -)")
	}
	return c.ManualResolutionEnvironment, nil
}

// NewStrandedFormationOps 装配人工处置入口。密钥缺失或不足 32 字节返回错误,调用方据此保持未装配(入口关闭,fail-closed)。
func NewStrandedFormationOps(store strandedFormationStore, secret, target, replica string) (*StrandedFormationOps, error) {
	if store == nil || target == "" {
		return nil, errors.New("match: 人工处置入口缺少成局权威或目标标识")
	}
	verifier, err := serviceauth.New(map[string]string{config.ManualResolutionCaller: secret})
	if err != nil {
		return nil, errors.New("match: 人工处置调用方密钥缺失或不足32字节")
	}
	if replica == "" {
		replica = "unknown"
	}
	return &StrandedFormationOps{store: store, verifier: verifier, target: target, replica: replica}, nil
}

// NewStrandedFormationOpsFromConfig 按 GlobalMatch.ManualResolutionSecretEnv 装配。没配环境变量名 = 入口关闭,回 nil, nil;
// 配了却读不到有效密钥、或没写有效的环境名 ManualResolutionEnvironment,回错误(调用方打 ERROR,入口同样关闭)。
func NewStrandedFormationOpsFromConfig(store *authority.Store, c config.GlobalMatchConf, getenv func(string) string) (*StrandedFormationOps, error) {
	name := strings.TrimSpace(c.ManualResolutionSecretEnv)
	if name == "" || store == nil {
		return nil, nil
	}
	target, err := ManualResolutionTarget(c)
	if err != nil {
		return nil, err
	}
	replica, _ := os.Hostname()
	return NewStrandedFormationOps(store, getenv(name), target, replica)
}

// verify 是两个方法共用的调用方准入,拒绝发生在任何读写之前:带会话(客户端来源)、入口关闭(o 为 nil)、验签失败、
// 调用方不是 ops-formation 一律 PermissionDenied(确定的拒绝,不拖住排空证明)。
func (o *StrandedFormationOps) verify(ctx context.Context, method string, in proto.Message) error {
	if details, client := ctxkeys.GetSessionDetails(ctx); client && details != nil {
		return status.Error(codes.PermissionDenied, "客户端不能调用人工处置入口")
	}
	if o == nil || o.verifier == nil {
		logx.WithContext(ctx).Errorf("[FormationManual] Rejecting %s: entry is disabled (GlobalMatch.ManualResolutionSecretEnv not configured or its secret is invalid)", method)
		return status.Error(codes.PermissionDenied, "人工处置入口未开启")
	}
	caller, err := o.verifier.Verify(ctx, method, in)
	if err != nil || caller != config.ManualResolutionCaller {
		logx.WithContext(ctx).Errorf("[FormationManual] Rejecting %s: caller proof is invalid", method)
		return status.Error(codes.PermissionDenied, "人工处置调用方证明无效")
	}
	return nil
}

// ListStrandedFormations 只读列一页。读失败带内 retry = true:只读查询没有结局未知。
func (o *StrandedFormationOps) ListStrandedFormations(ctx context.Context, in *pb.ListStrandedFormationsRequest) (*pb.ListStrandedFormationsResponse, error) {
	if err := o.verify(ctx, pb.MatchInternal_ListStrandedFormations_FullMethodName, in); err != nil {
		return nil, err
	}
	if in.GetLimit() < 1 || in.GetLimit() > authority.StrandedFormationPageMax {
		return nil, status.Errorf(codes.InvalidArgument, "limit 必须在 1..%d", authority.StrandedFormationPageMax)
	}
	formations, next, stamp, err := o.store.ListStrandedFormations(ctx, authority.ExpiredOutcomeCursor{AfterRetryMs: in.GetAfterRetryAtMs(), AfterBattle: in.GetAfterBattleId()}, int(in.GetLimit()))
	if err != nil {
		logx.WithContext(ctx).Errorf("[FormationManual] Listing stranded formations failed: %v", err)
		return &pb.ListStrandedFormationsResponse{Retry: true, Target: o.target}, nil
	}
	return &pb.ListStrandedFormationsResponse{Formations: formations, NextRetryAtMs: next.AfterRetryMs, NextBattleId: next.AfterBattle, NowMs: stamp, Target: o.target}, nil
}

// ResolveStrandedFormation 按 mode 做锁外 SHOW、锁内 DRY_RUN 或 ABORT。
func (o *StrandedFormationOps) ResolveStrandedFormation(ctx context.Context, in *pb.ResolveStrandedFormationRequest) (*pb.ResolveStrandedFormationResponse, error) {
	if err := o.verify(ctx, pb.MatchInternal_ResolveStrandedFormation_FullMethodName, in); err != nil {
		return nil, err
	}
	if in.GetBattleId() == 0 {
		return nil, status.Error(codes.InvalidArgument, "缺少 battle_id")
	}
	out := &pb.ResolveStrandedFormationResponse{Target: o.target, Replica: o.replica}
	switch in.GetMode() {
	case pb.StrandedFormationMode_STRANDED_FORMATION_MODE_SHOW:
		formation, stamp, err := o.store.ShowStrandedFormation(ctx, in.GetBattleId())
		return o.reply(ctx, in.GetBattleId(), out, formation, stamp, err), nil
	case pb.StrandedFormationMode_STRANDED_FORMATION_MODE_DRY_RUN, pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT:
		return o.manualAbort(ctx, in, out)
	default:
		return nil, status.Error(codes.InvalidArgument, "mode 必须是 SHOW、DRY_RUN 或 ABORT")
	}
}

// manualAbort 是 DRY_RUN / ABORT:校验参数、核对目标环境,再交给 Store 在锁内复核(DRY_RUN 回滚、ABORT 提交)。
func (o *StrandedFormationOps) manualAbort(ctx context.Context, in *pb.ResolveStrandedFormationRequest, out *pb.ResolveStrandedFormationResponse) (*pb.ResolveStrandedFormationResponse, error) {
	if in.GetExpectDigest() == "" {
		return nil, status.Error(codes.InvalidArgument, "缺少 expect_digest:先 SHOW")
	}
	if err := authority.ValidateManualAudit(in.GetOperatorName(), in.GetReason()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if in.GetExpectTarget() != o.target {
		out.Verdict, out.Refusal = pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED, "target_mismatch"
		o.audit(ctx, in, out)
		return out, nil
	}
	formation, stamp, err := o.store.ManualAbortFormation(ctx, authority.ManualAbortRequest{
		BattleID: in.GetBattleId(), ExpectDigest: in.GetExpectDigest(), Operator: in.GetOperatorName(), Reason: in.GetReason(),
		Replica: o.replica, Execute: in.GetMode() == pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT,
	})
	if errors.Is(err, authority.ErrManualCommitUnknown) {
		// 结局未知:回 Unavailable,发布准入据此 Hold 本副本的排空证明;绝不报成功。
		logx.WithContext(ctx).Errorf("[FormationManual] ABORT commit outcome unknown battle_id=%d operator=%q: %v; rerun show before anything else", in.GetBattleId(), in.GetOperatorName(), err)
		return nil, status.Error(codes.Unavailable, "ABORT 提交结果未知:先 show 重查,看到 source=MANUAL_ABORT 的终局即已完成")
	}
	o.reply(ctx, in.GetBattleId(), out, formation, stamp, err)
	o.audit(ctx, in, out)
	return out, nil
}

// reply 把 Store 的结论映射成带内 verdict,填进 out 并返回它。
func (o *StrandedFormationOps) reply(ctx context.Context, battle uint64, out *pb.ResolveStrandedFormationResponse, formation *pb.StrandedFormation, stamp uint64, err error) *pb.ResolveStrandedFormationResponse {
	out.Formation, out.NowMs = formation, stamp
	switch {
	case err == nil:
		out.Verdict = pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE
	case errors.Is(err, authority.ErrManualNotFound):
		out.Verdict = pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_NOT_FOUND
	case errors.Is(err, authority.ErrManualNotAllowed):
		out.Verdict, out.Refusal = pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_REFUSED, authority.ManualRefusalCode(err)
	case errors.Is(err, authority.ErrManualChanged):
		out.Verdict = pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_CHANGED
	case errors.Is(err, authority.ErrManualAlreadyResolved):
		out.Verdict = pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_RESOLVED
	default:
		// 读库失败,或写入在提交之前失败:事务已回滚,没有改动,结局已知。
		logx.WithContext(ctx).Errorf("[FormationManual] Stranded formation check failed battle_id=%d: %v", battle, err)
		out.Verdict = pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_RETRY
	}
	return out
}

// audit 写审计日志行并放进回包,工具原样打印、贴进工单。真正写了 ABORT 打 ERROR(确保进日志检索),其余打 INFO。
// battle_id 只进正文,不做指标标签。操作人与依据已由 ValidateManualAudit 排除控制字符;两者都是调用方填的自由文本、允许空格,
// 一律 %q 引起来(2026-11-05 评审补):否则操作人填成 `x executed=true battle_id=1` 就能在 INFO 行里伪造出字段,
// 按关键字检索日志或贴工单的人会被误导。其余字段都来自服务端。
func (o *StrandedFormationOps) audit(ctx context.Context, in *pb.ResolveStrandedFormationRequest, out *pb.ResolveStrandedFormationResponse) {
	f := out.GetFormation()
	executed := in.GetMode() == pb.StrandedFormationMode_STRANDED_FORMATION_MODE_ABORT && out.GetVerdict() == pb.StrandedFormationVerdict_STRANDED_FORMATION_VERDICT_ELIGIBLE
	out.AuditLine = fmt.Sprintf("[FormationManual] operator=%q battle_id=%d mode=%s verdict=%s refusal=%s class=%s failed_check=%s state_before=%d digest_before=%s effective_deadline_ms=%d now_ms=%d target=%s replica=%s reason=%q executed=%t",
		in.GetOperatorName(), in.GetBattleId(), in.GetMode(), out.GetVerdict(), out.GetRefusal(), f.GetFormationClass(), f.GetFailedCheck(),
		f.GetState(), f.GetDigest(), f.GetEffectiveDeadlineMs(), out.GetNowMs(), out.GetTarget(), out.GetReplica(), in.GetReason(), executed)
	if executed {
		logx.WithContext(ctx).Error(out.AuditLine)
		return
	}
	logx.WithContext(ctx).Info(out.AuditLine)
}
