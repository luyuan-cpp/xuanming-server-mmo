package logic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"scene_manager/internal/agones"
	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// SceneAgonesGsKeyFmt 记录一个 Scene 是在哪个 Agones GameServer 上开的。
//
// 这个映射是**回滚的唯一依据**:创建失败、节点死亡、销毁时都要按
// gameServerName 精确把 rooms 计数减回去。不存这一条就只能猜,猜错等于
// 把别的进程的容量凭空放大。
const SceneAgonesGsKeyFmt = "scene:%d:agones_gs"

func sceneAgonesGsKey(sceneID uint64) string {
	return fmt.Sprintf(SceneAgonesGsKeyFmt, sceneID)
}

// 全局分配器句柄。用 setter 而不是塞进 ServiceContext,是为了让单元测试
// 能在不构造整个 svc.ServiceContext 的前提下注入 fake。
var (
	agonesAllocatorMu sync.RWMutex
	agonesAllocator   agones.Allocator
)

// SetAgonesAllocator 注入分配器。传 nil 表示不启用。
// 进程启动时调用一次;测试里配合 t.Cleanup 还原。
func SetAgonesAllocator(a agones.Allocator) {
	agonesAllocatorMu.Lock()
	defer agonesAllocatorMu.Unlock()
	agonesAllocator = a
}

// GetAgonesAllocator 返回当前分配器,未启用时返回 nil。
func GetAgonesAllocator() agones.Allocator {
	agonesAllocatorMu.RLock()
	defer agonesAllocatorMu.RUnlock()
	return agonesAllocator
}

// AgonesEnabled 报告是否走 Agones 分配路径。
//
// 注意这里同时要求配置打开**且**分配器已注入:配置说要用但分配器没构造出来
// (in-cluster config 拿不到之类)时返回 false 会静默绕过容量约束,
// 所以构造失败的处理放在启动路径 —— 要么起不来,要么明确把配置关掉。
func AgonesEnabled(svcCtx *svc.ServiceContext) bool {
	return svcCtx.Config.Agones.Enabled && GetAgonesAllocator() != nil
}

