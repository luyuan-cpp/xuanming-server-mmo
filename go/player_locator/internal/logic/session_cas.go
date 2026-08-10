package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"player_locator/internal/svc"
)

// 所有会话生命周期变更都比较完整的 protobuf 字节。protobuf 中同时包含
// session_id 与 session_version，因此这相当于一个不可拆分的双字段 CAS，且不会
// 要求 Redis Lua 理解 protobuf wire format。
var ErrSessionCleanupPending = errors.New("previous session cleanup is still pending")

type replaceSessionResult int

const (
	replaceSessionConflict       replaceSessionResult = 0
	replaceSessionApplied        replaceSessionResult = 1
	replaceSessionCleanupPending replaceSessionResult = 2
)

var replaceSessionAndCancelLeaseScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if ARGV[1] == "missing" then
    if current then return 0 end

    -- commitLeaseExpiry 已删 session、但外部 Gate/Scene 副作用尚未全部成功时，
    -- processing claim 是唯一的清理所有权。此时建立新 session 会撤销旧 claim，
    -- 造成旧场景永久残留；若旧调用迟到，还可能误清新 location。fail-closed
    -- 拒绝首次写入，直到旧 worker 成功 ack。
    if redis.call("ZSCORE", KEYS[3], ARGV[5])
        or redis.call("HGET", KEYS[4], ARGV[5])
        or redis.call("HGET", KEYS[5], ARGV[5]) then
        return 2
    end
else
    if (not current) or current ~= ARGV[2] then return 0 end
end

if tonumber(ARGV[4]) > 0 then
    redis.call("SET", KEYS[1], ARGV[3], "PX", ARGV[4])
else
    redis.call("SET", KEYS[1], ARGV[3])
end
redis.call("ZREM", KEYS[2], ARGV[5])
redis.call("ZREM", KEYS[3], ARGV[5])
redis.call("HDEL", KEYS[4], ARGV[5])
redis.call("HDEL", KEYS[5], ARGV[5])
return 1
`)

var setDisconnectingScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if (not current) or current ~= ARGV[1] then return 0 end

if tonumber(ARGV[3]) > 0 then
    redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3])
else
    redis.call("SET", KEYS[1], ARGV[2])
end
redis.call("ZADD", KEYS[2], ARGV[4], ARGV[5])
redis.call("ZREM", KEYS[3], ARGV[5])
redis.call("HDEL", KEYS[4], ARGV[5])
redis.call("HDEL", KEYS[5], ARGV[5])
return 1
`)

var deleteSessionIfUnchangedScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if ARGV[1] == "missing" then
    if current then return 0 end
else
    if (not current) or current ~= ARGV[2] then return 0 end
end

redis.call("DEL", KEYS[1], KEYS[2])
redis.call("ZREM", KEYS[3], ARGV[3])
redis.call("ZREM", KEYS[4], ARGV[3])
redis.call("HDEL", KEYS[5], ARGV[3])
redis.call("HDEL", KEYS[6], ARGV[3])
return 1
`)

// claimExpiredLeasesScript 实现 ready -> processing 的可靠 claim：
// 1. processing 超时任务先回 ready；2. claim 时复制会话载荷；3. 只有 ack 才删除
// processing 记录。进程在任意一步退出，任务都会在 claim deadline 后重新出现。
var claimExpiredLeasesScript = redis.NewScript(`
local recovered = redis.call("ZRANGEBYSCORE", KEYS[2], "-inf", ARGV[1], "LIMIT", 0, ARGV[3])
for _, player_id in ipairs(recovered) do
    redis.call("ZREM", KEYS[2], player_id)
    redis.call("ZADD", KEYS[1], ARGV[1], player_id)
    redis.call("HDEL", KEYS[3], player_id)
end

local expired = redis.call("ZRANGEBYSCORE", KEYS[1], "-inf", ARGV[1], "LIMIT", 0, ARGV[3])
local result = {}
for _, player_id in ipairs(expired) do
    redis.call("ZREM", KEYS[1], player_id)
    redis.call("ZADD", KEYS[2], ARGV[2], player_id)

    local token = ARGV[4] .. ":" .. player_id
    redis.call("HSET", KEYS[3], player_id, token)

    local payload = redis.call("GET", ARGV[5] .. player_id)
    if payload then
        redis.call("HSET", KEYS[4], player_id, payload)
    else
        payload = redis.call("HGET", KEYS[4], player_id)
        if not payload then payload = "" end
    end

    table.insert(result, player_id)
    table.insert(result, token)
    table.insert(result, payload)
