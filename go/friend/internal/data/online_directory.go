package data

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	pb "proto/friend"
	plpb "proto/player_locator"
)

// ErrInvalidDirectoryInput 只表示客户端参数不合法，其他错误均为依赖故障。
var ErrInvalidDirectoryInput = errors.New("在线目录参数不合法")

const (
	directoryScanCount  = 64
	directoryScanRounds = 4
	directoryMaxBatch   = 1024
	directoryMaxPage    = 50
)

// OnlineDirectory 只读 player_locator 的会话与玩家展示缓存；不维护第二份在线真值。
// 共享 Redis 是 node 模式（与 SessionReader 相同约束）。home zone 经 data_service
// 查询，不能假定 player:zone:* 和会话同库。每页最多四次 SCAN，空页仍可有续页。
type OnlineDirectory struct {
	Redis       *redis.Redis
	DataService dspb.DataServiceClient
}

// DirectoryCursor 的偏移保留 SCAN 最后一个批次的未消费部分，COUNT 只是提示，
// 不能简单截取 limit 后丢弃该批剩余玩家。页内按 key 排序以便继续；上下线期间
// SCAN 允许重复，客户端按 uint64 ID 去重，刷新从空游标开始。不承诺跨页快照。
type directoryCursor struct {
	scan   uint64
	offset int
}

func parseDirectoryCursor(raw string) (directoryCursor, error) {
	if raw == "" {
		return directoryCursor{}, nil
	}
	if len(raw) > 48 {
		return directoryCursor{}, ErrInvalidDirectoryInput
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return directoryCursor{}, ErrInvalidDirectoryInput
	}
	scan, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return directoryCursor{}, ErrInvalidDirectoryInput
	}
	offset, err := strconv.ParseUint(parts[2], 10, 16)
	if err != nil || offset > directoryMaxBatch {
		return directoryCursor{}, ErrInvalidDirectoryInput
	}
	return directoryCursor{scan: scan, offset: int(offset)}, nil
}

func encodeDirectoryCursor(scan uint64, offset int) string {
	if scan == 0 && offset == 0 {
		return ""
	}
	return fmt.Sprintf("v1:%d:%d", scan, offset)
}

// ValidateDirectoryInput 在限流计数、RPC 和 Redis 读取前执行。
func ValidateDirectoryInput(cursor, query string) error {
	if _, err := parseDirectoryCursor(cursor); err != nil {
		return err
	}
	if !utf8.ValidString(query) || utf8.RuneCountInString(query) > 64 {
		return ErrInvalidDirectoryInput
	}
	return nil
}

func (d OnlineDirectory) List(ctx context.Context, caller uint64, cursor string, limit uint32, exclude []uint64, query string) (*pb.RecommendFriendsResponse, error) {
	if err := ValidateDirectoryInput(cursor, query); err != nil {
		return nil, err
	}
	if d.Redis == nil || d.DataService == nil {
		return nil, errors.New("在线目录依赖未配置")
	}
	pos, _ := parseDirectoryCursor(cursor)
	if limit == 0 {
		limit = 12
	}
	if limit > directoryMaxPage {
		limit = directoryMaxPage
	}
	zones, err := d.DataService.BatchGetPlayerHomeZone(ctx, &dspb.BatchGetPlayerHomeZoneRequest{PlayerIds: []uint64{caller}})
	if err != nil {
		return nil, err
	}
	zone := zones.GetPlayerZoneMap()[caller]
	if zone == 0 {
		return nil, errors.New("在线目录调用者归属区未知")
	}
	skip := map[uint64]bool{caller: true}
	for _, id := range exclude {
		skip[id] = true
	}
	query = strings.ToLower(strings.TrimSpace(query))
	result := &pb.RecommendFriendsResponse{OnlineDirectory: true}
	for round := 0; round < directoryScanRounds; round++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		keys, next, err := d.Redis.ScanCtx(ctx, pos.scan, playerSessionKeyPrefix+"*", directoryScanCount)
		if err != nil {
			return nil, err
		}
		if len(keys) > directoryMaxBatch {
			return nil, errors.New("在线目录会话扫描批次超出安全上限")
		}
		sort.Strings(keys)
		start := pos.offset
		if start > len(keys) {
			start = len(keys)
		} // 上下线缩短当前批次，继续下一批。
		entries, err := d.readEntries(ctx, keys[start:], skip, query, zone)
		if err != nil {
			return nil, err
		}
		for i := start; i < len(keys); i++ {
			entry := entries[keys[i]]
			if entry == nil || skip[entry.GetCandidatePlayerId()] {
				continue
			}
			skip[entry.GetCandidatePlayerId()] = true
			result.Candidates = append(result.Candidates, entry)
			if uint32(len(result.Candidates)) == limit {
				if i+1 < len(keys) {
					result.NextCursor = encodeDirectoryCursor(pos.scan, i+1)
				} else {
					result.NextCursor = encodeDirectoryCursor(next, 0)
				}
				return result, nil
			}
		}
		pos = directoryCursor{scan: next}
		if next == 0 {
			return result, nil
		}
	}
	result.NextCursor = encodeDirectoryCursor(pos.scan, 0)
	return result, nil
}

