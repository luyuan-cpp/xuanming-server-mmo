package savebarrier

import (
	"strings"
	"testing"
)

const (
	fenceTestInstance = "6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f"
	fenceTestNonceA   = "0123456789abcdef0123456789abcdef"
	fenceTestNonceB   = "fedcba9876543210fedcba9876543210"
)

func TestParseHandoffFenceAcceptsHolderAndTombstone(t *testing.T) {
	held, err := ParseHandoffFence("7|" + fenceTestInstance + "|3|" + fenceTestNonceA + "|H|" + fenceTestNonceB)
	if err != nil {
		t.Fatalf("合法持有资格被拒: %v", err)
	}
	if held.OwnerEpoch != 7 || held.SceneInstance != fenceTestInstance || held.Generation != 3 ||
		held.AdmissionToken != fenceTestNonceA || held.State != HandoffFenceHeld || held.Token != fenceTestNonceB {
		t.Fatalf("字段解析错位: %+v", held)
	}
	for _, state := range []string{"F", "R"} {
		if _, err := ParseHandoffFence("7|" + fenceTestInstance + "|3|" + fenceTestNonceA + "|" + state + "|" + fenceTestNonceB); err != nil {
			t.Fatalf("状态 %s 被拒: %v", state, err)
		}
	}
	tomb, err := ParseHandoffFence("9||0|" + strings.Repeat("0", 32) + "|R|" + fenceTestNonceB)
	if err != nil || tomb.Generation != 0 || tomb.SceneInstance != "" || tomb.State != HandoffFenceRevoked {
		t.Fatalf("恢复墓碑应可解析: %+v %v", tomb, err)
	}
}

// 与 C++ ParseFence / Lua fence() 的拒绝面逐条对齐:Go 侧多认一种原文,就会把 C++ 不承认的资格当成持有证据。
func TestParseHandoffFenceRejectsWhatCppRejects(t *testing.T) {
	zero := strings.Repeat("0", 32)
	valid := func(e, i, n, a, k, tok string) string { return strings.Join([]string{e, i, n, a, k, tok}, "|") }
	cases := []struct{ name, raw string }{
		{"字段太少", "7|" + fenceTestInstance + "|3|" + fenceTestNonceA + "|H"},
		{"字段太多", valid("7", fenceTestInstance, "3", fenceTestNonceA, "H", fenceTestNonceB) + "|x"},
		{"epoch 为 0", valid("0", fenceTestInstance, "3", fenceTestNonceA, "H", fenceTestNonceB)},
		{"epoch 前导零", valid("07", fenceTestInstance, "3", fenceTestNonceA, "H", fenceTestNonceB)},
		{"epoch 溢出", valid("18446744073709551616", fenceTestInstance, "3", fenceTestNonceA, "H", fenceTestNonceB)},
		{"epoch 非数字", valid("+7", fenceTestInstance, "3", fenceTestNonceA, "H", fenceTestNonceB)},
		{"世代前导零", valid("7", fenceTestInstance, "03", fenceTestNonceA, "H", fenceTestNonceB)},
		{"加载 nonce 大写", valid("7", fenceTestInstance, "3", strings.ToUpper(fenceTestNonceA), "H", fenceTestNonceB)},
		{"资格 nonce 全零", valid("7", fenceTestInstance, "3", fenceTestNonceA, "H", zero)},
		{"资格 nonce 过短", valid("7", fenceTestInstance, "3", fenceTestNonceA, "H", fenceTestNonceB[:31])},
		{"未知状态", valid("7", fenceTestInstance, "3", fenceTestNonceA, "X", fenceTestNonceB)},
		{"状态两个字母", valid("7", fenceTestInstance, "3", fenceTestNonceA, "HH", fenceTestNonceB)},
		{"实例含空格", valid("7", "a b", "3", fenceTestNonceA, "H", fenceTestNonceB)},
		{"实例含控制字符", valid("7", "a\x7f", "3", fenceTestNonceA, "H", fenceTestNonceB)},
		{"实例超长", valid("7", strings.Repeat("a", 257), "3", fenceTestNonceA, "H", fenceTestNonceB)},
		{"持有者缺实例", valid("7", "", "3", fenceTestNonceA, "H", fenceTestNonceB)},
		{"持有者加载 nonce 全零", valid("7", fenceTestInstance, "3", zero, "H", fenceTestNonceB)},
		{"墓碑不是 R", valid("7", "", "0", zero, "H", fenceTestNonceB)},
		{"墓碑带实例", valid("7", fenceTestInstance, "0", zero, "R", fenceTestNonceB)},
		{"墓碑带加载 nonce", valid("7", "", "0", fenceTestNonceA, "R", fenceTestNonceB)},
	}
	for _, c := range cases {
		if fence, err := ParseHandoffFence(c.raw); err == nil {
			t.Errorf("%s: 不规范原文被接受: %q -> %+v", c.name, c.raw, fence)
		}
	}
	if _, err := ParseHandoffFence(valid("7", strings.Repeat("a", 256), "3", fenceTestNonceA, "H", fenceTestNonceB)); err != nil {
		t.Fatalf("256 字节实例号应被接受(与 C++ 上限相同): %v", err)
	}
}
