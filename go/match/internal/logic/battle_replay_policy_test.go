package logic

import (
	"testing"

	matchpb "proto/match"
)

// 每个 MatchMode 枚举值都必须在决策表里显式占一行(同 TestSpectateVisibilityDecidesEveryMatchMode)。
func TestReplayPolicyDecidesEveryMatchMode(t *testing.T) {
	for value, name := range matchpb.MatchMode_name {
		if _, decided := replayPolicyOfMode(matchpb.MatchMode(value)); !decided {
			t.Errorf("MatchMode %s(%d) 不在战报录制决策表里", name, value)
		}
	}
}

// 负责人 2026-11-06 定:只录 PVP。
func TestRecordReplayOnlyPVP(t *testing.T) {
	cases := map[matchpb.MatchMode]bool{
		matchpb.MatchMode_MATCH_MODE_1V1:           true,
		matchpb.MatchMode_MATCH_MODE_5V5:           true,
		matchpb.MatchMode_MATCH_MODE_PVP_TEAM:      true,
		matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE: true,
		matchpb.MatchMode_MATCH_MODE_3V3:           true,
		matchpb.MatchMode_MATCH_MODE_PVE_SOLO:      false,
		matchpb.MatchMode_MATCH_MODE_PVE_TEAM:      false,
		matchpb.MatchMode_MATCH_MODE_UNSPECIFIED:   false,
	}
	for mode, want := range cases {
		if got := recordReplayFor(true, mode); got != want {
			t.Errorf("recordReplayFor(true, %s) = %v, want %v", mode, got, want)
		}
	}
}

// 全局开关关掉时一律不录;表外的值不录。
func TestRecordReplayDisabledOrUnknownMode(t *testing.T) {
	for value := range matchpb.MatchMode_name {
		if recordReplayFor(false, matchpb.MatchMode(value)) {
			t.Errorf("开关关闭时 %d 仍被录制", value)
		}
	}
	if recordReplayFor(true, matchpb.MatchMode(9999)) {
		t.Error("表外的模式不应录制")
	}
}
