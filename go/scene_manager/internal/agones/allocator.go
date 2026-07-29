// Package agones 提供 SceneManager 侧的 Agones 容量预占能力。
//
// 模型(与 docs/design/agones-scene-node-high-density.md 一致):
//
//	1 Agones GameServer = 1 个 Scene Node Pod / C++ 进程 = N 个 ECS Scene 房间
//
// 所以这里预占的不是"一个 Pod",而是某个已有进程上的**一个房间名额**
// (Agones Counters and Lists 的 `rooms` Counter)。
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
// 匹配到的 GameServer 的 rooms 计数都已到 capacity。
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
	// RoomsAmount 本次预占的房间数,正常恒为 1。
	RoomsAmount int64
}

// AllocationResult 是一次成功预占的结果。
type AllocationResult struct {
	GameServerName string
	// PodIP 用来映射回 SceneManager 从 etcd 学到的 knownNodes。
	// Agones 的 status.address 是**宿主机**地址,不是 Pod IP,
	// 所以这个字段由实现单独解析,见 k8s_allocator.go::resolvePodIP。
	PodIP string
	// RoomsCount / RoomsCapacity 是预占**之后**的计数快照,只用于观测。
	RoomsCount    int64
	RoomsCapacity int64
}

// GameServerRooms 是 reconcile 用的只读快照。
type GameServerRooms struct {
	Name     string
	PodIP    string
	State    string
	Count    int64
	Capacity int64
}

// Allocator 是 SceneManager 依赖的唯一 Agones 接口。
//
// 抽象出来是为了让 logic 层的单元测试用 fake 跑,不依赖真实 K8s ——
// 创建失败回滚、GSA 成功但节点未注册、幂等、节点死亡这些分支必须能在
// 没有集群的情况下覆盖到。
type Allocator interface {
	// Allocate 原子地选一个 GameServer 并把它的 rooms 计数 +RoomsAmount。
	// 没有容量时返回 ErrNoCapacity。
	Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error)

	// AcquireRoomOnGameServer 在一个**指定**的 GameServer 上预占名额。
	//
	// 存在的理由只有一个:镜像 Scene 必须与源 Scene 共置(复用已驻留的
	// 地图/AI/spawn 数据)。GameServerAllocation 只能按标签选,没法点名,
	// 所以共置路径要走这条。超出 capacity 时返回 ErrNoCapacity,调用方
	// 回落到 Allocate 自由选择。
	AcquireRoomOnGameServer(ctx context.Context, namespace, gameServerName string, delta int64) error

	// ReleaseRoom 精确回滚:把指定 GameServer 的 rooms 计数减 delta。
	// 必须按 gameServerName 精确回滚,不能"随便找一个减" ——
	// 减错对象会让另一个进程的容量凭空多出来。
	ReleaseRoom(ctx context.Context, namespace, gameServerName string, delta int64) error

	// ListGameServerRooms 拉取某 zone 下所有 GameServer 的 rooms 快照,
	// 供周期性 reconcile 比对 Redis 与节点上报。
	ListGameServerRooms(ctx context.Context, namespace, zoneLabel string) ([]GameServerRooms, error)
}
