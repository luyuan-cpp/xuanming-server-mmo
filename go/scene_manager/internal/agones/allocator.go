// Package agones 提供 SceneManager 侧的 Agones 容量预占能力。
//
// 模型(与 docs/design/agones-scene-node-high-density.md 一致):
//
//	1 Agones GameServer = 1 个 Scene Node Pod / C++ 进程 = N 个 ECS Scene 房间
//
// 所以这里预占的不是"一个 Pod",而是某个已有进程上的**一个房间名额**:
// GameServer 上 Agones List `rooms` 里的一个条目(设计稿 §13)。每次占用一个条目,值由调用方给,
// 全局唯一;占用数就是条目数。名额有了身份,增删都按条目做,天然幂等,调用方可以放心重试。
//
// 刻意**不**引入 agones.dev/agones 这个 Go module:它会把 Agones 自己的
// k8s.io/* 版本拖进来,和本仓库现有的 client-go v0.29.3 打架。这里只用
// client-go 的 dynamic/unstructured 客户端按 GVR 操作,依赖面最小。
package agones

import (
	"context"
	"errors"
)

// ErrNoCapacity 表示 Agones 没有可用容量:没有匹配的 GameServer,或所有
// 匹配到的 GameServer 的 rooms 列表都已到 capacity,或指定的 GameServer 不存在。
//
// 调用方必须 fail-closed —— 不允许悄悄退回"按 Redis 负载挑一个节点",
// 那样会绕过容量约束,把房间塞进一个 Agones 认为已经满了的进程。
var ErrNoCapacity = errors.New("agones: no game server with available room capacity")

// ErrNotConfigured 表示分配器没有配置好(未启用 / 集群外运行)。
var ErrNotConfigured = errors.New("agones: allocator not configured")

// AllocationRequest 描述一次房间名额预占。
type AllocationRequest struct {
	// Namespace 是 Fleet 所在的命名空间(zone 命名空间)。
	Namespace string
	// ZoneLabel / RoleLabel / BuildLabel 对应 Fleet 模板里的
	// mmorpg.io/zone、mmorpg.io/role、mmorpg.io/build。
	// BuildLabel 为空表示不按构建版本过滤(滚动升级期间常见)。
	ZoneLabel  string
	RoleLabel  string
	BuildLabel string
	// Entry 是这次占用的条目,高密度模式下必填,全局唯一(调用方生成,见 logic.newAgonesRoomEntry)。
	// 非高密度模式不记名额,忽略它。
	Entry string
}

// AllocationResult 是一次成功预占的结果。
type AllocationResult struct {
	GameServerName string
	// PodIP 用来映射回 SceneManager 从 etcd 学到的 knownNodes。
	// Agones 的 status.address 是**宿主机**地址,不是 Pod IP,
	// 所以这个字段由实现单独解析,见 k8s_allocator.go::resolvePodIP。
	PodIP string
	// RoomsCount / RoomsCapacity 是预占**之后**的条目数与容量快照,只用于观测。
	RoomsCount    int64
	RoomsCapacity int64
}

// GameServerRooms 是对账用的只读快照。
type GameServerRooms struct {
	Name  string
	PodIP string
	State string
	// Entries 是 rooms 列表里的全部条目;非高密度模式为空。
	Entries  []string
	Capacity int64
}

// Count 是这台 GameServer 当前占用的名额数。
func (g GameServerRooms) Count() int64 {
	return int64(len(g.Entries))
}

// Allocator 是 SceneManager 依赖的唯一 Agones 接口。
//
// 抽象出来是为了让 logic 层的单元测试用 fake 跑,不依赖真实 K8s ——
// 创建失败回滚、GSA 成功但节点未注册、幂等、节点死亡这些分支必须能在
// 没有集群的情况下覆盖到。
//
// 三个写方法都按条目幂等:同一个条目加两次只占一份,删两次只还一份。非高密度模式下没有名额可记,
// AddRoomEntry / RemoveRoomEntry 直接成功。
type Allocator interface {
	// Allocate 原子地选一个有空位的 GameServer,并在同一步把 req.Entry 加进它的 rooms 列表。
	// 返回成功时条目一定已经在列表里(实现核对过);没有容量时返回 ErrNoCapacity。
	// 返回错误但带了 GameServerName 时,条目可能已在那台 GameServer 上,调用方按条目删即可。
	Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error)

	// AddRoomEntry 在一个**指定**的 GameServer 上占一个名额:条目已在就是成功;列表满了、
	// GameServer 不存在时返回 ErrNoCapacity。
	//
	// 用在必须点名 GameServer 的地方:镜像共置(复用源 Scene 已驻留的地图/AI/spawn 数据)、
	// 改派之后把名额转到新节点、对账补加缺失的条目。GameServerAllocation 只能按标签选,没法点名。
	AddRoomEntry(ctx context.Context, namespace, gameServerName, entry string) error

	// RemoveRoomEntry 还一个名额:条目不在、GameServer 已不存在都算成功。
	// 必须按 gameServerName 与条目精确删,不能"随便找一个删" —— 删错对象会让另一个进程的容量凭空多出来。
	RemoveRoomEntry(ctx context.Context, namespace, gameServerName, entry string) error

	// ListGameServerRooms 拉取某 zone 下所有 GameServer 的 rooms 快照(含全部条目),供周期性对账。
	ListGameServerRooms(ctx context.Context, namespace, zoneLabel string) ([]GameServerRooms, error)
}
