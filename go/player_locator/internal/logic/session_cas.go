package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"player_locator/internal/svc"
	pb "proto/player_locator"
	"shared/redisslot"
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

// 会话按 gate 建的索引在下面三段会话 Lua 里与会话写入原子维护(docs/design/player-locator-gate-session-index.md):
//  1. 索引键与成员都由 Go 从「CAS 的 expected 字节解出的旧会话」和「即将写入的新会话」推导(indexedGate)。
//     整字节 CAS 保证提交时 Redis 里的就是 expected,推导与提交之间没有窗口。
//  2. 不变量 I1:会话 ONLINE 且 gate_instance_id 非空 ⇔ player_id 在「它的 slot + 它的 gate」这把索引里。
//  3. 「这一侧没有索引」的哨兵是 ARGV 里的空串 gate,不是 KEYS 里的空串:空串键哈希到 slot 0,上 Cluster 就是 CROSSSLOT。
//     KEYS 里对应位置放本 slot 的登记集占位(gateIndexKeyOrPlaceholder),脚本凭 ARGV 判断,不碰它。
//  4. 一个 slot 的索引空了就把 gate 移出本 slot 的登记集。不收紧的话,一个长寿 gate 迟早碰遍 16384 个 slot,
//     登记集膨胀到 gate 数 × 16384,清扫每拍都要读一遍。这一步只为省内存,不是不变量,所以用 pcall:
//     Redis 不回滚报错前的写入,不能让它把脚本打断在写入之后。
//  5. 写入顺序:可能因键类型不对而报错的簿记排在前面,会话字节最后写。报错时会话原样,调用方照常按失败处理。
//  6. commitLeaseExpiry / rearm / claim / renew / ack / enqueue 这几段**刻意不碰索引**:它们只处理
//     DISCONNECTING 或已删的会话(handleLeaseExpiry 在 commit 前已在 Go 侧校验 State==DISCONNECTING),
//     这些会话本来就不在索引里。不要往这些脚本里补 SREM —— 那会让索引维护散落到不需要它的路径上。
//
// 全部 KEYS 同 slot:会话键的 {pid} 与租约、索引的 slot-tag 按构造同槽,script_slots_contract_test.go 钉住。

// KEYS 见 replaceSessionKeys;ARGV[1]=mode,ARGV[2]=expected,ARGV[3]=updated,ARGV[4]=ttl 毫秒,ARGV[5]=成员,
// ARGV[6]=旧会话的 gate 或 "",ARGV[7]=新会话的 gate 或 ""。
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

-- 同一 gate 上顶号时两个 gate 相等:跳过 SREM 只 SADD。顺序不能改成先 SADD 再 SREM,
-- 那会把同 gate 顶号的成员删掉(TestGateIndex_ReplaceLoginOnSameGateKeepsMember 钉住)。
-- missing 模式在登记任何清理前返回 2、CAS 失败返回 0,这两条早退路径都不碰索引。
if ARGV[6] ~= "" and ARGV[6] ~= ARGV[7] then
    redis.call("SREM", KEYS[7], ARGV[5])
    if redis.call("SCARD", KEYS[7]) == 0 then redis.pcall("SREM", KEYS[6], ARGV[6]) end
end
if ARGV[7] ~= "" then
    redis.call("SADD", KEYS[8], ARGV[5])
    redis.call("SADD", KEYS[6], ARGV[7])
end
redis.call("ZREM", KEYS[2], ARGV[5])
redis.call("ZREM", KEYS[3], ARGV[5])
redis.call("HDEL", KEYS[4], ARGV[5])
redis.call("HDEL", KEYS[5], ARGV[5])
if tonumber(ARGV[4]) > 0 then
    redis.call("SET", KEYS[1], ARGV[3], "PX", ARGV[4])
else
    redis.call("SET", KEYS[1], ARGV[3])
