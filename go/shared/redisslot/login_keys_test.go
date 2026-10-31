package redisslot

import (
	"strings"
	"testing"
)

// AccountTag 的金样由独立实现(Python,按 Redis Cluster 规范的 CRC16 与本包 SlotTag 的枚举规则)算出。
// 改动任何一个值都意味着存量键换槽。
func TestAccountTagGoldenVectors(t *testing.T) {
	cases := map[string]string{
		"robot_0001":   "arun",
		"acct-refresh": "fvs",
		"wx_union1":    "acun",
		"a{b}c":        "twl",
		"}{":           "aeyc",
		"{42}":         "afhj",
		"玩家甲":          "ahuc",
		" Seller 中文 ":  "fwp",
	}
	for account, want := range cases {
		if got := AccountTag(account); got != want {
			t.Errorf("AccountTag(%q) = %q, want %q", account, got, want)
		}
	}
}

// 带花括号的账号也必须落在 AccountTag 决定的槽里:键里第一个花括号对永远是 tag,账号原文排在它后面。
func TestAccountKeysShareTheAccountSlot(t *testing.T) {
	for _, account := range []string{"robot_0001", "a{b}c", "}{", "{42}", "{", "}", "{}", "玩家甲", " Seller 中文 ", "a:b:{c}:d"} {
		tag := AccountTag(account)
		want := HashSlot("{" + tag + "}")
		for _, key := range []string{
			AccountDeviceSetKey(account), AccountRefreshKey(account),
			AccessTokenKey(tag, "s"), RefreshTokenKey(tag, "s"), RefreshFamilyKey(tag, "f"),
			RefreshTokenKeyPrefix(tag) + "现拼的", RefreshFamilyKeyPrefix(tag) + "现拼的",
		} {
			if !HasHashTag(key) {
				t.Errorf("account %q: %q 没带 hash tag", account, key)
			}
			if got := HashSlot(key); got != want {
				t.Errorf("account %q: %q 在 slot %d, want %d", account, key, got, want)
			}
		}
	}
}

// 一条连接的 login_session 与会话栅栏必须同槽,CommitLogin 才能放进同一段脚本。
func TestConnectionKeysShareTheConnectionSlot(t *testing.T) {
	for _, connection := range []string{LoginConnectionID("gate-uuid-1", 7), LoginConnectionID("原gate", 8), LoginConnectionID("g", 18446744073709551615)} {
		fence, session := SessionFenceKey(connection), LoginSessionKey(connection)
		if !HasHashTag(fence) || !HasHashTag(session) {
			t.Errorf("connection %q: 键没带 hash tag: %q %q", connection, fence, session)
		}
		if HashSlot(fence) != HashSlot(session) || HashSlot(fence) != CRC16(connection)&(SlotCount-1) {
			t.Errorf("connection %q: %q 与 %q 不同槽", connection, fence, session)
		}
	}
}

func TestLoginKeyGoldenVectors(t *testing.T) {
	cases := []struct {
		got  string
		want string
	}{
		{LoginConnectionID("gate-uuid-1", 7), "gate-uuid-1:7"},
		{LoginConnectionID("g", 18446744073709551615), "g:18446744073709551615"},
		{LoginSessionKey("gate-uuid-1:7"), "login_session:{gate-uuid-1:7}"},
		{SessionFenceKey("gate-uuid-1:7"), "login:sessfence:{gate-uuid-1:7}"},
		{AccountDeviceSetKey("robot_0001"), "login_account_sessions:{arun}:robot_0001"},
		{AccountDeviceSetKey("a{b}c"), "login_account_sessions:{twl}:a{b}c"},
		{AccountRefreshKey("robot_0001"), "account_refresh:{arun}:robot_0001"},
		{AccessTokenKey("arun", "secret"), "access_token:{arun}:secret"},
		{RefreshTokenKey("arun", "secret"), "refresh_token:{arun}:secret"},
		{RefreshTokenKeyPrefix("arun"), "refresh_token:{arun}:"},
		{RefreshFamilyKey("arun", "family"), "refresh_family:{arun}:family"},
		{RefreshFamilyKeyPrefix("arun"), "refresh_family:{arun}:"},
		{PasswordQuotaKey("known:abcd"), "login:password:quota:v1:{known:abcd}"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("键形 = %q, want %q", c.got, c.want)
		}
	}
}

func TestValidAccountTag(t *testing.T) {
	for _, tag := range []string{"arun", "fvs", "afps", "emj", "hia", "a", "aa", AccountTag("任意账号"), AccountTag("{42}")} {
		if !ValidAccountTag(tag) {
			t.Errorf("ValidAccountTag(%q) = false, want true", tag)
		}
	}
	// zzzz 字母合法,但它所在 slot 的规范 tag 是 agfo。
	for _, tag := range []string{"", "zzzz", "ARUN", "a1", "a}", "{a", "a.b", "abcde", strings.Repeat("a", 64), "玩"} {
		if ValidAccountTag(tag) {
			t.Errorf("ValidAccountTag(%q) = true, want false", tag)
		}
	}
}
