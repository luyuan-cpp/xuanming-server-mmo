package logic

import (
	"fmt"
	"strconv"

	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// This file holds the server-side Lua scripts that make scene-manager's
// two critical paths — "player enters a scene" and "lifecycle destroys an
// idle scene" — race-safe against each other.
//
// Problem without CAS:
//   goroutine A (lifecycle tick):
//     1. GET instance:{id}:player_count == "0"
//     2. (context switch)
//     3. DEL scene:{id}:node, ZREM ..., DEL instance:{id}:player_count
//   goroutine B (EnterScene RPC, between A.1 and A.3):
//     1. GET scene:{id}:node -> "node-7" (scene still alive)
//     2. UpdatePlayerLocation, route to gate
//     3. INCR instance:{id}:player_count -> new key "1"
//   Result: player routed into a dead scene; an orphan player_count key
//   lives forever; node-scene-count drifts.
//
// Fix: both paths touch the same keys inside a single Lua script so Redis
// serializes them.

// luaAtomicIncrPlayerCount atomically:
//  1. checks scene:{id}:node still exists,
//  2. if yes, INCRs instance:{id}:player_count and returns the new count,
//  3. if no, returns -1 without creating the player_count key.
//
// KEYS[1] = scene:{id}:node
// KEYS[2] = instance:{id}:player_count
// Return: (int) new count, or -1 if scene is gone.
const luaAtomicIncrPlayerCount = `
if redis.call('EXISTS', KEYS[1]) == 0 then
    return -1
end
return redis.call('INCR', KEYS[2])
`

// luaReserveBestWorldChannel atomically selects the least-loaded live world
// channel from KEYS scene/count pairs and reserves one player slot on it.
// Selection and INCR have to be a single Redis operation during login storms,
// otherwise many concurrent calls can observe the same zero-count channel.
//
// KEYS are laid out as 2N + 1 + 1 entries so every key the script touches is
// declared up-front, which is the Redis Cluster contract:
//
//	KEYS[1..2N] = scene:{id}:node, instance:{id}:player_count, ...
//	              (interleaved per candidate)
//	KEYS[2N+1]  = node:zone:{zoneId}:{bestNode}:player_count (one per candidate, packed
//	              starting at index 2N+1 so the script can look up the
//	              chosen candidate's node counter without string-building
//	              a key from data).
//
// ARGV[i] for i in 1..N = nodeId string for candidate i, used both for the
// equality check against scene:{id}:node and to remember the picked node.
//
// Return = {sceneId, nodeId, newCount}, or nil if every candidate went stale.
//
// NOTE: keys are constructed from Go (see ReserveBestWorldChannel) and
// passed in fixed slots. The script never builds a key from ARGV data,
// which means it stays Cluster-safe.
const luaReserveBestWorldChannel = `
local n = #ARGV
local bestIdx = nil
local bestCount = nil

for i = 1, n do
    local sceneNodeKey = KEYS[(i - 1) * 2 + 1]
    local countKey = KEYS[(i - 1) * 2 + 2]
    if redis.call('GET', sceneNodeKey) == ARGV[i] then
        local raw = redis.call('GET', countKey)
        local count = tonumber(raw or '0') or 0
        if bestCount == nil or count < bestCount then
            bestIdx = i
            bestCount = count
        end
    end
end

if bestIdx == nil then
    return nil
end

local bestCountKey = KEYS[(bestIdx - 1) * 2 + 2]
local bestNodeKey = KEYS[2 * n + bestIdx]
local newCount = redis.call('INCR', bestCountKey)
redis.call('INCR', bestNodeKey)

-- ARGV[bestIdx] is the chosen nodeId; recover the sceneId from its
-- scene:{id}:node key by stripping the prefix/suffix on the caller side.
-- To keep the script simple we return the sceneId via ARGV index — Go
-- side knows the candidate order and can map back.
return {tostring(bestIdx), ARGV[bestIdx], tostring(newCount)}
`

// luaAtomicDestroyInstance atomically:
//  1. checks instance:{id}:player_count is missing or equals "0",
//  2. if yes, reads scene:{id}:node into a local, then wipes every
//     scene-scoped Redis key and removes the scene from the active set,
//     and returns the nodeId (so the caller can fire DestroyScene RPC and
//     decrement node counters outside the script),
//  3. if no (player count > 0), returns empty string without touching
//     anything — destroy was a false alarm, try again next tick.
//
// KEYS[1] = scene:{id}:node
// KEYS[2] = instance:{id}:player_count
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
local pc = redis.call('GET', KEYS[2])
if pc and pc ~= '0' and pc ~= '' then
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

// AtomicIncrPlayerCountIfSceneExists is the race-safe replacement for
// IncrInstancePlayerCount used inside EnterScene. It guarantees the
// player_count key is only ever created/incremented when scene:{id}:node
// still exists, eliminating orphan keys when the scene gets destroyed in
// the middle of EnterScene.
//
// Returns the new count, or -1 if the scene was destroyed between
// resolveScene and this call. Callers should treat -1 as "retry or
// fail the request".
func AtomicIncrPlayerCountIfSceneExists(svcCtx *svc.ServiceContext, sceneId uint64) (int64, error) {
	keys := []string{
		sceneNodeKey(sceneId),
		fmt.Sprintf(InstancePlayerCountKey, sceneId),
	}
	raw, err := svcCtx.Redis.Eval(luaAtomicIncrPlayerCount, keys)
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
//	KEYS[1..2N]   = scene:{id}:node, instance:{id}:player_count, ...
//	                interleaved per candidate (N candidates total)
//	KEYS[2N+1..3N] = node:zone:{zoneId}:{candidateNode}:player_count, one per candidate,
//	                 indexed identically to ARGV so the script can pick
//	                 the winner's node key without string-building.
//
// ARGV[i] = candidate i's nodeId string. Used both as the value the
// scene:{id}:node GET is compared to, and as the node identifier
// returned to Go.
func ReserveBestWorldChannel(svcCtx *svc.ServiceContext, candidates []WorldChannelCandidate) (uint64, string, int64, error) {
	if len(candidates) == 0 {
		return 0, "", 0, nil
	}

	n := len(candidates)
	keys := make([]string, 0, 3*n)
	// Pairs: scene_node_key, count_key, scene_node_key, count_key, ...
	for _, c := range candidates {
		keys = append(keys,
			fmt.Sprintf(SceneNodeKeyFmt, c.SceneID),
			fmt.Sprintf(InstancePlayerCountKey, c.SceneID),
		)
	}
	// Trailing N entries: per-candidate node player_count keys.
	for _, c := range candidates {
		keys = append(keys, nodePlayerCountKey(c.ZoneID, c.NodeID))
	}

	args := make([]any, 0, n)
	for _, c := range candidates {
		args = append(args, c.NodeID)
	}

	raw, err := svcCtx.Redis.Eval(luaReserveBestWorldChannel, keys, args...)
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
// (player_count > 0), returns "" without touching anything. Otherwise
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
		fmt.Sprintf(InstancePlayerCountKey, sceneId),
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
