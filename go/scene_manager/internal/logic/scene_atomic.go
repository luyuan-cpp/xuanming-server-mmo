package logic

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"scene_manager/internal/svc"

	"github.com/google/uuid"
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

// sceneClaimRecordTTLSeconds 是认领记录的存活时间。只要长过一次「连接中断 + 调用方下一拍重来」:
// 过期之后重来的认领读不到快照,按「已被别人销毁」跳过副作用,只有 Agones 对账告警能发现。
const sceneClaimRecordTTLSeconds = 600

// luaSceneClaimPrelude 是认领脚本共用的开头,解决「已执行、回包丢失」(R5):go-redis 对读超时 / EOF 会原样重发
// EVAL,重发时映射已经没了,认领落空,调用方按「已被别人销毁」跳过级联、C++ DestroyScene、SREM、计数、
// Agones 归还,这些副作用永久丢失。
//
// KEYS[8] 是认领记录,ARGV[2] 是本次认领的 token,ARGV[3] 是记录的存活秒数。同一 token 的记录在,说明脚本已经
// 执行过:什么都不再动,原样返回当时的快照。认领落空和 busy 不写记录,它们什么都没改,重发时重新判定就是对的。
// 记录里的 token 不是当前这个时,说明是另一次认领留下的,照常往下走。
const luaSceneClaimPrelude = `
local prior = redis.call('GET', KEYS[8])
if prior then
    local record = cjson.decode(prior)
    if record.token == ARGV[2] then
        return record.reply
    end
end
local function recordClaim(reply)
    redis.call('SET', KEYS[8], cjson.encode({token = ARGV[2], reply = reply}), 'EX', ARGV[3])
    return reply
end
`

// 三段认领脚本的应答格式相同,由 parseSceneClaimReply 解:{状态, nodeId, agonesGs, 成员...}。
// 状态是 'claimed'、'stale'(比对落空,一个键都没动)或 'busy'(场景里有人,只有空闲回收脚本会回)。
// 格式统一是因为 SceneClaimTokens 按场景记 token:空闲回收和 force 都可能接手同一个结局未知的认领,
// 接手的一方拿到的快照要能照常做完副作用。

// luaAtomicDestroyInstance 是空闲回收的认领脚本:
//  1. 场景里有人:把空闲时钟重置成 ARGV[4],一个键都不删,回 'busy'。重置放在脚本里,并发销毁之后不会把
//     已经摘掉的实例重新加回 active 集合(ZADD XX),那会留下永久僵尸;
//  2. 没人:读出属主和 agones_gs,抹掉场景的全部键,回 'claimed'。
//
// 映射已经不在时同样回 'claimed',NodeID 为空:这是 active 集合里的残留,要摘掉,不能当成有人进场再加回去,
// agones_gs 还在就交给调用方归还。
//
// KEYS[1..8] 见 sceneClaimKeys。
// ARGV[1] = sceneId,ARGV[2..3] 见 luaSceneClaimPrelude,ARGV[4] = 当前 Unix 秒。
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
const luaAtomicDestroyInstance = luaSceneClaimPrelude + `
if redis.call('SCARD', KEYS[2]) > 0 then
    redis.call('ZADD', KEYS[3], 'XX', ARGV[4], ARGV[1])
    return {'busy', '', ''}
end
local nodeId = redis.call('GET', KEYS[1])
local agonesGs = redis.call('GET', KEYS[7])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[4], KEYS[5], KEYS[6], KEYS[7])
redis.call('ZREM', KEYS[3], ARGV[1])
return recordClaim({'claimed', nodeId or '', agonesGs or ''})
`

