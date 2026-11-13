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

	tradepb "proto/trade"
	"shared/authoritysql"
	"shared/roleidentity"
	"shared/serviceauth"
	"trade/internal/commerce"
	"trade/internal/config"
)

// 转人工合同的人工处置入口(TradeAdmin.ListManualContracts / ResolveManualContract,2026-11-11,
// 设计 docs/design/jubaozhai-market.md §15.7、§15.7.1,手册 docs/ops/trade-commerce-quarantine-runbook.md §4.2)。
// 判定、锁序与写入都在 commerce 包(manual.go、manual_plan.go),这里只做:
//   - 调用方准入:带客户端会话一律 PermissionDenied(会话拦截器已挡,这里再挡一层);入口没开一律 PermissionDenied;
//     只认 serviceauth 调用方 config.ManualResolutionCaller,签名绑定方法与整个请求,时间戳窗口 5 分钟;
//   - DRY_RUN / EXECUTE 核对目标环境(expect_target 与本部署的环境名逐字相等);
//   - 把 commerce 的结论映射成带内 verdict,写审计日志行 [CommerceManual]。
//
// 为什么工具不直连交易库:写者身份、角色护栏的权威读与本库写者闸门都在服务进程里;交给运行中的副本执行,
// 人工处置与引擎同一进程模型,时间也用引擎的时钟(Due 按它取到期行)。与 match 的 formationfix 入口同一做法。
//
// 错误语义:确定的拒绝与没写成的失败都走带内 verdict 或 PermissionDenied / InvalidArgument;只有 EXECUTE 提交结果未知
// 回 Unavailable,运维先 SHOW 重查,看到这一条留痕就是已完成。

// manualContractStore 是入口对合同权威的全部依赖,生产实现是 engineManualStore。收窄成接口只为测试隔离:
// 准入、目标核对与结论映射不起数据库也能确定性复现。
type manualContractStore interface {
	List(ctx context.Context, afterUpdatedMs, afterID uint64, limit int) ([]*tradepb.TradeCommerceRecord, error)
	Show(ctx context.Context, id uint64) (commerce.ManualReport, error)
	Resolve(ctx context.Context, req commerce.ManualRequest, execute bool) (commerce.ManualResult, error)
}

// engineManualStore 是生产实现:引擎的交易库与角色护栏,时间用引擎的时钟。
type engineManualStore struct{ engine *commerce.Engine }

func (s engineManualStore) deps() commerce.ManualDeps {
	return commerce.ManualDeps{Repo: s.engine.Repo, Roles: s.engine.Roles}
}

func (s engineManualStore) nowMs() uint64 { return uint64(s.engine.Now().UnixMilli()) }

func (s engineManualStore) List(ctx context.Context, afterUpdatedMs, afterID uint64, limit int) ([]*tradepb.TradeCommerceRecord, error) {
	return s.engine.Repo.ListManual(ctx, afterUpdatedMs, afterID, limit)
}

func (s engineManualStore) Show(ctx context.Context, id uint64) (commerce.ManualReport, error) {
	return commerce.ShowManual(ctx, s.deps(), id, s.nowMs())
}

func (s engineManualStore) Resolve(ctx context.Context, req commerce.ManualRequest, execute bool) (commerce.ManualResult, error) {
	return commerce.ResolveManual(ctx, s.deps(), req, s.nowMs(), execute)
}

// ManualContractOps 启动时装配一次,之后只读,可并发调用。nil 表示入口关闭。
type ManualContractOps struct {
	store    manualContractStore
	verifier *serviceauth.Verifier
	target   string // 本部署的环境名:Commerce.ManualResolutionEnvironment
	replica  string // 本副本主机名,只进审计行
}

