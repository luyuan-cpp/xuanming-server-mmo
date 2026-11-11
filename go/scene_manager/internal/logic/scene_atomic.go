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

// sceneClaimRecordTTLSeconds 是改派与名额转移回执(world_init.go、agones_binding.go)的存活时间,长过一次「连接中断 +
// 调用方下一拍重来」。销毁认领不用它:认领的快照记在拆除任务里,副作用做完才删(scene_teardown_job.go)。
const sceneClaimRecordTTLSeconds = 600

// 销毁认领脚本有三段:空闲回收用 luaAtomicDestroyInstance,死节点收尾用 luaAtomicDestroyOrphanOfDeadNode,
// 其余拆除一律用 luaAtomicDestroyInstanceForce。都由下面几块拼成,拆除任务的整体说明见 scene_teardown_job.go。
//
// KEYS(由 sceneTeardownRequest.keys 拼,顺序是脚本的契约):
//   - [1..9] sceneClaimKeys:场景的状态键、zone 的 active 集合、拆除任务 scene:{id}:teardown、本 zone 的待办集合;
//   - [10..11] 期望属主的场景集合与 scene_count,只在期望属主非空时传;
//   - 其后:要在同一步把场景摘掉的集合(世界频道的频道集合 / 排空集合);
//   - 最后:死节点收尾任务表,只在死节点收尾时传(个数见 ARGV[9],摘集合的循环跳过它)。
//
// ARGV:
//   - [1] sceneId(十进制,ZREM / SREM 的成员);[2] 本次认领的 token;[3] 当前 Unix 秒;
//   - [4] 拆除任务种类;[5] reason;[6] 期望属主,空串表示映射必须已不存在;[7] '1' 表示同一步扣属主的 scene_count;
//   - [8] 认领方的 zone(十进制),也就是 KEYS 里 active 集合、待办与属主键所属的 zone;
//   - [9] KEYS 末尾的死节点收尾任务表个数('0' 或 '1');[10] 收尾任务成员;[11] 收尾任务的入队时刻(Unix 毫秒)。
//     [10]、[11] 只在 [9] 为 '1' 时有意义(deadNodeTaskFence)。
//
// 应答由 parseSceneClaimReply 解:{状态, 属主, agones_gs, 镜像源, 镜像标记, 成员...}。状态是 'claimed'、
// 'stale'(期望落空,一个键都没动)、'pending'(这个场景的上一次拆除还没做完,一个键都没动)、'foreign'
// (场景登记在别的 zone,一个键都没动)、'withdrawn'(死节点收尾任务已被撤或换了一次判死,一个键都没动,只有死节点
// 收尾脚本会回)或 'busy'(场景里有人,只有空闲回收脚本会回)。

// luaSceneClaimPrelude 是认领脚本的开头,先看拆除任务:
//   - 同一 token 的任务在:脚本已经执行过,这次是 go-redis 读超时 / EOF 之后的原样重发(R5)。原样返回当时的快照,
//     一个键都不再动。否则重发看到映射已删,回 'stale',调用方以为别人销毁了而跳过全部副作用。
//   - 别的 token 的任务在:这个场景已被另一次认领拿走、副作用还没做完,回 'pending'、一个键都不动。同一个 scene_id
//     被重新登记时(号源出错)也挡在这里,旧任务做完之前新场景不会被认领。
const luaSceneClaimPrelude = `
local pendingToken = redis.call('HGET', KEYS[8], 'token')
if pendingToken then
    if pendingToken == ARGV[2] then
        return cjson.decode(redis.call('HGET', KEYS[8], 'reply'))
    end
    return {'pending', '', '', '', ''}
end
`

// luaSceneClaimZoneCheck 守 zone 归属(docs/design/control-plane-zone-scoping-20261105.md §4.3:zone z 的控制域键只由
// zone z 的 scene_manager 读写):场景登记的 zone(scene:{id}:zone,登记脚本写)不是认领方的 zone(ARGV[8])就回
// 'foreign'、一个键都不动。同组几个 zone 共用一个控制域实例,别的 zone 的场景键在这里读得到、也删得掉,删了却会按
// 本 zone 的 active 集合、待办和节点计数去记账。没有 zone 记录(登记之前的旧数据)时不拦。
// 放在认领的同一步里比,调用方事先的判断(级联先读子镜像的 zone)只是少一次无用的 EVAL,挡不住所有调用方。
const luaSceneClaimZoneCheck = `
local sceneZone = redis.call('GET', KEYS[6])
if sceneZone and sceneZone ~= ARGV[8] then
    return {'foreign', '', '', '', ''}
end
`

