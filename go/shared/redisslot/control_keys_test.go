package redisslot

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 族表本身要完整:每个枚举值都有前缀,前缀互不相同,不含冒号与花括号(否则 IsControlTag 的切分与 tag 截断都会出错)。
func TestControlFamilyTableIsComplete(t *testing.T) {
	seen := map[string]bool{}
	for f := ControlFamily(0); f < controlFamilyCount; f++ {
		prefix := f.Prefix()
		if prefix == "" || strings.ContainsAny(prefix, ":{}") {
			t.Fatalf("family %d has invalid prefix %q", f, prefix)
		}
		if seen[prefix] {
			t.Fatalf("prefix %q registered twice", prefix)
		}
		seen[prefix] = true
	}
	if len(ControlFamilyPrefixes()) != int(controlFamilyCount) {
		t.Fatalf("ControlFamilyPrefixes 长度 %d,族数 %d", len(ControlFamilyPrefixes()), controlFamilyCount)
	}
}

// 金样:设计稿 §3.1 的键形例子。tag 写在原来 zone 或实体号的位置,同一个 tag 的键必然同槽。
func TestControlTagShapes(t *testing.T) {
	cases := map[string]string{
		ZoneControlTag(ControlFamilySceneControl, 7):                    "{ctl:7}",
		ZoneControlTag(ControlFamilyLoginQueue, 3):                      "{q:3}",
		ZoneControlTag(ControlFamilyGateMark, 0):                        "{gm:0}",
		ZoneControlTag(ControlFamilyGuildRank, 12):                      "{gr:12}",
		ControlTag(ControlFamilyAssetWake, "63"):                        "{aw:63}",
		ControlTag(ControlFamilyGuild, "900"):                           "{g:900}",
		ControlTag(ControlFamilyPlayerGuild, "42"):                      "{pg:42}",
		ControlTag(ControlFamilyPlayerLocator, ControlPlayerLocatorIdentity): "{pl:gix}",
		ControlTag(ControlFamilyFriend, "42"):                           "{f:42}",
		ControlTag(ControlFamilyFriendRequestQuota, "42"):               "{rq:42}",
		ControlTag(ControlFamilyFriendDirectory, "42"):                  "{directory:42}",
		ControlTag(ControlFamilyGuildApply, "42"):                       "{apply:42}",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	// friend / guild 今天的键形(不改名)正好落在登记过的族里。
	for _, key := range []string{"friend:{f:7}:list:v3", "friend:{rq:7}", "friend:{directory:7}", "guild:{apply:7}"} {
		tag, ok := HashTag(key)
		if !ok || !IsControlTag(tag) {
			t.Errorf("%q 的 tag 应当是登记过的控制族", key)
		}
	}
	scenes := "node:" + ZoneControlTag(ControlFamilySceneControl, 7) + ":3:scenes"
	nodeKey := "scene:" + ZoneControlTag(ControlFamilySceneControl, 7) + ":1234:node"
	if HashSlot(scenes) != HashSlot(nodeKey) {
		t.Fatalf("同一个 zone 的控制域键必须同槽")
	}
}

func TestIsControlTag(t *testing.T) {
	yes := []string{"ctl:7", "q:0", "gm:12", "aw:63", "g:1", "pg:1", "gr:1", "pl:gix", "f:1", "rq:1", "directory:1", "apply:18446744073709551615", "ctl:7:extra"}
	for _, tag := range yes {
		if !IsControlTag(tag) {
			t.Errorf("IsControlTag(%q) = false, want true", tag)
		}
	}
	no := []string{
		"", "ctl", "ctl:", ":7", "unknown:7", "CTL:7", "player:7",
		"42",                             // 玩家 tag
		"afps",                           // slot-tag
		"7a1c2d3e-0b4f-4f6e-9c1d-2e3f4a5b6c7d:3", // 登录连接身份
	}
	for _, tag := range no {
		if IsControlTag(tag) {
			t.Errorf("IsControlTag(%q) = true, want false", tag)
		}
	}
	// 玩家域的三种 tag 形状一个都不能被认成控制族。
	for slot := uint16(0); slot < SlotCount; slot++ {
		if IsControlTag(SlotTag(slot)) {
			t.Fatalf("slot-tag %q 被认成了控制族", SlotTag(slot))
		}
	}
	if tag, _ := HashTag(LoginSessionKey(LoginConnectionID("gate-instance-1", 9))); IsControlTag(tag) {
		t.Fatalf("登录连接身份 %q 被认成了控制族", tag)
	}
	if tag, _ := HashTag(LocationKey(42)); IsControlTag(tag) {
		t.Fatalf("玩家 tag %q 被认成了控制族", tag)
	}
}

func TestControlTagRejectsBadIdentity(t *testing.T) {
	for _, identity := range []string{"", "a{b", "a}b", "{}"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("ControlTag(%q) 应当 panic", identity)
				}
			}()
			ControlTag(ControlFamilySceneControl, identity)
		}()
	}
	defer func() {
		if recover() == nil {
			t.Errorf("越界的族应当 panic")
		}
	}()
	_ = controlFamilyCount.Prefix()
}

// 跨语言契约:C++ redis_slot.h 的 kControlFamilyPrefixes 与本表逐项、同序一致。C++ 侧的控制集群守卫靠它认族,
// 两边有一项不同,同一个键就会一边放行、一边拒绝。
func TestControlFamiliesMatchCpp(t *testing.T) {
	const header = "cpp/libs/engine/infra/storage/redis_cluster/redis_slot.h"
	data, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(header)))
	if err != nil {
		t.Fatalf("read %s: %v", header, err)
	}
	block := regexp.MustCompile(`(?s)kControlFamilyPrefixes\[\]\s*=\s*\{(.*?)\};`).FindSubmatch(data)
	if block == nil {
		t.Fatalf("%s 里找不到 kControlFamilyPrefixes", header)
	}
	var cpp []string
	for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllSubmatch(block[1], -1) {
		cpp = append(cpp, string(m[1]))
	}
	goPrefixes := ControlFamilyPrefixes()
	if strings.Join(cpp, ",") != strings.Join(goPrefixes, ",") {
		t.Fatalf("C++ 族表 %v 与 Go 族表 %v 不一致", cpp, goPrefixes)
	}
}
