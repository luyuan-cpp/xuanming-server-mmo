package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// 别的 zone 的场景节点判定(docs/design/control-plane-zone-scoping-20261105.md §3.5)。
//
// 本进程只 watch 自己 zone 的场景节点(knownNodes 只有本 zone),也不读别的 zone 的控制域键(负载集、death_at,§4.3)。
// 位置落在别的 zone 的玩家(访客掉线后从家乡 zone 登录、清理任务落到本 zone 等少见路径)要判断持有节点的死活时,
// 证据只取两样与 zone 无关的东西:
//
//  1. 那个 zone 前缀 SceneNodeService.rpc/zone/<z>/ 的 etcd 线性一致读(超时 foreignZoneRegistryReadTimeout)。
//     读失败、值解析不了、值里的 zone 与前缀不符,一律按「拿不到证据」处理(fail-closed,不接管);
//  2. 本副本第一次看到它不在册的时刻,满一个再入屏障才算死亡证据。
//
// 与本 zone 的 death_at 一样安全:death_at 是那个 zone 的领导者第一次观察到节点消失的时刻,本副本的首次观察只会
// 更晚,屏障结束点跟着更晚。负载集只是 etcd 注册的派生物,注册已消失时它不提供更强的证据。接管之后僵尸写入由
// owner_epoch 挡住,与本 zone 相同。代价是首次观察那一次一定回可重试的拒绝,玩家多等一个屏障。

// foreignZoneRegistryReadTimeout 是读别的 zone 注册表的预算。挂在 EnterScene / LeaveScene 的少见路径上,不能给长。
const foreignZoneRegistryReadTimeout = 2 * time.Second

// foreignAbsenceLimit 是首次观察记录的上限,只为给内存封顶;正常远到不了(每个死掉的别 zone 节点一条)。
const foreignAbsenceLimit = 1 << 16

// sceneNodeZonePrefix 是 zone 的场景节点注册前缀,与 C++ EtcdManager::MakeNodeEtcdPrefix 的键形一致
// (SceneNodeService.rpc/zone/<z>/node_type/<t>/node_id/<n>)。带结尾斜杠:zone 7 不能前缀匹配到 zone 70。
func sceneNodeZonePrefix(zoneID uint32) string {
	return sceneNodePrefix + "zone/" + strconv.FormatUint(uint64(zoneID), 10) + "/"
}

// zoneRegistryFetchOverride 让单测不起 etcd 也能给出某个 zone 的注册表。返回的条目不用再校验 zone。
var zoneRegistryFetchOverride func(ctx context.Context, zoneID uint32) ([]nodeEntry, error)

// travelMapOpenOverride 让 EnterScene 的单测按「目标 zone 开没开这张图」给结论,不经 World 表与注册表。
// foreignZoneOpensWorldMap 自己的判定由 foreign_zone_registry_test.go 直接测。
var travelMapOpenOverride func(zoneID uint32, confID uint64) (bool, error)

