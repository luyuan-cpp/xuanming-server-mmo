// Package noderegistry 按 C++ etcd_service 约定把 Go 服务注册成可被 C++ 节点 / 路由服发现的
// NodeInfo:负责 node_id 的 CAS 分配、租约、keepalive、失租重注册与注销。
//
// # 与既有副本的关系(契约 zone_contract_v1 §2)
//
// 仓库里已有 7 份非 login 的同类实现:4 份 internal/noderegistry(match / scene_manager /
// data_service / client_rpc_router 一脉)+ friend / guild / player_locator 的 internal/node,
// 以及 login 的异源实现(internal/logic/pkg/etcd)。它们都 import proto 模块,且是
// internal 包,跨 module 引不到,只能逐字复制 —— 这正是"改一处漏六处"的来源。
// 本包是**唯一实现**:chat 是首个消费者;既有副本本轮一行不动,迁移放在后续批次逐个替换,
// 迁移时以本包语义为准,不要反向往副本里补丁。
//
// # 为什么不 import proto
//
// shared 模块的既有纪律是不依赖 proto 模块(shared 自带 table / tip 生成物,proto 模块反过来
// 依赖 shared)。所以 NodeInfo 的 protojson 由调用方回调 Spec.BuildValue 生成;本包只用
// 手写镜像 struct(nodeInfoMirror,字段名照 client_rpc_router/internal/discovery 的
// nodeRegistrationJSON)校验回调产物的关键字段,不合格拒注册(fail-closed)。
//
// # 两把租约分开,本包不提供 Lost()
//
// 本包的租约只承载"服务发现可见性":丢了就重注册,顶多有一个 LeaseTTL 的不可见窗口。
// Snowflake 发号器的 worker id 归 shared/snowflakealloc 管,失去所有权只听
// snowflakealloc.Handle.Lost()。两把租约**不合并、不共用 lease**:合并后发现键的抖动会连带
// fence 发号器,而发号器的正确性又不该寄托在发现键上。所以这里刻意不暴露 Lost() 之类的信号,
// 免得调用方把它误接到发号器上。
//
// # 失租策略两档(决策 D-11)
//
// 失租后先 CAS 重夺原 node_id;原 id 已被别的实例占用时按 Spec.OnReclaimFailed:
//   - ReallocateNewID(默认):分配新 node_id 继续服务,回调 OnNodeIDChanged。
//     适用于 node_id 只是"发现路径里的一个编号"的服务(chat 等全局无状态服务)——
//     换号只让路由服镜像里的 key 变一下,不影响任何持久身份。
//   - ExitProcess:Revoke 新租约后 logx.Error + os.Exit(1),交给编排器重启。
//     适用于 login 以及任何以 NodeId 派生持久身份或 per-node topic(`{type}-{id}`)的服务——
//     这类服务在进程内换号,等于把旧身份上的在途消息 / 状态悄悄丢给了别人。
//     D-11 的前提是"node_id = worker id"只对 C++ / login 成立,Go 全局服务不成立。
//
// # 使用顺序
//
//	reg, err := noderegistry.RegisterAfterListening(ctx, cli, c.ListenOn, 30*time.Second, spec)
//	reg.KeepAlive()
//	...
//	reg.Close()      // 先于 gRPC Stop:先从发现里消失,再停止接客
//	server.Stop()
//
// # 并发模型
//
// Registration 的可变状态(lease / key / value / closed)由 mu 保护,只有 keepalive 这一条
// goroutine 会改写,Close 与它之间靠 closed 标记收口;NodeID() 走 atomic,调用方任意 goroutine
// 可读。Register / RegisterAfterListening 本身不起 goroutine。
package noderegistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/safego"
	"shared/snowflake"
)

// ReclaimPolicy 决定"失租后原 node_id 已被别人占用"时怎么办,见包注释「失租策略两档」。
type ReclaimPolicy int

const (
	// ReallocateNewID 分配新 node_id 继续服务(默认零值)。
	ReallocateNewID ReclaimPolicy = iota
	// ExitProcess Revoke 新租约后退出进程,交给编排器重启。
	ExitProcess
)

// String 供日志使用。
func (p ReclaimPolicy) String() string {
	switch p {
	case ReallocateNewID:
		return "ReallocateNewID"
	case ExitProcess:
		return "ExitProcess"
	default:
		return "ReclaimPolicy(" + strconv.Itoa(int(p)) + ")"
	}
}

