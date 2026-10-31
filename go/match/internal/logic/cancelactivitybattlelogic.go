package logic

import (
	"context"
	"errors"
	"time"

	"match/internal/authority"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"
	pb "proto/match"
	"shared/serviceauth"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type CancelActivityBattleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewCancelActivityBattleLogic(ctx context.Context, sc *svc.ServiceContext) *CancelActivityBattleLogic {
	return &CancelActivityBattleLogic{ctx: ctx, svcCtx: sc}
}
func (l *CancelActivityBattleLogic) CancelActivityBattle(in *pb.CancelActivityBattleRequest) (*pb.CancelActivityBattleResponse, error) {
	if details, client := ctxkeys.GetSessionDetails(l.ctx); client && details != nil {
		return nil, status.Error(codes.PermissionDenied, "客户端不能取消内部活动开战原件")
	}
	if l.svcCtx == nil || l.svcCtx.ActivityCancelAuth == nil || len(l.svcCtx.ActivityCancelSecret) < 32 {
		return nil, status.Error(codes.Unavailable, "活动取消独立签名依赖未就绪")
	}
	method := pb.MatchInternal_CancelActivityBattle_FullMethodName
	caller, err := l.svcCtx.ActivityCancelAuth.Verify(l.ctx, method, in)
	if err != nil || caller != "guild" {
		return nil, status.Error(codes.PermissionDenied, "活动取消调用证明无效")
	}
	original := in.GetOriginal()
	if validateStartActivityBattle(original) != "" || original.GetRequestId() == 0 || original.GetActivityContext().GetLaunchRequestId() != original.RequestId {
		return nil, status.Error(codes.InvalidArgument, "活动取消缺完整原开战身份")
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "原开战正文无效")
	}
	digest, err := serviceauth.Digest(in)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "原取消摘要无效")
	}
	out, err := l.svcCtx.Authority.CancelActivity(l.ctx, &pb.MatchDirectIntentRecord{Kind: 2, OwnerId: original.ActivityContext.GuildId, RequestLow: original.RequestId}, payload)
	if err != nil {
		if errors.Is(err, authority.ErrDirectConflict) {
			return nil, status.Error(codes.FailedPrecondition, "原开战正文与持久意图冲突")
		}
		return nil, status.Error(codes.Unavailable, "原活动取消提交结果尚未确认")
	}
	out.RequestDigest = digest
	if err = serviceauth.Sign(method+"/reply", out, "match", l.svcCtx.ActivityCancelSecret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, status.Error(codes.Unavailable, "活动取消回执签名失败")
	}
	return out, nil
}
