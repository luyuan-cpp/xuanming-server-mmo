package constants

import (
	"testing"

	"shared/generated/tip"
	"shared/serverbase"
)

func TestJianghuTipCodesStayInJianghuSegmentAndAreRecognized(t *testing.T) {
	var seg tip.Segment
	found := false
	for _, s := range tip.Segments {
		if s.Domain == "jianghu" {
			seg = s
			found = true
			break
		}
	}
	if !found {
		t.Fatal("生成的段表里没有 jianghu 段")
	}
	codes := map[string]uint32{
		"ErrJianghuDuelDisabled":             ErrJianghuDuelDisabled,
		"ErrJianghuBlockedByPeer":            ErrJianghuBlockedByPeer,
		"ErrJianghuTargetRefusesStrangers":   ErrJianghuTargetRefusesStrangers,
		"ErrJianghuSelfRefusesStrangers":     ErrJianghuSelfRefusesStrangers,
		"ErrJianghuSameTargetCooldown":       ErrJianghuSameTargetCooldown,
		"ErrJianghuDailyInviteLimitReached":  ErrJianghuDailyInviteLimitReached,
		"ErrJianghuTeamSizeMismatch":         ErrJianghuTeamSizeMismatch,
		"ErrJianghuTeamChangedAborted":       ErrJianghuTeamChangedAborted,
		"ErrJianghuInsufficientWritOrSilver": ErrJianghuInsufficientWritOrSilver,
		"ErrJianghuInvitePendingOrExpired":   ErrJianghuInvitePendingOrExpired,
		"ErrJianghuNotInviteParticipant":     ErrJianghuNotInviteParticipant,
		"ErrJianghuNoActiveDefeatMark":       ErrJianghuNoActiveDefeatMark,
		"ErrJianghuGrudgeNotFound":           ErrJianghuGrudgeNotFound,
		"ErrJianghuServiceBusy":              ErrJianghuServiceBusy,
	}
	for name, code := range codes {
		if code < seg.Base || code >= seg.Base+seg.Width {
			t.Errorf("%s = %d 落在 jianghu 段 [%d,%d) 之外", name, code, seg.Base, seg.Base+seg.Width)
		}
		if serverbase.TipVerdict(code) == serverbase.VerdictUnknown {
			t.Errorf("%s = %d 未被生成段表识别", name, code)
		}
	}
}
