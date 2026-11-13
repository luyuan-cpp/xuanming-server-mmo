package logic

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"scene_manager/internal/constants"
	"scene_manager/internal/svc"
)

// 进场时按需铺大世界频道(docs/design/control-plane-zone-scoping-20261105.md §4.3 第 2 条、§6 第 1 条)。
//
// 世界频道平时由领导者铺:节点出现(watch PUT)、全量同步、自动扩缩容都会调 initWorldScenesForZone。zone 刚起、领导者
// 还没铺完的那几秒里,进场找不到频道就失败,客户端只提示「进入场景失败,请稍后再试」,玩家得自己重进。跨 zone 传送的
// 第二条腿更糟:源 scene 已经销毁了实体,玩家没有原地可回。CreateScene 早有同样的慢路径(createscenelogic.go
// 「Slow path: no channels exist」),EnterScene 走同一个:一张大世界图一个频道都没有时,在 zone 锁内补铺,再挑一次。
//
// 补铺只用负载集里的节点(getNodesForPurpose):负载集只有领导者写,体现了它对节点存活的裁决(死节点、再入屏障)。
// 负载集里还没有可用节点、而本副本的 etcd 镜像里已经有可用的大世界节点注册,说明领导者还没完成第一次同步:在请求期限内
// 按退避等它把节点放进负载集,不拿镜像自己挑节点。镜像里也没有,就照原来的口径立刻失败,不等。
//
// 同一进程里同一 (zone, conf) 同一时刻只补一次,同时到的请求等它结束,做法同 channel_move_flight.go(键与返回值不同,
// 所以各写各的)。多副本之间由 zone 锁串行,后拿到锁的一方重查集合、只补缺的。正确性仍由 zone 锁与 initWorldScenesForZone
// 的幂等保证(C++ 侧 CreateScene 按 scene_id 幂等),这里只省重复的活。

// worldChannelWarmupWait 是等领导者把节点放进负载集的上限;请求自己的期限更短时以请求为准。EnterScene 的服务端超时是
// 8 秒(etc/scene_manager_service.yaml 的 Timeout),选主加第一次全量同步通常在一两秒内;等满还没有,按原口径失败。
const worldChannelWarmupWait = 5 * time.Second

// 等待的退避:100ms 起,每次翻倍,封顶 1 秒,±20% 抖动(同一时刻进场的玩家不要整齐地一起重查)。
const (
	worldChannelWarmupFirstBackoff = 100 * time.Millisecond
	worldChannelWarmupMaxBackoff   = time.Second
)

