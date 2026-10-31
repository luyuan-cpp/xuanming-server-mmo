package data

// 解散对「已不是本帮成员」的区分(docs/design/client-request-ordering.md O14 / §6.4,批次 E2;真库,GUILD_TEST_MYSQL_DSN 未设即 Skip)。
//
// 带 expected_guild_id 的晚到解散可能在预检之后、锁住 guild 行之前,操作者已经转让并离开了这个帮。repo 必须在同一把 guild 行锁下
// 答出「他已不是成员」(ErrNotGuildMember,logic 回 kGuildNotInGuild),而不是「他不是帮主」;两种拒绝都整体回滚。
// 帮里仍是普通成员的人照旧是 ErrRankTooLow(旧客户端两者都答 kGuildNotLeader,见 logic.DisbandGuild)。
//
// 选中本文件:`go test ./internal/data -run TestDisbandGuildRefusal -count=1 -v`。

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"guild/internal/constants"
)

func TestDisbandGuildRefusalDistinguishesFormerMember(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)
	const (
		guildID      uint64 = 7511
		otherGuild   uint64 = 7512
		formerLeader uint64 = 8511 // 原帮主:已把帮主交给 heir 并离帮,现在是 otherGuild 的成员
		heir         uint64 = 8512
		member       uint64 = 8513
		otherLeader  uint64 = 8514
		zone         uint32 = 3
	)
	seedManagedGuild(t, ctx, db, guildID, zone, 1, 50, formerLeader, map[uint64]uint32{
		heir: constants.RoleMember, member: constants.RoleMember,
	})
	seedManagedGuild(t, ctx, db, otherGuild, zone, 1, 50, otherLeader, nil)
	// 转让 + 离帮 + 入别帮,直写 SQL(被测的只有解散这一步)。
	mustExec(t, ctx, db, "UPDATE guild SET leader_id = ? WHERE guild_id = ?", heir, guildID)
	mustExec(t, ctx, db, "UPDATE guild_member SET role = ? WHERE guild_id = ? AND player_id = ?", constants.RoleLeader, guildID, heir)
	mustExec(t, ctx, db, "DELETE FROM guild_member WHERE guild_id = ? AND player_id = ?", guildID, formerLeader)
	seedMemberRow(t, ctx, db, otherGuild, formerLeader, constants.RoleMember)

	_, err := repo.DisbandGuild(ctx, guildID, formerLeader, testNowMs)
	assert.ErrorIs(t, err, ErrNotGuildMember, "已不是本帮成员:要答「不在该帮」,不是「不是帮主」")
	assert.NotErrorIs(t, err, ErrRankTooLow)

	_, err = repo.DisbandGuild(ctx, guildID, member, testNowMs)
	assert.ErrorIs(t, err, ErrRankTooLow, "仍是本帮成员、只是不是帮主:照旧")
	assert.NotErrorIs(t, err, ErrNotGuildMember)

	assert.Equal(t, 1, guildRowCount(t, ctx, db, guildID), "被拒的解散整体回滚")
	assert.Equal(t, 1, guildRowCount(t, ctx, db, otherGuild), "别的帮原封不动")
	assert.Equal(t, heir, leaderIDOf(t, ctx, db, guildID))
	for _, p := range []uint64{heir, member} {
		_, found := memberRoleOf(t, ctx, db, guildID, p)
		assert.True(t, found, "player %d 仍在本帮", p)
	}
	role, found := memberRoleOf(t, ctx, db, otherGuild, formerLeader)
	assert.True(t, found)
	assert.Equal(t, constants.RoleMember, role)

	// 新帮主照常能解散:区分只发生在拒绝分支,成功路径不变。
	res, err := repo.DisbandGuild(ctx, guildID, heir, testNowMs)
	assert.NoError(t, err)
	assert.Equal(t, zone, res.ZoneID)
	assert.Zero(t, guildRowCount(t, ctx, db, guildID))
}
