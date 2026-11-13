package logic

import (
	"context"
	"encoding/json"
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

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

// SceneAgonesGsKeyFmt 记录一个 Scene 占的 Agones 名额:"<GameServer 名>/<条目>"(agonesRoom,
// 见 agones_room_ledger.go);非高密度模式只有 GameServer 名。
//
// 这个记录是**归还的唯一依据**:创建失败、节点死亡、销毁时都要按它精确删掉那一条。不存这一条就只能猜,
// 猜错等于把别的进程的容量凭空放大。键是 scene:{ctl:<z>}:<id>:agones_gs,%s 是 sceneControlTag(zone)。
const SceneAgonesGsKeyFmt = "scene:%s:%d:agones_gs"

func sceneAgonesGsKey(zoneID uint32, sceneID uint64) string {
	return fmt.Sprintf(SceneAgonesGsKeyFmt, sceneControlTag(zoneID), sceneID)
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

// agonesPlacement 是一次成功预占的结果:既拿到了 Agones 的名额(GameServer 上的一个条目,在途占用已写),
// 也确认了这个 Pod 就是 SceneManager 已知的那个 scene node。
//
// 调用方接下来要么把 room().record() 写进记录(登记脚本、改派转移的 CAS,它们在同一步消费在途占用),
// 要么调 abandonAgonesPlacement 放弃。两样都不做,名额就由对账在在途占用过期之后收回。
type agonesPlacement struct {
	NodeID         string
	GameServerName string
	Namespace      string
	// Entry 是这次占用的条目;非高密度模式为空(不记名额)。
	Entry string
}

// room 是这次占用的名额,写进 scene:{id}:agones_gs 的就是它的 record()。
func (p *agonesPlacement) room() agonesRoom {
	return agonesRoom{GameServer: p.GameServerName, Entry: p.Entry}
}

// beginAgonesRoom 为一次占用生成条目并写在途占用(agones_room_ledger.go)。非高密度模式不记名额,回空串。
// 回错误时不能去要名额:没有在途占用的条目会被对账当成孤儿删掉。
func beginAgonesRoom(svcCtx *svc.ServiceContext, zoneID uint32, sceneID uint64) (string, error) {
	if !svcCtx.Config.Agones.HighDensityEnabled {
		return "", nil
	}
	entry := newAgonesRoomEntry(sceneID)
	if err := holdAgonesRoom(svcCtx, zoneID, entry); err != nil {
		return "", fmt.Errorf("agones: hold room entry for scene %d: %w", sceneID, err)
	}
	return entry, nil
}

// abandonAgonesRoom 放弃一次还没提交的占用:先删在途占用,再尽力删条目。删条目失败不用管:没有记录、也没有
// 在途占用的条目就是孤儿,对账下一轮收。room.GameServer 为空(不知道条目落在哪台上)时只删在途占用。
func abandonAgonesRoom(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, namespace string, room agonesRoom, reason string) {
	if room.Entry == "" {
		return
	}
	dropAgonesRoomHold(svcCtx, zoneID, room.Entry)
	if room.GameServer != "" {
		_ = releaseAgonesRoom(ctx, svcCtx, namespace, room, reason)
	}
}

// abandonAgonesPlacement 放弃一次成功预占、但决定不提交的名额(登记没落地等),见 abandonAgonesRoom。
func abandonAgonesPlacement(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, placement *agonesPlacement, reason string) {
	if placement == nil {
		return
	}
	abandonAgonesRoom(ctx, svcCtx, zoneID, placement.Namespace, placement.room(), reason)
}

// AcquireAgonesPlacement 走完整的"预占 + 映射 + 校验"链路。
//
// 顺序是刻意的:
//  1. 写在途占用(条目里有 sceneID)
//  2. 创建 GSA,选中 GameServer 与加条目在 Agones 侧是同一个操作;分配器核对过条目确实在列表里
//  3. status 必须是 Allocated
//  4. 取 gameServerName / PodIP
//  5. PodIP 映射回 knownNodes
//  6. 校验 zone 与 role
//
// 2 之后任何一步失败都放弃这一条(abandonAgonesRoom)—— 不放弃的话它要等在途占用过期才被对账收回。
// 返回的 placement 里带条目,调用方必须用登记脚本把 placement.room().record() 写进 scene:{id}:agones_gs,
// 否则后续归还无从下手。
func AcquireAgonesPlacement(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	zoneID uint32,
	sceneID uint64,
	purpose constants.NodePurpose,
) (*agonesPlacement, error) {
	allocator := GetAgonesAllocator()
	if allocator == nil {
		return nil, agones.ErrNotConfigured
	}

	ns := agonesNamespace(svcCtx)
	zoneLabel := agonesZoneLabel(zoneID)
	roleLabel := agonesRoleLabel(purpose)

	entry, err := beginAgonesRoom(svcCtx, zoneID, sceneID)
	if err != nil {
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeError, 0)
		return nil, err
	}
	abandon := func(gsName, reason string) {
		abandonAgonesRoom(ctx, svcCtx, zoneID, ns, agonesRoom{GameServer: gsName, Entry: entry}, reason)
	}

	start := time.Now()
	result, err := allocator.Allocate(ctx, agones.AllocationRequest{
		Namespace:  ns,
		ZoneLabel:  zoneLabel,
		RoleLabel:  roleLabel,
		BuildLabel: svcCtx.Config.Agones.BuildLabel,
		Entry:      entry,
	})
	elapsed := time.Since(start)

	if err != nil {
		outcome := metrics.AgonesOutcomeError
		if isNoCapacity(err) {
			outcome = metrics.AgonesOutcomeNoCapacity
		}
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, outcome, elapsed)

		// Allocate 可能"条目已经加上了,但 PodIP 解析或条目核对失败"。这种情况下 result 非 nil 且带
		// GameServerName —— 按条目删。没有 GameServerName 时(GSA 本身回错)条目可能落在某台上,也可能没有:
		// 只删在途占用,真落了的话它就是孤儿,对账下一轮收。
		gsName := ""
		if result != nil {
			gsName = result.GameServerName
		}
		if gsName != "" {
			metrics.ObserveAgonesMappingFailure(zoneLabel, "no_pod_ip")
		}
		abandon(gsName, "allocate_partial_failure")
		return nil, err
	}

	if result == nil || result.PodIP == "" {
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeMappingFailed, elapsed)
		metrics.ObserveAgonesMappingFailure(zoneLabel, "no_pod_ip")
		gsName := ""
		if result != nil {
			gsName = result.GameServerName
		}
		abandon(gsName, "no_pod_ip")
		return nil, fmt.Errorf("agones: allocation returned no pod ip")
	}

	nodeID, entryZone, entryType, ok := FindNodeByPodIP(result.PodIP)
	if !ok {
		// GSA 成功但这个 Pod 还没把自己注册进 etcd(启动竞态),或者
		// 注册已经过期。fail-closed:还名额、拒绝本次创建、让调用方重试。
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeMappingFailed, elapsed)
		metrics.ObserveAgonesMappingFailure(zoneLabel, "unknown_pod_ip")
		abandon(result.GameServerName, "unknown_pod_ip")
		return nil, fmt.Errorf("agones: pod ip %s (gs=%s) is not a registered scene node yet",
			result.PodIP, result.GameServerName)
	}

	if entryZone != zoneID {
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeMappingFailed, elapsed)
		metrics.ObserveAgonesMappingFailure(zoneLabel, "zone_mismatch")
		abandon(result.GameServerName, "zone_mismatch")
		return nil, fmt.Errorf("agones: allocated gs=%s belongs to zone %d, requested %d",
			result.GameServerName, entryZone, zoneID)
	}

	// role 校验:Fleet 标签和 C++ 侧 SCENE_NODE_TYPE 是两条独立链路
	// (一个来自 YAML label,一个来自容器 env + etcd 注册)。两者不一致
	// 说明模板写错了,放行会让 world 的房间跑到 instance 进程上。
	if !constants.MatchesPurpose(entryType, purpose) {
		metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeMappingFailed, elapsed)
		metrics.ObserveAgonesMappingFailure(zoneLabel, "role_mismatch")
		abandon(result.GameServerName, "role_mismatch")
		return nil, fmt.Errorf("agones: allocated gs=%s has scene_node_type=%d which does not match purpose %d",
			result.GameServerName, entryType, purpose)
	}

	metrics.ObserveAgonesAllocation(zoneLabel, roleLabel, metrics.AgonesOutcomeOK, elapsed)
	logx.Infof("[Agones] allocated gs=%s pod=%s node=%s zone=%d role=%s rooms=%d/%d scene=%d",
		result.GameServerName, result.PodIP, nodeID, zoneID, roleLabel,
		result.RoomsCount, result.RoomsCapacity, sceneID)

	return &agonesPlacement{
		NodeID:         nodeID,
		GameServerName: result.GameServerName,
		Namespace:      ns,
		Entry:          entry,
	}, nil
}

