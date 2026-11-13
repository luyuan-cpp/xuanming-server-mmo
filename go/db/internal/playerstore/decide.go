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
//
// 世代倒退只对带数据行的存盘判(hasRows):它说明写方没把加载到的内容和世代带回来,这一笔会拿更旧的内容盖掉库里的。
// 不带行的存盘(实体已经没了的空行释放、只抬代次)不写任何数据;scene 那时手里可能已经没有加载世代,发的是自己
// 二进制的世代,旧 scene 持有更新世代的存档时它就比库里低。这不算倒退,否则这笔释放永远提交不了。
// 写权行的世代由写入语句取 GREATEST 保持只升不降(store.go sqlSaveTail),不会被这样一笔拉低。
func decideSave(row *ownerRow, ownerEpoch, saveSeq uint64, schemaGen uint32, hasRows bool, instance []byte, knownGen uint32) storepb.SavePlayerResult {
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
	case hasRows && schemaGen < row.schemaGen:
		return storepb.SavePlayerResult_SAVE_PLAYER_SCHEMA_REGRESSED
	case schemaGen > knownGen:
		return storepb.SavePlayerResult_SAVE_PLAYER_SCHEMA_TOO_NEW
	default:
		return storepb.SavePlayerResult_SAVE_PLAYER_COMMITTED
	}
}

// decideClaim 是 LoadAndClaim 的写权判定(规格 §4)。row 为 nil 表示写权行不存在:
// 角色不存在或未建完,不造空档。
//
// 同实例同代次的重新认领也放行,判定不看 claim_seq:它只在写入时加 1,给判死比较用。
//
// 世代只核「本 go/db 认不认识」。两种情况都回 SCHEMA_TOO_NEW、不抬代次,scene 按进场失败处理,玩家重试即可:
//   - 库里写过的世代比本 go/db 新(row.schemaGen > knownGen):go/db 回退到了比存档更旧的版本。放进来的话,
//     scene 之后每一笔存盘带的世代都不低于存档的世代,全被 decideSave 判 SCHEMA_TOO_NEW,进度只留在内存里。
//   - scene 比本 go/db 认识的世代新(binarySchemaGen > knownGen):本 go/db 存不下这个 scene 之后的存盘。
//     不拦的话,发布窗口里新 scene 经旧 go/db 认领成功、抬了代次,旧持有者被围住,新 scene 却一笔也落不了盘
//     (数据覆写审计 r2-ps#11)。
//
// 库里的世代比 scene 新不拦(2026-11-12)。scene 把自己不认识的内容原样带回:行里不认识的顶层字段由存盘状态机
// 按行保管、每次写这一行时接回去,嵌套字段跟着对象走,更新版本才有的行它不碰;存盘世代取 max(自己的, 加载到的),
// 所以不会倒退(混版本设计 §4、§5)。原先这里按「scene 解释不了这份存档」拒绝,后果是金丝雀 zone 里存过盘的玩家
// 进不了还没升级的 zone(包括回自己的 home zone),zone 也没法回退。
func decideClaim(row *ownerRow, ownerEpoch uint64, instance []byte, binarySchemaGen, knownGen uint32) storepb.LoadAndClaimResult {
	switch {
	case row == nil:
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_NO_OWNER
	case ownerEpoch < row.ownerEpoch:
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_STALE_OWNER
	case ownerEpoch == row.ownerEpoch && len(row.holder) > 0 && !bytes.Equal(row.holder, instance):
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_NOT_HOLDER
	case row.schemaGen > knownGen, binarySchemaGen > knownGen:
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_SCHEMA_TOO_NEW
	default:
		return storepb.LoadAndClaimResult_LOAD_AND_CLAIM_CLAIMED
	}
}
