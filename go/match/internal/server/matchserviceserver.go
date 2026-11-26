package server

import (
	"context"

	"match/internal/logic"
	"match/internal/svc"

	battlepb "proto/battle"
	base "proto/common/base"
	matchpb "proto/match"
)

// MatchServiceServer 是 gRPC 入口薄包装(照 scene_manager 的
// internal/server 模式):每个 RPC 委托对应 logic,不写业务。
type MatchServiceServer struct {
	svcCtx *svc.ServiceContext
	matchpb.UnimplementedMatchServiceServer
}

func NewMatchServiceServer(svcCtx *svc.ServiceContext) *MatchServiceServer {
	return &MatchServiceServer{
		svcCtx: svcCtx,
	}
}

func (s *MatchServiceServer) GetNetworkProbePlan(ctx context.Context, in *matchpb.GetNetworkProbePlanRequest) (*matchpb.GetNetworkProbePlanResponse, error) {
	return logic.GetNetworkProbePlan(ctx, s.svcCtx)
}
func (s *MatchServiceServer) CommitNetworkProbe(ctx context.Context, in *matchpb.CommitNetworkProbeRequest) (*matchpb.CommitNetworkProbeResponse, error) {
	return logic.CommitNetworkProbe(ctx, s.svcCtx, in)
}

// JoinQueue 玩家入队(客户端经 gate gRPC 直达)。
func (s *MatchServiceServer) JoinQueue(ctx context.Context, in *matchpb.JoinQueueRequest) (*matchpb.JoinQueueResponse, error) {
	l := logic.NewJoinQueueLogic(ctx, s.svcCtx)
	return l.JoinQueue(in)
}

// CancelQueue 取消排队。
func (s *MatchServiceServer) CancelQueue(ctx context.Context, in *matchpb.CancelQueueRequest) (*base.Empty, error) {
	l := logic.NewCancelQueueLogic(ctx, s.svcCtx)
	return l.CancelQueue(in)
}

// GetQueueStatus 查询排队状态。
func (s *MatchServiceServer) GetQueueStatus(ctx context.Context, in *matchpb.GetQueueStatusRequest) (*matchpb.GetQueueStatusResponse, error) {
	l := logic.NewGetQueueStatusLogic(ctx, s.svcCtx)
	return l.GetQueueStatus(in)
}

// ChallengePlayer 场景发起 PK(切磋)。
func (s *MatchServiceServer) ChallengePlayer(ctx context.Context, in *matchpb.ChallengePlayerRequest) (*matchpb.ChallengePlayerResponse, error) {
	l := logic.NewChallengeLogic(ctx, s.svcCtx)
	return l.ChallengePlayer(in)
}

// RespondChallenge 应战 / 拒战。
func (s *MatchServiceServer) RespondChallenge(ctx context.Context, in *matchpb.RespondChallengeRequest) (*matchpb.RespondChallengeResponse, error) {
	l := logic.NewChallengeLogic(ctx, s.svcCtx)
	return l.RespondChallenge(in)
}

// NotifyChallengeInvite / NotifyChallengeResult:S2C 推送消息借 service 声明
// 拿 message id 的占位 RPC,实际下行走 Kafka gate PushToPlayerEvent。
func (s *MatchServiceServer) NotifyChallengeInvite(ctx context.Context, in *matchpb.ChallengeInviteS2C) (*base.Empty, error) {
	l := logic.NewChallengeLogic(ctx, s.svcCtx)
	return l.NotifyChallengeInvite(in)
}

func (s *MatchServiceServer) NotifyChallengeResult(ctx context.Context, in *matchpb.ChallengeResultS2C) (*base.Empty, error) {
	l := logic.NewChallengeLogic(ctx, s.svcCtx)
	return l.NotifyChallengeResult(in)
}

// WatchBattle 观战接入(二期,设计文档 §10;battle_id=0 随机观战)。
func (s *MatchServiceServer) WatchBattle(ctx context.Context, in *matchpb.WatchBattleRequest) (*matchpb.WatchBattleResponse, error) {
	l := logic.NewWatchBattleLogic(ctx, s.svcCtx)
	return l.WatchBattle(in)
}

// ListWatchableBattles 可观战列表(二期,设计文档 §10)。
func (s *MatchServiceServer) ListWatchableBattles(ctx context.Context, in *matchpb.ListWatchableBattlesRequest) (*matchpb.ListWatchableBattlesResponse, error) {
	l := logic.NewListWatchableBattlesLogic(ctx, s.svcCtx)
	return l.ListWatchableBattles(in)
}

