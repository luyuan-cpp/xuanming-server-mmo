package logic

import (
	"errors"
	"fmt"
	"strconv"

	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// This file holds the server-side Lua scripts that make scene-manager's
// two critical paths — "player enters a scene" and "lifecycle destroys an
// idle scene" — race-safe against each other.
//
// Problem without CAS:
//   goroutine A (lifecycle tick):
//     1. SCARD instance:<id>:members == 0
//     2. (context switch)
//     3. DEL scene:{id}:node, ZREM ..., DEL instance:<id>:members
//   goroutine B (EnterScene RPC, between A.1 and A.3):
//     1. GET scene:{id}:node -> "node-7" (scene still alive)
//     2. UpdatePlayerLocation, route to gate
//     3. SADD instance:<id>:members -> new key with one member
//   Result: player routed into a dead scene; node-scene-count drifts.
//
// Fix: both paths touch the same keys inside a single Lua script so Redis
// serializes them.
//
// 这些脚本的键全部在控制域单实例上,一段脚本里可以跨场景、跨节点(文件 scene_membership.go 讲成员集合)。
// 它们不碰任何玩家域键,scene_keys_contract_test.go 钉住这一点。

// luaAtomicAddSceneMember atomically:
//  1. checks scene:{id}:node still exists,
//  2. if yes, SADDs the player into instance:<id>:members (and the node's set
//     when KEYS[3] is passed) and returns the new SCARD,
//  3. if no, returns -1 without creating the members key.
//
// KEYS[1] = scene:{id}:node
// KEYS[2] = instance:<id>:members
// KEYS[3] = node:zone:{zoneId}:{nodeId}:members(节点未知时不传)
// ARGV[1] = 十进制 player_id
// Return: (int) new count, or -1 if scene is gone.
const luaAtomicAddSceneMember = `
if redis.call('EXISTS', KEYS[1]) == 0 then
    return -1
end
redis.call('SADD', KEYS[2], ARGV[1])
if KEYS[3] ~= nil then
    redis.call('SADD', KEYS[3], ARGV[1])
end
return redis.call('SCARD', KEYS[2])
`

// luaReserveBestWorldChannel atomically selects the least-loaded live world
// channel from KEYS scene/members pairs and reserves one player slot on it.
// Selection and SADD have to be a single Redis operation during login storms,
// otherwise many concurrent calls can observe the same empty channel.
//
// KEYS are laid out as 3N entries so every key the script touches is declared up-front:
//
//	KEYS[1..2N]    = scene:{id}:node, instance:<id>:members, ...
//	                 (interleaved per candidate)
//	KEYS[2N+1..3N] = node:zone:{zoneId}:{candidateNode}:members (one per candidate, so the
//	                 script can look up the chosen candidate's node set without
//	                 string-building a key from data).
//
// ARGV[i] for i in 1..N = nodeId string for candidate i, used both for the
// equality check against scene:{id}:node and to remember the picked node.
// ARGV[N+1] = 十进制 player_id。
//
// Return = {bestIdx, nodeId, newCount}, or nil if every candidate went stale.
//
// 预占是集合成员,同一玩家重复预占不累加;玩家已经在所选频道时 SADD 是空操作,释放预占由 removeSceneMember 的
// location 保住他的成员身份(scene_membership.go)。
//
// NOTE: keys are constructed from Go (see ReserveBestWorldChannel) and
// passed in fixed slots. The script never builds a key from ARGV data.
const luaReserveBestWorldChannel = `
local n = #ARGV - 1
local player = ARGV[n + 1]
local bestIdx = nil
local bestCount = nil

for i = 1, n do
    local sceneNodeKey = KEYS[(i - 1) * 2 + 1]
    local membersKey = KEYS[(i - 1) * 2 + 2]
    if redis.call('GET', sceneNodeKey) == ARGV[i] then
        local count = redis.call('SCARD', membersKey)
        if bestCount == nil or count < bestCount then
            bestIdx = i
            bestCount = count
        end
    end
end

if bestIdx == nil then
    return nil
end

local bestMembersKey = KEYS[(bestIdx - 1) * 2 + 2]
local bestNodeKey = KEYS[2 * n + bestIdx]
redis.call('SADD', bestMembersKey, player)
redis.call('SADD', bestNodeKey, player)
local newCount = redis.call('SCARD', bestMembersKey)

-- ARGV[bestIdx] is the chosen nodeId; recover the sceneId from its
-- scene:{id}:node key by stripping the prefix/suffix on the caller side.
-- To keep the script simple we return the sceneId via ARGV index — Go
-- side knows the candidate order and can map back.
return {tostring(bestIdx), ARGV[bestIdx], tostring(newCount)}
`

// luaAtomicDestroyInstance atomically:
//  1. checks instance:<id>:members is empty,
//  2. if yes, reads scene:{id}:node into a local, then wipes every
//     scene-scoped Redis key and removes the scene from the active set,
//     and returns the nodeId (so the caller can fire DestroyScene RPC and
//     decrement node counters outside the script),
//  3. if no (player count > 0), returns empty string without touching
//     anything — destroy was a false alarm, try again next tick.
//
// KEYS[1] = scene:{id}:node
// KEYS[2] = instance:<id>:members
// KEYS[3] = instances:zone:{zone}:active (sorted set)
// KEYS[4] = scene:{id}:mirror
// KEYS[5] = scene:{id}:source
// KEYS[6] = scene:{id}:zone
// KEYS[7] = scene:{id}:agones_gs
// ARGV[1] = sceneId as string (for ZREM)
// Return: {nodeId, agonesGsName} on destroy, {"", ""} on abort.
//
// scene:{id}:agones_gs 必须和其余 scene 状态在**同一个脚本里**删除,
// 并且它的值要在删之前读出来返回给调用方:
//   - 不一起删 -> 留下悬空映射,下次同 id 复用时会把计数减到别人头上
//   - 不返回   -> 删完就再也不知道该向哪个 GameServer 归还 rooms 名额
//
// Note: we do NOT touch node:zone:{zoneId}:{nodeId}:scenes inside the script because we
// don't know the node id until after we read it (and the key name is
// dynamic). The caller does that SREM outside, which is fine: the
// reconciliation loop double-checks scene:{id}:node before destroying so
// a stale node-scenes set entry is harmless.
const luaAtomicDestroyInstance = `
if redis.call('SCARD', KEYS[2]) > 0 then
    return {'', ''}
end
local nodeId = redis.call('GET', KEYS[1])
local agonesGs = redis.call('GET', KEYS[7])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[4], KEYS[5], KEYS[6], KEYS[7])
redis.call('ZREM', KEYS[3], ARGV[1])
if not nodeId then
    nodeId = ''
end
if not agonesGs then
    agonesGs = ''
end
return {nodeId, agonesGs}
`

// luaAtomicDestroyInstanceForce 是 luaAtomicDestroyInstance 的 force 版:
// 不做人数守卫;ARGV[2] 为空时无条件抹掉场景键,非空时只在属主等于它时抹;
// 返回抹除前的 {nodeId, agonesGs, '1', 成员 1, 成员 2, ...}。读与 DEL 在同一脚本里原子执行,
// 并发的销毁(空闲回收的非 force 脚本、explicit / 级联 / 收尾的 force 脚本,或领导者降级
// 窗口里的新旧领导者)只有第一个能拿到非空返回 —— 计数减法 / Agones 名额归还 / 镜像级联
// 这些副作用因此恰好执行一次,不双减计数、不双还名额。
// 成员交回给调用方,由它从节点集合里摘掉(节点键要等读到属主才知道,不在脚本里现拼)。
//
// KEYS[1..7] 与 luaAtomicDestroyInstance 相同。
// ARGV[1] = sceneId(ZREM 用)
// ARGV[2] = 期望属主;空串表示不比对(explicit / source_migrated / source_scaled_in 经
// destroyInstanceForce,以及镜像级联,语义同前)。空闲回收走的是 luaAtomicDestroyInstance,不经这里。
// 非空时只有 scene:{id}:node 仍等于它才抹,否则一个键都不动,返回两个空串。
// 死节点收尾靠它(2026-10-22):收尾读到「属主是死节点」之后,只要有写者用 reassignSceneNode 把实例
// 改派到活节点并 CreateScene,不比对的话这里会把新属主的场景抹掉、向新节点发 DestroyScene、扣新节点的
// 计数。比较必须和删除在同一个脚本里,收尾自己的读挡不住这个窗口。(修复时这样的写者是 resolveScene 的
// 死节点自愈;10-23 起它只重建世界频道,比对作为纵深防御保留,见 destroyOrphanOfDeadNode。)
// ARGV[2] 非空而映射已不存在时同样不动(GET 返回 false,不等于 ARGV[2]):删映射的那条路径(并发销毁,
// 或 rollbackInstanceAllocation)负责其余清理,这里再抹会和它双还 Agones 名额。ARGV[2] 为空时不做这个
// 判断,映射缺失仍会抹掉余下的键并返回 agonesGs,与 rollbackInstanceAllocation 交错时可能双还名额 ——
// 这是既有残余(docs/PROGRESS.md 2026-10-23「死节点收尾与显式 DestroyScene…」条目 R3)。
//
// 另有两个可选参数,只由 atomicTeardownScene(排空收尾、孤儿清理,都是世界频道)使用,AtomicDestroyForce 不传:
//   - ARGV[3] = 'absent':调用方先前读到映射不存在,要求此刻仍不存在;期间被写入了属主就一个键都不动。
//   - KEYS[8..]:在删映射的同一步把成员 ARGV[1] 从这些集合里摘掉(频道集合 / 排空集合)。
//
// 认领成功时第 3 个元素固定是 '1',其后才是成员;比对落空只回两个空串,调用方据此区分「落空」与「认领了但各键本来就空」。
const luaAtomicDestroyInstanceForce = `
local nodeId = redis.call('GET', KEYS[1])
if ARGV[2] ~= nil and ARGV[2] ~= '' and nodeId ~= ARGV[2] then
    return {'', ''}
end
-- ARGV[3] = 'absent'(可选,见 atomicTeardownScene):调用方先前读到映射不存在;期间若被写入了属主,一个键都不动。
if ARGV[3] == 'absent' and nodeId then
    return {'', ''}
end
local agonesGs = redis.call('GET', KEYS[7])
local members = redis.call('SMEMBERS', KEYS[2])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[4], KEYS[5], KEYS[6], KEYS[7])
redis.call('ZREM', KEYS[3], ARGV[1])
-- KEYS[8] 起(可选,见 atomicTeardownScene):在删映射的同一步里把场景从这些集合摘掉。
for i = 8, #KEYS do
    redis.call('SREM', KEYS[i], ARGV[1])
end
if not nodeId then
    nodeId = ''
end
if not agonesGs then
    agonesGs = ''
end
local out = {nodeId, agonesGs, '1'}
for i = 1, #members do
    out[i + 3] = members[i]
end
return out
`

// AtomicDestroyForce 原子认领并抹掉一个场景的全部 Redis 键(不看人数)。
// 返回抹除前的 (nodeId, agonesGs, members)。三者全空说明别的进程
// 已抢先抹掉 —— 调用方必须跳过一切副作用(计数、归还、级联、RPC)。
//
// expectedOwner 非空时只在 scene:{id}:node 仍等于它时才抹;不等(含映射已不存在)时什么都不动,
// 返回值同样全空。认领成功时返回的 nodeId 必然等于 expectedOwner,所以调用方仍按「全空即未认领」判断。
// 空串表示不比对。
func AtomicDestroyForce(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, expectedOwner string) (string, string, []string, error) {
	raw, err := svcCtx.Redis.Eval(luaAtomicDestroyInstanceForce, sceneDestroyKeys(zoneId, sceneId), fmt.Sprintf("%d", sceneId), expectedOwner)
	if err != nil {
		return "", "", nil, err
	}
	claim := parseForceDestroyReply(raw)
	return claim.NodeID, claim.AgonesGs, claim.Members, nil
}

// sceneDestroyKeys 是 luaAtomicDestroyInstanceForce 的 KEYS[1..7]:一个场景在 Redis 里的全部状态键,
// 外加 zone 的 active instances 集合(ZREM 用)。顺序是脚本的契约。
func sceneDestroyKeys(zoneId uint32, sceneId uint64) []string {
	return []string{
		sceneNodeKey(sceneId),
		instanceMembersKey(sceneId),
		activeInstancesKey(zoneId),
		sceneMirrorFlagKey(sceneId),
		sceneSourceKey(sceneId),
		sceneZoneKey(sceneId),
		sceneAgonesGsKey(sceneId),
	}
}

// teardownClaim 是 luaAtomicDestroyInstanceForce 的应答。Claimed=false 表示比对落空、一个键都没动;
// Claimed=true 时其余字段是脚本删除前读到的值(属主为空串 = 映射本来就不在)。
// Members 是场景里剩的成员,要由调用方从节点集合里摘掉(removeResidualSceneMembers)。
type teardownClaim struct {
	Claimed  bool
	NodeID   string
	AgonesGs string
	Members  []string
}

// parseForceDestroyReply 解 luaAtomicDestroyInstanceForce 的应答,格式见脚本注释。
func parseForceDestroyReply(raw any) teardownClaim {
	parts := toSlice(raw)
	if len(parts) < 3 || toString(parts[2]) != "1" {
		return teardownClaim{}
	}
	members := make([]string, 0, len(parts)-3)
	for _, member := range parts[3:] {
		members = append(members, toString(member))
	}
	return teardownClaim{Claimed: true, NodeID: toString(parts[0]), AgonesGs: toString(parts[1]), Members: members}
}

// atomicTeardownScene 原子认领并抹掉一个世界频道的场景键,并在同一步里把它从 unlistKeys 这些集合里摘掉
// (成员为十进制 sceneId,用于频道集合 / 排空集合)。给按「先读后拆」两步走的收尾路径用:排空收尾、孤儿清理。
//
// 与 AtomicDestroyForce 的区别(2026-10-23):
//   - expectedOwner 为空串表示「先前读到映射不存在,现在必须仍不存在」,而不是「不比对」:两步之间若有
//     写者(懒改派等)给它写了属主,一个键都不动。否则会把新属主的映射抹掉,而调用方按空属主做副作用,
//     新节点上的实体与计数都没人收。
//   - 集合摘除与删映射同一步:不会出现「集合里还列着、映射已删」的中间态 —— 那个状态会被任意副本上的
//     懒改派当成陈旧频道复活。
//   - 返回 Claimed,区分「比对落空」与「认领成功但各键本来就空」。
func atomicTeardownScene(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, expectedOwner string, unlistKeys ...string) (teardownClaim, error) {
	keys := append(sceneDestroyKeys(zoneId, sceneId), unlistKeys...)
	ownerMode := ""
	if expectedOwner == "" {
		ownerMode = "absent"
	}
	raw, err := svcCtx.Redis.Eval(luaAtomicDestroyInstanceForce, keys, strconv.FormatUint(sceneId, 10), expectedOwner, ownerMode)
	if err != nil {
		return teardownClaim{}, err
	}
	return parseForceDestroyReply(raw), nil
}

// AtomicAddSceneMemberIfSceneExists 是 EnterScene 指定场景时的预占:只有 scene:{id}:node 还在才把玩家加进成员集合,
// 场景在 EnterScene 中途被销毁时不会造出孤儿键。nodeID 为空时不动节点集合。
//
// Returns the new count, or -1 if the scene was destroyed between
// resolveScene and this call. Callers should treat -1 as "retry or
// fail the request".
func AtomicAddSceneMemberIfSceneExists(svcCtx *svc.ServiceContext, sceneId uint64, zoneId uint32, nodeId string, playerId uint64) (int64, error) {
	keys := []string{
		sceneNodeKey(sceneId),
		instanceMembersKey(sceneId),
	}
	if nodeId != "" {
		keys = append(keys, nodeMembersKey(zoneId, nodeId))
	}
	raw, err := svcCtx.Redis.Eval(luaAtomicAddSceneMember, keys, strconv.FormatUint(playerId, 10))
	if err != nil {
		return 0, err
	}
	return toInt64(raw), nil
}

// ReserveBestWorldChannel picks and reserves a world channel in one Redis
// script so bursty EnterScene traffic spreads across channels instead of
// stampeding the same zero-count scene.
//
// KEY layout passed to the script:
//
//	KEYS[1..2N]   = scene:{id}:node, instance:<id>:members, ...
//	                interleaved per candidate (N candidates total)
//	KEYS[2N+1..3N] = node:zone:{zoneId}:{candidateNode}:members, one per candidate,
//	                 indexed identically to ARGV so the script can pick
//	                 the winner's node key without string-building.
//
// ARGV[i] = candidate i's nodeId string. Used both as the value the
// scene:{id}:node GET is compared to, and as the node identifier
// returned to Go. ARGV[N+1] = 十进制 player_id。
func ReserveBestWorldChannel(svcCtx *svc.ServiceContext, candidates []WorldChannelCandidate, playerId uint64) (uint64, string, int64, error) {
	if len(candidates) == 0 {
		return 0, "", 0, nil
	}

	n := len(candidates)
	keys := make([]string, 0, 3*n)
	// Pairs: scene_node_key, members_key, scene_node_key, members_key, ...
	for _, c := range candidates {
		keys = append(keys,
			fmt.Sprintf(SceneNodeKeyFmt, c.SceneID),
			instanceMembersKey(c.SceneID),
		)
	}
	// Trailing N entries: per-candidate node members keys.
	for _, c := range candidates {
		keys = append(keys, nodeMembersKey(c.ZoneID, c.NodeID))
	}

	args := make([]any, 0, n+1)
	for _, c := range candidates {
		args = append(args, c.NodeID)
	}
	args = append(args, strconv.FormatUint(playerId, 10))

	raw, err := svcCtx.Redis.Eval(luaReserveBestWorldChannel, keys, args...)
	// 脚本回 nil 表示候选全部陈旧,go-redis 把它翻成 redis.Nil:这是「没选到」,不是读错。
	if errors.Is(err, redis.Nil) {
		return 0, "", 0, nil
	}
	if err != nil {
		return 0, "", 0, err
	}

	parts := toSlice(raw)
	if len(parts) < 3 {
		return 0, "", 0, nil
	}

	bestIdx, _ := strconv.Atoi(toString(parts[0]))
	if bestIdx < 1 || bestIdx > n {
		logx.Errorf("[Lua] luaReserveBestWorldChannel returned out-of-range bestIdx=%d (n=%d)", bestIdx, n)
		return 0, "", 0, nil
	}
	winner := candidates[bestIdx-1]
	count := toInt64(parts[2])
	// parts[1] is the same nodeId we passed in; trust the Go-side mapping.
	return winner.SceneID, winner.NodeID, count, nil
}

// AtomicDestroyIfIdle is the race-safe replacement for the multi-step
// Redis cleanup in destroyInstance. If the scene still has players
// (members not empty), returns "" without touching anything. Otherwise
// wipes the scene's Redis state and returns the nodeId the scene was on
// so the caller can notify C++ and fix counters.
//
// 第二个返回值是这个 Scene 所在的 Agones GameServer 名字(非 Agones 模式
// 下为空)。它和 scene 状态在同一个脚本里被读出并删除,调用方拿它去精确
// 归还 rooms 名额 —— 先删后读会永久丢失回滚依据。
//
// Returns ("", "", nil) when the destroy was aborted because the scene
// picked up a player in the meantime; callers must skip further work in
// that case (no RPC, no counter decrement, no room release).
func AtomicDestroyIfIdle(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64) (string, string, error) {
	keys := []string{
		sceneNodeKey(sceneId),
		instanceMembersKey(sceneId),
		activeInstancesKey(zoneId),
		sceneMirrorFlagKey(sceneId),
		sceneSourceKey(sceneId),
		sceneZoneKey(sceneId),
		sceneAgonesGsKey(sceneId),
	}
	raw, err := svcCtx.Redis.Eval(luaAtomicDestroyInstance, keys, fmt.Sprintf("%d", sceneId))
	if err != nil {
		return "", "", err
	}
	parts := toSlice(raw)
	if len(parts) < 2 {
		return "", "", nil
	}
	return toString(parts[0]), toString(parts[1]), nil
}

// toInt64 converts the untyped return of Redis.Eval to int64.
// go-zero's redis client returns int64 for RESP integer replies but some
// transports surface it as string or []byte. Be defensive.
func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case []byte:
		n, _ := strconv.ParseInt(string(x), 10, 64)
		return n
	}
	logx.Errorf("[Lua] unexpected int reply type %T: %v", v, v)
	return 0
}

// toString converts the untyped return of Redis.Eval to string. Same
// defensiveness as toInt64 — the return shape is backend-dependent.
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func toSlice(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i := range x {
			out[i] = x[i]
		}
		return out
	case nil:
		return nil
	}
	logx.Errorf("[Lua] unexpected array reply type %T: %v", v, v)
	return nil
}