// luaAtomicDestroyInstanceForce 是 luaAtomicDestroyInstance 的 force 版:不做人数守卫,认领以映射存在为前提;
// 回 'claimed' 时带抹除前的 nodeId、agonesGs 和成员。读与 DEL 在同一脚本里原子执行,
// 并发的销毁(空闲回收的非 force 脚本、explicit / 级联 / 收尾的 force 脚本,或领导者降级
// 窗口里的新旧领导者)只有第一个能认领 —— 计数减法 / Agones 名额归还 / 镜像级联
// 这些副作用因此恰好执行一次,不双减计数、不双还名额。
// 成员交回给调用方,由它从节点集合里摘掉(节点键要等读到属主才知道,不在脚本里现拼)。
//
// KEYS[1..8] 见 sceneClaimKeys。
// ARGV[1] = sceneId,ARGV[2..3] 见 luaSceneClaimPrelude。
// ARGV[4] = 期望属主;空串表示不比对(explicit / source_migrated / source_scaled_in 经
// destroyInstanceForce,以及镜像级联,语义同前)。空闲回收走的是 luaAtomicDestroyInstance,不经这里。
// 非空时只有 scene:{id}:node 仍等于它才抹,否则一个键都不动,回 'stale'。
// 死节点收尾靠它(2026-10-22):收尾读到「属主是死节点」之后,只要有写者用 reassignSceneNode 把实例
// 改派到活节点并 CreateScene,不比对的话这里会把新属主的场景抹掉、向新节点发 DestroyScene、扣新节点的
// 计数。比较必须和删除在同一个脚本里,收尾自己的读挡不住这个窗口。(修复时这样的写者是 resolveScene 的
// 死节点自愈;10-23 起它只重建世界频道,比对作为纵深防御保留,见 destroyOrphanOfDeadNode。)
// 映射不存在时一律不动:删映射的路径(并发销毁,或 luaRollbackInstanceAllocation)在同一步里抹了全部键并负责
// 其余清理,这里再认领会双还名额。
//
// 另有两个可选参数,只由 atomicTeardownScene(排空收尾、孤儿清理,都是世界频道)使用,AtomicDestroyForce 不传:
//   - ARGV[5] = 'absent':调用方先前读到映射不存在,要求此刻仍不存在;期间被写入了属主就一个键都不动。
//     这是映射不存在时唯一能认领的模式,认领时 NodeID 为空,用来收掉频道集合里的残留。
//   - KEYS[9..]:在删映射的同一步把成员 ARGV[1] 从这些集合里摘掉(频道集合 / 排空集合)。
const luaAtomicDestroyInstanceForce = luaSceneClaimPrelude + `
local nodeId = redis.call('GET', KEYS[1])
if ARGV[4] ~= '' and nodeId ~= ARGV[4] then
    return {'stale', '', ''}
end
if ARGV[5] == 'absent' then
    if nodeId then
        return {'stale', '', ''}
    end
elseif not nodeId then
    return {'stale', '', ''}
end
local agonesGs = redis.call('GET', KEYS[7])
local members = redis.call('SMEMBERS', KEYS[2])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[4], KEYS[5], KEYS[6], KEYS[7])
redis.call('ZREM', KEYS[3], ARGV[1])
for i = 9, #KEYS do
    redis.call('SREM', KEYS[i], ARGV[1])
end
local out = {'claimed', nodeId or '', agonesGs or ''}
for i = 1, #members do
    out[i + 3] = members[i]
end
return recordClaim(out)
`

// luaRollbackInstanceAllocation 撤销一次失败的实例创建,R3 的修法:原来逐条 DEL、不查错,中途中断会留下
// 「映射没了、其它键还在」的残留。只有映射仍指向这次分配的节点才认领,否则回 'stale'、一个键都不动:
// 期间被销毁或改派了,收尾归那条路径。
//
// 与销毁脚本不同,节点反向索引、计数和镜像源的子集合都在脚本里摘:这次分配的节点已知,没有理由留到脚本外。
//
// KEYS[1..8] 见 sceneClaimKeys,KEYS[9] = 节点场景集合,KEYS[10] = 节点 scene_count,
// KEYS[11] = 镜像源的子集合(不是镜像时不传)。
// ARGV[1] = sceneId,ARGV[2..3] 见 luaSceneClaimPrelude,ARGV[4] = 这次分配的节点。
const luaRollbackInstanceAllocation = luaSceneClaimPrelude + `
if redis.call('GET', KEYS[1]) ~= ARGV[4] then
    return {'stale', '', ''}
end
local agonesGs = redis.call('GET', KEYS[7])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[4], KEYS[5], KEYS[6], KEYS[7])
redis.call('ZREM', KEYS[3], ARGV[1])
redis.call('SREM', KEYS[9], ARGV[1])
redis.call('DECR', KEYS[10])
if KEYS[11] then
    redis.call('SREM', KEYS[11], ARGV[1])
end
return recordClaim({'claimed', ARGV[4], agonesGs or ''})
`

// AtomicDestroyForce 原子认领并抹掉一个场景的全部 Redis 键(不看人数)。
// 返回抹除前的 (nodeId, agonesGs, members)。三者全空说明别的进程
// 已抢先抹掉 —— 调用方必须跳过一切副作用(计数、归还、级联、RPC)。
//
// expectedOwner 非空时只在 scene:{id}:node 仍等于它时才抹;不等(含映射已不存在)时什么都不动,
// 返回值同样全空。认领成功时返回的 nodeId 必然等于 expectedOwner,所以调用方仍按「全空即未认领」判断。
// 空串表示不比对。
func AtomicDestroyForce(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, expectedOwner string) (string, string, []string, error) {
	claim, err := evalSceneClaim(svcCtx, sceneId, luaAtomicDestroyInstanceForce, sceneClaimKeys(zoneId, sceneId), expectedOwner)
	if err != nil {
		return "", "", nil, err
	}
	return claim.NodeID, claim.AgonesGs, claim.Members, nil
}

// sceneClaimKeys 是认领脚本的 KEYS[1..8]:一个场景在 Redis 里的全部状态键、zone 的 active instances 集合
// (ZREM 用)和认领记录。顺序是脚本的契约。
func sceneClaimKeys(zoneId uint32, sceneId uint64) []string {
	return []string{
		sceneNodeKey(sceneId),
		instanceMembersKey(sceneId),
		activeInstancesKey(zoneId),
		sceneMirrorFlagKey(sceneId),
		sceneSourceKey(sceneId),
		sceneZoneKey(sceneId),
		sceneAgonesGsKey(sceneId),
		sceneClaimRecordKey(sceneId),
	}
}