// RequestBattleTicket 客户端丢票补签(客户端协议;turn-based §18 D25、client-rpc-router.md D33):
// match 按观战索引定位房间所在 battle 节点,再调 BattleNode.IssueBattleTicket 由 battle 核对名单自签。
func (s *MatchServiceServer) RequestBattleTicket(ctx context.Context, in *battlepb.RequestBattleTicketRequest) (*battlepb.RequestBattleTicketResponse, error) {
	l := logic.NewRequestBattleTicketLogic(ctx, s.svcCtx)
	return l.RequestBattleTicket(in)
}

// ListBattleReports 本人最近的 PVP 战报列表(客户端协议;docs/design/battle-report-replay.md §7.3)。
func (s *MatchServiceServer) ListBattleReports(ctx context.Context, in *matchpb.ListBattleReportsRequest) (*matchpb.ListBattleReportsResponse, error) {
	l := logic.NewBattleReportLogic(ctx, s.svcCtx)
	return l.ListBattleReports(in)
}

// GetBattleReport 取一局的回放正文(客户端协议;只许本人列表里有这一局的玩家取)。
func (s *MatchServiceServer) GetBattleReport(ctx context.Context, in *matchpb.GetBattleReportRequest) (*matchpb.GetBattleReportResponse, error) {
	l := logic.NewBattleReportLogic(ctx, s.svcCtx)
	return l.GetBattleReport(in)
}

// ListBattleScripts 回官方剧本、本人的剧本与选用(客户端协议,docs/design/battle-script.md §7)。
func (s *MatchServiceServer) ListBattleScripts(ctx context.Context, in *matchpb.ListBattleScriptsRequest) (*matchpb.ListBattleScriptsResponse, error) {
	return logic.NewBattleScriptLogic(ctx, s.svcCtx).ListBattleScripts(in)
}

// SubmitBattleScript 新建或改写本人的剧本,存为待审的新版本(客户端协议)。
func (s *MatchServiceServer) SubmitBattleScript(ctx context.Context, in *matchpb.SubmitBattleScriptRequest) (*matchpb.SubmitBattleScriptResponse, error) {
	return logic.NewBattleScriptLogic(ctx, s.svcCtx).SubmitBattleScript(in)
}

// DeleteBattleScript 删除本人的剧本(客户端协议)。
func (s *MatchServiceServer) DeleteBattleScript(ctx context.Context, in *matchpb.DeleteBattleScriptRequest) (*matchpb.DeleteBattleScriptResponse, error) {
	return logic.NewBattleScriptLogic(ctx, s.svcCtx).DeleteBattleScript(in)
}

// SetBattleScriptLoadout 为某种对局选用剧本或清除选用(客户端协议)。
func (s *MatchServiceServer) SetBattleScriptLoadout(ctx context.Context, in *matchpb.SetBattleScriptLoadoutRequest) (*matchpb.SetBattleScriptLoadoutResponse, error) {
	return logic.NewBattleScriptLogic(ctx, s.svcCtx).SetBattleScriptLoadout(in)
}

// GetRankedProfile 读取论剑排位大厅档案、本季进度、预设与展示权益(448)。
func (s *MatchServiceServer) GetRankedProfile(ctx context.Context, in *matchpb.GetRankedProfileRequest) (*matchpb.GetRankedProfileResponse, error) {
	return logic.NewRankedLogic(ctx, s.svcCtx).GetRankedProfile(in)
}

// SaveRankedPreset 保存单排默认论剑预设(449)。
func (s *MatchServiceServer) SaveRankedPreset(ctx context.Context, in *matchpb.SaveRankedPresetRequest) (*matchpb.SaveRankedPresetResponse, error) {
	return logic.NewRankedLogic(ctx, s.svcCtx).SaveRankedPreset(in)
}

// UpdateRankedCosmetic 校验归属并更新论剑展示权益佩戴(450)。
func (s *MatchServiceServer) UpdateRankedCosmetic(ctx context.Context, in *matchpb.UpdateRankedCosmeticRequest) (*matchpb.UpdateRankedCosmeticResponse, error) {
	return logic.NewRankedLogic(ctx, s.svcCtx).UpdateRankedCosmetic(in)
}

// GetRankedBattleResult 查询单局或最近一局论剑排位权威结算摘要(451)。
func (s *MatchServiceServer) GetRankedBattleResult(ctx context.Context, in *matchpb.GetRankedBattleResultRequest) (*matchpb.GetRankedBattleResultResponse, error) {
	return logic.NewRankedLogic(ctx, s.svcCtx).GetRankedBattleResult(in)
}

// ---- 全服赛季战队锦标赛《群仙问道赛》V1(465..475) ----

func (s *MatchServiceServer) GetTournamentOverview(ctx context.Context, in *matchpb.GetTournamentOverviewRequest) (*matchpb.GetTournamentOverviewResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).GetTournamentOverview(in)
}

