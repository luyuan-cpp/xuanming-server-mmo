package logic

// 删角收尾提交之后的推送与摘要 bump(role_fence_purge.go;不连库,数据库部分见 data 的 TestTiDBRoleFencePurge*)。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/data"
	pb "proto/guild"
)

func TestAfterRolePurgeNotifiesRemainingMembers(t *testing.T) {
	cases := []struct {
		name   string
		res    data.RolePurgeResult
		kind   pb.GuildChangeKind
		target uint64
	}{
		{"普通成员按退帮推送", data.RolePurgeResult{Player: 22, GuildID: 44, ZoneID: 1, Members: []uint64{11, 23}},
			pb.GuildChangeKind_GUILD_CHANGE_KIND_MEMBER_LEFT, 22},
		{"帮主让位按转让推送", data.RolePurgeResult{Player: 11, GuildID: 44, ZoneID: 1, Successor: 24, Members: []uint64{22, 23, 24}},
			pb.GuildChangeKind_GUILD_CHANGE_KIND_LEADER_TRANSFERRED, 24},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notifier, bumper := &recordingNotifier{}, &recordingSyncBumper{}
			l := NewGuildLogic(nil, nil, nil, nil, WithNotifier(notifier), WithSyncBumper(bumper))

			l.afterRolePurge(context.Background(), tc.res)

			require.Equal(t, 1, notifier.total())
			pushes := notifier.pushesOf(tc.kind)
			require.Len(t, pushes, 1)
			assert.Equal(t, tc.res.GuildID, pushes[0].change.GetGuildId())
			assert.Equal(t, tc.res.Player, pushes[0].change.GetActorPlayerId())
			assert.Equal(t, tc.target, pushes[0].change.GetTargetPlayerId())
			assert.Equal(t, tc.res.Members, pushes[0].recipients, "被删的人不在收件人里")
			require.Equal(t, 1, bumper.count())
			assert.Equal(t, tc.res.Members, bumper.playersOf(syncGuild))
		})
	}

	t.Run("单人帮解散什么都不发", func(t *testing.T) {
		notifier, bumper := &recordingNotifier{}, &recordingSyncBumper{}
		l := NewGuildLogic(nil, nil, nil, nil, WithNotifier(notifier), WithSyncBumper(bumper))

		l.afterRolePurge(context.Background(), data.RolePurgeResult{Player: 11, GuildID: 44, ZoneID: 1, Disbanded: true})

		assert.Zero(t, notifier.total())
		assert.Zero(t, bumper.count())
	})
}
