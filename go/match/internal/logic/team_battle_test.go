package logic

// 整队开战票据域(docs/design/team-system.md §E.1 第 3、6 步)的人数口径与锁时长。
// 票据预检、launch 原件与成局经 SQL,见 authority 包与 internal/team/sql_store_integration_test.go(需要 TiDB)。

import (
	"testing"

	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

// #18:人数口径、锁时长;kMaxBattleTeamSize 与 team.Capacity 同值。
func TestTeamBattleSizeAndLockTTL(t *testing.T) {
	// team.Capacity == 5(§D.1)。logic 与 team 互不 import,两边测试各自钉住
	// 同一个字面量:改任一侧都会让测试失败,提醒同步。
	require.Equal(t, 5, kMaxBattleTeamSize)

	svcCtx, _ := newTestSvcCtx(t)
	useTestCatalog(t, svcCtx, map[uint32]uint32{1: 3, 2: 5})
	starter := NewTeamBattleStarter(svcCtx)
	pveTeam, pvpTeam := uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), uint32(matchpb.MatchMode_MATCH_MODE_PVP_TEAM)
	require.Equal(t, uint32(3), starter.TeamSizeFor(pveTeam, 1))
	require.Equal(t, uint32(kMaxBattleTeamSize), starter.TeamSizeFor(pveTeam, 2), "与 PVE_TEAM 排队同口径")
	require.Zero(t, starter.TeamSizeFor(pveTeam, 99), "PlayMode 表没有这一行 = 未开放组队")
	require.Equal(t, uint32(kMaxBattleTeamSize), starter.TeamSizeFor(pvpTeam, 1), "PVP_TEAM 是每侧人数上限")
	require.Zero(t, starter.TeamSizeFor(pvpTeam, 99), "PVP_TEAM 同样只认 PlayMode 表")
	require.Zero(t, starter.TeamSizeFor(uint32(matchpb.MatchMode_MATCH_MODE_1V1), 1), "非组队模式没有组队上限")

	// 5 人:备战预算 53s(见 matchedWorstCaseSeconds)+ 补偿 25s + 余量 10s(§E.1 第 6 步)。
	require.Equal(t, 88, starter.MatchLockTTLSeconds(5))
	require.Equal(t, matchedWorstCaseSeconds(5)+compensationWorstCaseSeconds(5)+10, starter.MatchLockTTLSeconds(5))
}
