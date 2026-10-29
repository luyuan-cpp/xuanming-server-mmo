package logic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/savebarrier"

	"github.com/zeromicro/go-zero/core/stores/redis"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ---------------------------------------------------------------------------
// GO-3:同号新进程顶替后的持有者判定(cross-zone-scene-travel.md §13.10)
//
// location 只记 node_id。C++ 分配 node_id 时从 1 往上取最小空号(node_allocator.cpp),所以 scene 进程崩溃、
// 租约到期后重启,几乎总会拿回原来的号。此时 location 指向的 (zone,node) 仍在册,playerLocationOwnerDead
// 不成立;落点又是同物理节点,不铸新 epoch。新进程按原 E 登记交接资格,而旧进程以同一个 E 盖过章,
// kRegisterFenceLua 对「同 E、不同实例」一律拒绝(防止两个进程同持一代),新进程永远进不了这名玩家;
// LeaveScene 也拿不到释放证据,等待预算不授权删除。结果是崩溃节点上的玩家被永久挡在门外。
//
// 判据用持有进程**自己盖的章**:交接资格(save_barrier 的 handoff_fence)里的 SceneInstance 是登记成功、
// 真正开始持有这名玩家的那个进程的启动 UUID(role_admission_system.cpp ReadLocalNode = NodeInfo.node_uuid)。
// 它回答「此刻谁持有」,不回答「路由当时想派给谁」—— 后者就是旧方案 R1 的毛病:滞留的路由事件
// 投给了同号新进程,按路由时的实例号判定会冤枉一个健康的持有者。这里没有这个问题:新进程若真的
// 登记成功,章就是它自己的,与在册实例一致,不判顶替。
//
// 放行与 §11.5 的「属主进程已从注册表消失」是同一个例外,只是把「进程」从 node_id 精确到实例:
//   - 持有实例在 etcd 注册表里**正面**消失:先看本副本镜像,再向 etcd 做一次线性一致读复核
//     (镜像可能滞后,滞后的方向恰好会让刚注册的持有者「看起来不在」);读不到 etcd 一律不放行;
//   - 再入屏障从本副本观察到它消失的时刻起算(watch 删除 / 换实例时登记;没登记过的,以本次复核为
//     首次观察),与 playerLocationOwnerDead 的 nodeGoneObservedAt 同一口径;
//   - 放行后按无持有者落点,一定铸造新 epoch:旧实例若其实还活着,它手里的 E 之后每一次存盘都被
//     owner CAS 拒绝,交接标记也因资格已被新持有者轮换而写不出去 —— 只丢它未落盘的进度,不会旧盖新。
// ---------------------------------------------------------------------------

// freshRegistryReadTimeout:etcd 复核的时限,同时用于一轮读本身与调用者的等待。EnterScene 的服务端超时预算
// (scene_manager_service.yaml 文件头)只给 Redis 留了 1500ms 余量,复核必须远小于它;超时按「复核不了」回可重试的 17。
const freshRegistryReadTimeout = 500 * time.Millisecond

// instanceGoneRetention:实例消失记录的保留期。实例 UUID 每次进程启动新生成、不会被别的进程复用,记录规模等于
// 这段时间里死掉的 scene 进程数,所以可以远长于 location 的寿命。不复用 death_at 的 10 分钟:记录被清掉后,
// 名下还留着 location 的长尾玩家会被重新压一个屏障(屏障对早已停笔的进程没有任何安全收益)。
const instanceGoneRetention = 24 * time.Hour

// replacedLogInterval:同一个持有实例的「复核失败 / 屏障起算」日志的最短间隔,崩溃节点上的几千名玩家
// 一起重试时不刷屏。被抑制的条数在下一条日志里带出。
const replacedLogInterval = 30 * time.Second

// sceneNodeAllocationPrefix 是 C++ scene 节点号占位键的前缀(etcd_manager.cpp MakeNodeAllocationPrefix):
// SceneNodeService.rpc/allocated/node_type/<T>/node_id/<N>,值是该进程的 node_uuid 原文(不是 JSON)。
// 占位键与服务注册键挂同一个租约、同生同灭,每个活进程恰有一条;失租重注册(ReRegisterExistingNode)保留同值。
// 所以「占位键里有这个 UUID」就是「这个进程的租约还活着」的正面证据,而且每条只有几十字节。
const sceneNodeAllocationPrefix = sceneNodePrefix + "allocated/"

