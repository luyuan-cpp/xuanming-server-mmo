package data

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	dbpb "proto/common/database"
	pb "proto/friend"
	plpb "proto/player_locator"
	"testing"
)

// 新角色先在 Scene 内存初始化等级，首次存盘前 PlayerAllData 快照可仍无等级。
// 有效在线身份不能因此从目录消失；零等级只表示展示资料未知，不能补造为 1。
func TestOnlineDirectoryKeepsOnlineNewPlayerBeforeFirstSnapshot(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 64)
	prefix := string((&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName())
	for id := uint64(1); id <= 6; id++ {
		putPlayerSession(t, mr, id, plpb.PlayerSessionState_SESSION_STATE_ONLINE, 123)
		profile := &dbpb.PlayerAllData{}
		require.NoError(t, proto.Unmarshal([]byte(directoryProfile(t, id, fmt.Sprintf("道友%d", id))), profile))
		switch id {
		case 2:
			profile.PlayerDatabaseData.LevelComponent = nil
		case 3:
			profile.PlayerDatabaseData.ProfileComponent.Name = ""
		case 4:
			profile.PlayerDatabaseData.PlayerId = 0
		case 5:
			profile.PlayerDatabaseData.PlayerId = 999
		}
		raw, err := proto.Marshal(profile)
		require.NoError(t, err)
		require.NoError(t, mr.Set(fmt.Sprintf("%s:%d", prefix, id), string(raw)))
	}
	directory := OnlineDirectory{Redis: reader.rdb, DataService: directoryDataService{zones: map[uint64]uint32{1: 1, 2: 1, 3: 1, 4: 1, 5: 1, 6: 1}}}
	response, err := directory.List(context.Background(), 1, "", 10, nil, "")
	require.NoError(t, err)
	got := map[uint64]*pb.RecommendEntry{}
	for _, entry := range response.GetCandidates() {
		got[entry.GetCandidatePlayerId()] = entry
	}
	require.Len(t, got, 2, "只允许有效新角色和已存盘角色；自身、无姓名、空/错误身份仍排除")
	require.Contains(t, got, uint64(2), "合法在线新角色不能因快照未首存被隐藏")
	require.True(t, got[2].GetIsOnline())
	require.Zero(t, got[2].GetLevel(), "未知等级必须原样返回，不伪造默认等级")
	require.Equal(t, uint32(12), got[6].GetLevel())
	friends := []*pb.FriendEntry{{FriendPlayerId: 2, IsOnline: true}}
	require.NoError(t, directory.FillFriendProfiles(context.Background(), friends))
	require.Equal(t, friends[0].GetName(), got[2].GetName())
	require.Equal(t, friends[0].GetLevel(), got[2].GetLevel())
}