// luaSceneClaimOwnerCheck 比对期望属主(R3:认领以映射等于期望为前提)。ARGV[6] 非空时 scene:{id}:node 必须等于它,
// 空串时映射必须已不存在。后者只有两个调用方:空闲回收清扫 active 集合里映射已不在的残留,世界频道收尾在先读到
// 映射不存在之后拆残留(atomicTeardownScene)。不符就一个键都不动。
//
// 死节点收尾靠它(2026-10-22):收尾读到「属主是死节点」之后,只要有写者用 reassignSceneNode 把实例改派到活节点并
// CreateScene,不比对的话这里会把新属主的场景抹掉、向新节点发 DestroyScene、扣新节点的计数。比较必须和删除在同一个
// 脚本里,收尾自己的读挡不住这个窗口。所有调用方都带期望属主,属主的反向索引与 scene_count 因此能在同一步里改
// (键由调用方按期望属主拼好放进 KEYS[10..11],脚本不从数据拼键)。
const luaSceneClaimOwnerCheck = `
local nodeId = redis.call('GET', KEYS[1])
if ARGV[6] == '' then
    if nodeId then
        return {'stale', '', '', '', ''}
    end
    nodeId = ''
elseif nodeId ~= ARGV[6] then
    return {'stale', '', '', '', ''}
end
`

// luaSceneClaimDeadNodeTaskCheck 只在死节点收尾脚本里:KEYS 末尾的收尾任务表里,成员 ARGV[10] 的入队时刻必须仍是
// ARGV[11](deadNodeTaskFence),否则回 'withdrawn'、一个键都不动。任务不在了,是判死的那一代回来、任务被撤
// (luaWithdrawDeadNodeForReturningInstance);入队时刻变了,是撤销之后同一 node_id 又判死了一次,新任务有自己的
// 屏障,这一轮按旧快照拆不得(R1-I6,reentry_barrier.go)。
//
// 必须和删除在同一个脚本里:收尾只在每条任务开始时判一次「判死的那一代回来没有」,之后逐个认领场景的这段时间里它回来,
// 只有这里拦得住。放在属主比对之前:回来的就是这个 node_id,属主比对拦不住;先回 'withdrawn',调用方才知道停掉整轮。
const luaSceneClaimDeadNodeTaskCheck = `
local deadNodeTaskQueuedAt = redis.call('ZSCORE', KEYS[#KEYS], ARGV[10])
if not deadNodeTaskQueuedAt or tonumber(deadNodeTaskQueuedAt) ~= tonumber(ARGV[11]) then
    return {'withdrawn', '', '', '', ''}
end
`

// luaSceneClaimIdleGuard 只在空闲回收脚本里:场景里有人就把空闲时钟重置成 ARGV[3]、一个键都不删,回 'busy'。
// 重置放在脚本里并用 ZADD XX:并发销毁已经把实例摘出 active 集合时不会把它加回去,那会留下永久僵尸。
const luaSceneClaimIdleGuard = `
if redis.call('SCARD', KEYS[2]) > 0 then
    redis.call('ZADD', KEYS[3], 'XX', ARGV[3], ARGV[1])
    return {'busy', '', '', '', ''}
end
`

