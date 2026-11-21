// Package contact 识别聊天和资料中的变体联系方式与外部引流广告(docs/design/rename-and-content-safety.md §7)。
//
// 纯内存计算、无状态。复用 textnorm 折叠序列(NFKD + 繁简 + 小写 + orig 下标):
//   - 带圈数字 ①、全角 １、上标 ¹ 等已经在 textnorm 里折成 ASCII 数字 '0'..'9';
//   - 这里再额外把线索词之后的中文数字/谐音字(零一壹幺二两三叁四肆五伍六陆七柒拐八捌九玖)
//     规范化成数字串,拦住 "加v 一二三四五六" 这类拆字变体;
//   - 支持白名单词覆盖豁免(Allowed spans)。
package contact

import (
	"regexp"
	"unicode"

	"content_guard/internal/textnorm"
)

type cueKind uint8

const (
	cueWeChat cueKind = 1
	cueQQ     cueKind = 2
	cueTrade  cueKind = 3
)

type cueDef struct {
	folded string
	kind   cueKind
}

var channelCues = []cueDef{
	{"weixin", cueWeChat},
	{"weix", cueWeChat},
	{"微x", cueWeChat},
	{"wei信", cueWeChat},
	{"微信", cueWeChat},
	{"威信", cueWeChat},
	{"徽信", cueWeChat},
	{"v信", cueWeChat},
	{"加微", cueWeChat},
	{"加wx", cueWeChat},
	{"加vx", cueWeChat},
	{"加v", cueWeChat},
	{"wx", cueWeChat},
	{"vx", cueWeChat},
	{"qq群", cueQQ},
	{"q群", cueQQ},
	{"扣扣", cueQQ},
	{"企鹅", cueQQ},
	{"加q", cueQQ},
	{"qq", cueQQ},
	{"支付宝", cueTrade},
	{"zfb", cueTrade},
	{"闲鱼", cueTrade},
	{"咸鱼", cueTrade},
}

