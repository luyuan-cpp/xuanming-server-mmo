package main

// 玩家存储落点记录(player:placement:{id})的键名 / 值格式镜像(docs/design/player-storage-placement.md §4)。
//
// 唯一出处是 go/shared/placement(go/db 选库、data_service 建角钉落点都用那一份)。merge_zone 是独立 module,
// 引那个包要把 sarama / go-zero / etcd / grpc 与另一版 go-redis 一起拖进本工具的 go.mod,所以在这里镜像一份,
// 用与 go/shared/placement/placement_test.go **同一组测试向量字面量**钉住(placement_codec_test.go)。
// 改这里必须同步改那边,反之亦然:两边对「合法值」的理解一旦分叉,工具写下的记录 go/db 读不懂(fail-closed,
// 该玩家的存盘全部进重试 / 死信),或者 go/db 认的记录工具当成畸形而拒绝搬迁 —— 两种都不会报在工具这一侧。
//
// 键与值:
//
//	player:placement:{player_id}                    与 player:zone 同库(mapping Redis,DB 0),无 TTL
//	"{storage_id}:{version}"                        稳定态
//	"{storage_id}:{version}:frozen:{run_id}"        冻结(搬库进行中,run_id 由搬库工具生成)
//
// 读方约定(与 go/db 相同):键不存在 = 没有记录,有效落点回落 home_zone;值畸形 = errPlacementMalformed,
// 必须 fail-closed(当作查询失败),**不得**当作没有记录 —— 那会让玩家回落 home_zone,正是落点记录要防的错库。

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// placementKeyPrefix 是落点记录的键前缀(shared/placement.KeyPrefix)。
const placementKeyPrefix = "player:placement:"

// placementKey 返回 player:placement:{id}(shared/placement.Key)。
// home_zone 的键 player:zone:{id} 见 main.go 的 playerZoneKey,合服围栏键见 fence.go 的 mergeFenceKey,
// 两者与 shared/placement 的 HomeZoneKey / MergeFenceKey 逐字节相同(placement_codec_test.go 钉住)。
func placementKey(playerID uint64) string {
	return placementKeyPrefix + strconv.FormatUint(playerID, 10)
}

// 落点编号空间(§4.2),库名由编号唯一派生,不需要配置表:
//   - 0:未指定;
//   - 1..maxZoneStorageID:zone 库,库名 zone_{id}_db(与 zoneDBName 同规则);
//   - >= firstNonZoneStorageID:非 zone 库(Phase 2 全局库等),库名 player_store_{id}_db。
const (
	firstNonZoneStorageID  uint32 = 1000000
	maxZoneStorageID       uint32 = firstNonZoneStorageID - 1
	defaultGlobalStorageID uint32 = firstNonZoneStorageID
)

// isZoneStorage 判断 storageID 是否属于 zone 库家族。
func isZoneStorage(storageID uint32) bool {
	return storageID >= 1 && storageID <= maxZoneStorageID
}

// storeDBName 返回落点库名;storageID 为 0 时返回 false。zone 家族的规则与 zoneDBName 相同。
// 库名会直接拼进 SQL 标识符:它只由 uint32 格式化而来,不含任何可逃逸的字符。
func storeDBName(storageID uint32) (string, bool) {
	switch {
	case storageID == 0:
		return "", false
	case isZoneStorage(storageID):
		return fmt.Sprintf("zone_%d_db", storageID), true
	default:
		return fmt.Sprintf("player_store_%d_db", storageID), true
	}
}

// storageIDFromDBName 是 storeDBName 的反函数;名字不属于任何家族时返回 false。
func storageIDFromDBName(name string) (uint32, bool) {
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
	if zone != isZoneStorage(storageID) {
		return 0, false
	}
	return storageID, true
}

// 能力标记(§4.3):go/db 启动成功后写入(go/db/internal/kafka.MarkPlacementCapability),合服 pin 模式与搬库
// 工具据此确认各 zone 的 go/db 已经按落点选库。能力只增不减,值变化时 shared 侧加新常量,不改旧值。
const (
	capabilityKeyPrefix = "db:capability:zone:"
	capabilityRoutingV1 = "placement-routing-v1"
)

// capabilityKey 返回 db:capability:zone:{zone}。
func capabilityKey(zone uint32) string {
	return capabilityKeyPrefix + strconv.FormatUint(uint64(zone), 10)
}