// AcquireAgonesRoomOnNode 在一个**指定**的 scene node 上为 sceneID 预占房间名额。
//
// 用于镜像共置(镜像必须和源 Scene 同进程,这条业务约束优先于"让 Agones 自由挑进程")、改派之后把名额转到
// 新节点、对账把名额转过来。链路是 nodeID -> PodIP(knownNodes)-> GameServer 名字(ListGameServerRooms
// 按 PodIP 反查)-> 写在途占用 -> 在这台 GameServer 的 rooms 列表里加条目(带 CAS,满了拒绝)。
//
// 任何一环断掉(节点没注册、Agones 版本不给 Pod 地址、容量满)都返回错误,
// 由调用方回落到自由分配 —— 不允许"找不到就直接放行",那会绕过容量约束。
func AcquireAgonesRoomOnNode(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	nodeID string,
	zoneID uint32,
	sceneID uint64,
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

	return acquireAgonesRoomOnGameServer(ctx, svcCtx, zoneID, sceneID, nodeID, gsName)
}

// acquireAgonesRoomOnGameServer 在已知的 GameServer(nodeID 所在的那台)上为 sceneID 占一个名额:写在途占用,
// 再在它的 rooms 列表里加条目。对账已经从列表快照里知道节点对应哪台 GameServer,直接用这一步,不再每个场景 LIST 一次。
func acquireAgonesRoomOnGameServer(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	zoneID uint32,
	sceneID uint64,
	nodeID, gsName string,
) (*agonesPlacement, error) {
	allocator := GetAgonesAllocator()
	if allocator == nil {
		return nil, agones.ErrNotConfigured
	}
	ns := agonesNamespace(svcCtx)
	zoneLabel := agonesZoneLabel(zoneID)

	entry, err := beginAgonesRoom(svcCtx, zoneID, sceneID)
	if err != nil {
		metrics.ObserveAgonesAllocation(zoneLabel, "colocate", metrics.AgonesOutcomeError, 0)
		return nil, err
	}

	start := time.Now()
	if err := allocator.AddRoomEntry(ctx, ns, gsName, entry); err != nil {
		outcome := metrics.AgonesOutcomeError
		if isNoCapacity(err) {
			outcome = metrics.AgonesOutcomeNoCapacity
		}
		metrics.ObserveAgonesAllocation(zoneLabel, "colocate", outcome, time.Since(start))
		// 结局未知的失败(请求已生效、回包丢了)也按条目删,删两次无害。
		abandonAgonesRoom(ctx, svcCtx, zoneID, ns, agonesRoom{GameServer: gsName, Entry: entry}, "colocate_failed")
		return nil, err
	}
	metrics.ObserveAgonesAllocation(zoneLabel, "colocate", metrics.AgonesOutcomeOK, time.Since(start))

	return &agonesPlacement{
		NodeID:         nodeID,
		GameServerName: gsName,
		Namespace:      ns,
		Entry:          entry,
	}, nil
}