end
return 1
`)

// KEYS 见 setDisconnectingKeys;ARGV[1]=expected,ARGV[2]=updated,ARGV[3]=ttl 毫秒,ARGV[4]=到期 unix 秒,ARGV[5]=成员,
// ARGV[6]=旧会话(ONLINE)的 gate 或 ""。转成 DISCONNECTING 后不再是任何 gate 的成员。
var setDisconnectingScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if (not current) or current ~= ARGV[1] then return 0 end

redis.call("ZADD", KEYS[2], ARGV[4], ARGV[5])
redis.call("ZREM", KEYS[3], ARGV[5])
redis.call("HDEL", KEYS[4], ARGV[5])
redis.call("HDEL", KEYS[5], ARGV[5])
if ARGV[6] ~= "" then
    redis.call("SREM", KEYS[7], ARGV[5])
    if redis.call("SCARD", KEYS[7]) == 0 then redis.pcall("SREM", KEYS[6], ARGV[6]) end
end
if tonumber(ARGV[3]) > 0 then
    redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3])
else
    redis.call("SET", KEYS[1], ARGV[2])
end
return 1
`)

// KEYS 见 deleteSessionKeys;ARGV[1]=mode,ARGV[2]=expected,ARGV[3]=成员,ARGV[4]=被删会话的 gate 或 ""。
var deleteSessionIfUnchangedScript = redis.NewScript(`
local current = redis.call("GET", KEYS[1])
if ARGV[1] == "missing" then
    if current then return 0 end
else
    if (not current) or current ~= ARGV[2] then return 0 end
end

redis.call("ZREM", KEYS[2], ARGV[3])
redis.call("ZREM", KEYS[3], ARGV[3])
redis.call("HDEL", KEYS[4], ARGV[3])
redis.call("HDEL", KEYS[5], ARGV[3])
if ARGV[4] ~= "" then
    redis.call("SREM", KEYS[7], ARGV[3])
    if redis.call("SCARD", KEYS[7]) == 0 then redis.pcall("SREM", KEYS[6], ARGV[4]) end
end
redis.call("DEL", KEYS[1])
return 1
`)

// claimExpiredLeasesScript 实现一个 slot 上 ready -> processing 的可靠 claim：
// 1. processing 超时任务先回 ready；2. 只有 ack 才删除 processing 记录。进程在任意一步退出，任务都会在
// claim deadline 后重新出现。
//
// 脚本只碰本 slot 的租约键,只返回成员与令牌,不读其他玩家的会话:成员各有各的 {pid},放进这段脚本就跨槽。
// 载荷由 pinLeaseClaimPayloadScript 逐人钉住。
// KEYS[1]=ready,KEYS[2]=processing,KEYS[3]=claim_tokens;ARGV[1]=now,ARGV[2]=认领截止,ARGV[3]=批量,ARGV[4]=批次令牌。
// 返回 {成员, 令牌, 成员, 令牌, ...}。
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

    table.insert(result, player_id)
    table.insert(result, token)
end
return result
`)

// pinLeaseClaimPayloadScript 把这次 claim 要用的会话字节钉进 claim_payloads:会话还在就取它(覆盖旧值),
// 不在就用已经钉过的那份(上一轮 commit 已删会话、或 MarkOffline 入队的快照)。令牌已失效(认领之后被 Reconnect /
// SetSession / SetDisconnecting 撤销)时返回 nil,调用方丢弃这个 claim。
// KEYS 见 pinLeaseClaimPayloadKeys;ARGV[1]=成员,ARGV[2]=令牌。
var pinLeaseClaimPayloadScript = redis.NewScript(`
if redis.call("HGET", KEYS[2], ARGV[1]) ~= ARGV[2] then return false end

local payload = redis.call("GET", KEYS[1])
if payload then
    redis.call("HSET", KEYS[3], ARGV[1], payload)
    return payload
