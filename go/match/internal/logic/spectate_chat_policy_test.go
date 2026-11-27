package logic

import (
	"testing"

	"match/internal/config"

	battlepb "proto/battle"
	comp "proto/common/component"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

func TestSpectateChatPolicyDecidesEveryMatchMode(t *testing.T) {
	for value, name := range matchpb.MatchMode_name {
		_, decided := spectateChatRuleOfMode(matchpb.MatchMode(value))
		require.True(t, decided, "%s 没进观战交流决策表:在 spectateChatRuleOfMode 里显式写一行", name)
	}
	rule, decided := spectateChatRuleOfMode(unknownMatchMode())
	require.False(t, decided)
	require.Equal(t, spectateChatModeForbid, rule)
}

func TestSpectateChatPolicyFreezeMatrix(t *testing.T) {
	allow := comp.ESpectateChatPreference_SPECTATE_CHAT_PREF_ALLOW
	forbid := comp.ESpectateChatPreference_SPECTATE_CHAT_PREF_FORBID
	unset := comp.ESpectateChatPreference_SPECTATE_CHAT_PREF_UNSPECIFIED

	snap := func(id uint64, team uint32, captain bool, pref comp.ESpectateChatPreference) *battlepb.BattlePlayerSnapshot {
		return &battlepb.BattlePlayerSnapshot{
			PlayerId:         id,
			TeamIndex:        team,
			IsTeamCaptain:    captain,
			SpectateChatPref: pref,
		}
	}
	aiSnap := func(id uint64, team uint32, pref comp.ESpectateChatPreference) *battlepb.BattlePlayerSnapshot {
		return &battlepb.BattlePlayerSnapshot{
			PlayerId:         id,
			TeamIndex:        team,
			AiControlled:     true,
			SpectateChatPref: pref,
		}
	}

	defaultCfg := config.SpectateChatConf{} // 零值 = 默认开启、未设默认允许、PVP 全员一票否决

	// 1. 私密不可观战局恒 false，即使双方都开启
	require.False(t, spectateChatEnabledForBattle(defaultCfg, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, visPrivate, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, true, allow), snap(2, 1, true, allow)}))

	// 2. 全局开关 Disabled=true 恒 false（不影响正常观战可见性）
	disabledCfg := config.SpectateChatConf{Disabled: true}
	require.False(t, spectateChatEnabledForBattle(disabledCfg, matchpb.MatchMode_MATCH_MODE_PVE_SOLO, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, true, allow)}))

	// 3. 单人 PVE：只看本人偏好（未设走默认允许；显式关闭则禁止交流，但不影响观战）
	require.True(t, spectateChatEnabledForBattle(defaultCfg, matchpb.MatchMode_MATCH_MODE_PVE_SOLO, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, false, unset)}))
	require.False(t, spectateChatEnabledForBattle(defaultCfg, matchpb.MatchMode_MATCH_MODE_PVE_SOLO, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, false, forbid)}))

	// 4. 组队 PVE：以队长为准
	require.True(t, spectateChatEnabledForBattle(defaultCfg, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, true, allow), snap(2, 0, false, forbid)}))
	require.False(t, spectateChatEnabledForBattle(defaultCfg, matchpb.MatchMode_MATCH_MODE_PVE_TEAM, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, true, forbid), snap(2, 0, false, allow)}))

	// 5. 1v1 切磋 / 生死状 / 排位 PVP：一票否决（任一真人关闭即整场禁止交流）
	require.True(t, spectateChatEnabledForBattle(defaultCfg, matchpb.MatchMode_MATCH_MODE_JIANGHU_DUEL, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, true, allow), snap(2, 1, true, unset)}))
	require.False(t, spectateChatEnabledForBattle(defaultCfg, matchpb.MatchMode_MATCH_MODE_JIANGHU_DUEL, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, true, allow), snap(2, 1, true, forbid)}))

	// 6. 帮战踢馆（守方 AI 不参与投票，只看真人）
	guildWar := &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_WAR}
	require.True(t, spectateChatEnabledForBattle(defaultCfg, matchpb.MatchMode_MATCH_MODE_GUILD_WAR_SIEGE, visPublic, guildWar,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, true, allow), aiSnap(99, 1, forbid)}))

	// 7. 3v3 团队 PVP：默认全员一票否决；切 TeamPvpCaptainOnly=true 时按双方队长裁决
	team3v3 := []*battlepb.BattlePlayerSnapshot{
		snap(1, 0, true, allow), snap(2, 0, false, forbid),
		snap(3, 1, true, allow), snap(4, 1, false, allow),
	}
	require.False(t, spectateChatEnabledForBattle(defaultCfg, matchpb.MatchMode_MATCH_MODE_PVP_TEAM, visPublic, nil, team3v3))
	captainOnlyCfg := config.SpectateChatConf{TeamPvpCaptainOnly: true}
	require.True(t, spectateChatEnabledForBattle(captainOnlyCfg, matchpb.MatchMode_MATCH_MODE_PVP_TEAM, visPublic, nil, team3v3))

	// 8. 锦标赛可单独通过 TournamentDisabled 锁定禁止自由文本交流
	tournOff := config.SpectateChatConf{TournamentDisabled: true}
	require.False(t, spectateChatEnabledForBattle(tournOff, matchpb.MatchMode_MATCH_MODE_SEASON_TOURNAMENT, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, true, allow), snap(2, 1, true, allow)}))

	// 9. DefaultForbidWhenUnset=true 时未设按禁止
	defaultForbidCfg := config.SpectateChatConf{DefaultForbidWhenUnset: true}
	require.False(t, spectateChatEnabledForBattle(defaultForbidCfg, matchpb.MatchMode_MATCH_MODE_PVE_SOLO, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, false, unset)}))
	require.True(t, spectateChatEnabledForBattle(defaultForbidCfg, matchpb.MatchMode_MATCH_MODE_PVE_SOLO, visPublic, nil,
		[]*battlepb.BattlePlayerSnapshot{snap(1, 0, false, allow)}))
}