func (s *MatchServiceServer) CreateTournamentSquad(ctx context.Context, in *matchpb.CreateTournamentSquadRequest) (*matchpb.CreateTournamentSquadResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).CreateTournamentSquad(in)
}

func (s *MatchServiceServer) InviteTournamentMember(ctx context.Context, in *matchpb.InviteTournamentMemberRequest) (*matchpb.InviteTournamentMemberResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).InviteTournamentMember(in)
}

func (s *MatchServiceServer) RespondTournamentInvite(ctx context.Context, in *matchpb.RespondTournamentInviteRequest) (*matchpb.RespondTournamentInviteResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).RespondTournamentInvite(in)
}

func (s *MatchServiceServer) LeaveTournamentSquad(ctx context.Context, in *matchpb.LeaveTournamentSquadRequest) (*matchpb.LeaveTournamentSquadResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).LeaveTournamentSquad(in)
}

func (s *MatchServiceServer) UpdateTournamentLineup(ctx context.Context, in *matchpb.UpdateTournamentLineupRequest) (*matchpb.UpdateTournamentLineupResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).UpdateTournamentLineup(in)
}

func (s *MatchServiceServer) RegisterTournamentSquad(ctx context.Context, in *matchpb.RegisterTournamentSquadRequest) (*matchpb.RegisterTournamentSquadResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).RegisterTournamentSquad(in)
}

func (s *MatchServiceServer) ListTournamentStandings(ctx context.Context, in *matchpb.ListTournamentStandingsRequest) (*matchpb.ListTournamentStandingsResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).ListTournamentStandings(in)
}

func (s *MatchServiceServer) StartTournamentMatch(ctx context.Context, in *matchpb.StartTournamentMatchRequest) (*matchpb.StartTournamentMatchResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).StartTournamentMatch(in)
}

func (s *MatchServiceServer) CheerTournamentSquad(ctx context.Context, in *matchpb.CheerTournamentSquadRequest) (*matchpb.CheerTournamentSquadResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).CheerTournamentSquad(in)
}

func (s *MatchServiceServer) ClaimTournamentReward(ctx context.Context, in *matchpb.ClaimTournamentRewardRequest) (*matchpb.ClaimTournamentRewardResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).ClaimTournamentReward(in)
}

// ---- 全服赛季战队锦标赛《群仙问道赛》V2(506..508) ----

func (s *MatchServiceServer) SaluteTournamentChampion(ctx context.Context, in *matchpb.SaluteTournamentChampionRequest) (*matchpb.SaluteTournamentChampionResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).SaluteTournamentChampion(in)
}

func (s *MatchServiceServer) SendTournamentSpectateCheer(ctx context.Context, in *matchpb.SendTournamentSpectateCheerRequest) (*matchpb.SendTournamentSpectateCheerResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).SendTournamentSpectateCheer(in)
}

func (s *MatchServiceServer) GetTournamentSpectateCheer(ctx context.Context, in *matchpb.GetTournamentSpectateCheerRequest) (*matchpb.GetTournamentSpectateCheerResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).GetTournamentSpectateCheer(in)
}

// ---- 全服赛季战队锦标赛《群仙问道赛》V3 ----

func (s *MatchServiceServer) UpdateTournamentSquadEmblem(ctx context.Context, in *matchpb.UpdateTournamentSquadEmblemRequest) (*matchpb.UpdateTournamentSquadEmblemResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).UpdateTournamentSquadEmblem(in)
}

func (s *MatchServiceServer) StartTournamentWarmup(ctx context.Context, in *matchpb.StartTournamentWarmupRequest) (*matchpb.StartTournamentWarmupResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).StartTournamentWarmup(in)
}

func (s *MatchServiceServer) PredictTournamentMatch(ctx context.Context, in *matchpb.PredictTournamentMatchRequest) (*matchpb.PredictTournamentMatchResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).PredictTournamentMatch(in)
}

func (s *MatchServiceServer) GetTournamentPredictionBoard(ctx context.Context, in *matchpb.GetTournamentPredictionBoardRequest) (*matchpb.GetTournamentPredictionBoardResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).GetTournamentPredictionBoard(in)
}

// ---- 全服赛季战队锦标赛《群仙问道赛》V4 ----

func (s *MatchServiceServer) BanTournamentLeyline(ctx context.Context, in *matchpb.BanTournamentLeylineRequest) (*matchpb.BanTournamentLeylineResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).BanTournamentLeyline(in)
}

func (s *MatchServiceServer) GetTournamentHighlights(ctx context.Context, in *matchpb.GetTournamentHighlightsRequest) (*matchpb.GetTournamentHighlightsResponse, error) {
	return logic.NewTournamentLogic(ctx, s.svcCtx).GetTournamentHighlights(in)
}