// instanceGoneObservedAt:本副本观察到某个 scene 进程实例从注册表消失的时刻,键是实例 UUID。
// 与 nodeGoneObservedAt 同理只在进程内:每个副本各自从自己看到的那一刻起算屏障。
var (
	instanceGoneObservedMu sync.Mutex
	instanceGoneObservedAt = make(map[string]time.Time)
)

// freshRegistryInstancesOverride 非空时替代 etcd 复核,只供单测注入(返回在册实例集合)。
var freshRegistryInstancesOverride func(ctx context.Context) (map[string]struct{}, error)

// noteInstanceGoneFromRegistry 记下本副本观察到实例消失的时刻。覆盖写:实例失租后可能以同一 UUID 重注册
// (ReRegisterExistingNode 保留身份),之后再消失时屏障从最近一次起算。顺手清掉过期记录。
func noteInstanceGoneFromRegistry(instance string) {
	if instance == "" {
		return
	}
	now := time.Now()
	instanceGoneObservedMu.Lock()
	defer instanceGoneObservedMu.Unlock()
	for key, at := range instanceGoneObservedAt {
		if now.Sub(at) > instanceGoneRetention {
			delete(instanceGoneObservedAt, key)
		}
	}
	instanceGoneObservedAt[instance] = now
}

// forgetInstanceGoneObservation:实例重新在册,作废它旧的消失记录。之后再消失时由 watch DELETE / 差集重新起算;
// 漏掉的话以复核为首次观察,屏障只会更晚,方向安全。不清的话,漏掉一次「重注册 + 再消失」会拿陈旧时刻提前放行。
func forgetInstanceGoneObservation(instance string) {
	if instance == "" {
		return
	}
	instanceGoneObservedMu.Lock()
	delete(instanceGoneObservedAt, instance)
	instanceGoneObservedMu.Unlock()
}

// reconcileInstanceGoneObservationsAfterResync 在 fullSync 换上新镜像之后调用:在册的实例作废记录;
// 仍不在册的已记录实例一律改记为此刻 —— 镜像断档期间可能发生过「回来又消失」,保守地后推一个屏障。
func reconcileInstanceGoneObservationsAfterResync(registered map[string]struct{}) {
	now := time.Now()
	instanceGoneObservedMu.Lock()
	defer instanceGoneObservedMu.Unlock()
	for instance := range instanceGoneObservedAt {
		if _, ok := registered[instance]; ok {
			delete(instanceGoneObservedAt, instance)
		} else {
			instanceGoneObservedAt[instance] = now
		}
	}
}

func instanceGoneObservedSince(instance string) (time.Time, bool) {
	instanceGoneObservedMu.Lock()
	defer instanceGoneObservedMu.Unlock()
	at, ok := instanceGoneObservedAt[instance]
	return at, ok
}

// noteInstanceRetiredIfGone:某个注册条目不再代表 gone 的实例时调用(watch DELETE、同一个键换了实例)。
// 实例已没有任何键在册才登记。调用前 knownNodes 必须已经反映这次变更。
func noteInstanceRetiredIfGone(gone nodeEntry) {
	if gone.reg.NodeUuid == "" || knownInstanceRegistered(gone.reg.NodeUuid) {
		return
	}
	noteInstanceGoneFromRegistry(gone.reg.NodeUuid)
}

// knownInstanceRegistered:本副本镜像里是否有任何键登记着这个实例(不论 zone / node_id)。
func knownInstanceRegistered(instance string) bool {
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	for _, entry := range knownNodes {
		if entry.reg.NodeUuid == instance {
			return true
		}
	}
	return false
}

// knownNodeRegisteredInstance 返回本副本镜像里 (zone,node) 唯一一条注册的实例 UUID。
// 没有注册(走 playerLocationOwnerDead)、身份歧义、或注册不带 UUID(旧版本)时返回 false。
func knownNodeRegisteredInstance(zoneID uint32, nodeID string) (string, bool) {
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	instance, matches := "", 0
	for _, entry := range knownNodes {
		if entry.reg.ZoneId == zoneID && entry.nodeID == nodeID {
			instance = entry.reg.NodeUuid
			matches++
		}
	}
	return instance, matches == 1 && instance != ""
}

