// Package textnorm 把玩家文字折成用于匹配的形态(docs/design/rename-and-content-safety.md §6.3)。
//
// 折叠只用于匹配,不改玩家原文:每个折叠后的字符都记着它来自原文的第几个字符,打码时按这个下标对回去。
// 词库里的词和待检文字走同一个 Fold,所以折叠规则偏宽(比如繁简字表多转了一个字)不会造成漏判或误判,
// 只要两边一致。改这里的规则等于同时改了两边;已发布的词库不用重新发布,服务加载时会按新规则重新折叠。
//
// 本包不连库、不读配置、不打日志。
package textnorm

import (
	_ "embed"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// t2s.txt 由同目录 gen_t2s.ps1 生成:每行两个字符,左繁右简。
//
//go:embed t2s.txt
var t2sData string

var t2s = mustParseT2S(t2sData)

// mustParseT2S 解析内嵌的繁简字表。表随代码提交,格式不对说明构建产物坏了:启动即 panic,不带着半张表对外判定。
func mustParseT2S(data string) map[rune]rune {
	table := make(map[rune]rune, 2600)
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		from, size := utf8.DecodeRuneInString(line)
		to, toSize := utf8.DecodeRuneInString(line[size:])
		if from == utf8.RuneError || to == utf8.RuneError || size+toSize != len(line) || from == to {
			panic("textnorm: malformed t2s.txt line: " + line)
		}
		table[from] = to
	}
	return table
}

// Elem 是折叠后的一个字符。
type Elem struct {
	R rune
	// Orig 是它来自原文的第几个字符(按 rune 计,从 0 起)。一个原文字符可能折成多个 Elem(㍿ → 株式会社),它们的 Orig 相同。
	Orig int32
	// Skip 为真的字符在匹配时可以跳过:空白、标点、符号、不可见字符、组合记号。
	Skip bool
}

// confusables 将视觉上与拉丁小写字母难以区分的西里尔/希腊/兼容同形字折到 ASCII 拉丁小写
// (移植自 xuanming-server/pkg/namecheck/normalize.go,防 "аdmin" / "gм" / "αv" 类跨脚本绕过)。
// 刻意不折 '0'→'o' / '1'→'l':contact 引流检测依赖保留数字串识别手机号与 QQ/微信号。
var confusables = map[rune]rune{
	// 西里尔小写 → 拉丁
	'а': 'a', 'в': 'b', 'с': 'c', 'ԁ': 'd', 'е': 'e', 'ѕ': 's',
	'һ': 'h', 'і': 'i', 'ј': 'j', 'к': 'k', 'ӏ': 'l', 'м': 'm',
	'н': 'h', 'о': 'o', 'р': 'p', 'т': 't', 'у': 'y', 'х': 'x',
	'ү': 'y', 'ғ': 'f', 'ԛ': 'q', 'ѡ': 'w', 'ѵ': 'v', 'ᴦ': 'r',
	// 希腊小写 → 拉丁
	'α': 'a', 'β': 'b', 'ε': 'e', 'ζ': 'z', 'η': 'n', 'ι': 'i',
	'κ': 'k', 'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u',
	'χ': 'x', 'γ': 'y', 'μ': 'u', 'σ': 'o', 'ϲ': 'c', 'ϳ': 'j',
	// 其它常见冒充字形
	'ǀ': 'l', 'ɩ': 'l', 'ɑ': 'a', 'ɡ': 'g', 'ɪ': 'i', 'ʏ': 'y',
	'ʙ': 'b', 'ʜ': 'h', 'ᴏ': 'o',
}

// Fold 把原文逐字符折叠。顺序固定:
//  1. NFKD:全角 → 半角,带圈数字、上下标 → 数字,兼容字形 → 正字,带音标的字母拆成字母加组合记号;
//  2. 转小写;
//  3. 繁体 → 简体;
//  4. 西里尔/希腊同形字 → 拉丁小写;
//  5. 标出可跳过的字符(组合记号在这一步被标成可跳过,所以 é 能对上词库里的 e)。
//
// 返回折叠后的序列和原文的字符数组(打码在后者上做)。非法 UTF-8 的字节在第二个返回值里是 U+FFFD,
// 按符号处理(可跳过);gRPC 入口的 string 字段本来就拒收非法 UTF-8,这里只保证不崩。
func Fold(text string) ([]Elem, []rune) {
	orig := []rune(text)
	elems := make([]Elem, 0, len(orig))
	var encoded [utf8.UTFMax]byte
	var decomposed []byte
	for i, r := range orig {
		if r < utf8.RuneSelf {
			// ASCII 不受 NFKD、繁简与跨脚本同形字影响,直接走。
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			elems = append(elems, Elem{R: r, Orig: int32(i), Skip: Skippable(r)})
			continue
		}
		n := utf8.EncodeRune(encoded[:], r)
		decomposed = norm.NFKD.Append(decomposed[:0], encoded[:n]...)
		for off := 0; off < len(decomposed); {
			folded, size := utf8.DecodeRune(decomposed[off:])
			off += size
			folded = unicode.ToLower(folded)
			if simplified, ok := t2s[folded]; ok {
				folded = simplified
			}
			if mapped, ok := confusables[folded]; ok {
				folded = mapped
			}
			elems = append(elems, Elem{R: folded, Orig: int32(i), Skip: Skippable(folded)})
		}
	}
	return elems, orig
}

// FoldWord 折叠一个词库词条:走 Fold,再去掉可跳过的字符。匹配时文字里的这些字符会被跳过,词条里留着它们就永远对不上。
// 折完为空(词条全是符号)返回空切片,由调用方丢弃并计数。
func FoldWord(word string) []rune {
	elems, _ := Fold(word)
	out := make([]rune, 0, len(elems))
	for _, e := range elems {
		if !e.Skip {
			out = append(out, e.R)
		}
	}
	return out
}

// Skippable 判断一个已折叠的字符在匹配时是否跳过。
// ASCII 里字母数字之外的全部跳过;其余按 Unicode 类别:空白、标点、符号(含表情)、控制符、格式符(零宽字符)、
// 组合记号、私用区。数字和各种文字的字母不跳过。
func Skippable(r rune) bool {
	if r < utf8.RuneSelf {
		return !IsASCIIAlnum(r)
	}
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsControl(r) ||
		unicode.In(r, unicode.Cf, unicode.Mn, unicode.Me, unicode.Co)
}

// IsASCIIAlnum 判断是否是(已折成小写的)英文字母或数字。「整词」匹配用它判词边界。
func IsASCIIAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// IsMark 判断原文字符是否是组合记号。打码时把紧跟在命中段后面的组合记号一并盖掉,不让它落在 * 上。
func IsMark(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me)
}