// agonesNamespace 解析 Fleet 所在命名空间。
func agonesNamespace(svcCtx *svc.ServiceContext) string {
	if ns := svcCtx.Config.Agones.Namespace; ns != "" {
		return ns
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

// agonesZoneLabel 返回 Fleet 上 mmorpg.io/zone 标签应有的值。
//
// 部署生成器(k8s_deploy.ps1)写的是 zone **名字**,同时还写了
// mmorpg.io/zone-id。SceneManager 手里只有 zone_id,所以这里按 zone-id 匹配。
func agonesZoneLabel(zoneID uint32) string {
	return strconv.FormatUint(uint64(zoneID), 10)
}

// agonesRoleLabel 把 NodePurpose 翻成 Fleet 上的 mmorpg.io/role。
func agonesRoleLabel(purpose constants.NodePurpose) string {
	if purpose == constants.NodePurposeWorld {
		return "world"
	}
	return "instance"
}

// agonesPlacement 是一次成功预占的结果:既拿到了 Agones 的名额,
// 也确认了这个 Pod 就是 SceneManager 已知的那个 scene node。
type agonesPlacement struct {
	NodeID         string
	GameServerName string
	Namespace      string
}

// AcquireAgonesPlacement 走完整的"预占 + 映射 + 校验"链路。
//
// 顺序是刻意的:
//  1. 创建 GSA,rooms 在 Agones 侧原子 +1
//  2. status 必须是 Allocated
//  3. 取 gameServerName / PodIP / counter 结果
//  4. PodIP 映射回 knownNodes
//  5. 校验 zone 与 role
//
// 4/5 任何一步失败都会把刚预占的名额还回去 —— 不还就是永久漂移。
// 返回的 placement 里带 GameServerName,调用方必须把它写进
// scene:{id}:agones_gs,否则后续回滚无从下手。
func AcquireAgonesPlacement(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	zoneID uint32,
	purpose constants.NodePurpose,
) (*agonesPlacement, error) {
	allocator := GetAgonesAllocator()
	if allocator == nil {
		return nil, agones.ErrNotConfigured
	}

	ns := agonesNamespace(svcCtx)
	zoneLabel := agonesZoneLabel(zoneID)
	roleLabel := agonesRoleLabel(purpose)

	start := time.Now()
	result, err := allocator.Allocate(ctx, agones.AllocationRequest{
		Namespace:   ns,
		ZoneLabel:   zoneLabel,
		RoleLabel:   roleLabel,
		BuildLabel:  svcCtx.Config.Agones.BuildLabel,
		RoomsAmount: 1,
	})
	elapsed := time.Since(start)

	if err != nil {
		outcome := metrics.AgonesOutcomeError
		if isNoCapacity(err) {
			outcome = metrics.AgonesOutcomeNoCapacity
		}
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, outcome, elapsed)

		// Allocate 可能"名额已经加上了,但 PodIP 解析失败"。这种情况下
		// result 非 nil 且带 GameServerName —— 必须还回去。
		if result != nil && result.GameServerName != "" {
			ReleaseAgonesRoom(ctx, svcCtx, ns, result.GameServerName, "allocate_partial_failure")
			metrics.ObserveAgonesMappingFailure(zoneLabel, "no_pod_ip")
		}
		return nil, err
	}

	if result == nil || result.PodIP == "" {
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeMappingFailed, elapsed)
		metrics.ObserveAgonesMappingFailure(zoneLabel, "no_pod_ip")
		if result != nil && result.GameServerName != "" {
			ReleaseAgonesRoom(ctx, svcCtx, ns, result.GameServerName, "no_pod_ip")
		}
		return nil, fmt.Errorf("agones: allocation returned no pod ip")
	}

	nodeID, entryZone, entryType, ok := FindNodeByPodIP(result.PodIP)
	if !ok {
		// GSA 成功但这个 Pod 还没把自己注册进 etcd(启动竞态),或者
		// 注册已经过期。fail-closed:还名额、拒绝本次创建、让调用方重试。
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeMappingFailed, elapsed)
		metrics.ObserveAgonesMappingFailure(zoneLabel, "unknown_pod_ip")
		ReleaseAgonesRoom(ctx, svcCtx, ns, result.GameServerName, "unknown_pod_ip")
		return nil, fmt.Errorf("agones: pod ip %s (gs=%s) is not a registered scene node yet",
			result.PodIP, result.GameServerName)
	}

	if entryZone != zoneID {
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeMappingFailed, elapsed)
		metrics.ObserveAgonesMappingFailure(zoneLabel, "zone_mismatch")
		ReleaseAgonesRoom(ctx, svcCtx, ns, result.GameServerName, "zone_mismatch")
		return nil, fmt.Errorf("agones: allocated gs=%s belongs to zone %d, requested %d",
			result.GameServerName, entryZone, zoneID)
	}

	// role 校验:Fleet 标签和 C++ 侧 SCENE_NODE_TYPE 是两条独立链路
	// (一个来自 YAML label,一个来自容器 env + etcd 注册)。两者不一致
	// 说明模板写错了,放行会让 world 的房间跑到 instance 进程上。
	if !constants.MatchesPurpose(entryType, purpose) {
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeMappingFailed, elapsed)
		metrics.ObserveAgonesMappingFailure(zoneLabel, "role_mismatch")
		ReleaseAgonesRoom(ctx, svcCtx, ns, result.GameServerName, "role_mismatch")
		return nil, fmt.Errorf("agones: allocated gs=%s has scene_node_type=%d which does not match purpose %d",
			result.GameServerName, entryType, purpose)
	}

	metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeOK, elapsed)
	logx.Infof("[Agones] allocated gs=%s pod=%s node=%s zone=%d role=%s rooms=%d/%d",
		result.GameServerName, result.PodIP, nodeID, zoneID, roleLabel,
		result.RoomsCount, result.RoomsCapacity)

	return &agonesPlacement{
		NodeID:         nodeID,
		GameServerName: result.GameServerName,
		Namespace:      ns,
	}, nil
}

