// Package snowflakealloc 提供基于 etcd 的 Snowflake worker id 分配器。
//
// 与服务发现层的 NodeInfo.NodeId 分配解耦:
//   - NodeInfo.NodeId 由 node allocator(go/<service>/internal/node) 管理,
//     用于 C++ 端服务发现路由。
//   - Snowflake worker id 由本包管理,用于 ID 生成器的种子。
//
// 解耦的好处:即使 NodeInfo 在边缘场景(reRegister CAS 失败、etcd 短暂分裂)
// 出问题,Snowflake ID 也不受影响 —— 因为 worker id 由独立 lease 锚定到 hostname。
//
// hostname 亲和性:
//   - 同 hostname 重启 → 复用同一个 worker id(避免 worker id 变化导致的
//     Snowflake 时间戳回退冲突)
//   - 不同 hostname → 双 key CAS 拿一个空闲 worker id
//
// 用法:
//
//	leaseID, workerID, err := snowflakealloc.AllocateWithKeepAlive(
//	    ctx, cli, "/guild", os.Hostname(), 60)
//	if err != nil { panic(err) }
//	sf := snowflake.NewNode(workerID)
//
// 关键:**prefix 必须按服务区分**,否则两个服务的 worker id 池会互相干扰。
// 推荐 prefix:"/guild" / "/scene_manager" / "/<service_name>"。
package snowflakealloc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/snowflake"
)

// Options 控制分配器行为。
type Options struct {
	// LeaseTTL 是 etcd lease 的 TTL(秒)。0 时默认 60。
	LeaseTTL int64
	// MaxWorkerID 是 worker id 上限(含)。0 时默认取 shared/snowflake.NodeMask。
	// 调用方可以传更小的值,如果 Snowflake 实现位宽更窄(例如 bwmarrin 是 10 bit)。
	MaxWorkerID uint64
}

func (o Options) ttl() int64 {
	if o.LeaseTTL <= 0 {
		return 60
	}
	return o.LeaseTTL
}

func (o Options) maxWorkerID() uint64 {
	if o.MaxWorkerID == 0 {
		return uint64(snowflake.NodeMask)
	}
	return o.MaxWorkerID
}

// nodeKey 是 hostname → workerID 映射,用来支持 "同 hostname 重启复用同 id"。
//
//	<prefix>/snowflake_nodes/<hostname> = <id>
// leaseAlive 判断 nodeKey 当前挂着的 lease 是否仍存活(TTL > 0)。
// lease==0(key 无 lease)或 TimeToLive 返回 TTL<=0(已过期 / 已 revoke)视为已死;
// 查询出错时按"存活"处理 —— fail-closed:宁可多分配一个新 id,也不能抢活着的 id
// (抢到手的代价是原持有者 keepalive 检测到 ownership lost 后 fence + 退出)。
func leaseAlive(ctx context.Context, cli *clientv3.Client, lease int64) bool {
	if lease == 0 {
		return false
	}
	resp, err := cli.TimeToLive(ctx, clientv3.LeaseID(lease))
	if err != nil {
		logx.Errorf("[snowflakealloc] TimeToLive(lease=%d) failed, treating holder as alive: %v", lease, err)
		return true
	}
	return resp.TTL > 0
}

// releasedKey 是 Close() 留下的"已优雅释放"标记(挂在同一 lease 上,随 lease 一起消失)。
// value 固定为非数字的 "released":scanUsedWorkerIDs 按 nodeKey 前缀扫时会尝试把 value
// 解析成 id,非数字直接忽略,不会把它误算成占用。
// 语义:同 hostname 的后继进程只有在(lease 已死)或(有此标记 = 前任已优雅退出)时
// 才允许接管原 worker id;lease 活着且无标记 = 前任仍在运行(或刚崩溃、lease 未到期),
// 一律不抢,改派生键分配新 id。
func releasedKey(prefix, hostname string) string {
	return nodeKey(prefix, hostname) + "/released"
}

func nodeKey(prefix, hostname string) string {
	return fmt.Sprintf("%s/snowflake_nodes/%s", prefix, hostname)
}

// idKey 是 workerID 占位 key,反向映射回 hostname,用于扫描已占 ID。
//
//	<prefix>/snowflake_ids/<id> = <hostname>
func idKey(prefix string, id uint64) string {
	return fmt.Sprintf("%s/snowflake_ids/%d", prefix, id)
}

// guardKey 记录某个 worker id **最近一次发号所在的秒**(自 snowflake.Epoch 起)。
//
//	<prefix>/snowflake_guard/<id> = <epochSec>
//
// 刻意**不挂 lease**:它必须比持有者活得久 —— 下一任拿到同一个 worker id 时要靠它
// 把发号起点抬到前任高水位之上。挂了 lease 就会随前任一起消失,等于没有。
// 量级是每个 worker id 一行,被池上界(NodeMask)封死,不会无界增长。
// 这是 C++ 侧 `SETEX snowflake_guard:{node_type}:{node_id} 600 {now}` 的等价物。
func guardKey(prefix string, id uint64) string {
	return fmt.Sprintf("%s/snowflake_guard/%d", prefix, id)
}

