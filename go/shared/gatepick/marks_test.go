package gatepick

import (
	"testing"

	"shared/redisslot"
)

// 键名是 login、scene_manager 与排空脚本之间的契约:改了任何一边都读不到对方写的标记,而且不报错。
func TestMarkKeys_Golden(t *testing.T) {
	for got, want := range map[string]string{
		DrainingKey(3, 7):                      "gate:{gm:3}:7:draining",
		DrainedKey(3, 7):                       "gate:{gm:3}:7:drained",
		StaleKey(3, 7):                         "gate:{gm:3}:7:stale",
		StaleKey(4_294_967_295, 4_294_967_295): "gate:{gm:4294967295}:4294967295:stale",
		DrainingKey(0, 1):                      "gate:{gm:0}:1:draining",
		MarkTag(12):                            "{gm:12}",
	} {
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// 一个 zone 的三种标记、所有 gate 必须同槽:读者一次 MGET 读一个 zone 的全部 gate,排空脚本在一段 Lua 里核对同 zone 其它
// 候选的排空键。tag 还得是控制集群登记过的族,否则控制集群句柄的守卫会把它当成玩家域的键拒绝。
func TestMarkKeys_OneZoneSharesOneSlotAndARegisteredTag(t *testing.T) {
	const zone = 5
	keys := []string{
		DrainingKey(zone, 1), DrainedKey(zone, 1), StaleKey(zone, 1),
		DrainingKey(zone, 900_001), DrainedKey(zone, 900_001), StaleKey(zone, 900_001),
	}
	for _, key := range keys {
		tag, tagged := redisslot.HashTag(key)
		if !tagged || tag != "gm:5" || !redisslot.IsControlTag(tag) {
			t.Fatalf("%q: tag=%q tagged=%v, want the registered control tag gm:5", key, tag, tagged)
		}
		if redisslot.HashSlot(key) != redisslot.HashSlot(keys[0]) {
			t.Fatalf("%q and %q are in different slots", keys[0], key)
		}
	}
	// 不同 zone 的标记不共用 tag:它们之间没有原子操作,可以落在不同主节点上。
	other, _ := redisslot.HashTag(DrainingKey(zone+1, 1))
	if other == "gm:5" {
		t.Fatal("marks of different zones must not share a hash tag")
	}
}

// 停写标记每 ReportPeriod 续写一次:过期时间要盖住至少两次漏写,否则一次网络抖动就让读者暂时看不见一台卡住的 gate。
func TestStaleMarkTTL_CoversMissedRefreshes(t *testing.T) {
	if StaleMarkTTL != StaleAfter {
		t.Fatalf("StaleMarkTTL = %s, want StaleAfter (%s)", StaleMarkTTL, StaleAfter)
	}
	if StaleMarkTTL <= 2*ReportPeriod {
		t.Fatalf("StaleMarkTTL = %s must exceed two refresh periods (%s)", StaleMarkTTL, 2*ReportPeriod)
	}
}
