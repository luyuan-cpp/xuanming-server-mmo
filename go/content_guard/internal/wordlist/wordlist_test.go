package wordlist

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"content_guard/internal/textnorm"
)

func find(lex *Lexicon, text string) []Hit {
	matcher, _ := Compile(lex)
	elems, _ := textnorm.Fold(text)
	return matcher.Find(elems)
}

func deny(word string, category Category, mode Mode) Entry {
	return Entry{Word: word, Category: category, Mode: mode}
}

func TestMarshalParseRoundTrip(t *testing.T) {
	lex := &Lexicon{
		Deny: []Entry{
			deny("加微信", CategoryAd, ModeSub),
			deny("av", CategoryPorn, ModeToken),
			deny("gm", CategoryImpersonation, ModePrefix),
		},
		Allow: []string{"臭豆腐"},
	}
	text, err := lex.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := "#content_guard_lexicon 1\nD\tad\tsub\t加微信\nD\tporn\ttoken\tav\nD\timpersonation\tprefix\tgm\nA\t臭豆腐\n"
	if string(text) != want {
		t.Fatalf("Marshal =\n%q\nwant\n%q", text, want)
	}
	parsed, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(parsed, lex) {
		t.Fatalf("round trip changed the lexicon: %+v", parsed)
	}
	// Windows 换行也要能读:词库文件可能经过人手。
	if _, err = Parse([]byte(strings.ReplaceAll(want, "\n", "\r\n"))); err != nil {
		t.Fatalf("Parse with CRLF: %v", err)
	}
}

func TestParseRejectsMalformedText(t *testing.T) {
	header := "#content_guard_lexicon 1\n"
	cases := map[string]string{
		"missing header":       "D\tad\tsub\t加微信\n",
		"future format":        "#content_guard_lexicon 2\n",
		"unknown record type":  header + "X\tfoo\n",
		"unknown category":     header + "D\tspam\tsub\tfoo\n",
		"unknown mode":         header + "D\tad\tregex\tfoo\n",
		"deny field count":     header + "D\tad\tfoo\n",
		"allow field count":    header + "A\tfoo\tbar\n",
		"empty word":           header + "D\tad\tsub\t\n",
		"word too long":        header + "D\tad\tsub\t" + strings.Repeat("字", MaxWordRunes+1) + "\n",
		"not utf8":             header + "A\t\xff\n",
		"unspecified category": header + "D\tnone\tsub\tfoo\n",
	}
	for name, text := range cases {
		if _, err := Parse([]byte(text)); err == nil {
			t.Errorf("%s: Parse accepted malformed text", name)
		}
	}
}

func TestMarshalRejectsInvalidEntries(t *testing.T) {
	bad := []Lexicon{
		{Deny: []Entry{deny("a\tb", CategoryAd, ModeSub)}},
		{Deny: []Entry{deny("ok", CategoryUnspecified, ModeSub)}},
		{Deny: []Entry{deny("ok", CategoryAd, modeEnd)}},
		{Allow: []string{""}},
	}
	for i := range bad {
		if _, err := bad[i].Marshal(); !errors.Is(err, ErrWord) {
			t.Errorf("case %d: Marshal error = %v, want ErrWord", i, err)
		}
	}
}

func TestDefaultMode(t *testing.T) {
	cases := map[string]Mode{"gm": ModeToken, "a1": ModeToken, "sex": ModeToken, "abcd": ModeSub, "微信": ModeSub, "a微": ModeSub}
	for word, want := range cases {
		if got := DefaultMode(word); got != want {
			t.Errorf("DefaultMode(%q) = %v, want %v", word, got, want)
		}
	}
}

func TestCategoryAndModeNames(t *testing.T) {
	for c := CategoryPolitics; c < categoryEnd; c++ {
		parsed, ok := ParseCategory(c.String())
		if !ok || parsed != c {
			t.Errorf("category %d does not round trip through its name %q", c, c.String())
		}
	}
	if CategoryUnspecified.String() != "none" || categoryEnd.String() != "none" {
		t.Errorf("out-of-range categories must be named none")
	}
	if _, ok := ParseCategory("none"); ok {
		t.Errorf("ParseCategory must not accept none")
	}
	for m := ModeSub; m < modeEnd; m++ {
		parsed, ok := ParseMode(m.String())
		if !ok || parsed != m {
			t.Errorf("mode %d does not round trip through its name %q", m, m.String())
		}
	}
}

