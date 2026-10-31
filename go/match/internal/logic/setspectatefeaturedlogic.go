package logic

import (
	"context"

	"match/internal/config"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	battlepb "proto/battle"
	pb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetSpectateFeaturedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetSpectateFeaturedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetSpectateFeaturedLogic {
	return &SetSpectateFeaturedLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// SetSpectateFeatured 运营把一场公开局标成 / 取消标成赛事局(battle-spectate-tiered-fanout.md §15.6,Q3):
//   - 调用方只认 serviceauth 的 ops,签名绑定方法与整个请求,不放行别的方法;运营入口未配置密钥或验签失败一律回 DENIED;
//   - 记录不存在 → NOT_FOUND;非公开 → PRIVATE;重复设置同值幂等成功。可见性闸与 WatchBattle 同一判据;
//   - 置位写标记并立即尝试升 L2(受全服配额约束,配额满或升档失败都不算错,回当前档位,下一名观众再试);
//     battle 回房间不存在时撤掉标记并回 NOT_FOUND:成局在战斗结束后仍是 state 4,读得到记录,
//     留着标记会让一场不存在的战斗排在观战列表最前;
//   - 取消只删标记、不降档(一场只升不降);
//   - 中继不可用(RelayEnabled 关闭,Q6)时只写标记:列表照样排在前面,但没有 L2 可升。
func (l *SetSpectateFeaturedLogic) SetSpectateFeatured(in *pb.SetSpectateFeaturedRequest) (*pb.SetSpectateFeaturedResponse, error) {
	battleId := in.GetBattleId()
	if !l.callerAllowed(in) {
		// 拒绝发生在任何副作用之前,结局是确定的,走业务拒绝。回 gRPC 错误会被发布准入记成未知结局,
		// 密钥配错或时钟偏差调用一次,本副本的排空证明就一直 HOLD(go/shared/drainproof)。
		l.Infof("[spectate] SetSpectateFeatured denied battle=%d", battleId)
		return &pb.SetSpectateFeaturedResponse{Reject: pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_DENIED}, nil
	}

	record, err := loadSpectateBattleFn(l.svcCtx, battleId)
	if err != nil {
		return l.internalReject("读观战记录", battleId, err), nil
	}
	if record == nil {
		return &pb.SetSpectateFeaturedResponse{Reject: pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NOT_FOUND}, nil
	}
	if !isSpectatePublic(record.GetVisibility()) {
		return &pb.SetSpectateFeaturedResponse{Reject: pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_PRIVATE}, nil
	}

	if !in.GetFeatured() {
		if err := unmarkSpectateFeatured(l.ctx, l.svcCtx, battleId); err != nil {
			return l.internalReject("取消赛事局标记", battleId, err), nil
		}
	} else {
		if err := markSpectateFeatured(l.ctx, l.svcCtx, battleId); err != nil {
			return l.internalReject("写赛事局标记", battleId, err), nil
		}
		if l.svcCtx.Config.Spectate.RelayUsable() {
			tier, err := readSpectateTier(l.ctx, l.svcCtx, battleId)
			if err != nil {
				return l.internalReject("读观战档位", battleId, err), nil
			}
			if tier != battlepb.ESpectateTier_SPECTATE_TIER_BROADCAST {
				_, _, rejectTip := promoteSpectateBroadcast(l.ctx, l.svcCtx, record)
				if _, _, roomMissing := spectateRejectVerdict(rejectTip); roomMissing {
					// 只撤标记,记录与活跃集合留给观众路径的懒剔除。
					if err := unmarkSpectateFeatured(l.ctx, l.svcCtx, battleId); err != nil {
						return l.internalReject("撤销赛事局标记", battleId, err), nil
					}
					return &pb.SetSpectateFeaturedResponse{Reject: pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_NOT_FOUND}, nil
				}
			}
		}
	}

	tier, err := readSpectateTier(l.ctx, l.svcCtx, battleId)
	if err != nil {
		return l.internalReject("读观战档位", battleId, err), nil
	}
	l.Infof("[spectate] Featured flag updated battle=%d featured=%t tier=%s", battleId, in.GetFeatured(), tier.String())
	return &pb.SetSpectateFeaturedResponse{Tier: tier}, nil
}

// callerAllowed 判定调用方是不是通过验签的运营工具。客户端来源(带会话)、入口未开启(没配密钥)、验签失败都不放行。
func (l *SetSpectateFeaturedLogic) callerAllowed(in *pb.SetSpectateFeaturedRequest) bool {
	if details, client := ctxkeys.GetSessionDetails(l.ctx); client && details != nil {
		return false
	}
	if l.svcCtx == nil || l.svcCtx.SpectateOpsAuth == nil {
		return false
	}
	caller, err := l.svcCtx.SpectateOpsAuth.Verify(l.ctx, pb.MatchInternal_SetSpectateFeatured_FullMethodName, in)
	return err == nil && caller == config.SpectateOpsCaller
}

func (l *SetSpectateFeaturedLogic) internalReject(stage string, battleId uint64, err error) *pb.SetSpectateFeaturedResponse {
	l.Errorf("[spectate] SetSpectateFeatured %s failed battle=%d: %v", stage, battleId, err)
	return &pb.SetSpectateFeaturedResponse{Reject: pb.SpectateFeaturedReject_SPECTATE_FEATURED_REJECT_INTERNAL}
}
