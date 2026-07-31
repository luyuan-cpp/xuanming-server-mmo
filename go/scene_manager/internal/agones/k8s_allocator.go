package agones

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/zeromicro/go-zero/core/logx"
)

// Agones 的两个 GVR。用 dynamic client 按 GVR 操作,不引入 Agones 的 Go module。
var (
	gsaGVR = schema.GroupVersionResource{
		Group:    "allocation.agones.dev",
		Version:  "v1",
		Resource: "gameserverallocations",
	}
	gsGVR = schema.GroupVersionResource{
		Group:    "agones.dev",
		Version:  "v1",
		Resource: "gameservers",
	}
	podGVR = schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "pods",
	}
)

// CounterName 是高密度模式使用的 Agones Counter 名字。
// Fleet 模板里的 spec.template.spec.counters.rooms 必须同名。
const CounterName = "rooms"

// K8sAllocatorOptions 控制分配器行为。
type K8sAllocatorOptions struct {
	// HighDensity 决定是否使用 Counters and Lists(beta 能力)。
	// 关闭时 GSA 只按 Ready 选,不带 counters 动作 —— 退化成
	// "一个 GameServer 一次只接一个房间",但仍然走 Agones 分配。
	HighDensity bool
	// CounterRollbackRetries / CounterRollbackBackoff 控制回滚 counter 的
	// CAS 重试。冲突是正常现象(另一个请求同时在改同一个 GameServer)。
	CounterRollbackRetries int
	CounterRollbackBackoff time.Duration
	// RequestTimeout 单次 K8s API 调用的超时。
	RequestTimeout time.Duration
}

func (o *K8sAllocatorOptions) withDefaults() {
	if o.CounterRollbackRetries <= 0 {
		o.CounterRollbackRetries = 5
	}
	if o.CounterRollbackBackoff <= 0 {
		o.CounterRollbackBackoff = 100 * time.Millisecond
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 5 * time.Second
	}
}

// K8sAllocator 是 Allocator 的真实实现,基于 client-go dynamic client。
type K8sAllocator struct {
	dyn  dynamic.Interface
	opts K8sAllocatorOptions
}

// NewK8sAllocatorInCluster 用 in-cluster 配置构造分配器。
// SceneManager 跑在集群里,不读 kubeconfig、不走任何外部凭证。
func NewK8sAllocatorInCluster(opts K8sAllocatorOptions) (*K8sAllocator, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("agones: in-cluster config unavailable: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("agones: dynamic client: %w", err)
	}
	return NewK8sAllocator(dyn, opts), nil
}

// NewK8sAllocator 允许注入 dynamic client(测试可用 dynamic/fake)。
func NewK8sAllocator(dyn dynamic.Interface, opts K8sAllocatorOptions) *K8sAllocator {
	opts.withDefaults()
	return &K8sAllocator{dyn: dyn, opts: opts}
}

// buildSelectors 构造 GSA 的选择器列表。顺序即优先级:
//  1. 已经 Allocated 且 rooms 还有余量的 —— 高密度的关键,优先把房间塞进
//     已经在用的进程,而不是点亮一个新的。
//  2. Ready 且有余量的 —— 前者没有时才开新进程。
//
// 两条都带上 zone/role(/build)标签,避免把 world 的房间塞进 instance 进程。
func (a *K8sAllocator) buildSelectors(req AllocationRequest) []any {
	matchLabels := map[string]any{
		"mmorpg.io/zone": req.ZoneLabel,
		"mmorpg.io/role": req.RoleLabel,
	}
	if req.BuildLabel != "" {
		matchLabels["mmorpg.io/build"] = req.BuildLabel
	}

	newSelector := func(state string) map[string]any {
		labels := make(map[string]any, len(matchLabels))
		for k, v := range matchLabels {
			labels[k] = v
		}
		sel := map[string]any{
			"matchLabels":     labels,
			"gameServerState": state,
		}
		if a.opts.HighDensity {
			sel["counters"] = map[string]any{
				CounterName: map[string]any{
					// minAvailable 是 Agones 的"至少还剩这么多名额"过滤。
					// 没有它就会选中已经满员的 GameServer。
					"minAvailable": int64(1),
				},
			}
		}
		return sel
	}

	if a.opts.HighDensity {
		return []any{newSelector("Allocated"), newSelector("Ready")}
	}
	// 非高密度:只认 Ready,一个进程一次只接一局。
	return []any{newSelector("Ready")}
}

