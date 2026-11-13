package logic

import (
	"fmt"
	"strconv"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

// 新场景的登记(缺口清单第 1 条残余)。
//
// 原来一个新场景的登记是十来条独立命令:映射、节点计数、节点场景集合、agones_gs、active 集合或频道集合、
// 镜像标记和镜像源索引。进程在中途崩溃会留下「映射在、却不在 active 集合 / 频道集合里」的场景:空闲回收和
// 频道逻辑都找不到它,节点计数多加了一次,Agones 名额永远不还。现在这些写入在一段脚本里完成,要么全有、要么全无。
//
// 登记带一次性 token,记在自己的记录键 scene:{ctl:<z>}:<id>:registration(TTL sceneRegistrationRecordTTLSeconds),
// 不和销毁认领的记录共用:两边的 token 互不相干,共用一把键就会互相覆盖。
//   - go-redis 重发同一段 EVAL 时,记录里是同一个 token,不再写任何键,计数不会加两次;
//   - EVAL 最终回错、结局未知时,用同一个 token 跑 luaAbortSceneRegistration 问清楚:登记已经落地就照常往下走;
//     还没落地就在记录里写下「已作废」,之后才到的那次登记看到它什么都不写,调用方这时才能放心归还 Agones 名额。
//     只读一下映射在不在判断不了:原来那次 EVAL 可能还在路上,读完之后才执行。
//   - 同一 token 重放「已登记」之前先核对映射仍指向这次的节点:登记落地之后、重发或作废查询到达之前,场景可能已被
//     销毁或改派(死节点收尾等)。这时回 conflict,调用方不能再向节点发 CreateScene,否则节点上留下一个没有 Redis
//     状态的实体。
//
// 新铸的 scene_id 不该有任何记录或映射。见到别的 token 的记录或已有映射,一个键都不写,回 conflict:
// 覆盖的话两个场景共用一个 id,路由互相覆盖,玩家被静默送错节点。conflict 有两种来历,调用方分不清:
//   - 号被重复使用:这次什么都没写;
//   - 这次登记其实已经落地,随后场景被销毁或改派:名额由销毁方按记录删过,或随场景转走了。
// 两种来历下调用方都可以放心删自己这次占的条目:条目每次占用各不相同(agones_room_ledger.go),删的只是自己那一条,
// 销毁方删同一条是幂等的。2026-11-10 之前名额是一个计数,分不清删的是谁的,conflict 怕双还、一律不还。
//
// Agones 名额的在途占用(agones_room_ledger.go,2026-11-10):占用了名额的登记在写任何键之前先 ZREM 自己的在途占用。
// 删不到说明这次占用拖过了截止时刻、已被对账取消(条目会被当成孤儿删掉),这时一个键都不写,回 fenced:登记了的话
// 记录会指向一条已经被删的条目,这个场景就不占名额,节点会被超卖。

// sceneRegistrationRecordTTLSeconds 是登记记录的存活时间。只要长过 go-redis 的重发加一次作废查询;
// 记录过期之后,同一 token 的登记才到会按新登记处理(映射已在时照样回 conflict)。
const sceneRegistrationRecordTTLSeconds = 600

// sceneRegistrationRecordKey 是登记的一次性记录(token 与结局),见文件头。
func sceneRegistrationRecordKey(zoneID uint32, sceneID uint64) string {
	return fmt.Sprintf("scene:%s:%d:registration", sceneControlTag(zoneID), sceneID)
}

// luaSceneRegistrationPrelude 是两段登记脚本共用的开头:查记录、映射与拆除任务,消费 Agones 名额的在途占用,
// 写一个场景都有的四样。
//
// KEYS[1] = scene:{ctl:<z>}:<id>:node,KEYS[2] = 节点 scene_count,KEYS[3] = 节点场景集合,
// KEYS[4] = scene:{ctl:<z>}:<id>:agones_gs,KEYS[5] = scene:{ctl:<z>}:<id>:registration,KEYS[6] = scene:{ctl:<z>}:<id>:teardown,
// KEYS[7] = 本 zone 的 Agones 在途占用 agones_rooms:{ctl:<z>}:holds。全部 KEYS 同一个 zone 的 tag,同槽。
// ARGV[1] = sceneId,ARGV[2] = token,ARGV[3] = 记录存活秒数,ARGV[4] = 节点,
// ARGV[5] = 名额记录「GameServer/条目」(空串表示不占名额),ARGV[6] = 名额条目(在途占用的成员;空串表示没有在途占用)。
// 场景所在的 zone 就是键的 tag,不再另写 scene:<id>:zone(docs/design/redis-cluster-all-domains-20261110.md §3.3)。
//
// 拆除任务还在(scene_teardown_job.go,上一个用这个号的场景没拆完)同样回 conflict:登记了的话,旧任务补发的
// DestroyScene 会打到新场景上。号源不重号时不会发生,是纵深防御。
// 在途占用删不到回 fenced,见文件头。fenced 不写登记记录:重发的那次照样删不到,结局相同。
const luaSceneRegistrationPrelude = `
local prior = redis.call('GET', KEYS[5])
if prior then
    local record = cjson.decode(prior)
    if record.token ~= ARGV[2] then
        return {'conflict'}
    end
    if record.reply[1] == 'registered' and redis.call('GET', KEYS[1]) ~= ARGV[4] then
        return {'conflict'}
    end
    return record.reply
end
if redis.call('EXISTS', KEYS[1], KEYS[6]) > 0 then
    return {'conflict'}
end
if ARGV[6] ~= '' and redis.call('ZREM', KEYS[7], ARGV[6]) == 0 then
    return {'fenced'}
end
redis.call('SET', KEYS[1], ARGV[4])
redis.call('INCR', KEYS[2])
redis.call('SADD', KEYS[3], ARGV[1])
if ARGV[5] ~= '' then
    redis.call('SET', KEYS[4], ARGV[5])
end
`

// luaSceneRegistrationRecord 是两段登记脚本共用的结尾:写下记录,重发和作废查询都靠它。
const luaSceneRegistrationRecord = `
local reply = {'registered'}
redis.call('SET', KEYS[5], cjson.encode({token = ARGV[2], reply = reply}), 'EX', ARGV[3])
return reply
`

// luaRegisterInstance 登记一个按需实例。KEYS[1..7]、ARGV[1..6] 见 luaSceneRegistrationPrelude。
//
// KEYS[8] = zone 的 active instances 有序集合,KEYS[9] = scene:{ctl:<z>}:<id>:mirror,KEYS[10] = scene:{ctl:<z>}:<id>:source,
// KEYS[11] = 镜像源的子集合 scene:{ctl:<z>}:<源>:mirrors(没有镜像源时不传)。
// ARGV[7] = 创建时刻 Unix 秒(空闲时钟起点),ARGV[8] = '1' 表示镜像,ARGV[9] = 镜像源 scene_id。
const luaRegisterInstance = luaSceneRegistrationPrelude + `
redis.call('ZADD', KEYS[8], ARGV[7], ARGV[1])
if ARGV[8] == '1' then
    redis.call('SET', KEYS[9], '1')
end
if KEYS[11] then
    redis.call('SET', KEYS[10], ARGV[9])
    redis.call('SADD', KEYS[11], ARGV[1])
end
` + luaSceneRegistrationRecord

// luaRegisterWorldChannel 登记一个世界频道。KEYS[1..7]、ARGV[1..6] 见 luaSceneRegistrationPrelude。
//
// KEYS[8] = 这个地图的频道集合。
const luaRegisterWorldChannel = luaSceneRegistrationPrelude + `
redis.call('SADD', KEYS[8], ARGV[1])
` + luaSceneRegistrationRecord

// luaAbortSceneRegistration 在登记结局未知时问清楚,并保证它之后不会再落地。
//
// KEYS[1] = scene:{ctl:<z>}:<id>:registration,KEYS[2] = scene:{ctl:<z>}:<id>:node。ARGV[1] = 登记用的 token,ARGV[2] = 记录存活秒数,
// ARGV[3] = 登记的节点。
// 记录是这个 token 的:返回登记当时的结果('registered',映射已不指向这个节点时回 'conflict',同登记脚本),
// 或上一次作废的结果。
// 没有记录:登记没有执行过,写下作废记录,回 'aborted';之后才到的登记读到同一个 token 的作废记录,什么都不写。
// 记录是别的 token 的:回 'conflict',同登记脚本。
const luaAbortSceneRegistration = `
local prior = redis.call('GET', KEYS[1])
if prior then
    local record = cjson.decode(prior)
    if record.token ~= ARGV[1] then
        return {'conflict'}
    end
    if record.reply[1] == 'registered' and redis.call('GET', KEYS[2]) ~= ARGV[3] then
        return {'conflict'}
    end
    return record.reply
end
local reply = {'aborted'}
redis.call('SET', KEYS[1], cjson.encode({token = ARGV[1], reply = reply}), 'EX', ARGV[2])
return reply
`

// registrationOutcome 是一次登记的结局。
type registrationOutcome int

const (
	// registrationRegistered:场景的全部登记写入已经落地。
	registrationRegistered registrationOutcome = iota
	// registrationConflict:scene_id 已被占用,或这次登记落地后场景已被销毁或改派,这一次一个键都没写。见文件头。
	registrationConflict
	// registrationAborted:登记没有落地,而且以后也不会落地。
	registrationAborted
	// registrationUnknown:Redis 出错,不知道登记有没有落地。
	registrationUnknown
	// registrationFenced:这次登记占的 Agones 名额的在途占用已被对账取消(拖过了截止时刻),一个键都没写。见文件头。
	registrationFenced
)

func (o registrationOutcome) String() string {
	switch o {
	case registrationRegistered:
		return "registered"
	case registrationConflict:
		return "conflict"
	case registrationAborted:
		return "aborted"
	case registrationFenced:
		return "fenced"
	default:
		return "unknown"
	}
}

// releasesReservation 说明登记失败时调用方能不能放弃为它预占的 Agones 名额(abandonAgonesPlacement)。
// aborted、conflict、fenced 都能:记录没指向这次占的条目(conflict 时即使登记落过地,名额也已由销毁方删过或随场景
// 转走),删的只是自己这一条,删两次无害。unknown 不能:登记可能已经落地,记录指向这一条,删了就少记、会超卖;
// 这时条目与在途占用都留着,在途占用过期之后对账按记录判(agones_reconcile.go)。
func (o registrationOutcome) releasesReservation() bool {
	return o == registrationAborted || o == registrationConflict || o == registrationFenced
}

// sceneRegistration 是每个新场景都要登记的内容。
type sceneRegistration struct {
	SceneID uint64
	ZoneID  uint32
	NodeID  string
	// AgonesRoom 是场景占的 Agones 名额(agonesRoom.record() 写进 scene:{ctl:<z>}:<id>:agones_gs,Entry 是要消费的在途占用);
	// 不占名额时为空。
	AgonesRoom agonesRoom
}

// instanceRegistration 是按需实例在 sceneRegistration 之外要登记的内容。
type instanceRegistration struct {
	sceneRegistration
	CreatedAt     int64 // active 集合的分数,空闲时钟从这里开始
	Mirror        bool
	SourceSceneID uint64 // 镜像源;没有时为 0
}

// registerInstance 原子登记一个按需实例,见文件头。
func registerInstance(svcCtx *svc.ServiceContext, reg instanceRegistration) registrationOutcome {
	keys, extraArgs := instanceRegistrationParams(reg)
	return runSceneRegistration(svcCtx, metrics.SceneRegistrationKindInstance, reg.sceneRegistration, luaRegisterInstance, keys, extraArgs...)
}

// instanceRegistrationParams 是 luaRegisterInstance 的 KEYS 与 ARGV[7..9]。
func instanceRegistrationParams(reg instanceRegistration) ([]string, []any) {
	keys := append(sceneRegistrationKeys(reg.sceneRegistration),
		activeInstancesKey(reg.ZoneID), sceneMirrorFlagKey(reg.ZoneID, reg.SceneID), sceneSourceKey(reg.ZoneID, reg.SceneID))
	if reg.SourceSceneID > 0 {
		keys = append(keys, sceneMirrorsKey(reg.ZoneID, reg.SourceSceneID))
	}
	mirror := "0"
	if reg.Mirror {
		mirror = "1"
	}
	return keys, []any{reg.CreatedAt, mirror, strconv.FormatUint(reg.SourceSceneID, 10)}
}

// registerWorldChannel 原子登记一个世界频道,见文件头。
func registerWorldChannel(svcCtx *svc.ServiceContext, reg sceneRegistration, confID uint64) registrationOutcome {
	keys := append(sceneRegistrationKeys(reg), worldChannelsKey(reg.ZoneID, confID))
	return runSceneRegistration(svcCtx, metrics.SceneRegistrationKindWorldChannel, reg, luaRegisterWorldChannel, keys)
}

// sceneRegistrationKeys 是登记脚本的 KEYS[1..7],顺序是脚本的契约。
func sceneRegistrationKeys(reg sceneRegistration) []string {
	return []string{
		sceneNodeKey(reg.ZoneID, reg.SceneID),
		nodeSceneCountKey(reg.ZoneID, reg.NodeID),
		nodeScenesKey(reg.ZoneID, reg.NodeID),
		sceneAgonesGsKey(reg.ZoneID, reg.SceneID),
		sceneRegistrationRecordKey(reg.ZoneID, reg.SceneID),
		sceneTeardownJobKey(reg.ZoneID, reg.SceneID),
		agonesRoomHoldsKey(reg.ZoneID),
	}
}

// runSceneRegistration 跑一段登记脚本;EVAL 回错时用同一个 token 作废,把结局问清楚。
func runSceneRegistration(svcCtx *svc.ServiceContext, kind string, reg sceneRegistration, script string, keys []string, extraArgs ...any) registrationOutcome {
	token := uuid.NewString()
	raw, err := evalSceneRegistration(svcCtx, script, keys, reg, token, extraArgs...)
	if err != nil {
		logx.Errorf("[SceneRegistration] %s %d on node %s: registration outcome unknown, aborting with the same token: %v",
			kind, reg.SceneID, reg.NodeID, err)
		raw, err = evalAbortSceneRegistration(svcCtx, reg, token)
	}
	outcome := registrationUnknown
	if err != nil {
		logx.Errorf("[SceneRegistration] %s %d on node %s: abort failed too, outcome stays unknown; keeping the Agones room %q "+
			"and its hold, the reconciler keeps it if the registration landed and reclaims it otherwise: %v",
			kind, reg.SceneID, reg.NodeID, reg.AgonesRoom.record(), err)
	} else {
		outcome = parseRegistrationReply(raw)
	}
	switch outcome {
	case registrationConflict:
		logx.Errorf("[SceneRegistration] %s %d on node %s: scene id already in use, or the scene was destroyed or moved "+
			"right after this registration landed; nothing written this time", kind, reg.SceneID, reg.NodeID)
	case registrationFenced:
		logx.Errorf("[SceneRegistration] %s %d on node %s: the hold of Agones room %q expired before registering "+
			"(the reconciler reclaims the room); nothing written", kind, reg.SceneID, reg.NodeID, reg.AgonesRoom.record())
	}
	metrics.ObserveSceneRegistration(reg.ZoneID, kind, outcome.String())
	return outcome
}

// evalSceneRegistration 用给定的 token 跑一次登记脚本。keys 与 extraArgs(ARGV[7..])按脚本各自的契约给。
func evalSceneRegistration(svcCtx *svc.ServiceContext, script string, keys []string, reg sceneRegistration, token string, extraArgs ...any) (any, error) {
	args := append([]any{
		strconv.FormatUint(reg.SceneID, 10), token, sceneRegistrationRecordTTLSeconds,
		reg.NodeID, reg.AgonesRoom.record(), reg.AgonesRoom.Entry,
	}, extraArgs...)
	return svcCtx.Redis.Eval(script, keys, args...)
}

// evalAbortSceneRegistration 用登记时的 token 跑作废脚本,见 luaAbortSceneRegistration。
func evalAbortSceneRegistration(svcCtx *svc.ServiceContext, reg sceneRegistration, token string) (any, error) {
	return svcCtx.Redis.Eval(luaAbortSceneRegistration,
		[]string{sceneRegistrationRecordKey(reg.ZoneID, reg.SceneID), sceneNodeKey(reg.ZoneID, reg.SceneID)},
		token, sceneRegistrationRecordTTLSeconds, reg.NodeID)
}

// parseRegistrationReply 解登记脚本和作废脚本的应答。认不出的应答按结局未知处理。
func parseRegistrationReply(raw any) registrationOutcome {
	parts := toSlice(raw)
	if len(parts) == 0 {
		logx.Errorf("[SceneRegistration] unexpected script reply %v", raw)
		return registrationUnknown
	}
	switch toString(parts[0]) {
	case "registered":
		return registrationRegistered
	case "conflict":
		return registrationConflict
	case "aborted":
		return registrationAborted
	case "fenced":
		return registrationFenced
	}
	logx.Errorf("[SceneRegistration] unexpected script status %v", parts[0])
	return registrationUnknown
}
