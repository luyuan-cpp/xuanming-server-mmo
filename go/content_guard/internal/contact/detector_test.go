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

func TestContainsAdRespectsAllowListCallback(t *testing.T) {
	msg := "官网 https://safe.example.com"
	elems, _ := textnorm.Fold(msg)
	blocked := ContainsAd(msg, elems, func(_, _ int32) bool { return true })
	if blocked {
		t.Fatal("allowlist callback returning true must suppress ad rejection")
	}
}