func (a *K8sAllocator) Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error) {
	if a.dyn == nil {
		return nil, ErrNotConfigured
	}
	if req.RoomsAmount <= 0 {
		req.RoomsAmount = 1
	}

	spec := map[string]any{
		"scheduling": "Packed",
		"selectors":  a.buildSelectors(req),
	}
	if a.opts.HighDensity {
		// counters.action=Increment 让"选中"和"计数 +1"在 Agones 侧是同一个
		// 原子操作。分两步做就会出现"选中了但还没加成功"的窗口,并发下超卖。
		spec["counters"] = map[string]any{
			CounterName: map[string]any{
				"action": "Increment",
				"amount": req.RoomsAmount,
			},
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "allocation.agones.dev/v1",
		"kind":       "GameServerAllocation",
		"metadata": map[string]any{
			"namespace": req.Namespace,
		},
		"spec": spec,
	}}

	callCtx, cancel := context.WithTimeout(ctx, a.opts.RequestTimeout)
	defer cancel()

	// GameServerAllocation 是一次性资源:Create 的返回值里带 status,
	// 对象本身不会被持久化。
	created, err := a.dyn.Resource(gsaGVR).Namespace(req.Namespace).
		Create(callCtx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("agones: create GameServerAllocation: %w", err)
	}

	state, _, _ := unstructured.NestedString(created.Object, "status", "state")
	if state != "Allocated" {
		// UnAllocated / Contention 都是"这次没拿到",不是基础设施故障。
		return nil, fmt.Errorf("%w (state=%s)", ErrNoCapacity, state)
	}

	gsName, _, _ := unstructured.NestedString(created.Object, "status", "gameServerName")
	if gsName == "" {
		return nil, fmt.Errorf("agones: allocation returned empty gameServerName")
	}

	count, _, _ := unstructured.NestedInt64(created.Object, "status", "counters", CounterName, "count")
	capacity, _, _ := unstructured.NestedInt64(created.Object, "status", "counters", CounterName, "capacity")

	podIP, err := a.resolvePodIP(ctx, req.Namespace, gsName, created.Object)
	if err != nil {
		// 拿不到 PodIP 就无法映射回 knownNodes。这里**不**吞掉错误:
		// 调用方必须把刚预占的房间还回去,否则计数漂移。
		return &AllocationResult{
			GameServerName: gsName,
			RoomsCount:     count,
			RoomsCapacity:  capacity,
		}, fmt.Errorf("agones: resolve pod ip for %s: %w", gsName, err)
	}

	return &AllocationResult{
		GameServerName: gsName,
		PodIP:          podIP,
		RoomsCount:     count,
		RoomsCapacity:  capacity,
	}, nil
}