// luaSceneClaimCommit 提交认领:读快照、删场景的全部状态键、改属主的反向索引与计数、摘集合、写拆除任务,
// 全在同一步里。读都在写之前:脚本中途报错时 Redis 不回滚已经执行的写。
//
// scene:{id}:agones_gs 必须和其余场景状态一起删,值要在删之前读出来:不一起删会留下悬空映射,下次同 id 复用时把计数
// 减到别人头上;不读出来就再也不知道该向哪个 GameServer 归还名额。镜像源与镜像标记同理:事后再读只会读到空,
// 镜像不从源的子集合里摘掉,源频道的 channelHasMirrors 恒真、永远缩不了容。
//
// scene_count 只在 > 0 时才减:计数键可能已被死节点收尾整个删掉,DECR 会把它重建成 -1,被复用同一 node_id 的
// 新进程继承(同 luaReassignSceneNode 对老节点的处理)。
//
// 摘集合的循环止于 KEYS 末尾的死节点收尾任务表之前(ARGV[9] 个):那是只读的核对键,类型是有序集合,SREM 会报
// WRONGTYPE,而那时前面的删除已经执行、不会回滚。
const luaSceneClaimCommit = `
local agonesGs = redis.call('GET', KEYS[7]) or ''
local source = redis.call('GET', KEYS[5]) or ''
local mirror = redis.call('GET', KEYS[4]) or ''
local members = redis.call('SMEMBERS', KEYS[2])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[4], KEYS[5], KEYS[6], KEYS[7])
redis.call('ZREM', KEYS[3], ARGV[1])
local unlistFrom = 10
if nodeId ~= '' then
    redis.call('SREM', KEYS[10], ARGV[1])
    if ARGV[7] == '1' then
        local count = tonumber(redis.call('GET', KEYS[11]) or '0') or 0
        if count > 0 then
            redis.call('DECR', KEYS[11])
        end
    end
    unlistFrom = 12
end
local unlistTo = #KEYS - (tonumber(ARGV[9] or '0') or 0)
for i = unlistFrom, unlistTo do
    redis.call('SREM', KEYS[i], ARGV[1])
end
local reply = {'claimed', nodeId, agonesGs, source, mirror}
for i = 1, #members do
    reply[i + 5] = members[i]
end
redis.call('HSET', KEYS[8], 'token', ARGV[2], 'kind', ARGV[4], 'reason', ARGV[5], 'claimed_at', ARGV[3],
    'reply', cjson.encode(reply))
redis.call('ZADD', KEYS[9], ARGV[3], ARGV[1])
return reply
`

// luaAtomicDestroyInstance 是空闲回收的认领脚本:有人就不拆、只重置空闲时钟,没人才拆。
const luaAtomicDestroyInstance = luaSceneClaimPrelude + luaSceneClaimZoneCheck + luaSceneClaimOwnerCheck +
	luaSceneClaimIdleGuard + luaSceneClaimCommit

// luaAtomicDestroyInstanceForce 是其余拆除的认领脚本,不看人数,只看期望属主。调用方:显式 DestroyScene、镜像级联、
// 源迁移 / 缩容时的镜像级联(都经 AtomicDestroyForce),世界频道的排空收尾与孤儿清理(atomicTeardownScene),
// 建实例失败的回滚(atomicRollbackInstanceAllocation)。
const luaAtomicDestroyInstanceForce = luaSceneClaimPrelude + luaSceneClaimZoneCheck + luaSceneClaimOwnerCheck + luaSceneClaimCommit

// luaAtomicDestroyOrphanOfDeadNode 是死节点收尾的认领脚本(atomicClaimOrphanOfDeadNode):不看人数,先核收尾任务仍是
// 这一次判死,再核属主仍是死节点。
const luaAtomicDestroyOrphanOfDeadNode = luaSceneClaimPrelude + luaSceneClaimZoneCheck + luaSceneClaimDeadNodeTaskCheck +
	luaSceneClaimOwnerCheck + luaSceneClaimCommit

// errSceneInOtherZone 表示要拆的场景登记在别的 zone:它的键只由那个 zone 的 scene_manager 读写,这里一个键都没动。
var errSceneInOtherZone = errors.New("scene is registered in another zone")

// sceneClaimKeys 是认领脚本的 KEYS[1..9],见 luaSceneClaimPrelude 之前的说明。
func sceneClaimKeys(zoneId uint32, sceneId uint64) []string {
	return []string{
		sceneNodeKey(sceneId),
		instanceMembersKey(sceneId),
		activeInstancesKey(zoneId),
		sceneMirrorFlagKey(sceneId),
		sceneSourceKey(sceneId),
		sceneZoneKey(sceneId),
		sceneAgonesGsKey(sceneId),
		sceneTeardownJobKey(sceneId),
		sceneTeardownPendingKey(zoneId),
	}
}

