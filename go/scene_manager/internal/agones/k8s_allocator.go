package agones

import (
	"context"
	"fmt"
	"slices"
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

// RoomListName 是高密度模式使用的 Agones List 名字。
// Fleet 模板里的 spec.template.spec.lists.rooms 必须同名(tools/scripts/k8s_deploy.ps1 New-SceneFleetYaml),
// FleetAutoscaler 的 list.key 也是它。改一个就要改另一个。
const RoomListName = "rooms"

// K8sAllocatorOptions 控制分配器行为。
type K8sAllocatorOptions struct {
	// HighDensity 决定是否使用 Counters and Lists 的 rooms 列表记名额。
	// 关闭时 GSA 只按 Ready 选,不带列表动作 —— 退化成
	// "一个 GameServer 一次只接一个房间",但仍然走 Agones 分配,也没有名额可记。
	HighDensity bool
	// ConflictRetries / ConflictBackoff 控制按条目增删时的乐观并发重试。
	// 409 冲突是正常现象(另一个请求同时在改同一个 GameServer)。
	ConflictRetries int
	ConflictBackoff time.Duration
	// RequestTimeout 单次 K8s API 调用的超时。
	RequestTimeout time.Duration
}

func (o *K8sAllocatorOptions) withDefaults() {
	if o.ConflictRetries <= 0 {
		o.ConflictRetries = 5
	}
	if o.ConflictBackoff <= 0 {
		o.ConflictBackoff = 100 * time.Millisecond
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
//  1. 已经 Allocated 且 rooms 还有空位的 —— 高密度的关键,优先把房间塞进
//     已经在用的进程,而不是点亮一个新的。
//  2. Ready 且有空位的 —— 前者没有时才开新进程。
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
			sel["lists"] = map[string]any{
				RoomListName: map[string]any{
					// minAvailable 是 Agones 的"列表至少还剩这么多空位"过滤。
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
	if a.opts.HighDensity && req.Entry == "" {
		return nil, fmt.Errorf("agones: high-density allocation needs a room entry")
	}

	spec := map[string]any{
		"scheduling": "Packed",
		"selectors":  a.buildSelectors(req),
	}
	if a.opts.HighDensity {
		// lists.addValues 让"选中"和"占位"在 Agones 侧是同一个操作。分两步做就会出现
		// "选中了但还没占上"的窗口,并发下超卖。
		spec["lists"] = map[string]any{
			RoomListName: map[string]any{
				"addValues": []any{req.Entry},
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

	result := &AllocationResult{GameServerName: gsName}
	if a.opts.HighDensity {
		// 核对条目确实在列表里:防列表已满时 Agones 不加值却照样回 Allocated。没有这一条就当没拿到容量;
		// 这台 GameServer 照常 Allocated,不占名额,无害。核对本身出错时把 GameServerName 带回去,调用方按条目删。
		recorded, entries, capacity, verifyErr := a.allocationRecordsEntry(ctx, req.Namespace, gsName, req.Entry, created.Object)
		if verifyErr != nil {
			return result, fmt.Errorf("agones: verify room entry on %s: %w", gsName, verifyErr)
		}
		if !recorded {
			return nil, fmt.Errorf("%w (gs=%s allocated without the room entry; list full)", ErrNoCapacity, gsName)
		}
		result.RoomsCount, result.RoomsCapacity = int64(len(entries)), capacity
	}

	podIP, err := a.resolvePodIP(ctx, req.Namespace, gsName, created.Object)
	if err != nil {
		// 拿不到 PodIP 就无法映射回 knownNodes。这里**不**吞掉错误:
		// 调用方必须把刚占的条目删掉,否则它一直占着名额(对账也会收,只是要等)。
		return result, fmt.Errorf("agones: resolve pod ip for %s: %w", gsName, err)
	}
	result.PodIP = podIP
	return result, nil
}

// allocationRecordsEntry 核对分配之后 entry 是否在 GameServer 的 rooms 列表里,顺带返回列表快照。
// 先看 GSA 返回对象里的 status.lists;返回对象里没有 lists(Agones 版本不回这一段)时 GET 这台 GameServer。
func (a *K8sAllocator) allocationRecordsEntry(ctx context.Context, namespace, gsName, entry string, gsaObj map[string]any) (bool, []string, int64, error) {
	if _, found, _ := unstructured.NestedMap(gsaObj, "status", "lists"); found {
		entries, capacity, err := roomList(gsaObj)
		if err != nil {
			return false, nil, 0, err
		}
		return slices.Contains(entries, entry), entries, capacity, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, a.opts.RequestTimeout)
	defer cancel()
	gs, err := a.dyn.Resource(gsGVR).Namespace(namespace).Get(callCtx, gsName, metav1.GetOptions{})
	if err != nil {
		return false, nil, 0, err
	}
	entries, capacity, err := roomList(gs.Object)
	if err != nil {
		return false, nil, 0, err
	}
	return slices.Contains(entries, entry), entries, capacity, nil
}

// roomList 读对象 status.lists.rooms 的条目与容量。没有 values 时是空列表。
func roomList(obj map[string]any) ([]string, int64, error) {
	entries, _, err := unstructured.NestedStringSlice(obj, "status", "lists", RoomListName, "values")
	if err != nil {
		return nil, 0, fmt.Errorf("status.lists.%s.values: %w", RoomListName, err)
	}
	capacity, _, err := unstructured.NestedInt64(obj, "status", "lists", RoomListName, "capacity")
	if err != nil {
		return nil, 0, fmt.Errorf("status.lists.%s.capacity: %w", RoomListName, err)
	}
	return entries, capacity, nil
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

// AddRoomEntry 在指定 GameServer 的 rooms 列表里加 entry,见 Allocator。
//
// 容量检查不能省:共置与改派路径会点名 GameServer,不查就会把房间塞进一个已经满员的进程,
// 绕过 Agones 的容量语义。
func (a *K8sAllocator) AddRoomEntry(ctx context.Context, namespace, gameServerName, entry string) error {
	if !a.opts.HighDensity {
		// 没开 Lists 就没有名额可记。
		return nil
	}
	return a.updateRoomEntries(ctx, namespace, gameServerName, func(entries []string, capacity int64) ([]string, error) {
		if slices.Contains(entries, entry) {
			return nil, nil
		}
		if capacity > 0 && int64(len(entries)) >= capacity {
			return nil, fmt.Errorf("%w (gs=%s rooms %d/%d)", ErrNoCapacity, gameServerName, len(entries), capacity)
		}
		return append(slices.Clone(entries), entry), nil
	}, false)
}

// RemoveRoomEntry 从指定 GameServer 的 rooms 列表里删 entry,见 Allocator。
func (a *K8sAllocator) RemoveRoomEntry(ctx context.Context, namespace, gameServerName, entry string) error {
	if !a.opts.HighDensity {
		return nil
	}
	return a.updateRoomEntries(ctx, namespace, gameServerName, func(entries []string, _ int64) ([]string, error) {
		i := slices.Index(entries, entry)
		if i < 0 {
			return nil, nil
		}
		return slices.Delete(slices.Clone(entries), i, i+1), nil
	}, true)
}

// updateRoomEntries 是按条目增删的共同实现:读 GameServer,交给 mutate 算出新的条目列表,带读到的
// resourceVersion 更新。mutate 回 nil 列表表示已经是想要的样子,不写。409 冲突时重读重试,有上限。
// GameServer 不存在时:goneIsSuccess(删除)算成功 —— 计数随对象一起消失;否则(加)回 ErrNoCapacity。
func (a *K8sAllocator) updateRoomEntries(ctx context.Context, namespace, gameServerName string,
	mutate func(entries []string, capacity int64) ([]string, error), goneIsSuccess bool) error {

	if a.dyn == nil {
		return ErrNotConfigured
	}

	var lastErr error
	for attempt := 1; attempt <= a.opts.ConflictRetries; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, a.opts.RequestTimeout)
		gs, err := a.dyn.Resource(gsGVR).Namespace(namespace).Get(callCtx, gameServerName, metav1.GetOptions{})
		if err != nil {
			cancel()
			if errors.IsNotFound(err) {
				if goneIsSuccess {
					logx.Infof("[Agones] rooms %s/%s: gameserver already gone, nothing to remove", namespace, gameServerName)
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

		entries, capacity, err := roomList(gs.Object)
		if err != nil {
			cancel()
			return fmt.Errorf("agones: read rooms of %s/%s: %w", namespace, gameServerName, err)
		}
		next, err := mutate(entries, capacity)
		if err != nil {
			cancel()
			return err
		}
		if next == nil {
			cancel()
			return nil
		}
		if err := unstructured.SetNestedStringSlice(gs.Object, next, "status", "lists", RoomListName, "values"); err != nil {
			cancel()
			return fmt.Errorf("agones: set rooms values: %w", err)
		}

		// 必须用 Update 而不是 UpdateStatus。
		//
		// GameServer 的 CRD **没有声明 status 子资源** —— 实测
		// (Agones 1.58,kubectl get crd gameservers.agones.dev):
		//   subresources = {"scale":{...}}
		// 只有 scale,没有 status。所以 /status 端点不存在,UpdateStatus 会直接
		// 报 "the server could not find the requested resource",整条增删名额的
		// 路径(共置、改派、归还、对账)全部失效。
		//
		// 单测抓不到这个:fake allocator 不建模 API 表面,UpdateStatus 和
		// Update 在它眼里没区别。
		//
		// Update 仍然是安全的 read-modify-write:gs 带着 GET 回来的
		// resourceVersion,并发改动会拿到 409,由这里的有界重试处理。
		_, err = a.dyn.Resource(gsGVR).Namespace(namespace).Update(callCtx, gs, metav1.UpdateOptions{})
		cancel()
		if err == nil {
			logx.Infof("[Agones] rooms %s/%s: %d -> %d entries", namespace, gameServerName, len(entries), len(next))
			return nil
		}
		lastErr = err
		if !errors.IsConflict(err) {
			return fmt.Errorf("agones: update rooms of %s/%s: %w", namespace, gameServerName, err)
		}
		// 409 冲突:别人刚改过,重读重试。
		if !a.sleepBackoff(ctx, attempt) {
			break
		}
	}

	return fmt.Errorf("agones: update rooms of %s/%s exhausted %d attempts: %w",
		namespace, gameServerName, a.opts.ConflictRetries, lastErr)
}

func (a *K8sAllocator) sleepBackoff(ctx context.Context, attempt int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(attempt) * a.opts.ConflictBackoff):
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
		entries, capacity, err := roomList(item.Object)
		if err != nil {
			// 对账按条目判,读不出条目的这一台不能当成空的:当成空会漏判,整轮失败让调用方跳过。
			return nil, fmt.Errorf("agones: read rooms of %s/%s: %w", namespace, name, err)
		}
		// 对账拿不到 PodIP 也能按条目判「条目 → 记录」,所以这里不为它额外发一次 Pod GET
		// (每轮对账会放大成 N 次请求);拿不到 PodIP 的那台只少做「记录 → 条目」。
		podIP := podIPFromAddresses(item.Object)
		out = append(out, GameServerRooms{
			Name:     name,
			PodIP:    podIP,
			State:    state,
			Entries:  entries,
			Capacity: capacity,
		})
	}
	return out, nil
}