func (d OnlineDirectory) readEntries(ctx context.Context, keys []string, skip map[uint64]bool, query string, zone uint32) (map[string]*pb.RecommendEntry, error) {
	entries := make(map[string]*pb.RecommendEntry)
	if len(keys) == 0 {
		return entries, nil
	}
	ids := make([]uint64, len(keys))
	readKeys := make([]string, 0, len(keys)*2)
	prefix := string((&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName())
	for i, key := range keys {
		id, err := strconv.ParseUint(strings.TrimPrefix(key, playerSessionKeyPrefix), 10, 64)
		if err != nil || id == 0 || skip[id] {
			continue
		}
		ids[i] = id
		readKeys = append(readKeys, key, prefix+":"+strconv.FormatUint(id, 10))
	}
	if len(readKeys) == 0 {
		return entries, nil
	}
	values, err := d.Redis.MgetCtx(ctx, readKeys...)
	if err != nil {
		return nil, err
	}
	if len(values) != len(readKeys) {
		return nil, errors.New("在线目录会话读取数量不符")
	}
	candidates := make([]uint64, 0, len(keys))
	valueIndex := 0
	for i, id := range ids {
		if id == 0 {
			continue
		}
		sessionRaw, profileRaw := values[valueIndex], values[valueIndex+1]
		valueIndex += 2
		session := &plpb.PlayerSession{}
		profile := &dbpb.PlayerAllData{}
		if sessionRaw == "" || profileRaw == "" || proto.Unmarshal([]byte(sessionRaw), session) != nil || proto.Unmarshal([]byte(profileRaw), profile) != nil {
			continue
		}
		player := profile.GetPlayerDatabaseData()
		if session.GetPlayerId() != id || session.GetState() != plpb.PlayerSessionState_SESSION_STATE_ONLINE || player.GetPlayerId() != id {
			continue
		}
		name := strings.TrimSpace(player.GetProfileComponent().GetName())
		if name == "" || player.GetLevelComponent().GetLevel() == 0 || player.GetUint32PbComponent().GetClass() == 0 {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(name), query) && !strings.Contains(strconv.FormatUint(id, 10), query) {
			continue
		}
		entries[keys[i]] = &pb.RecommendEntry{CandidatePlayerId: id, IsOnline: true, LastActiveMs: session.GetLastActiveTs(), Name: name, Level: player.GetLevelComponent().GetLevel(), ClassId: player.GetUint32PbComponent().GetClass(), Gender: player.GetProfileComponent().GetGender(), AppearanceId: player.GetProfileComponent().GetAppearanceId(), ZoneId: zone}
		candidates = append(candidates, id)
	}
	if len(candidates) == 0 {
		return entries, nil
	}
	zones, err := d.DataService.BatchGetPlayerHomeZone(ctx, &dspb.BatchGetPlayerHomeZoneRequest{PlayerIds: candidates})
	if err != nil {
		return nil, err
	}
	for key, entry := range entries {
		if zones.GetPlayerZoneMap()[entry.GetCandidatePlayerId()] != zone {
			delete(entries, key)
		}
	}
	return entries, nil
}
