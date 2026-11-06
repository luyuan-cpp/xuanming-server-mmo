package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/safego"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

// 场景拆除任务:认领之后的副作用不会因为进程退出、回包丢失而丢(docs/design/scene-creation-architecture.md
// 「拆除任务」)。
//
// 认领脚本(scene_atomic.go)删掉场景的状态键、在同一步改好属主的反向索引与 scene_count 之后,还有一串副作用
// 要在脚本外做:镜像级联、C++ DestroyScene、从镜像源的子集合摘掉自己、摘剩余成员、还 Agones 名额。以前它们只在
// 认领方的内存里,认领方在这之间退出(崩溃,或滚动发布把空闲回收的 ctx 取消)、或者认领的回包丢了,就永远没人做:
// 节点上留下没有映射的实体,Agones 的 rooms 多占一份,源频道的子集合挂着死 id。
//
// 做法是事务性发件箱:
//   - 认领脚本在提交的同一步写任务 scene:{id}:teardown(hash,不带 TTL),并把场景放进本 zone 的待办有序集合
//     scene_teardown:zone:{z}:pending,分数是认领时刻。认领提交了,任务就一定在;任务在,别的认领一律落空。
//   - 认领方当场按任务做副作用(completeSceneTeardown),全部做完才删任务。
//   - 没做完的(进程退出、某一步失败、Eval 回错让调用方以为没认领到)留在待办里。本 zone 的领导者每
//     sceneTeardownSweepInterval 扫一次,认领超过 sceneTeardownJobLease 的任务按同一份做法重做。
//   - 副作用都能重跑:级联、DestroyScene(C++ 找不到实体只打日志)、SREM、DEL、摘成员本来就幂等。
//     不幂等的只有 Agones 归还,由 takeTeardownRoom 在任务上原子置位,一个任务至多还一次。
//   - Agones 是「至多一次」:先置位再调 K8s。置位之后、调用之前进程退出,这份名额不还;这时 scene_count 已经在认领时
//     减了,两数对不上,Agones 对账(agones_reconcile.go)会告警。反过来「至少一次」会在置位丢失时还两次,把容量算少、
//     让 FleetAutoscaler 超卖。与 TransferAgonesRoomForScene 结局未知时「两边都不还」是同一取舍。
//
// 任务做完之前,别的认领对这个场景一律落空,同一个 scene_id 被重新登记时(号源出错)也认领不到,直到旧任务做完。

// 拆除任务的种类,决定认领之后做哪些副作用。值会写进 Redis,改名要兼顾还没做完的旧任务。
const (
	// teardownKindInstance 是副本与镜像的销毁(destroyInstanceInternal:空闲回收、显式 DestroyScene、镜像级联、
	// 死节点收尾、源迁移 / 缩容时的镜像级联),做法见 runInstanceTeardown。
	teardownKindInstance = "instance"
	// teardownKindWorldDrain 是缩容排空完成的世界频道(finishDrainedChannel),做法见 runWorldDrainTeardown。
	teardownKindWorldDrain = "world_drain"
	// teardownKindWorldOrphan 是地图已不在 World 表里的孤儿频道(tearDownOrphanScene),做法见 runWorldOrphanTeardown。
	teardownKindWorldOrphan = "world_orphan"
	// teardownKindRollback 是建实例时 C++ CreateScene 失败后的回滚(rollbackInstanceAllocation),做法见 runRollbackTeardown。
	teardownKindRollback = "rollback"
)

const (
	// sceneTeardownSweepInterval 是领导者扫一遍本 zone 待办的间隔。
	sceneTeardownSweepInterval = 15 * time.Second
	// sceneTeardownJobLease 是留给认领方当场做完的时间:认领超过它还没删的任务才由扫描接手。要长过认领方最慢的正常
	// 路径 —— go-redis 对一次 EVAL 的内部重发最坏约 4 ×(DialTimeout 5s + ReadTimeout 3s)≈ 33s,显式 DestroyScene
	// 副作用的预算 10s。短了也只是扫描与认领方同时做,结果仍然正确(DestroyScene 多发一次)。
	sceneTeardownJobLease = 90 * time.Second
	// sceneTeardownSweepBatch 是一次扫描最多接手的任务数,剩下的下一拍再做。
	sceneTeardownSweepBatch = 100
	// sceneTeardownJobTimeout 是扫描接手一个任务的预算:级联、DestroyScene、ReleaseRoom 都在里面。
	sceneTeardownJobTimeout = 10 * time.Second
)

