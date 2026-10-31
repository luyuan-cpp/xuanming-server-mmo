package logic

// 成局原件的分队(gatherTeamAssignment):PVP_TEAM 的分边在提交点写进原件,这里只校验、不重算;其余模式按成员序。
// 撮合如何凑完整两边见 global_match_test.go 与 poolgroup 包。

import (
	"testing"

	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

func pvpFormation(sides map[uint64]uint32) *matchpb.MatchFormationRecord {
	f := &matchpb.MatchFormationRecord{Mode: uint32(matchpb.MatchMode_MATCH_MODE_PVP_TEAM)}
	for player, side := range sides {
		f.Members = append(f.Members, &matchpb.MatchFormationMember{PlayerId: player, TeamIndex: side})
	}
	return f
}

func TestGatherTeamAssignmentUsesExplicitPvpSides(t *testing.T) {
	members := []uint64{76001, 76002, 76003, 76004}
	teams, err := gatherTeamAssignment(nil, pvpFormation(map[uint64]uint32{76001: 0, 76002: 1, 76003: 0, 76004: 1}), members)
	require.NoError(t, err)
	require.Equal(t, []uint32{0, 1, 0, 1}, teams, "完整分边原样送建房")
}

func TestGatherTeamAssignmentRejectsInvalidPvpSides(t *testing.T) {
	cases := []struct {
		name    string
		sides   map[uint64]uint32
		members []uint64
	}{
		{"两边人数不等", map[uint64]uint32{76101: 0, 76102: 0, 76103: 1}, []uint64{76101, 76102, 76103}},
		{"有一边没人", map[uint64]uint32{76101: 0, 76102: 0}, []uint64{76101, 76102}},
		{"队号越界", map[uint64]uint32{76101: 0, 76102: 2}, []uint64{76101, 76102}},
		{"名单里有人没分边", map[uint64]uint32{76101: 0, 76102: 1}, []uint64{76101, 76103}},
		{"名单短于原件", map[uint64]uint32{76101: 0, 76102: 1}, []uint64{76101}},
		{"玩家为 0", map[uint64]uint32{0: 0, 76102: 1}, []uint64{0, 76102}},
		{"每边超过 5 人", map[uint64]uint32{1: 0, 2: 0, 3: 0, 4: 0, 5: 0, 6: 0, 7: 1, 8: 1, 9: 1, 10: 1, 11: 1, 12: 1},
			[]uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := gatherTeamAssignment(nil, pvpFormation(c.sides), c.members)
			require.Error(t, err)
		})
	}
}

func TestGatherTeamAssignmentRejectsDuplicateFormationMember(t *testing.T) {
	f := pvpFormation(map[uint64]uint32{76201: 0})
	f.Members = append(f.Members, &matchpb.MatchFormationMember{PlayerId: 76201, TeamIndex: 1})
	_, err := gatherTeamAssignment(nil, f, []uint64{76201, 76201})
	require.Error(t, err)
}

// 其余模式走成员序:PVE 全员 0 队;切磋发起者在 0 队;1V1 按 battle_id 决定是否对调,同一局结果不变。
func TestTeamAssignmentByMode(t *testing.T) {
	pve := &matchpb.MatchFormationRecord{Mode: uint32(matchpb.MatchMode_MATCH_MODE_PVE_TEAM), BattleId: 1}
	teams, err := gatherTeamAssignment(nil, pve, []uint64{1, 2, 3})
	require.NoError(t, err)
	require.Equal(t, []uint32{0, 0, 0}, teams)

	challenge := &matchpb.MatchFormationRecord{Mode: uint32(matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE), BattleId: 1}
	teams, err = gatherTeamAssignment(nil, challenge, []uint64{1, 2})
	require.NoError(t, err)
	require.Equal(t, []uint32{0, 1}, teams)

	for battleId := uint64(1); battleId <= 64; battleId++ {
		duel := &matchpb.MatchFormationRecord{Mode: uint32(matchpb.MatchMode_MATCH_MODE_1V1), BattleId: battleId}
		teams, err = gatherTeamAssignment(nil, duel, []uint64{1, 2})
		require.NoError(t, err)
		require.Equal(t, uint32(1), teams[0]+teams[1], "两人各占一队 battle=%d", battleId)
		require.Equal(t, sideFlip(battleId), teams[0], "按 battle_id 决定是否对调 battle=%d", battleId)
	}
}