// sceneTeardownRequest 是一次拆除认领的参数。
type sceneTeardownRequest struct {
	ZoneID  uint32
	SceneID uint64
	// Kind 是拆除任务的种类(teardownKind*),决定认领之后做哪些副作用。
	Kind string
	// Reason 是 Agones 归还与 instance_destroyed_total 的 reason。
	Reason string
	// ExpectedOwner 是认领时 scene:{id}:node 必须等于的值;空串表示映射必须已不存在(清扫残留)。
	ExpectedOwner string
	// CountOwner 为 true 时在同一步扣期望属主的 scene_count(只在 > 0 时减)。
	CountOwner bool
	// UnlistKeys 是要在同一步把场景摘掉的集合。
	UnlistKeys []string
	// DeadNodeTask 非空时(只有死节点收尾)认领在同一步核对这条收尾任务仍是这一次判死(luaSceneClaimDeadNodeTaskCheck)。
	// 只配 luaAtomicDestroyOrphanOfDeadNode 使用。
	DeadNodeTask *deadNodeTaskFence
}

// keys 按 luaSceneClaimPrelude 之前写的布局拼出脚本的 KEYS。
func (r sceneTeardownRequest) keys() []string {
	keys := sceneClaimKeys(r.ZoneID, r.SceneID)
	if r.ExpectedOwner != "" {
		keys = append(keys, nodeScenesKey(r.ZoneID, r.ExpectedOwner), nodeSceneCountKey(r.ZoneID, r.ExpectedOwner))
	}
	keys = append(keys, r.UnlistKeys...)
	if r.DeadNodeTask != nil {
		keys = append(keys, r.DeadNodeTask.tasksKey)
	}
	return keys
}

// fenceArgs 是 ARGV[9..11]:KEYS 末尾的死节点收尾任务表个数、任务成员与入队时刻。不核对任务时是 '0' 与两个空串。
func (r sceneTeardownRequest) fenceArgs() []any {
	if r.DeadNodeTask == nil {
		return []any{"0", "", ""}
	}
	return []any{"1", r.DeadNodeTask.member, strconv.FormatInt(r.DeadNodeTask.queuedAtMs, 10)}
}

// sceneClaim 是认领脚本的应答。Claimed=false 时其余字段都为空:期望落空,场景里有人(Busy),这个场景的上一次拆除
// 还没做完(Pending,任务做完之前谁都认领不到),场景登记在别的 zone(Foreign),或死节点收尾任务已被撤 / 换了一次
// 判死(Withdrawn,只有 luaAtomicDestroyOrphanOfDeadNode 会回)。
// Claimed=true 时其余字段是脚本删除前读到的值(属主为空串 = 映射本来就不在),认领之后的副作用一律以它们为准。
// Source 是镜像源 scene_id 的十进制(不是镜像时为空),MirrorFlag 是 scene:{id}:mirror 的值("1" 表示镜像)。
// Members 是场景里剩的成员,要由副作用从节点集合里摘掉(removeResidualSceneMembers)。
// Token 是这次认领的 token,也是拆除任务的身份,只在 Claimed 时有值。
type sceneClaim struct {
	Claimed    bool
	Busy       bool
	Pending    bool
	Foreign    bool
	Withdrawn  bool
	NodeID     string
	AgonesGs   string
	Source     string
	MirrorFlag string
	Members    []string
	Token      string
}

// parseSceneClaimReply 解认领脚本的应答,格式见 luaSceneClaimPrelude 之前的说明。raw 是 Eval 的返回值,
// 或拆除任务里存的快照解出来的 []string(loadSceneTeardownJob)。
func parseSceneClaimReply(raw any) sceneClaim {
	parts := toSlice(raw)
	if len(parts) < 5 {
		return sceneClaim{}
	}
	switch toString(parts[0]) {
	case "busy":
		return sceneClaim{Busy: true}
	case "pending":
		return sceneClaim{Pending: true}
	case "foreign":
		return sceneClaim{Foreign: true}
	case "withdrawn":
		return sceneClaim{Withdrawn: true}
	case "claimed":
		members := make([]string, 0, len(parts)-5)
		for _, member := range parts[5:] {
			members = append(members, toString(member))
		}
		return sceneClaim{
			Claimed:    true,
			NodeID:     toString(parts[1]),
			AgonesGs:   toString(parts[2]),
			Source:     toString(parts[3]),
			MirrorFlag: toString(parts[4]),
			Members:    members,
		}
	}
	return sceneClaim{}
}

