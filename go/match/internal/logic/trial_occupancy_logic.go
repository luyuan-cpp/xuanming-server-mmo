package logic

import (
	"context"
	"errors"

	"match/internal/authority"
	"match/internal/svc"
	pb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

// 帮会历练占用的三个内部 RPC(docs/design/horizontal-scale-logical-db.md §4),调用方是 go/guild。
//
// 结论一律走响应里的 reject,不回 gRPC 错误:写入失败回 Unavailable 的话,发布准入记成未知结局,会拖住本实例的排空证明。
// 占用行有租约,写入结局不明也不会把人锁死,guild 按 RETRY 处理,重放幂等。

// AcquireTrialOccupancy 占住名单上的全部角色。
func AcquireTrialOccupancy(ctx context.Context, s *svc.ServiceContext, in *pb.AcquireTrialOccupancyRequest) (*pb.AcquireTrialOccupancyResponse, error) {
	if !guildCallVerified(ctx, s, pb.MatchInternal_AcquireTrialOccupancy_FullMethodName, in) {
		return &pb.AcquireTrialOccupancyResponse{Reject: pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_DENIED}, nil
	}
	offender, err := s.Authority.AcquireTrialOccupancy(ctx, in.GetOwnerId(), in.GetRoster(), in.GetTtlMs())
	switch {
	case err == nil:
		return &pb.AcquireTrialOccupancyResponse{}, nil
	case errors.Is(err, authority.ErrMatchEntryOccupied):
		return &pb.AcquireTrialOccupancyResponse{Reject: pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_OCCUPIED, OffenderPlayerId: offender}, nil
	case errors.Is(err, authority.ErrOriginalPlayer):
		return &pb.AcquireTrialOccupancyResponse{Reject: pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_ROLE}, nil
	case errors.Is(err, authority.ErrOccupancyInvalid):
		logx.WithContext(ctx).Errorf("[match] Invalid trial occupancy acquire, this is a guild defect: owner=%d roster=%d ttl_ms=%d", in.GetOwnerId(), len(in.GetRoster()), in.GetTtlMs())
		return &pb.AcquireTrialOccupancyResponse{Reject: pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_DENIED}, nil
	default:
		logx.WithContext(ctx).Errorf("[match] Trial occupancy acquire failed: owner=%d err=%v", in.GetOwnerId(), err)
		return &pb.AcquireTrialOccupancyResponse{Reject: pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_RETRY}, nil
	}
}

// ReleaseTrialOccupancy 放开这个大厅的占用。
func ReleaseTrialOccupancy(ctx context.Context, s *svc.ServiceContext, in *pb.ReleaseTrialOccupancyRequest) (*pb.ReleaseTrialOccupancyResponse, error) {
	if !guildCallVerified(ctx, s, pb.MatchInternal_ReleaseTrialOccupancy_FullMethodName, in) {
		return &pb.ReleaseTrialOccupancyResponse{Reject: pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_DENIED}, nil
	}
	if err := s.Authority.ReleaseTrialOccupancy(ctx, in.GetOwnerId(), in.GetPlayerIds()); err != nil {
		logx.WithContext(ctx).Errorf("[match] Trial occupancy release failed: owner=%d err=%v", in.GetOwnerId(), err)
		return &pb.ReleaseTrialOccupancyResponse{Reject: pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_RETRY}, nil
	}
	return &pb.ReleaseTrialOccupancyResponse{}, nil
}

// GetBattleCompletion 回一局的 SQL 终局。没有终局是 UNKNOWN,读失败是 RETRY,不能把读不到当成没有结果。
func GetBattleCompletion(ctx context.Context, s *svc.ServiceContext, in *pb.GetBattleCompletionRequest) (*pb.GetBattleCompletionResponse, error) {
	if !guildCallVerified(ctx, s, pb.MatchInternal_GetBattleCompletion_FullMethodName, in) {
		return &pb.GetBattleCompletionResponse{Reject: pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_DENIED}, nil
	}
	row, err := s.Authority.BattleCompletion(ctx, in.GetBattleId())
	if err != nil {
		logx.WithContext(ctx).Errorf("[match] Battle completion read failed: battle=%d err=%v", in.GetBattleId(), err)
		return &pb.GetBattleCompletionResponse{Reject: pb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_RETRY}, nil
	}
	if row == nil {
		return &pb.GetBattleCompletionResponse{}, nil
	}
	out := &pb.GetBattleCompletionResponse{Decision: row.Decision}
	if row.Decision == pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE {
		out.RawPayload = row.RawPayload
	}
	return out, nil
}
