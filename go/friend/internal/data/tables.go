// Package data 是 friend 的存储层:mmorpg_friend 库的表清单与手写参数化 SQL。
//
// 表结构的唯一事实源是 proto/friend/friend_table.proto(D-14 第 2 条):建表 / 加列只经
// go/schemamigrate,本包不写任何 DDL。列的增删一律先改 proto 再让 Codex 重生成 ——
// 反过来先改 SQL 会让 schemamigrate 的 Plan 与真实库永久对不上,启动即拒。
//
// 注意:friend_repo.go 里目前还留着几个手写 struct(FriendEntry / FriendRequestEntry 等),
// 它们是 Redis 缓存的 JSON 形状,不是表结构的事实源;把行直接扫进 pb message(AGENTS.md §3)
// 属于 F2 批的事务重写,本批不动。
package data

import (
	friendpb "proto/friend"

	"google.golang.org/protobuf/proto"
)

// DatabaseName 是 friend 独占的逻辑库名,本地与 K8s 同名(D-14 第 1 条)。
// config.Validate 断言 MySQL.DBName 等于它;schemamigrate 连接后再断言 SELECT DATABASE() 等于它 ——
// 两道防线都是为了不把 friend 的表建进别的服务的库(friend / guild 的表历史上同在一个初始化脚本里,
// 误连 mmorpg_guild 不会报错,只会把表建到错的地方)。
const DatabaseName = "mmorpg_friend"

// Tables 返回本库全部表对应的 message。每个 message 都带 OptionTableName;
// 新增表 = 在 friend_table.proto 加 message 并在这里追加一项(schemamigrate 会把后加的表建出来)。
//
// 顺序固定为 边 → 申请 → 配额 → 拉黑,与 friend_table.proto 的 message 顺序一致:
// schemamigrate 按本切片顺序建表,顺序稳定 = Plan 输出可逐行 diff,评审时不会被重排噪声淹没。
func Tables() []proto.Message {
	return []proto.Message{
		&friendpb.FriendEdgeRecord{},
		&friendpb.FriendRequestRecord{},
		&friendpb.FriendCapacityRecord{},
		&friendpb.FriendBlockRecord{},
	}
}
