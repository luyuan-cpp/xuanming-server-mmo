package redisslot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 玩家域键形的跨语言金样:cpp/tests/currency_test/currency_test.cpp 的 PlayerDomainRedisKeysMatchGoContract
// 钉同一组字面量。改任何一个都意味着跨语言契约变了,两侧同批改。
func TestPlayerKeyGoldenVectors(t *testing.T) {
	const maxID = uint64(18446744073709551615)
	cases := []struct {
		got  string
		want string
	}{
		{LocationKey(42), "player:{42}:location"},
		{LocationKey(maxID), "player:{18446744073709551615}:location"},
		{LocationRevisionKey(42), "player:{42}:location_revision"},
		{LocationRevisionFloorKey(42), "player:{42}:location_revision_floor"},
		{LeaveReleaseWaitKey(42), "player:{42}:leave_release_wait"},
		{ChannelSwitchCooldownKey(42), "player:{42}:channel_switch_cooldown"},
		{EnterSceneDedupKey(42, "req-1"), "enter_scene:dedup:{42}:req-1"},
		{LoginGenerationKey(42), "player:{42}:login_generation"},
		{PlayerLockerKey(42), "player_locker:{42}"},
		{LoginIdempotentKey(42, "req-1"), "login_idempotent:{42}:req-1"},
		{BattleLockKey(42), "battle:lock:{42}"},
		{BattleLockKey(maxID), "battle:lock:{18446744073709551615}"},
		{BattleCtxKey(42), "battle:ctx:{42}"},
		{AfkPassKey(42), "player:afk_pass:{42}"},
		{SessionKey(42), "player:session:{42}"},
		{SessionKey(maxID), "player:session:{18446744073709551615}"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("键形 = %q, want %q", c.got, c.want)
		}
	}
}

// 同一玩家的键全部落在玩家所在 slot:这是同一段 Lua 能声明它们的前提。
func TestPlayerKeysShareThePlayerSlot(t *testing.T) {
	for _, pid := range []uint64{1, 42, 1000003, 18446744073709551615} {
		slot := PlayerSlot(pid)
		for _, key := range []string{
			LocationKey(pid), LocationRevisionKey(pid), LocationRevisionFloorKey(pid), LeaveReleaseWaitKey(pid),
			ChannelSwitchCooldownKey(pid), EnterSceneDedupKey(pid, "r"), LoginGenerationKey(pid), PlayerLockerKey(pid),
			LoginIdempotentKey(pid, "r"), BattleLockKey(pid), BattleCtxKey(pid), AfkPassKey(pid), SessionKey(pid),
		} {
			if !HasHashTag(key) {
				t.Errorf("pid %d: %q 没带 hash tag", pid, key)
			}
			if got := HashSlot(key); got != slot {
				t.Errorf("pid %d: %q 在 slot %d, 玩家在 slot %d", pid, key, got, slot)
			}
		}
	}
}

func TestParseSessionKey(t *testing.T) {
	for _, pid := range []uint64{1, 42, 18446744073709551615} {
		got, ok := ParseSessionKey(SessionKey(pid))
		if !ok || got != pid {
			t.Fatalf("ParseSessionKey(SessionKey(%d)) = %d, %v", pid, got, ok)
		}
	}
	for _, key := range []string{
		"", "player:session:42", "player:session:{}", "player:session:{0}", "player:session:{042}",
		"player:session:{18446744073709551616}", "player:session:{4x}", "player:location:{42}", "player:session:{42}x",
	} {
		if got, ok := ParseSessionKey(key); ok {
			t.Errorf("ParseSessionKey(%q) = %d, 应拒绝", key, got)
		}
	}
	if !strings.HasPrefix(SessionKey(42), strings.TrimSuffix(SessionKeyPattern, "{*}")) {
		t.Fatalf("SessionKeyPattern = %q 与 SessionKey 的前缀不一致", SessionKeyPattern)
	}
}

// C++ 用 hiredis 格式串拼战斗键,这里逐字解析头文件与 Go 的构造函数比对:两边各写各的一旦漂移,
// match 读不到 scene 写的锁,战斗中的玩家能排进下一局,而且一个错都不报。
func TestBattleKeyFormatsMatchCpp(t *testing.T) {
	const header = "cpp/libs/services/scene/battle/system/battle_lock_rules.h"
	data, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(header)))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("找不到 C++ 源码(只拷了 go/ 的构建环境),跳过逐字比对")
	}
	if err != nil {
		t.Fatalf("读 %s: %v", header, err)
	}
	src := string(data)
	const pid = uint64(42)
	for name, want := range map[string]string{
		"kBattleLockKeyFmt": BattleLockKey(pid),
		"kBattleCtxKeyFmt":  BattleCtxKey(pid),
	} {
		literal := cppCharArrayConstant(t, src, name)
		if got := strings.ReplaceAll(literal, "%llu", strconv.FormatUint(pid, 10)); got != want {
			t.Errorf("C++ %s = %q,展开后 %q 与 Go %q 不一致", name, literal, got, want)
		}
	}
}

// cppCharArrayConstant 取 `<name>[] = "..."` 的字面量。常量改名或改写法时让用例失败。
func cppCharArrayConstant(t *testing.T, src, name string) string {
	t.Helper()
	decl := name + "[] = \""
	start := strings.Index(src, decl)
	if start < 0 {
		t.Fatalf("C++ 头文件里找不到 %s(按 %q 定位)", name, decl)
	}
	rest := src[start+len(decl):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("C++ 常量 %s 的字面量没有闭合", name)
	}
	return rest[:end]
}