end
return result
`)

// commitLeaseExpiryScript 只删除 claim 时看到的那一版 session。Reconnect/SetSession
// 若已经写入新版本，脚本会把旧 claim 当 stale 丢弃，绝不会删在线新会话。
// 返回值：0=claim 已失效；1=本 claim 已获得清理权；2=session 已换代。
var commitLeaseExpiryScript = redis.NewScript(`
if redis.call("HGET", KEYS[5], ARGV[1]) ~= ARGV[2] then return 0 end

local current = redis.call("GET", KEYS[1])
if current and current ~= ARGV[3] then
    redis.call("ZREM", KEYS[4], ARGV[1])
    redis.call("HDEL", KEYS[5], ARGV[1])
    redis.call("HDEL", KEYS[6], ARGV[1])
    return 2
end

redis.call("DEL", KEYS[1], KEYS[2])
return 1
`)

var ackLeaseClaimScript = redis.NewScript(`
if redis.call("HGET", KEYS[2], ARGV[1]) ~= ARGV[2] then return 0 end
redis.call("ZREM", KEYS[1], ARGV[1])
redis.call("HDEL", KEYS[2], ARGV[1])
redis.call("HDEL", KEYS[3], ARGV[1])
return 1
`)

// renewLeaseClaimsScript 只给仍属于本 worker token 的 processing claim 续 deadline。
// claim 已被 ack、Reconnect 撤销或超时后由别的 worker 换 token 时不会复活旧任务。
var renewLeaseClaimsScript = redis.NewScript(`
local renewed = 0
for i = 2, #ARGV, 2 do
    local player_id = ARGV[i]
    local token = ARGV[i + 1]
    if redis.call("HGET", KEYS[2], player_id) == token then
        redis.call("ZADD", KEYS[1], ARGV[1], player_id)
        renewed = renewed + 1
    end
end
return renewed
`)

// rearmLeaseClaimScript 先确认 claim token 与完整 session 都没变，再原子续 ready
// deadline 和 session TTL。ONLINE 会话或换代会话不会被 AFK 分支重新加 TTL。
// 返回值：0=claim 已失效；1=已续租；2=session 已换代。
var rearmLeaseClaimScript = redis.NewScript(`
if redis.call("HGET", KEYS[4], ARGV[1]) ~= ARGV[2] then return 0 end

local current = redis.call("GET", KEYS[1])
if (not current) or current ~= ARGV[3] then
    redis.call("ZREM", KEYS[3], ARGV[1])
    redis.call("HDEL", KEYS[4], ARGV[1])
    redis.call("HDEL", KEYS[5], ARGV[1])
    return 2
end

redis.call("ZADD", KEYS[2], ARGV[4], ARGV[1])
redis.call("PERSIST", KEYS[1])
redis.call("ZREM", KEYS[3], ARGV[1])
redis.call("HDEL", KEYS[4], ARGV[1])
redis.call("HDEL", KEYS[5], ARGV[1])
return 1
`)

type leaseClaim struct {
	playerID uint64
	token    string
	payload  []byte
}

// enqueueOfflineCleanupScript 给"会话已 CAS 删除、但 SceneManager.LeaveScene
// 同步调用失败"的正常登出路径提供持久重试通道:把(改成 DISCONNECTING 态的)
// 会话快照写进 claim payload 哈希并加入 ready ZSET,复用 LeaseMonitor 既有的
// at-least-once 机制驱动重试。
//
// 守卫:会话键已经重新出现(玩家瞬间重登)时放弃入队 —— claim 脚本会优先读
// 活会话作为 payload,旧清理会被 State=ONLINE 分支安全丢弃,而新登录的
// EnterScene 自会改写位置;此时入队只会白白触发一次 SetSession fail-closed。
var enqueueOfflineCleanupScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) then return 0 end
redis.call("ZADD", KEYS[2], ARGV[1], ARGV[2])
redis.call("HSET", KEYS[3], ARGV[2], ARGV[3])
return 1
`)

func enqueueOfflineCleanup(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	playerID uint64,
	payload []byte,
) (bool, error) {
	result, err := enqueueOfflineCleanupScript.Run(
		ctx,
		svcCtx.RedisClient,
		[]string{sessionKey(playerID), LeaseZSetKey, leaseClaimPayloadHashKey},
		time.Now().Unix(),
		strconv.FormatUint(playerID, 10),
		payload,
	).Int()
	return result == 1, err
}

func sessionLifecycleKeys(playerID uint64) []string {
	return []string{
		sessionKey(playerID),
		LeaseZSetKey,
		LeaseProcessingZSetKey,
		leaseClaimTokenHashKey,
		leaseClaimPayloadHashKey,
	}
}

