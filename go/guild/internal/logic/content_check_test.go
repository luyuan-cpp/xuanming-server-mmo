package logic

import (
	"context"
	"testing"

	"guild/internal/constants"
	cgpb "proto/content_guard"
	"shared/contentguard"
)

// 帮会名与公告只有「通过」才算能用:命中、打码、不认识的结局回这一类文字的命中提示,不可达回可重试的提示。
func TestCheckTextMapsVerdictsToTips(t *testing.T) {
	ctx := context.Background()
	verdict := contentguard.Pass
	type call struct {
		scene    cgpb.TextScene
		playerID uint64
		text     string
	}
	var calls []call
	l := &GuildLogic{}
	WithContentGuard(contentguard.Func(func(_ context.Context, scene cgpb.TextScene, playerID uint64, text string) contentguard.Result {
		calls = append(calls, call{scene, playerID, text})
		return contentguard.Result{Verdict: verdict}
	}))(l)

	cases := []struct {
		name    string
		verdict contentguard.Verdict
		want    uint32 // 0 = 放行
	}{
		{"pass", contentguard.Pass, 0},
		{"reject", contentguard.Reject, constants.ErrGuildNameSensitive},
		{"masked is not usable for a name", contentguard.Masked, constants.ErrGuildNameSensitive},
		{"unavailable", contentguard.Unavailable, constants.ErrTextCheckUnavailable},
		{"zero value fails closed", contentguard.Verdict(0), constants.ErrGuildNameSensitive},
	}
	for _, c := range cases {
		verdict = c.verdict
		tip := l.checkText(ctx, cgpb.TextScene_TEXT_SCENE_GUILD_NAME, 7, "逍遥帮", constants.ErrGuildNameSensitive)
		switch {
		case c.want == 0 && tip != nil:
			t.Errorf("%s: got tip %d, want none", c.name, tip.GetId())
		case c.want != 0 && tip.GetId() != c.want:
			t.Errorf("%s: got tip %d, want %d", c.name, tip.GetId(), c.want)
		}
	}
	if len(calls) != len(cases) {
		t.Fatalf("every check must reach the guard once: %d calls for %d cases", len(calls), len(cases))
	}
	if got := calls[0]; got != (call{cgpb.TextScene_TEXT_SCENE_GUILD_NAME, 7, "逍遥帮"}) {
		t.Fatalf("guard received %+v, want the scene, the caller's player id and the text", got)
	}

	// 公告用自己的命中提示。
	verdict = contentguard.Reject
	tip := l.checkText(ctx, cgpb.TextScene_TEXT_SCENE_GUILD_ANNOUNCEMENT, 7, "公告", constants.ErrAnnouncementSensitive)
	if tip.GetId() != constants.ErrAnnouncementSensitive {
		t.Fatalf("announcement reject tip = %d, want %d", tip.GetId(), constants.ErrAnnouncementSensitive)
	}
}

func TestCheckTextSkipsEmptyTextAndMissingGuard(t *testing.T) {
	ctx := context.Background()
	called := false
	l := &GuildLogic{}
	// 没接内容安全(单测):不送检、不拦。
	if tip := l.checkText(ctx, cgpb.TextScene_TEXT_SCENE_GUILD_NAME, 1, "逍遥帮", constants.ErrGuildNameSensitive); tip != nil {
		t.Fatalf("no guard wired: tip=%v, want none", tip)
	}
	// 传 nil 被忽略,不会把字段覆盖成 nil 之后再解引用。
	WithContentGuard(nil)(l)
	WithContentGuard(contentguard.Func(func(context.Context, cgpb.TextScene, uint64, string) contentguard.Result {
		called = true
		return contentguard.Result{Verdict: contentguard.Reject}
	}))(l)
	// 清空公告(空串)不送检。
	if tip := l.checkText(ctx, cgpb.TextScene_TEXT_SCENE_GUILD_ANNOUNCEMENT, 1, "", constants.ErrAnnouncementSensitive); tip != nil || called {
		t.Fatalf("empty text must not be sent for review: tip=%v called=%v", tip, called)
	}
}

func TestContentTipsAreDistinct(t *testing.T) {
	codes := map[uint32]string{}
	for name, code := range map[string]uint32{
		"ErrGuildNameSensitive":    constants.ErrGuildNameSensitive,
		"ErrAnnouncementSensitive": constants.ErrAnnouncementSensitive,
		"ErrTextCheckUnavailable":  constants.ErrTextCheckUnavailable,
		"ErrGuildNameInvalid":      constants.ErrGuildNameInvalid,
		"ErrGuildNameTaken":        constants.ErrGuildNameTaken,
	} {
		if other, dup := codes[code]; dup {
			t.Errorf("%s and %s share tip code %d", name, other, code)
		}
		codes[code] = name
	}
}