func nodeKeyPrefix(prefix string) string {
	return prefix + "/snowflake_nodes/"
}

func idKeyPrefix(prefix string) string {
	return prefix + "/snowflake_ids/"
}

// Allocate 为 (prefix, hostname) 分配一个 Snowflake worker id。
//
// 行为:
//  1. 拿一个新 lease,TTL 由 opts 决定。
//  2. 如果 nodeKey(hostname) 还在(进程刚重启,etcd 老 lease 尚未过期或上次进程没等到 TTL):
//     CAS 重新认领:If Value(nodeKey)==<oldID> Then Put nodeKey+idKey with new lease。
//     成功就复用 oldID。
//  3. 否则扫 idKeyPrefix + nodeKeyPrefix(后者是兼容 lease 过期前的并发场景)合并 used 集合,
//     找最小空闲 worker id,**从 0 开始**(Snowflake worker_id=0 是合法的,无 "未分配" 哨兵语义,
//     这点和 NodeInfo.NodeId 不同)。
//  4. CAS If CreateRev(nodeKey)==0 AND CreateRev(idKey)==0 Then Put 双 key。
//     成功返回 (leaseID, workerID, nil)。失败重新扫描重试(意味着别人并发抢到了)。
//
// 调用方负责处理 lease 的 KeepAlive,或使用 AllocateWithKeepAlive。
//
// 错误条件:worker id 池耗尽(used 集合 size > maxWorkerID),返回错误。
func Allocate(ctx context.Context, cli *clientv3.Client, prefix, hostname string, opts Options) (clientv3.LeaseID, uint64, error) {
	if prefix == "" {
		return 0, 0, fmt.Errorf("snowflakealloc: empty prefix")
	}
	if hostname == "" {
		return 0, 0, fmt.Errorf("snowflakealloc: empty hostname")
	}

	leaseResp, err := cli.Grant(ctx, opts.ttl())
	if err != nil {
		return 0, 0, fmt.Errorf("snowflakealloc: grant lease: %w", err)
	}
	leaseID := leaseResp.ID

	nKey := nodeKey(prefix, hostname)
	maxID := opts.maxWorkerID()

	for {
		select {
		case <-ctx.Done():
			_, _ = cli.Revoke(context.Background(), leaseID)
			return 0, 0, ctx.Err()
		default:
		}

		// 1) 尝试复用 hostname 已有的 workerID。
		// 注意:此处用 Value 比较而不是 CreateRevision == 0 —— 我们希望
		// "如果 hostname 的 key 还在,且 value 是某个数字 X,就把同一个 X 用新 lease 抢回来"。
		resp, err := cli.Get(ctx, nKey)
		if err != nil {
			_, _ = cli.Revoke(context.Background(), leaseID)
			return 0, 0, fmt.Errorf("snowflakealloc: get nodeKey: %w", err)
		}
		if len(resp.Kvs) > 0 {
			released := false
			if relResp, rerr := cli.Get(ctx, releasedKey(prefix, hostname)); rerr == nil && len(relResp.Kvs) > 0 {
				released = true
			}
			if !released && leaseAlive(ctx, cli, resp.Kvs[0].Lease) {
				// 亲和键上的持有者还活着(lease 未过期)且没有优雅释放标记:**不抢**。
				// 2026-09-02 事故:同一主机上按 -Zone 起第二个 login,它按 hostname 亲和
				// "复用"了 worker 0,把 nodeKey 挂到自己的新 lease 上,活着的 zone1 login
				// 在 keepalive 里检测到 ownership lost 后 fence + 退出,整条登录链路 UNAVAILABLE。
				// 亲和复用的本意是"进程重启、老 lease 尚未过期时拿回原 id",不是让同主机的
				// 第二个进程抢活着的 id。但又不能停在原键上走新 id 分配 —— 下面的双 key CAS
				// 要求 nodeKey 不存在,会永远失败直到 ctx 超时(kill 后 60s 内重启也会撞上:
				// 进程已死、lease 尚在)。所以派生一个本进程独有的亲和键继续分配;旧 id 等旧
				// lease 自然过期后释放。
				derived := fmt.Sprintf("%s#%x", hostname, uint64(leaseID))
				logx.Errorf("[snowflakealloc] affinity key busy with a live lease (prefix=%s, host=%s, lease=%d); "+
					"allocating under derived key %q instead of stealing", prefix, hostname, resp.Kvs[0].Lease, derived)
				hostname = derived
				nKey = nodeKey(prefix, hostname)
				continue
			}
			oldIDStr := string(resp.Kvs[0].Value)
			if oldID, perr := strconv.ParseUint(oldIDStr, 10, 64); perr == nil && oldID <= maxID {
				iKey := idKey(prefix, oldID)
				// CAS: Value(nKey)==oldIDStr 才接管。这样防止 hostname key 过期后
				// 别人已经抢过 oldID 又写回别的 value 的极端竞态。
				txnResp, err := cli.Txn(ctx).
					If(clientv3.Compare(clientv3.Value(nKey), "=", oldIDStr)).
					Then(
						clientv3.OpPut(nKey, oldIDStr, clientv3.WithLease(leaseID)),
						clientv3.OpPut(iKey, hostname, clientv3.WithLease(leaseID)),
					).
					Commit()
				if err == nil && txnResp.Succeeded {
					logx.Infof("[snowflakealloc] reused worker_id=%d (prefix=%s, host=%s)", oldID, prefix, hostname)
					return leaseID, oldID, nil
				}
				// CAS 失败 → key 的 value 已变(典型:lease 过期重写),继续走分配新 ID 流程
			}
		}

		// 2) 扫 used 集合,找最小空闲 id。
		used, err := scanUsedWorkerIDs(ctx, cli, prefix)
		if err != nil {
			_, _ = cli.Revoke(context.Background(), leaseID)
			return 0, 0, err
		}

		var targetID uint64 = maxID + 1
		for i := uint64(0); i <= maxID; i++ {
			if !used[i] {
				targetID = i
				break
			}
		}
		if targetID > maxID {
			_, _ = cli.Revoke(context.Background(), leaseID)
			return 0, 0, fmt.Errorf("snowflakealloc: worker id pool exhausted (max %d, prefix=%s)", maxID, prefix)
		}

		// 3) 双 key CAS 抢占。两个 key 都必须不存在才能拿下,任何一个有人就放弃。
		idStr := strconv.FormatUint(targetID, 10)
		iKey := idKey(prefix, targetID)
		txnResp, err := cli.Txn(ctx).
			If(
				clientv3.Compare(clientv3.CreateRevision(nKey), "=", 0),
				clientv3.Compare(clientv3.CreateRevision(iKey), "=", 0),
			).
			Then(
				clientv3.OpPut(nKey, idStr, clientv3.WithLease(leaseID)),
				clientv3.OpPut(iKey, hostname, clientv3.WithLease(leaseID)),
			).
			Commit()
		if err != nil {
			_, _ = cli.Revoke(context.Background(), leaseID)
			return 0, 0, fmt.Errorf("snowflakealloc: claim txn: %w", err)
		}
		if txnResp.Succeeded {
			logx.Infof("[snowflakealloc] allocated worker_id=%d (prefix=%s, host=%s)", targetID, prefix, hostname)
			return leaseID, targetID, nil
		}
		// CAS 失败 → 并发抢占,重试整个循环(重新扫 used)
	}
}