func sceneTeardownJobKey(sceneID uint64) string {
	return fmt.Sprintf("scene:%d:teardown", sceneID)
}

func sceneTeardownPendingKey(zoneID uint32) string {
	return fmt.Sprintf("scene_teardown:zone:%d:pending", zoneID)
}

// sceneTeardownJob 是一条拆除任务。Claim 是认领脚本删除前读到的快照(Token 是任务的身份),副作用一律以它为准。
type sceneTeardownJob struct {
	ZoneID  uint32
	SceneID uint64
	Kind    string
	// Reason 是 Agones 归还与 instance_destroyed_total 的 reason。
	Reason string
	Claim  sceneClaim
}

func newSceneTeardownJob(req sceneTeardownRequest, claim sceneClaim) sceneTeardownJob {
	return sceneTeardownJob{ZoneID: req.ZoneID, SceneID: req.SceneID, Kind: req.Kind, Reason: req.Reason, Claim: claim}
}

// luaTakeTeardownRoom 拿「还 Agones 名额」这一步:一个任务只交给一个调用方。
// KEYS[1] = scene:{id}:teardown。ARGV[1] = 任务 token,ARGV[2] = 这次调用的 id。
// 回 1:这次调用拿到了,应当去还;回 0:任务不在、token 不符(已做完,或已换成别的任务),或已被别的调用拿过。
// 同一个调用 id 重发(go-redis 读超时之后原样重发)仍回 1,不会被自己第一次的置位挡在外面。
const luaTakeTeardownRoom = `
if redis.call('HGET', KEYS[1], 'token') ~= ARGV[1] then
    return 0
end
local taker = redis.call('HGET', KEYS[1], 'room')
if taker then
    if taker == ARGV[2] then
        return 1
    end
    return 0
end
redis.call('HSET', KEYS[1], 'room', ARGV[2])
return 1
`

// luaFinishSceneTeardown 删掉做完的任务。KEYS[1] = 任务,KEYS[2] = 本 zone 的待办集合。ARGV[1] = sceneId,
// ARGV[2] = 任务 token。token 不符时不动(任务已被别人做完删掉,或已换成别的任务),回 0。
const luaFinishSceneTeardown = `
if redis.call('HGET', KEYS[1], 'token') ~= ARGV[2] then
    return 0
end
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[1])
return 1
`

// luaDropDanglingTeardown 摘掉没有任务的待办条目。正常情况下不会出现(写任务与进待办同一步,删也同一步),
// 只防人工删键之类的残留让扫描每拍空转。KEYS 同 luaFinishSceneTeardown,ARGV[1] = sceneId。
const luaDropDanglingTeardown = `
if redis.call('EXISTS', KEYS[1]) == 1 then
    return 0
end
return redis.call('ZREM', KEYS[2], ARGV[1])
`

// errTeardownDeferred 表示有步骤这次没做成,任务留给扫描重做。
var errTeardownDeferred = errors.New("scene teardown step deferred")

// teardownDeferred 把这次没做成的步骤拼成一个错误,没有就回 nil。
func teardownDeferred(steps []string) error {
	if len(steps) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", errTeardownDeferred, strings.Join(steps, ", "))
}

