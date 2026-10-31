// Package savebarrier 定义 player:{id}:save_barrier 这个 hash 的键名、交接资格字段,以及建角时写权行的初始版本。
// 存档不在这里:存档只在 TiDB 的 player_store。hash 里只剩交接资格(handoff_fence.go),它是 Redis 表 C 的协调用途
// (docs/design/global-data-layer-tidb-decision.md §9.3)。键名没改,C++ 的交接脚本按同一个键读写。
package savebarrier

import "fmt"

// InitialOwnerEpoch/InitialSaveSeq 只供严格全新 ID 的目录和 player_store 初始行共同提交使用。
// 这不是 actor 号源：加载先采纳此地板，首任 actor 仍由 SceneManager 从 1 铸出（取 max(2, 当前 + 1)）。
const InitialOwnerEpoch uint64 = 1
const InitialSaveSeq uint64 = 1

// Key 不设 TTL。
func Key(playerID uint64) string {
	return fmt.Sprintf("player:{%d}:save_barrier", playerID)
}