// AcquireAgonesRoomOnNode 在一个**指定**的 scene node 上预占房间名额。
//
// 用于镜像共置:镜像必须和源 Scene 同进程,这条业务约束优先于"让 Agones
// 自由挑进程"。链路是 nodeID -> PodIP(knownNodes)-> GameServer 名字
// (ListGameServerRooms 按 PodIP 反查)-> 对该 GameServer 的 rooms 做
// 带 CAS 的 +1。
//
// 任何一环断掉(节点没注册、Agones 版本不给 Pod 地址、容量满)都返回错误,
// 由调用方回落到自由分配 —— 不允许"找不到就直接放行",那会绕过容量约束。
func AcquireAgonesRoomOnNode(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	nodeID string,
	zoneID uint32,
) (*agonesPlacement, error) {
	allocator := GetAgonesAllocator()
	if allocator == nil {
		return nil, agones.ErrNotConfigured
	}

	podIP, ok := FindPodIPByNodeID(zoneID, nodeID)
	if !ok || podIP == "" {
		return nil, fmt.Errorf("agones: node %s has no known pod ip", nodeID)
	}

	ns := agonesNamespace(svcCtx)
	zoneLabel := agonesZoneLabel(zoneID)

	servers, err := allocator.ListGameServerRooms(ctx, ns, zoneLabel)
	if err != nil {
		return nil, fmt.Errorf("agones: list gameservers for co-location: %w", err)
	}

	gsName := ""
	for _, gs := range servers {
		if gs.PodIP == podIP {
			gsName = gs.Name
			break
		}
	}
	if gsName == "" {
		// 老版本 Agones 的 GameServer status 里没有 Pod 地址,这里就查不到。
		// 记一次映射失败,让调用方降级为自由分配(镜像失去共置优化但可用)。
		metrics.ObserveAgonesMappingFailure(zoneLabel, "unknown_pod_ip")
		return nil, fmt.Errorf("agones: no gameserver matches pod ip %s (node %s)", podIP, nodeID)
	}

	start := time.Now()
	if err := allocator.AcquireRoomOnGameServer(ctx, ns, gsName, 1); err != nil {
		outcome := metrics.AgonesOutcomeError
		if isNoCapacity(err) {
			outcome = metrics.AgonesOutcomeNoCapacity
		}
		metrics.ObserveAgonesAllocation(zoneLabel, "colocate", outcome, time.Since(start))
		return nil, err
	}
	metrics.ObserveAgonesAllocation(zoneLabel, "colocate", metrics.AgonesOutcomeOK, time.Since(start))

	return &agonesPlacement{
		NodeID:         nodeID,
		GameServerName: gsName,
		Namespace:      ns,
	}, nil
}

// ReserveAgonesRoomForWorldChannel 为一个**世界频道**预占房间名额,并把
// scene:{id}:agones_gs 记下来。
//
// 世界频道与副本的分配策略不同,不能直接复用 AcquireAgonesPlacement:
// 频道的落点是 assignNodeByHash 的一致性哈希结果,rebalance 依赖这个分布
// 保持稳定;让 GSA 自由挑会和 rebalance 互相打架(它挑一个、rebalance 又
// 想搬回哈希目标)。所以这里先按**指定节点**预占,只有那个节点没容量时
// 才回落到自由分配,并把实际落点返回给调用方。
//
// 返回 (实际节点, 是否成功)。Agones 未启用时返回 (preferredNode, true) —— 不做任何事。
//
// 没有这一步的后果:世界频道不占 rooms 名额 -> Counter 少算 ->
// FleetAutoscaler(policy=Counter,key=rooms)对大世界的人数增长完全无感,
// Pod 永远不会为世界负载扩容。
func ReserveAgonesRoomForWorldChannel(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	sceneID uint64,
	preferredNode string,
	zoneID uint32,
) (string, bool) {
	if !AgonesEnabled(svcCtx) {
		return preferredNode, true
	}

	placement, err := AcquireAgonesRoomOnNode(ctx, svcCtx, preferredNode, zoneID)
	if err != nil {
		logx.Infof("[Agones] world channel %d: hash-target node %s cannot take a room (%v), "+
			"falling back to free allocation (rebalance will re-home it later)", sceneID, preferredNode, err)

		placement, err = AcquireAgonesPlacement(ctx, svcCtx, zoneID, constants.NodePurposeWorld)
		if err != nil {
			logx.Errorf("[Agones] world channel %d: no room capacity in zone %d: %v", sceneID, zoneID, err)
			return "", false
		}
	}

	// 必须在调 C++ CreateScene 之前写:中途崩溃时这是唯一能找回
	// "该向哪个 GameServer 归还名额" 的依据。
	if err := svcCtx.Redis.Set(sceneAgonesGsKey(sceneID), placement.GameServerName); err != nil {
		logx.Errorf("[Agones] world channel %d: failed to record agones gs: %v", sceneID, err)
	}
	return placement.NodeID, true
}