// releaseTeardownRoom 还这个任务占的 Agones 名额,一个任务至多还一次,见 luaTakeTeardownRoom。
// 回错误只在「拿这一步」本身结局未知时:任务留给扫描,扫描拿不到(已被这次拿过)就不还 —— 至多一次。
// 拿到之后 ReleaseRoom 失败只记日志与指标(ReleaseAgonesRoom 里),不重试:结局未知,重试可能还两次。
func releaseTeardownRoom(ctx context.Context, svcCtx *svc.ServiceContext, job sceneTeardownJob) error {
	if job.Claim.AgonesGs == "" || GetAgonesAllocator() == nil {
		return nil
	}
	raw, err := svcCtx.Redis.Eval(luaTakeTeardownRoom, []string{sceneTeardownJobKey(job.SceneID)}, job.Claim.Token, uuid.NewString())
	if err != nil {
		return err
	}
	if toInt64(raw) != 1 {
		return nil
	}
	ReleaseAgonesRoomForScene(ctx, svcCtx, job.SceneID, job.Claim.AgonesGs, job.Reason)
	return nil
}

// unlinkFromSourceMirrors 把场景从它的镜像源的子集合 scene:{源}:mirrors 里摘掉。source 是认领快照里的镜像源
// (十进制 scene_id,空串表示不是镜像)。
//
// 源登记在别的 zone 时不动:那个子集合是那个 zone 的键,只由它的 scene_manager 读写
// (docs/design/control-plane-zone-scoping-20261105.md §4.3)。新建镜像时源登记在同组别的 zone 就不写
// scene:{id}:source(createInstance),这里给更早登记的数据兜底。本实例里没有源的 zone 记录时照摘:源已经销毁
// (zone 记录随销毁删掉),或源在别的组的实例里;两种情况下这份子集合都只有本 zone 写。读不到或解析不了源的 zone、
// SREM 失败时回错误,调用方的拆除任务留着重做。
func unlinkFromSourceMirrors(svcCtx *svc.ServiceContext, zoneID uint32, sceneID uint64, source string) error {
	sourceID, _ := strconv.ParseUint(source, 10, 64)
	if sourceID == 0 {
		return nil
	}
	sourceZone, err := lookupSceneZone(svcCtx, sourceID)
	if err != nil {
		return err
	}
	if sourceZone != 0 && sourceZone != zoneID {
		logx.Infof("[SceneTeardown] scene %d: its mirror source %d is registered in zone %d, not %d; leaving the source's mirror set to that zone",
			sceneID, sourceID, sourceZone, zoneID)
		return nil
	}
	_, err = svcCtx.Redis.Srem(sceneMirrorsKey(sourceID), strconv.FormatUint(sceneID, 10))
	return err
}

// finishSceneTeardownJob 删掉做完的任务。回 false:任务已被别人做完删掉,调用方不要再记完成。
func finishSceneTeardownJob(svcCtx *svc.ServiceContext, job sceneTeardownJob) (bool, error) {
	raw, err := svcCtx.Redis.Eval(luaFinishSceneTeardown,
		[]string{sceneTeardownJobKey(job.SceneID), sceneTeardownPendingKey(job.ZoneID)},
		strconv.FormatUint(job.SceneID, 10), job.Claim.Token)
	if err != nil {
		return false, err
	}
	return toInt64(raw) == 1, nil
}

// runSceneTeardown 按任务种类做认领之后的副作用,可以重跑。回 nil 表示全部做完,可以删任务;回错误时任务留给扫描。
// preMirrorChildren 是认领方在认领前读到的镜像子集合,只给级联在提交后重读失败时兜底;扫描接手时为 nil。
func runSceneTeardown(ctx context.Context, svcCtx *svc.ServiceContext, job sceneTeardownJob, preMirrorChildren []string) error {
	switch job.Kind {
	case teardownKindInstance:
		return runInstanceTeardown(ctx, svcCtx, job, preMirrorChildren)
	case teardownKindWorldDrain:
		return runWorldDrainTeardown(ctx, svcCtx, job, preMirrorChildren)
	case teardownKindWorldOrphan:
		return runWorldOrphanTeardown(ctx, svcCtx, job)
	case teardownKindRollback:
		return runRollbackTeardown(ctx, svcCtx, job)
	}
	return fmt.Errorf("unknown scene teardown kind %q", job.Kind)
}