// Spec 是一次注册的全部输入。依赖以参数注入,本包不读任何服务 config。
type Spec struct {
	// Prefix = base.ENodeType_name[nodeType] + ".rpc",由调用方派生,不手写;
	// 不得含 "/"(扫描已占 id 时用 Prefix+"/" 做前缀,含斜杠会串到别的子树)。
	Prefix string
	// NodeType 是 ENodeType 枚举值,写进两把 key 的 node_type 段。
	NodeType uint32
	// ZoneId 必填(拒 0);K8s 全局服务取命令行 -ZoneId。只影响 rpcPath。
	ZoneId uint32
	// LeaseTTL(秒)就是崩溃场景的切换窗口:路由服 PickRandom 不看连接状态,
	// 死节点在 TTL 内仍会被选中。chat 用 60。
	LeaseTTL int64
	// BuildValue 生成 rpcPath 的值:protojson(NodeInfo),必须使用 protojson 默认的
	// lowerCamelCase 字段名(与 C++ MessageToJsonString 一致)。nodeId / nodeUuid 必须
	// 原样写入入参;换号重注册时会用新 nodeID 再调一次。
	BuildValue func(nodeID uint32, nodeUUID string) ([]byte, error)
	// OnReclaimFailed 见 ReclaimPolicy。零值 = ReallocateNewID。
	OnReclaimFailed ReclaimPolicy
	// OnNodeIDChanged 可选:ReallocateNewID 换号成功后在 keepalive goroutine 里同步回调,
	// 不要在里面阻塞;panic 会被 safego 兜住,不影响后续 keepalive。
	OnNodeIDChanged func(oldID, newID uint32)
	// LogPrefix 可选,空则用 "[NodeRegistry <Prefix>]"。
	LogPrefix string
}

// Registration 是一次成功注册的句柄。只能由 Register / RegisterAfterListening 构造。
type Registration struct {
	// NodeUUID 是本进程注册身份,写在 allocKey 的值与 NodeInfo.nodeUuid 里;进程内不变
	// (换 node_id 不换 uuid —— uuid 是"哪个进程",node_id 只是发现路径上的编号)。
	NodeUUID string

	cli    *clientv3.Client
	spec   Spec
	nodeID atomic.Uint32

	// ctx 是注册句柄自身的生命周期(keepalive / 重注册的全部 etcd 调用都挂在它下面),
	// 与 Register 的入参 ctx(启动期超时)无关;Close 取消它。
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	leaseID  clientv3.LeaseID
	rpcKey   string
	allocKey string
	value    string
	closed   bool

	keepAliveOnce sync.Once
	closeOnce     sync.Once
	// loopStarted / loopDone:Close 要等 keepalive goroutine 真正退出再返回。
	// 为什么:keepalive 可能正卡在 reRegisterOnce 里,Txn 已在服务端提交、commit() 看到 closed 后才去
	// Revoke 新租约;Close 若不等它就返回,调用方紧接着关 etcd client 会让这次 Revoke 失败,
	// 新租约上的双 key 在 LeaseTTL 内仍可被路由服发现。loopStarted 只在 KeepAlive 真正起了
	// goroutine 时置位 —— 从没调过 KeepAlive 的句柄(测试、注册后立刻 Close)不必等。
	loopStarted atomic.Bool
	loopDone    chan struct{}
}

const (
	// nodeIDMin:0 永远保留为"未分配 / 非法值"(C++ 同口径),扫描时也一律忽略。
	nodeIDMin uint32 = 1
	// nodeIDMax 与 Snowflake worker id 位宽对齐,与既有副本同值。
	nodeIDMax = uint32(snowflake.NodeMask)

	// nodeIDSegment 是两种 key 共有的尾段标记,已占 id 统一从它后面取。
	nodeIDSegment = "/node_id/"

	// protojson 对 ENodeProtocolType 的两种合法表示:NodeInfo.protocol_type 若声明为枚举则输出
	// 枚举名,声明为 uint32 则输出数字。本包不 import proto,两个值照
	// base.ENodeProtocolType_PROTOCOL_GRPC 手抄,契约 §2 钉死。
	protocolGRPCName          = "PROTOCOL_GRPC"
	protocolGRPCNumber uint32 = 1

	registerTimeout     = 10 * time.Second // Register 内 Grant + 扫描 + CAS 的总预算
	reRegisterOpTimeout = 10 * time.Second // 每一轮重注册的 etcd 调用预算
	closeTimeout        = 5 * time.Second
	revokeTimeout       = 5 * time.Second
	backoffMin          = time.Second
	backoffMax          = 30 * time.Second

	listenPoll        = 100 * time.Millisecond
	listenDialTimeout = 500 * time.Millisecond

	// safego 点位名,会变成 safego_panic_total{point} 的 label,必须是常量。
	keepAlivePoint = "noderegistry.keepalive"
	callbackPoint  = "noderegistry.on_node_id_changed"
)

