package main

import "testing"

// 幂等键必须非 0(0 = 旧客户端、服务器不去重),且每次都是新的:同一个账号反复跑冒烟,账本里留着上一轮的键。
func TestSmokeClientRequestKeyIsNonZeroAndDistinct(t *testing.T) {
	const draws = 1000
	seen := make(map[uint64]bool, draws)
	for i := 0; i < draws; i++ {
		key := smokeClientRequestKey()
		if key == 0 {
			t.Fatal("generated key 0, which turns deduplication off")
		}
		if seen[key] {
			t.Fatalf("key %d generated twice in %d draws", key, draws)
		}
		seen[key] = true
	}
}