// placementRecord 是一条落点记录(shared/placement.Record)。
type placementRecord struct {
	StorageID uint32
	Version   uint64
	// Frozen 为 true 表示搬库进行中:go/db 延后该玩家的写(不耗重试次数),读照常。
	Frozen bool
	// RunID 仅在 Frozen 时有意义:持有这次搬迁的工具运行号,续跑 / 撤销按它认领。
	RunID string
}

// SameRoute 判断两条记录是否指向同一个库的同一个版本(不看冻结与否)。
func (r placementRecord) SameRoute(o placementRecord) bool {
	return r.StorageID == o.StorageID && r.Version == o.Version
}

// errPlacementMalformed 表示落点记录的值不符合格式(shared/placement.ErrMalformed)。读方必须 fail-closed。
var errPlacementMalformed = errors.New("placement: malformed placement record")

const placementFrozenTag = "frozen"

// Encode 返回记录的 Redis 值。调用方需先保证记录合法(Validate)。
func (r placementRecord) Encode() string {
	if r.Frozen {
		return frozenValue(r.StorageID, r.Version, r.RunID)
	}
	return stableValue(r.StorageID, r.Version)
}

// Validate 检查记录各字段:storage_id、version 必须 > 0;冻结态必须带合法 run_id,稳定态不许带。
func (r placementRecord) Validate() error {
	if r.StorageID == 0 || r.Version == 0 {
		return fmt.Errorf("%w: storage_id and version must be > 0 (got %d:%d)", errPlacementMalformed, r.StorageID, r.Version)
	}
	if r.Frozen && !validRunID(r.RunID) {
		return fmt.Errorf("%w: invalid run_id %q", errPlacementMalformed, r.RunID)
	}
	if !r.Frozen && r.RunID != "" {
		return fmt.Errorf("%w: stable record must not carry run_id", errPlacementMalformed)
	}
	return nil
}

// stableValue 返回稳定态的值 "{storage_id}:{version}"。
func stableValue(storageID uint32, version uint64) string {
	return strconv.FormatUint(uint64(storageID), 10) + ":" + strconv.FormatUint(version, 10)
}

// frozenValue 返回冻结态的值 "{storage_id}:{version}:frozen:{run_id}"。
func frozenValue(storageID uint32, version uint64, runID string) string {
	return stableValue(storageID, version) + ":" + placementFrozenTag + ":" + runID
}

// validRunID 限定 run_id 只含 [A-Za-z0-9_-]、非空且不超过 64 字节,保证它不会引入分隔符。
func validRunID(runID string) bool {
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

// parsePlacement 解析一条存在的记录值(shared/placement.Parse)。空串也是畸形:调用方先用「键是否存在」区分缺席。
func parsePlacement(raw string) (placementRecord, error) {
	parts := strings.Split(raw, ":")
	var rec placementRecord
	switch len(parts) {
	case 2:
	case 4:
		if parts[2] != placementFrozenTag {
			return placementRecord{}, fmt.Errorf("%w: %q", errPlacementMalformed, raw)
		}
		rec.Frozen = true
		rec.RunID = parts[3]
	default:
		return placementRecord{}, fmt.Errorf("%w: %q", errPlacementMalformed, raw)
	}
	storageID, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return placementRecord{}, fmt.Errorf("%w: storage_id in %q", errPlacementMalformed, raw)
	}
	version, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return placementRecord{}, fmt.Errorf("%w: version in %q", errPlacementMalformed, raw)
	}
	rec.StorageID = uint32(storageID)
	rec.Version = version
	if err := rec.Validate(); err != nil {
		return placementRecord{}, err
	}
	return rec, nil
}

// parseHomeZone 解析 player:zone:{id} 的值(shared/placement.ParseHomeZone)。空串按缺席返回 (0,false,nil);
// 非法值与 0 视为畸形,调用方 fail-closed。
func parseHomeZone(raw string) (uint32, bool, error) {
	if raw == "" {
		return 0, false, nil
	}
	zone, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || zone == 0 {
		return 0, false, fmt.Errorf("%w: home zone %q", errPlacementMalformed, raw)
	}
	return uint32(zone), true, nil
}

// effectiveStorage 返回玩家的有效落点(§1):有记录用记录;没有记录用 home_zone;都没有用 fallbackZone
// (go/db 里是处理任务的那个 zone;本工具按各调用点的口径传)。
func effectiveStorage(rec placementRecord, recPresent bool, homeZone uint32, homePresent bool, fallbackZone uint32) uint32 {
	switch {
	case recPresent:
		return rec.StorageID
	case homePresent:
		return homeZone
	default:
		return fallbackZone
	}
}
