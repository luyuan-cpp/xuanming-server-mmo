package logic

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"data_service/internal/constants"
	"data_service/internal/metrics"
	"data_service/internal/routing"
	"data_service/internal/svc"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	playerDataPrefix = "player:{%d}:" // hash-tag ensures same slot
	playerVersionKey = "player:{%d}:__version"
)

func playerField(playerID uint64, field string) string {
	return fmt.Sprintf("player:{%d}:%s", playerID, field)
}

func versionKey(playerID uint64) string {
	return fmt.Sprintf(playerVersionKey, playerID)
}

func isInternalPlayerField(field string) bool {
	return field == "__version" || field == "__lock"
}

// ── LoadPlayerData ─────────────────────────────────────────────

type LoadPlayerDataReq struct {
	PlayerID uint64
	Fields   []string
}

type LoadPlayerDataResp struct {
	ErrorCode uint32
	Data      map[string][]byte
	Version   uint64
}

func LoadPlayerData(ctx context.Context, svcCtx *svc.ServiceContext, req *LoadPlayerDataReq) (*LoadPlayerDataResp, error) {
	client, err := svcCtx.Router.ClientForPlayer(ctx, req.PlayerID)
	if err != nil {
		return &LoadPlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
	}

	result := make(map[string][]byte)

	if len(req.Fields) == 0 {
		// Load all fields via SCAN with player prefix
		pattern := fmt.Sprintf("player:{%d}:*", req.PlayerID)
		var keys []string
		var cursor uint64
		for {
			var batch []string
			var err error
			batch, cursor, err = scan(ctx, client, cursor, pattern, 100)
			if err != nil {
				return &LoadPlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
			}
			keys = append(keys, batch...)
			if cursor == 0 {
				break
			}
		}

		if len(keys) > 0 {
			vals, err := mget(ctx, client, keys...)
			if err != nil {
				return &LoadPlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
			}
			prefix := fmt.Sprintf("player:{%d}:", req.PlayerID)
			for i, key := range keys {
				if vals[i] == nil {
					continue
				}
				fieldName := key[len(prefix):]
				if isInternalPlayerField(fieldName) {
					continue
				}
				result[fieldName] = []byte(vals[i].(string))
			}
		}
	} else {
		for _, field := range req.Fields {
			if isInternalPlayerField(field) {
				return &LoadPlayerDataResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
			}
		}
		keys := make([]string, len(req.Fields))
		for i, f := range req.Fields {
			keys[i] = playerField(req.PlayerID, f)
		}
		vals, err := mget(ctx, client, keys...)
		if err != nil {
			return &LoadPlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
		}
		for i, f := range req.Fields {
			if vals[i] == nil {
				continue
			}
			result[f] = []byte(vals[i].(string))
		}
	}

	// Read version. 读不到版本必须 fail-closed:回一个 Version=0 的"成功"会让
	// 调用方以为可以跳过版本校验(见 readVersion 注释),等于关掉乐观锁。
	ver, err := readVersion(ctx, client, req.PlayerID)
	if err != nil {
		return &LoadPlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
	}

	return &LoadPlayerDataResp{Data: result, Version: ver}, nil
}

// ── SavePlayerData ─────────────────────────────────────────────

type SavePlayerDataReq struct {
	PlayerID        uint64
	Data            map[string][]byte
	ExpectedVersion uint64
}

type SavePlayerDataResp struct {
	ErrorCode  uint32
	NewVersion uint64
}