// completeSceneTeardown 做一个任务的副作用,全部做完就删掉任务,返回任务是否由这次删掉。认领方与扫描共用。
// recovered 标出是不是扫描接手的(指标 outcome)。
//
// ctx 被取消(滚动发布时空闲回收的循环 ctx、调用方超时)时副作用会半途停下,任务留在待办里,由本 zone 下一任领导者
// 在租期之后补做。所以认领方直接传自己的 ctx,不需要为了「做完」而摘掉取消。
func completeSceneTeardown(ctx context.Context, svcCtx *svc.ServiceContext, job sceneTeardownJob, preMirrorChildren []string, recovered bool) bool {
	if err := runSceneTeardown(ctx, svcCtx, job, preMirrorChildren); err != nil {
		logx.Errorf("[SceneTeardown] zone=%d scene=%d kind=%s reason=%s: not finished, left for the sweep: %v",
			job.ZoneID, job.SceneID, job.Kind, job.Reason, err)
		metrics.ObserveSceneTeardownJob(job.ZoneID, job.Kind, "deferred")
		return false
	}
	finished, err := finishSceneTeardownJob(svcCtx, job)
	if err != nil {
		// 副作用都做完了,只是删任务的结局未知。没删掉的话扫描会重跑一遍,全是幂等步骤。
		logx.Errorf("[SceneTeardown] zone=%d scene=%d kind=%s: side effects done but removing the job failed: %v",
			job.ZoneID, job.SceneID, job.Kind, err)
		metrics.ObserveSceneTeardownJob(job.ZoneID, job.Kind, "deferred")
		return false
	}
	if !finished {
		return false
	}
	outcome := "completed"
	if recovered {
		outcome = "recovered"
	}
	metrics.ObserveSceneTeardownJob(job.ZoneID, job.Kind, outcome)
	onSceneTeardownFinished(job)
	return true
}

// onSceneTeardownFinished 记各路径「做完了」的业务指标。只在任务被删的那一次调用,重跑与接手都不会重复计数。
func onSceneTeardownFinished(job sceneTeardownJob) {
	switch job.Kind {
	case teardownKindInstance:
		kind := "instance"
		if job.Claim.MirrorFlag == "1" {
			kind = "mirror"
		}
		metrics.ObserveInstanceDestroyed(job.ZoneID, kind, job.Reason)
	case teardownKindWorldDrain:
		metrics.ObserveWorldAutoscale(job.ZoneID, "scale_in", "drained")
	}
}

// loadSceneTeardownJob 读一条任务。任务不在时 ok=false。
func loadSceneTeardownJob(svcCtx *svc.ServiceContext, zoneID uint32, sceneID uint64) (sceneTeardownJob, bool, error) {
	fields, err := svcCtx.Redis.Hmget(sceneTeardownJobKey(sceneID), "token", "kind", "reason", "reply")
	if err != nil {
		return sceneTeardownJob{}, false, err
	}
	if len(fields) < 4 || fields[0] == "" {
		return sceneTeardownJob{}, false, nil
	}
	var reply []string
	if err := json.Unmarshal([]byte(fields[3]), &reply); err != nil {
		return sceneTeardownJob{}, false, fmt.Errorf("decode the claim snapshot of scene %d: %w", sceneID, err)
	}
	claim := parseSceneClaimReply(reply)
	if !claim.Claimed {
		return sceneTeardownJob{}, false, fmt.Errorf("teardown job of scene %d holds a snapshot that is not a claim: %v", sceneID, reply)
	}
	claim.Token = fields[0]
	return sceneTeardownJob{ZoneID: zoneID, SceneID: sceneID, Kind: fields[1], Reason: fields[2], Claim: claim}, true, nil
}

