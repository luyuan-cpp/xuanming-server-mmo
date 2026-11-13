// Package inbox 是玩家持久收件箱的核心:类型注册表、owner_epoch 围栏、seq 分配、分片、
// 存储(入箱 / 拉取 / 确认 / 作废 / 查询)、在线提醒与只在领导者上跑的后台循环
// (docs/design/player-inbox-service.md §3–§10)。
package inbox

import "shared/inboxshard"

// ShardCount 是收件箱三张表的 shard 模数(设计 §4.2)。
//
// 与 placement.RouteRowShard 同为 64,但**常量独立定义、不联动修改**:两张表的模数各自是建表时定下的契约,
// 改它等于改全部存量行的主键。TiDB 默认不执行 CHECK 约束,shard 的一致性靠这几样东西保证:
// 本文件唯一的计算函数、按分片扫描时的读出断言(checkRowShard)、整库核对(CheckRowShards)、单测。
const ShardCount = inboxshard.ShardCount

// RowShard 返回 player_id 所在的 shard = mix64(player_id) % 64。**全服务唯一的计算入口**,SQL 里一律传它的结果,
// 不在别处取模(评审 L5)。
//
// 为什么先混合再取模:分片不能依赖 player_id 的位布局。定这条契约时 player_id 还有 bwmarrin SnowFlake 一套号源,
// 低 9 位是 step,同一毫秒只建一个角色时 step = 0,直接 % 64 会把这批玩家全部压进 shard 0。现在 player_id 只由
// 发号器发(AGENTS §7 第 1 条;login 的 bwmarrin 号源 2026-11-12 已删),mix64 与任何位布局无关,换号源不用动建表契约。
//
// 这是建表契约(2026-10-16 首次建表前由 player_id % 64 改为现公式)。按旧公式写出的行**不会**被读出断言拦下
// (评审 R3):按玩家的读写全都带 WHERE `shard` = RowShard(player_id),旧行根本查不出来,是静默不可见 ——
// 锁内判重读不到它、同一 request_id 会再入箱一次;只有按分片扫描的读(DueNotifications)会撞上 checkRowShard。
// 所以按旧公式写过数据的开发库必须先清空收件箱三张表;CheckRowShards 整库核对,发现一行不一致即报错。
// 上线之后再改公式等于迁移全部主键。
//
// 唯一公式已移到shared/inboxshard；data_service挂起护栏与本服务共用，不再复制第二份算法。
func RowShard(playerID uint64) uint32 {
	return inboxshard.RowShard(playerID)
}

// mix64 是 splitmix64 的终结函数(Steele、Lea、Flood 2014;即 Stafford 的 Mix13 参数):
// 两轮「右移异或 + 乘奇数常量」,是 uint64 上的双射,mix64(0) = 0。参考值见 shard_test.go TestMix64MatchesSplitMix64。
func mix64(x uint64) uint64 {
	return inboxshard.Mix64(x)
}