end
payload = redis.call("HGET", KEYS[3], ARGV[1])
if payload then return payload end
return ""
`)

// commitLeaseExpiryScript 只删除 claim 时看到的那一版 session。Reconnect/SetSession
// 若已经写入新版本，脚本会把旧 claim 当 stale 丢弃，绝不会删在线新会话。
// 返回值：0=claim 已失效；1=本 claim 已获得清理权；2=session 已换代。
var commitLeaseExpiryScript = redis.NewScript(`
if redis.call("HGET", KEYS[4], ARGV[1]) ~= ARGV[2] then return 0 end

local current = redis.call("GET", KEYS[1])
if current and current ~= ARGV[3] then
    redis.call("ZREM", KEYS[3], ARGV[1])
    redis.call("HDEL", KEYS[4], ARGV[1])
    redis.call("HDEL", KEYS[5], ARGV[1])
    return 2
end

redis.call("DEL", KEYS[1])
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
// 守卫:会话键已经重新出现(玩家瞬间重登)时放弃入队 —— 钉载荷时会优先读
// 活会话,旧清理会被 State=ONLINE 分支安全丢弃,而新登录的
// EnterScene 自会改写位置;此时入队只会白白触发一次 SetSession fail-closed。
var enqueueOfflineCleanupScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) then return 0 end
redis.call("ZADD", KEYS[2], ARGV[1], ARGV[2])
redis.call("HSET", KEYS[3], ARGV[2], ARGV[3])
return 1
`)

// 以下是各段脚本的 KEYS,生产与 script_slots_contract_test.go 共用同一份。

// sessionLifecycleKeys 是会话键加本 slot 的四把租约键:KEYS[1]=会话,KEYS[2]=ready,KEYS[3]=processing,
// KEYS[4]=claim_tokens,KEYS[5]=claim_payloads。
func sessionLifecycleKeys(playerID uint64) []string {
	lease := leaseKeysForPlayer(playerID)
	return []string{sessionKey(playerID), lease.ready, lease.processing, lease.claimTokens, lease.claimPayloads}
}

// replaceSessionKeys:sessionLifecycleKeys 之后是 KEYS[6]=登记集,KEYS[7]=旧 gate 的索引,KEYS[8]=新 gate 的索引。
func replaceSessionKeys(playerID uint64, previousGate, nextGate string) []string {
	slot := redisslot.PlayerSlot(playerID)
	return append(sessionLifecycleKeys(playerID),
		gateRegistryKey(slot),
		gateIndexKeyOrPlaceholder(slot, previousGate),
		gateIndexKeyOrPlaceholder(slot, nextGate),
	)
}

// setDisconnectingKeys:sessionLifecycleKeys 之后是 KEYS[6]=登记集,KEYS[7]=旧 gate 的索引。
func setDisconnectingKeys(playerID uint64, gate string) []string {
	slot := redisslot.PlayerSlot(playerID)
	return append(sessionLifecycleKeys(playerID), gateRegistryKey(slot), gateIndexKeyOrPlaceholder(slot, gate))
}

// deleteSessionKeys 与 setDisconnectingKeys 同形。
func deleteSessionKeys(playerID uint64, gate string) []string {
	return setDisconnectingKeys(playerID, gate)
}

func claimExpiredLeasesKeys(slot uint16) []string {
	lease := leaseKeysForSlot(slot)
	return []string{lease.ready, lease.processing, lease.claimTokens}
}

func pinLeaseClaimPayloadKeys(playerID uint64) []string {
	lease := leaseKeysForPlayer(playerID)
	return []string{sessionKey(playerID), lease.claimTokens, lease.claimPayloads}
}

func ackLeaseClaimKeys(playerID uint64) []string {
	lease := leaseKeysForPlayer(playerID)
	return []string{lease.processing, lease.claimTokens, lease.claimPayloads}
}

func renewLeaseClaimsKeys(slot uint16) []string {
	lease := leaseKeysForSlot(slot)
	return []string{lease.processing, lease.claimTokens}
}

func enqueueOfflineCleanupKeys(playerID uint64) []string {
	lease := leaseKeysForPlayer(playerID)
	return []string{sessionKey(playerID), lease.ready, lease.claimPayloads}
}

// readyAt 是这条清理最早被 LeaseMonitor 领走的时刻(ready ZSET 的分数,unix 秒)。等持有者释放时由调用方延后
// (docs/design/location-release-binding.md §3.4),其余情形传 time.Now()。
func enqueueOfflineCleanup(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	playerID uint64,
	payload []byte,
	readyAt time.Time,
) (bool, error) {
	result, err := enqueueOfflineCleanupScript.Run(
		ctx,
		svcCtx.PlayerRedis,
		enqueueOfflineCleanupKeys(playerID),
		readyAt.Unix(),
		strconv.FormatUint(playerID, 10),
		payload,
	).Int()
	return result == 1, err
}

// replaceSessionAndCancelLease 以整字节 CAS 写入会话并撤销 lease 簿记,同一段 Lua 内维护 gate 索引。
// previous 是 expected 字节解出的旧会话(missing 模式下为 nil),next 是 updated 字节对应的新会话;
// 两者只用来推导索引键,调用方必须保证它们与 expected / updated 一一对应。
func replaceSessionAndCancelLease(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	playerID uint64,
	expected []byte,
	updated []byte,
	ttl time.Duration,
	previous *pb.PlayerSession,
	next *pb.PlayerSession,
) (replaceSessionResult, error) {
	mode := "present"
	if expected == nil {
		mode = "missing"
	}
	previousGate, nextGate := indexedGate(previous), indexedGate(next)
	result, err := replaceSessionAndCancelLeaseScript.Run(
		ctx,
		svcCtx.PlayerRedis,
		replaceSessionKeys(playerID, previousGate, nextGate),
		mode,
		expected,
		updated,
		ttl.Milliseconds(),
		strconv.FormatUint(playerID, 10),
		previousGate,
		nextGate,
	).Int()
	return replaceSessionResult(result), err
}

// transitionToDisconnecting 是 ONLINE → DISCONNECTING 的唯一提交点(SetDisconnecting 与死 gate 清扫共用):
// 整字节 CAS 写入 updated、ZADD 断线租约(deadline 为 unix 秒)、撤销旧 claim,并把玩家摘出旧 gate 的索引。
// previous 是 expected 字节解出的旧会话。返回 false = 会话已被改写,CAS 未生效。
func transitionToDisconnecting(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	playerID uint64,
	expected []byte,
	updated []byte,
	deadlineUnix int64,
	previous *pb.PlayerSession,
) (bool, error) {
	gate := indexedGate(previous)
	swapped, err := setDisconnectingScript.Run(
		ctx,
		svcCtx.PlayerRedis,
		setDisconnectingKeys(playerID, gate),
		expected,
		updated,
		0,
		deadlineUnix,
		strconv.FormatUint(playerID, 10),
		gate,
	).Int()
	return swapped == 1, err
}

// deleteSessionIfUnchanged 整字节 CAS 删除会话及其簿记;current 是 expected 字节解出的会话(missing 模式下为 nil),
// 只用来推导要摘除的索引成员。
func deleteSessionIfUnchanged(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	playerID uint64,
	expected []byte,
	current *pb.PlayerSession,
) (bool, error) {
	mode := "present"
	if expected == nil {
		mode = "missing"
	}
	gate := indexedGate(current)
	result, err := deleteSessionIfUnchangedScript.Run(
		ctx,
		svcCtx.PlayerRedis,
		deleteSessionKeys(playerID, gate),
		mode,
		expected,
		strconv.FormatUint(playerID, 10),
		gate,
	).Int()
	return result == 1, err
}

// claimExpiredLeases 认领一个 slot 上到期的租约,分两步:先按 slot 认领(只拿成员与令牌),再逐人钉载荷
// (这一步才声明该玩家的会话键)。两步之间崩溃,认领按 deadline 回 ready,下一个 worker 重来;两步之间被
// Reconnect / SetSession 撤销,钉载荷时发现令牌已失效,这个 claim 直接丢弃。
//
// 返回的 claim 都已钉好载荷。出错时已钉好的 claim 照样返回,调用方照常处理;没钉上的留在 processing,按 deadline 重来。
func claimExpiredLeases(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	slot uint16,
	now time.Time,
	batchSize int,
	claimTTL time.Duration,
) ([]leaseClaim, error) {
	batchToken := uuid.NewString()
	result, err := claimExpiredLeasesScript.Run(
		ctx,
		svcCtx.PlayerRedis,
		claimExpiredLeasesKeys(slot),
		now.Unix(),
		now.Add(claimTTL).Unix(),
		batchSize,
		batchToken,
	).Slice()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	if len(result)%2 != 0 {
		return nil, fmt.Errorf("lease claim returned malformed tuple count %d", len(result))
	}

	claims := make([]leaseClaim, 0, len(result)/2)
	for i := 0; i < len(result); i += 2 {
		member := redisResultString(result[i])
		playerID, ok := parseIndexMember(member)
		if !ok || redisslot.PlayerSlot(playerID) != slot {
			// 写路径不会写出这种成员。它声明不出同槽的会话键,只能留在 processing,按 deadline 周期性地喊出来,不拖累同批的其他人。
			logx.Errorf("LeaseMonitor: malformed lease member %q in slot %d left in place for investigation", member, slot)
			continue
		}
		token := redisResultString(result[i+1])
		payload, err := pinLeaseClaimPayload(ctx, svcCtx, playerID, token)
		if errors.Is(err, redis.Nil) {
			continue // 令牌已失效:认领之后这次清理已被撤销
		}
		if err != nil {
			if isKeyTypeError(err) {
				logx.Errorf("LeaseMonitor: pin claim payload player=%d skipped, key has the wrong type: %v", playerID, err)
				continue
			}
			return claims, fmt.Errorf("pin claim payload player %d: %w", playerID, err)
		}
		claims = append(claims, leaseClaim{playerID: playerID, token: token, payload: []byte(payload)})
	}
	return claims, nil
}

// pinLeaseClaimPayload 返回这次 claim 的载荷;令牌已失效时返回 redis.Nil。
func pinLeaseClaimPayload(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, token string) (string, error) {
	return pinLeaseClaimPayloadScript.Run(
		ctx,
		svcCtx.PlayerRedis,
		pinLeaseClaimPayloadKeys(playerID),
		strconv.FormatUint(playerID, 10),
		token,
	).Text()
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
		svcCtx.PlayerRedis,
		ackLeaseClaimKeys(claim.playerID),
		strconv.FormatUint(claim.playerID, 10),
		claim.token,
	).Int()
	return err
}

// renewLeaseClaims 按 slot 分组续期。一个 slot 失败不影响别的 slot,错误合并返回;返回值是成功续上的总数。
func renewLeaseClaims(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	claims []leaseClaim,
	deadline time.Time,
) (int, error) {
	if len(claims) == 0 {
		return 0, nil
	}
	bySlot := make(map[uint16][]leaseClaim)
	for _, claim := range claims {
		slot := redisslot.PlayerSlot(claim.playerID)
		bySlot[slot] = append(bySlot[slot], claim)
	}
	renewed := 0
	var errs error
	for slot, group := range bySlot {
		args := make([]any, 1, 1+len(group)*2)
		args[0] = deadline.Unix()
		for _, claim := range group {
			args = append(args, strconv.FormatUint(claim.playerID, 10), claim.token)
		}
		n, err := renewLeaseClaimsScript.Run(ctx, svcCtx.PlayerRedis, renewLeaseClaimsKeys(slot), args...).Int()
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("slot %d: %w", slot, err))
			continue
		}
		renewed += n
	}
	return renewed, errs
}

func commitLeaseExpiry(ctx context.Context, svcCtx *svc.ServiceContext, claim leaseClaim) (int, error) {
	return commitLeaseExpiryScript.Run(
		ctx,
		svcCtx.PlayerRedis,
		sessionLifecycleKeys(claim.playerID),
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
		svcCtx.PlayerRedis,
		sessionLifecycleKeys(claim.playerID),
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