// resolvePodIP 解析 GameServer 对应的 Pod IP。
//
// 为什么不能直接用 status.address:Agones 的 status.address 是**宿主机**
// 地址(给外部客户端连 hostPort 用的),而 Scene Node 是内部服务、用的是
// portPolicy: None,C++ 侧注册进 etcd 的是 POD_IP。两者不是一个东西。
//
// 解析顺序:
//  1. status.addresses 里 type=="Pod" 的条目(Agones 较新版本提供,零额外请求)
//  2. 退化:按 GameServer 名字 GET Pod 读 status.podIP。Agones 保证
//     GameServer 与其 Pod 同名。
func (a *K8sAllocator) resolvePodIP(ctx context.Context, namespace, gsName string, gsaObj map[string]any) (string, error) {
	if addr := podIPFromAddresses(gsaObj); addr != "" {
		return addr, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, a.opts.RequestTimeout)
	defer cancel()

	pod, err := a.dyn.Resource(podGVR).Namespace(namespace).Get(callCtx, gsName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	podIP, _, _ := unstructured.NestedString(pod.Object, "status", "podIP")
	if podIP == "" {
		return "", fmt.Errorf("pod %s has no status.podIP yet", gsName)
	}
	return podIP, nil
}

// podIPFromAddresses 从 status.addresses 里挑出 type=="Pod" 的地址。
// 较新的 Agones 会同时给出 Node 和 Pod 两类地址;老版本没有这个字段,
// 返回空字符串,由调用方决定要不要退化成 GET Pod。
func podIPFromAddresses(obj map[string]any) string {
	addresses, found, err := unstructured.NestedSlice(obj, "status", "addresses")
	if !found || err != nil {
		return ""
	}
	for _, raw := range addresses {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// 类型名实测是 "PodIP",不是 "Pod"。
		// Agones 1.58 的 GameServer.status.addresses 长这样:
		//   [{address:192.168.58.2, type:InternalIP},
		//    {address:pandora-agones, type:Hostname},
		//    {address:10.244.43.202, type:PodIP}]
		// 之前只认 "Pod",主路径永远匹配不上,每次分配都白走一次退化的
		// GET Pod —— 功能上看不出来(退化路径能work),但请求量翻倍,
		// 而且注释里"零额外请求"是假的。两个都认,兼容将来改名。
		if t, _ := entry["type"].(string); t == "PodIP" || t == "Pod" {
			if addr, _ := entry["address"].(string); addr != "" {
				return addr
			}
		}
	}
	return ""
}

// ReleaseRoom 把指定 GameServer 的 rooms 计数减 delta。
//
// 用 read-modify-write + resourceVersion 乐观并发(Update 带上读到的
// resourceVersion,冲突时 API server 返回 409)。冲突重试有上限;
// 最终失败必须由调用方记指标 + 可恢复事件,不能只打一条普通日志 ——
// 这类漂移不会自愈,靠 reconcile 才能发现。
func (a *K8sAllocator) ReleaseRoom(ctx context.Context, namespace, gameServerName string, delta int64) error {
	return a.adjustRoomCount(ctx, namespace, gameServerName, -delta)
}

// AcquireRoomOnGameServer 在指定 GameServer 上 +delta,超 capacity 则拒绝。
func (a *K8sAllocator) AcquireRoomOnGameServer(ctx context.Context, namespace, gameServerName string, delta int64) error {
	return a.adjustRoomCount(ctx, namespace, gameServerName, delta)
}

// adjustRoomCount 是 Acquire/Release 的共同实现:带 CAS 的 read-modify-write。
//
// delta > 0 时会检查 capacity —— 这一步不能省,否则共置路径会把房间塞进
// 一个已经满员的进程,绕过 Agones 的容量语义。
// delta < 0 时钳到 0,计数永远不为负。
func (a *K8sAllocator) adjustRoomCount(ctx context.Context, namespace, gameServerName string, delta int64) error {
	if a.dyn == nil {
		return ErrNotConfigured
	}
	if !a.opts.HighDensity {
		// 没开 Counters 就没有计数可动。Agones 会在 GameServer 被 Shutdown
		// 时自行回收,acquire 也没有可占的名额概念。
		return nil
	}
	if delta == 0 {
		return nil
	}

	var lastErr error
	for attempt := 1; attempt <= a.opts.CounterRollbackRetries; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, a.opts.RequestTimeout)
		gs, err := a.dyn.Resource(gsGVR).Namespace(namespace).Get(callCtx, gameServerName, metav1.GetOptions{})
		if err != nil {
			cancel()
			if errors.IsNotFound(err) {
				if delta < 0 {
					// GameServer 已经没了(进程被替换/缩容)。计数随对象一起消失,
					// 没有可回滚的东西,这不是失败。
					logx.Infof("[Agones] release: gameserver %s/%s already gone, nothing to roll back",
						namespace, gameServerName)
					return nil
				}
				return fmt.Errorf("%w (gameserver %s/%s not found)", ErrNoCapacity, namespace, gameServerName)
			}
			lastErr = err
			if !a.sleepBackoff(ctx, attempt) {
				break
			}
			continue
		}

		count, _, _ := unstructured.NestedInt64(gs.Object, "status", "counters", CounterName, "count")
		capacity, _, _ := unstructured.NestedInt64(gs.Object, "status", "counters", CounterName, "capacity")

		newCount := count + delta
		if delta > 0 && capacity > 0 && newCount > capacity {
			cancel()
			return fmt.Errorf("%w (gs=%s rooms %d+%d > capacity %d)",
				ErrNoCapacity, gameServerName, count, delta, capacity)
		}
		if newCount < 0 {
			logx.Errorf("[Agones] adjust would drive %s/%s rooms negative (count=%d delta=%d), clamping to 0",
				namespace, gameServerName, count, delta)
			newCount = 0
		}

		if err := unstructured.SetNestedField(gs.Object, newCount, "status", "counters", CounterName, "count"); err != nil {
			cancel()
			return fmt.Errorf("agones: set rooms count: %w", err)
		}

		// 必须用 Update 而不是 UpdateStatus。
		//
		// GameServer 的 CRD **没有声明 status 子资源** —— 实测
		// (Agones 1.58,kubectl get crd gameservers.agones.dev):
		//   subresources = {"scale":{...}}
		// 只有 scale,没有 status。所以 /status 端点不存在,UpdateStatus 会直接
		// 报 "the server could not find the requested resource",整条归还名额的
		// 路径(创建失败回滚、排空归还、孤儿清理归还)全部失效 —— 而 rooms
		// Counter 只增不减就是持续超卖。
		//
		// 单测抓不到这个:fake allocator 不建模 API 表面,UpdateStatus 和
		// Update 在它眼里没区别。
		//
		// 换成 Update 仍然是安全的 read-modify-write:gs 带着 GET 回来的
		// resourceVersion,并发改动会拿到 409,由下面的有界重试处理。
		_, err = a.dyn.Resource(gsGVR).Namespace(namespace).
			Update(callCtx, gs, metav1.UpdateOptions{})
		cancel()
		if err == nil {
			logx.Infof("[Agones] rooms %s/%s: %d -> %d (delta=%d)", namespace, gameServerName, count, newCount, delta)
			return nil
		}
		lastErr = err
		if !errors.IsConflict(err) {
			return fmt.Errorf("agones: update rooms count: %w", err)
		}
		// 409 冲突:别人刚改过,重读重试。
		if !a.sleepBackoff(ctx, attempt) {
			break
		}
	}

	return fmt.Errorf("agones: adjust rooms on %s/%s exhausted %d attempts: %w",
		namespace, gameServerName, a.opts.CounterRollbackRetries, lastErr)
}

