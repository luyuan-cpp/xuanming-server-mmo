package team

// 人数档真人 PVP 的组队人数规则(pvp_party_rules.go,设计文档 global-match-hierarchical-pools.md 开头第 9 条)。

import (
	"testing"

	matchpb "proto/match"
)

func TestCheckPvpPartySize(t *testing.T) {
	pvp, pve := matchpb.MatchMode_MATCH_MODE_PVP_TEAM, matchpb.MatchMode_MATCH_MODE_PVE_TEAM
	cases := []struct {
		name             string
		mode             matchpb.MatchMode
		perSide, members uint32
		want             uint32
	}{
		{"5V5 不收 4 人队", pvp, 5, 4, ErrPvpPartySize},
		{"5V5 收 3 人队", pvp, 5, 3, 0},
		{"5V5 收满 5 人", pvp, 5, 5, 0},
		{"5V5 收双排", pvp, 5, 2, 0},
		{"4 人队可以打 4V4", pvp, 4, 4, 0},
		{"4V4 收 3 人队", pvp, 4, 3, 0},
		{"PVE 组队不受限", pve, 5, 4, 0},
	}
	for _, c := range cases {
		if got := CheckPvpPartySize(c.mode, c.perSide, c.members); got != c.want {
			t.Errorf("%s: CheckPvpPartySize(%s, %d, %d) = %d, want %d", c.name, c.mode, c.perSide, c.members, got, c.want)
		}
	}
}
