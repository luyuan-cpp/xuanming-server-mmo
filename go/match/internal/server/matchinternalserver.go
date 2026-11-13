package server

import (
	"context"
	"os"

	"match/internal/logic"
	"match/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	matchpb "proto/match"
	"shared/accountdirectory"
)

// MatchInternalServer 是 match 内部服务 MatchInternal 的 gRPC 入口薄包装(照 MatchServiceServer):
// 每个 RPC 委托对应 logic,不写业务。设计见 docs/design/guild-phase2/06-activities.md §6.18.3。
//
// 活动开局和历练占用的合法调用方是 go/guild(帮会同道历练全员同意后确认开战),SetSpectateFeatured 的合法调用方是运营工具
// (serviceauth 调用方 ops,见 logic/setspectatefeaturedlogic.go)。客户端来源(带会话 metadata)的调用
// 在 match_service.go 装配的入口准入(roleidentity.EntryInterceptor)就被 PermissionDenied,logic 入口再防一道;
// 内部调用方按方法验 serviceauth(guild / battle / scene 终局只读查询 / 观战运维 / 成局人工处置运维),部署层 NetworkPolicy 另作一层收口。
type MatchInternalServer struct {
	svcCtx *svc.ServiceContext
	// scene 冻结到期时的 SQL 终局只读查询(GetBattleTerminalDecision)。装配失败(缺场景密钥 / 没有成局权威)时为 nil,
	// 入口一律 PermissionDenied。
	terminalDecision *logic.BattleTerminalDecisionQuery
	// 永久 UNKNOWN 全球成局的人工处置入口(ListStrandedFormations / ResolveStrandedFormation,logic/stranded_formation_ops.go)。
	// GlobalMatch.ManualResolutionSecretEnv 没配、密钥无效或缺有效的 ManualResolutionEnvironment 时为 nil,入口一律 PermissionDenied。
	strandedFormations *logic.StrandedFormationOps
	matchpb.UnimplementedMatchInternalServer
}

func NewMatchInternalServer(svcCtx *svc.ServiceContext) *MatchInternalServer {
	s := &MatchInternalServer{
		svcCtx: svcCtx,
	}
	// 与 TeamAuthority.ReadMembership 同一把场景密钥(team.NewAuthorityServer 缺它拒启,这里不重复拒启)。
	if svcCtx != nil && svcCtx.Authority != nil {
		query, err := logic.NewBattleTerminalDecisionQuery(svcCtx.Authority, os.Getenv(accountdirectory.SceneSecretEnv))
		if err != nil {
			logx.Errorf("[match] GetBattleTerminalDecision disabled, scene freeze expiry will treat every query as unknown: %v", err)
		} else {
			s.terminalDecision = query
		}
		// 人工处置入口只在处置事故时开:没配环境变量名就静默关闭;配了却读不到有效密钥、或没写有效的环境名,打 ERROR,同样关闭,不拒启。
		ops, err := logic.NewStrandedFormationOpsFromConfig(svcCtx.Authority, svcCtx.Config.GlobalMatch, os.Getenv)
		if err != nil {
			logx.Errorf("[match] Stranded formation manual resolution disabled: %v", err)
		} else {
			s.strandedFormations = ops
		}
	}
	return s
}

// StartActivityBattle 活动开局:同步返回 battle_id,gather 异步进行。
func (s *MatchInternalServer) StartActivityBattle(ctx context.Context, in *matchpb.StartActivityBattleRequest) (*matchpb.StartActivityBattleResponse, error) {
	l := logic.NewStartActivityBattleLogic(ctx, s.svcCtx)
	return l.StartActivityBattle(in)
}

func (s *MatchInternalServer) CancelActivityBattle(ctx context.Context, in *matchpb.CancelActivityBattleRequest) (*matchpb.CancelActivityBattleResponse, error) {
	return logic.NewCancelActivityBattleLogic(ctx, s.svcCtx).CancelActivityBattle(in)
}

func (s *MatchInternalServer) CommitBattleOutcome(ctx context.Context, in *matchpb.CommitBattleOutcomeRequest) (*matchpb.CommitBattleOutcomeResponse, error) {
	return logic.CommitBattleOutcome(ctx, s.svcCtx, in)
}

func (s *MatchInternalServer) AcquireTrialOccupancy(ctx context.Context, in *matchpb.AcquireTrialOccupancyRequest) (*matchpb.AcquireTrialOccupancyResponse, error) {
	return logic.AcquireTrialOccupancy(ctx, s.svcCtx, in)
}

func (s *MatchInternalServer) ReleaseTrialOccupancy(ctx context.Context, in *matchpb.ReleaseTrialOccupancyRequest) (*matchpb.ReleaseTrialOccupancyResponse, error) {
	return logic.ReleaseTrialOccupancy(ctx, s.svcCtx, in)
}

func (s *MatchInternalServer) GetBattleCompletion(ctx context.Context, in *matchpb.GetBattleCompletionRequest) (*matchpb.GetBattleCompletionResponse, error) {
	return logic.GetBattleCompletion(ctx, s.svcCtx, in)
}

// GetBattleTerminalDecision scene 冻结到期时只读查一局的 SQL 终局(logic/battle_terminal_decision.go)。
func (s *MatchInternalServer) GetBattleTerminalDecision(ctx context.Context, in *matchpb.GetBattleTerminalDecisionRequest) (*matchpb.GetBattleTerminalDecisionResponse, error) {
	return s.terminalDecision.GetBattleTerminalDecision(ctx, in)
}

// ListStrandedFormations 运维工具 formationfix 列已过截止、仍未裁决的成局(logic/stranded_formation_ops.go)。
func (s *MatchInternalServer) ListStrandedFormations(ctx context.Context, in *matchpb.ListStrandedFormationsRequest) (*matchpb.ListStrandedFormationsResponse, error) {
	return s.strandedFormations.ListStrandedFormations(ctx, in)
}

// ResolveStrandedFormation 运维工具 formationfix 核对 / 演练 / 人工 ABORT 一局永久 UNKNOWN 的成局。
func (s *MatchInternalServer) ResolveStrandedFormation(ctx context.Context, in *matchpb.ResolveStrandedFormationRequest) (*matchpb.ResolveStrandedFormationResponse, error) {
	return s.strandedFormations.ResolveStrandedFormation(ctx, in)
}

// ListPendingBattleScripts 运营拉一页待审的玩家剧本(docs/design/battle-script.md §6.3,调用方 ops)。
func (s *MatchInternalServer) ListPendingBattleScripts(ctx context.Context, in *matchpb.ListPendingBattleScriptsRequest) (*matchpb.ListPendingBattleScriptsResponse, error) {
	return logic.NewBattleScriptReviewLogic(ctx, s.svcCtx).ListPendingBattleScripts(in)
}

// ReviewBattleScript 运营通过或驳回一版待审的玩家剧本(调用方 ops)。
func (s *MatchInternalServer) ReviewBattleScript(ctx context.Context, in *matchpb.ReviewBattleScriptRequest) (*matchpb.ReviewBattleScriptResponse, error) {
	return logic.NewBattleScriptReviewLogic(ctx, s.svcCtx).ReviewBattleScript(in)
}

// SetSpectateFeatured 运营把一场公开局标成 / 取消标成赛事局(battle-spectate-tiered-fanout.md §15.6)。
func (s *MatchInternalServer) SetSpectateFeatured(ctx context.Context, in *matchpb.SetSpectateFeaturedRequest) (*matchpb.SetSpectateFeaturedResponse, error) {
	return logic.NewSetSpectateFeaturedLogic(ctx, s.svcCtx).SetSpectateFeatured(in)
}