// scanUsedWorkerIDs 合并扫 idKey 前缀 + nodeKey 前缀,
// 任一前缀显示的 id 都算占用。
func scanUsedWorkerIDs(ctx context.Context, cli *clientv3.Client, prefix string) (map[uint64]bool, error) {
	used := make(map[uint64]bool)

	// idKey 前缀:value 是 hostname,key 尾部数字是 id
	idResp, err := cli.Get(ctx, idKeyPrefix(prefix), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: scan id prefix: %w", err)
	}
	idP := idKeyPrefix(prefix)
	for _, kv := range idResp.Kvs {
		tail := strings.TrimPrefix(string(kv.Key), idP)
		if id, err := strconv.ParseUint(tail, 10, 64); err == nil {
			used[id] = true
		}
	}

	// nodeKey 前缀:value 是 id 字符串
	// 这个扫描是冗余的(idKey 已经覆盖),但保留以防 idKey 因运维误操作丢失却 nodeKey 还在的不对称状态。
	nodeResp, err := cli.Get(ctx, nodeKeyPrefix(prefix), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("snowflakealloc: scan node prefix: %w", err)
	}
	for _, kv := range nodeResp.Kvs {
		if id, err := strconv.ParseUint(string(kv.Value), 10, 64); err == nil {
			used[id] = true
		}
	}

	return used, nil
}

