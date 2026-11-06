package store

import (
	"strconv"
	"strings"
)

// 审计两张表(transaction_log、player_snapshot)的分桶列 shard(2026-11-06,缺口清单第 15 条,
// docs/design/audit-pipeline-horizontal-scale-20261106.md §2.C)。
//
// 为什么要它:TiDB 的二级索引按键有序存放。timestamp_sec / created_at 单调增长,低基数前缀(tx_type、
// item_config_id、zone_id)+ 时间的索引同样只在末尾追加,全服的追加写都落在每条索引最末尾的同一个 Region,
// 加 TiKV 也只有那一个 leader 在写。在时间列前面插一列 shard,每条按时间的索引就从「一个末尾」变成「16 段各自的末尾」。
// 索引清单的真源是 proto/common/database/rollback_database_table.proto 的 OptionIndex。

// AuditShardCount 是分桶数,与两张表的 SHARD_ROW_ID_BITS=4 相同(2^4 = 16)。
// **改它 = 改已落库行的 shard 含义**:按段删的清理与按段扫的查询都会漏掉落在新编号之外的老行。
// 上线后不能改;开发期改了要重建流水库(项目未上线,数据可清)。
const AuditShardCount = 16

// AuditShard 是一行审计记录的分桶号:流水传 tx_id,快照传行号 id。两者都是发号器打散过的值
// (idsegment.MixID),取模后均匀。规则只有这一处。
func AuditShard(id uint64) uint32 { return uint32(id % AuditShardCount) }

// auditShardInSQL 是「所有桶」的谓词 `shard IN (0,1,…,15)`。按时间 / tx_type / item_config_id / zone_id 过滤、
// 又没有玩家条件的查询带上它,优化器才能把 (…, shard, 时间) 索引按 16 段区间扫;不带时 shard 这一列没有条件,
// 这些索引只能用到 shard 之前的前缀(或者整表扫)。全是常量,直接拼进 SQL,不占占位符。
var auditShardInSQL = func() string {
	var b strings.Builder
	b.WriteString("shard IN (")
	for i := 0; i < AuditShardCount; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteByte(')')
	return b.String()
}()
