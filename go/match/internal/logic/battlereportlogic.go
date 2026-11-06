package logic

import (
	"context"
	"errors"

	"match/internal/authority"
	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

// BattleReportLogic 是战报回放的两条客户端协议(docs/design/battle-report-replay.md §7.3):
//   - player_id 只取 gate 注入的会话身份(x-session-detail-bin → ctxkeys),请求体里没有 player_id;
//     没有会话身份即不可信来源,fail-closed;
//   - 两次读都是主键范围内的只读查询,不开事务、不加锁;存储错误回 ErrInternal,不在服务端重试;
//   - 限频由 gate 的按消息限频负责(MessageLimiter 表),这里不另做。
type BattleReportLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBattleReportLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BattleReportLogic {
	return &BattleReportLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListBattleReports 回本人最近的 PVP 战报列表,新的在前,最多 authority.BattleReportsKeptPerPlayer 条。
func (l *BattleReportLogic) ListBattleReports(_ *matchpb.ListBattleReportsRequest) (*matchpb.ListBattleReportsResponse, error) {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok || detail.GetPlayerId() == 0 {
		l.Errorf("[battle_report] ListBattleReports missing session identity")
		metrics.ObserveBattleReportGet("list", "no_session")
		return &matchpb.ListBattleReportsResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	reports, err := l.svcCtx.Authority.BattleReports(l.ctx, detail.GetPlayerId())
	if err != nil {
		l.Errorf("[battle_report] ListBattleReports read failed player=%d: %v", detail.GetPlayerId(), err)
		metrics.ObserveBattleReportGet("list", "error")
		return &matchpb.ListBattleReportsResponse{ErrorMessage: tipErr(constants.ErrInternal, "战报列表暂时读不到")}, nil
	}
	metrics.ObserveBattleReportGet("list", "ok")
	return &matchpb.ListBattleReportsResponse{Reports: reports}, nil
}

// GetBattleReport 回一局的回放外壳。本人列表里没有这一局(不存在、已被挤出、不是参战者),或这一局没存回放,
// 统一回 ErrBattleReportNotFound(BR-9)。
func (l *BattleReportLogic) GetBattleReport(in *matchpb.GetBattleReportRequest) (*matchpb.GetBattleReportResponse, error) {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok || detail.GetPlayerId() == 0 {
		l.Errorf("[battle_report] GetBattleReport missing session identity battle=%d", in.GetBattleId())
		metrics.ObserveBattleReportGet("get", "no_session")
		return &matchpb.GetBattleReportResponse{ErrorMessage: tipErr(constants.ErrInternal, "缺少玩家身份")}, nil
	}
	if in.GetBattleId() == 0 {
		metrics.ObserveBattleReportGet("get", "not_found")
		return &matchpb.GetBattleReportResponse{ErrorMessage: tipErr(constants.ErrBattleReportNotFound, "没有这一局的回放")}, nil
	}
	envelope, err := l.svcCtx.Authority.BattleReport(l.ctx, detail.GetPlayerId(), in.GetBattleId())
	switch {
	case errors.Is(err, authority.ErrBattleReportNotFound):
		metrics.ObserveBattleReportGet("get", "not_found")
		return &matchpb.GetBattleReportResponse{ErrorMessage: tipErr(constants.ErrBattleReportNotFound, "没有这一局的回放")}, nil
	case err != nil:
		l.Errorf("[battle_report] GetBattleReport read failed player=%d battle=%d: %v", detail.GetPlayerId(), in.GetBattleId(), err)
		metrics.ObserveBattleReportGet("get", "error")
		return &matchpb.GetBattleReportResponse{ErrorMessage: tipErr(constants.ErrInternal, "回放暂时读不到")}, nil
	}
	metrics.ObserveBattleReportGet("get", "ok")
	return &matchpb.GetBattleReportResponse{Replay: envelope}, nil
}
