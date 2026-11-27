package logic

import (
	"testing"

	battlepb "proto/battle"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
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

func TestRankedV7LiveSpectateVisibilitySummaryAndRematchTTL(t *testing.T) {
	// 高段正式自养排位允许公开实时围观；低段/免费预设/活动局保持私密。
	require.Equal(t, battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PUBLIC,
		spectateVisibilityForRankedBattle(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, nil, nil,
			&battlepb.RankedMatchContext{SeasonId: 1, TeamSize: 1, UseFreePreset: false, AllowPublicSpectate: true, PeakRatingCenti: 165000}))
	require.Equal(t, battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE,
		spectateVisibilityForRankedBattle(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, nil, nil,
			&battlepb.RankedMatchContext{SeasonId: 1, TeamSize: 1, UseFreePreset: false, AllowPublicSpectate: false, PeakRatingCenti: 148000}))
	require.Equal(t, battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE,
		spectateVisibilityForRankedBattle(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, nil, nil,
			&battlepb.RankedMatchContext{SeasonId: 1, TeamSize: 1, UseFreePreset: true, AllowPublicSpectate: true, PeakRatingCenti: 180000}))
	require.Equal(t, battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PRIVATE,
		spectateVisibilityForRankedBattle(matchpb.MatchMode_MATCH_MODE_PVP_TEAM,
			&battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST}, nil,
			&battlepb.RankedMatchContext{SeasonId: 1, TeamSize: 1, UseFreePreset: false, AllowPublicSpectate: true, PeakRatingCenti: 180000}))

	// 观战摘要透传正式排位人数档与最高分。
	rawCreate, err := proto.Marshal(&battlepb.CreateBattleRequest{
		BattleId:           77001,
		MatchMode:          uint32(matchpb.MatchMode_MATCH_MODE_PVP_TEAM),
		SpectateVisibility: battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PUBLIC,
		Players: []*battlepb.BattlePlayerSnapshot{
			{PlayerId: 101, PlayerName: "青云"},
			{PlayerId: 201, PlayerName: "玄烈"},
		},
		RankedContext: &battlepb.RankedMatchContext{
			SeasonId:            1,
			TeamSize:            1,
			UseFreePreset:       false,
			AllowPublicSpectate: true,
			PeakRatingCenti:     178500,
		},
	})
	require.NoError(t, err)
	rec, err := spectateRecordFromFormation(&matchpb.MatchFormationRecord{
		BattleId:      77001,
		Mode:          uint32(matchpb.MatchMode_MATCH_MODE_PVP_TEAM),
		ConfigId:      5,
		CreatedMs:     123456,
		CreateRequest: rawCreate,
	})
	require.NoError(t, err)
	require.Equal(t, battlepb.ESpectateVisibility_SPECTATE_VISIBILITY_PUBLIC, rec.GetVisibility())
	require.Equal(t, uint32(1), rec.GetSummary().GetRankedTeamSize())
	require.Equal(t, int64(178500), rec.GetSummary().GetRankedPeakRatingCenti())

	// 不服再战切磋 TTL 收缩至 15 秒，普通切磋保持 60 秒。
	require.Equal(t, 60, challengeTTLSecondsFor(60, nil))
	require.Equal(t, 60, challengeTTLSecondsFor(60, &battlepb.CustomDuelRules{AllowPublicSpectate: true}))
	require.Equal(t, 15, challengeTTLSecondsFor(60, &battlepb.CustomDuelRules{RematchSourceBattleId: 77001}))
	require.Equal(t, 10, challengeTTLSecondsFor(10, &battlepb.CustomDuelRules{RematchSourceBattleId: 77001}))
}