func TestSubModeSkipsSeparators(t *testing.T) {
	lex := &Lexicon{Deny: []Entry{deny("加微信", CategoryAd, ModeSub)}}
	// 快0 来1 加2 空格3 微4 -5 信6 吧7
	got := find(lex, "快来加 微-信吧")
	want := []Hit{{Start: 2, End: 6, Category: CategoryAd}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hits = %+v, want %+v", got, want)
	}
	if hits := find(lex, "加班微笑信任"); hits != nil {
		t.Fatalf("other characters in between must not match: %+v", hits)
	}
}

func TestSubModeFoldsWidthCaseAndScript(t *testing.T) {
	lex := &Lexicon{Deny: []Entry{deny("发财vx", CategoryAd, ModeSub)}}
	for _, text := range []string{"发财vx", "發財ＶＸ", "发 财 V.X"} {
		if hits := find(lex, text); len(hits) != 1 || hits[0].Category != CategoryAd {
			t.Errorf("%q: hits = %+v, want one ad hit", text, hits)
		}
	}
}

func TestTokenModeNeedsBoundariesAndContiguity(t *testing.T) {
	lex := &Lexicon{Deny: []Entry{deny("av", CategoryPorn, ModeToken)}}
	matches := map[string]bool{
		"看av吗":     true,  // 两侧是汉字,算边界
		"AV":       true,  // 大小写折叠
		"av 资源":    true,  // 后面是空格
		"have fun": false, // 夹在英文单词里
		"av1":      false, // 后面紧跟数字
		"1av":      false, // 前面紧跟数字
		"a v":      false, // 整词必须连续
	}
	for text, want := range matches {
		if got := len(find(lex, text)) > 0; got != want {
			t.Errorf("%q: matched = %v, want %v", text, got, want)
		}
	}
	// 看0 a1 v2 吗3
	if got := find(lex, "看av吗"); !reflect.DeepEqual(got, []Hit{{Start: 1, End: 2, Category: CategoryPorn}}) {
		t.Fatalf("hits = %+v", got)
	}
}

func TestPrefixModeOnlyMatchesAtStart(t *testing.T) {
	// gm 是内置词(开头方式),不需要词库。
	matches := map[string]bool{
		"GM01":  true,
		"gm小明":  true,
		" gm":   true, // 开头的可跳过字符不算
		"g-m01": true, // 开头方式允许中间夹可跳过字符
		"sigma": false,
		"大gm":   false,
	}
	for text, want := range matches {
		hits := find(nil, text)
		if got := len(hits) > 0; got != want {
			t.Errorf("%q: matched = %v, want %v (%+v)", text, got, want, hits)
		}
	}
}

func TestBuiltinWordsWorkWithoutLexicon(t *testing.T) {
	// 官0 方1 客2 服3
	got := find(nil, "官方客服")
	want := []Hit{
		{Start: 0, End: 1, Category: CategoryImpersonation},
		{Start: 2, End: 3, Category: CategoryImpersonation},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hits = %+v, want %+v", got, want)
	}
}

func TestAllowListSuppressesContainedHits(t *testing.T) {
	lex := &Lexicon{
		Deny:  []Entry{deny("臭", CategoryAbuse, ModeSub)},
		Allow: []string{"臭豆腐"},
	}
	if hits := find(lex, "我爱吃臭豆腐"); hits != nil {
		t.Fatalf("hit inside an allow word must be suppressed: %+v", hits)
	}
	if hits := find(lex, "你真臭"); len(hits) != 1 {
		t.Fatalf("hit outside any allow word must stay: %+v", hits)
	}
	// 我0 爱1 吃2 臭3 豆4 腐5 ,6 你7 真8 臭9:只有后一个臭算。
	if got := find(lex, "我爱吃臭豆腐,你真臭"); !reflect.DeepEqual(got, []Hit{{Start: 9, End: 9, Category: CategoryAbuse}}) {
		t.Fatalf("hits = %+v", got)
	}
}

