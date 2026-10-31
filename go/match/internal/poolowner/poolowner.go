// Package poolowner 决定一个匹配池由哪个 match 副本负责撮合
// （docs/design/global-match-hierarchical-pools.md §5.2）。
//
// 用最高随机权重哈希（rendezvous / HRW）：每个副本对每个键算一个分数，分数最高者负责。
// 副本增减时只有原本落在增减副本上的键换主，其余池不动。成员表来自 etcd 里
// 注册的 MatchNodeService 实例，所有副本看到的是同一份注册信息。
//
// 归属只是分工，不承担正确性：成员表短暂不一致时两个副本可能同时撮合同一个池，
// 提交点（authority.claimTx 的行锁与代次复核）保证同一玩家不会进两局，代价只是几次提交冲突。
package poolowner

import (
	"hash/fnv"
)

// Owner 返回 key 的负责副本；members 为空返回空串。结果只取决于成员集合，与顺序无关。
func Owner(key string, members []string) string {
	var owner string
	var best uint64
	for _, member := range members {
		if member == "" {
			continue
		}
		score := weight(member, key)
		if owner == "" || score > best || (score == best && member < owner) {
			owner, best = member, score
		}
	}
	return owner
}

// Owns 报告 self 是否负责 key。self 不在成员表里（尚未注册或已注销）时不负责任何池。
func Owns(self, key string, members []string) bool {
	return self != "" && Owner(key, members) == self
}

func weight(member, key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(member))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}
