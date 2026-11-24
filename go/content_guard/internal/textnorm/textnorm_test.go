package textnorm

import (
	"testing"
)

// kept 把折叠结果里不跳过的字符拼起来:这就是「匹配时看到的文字」。
func kept(text string) string {
	return string(FoldWord(text))
}

func TestFoldNormalizesVariants(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ascii upper", "HeLLo", "hello"},
		{"full width letters and digits", "ＡＢｃ１２", "abc12"},
		{"circled digits", "①②", "12"},
		{"accented letter loses its mark", string(rune(0x00E9)), "e"},
		{"traditional to simplified", "體國發", "体国发"},
		{"supplement pair", "以後", "以后"},
		{"space between characters", "官 方", "官方"},
		{"zero width space", "官" + string(rune(0x200B)) + "方", "官方"},
		{"emoji between characters", "官😀方", "官方"},
		{"punctuation and symbols", "加-微.信~", "加微信"},
		{"square company sign expands", "㍿", "株式会社"},
		{"only symbols", "!!!…", ""},
	}
	for _, c := range cases {
		if got := kept(c.in); got != c.want {
			t.Errorf("%s: kept(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestFoldKeepsOriginalIndex(t *testing.T) {
	elems, orig := Fold("a㍿ b")
	if string(orig) != "a㍿ b" {
		t.Fatalf("orig = %q", string(orig))
	}
	// a(0) 株式会社(都来自 1) 空格(2,可跳过) b(3)
	want := []Elem{
		{R: 'a', Orig: 0},
		{R: '株', Orig: 1}, {R: '式', Orig: 1}, {R: '会', Orig: 1}, {R: '社', Orig: 1},
		{R: ' ', Orig: 2, Skip: true},
		{R: 'b', Orig: 3},
	}
	if len(elems) != len(want) {
		t.Fatalf("got %d elems, want %d: %+v", len(elems), len(want), elems)
	}
	for i := range want {
		if elems[i] != want[i] {
			t.Errorf("elems[%d] = %+v, want %+v", i, elems[i], want[i])
		}
	}
}

func TestFoldMarksCombiningMarkSkippable(t *testing.T) {
	// 预组合的 é 拆成 e 加组合记号;记号可跳过,两个 Elem 都指回原文的同一个字符。
	elems, _ := Fold(string(rune(0x00E9)))
	if len(elems) != 2 || elems[0].R != 'e' || elems[0].Skip || !elems[1].Skip || elems[1].Orig != 0 {
		t.Fatalf("unexpected fold of é: %+v", elems)
	}
	if !IsMark(rune(0x0301)) || IsMark('e') {
		t.Fatalf("IsMark misclassifies the combining acute accent or a letter")
	}
}

func TestSkippable(t *testing.T) {
	for _, r := range []rune{' ', '-', '_', '*', rune(0x09), rune(0x3000), rune(0x200B), '，', '。', '★', '😀'} {
		if !Skippable(r) {
			t.Errorf("Skippable(%q) = false, want true", r)
		}
	}
	for _, r := range []rune{'a', 'Z', '0', '9', '官', 'あ', 'я', '٣'} {
		if Skippable(r) {
			t.Errorf("Skippable(%q) = true, want false", r)
		}
	}
}

func TestT2STableIsLoaded(t *testing.T) {
	if len(t2s) < 2000 {
		t.Fatalf("t2s table has only %d pairs; t2s.txt is probably truncated", len(t2s))
	}
	for from, to := range t2s {
		if from < 0x3400 || to < 0x3400 {
			t.Fatalf("t2s pair %q -> %q is outside the CJK range", from, to)
		}
	}
	if t2s['體'] != '体' || t2s['後'] != '后' {
		t.Fatalf("t2s is missing well-known pairs")
	}
}

func TestFoldSurvivesInvalidUTF8(t *testing.T) {
	// 非法字节变成 U+FFFD,按符号跳过;两边的合法字符照常折叠。
	if got := kept("a\xffb"); got != "ab" {
		t.Fatalf("kept(invalid) = %q, want %q", got, "ab")
	}
}

func TestFoldNormalizesHomoglyphsWithoutTouchingDigits(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"cyrillic gm", "gм01", "gm01"},
		{"cyrillic admin", "аdmіn", "admin"},
		{"greek av", "αv", "av"},
		{"digits preserved for contact detection", "13900139000", "13900139000"},
	}
	for _, c := range cases {
		if got := kept(c.in); got != c.want {
			t.Errorf("%s: kept(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
