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
	"strings"
	"unicode"
	"unicode/utf8"

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

// domainRe 是最常见的外链广告域名:一段字母数字加点号加常见顶级域。域名后面的边界(不能紧跟字母数字,否则是 abc.community
// 这种普通单词)在 hasUnallowedDomain 里判,不写进正则:Go 的正则没有零宽断言,边界字符写进去会被这一个匹配吃掉,
// 紧挨着的第二个域名就匹配不到了,白名单要比对的范围也会多出前后各一个字。
// 前面的边界不用判:这一段字符类已经包含全部字母数字,最左匹配必然从这一段的开头起。
//
// 两条正则都在折叠后的文字上找(见 ContainsAd),折叠已经转成小写,所以不带 (?i)。
var domainRe = regexp.MustCompile(`[a-z0-9-]{2,}\.(?:com|cn|net|org|xyz|top|vip|cc|tv|me|gg|io|club|shop|work|site)`)

// urlRe 是带协议头或 www. 前缀的网址;第 1 个分组是主机名,白名单只比对这一段。
var urlRe = regexp.MustCompile(`(?:https?://(?:www\.)?|www\.)([a-z0-9.-]+)`)

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
// elems 是 textnorm.Fold 的结果;allowCheck 收到的是原文的字符下标区间(两端都含),nil = 没有白名单。
func ContainsAd(elems []textnorm.Elem, allowCheck func(startOrig, endOrig int32) bool) bool {
	if len(elems) == 0 {
		return false
	}
	// 网址和域名在折叠后的文字上找,不在原文上找:全角字母、全角句点、带圈数字、西里尔同形字写的网址折叠后和半角的一样,
	// 在原文上找的话切一下全角输入就绕过去了。折叠序列一个元素一个字符,所以折叠串的第 k 个字符就是 elems[k]。
	folded := foldedString(elems)
	if hasUnallowedURL(folded, elems, allowCheck) || hasUnallowedDomain(folded, elems, allowCheck) {
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

func foldedString(elems []textnorm.Elem) string {
	var b strings.Builder
	b.Grow(len(elems))
	for _, e := range elems {
		b.WriteRune(e.R)
	}
	return b.String()
}

// allowed 报告折叠串里的字节区间 [from, to) 是否整段落在白名单里。
func allowed(folded string, elems []textnorm.Elem, from, to int, allowCheck func(int32, int32) bool) bool {
	if allowCheck == nil {
		return false
	}
	first := utf8.RuneCountInString(folded[:from])
	last := first + utf8.RuneCountInString(folded[from:to]) - 1
	return allowCheck(elems[first].Orig, elems[last].Orig)
}

// hasUnallowedURL 报告有没有主机名不在白名单里的网址。白名单只比对主机名:名单里登记 xuanming.com,
// 带 https:// 或 www. 前缀的写法照样放行,不用把每种前缀再各登记一遍。
func hasUnallowedURL(folded string, elems []textnorm.Elem, allowCheck func(int32, int32) bool) bool {
	for _, loc := range urlRe.FindAllStringSubmatchIndex(folded, -1) {
		from, to := loc[2], loc[3]
		// 主机名末尾的句点和连字符是句子的标点,不算主机名;去掉之后什么都不剩(「www...」这种笑声)就不是网址。
		for to > from && (folded[to-1] == '.' || folded[to-1] == '-') {
			to--
		}
		if to > from && !allowed(folded, elems, from, to, allowCheck) {
			return true
		}
	}
	return false
}

// hasUnallowedDomain 报告有没有不在白名单里的域名。白名单比对的范围恰好是域名本身。
func hasUnallowedDomain(folded string, elems []textnorm.Elem, allowCheck func(int32, int32) bool) bool {
	for _, loc := range domainRe.FindAllStringIndex(folded, -1) {
		if loc[1] < len(folded) && textnorm.IsASCIIAlnum(rune(folded[loc[1]])) {
			continue
		}
		if !allowed(folded, elems, loc[0], loc[1], allowCheck) {
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