// evalSceneClaim 用一个新 token 跑一段销毁认领脚本。
//
// go-redis 的内部重发带的是同一组参数,脚本按 token 认出自己、原样返回快照(luaSceneClaimPrelude)。重试也用完、
// Eval 回了错时结局未知:脚本若已执行,拆除任务已经在待办里,由扫描在租期之后做完副作用(scene_teardown_job.go),
// 调用方按「这次没认领到」处理即可,不用记住 token 去接手。
func evalSceneClaim(svcCtx *svc.ServiceContext, script string, req sceneTeardownRequest) (sceneClaim, error) {
	return evalSceneClaimWithToken(svcCtx, script, req, uuid.NewString())
}

func evalSceneClaimWithToken(svcCtx *svc.ServiceContext, script string, req sceneTeardownRequest, token string) (sceneClaim, error) {
	countOwner := "0"
	if req.CountOwner {
		countOwner = "1"
	}
	args := append([]any{
		strconv.FormatUint(req.SceneID, 10), token, time.Now().Unix(), req.Kind, req.Reason, req.ExpectedOwner, countOwner,
		strconv.FormatUint(uint64(req.ZoneID), 10),
	}, req.fenceArgs()...)
	raw, err := svcCtx.Redis.Eval(script, req.keys(), args...)
	if err != nil {
		return sceneClaim{}, err
	}
	claim := parseSceneClaimReply(raw)
	if claim.Claimed {
		claim.Token = token
	}
	return claim, nil
}

// sceneClaimOwnerAttempts 是「谁是属主都拆」的认领最多读几次属主,见 AtomicDestroyForce。
const sceneClaimOwnerAttempts = 3

// AtomicDestroyForce 原子认领一个副本 / 镜像的拆除(不看人数),认领时在同一步扣属主的 scene_count、摘属主的
// 反向索引,并写下拆除任务。reason 是 explicit / cascade / node_death / source_migrated / source_scaled_in。
//
// expectedOwner 非空时只在 scene:{id}:node 仍等于它时才拆;不等(含映射已不存在)一个键都不动。死节点收尾不走这里,
// 它还要核对收尾任务,见 atomicClaimOrphanOfDeadNode。
// 空串表示谁是属主都拆:先读属主、按读到的值认领;读完之后被改派就重读,至多读 sceneClaimOwnerAttempts 次。
// 映射已不存在时不认领 —— 映射没了、别的键还在的残留只由空闲回收清扫(R3)。上一次拆除还没做完(Pending)、
// 场景登记在别的 zone(Foreign)时也不重读,原样返回。
//
// 是否认领只看 Claimed:未认领时调用方必须跳过一切副作用。
func AtomicDestroyForce(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, expectedOwner, reason string) (sceneClaim, error) {
	req := sceneTeardownRequest{ZoneID: zoneId, SceneID: sceneId, Kind: teardownKindInstance, Reason: reason, CountOwner: true}
	if expectedOwner != "" {
		req.ExpectedOwner = expectedOwner
		return evalSceneClaim(svcCtx, luaAtomicDestroyInstanceForce, req)
	}
	for attempt := 0; attempt < sceneClaimOwnerAttempts; attempt++ {
		owner, err := svcCtx.Redis.Get(sceneNodeKey(sceneId))
		if err != nil {
			return sceneClaim{}, err
		}
		if owner == "" {
			return sceneClaim{}, nil
		}
		req.ExpectedOwner = owner
		claim, err := evalSceneClaim(svcCtx, luaAtomicDestroyInstanceForce, req)
		if err != nil || claim.Claimed || claim.Pending || claim.Foreign {
			return claim, err
		}
	}
	return sceneClaim{}, fmt.Errorf("scene %d: owner changed between the read and the claim %d times in a row", sceneId, sceneClaimOwnerAttempts)
}