func (a *K8sAllocator) sleepBackoff(ctx context.Context, attempt int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(attempt) * a.opts.CounterRollbackBackoff):
		return true
	}
}

func (a *K8sAllocator) ListGameServerRooms(ctx context.Context, namespace, zoneLabel string) ([]GameServerRooms, error) {
	if a.dyn == nil {
		return nil, ErrNotConfigured
	}

	callCtx, cancel := context.WithTimeout(ctx, a.opts.RequestTimeout)
	defer cancel()

	list, err := a.dyn.Resource(gsGVR).Namespace(namespace).List(callCtx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("mmorpg.io/zone=%s", zoneLabel),
	})
	if err != nil {
		return nil, fmt.Errorf("agones: list gameservers: %w", err)
	}

	out := make([]GameServerRooms, 0, len(list.Items))
	for i := range list.Items {
		item := list.Items[i]
		name := item.GetName()
		state, _, _ := unstructured.NestedString(item.Object, "status", "state")
		count, _, _ := unstructured.NestedInt64(item.Object, "status", "counters", CounterName, "count")
		capacity, _, _ := unstructured.NestedInt64(item.Object, "status", "counters", CounterName, "capacity")
		// reconcile 只做比对告警,拿不到 PodIP 也不影响结论,所以这里不为它
		// 额外发一次 Pod GET(每轮 reconcile 会放大成 N 次请求)。
		podIP := podIPFromAddresses(item.Object)
		out = append(out, GameServerRooms{
			Name:     name,
			PodIP:    podIP,
			State:    state,
			Count:    count,
			Capacity: capacity,
		})
	}
	return out, nil
}
