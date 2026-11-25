package logic

import (
	"context"
	"strings"
	"testing"
	"time"

	"match/internal/catalog"
	"match/internal/scope"
	battlepb "proto/battle"
	matchpb "proto/match"
	"shared/serviceauth"

	"github.com/stretchr/testify/require"
)

func festivalBeastRequest(playerID uint64, configID uint32) *matchpb.StartActivityBattleRequest {
	return &matchpb.StartActivityBattleRequest{
		BattleConfigId:  configID,
		MemberPlayerIds: []uint64{playerID},
		RequestHigh:     0x1122334455667788,
		RequestLow:      0x99aabbccddeeff01,
		ActivityContext: &battlepb.BattleActivityContext{
			Kind:              battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST,
			InitiatorPlayerId: playerID,
			FestivalBeast: &battlepb.FestivalBeastBattleContext{
				ActivityInstanceId:      202601,
				ConfigRevision:          1,
				RewardIdentity:          "festival_beast_chunxishou_v1",
				RewardDefinitionVersion: 1,
				RewardPetTableId:        101,
				RequiredWins:            3,
				LockedPetId:             7001,
				LockedOwnerLevel:        30,
				TierId:                  3,
				RoundLimit:              12,
				CycleRounds:             3,
				FirstHeavyRound:         3,
				TelegraphLeadRounds:     1,
				HeavyDamagePercent:      180,
				BossName:                "春禧兽",
				BossHealth:              26000,
				BossPhysicalAttack:      1100,
				BossDefense:             1300,
				BossResistance:          12,
				BossSpeed:               760,
				EffectiveStartAtMs:      1795564800000,
				EffectiveEndAtMs:        1796774400000,
				ClientRequestId:         "11223344-5566-4788-99aa-bbccddeeff01",
			},
		},
	}
}

func TestFestivalBeastStartActivityValidatesAndIsolatesCaller(t *testing.T) {
	svcCtx, _ := newTestSvcCtx(t)
	guildSecret := strings.Repeat("g", 32)
	sceneSecret := strings.Repeat("s", 32)
	var err error
	svcCtx.GuildCallAuth, err = serviceauth.New(map[string]string{"guild": guildSecret})
	require.NoError(t, err)
	svcCtx.SceneCallAuth, err = serviceauth.New(map[string]string{"scene": sceneSecret})
	require.NoError(t, err)
	cat, err := catalog.New(catalog.Rows{
		Dungeons:   []catalog.DungeonRow{{Id: 111, MaxTeamSize: 1, TimeLimitSeconds: 600, RoundLimit: 12, MonsterCount: 1, BossTrialKind: 7, Difficulty: 1}},
		ZoneGroups: []scope.Range{{GroupID: 1, ZoneBegin: 1, ZoneEnd: 100}},
	})
	require.NoError(t, err)
	svcCtx.Catalog = cat

	// 1. 多人队伍、缺失锁定宠物或坏时间窗一律拒绝为 INVALID_ARGUMENT。
	twoPlayers := festivalBeastRequest(8001, 111)
	twoPlayers.MemberPlayerIds = []uint64{8001, 8002}
	resp, err := startActivityBattle(context.Background(), svcCtx, twoPlayers)
	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0)

	noPet := festivalBeastRequest(8001, 111)
	noPet.ActivityContext.FestivalBeast.LockedPetId = 0
	resp, err = startActivityBattle(context.Background(), svcCtx, noPet)
	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0)

	badWindow := festivalBeastRequest(8001, 111)
	badWindow.ActivityContext.FestivalBeast.EffectiveEndAtMs = badWindow.ActivityContext.FestivalBeast.EffectiveStartAtMs
	resp, err = startActivityBattle(context.Background(), svcCtx, badWindow)
	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0)

	// 2. guild 签名不能伪造瑞兽迎春开局（必须只认 scene 签名）。
	guildSigned := festivalBeastRequest(8001, 111)
	require.NoError(t, serviceauth.Sign(matchpb.MatchInternal_StartActivityBattle_FullMethodName, guildSigned, "guild", guildSecret, uint64(time.Now().UnixMilli())))
	resp, err = startActivityBattle(context.Background(), svcCtx, guildSigned)
	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INTERNAL, 0)

	// 3. scene 签名但 battle_config_id 不是 boss_trial_kind=7 → INVALID_ARGUMENT。
	wrongConfig := festivalBeastRequest(8001, 1)
	require.NoError(t, serviceauth.Sign(matchpb.MatchInternal_StartActivityBattle_FullMethodName, wrongConfig, "scene", sceneSecret, uint64(time.Now().UnixMilli())))
	resp, err = startActivityBattle(context.Background(), svcCtx, wrongConfig)
	requireActivityRejected(t, resp, err, matchpb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_INVALID_ARGUMENT, 0)
}

func TestCatalogRejectsFestivalBeastDungeonInPlayMode(t *testing.T) {
	_, err := catalog.New(catalog.Rows{
		Dungeons: []catalog.DungeonRow{{Id: 111, MaxTeamSize: 1, TimeLimitSeconds: 600, RoundLimit: 12, MonsterCount: 1, BossTrialKind: 7, Difficulty: 1}},
		PlayModes: []catalog.PlayModeRow{{
			Id:          999,
			Mode:        matchpb.MatchMode_MATCH_MODE_PVE_SOLO,
			ConfigId:    111,
			Enabled:     true,
			TeamSizeMax: 1,
		}},
		ZoneGroups: []scope.Range{{GroupID: 1, ZoneBegin: 1, ZoneEnd: 100}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "瑞兽迎春")
}