// worldChannelWarmupSleep 等 d 或 ctx 结束,返回 false = ctx 先结束。单测替换它,不用真的等。
var worldChannelWarmupSleep = func(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// reserveWorldChannelCreatingOnDemand 是进场挑大世界频道的入口。先照常挑(ReserveBestWorldChannelForEnter);只有这张图是
// World 表里的图、而它在 zoneID 里一个频道都没有时,按需补铺一次再挑。其余结果原样返回:挑到了、读失败,
// 以及频道在却一个都用不了(都满、属主都死了且没有活节点接手)—— 后几种补铺帮不上,由自动扩缩容与懒改派处理。
func reserveWorldChannelCreatingOnDemand(ctx context.Context, svcCtx *svc.ServiceContext, confID uint64, zoneID uint32,
	playerID uint64) (uint64, string, error) {
	sceneID, nodeID, err := ReserveBestWorldChannelForEnter(ctx, svcCtx, confID, zoneID, playerID)
	if err != nil || sceneID != 0 || !worldConfInTable(confID) {
		return sceneID, nodeID, err
	}
	// 挑回 0 而没有错误有好几种原因(见上),补铺只针对「集合是空的」这一种;读不到集合就不补。
	if channels, scardErr := svcCtx.Redis.Scard(worldChannelsKey(zoneID, confID)); scardErr != nil || channels > 0 {
		return sceneID, nodeID, err
	}
	ensureWorldChannelsOnDemand(ctx, svcCtx, zoneID, confID)
	return ReserveBestWorldChannelForEnter(ctx, svcCtx, confID, zoneID, playerID)
}

// worldChannelOnDemandKey 是按需补铺的合并键。
type worldChannelOnDemandKey struct {
	zoneID uint32
	confID uint64
}

// worldChannelOnDemandFlight 是一次在跑的补铺。done 在补铺结束时关闭。
type worldChannelOnDemandFlight struct {
	done chan struct{}
	// waiters 是等这一次的请求数,只在 worldChannelOnDemandMu 下读写。
	waiters int
}

var (
	worldChannelOnDemandMu sync.Mutex
	// worldChannelOnDemandFlights:在跑的补铺。一次结束就删,不会累积。
	worldChannelOnDemandFlights = make(map[worldChannelOnDemandKey]*worldChannelOnDemandFlight)
)

// ensureWorldChannelsOnDemand 在 zoneID 里为 confID 补铺频道,本进程同一 (zone, conf) 只跑一次。返回时不保证铺成了
// (zone 锁等超时、没有可用节点、请求期限到了),调用方再挑一次,挑不到照原口径失败。
func ensureWorldChannelsOnDemand(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) {
	joinWorldChannelOnDemand(ctx, worldChannelOnDemandKey{zoneID: zoneID, confID: confID}, func() {
		layWorldChannelsOnDemand(ctx, svcCtx, zoneID, confID)
	})
}

// joinWorldChannelOnDemand:本进程里同一 key 已经有一次在跑时不再跑 run,等它结束;等待的一方按自己的 ctx 放弃等待。
// 在跑的那一次用发起者自己的 ctx(同 joinChannelMove):发起者被取消时补铺提前结束,等待者照样醒来再挑一次,
// 挑不到由上游重试,重试时再发起一次。
func joinWorldChannelOnDemand(ctx context.Context, key worldChannelOnDemandKey, run func()) {
	worldChannelOnDemandMu.Lock()
	if flight, running := worldChannelOnDemandFlights[key]; running {
		flight.waiters++
		worldChannelOnDemandMu.Unlock()
		select {
		case <-flight.done:
		case <-ctx.Done():
		}
		return
	}
	flight := &worldChannelOnDemandFlight{done: make(chan struct{})}
	worldChannelOnDemandFlights[key] = flight
	worldChannelOnDemandMu.Unlock()

	defer func() {
		worldChannelOnDemandMu.Lock()
		delete(worldChannelOnDemandFlights, key)
		waiters := flight.waiters
		worldChannelOnDemandMu.Unlock()
		close(flight.done)
		if waiters > 0 {
			logx.Infof("[World] On-demand channel creation for zone %d conf %d shared with %d concurrent requests",
				key.zoneID, key.confID, waiters)
		}
	}()
	run()
}

// layWorldChannelsOnDemand 是一次补铺:等负载集里有可用的大世界节点(只在本副本镜像里已经有可用注册时等),
// 集合仍是空的就在 zone 锁内铺(waitForLock=true:别的副本正在铺时等它铺完,拿到锁后重查集合、只补缺的)。
func layWorldChannelsOnDemand(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) {
	waitCtx, cancel := context.WithTimeout(ctx, worldChannelWarmupWait)
	defer cancel()
	backoff := worldChannelWarmupFirstBackoff
	for len(getNodesForPurpose(svcCtx, zoneID, constants.NodePurposeWorld)) == 0 {
		if !hasWorldHostingRegistration(mirroredRegistrationsOf(zoneID), svcCtx.Config.StrictNodeTypeSeparation) {
			logx.Infof("[World] Zone %d has no world-hosting node registered; not creating channels of conf %d on demand",
				zoneID, confID)
			return
		}
		if !worldChannelWarmupSleep(waitCtx, jitteredWarmupBackoff(backoff)) {
			logx.Errorf("[World] Zone %d: world-hosting nodes are registered but none is in the load set yet (leader not synced); gave up creating channels of conf %d on demand",
				zoneID, confID)
			return
		}
		backoff = min(2*backoff, worldChannelWarmupMaxBackoff)
	}
	// 等的这段时间里领导者可能已经铺好了:再看一眼,铺好了就不再对每个频道发一遍 CreateScene。
	if channels, err := svcCtx.Redis.Scard(worldChannelsKey(zoneID, confID)); err != nil || channels > 0 {
		return
	}
	logx.Infof("[World] Zone %d has no channel of world conf %d; creating them on demand for an entering player", zoneID, confID)
	initWorldScenesForZone(ctx, svcCtx, zoneID, []uint64{confID}, true)
}

// jitteredWarmupBackoff 给退避加 ±20% 抖动。
func jitteredWarmupBackoff(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// mirroredRegistrationsOf 返回本副本 etcd 镜像里 zoneID 的注册条目(拷贝,调用方随便读)。
func mirroredRegistrationsOf(zoneID uint32) []nodeEntry {
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()
	var entries []nodeEntry
	for _, entry := range knownNodes {
		if entry.reg.ZoneId == zoneID {
			entries = append(entries, entry)
		}
	}
	return entries
}
