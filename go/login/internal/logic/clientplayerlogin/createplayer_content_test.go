package clientplayerloginlogic

import (
	"context"
	"testing"

	"login/internal/svc"
	cgpb "proto/content_guard"
	"shared/contentguard"
	"shared/generated/pb/table"
)

// 名字类场景只有「通过」才算能用:命中、打码、不认识的结局都按命中处理,不可达回可重试的提示。
func TestRoleNameContentTip(t *testing.T) {
	sensitive := uint32(table.LoginError_kRoleNameSensitive)
	unavailable := uint32(table.LoginError_kRoleNameCheckUnavailable)
	cases := []struct {
		name   string
		result contentguard.Result
		want   uint32 // 0 = 放行(没有提示)
	}{
		{"pass", contentguard.Result{Verdict: contentguard.Pass}, 0},
		{"reject", contentguard.Result{Verdict: contentguard.Reject, Category: cgpb.TextCategory_TEXT_CATEGORY_POLITICS}, sensitive},
		{"masked text is meaningless for a name", contentguard.Result{Verdict: contentguard.Masked, Masked: "**"}, sensitive},
		{"unavailable", contentguard.Result{Verdict: contentguard.Unavailable}, unavailable},
		{"zero value fails closed", contentguard.Result{}, sensitive},
	}
	for _, c := range cases {
		tip := roleNameContentTip(c.result, "acc")
		switch {
		case c.want == 0 && tip != nil:
			t.Errorf("%s: got tip %d, want none", c.name, tip.GetId())
		case c.want != 0 && tip.GetId() != c.want:
			t.Errorf("%s: got tip %d, want %d", c.name, tip.GetId(), c.want)
		}
	}
	if sensitive == unavailable {
		t.Fatalf("the two tips must differ: one asks the player to change the name, the other to retry later")
	}
}

func TestCheckRoleNameContentSendsOnlyPlayerChosenNames(t *testing.T) {
	type call struct {
		scene    cgpb.TextScene
		playerID uint64
		text     string
	}
	var calls []call
	guard := contentguard.Func(func(_ context.Context, scene cgpb.TextScene, playerID uint64, text string) contentguard.Result {
		calls = append(calls, call{scene, playerID, text})
		if text == "坏名字" {
			return contentguard.Result{Verdict: contentguard.Reject}
		}
		return contentguard.Result{Verdict: contentguard.Pass}
	})
	l := &CreatePlayerLogic{ctx: context.Background(), svcCtx: &svc.ServiceContext{ContentGuard: guard}}

	// 空名 = 服务端生成默认名,不送检。
	if tip := l.checkRoleNameContent("", "acc"); tip != nil || len(calls) != 0 {
		t.Fatalf("a generated name must not be sent for review: tip=%v calls=%d", tip, len(calls))
	}
	if tip := l.checkRoleNameContent("逍遥子", "acc"); tip != nil {
		t.Fatalf("a clean name must pass: tip=%v", tip)
	}
	tip := l.checkRoleNameContent("坏名字", "acc")
	if tip.GetId() != uint32(table.LoginError_kRoleNameSensitive) {
		t.Fatalf("a refused name must get the sensitive-name tip, got %v", tip)
	}
	want := []call{
		{cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 0, "逍遥子"},
		{cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 0, "坏名字"},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v (role-name scene, no player id yet, the display name)", i, calls[i], want[i])
		}
	}
}

// 直接构造 ServiceContext 的单测没有内容安全客户端:按没接处理,不能空指针。
func TestCheckRoleNameContentToleratesMissingGuard(t *testing.T) {
	l := &CreatePlayerLogic{ctx: context.Background(), svcCtx: &svc.ServiceContext{}}
	if tip := l.checkRoleNameContent("逍遥子", "acc"); tip != nil {
		t.Fatalf("no guard wired: tip=%v, want none", tip)
	}
}