var (
	errInvalidSpec       = errors.New("noderegistry: invalid spec")
	errInvalidValue      = errors.New("noderegistry: BuildValue produced an invalid NodeInfo")
	errNoAvailableNodeID = errors.New("noderegistry: no available node_id")
)

// exitProcess 是 ExitProcess 策略的最后一步。抽成包级变量只为集成测试能替换掉 os.Exit;
// 生产路径恒为下面的实现。logx.Close 先冲掉日志缓冲,否则最关键的那条 Error 可能丢
// (login.go 同一写法)。
var exitProcess = func() {
	_ = logx.Close()
	os.Exit(1)
}

// RpcPath 是 per-zone 服务发现 key(值 = protojson NodeInfo),与 C++ etcd_service 逐字一致。
func RpcPath(prefix string, zoneId, nodeType, nodeId uint32) string {
	return fmt.Sprintf("%s/zone/%d/node_type/%d/node_id/%d", prefix, zoneId, nodeType, nodeId)
}

// AllocationKey 是跨 zone 的全局占位 key(值 = nodeUUID)。路径里不带 zone:
// 两个 zone 的实例不能同时持有同一个 (node_type, node_id)。
func AllocationKey(prefix string, nodeType, nodeId uint32) string {
	return fmt.Sprintf("%s/allocated/node_type/%d/node_id/%d", prefix, nodeType, nodeId)
}

// Register 分配 node_id 并写入双 key,返回的句柄尚未开始续租 —— 调用方随后必须 KeepAlive()。
//
// 所有校验(spec、BuildValue 产物)都在任何 Put 之前完成;失败时 Revoke 已申请的租约,
// etcd 里不留任何东西。
func Register(ctx context.Context, cli *clientv3.Client, spec Spec) (*Registration, error) {
	if err := validateInputs(cli, spec); err != nil {
		return nil, err
	}

	opCtx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()

	grant, err := cli.Grant(opCtx, spec.LeaseTTL)
	if err != nil {
		return nil, fmt.Errorf("noderegistry: etcd lease grant (prefix=%s): %w", spec.Prefix, err)
	}

	r := newRegistration(cli, spec, uuid.NewString())
	id, value, err := r.allocate(opCtx, grant.ID)
	if err != nil {
		revokeLease(cli, grant.ID, r.logPrefix())
		r.cancel()
		return nil, fmt.Errorf("noderegistry: node_id allocation (prefix=%s): %w", spec.Prefix, err)
	}
	r.commit(grant.ID, id, value)

	logx.Infof("%s registered: zone=%d node_type=%d node_id=%d uuid=%s lease=%x ttl=%ds policy=%s",
		r.logPrefix(), spec.ZoneId, spec.NodeType, id, r.NodeUUID, int64(grant.ID), spec.LeaseTTL, spec.OnReclaimFailed)
	return r, nil
}

// RegisterAfterListening 先确认 listenAddr 已可 TCP 连通再注册:发现键一出现,路由服就可能
// 把请求打过来,端口还没 accept 时注册等于公布一个会拒连的节点。
//
// listenAddr 是服务的监听地址(ListenOn);"0.0.0.0:port" / "[::]:port" / ":port" 本机只能拨
// loopback,所以探 "127.0.0.1:port"。timeout 内始终连不上即返回错误,不注册。
func RegisterAfterListening(ctx context.Context, cli *clientv3.Client, listenAddr string, timeout time.Duration, spec Spec) (*Registration, error) {
	// 配置错误先报,别让它白等一个 timeout。
	if err := validateInputs(cli, spec); err != nil {
		return nil, err
	}
	dialAddr, err := dialAddrForListen(listenAddr)
	if err != nil {
		return nil, err
	}
	if err := waitForListening(ctx, dialAddr, timeout); err != nil {
		return nil, fmt.Errorf("noderegistry: %s (dial %s) not accepting within %v, refusing to register: %w",
			listenAddr, dialAddr, timeout, err)
	}
	return Register(ctx, cli, spec)
}

// NodeID 返回当前 node_id(换号后随之变化),任意 goroutine 可调。
func (r *Registration) NodeID() uint32 { return r.nodeID.Load() }

