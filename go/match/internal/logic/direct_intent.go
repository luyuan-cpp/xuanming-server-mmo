package logic

import (
	"bytes"

	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	pb "proto/match"
)

// 成功回包只承诺原意图已经在 SQL 提交。之后丢响应/进程退出由同一 formation 恢复。
func (l *StartActivityBattleLogic) startGlobalActivity(in *pb.StartActivityBattleRequest) (*pb.StartActivityBattleResponse, error) {
	kind := activityKindLabel(in.GetActivityContext().GetKind())
	if in.RequestId == 0 || in.ActivityContext.GetLaunchRequestId() != in.RequestId {
		return l.reject(kind, pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0), nil
	}
	key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: in.ActivityContext.GuildId, RequestLow: in.RequestId}
	payload, e := proto.MarshalOptions{Deterministic: true}.Marshal(in)
	if e != nil {
		return l.reject(kind, pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0), nil
	}
	old, original, e := l.svcCtx.Authority.FindDirect(l.ctx, key)
	if e != nil {
		return l.reject(kind, pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0), nil
	}
	if old != nil {
		if !bytes.Equal(original, payload) {
			return l.reject(kind, pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0), nil
		}
		if old.State < 4 {
			launchGlobalFormation(l.svcCtx, old)
		}
		return &pb.StartActivityBattleResponse{BattleId: old.BattleId, FormationState: old.State}, nil
	}
	for _, pid := range in.MemberPlayerIds {
		reject := l.precheckMember(pid)
		if reject != pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE {
			return l.reject(kind, reject, pid), nil
		}
	}
	f, e := reserveGlobalDirect(l.ctx, l.svcCtx, key, payload, pb.MatchMode_MATCH_MODE_PVE_TEAM, in.BattleConfigId, in.MemberPlayerIds, directOptions{activityContext: in.ActivityContext})
	if e != nil {
		return l.reject(kind, pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0), nil
	}
	launchGlobalFormation(l.svcCtx, f)
	metrics.ObserveActivityBattle(kind, activityResultStarted)
	return &pb.StartActivityBattleResponse{BattleId: f.BattleId, FormationState: f.State}, nil
}