// manualResolutionEnvironmentRe 是环境名的字符集:进审计行与工具输出不用转义,也不会被读成别的字段。
var manualResolutionEnvironmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// NewManualContractOps 装配入口。密钥缺失或不足 32 字节、环境名不合规回错误,调用方据此保持未装配(入口关闭,fail-closed)。
func NewManualContractOps(store manualContractStore, secret, target, replica string) (*ManualContractOps, error) {
	if store == nil {
		return nil, errors.New("trade: manual resolution entry has no contract store")
	}
	if !manualResolutionEnvironmentRe.MatchString(target) {
		return nil, errors.New("trade: Commerce.ManualResolutionEnvironment must be 1-64 ASCII letters, digits, '.', '_' or '-'")
	}
	verifier, err := serviceauth.New(map[string]string{config.ManualResolutionCaller: secret})
	if err != nil {
		return nil, errors.New("trade: manual resolution caller secret is missing or shorter than 32 bytes")
	}
	if replica == "" {
		replica = "unknown"
	}
	return &ManualContractOps{store: store, verifier: verifier, target: target, replica: replica}, nil
}

// NewManualContractOpsFromConfig 按 Commerce.ManualResolutionSecretEnv 装配。没配环境变量名 = 入口关闭,回 nil, nil;
// 配了却没有合同引擎(资产通道没开)、读不到有效密钥或没写有效的环境名,回错误(调用方打 ERROR,入口同样关闭)。
func NewManualContractOpsFromConfig(engine *commerce.Engine, c config.CommerceConf, getenv func(string) string) (*ManualContractOps, error) {
	name := strings.TrimSpace(c.ManualResolutionSecretEnv)
	if name == "" {
		return nil, nil
	}
	if engine == nil {
		return nil, errors.New("trade: manual resolution entry needs the commerce engine (AssetOp.Enabled)")
	}
	replica, _ := os.Hostname()
	return NewManualContractOps(engineManualStore{engine: engine}, getenv(name), c.ManualResolutionEnvironment, replica)
}

// verify 是两个方法共用的调用方准入,拒绝发生在任何读写之前:带会话(客户端来源)、入口关闭(o 为 nil)、验签失败、
// 调用方不是 ops-commerce 一律 PermissionDenied(确定的拒绝)。
func (o *ManualContractOps) verify(ctx context.Context, method string, in proto.Message) error {
	if _, client := roleidentity.Original(ctx); client {
		return status.Error(codes.PermissionDenied, "clients cannot call the manual resolution entry")
	}
	if o == nil || o.verifier == nil {
		logx.WithContext(ctx).Errorf("[CommerceManual] Rejecting %s: entry is disabled (Commerce.ManualResolutionSecretEnv not configured or its secret is invalid)", method)
		return status.Error(codes.PermissionDenied, "manual resolution entry is disabled")
	}
	caller, err := o.verifier.Verify(ctx, method, in)
	if err != nil || caller != config.ManualResolutionCaller {
		logx.WithContext(ctx).Errorf("[CommerceManual] Rejecting %s: caller proof is invalid", method)
		return status.Error(codes.PermissionDenied, "manual resolution caller proof is invalid")
	}
	return nil
}

// ListManualContracts 只读列一页转人工的合同。读失败带内 retry = true:只读查询没有结局未知。
func (o *ManualContractOps) ListManualContracts(ctx context.Context, in *tradepb.ListManualContractsRequest) (*tradepb.ListManualContractsResponse, error) {
	if err := o.verify(ctx, tradepb.TradeAdmin_ListManualContracts_FullMethodName, in); err != nil {
		return nil, err
	}
	if in.GetLimit() < 1 || in.GetLimit() > commerce.ManualListPageMax {
		return nil, status.Errorf(codes.InvalidArgument, "limit must be within [1, %d]", commerce.ManualListPageMax)
	}
	rows, err := o.store.List(ctx, in.GetAfterUpdatedMs(), in.GetAfterCommerceId(), int(in.GetLimit()))
	if err != nil {
		logx.WithContext(ctx).Errorf("[CommerceManual] Listing manual contracts failed: %v", err)
		return &tradepb.ListManualContractsResponse{Retry: true, Target: o.target}, nil
	}
	out := &tradepb.ListManualContractsResponse{Contracts: rows, Target: o.target}
	if len(rows) == int(in.GetLimit()) {
		last := rows[len(rows)-1]
		out.NextAfterUpdatedMs, out.NextAfterCommerceId = last.GetUpdatedMs(), last.GetCommerceId()
	}
	return out, nil
}

