package main

// 清理源 zone 在 scene_manager Redis 里的热状态(-clear-source-hot-state)。
//
// 为什么需要这一步(2026-09-08):
//   scene_manager 的 EnterScene 有两道「不安全交接」门禁,判定依据是
//   player:{id}:location 里记录的属主节点。合服后源区永久下线,但这些
//   location 键不会自己消失:玩家上次退出时若走的是崩溃 / 强踢 / zone-down,
//   LeaveScene 根本没跑,键就永远指向一个已经不存在的 (zone, node)。
//   scene_manager 侧已经会把「整个 zone 没有活节点」的陈旧位置当作不存在
//   (playerLocationOwnerGone),但那是兜底;合服窗口里我们**知道**源区已经
//   下线,应当把这些垃圾一次性清掉,不要让每次登录都去做三次 Redis 判定,
//   也不要让 scene:{id}:* / world_channels / node_load 这些残骸误导
//   scene_manager 的孤儿清理与 rebalance。
//
// 安全前置(拒绝执行,不是警告):
//   scene_nodes:zone:{S}:load 必须为空。它是 C++ 场景节点的心跳负载集,
//   非空 = 源区还有节点在跑 = zone-down 没完成 = 节点可能还在写这些键。
//   此时删掉 location 会把「老节点仍在存盘」的双写窗口重新打开。
//
// 步骤(全部幂等、dry-run 只计数、SCAN + pipeline 分批,绝不 KEYS *):
//   1. player:{id}:location —— 值是 protobuf PlayerLocation;只删 zone_id==S
//      (zone_id==0 的旧数据按 scene:{scene_id}:zone 反查)。玩家清单优先用
//      合服第 0 步收集的 home_zone==S 列表;列表为空(重跑 / 已 remap)则
//      SCAN player:*:location 兜底。
//   2. scene:{id}:zone == S 的场景:DEL scene:{id}:{zone,node,mirror,source,mirrors}
//      + instance:{id}:player_count;再 DEL instances:zone:{S}:active。
//   3. world_channels:zone:{S}:* / world_channels:{draining,cooldown}:zone:{S}:*
//      / world_channels:desired:zone:{S}。
//   4. node:zone:{S}:* (scene_count / player_count / scene_node_type /
//      death_at / scenes 反向索引)+ scene_nodes:zone:{S}:load 本身。
//
// 键格式与 go/scene_manager/internal/logic 保持一致(load_reporter.go /
// createscenelogic.go / world_init.go / world_autoscale.go / reentry_barrier.go /
// changesceneutil.go)。merge_zone 是独立 go.mod,不引主工程包,所以这里
// 重复一份字面量;改动任何一方都要同步另一方。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

const (
	sceneNodeLoadKeyFmt       = "scene_nodes:zone:%d:load"
	playerLocationKeyFmt      = "player:%d:location"
	playerLocationScanPattern = "player:*:location"
	sceneZoneKeyScanPattern   = "scene:*:zone"
	activeInstancesKeyFmt     = "instances:zone:%d:active"
	worldChannelsZonePattern  = "world_channels:zone:%d:*"
	worldDrainingZonePattern  = "world_channels:draining:zone:%d:*"
	worldCooldownZonePattern  = "world_channels:cooldown:zone:%d:*"
	worldDesiredZoneKeyFmt    = "world_channels:desired:zone:%d"
	nodeZonePattern           = "node:zone:%d:*"
	hotStateScanCount         = 500
	hotStatePipelineBatch     = 500
	hotStateSceneKeysPerScene = 6 // 与 sceneHotStateKeys 返回的键数一致
)

// hotStateClearReport 是 -clear-source-hot-state 的计数汇总;dry-run 下
// Deleted 一律为 0,Would* / Found 仍是真实扫描结果。
type hotStateClearReport struct {
	LocationsChecked   int // 读到的 location 键数
	LocationsMatched   int // zone_id==S(含反查命中)的 location 数
	LocationsDeleted   int
	LocationsUndecided int // zone_id==0 且 scene:{id}:zone 已不存在:无法证明归属,保留
	ScenesMatched      int // scene:{id}:zone == S 的场景数
	SceneKeysDeleted   int
	WorldChannelKeys   int // 匹配到的 world_channels* 键数
	WorldChannelDel    int
	NodeKeys           int // node:zone:{S}:* + 负载集
	NodeKeysDeleted    int
	ActiveSetDeleted   int
}