func replaceSessionAndCancelLease(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	playerID uint64,
	expected []byte,
	updated []byte,
	ttl time.Duration,
) (replaceSessionResult, error) {
	mode := "present"
	if expected == nil {
		mode = "missing"
	}
	result, err := replaceSessionAndCancelLeaseScript.Run(
		ctx,
		svcCtx.RedisClient,
		sessionLifecycleKeys(playerID),
		mode,
		expected,
		updated,
		ttl.Milliseconds(),
		strconv.FormatUint(playerID, 10),
	).Int()
	return replaceSessionResult(result), err
}

func deleteSessionIfUnchanged(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	playerID uint64,
	expected []byte,
) (bool, error) {
	mode := "present"
	if expected == nil {
		mode = "missing"
	}
	keys := []string{
		sessionKey(playerID),
		locationKey(int64(playerID)),
		LeaseZSetKey,
		LeaseProcessingZSetKey,
		leaseClaimTokenHashKey,
		leaseClaimPayloadHashKey,
	}
	result, err := deleteSessionIfUnchangedScript.Run(
		ctx,
		svcCtx.RedisClient,
		keys,
		mode,
		expected,
		strconv.FormatUint(playerID, 10),
	).Int()
	return result == 1, err
}

func claimExpiredLeases(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	now time.Time,
	batchSize int,
	claimTTL time.Duration,
) ([]leaseClaim, error) {
	batchToken := uuid.NewString()
	result, err := claimExpiredLeasesScript.Run(
		ctx,
		svcCtx.RedisClient,
		[]string{LeaseZSetKey, LeaseProcessingZSetKey, leaseClaimTokenHashKey, leaseClaimPayloadHashKey},
		now.Unix(),
		now.Add(claimTTL).Unix(),
		batchSize,
		batchToken,
		sessionKeyPrefix,
	).Slice()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	if len(result)%3 != 0 {
		return nil, fmt.Errorf("lease claim returned malformed tuple count %d", len(result))
	}

	claims := make([]leaseClaim, 0, len(result)/3)
	for i := 0; i < len(result); i += 3 {
		playerID, err := strconv.ParseUint(redisResultString(result[i]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse claimed player id: %w", err)
		}
		claims = append(claims, leaseClaim{
			playerID: playerID,
			token:    redisResultString(result[i+1]),
			payload:  []byte(redisResultString(result[i+2])),
		})
	}
	return claims, nil
}

func redisResultString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}

func ackLeaseClaim(ctx context.Context, svcCtx *svc.ServiceContext, claim leaseClaim) error {
	_, err := ackLeaseClaimScript.Run(
		ctx,
		svcCtx.RedisClient,
		[]string{LeaseProcessingZSetKey, leaseClaimTokenHashKey, leaseClaimPayloadHashKey},
		strconv.FormatUint(claim.playerID, 10),
		claim.token,
	).Int()
	return err
}

func renewLeaseClaims(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	claims []leaseClaim,
	deadline time.Time,
) (int, error) {
	if len(claims) == 0 {
		return 0, nil
	}
	args := make([]any, 1, 1+len(claims)*2)
	args[0] = deadline.Unix()
	for _, claim := range claims {
		args = append(args, strconv.FormatUint(claim.playerID, 10), claim.token)
	}
	return renewLeaseClaimsScript.Run(
		ctx,
		svcCtx.RedisClient,
		[]string{LeaseProcessingZSetKey, leaseClaimTokenHashKey},
		args...,
	).Int()
}

func commitLeaseExpiry(ctx context.Context, svcCtx *svc.ServiceContext, claim leaseClaim) (int, error) {
	keys := []string{
		sessionKey(claim.playerID),
		locationKey(int64(claim.playerID)),
		LeaseZSetKey,
		LeaseProcessingZSetKey,
		leaseClaimTokenHashKey,
		leaseClaimPayloadHashKey,
	}
	return commitLeaseExpiryScript.Run(
		ctx,
		svcCtx.RedisClient,
		keys,
		strconv.FormatUint(claim.playerID, 10),
		claim.token,
		claim.payload,
	).Int()
}

func rearmLeaseClaim(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	claim leaseClaim,
	seconds int64,
) error {
	result, err := rearmLeaseClaimScript.Run(
		ctx,
		svcCtx.RedisClient,
		[]string{
			sessionKey(claim.playerID),
			LeaseZSetKey,
			LeaseProcessingZSetKey,
			leaseClaimTokenHashKey,
			leaseClaimPayloadHashKey,
		},
		strconv.FormatUint(claim.playerID, 10),
		claim.token,
		claim.payload,
		time.Now().Add(time.Duration(seconds)*time.Second).Unix(),
		0,
	).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("lease claim no longer owned")
	}
	// result=2 是正常换代：脚本已丢弃旧 claim，无需把 ONLINE 会话重新续租。
	return nil
}
