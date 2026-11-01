package logic

import (
	"bytes"
	"context"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"
	pb "proto/match"
)

// guildCallVerified 核 guild 调 MatchInternal 的签名(StartActivityBattle、占用、终局读)。
// match 读不到 guild 的库,这些请求由这把密钥担保。缺密钥拒绝,不降级成无签名。客户端来源一律拒绝。
func guildCallVerified(ctx context.Context, s *svc.ServiceContext, method string, in proto.Message) bool {
	if details, client := ctxkeys.GetSessionDetails(ctx); client && details != nil {
		return false
	}
	if s == nil || s.GuildCallAuth == nil {
		return false
	}
	caller, err := s.GuildCallAuth.Verify(ctx, method, in)
	if err != nil || caller != "guild" {
		logx.WithContext(ctx).Errorf("[match] Rejecting %s: guild call proof is invalid", method)
		return false
	}
	return true
}

// activityStartPayload 是持久化的开局意图正文:不含 auth,每次重试的时间戳不同,正文要逐字节相等,取消也按它比。
func activityStartPayload(in *pb.StartActivityBattleRequest) ([]byte, error) {
	unsigned := proto.Clone(in).(*pb.StartActivityBattleRequest)
	unsigned.Auth = nil
	return proto.MarshalOptions{Deterministic: true}.Marshal(unsigned)
}

// 成功回包只承诺原意图已经在 SQL 提交。之后丢响应/进程退出由同一 formation 恢复。
func (l *StartActivityBattleLogic) startGlobalActivity(in *pb.StartActivityBattleRequest) (*pb.StartActivityBattleResponse, error) {
	kind := activityKindLabel(in.GetActivityContext().GetKind())
	if in.RequestId == 0 || in.ActivityContext.GetLaunchRequestId() != in.RequestId {
		return l.reject(kind, pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0), nil
	}
	if !guildCallVerified(l.ctx, l.svcCtx, pb.MatchInternal_StartActivityBattle_FullMethodName, in) {
		return l.reject(kind, pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0), nil
	}
	key := &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: in.ActivityContext.GuildId, RequestLow: in.RequestId}
	payload, e := activityStartPayload(in)
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
	// 提交点在 PrepareDirectAdmission:占用还由这个大厅持有、名单身份逐项相等才写票。租约丢了走 INTERNAL,guild 按开局失败收尾。
	f, e := reserveGlobalDirect(l.ctx, l.svcCtx, key, payload, pb.MatchMode_MATCH_MODE_PVE_TEAM, in.BattleConfigId, in.MemberPlayerIds, directOptions{activityContext: in.ActivityContext})
	if e != nil {
		return l.reject(kind, pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0), nil
	}
	launchGlobalFormation(l.svcCtx, f)
	metrics.ObserveActivityBattle(kind, activityResultStarted)
	return &pb.StartActivityBattleResponse{BattleId: f.BattleId, FormationState: f.State}, nil
}