// Handle 把 lease + worker id + keepalive 取消函数打包,方便调用方在进程退出时清理。
type Handle struct {
	LeaseID  clientv3.LeaseID
	WorkerID uint64
	// GuardEpochSec 是**前任持有者**在这个 worker id 上最后记录的发号秒(自 snowflake.Epoch 起)。
	// 用 NewNode() 构造发号器时会被当作地板注入;0 表示这个 id 此前没人用过。
	//
	// ⚠️ Allocate 之后**只读**。曾经在 advanceGuard 里把它改写成"本进程刚写的水位",
	// 那既与本注释的语义矛盾(它是继承来的地板,不是自己的水位),又让这个导出字段
	// 与 keepalive goroutine 的写入发生数据竞争。自己的水位改用 guardWritten 跟踪。
	GuardEpochSec uint64
	prefix        string
	cancel        context.CancelFunc
	cli           *clientv3.Client
	// host 是分配时实际写入 idKey 的亲和键(可能是派生键 hostname#lease),
	// Close() 用它定位 releasedKey。
	host string

	// guardMu 串行化水位推进,guardWritten 是本进程**已成功落盘**的最大水位。
	//
	// 必须串行的原因:advanceGuard 有两个调用方并发 —— NewNode() 的同步首写(调用方
	// 协程)与 keepalive goroutine 的 tick,而 goroutine 在 NewNode() 之前就已启动。
	// 不串行会有两个后果:①对 guardWritten 的读改写是数据竞争;②两次 Put 可能乱序
	// 落盘,把已写的大值覆盖成小值 —— **水位倒退**,地板契约直接破,而这正是整套机制
	// 要防的东西。锁跨 Put 持有是刻意的:它同时保证了"值的顺序 = 落盘的顺序"。
	guardMu      sync.Mutex
	guardWritten uint64

	// node 是经 NewNode() 构造出的发号器(为空 = 调用方还没构造)。
	// advanceGuard 靠它读真实高水位:发号器借位(step 耗尽 / 时钟停摆)时
	// lastTime 会跑到墙钟前面,只写墙钟的水位对下一任就是低地板。
	// 单写单读:NewNode 在启动期调用一次,之后只有 keepalive goroutine 读。
	node atomic.Pointer[snowflake.Node]

	closing  atomic.Bool
	lost     chan struct{}
	lostOnce sync.Once
}

// Lost 在本进程**不再持有** worker id 的 etcd 租约时关闭。
//
// 为什么必须暴露:租约一过期,etcd 就可以把同一个 worker id 分给别的进程,
// 而本进程的 snowflake.Node 完全不知道,会继续用这个 worker id 发号 —— 两边
// 同一秒发出的号逐位相同。启动 guard 挡不住这种情况(它只覆盖"旧进程已经退出"
// 的重启窗口)。
//
// C++ 侧对同一个问题的处理是 Node::OnNodeIdConflictShutdown:立刻 fence 发号器,
// 存盘,然后退出。Go 服务没有"局内状态"要抢救,所以正确动作就是停止服务、
// 让编排把它拉起来 —— 重启后会拿一个新租约,并被启动 guard 兜住。
//
// Close() 引起的正常关闭**不会**触发本 channel。
func (h *Handle) Lost() <-chan struct{} {
	if h == nil {
		return nil
	}
	return h.lost
}

func (h *Handle) markLost() {
	h.lostOnce.Do(func() { close(h.lost) })
}