func (r hotStateClearReport) String() string {
	return fmt.Sprintf("locations(checked=%d matched=%d deleted=%d undecided=%d) scenes(matched=%d keys_deleted=%d) world_channels(keys=%d deleted=%d) node_keys(found=%d deleted=%d) active_set_deleted=%d",
		r.LocationsChecked, r.LocationsMatched, r.LocationsDeleted, r.LocationsUndecided,
		r.ScenesMatched, r.SceneKeysDeleted, r.WorldChannelKeys, r.WorldChannelDel,
		r.NodeKeys, r.NodeKeysDeleted, r.ActiveSetDeleted)
}

// errSourceZoneStillLive 表示源区负载集非空,zone-down 未完成。
var errSourceZoneStillLive = errors.New("source zone still has live scene nodes")

// playerLocation 是 proto/scene_manager/storage.proto PlayerLocation 的最小镜像:
//
//	uint64 scene_id = 1; string node_id = 2; uint64 update_time = 3; uint32 zone_id = 4;
//
// 只解这四个字段;未知字段按 wire type 跳过,所以 proto 追加字段不会让本工具
// 误判(但**改号**会,改 storage.proto 字段号时必须同步这里)。
type playerLocation struct {
	SceneID    uint64
	NodeID     string
	UpdateTime uint64
	ZoneID     uint32
}

// decodePlayerLocation 手写 protobuf 解码。merge_zone 不引 proto 生成包
// (见 post_merge_stamp.go 顶部的取舍说明),而 PlayerLocation 只有 4 个
// 标量字段,手解比拖进整个 codegen 模块便宜得多。
func decodePlayerLocation(b []byte) (playerLocation, error) {
	var loc playerLocation
	i := 0
	for i < len(b) {
		tag, n, err := readUvarint(b[i:])
		if err != nil {
			return loc, fmt.Errorf("tag at %d: %w", i, err)
		}
		i += n
		fieldNum := tag >> 3
		wireType := tag & 7
		switch wireType {
		case 0: // varint
			v, n, err := readUvarint(b[i:])
			if err != nil {
				return loc, fmt.Errorf("varint field %d: %w", fieldNum, err)
			}
			i += n
			switch fieldNum {
			case 1:
				loc.SceneID = v
			case 3:
				loc.UpdateTime = v
			case 4:
				loc.ZoneID = uint32(v)
			}
		case 2: // length-delimited
			l, n, err := readUvarint(b[i:])
			if err != nil {
				return loc, fmt.Errorf("length field %d: %w", fieldNum, err)
			}
			i += n
			if uint64(len(b)-i) < l {
				return loc, fmt.Errorf("field %d length %d exceeds buffer", fieldNum, l)
			}
			if fieldNum == 2 {
				loc.NodeID = string(b[i : i+int(l)])
			}
			i += int(l)
		case 1: // 64-bit
			if len(b)-i < 8 {
				return loc, fmt.Errorf("field %d: truncated fixed64", fieldNum)
			}
			i += 8
		case 5: // 32-bit
			if len(b)-i < 4 {
				return loc, fmt.Errorf("field %d: truncated fixed32", fieldNum)
			}
			i += 4
		default:
			return loc, fmt.Errorf("field %d: unsupported wire type %d", fieldNum, wireType)
		}
	}
	return loc, nil
}