// acquirePlayerLock acquires a per-player lock, returning the Redis client for
// that player plus the lock ownership token. On failure it returns a non-nil
// error code. Caller must defer svcCtx.Router.ReleasePlayerLock(ctx, client, id, token).
func acquirePlayerLock(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (goredis.Cmdable, string, uint32, error) {
	client, err := svcCtx.Router.ClientForPlayer(ctx, playerID)
	if err != nil {
		metrics.ObservePlayerLock("error")
		return nil, "", constants.ErrCodeRedis, err
	}
	token, locked, err := svcCtx.Router.AcquirePlayerLock(ctx, client, playerID)
	if err != nil {
		metrics.ObservePlayerLock("error")
		return nil, "", constants.ErrCodeRedis, err
	}
	if !locked {
		metrics.ObservePlayerLock("conflict")
		logx.Errorf("player %d data is being written by another process", playerID)
		return nil, "", constants.ErrCodeLockConflict, nil
	}
	metrics.ObservePlayerLock("acquired")
	return client, token, 0, nil
}

// saveFieldsScript 原子完成「版本比较 → 字段写入 → 版本自增」。
//
// 拆开做(先 GET 比版本、再 Pipeline 写)会留一条竞态:两个写者在对方写入前
// 都读到相同版本、双双通过检查,随后交错写入 —— 乐观锁失效,玩家数据出现
// 字段级撕裂。玩家锁挡不住这条路:它带 TTL,慢写者超时后锁会被下一个写者
// 合法拿走。所有 player:{id}:* key 共享 {id} hash-tag(见 playerDataPrefix),
// 同一 slot,Cluster 下单脚本可覆盖。
//
// KEYS[1] = data lock key,KEYS[2] = version key,KEYS[3..n] = field keys;
// ARGV[1] = lock token,ARGV[2] = expected_version(0 = 跳过检查),
// ARGV[3..n] = 字段值,与 KEYS 同序对应。
// 返回 {1, new_version}、{0, current_version}(版本冲突)或 {-1, current_version}(锁已失效)。
const saveFieldsScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return {-1, tonumber(redis.call('GET', KEYS[2]) or '0')}
end
local expected = tonumber(ARGV[2])
if expected ~= 0 then
	local cur = tonumber(redis.call('GET', KEYS[2]) or '0')
	if cur ~= expected then
		return {0, cur}
	end
end
for i = 3, #KEYS do
	redis.call('SET', KEYS[i], ARGV[i])
end
return {1, redis.call('INCR', KEYS[2])}`

type saveFieldsStatus int64

const (
	saveFieldsLockLost        saveFieldsStatus = -1
	saveFieldsVersionMismatch saveFieldsStatus = 0
	saveFieldsOK              saveFieldsStatus = 1
)

// saveFieldsAtomic runs saveFieldsScript. status 精确区分版本冲突与锁失效；
// 后者意味着当前调用已经是 TTL 后恢复的旧持锁者，绝不能继续写。
func saveFieldsAtomic(ctx context.Context, client goredis.Cmdable, playerID uint64, token string, expectedVersion uint64, data map[string][]byte) (saveFieldsStatus, uint64, error) {
	keys := make([]string, 0, len(data)+2)
	argv := make([]interface{}, 0, len(data)+2)
	keys = append(keys, routing.PlayerDataLockKey(playerID), versionKey(playerID))
	argv = append(argv, token, expectedVersion)
	for field, val := range data {
		keys = append(keys, playerField(playerID, field))
		argv = append(argv, val)
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := client.Eval(ctx, saveFieldsScript, keys, argv...).Result()
	if err != nil {
		return saveFieldsLockLost, 0, err
	}
	arr, ok := res.([]interface{})
	if !ok || len(arr) != 2 {
		return saveFieldsLockLost, 0, fmt.Errorf("unexpected saveFieldsScript reply: %T %v", res, res)
	}
	status, statusOK := arr[0].(int64)
	ver, versionOK := arr[1].(int64)
	if !statusOK || !versionOK || ver < 0 {
		return saveFieldsLockLost, 0, fmt.Errorf("unexpected saveFieldsScript values: %T/%v %T/%v",
			arr[0], arr[0], arr[1], arr[1])
	}
	if status != int64(saveFieldsLockLost) && status != int64(saveFieldsVersionMismatch) && status != int64(saveFieldsOK) {
		return saveFieldsLockLost, 0, fmt.Errorf("unexpected saveFieldsScript status: %d", status)
	}
	return saveFieldsStatus(status), uint64(ver), nil
}

func SavePlayerData(ctx context.Context, svcCtx *svc.ServiceContext, req *SavePlayerDataReq) (*SavePlayerDataResp, error) {
	startTime := time.Now()
	for field := range req.Data {
		if isInternalPlayerField(field) {
			metrics.ObserveSavePlayerData("invalid_request", startTime)
			return &SavePlayerDataResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
		}
	}
	client, token, errCode, err := acquirePlayerLock(ctx, svcCtx, req.PlayerID)
	if errCode != 0 {
		outcome := "redis_error"
		if errCode == constants.ErrCodeLockConflict {
			outcome = "lock_conflict"
		}
		metrics.ObserveSavePlayerData(outcome, startTime)
		return &SavePlayerDataResp{ErrorCode: errCode}, err
	}
	defer svcCtx.Router.ReleasePlayerLock(ctx, client, req.PlayerID, token)

	status, ver, err := saveFieldsAtomic(ctx, client, req.PlayerID, token, req.ExpectedVersion, req.Data)
	if err != nil {
		metrics.ObserveSavePlayerData("redis_error", startTime)
		return &SavePlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	if status == saveFieldsLockLost {
		metrics.ObserveSavePlayerData("lock_conflict", startTime)
		logx.Errorf("player %d lock ownership expired before atomic save; stale writer fenced", req.PlayerID)
		return &SavePlayerDataResp{ErrorCode: constants.ErrCodeLockConflict, NewVersion: ver}, nil
	}
	if status == saveFieldsVersionMismatch {
		logx.Errorf("version mismatch for player %d: expected %d, got %d", req.PlayerID, req.ExpectedVersion, ver)
		metrics.ObserveVersionMismatch("save_player_data")
		metrics.ObserveSavePlayerData("version_mismatch", startTime)
		return &SavePlayerDataResp{ErrorCode: constants.ErrCodeVersionMismatch, NewVersion: ver}, nil
	}

	metrics.ObserveSavePlayerData("ok", startTime)
	return &SavePlayerDataResp{NewVersion: ver}, nil
}

// ── GetPlayerField ─────────────────────────────────────────────

func GetPlayerField(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, field string) ([]byte, error) {
	if isInternalPlayerField(field) {
		return nil, fmt.Errorf("field %q is internal", field)
	}
	client, err := svcCtx.Router.ClientForPlayer(ctx, playerID)
	if err != nil {
		return nil, err
	}
	val, err := getString(ctx, client, playerField(playerID, field))
	if err == goredis.Nil {
		return nil, nil
	}
	return []byte(val), err
}

// ── SetPlayerField ─────────────────────────────────────────────

type SetPlayerFieldResp struct {
	ErrorCode  uint32
	NewVersion uint64
}

func SetPlayerField(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, field string, value []byte, expectedVersion uint64) (*SetPlayerFieldResp, error) {
	if isInternalPlayerField(field) {
		return &SetPlayerFieldResp{ErrorCode: constants.ErrCodeInvalidRequest}, nil
	}
	client, token, errCode, err := acquirePlayerLock(ctx, svcCtx, playerID)
	if errCode != 0 {
		return &SetPlayerFieldResp{ErrorCode: errCode}, err
	}
	defer svcCtx.Router.ReleasePlayerLock(ctx, client, playerID, token)

	status, ver, err := saveFieldsAtomic(ctx, client, playerID, token, expectedVersion, map[string][]byte{field: value})
	if err != nil {
		return &SetPlayerFieldResp{ErrorCode: constants.ErrCodeRedis}, err
	}
	if status == saveFieldsLockLost {
		return &SetPlayerFieldResp{ErrorCode: constants.ErrCodeLockConflict, NewVersion: ver}, nil
	}
	if status == saveFieldsVersionMismatch {
		return &SetPlayerFieldResp{ErrorCode: constants.ErrCodeVersionMismatch, NewVersion: ver}, nil
	}

	return &SetPlayerFieldResp{NewVersion: ver}, nil
}

// ── Redis helper interfaces ────────────────────────────────────
// go-redis Cmdable doesn't expose Scan/Pipeline; define minimal interfaces
// so callers don't need type switches.

type redisScanner interface {
	Scan(ctx context.Context, cursor uint64, match string, count int64) *goredis.ScanCmd
}

// ── Helpers ────────────────────────────────────────────────────

// readVersion 读乐观锁版本号。
//
// 必须把「版本 key 不存在」和「读失败」分开返回:saveFieldsScript 里
// expected_version==0 的语义是**跳过版本校验**(见该脚本 ARGV[2] 注释)。
// 旧写法把 Redis 读错误(超时、连接抖动、类型异常)一律降级成 0 并且不带错误,
// LoadPlayerData 又把它当成正常结果返回(同函数里其它读失败都会置 ErrCodeRedis),
// 于是调用方拿着 version=0 去存盘 —— 乐观锁被静默关掉,变成无条件覆盖。
// 两个 scene 节点并发存同一个玩家时,后到的那次会直接盖掉前一次,表现为随机回档。
// key 不存在(首次存盘)仍然合法返回 0。
func readVersion(ctx context.Context, client goredis.Cmdable, playerID uint64) (uint64, error) {
	val, err := getString(ctx, client, versionKey(playerID))
	if err != nil {
		if err == goredis.Nil {
			return 0, nil
		}
		return 0, err
	}
	if val == "" {
		return 0, nil
	}
	v, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("corrupt version value %q for player %d: %w", val, playerID, err)
	}
	return v, nil
}

func getString(ctx context.Context, c goredis.Cmdable, key string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.Get(ctx, key).Result()
}

func mget(ctx context.Context, c goredis.Cmdable, keys ...string) ([]interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.MGet(ctx, keys...).Result()
}

func scan(ctx context.Context, c goredis.Cmdable, cursor uint64, pattern string, count int64) ([]string, uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	s, ok := c.(redisScanner)
	if !ok {
		return nil, 0, fmt.Errorf("redis client %T does not support Scan", c)
	}
	return s.Scan(ctx, cursor, pattern, count).Result()
}

// ── DeletePlayerData ───────────────────────────────────────────

type DeletePlayerDataReq struct {
	PlayerID          uint64
	DeleteZoneMapping bool
}

type DeletePlayerDataResp struct {
	ErrorCode   uint32
	KeysDeleted uint32
}

func DeletePlayerData(ctx context.Context, svcCtx *svc.ServiceContext, req *DeletePlayerDataReq) (*DeletePlayerDataResp, error) {
	client, token, errCode, err := acquirePlayerLock(ctx, svcCtx, req.PlayerID)
	if errCode != 0 {
		return &DeletePlayerDataResp{ErrorCode: errCode}, err
	}
	defer svcCtx.Router.ReleasePlayerLock(ctx, client, req.PlayerID, token)

	// Scan all keys for this player
	pattern := fmt.Sprintf("player:{%d}:*", req.PlayerID)
	var allKeys []string
	var cursor uint64
	for {
		batch, next, err := scan(ctx, client, cursor, pattern, 100)
		if err != nil {
			return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
		}
		allKeys = append(allKeys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
	}

	// 锁键本身由 defer 的 token 校验释放，不能混入业务数据删除列表。
	lockKey := routing.PlayerDataLockKey(req.PlayerID)
	dataKeys := allKeys[:0]
	for _, key := range allKeys {
		if key != lockKey {
			dataKeys = append(dataKeys, key)
		}
	}
	allKeys = dataKeys

	if len(allKeys) > 0 {
		owned, deleteErr := deletePlayerKeysAtomic(ctx, client, lockKey, token, allKeys)
		if deleteErr != nil {
			err = deleteErr
			logx.Errorf("[DeletePlayerData] player %d: failed to delete %d Redis keys: %v", req.PlayerID, len(allKeys), err)
			return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeRedis}, err
		}
		if !owned {
			logx.Errorf("[DeletePlayerData] player %d: lock expired before delete; stale deleter fenced", req.PlayerID)
			return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeLockConflict}, nil
		}
	}

	deleted := uint32(len(allKeys))

	// Optionally remove zone mapping
	if req.DeleteZoneMapping {
		owned, deletedMapping, err := svcCtx.Router.DeletePlayerZoneIfLocked(ctx, req.PlayerID, token)
		if err != nil {
			logx.Errorf("[DeletePlayerData] player %d: failed to delete zone mapping: %v", req.PlayerID, err)
			return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeRedis, KeysDeleted: deleted}, err
		}
		if !owned {
			logx.Errorf("[DeletePlayerData] player %d: mapping lock expired before delete; stale deleter fenced", req.PlayerID)
			return &DeletePlayerDataResp{ErrorCode: constants.ErrCodeLockConflict, KeysDeleted: deleted}, nil
		}
		if deletedMapping {
			deleted++
		}
	}

	logx.Infof("[DeletePlayerData] player %d: deleted=%d zone=%v", req.PlayerID, deleted, req.DeleteZoneMapping)
	return &DeletePlayerDataResp{KeysDeleted: deleted}, nil
}

const deletePlayerKeysScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return -1
end
if #KEYS == 1 then
	return 0
end
return redis.call('DEL', unpack(KEYS, 2, #KEYS))`

func deletePlayerKeysAtomic(ctx context.Context, c goredis.Cmdable, lockKey, token string, keys []string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	allKeys := make([]string, 0, len(keys)+1)
	allKeys = append(allKeys, lockKey)
	allKeys = append(allKeys, keys...)
	result, err := c.Eval(ctx, deletePlayerKeysScript, allKeys, token).Int64()
	if err != nil {
		return false, err
	}
	return result >= 0, nil
}