// ReserveAgonesRoomForWorldChannel 为一个新建的**世界频道**预占房间名额:先按指定节点占,那个节点没容量时回落自由分配。
// 见 reserveAgonesRoomForWorldChannel。
//
// 没有这一步的后果:世界频道不占 rooms 名额 -> 列表少算 ->
// FleetAutoscaler(policy=List,key=rooms)对大世界的人数增长完全无感,
// Pod 永远不会为世界负载扩容。
func ReserveAgonesRoomForWorldChannel(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	sceneID uint64,
	preferredNode string,
	zoneID uint32,
) (string, *agonesPlacement, bool) {
	return reserveAgonesRoomForWorldChannel(ctx, svcCtx, zoneID, sceneID, preferredNode, true)
}

// reserveAgonesRoomForWorldChannel 在把世界频道放到 preferredNode 之前,先在那台 GameServer 上占名额(写在途占用、加条目)。
// 新建频道与四条改派路径都先占名额再放:不把房间塞进 Agones 认为已经满了的进程(docs/design/agones-scene-node-high-density.md
// §13.10)。
//
// 世界频道与副本的分配策略不同,不能直接复用 AcquireAgonesPlacement:频道的落点是 assignNodeByHash 的一致性哈希结果,
// rebalance 依赖这个分布保持稳定;让 GSA 自由挑会和 rebalance 互相打架(它挑一个、rebalance 又想搬回哈希目标)。
// 所以先按**指定节点**占。那个节点没容量时:
//   - allowFallback 为 true(新建频道,或属主已死、频道必须搬走):回落到 GSA 自由分配,在本 zone 挑一台有空位、承载大世界的
//     GameServer,返回它对应的节点。落点偏离哈希目标的,之后由 rebalance 搬回去;
//   - allowFallback 为 false(rebalance 换更好的家):不放,搬到别处就违背了这次迁移的目的。
//
// 返回 (实际节点, 预占的名额, 是否成功)。Agones 未启用时返回 (preferredNode, nil, true) —— 不做任何事。
// 返回 false 时一个名额都没留:调用方这次不放。成功时调用方要么把名额写进记录(新建:登记脚本;改派:CAS 落地之后
// commitReservedAgonesRoom),要么 abandonAgonesPlacement 放弃。
func reserveAgonesRoomForWorldChannel(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	zoneID uint32,
	sceneID uint64,
	preferredNode string,
	allowFallback bool,
) (string, *agonesPlacement, bool) {
	if !AgonesEnabled(svcCtx) {
		return preferredNode, nil, true
	}

	placement, err := AcquireAgonesRoomOnNode(ctx, svcCtx, preferredNode, zoneID, sceneID)
	if err == nil {
		return preferredNode, placement, true
	}
	if !allowFallback {
		logx.Infof("[Agones] world channel %d: node %s cannot take a room (%v); not placing it there", sceneID, preferredNode, err)
		return "", nil, false
	}
	logx.Infof("[Agones] world channel %d: hash-target node %s cannot take a room (%v), "+
		"falling back to free allocation (rebalance will re-home it later)", sceneID, preferredNode, err)

	placement, err = AcquireAgonesPlacement(ctx, svcCtx, zoneID, sceneID, constants.NodePurposeWorld)
	if err != nil {
		logx.Errorf("[Agones] world channel %d: no room capacity in zone %d: %v", sceneID, zoneID, err)
		return "", nil, false
	}
	return placement.NodeID, placement, true
}

