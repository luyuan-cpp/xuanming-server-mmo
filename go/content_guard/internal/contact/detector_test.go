package contact

import (
	"testing"

	"content_guard/internal/textnorm"
)

func TestContainsAdCatchesObfuscatedContactsAndURLs(t *testing.T) {
	ads := []string{
		"加我微信号 wx123456 领折扣",
		"加v：①②③④⑤⑥⑦",
		"企鹅群 987654",
		"加微 一三八零零一",
		"VX：abc_998877",
		"详情看 https://example.xyz/pay",
		"来 www.test-shop.top 买金",
		"联系 13812345678 低价代练",
		"咸鱼搜 123456 出号",
	}
	for _, s := range ads {
		elems, _ := textnorm.Fold(s)
		if !ContainsAd(elems, nil) {
			t.Errorf("expected ad detection for %q", s)
		}
	}

	clean := []string{
		"晚上八点青云试炼缺个奶妈",
		"刚刚打出 12345 暴击伤害太爽了",
		"今天买丹药花了 50000 灵石",
		"队伍集合,加我一起刷本",
		"wx123 这个缩写? 不带数字只写加我吧",
		"欢迎各位道友加入青云门!",
	}
	for _, s := range clean[:4] {
		elems, _ := textnorm.Fold(s)
		if ContainsAd(elems, nil) {
			t.Errorf("unexpected ad detection for clean message %q", s)
		}
	}
	elems, _ := textnorm.Fold(clean[5])
	if ContainsAd(elems, nil) {
		t.Errorf("unexpected ad detection for %q", clean[5])
	}
}

// 域名的白名单按域名本身的范围比对;紧挨着的第二个域名照样要判;后面紧跟字母的不是域名。
func TestDomainDetectionUsesTheExactSpanAndSeesAdjacentDomains(t *testing.T) {
	contains := func(text string, allow func(start, end int32) bool) bool {
		elems, _ := textnorm.Fold(text)
		return ContainsAd(elems, allow)
	}
	// 「官网」占下标 0、1,域名 xuanming.com 占 2..13。
	official := "官网xuanming.com欢迎"
	var asked [][2]int32
	if contains(official, func(start, end int32) bool {
		asked = append(asked, [2]int32{start, end})
		return start == 2 && end == 13
	}) {
		t.Fatalf("an allow entry covering exactly the domain must exempt it; asked spans %v", asked)
	}
	if len(asked) != 1 || asked[0] != [2]int32{2, 13} {
		t.Fatalf("the allow callback must be asked about the domain itself, got %v", asked)
	}
	// 第一个域名在白名单里,紧挨着的第二个不在:照样是广告。
	if !contains("xuanming.com,spam-shop.top", func(start, _ int32) bool { return start == 0 }) {
		t.Fatal("a second domain right after an allowed one must still be detected")
	}
	for _, clean := range []string{"abc.community 是个单词", "版本 v1.top10 榜单"} {
		if contains(clean, nil) {
			t.Errorf("a suffix followed by letters or digits is not a domain: %q", clean)
		}
	}
	if !contains("来abc.com", nil) || !contains("x1.cn。", nil) {
		t.Fatal("plain domains at the end of the text or before punctuation must be detected")
	}
}

// 全角字母、全角句点写的网址和半角的是同一个判法:识别在折叠后的文字上做。
func TestFullWidthURLsAndDomainsAreDetected(t *testing.T) {
	for _, ad := range []string{"来ａｂｃ．ｃｏｍ买金", "ｈｔｔｐｓ：／／ｘｙ．ｔｏｐ", "ＷＷＷ．ｓｈｏｐ８８．ｃｎ"} {
		elems, _ := textnorm.Fold(ad)
		if !ContainsAd(elems, nil) {
			t.Errorf("a full-width URL or domain must be detected: %q", ad)
		}
	}
}

// 白名单只比对网址的主机名:登记了 xuanming.com,带 https://、www. 前缀和路径的写法照样放行;
// 把它当前缀接上别的域名的不放行;「www...」这种笑声不是网址。
func TestURLAllowListComparesOnlyTheHost(t *testing.T) {
	// 「官网 https://www.」占下标 0..14,xuanming.com 占 15..26。
	official := "官网 https://www.xuanming.com/pay"
	elems, _ := textnorm.Fold(official)
	if ContainsAd(elems, func(start, end int32) bool { return start == 15 && end == 26 }) {
		t.Fatal("an allowed host must stay allowed behind a scheme, a www. prefix and a path")
	}
	// xuanming.com 占 8..19,整个主机名 xuanming.com.evil.top 占 8..28。
	spoof := "https://xuanming.com.evil.top"
	elems, _ = textnorm.Fold(spoof)
	if !ContainsAd(elems, func(start, end int32) bool { return start == 8 && end == 19 }) {
		t.Fatal("an allowed host used as a prefix of another domain must still be detected")
	}
	elems, _ = textnorm.Fold("笑死www...")
	if ContainsAd(elems, nil) {
		t.Fatal("www followed only by dots is laughter, not a URL")
	}
}

func TestContainsAdRespectsAllowListCallback(t *testing.T) {
	msg := "官网 https://safe.example.com"
	elems, _ := textnorm.Fold(msg)
	blocked := ContainsAd(elems, func(_, _ int32) bool { return true })
	if blocked {
		t.Fatal("allowlist callback returning true must suppress ad rejection")
	}
}
