package engine

import (
	"encoding/json"
	"os"
	"testing"

	"content_guard/internal/wordlist"
)

// vectorFile 是 testdata/vectors.json 的形状。这份文件是两版服务器共用的行为约定,改它要同步 Java 版。
type vectorFile struct {
	Deny []struct {
		Word     string `json:"word"`
		Category string `json:"category"`
		Mode     string `json:"mode"`
	} `json:"deny"`
	Allow []string `json:"allow"`
	Cases []struct {
		Name     string `json:"name"`
		Scene    string `json:"scene"`
		Text     string `json:"text"`
		Verdict  string `json:"verdict"`
		Masked   string `json:"masked"`
		Category string `json:"category"`
	} `json:"cases"`
}

func TestSharedVectors(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var vectors vectorFile
	if err = json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(vectors.Cases) == 0 {
		t.Fatalf("vectors.json has no cases")
	}
	lex := &wordlist.Lexicon{Allow: vectors.Allow}
	for _, d := range vectors.Deny {
		category, ok := wordlist.ParseCategory(d.Category)
		if !ok {
			t.Fatalf("vectors.json: unknown category %q", d.Category)
		}
		mode, ok := wordlist.ParseMode(d.Mode)
		if !ok {
			t.Fatalf("vectors.json: unknown mode %q", d.Mode)
		}
		lex.Deny = append(lex.Deny, wordlist.Entry{Word: d.Word, Category: category, Mode: mode})
	}
	e := New()
	e.Swap(1, lex)

	classes := map[string]SceneClass{"name": SceneName, "notice": SceneNotice, "chat": SceneChat}
	verdicts := map[Verdict]string{VerdictPass: "pass", VerdictMasked: "masked", VerdictReject: "reject"}
	for _, c := range vectors.Cases {
		class, ok := classes[c.Scene]
		if !ok {
			t.Fatalf("%s: unknown scene %q", c.Name, c.Scene)
		}
		got := e.Check(class, c.Text)
		if verdicts[got.Verdict] != c.Verdict {
			t.Errorf("%s: verdict = %s, want %s", c.Name, verdicts[got.Verdict], c.Verdict)
			continue
		}
		if got.Category.String() != c.Category {
			t.Errorf("%s: category = %s, want %s", c.Name, got.Category, c.Category)
		}
		if got.Masked != c.Masked {
			t.Errorf("%s: masked = %q, want %q", c.Name, got.Masked, c.Masked)
		}
		if len(got.Masked) > len(c.Text) {
			t.Errorf("%s: masked text is longer than the original in bytes", c.Name)
		}
	}
}

func TestNewEngineHasOnlyBuiltinWords(t *testing.T) {
	e := New()
	if v := e.Current().Version; v != 0 {
		t.Fatalf("fresh engine version = %d, want 0", v)
	}
	got := e.Check(SceneName, "官方小助手")
	if got.Verdict != VerdictReject || got.Category != wordlist.CategoryImpersonation || got.Version != 0 {
		t.Fatalf("built-in word in a name: %+v", got)
	}
	if got = e.Check(SceneChat, "官方活动开始了"); got.Verdict != VerdictPass {
		t.Fatalf("built-in impersonation word must pass in chat: %+v", got)
	}
}

func TestSwapReplacesLexiconAndReportsVersion(t *testing.T) {
	e := New()
	if got := e.Check(SceneChat, "你个笨蛋"); got.Verdict != VerdictPass {
		t.Fatalf("before swap the word is unknown: %+v", got)
	}
	report := e.Swap(7, &wordlist.Lexicon{Deny: []wordlist.Entry{{Word: "笨蛋", Category: wordlist.CategoryAbuse, Mode: wordlist.ModeSub}}})
	if report.DenyWords == 0 || e.Current().Version != 7 || e.Current().Report != report {
		t.Fatalf("swap did not install the lexicon: report=%+v current=%+v", report, e.Current())
	}
	got := e.Check(SceneChat, "你个笨蛋")
	if got.Verdict != VerdictMasked || got.Masked != "你个**" || got.Version != 7 {
		t.Fatalf("after swap: %+v", got)
	}
	// 再换一份不含这个词的词库,它不再命中:替换是整体替换,不是合并。
	e.Swap(8, &wordlist.Lexicon{})
	if got = e.Check(SceneChat, "你个笨蛋"); got.Verdict != VerdictPass || got.Version != 8 {
		t.Fatalf("after second swap: %+v", got)
	}
}