// AllocateWithKeepAlive 调用 Allocate 并在后台启动 KeepAlive。
// 返回的 Handle.Close() 会取消 KeepAlive 并 Revoke lease。
//
// keepalive goroutine 必须 drain 响应 channel —— 不读会导致 etcd client 的
// 16-slot buffer 填满后频繁打 "lease keepalive response queue is full" 日志。
// 这里用 for-range 静默吃掉响应,lease 本身由 etcd client 内部维护。
func AllocateWithKeepAlive(ctx context.Context, cli *clientv3.Client, prefix, hostname string, opts Options) (*Handle, error) {
	leaseID, workerID, err := Allocate(ctx, cli, prefix, hostname, opts)
	if err != nil {
		return nil, err
	}

	kaCtx, cancel := context.WithCancel(context.Background())
	ch, err := cli.KeepAlive(kaCtx, leaseID)
	if err != nil {
		cancel()
		_, _ = cli.Revoke(context.Background(), leaseID)
		return nil, fmt.Errorf("snowflakealloc: keepalive: %w", err)
	}

	// 读前任在这个 worker id 上留下的高水位。读不到(首次使用 / 被运维清过)就是 0,
	// 退化成只有启动 guard 的旧强度,不阻断启动。
	guardSec, gerr := readGuard(ctx, cli, prefix, workerID)
	if gerr != nil {
		logx.Errorf("[snowflakealloc] read guard watermark failed (prefix=%s, worker_id=%d): %v; "+
			"falling back to boot-guard only — a clock-skewed takeover could replay the previous holder's ids",
			prefix, workerID, gerr)
	}

	h := &Handle{
		LeaseID:       leaseID,
		WorkerID:      workerID,
		GuardEpochSec: guardSec,
		// 起点 = 前任已落盘的水位:低于它的目标值无需重复写(它已经罩住了)。
		guardWritten: guardSec,
		prefix:       prefix,
		cancel:       cancel,
		cli:          cli,
		lost:         make(chan struct{}),
	}

	// 所有权由 **key** 表达,而 KeepAlive 只观测 **lease** —— 两者错配就是静默双发号盲区:
	// Allocate 的 hostname 复用分支是无条件抢占(只 CAS nodeKey 的 value),
	// 别人接管时会把 nodeKey/idKey 直接 Put 到他自己的 lease 上。etcd 的 Put 只是把 key
	// 从旧 lease 摘下改挂新 lease,**不会撤销也不会通知旧 lease** —— 于是被抢的一方
	// KeepAlive 仍然成功、channel 不关、自 fencing 也不触发(它续租得好好的),
	// 却已经不再拥有这个 worker id。运维直接 etcdctl del 同理。
	// 所以必须补一条按 key 归属判定的通道:watch nodeKey,发现它改挂到别的 lease 或被删,
	// 立刻 markLost。起点 revision 取自下面这次 Get,并在同一次 Get 里先校验一遍当前归属,
	// 堵住"抢占发生在 Allocate 与 watch 之间"的窗口。
	// Allocate 在亲和键被活持有者占用时会改用派生键(hostname#lease)分配,所以
	// 这里不能用调用方传入的 hostname 反推 nodeKey,而要以 idKey 的 value(分配时
	// 写入的实际亲和键)为准;读不到时退回传入的 hostname。
	effectiveHost := hostname
	if idResp, gerr := cli.Get(ctx, idKey(prefix, workerID)); gerr == nil && len(idResp.Kvs) > 0 && len(idResp.Kvs[0].Value) > 0 {
		effectiveHost = string(idResp.Kvs[0].Value)
	} else if gerr != nil {
		logx.Errorf("[snowflakealloc] read idKey for effective affinity failed (prefix=%s, worker_id=%d): %v; watching %q",
			prefix, workerID, gerr, hostname)
	}
	h.host = effectiveHost
	nKey := nodeKey(prefix, effectiveHost)
	ownershipRev, err := verifyKeyOwnership(ctx, cli, nKey, leaseID)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("snowflakealloc: verify ownership: %w", err)
	}
	watchCh := cli.Watch(kaCtx, nKey, clientv3.WithRev(ownershipRev+1))

	fenceAfter := selfFenceAfter(opts.ttl())
	go func() {
		// 自 fencing 看门狗:只靠"KeepAlive channel 关闭"感知失租**恒晚于服务端过期点**。
		// clientv3(v3.5.x / v3.6.x 同)把 ka.deadline 设成 **收到响应的时刻** + TTL
		// (lease.go 的 recvKeepAlive),而服务端的过期点是它**处理续租的时刻** + TTL,
		// 前者恒晚一个 RTT;再叠加 deadlineLoop 每 1s 才扫一轮,channel 关闭时
		// worker id 可能已经被 etcd 判过期、并分给别的进程了。
		//
		// 更关键的是:本包的消费方(guild / scene_manager)收到 Lost() 后走的是
		// **优雅停** s.Stop(),排空在途请求期间仍会继续 Generate()。所以光"早点知道"不够,
		// 还得留出足够排空的余量 —— 这里按 **发出请求侧的单调时间** 判定:距上一次成功续租
		// 超过 fenceAfter(TTL 的 2/3)就主动认定失去身份。TTL=60s 时即 40s 触发,
		// 此时服务端 lease 仍有约 20s 才过期,这 20s 就是留给优雅排空的预算(§租约与
		// 重启时间预算必须闭合)。
		//
		// 不会误报:clientv3 的续租间隔是 TTL/3(20s),健康时距上次续租恒 ≤ ~20s < 40s。
		//
		// 节拍取 guardWriteInterval(1s)而不是 fenceAfter/4:持久水位的写入节奏
		// 由水位契约决定(见 guardLeadSec),比 fencing 判定所需的粒度更密。
		// 两件事共用这一个 ticker —— fencing 判定只是一次时间比较,提高频率无成本,
		// 反而让失租感知更快;不为水位另起第二套定时器(§15.2)。
		ticker := time.NewTicker(guardWriteInterval)
		defer ticker.Stop()
		lastRenew := time.Now() // 带单调读数,time.Since 不受墙钟跳变影响

		for {
			select {
			case _, ok := <-ch:
				if !ok {
					h.onKeepAliveEnded(prefix, hostname)
					return
				}
				lastRenew = time.Now()
			case <-ticker.C:
				if h.closing.Load() {
					return
				}
				if since := time.Since(lastRenew); since >= fenceAfter {
					h.onSelfFenced(prefix, hostname, since, fenceAfter)
					return
				}
				// 顺带推进持久水位。复用这个已有的 ticker,不另起 goroutine / 第二套定时器。
				// 地板契约:水位必须**不早于**最后一次发号所在的逻辑秒(见 advanceGuard
				// 取 max 的注释);按 tick 粒度写意味着借位后最多一个 tick 内水位追平,
				// 该窗口内前任崩溃仍有残余风险,但已从"整个借位期"缩到"单个 tick"。
				h.advanceGuard(kaCtx)
			case wr, ok := <-watchCh:
				if h.closing.Load() {
					return
				}
				if !ok || wr.Err() != nil {
					// watch 断了就再也看不见被抢占,不能假装还持有所有权。
					// fail-closed:按失去身份处理,交给编排重拉重新抢号。
					h.onOwnershipLost(prefix, hostname,
						fmt.Sprintf("ownership watch ended (closed=%v, err=%v)", !ok, wr.Err()))
					return
				}
				for _, ev := range wr.Events {
					if ev.Type == clientv3.EventTypeDelete {
						h.onOwnershipLost(prefix, hostname, "node key deleted")
						return
					}
					// 被接管时 value 不变(还是同一个 worker id),**变的是 lease** ——
					// 所以只能比 lease,比 value 是看不出来的。
					if ev.Kv != nil && clientv3.LeaseID(ev.Kv.Lease) != h.LeaseID {
						h.onOwnershipLost(prefix, hostname,
							fmt.Sprintf("node key re-attached to lease %x (ours is %x)",
								ev.Kv.Lease, h.LeaseID))
						return
					}
				}
			}
		}
	}()

	return h, nil
}