// commitReservedAgonesRoom 在改派 CAS 落地之后,把改派之前预占的名额提交进记录(commitAgonesRoomTransfer:属主仍是 newNode
// 才改记录、同一步消费在途占用,改成了就删旧条目)。placement 为 nil(没开 Agones)时什么都不做。
// 调用方传不随请求取消的 ctx:K8s 删旧条目可能已经生效、本端却拿到错误。
func commitReservedAgonesRoom(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, sceneID uint64,
	newNode string, placement *agonesPlacement) {
	if placement == nil {
		return
	}
	commitAgonesRoomTransfer(ctx, svcCtx, sceneID, newNode, zoneID, placement)
}

// luaCommitAgonesRoomTransfer 是名额记录改写的比较并交换:只有 scene:{id}:node 仍是 newNode
// (归属确实落在新节点,场景也没被销毁),才把 scene:{id}:agones_gs 改成新记录,并在同一个脚本里
// 原子读出被覆盖的旧记录。同一步消费这次占用的在途占用(agones_room_ledger.go)。
//
// KEYS[1] = scene:{id}:node
// KEYS[2] = scene:{id}:agones_gs
// KEYS[3] = scene:{id}:agones_transfer_receipt:{token},本次调用的回执(见下)
// KEYS[4] = 本 zone 的在途占用 agones_rooms:{ctl:<z>}:holds(四把键同一个 zone 的 tag,同槽)
// ARGV[1] = newNode
// ARGV[2] = 新记录「新 GameServer/条目」
// ARGV[3] = 回执存活秒数
// ARGV[4] = 这次占用的条目(在途占用的成员;非高密度模式为空串,不消费)
// ARGV[5] = 新 GameServer 名
// 返回 {1, prev}:已改到新记录,prev 是被覆盖的旧记录(没有记录时为空串);
// {0, ”}:属主已不是 newNode,或场景已被销毁,或这次调用已被作废,记录没动;
// {2, ”}:记录已经在新 GameServer 上(别的写者转过了,它的条目在用),记录没动;
// {3, ”}:在途占用已被对账取消(这次占用拖过了截止时刻),一个键都没动。
// 0 / 2 也消费了在途占用:这次占用不会再被提交,调用方删掉自己的条目。
//
// 回执(2026-11-05,与 luaReassignSceneNode 同一套,见那里)。go-redis 对读超时 / EOF 会原样重发 EVAL;第一次已经
// 改写、回包丢了,重发时在途占用已消费,没有回执会回 3:调用方把刚记账的新条目删掉,旧条目永不归还。
//   - 改写成功时把被覆盖的旧记录以 cjson 写进回执,同一 token 再来原样回 {1, prev}、什么都不动。0 / 2 / 3 不写回执。
//   - Eval 最终回错时调用方用 luaResolveCasReceipt 问清结局,没执行的写下 'aborted',之后才到的 EVAL 回 {0, ”}。
//
// 「记录已经在新 GameServer 上」只比 GameServer 名(记录里 "/" 之前的部分):条目每次占用各不相同,比整串永远不等。
const luaCommitAgonesRoomTransfer = `
local receipt = redis.call('GET', KEYS[3])
if receipt == 'aborted' then
    return {0, ''}
end
if receipt then
    return {1, cjson.decode(receipt).prev}
end
if ARGV[4] ~= '' and redis.call('ZREM', KEYS[4], ARGV[4]) == 0 then
    return {3, ''}
end
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
    return {0, ''}
end
local prev = redis.call('GET', KEYS[2])
if not prev then
    prev = ''
end
local prevGs = prev
local slash = string.find(prev, '/', 1, true)
if slash then
    prevGs = string.sub(prev, 1, slash - 1)
end
if prevGs == ARGV[5] then
    return {2, ''}
end
redis.call('SET', KEYS[2], ARGV[2])
redis.call('SET', KEYS[3], cjson.encode({prev = prev}), 'EX', ARGV[3])
return {1, prev}
`

