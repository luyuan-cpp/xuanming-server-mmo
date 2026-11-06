package logic

import (
	"errors"
	"testing"

	"friend/internal/constants"
	"friend/internal/data"
	pb "proto/friend"
	"shared/rolefence"

	"github.com/stretchr/testify/require"
)

// 入口不再预检原角色归属(docs/design/gate-online-identity.md §9.3):每个读写方法在自己的事务里按原件复核,
// 核不过(角色冻结、换主、自愈读不到目录)回 ErrRoleNotVerified —— 业务拒绝,不是故障码,不会把排空证明打成 Hold。
// 这张表逐个方法钉住这一点:任何一个方法不走 storageFailure、把它报成存储故障,都会在这里红。
func TestEveryClientMethodReportsRoleNotVerifiedInBand(t *testing.T) {
	// 键是假 store 的失败注入名(fakeFriendStore.take 的参数),值调用对应的客户端方法并返回 tip 码。
	calls := map[string]func(f *fixture) (uint32, error){
		"AddFriendRequest": func(f *fixture) (uint32, error) {
			r, err := f.logic().AddFriend(playerCtx(6001), &pb.AddFriendRequest{TargetPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		},
		"AcceptFriend": func(f *fixture) (uint32, error) {
			r, err := f.logic().AcceptFriend(playerCtx(6002), &pb.AcceptFriendRequest{FromPlayerId: peerID, RequestId: peerID})
			return r.GetErrorMessage().GetId(), err
		},
		"RejectFriend": func(f *fixture) (uint32, error) {
			r, err := f.logic().RejectFriend(playerCtx(6003), &pb.RejectFriendRequest{RequestId: peerID})
			return r.GetErrorMessage().GetId(), err
		},
		"RemoveFriend": func(f *fixture) (uint32, error) {
			r, err := f.logic().RemoveFriend(playerCtx(6004), &pb.RemoveFriendRequest{TargetPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		},
		"Block": func(f *fixture) (uint32, error) {
			r, err := f.logic().Block(playerCtx(6005), &pb.BlockRequest{TargetPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		},
		"Unblock": func(f *fixture) (uint32, error) {
			r, err := f.logic().Unblock(playerCtx(6006), &pb.UnblockRequest{TargetPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		},
		"GetFriendList": func(f *fixture) (uint32, error) {
			r, err := f.logic().GetFriendList(playerCtx(6007), &pb.GetFriendListRequest{})
			return r.GetErrorMessage().GetId(), err
		},
		"GetPendingRequests": func(f *fixture) (uint32, error) {
			r, err := f.logic().GetPendingRequests(playerCtx(6008), &pb.GetPendingRequestsRequest{})
			return r.GetErrorMessage().GetId(), err
		},
		"ListBlocks": func(f *fixture) (uint32, error) {
			r, err := f.logic().ListBlocks(playerCtx(6009), &pb.ListBlocksRequest{})
			return r.GetErrorMessage().GetId(), err
		},
	}
	roleErrors := map[string]error{
		"冻结或换主": data.ErrOriginalRole,
		"目录读不到": errors.Join(rolefence.ErrUnavailable, errors.New("data_service down")),
	}
	for method, call := range calls {
		for reason, roleErr := range roleErrors {
			t.Run(method+"/"+reason, func(t *testing.T) {
				f := newFixture(t, nil)
				f.store.fail(method, roleErr)
				id, err := call(f)
				require.NoError(t, err, "角色核不过也必须 in-band(F2-1)")
				require.Equal(t, constants.ErrRoleNotVerified, id)
				require.Equal(t, 1, f.store.calls[method], "拒绝来自业务事务里的复核,不是入口")
			})
		}
	}
}

// RecommendFriends 的在线玩家路径只读 Redis 在线目录与 data_service,不碰好友库:入口准入的验签原件是它唯一的身份要求,
// 没有归属复核(§9.3 接受的变化,只返回公开的在线推荐)。这里钉住它不调任何仓储方法,也不因此放过没有原件的调用。
func TestRecommendOnlinePathNeedsOnlyTheVerifiedOriginal(t *testing.T) {
	f := newFixture(t, nil)
	_, err := f.logic().RecommendFriends(playerCtx(6010), &pb.RecommendFriendsRequest{OnlineOnly: true})
	require.NoError(t, err)
	for method, n := range f.store.calls {
		require.Zero(t, n, "在线玩家推荐不该调仓储方法 %s", method)
	}

	resp, err := f.logic().RecommendFriends(playerCtx(0), &pb.RecommendFriendsRequest{OnlineOnly: true})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId(), "没有原件的调用照旧拒绝")
}
