package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	base "proto/common/base"
	"shared/accountdirectory"
)

const (
	realNameTestPI    = "1hpfml09b57f3f8185f8cb5094ea3f26278efb"
	realNameTestOther = "1hpfml00000000000000000000000000000000"
)

// pi 只写一次:相同的值重放成功,不同的值不覆盖;登录读目录同一条语句带出 pi;按玩家查走角色所属账号。
func TestTiDBAccountRealNameIsWrittenOnce(t *testing.T) {
	s, _ := accountDirectoryTiDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := s.SetRealName(ctx, "real-name", realNameTestPI)
	require.ErrorIs(t, err, accountdirectory.ErrMissing, "账号行要先由登录建出来")
	_, err = s.Read(ctx, "real-name", true)
	require.NoError(t, err)
	out, err := s.ReadWithLifecycle(ctx, "real-name", false)
	require.NoError(t, err)
	require.Empty(t, out.RealNamePi, "新账号没有实名")

	for _, pi := range []string{realNameTestPI, realNameTestPI, realNameTestOther} {
		stored, err := s.SetRealName(ctx, "real-name", pi)
		require.NoError(t, err)
		require.Equal(t, realNameTestPI, stored, "第一次写入之后库里的值不变")
	}
	out, err = s.ReadWithLifecycle(ctx, "real-name", false)
	require.NoError(t, err)
	require.Equal(t, realNameTestPI, out.RealNamePi)

	_, err = s.SetRealName(ctx, "real-name", "not-a-pi")
	require.ErrorIs(t, err, accountdirectory.ErrInvalid)

	_, err = createInitialRoleForTest(s, ctx, "real-name", &base.AccountSimplePlayer{ZoneId: 1, PlayerId: 3301})
	require.NoError(t, err)
	pi, found, err := s.RealNameOfPlayer(ctx, 3301)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, realNameTestPI, pi)
	_, found, err = s.RealNameOfPlayer(ctx, 3302)
	require.NoError(t, err)
	require.False(t, found, "目录里没有的角色")

	// 客服重置:清掉之后可以重新绑定别的实名;重放什么都不改。
	had, err := s.ResetRealName(ctx, "real-name")
	require.NoError(t, err)
	require.True(t, had)
	had, err = s.ResetRealName(ctx, "real-name")
	require.NoError(t, err)
	require.False(t, had, "重放:本来就没有 pi")
	stored, err := s.SetRealName(ctx, "real-name", realNameTestOther)
	require.NoError(t, err)
	require.Equal(t, realNameTestOther, stored, "重置之后能绑新的实名")
	_, err = s.ResetRealName(ctx, "no-such-account")
	require.ErrorIs(t, err, accountdirectory.ErrMissing)
}
