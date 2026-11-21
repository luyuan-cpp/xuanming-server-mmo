package server

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cgpb "proto/content_guard"

	"content_guard/internal/config"
	"content_guard/internal/engine"
	"content_guard/internal/platform"
	"content_guard/internal/wordlist"
)

func newServer(requireLexicon bool) (*Server, *engine.Engine) {
	e := engine.New()
	return New(e, requireLexicon, 64), e
}

func check(t *testing.T, s *Server, scene cgpb.TextScene, text string) *cgpb.CheckTextResponse {
	t.Helper()
	out, err := s.CheckText(context.Background(), &cgpb.CheckTextRequest{Scene: scene, Text: text})
	if err != nil {
		t.Fatalf("CheckText(%v, %q): %v", scene, text, err)
	}
	return out
}

// 协议里每个场景都必须有动作大类:漏了映射的场景会被当成非法请求拒掉,调用方的这个入口就永远用不了。
func TestEveryProtoSceneIsMapped(t *testing.T) {
	names := map[string]bool{}
	for value := range cgpb.TextScene_name {
		scene := cgpb.TextScene(value)
		_, name, ok := classOf(scene)
		if scene == cgpb.TextScene_TEXT_SCENE_UNSPECIFIED {
			if ok {
				t.Errorf("TEXT_SCENE_UNSPECIFIED must not be accepted")
			}
			continue
		}
		if !ok {
			t.Errorf("%v has no scene class in classOf", scene)
			continue
		}
		if name == "" || name == "unknown" || names[name] {
			t.Errorf("%v has a missing or duplicate metric name %q", scene, name)
		}
		names[name] = true
	}
}

// 核心包的类别值直接转成协议枚举,两边的数值和名字必须逐个相同。
func TestCategoryValuesMatchProto(t *testing.T) {
	for value, protoName := range cgpb.TextCategory_name {
		got := wordlist.Category(value).String()
		want := "none"
		if value != 0 {
			want = strings.ToLower(strings.TrimPrefix(protoName, "TEXT_CATEGORY_"))
		}
		if got != want {
			t.Errorf("category %d: wordlist says %q, proto says %q", value, got, want)
		}
	}
	// 反过来:核心包里有名字的类别在协议里都得有。
	for _, name := range []string{"politics", "porn", "abuse", "ad", "impersonation", "other"} {
		category, ok := wordlist.ParseCategory(name)
		if !ok {
			t.Fatalf("wordlist lost category %q", name)
		}
		if _, exists := cgpb.TextCategory_name[int32(category)]; !exists {
			t.Errorf("category %q (%d) is missing from the proto enum", name, category)
		}
	}
}

func TestFullMethodNameMatchesGeneratedConstant(t *testing.T) {
	if config.CheckTextFullMethod != cgpb.ContentGuard_CheckText_FullMethodName {
		t.Fatalf("config.CheckTextFullMethod = %q, generated = %q", config.CheckTextFullMethod, cgpb.ContentGuard_CheckText_FullMethodName)
	}
	if config.CheckPlatformTextFullMethod != cgpb.ContentGuard_CheckPlatformText_FullMethodName {
		t.Fatalf("config.CheckPlatformTextFullMethod = %q, generated = %q", config.CheckPlatformTextFullMethod, cgpb.ContentGuard_CheckPlatformText_FullMethodName)
	}
}

func TestCheckTextVerdicts(t *testing.T) {
	s, e := newServer(false)
	e.Swap(5, &wordlist.Lexicon{Deny: []wordlist.Entry{
		{Word: "笨蛋", Category: wordlist.CategoryAbuse, Mode: wordlist.ModeSub},
		{Word: "加微信", Category: wordlist.CategoryAd, Mode: wordlist.ModeSub},
	}})

	pass := check(t, s, cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, "一起刷副本")
	if pass.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_PASS || pass.GetMaskedText() != "" ||
		pass.GetCategory() != cgpb.TextCategory_TEXT_CATEGORY_UNSPECIFIED || pass.GetLexiconVersion() != 5 {
		t.Fatalf("pass: %+v", pass)
	}
	masked := check(t, s, cgpb.TextScene_TEXT_SCENE_CHAT_PRIVATE, "你个笨蛋")
	if masked.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_MASKED || masked.GetMaskedText() != "你个**" ||
		masked.GetCategory() != cgpb.TextCategory_TEXT_CATEGORY_ABUSE {
		t.Fatalf("masked: %+v", masked)
	}
	reject := check(t, s, cgpb.TextScene_TEXT_SCENE_CHAT_TEAM, "加微信")
	if reject.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_REJECT || reject.GetMaskedText() != "" ||
		reject.GetCategory() != cgpb.TextCategory_TEXT_CATEGORY_AD {
		t.Fatalf("reject: %+v", reject)
	}
	// 同一个词在名字里是拒绝,不是打码。
	name := check(t, s, cgpb.TextScene_TEXT_SCENE_ROLE_NAME, "大笨蛋")
	if name.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_REJECT {
		t.Fatalf("name: %+v", name)
	}
	// 寄售标题全服可见:辱骂与广告引流均按拒绝处理,不打码放行。
	tradeAbuse := check(t, s, cgpb.TextScene_TEXT_SCENE_TRADE_TITLE, "大笨蛋")
	if tradeAbuse.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_REJECT {
		t.Fatalf("tradeAbuse: %+v", tradeAbuse)
	}
	// 公告里冒充官方那一类不管。
	notice := check(t, s, cgpb.TextScene_TEXT_SCENE_GUILD_ANNOUNCEMENT, "官方活动今晚开")
	if notice.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_PASS {
		t.Fatalf("notice: %+v", notice)
	}
}