// KeepAlive 启动后台续租;失租时按 Spec.OnReclaimFailed 重注册。幂等,Close 之后调用无效果。
func (r *Registration) KeepAlive() {
	r.keepAliveOnce.Do(func() {
		// 先置位再起 goroutine:Close 看到 loopStarted=true 时 loopDone 一定会被关闭。
		r.loopStarted.Store(true)
		// safego.Go:重注册里有 etcd Txn 与调用方回调,panic 若打死进程,连"租约丢了"
		// 这条最关键的诊断都留不下;兜住后至少在 safego_panic_total 上可见。
		// defer close 写在闭包里:panic 被 safego 兜住时 defer 照样执行,Close 不会白等满超时。
		safego.Go(keepAlivePoint, func() {
			defer close(r.loopDone)
			r.keepAliveLoop()
		})
	})
}

// Close 停止续租,用一个 Txn 删除双 key,再 Revoke 租约。幂等。
//
// 必须**先于** gRPC Stop 调用:先从发现里消失,在途请求还能被处理完。
//
// 删除带条件 Value(allocKey)==NodeUUID:本进程若曾失租且 id 已被别人接管(ExitProcess 策略
// 退出前、或重注册尚未完成时),无条件删会把**别人的**发现键删掉。allocKey 仍是我们的 uuid 时,
// 同 Txn 写入的 rpcKey 也必然是我们的。
//
// 返回前等 keepalive goroutine 退出(最多 closeTimeout):它若正处在重注册的提交点,commit() 会在
// 看到 closed 后 Revoke 新租约;等它结束再返回,调用方随后关 etcd client 就不会打断这次 Revoke。
// 在 OnNodeIDChanged 回调里调 Close 不会死锁,只是等满 closeTimeout(回调就跑在 keepalive goroutine 上)。
func (r *Registration) Close() {
	r.closeOnce.Do(func() {
		r.cancel()

		// 先置 closed 再等:keepalive 此后到达 commit() 的任何提交都会被拒并 Revoke。
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		if r.loopStarted.Load() {
			t := time.NewTimer(closeTimeout)
			select {
			case <-r.loopDone:
			case <-t.C:
				logx.Errorf("%s close: keepalive goroutine did not exit within %v; continuing (a just-granted lease may linger until TTL)",
					r.logPrefix(), closeTimeout)
			}
			t.Stop()
		}

		// keepalive 已退出(或超时),此时读到的就是最后一次提交的身份。
		r.mu.Lock()
		lease, rpcKey, allocKey, id := r.leaseID, r.rpcKey, r.allocKey, r.nodeID.Load()
		r.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()

		resp, err := r.cli.Txn(ctx).
			If(clientv3.Compare(clientv3.Value(allocKey), "=", r.NodeUUID)).
			Then(
				clientv3.OpDelete(rpcKey),
				clientv3.OpDelete(allocKey),
			).
			Commit()
		switch {
		case err != nil:
			logx.Errorf("%s close: delete keys failed node_id=%d (keys expire with lease): %v", r.logPrefix(), id, err)
		case !resp.Succeeded:
			logx.Infof("%s close: node_id=%d no longer held by uuid=%s, leaving keys untouched", r.logPrefix(), id, r.NodeUUID)
		}
		if _, err := r.cli.Revoke(ctx, lease); err != nil {
			logx.Errorf("%s close: revoke lease=%x failed: %v", r.logPrefix(), int64(lease), err)
		}
		logx.Infof("%s closed: node_id=%d uuid=%s", r.logPrefix(), id, r.NodeUUID)
	})
}

// ---- 内部 ----------------------------------------------------------------------------

func newRegistration(cli *clientv3.Client, spec Spec, nodeUUID string) *Registration {
	ctx, cancel := context.WithCancel(context.Background())
	return &Registration{
		NodeUUID: nodeUUID,
		cli:      cli,
		spec:     spec,
		ctx:      ctx,
		cancel:   cancel,
		loopDone: make(chan struct{}),
	}
}

func (r *Registration) logPrefix() string {
	if r.spec.LogPrefix != "" {
		return r.spec.LogPrefix
	}
	return "[NodeRegistry " + r.spec.Prefix + "]"
}

func validateInputs(cli *clientv3.Client, spec Spec) error {
	if cli == nil {
		return fmt.Errorf("%w: nil etcd client", errInvalidSpec)
	}
	return spec.validate()
}

