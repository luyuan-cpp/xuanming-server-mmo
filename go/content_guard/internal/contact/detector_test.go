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
		if !ContainsAd(s, elems, nil) {
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
		if ContainsAd(s, elems, nil) {
			t.Errorf("unexpected ad detection for clean message %q", s)
		}
	}
	elems, _ := textnorm.Fold(clean[5])
	if ContainsAd(clean[5], elems, nil) {
		t.Errorf("unexpected ad detection for %q", clean[5])
	}
}

// 域名的白名单按域名本身的范围比对;紧挨着的第二个域名照样要判;后面紧跟字母的不是域名。
func TestDomainDetectionUsesTheExactSpanAndSeesAdjacentDomains(t *testing.T) {
	contains := func(text string, allow func(start, end int32) bool) bool {
		elems, _ := textnorm.Fold(text)
		return ContainsAd(text, elems, allow)
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

func TestContainsAdRespectsAllowListCallback(t *testing.T) {
	msg := "官网 https://safe.example.com"
	elems, _ := textnorm.Fold(msg)
	blocked := ContainsAd(msg, elems, func(_, _ int32) bool { return true })
	if blocked {
		t.Fatal("allowlist callback returning true must suppress ad rejection")
	}
}
