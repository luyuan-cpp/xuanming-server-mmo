package poolowner

import (
	"fmt"
	"testing"
)

func TestOwnerIsOrderIndependentAndUnique(t *testing.T) {
	members := []string{"a", "b", "c"}
	reversed := []string{"c", "b", "a"}
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("5:%d", i)
		owner := Owner(key, members)
		if owner == "" || owner != Owner(key, reversed) {
			t.Fatalf("key %s 归属与成员顺序有关: %q vs %q", key, owner, Owner(key, reversed))
		}
		owners := 0
		for _, m := range members {
			if Owns(m, key, members) {
				owners++
			}
		}
		if owners != 1 {
			t.Fatalf("key %s 应恰好一个负责副本, got %d", key, owners)
		}
	}
}

func TestMembershipChangeMovesOnlyAffectedKeys(t *testing.T) {
	before := []string{"a", "b", "c"}
	after := []string{"a", "b", "c", "d"}
	moved := 0
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("3:%d", i)
		was, now := Owner(key, before), Owner(key, after)
		if was != now {
			if now != "d" {
				t.Fatalf("key %s 从 %s 换到 %s，只应换到新副本", key, was, now)
			}
			moved++
		}
	}
	if moved == 0 || moved == 1000 {
		t.Fatalf("新增副本应分走一部分键, moved=%d", moved)
	}
}

func TestEmptyAndUnregistered(t *testing.T) {
	if Owner("k", nil) != "" || Owns("a", "k", nil) {
		t.Fatal("空成员表不应有负责者")
	}
	if Owns("", "k", []string{""}) || Owns("x", "k", []string{"a", "b"}) {
		t.Fatal("未注册的副本不负责任何池")
	}
}
