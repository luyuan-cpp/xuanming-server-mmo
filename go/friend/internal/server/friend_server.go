// Package server 是 friend 服务的 gRPC 入口薄包装(照 chat / trade / match 的 internal/server 模式):
// 每个 RPC 委托给 logic,不写业务。
package server

import (
	"context"

	"friend/internal/logic"

	friendpb "proto/friend"
)

// FriendServer 实现 friendpb.ClientPlayerFriendServer。
//
// 该 service 标了 OptionIsClientProtocolService=true:只接客户端经 gate → 路由服转发来的请求。
// 东西向 / 运维方法不许加到这个 service 上 —— 加进来就等于对客户端开放了一个消息号
// (生成器会给带 ClientPlayer 前缀的服务出客户端 handler)。
//
// 内嵌 UnimplementedClientPlayerFriendServer 承担两件事:
//  1. 满足生成代码的 mustEmbed 约束,将来 proto 加 RPC 时本文件不会编译不过;
//  2. 让 NotifyFriendEvent 自动回 gRPC Unimplemented。它是 S2C 推送方向
//     (服务端 → 客户端),服务端**不提供**这个方向的 handler:推送由
//     kafkautil.PushToPlayer 经 gate 下发,不是客户端拨过来的。客户端伪造调用在
//     **会话层**就被挡住了(internal/session 的 ClientMethods 白名单不收它,
//     friend_test.go 的 TestSessionRejectsNotifyFriendEventFromClient 守住),
//     这里是第二道防线 —— "今天返回 Unimplemented"不是安全保证,白名单才是。
//
// F2 批补上了 Block / Unblock / ListBlocks / RecommendFriends 四个方法的转发。
// 它们在 F1 批也靠内嵌的 Unimplemented 兜着,当时**刻意不写占位方法**的理由仍然有效、
// 且适用于将来任何新 RPC:占位要么回 constants.ErrStorage(谎称故障、把误告警刷成常态),
// 要么回 constants.ErrInvalidParameter(谎称客户端参数错),两种都比一个诚实的
// Unimplemented 更难排查 —— 后者在指标上是 transport_error,一眼能看出"这个方法没人接"。
//
// 注意:现在 10 个 C2S 方法全部转发到 logic,与会话白名单的 10 条恰好一一对应,
// 但两者仍是两件事:白名单管**准入**(哪些方法允许客户端来源调用),这里管**是否有实现**。
// 新增 RPC 时两边都要各自表态,不能因为一边动了就假定另一边自动跟上。
type FriendServer struct {
	friendpb.UnimplementedClientPlayerFriendServer
	logic *logic.FriendLogic
}

// NewFriendServer 返回接口类型而不是 *FriendServer:调用方(main)只需要能注册进 gRPC,
// 拿到具体类型只会诱使它绕过 logic 直接调方法。
func NewFriendServer(deps *logic.Deps) friendpb.ClientPlayerFriendServer {
	return &FriendServer{logic: logic.NewFriendLogic(deps)}
}

// AddFriend 发起好友申请。目标取请求体,发起者取会话(D-9:客户端请求不带自身 player_id)。
func (s *FriendServer) AddFriend(ctx context.Context, in *friendpb.AddFriendRequest) (*friendpb.AddFriendResponse, error) {
	return s.logic.AddFriend(ctx, in)
}

// AcceptFriend 通过别人发来的好友申请。
func (s *FriendServer) AcceptFriend(ctx context.Context, in *friendpb.AcceptFriendRequest) (*friendpb.AcceptFriendResponse, error) {
	return s.logic.AcceptFriend(ctx, in)
}

// RejectFriend 拒绝别人发来的好友申请。
func (s *FriendServer) RejectFriend(ctx context.Context, in *friendpb.RejectFriendRequest) (*friendpb.RejectFriendResponse, error) {
	return s.logic.RejectFriend(ctx, in)
}

// RemoveFriend 删除好友(双向)。
func (s *FriendServer) RemoveFriend(ctx context.Context, in *friendpb.RemoveFriendRequest) (*friendpb.RemoveFriendResponse, error) {
	return s.logic.RemoveFriend(ctx, in)
}

// GetFriendList 取调用者自己的好友列表。
func (s *FriendServer) GetFriendList(ctx context.Context, in *friendpb.GetFriendListRequest) (*friendpb.GetFriendListResponse, error) {
	return s.logic.GetFriendList(ctx, in)
}

// GetPendingRequests 取调用者自己的待处理(入站)申请列表。
func (s *FriendServer) GetPendingRequests(ctx context.Context, in *friendpb.GetPendingRequestsRequest) (*friendpb.GetPendingRequestsResponse, error) {
	return s.logic.GetPendingRequests(ctx, in)
}

// Block 把目标拉进调用者自己的黑名单。
//
// 拉黑不是"删好友的加强版":删好友只断当前关系,对方下一秒就能加回来;拉黑要持久地
// 拒绝对方再次发起申请,所以它必须落自己的表(friend_block),并顺带断掉双向好友边与
// 两个方向的待处理申请(见 internal/data/block_repo.go 的事务)。
func (s *FriendServer) Block(ctx context.Context, in *friendpb.BlockRequest) (*friendpb.BlockResponse, error) {
	return s.logic.Block(ctx, in)
}

// Unblock 解除拉黑。它只让黑名单变短,**不会**把之前被拉黑时断掉的好友关系加回来 ——
// 想重新成为好友必须重新走 AddFriend(单向解除不该替对方做"重新加好友"的决定)。
func (s *FriendServer) Unblock(ctx context.Context, in *friendpb.UnblockRequest) (*friendpb.UnblockResponse, error) {
	return s.logic.Unblock(ctx, in)
}

// ListBlocks 取调用者自己的黑名单。只回 id 与拉黑时刻,昵称由客户端另查(同 FriendEntry)。
func (s *FriendServer) ListBlocks(ctx context.Context, in *friendpb.ListBlocksRequest) (*friendpb.ListBlocksResponse, error) {
	return s.logic.ListBlocks(ctx, in)
}

// RecommendFriends 返回可加好友的候选。
//
// 推荐是可降级的展示功能:候选取不满、甚至一条都没有,都不是失败。所以这里与其它方法
// 一样只做转发,不在入口处补"至少 N 条"的兜底 —— 兜底会把"当前确实没人可推"
// 伪装成一次成功的推荐。
func (s *FriendServer) RecommendFriends(ctx context.Context, in *friendpb.RecommendFriendsRequest) (*friendpb.RecommendFriendsResponse, error) {
	return s.logic.RecommendFriends(ctx, in)
}