// selfFenceAfter 是"距上次成功续租多久就主动认定失去 worker id"。
// 取 TTL 的 2/3:既远大于 clientv3 的续租间隔(TTL/3)不会误报,
// 又在服务端过期点之前留下约 TTL/3 的余量给调用方优雅排空。
// TTL 极小时钳一个下界,避免 fenceAfter 退化到与续租间隔同量级而误报。
func selfFenceAfter(ttlSec int64) time.Duration {
	d := time.Duration(ttlSec) * time.Second * 2 / 3
	if d < 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// NewNode 用本 Handle 的 worker id 构造发号器,并把前任高水位作为**地板**注入。
//
// 请一律用它取代裸 snowflake.NewNode(h.WorkerID):裸构造只有"不在构造秒发号"的点排除,
// 顶不住跨机时钟偏斜接管、本机时钟回拨、前任借过逻辑秒这三类情况。
func (h *Handle) NewNode() *snowflake.Node {
	n := snowflake.NewNode(h.WorkerID)
	if h.GuardEpochSec > 0 {
		n.SetGuardTime(h.GuardEpochSec)
	}
	// 留一份引用给 advanceGuard 读高水位(见 Handle.node 注释)。
	h.node.Store(n)

	// **同步写一次水位再把发号器交出去**:否则从这里到 keepalive goroutine 的
	// 第一个 tick 之间(≤guardWriteInterval),我们已经在发号,而 etcd 里还是
	// 前任的旧水位 —— 这段发出的号不被任何地板覆盖,崩溃 + 时钟回拨即可被继任者重放。
	// 写失败只告警不阻断:与 advanceGuard 同理,水位是"下一任的地板",
	// 本进程自己的唯一性由 SetGuardTime 注入的地板与高水位单调保证。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	h.advanceGuard(ctx)
	cancel()
	return n
}

// readGuard 读某个 worker id 的持久高水位。key 不存在返回 0(此前没人用过)。
func readGuard(ctx context.Context, cli *clientv3.Client, prefix string, id uint64) (uint64, error) {
	resp, err := cli.Get(ctx, guardKey(prefix, id))
	if err != nil {
		return 0, err
	}
	if len(resp.Kvs) == 0 {
		return 0, nil
	}
	sec, perr := strconv.ParseUint(string(resp.Kvs[0].Value), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("malformed guard value %q: %w", resp.Kvs[0].Value, perr)
	}
	return sec, nil
}

// advanceGuard 把本 worker id 的持久高水位推进到"墙钟秒与发号器真实高水位的较大者"。
// 写失败只告警不阻断:水位是"下一任的地板",本进程自己的唯一性不依赖它。
//
// 必须取 max 而不能只写墙钟:发号器在 step 耗尽 / 时钟停摆时会**借位**
// (snowflake.Generate 的 default 分支,lastTime 跑到墙钟前面),此时只写墙钟
// 的水位低于真实已发号的秒;若前任在借位窗口内崩溃,继任者以低地板 + step=0
// 重发,与前任借位期间的号逐位相同。这正是 snowflake.go SetGuardTime 注释里
// 点名要 guard 覆盖的第三类情况("前任因发满 step 池借过逻辑秒")。
// 同理,回拨窗口内墙钟 <= GuardEpochSec 时也不能直接冻结 —— 高水位可能仍在涨。
func (h *Handle) advanceGuard(ctx context.Context) {
	target := snowflake.NowEpochSec()
	if n := h.node.Load(); n != nil {
		if hw := n.HighWaterEpochSec(); hw > target {
			target = hw
		}
	}
	// **前推 guardLeadSec**:水位的契约是"不早于本进程可能发到的最大逻辑秒",
	// 而两次写入之间我们还在继续发号。只写当前值的话,[上次写入, 崩溃时刻] 这段
	// (最长一个写入间隔)发出的号就超出了已持久化的水位 —— 继任者按该水位当地板
	// 时罩不住这段,时钟回拨叠加即可重放。前推量 > 写入间隔就把这段覆盖掉了。
	// 与 login 的毫秒级水位(1s 节拍 / 2s 前推)同一套推导,数值口径也一致。
	target += guardLeadSec

	// 锁跨 Put 持有:见 guardMu 注释 —— 它同时挡住数据竞争与"乱序落盘导致水位倒退"。
	h.guardMu.Lock()
	defer h.guardMu.Unlock()
	if target <= h.guardWritten {
		return // 水位没有前进,不重复写
	}
	putCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err := h.cli.Put(putCtx, guardKey(h.prefix, h.WorkerID), strconv.FormatUint(target, 10))
	cancel()
	if err != nil {
		logx.Errorf("[snowflakealloc] advance guard watermark failed (prefix=%s, worker_id=%d): %v",
			h.prefix, h.WorkerID, err)
		return
	}
	h.guardWritten = target
}

const (
	// guardWriteInterval 是持久水位的写入节拍(同时也是 fencing 看门狗的 tick)。
	guardWriteInterval = time.Second
	// guardLeadSec 是水位的**前推量**(秒),必须 **> 写入间隔**。
	//
	// 水位契约:任何时刻,已持久化的水位 ≥ 本进程可能发到的最大逻辑秒。
	// 写入间隔 1s、前推 2s ⇒ 两次写入之间即使写不出去,上一次写的 (now+2)
	// 也仍然覆盖着这 1s 内能发到的秒,契约不破。
	//
	// 上界同样有约束:继任者拿它当地板,若前推过大(比如 20s),继任者的首个号
	// 会被顶到墙钟前面很多,撞上 snowflake 的借位预算(maxBorrowAheadSec=10)
	// 直接 fail-closed 发不出号。2s 既满足下界又远离上界,且正常重启耗时 > 2s,
	// 继任者拿到手时墙钟通常已越过水位,零等待。
	guardLeadSec = 2
)

// guardMsKey 是毫秒级持久水位 key,给 **bwmarrin 布局**的消费方(login PlayerId)用。
// 与 guardKey 的秒级/snowflake.Epoch 口径完全独立:bwmarrin 是毫秒 epoch,秒级地板
// 塞不进去;值统一存 **Unix 毫秒**(不带任何自定义 epoch),避免两套 epoch 换算错位。
func guardMsKey(prefix string, id uint64) string {
	return fmt.Sprintf("%s/guard_ms/%d", prefix, id)
}

// ReadMsWatermark 读本 worker id 的毫秒级持久水位(Unix ms)。key 不存在返回 0。
// 读失败必须返回错误让调用方 fail-closed —— 水位是防跨重启重放的唯一地板,
// 读不到就当 0 启动等于没有地板。
func (h *Handle) ReadMsWatermark(ctx context.Context) (uint64, error) {
	resp, err := h.cli.Get(ctx, guardMsKey(h.prefix, h.WorkerID))
	if err != nil {
		return 0, err
	}
	if len(resp.Kvs) == 0 {
		return 0, nil
	}
	ms, perr := strconv.ParseUint(string(resp.Kvs[0].Value), 10, 64)
	if perr != nil {
		return 0, fmt.Errorf("malformed ms watermark %q: %w", resp.Kvs[0].Value, perr)
	}
	return ms, nil
}

// PutMsWatermark 写毫秒级持久水位。写入节奏与前推量由调用方决定(它才知道
// 自己发号器的时钟语义);本方法只保证单键覆盖写。写失败与 advanceGuard 同理
// 只回错误不重试 —— 水位是"下一任的地板",本进程自己的唯一性不依赖它。
func (h *Handle) PutMsWatermark(ctx context.Context, ms uint64) error {
	putCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := h.cli.Put(putCtx, guardMsKey(h.prefix, h.WorkerID), strconv.FormatUint(ms, 10))
	return err
}

// verifyKeyOwnership 确认 nodeKey 当前确实挂在 leaseID 上,并返回可用作 watch 起点的 revision。
// 堵住"Allocate 成功之后、watch 建立之前被人抢走"的窗口。
func verifyKeyOwnership(ctx context.Context, cli *clientv3.Client, nKey string,
	leaseID clientv3.LeaseID) (int64, error) {
	resp, err := cli.Get(ctx, nKey)
	if err != nil {
		return 0, err
	}
	if len(resp.Kvs) != 1 {
		return 0, fmt.Errorf("node key %s missing right after allocation", nKey)
	}
	if got := clientv3.LeaseID(resp.Kvs[0].Lease); got != leaseID {
		return 0, fmt.Errorf("node key %s already re-attached to lease %x (ours is %x)", nKey, got, leaseID)
	}
	return resp.Header.Revision, nil
}

// onOwnershipLost 在"key 归属已经不是自己"时调用。与失租不同,这里 lease 可能仍然活着、
// KeepAlive 也仍在成功 —— 但 worker id 已经被别人接管,继续发号就是确定性撞号。
func (h *Handle) onOwnershipLost(prefix, hostname, reason string) {
	if h.closing.Load() {
		return
	}
	logx.Errorf("[snowflakealloc] worker id ownership lost (prefix=%s, host=%s, worker_id=%d): %s; "+
		"the lease may still be alive but another process now owns this id — MUST stop minting IDs now",
		prefix, hostname, h.WorkerID, reason)
	h.markLost()
}

// onSelfFenced 在"距上次成功续租超过安全余量"时调用:此时**还没有**收到 channel 关闭,
// 但已经无法证明自己仍持有 worker id,按 fail-closed 立即报信,不等 etcd 的确认。
func (h *Handle) onSelfFenced(prefix, hostname string, since, budget time.Duration) {
	if h.closing.Load() {
		return
	}
	logx.Errorf("[snowflakealloc] no keepalive ack for %v (budget %v; prefix=%s, host=%s, worker_id=%d); "+
		"cannot prove this process still owns the worker id — MUST stop minting IDs now, "+
		"the etcd lease will expire shortly and the id may be handed to another process",
		since, budget, prefix, hostname, h.WorkerID)
	h.markLost()
}

// onKeepAliveEnded 在 KeepAlive 响应流结束时调用。
// 结束有两种可能:Close() 主动取消,或者租约真的没了(etcd 不可达超过 TTL /
// 租约被撤销)。只有后者是"身份丢失",要通知调用方停止发号。多次调用安全。
func (h *Handle) onKeepAliveEnded(prefix, hostname string) {
	if h.closing.Load() {
		return
	}
	logx.Errorf("[snowflakealloc] keepalive channel closed (prefix=%s, host=%s, worker_id=%d); "+
		"this process no longer owns the worker id and MUST stop minting IDs",
		prefix, hostname, h.WorkerID)
	h.markLost()
}

// Close 停止 KeepAlive。**刻意不 Revoke lease**:worker id 会在 lease 自然过期(TTL)
// 之后才被释放,给下一个持有者留出一段隔离期。多次调用安全。
//
// 为什么不能 Revoke(这里曾经 Revoke,是一个真实缺陷):
//   - 分配器取的是**最小空闲 id**(见 Allocate 第 2 步),所以刚被释放的 id 正是下一个
//     启动者优先拿到的那个 —— 不是"可能撞",是"优先撞";
//   - snowflake.NewNode 的启动 guard 只保证"绝不在构造那一秒发号"(**点排除**),
//     它不是"以前任高水位为地板"。新持有者用的是**自己机器的时钟**:若它比前任慢
//     (NTP 漂移 / 快照恢复 / 未同步),它的 now+1 可能仍 ≤ 前任最后发号的那一秒,
//     于是逐位重发前任已经发出去的号,且全程静默无告警;
//   - 立即 Revoke 恰好删掉了 lease TTL 这个唯一能吸收跨机时钟偏斜的缓冲,导致
//     **优雅退出比崩溃退出更危险**(崩溃不走本函数,反而有 ≤TTL 的天然隔离)。
//     去掉 Revoke 后两条退出路径行为一致。
//
// 代价:优雅退出后该 worker id 多占 ≤TTL(默认 60s)。池上界是 NodeMask=131071,
// 且同 hostname 重启走 Allocate 的复用分支(nodeKey 仍在 ⇒ Value CAS 换新 lease,
// 不消耗新槽位),所以耗尽需要 TTL 内出现十万级不同 hostname,现实中不可达。
//
// 注意这不是彻底修复,只是把可容忍的时钟偏斜从"数秒"抬到"≤TTL"。要在任意偏斜下都不撞,
// 需要补 C++ 侧那套持久化水位(SETEX snowflake_guard + SetGuardTime 的地板语义)。
func (h *Handle) Close() {
	if h == nil {
		return
	}
	// 先置标记再取消 keepalive,否则 goroutine 会把正常关闭误判成失租。
	h.closing.Store(true)
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
	// 优雅释放标记:挂在仍存活的 lease 上,随它一起过期。同 hostname 的后继进程
	// 据此判定"前任已退出,可以在 TTL 内接管原 worker id";没有标记(崩溃 / 仍在运行)
	// 则后继进程走派生键拿新 id,不会抢活着的持有者(见 Allocate 复用分支)。
	// 尽力而为:写失败只损失"重启复用同 id"这一优化,不影响正确性。
	if h.cli != nil && h.LeaseID != 0 && h.host != "" {
		putCtx, putCancel := context.WithTimeout(context.Background(), 2*time.Second)
		if _, err := h.cli.Put(putCtx, releasedKey(h.prefix, h.host), "released", clientv3.WithLease(h.LeaseID)); err != nil {
			logx.Errorf("[snowflakealloc] write released marker failed (prefix=%s, host=%s): %v", h.prefix, h.host, err)
		}
		putCancel()
	}
	h.LeaseID = 0
}