func TestMaskCoversTrailingCombiningMark(t *testing.T) {
	e := New()
	e.Swap(1, &wordlist.Lexicon{Deny: []wordlist.Entry{{Word: "be", Category: wordlist.CategoryAbuse, Mode: wordlist.ModeSub}}})
	// b e ◌́ x:命中 be,紧跟的组合记号一并盖掉,不让它落在 * 上。
	got := e.Check(SceneChat, "be"+string(rune(0x0301))+"x")
	if got.Verdict != VerdictMasked || got.Masked != "***x" {
		t.Fatalf("got %+v, want masked ***x", got)
	}
}

func TestUnknownSceneClassFailsClosed(t *testing.T) {
	e := New()
	// 不认识的大类按名字类处理:冒充官方的词也拒绝。
	if got := e.Check(sceneClassEnd, "官方"); got.Verdict != VerdictReject {
		t.Fatalf("unknown scene class must reject: %+v", got)
	}
}

func TestActionTable(t *testing.T) {
	type row struct {
		class    SceneClass
		category wordlist.Category
		want     action
	}
	rows := []row{
		{SceneName, wordlist.CategoryImpersonation, actionReject},
		{SceneName, wordlist.CategoryAbuse, actionReject},
		{SceneName, wordlist.CategoryAd, actionReject},
		{SceneNotice, wordlist.CategoryImpersonation, actionIgnore},
		{SceneNotice, wordlist.CategoryAbuse, actionReject},
		{SceneNotice, wordlist.CategoryPolitics, actionReject},
		{SceneChat, wordlist.CategoryImpersonation, actionIgnore},
		{SceneChat, wordlist.CategoryAbuse, actionMask},
		{SceneChat, wordlist.CategoryPolitics, actionReject},
		{SceneChat, wordlist.CategoryPorn, actionReject},
		{SceneChat, wordlist.CategoryAd, actionReject},
		{SceneChat, wordlist.CategoryOther, actionReject},
		{ScenePrivateLabel, wordlist.CategoryImpersonation, actionReject},
		{ScenePrivateLabel, wordlist.CategoryAbuse, actionReject},
	}
	for _, r := range rows {
		if got := actionFor(r.class, r.category); got != r.want {
			t.Errorf("actionFor(%d, %s) = %d, want %d", r.class, r.category, got, r.want)
		}
	}
}

func TestContactAdDetectionAndAllowlist(t *testing.T) {
	e := New()
	e.Swap(2, &wordlist.Lexicon{Allow: []string{"13800138000"}})

	for _, bad := range []string{
		"加 v 一二三四五六",
		"微信①②③④⑤",
		"详情见 https://bad.example.com/gift",
		"欢迎加 13912345678",
	} {
		got := e.Check(SceneChat, bad)
		if got.Verdict != VerdictReject || got.Category != wordlist.CategoryAd {
			t.Errorf("%q: got %+v, want Reject Ad", bad, got)
		}
	}
	// 辱骂与引流并存时,拒绝高于打码。
	e.Swap(3, &wordlist.Lexicon{
		Deny:  []wordlist.Entry{{Word: "笨蛋", Category: wordlist.CategoryAbuse, Mode: wordlist.ModeSub}},
		Allow: []string{"13800138000"},
	})
	if got := e.Check(SceneChat, "笨蛋 加vx123456"); got.Verdict != VerdictReject || got.Category != wordlist.CategoryAd {
		t.Fatalf("combined abuse + contact ad got %+v, want Reject Ad", got)
	}
	// 白名单精确放行。
	if got := e.Check(SceneChat, "官方示例 13800138000"); got.Verdict != VerdictPass {
		t.Fatalf("allowlisted number got %+v, want Pass", got)
	}
	// 私有标签不做联系方式启发式拦截。
	if got := e.Check(ScenePrivateLabel, "加v123456方案"); got.Verdict != VerdictPass {
		t.Fatalf("private label got %+v, want Pass", got)
	}
}
