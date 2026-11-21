package data

import (
	"testing"
	"time"

	"guild/internal/constants"

	"github.com/stretchr/testify/require"
)

// 真库验证帮会改名本地事务与旧名保护表(docs/design/rename-and-content-safety.md §5)。
// GUILD_TEST_MYSQL_DSN 未设置时由 openGuildIntegrationRepo 自动 Skip。
func TestRenameGuildTransactionHoldAndCreateGuard(t *testing.T) {
	ctx, db, repo := openGuildIntegrationRepo(t)

	// 建两个帮会:1001(帮主 101,成员 102,资金 2500)与 1002(帮主 201,资金 2500)。
	seedManagedGuild(t, ctx, db, 1001, 1, 1, 30, 101, map[uint64]uint32{102: constants.RoleMember})
	seedManagedGuild(t, ctx, db, 1002, 1, 1, 30, 201, nil)
	mustExec(t, ctx, db, `UPDATE guild SET funds = 2500 WHERE guild_id IN (1001, 1002)`)

	now := testNowMs
	cooldown := 7 * 24 * time.Hour
	hold := 7 * 24 * time.Hour

	// 1. 非帮主不能改名。
	_, _, err := repo.RenameGuild(ctx, 1001, 102, "青云门", cooldown, hold, 1000, now)
	require.ErrorIs(t, err, ErrRankTooLow)

	// 2. 资金不足被拒。
	_, _, err = repo.RenameGuild(ctx, 1001, 101, "青云门", cooldown, hold, 9999, now)
	require.ErrorIs(t, err, ErrFundsInsufficient)

	// 3. 与当前名字规范化相同被拒。
	_, _, err = repo.RenameGuild(ctx, 1001, 101, "MANAGE-GUILD-1001", cooldown, hold, 1000, now)
	require.ErrorIs(t, err, ErrGuildRenameSameName)

	// 4. 与他帮现有名冲突被 uk_guild 拒。
	_, _, err = repo.RenameGuild(ctx, 1001, 101, "manage-guild-1002", cooldown, hold, 1000, now)
	require.ErrorIs(t, err, ErrGuildNameTaken)

	// 5. 帮主正常改名成功:扣资金、写 renamed_ms、旧名进入 guild_name_hold。
	snap, nextMs, err := repo.RenameGuild(ctx, 1001, 101, "青云剑宗", cooldown, hold, 1000, now)
	require.NoError(t, err)
	require.Equal(t, "青云剑宗", snap.Name)
	require.EqualValues(t, 1500, snap.Funds)
	require.EqualValues(t, now, snap.RenamedMs)
	require.EqualValues(t, now+uint64(cooldown.Milliseconds()), nextMs)

	// 6. 冷却期内再改名被拒并返回下次可改时刻。
	_, cdNext, err := repo.RenameGuild(ctx, 1001, 101, "太虚观", cooldown, hold, 1000, now+1000)
	require.ErrorIs(t, err, ErrGuildRenameCooldown)
	require.EqualValues(t, nextMs, cdNext)

	// 7. 旧名处于保护期内:他帮改名与新建帮会均被拒。
	_, _, err = repo.RenameGuild(ctx, 1002, 201, "manage-guild-1001", cooldown, hold, 1000, now+1000)
	require.ErrorIs(t, err, ErrGuildNameTaken)

	createErr := repo.CreateGuild(ctx, &GuildData{
		GuildID:      1003,
		Name:         "manage-guild-1001",
		LeaderID:     301,
		Level:        1,
		CreateTimeMs: now + 1000,
		MaxMembers:   30,
		ZoneID:       1,
		Members:      []MemberData{{PlayerID: 301, Role: constants.RoleLeader, JoinTimeMs: now + 1000, LastActiveMs: now + 1000}},
	})
	require.ErrorIs(t, createErr, ErrGuildNameTaken)

	// 8. 保护期内本帮改回旧名(冷却设为 0 验证保护归属)放行并清掉保护行。
	snapBack, _, err := repo.RenameGuild(ctx, 1001, 101, "manage-guild-1001", 0, hold, 500, now+2000)
	require.NoError(t, err)
	require.Equal(t, "manage-guild-1001", snapBack.Name)
	require.EqualValues(t, 1000, snapBack.Funds)

	// 9. "青云剑宗" 现在受 1001 保护,过期后 1002 可以接管。
	afterHold := now + 2000 + uint64(hold.Milliseconds()) + 1
	snapOther, _, err := repo.RenameGuild(ctx, 1002, 201, "青云剑宗", cooldown, hold, 1000, afterHold)
	require.NoError(t, err)
	require.Equal(t, "青云剑宗", snapOther.Name)
}