// atomicClaimOrphanOfDeadNode 原子认领死节点收尾的一个场景(reason=node_death,不看人数):同一步里先核对收尾任务
// task 仍是这一次判死(否则 Withdrawn,R1-I6),再核对属主仍是 deadNode(否则落空),都对才删场景、扣死节点的
// scene_count、摘它的反向索引并写拆除任务。三种落空都一个键不动。
func atomicClaimOrphanOfDeadNode(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, deadNode string, task deadNodeTaskFence) (sceneClaim, error) {
	return evalSceneClaim(svcCtx, luaAtomicDestroyOrphanOfDeadNode, sceneTeardownRequest{
		ZoneID: zoneId, SceneID: sceneId, Kind: teardownKindInstance, Reason: "node_death",
		ExpectedOwner: deadNode, CountOwner: true, DeadNodeTask: &task,
	})
}

// atomicRollbackInstanceAllocation 原子撤销一次失败的实例创建(R3:原来逐条 DEL、不查错,中途中断会留下「映射没了、
// 其它键还在」的残留)。只有映射仍指向这次分配的节点才认领,否则一个键都不动:期间被销毁或改派了,收尾归那条路径。
// 节点反向索引与 scene_count 在同一步里改。镜像源的子集合按快照里的镜像源摘(runRollbackTeardown),不按请求里的
// SourceSceneId:源在别的 zone 时登记不写 scene:{id}:source,也不进源的子集合,快照与登记天然一致。
func atomicRollbackInstanceAllocation(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64, nodeId string) (sceneClaim, error) {
	return evalSceneClaim(svcCtx, luaAtomicDestroyInstanceForce, sceneTeardownRequest{
		ZoneID: zoneId, SceneID: sceneId, Kind: teardownKindRollback, Reason: "create_rpc_failed",
		ExpectedOwner: nodeId, CountOwner: true,
	})
}

// atomicTeardownScene 原子认领一个世界频道的拆除,给按「先读后拆」两步走的收尾路径用:排空收尾、孤儿清理。
//   - req.ExpectedOwner 是先前读到的属主。空串表示先前读到映射不存在、现在必须仍不存在:两步之间若有写者(懒改派等)
//     给它写了属主,一个键都不动。否则会把新属主的映射抹掉,而调用方按空属主做副作用,新节点上的实体与计数都没人收。
//   - req.UnlistKeys 是频道集合 / 排空集合:集合摘除与删映射同一步,不会出现「集合里还列着、映射已删」的中间态 ——
//     那个状态会被任意副本上的懒改派当成陈旧频道复活。
func atomicTeardownScene(svcCtx *svc.ServiceContext, req sceneTeardownRequest) (sceneClaim, error) {
	return evalSceneClaim(svcCtx, luaAtomicDestroyInstanceForce, req)
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

// ClaimIdleSceneTeardown 是空闲回收的认领。场景里有人时回 Busy,除重置空闲时钟外什么都不动;否则删除场景映射、
// 写下拆除任务并回 Claimed,同一步扣属主的 scene_count。节点实体销毁等副作用由后续拆除任务推进。
//
// 属主先读一次,按读到的值认领。映射已经不在时按「必须仍不在」认领,收掉 active 集合里的残留:NodeID 为空,
// agones_gs 还在就由副作用归还,不当成有人进场再加回去。属主在读与认领之间变了就回未认领,下一拍重判。
func ClaimIdleSceneTeardown(svcCtx *svc.ServiceContext, zoneId uint32, sceneId uint64) (sceneClaim, error) {
	owner, err := svcCtx.Redis.Get(sceneNodeKey(sceneId))
	if err != nil {
		return sceneClaim{}, err
	}
	return evalSceneClaim(svcCtx, luaAtomicDestroyInstance, sceneTeardownRequest{
		ZoneID: zoneId, SceneID: sceneId, Kind: teardownKindInstance, Reason: "idle",
		ExpectedOwner: owner, CountOwner: true,
	})
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
