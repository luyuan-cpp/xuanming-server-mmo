// Package playerstore 是玩家存盘直接提交 TiDB 的内核:写权判定,以及 Save、LoadAndClaim 两个事务。
//
// 规格见 docs/design/player-save-direct-commit.md。这里不碰 Redis、Kafka,也不认识 gRPC:
// 调用方按玩家落点选好库,把 *sql.DB 交进来。
package playerstore

import (
	"bytes"

	storepb "proto/player_store"
)

// ownerRow 是写权行(kind=0)里判定要用的内容。released 只写不判:释放不是终态,见 decideSave。
type ownerRow struct {
	ownerEpoch uint64
	saveSeq    uint64
	schemaGen  uint32
	holder     []byte
}

// decideSave 是 Save 的写权判定(规格 §4)。row 为 nil 表示写权行不存在。
// E、q、实例是否为空由 validateSave 在进事务前挡掉,这里不再判。
//
// 三个容易改错的地方:
//   - E 比库里高、持有者是自己,是合法状态,要放行并抬代次。路由回滚后采纳新 E、墙钟落后时复用实体、
//     载入在途被新路由覆盖,都会走到这里;拒绝的话实体再也存不进,下线后进度全丢。
//     有中间持有者时持有者必不是自己,不会误放。
//   - 同实例已释放也放行,写入时按请求重写 released。断线后 30 秒内重连会复用实体,
//     把释放当终态的话,重连之后的进度全丢。
//   - 持有者核对排在同序号判定之前:不能让别的实例靠序号相同拿到「幂等成功」。
func decideSave(row *ownerRow, ownerEpoch, saveSeq uint64, schemaGen uint32, instance []byte, knownGen uint32) storepb.SavePlayerResult {
	switch {
	case row == nil:
		return storepb.SavePlayerResult_SAVE_PLAYER_NO_OWNER
	case ownerEpoch < row.ownerEpoch:
		return storepb.SavePlayerResult_SAVE_PLAYER_STALE_OWNER
	case !bytes.Equal(row.holder, instance):
		if ownerEpoch == row.ownerEpoch {
			return storepb.SavePlayerResult_SAVE_PLAYER_NOT_HOLDER
		}
		return storepb.SavePlayerResult_SAVE_PLAYER_UNCLAIMED
	case saveSeq == row.saveSeq:
		return storepb.SavePlayerResult_SAVE_PLAYER_EQUAL
	case saveSeq < row.saveSeq:
		return storepb.SavePlayerResult_SAVE_PLAYER_OLDER_SEQ
	case schemaGen < row.schemaGen:
		return storepb.SavePlayerResult_SAVE_PLAYER_SCHEMA_REGRESSED
	case schemaGen > knownGen:
		return storepb.SavePlayerResult_SAVE_PLAYER_SCHEMA_TOO_NEW
	default:
		return storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED
	}
}

// decideClaim 是 LoadAndClaim 的写权判定(规格 §4)。row 为 nil 表示写权行不存在:
// 角色不存在或未建完,不造空档。
func decideClaim(row *ownerRow, ownerEpoch uint64, instance []byte, binarySchemaGen uint32) storepb.LoadAndClaimResult {
	switch {
	case row == nil:
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_NO_OWNER
	case ownerEpoch < row.ownerEpoch:
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_STALE_OWNER
	case ownerEpoch == row.ownerEpoch && len(row.holder) > 0 && !bytes.Equal(row.holder, instance):
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_NOT_HOLDER
	case row.schemaGen > binarySchemaGen:
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_SCHEMA_TOO_NEW
	default:
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED
	}
}