// sceneAgonesTransferReceiptKey 是 luaCommitAgonesRoomTransfer 一次调用的回执键,存活 sceneClaimRecordTTLSeconds。
// 与脚本里的其余键同一个 zone 的 tag(scene:{ctl:<z>}:<id>:agones_transfer_receipt:<token>)。
func sceneAgonesTransferReceiptKey(zoneID uint32, sceneID uint64, token string) string {
	return fmt.Sprintf("scene:%s:%d:agones_transfer_receipt:%s", sceneControlTag(zoneID), sceneID, token)
}

const (
	agonesTransferStale   = 0
	agonesTransferApplied = 1
	agonesTransferNoop    = 2
	agonesTransferFenced  = 3
)

// agonesTransferResult 是一次名额转移的结局,对账按它记指标。
type agonesTransferResult int

const (
	// agonesTransferMoved:记录改到了新 GameServer,旧条目已删(删失败的由对账收)。
	agonesTransferMoved agonesTransferResult = iota
	// agonesTransferNotNeeded:记录已经在新 GameServer 上,或属主已变 / 场景已销毁 / 在途占用被取消;新条目已放弃。
	agonesTransferNotNeeded
	// agonesTransferNoCapacity:新 GameServer 满了,一个键都没动,旧名额留着。
	agonesTransferNoCapacity
	// agonesTransferFailed:没能在新 GameServer 上占到名额(Redis / K8s 出错、节点映射不上),一个键都没动。
	agonesTransferFailed
	// agonesTransferUnknown:CAS 结局未知,两边都没动,在途占用过期之后由对账按记录收。
	agonesTransferUnknown
)