// validate 只做纯校验,不碰 etcd。
func (s Spec) validate() error {
	switch {
	case s.Prefix == "":
		return fmt.Errorf("%w: empty Prefix", errInvalidSpec)
	case strings.Contains(s.Prefix, "/"):
		return fmt.Errorf("%w: Prefix %q must not contain '/'", errInvalidSpec, s.Prefix)
	case s.ZoneId == 0:
		return fmt.Errorf("%w: ZoneId must be non-zero", errInvalidSpec)
	case s.LeaseTTL <= 0:
		return fmt.Errorf("%w: LeaseTTL must be > 0, got %d", errInvalidSpec, s.LeaseTTL)
	case s.BuildValue == nil:
		return fmt.Errorf("%w: nil BuildValue", errInvalidSpec)
	case s.OnReclaimFailed != ReallocateNewID && s.OnReclaimFailed != ExitProcess:
		return fmt.Errorf("%w: unknown OnReclaimFailed %s", errInvalidSpec, s.OnReclaimFailed)
	}
	return nil
}

// nodeInfoMirror 只声明要校验的字段。protojson 里 uint64 会编码成字符串,uint32 是数字;
// 这里用到的全是 uint32,protocolType 两种表示都可能出现,所以留 RawMessage 自己判。
type nodeInfoMirror struct {
	NodeId   uint32 `json:"nodeId"`
	NodeType uint32 `json:"nodeType"`
	NodeUuid string `json:"nodeUuid"`
	ZoneId   uint32 `json:"zoneId"`
	Endpoint struct {
		Port uint32 `json:"port"`
	} `json:"endpoint"`
	GrpcEndpoint struct {
		Port uint32 `json:"port"`
	} `json:"grpcEndpoint"`
	ProtocolType json.RawMessage `json:"protocolType"`
}

// validateValue 校验 BuildValue 产物。任何一项不符都拒注册:C++ / 路由服拿到一个 zone 错、
// 端口为 0 或协议不对的 NodeInfo,会把流量路由到错处且没有任何报错(fail-closed)。
//
// nodeType 与 nodeUuid 的相等校验比契约列出的"非空"更严:值里的 uuid 与 allocKey 的值
// 不一致时,Kafka target_instance_id 防僵尸与 Close 的归属判断都会错位。
func validateValue(raw []byte, spec Spec, nodeID uint32, nodeUUID string) error {
	var m nodeInfoMirror
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("%w: not a JSON NodeInfo: %v", errInvalidValue, err)
	}
	switch {
	case m.NodeId != nodeID:
		return fmt.Errorf("%w: nodeId=%d, want %d", errInvalidValue, m.NodeId, nodeID)
	case m.ZoneId != spec.ZoneId:
		return fmt.Errorf("%w: zoneId=%d, want %d", errInvalidValue, m.ZoneId, spec.ZoneId)
	case m.NodeType != spec.NodeType:
		return fmt.Errorf("%w: nodeType=%d, want %d", errInvalidValue, m.NodeType, spec.NodeType)
	case m.NodeUuid == "":
		return fmt.Errorf("%w: empty nodeUuid", errInvalidValue)
	case m.NodeUuid != nodeUUID:
		return fmt.Errorf("%w: nodeUuid=%q, want %q", errInvalidValue, m.NodeUuid, nodeUUID)
	case m.Endpoint.Port == 0:
		return fmt.Errorf("%w: endpoint.port is 0", errInvalidValue)
	case m.GrpcEndpoint.Port == 0:
		return fmt.Errorf("%w: grpcEndpoint.port is 0", errInvalidValue)
	case !isGRPCProtocol(m.ProtocolType):
		return fmt.Errorf("%w: protocolType=%s, want %q or %d", errInvalidValue,
			string(m.ProtocolType), protocolGRPCName, protocolGRPCNumber)
	}
	return nil
}

func isGRPCProtocol(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		return name == protocolGRPCName
	}
	var num uint32
	if err := json.Unmarshal(raw, &num); err == nil {
		return num == protocolGRPCNumber
	}
	return false
}

// nodeIDFromKey 从 key 尾部 "/node_id/<n>" 取 node_id。rpcPath 与 allocKey 尾段同形,
// 所以一个函数通吃。0 与越界值返回 false(不参与占用判断)。
func nodeIDFromKey(key string) (uint32, bool) {
	i := strings.LastIndex(key, nodeIDSegment)
	if i < 0 {
		return 0, false
	}
	id, err := strconv.ParseUint(key[i+len(nodeIDSegment):], 10, 32)
	if err != nil || uint32(id) < nodeIDMin || uint32(id) > nodeIDMax {
		return 0, false
	}
	return uint32(id), true
}