// TransferAgonesRoomForScene 把一个场景的房间名额从旧节点转到新节点。
//
// 世界频道 rebalance 会把频道从 oldNode 搬到 newNode。不转移名额的话:
// 旧 GameServer 的计数永远不还(容量凭空蒸发),新 GameServer 没记账
// (容量被超卖)。两个方向都会让 FleetAutoscaler 算错。
//
// 顺序是**先占新的再还旧的**:反过来的话,中间窗口里新节点可能已经没容量了,
// 于是名额两头都不在,频道变成"不占任何容量"的幽灵。
func TransferAgonesRoomForScene(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	sceneID uint64,
	newNode string,
	zoneID uint32,
) {
	if !AgonesEnabled(svcCtx) {
		return
	}

	oldGs, _ := svcCtx.Redis.Get(sceneAgonesGsKey(sceneID))

	placement, err := AcquireAgonesRoomOnNode(ctx, svcCtx, newNode, zoneID)
	if err != nil {
		// 新节点占不到名额。**不**释放旧的 —— 释放了就等于这个频道不占任何
		// 容量,比多占一份更糟。留给 reconcile 去暴露漂移。
		logx.Errorf("[Agones] scene %d: migrated to node %s but could not reserve a room there (%v); "+
			"keeping the old reservation on %s, watch scene_manager_agones_counter_drift",
			sceneID, newNode, err, oldGs)
		return
	}

	if err := svcCtx.Redis.Set(sceneAgonesGsKey(sceneID), placement.GameServerName); err != nil {
		logx.Errorf("[Agones] scene %d: failed to update agones gs mapping: %v", sceneID, err)
	}

	if oldGs != "" && oldGs != placement.GameServerName {
		ReleaseAgonesRoom(ctx, svcCtx, agonesNamespace(svcCtx), oldGs, "world_channel_migrated")
	}
}

// ReleaseAgonesRoom 把一个房间名额还给 Agones。
//
// 最终失败**不是**只打条日志就算了:计数从此漂移,只有 reconcile 才能发现。
// 所以这里同时记指标(agones_counter_rollback_total{outcome="failed"})并打
// ERROR,运维应当对这个指标配告警。
func ReleaseAgonesRoom(ctx context.Context, svcCtx *svc.ServiceContext, namespace, gameServerName, reason string) {
	if gameServerName == "" {
		return
	}
	allocator := GetAgonesAllocator()
	if allocator == nil {
		return
	}
	if namespace == "" {
		namespace = agonesNamespace(svcCtx)
	}

	if err := allocator.ReleaseRoom(ctx, namespace, gameServerName, 1); err != nil {
		metrics.ObserveAgonesCounterRollback("failed")
		logx.Errorf("[Agones] COUNTER DRIFT: failed to release room on %s/%s (reason=%s): %v; "+
			"this will NOT self-heal, watch scene_manager_agones_counter_drift",
			namespace, gameServerName, reason, err)
		return
	}
	metrics.ObserveAgonesCounterRollback("ok")
	logx.Infof("[Agones] released room on %s/%s (reason=%s)", namespace, gameServerName, reason)
}

// ReleaseAgonesRoomForScene 按 scene id 回滚。读 scene:{id}:agones_gs 拿到
// 精确的 GameServer 名字。没有映射(非 Agones 模式创建的 Scene)时是 no-op。
func ReleaseAgonesRoomForScene(ctx context.Context, svcCtx *svc.ServiceContext, sceneID uint64, gsName, reason string) {
	if GetAgonesAllocator() == nil {
		return
	}
	if gsName == "" {
		gsName, _ = svcCtx.Redis.Get(sceneAgonesGsKey(sceneID))
	}
	if gsName == "" {
		return
	}
	ReleaseAgonesRoom(ctx, svcCtx, agonesNamespace(svcCtx), gsName, reason)
}

// isNoCapacity 区分"没容量"和"基础设施出错"。前者是正常的背压,
// 后者要单独告警。ErrNoCapacity 被 %w 包过一层,所以用 errors.Is。
func isNoCapacity(err error) bool {
	return errors.Is(err, agones.ErrNoCapacity)
}
