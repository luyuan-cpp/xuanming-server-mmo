package team

import (
	"testing"

	matchpb "proto/match"
	teampb "proto/team"

	"github.com/stretchr/testify/require"
)

func TestConfirmRankedPresetAndRosterReset(t *testing.T) {
	now := uint64(10_000)
	sessions := Sessions{101: SessionOnline, 102: SessionOnline, 103: SessionOnline}
	cfg := RuleConfig{AllowCrossZone: true}

	// 1. 建队 + 加入队员 102
	dCreate := Apply(CreateOp(101, 5001, 1), nil, now, sessions, cfg)
	require.Equal(t, uint32(0), dCreate.Code)
	rec := dCreate.Record

	dApply := Apply(ApplyOp(102, 1), rec, now+10, sessions, cfg)
	require.Equal(t, uint32(0), dApply.Code)
	rec = dApply.Record

	dJoin := Apply(HandleApplicationOp(101, 102, true), rec, now+20, sessions, cfg)
	require.Equal(t, uint32(0), dJoin.Code)
	rec = dJoin.Record

	// 2. 队长与队员分别确认排位预设
	d1 := Apply(ConfirmRankedPresetOp(101, 102, 202, 1), rec, now+30, sessions, cfg)
	require.Equal(t, uint32(0), d1.Code)
	require.True(t, d1.Changed)
	require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_RANKED_PRESET_CONFIRMED, d1.Reason)
	rec = d1.Record

	// 同参数重复确认幂等无变更
	dDup := Apply(ConfirmRankedPresetOp(101, 102, 202, 1), rec, now+35, sessions, cfg)
	require.Equal(t, uint32(0), dDup.Code)
	require.False(t, dDup.Changed)

	d2 := Apply(ConfirmRankedPresetOp(102, 103, 201, 1), rec, now+40, sessions, cfg)
	require.Equal(t, uint32(0), d2.Code)
	require.True(t, d2.Changed)
	rec = d2.Record

	for _, pid := range []uint64{101, 102} {
		m := FindMember(rec, pid)
		require.NotNil(t, m)
		require.True(t, m.GetRankedPresetConfirmed())
	}

	// 3. 新成员加入使既有全员确认态失效,但保留各自上次预设选择
	dApply3 := Apply(ApplyOp(103, 1), rec, now+50, sessions, cfg)
	rec = dApply3.Record
	dJoin3 := Apply(HandleApplicationOp(101, 103, true), rec, now+60, sessions, cfg)
	require.Equal(t, uint32(0), dJoin3.Code)
	rec = dJoin3.Record
	m101 := FindMember(rec, 101)
	require.False(t, m101.GetRankedPresetConfirmed())
	require.Equal(t, uint32(102), m101.GetRankedCharacterPresetId())
	require.Equal(t, uint32(202), m101.GetRankedPetPresetId())

	// 4. 全员重新确认后踢人,留队成员确认态再次失效
	for _, pid := range []uint64{101, 102, 103} {
		d := Apply(ConfirmRankedPresetOp(pid, 101, 203, 1), rec, now+70, sessions, cfg)
		require.Equal(t, uint32(0), d.Code)
		rec = d.Record
	}
	dKick := Apply(KickOp(101, 103), rec, now+80, sessions, cfg)
	require.Equal(t, uint32(0), dKick.Code)
	rec = dKick.Record
	for _, m := range rec.GetMembers() {
		require.False(t, m.GetRankedPresetConfirmed())
	}

	// 5. 四人队规则: 可打 4V4, 5V5 拒绝
	require.Equal(t, uint32(0), CheckPvpPartySize(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, 4, 4))
	require.Equal(t, ErrPvpPartySize, CheckPvpPartySize(matchpb.MatchMode_MATCH_MODE_PVP_TEAM, 5, 4))
}
