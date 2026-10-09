// Package placement 是玩家「存储落点」记录在 Redis 里的键名 / 值格式契约
// (docs/design/player-storage-placement.md §4)。
//
// home_zone(player:zone:{id})回答「玩家属于哪个区」,落点记录回答「玩家主数据在哪个库」。
// 两者分开之后,合服只改 home_zone,搬库只改落点记录。选库只在 go/db 一处发生:
// 按处理那一刻的记录选,生产者消息里不带任何落点信息(§0.2、§3 P-3)。
//
// 键与值(与 tools/merge_zone/placement_codec.go 的镜像一字不差,两边用同一组测试向量字面量):
//
//	player:placement:{player_id}                    与 player:zone 同库(mapping Redis,DB 0),无 TTL
//	"{storage_id}:{version}"                        稳定态
//	"{storage_id}:{version}:frozen:{run_id}"        冻结(搬库进行中,run_id 由搬库工具生成)
//
// 读方的约定:
//   - 键不存在 = 没有记录,有效落点按 EffectiveStorage 回落 home_zone(旧语义);
//   - 值畸形 = ErrMalformed,读方必须 fail-closed(当作查询失败,**不得**当作没有记录——
//     那会让玩家回落 home_zone 选库,正是本包要防的错库写)。
//
// 本包只依赖标准库:data_service、db 用的 Redis 客户端不同,读写各自用自己的客户端。
package placement

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// KeyPrefix 是落点记录的键前缀。
const KeyPrefix = "player:placement:"

// Key 返回 player:placement:{id}。
func Key(playerID uint64) string {
	return KeyPrefix + strconv.FormatUint(playerID, 10)
}

// HomeZoneKey 返回 player:zone:{id}。键由 data_service(go/data_service/internal/routing)
// 维护,这里镜像一份只为让 go/db 能在同一次 MGET 里读到 home_zone(§6.2)。
func HomeZoneKey(playerID uint64) string {
	return "player:zone:" + strconv.FormatUint(playerID, 10)
}

// MergeFenceKey 返回合服围栏键 merge:in_progress:{zone}(写者 tools/merge_zone/fence.go)。
func MergeFenceKey(zone uint32) string {
	return "merge:in_progress:" + strconv.FormatUint(uint64(zone), 10)
}

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

// 能力标记(§4.3):go/db 启动成功后写入,合服 pin 模式与搬库工具据此确认各 zone 的 go/db
// 已经按落点选库。能力只增不减,值变化时加新常量,不改旧值。
const (
	CapabilityKeyPrefix = "db:capability:zone:"
	CapabilityRoutingV1 = "placement-routing-v1"
)

// CapabilityKey 返回 db:capability:zone:{zone}。
func CapabilityKey(zone uint32) string {
	return CapabilityKeyPrefix + strconv.FormatUint(uint64(zone), 10)
}

// Record 是一条落点记录。
type Record struct {
	StorageID uint32
	Version   uint64
	// Frozen 为 true 表示搬库进行中:go/db 延后该玩家的写(不耗重试次数),读照常。
	Frozen bool
	// RunID 仅在 Frozen 时有意义:持有这次搬迁的工具运行号,续跑 / 撤销按它认领。
	RunID string
}

// SameRoute 判断两条记录是否指向同一个库的同一个版本(不看冻结与否),用于落库后复核(§6.3)。
func (r Record) SameRoute(o Record) bool {
	return r.StorageID == o.StorageID && r.Version == o.Version
}

// ErrMalformed 表示落点记录的值不符合格式。读方必须 fail-closed。
var ErrMalformed = errors.New("placement: malformed placement record")

const frozenTag = "frozen"

// Encode 返回记录的 Redis 值。调用方需先保证记录合法(Validate)。
func (r Record) Encode() string {
	if r.Frozen {
		return FrozenValue(r.StorageID, r.Version, r.RunID)
	}
	return StableValue(r.StorageID, r.Version)
}

// Validate 检查记录各字段:storage_id、version 必须 > 0;冻结态必须带合法 run_id。
func (r Record) Validate() error {
	if r.StorageID == 0 || r.Version == 0 {
		return fmt.Errorf("%w: storage_id and version must be > 0 (got %d:%d)", ErrMalformed, r.StorageID, r.Version)
	}
	if r.Frozen && !ValidRunID(r.RunID) {
		return fmt.Errorf("%w: invalid run_id %q", ErrMalformed, r.RunID)
	}
	if !r.Frozen && r.RunID != "" {
		return fmt.Errorf("%w: stable record must not carry run_id", ErrMalformed)
	}
	return nil
}

// StableValue 返回稳定态的值 "{storage_id}:{version}"。
func StableValue(storageID uint32, version uint64) string {
	return strconv.FormatUint(uint64(storageID), 10) + ":" + strconv.FormatUint(version, 10)
}

// FrozenValue 返回冻结态的值 "{storage_id}:{version}:frozen:{run_id}"。
func FrozenValue(storageID uint32, version uint64, runID string) string {
	return StableValue(storageID, version) + ":" + frozenTag + ":" + runID
}

// ValidRunID 限定 run_id 只含 [A-Za-z0-9_-]、非空且不超过 64 字节,保证它不会引入分隔符。
func ValidRunID(runID string) bool {
	if runID == "" || len(runID) > 64 {
		return false
	}
	for _, c := range runID {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// Parse 解析一条存在的记录值。空串也是畸形:调用方应先用「键是否存在」区分缺席。
func Parse(raw string) (Record, error) {
	parts := strings.Split(raw, ":")
	var rec Record
	switch len(parts) {
	case 2:
	case 4:
		if parts[2] != frozenTag {
			return Record{}, fmt.Errorf("%w: %q", ErrMalformed, raw)
		}
		rec.Frozen = true
		rec.RunID = parts[3]
	default:
		return Record{}, fmt.Errorf("%w: %q", ErrMalformed, raw)
	}
	storageID, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return Record{}, fmt.Errorf("%w: storage_id in %q", ErrMalformed, raw)
	}
	version, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return Record{}, fmt.Errorf("%w: version in %q", ErrMalformed, raw)
	}
	rec.StorageID = uint32(storageID)
	rec.Version = version
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// ParseHomeZone 解析 player:zone:{id} 的值(十进制 zone 字符串)。空串按缺席返回 (0,false,nil);
// 非法值与 0 视为畸形,调用方 fail-closed。
func ParseHomeZone(raw string) (uint32, bool, error) {
	if raw == "" {
		return 0, false, nil
	}
	zone, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || zone == 0 {
		return 0, false, fmt.Errorf("%w: home zone %q", ErrMalformed, raw)
	}
	return uint32(zone), true, nil
}

// EffectiveStorage 返回玩家的有效落点(§1):有记录用记录;没有记录用 home_zone;
// 都没有用 fallbackZone(处理任务的 go/db 所在 zone,旧语义)。
func EffectiveStorage(rec Record, recPresent bool, homeZone uint32, homePresent bool, fallbackZone uint32) uint32 {
	switch {
	case recPresent:
		return rec.StorageID
	case homePresent:
		return homeZone
	default:
		return fallbackZone
	}
}