// failedAgonesTransfer 记下「新节点上没占到名额」并折成结局:一个键都没动,旧名额留着,对账每轮重试。
func failedAgonesTransfer(sceneID uint64, newNode string, err error) agonesTransferResult {
	logx.Errorf("[Agones] scene %d: on node %s but could not reserve a room there (%v); "+
		"keeping the old room, the reconciler retries the transfer", sceneID, newNode, err)
	if isNoCapacity(err) {
		return agonesTransferNoCapacity
	}
	return agonesTransferFailed
}

// commitAgonesRoomTransfer 把已在 newNode 的 GameServer 上占好的名额(placement)提交进记录,再按结局删旧条目或放弃新条目。
// 改派 CAS 落地之后调(commitReservedAgonesRoom),对账发现「场景在这个节点、名额记在别的 GameServer 上」时也调。
//
// 场景换节点不转移名额的话:旧 GameServer 永远不还(容量凭空蒸发),新 GameServer 没记账(容量被超卖)。
// 顺序是**先占新的再还旧的**:反过来的话,中间窗口里新节点可能已经没容量了,于是名额两头都不在,频道变成"不占任何容量"的幽灵。
//
// 记录改写走 luaCommitAgonesRoomTransfer 的比较并交换,要还的旧条目取脚本原子读出的值,不在占名额之前预读:
// K8s 往返期间场景可能被销毁(销毁方已按旧记录删过条目),或被另一个写者再次搬走并转过名额。
//   - 改成了:删旧记录指向的那一条。
//   - 没改成(属主已不是 newNode、场景已销毁、记录已在新 GameServer、在途占用被取消):放弃这次占的新条目。
//     删的都是自己这次占的那一条,不会删到别人的(agones_room_ledger.go 的条目说明)。
//   - Redis 出错:先用同一个 token 问清结局(2026-11-05):已改写就照常删旧条目;没改写就同时作废,放弃新条目。
//     问也出错才是结局未知:两边都不动,在途占用过期之后对账按记录判 —— 记录指向新条目就留新删旧,否则删新留旧。
func commitAgonesRoomTransfer(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	sceneID uint64,
	newNode string,
	zoneID uint32,
	placement *agonesPlacement,
) agonesTransferResult {
	room := placement.room()

	// 每次调用一个新 token、一把自己的回执键,go-redis 内部重发带同一个,脚本据此认出重发(见 luaCommitAgonesRoomTransfer)。
	receiptKey := sceneAgonesTransferReceiptKey(zoneID, sceneID, uuid.NewString())
	keys := []string{sceneNodeKey(zoneID, sceneID), sceneAgonesGsKey(zoneID, sceneID), receiptKey, agonesRoomHoldsKey(zoneID)}
	raw, err := svcCtx.Redis.Eval(luaCommitAgonesRoomTransfer, keys,
		newNode, room.record(), sceneClaimRecordTTLSeconds, room.Entry, room.GameServer)
	var status int64
	var prev string
	if err != nil {
		// go-redis 的重发也都没拿到应答,脚本可能已经执行。用同一个 token 问清结局(见 resolveAgonesTransfer)。
		var resolveErr error
		status, prev, resolveErr = resolveAgonesTransfer(svcCtx, receiptKey)
		if resolveErr != nil {
			logx.Errorf("[Agones] scene %d: recording the room %s on node %s failed with an unknown outcome; "+
				"leaving both rooms to the reconciler (it keeps whichever the record names once the hold expires): %v / %v",
				sceneID, room.record(), newNode, err, resolveErr)
			return agonesTransferUnknown
		}
		if status == agonesTransferApplied {
			logx.Infof("[Agones] scene %d: recording the room %s lost its reply; resolved as applied: %v", sceneID, room.record(), err)
		} else {
			// 脚本自己报错也走到这里,用 Errorf 不埋掉。
			logx.Errorf("[Agones] scene %d: recording the room %s failed; resolved as not applied (now fenced off), "+
				"abandoning the room just taken: %v", sceneID, room.record(), err)
		}
	} else {
		parts := toSlice(raw)
		if len(parts) < 2 {
			logx.Errorf("[Agones] scene %d: unexpected transfer script reply %v; leaving both rooms to the reconciler", sceneID, raw)
			return agonesTransferUnknown
		}
		status, prev = toInt64(parts[0]), toString(parts[1])
	}
	switch status {
	case agonesTransferApplied:
		if old := parseAgonesRoom(prev); old.Entry != "" {
			_ = releaseAgonesRoom(ctx, svcCtx, agonesNamespace(svcCtx), old, "scene_migrated")
		}
		return agonesTransferMoved
	case agonesTransferNoop:
		abandonAgonesPlacement(ctx, svcCtx, zoneID, placement, "transfer_already_recorded")
	case agonesTransferStale:
		logx.Infof("[Agones] scene %d: owner is no longer %s (moved again or destroyed); abandoning the room just taken on %s",
			sceneID, newNode, room.GameServer)
		abandonAgonesPlacement(ctx, svcCtx, zoneID, placement, "transfer_owner_changed")
	case agonesTransferFenced:
		logx.Errorf("[Agones] scene %d: the hold of room %s expired before the transfer was recorded; abandoning it",
			sceneID, room.record())
		abandonAgonesPlacement(ctx, svcCtx, zoneID, placement, "transfer_hold_expired")
	default:
		logx.Errorf("[Agones] scene %d: unexpected transfer script status %v; leaving both rooms to the reconciler", sceneID, status)
		return agonesTransferUnknown
	}
	return agonesTransferNotNeeded
}

