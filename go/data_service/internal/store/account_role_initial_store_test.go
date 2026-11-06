package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	databasepb "proto/common/database"
	storepb "proto/player_store"
)

// 身份一次写全:职业、名字、外观、性别都在 kProfile(player-save-direct-commit.md §1.1),没有别的数据行。
// 写权行没有 payload,新角色不算在线。
func TestInitialPlayerStoreRowsCarryTheIdentity(t *testing.T) {
	role := &base.AccountSimplePlayer{PlayerId: 7, ClassId: 3, Gender: 2, ZoneId: 1, Name: "初始", AppearanceId: "look"}

	rows, err := initialPlayerStoreRows(role)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	require.Equal(t, storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER, rows[0].kind)
	require.EqualValues(t, 1, rows[0].released)
	require.NotNil(t, rows[0].payload, "payload 列 NOT NULL,nil 会被驱动写成 NULL")
	require.Empty(t, rows[0].payload)

	require.Equal(t, storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE, rows[1].kind)
	var profile databasepb.PlayerDatabase
	require.NoError(t, proto.Unmarshal(rows[1].payload, &profile))
	require.EqualValues(t, 3, profile.GetUint32PbComponent().GetClass())
	require.Equal(t, "初始", profile.GetProfileComponent().GetName())
	require.Equal(t, "look", profile.GetProfileComponent().GetAppearanceId())
	require.EqualValues(t, 2, profile.GetProfileComponent().GetGender())
	require.Zero(t, profile.GetUint64PbComponent().GetRegistrationTimestamp(), "注册时间留空,首次进场由 scene 判新号")
	require.Nil(t, profile.GetLevelComponent(), "等级在 kCharacter,建角不写")
	require.Zero(t, profile.GetPlayerId(), "player_id 是元数据,不进 payload")
}