// usedNodeIDsFromKeys 把一批 key 折成已占 id 集合。
//
// 为什么从 key 路径而不是从值取:既有副本从 rpcPath 的值 protojson.Unmarshal 出 NodeId,
// 值解析失败(异源写入 / 字段改名)的节点就被当成空位,CAS 只守 allocKey,于是会覆盖别人
// 的 rpcPath。key 路径是 C++ 与 Go 共同的唯一契约。
// 两个子树(zone/… 与 allocated/…)的 id 都算占用:这是超集,只会让可选 id 变少,不会撞号。
func usedNodeIDsFromKeys(keys []string) map[uint32]bool {
	used := make(map[uint32]bool, len(keys))
	for _, k := range keys {
		if id, ok := nodeIDFromKey(k); ok {
			used[id] = true
		}
	}
	return used
}

func scanUsedNodeIDs(ctx context.Context, cli *clientv3.Client, prefix string) (map[uint32]bool, error) {
	// Prefix+"/" 而不是 Prefix:避免 "XNodeService.rpc" 前缀扫到 "XNodeService.rpcY/…"。
	resp, err := cli.Get(ctx, prefix+"/", clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return nil, fmt.Errorf("etcd get %s/: %w", prefix, err)
	}
	keys := make([]string, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		keys = append(keys, string(kv.Key))
	}
	return usedNodeIDsFromKeys(keys), nil
}

// buildValue 调回调并校验,返回可直接 Put 的值。
func (r *Registration) buildValue(nodeID uint32) (string, error) {
	raw, err := r.spec.BuildValue(nodeID, r.NodeUUID)
	if err != nil {
		return "", fmt.Errorf("BuildValue(node_id=%d): %w", nodeID, err)
	}
	if err := validateValue(raw, r.spec, nodeID, r.NodeUUID); err != nil {
		return "", err
	}
	return string(raw), nil
}

// allocate 从 1..nodeIDMax 顺序找第一个空位,用 CAS 占住,返回 id 与已写入的值。
//
// 与既有副本的差异:Txn 传输错误直接返回而不是 continue。etcd 不通时 continue 会对十几万个
// id 逐个发必然失败的 Txn;"写不进去"也不等于"这个 id 被占了"。
func (r *Registration) allocate(ctx context.Context, lease clientv3.LeaseID) (uint32, string, error) {
	used, err := scanUsedNodeIDs(ctx, r.cli, r.spec.Prefix)
	if err != nil {
		return 0, "", err
	}
	for id := nodeIDMin; id <= nodeIDMax; id++ {
		if used[id] {
			continue
		}
		value, err := r.buildValue(id)
		if err != nil {
			return 0, "", err
		}
		ok, err := claimNodeID(ctx, r.cli,
			AllocationKey(r.spec.Prefix, r.spec.NodeType, id),
			RpcPath(r.spec.Prefix, r.spec.ZoneId, r.spec.NodeType, id),
			r.NodeUUID, value, lease)
		if err != nil {
			return 0, "", fmt.Errorf("claim node_id=%d: %w", id, err)
		}
		if ok {
			return id, value, nil
		}
		// CAS 落空 = 扫描之后被别人抢先,试下一个。
	}
	return 0, "", fmt.Errorf("%w in [%d, %d] (prefix=%s)", errNoAvailableNodeID, nodeIDMin, nodeIDMax, r.spec.Prefix)
}

// claimNodeID:同一租约、同一 Txn,allocKey 不存在才写双 key(契约 §2)。
func claimNodeID(ctx context.Context, cli *clientv3.Client, allocKey, rpcKey, nodeUUID, value string, lease clientv3.LeaseID) (bool, error) {
	resp, err := cli.Txn(ctx).
		If(clientv3.Compare(clientv3.Version(allocKey), "=", 0)).
		Then(
			clientv3.OpPut(allocKey, nodeUUID, clientv3.WithLease(lease)),
			clientv3.OpPut(rpcKey, value, clientv3.WithLease(lease)),
		).
		Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}