func TestCheckTextRejectsBadRequests(t *testing.T) {
	s, _ := newServer(false)
	cases := map[string]*cgpb.CheckTextRequest{
		"unspecified scene": {Scene: cgpb.TextScene_TEXT_SCENE_UNSPECIFIED, Text: "x"},
		"unknown scene":     {Scene: cgpb.TextScene(9999), Text: "x"},
		"text too long":     {Scene: cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, Text: strings.Repeat("a", 65)},
	}
	for name, request := range cases {
		_, err := s.CheckText(context.Background(), request)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument", name, status.Code(err))
		}
	}
	// 上限是闭区间:正好等于上限的要放行。
	if _, err := s.CheckText(context.Background(), &cgpb.CheckTextRequest{Scene: cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, Text: strings.Repeat("a", 64)}); err != nil {
		t.Fatalf("text exactly at the limit must be accepted: %v", err)
	}
}

func TestRequireLexiconFailsClosedUntilLoaded(t *testing.T) {
	s, e := newServer(true)
	if s.Ready() {
		t.Fatalf("server must not be ready before a lexicon is loaded")
	}
	_, err := s.CheckText(context.Background(), &cgpb.CheckTextRequest{Scene: cgpb.TextScene_TEXT_SCENE_ROLE_NAME, Text: "逍遥子"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", status.Code(err))
	}
	e.Swap(1, &wordlist.Lexicon{})
	if !s.Ready() {
		t.Fatalf("server must be ready after a lexicon is loaded")
	}
	if out := check(t, s, cgpb.TextScene_TEXT_SCENE_ROLE_NAME, "逍遥子"); out.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_PASS {
		t.Fatalf("after load: %+v", out)
	}
}

func TestWithoutRequireLexiconBuiltinWordsStillApply(t *testing.T) {
	s, _ := newServer(false)
	if !s.Ready() {
		t.Fatalf("server without RequireLexicon is always ready")
	}
	out := check(t, s, cgpb.TextScene_TEXT_SCENE_GUILD_NAME, "官方帮会")
	if out.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_REJECT || out.GetCategory() != cgpb.TextCategory_TEXT_CATEGORY_IMPERSONATION || out.GetLexiconVersion() != 0 {
		t.Fatalf("built-in word in a guild name: %+v", out)
	}
}

type stubReviewer struct {
	res     platform.ReviewResult
	syncRes platform.ReviewResult
}

func (r stubReviewer) Review(_ context.Context, _ cgpb.TextScene, _ uint64, _, _ string, _ bool) platform.ReviewResult {
	return r.res
}

func (r stubReviewer) ReviewSync(_ context.Context, _ cgpb.TextScene, _ uint64, _, _ string) platform.ReviewResult {
	if r.syncRes.Outcome != platform.OutcomeSkip || r.syncRes.Category != cgpb.TextCategory_TEXT_CATEGORY_UNSPECIFIED {
		return r.syncRes
	}
	return r.res
}

func TestExternalReviewerOverridesLocalPassAndSupportsWorldAsyncRecheck(t *testing.T) {
	e := engine.New()
	rej := New(e, false, 64, WithReviewer(stubReviewer{
		res: platform.ReviewResult{Outcome: platform.OutcomeReject, Category: cgpb.TextCategory_TEXT_CATEGORY_AD},
	}))
	out := check(t, rej, cgpb.TextScene_TEXT_SCENE_ROLE_NAME, "逍遥剑仙")
	if out.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_REJECT || out.GetCategory() != cgpb.TextCategory_TEXT_CATEGORY_AD {
		t.Fatalf("external reject got %+v", out)
	}

	unavail := New(e, false, 64, WithReviewer(stubReviewer{
		res: platform.ReviewResult{Outcome: platform.OutcomeUnavailable},
	}))
	_, err := unavail.CheckText(context.Background(), &cgpb.CheckTextRequest{
		Scene:   cgpb.TextScene_TEXT_SCENE_ROLE_NAME,
		Text:    "逍遥剑仙",
		Account: "wx_abc",
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("external unavailable code = %v, want Unavailable", status.Code(err))
	}

	pendingAndReject := New(e, false, 64, WithReviewer(stubReviewer{
		res:     platform.ReviewResult{Outcome: platform.OutcomePending},
		syncRes: platform.ReviewResult{Outcome: platform.OutcomeReject, Category: cgpb.TextCategory_TEXT_CATEGORY_POLITICS},
	}))
	worldOut, err := pendingAndReject.CheckText(context.Background(), &cgpb.CheckTextRequest{
		Scene:         cgpb.TextScene_TEXT_SCENE_CHAT_WORLD,
		Text:          "正常表象内容",
		PlayerId:      7,
		AsyncPlatform: true,
	})
	if err != nil || worldOut.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_PASS || !worldOut.GetPendingPlatformReview() {
		t.Fatalf("world chat async pending got %+v err=%v", worldOut, err)
	}
	recheckOut, err := pendingAndReject.CheckPlatformText(context.Background(), &cgpb.CheckTextRequest{
		Scene:    cgpb.TextScene_TEXT_SCENE_CHAT_WORLD,
		Text:     "正常表象内容",
		PlayerId: 7,
	})
	if err != nil || recheckOut.GetVerdict() != cgpb.TextVerdict_TEXT_VERDICT_REJECT || recheckOut.GetCategory() != cgpb.TextCategory_TEXT_CATEGORY_POLITICS {
		t.Fatalf("CheckPlatformText got %+v err=%v", recheckOut, err)
	}
}