// StartSceneTeardownSweeper 起拆除任务的扫描循环,立即返回。只有领导者真正扫描,只扫本进程编排的 zone。
func StartSceneTeardownSweeper(ctx context.Context, svcCtx *svc.ServiceContext) {
	logx.Infof("[SceneTeardown] sweeper started: interval=%v lease=%v batch=%d",
		sceneTeardownSweepInterval, sceneTeardownJobLease, sceneTeardownSweepBatch)
	safego.Loop(ctx, SafePointSceneTeardownSweep, sceneTeardownSweepInterval, func(ctx context.Context) {
		for _, zoneID := range orchestratedZones(svcCtx) {
			if !isLeader() {
				// 积压年龄只由领导者发布。降级之后归零,免得卸任时的旧值一直挂在这个副本上触发告警。
				metrics.SetSceneTeardownOldestPendingSeconds(zoneID, 0)
				continue
			}
			sweepSceneTeardownJobs(ctx, svcCtx, zoneID, time.Now())
		}
	})
}

// sweepSceneTeardownJobs 接手本 zone 认领超过 sceneTeardownJobLease 还没做完的任务,返回这一拍做完了几条。
// now 由调用方给出,单测用它跳过租期。
func sweepSceneTeardownJobs(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, now time.Time) int {
	pendingKey := sceneTeardownPendingKey(zoneID)
	observeOldestSceneTeardown(svcCtx, zoneID, now)
	due, err := svcCtx.Redis.ZrangebyscoreWithScoresAndLimit(pendingKey, 0, now.Add(-sceneTeardownJobLease).Unix(), 0, sceneTeardownSweepBatch)
	if err != nil {
		logx.Errorf("[SceneTeardown] zone=%d: listing due jobs failed, retrying next sweep: %v", zoneID, err)
		return 0
	}
	finished := 0
	for _, entry := range due {
		// 降级或进程退出即停:剩下的由新领导者的下一拍接手。
		if !isLeader() || ctx.Err() != nil {
			break
		}
		sceneID, err := strconv.ParseUint(entry.Key, 10, 64)
		if err != nil {
			logx.Errorf("[SceneTeardown] zone=%d: malformed pending member %q, leaving it", zoneID, entry.Key)
			continue
		}
		job, ok, err := loadSceneTeardownJob(svcCtx, zoneID, sceneID)
		if err != nil {
			logx.Errorf("[SceneTeardown] zone=%d scene=%d: reading the job failed, retrying next sweep: %v", zoneID, sceneID, err)
			continue
		}
		if !ok {
			if _, err := svcCtx.Redis.Eval(luaDropDanglingTeardown,
				[]string{sceneTeardownJobKey(sceneID), pendingKey}, strconv.FormatUint(sceneID, 10)); err != nil {
				logx.Errorf("[SceneTeardown] zone=%d scene=%d: dropping a pending entry without a job failed: %v", zoneID, sceneID, err)
			}
			continue
		}
		logx.Infof("[SceneTeardown] zone=%d scene=%d kind=%s reason=%s: claimed %ds ago and not finished, taking it over",
			zoneID, sceneID, job.Kind, job.Reason, now.Unix()-entry.Score)
		jobCtx, cancel := context.WithTimeout(ctx, sceneTeardownJobTimeout)
		if completeSceneTeardown(jobCtx, svcCtx, job, nil, true) {
			finished++
		}
		cancel()
	}
	return finished
}

// observeOldestSceneTeardown 发布本 zone 最老一条待办的年龄(秒,没有待办为 0),给「拆除任务做不完」告警用。
func observeOldestSceneTeardown(svcCtx *svc.ServiceContext, zoneID uint32, now time.Time) {
	oldest, err := svcCtx.Redis.ZrangeWithScores(sceneTeardownPendingKey(zoneID), 0, 0)
	if err != nil {
		logx.Errorf("[SceneTeardown] zone=%d: reading the oldest pending job failed: %v", zoneID, err)
		return
	}
	age := int64(0)
	if len(oldest) > 0 {
		age = max(now.Unix()-oldest[0].Score, 0)
	}
	metrics.SetSceneTeardownOldestPendingSeconds(zoneID, age)
}
