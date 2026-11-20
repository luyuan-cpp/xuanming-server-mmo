// Package inboxshard 保存玩家收件箱已经建表的分片契约；服务与跨域挂起守卫共用。
package inboxshard

// ShardCount 与route分片模数独立，改变即迁移全部收件箱主键。
const ShardCount = 64

func RowShard(playerID uint64) uint32 { return uint32(Mix64(playerID) % ShardCount) }

// Mix64 是 splitmix64 终结函数:分片只靠它摊匀,不依赖 player_id 的位布局(发号器的号没有位布局,
// AGENTS.md §7 第 1 条)。改它和改 ShardCount 一样要迁移全部收件箱主键。
func Mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
