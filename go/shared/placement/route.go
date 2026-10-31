package placement

// player_route 的行分片。真源是全局库 player_route(data_service 的 store 包),读写都按聚簇键
// (route_shard, player_id) 点查(docs/design/player-route-mapping-durability-proposal.md §15)。

// RouteRowShards 是 player_route 的固定分片数:route_shard = player_id % RouteRowShards,
// 是聚簇主键 (route_shard, player_id) 的首列。目的是把 player_id 在 TiDB 上拆成 64 条写入尾部
// (提案 §11.2「第 1a 批热点修复」),避免开服首日只写同一个尾部 Region。
//
// **模数建表后不可改**(任何环境建过 player_route 之后):改模数 = 每一行换主键 = 整表重写;
// data_service store 包 schema.go 的 chk_route_shard 里写死的模数与这里必须一致(route_store_test.go 钉住)。
// 上限 256:route_shard 列是 tinyint unsigned。
//
// 2026-10-13 按容量模型(docs/design/capacity-model-hok-scale.md §4.1)定为 64:单 Region 的写入上限没测过,
// 而模数建表后改不了,取 64 是保险。
const RouteRowShards uint32 = 64

// RouteRowShard 返回 player_route.route_shard。写者与读者都只能经它计算,不得自己再写一份取模:算错片的写者
// 在 CHECK 不生效的库上能给同一个 player_id 写出两行,而按正确片点查的读者看不到另一行。
func RouteRowShard(playerID uint64) uint32 {
	return uint32(playerID % uint64(RouteRowShards))
}
