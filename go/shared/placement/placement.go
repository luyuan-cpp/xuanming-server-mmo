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