// ResolveManualContract 按 mode 做锁外 SHOW、锁内 DRY_RUN(不写)或 EXECUTE。
func (o *ManualContractOps) ResolveManualContract(ctx context.Context, in *tradepb.ResolveManualContractRequest) (*tradepb.ResolveManualContractResponse, error) {
	if err := o.verify(ctx, tradepb.TradeAdmin_ResolveManualContract_FullMethodName, in); err != nil {
		return nil, err
	}
	if in.GetCommerceId() == 0 {
		return nil, status.Error(codes.InvalidArgument, "missing commerce_id")
	}
	out := &tradepb.ResolveManualContractResponse{Target: o.target}
	switch in.GetMode() {
	case tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_SHOW:
		report, err := o.store.Show(ctx, in.GetCommerceId())
		o.showReply(ctx, in.GetCommerceId(), out, report, err)
		return out, nil
	case tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_DRY_RUN, tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_EXECUTE:
		return o.resolve(ctx, in, out)
	default:
		return nil, status.Error(codes.InvalidArgument, "mode must be SHOW, DRY_RUN or EXECUTE")
	}
}

// showReply 把 ShowManual 的报告填进回包。
func (o *ManualContractOps) showReply(ctx context.Context, id uint64, out *tradepb.ResolveManualContractResponse, report commerce.ManualReport, err error) {
	switch {
	case errors.Is(err, commerce.ErrManualNotFound):
		out.Verdict = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_NOT_FOUND
		return
	case err != nil:
		logx.WithContext(ctx).Errorf("[CommerceManual] Showing contract failed commerce_id=%d: %v", id, err)
		out.Verdict, out.Refusal = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_RETRY, err.Error()
		return
	}
	out.Verdict = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK
	out.Record, out.Linked = report.Record, report.Linked
	if report.DecodeErr != nil {
		out.Refusal = "workflow undecodable: " + report.DecodeErr.Error()
	}
	for _, l := range report.Legs {
		out.Legs = append(out.Legs, &tradepb.ManualContractLeg{Leg: l.Leg, OpStatus: l.OpStatus, FinalMs: l.FinalMs, ReadFailed: l.ReadErr != nil})
	}
	for _, p := range report.Parties {
		out.Parties = append(out.Parties, &tradepb.ManualContractParty{
			Party: p.Party, PlayerId: p.Player, ContractOwnerRevision: p.ContractRevision, CurrentOwnerRevision: p.CurrentRevision,
			SameAccount: p.SameAccount, Deleted: p.Deleted, Frozen: p.Frozen, Unknown: p.Unknown, Changed: p.Changed,
		})
	}
	for _, v := range report.Verdicts {
		out.Actions = append(out.Actions, &tradepb.ManualContractActionVerdict{Action: string(v.Action), Allowed: v.Allowed, Detail: v.Detail})
	}
}

// resolve 是 DRY_RUN / EXECUTE:核对目标环境,再交给 commerce 在锁内判定(DRY_RUN 不写、EXECUTE 写)。
func (o *ManualContractOps) resolve(ctx context.Context, in *tradepb.ResolveManualContractRequest, out *tradepb.ResolveManualContractResponse) (*tradepb.ResolveManualContractResponse, error) {
	execute := in.GetMode() == tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_EXECUTE
	if in.GetExpectTarget() != o.target {
		out.Verdict, out.Refusal = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_REFUSED, "target_mismatch"
		o.audit(ctx, in, out)
		return out, nil
	}
	req, err := manualRequestOf(in)
	if err != nil {
		out.Verdict, out.Refusal = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_REFUSED, err.Error()
		o.audit(ctx, in, out)
		return out, nil
	}
	result, err := o.store.Resolve(ctx, req, execute)
	switch {
	case err == nil:
		out.Verdict = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK
		out.Plan, out.Stage, out.Status = result.Audit, result.Stage, result.Status
		if result.Linked != nil {
			out.LinkedPlan = result.Linked.Audit
		}
	case errors.Is(err, commerce.ErrManualNotFound):
		out.Verdict = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_NOT_FOUND
	case errors.Is(err, commerce.ErrManualVersionChanged):
		out.Verdict, out.Refusal = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_VERSION_CHANGED, err.Error()
	case errors.Is(err, commerce.ErrManualNotAllowed):
		out.Verdict, out.Refusal = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_REFUSED, err.Error()
	case execute && authoritysql.IsCommitOutcomeUnknown(err):
		// 结局未知:不报成功也不报没写;回 Unavailable,运维先 SHOW 重查。
		logx.WithContext(ctx).Errorf("[CommerceManual] EXECUTE commit outcome unknown commerce_id=%d operator=%q: %v; run show before anything else",
			in.GetCommerceId(), in.GetOperator(), err)
		return nil, status.Error(codes.Unavailable, "commit outcome unknown: run show first; a manual_actions entry with this operator means it was applied")
	default:
		// 读库失败,或写入在提交之前失败:事务已回滚,没有改动。
		logx.WithContext(ctx).Errorf("[CommerceManual] Manual resolution failed commerce_id=%d: %v", in.GetCommerceId(), err)
		out.Verdict, out.Refusal = tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_RETRY, err.Error()
	}
	o.audit(ctx, in, out)
	return out, nil
}

