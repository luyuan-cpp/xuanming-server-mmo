package logic

import (
	"sort"
	"testing"

	"match/internal/catalog"
	"match/internal/svc"
	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

// useTestCatalog 给单测装一份模板目录(生产目录在启动时由 catalog.Load 从 PlayMode 表编译,这里是测试替身)。
//
//   - PVE_TEAM 按 pveTeam 给的人数上限开放(上限 0 的模板不开放组队);
//   - 其余入口(PVE_SOLO、1V1、5V5、PVP_TEAM 每侧 5 人、切磋)对模板 0 与 pveTeam 里的各模板全部开放
//     (PVE_SOLO 必须引用有怪物组的副本,模板 0 除外),等价于改表前"只有 PVE_TEAM 有人数白名单"的测试口径。
//
// 重复调用即"换一份表"(相当于改表后重启),供测试上限调整前后的行为。
func useTestCatalog(t *testing.T, svcCtx *svc.ServiceContext, pveTeam map[uint32]uint32) {
	t.Helper()
	configs := []uint32{0}
	for config := range pveTeam {
		if config != 0 {
			configs = append(configs, config)
		}
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i] < configs[j] })

	var rows catalog.Rows
	add := func(mode matchpb.MatchMode, config, size uint32) {
		rows.PlayModes = append(rows.PlayModes, catalog.PlayModeRow{Id: uint32(len(rows.PlayModes) + 1), Mode: mode, ConfigId: config, Enabled: true, TeamSizeMax: size})
	}
	for _, config := range configs {
		if config != 0 {
			rows.Dungeons = append(rows.Dungeons, catalog.DungeonRow{Id: config, MaxTeamSize: kMaxBattleTeamSize, MonsterCount: 1})
			add(matchpb.MatchMode_MATCH_MODE_PVE_SOLO, config, 1)
			if size := pveTeam[config]; size > 0 {
				add(matchpb.MatchMode_MATCH_MODE_PVE_TEAM, config, size)
			}
		}
		add(matchpb.MatchMode_MATCH_MODE_1V1, config, 0)
		add(matchpb.MatchMode_MATCH_MODE_5V5, config, 0)
		add(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, config, kMaxBattleTeamSize)
		add(matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, config, 0)
	}
	c, err := catalog.New(rows)
	require.NoError(t, err)
	svcCtx.Catalog = c
}
