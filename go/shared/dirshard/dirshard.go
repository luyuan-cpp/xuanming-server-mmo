// Package dirshard 是账号、玩家、名字目录的分片规则(docs/design/horizontal-scale-logical-db.md §5.1)。
//
// 目录分成 K 个逻辑库,K 是 64 的约数。64 个槽固定,K 只决定槽怎么合并到库。
// 这些函数的结果上线后不能变:变了就是把已有的行路由到别的库,dirshard_test.go 钉着金样。
// K 也不能改,扩容靠把逻辑库搬到新集群(§8)。
package dirshard

import (
	"fmt"
	"hash/crc32"

	"shared/placement"
)

// Slots 是固定的槽数,等于 player_route 的行分片数。K 必须整除它:玩家的槽就是行分片,一个库里的行分片才是连续可预切的。
const Slots = int(placement.RouteRowShards)

// Validate 核 K。0 不合法,调用方要先把「没配」换成 1(Normalize)。
func Validate(k int) error {
	if k < 1 || k > Slots || Slots%k != 0 {
		return fmt.Errorf("directory logical shards %d must be a divisor of %d", k, Slots)
	}
	return nil
}

// Normalize 把「没配」换成 1。负数不纠正,留给 Validate 拒绝。
func Normalize(k int) int {
	if k == 0 {
		return 1
	}
	return k
}

// SlotOfPlayer 是玩家所在的片:角色行、路由行、入场登记、过户收据、名字副本都放这里。
func SlotOfPlayer(id uint64, k int) int { return int(placement.RouteRowShard(id)) % k }

// SlotOfAccount 是账号所在的片:账号目录和账号到角色的索引放这里。账号按原字节算,不 trim、不 casefold,和目录的比较口径相同。
func SlotOfAccount(account string, k int) int { return slotOfBytes(account, k) }

// SlotOfName 是名字所在的片。norm 必须是 playername.Normalize 算出来的判重键。
func SlotOfName(norm string, k int) int { return slotOfBytes(norm, k) }

func slotOfBytes(key string, k int) int {
	return int(crc32.ChecksumIEEE([]byte(key))%uint32(Slots)) % k
}

// DirectoryDBName 是第 slot 片的库名:K=1 就是 base,否则 <base>_00。
func DirectoryDBName(base string, slot, k int) string {
	if k == 1 {
		return base
	}
	return fmt.Sprintf("%s_%02d", base, slot)
}

// DirectoryDBNames 是全部片的库名,下标就是片号。
func DirectoryDBNames(base string, k int) []string {
	names := make([]string, k)
	for slot := range names {
		names[slot] = DirectoryDBName(base, slot, k)
	}
	return names
}

// LedgerDBName 是流水和快照的逻辑库(transaction_log、player_snapshot、rollback_audit_log)。
// 不分片,和目录分开:写入量大,不和目录抢闸门和连接。
func LedgerDBName(base string) string { return base + "_ledger" }
