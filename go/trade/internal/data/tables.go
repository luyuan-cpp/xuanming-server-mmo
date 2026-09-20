// Package data 是 trade 的存储层:mmorpg_trade 库的表清单与手写参数化 SQL。
//
// 表结构的唯一事实源是 proto/trade/trade_table.proto(D-14 第 2 条):建表 / 加列只经
// go/schemamigrate,本包不写任何 DDL;行直接扫进 tradepb.TradeListingRecord / TradeFavoriteRecord,
// 不另写 Go struct(AGENTS.md §3)。
package data

import (
	tradepb "proto/trade"

	"google.golang.org/protobuf/proto"
)

// DatabaseName 是 trade 独占的逻辑库名,本地与 K8s 同名(D-14 第 1 条)。
// config.Validate 断言 MySQL.DBName 等于它;schemamigrate 连接后再断言 SELECT DATABASE() 等于它 ——
// 两道防线都是为了不把 trade 的表建进别的服务的库。
const DatabaseName = "mmorpg_trade"

// Tables 返回本库全部表对应的 message。每个 message 都带 OptionTableName;
// 新增表 = 在 trade_table.proto 加 message 并在这里追加一项(schemamigrate 会把后加的表建出来)。
func Tables() []proto.Message {
	return []proto.Message{
		&tradepb.TradeListingRecord{},
		&tradepb.TradeFavoriteRecord{},
		// 通用资产通道(guild-phase2/04-asset-channel.md §S4)在 trade 侧的两张表。
		// schemamigrate 会把"基线之后才加进清单"的表用 CREATE TABLE IF NOT EXISTS 补出来
		// (它相对 go/db 修掉的正是这个缺口),所以已经建过库的环境不需要手工 DDL。
		&tradepb.TradePlayerOpSeqRecord{},
		&tradepb.TradeAssetOpRecord{},
	}
}