// reclaimNodeID 用新租约重夺原 node_id,一笔 Txn 覆盖两种"仍属于我们"的状态:
//   - allocKey 已不存在(旧租约过期被 etcd 删掉)→ 重新 Create;
//   - allocKey 仍在且值是我们的 uuid(客户端先判定失租、服务端旧租约还没到期)→ 改挂新租约。
//
// 为什么不能只判 Version==0(既有副本的写法):第二种状态下会被误判成"被别人占了",
// ReallocateNewID 平白换号,ExitProcess 则直接把一个没人抢的进程杀掉。拆成两笔 Txn 也不行,
// key 恰在两笔之间过期时两头落空(snowflakealloc F2 同一教训)。
func reclaimNodeID(ctx context.Context, cli *clientv3.Client, allocKey, rpcKey, nodeUUID, value string, lease clientv3.LeaseID) (bool, error) {
	puts := []clientv3.Op{
		clientv3.OpPut(allocKey, nodeUUID, clientv3.WithLease(lease)),
		clientv3.OpPut(rpcKey, value, clientv3.WithLease(lease)),
	}
	resp, err := cli.Txn(ctx).
		If(clientv3.Compare(clientv3.Version(allocKey), "=", 0)).
		Then(puts...).
		Else(clientv3.OpTxn(
			[]clientv3.Cmp{clientv3.Compare(clientv3.Value(allocKey), "=", nodeUUID)},
			puts,
			nil,
		)).
		Commit()
	if err != nil {
		return false, err
	}
	if resp.Succeeded {
		return true, nil
	}
	if len(resp.Responses) == 0 {
		return false, errors.New("reclaim txn: else branch returned no response")
	}
	return resp.Responses[0].GetResponseTxn().GetSucceeded(), nil
}

// snapshot 读当前身份,供重注册在锁外做 etcd 调用。
func (r *Registration) snapshot() (lease clientv3.LeaseID, id uint32, rpcKey, allocKey, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leaseID, r.nodeID.Load(), r.rpcKey, r.allocKey, r.value
}

// commit 原子地切换到新身份。句柄已 Close 时拒绝并 Revoke 新租约(新租约上挂着的双 key
// 随之删除)—— 否则 Close 与重注册交错时会留下一个没人续租、也没人注销的发现键,
// 在 TTL 内被路由服选中。
func (r *Registration) commit(lease clientv3.LeaseID, id uint32, value string) bool {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		revokeLease(r.cli, lease, r.logPrefix())
		return false
	}
	r.leaseID = lease
	r.rpcKey = RpcPath(r.spec.Prefix, r.spec.ZoneId, r.spec.NodeType, id)
	r.allocKey = AllocationKey(r.spec.Prefix, r.spec.NodeType, id)
	r.value = value
	r.nodeID.Store(id)
	r.mu.Unlock()
	return true
}

// keepAliveLoop 是唯一改写身份的 goroutine:续租 → 失租 → 重注册 → 在新租约上继续续租。
// 用循环而不是既有副本的"重注册里再起一条 goroutine 递归",生命周期只有一条线,好推理。
func (r *Registration) keepAliveLoop() {
	for {
		lease, _, _, _, _ := r.snapshot()
		ch, err := r.cli.KeepAlive(r.ctx, lease)
		if err != nil {
			if r.ctx.Err() != nil {
				return
			}
			logx.Errorf("%s keepalive start failed lease=%x: %v; treating as lease lost", r.logPrefix(), int64(lease), err)
			// KeepAlive 立即失败(如客户端续租循环已停摆)时,不退避会以最快速度反复 Grant 新租约。
			if !r.sleep(backoffMin) {
				return
			}
		} else {
			// 通道关闭 = 租约过期 / 续租流无法恢复 / ctx 取消。响应必须持续消费,否则客户端丢包告警。
			for range ch {
			}
		}
		if r.ctx.Err() != nil {
			return
		}
		logx.Errorf("%s lease lost: node_id=%d lease=%x, re-registering (policy=%s)",
			r.logPrefix(), r.NodeID(), int64(lease), r.spec.OnReclaimFailed)
		if !r.reRegister(lease) {
			return
		}
	}
}

// attemptResult 是一轮重注册的结果。
type attemptResult int

const (
	attemptRetry      attemptResult = iota // 暂态失败,退避后再来
	attemptRegistered                      // 已在新租约上注册
	attemptStop                            // 句柄已关闭或进程将退出,keepalive 结束
)