func TestEveryWordAtAStartIsReported(t *testing.T) {
	lex := &Lexicon{Deny: []Entry{
		deny("笨", CategoryAbuse, ModeSub),
		deny("笨蛋大王", CategoryPolitics, ModeSub),
	}}
	got := find(lex, "笨蛋大王")
	want := []Hit{
		{Start: 0, End: 0, Category: CategoryAbuse},
		{Start: 0, End: 3, Category: CategoryPolitics},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hits = %+v, want %+v", got, want)
	}
}

func TestExpandedCharacterMapsBackToOneOriginal(t *testing.T) {
	lex := &Lexicon{Deny: []Entry{deny("株式会社", CategoryOther, ModeSub)}}
	if got := find(lex, "a㍿"); !reflect.DeepEqual(got, []Hit{{Start: 1, End: 1, Category: CategoryOther}}) {
		t.Fatalf("hits = %+v", got)
	}
}

func TestCompileReport(t *testing.T) {
	lex := &Lexicon{
		Deny: []Entry{
			deny("笨蛋", CategoryAbuse, ModeSub),
			deny("笨 蛋", CategoryAbuse, ModeSub),   // 折叠后与上一条相同
			deny("!!!", CategoryAbuse, ModeSub),   // 折叠后为空
			deny("官方", CategoryPolitics, ModeSub), // 与内置词相同,内置词先到
			deny("笨蛋", CategoryAbuse, ModeToken),  // 匹配方式不同,不算重复
		},
		Allow: []string{"臭豆腐", "臭豆腐"},
	}
	_, report := Compile(lex)
	want := Report{DenyWords: len(builtinDeny) + 2, AllowWords: 1, Dropped: 1, Duplicates: 3}
	if report != want {
		t.Fatalf("report = %+v, want %+v", report, want)
	}
	// 内置词先到:词库里类别不同的同一个词改不了它。
	if got := find(lex, "官方"); len(got) != 1 || got[0].Category != CategoryImpersonation {
		t.Fatalf("built-in word must keep its category: %+v", got)
	}
}

func TestFindOnEmptyOrSymbolOnlyText(t *testing.T) {
	for _, text := range []string{"", "   ", "!!!"} {
		if hits := find(nil, text); hits != nil {
			t.Errorf("%q: hits = %+v, want none", text, hits)
		}
	}
}

func TestRepeatedRuneBypassDefense(t *testing.T) {
	lex := &Lexicon{
		Deny: []Entry{
			deny("加微信", CategoryAd, ModeSub),
			deny("笨蛋", CategoryAbuse, ModeSub),
			deny("av", CategoryPorn, ModeToken),
			deny("qq", CategoryAd, ModeSub), // 词库本身含叠字,不得因输入折叠失效
		},
		Allow: []string{"臭豆腐"},
	}
	// 内置词叠字绕过: "管理理员"
	if hits := find(lex, "管理理员"); len(hits) == 0 || hits[0].Category != CategoryImpersonation {
		t.Fatalf("repeated built-in word missed: %+v", hits)
	}
	// 引流词叠字绕过: "加微微微信"
	if hits := find(lex, "加微微微信"); len(hits) == 0 || hits[0].Start != 0 || hits[0].End != 4 {
		t.Fatalf("repeated ad word span wrong: %+v", hits)
	}
	// 叠字打码覆盖整段重复字符: "笨笨蛋" 应覆盖 [0..2]
	if hits := find(lex, "笨笨蛋"); len(hits) == 0 || hits[0].Start != 0 || hits[0].End != 2 {
		t.Fatalf("repeated abuse span should cover full run: %+v", hits)
	}
	// 整词连续叠字命中 "aavv",但带空格 "a a v" 保持不命中 ModeToken
	if hits := find(lex, "看aavv吗"); len(hits) == 0 || hits[0].Category != CategoryPorn {
		t.Fatalf("contiguous repeated token missed: %+v", hits)
	}
	if hits := find(lex, "a a v"); len(hits) != 0 {
		t.Fatalf("non-contiguous repeated token must not match: %+v", hits)
	}
	// 词库本身含叠字的词照常命中 "qq",不误伤单个 "q"
	if hits := find(lex, "qq"); len(hits) == 0 || hits[0].Category != CategoryAd {
		t.Fatalf("double-letter deny word qq missed: %+v", hits)
	}
	if hits := find(lex, "q"); len(hits) != 0 {
		t.Fatalf("single q must not match double-letter deny word qq: %+v", hits)
	}
}