// resolveAgonesTransfer 在转移脚本结论未知时用同一个 token 问清结局(luaResolveCasReceipt),折回脚本自己的返回状态:
//   - 已改写:agonesTransferApplied 与回执里记的被覆盖旧记录,调用方照常删旧条目;
//   - 没改写:问的同时已作废,以后也不会改写,折成 agonesTransferStale,调用方放弃刚占的新条目。
//     不分原本会落成 0、2 还是 3:都没改记录,刚占的那条都没人记账。
//
// 问也出错、或回执内容解不开时回 error,结局仍未知。
func resolveAgonesTransfer(svcCtx *svc.ServiceContext, receiptKey string) (int64, string, error) {
	verdict, err := resolveCasReceipt(svcCtx, receiptKey)
	if err != nil {
		return 0, "", err
	}
	if verdict == casReceiptAborted {
		return agonesTransferStale, "", nil
	}
	var receipt struct {
		Prev string `json:"prev"`
	}
	if err := json.Unmarshal([]byte(verdict), &receipt); err != nil {
		return 0, "", fmt.Errorf("decode transfer receipt %s: %w", receiptKey, err)
	}
	return agonesTransferApplied, receipt.Prev, nil
}

// releaseAgonesRoom 把一个房间名额还给 Agones:从 GameServer 的 rooms 列表里删掉这一条。
//
// 按条目删是幂等的,所以失败可以放心重试(拆除任务就重做到成功);不重试的调用方也不用担心永久泄漏:
// 没有记录、也没有在途占用的条目由对账收。失败记 agones_counter_rollback_total{outcome="failed"} 与 ERROR 日志。
// 非高密度模式(room.Entry 为空)不记名额,直接成功。
func releaseAgonesRoom(ctx context.Context, svcCtx *svc.ServiceContext, namespace string, room agonesRoom, reason string) error {
	if room.GameServer == "" || room.Entry == "" {
		return nil
	}
	allocator := GetAgonesAllocator()
	if allocator == nil {
		return nil
	}
	if namespace == "" {
		namespace = agonesNamespace(svcCtx)
	}

	if err := allocator.RemoveRoomEntry(ctx, namespace, room.GameServer, room.Entry); err != nil {
		metrics.ObserveAgonesCounterRollback("failed")
		logx.Errorf("[Agones] failed to release room %s on %s/%s (reason=%s): %v; "+
			"safe to retry, otherwise the reconciler reclaims it", room.Entry, namespace, room.GameServer, reason, err)
		return err
	}
	metrics.ObserveAgonesCounterRollback("ok")
	logx.Infof("[Agones] released room %s on %s/%s (reason=%s)", room.Entry, namespace, room.GameServer, reason)
	return nil
}

// isNoCapacity 区分"没容量"和"基础设施出错"。前者是正常的背压,
// 后者要单独告警。ErrNoCapacity 被 %w 包过一层,所以用 errors.Is。
func isNoCapacity(err error) bool {
	return errors.Is(err, agones.ErrNoCapacity)
}