// reRegister 带指数退避重试,直到注册成功、句柄关闭或按策略退出。
func (r *Registration) reRegister(oldLease clientv3.LeaseID) bool {
	backoff := backoffMin
	for {
		if r.ctx.Err() != nil {
			return false
		}
		switch r.reRegisterOnce(oldLease) {
		case attemptRegistered:
			return true
		case attemptStop:
			return false
		}
		if !r.sleep(backoff) {
			return false
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// reRegisterOnce 是一轮重注册:新租约 → CAS 重夺原 id → 失败按策略换号或退出。
//
// 关键安全点:失租期间原 id 可能已被别的实例合法占用,任何路径都不得无条件 Put 旧 key
// (会覆盖别人的 NodeInfo,C++ / 路由服把流量打到错误节点)。
func (r *Registration) reRegisterOnce(oldLease clientv3.LeaseID) attemptResult {
	ctx, cancel := context.WithTimeout(r.ctx, reRegisterOpTimeout)
	defer cancel()

	grant, err := r.cli.Grant(ctx, r.spec.LeaseTTL)
	if err != nil {
		logx.Errorf("%s re-register: grant lease failed: %v", r.logPrefix(), err)
		return attemptRetry
	}
	newLease := grant.ID

	_, oldID, rpcKey, allocKey, value := r.snapshot()
	reclaimed, err := reclaimNodeID(ctx, r.cli, allocKey, rpcKey, r.NodeUUID, value, newLease)
	if err != nil {
		revokeLease(r.cli, newLease, r.logPrefix())
		logx.Errorf("%s re-register: reclaim txn node_id=%d failed: %v", r.logPrefix(), oldID, err)
		return attemptRetry
	}
	if reclaimed {
		if !r.commit(newLease, oldID, value) {
			return attemptStop
		}
		// 双 key 已改挂新租约,旧租约上不再有我们的 key;旧租约若服务端还活着就顺手结束它。
		revokeLease(r.cli, oldLease, "")
		logx.Infof("%s re-register: reclaimed original node_id=%d lease=%x", r.logPrefix(), oldID, int64(newLease))
		return attemptRegistered
	}

	// 原 id 已归别的 uuid。
	if r.spec.OnReclaimFailed == ExitProcess {
		revokeLease(r.cli, newLease, r.logPrefix())
		logx.Errorf("%s re-register: node_id=%d was taken over by another instance and policy=ExitProcess "+
			"(node_id backs a persistent identity); exiting so the orchestrator restarts us with a fresh id",
			r.logPrefix(), oldID)
		exitProcess()
		return attemptStop
	}

	logx.Errorf("%s re-register: node_id=%d was taken over by another instance, allocating a new one",
		r.logPrefix(), oldID)
	newID, newValue, err := r.allocate(ctx, newLease)
	if err != nil {
		revokeLease(r.cli, newLease, r.logPrefix())
		logx.Errorf("%s re-register: allocate new node_id failed: %v", r.logPrefix(), err)
		return attemptRetry
	}
	if !r.commit(newLease, newID, newValue) {
		return attemptStop
	}
	revokeLease(r.cli, oldLease, "")
	logx.Infof("%s re-register: node_id changed %d -> %d lease=%x", r.logPrefix(), oldID, newID, int64(newLease))
	if cb := r.spec.OnNodeIDChanged; cb != nil && newID != oldID {
		safego.Run(callbackPoint, func() { cb(oldID, newID) })
	}
	return attemptRegistered
}

// sleep 可被 Close 打断;返回 false 表示句柄已关闭。
func (r *Registration) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// revokeLease 尽力 Revoke。logPrefix 为空表示失败也不打日志(撤销一个多半已过期的旧租约,
// "lease not found" 是预期内的)。
func revokeLease(cli *clientv3.Client, lease clientv3.LeaseID, logPrefix string) {
	ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
	defer cancel()
	if _, err := cli.Revoke(ctx, lease); err != nil && logPrefix != "" {
		logx.Errorf("%s revoke lease=%x failed (expires after TTL): %v", logPrefix, int64(lease), err)
	}
}

// dialAddrForListen 把监听地址换成本机可拨的地址。
func dialAddrForListen(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("%w: bad listenAddr %q: %v", errInvalidSpec, listenAddr, err)
	}
	if p, perr := strconv.ParseUint(port, 10, 16); perr != nil || p == 0 {
		return "", fmt.Errorf("%w: listenAddr %q needs a fixed non-zero port", errInvalidSpec, listenAddr)
	}
	if host == "" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// waitForListening 反复拨 addr 直到连上(随即关掉)、超时或 ctx 结束;返回最后一次拨号错误。
// 写法照 data_service.go waitForListening,多了 ctx 取消。
func waitForListening(ctx context.Context, addr string, timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("%w: listen wait timeout must be > 0, got %v", errInvalidSpec, timeout)
	}
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, listenDialTimeout)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if !time.Now().Before(deadline) {
			return err
		}
		t := time.NewTimer(listenPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
