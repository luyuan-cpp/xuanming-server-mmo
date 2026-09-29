package logic

import (
	"context"
	"testing"

	"friend/internal/constants"
	"friend/internal/data"
	"github.com/stretchr/testify/require"
	pb "proto/friend"
)

func TestOnlineDirectoryRejectsInvalidIdentityCursorAndRate(t *testing.T) {
	f := newFixture(t, nil)
	resp, err := f.logic().RecommendFriends(context.Background(), &pb.RecommendFriendsRequest{OnlineOnly: true})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId())
	resp, err = f.logic().RecommendFriends(playerCtx(1), &pb.RecommendFriendsRequest{OnlineOnly: true, Cursor: "bad"})
	require.NoError(t, err)
	require.True(t, resp.GetOnlineDirectory())
	require.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId())
	// data_service 未配置必须明确失败，不能回落随机推荐。
	resp, err = f.logic().RecommendFriends(playerCtx(1), &pb.RecommendFriendsRequest{OnlineOnly: true})
	require.NoError(t, err)
	require.True(t, resp.GetOnlineDirectory())
	require.Equal(t, constants.ErrStorage, resp.GetErrorMessage().GetId())
	require.NoError(t, f.friendMr.Set("friend:{directory:1}", "60"))
	resp, err = f.logic().RecommendFriends(playerCtx(1), &pb.RecommendFriendsRequest{OnlineOnly: true})
	require.NoError(t, err)
	require.Equal(t, constants.ErrRateLimited, resp.GetErrorMessage().GetId())
}

func TestFriendInviteListDoesNotReportUnknownPresenceAsOffline(t *testing.T) {
	for _, failure := range []string{"redis_error", "corrupt_session", "missing_reader"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t, nil)
			f.store.friends = []data.FriendEntry{{FriendPlayerID: 8003, SinceMs: 1}}
			f.deps.Sessions = data.NewSessionReader(f.svcCtx.SharedRedis, 64)
			switch failure {
			case "redis_error":
				f.sharedMr.SetError("ERR presence storage unavailable")
			case "corrupt_session":
				require.NoError(t, f.sharedMr.Set(testPlayerSessionKey(8003), "\xff\xff\xff"))
			case "missing_reader":
				f.deps.Sessions = nil
			}
			resp, err := f.logic().GetFriendList(playerCtx(1), &pb.GetFriendListRequest{})
			require.NoError(t, err)
			require.Equal(t, constants.ErrStorage, resp.GetErrorMessage().GetId(), "未知在线状态必须明确失败，不能返回成功并让客户端禁邀")
			require.Empty(t, resp.GetFriends())
		})
	}
}