func readUvarint(b []byte) (uint64, int, error) {
	var x uint64
	var s uint
	for i, c := range b {
		if i >= 10 {
			return 0, 0, errors.New("varint overflow")
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1, nil
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0, errors.New("truncated varint")
}

// locationZoneDecision 是纯判定:这条 location 属不属于源区 S。
//   - zone_id==S                          → belongs
//   - zone_id!=0 且 !=S                   → not belongs
//   - zone_id==0:按 scene:{scene_id}:zone 反查(sceneZone 为 "" 表示查无) →
//     反查==S belongs;反查为其它 zone not belongs;查无 → undecided(保留,
//     没有证据证明它属于源区,删错就是把活玩家的位置抹掉)。
type locationDecision int

const (
	locationKeep locationDecision = iota
	locationDelete
	locationUndecided
)

func decideLocationZone(loc playerLocation, src uint32, sceneZone string) locationDecision {
	if loc.ZoneID != 0 {
		if loc.ZoneID == src {
			return locationDelete
		}
		return locationKeep
	}
	if loc.SceneID == 0 || sceneZone == "" {
		return locationUndecided
	}
	z, err := strconv.ParseUint(sceneZone, 10, 32)
	if err != nil {
		return locationUndecided
	}
	if uint32(z) == src {
		return locationDelete
	}
	return locationKeep
}

// sceneHotStateKeys 返回一个场景在 scene_manager Redis 里的全部固定键。
// 顺序无关;数量必须与 hotStateSceneKeysPerScene 一致。
func sceneHotStateKeys(sceneID uint64) []string {
	return []string{
		fmt.Sprintf("scene:%d:zone", sceneID),
		fmt.Sprintf("scene:%d:node", sceneID),
		fmt.Sprintf("scene:%d:mirror", sceneID),
		fmt.Sprintf("scene:%d:source", sceneID),
		fmt.Sprintf("scene:%d:mirrors", sceneID),
		fmt.Sprintf("instance:%d:player_count", sceneID),
	}
}

// parseSceneIDFromZoneKey 把 "scene:{id}:zone" 解析成 id;不匹配返回 (0,false)。
func parseSceneIDFromZoneKey(key string) (uint64, bool) {
	if !strings.HasPrefix(key, "scene:") || !strings.HasSuffix(key, ":zone") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(key, "scene:"), ":zone")
	id, err := strconv.ParseUint(mid, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// parsePlayerIDFromLocationKey 把 "player:{id}:location" 解析成 id。
func parsePlayerIDFromLocationKey(key string) (uint64, bool) {
	if !strings.HasPrefix(key, "player:") || !strings.HasSuffix(key, ":location") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(key, "player:"), ":location")
	id, err := strconv.ParseUint(mid, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// sourceZoneScanPatterns 列出按 zone 前缀整体清除的 SCAN 模式(步骤 3/4)。
func sourceZoneScanPatterns(src uint32) []string {
	return []string{
		fmt.Sprintf(worldChannelsZonePattern, src),
		fmt.Sprintf(worldDrainingZonePattern, src),
		fmt.Sprintf(worldCooldownZonePattern, src),
		fmt.Sprintf(nodeZonePattern, src),
	}
}

// sourceZoneFixedKeys 列出按 zone 直接 DEL 的固定键(步骤 2 尾 / 3 / 4)。
func sourceZoneFixedKeys(src uint32) []string {
	return []string{
		fmt.Sprintf(worldDesiredZoneKeyFmt, src),
		fmt.Sprintf(sceneNodeLoadKeyFmt, src),
	}
}

// ── Redis 执行层 ─────────────────────────────────────────────────

// assertSourceZoneDown 是前置门禁:负载集非空即拒绝。
func assertSourceZoneDown(ctx context.Context, rdb redis.UniversalClient, src uint32) error {
	n, err := rdb.ZCard(ctx, fmt.Sprintf(sceneNodeLoadKeyFmt, src)).Result()
	if err != nil {
		return fmt.Errorf("zcard %s: %w", fmt.Sprintf(sceneNodeLoadKeyFmt, src), err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %s has %d members — finish zone-down first", errSourceZoneStillLive, fmt.Sprintf(sceneNodeLoadKeyFmt, src), n)
	}
	return nil
}

// clearSourceZoneHotState 执行全部四步;任何 Redis 错误立即返回(已删的部分
// 不回滚 —— 每一步都是幂等的删除,重跑即可补齐)。
func clearSourceZoneHotState(ctx context.Context, rdb redis.UniversalClient, src uint32, playerIDs []uint64, dryRun bool) (hotStateClearReport, error) {
	var rep hotStateClearReport
	if err := assertSourceZoneDown(ctx, rdb, src); err != nil {
		return rep, err
	}

	// 1) player locations
	if len(playerIDs) > 0 {
		for start := 0; start < len(playerIDs); start += hotStatePipelineBatch {
			end := start + hotStatePipelineBatch
			if end > len(playerIDs) {
				end = len(playerIDs)
			}
			keys := make([]string, 0, end-start)
			for _, pid := range playerIDs[start:end] {
				keys = append(keys, fmt.Sprintf(playerLocationKeyFmt, pid))
			}
			if err := clearLocationBatch(ctx, rdb, src, keys, dryRun, &rep); err != nil {
				return rep, err
			}
		}
	} else {
		log.Printf("Hot-state: player list empty (already remapped?) — falling back to SCAN %s", playerLocationScanPattern)
		var cur uint64
		for {
			keys, next, err := rdb.Scan(ctx, cur, playerLocationScanPattern, hotStateScanCount).Result()
			if err != nil {
				return rep, fmt.Errorf("scan %s: %w", playerLocationScanPattern, err)
			}
			valid := keys[:0]
			for _, k := range keys {
				if _, ok := parsePlayerIDFromLocationKey(k); ok {
					valid = append(valid, k)
				}
			}
			if err := clearLocationBatch(ctx, rdb, src, valid, dryRun, &rep); err != nil {
				return rep, err
			}
			cur = next
			if cur == 0 {
				break
			}
		}
	}

	// 2) scenes registered to the source zone
	{
		var cur uint64
		for {
			keys, next, err := rdb.Scan(ctx, cur, sceneZoneKeyScanPattern, hotStateScanCount).Result()
			if err != nil {
				return rep, fmt.Errorf("scan %s: %w", sceneZoneKeyScanPattern, err)
			}
			if len(keys) > 0 {
				pipe := rdb.Pipeline()
				gets := make([]*redis.StringCmd, len(keys))
				for i, k := range keys {
					gets[i] = pipe.Get(ctx, k)
				}
				if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
					return rep, fmt.Errorf("pipeline get scene zones: %w", err)
				}
				var doomed []string
				for i, k := range keys {
					v, err := gets[i].Result()
					if err != nil {
						continue // 扫描与读取之间被删了,跳过
					}
					z, perr := strconv.ParseUint(v, 10, 32)
					if perr != nil || uint32(z) != src {
						continue
					}
					id, ok := parseSceneIDFromZoneKey(k)
					if !ok {
						continue
					}
					rep.ScenesMatched++
					doomed = append(doomed, sceneHotStateKeys(id)...)
				}
				n, err := deleteKeysBatched(ctx, rdb, doomed, dryRun)
				if err != nil {
					return rep, err
				}
				rep.SceneKeysDeleted += n
			}
			cur = next
			if cur == 0 {
				break
			}
		}
		activeKey := fmt.Sprintf(activeInstancesKeyFmt, src)
		n, err := deleteKeysBatched(ctx, rdb, []string{activeKey}, dryRun)
		if err != nil {
			return rep, err
		}
		rep.ActiveSetDeleted += n
	}

	// 3) + 4) zone-prefixed scan patterns
	for _, pat := range sourceZoneScanPatterns(src) {
		isNode := strings.HasPrefix(pat, "node:")
		var cur uint64
		for {
			keys, next, err := rdb.Scan(ctx, cur, pat, hotStateScanCount).Result()
			if err != nil {
				return rep, fmt.Errorf("scan %s: %w", pat, err)
			}
			n, err := deleteKeysBatched(ctx, rdb, keys, dryRun)
			if err != nil {
				return rep, err
			}
			if isNode {
				rep.NodeKeys += len(keys)
				rep.NodeKeysDeleted += n
			} else {
				rep.WorldChannelKeys += len(keys)
				rep.WorldChannelDel += n
			}
			cur = next
			if cur == 0 {
				break
			}
		}
	}
	for _, k := range sourceZoneFixedKeys(src) {
		exists, err := rdb.Exists(ctx, k).Result()
		if err != nil {
			return rep, fmt.Errorf("exists %s: %w", k, err)
		}
		if exists == 0 {
			continue
		}
		n, err := deleteKeysBatched(ctx, rdb, []string{k}, dryRun)
		if err != nil {
			return rep, err
		}
		if strings.HasPrefix(k, "scene_nodes:") {
			rep.NodeKeys++
			rep.NodeKeysDeleted += n
		} else {
			rep.WorldChannelKeys++
			rep.WorldChannelDel += n
		}
	}
	return rep, nil
}

// clearLocationBatch 读一批 location、解码、按归属删除。
func clearLocationBatch(ctx context.Context, rdb redis.UniversalClient, src uint32, keys []string, dryRun bool, rep *hotStateClearReport) error {
	if len(keys) == 0 {
		return nil
	}
	pipe := rdb.Pipeline()
	gets := make([]*redis.StringCmd, len(keys))
	for i, k := range keys {
		gets[i] = pipe.Get(ctx, k)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("pipeline get locations: %w", err)
	}

	type pending struct {
		key string
		loc playerLocation
	}
	var direct []string   // zone_id 已经能判定要删的
	var lookups []pending // zone_id==0 需要反查 scene zone 的
	for i, k := range keys {
		raw, err := gets[i].Result()
		if err != nil {
			continue // 不存在
		}
		rep.LocationsChecked++
		loc, derr := decodePlayerLocation([]byte(raw))
		if derr != nil {
			log.Printf("WARN: %s: undecodable PlayerLocation (%v) — kept", k, derr)
			rep.LocationsUndecided++
			continue
		}
		switch decideLocationZone(loc, src, "") {
		case locationDelete:
			direct = append(direct, k)
		case locationKeep:
		case locationUndecided:
			if loc.SceneID != 0 {
				lookups = append(lookups, pending{key: k, loc: loc})
			} else {
				rep.LocationsUndecided++
			}
		}
	}
	if len(lookups) > 0 {
		pipe := rdb.Pipeline()
		zones := make([]*redis.StringCmd, len(lookups))
		for i, p := range lookups {
			zones[i] = pipe.Get(ctx, fmt.Sprintf("scene:%d:zone", p.loc.SceneID))
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("pipeline get scene zones for legacy locations: %w", err)
		}
		for i, p := range lookups {
			sz, _ := zones[i].Result() // Nil → ""
			switch decideLocationZone(p.loc, src, sz) {
			case locationDelete:
				direct = append(direct, p.key)
			case locationUndecided:
				rep.LocationsUndecided++
			}
		}
	}
	rep.LocationsMatched += len(direct)
	n, err := deleteKeysBatched(ctx, rdb, direct, dryRun)
	if err != nil {
		return err
	}
	rep.LocationsDeleted += n
	return nil
}

// deleteKeysBatched 按 pipeline 批量 DEL;dry-run 返回 0(不写)。
// 返回值是实际 DEL 命令报告的删除数(键已不存在时不计,天然幂等)。
func deleteKeysBatched(ctx context.Context, rdb redis.UniversalClient, keys []string, dryRun bool) (int, error) {
	if len(keys) == 0 || dryRun {
		return 0, nil
	}
	deleted := 0
	for start := 0; start < len(keys); start += hotStatePipelineBatch {
		end := start + hotStatePipelineBatch
		if end > len(keys) {
			end = len(keys)
		}
		pipe := rdb.Pipeline()
		cmds := make([]*redis.IntCmd, 0, end-start)
		// Cluster 下不同 slot 的键不能放进同一条 DEL;逐键 DEL 走 pipeline
		// 既能跨 slot 又保留批量往返的收益。
		for _, k := range keys[start:end] {
			cmds = append(cmds, pipe.Del(ctx, k))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return deleted, fmt.Errorf("pipeline del: %w", err)
		}
		for _, c := range cmds {
			deleted += int(c.Val())
		}
	}
	return deleted, nil
}