// allocationInstances 把占位键的值集合成在册实例集合。值是 UUID 原文;空值跳过(不是任何进程)。
func allocationInstances(values [][]byte) map[string]struct{} {
	instances := make(map[string]struct{}, len(values))
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		instances[string(value)] = struct{}{}
	}
	return instances
}

// ---------------------------------------------------------------------------
// etcd 复核的合并读
//
// 节点崩溃后几千名玩家几乎同时回来,各自复核会对 etcd 打几千次前缀读。这里把同一时刻的复核合并成一轮,
// 但**只复用在调用者到达之后才发出的那一轮读**:调用者加入「下一轮」,下一轮在当前这轮结束后才发出,所以每一轮读
// 的线性化点都晚于它的每个加入者的镜像检查,与逐请求各读一次等价(按时间缓存「不在册」的结论则不等价:
// 同一个 UUID 可以重注册回来)。
// ---------------------------------------------------------------------------

type registryRecheckRound struct {
	done      chan struct{}
	instances map[string]struct{}
	err       error
}

type registryRecheck struct {
	mu      sync.Mutex
	running bool
	next    *registryRecheckRound
	read    func(ctx context.Context) (map[string]struct{}, error)
}

// join 加入下一轮,必要时启动读循环。read 取第一个触发读循环的调用者给的(生产上全进程只有一个 svcCtx)。
func (r *registryRecheck) join(read func(ctx context.Context) (map[string]struct{}, error)) *registryRecheckRound {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next == nil {
		r.next = &registryRecheckRound{done: make(chan struct{})}
	}
	round := r.next
	if !r.running {
		r.running = true
		r.read = read
		go r.run()
	}
	return round
}