// sceneClaim 是认领脚本的应答。Claimed=false 时其余字段都为空:比对落空,或场景里有人(Busy)。
// Claimed=true 时其余字段是脚本删除前读到的值(属主为空串 = 映射本来就不在)。
// Members 是场景里剩的成员,要由调用方从节点集合里摘掉(removeResidualSceneMembers)。
type sceneClaim struct {
	Claimed  bool
	Busy     bool
	NodeID   string
	AgonesGs string
	Members  []string
}

// parseSceneClaimReply 解认领脚本的应答,格式见 luaSceneClaimPrelude 之后的说明。
func parseSceneClaimReply(raw any) sceneClaim {
	parts := toSlice(raw)
	if len(parts) < 3 {
		return sceneClaim{}
	}
	switch toString(parts[0]) {
	case "busy":
		return sceneClaim{Busy: true}
	case "claimed":
		members := make([]string, 0, len(parts)-3)
		for _, member := range parts[3:] {
			members = append(members, toString(member))
		}
		return sceneClaim{Claimed: true, NodeID: toString(parts[1]), AgonesGs: toString(parts[2]), Members: members}
	}
	return sceneClaim{}
}

// evalSceneClaim 跑一段销毁认领脚本,保证结局未知的认领能被接手。
//
// go-redis 内部重发带的是同一个 token,脚本自己就幂等。内部重试也用完、Eval 回了错时,脚本可能已经执行:
// 这里把 token 记下,这个进程下一次认领同一场景时带上它,才取得回当时的快照。不记的话副作用还是丢。
// LoadAndDelete 保证并发的两次认领不会拿到同一个 token,否则两边都做一遍副作用。
//
// 只能接手本进程的认领:进程在认领之后崩溃,副作用仍然丢(残余 R1,靠对账和告警兜底)。
func evalSceneClaim(svcCtx *svc.ServiceContext, sceneId uint64, script string, keys []string, extraArgs ...any) (sceneClaim, error) {
	token := uuid.NewString()
	if pending, ok := svcCtx.SceneClaimTokens.LoadAndDelete(sceneId); ok {
		token = pending.(string)
	}
	claim, err := evalSceneClaimWithToken(svcCtx, sceneId, token, script, keys, extraArgs...)
	if err != nil {
		svcCtx.SceneClaimTokens.Store(sceneId, token)
	}
	return claim, err
}

func evalSceneClaimWithToken(svcCtx *svc.ServiceContext, sceneId uint64, token, script string, keys []string, extraArgs ...any) (sceneClaim, error) {
	args := append([]any{strconv.FormatUint(sceneId, 10), token, sceneClaimRecordTTLSeconds}, extraArgs...)
	raw, err := svcCtx.Redis.Eval(script, keys, args...)
	if err != nil {
		return sceneClaim{}, err
	}
	return parseSceneClaimReply(raw), nil
}

// atomicRollbackInstanceAllocation 原子撤销一次失败的实例创建,见 luaRollbackInstanceAllocation。
//
// token 不进 SceneClaimTokens:这段脚本自己摘了计数和反向索引,销毁路径接手它的快照会再减一次。
// 回错时结局未知,但两种结局都能收敛:脚本没执行,实例还在 active 集合里,空闲回收按映射销毁并归还名额;
// 脚本执行了,其余状态已经干净,只有调用方手里的名额没还,由 Agones 对账告警发现。
func atomicRollbackInstanceAllocation(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, nodeId string, sourceSceneId uint64) (sceneClaim, error) {
	keys := append(sceneClaimKeys(zoneId, sceneId), nodeScenesKey(zoneId, nodeId), nodeSceneCountKey(zoneId, nodeId))
	if sourceSceneId > 0 {
		keys = append(keys, sceneMirrorsKey(sourceSceneId))
	}
	return evalSceneClaimWithToken(svcCtx, sceneId, uuid.NewString(), luaRollbackInstanceAllocation, keys, nodeId)
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
func atomicTeardownScene(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, expectedOwner string, unlistKeys ...string) (sceneClaim, error) {
	keys := append(sceneClaimKeys(zoneId, sceneId), unlistKeys...)
	ownerMode := ""
	if expectedOwner == "" {
		ownerMode = "absent"
	}
	return evalSceneClaim(svcCtx, sceneId, luaAtomicDestroyInstanceForce, keys, expectedOwner, ownerMode)
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
// Redis cleanup in destroyInstance. 场景里有人时回 Busy,除重置空闲时钟外什么都不动;
// 否则抹掉场景的全部键,回 Claimed,NodeID 是场景所在节点(映射已经不在时为空),
// AgonesGs 是它占用的 GameServer(非 Agones 模式下为空)。AgonesGs 和场景状态在同一个
// 脚本里被读出并删除,调用方拿它去精确归还 rooms 名额 —— 先删后读会永久丢失回滚依据。
func AtomicDestroyIfIdle(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64) (sceneClaim, error) {
	return evalSceneClaim(svcCtx, sceneId, luaAtomicDestroyInstance, sceneClaimKeys(zoneId, sceneId), time.Now().Unix())
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