// domainSuffixes 是最常见的外链广告顶级域名;要求前面是点号且紧接域名边界,减少自然英文误伤。
var domainSuffixRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])([a-z0-9-]{2,}\.(?:com|cn|net|org|xyz|top|vip|cc|tv|me|gg|io|club|shop|work|site))(?:$|[^a-z0-9])`)
var urlSchemeRe = regexp.MustCompile(`(?i)(?:https?://|www\.)[a-z0-9.-]+`)

// cnDigitMap 把渠道线索词之后常见的汉字数字与语音谐音折回阿拉伯数字。
var cnDigitMap = map[rune]byte{
	'〇': '0', '零': '0', '洞': '0',
	'一': '1', '壹': '1', '幺': '1', '妖': '1',
	'二': '2', '贰': '2', '两': '2',
	'三': '3', '叁': '3',
	'四': '4', '肆': '4',
	'五': '5', '伍': '5',
	'六': '6', '陆': '6',
	'七': '7', '柒': '7', '拐': '7',
	'八': '8', '捌': '8',
	'九': '9', '玖': '9',
}

// ContainsAd 报告文本是否包含外链 URL / 域名或变体联系方式引流(支持白名单词覆盖豁免)。
func ContainsAd(text string, elems []textnorm.Elem, allowCheck func(startOrig, endOrig int32) bool) bool {
	if text == "" || len(elems) == 0 {
		return false
	}
	if hasUnallowedRegexMatch(urlSchemeRe, text, allowCheck) {
		return true
	}
	if hasUnallowedRegexMatch(domainSuffixRe, text, allowCheck) {
		return true
	}

	// 只抽取非跳过元素组成连续线索匹配窗口,同时保留原始 Orig 下标用于白名单检查。
	dense := make([]textnorm.Elem, 0, len(elems))
	for _, e := range elems {
		if !e.Skip {
			dense = append(dense, e)
		}
	}
	if len(dense) == 0 {
		return false
	}

	// 检测裸 11 位大陆手机号(分隔符已跳过),常见引流形式。
	if hasBarePhoneNumber(dense, allowCheck) {
		return true
	}

	return hasCueWithContact(dense, allowCheck)
}

func hasUnallowedRegexMatch(re *regexp.Regexp, orig string, allowCheck func(int32, int32) bool) bool {
	locs := re.FindAllStringIndex(orig, -1)
	if len(locs) == 0 {
		return false
	}
	if allowCheck == nil {
		return true
	}
	for _, loc := range locs {
		startRune := int32(len([]rune(orig[:loc[0]])))
		endRune := int32(len([]rune(orig[:loc[1]])) - 1)
		if !allowCheck(startRune, endRune) {
			return true
		}
	}
	return false
}

func hasBarePhoneNumber(dense []textnorm.Elem, allowCheck func(int32, int32) bool) bool {
	run := 0
	for i, e := range dense {
		if e.R >= '0' && e.R <= '9' {
			run++
			if run == 11 {
				start := i - 10
				// 大陆手机号 1[3-9]xxxxxxxxx;前后不接数字。
				if dense[start].R == '1' && dense[start+1].R >= '3' && dense[start+1].R <= '9' {
					nextIsDigit := i+1 < len(dense) && dense[i+1].R >= '0' && dense[i+1].R <= '9'
					if !nextIsDigit && (allowCheck == nil || !allowCheck(dense[start].Orig, dense[i].Orig)) {
						return true
					}
				}
			}
		} else {
			run = 0
		}
	}
	return false
}

func hasCueWithContact(dense []textnorm.Elem, allowCheck func(int32, int32) bool) bool {
	const lookahead = 28
	for i := 0; i < len(dense); i++ {
		cueLen := 0
		for _, cue := range channelCues {
			if matchDenseCue(dense, i, []rune(cue.folded)) {
				cueLen = len([]rune(cue.folded))
				break
			}
		}
		if cueLen == 0 {
			continue
		}
		// 英文短线索(如 qq / wx / vx)前面若紧挨英文字母,属于普通单词一部分(如 "vortex" 不会, "mywx" 等排除后缀粘连)。
		if i > 0 && dense[i].R < 128 && unicode.IsLetter(dense[i].R) && textnorm.IsASCIIAlnum(dense[i-1].R) {
			continue
		}

		// 在线索词后面扫描最多 lookahead 个非跳过字符:凑出连续 >=5 个数字(含中文数字)或 >=6 个微信号字符。
		digits := 0
		handleChars := 0
		end := min(len(dense), i+cueLen+lookahead)
		for j := i + cueLen; j < end; j++ {
			r := dense[j].R
			if r >= '0' && r <= '9' {
				digits++
				handleChars++
			} else if _, ok := cnDigitMap[r]; ok {
				digits++
			} else if (r >= 'a' && r <= 'z') || r == '_' || r == '-' {
				// 线索紧跟非汉字分隔的多段微信号字符(必须含数字或下划线或 >=7 字母,避免普通英语词)。
				handleChars++
				digits = 0
			} else {
				// 若已经凑够数字串则提前结束;若中断且遇到中文普通字则重计。
				if digits >= 5 {
					break
				}
				digits = 0
				handleChars = 0
			}

			if digits >= 5 || (handleChars >= 6 && hasDigitInWindow(dense, i+cueLen, j)) {
				if allowCheck == nil || !allowCheck(dense[i].Orig, dense[j].Orig) {
					return true
				}
				break
			}
		}
	}
	return false
}

func hasDigitInWindow(dense []textnorm.Elem, from, to int) bool {
	for k := from; k <= to && k < len(dense); k++ {
		if dense[k].R >= '0' && dense[k].R <= '9' {
			return true
		}
	}
	return false
}

func matchDenseCue(dense []textnorm.Elem, start int, cue []rune) bool {
	if start+len(cue) > len(dense) {
		return false
	}
	for k, want := range cue {
		if dense[start+k].R != want {
			return false
		}
	}
	return true
}
