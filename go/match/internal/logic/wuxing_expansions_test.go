package logic

import (
	"testing"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

func TestValidateCustomDuelRules(t *testing.T) {
	require.True(t, validateCustomDuelRules(nil))
	for _, limit := range []uint32{0, 8, 12, 20} {
		require.True(t, validateCustomDuelRules(&battlepb.CustomDuelRules{RoundLimitOverride: limit}),
			"round limit %d must be allowed", limit)
	}
	for _, bad := range []uint32{1, 5, 10, 15, 30, 99} {
		require.False(t, validateCustomDuelRules(&battlepb.CustomDuelRules{RoundLimitOverride: bad}),
			"round limit %d must be rejected", bad)
	}
	require.True(t, validateCustomDuelRules(&battlepb.CustomDuelRules{SkillBanSlotsPerTeam: 0}))
	require.True(t, validateCustomDuelRules(&battlepb.CustomDuelRules{SkillBanSlotsPerTeam: 3}))
	require.False(t, validateCustomDuelRules(&battlepb.CustomDuelRules{SkillBanSlotsPerTeam: 1}))
	require.False(t, validateCustomDuelRules(&battlepb.CustomDuelRules{SkillBanSlotsPerTeam: 4}))
	require.False(t, validateCustomDuelRules(&battlepb.CustomDuelRules{UseFreePreset: true, SkillBanSlotsPerTeam: 3}),
		"free preset 4-skill template must reject 3-ban")
}

func TestSpectateVisibilityForBattleCustomDuelOptIn(t *testing.T) {
	require.Equal(t, battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE,
		spectateVisibilityForBattle(matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, nil, nil))
	require.Equal(t, battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE,
		spectateVisibilityForBattle(matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, nil, &battlepb.CustomDuelRules{AllowPublicSpectate: false}))
	require.Equal(t, battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PUBLIC,
		spectateVisibilityForBattle(matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, nil, &battlepb.CustomDuelRules{AllowPublicSpectate: true}))
	// 活动局永远强制私密，哪怕携带 AllowPublicSpectate。
	require.Equal(t, battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE,
		spectateVisibilityForBattle(matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE,
			&battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST},
			&battlepb.CustomDuelRules{AllowPublicSpectate: true}))
}

func TestBuildCustomDuelFreePresetContextUnrated(t *testing.T) {
	rules := &battlepb.CustomDuelRules{
		UseFreePreset:              true,
		ChallengerCharacterPresetId: 102,
		ChallengerPetPresetId:       202,
	}
	ctx, err := buildCustomDuelFreePresetContext(nil, 1001, 2002, rules, 103, 201)
	require.NoError(t, err)
	require.Equal(t, uint32(0), ctx.SeasonId, "自定义切磋绝不计排位赛季分")
	require.True(t, ctx.UseFreePreset)
	require.Len(t, ctx.Members, 2)
	require.Equal(t, uint64(1001), ctx.Members[0].PlayerId)
	require.Equal(t, uint32(102), ctx.Members[0].Presets.CharacterPresetId)
	require.Equal(t, uint32(202), ctx.Members[0].Presets.PetPresetId)
	require.Equal(t, uint64(2002), ctx.Members[1].PlayerId)
	require.Equal(t, uint32(103), ctx.Members[1].Presets.CharacterPresetId)
	require.Equal(t, uint32(201), ctx.Members[1].Presets.PetPresetId)
}