func (r *registryRecheck) run() {
	for {
		r.mu.Lock()
		round, read := r.next, r.read
		r.next = nil
		if round == nil {
			r.running = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		// 这一轮的读在此刻发出,晚于本轮每个加入者的到达。不挂任何调用者的 ctx:调用者可以先放弃等待,读本身照常收尾。
		ctx, cancel := context.WithTimeout(context.Background(), freshRegistryReadTimeout)
		round.instances, round.err = read(ctx)
		cancel()
		close(round.done)
	}
}

var allocationRecheck registryRecheck

// freshRegistryInstances 返回 etcd 里此刻在册的 scene 进程实例集合(线性一致读,见上面的合并规则)。
func freshRegistryInstances(ctx context.Context, svcCtx *svc.ServiceContext) (map[string]struct{}, error) {
	if freshRegistryInstancesOverride != nil {
		return freshRegistryInstancesOverride(ctx)
	}
	if svcCtx.Etcd == nil {
		return nil, errors.New("etcd client not configured")
	}
	client := svcCtx.Etcd
	round := allocationRecheck.join(func(readCtx context.Context) (map[string]struct{}, error) {
		resp, err := client.Get(readCtx, sceneNodeAllocationPrefix, clientv3.WithPrefix())
		if err != nil {
			return nil, err
		}
		values := make([][]byte, 0, len(resp.Kvs))
		for _, kv := range resp.Kvs {
			values = append(values, kv.Value)
		}
		return allocationInstances(values), nil
	})
	waitCtx, cancel := context.WithTimeout(ctx, freshRegistryReadTimeout)
	defer cancel()
	select {
	case <-round.done:
		return round.instances, round.err
	case <-waitCtx.Done():
		return nil, waitCtx.Err()
	}
}

// readHandoffFence 读玩家屏障 hash 里的交接资格。没有该字段返回 ok=false、err=nil;原文不规范返回错误。
func readHandoffFence(svcCtx *svc.ServiceContext, playerID uint64) (savebarrier.HandoffFence, bool, error) {
	raw, err := svcCtx.Redis.Hget(savebarrier.Key(playerID), savebarrier.HandoffFenceField)
	if errors.Is(err, redis.Nil) {
		return savebarrier.HandoffFence{}, false, nil
	}
	if err != nil {
		return savebarrier.HandoffFence{}, false, err
	}
	fence, err := savebarrier.ParseHandoffFence(raw)
	if err != nil {
		return savebarrier.HandoffFence{}, false, fmt.Errorf("%w: %q", err, raw)
	}
	return fence, true, nil
}

// replacedLogLimiter 让同一持有实例的日志每 replacedLogInterval 最多一条,返回此前被抑制的条数。
var replacedLogLimiter = struct {
	mu   sync.Mutex
	last map[string]time.Time
	held map[string]int
}{last: make(map[string]time.Time), held: make(map[string]int)}

func allowReplacedLog(instance string) (bool, int) {
	now := time.Now()
	replacedLogLimiter.mu.Lock()
	defer replacedLogLimiter.mu.Unlock()
	if at, ok := replacedLogLimiter.last[instance]; ok && now.Sub(at) < replacedLogInterval && now.Sub(at) >= 0 {
		replacedLogLimiter.held[instance]++
		return false, 0
	}
	if len(replacedLogLimiter.last) > 4096 {
		// 上限只为给内存封顶:每个消失的实例一条,正常远到不了。
		replacedLogLimiter.last = make(map[string]time.Time)
		replacedLogLimiter.held = make(map[string]int)
	}
	suppressed := replacedLogLimiter.held[instance]
	replacedLogLimiter.last[instance] = now
	delete(replacedLogLimiter.held, instance)
	return true, suppressed
}

// ownerReplacedVerdict 是 playerLocationOwnerReplaced 的结论。底层类型 uint8 数的是结论种类数(现 4 种)。
type ownerReplacedVerdict uint8

const (
	// ownerNotReplaced:没有「持有进程已被顶替」的证据,沿用原来的判定。
	ownerNotReplaced ownerReplacedVerdict = iota
	// ownerReplacedBarrierPending:持有实例已不在册,但本副本观察到它消失还不满一个再入屏障。
	// EnterScene 一个字节都不改地以可重试的 17 拒绝 —— 照旧路由只会让同号新进程按原 epoch 登记、必被拒。
	ownerReplacedBarrierPending
	// ownerReplacedUnverified:镜像已表明持有实例不是现任、也不在任何键下,但 etcd 复核没做成(超时 / 不可达)。
	// EnterScene 同样回 17、一个字节不改:照旧路由同样注定被拒,下一次重试再复核。
	ownerReplacedUnverified
	// ownerReplacedTakeover:持有实例已不在册且屏障已过,按无持有者落点。
	ownerReplacedTakeover
)

// playerLocationOwnerReplaced 判定 loc 记录的节点号虽然仍在册,但这名玩家的持有进程已经不在了(GO-3)。
// site 是再入屏障计数的点位名(EnterScene 与 LeaveScene 分开记,两者的重试节奏不同)。
//
// 以下全部成立才不是 ownerNotReplaced,任何一步拿不准都返回 ownerNotReplaced(沿用原判定,fail-closed):
//  1. loc 指向具体节点、zone 已知、owner_epoch 非 0,本副本已完成首次 etcd 全量同步;
//  2. 本副本镜像里 (zone,node) 恰有一条带 UUID 的注册(没有注册的走 playerLocationOwnerDead);
//  3. 交接资格存在、可解析、点名了一个实例,且 epoch 等于当前 owner_epoch —— epoch 不等说明铸造之后新持有者
//     还没登记,章说明不了当前这一代(这一类的残余见 §13.10)。状态 H / F / R 都算:kRegisterFenceLua 对
//     「同 E、不同实例」一律拒绝,撤销态(R)同样挡住新进程按原 E 登记;不点名实例的恢复墓碑(generation 0)不算;
//  4. 盖章的实例不是在册实例,也不在本副本镜像的任何键下;
//  5. etcd 线性一致复核:占位键里没有这个实例。复核做不成返回 ownerReplacedUnverified。
//
// 都成立之后看再入屏障:本副本观察到该实例消失(watch 删除 / 换实例时登记;没登记过的,以本次复核为
// 首次观察)满一个屏障时长才是 ownerReplacedTakeover,否则是 ownerReplacedBarrierPending。
// 屏障未过且已有观察记录时不再复核 etcd,重试期间不给 etcd 加压。
// 例外:资格是 F(持有者已在同一段 Lua 里原子写出交接标记、不再写这一代)时,屏障内不拦,交给换手门凭标记判断
// —— 否则失租进程的疏散改派会在屏障内全部被 17 挡住。
func (l *EnterSceneLogic) playerLocationOwnerReplaced(playerID uint64, loc *scene_manager.PlayerLocation,
	zoneID uint32, observedEpoch uint64, site string) ownerReplacedVerdict {
	if loc == nil || zoneID == 0 || loc.GetNodeId() == "" || observedEpoch == 0 || !knownNodesSynced.Load() {
		return ownerNotReplaced
	}
	nodeID := loc.GetNodeId()
	registered, ok := knownNodeRegisteredInstance(zoneID, nodeID)
	if !ok {
		return ownerNotReplaced
	}
	fence, ok, err := readHandoffFence(l.svcCtx, playerID)
	if err != nil {
		l.Logger.Errorf("[OwnerReplaced] cannot read the holder's handoff fence; keeping the location: player=%d zone=%d node=%s err=%v",
			playerID, zoneID, nodeID, err)
		return ownerNotReplaced
	}
	if !ok || fence.SceneInstance == "" || fence.OwnerEpoch != observedEpoch || fence.SceneInstance == registered ||
		knownInstanceRegistered(fence.SceneInstance) {
		return ownerNotReplaced
	}
	holder := fence.SceneInstance
	frozenWithMarker := fence.State == savebarrier.HandoffFenceFrozen

	barrier := sceneReentryBarrier(l.svcCtx)
	observedAt, observed := instanceGoneObservedSince(holder)
	if observed {
		if elapsed := time.Since(observedAt); elapsed < 0 || elapsed < barrier {
			if frozenWithMarker {
				return ownerNotReplaced
			}
			metrics.ObserveReentryBarrierBlocked(zoneID, site)
			return ownerReplacedBarrierPending
		}
	}
	instances, err := freshRegistryInstances(l.ctx, l.svcCtx)
	if err != nil {
		metrics.ObserveOwnerReplacedRecheck(zoneID, metrics.OwnerReplacedRecheckError)
		if allowed, suppressed := allowReplacedLog(holder); allowed {
			l.Logger.Errorf("[OwnerReplaced] etcd re-check failed; rejecting retryably without any write: player=%d zone=%d node=%s holder_instance=%s suppressed=%d err=%v",
				playerID, zoneID, nodeID, holder, suppressed, err)
		}
		return ownerReplacedUnverified
	}
	if _, present := instances[holder]; present {
		// 镜像滞后:持有实例其实在册(多半是刚注册,watch 还没送到)。它回来了,旧的消失记录作废。
		metrics.ObserveOwnerReplacedRecheck(zoneID, metrics.OwnerReplacedRecheckPresent)
		forgetInstanceGoneObservation(holder)
		l.Logger.Infof("[OwnerReplaced] holder instance is still registered in etcd although this replica's mirror misses it; keeping the location: player=%d zone=%d node=%s holder_instance=%s",
			playerID, zoneID, nodeID, holder)
		return ownerNotReplaced
	}
	metrics.ObserveOwnerReplacedRecheck(zoneID, metrics.OwnerReplacedRecheckAbsent)
	if !observed {
		// 本副本第一次确认它不在册:屏障从此刻起算(实际消失只会更早,方向安全)。
		noteInstanceGoneFromRegistry(holder)
		if allowed, suppressed := allowReplacedLog(holder); allowed {
			l.Logger.Infof("[OwnerReplaced] holder instance is gone from etcd while node %s is now registered by instance %s; reentry barrier %v starts now: player=%d zone=%d holder_instance=%s owner_epoch=%d suppressed=%d",
				nodeID, registered, barrier, playerID, zoneID, holder, observedEpoch, suppressed)
		}
		if frozenWithMarker {
			return ownerNotReplaced
		}
		metrics.ObserveReentryBarrierBlocked(zoneID, site)
		return ownerReplacedBarrierPending
	}
	return ownerReplacedTakeover
}
