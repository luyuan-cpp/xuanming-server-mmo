package logic

import (
	"context"
	"errors"
	"fmt"

	"friend/internal/constants"
	"friend/internal/data"
	"github.com/zeromicro/go-zero/core/logx"
	pb "proto/friend"
)

func (l *FriendLogic) listOnlinePlayers(ctx context.Context, caller uint64, req *pb.RecommendFriendsRequest, limit uint32) (*pb.RecommendFriendsResponse, error) {
	reject := func(code uint32, msg string) (*pb.RecommendFriendsResponse, error) {
		return &pb.RecommendFriendsResponse{OnlineDirectory: true, ErrorMessage: tipErr(code, msg)}, nil
	}
	if err := data.ValidateDirectoryInput(req.GetCursor(), req.GetQuery()); err != nil {
		return reject(constants.ErrInvalidParameter, "invalid online directory cursor or query")
	}
	rds := l.deps.SvcCtx.FriendRedis
	if rds == nil {
		return reject(constants.ErrStorage, "online directory limiter unavailable")
	}
	// 分页也计额度；每分钟 60 页，独立于加好友申请额度。读故障 fail-closed。
	countRaw, err := rds.EvalCtx(ctx, requestQuotaScript, []string{fmt.Sprintf("friend:{directory:%d}", caller)}, "60")
	if err != nil {
		logx.WithContext(ctx).Errorf("[friend] 在线目录限流读取失败: %v", err)
		return reject(constants.ErrStorage, "online directory limiter unavailable")
	}
	count, ok := countRaw.(int64)
	if !ok {
		return reject(constants.ErrStorage, "online directory limiter invalid")
	}
	if count > 60 {
		return reject(constants.ErrRateLimited, "online directory rate limited")
	}
	directory := data.OnlineDirectory{Redis: l.deps.SvcCtx.SharedRedis, DataService: l.deps.SvcCtx.DataService}
	resp, err := directory.List(ctx, caller, req.GetCursor(), limit, req.GetExcludePlayerIds(), req.GetQuery())
	if errors.Is(err, data.ErrInvalidDirectoryInput) {
		return reject(constants.ErrInvalidParameter, "invalid online directory parameters")
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("[friend] 在线目录读取失败: %v", err)
		return reject(constants.ErrStorage, "online directory unavailable")
	}
	return resp, nil
}