// fetchZoneRegistry 线性一致读 zone 的场景节点注册表。任何一条拿不准都整体报错:这是死亡证据的来源,
// 漏看一条在册的节点就会把活的持有者判成已消失。
func fetchZoneRegistry(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) ([]nodeEntry, error) {
	if zoneRegistryFetchOverride != nil {
		return zoneRegistryFetchOverride(ctx, zoneID)
	}
	if svcCtx.Etcd == nil {
		return nil, errors.New("etcd client not configured")
	}
	readCtx, cancel := context.WithTimeout(ctx, foreignZoneRegistryReadTimeout)
	defer cancel()
	prefix := sceneNodeZonePrefix(zoneID)
	resp, err := svcCtx.Etcd.Get(readCtx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd get %s: %w", prefix, err)
	}
	entries := make([]nodeEntry, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		entry, ok := parseNodeEntry(kv.Value)
		if !ok {
			return nil, fmt.Errorf("unparseable registration under %s: key=%s", prefix, kv.Key)
		}
		if entry.reg.ZoneId != zoneID {
			return nil, fmt.Errorf("registration %s claims zone %d under prefix %s", kv.Key, entry.reg.ZoneId, prefix)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// absenceObservations 记「第一次看到它不在册」的时刻。看到它在册就清掉;一直不在册的保留最早那一次。
// 与 nodeGoneObservedAt(本 zone,watch 驱动,保留最近一次消失)分开:语义不同,也不互相清理。
type absenceObservations struct {
	mu  sync.Mutex
	at  map[string]time.Time
	now func() time.Time
}

func newAbsenceObservations() *absenceObservations {
	return &absenceObservations{at: make(map[string]time.Time), now: time.Now}
}

// observe 返回本次观察之后该键的首次缺席时刻;present 时清掉记录并返回 ok=false。
func (o *absenceObservations) observe(key string, present bool) (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if present {
		delete(o.at, key)
		return time.Time{}, false
	}
	if since, ok := o.at[key]; ok {
		return since, true
	}
	if len(o.at) >= foreignAbsenceLimit {
		// 封顶时整表重来:只会让一部分接管多等一个屏障,方向是 fail-closed。
		logx.Errorf("[ForeignZone] absence observation table reached %d entries; resetting", len(o.at))
		o.at = make(map[string]time.Time)
	}
	since := o.now()
	o.at[key] = since
	return since, true
}

var (
	// foreignNodeAbsence 的键是 "<zone>/<node>",foreignZoneAbsence 的键是 "<zone>"。
	foreignNodeAbsence = newAbsenceObservations()
	foreignZoneAbsence = newAbsenceObservations()
)

// barrierElapsedSince:从 since 起是否已满一个再入屏障。墙钟回拨导致的负时长按「刚消失」处理。
func barrierElapsedSince(svcCtx *svc.ServiceContext, since time.Time, now time.Time) bool {
	elapsed := now.Sub(since)
	return elapsed >= 0 && elapsed >= sceneReentryBarrier(svcCtx)
}

// foreignOwnerDead 判定别的 zone 的 (zone,node) 是否已不可能再写这名玩家:etcd 里它不在册,且本副本第一次看到它
// 不在册已满一个再入屏障。身份有歧义(同一 (zone,node) 两条注册)时它在册,返回 false。
func foreignOwnerDead(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, nodeID, site string) bool {
	if zoneID == 0 || nodeID == "" || ownsControlZone(svcCtx, zoneID) {
		return false
	}
	entries, err := fetchZoneRegistry(ctx, svcCtx, zoneID)
	if err != nil {
		metrics.ObserveForeignZoneRegistryRead(zoneID, metrics.ForeignZoneRegistryReadError)
		logx.Errorf("[ForeignZone] cannot read zone %d registry; holder %s stays owner (fail-closed): %v", zoneID, nodeID, err)
		return false
	}
	metrics.ObserveForeignZoneRegistryRead(zoneID, metrics.ForeignZoneRegistryReadOK)
	present := false
	for _, entry := range entries {
		if entry.nodeID == nodeID {
			present = true
			break
		}
	}
	since, absent := foreignNodeAbsence.observe(nodeGoneObservedKey(zoneID, nodeID), present)
	if !absent {
		return false
	}
	if !barrierElapsedSince(svcCtx, since, foreignNodeAbsence.now()) {
		metrics.ObserveReentryBarrierBlocked(zoneID, site)
		return false
	}
	return true
}

// foreignZoneGone 判定别的 zone 是否整体下线:etcd 里那个 zone 一个场景节点都没有,且本副本第一次看到它空已满一个
// 再入屏障。合服后源区永久下线就是这个形态;它的控制域实例可能也已撤掉,所以只看 etcd。
func foreignZoneGone(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, site string) bool {
	if zoneID == 0 || ownsControlZone(svcCtx, zoneID) {
		return false
	}
	entries, err := fetchZoneRegistry(ctx, svcCtx, zoneID)
	if err != nil {
		metrics.ObserveForeignZoneRegistryRead(zoneID, metrics.ForeignZoneRegistryReadError)
		logx.Errorf("[ForeignZone] cannot read zone %d registry; its locations stay as they are (fail-closed): %v", zoneID, err)
		return false
	}
	metrics.ObserveForeignZoneRegistryRead(zoneID, metrics.ForeignZoneRegistryReadOK)
	since, absent := foreignZoneAbsence.observe(strconv.FormatUint(uint64(zoneID), 10), len(entries) > 0)
	if !absent {
		return false
	}
	if !barrierElapsedSince(svcCtx, since, foreignZoneAbsence.now()) {
		metrics.ObserveReentryBarrierBlocked(zoneID, site)
		return false
	}
	return true
}

// foreignRegisteredInstance 返回别的 zone 里 (zone,node) 唯一一条注册的实例 UUID(GO-3)。读失败、没有注册、
// 身份歧义或注册不带 UUID 时 ok=false。entries 是同一次读出的整份注册表,调用方据此再判盖章实例在不在册。
func foreignRegisteredInstance(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) (string, []nodeEntry, bool) {
	entries, err := fetchZoneRegistry(ctx, svcCtx, zoneID)
	if err != nil {
		metrics.ObserveForeignZoneRegistryRead(zoneID, metrics.ForeignZoneRegistryReadError)
		logx.Errorf("[ForeignZone] cannot read zone %d registry for the instance check of node %s: %v", zoneID, nodeID, err)
		return "", nil, false
	}
	metrics.ObserveForeignZoneRegistryRead(zoneID, metrics.ForeignZoneRegistryReadOK)
	instance, matches := "", 0
	for _, entry := range entries {
		if entry.nodeID == nodeID {
			instance = entry.reg.NodeUuid
			matches++
		}
	}
	return instance, entries, matches == 1 && instance != ""
}

// entriesRegisterInstance:这份注册表里有没有任何条目登记着这个实例。
func entriesRegisterInstance(entries []nodeEntry, instance string) bool {
	for _, entry := range entries {
		if entry.reg.NodeUuid == instance {
			return true
		}
	}
	return false
}

// foreignZoneOpensWorldMap 是跨 zone 传送第一条腿上「目标图在目标 zone 开没开」的与 zone 无关的判定(§4.3 第 2 条)。
// 目标 zone 恒不是本 zone,不读它的频道集合。开着的条件:confID 是 World 表里的图,且 etcd 里目标 zone 至少有一个能
// 承载大世界的场景节点(本进程不按类型分池时任何节点都算,与 node_selection.go 的 StrictNodeTypeSeparation 同口径)。
// 每个 zone 都给 World 表里的每张图铺频道(世界频道初始化),所以这与原来「频道集合非空」只差在目标 zone 刚起、
// 领导者还没铺频道的那几秒。err 非空 = 读不到注册表,调用方按原有口径放行(这道检查不是安全门)。
func foreignZoneOpensWorldMap(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) (bool, error) {
	if travelMapOpenOverride != nil {
		return travelMapOpenOverride(zoneID, confID)
	}
	if !worldConfInTable(confID) {
		return false, nil
	}
	entries, err := fetchZoneRegistry(ctx, svcCtx, zoneID)
	if err != nil {
		metrics.ObserveForeignZoneRegistryRead(zoneID, metrics.ForeignZoneRegistryReadError)
		return false, err
	}
	metrics.ObserveForeignZoneRegistryRead(zoneID, metrics.ForeignZoneRegistryReadOK)
	for _, entry := range entries {
		if !svcCtx.Config.StrictNodeTypeSeparation || constants.IsWorldHostingType(entry.reg.SceneNodeType) {
			return true, nil
		}
	}
	return false, nil
}

// worldConfInTable:confID 是不是 World 表里的一张图(走 worldConfIds,单测可替换)。
func worldConfInTable(confID uint64) bool {
	for _, id := range worldConfIds() {
		if id == confID {
			return true
		}
	}
	return false
}
