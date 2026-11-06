// Package placement 是玩家路由的公共契约:落点编号与库名的换算(本文件),player_route 的行分片(route.go),
// 别名合服的归属解析(merged_zone.go)。
//
// home_zone 回答「玩家建号时属于哪个区」,storage_id 回答「玩家主数据在哪个库」,都只在目录 player_route 一份
// (docs/design/horizontal-scale-logical-db.md §5.6)。合服不改这两个值,只写一条 zone_merged_into 别名;storage_id 建角后不变。
// 选库只在 go/db 一处发生:认领时调 GetPlayerRoute 选,生产者消息里不带任何落点信息(docs/design/player-storage-placement.md §0.2、§3 P-3)。
//
// 本包只依赖标准库:读写各自用自己的连接。
package placement

import (
	"fmt"
	"strconv"
	"strings"
)

// 落点编号空间(§4.2),库名由编号唯一派生,不需要配置表:
//   - 0:未指定;
//   - 1..MaxZoneStorageID:zone 库,库名 zone_{id}_db;
//   - >= FirstNonZoneStorageID:非 zone 库(如 Phase 2 全局库),库名 player_store_{id}_db。
const (
	FirstNonZoneStorageID  uint32 = 1000000
	MaxZoneStorageID       uint32 = FirstNonZoneStorageID - 1
	DefaultGlobalStorageID uint32 = FirstNonZoneStorageID
)

// 地域存档库(docs/design/player-data-tidb-scaling-plan.md §7.2):每个地域固定 K 个存档逻辑库,和区无关。
//
//	storage_id = FirstNonZoneStorageID + 地域码 × RegionStorageStride + 片号(0..K-1)
//
// 库名照旧由 StoreDBName 派生为 player_store_{storage_id}_db,例如地域 1 的 7 号库是 player_store_1001007_db;
// 地域 0(dev、test)的 0 号库正好是 DefaultGlobalStorageID。新角色建号时在本地域的 K 个库里按 player_id 取模选一个,
// 写进目录 player_route.storage_id;之后读写只认目录,不再按公式推,所以以后整库搬集群、别名合服都不用改玩家记录。
// K 上线后不能改(和目录分片数 Directory.LogicalShards 同一条规矩):改了取模结果就变,只影响之后建的号,但会让
// 各库的玩家数从此不均。扩容靠把逻辑库整库搬到新集群(tools/dbmove)。
const (
	// RegionStorageStride 是每个地域占的编号数,也是 K 的上限:片号 0..999。
	RegionStorageStride uint32 = 1000
	// MaxRegionCode 与发号器计数器的 6 位地域码同一值域(总体方案 §6.1):0 只给 dev、test。
	MaxRegionCode uint32 = 63
)

// RegionStorageID 返回地域 region 的第 shard 个存档库编号。
func RegionStorageID(region, shard uint32) (uint32, error) {
	if region > MaxRegionCode {
		return 0, fmt.Errorf("placement: region %d is out of range 0..%d", region, MaxRegionCode)
	}
	if shard >= RegionStorageStride {
		return 0, fmt.Errorf("placement: storage shard %d is out of range 0..%d", shard, RegionStorageStride-1)
	}
	return FirstNonZoneStorageID + region*RegionStorageStride + shard, nil
}

// ValidateRegionStorage 校验地域码与存档库数 K:K 在 1..RegionStorageStride 之间。
func ValidateRegionStorage(region, shards uint32) error {
	if region > MaxRegionCode {
		return fmt.Errorf("placement: region %d is out of range 0..%d", region, MaxRegionCode)
	}
	if shards == 0 || shards > RegionStorageStride {
		return fmt.Errorf("placement: storage shards %d is out of range 1..%d", shards, RegionStorageStride)
	}
	return nil
}

// RegionStorageIDForPlayer 给新角色选存档库:本地域 K 个库里按 player_id 取模。
// player_id 是发号器打散过的值(总体方案 §6.1),取模后均匀;0 不是合法玩家号。
func RegionStorageIDForPlayer(region, shards uint32, playerID uint64) (uint32, error) {
	if err := ValidateRegionStorage(region, shards); err != nil {
		return 0, err
	}
	if playerID == 0 {
		return 0, fmt.Errorf("placement: player_id 0 has no storage")
	}
	return RegionStorageID(region, uint32(playerID%uint64(shards)))
}

// RegionStorageDBNames 按片号顺序列出地域 region 的 K 个存档库名,给部署和本机脚本生成库名白名单、建库清单用。
func RegionStorageDBNames(region, shards uint32) ([]string, error) {
	if err := ValidateRegionStorage(region, shards); err != nil {
		return nil, err
	}
	names := make([]string, 0, shards)
	for shard := uint32(0); shard < shards; shard++ {
		id, err := RegionStorageID(region, shard)
		if err != nil {
			return nil, err
		}
		name, _ := StoreDBName(id)
		names = append(names, name)
	}
	return names, nil
}

// IsZoneStorage 判断 storageID 是否属于 zone 库家族。
func IsZoneStorage(storageID uint32) bool {
	return storageID >= 1 && storageID <= MaxZoneStorageID
}

// StoreDBName 返回落点库名;storageID 为 0 时返回 false。
// zone 家族的规则与 go/db/internal/config.ZoneDBName 相同,改一处必须两处同改。
func StoreDBName(storageID uint32) (string, bool) {
	switch {
	case storageID == 0:
		return "", false
	case IsZoneStorage(storageID):
		return fmt.Sprintf("zone_%d_db", storageID), true
	default:
		return fmt.Sprintf("player_store_%d_db", storageID), true
	}
}

// StorageIDFromDBName 是 StoreDBName 的反函数,用于白名单之外的「家族」判定(§6.1)。
// 名字不属于任何家族时返回 false。
func StorageIDFromDBName(name string) (uint32, bool) {
	var digits string
	var zone bool
	switch {
	case strings.HasPrefix(name, "zone_") && strings.HasSuffix(name, "_db"):
		digits, zone = strings.TrimSuffix(strings.TrimPrefix(name, "zone_"), "_db"), true
	case strings.HasPrefix(name, "player_store_") && strings.HasSuffix(name, "_db"):
		digits = strings.TrimSuffix(strings.TrimPrefix(name, "player_store_"), "_db")
	default:
		return 0, false
	}
	if digits == "" || digits[0] == '0' {
		return 0, false
	}
	id, err := strconv.ParseUint(digits, 10, 32)
	if err != nil {
		return 0, false
	}
	storageID := uint32(id)
	if zone != IsZoneStorage(storageID) {
		return 0, false
	}
	return storageID, true
}
