package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	base "proto/common/base"
	databasepb "proto/common/database"
	storepb "proto/player_store"
)

// 身份一次写全:名字、外观、性别在 kProfile,职业在 kHot,字段没有写错行。写权行没有 payload,新角色不算在线。
func TestInitialPlayerStoreRowsCarryTheIdentity(t *testing.T) {
	role := &base.AccountSimplePlayer{PlayerId: 7, ClassId: 3, Gender: 2, ZoneId: 1, Name: "初始", AppearanceId: "look"}

	rows, err := initialPlayerStoreRows(role)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	require.Equal(t, storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER, rows[0].kind)
	require.EqualValues(t, 1, rows[0].released)
	require.NotNil(t, rows[0].payload, "payload 列 NOT NULL,nil 会被驱动写成 NULL")
	require.Empty(t, rows[0].payload)

	require.Equal(t, storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT, rows[1].kind)
	var hot databasepb.PlayerDatabase
	require.NoError(t, proto.Unmarshal(rows[1].payload, &hot))
	require.EqualValues(t, 3, hot.GetUint32PbComponent().GetClass())
	require.Nil(t, hot.GetProfileComponent())

	require.Equal(t, storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE, rows[2].kind)
	var profile databasepb.PlayerDatabase
	require.NoError(t, proto.Unmarshal(rows[2].payload, &profile))
	require.Equal(t, "初始", profile.GetProfileComponent().GetName())
	require.Equal(t, "look", profile.GetProfileComponent().GetAppearanceId())
	require.EqualValues(t, 2, profile.GetProfileComponent().GetGender())
	require.Nil(t, profile.GetUint32PbComponent())
	require.Zero(t, profile.GetPlayerId(), "player_id 是元数据,不进 payload")
}