// manualRequestOf 把请求翻成 commerce.ManualRequest。动作名或缺口写错回 ErrManualNotAllowed(带内 REFUSED)。
func manualRequestOf(in *tradepb.ResolveManualContractRequest) (commerce.ManualRequest, error) {
	action, ok := commerce.ParseManualAction(in.GetAction())
	if !ok {
		return commerce.ManualRequest{}, fmt.Errorf("%w: unknown action %q", commerce.ErrManualNotAllowed, in.GetAction())
	}
	req := commerce.ManualRequest{
		CommerceID: in.GetCommerceId(), ExpectVersion: in.GetExpectVersion(), Action: action,
		Operator: in.GetOperator(), Reason: in.GetReason(),
		Party: in.GetParty(), To: in.GetToPlayerId(),
		LinkedID: in.GetLinkedCommerceId(), LinkedVersion: in.GetLinkedVersion(),
	}
	for _, raw := range in.GetParts() {
		part, err := commerce.ParsePartialPart(raw)
		if err != nil {
			return commerce.ManualRequest{}, err
		}
		req.Parts = append(req.Parts, part)
	}
	return req, nil
}

// audit 写审计日志行并放进回包,工具原样打印、贴进工单。真正写了的打 ERROR(确保进日志检索),其余打 INFO。
// commerce_id、player_id 只进正文,不做指标标签。操作人、依据、缺口是调用方填的自由文本,一律 %q 引起来,
// 免得填成 `x executed=true` 在行里伪造出字段(与 match 的 [FormationManual] 同一条教训)。其余字段都来自服务端。
func (o *ManualContractOps) audit(ctx context.Context, in *tradepb.ResolveManualContractRequest, out *tradepb.ResolveManualContractResponse) {
	plan := out.GetPlan()
	executed := in.GetMode() == tradepb.ManualContractMode_MANUAL_CONTRACT_MODE_EXECUTE && out.GetVerdict() == tradepb.ManualContractVerdict_MANUAL_CONTRACT_VERDICT_OK
	out.AuditLine = fmt.Sprintf("[CommerceManual] operator=%q commerce_id=%d action=%q mode=%s verdict=%s refusal=%q version_before=%d "+
		"quarantine_stage=%s quarantine_reason=%s stage=%s status=%d new_legs=%v superseded=%v retargeted=%v party=%s from=%d to=%d "+
		"linked=%d linked_version_before=%d parts=%q target=%s replica=%s reason=%q executed=%t",
		in.GetOperator(), in.GetCommerceId(), in.GetAction(), in.GetMode(), out.GetVerdict(), out.GetRefusal(), plan.GetVersionBefore(),
		plan.GetQuarantine().GetStage(), plan.GetQuarantine().GetReason(), out.GetStage(), out.GetStatus(),
		plan.GetNewLegs(), plan.GetSupersededLegs(), plan.GetRetargetedLegs(), plan.GetParty(), plan.GetFromPlayerId(), plan.GetToPlayerId(),
		plan.GetLinkedCommerceId(), out.GetLinkedPlan().GetVersionBefore(), strings.Join(in.GetParts(), ";"),
		out.GetTarget(), o.replica, in.GetReason(), executed)
	if executed {
		logx.WithContext(ctx).Error(out.AuditLine)
		return
	}
	logx.WithContext(ctx).Info(out.AuditLine)
}
